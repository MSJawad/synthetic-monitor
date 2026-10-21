// Package scheduler runs each check's probes on its interval, applies config
// changes without disturbing unchanged checks, and lets probes in flight
// finish when checks are removed or the service stops.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

// Result is the outcome of one probe of a check.
type Result struct {
	Check string
	Type  string
	// Scheduled is when the probe was due. It is earlier than Start when
	// the probe waited for a free slot.
	Scheduled time.Time
	probe.Result
}

// Sink receives every completed probe. Record is called on the probing
// goroutine, so it must return quickly.
type Sink interface {
	Record(Result)
}

// Options configures a Scheduler.
type Options struct {
	// MaxConcurrent caps the probes in flight at once across all checks.
	MaxConcurrent int
	// NewProber builds the prober for a check. Nil uses probe.New.
	NewProber func(config.Check) probe.Prober
	// Logger receives check changes and skipped runs. Nil discards them.
	Logger *slog.Logger
	// OnSkip, if set, is called when a check skips scheduled runs because
	// an earlier run overran them.
	OnSkip func(check string, runs int)
	// OnRemoved, if set, is called once a removed check has fully stopped,
	// including any probe it had in flight, so per-check state such as
	// metrics can be dropped. It must not call back into the Scheduler.
	OnRemoved func(check string)
}

// maxInitialSpread bounds how long after a start or reload a check's first
// probe can come, so every check reports within it even if its interval is
// an hour.
const maxInitialSpread = time.Minute

// Scheduler runs one goroutine per check. Each check's first probe comes at
// a random point within its interval (or within maxInitialSpread, if that is
// shorter), so checks do not fire in lockstep, and then keeps a fixed
// cadence. A check never runs two probes at once.
//
// Stopping is split in two. Stopping a check's schedule (when the check is
// removed or changed, or the scheduler stops) prevents new probes but lets
// one in flight finish and be recorded. Aborting cancels probes in flight;
// it happens only when Stop's deadline expires.
type Scheduler struct {
	sink      Sink
	newProber func(config.Check) probe.Prober
	logger    *slog.Logger
	onSkip    func(string, int)
	onRemoved func(string)
	slots     chan struct{} // one token per probe in flight

	// scheduling ends when Stop is called; probes run under probing, which
	// ends only if Stop's deadline expires.
	scheduling, probing          context.Context
	stopScheduling, abortProbing context.CancelFunc

	mu      sync.Mutex
	runners map[string]*runner
	// draining holds the latest stopped runner of each check whose
	// goroutine has not exited yet, so a replacement can wait for it.
	draining map[string]*runner
	stopped  bool
	wg       sync.WaitGroup // every runner goroutine, including stopped ones still finishing a probe
}

// runner is the goroutine that schedules one check.
type runner struct {
	check  config.Check
	prober probe.Prober
	stop   context.CancelFunc // stops scheduling this check
	done   chan struct{}      // closed once the goroutine has exited
}

// New returns a Scheduler that sends results to sink. Call Apply to start
// checks.
func New(sink Sink, opts Options) *Scheduler {
	if opts.NewProber == nil {
		opts.NewProber = func(c config.Check) probe.Prober { return probe.New(c, probe.Options{}) }
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := &Scheduler{
		sink:      sink,
		newProber: opts.NewProber,
		logger:    opts.Logger,
		onSkip:    opts.OnSkip,
		onRemoved: opts.OnRemoved,
		slots:     make(chan struct{}, max(opts.MaxConcurrent, 1)),
		runners:   make(map[string]*runner),
		draining:  make(map[string]*runner),
	}
	s.scheduling, s.stopScheduling = context.WithCancel(context.Background())
	s.probing, s.abortProbing = context.WithCancel(context.Background())
	return s
}

// Apply makes the running checks match checks. Checks are matched by name:
// unchanged ones keep running on their current schedule, removed ones stop
// after any probe in flight, and changed ones are replaced by a new runner
// that starts once the old one has finished. A check that is re-added while
// its removed runner is still finishing also waits for it. Apply does
// nothing after Stop.
func (s *Scheduler) Apply(checks []config.Check) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}

	want := make(map[string]config.Check, len(checks))
	for _, c := range checks {
		want[c.Name] = c
	}
	var added, updated, removed []string
	for name, r := range s.runners {
		c, keep := want[name]
		if keep && reflect.DeepEqual(c, r.check) {
			delete(want, name)
			continue
		}
		r.stop()
		delete(s.runners, name)
		s.draining[name] = r
		if keep {
			updated = append(updated, name)
		} else {
			removed = append(removed, name)
		}
	}
	for _, c := range checks {
		if _, start := want[c.Name]; !start {
			continue
		}
		if !slices.Contains(updated, c.Name) {
			added = append(added, c.Name)
		}
		var after <-chan struct{}
		if old, ok := s.draining[c.Name]; ok {
			after = old.done
		}
		s.runners[c.Name] = s.start(c, after)
	}
	attrs := []any{"unchanged", len(s.runners) - len(added) - len(updated)}
	for _, change := range []struct {
		key   string
		names []string
	}{{"added", added}, {"updated", updated}, {"removed", removed}} {
		if len(change.names) > 0 {
			attrs = append(attrs, change.key, change.names)
		}
	}
	s.logger.Info("checks applied", attrs...)
}

// Stop stops scheduling probes and waits for those in flight to finish. If
// ctx ends first, it aborts them, discards their results and returns an
// error once they have returned.
func (s *Scheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.stopScheduling()
	s.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(finished)
	}()
	defer s.abortProbing() // releases the context once every probe is done
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		s.abortProbing()
		<-finished
		return fmt.Errorf("aborted probes still in flight: %w", ctx.Err())
	}
}

// start launches a runner for c. If after is not nil, the runner waits for
// it to close before scheduling, so a stopped runner's last probe finishes
// before its replacement's first one starts.
//
// The runner's context derives from the scheduler's own, not a caller's:
// the scheduler's lifetime is governed by Stop, not by whoever applies a
// config.
//
//nolint:contextcheck // see above
func (s *Scheduler) start(c config.Check, after <-chan struct{}) *runner {
	ctx, stop := context.WithCancel(s.scheduling)
	r := &runner{check: c, prober: s.newProber(c), stop: stop, done: make(chan struct{})}
	s.wg.Go(func() {
		defer close(r.done)
		defer s.exited(r)
		defer stop()
		if after != nil {
			<-after
		}
		s.run(ctx, r)
	})
	return r
}

// exited does the bookkeeping for a runner whose goroutine is ending.
func (s *Scheduler) exited(r *runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := r.check.Name
	if s.draining[name] != r {
		return // still running at Stop, or a later runner of the check is draining
	}
	delete(s.draining, name)
	if _, active := s.runners[name]; !active && s.onRemoved != nil {
		s.onRemoved(name)
	}
}

// InFlight returns the number of probes currently running.
func (s *Scheduler) InFlight() int {
	return len(s.slots)
}

// run probes r's check on a fixed cadence until ctx ends.
func (s *Scheduler) run(ctx context.Context, r *runner) {
	interval := r.check.Interval
	next := time.Now().Add(rand.N(min(interval, maxInitialSpread))) //nolint:gosec // spreading load needs no unpredictability
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.runOnce(ctx, r, next)

		// The next run is one interval after this run's scheduled time,
		// not after it finished, so probe duration does not shift the
		// cadence. A run delayed long enough (waiting for a free slot) to
		// overrun later slots skips them rather than firing back to back.
		next = next.Add(interval)
		if now := time.Now(); !next.After(now) {
			missed := now.Sub(next)/interval + 1
			next = next.Add(missed * interval)
			s.logger.Warn("skipped scheduled probes because an earlier one overran", "check", r.check.Name, "skipped", int64(missed))
			if s.onSkip != nil {
				s.onSkip(r.check.Name, int(missed))
			}
		}
		timer.Reset(time.Until(next))
	}
}

// runOnce runs one probe once a slot is free, unless ctx ends first, and
// records the result.
func (s *Scheduler) runOnce(ctx context.Context, r *runner, scheduled time.Time) {
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return
	}
	defer func() { <-s.slots }()
	if ctx.Err() != nil {
		return // stopped while a slot was being acquired
	}

	// Deliberately not ctx: stopping a check's schedule must not cancel its
	// probe in flight. Only Stop's deadline aborts probes, via s.probing.
	res := r.prober.Probe(s.probing) //nolint:contextcheck // see above
	if res.Reason == probe.ReasonCanceled {
		return // aborted at shutdown; says nothing about the target
	}
	s.sink.Record(Result{Check: r.check.Name, Type: r.check.Type(), Scheduled: scheduled, Result: res})
}
