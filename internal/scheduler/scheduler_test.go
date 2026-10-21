package scheduler

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

// Every test runs inside a synctest bubble: time is simulated, so minutes of
// schedule pass instantly and timings are exact. synctest also fails a test
// whose goroutines are still blocked when it ends.

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestFirstProbeWithinIntervalThenFixedCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(time.Minute)
		synctest.Wait()

		starts := h.startsOf("a")
		if len(starts) < 6 {
			t.Fatalf("%d probes in a minute at a 10s interval, want at least 6", len(starts))
		}
		if starts[0] >= 10*time.Second {
			t.Errorf("first probe at %v, want within the first interval", starts[0])
		}
		for i := 1; i < len(starts); i++ {
			if gap := starts[i] - starts[i-1]; gap != 10*time.Second {
				t.Errorf("probes %d and %d are %v apart, want exactly 10s", i-1, i, gap)
			}
		}
		for _, lag := range h.lagsOf("a") {
			if lag != 0 {
				t.Errorf("probe started %v after it was scheduled, want on time without contention", lag)
			}
		}
		h.stop()
	})
}

func TestFirstProbesSpreadOverIntervalOrFirstMinute(t *testing.T) {
	tests := []struct {
		interval, spread time.Duration
	}{
		{10 * time.Second, 10 * time.Second}, // the whole interval
		{time.Hour, time.Minute},             // capped, so hourly checks report within a minute
	}
	for _, tt := range tests {
		t.Run(tt.interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, 100)
				var checks []config.Check
				for i := range 100 {
					checks = append(checks, check(fmt.Sprintf("c%d", i), tt.interval))
				}
				h.sched.Apply(checks)
				time.Sleep(tt.spread)
				synctest.Wait()

				buckets := make(map[time.Duration]bool)
				for _, c := range checks {
					starts := h.startsOf(c.Name)
					if len(starts) == 0 || starts[0] >= tt.spread {
						t.Fatalf("check %s first ran at %v, want within %v", c.Name, starts, tt.spread)
					}
					buckets[starts[0]/(tt.spread/10)] = true
				}
				// With 100 uniformly random start times, missing more than
				// two of ten equal buckets has a probability below 1e-13.
				if len(buckets) < 8 {
					t.Errorf("first probes fell in only %d of 10 equal parts of %v; want them spread out", len(buckets), tt.spread)
				}
				h.stop()
			})
		})
	}
}

func TestOverrunningProbeSkipsMissedSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		// In production a probe overruns its slot by waiting for a free
		// concurrency slot; a probe that takes 25s is the simplest stand-in.
		h.setDuration("a", 25*time.Second)
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(3 * time.Minute)
		synctest.Wait()

		starts := h.startsOf("a")
		for i := 1; i < len(starts); i++ {
			if gap := starts[i] - starts[i-1]; gap != 30*time.Second {
				t.Errorf("probes %d and %d are %v apart, want 30s: the next free 10s slot", i-1, i, gap)
			}
		}
		skips := h.skipsOf("a")
		if done := h.recorded("a"); len(skips) != done {
			t.Errorf("OnSkip called %d times for %d completed probes, all of which overran", len(skips), done)
		}
		for _, n := range skips {
			if n != 2 {
				t.Errorf("OnSkip reported %d skipped runs, want 2 (a 25s probe overruns two 10s slots)", n)
			}
		}
		if got := h.maxInFlight("a"); got != 1 {
			t.Errorf("check a had %d probes in flight at once, want 1", got)
		}
		h.stop()
	})
}

func TestConcurrencyLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 3)
		var checks []config.Check
		for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
			h.setDuration(name, 5*time.Second)
			checks = append(checks, check(name, 10*time.Second))
		}
		h.sched.Apply(checks)
		time.Sleep(time.Minute)
		synctest.Wait()

		if got := h.maxInFlightTotal(); got != 3 {
			t.Errorf("at most %d probes were in flight at once, want the limit of 3", got)
		}
		var waited bool
		for _, c := range checks {
			for _, lag := range h.lagsOf(c.Name) {
				waited = waited || lag > 0
			}
		}
		if !waited {
			t.Error("no probe started late, want waits for a free slot to show as lag")
		}
		h.stop()
	})
}

func TestApplyKeepsScheduleOfUnchangedChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		a := check("a", 10*time.Second)
		h.sched.Apply([]config.Check{a})
		time.Sleep(25 * time.Second)
		h.sched.Apply([]config.Check{a, check("b", 10*time.Second)})
		time.Sleep(13 * time.Second)
		h.sched.Apply([]config.Check{a})
		time.Sleep(30 * time.Second)
		synctest.Wait()

		starts := h.startsOf("a")
		for i := 1; i < len(starts); i++ {
			if gap := starts[i] - starts[i-1]; gap != 10*time.Second {
				t.Errorf("probes %d and %d of the unchanged check are %v apart, want 10s", i-1, i, gap)
			}
		}
		h.stop()
	})
}

func TestApplyReplacesChangedCheckAfterItsProbeInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		old := check("a", 10*time.Second)
		h.sched.Apply([]config.Check{old})
		time.Sleep(10 * time.Second) // the first probe is now in flight
		synctest.Wait()

		changed := old
		changed.Timeout = 5 * time.Second
		h.sched.Apply([]config.Check{changed})
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(h.startsOf("a")); n != 1 {
			t.Fatalf("%d probes of a started while the old one was in flight, want only that one", n)
		}

		h.unhold("a")
		releasedAt := time.Now()
		close(release)
		time.Sleep(30 * time.Second)
		synctest.Wait()

		probes := h.probesOf("a")
		if len(probes) < 2 || probes[0].timeout != old.Timeout {
			t.Fatalf("probes = %+v, want the old config's probe then the new one's", probes)
		}
		for _, p := range probes[1:] {
			if p.timeout != changed.Timeout || p.start.Before(releasedAt) {
				t.Errorf("probe %+v: want the new config, started after the old probe finished", p)
			}
		}
		if got := h.recorded("a"); got != len(probes) {
			t.Errorf("%d results recorded for %d probes; the old in-flight probe must be recorded too", got, len(probes))
		}
		if got := h.maxInFlight("a"); got != 1 {
			t.Errorf("check a had %d probes in flight at once, want 1", got)
		}
		h.stop()
	})
}

func TestApplyRemovedCheckFinishesProbeInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		h.sched.Apply(nil)
		close(release)
		time.Sleep(time.Minute)
		synctest.Wait()

		if n := len(h.startsOf("a")); n != 1 {
			t.Errorf("%d probes of the removed check, want only the one in flight", n)
		}
		if n := h.recorded("a"); n != 1 {
			t.Errorf("%d results recorded, want the in-flight probe's", n)
		}
		h.stop()
	})
}

func TestReaddedCheckWaitsForItsProbeStillInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		h.sched.Apply(nil) // removed, but its probe is still in flight
		h.unhold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(time.Minute)
		synctest.Wait()
		if n := len(h.startsOf("a")); n != 1 {
			t.Fatalf("the re-added check started %d probes while its old one was in flight, want it to wait", n-1)
		}

		close(release)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if n := len(h.startsOf("a")); n < 2 {
			t.Errorf("the re-added check did not run after the old probe finished")
		}
		if got := h.maxInFlight("a"); got != 1 {
			t.Errorf("check a had %d probes in flight at once, want 1", got)
		}
		h.stop()
	})
}

func TestOnRemovedFiresOnceRemovedCheckHasStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second), check("b", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		h.sched.Apply([]config.Check{check("b", 20*time.Second)}) // remove a, change b
		synctest.Wait()
		if got := h.removals(); len(got) != 0 {
			t.Fatalf("OnRemoved(%v) while a's probe was still in flight", got)
		}
		close(release)
		synctest.Wait()
		if got := h.removals(); len(got) != 1 || got[0] != "a" {
			t.Fatalf("OnRemoved calls = %v, want [a]: once for the removed check, never for the changed one", got)
		}
		h.stop()
	})
}

func TestOnRemovedSkipsCheckReaddedWhileStopping(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		h.sched.Apply(nil)
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		close(release)
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := h.removals(); len(got) != 0 {
			t.Fatalf("OnRemoved(%v) for a check that was re-added", got)
		}
		h.stop()
	})
}

func TestStopWaitsForProbesInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		release := h.hold("a")
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		stopped := make(chan error, 1)
		go func() { stopped <- h.sched.Stop(context.Background()) }()
		time.Sleep(time.Minute)
		synctest.Wait()
		select {
		case err := <-stopped:
			t.Fatalf("Stop returned (%v) while a probe was in flight", err)
		default:
		}

		close(release)
		if err := <-stopped; err != nil {
			t.Fatalf("Stop = %v, want nil", err)
		}
		if n := h.recorded("a"); n != 1 {
			t.Errorf("%d results recorded, want the in-flight probe's", n)
		}
	})
}

func TestStopAbortsProbesAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		h.hold("a") // never released
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(10 * time.Second)
		synctest.Wait()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		if err := h.sched.Stop(ctx); err == nil {
			t.Fatal("Stop = nil, want an error for the aborted probe")
		}
		if took := time.Since(start); took != 5*time.Second {
			t.Errorf("Stop took %v, want the 5s deadline", took)
		}
		if n := h.recorded("a"); n != 0 {
			t.Errorf("%d results recorded for an aborted probe, want 0", n)
		}
	})
}

func TestApplyAfterStopDoesNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		h.stop()
		h.sched.Apply([]config.Check{check("a", 10*time.Second)})
		time.Sleep(time.Minute)
		synctest.Wait()

		if n := len(h.startsOf("a")); n != 0 {
			t.Errorf("%d probes ran after Stop, want 0", n)
		}
	})
}

func TestReloadsDoNotLeakGoroutines(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, 10)
		base := []config.Check{check("a", 10*time.Second), check("b", 10*time.Second), check("c", 10*time.Second)}
		h.sched.Apply(base)
		time.Sleep(time.Second)
		synctest.Wait()
		before := runtime.NumGoroutine()

		for i := range 100 {
			// Alternately change a, remove b and add d, then restore.
			next := []config.Check{check("a", time.Duration(11+i%2)*time.Second), base[2], check("d", 10*time.Second)}
			if i%2 == 1 {
				next = base
			}
			h.sched.Apply(next)
			time.Sleep(3 * time.Second)
		}
		h.sched.Apply(base)
		time.Sleep(time.Minute)
		synctest.Wait()

		if after := runtime.NumGoroutine(); after != before {
			t.Errorf("goroutines: %d before 100 reloads, %d after; want equal", before, after)
		}
		h.stop()
	})
}

// harness drives a Scheduler with fake probers that record when they ran.
type harness struct {
	t     *testing.T
	sched *Scheduler
	epoch time.Time

	mu            sync.Mutex
	probes        map[string][]probeRun
	results       map[string]int
	lags          map[string][]time.Duration // Start minus Scheduled of each result
	skips         map[string][]int
	removed       []string
	inFlight      map[string]int
	maxPerCheck   map[string]int
	total, maxAll int
	durations     map[string]time.Duration
	holds         map[string]chan struct{}
}

type probeRun struct {
	start   time.Time
	timeout time.Duration // identifies which version of a check ran
}

func newHarness(t *testing.T, maxConcurrent int) *harness {
	t.Helper()
	h := &harness{
		t: t, epoch: time.Now(),
		probes: make(map[string][]probeRun), results: make(map[string]int),
		lags: make(map[string][]time.Duration), skips: make(map[string][]int),
		inFlight: make(map[string]int), maxPerCheck: make(map[string]int),
		durations: make(map[string]time.Duration), holds: make(map[string]chan struct{}),
	}
	h.sched = New(h, Options{
		MaxConcurrent: maxConcurrent,
		NewProber:     h.newProber,
		OnSkip: func(check string, runs int) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.skips[check] = append(h.skips[check], runs)
		},
		OnRemoved: func(check string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.removed = append(h.removed, check)
		},
	})
	return h
}

func check(name string, interval time.Duration) config.Check {
	return config.Check{Name: name, Interval: interval, Timeout: interval, TCP: &config.TCP{Address: name + ":1"}}
}

func (h *harness) Record(r Result) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.results[r.Check]++
	h.lags[r.Check] = append(h.lags[r.Check], r.Start.Sub(r.Scheduled))
}

func (h *harness) lagsOf(check string) []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]time.Duration(nil), h.lags[check]...)
}

func (h *harness) skipsOf(check string) []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.skips[check]...)
}

func (h *harness) removals() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.removed...)
}

func (h *harness) newProber(c config.Check) probe.Prober {
	return proberFunc(func(ctx context.Context) probe.Result {
		start := time.Now()
		h.mu.Lock()
		h.probes[c.Name] = append(h.probes[c.Name], probeRun{start: start, timeout: c.Timeout})
		h.inFlight[c.Name]++
		h.maxPerCheck[c.Name] = max(h.maxPerCheck[c.Name], h.inFlight[c.Name])
		h.total++
		h.maxAll = max(h.maxAll, h.total)
		d, hold := h.durations[c.Name], h.holds[c.Name]
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			h.inFlight[c.Name]--
			h.total--
			h.mu.Unlock()
		}()

		select {
		case <-time.After(d):
		case <-ctx.Done():
			return probe.Result{Start: start, Reason: probe.ReasonCanceled}
		}
		if hold != nil {
			select {
			case <-hold:
			case <-ctx.Done():
				return probe.Result{Start: start, Reason: probe.ReasonCanceled}
			}
		}
		return probe.Result{Start: start, Duration: time.Since(start)}
	})
}

// setDuration makes every probe of check take d.
func (h *harness) setDuration(check string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.durations[check] = d
}

// hold makes probes of check block until the returned channel is closed.
func (h *harness) hold(check string) chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch := make(chan struct{})
	h.holds[check] = ch
	return ch
}

// unhold lets probes of check that start from now on return at once.
func (h *harness) unhold(check string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.holds, check)
}

func (h *harness) probesOf(check string) []probeRun {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]probeRun(nil), h.probes[check]...)
}

// startsOf returns when each probe of check started, relative to the start
// of the test.
func (h *harness) startsOf(check string) []time.Duration {
	var starts []time.Duration
	for _, p := range h.probesOf(check) {
		starts = append(starts, p.start.Sub(h.epoch))
	}
	return starts
}

func (h *harness) recorded(check string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.results[check]
}

func (h *harness) maxInFlight(check string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxPerCheck[check]
}

func (h *harness) maxInFlightTotal() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxAll
}

// stop stops the scheduler, failing the test if a probe had to be aborted.
func (h *harness) stop() {
	h.t.Helper()
	if err := h.sched.Stop(context.Background()); err != nil {
		h.t.Fatalf("Stop = %v", err)
	}
}

// proberFunc adapts a function to the probe.Prober interface.
type proberFunc func(context.Context) probe.Result

func (f proberFunc) Probe(ctx context.Context) probe.Result { return f(ctx) }
