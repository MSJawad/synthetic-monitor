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

func (f *fakeCounter) Counts(_ context.Context, check string, since time.Time) (int, int, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.since == nil {
		f.since = make(map[string]time.Time)
	}
	f.since[check] = since
	if check == f.failOn {
		return 0, 0, time.Time{}, errors.New("database is locked")
	}
	c := f.counts[check]
	return c[0], c[1], f.first[check], nil
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
