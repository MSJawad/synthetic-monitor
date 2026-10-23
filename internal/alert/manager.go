package alert

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

const (
	// sendTimeout bounds one delivery attempt.
	sendTimeout = 10 * time.Second
	// Retries of a failed delivery wait minBackoff, doubling up to
	// maxBackoff.
	minBackoff = time.Second
	maxBackoff = 5 * time.Minute
)

// Store persists alert state and the outbox of notifications to deliver.
type Store interface {
	AlertStates(ctx context.Context) (map[string]State, error)
	// SaveAlertState stores a check's state, removing it if healthy, and
	// adds n, if not nil, to the outbox, in one transaction.
	SaveAlertState(ctx context.Context, check string, s State, n *Notification) error
	// PendingNotifications returns undelivered notifications, oldest first.
	PendingNotifications(ctx context.Context) ([]Notification, error)
	DeleteNotification(ctx context.Context, id int64) error
}

// Observer is told about alerting activity, for metrics.
type Observer interface {
	Firing(check string, firing bool)
	Delivered(kind Kind)
	DeliveryFailed(kind Kind, permanent bool)
	Pending(n int)
	DroppedResult()
}

type nopObserver struct{}

func (nopObserver) Firing(string, bool)       {}
func (nopObserver) Delivered(Kind)            {}
func (nopObserver) DeliveryFailed(Kind, bool) {}
func (nopObserver) Pending(int)               {}
func (nopObserver) DroppedResult()            {}

// Options configures a Manager.
type Options struct {
	Notifier Notifier
	// Observer, if set, receives alerting activity.
	Observer Observer
	// Logger receives deliveries and failures. Nil discards them.
	Logger *slog.Logger
	// QueueSize bounds results waiting to be evaluated. Results arriving
	// while it is full are dropped and counted. Defaults to 1024.
	QueueSize int
}

// Manager turns probe results into alert state and delivers the
// notifications that state changes call for.
//
// Evaluation runs on one goroutine, so each check's results are applied in
// the order they were recorded. A state change and its notification are
// saved together; delivery runs on a second goroutine that works through
// the saved notifications strictly in order, retrying each until the
// notifier accepts it, and deletes it only then. So a notification is never
// lost, even across restarts, though it can be delivered twice if the
// process dies between delivering it and deleting it.
type Manager struct {
	store    Store
	notifier Notifier
	observer Observer
	logger   *slog.Logger

	events     chan event
	wake       chan struct{} // holds a token when new notifications may be waiting
	stopEval   chan struct{}
	evalDone   chan struct{}
	draining   chan struct{} // closed once evaluation has stopped
	sendDone   chan struct{}
	cancelSend context.CancelFunc
	closeOnce  sync.Once
	closeErr   error

	// Owned by the evaluating goroutine once it starts.
	states     map[string]State
	thresholds map[string]Thresholds
	// hasSLO records which checks have an SLO, and so are burn-evaluated.
	hasSLO map[string]bool
	// lastFailure is each check's most recent failure, for a burn alert
	// raised when the check's state no longer holds one.
	lastFailure map[string]failure

	// firing mirrors which checks are firing, and since when, for readers
	// on other goroutines.
	firingMu sync.Mutex
	firing   map[string]time.Time
}

type failure struct {
	reason probe.Reason
	err    string
}

// event is one thing for the evaluating goroutine to apply. Results, burn
// evaluations and config changes share a queue so they are applied in the
// order they happened.
type event struct {
	result   *scheduler.Result
	isConfig bool
	checks   []config.Check // when isConfig
	isBurns  bool
	burns    map[string]*Burn // when isBurns
	at       time.Time        // when isBurns
	forget   string
}

// New loads saved alert state and starts evaluating and delivering. checks
// is the current config; saved state of checks no longer in it is resolved
// as removed, since they were taken out while the service was down.
// Notifications left undelivered by the last run are delivered first.
func New(ctx context.Context, store Store, checks []config.Check, opts Options) (*Manager, error) {
	states, err := store.AlertStates(ctx)
	if err != nil {
		return nil, fmt.Errorf("loading alert state: %w", err)
	}
	if opts.Observer == nil {
		opts.Observer = nopObserver{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 1024
	}
	m := &Manager{
		store: store, notifier: opts.Notifier, observer: opts.Observer, logger: opts.Logger,
		events:      make(chan event, opts.QueueSize),
		wake:        make(chan struct{}, 1),
		stopEval:    make(chan struct{}),
		evalDone:    make(chan struct{}),
		draining:    make(chan struct{}),
		sendDone:    make(chan struct{}),
		states:      states,
		thresholds:  make(map[string]Thresholds),
		hasSLO:      make(map[string]bool),
		lastFailure: make(map[string]failure),
		firing:      make(map[string]time.Time),
	}
	m.setChecks(checks)

	// Both loops outlive the caller's context: Close decides when they end.
	bg := context.WithoutCancel(ctx)
	for name, s := range states {
		if _, configured := m.thresholds[name]; !configured {
			m.forget(bg, name)
		} else if s.Firing {
			m.setFiring(name, s)
			m.observer.Firing(name, true)
		}
	}
	sendCtx, cancelSend := context.WithCancel(bg)
	m.cancelSend = cancelSend
	go m.evaluate(bg)
	go m.send(sendCtx)
	m.wakeSender()
	return m, nil
}

// Record queues a probe result for evaluation. It never blocks: if the
// queue is full, the result is dropped and counted.
func (m *Manager) Record(r scheduler.Result) {
	select {
	case m.events <- event{result: &r}:
	default:
		m.observer.DroppedResult()
	}
}

// SetChecks applies the thresholds of the current config. Call it before
// the scheduler starts probing new checks.
func (m *Manager) SetChecks(checks []config.Check) {
	m.enqueue(event{isConfig: true, checks: checks})
}

// SetBurns applies an SLO evaluation: for each check evaluated, the burn
// rule that fires, or nil if none does. A check with an SLO that is left
// out, as when its probes couldn't be counted, keeps its state.
func (m *Manager) SetBurns(burns map[string]*Burn) {
	m.enqueue(event{isBurns: true, burns: burns, at: time.Now()})
}

// Forget drops a removed check's state once its last probe has been
// recorded, sending a removal notice if its alert was firing.
func (m *Manager) Forget(check string) {
	m.enqueue(event{forget: check})
}

// enqueue queues a config change or burn evaluation, waiting for room:
// unlike results, these must not be dropped. It gives up once evaluation has stopped.
func (m *Manager) enqueue(e event) {
	select {
	case m.events <- e:
	case <-m.evalDone:
	}
}

// Close stops evaluation, after applying everything already queued, then
// keeps delivering pending notifications until none are left or ctx ends.
// Notifications not delivered by then stay saved for the next start.
func (m *Manager) Close(ctx context.Context) error {
	m.closeOnce.Do(func() {
		close(m.stopEval)
		<-m.evalDone
		close(m.draining)
		select {
		case <-m.sendDone:
		case <-ctx.Done():
			m.cancelSend()
			<-m.sendDone
			m.closeErr = fmt.Errorf("notifications left undelivered until the next start: %w", ctx.Err())
		}
		m.cancelSend()
	})
	return m.closeErr
}

func (m *Manager) evaluate(ctx context.Context) {
	defer close(m.evalDone)
	for {
		select {
		case e := <-m.events:
			m.apply(ctx, e)
		case <-m.stopEval:
			for {
				select {
				case e := <-m.events:
					m.apply(ctx, e)
				default:
					return
				}
			}
		}
	}
}

func (m *Manager) apply(ctx context.Context, e event) {
	switch {
	case e.result != nil:
		m.evaluateResult(ctx, *e.result)
	case e.isConfig:
		m.setChecks(e.checks)
	case e.isBurns:
		m.evaluateBurns(ctx, e.burns, e.at)
	default:
		m.forget(ctx, e.forget)
	}
}

func (m *Manager) evaluateResult(ctx context.Context, r scheduler.Result) {
	t, ok := m.thresholds[r.Check]
	if !ok {
		return // the check has been forgotten
	}
	if !r.OK() {
		m.lastFailure[r.Check] = failure{r.Reason, errorText(r)}
	}
	prev := m.states[r.Check]
	next, n := Next(prev, r, t)
	m.save(ctx, r.Check, prev, next, n)
}

func (m *Manager) evaluateBurns(ctx context.Context, burns map[string]*Burn, at time.Time) {
	for check, t := range m.thresholds {
		b, evaluated := burns[check]
		if !evaluated && m.hasSLO[check] {
			continue
		}
		prev := m.states[check]
		in := prev
		if b != nil && in.LastReason == "" {
			f := m.lastFailure[check]
			in.LastReason, in.LastError = f.reason, f.err
		}
		next, n := NextBurn(in, check, b, at, t)
		m.save(ctx, check, prev, next, n)
	}
}

// save stores a check's change of state and the notification it calls
// for, if anything changed.
func (m *Manager) save(ctx context.Context, check string, prev, next State, n *Notification) {
	if n == nil && next.Equal(prev) {
		return
	}
	if err := m.store.SaveAlertState(ctx, check, next, n); err != nil {
		// The in-memory state stays as it was, so the next result or
		// evaluation for this check makes the same transition and tries again.
		m.logger.Error("saving alert state", "check", check, "err", err)
		return
	}
	m.setState(check, prev, next)
	if n != nil {
		m.wakeSender()
	}
}

func (m *Manager) setChecks(checks []config.Check) {
	for _, c := range checks {
		m.thresholds[c.Name] = Thresholds{Failures: c.FailureThreshold, Successes: c.SuccessThreshold}
		m.hasSLO[c.Name] = c.SLO != nil
	}
}

func (m *Manager) forget(ctx context.Context, check string) {
	delete(m.thresholds, check)
	delete(m.hasSLO, check)
	delete(m.lastFailure, check)
	s, ok := m.states[check]
	if !ok {
		return
	}
	n := Removed(check, s, time.Now())
	if err := m.store.SaveAlertState(ctx, check, State{}, n); err != nil {
		m.logger.Error("removing alert state", "check", check, "err", err)
		return
	}
	delete(m.states, check)
	m.setFiring(check, State{})
	if n != nil {
		m.wakeSender()
	}
}

func (m *Manager) setState(check string, prev, next State) {
	if next.IsZero() {
		delete(m.states, check)
	} else {
		m.states[check] = next
	}
	if prev.Firing != next.Firing {
		m.setFiring(check, next)
		m.observer.Firing(check, next.Firing)
	}
}

func (m *Manager) setFiring(check string, s State) {
	m.firingMu.Lock()
	defer m.firingMu.Unlock()
	if s.Firing {
		m.firing[check] = s.FiringSince
	} else {
		delete(m.firing, check)
	}
}

// Firing returns when the failures behind each firing alert began, keyed by
// check. It is safe to call from any goroutine.
func (m *Manager) Firing() map[string]time.Time {
	m.firingMu.Lock()
	defer m.firingMu.Unlock()
	return maps.Clone(m.firing)
}

func (m *Manager) wakeSender() {
	select {
	case m.wake <- struct{}{}:
	default: // already woken
	}
}

// send delivers notifications from the outbox, oldest first, until ctx is
// cancelled or, once evaluation has stopped, the outbox is empty.
func (m *Manager) send(ctx context.Context) {
	defer close(m.sendDone)
	draining, finishing := m.draining, false
	for {
		pending, err := m.store.PendingNotifications(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			m.logger.Error("reading pending notifications", "err", err)
			if !sleep(ctx, minBackoff) {
				return
			}
			continue
		}
		m.observer.Pending(len(pending))
		if len(pending) == 0 {
			if finishing {
				return
			}
			select {
			case <-m.wake:
			case <-draining:
				// Look once more: the last results may have queued some.
				draining, finishing = nil, true
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, n := range pending {
			if !m.deliver(ctx, n) {
				return
			}
		}
	}
}

// deliver sends n, retrying with backoff until the notifier accepts or
// permanently rejects it, then removes it from the outbox. It returns false
// if ctx ended first, leaving n in the outbox.
func (m *Manager) deliver(ctx context.Context, n Notification) bool {
	for attempt := 1; ; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		err := m.notifier.Notify(attemptCtx, n)
		cancel()
		if err != nil && !IsPermanent(err) {
			if ctx.Err() != nil {
				return false
			}
			m.observer.DeliveryFailed(n.Kind, false)
			delay := backoff(attempt)
			m.logger.Warn("notification not delivered, will retry", "kind", n.Kind, "check", n.Check,
				"attempt", attempt, "retry_in", delay.String(), "err", err)
			if !sleep(ctx, delay) {
				return false
			}
			continue
		}

		if err != nil {
			m.observer.DeliveryFailed(n.Kind, true)
			m.logger.Error("notification rejected, giving up on it", "kind", n.Kind, "check", n.Check, "err", err)
		} else {
			m.observer.Delivered(n.Kind)
			m.logger.Info("notification delivered", "kind", n.Kind, "check", n.Check, "attempts", attempt)
		}
		if err := m.store.DeleteNotification(ctx, n.ID); err != nil {
			m.logger.Error("removing a delivered notification from the outbox; it may be sent again",
				"kind", n.Kind, "check", n.Check, "err", err)
		}
		return true
	}
}

// backoff returns how long to wait before retry number attempt: one
// second, doubling each time, at most five minutes.
func backoff(attempt int) time.Duration {
	return min(minBackoff<<min(attempt-1, 16), maxBackoff)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
