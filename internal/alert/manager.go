package alert

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
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
}

// event is one thing for the evaluating goroutine to apply. Results and
// config changes share a queue so they are applied in the order they
// happened.
type event struct {
	result   *scheduler.Result
	isConfig bool
	checks   []config.Check // when isConfig
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
		events:     make(chan event, opts.QueueSize),
		wake:       make(chan struct{}, 1),
		stopEval:   make(chan struct{}),
		evalDone:   make(chan struct{}),
		draining:   make(chan struct{}),
		sendDone:   make(chan struct{}),
		states:     states,
		thresholds: make(map[string]Thresholds),
	}
	m.setChecks(checks)

	// Both loops outlive the caller's context: Close decides when they end.
	bg := context.WithoutCancel(ctx)
	for name, s := range states {
		if _, configured := m.thresholds[name]; !configured {
			m.forget(bg, name)
		} else if s.Firing {
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

// Forget drops a removed check's state once its last probe has been
// recorded, sending a removal notice if its alert was firing.
func (m *Manager) Forget(check string) {
	m.enqueue(event{forget: check})
}

// enqueue queues a config change, waiting for room: unlike results, these
// must not be dropped. It gives up once evaluation has stopped.
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
	default:
		m.forget(ctx, e.forget)
	}
}

func (m *Manager) evaluateResult(ctx context.Context, r scheduler.Result) {
	t, ok := m.thresholds[r.Check]
	if !ok {
		return // the check has been forgotten
	}
	prev := m.states[r.Check]
	next, n := Next(prev, r, t)
	if n == nil && next.Equal(prev) {
		return
	}
	if err := m.store.SaveAlertState(ctx, r.Check, next, n); err != nil {
		// The in-memory state stays as it was, so the next result for this
		// check makes the same transition and tries again.
		m.logger.Error("saving alert state", "check", r.Check, "err", err)
		return
	}
	m.setState(r.Check, prev, next)
	if n != nil {
		m.wakeSender()
	}
}

func (m *Manager) setChecks(checks []config.Check) {
	for _, c := range checks {
		m.thresholds[c.Name] = Thresholds{Failures: c.FailureThreshold, Successes: c.SuccessThreshold}
	}
}

func (m *Manager) forget(ctx context.Context, check string) {
	delete(m.thresholds, check)
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
		m.observer.Firing(check, next.Firing)
	}
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
