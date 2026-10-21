package alert

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

// All Manager tests run in synctest bubbles, so retry backoff passes in
// simulated time and synctest.Wait returns once both loops are idle.

func TestManagerAlertsOnceAndRecoversOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier, obs := newMemStore(), &fakeNotifier{}, &recorder{}
		m := start(t, store, notifier, obs, check("web"))
		feed(m, "web", "SFFFFFFSS")
		synctest.Wait()

		if got := notifier.kinds(); got != "alert,recovery" {
			t.Errorf("delivered %s, want alert,recovery", got)
		}
		if got := obs.firingChanges(); got != "web:true,web:false" {
			t.Errorf("firing changes %s, want web:true,web:false", got)
		}
		if len(store.pending()) != 0 || len(store.states) != 0 {
			t.Errorf("after recovery: %d pending, %d states; want an empty outbox and no state", len(store.pending()), len(store.states))
		}
		closeManager(t, m)
	})
}

func TestManagerRetriesUntilDelivered(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, obs := newMemStore(), &recorder{}
		notifier := &fakeNotifier{fail: func(_ Notification, attempt int) error {
			if attempt <= 3 {
				return errors.New("connection refused")
			}
			return nil
		}}
		m := start(t, store, notifier, obs, check("web"))
		feed(m, "web", "FFF")
		synctest.Wait()
		if got := notifier.kinds(); got != "" {
			t.Fatalf("delivered %q before any retry was due", got)
		}

		time.Sleep(1*time.Second + 2*time.Second + 4*time.Second) // the first three backoffs
		synctest.Wait()
		if got := notifier.kinds(); got != "alert" || notifier.attempts != 4 {
			t.Errorf("delivered %q after %d attempts, want the alert once on the 4th", got, notifier.attempts)
		}
		if obs.retryable != 3 {
			t.Errorf("observer saw %d retryable failures, want 3", obs.retryable)
		}
		closeManager(t, m)
	})
}

func TestManagerKeepsOrderWhileNotifierIsDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		var down sync.Mutex
		isDown := true
		notifier := &fakeNotifier{fail: func(Notification, int) error {
			down.Lock()
			defer down.Unlock()
			if isDown {
				return errors.New("503 Service Unavailable")
			}
			return nil
		}}
		m := start(t, store, notifier, &recorder{}, check("web"))
		feed(m, "web", "FFFSS")
		time.Sleep(time.Minute)
		synctest.Wait()
		if len(store.pending()) != 2 {
			t.Fatalf("%d notifications pending while the notifier is down, want 2", len(store.pending()))
		}

		down.Lock()
		isDown = false
		down.Unlock()
		time.Sleep(maxBackoff)
		synctest.Wait()
		if got := notifier.kinds(); got != "alert,recovery" {
			t.Errorf("delivered %s, want alert then recovery", got)
		}
		closeManager(t, m)
	})
}

func TestManagerGivesUpOnPermanentRejection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, obs := newMemStore(), &recorder{}
		notifier := &fakeNotifier{fail: func(n Notification, _ int) error {
			if n.Kind == KindAlert {
				return Permanent(errors.New("401 Unauthorized"))
			}
			return nil
		}}
		m := start(t, store, notifier, obs, check("web"))
		feed(m, "web", "FFFSS")
		synctest.Wait()

		if got := notifier.kinds(); got != "recovery" || notifier.attempts != 2 {
			t.Errorf("delivered %q in %d attempts; want the rejected alert tried once and the recovery still sent", got, notifier.attempts)
		}
		if obs.permanent != 1 || len(store.pending()) != 0 {
			t.Errorf("%d permanent failures, %d pending; want 1 and 0", obs.permanent, len(store.pending()))
		}
		closeManager(t, m)
	})
}

func TestManagerRestartDuringIncidentDoesNotAlertAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier := newMemStore(), &fakeNotifier{}
		m := start(t, store, notifier, &recorder{}, check("web"))
		feed(m, "web", "FFF")
		synctest.Wait()
		closeManager(t, m)

		// A new process, with the target still down.
		obs := &recorder{}
		m = start(t, store, notifier, obs, check("web"))
		if got := obs.firingChanges(); got != "web:true" {
			t.Errorf("after restart, firing changes %q; want the saved alert reported as firing", got)
		}
		feed(m, "web", "FFFFFFSS")
		synctest.Wait()
		if got := notifier.kinds(); got != "alert,recovery" {
			t.Errorf("delivered %s across the restart, want one alert and one recovery", got)
		}
		closeManager(t, m)
	})
}

func TestManagerDeliversLeftoversAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newMemStore()
		unreachable := &fakeNotifier{fail: func(Notification, int) error { return errors.New("no route to host") }}
		m := start(t, store, unreachable, &recorder{}, check("web"))
		feed(m, "web", "FFFSS")
		synctest.Wait()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.Close(ctx); err == nil {
			t.Error("Close succeeded with a notification undelivered, want an error")
		}
		if len(store.pending()) != 2 {
			t.Fatalf("%d notifications saved for the next start, want 2", len(store.pending()))
		}

		working := &fakeNotifier{}
		m = start(t, store, working, &recorder{}, check("web"))
		synctest.Wait()
		if got := working.kinds(); got != "alert,recovery" {
			t.Errorf("delivered %q after restart, want the leftovers oldest first: alert,recovery", got)
		}
		closeManager(t, m)
	})
}

func TestManagerCloseDeliversNotificationsFromLastResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier := newMemStore(), &fakeNotifier{}
		// Hold the sender inside its read of the (still empty) outbox, so
		// the alert is saved and Close begins while it is busy.
		store.gate = make(chan struct{})
		m := start(t, store, notifier, &recorder{}, check("web"))
		for range 3 {
			m.Record(scheduler.Result{Check: "web", Start: time.Now(),
				Reason: probe.ReasonConnect, Err: errors.New("connection refused")})
		}
		closed := make(chan error, 1)
		go func() { closed <- m.Close(context.Background()) }()
		synctest.Wait()
		close(store.gate)

		if err := <-closed; err != nil {
			t.Fatal(err)
		}
		if got := notifier.kinds(); got != "alert" {
			t.Errorf("delivered %q by the time Close returned, want the alert raised by the last results", got)
		}
	})
}

func TestManagerResolvesRemovedChecks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier := newMemStore(), &fakeNotifier{}
		m := start(t, store, notifier, &recorder{}, check("web"), check("api"))
		feed(m, "web", "FFF")
		feed(m, "api", "FF") // failing, but below its threshold
		m.Forget("web")
		m.Forget("api")
		synctest.Wait()

		if got := notifier.kinds(); got != "alert,removed" {
			t.Errorf("delivered %s, want the alert then its removal notice, and nothing for api", got)
		}
		if len(store.states) != 0 {
			t.Errorf("states left for removed checks: %v", slices.Collect(maps.Keys(store.states)))
		}
		closeManager(t, m)
	})
}

func TestManagerResolvesChecksRemovedWhileDown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier := newMemStore(), &fakeNotifier{}
		store.states["old"] = State{Firing: true, FiringSince: time.Now().Add(-time.Hour)}
		m := start(t, store, notifier, &recorder{}, check("web"))
		synctest.Wait()

		if got := notifier.kinds(); got != "removed" || notifier.delivered[0].Check != "old" {
			t.Errorf("delivered %s, want a removal notice for old", got)
		}
		closeManager(t, m)
	})
}

func TestManagerRetriesFailedSave(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, notifier := newMemStore(), &fakeNotifier{}
		m := start(t, store, notifier, &recorder{}, check("web"))
		feed(m, "web", "FF")
		synctest.Wait()
		store.mu.Lock()
		store.failSaves = 1 // the save that would raise the alert fails
		store.mu.Unlock()
		feed(m, "web", "F")
		synctest.Wait()
		if got := notifier.kinds(); got != "" {
			t.Fatalf("delivered %q although the state change was not saved", got)
		}

		feed(m, "web", "F")
		synctest.Wait()
		if got := notifier.kinds(); got != "alert" {
			t.Errorf("delivered %q, want the alert once the next failure's save succeeds", got)
		}
		closeManager(t, m)
	})
}

func TestManagerDropsResultsWhenQueueIsFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, obs := newMemStore(), &recorder{}
		store.block = make(chan struct{})
		m, err := New(t.Context(), store, []config.Check{check("web")},
			Options{Notifier: &fakeNotifier{}, Observer: obs, QueueSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		feed(m, "web", "F") // its save blocks the evaluator
		synctest.Wait()
		feed(m, "web", "FFF") // one fits in the queue
		if obs.dropped != 2 {
			t.Errorf("dropped %d results, want 2", obs.dropped)
		}
		close(store.block)
		closeManager(t, m)
	})
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := backoff(i + 1); got != w {
			t.Errorf("backoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	if got := backoff(1000); got != maxBackoff {
		t.Errorf("backoff(1000) = %v, want the cap %v", got, maxBackoff)
	}
}

func check(name string) config.Check {
	return config.Check{Name: name, FailureThreshold: 3, SuccessThreshold: 2}
}

func start(t *testing.T, store Store, n Notifier, obs Observer, checks ...config.Check) *Manager {
	t.Helper()
	m, err := New(t.Context(), store, checks, Options{Notifier: n, Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func closeManager(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

// feed records a result for each character of seq: 'F' failed, 'S'
// succeeded, one second apart.
func feed(m *Manager, check, seq string) {
	for _, c := range seq {
		r := scheduler.Result{Check: check, Start: time.Now()}
		if c == 'F' {
			r.Reason, r.Err = probe.ReasonConnect, errors.New("connection refused")
		}
		m.Record(r)
		time.Sleep(time.Second)
	}
}

// memStore is an in-memory Store.
type memStore struct {
	mu        sync.Mutex
	states    map[string]State
	outbox    []Notification
	nextID    int64
	failSaves int           // fail this many saves
	block     chan struct{} // if set, saves wait for it to close
	gate      chan struct{} // if set, reads of the outbox return only once it closes, with what they saw before
}

func newMemStore() *memStore { return &memStore{states: make(map[string]State)} }

func (s *memStore) AlertStates(context.Context) (map[string]State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.states), nil
}

func (s *memStore) SaveAlertState(_ context.Context, check string, st State, n *Notification) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSaves > 0 {
		s.failSaves--
		return errors.New("disk I/O error")
	}
	if st.IsZero() {
		delete(s.states, check)
	} else {
		s.states[check] = st
	}
	if n != nil {
		s.nextID++
		saved := *n
		saved.ID = s.nextID
		s.outbox = append(s.outbox, saved)
	}
	return nil
}

func (s *memStore) PendingNotifications(context.Context) ([]Notification, error) {
	seen := s.pending()
	if s.gate != nil {
		<-s.gate
	}
	return seen, nil
}

func (s *memStore) pending() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outbox)
}

func (s *memStore) DeleteNotification(_ context.Context, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outbox = slices.DeleteFunc(s.outbox, func(n Notification) bool { return n.ID == id })
	return nil
}

// fakeNotifier records deliveries; fail, if set, decides each attempt.
type fakeNotifier struct {
	mu        sync.Mutex
	fail      func(n Notification, attempt int) error
	attempts  int
	tries     map[int64]int
	delivered []Notification
}

func (f *fakeNotifier) Notify(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.tries == nil {
		f.tries = make(map[int64]int)
	}
	f.tries[n.ID]++
	if f.fail != nil {
		if err := f.fail(n, f.tries[n.ID]); err != nil {
			return err
		}
	}
	f.delivered = append(f.delivered, n)
	return nil
}

func (f *fakeNotifier) kinds() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var kinds []string
	for _, n := range f.delivered {
		kinds = append(kinds, string(n.Kind))
	}
	return strings.Join(kinds, ",")
}

// recorder is an Observer that keeps what it is told.
type recorder struct {
	mu                   sync.Mutex
	firing               []string
	retryable, permanent int
	dropped              int
}

func (r *recorder) Firing(check string, firing bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.firing = append(r.firing, fmt.Sprintf("%s:%v", check, firing))
}

func (r *recorder) Delivered(Kind) {}

func (r *recorder) DeliveryFailed(_ Kind, permanent bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if permanent {
		r.permanent++
	} else {
		r.retryable++
	}
}

func (r *recorder) Pending(int) {}

func (r *recorder) DroppedResult() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropped++
}

func (r *recorder) firingChanges() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.firing, ",")
}
