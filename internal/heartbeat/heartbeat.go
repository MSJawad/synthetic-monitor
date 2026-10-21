// Package heartbeat receives pings from jobs the monitor cannot reach, such
// as scheduled jobs on a private network, and remembers when each heartbeat
// check was last pinged.
package heartbeat

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Store persists last pings, so a restart does not forget them.
type Store interface {
	Heartbeats(ctx context.Context) (map[string]time.Time, error)
	SaveHeartbeat(ctx context.Context, check string, at time.Time) error
	DeleteHeartbeat(ctx context.Context, check string) error
}

// Registry holds the last ping of each heartbeat check.
type Registry struct {
	store  Store
	onPing func(check string, at time.Time)
	mu     sync.Mutex
	last   map[string]time.Time
}

// New loads the pings saved in store. onPing, if not nil, is told of each
// of them and of every ping after, for metrics.
func New(ctx context.Context, store Store, onPing func(check string, at time.Time)) (*Registry, error) {
	last, err := store.Heartbeats(ctx)
	if err != nil {
		return nil, err
	}
	if onPing == nil {
		onPing = func(string, time.Time) {}
	}
	for check, at := range last {
		onPing(check, at)
	}
	return &Registry{store: store, onPing: onPing, last: last}, nil
}

// LastPing returns when check was last pinged, if ever.
func (r *Registry) LastPing(check string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.last[check]
	return at, ok
}

// Ping records that check was pinged at at. The ping is saved before it
// counts, so one that was acknowledged is never lost to a restart. It is
// kept to the millisecond in UTC, as the store keeps it, so it reads the
// same before and after a restart.
func (r *Registry) Ping(ctx context.Context, check string, at time.Time) error {
	at = at.Truncate(time.Millisecond).UTC()
	if err := r.store.SaveHeartbeat(ctx, check, at); err != nil {
		return err
	}
	r.mu.Lock()
	r.last[check] = at
	r.mu.Unlock()
	r.onPing(check, at)
	return nil
}

// Forget removes a check's pings once the check is removed, so a check
// added later under the same name starts afresh.
func (r *Registry) Forget(ctx context.Context, check string) error {
	r.mu.Lock()
	delete(r.last, check)
	r.mu.Unlock()
	return r.store.DeleteHeartbeat(ctx, check)
}

// Handler serves POST /heartbeat/{check}, which records a ping of a
// configured heartbeat check. Callers authenticate with
// "Authorization: Bearer <token>"; the token is checked before the name, so
// a caller without it cannot learn which checks exist.
func Handler(r *Registry, token string, isHeartbeat func(check string) bool, logger *slog.Logger) http.Handler {
	want := []byte("Bearer " + token)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /heartbeat/{check}", func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "missing or wrong token", http.StatusUnauthorized)
			return
		}
		check := req.PathValue("check")
		if !isHeartbeat(check) {
			http.Error(w, "no heartbeat check has that name", http.StatusNotFound)
			return
		}
		if err := r.Ping(req.Context(), check, time.Now()); err != nil {
			logger.Error("saving a heartbeat", "check", check, "err", err)
			http.Error(w, "the ping could not be saved; retry", http.StatusServiceUnavailable)
			return
		}
		logger.Debug("heartbeat received", "check", check)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
