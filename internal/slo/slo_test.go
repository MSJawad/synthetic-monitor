package slo

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

const month = 30 * 24 * time.Hour

func TestCompute(t *testing.T) {
	tests := []struct {
		name            string
		target          float64
		total, failed   int
		availability    float64
		budgetRemaining float64
	}{
		{"no failures", 0.995, 1000, 0, 1, 1},
		{"part of the budget used", 0.995, 1000, 2, 0.998, 0.6},
		{"budget exactly used", 0.995, 1000, 5, 0.995, 0},
		{"budget exactly used, three nines", 0.999, 3000, 3, 0.999, 0},
		{"budget overspent", 0.995, 1000, 10, 0.99, -1},
		{"everything failed", 0.9, 10, 10, 0, -9},
		{"one probe, failed", 0.999, 1, 1, 0, -999},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok := Compute(tt.target, month, tt.total, tt.failed)
			if !ok {
				t.Fatal("Compute reported no data")
			}
			if !near(s.Availability, tt.availability) || !near(s.BudgetRemaining, tt.budgetRemaining) {
				t.Errorf("availability %v, budget remaining %v; want %v and %v",
					s.Availability, s.BudgetRemaining, tt.availability, tt.budgetRemaining)
			}
		})
	}

	if s, _ := Compute(0.995, month, 1000, 5); s.BudgetRemaining > 0 {
		t.Errorf("budget remaining with 5 of 5 allowed failures = %v, want exactly 0 or less", s.BudgetRemaining)
	}
	if _, ok := Compute(0.995, month, 0, 0); ok {
		t.Error("Compute with no probes reported a status")
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// fakeCounter answers Counts from tables and records the windows asked for.
type fakeCounter struct {
	mu     sync.Mutex
	counts map[string][2]int
	first  map[string]time.Time
	failOn string
	since  map[string]time.Time
	calls  int
}

func (f *fakeCounter) Counts(_ context.Context, check string, since time.Time) (Counts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.since == nil {
		f.since = make(map[string]time.Time)
	}
	// The SLO window is the earliest asked for; the burn windows come after.
	if prev, ok := f.since[check]; !ok || since.Before(prev) {
		f.since[check] = since
	}
	if check == f.failOn {
		return Counts{}, errors.New("database is locked")
	}
	c := f.counts[check]
	return Counts{Total: c[0], Failed: c[1], First: f.first[check]}, nil
}

// history answers Counts from individual probes, so every window counts
// what it would in the store.
type history map[string][]sample

type sample struct {
	at time.Time
	ok bool
}

func (h history) Counts(_ context.Context, check string, since time.Time) (Counts, error) {
	var c Counts
	for _, p := range h[check] {
		if c.First.IsZero() || p.at.Before(c.First) {
			c.First = p.at
		}
		if p.at.Before(since) {
			continue
		}
		c.Total++
		if !p.ok {
			c.Failed++
			if c.FirstFailure.IsZero() || p.at.Before(c.FirstFailure) {
				c.FirstFailure = p.at
			}
		}
	}
	return c, nil
}

// probes returns a probe every interval for the span before now, oldest
// first; failed decides, by age, which ones fail.
func probes(now time.Time, span, interval time.Duration, failed func(age time.Duration, i int) bool) []sample {
	var out []sample
	for i, age := 0, span; age > 0; i, age = i+1, age-interval {
		out = append(out, sample{at: now.Add(-age), ok: !failed(age, i)})
	}
	return out
}

func TestBurnThresholds(t *testing.T) {
	month, week := 30*24*time.Hour, 7*24*time.Hour
	for _, tt := range []struct {
		rule   BurnRule
		window time.Duration
		want   float64
	}{
		{BurnRules[0], month, 14.4}, {BurnRules[1], month, 6},
		{BurnRules[0], week, 3.36}, {BurnRules[1], week, 1.4},
	} {
		if got := tt.rule.Threshold(tt.window); !near(got, tt.want) {
			t.Errorf("%v rule over %v: threshold %v, want %v", tt.rule.Long, tt.window, got, tt.want)
		}
	}
}

func TestEvaluateBurning(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	never := func(time.Duration, int) bool { return false }
	check := func(name string) config.Check {
		c := withSLO(name, 99.5, 30)
		c.FailureThreshold = 3
		return c
	}
	h := history{
		// Every other probe failing for the last 20 minutes, as when one of
		// two replicas is broken: never 3 in a row.
		"flaky": probes(now, 8*time.Hour, 30*time.Second, func(age time.Duration, i int) bool {
			return age <= 20*time.Minute && i%2 == 0
		}),
		// The same for 50 minutes, stopping 10 minutes ago: the last 5
		// minutes are clean, so the fast rule no longer fires, but the slow
		// one, which looks at the last 30, still does.
		"settling": probes(now, 8*time.Hour, 30*time.Second, func(age time.Duration, i int) bool {
			return age > 10*time.Minute && age <= time.Hour && i%2 == 0
		}),
		// The same pattern, long enough ago that both rules have cleared.
		"recovered": probes(now, 8*time.Hour, 30*time.Second, func(age time.Duration, i int) bool {
			return age > 2*time.Hour && age <= 150*time.Minute && i%2 == 0
		}),
		"healthy": probes(now, 8*time.Hour, 30*time.Second, never),
		// Probed hourly: one failure is a 100% failure rate, but not 3 failures.
		"hourly": probes(now, 8*time.Hour, time.Hour, func(age time.Duration, _ int) bool { return age <= time.Hour }),
	}
	statuses, err := Evaluate(t.Context(), h,
		[]config.Check{check("flaky"), check("settling"), check("recovered"), check("healthy"), check("hourly")}, now)
	if err != nil {
		t.Fatal(err)
	}

	flaky := statuses["flaky"]
	if b := flaky.Burning; b == nil || b.Rule != BurnRules[0] || b.Failed != 20 || b.Total != 120 ||
		!b.Since.Equal(now.Add(-20*time.Minute)) {
		t.Errorf("flaky: burning %+v; want the fast rule, 20 of 120 probes failed since 20 minutes ago", b)
	}
	// 20 of 120 failed against an allowance of 0.6: a rate of 33.3.
	if !near(flaky.Rates[time.Hour], 20/0.6) || !near(flaky.Rates[5*time.Minute], 5/0.05) {
		t.Errorf("flaky: rates %v; want 33.3 over an hour and 100 over 5 minutes", flaky.Rates)
	}
	if b := statuses["settling"].Burning; b == nil || b.Rule != BurnRules[1] {
		t.Errorf("settling: burning %+v; want the slow rule only", b)
	}
	for _, name := range []string{"recovered", "healthy", "hourly"} {
		if b := statuses[name].Burning; b != nil {
			t.Errorf("%s: burning %+v; want nil", name, b)
		}
	}
}

func withSLO(name string, target float64, days int) config.Check {
	return config.Check{Name: name, SLO: &config.SLO{Target: target, WindowDays: days}}
}

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	counter := &fakeCounter{
		counts: map[string][2]int{"web": {2000, 4}, "api": {100, 0}},
		// web's history goes back further than its window; api's began an
		// hour ago.
		first:  map[string]time.Time{"web": now.Add(-40 * 24 * time.Hour), "api": now.Add(-time.Hour)},
		failOn: "broken",
	}
	checks := []config.Check{
		withSLO("web", 99.5, 30),
		withSLO("api", 99.9, 7),
		{Name: "no-slo"},
		withSLO("new", 99, 30), // no probes yet
		withSLO("broken", 99, 30),
	}

	statuses, err := Evaluate(t.Context(), counter, checks, now)
	if err == nil {
		t.Error("Evaluate hid the error counting broken's probes")
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses for %d checks, want 2 (web and api): %+v", len(statuses), statuses)
	}
	if s := statuses["web"]; s.Total != 2000 || s.Failed != 4 || !near(s.BudgetRemaining, 0.6) {
		t.Errorf("web = %+v, want 2000 probes, 4 failed, 60%% of budget left", s)
	}
	if since := counter.since["api"]; !since.Equal(now.Add(-7 * 24 * time.Hour)) {
		t.Errorf("api counted from %v, want its 7-day window", since)
	}
	if s := statuses["web"]; s.Partial || !s.Since.Equal(now.Add(-30*24*time.Hour)) {
		t.Errorf("web: partial %v since %v; want its whole 30-day window", s.Partial, s.Since)
	}
	if s := statuses["api"]; !s.Partial || !s.Since.Equal(now.Add(-time.Hour)) {
		t.Errorf("api: partial %v since %v; want partial, since its first probe an hour ago", s.Partial, s.Since)
	}
	if _, asked := counter.since["no-slo"]; asked {
		t.Error("Evaluate counted probes of a check without an SLO")
	}
}

func TestRunPublishesImmediatelyThenEveryInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		counter := &fakeCounter{counts: map[string][2]int{"a": {10, 0}, "b": {10, 1}}}
		var mu sync.Mutex
		checks := []config.Check{withSLO("a", 99, 30)}
		var published []map[string]Status

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			Run(ctx, 30*time.Second, counter,
				func() []config.Check { mu.Lock(); defer mu.Unlock(); return checks },
				func(s map[string]Status) { mu.Lock(); defer mu.Unlock(); published = append(published, s) },
				slog.New(slog.DiscardHandler))
		}()

		synctest.Wait()
		mu.Lock()
		if len(published) != 1 {
			t.Errorf("published %d times at start, want 1", len(published))
		}
		checks = append(checks, withSLO("b", 99, 30)) // a reload adds b
		mu.Unlock()

		time.Sleep(30 * time.Second)
		synctest.Wait()
		mu.Lock()
		if len(published) != 2 || len(published[1]) != 2 {
			t.Errorf("after one interval: %d publications, the last with %d checks; want 2 and 2", len(published), len(published[len(published)-1]))
		}
		mu.Unlock()

		cancel()
		<-done
	})
}
