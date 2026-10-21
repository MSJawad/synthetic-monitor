package heartbeat

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// memStore keeps pings in memory and can be made to fail.
type memStore struct {
	mu    sync.Mutex
	pings map[string]time.Time
	fail  bool
}

func (s *memStore) Heartbeats(context.Context) (map[string]time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.pings), nil
}

func (s *memStore) SaveHeartbeat(_ context.Context, check string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk I/O error")
	}
	s.pings[check] = at
	return nil
}

func (s *memStore) DeleteHeartbeat(_ context.Context, check string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pings, check)
	return nil
}

var earlier = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

func TestRegistryLoadsSavedPings(t *testing.T) {
	store := &memStore{pings: map[string]time.Time{"cron": earlier}}
	seen := make(map[string]time.Time)
	r, err := New(t.Context(), store, func(check string, at time.Time) { seen[check] = at })
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := r.LastPing("cron"); !ok || !at.Equal(earlier) || !seen["cron"].Equal(earlier) {
		t.Errorf("LastPing = %v, %v; reported %v; want the saved ping, reported", at, ok, seen)
	}
	if err := r.Forget(t.Context(), "cron"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.LastPing("cron"); ok || len(store.pings) != 0 {
		t.Error("Forget kept the ping")
	}
}

// emptyStore has never saved a ping and returns a nil map, as a Store may.
type emptyStore struct{}

func (emptyStore) Heartbeats(context.Context) (map[string]time.Time, error) { return nil, nil }
func (emptyStore) SaveHeartbeat(context.Context, string, time.Time) error   { return nil }
func (emptyStore) DeleteHeartbeat(context.Context, string) error            { return nil }

func TestRegistryFromEmptyStore(t *testing.T) {
	r, err := New(t.Context(), emptyStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Ping(t.Context(), "cron", earlier); err != nil {
		t.Fatal(err)
	}
	if at, ok := r.LastPing("cron"); !ok || !at.Equal(earlier) {
		t.Errorf("LastPing = %v, %v; want the ping", at, ok)
	}
}

func TestHandler(t *testing.T) {
	store := &memStore{pings: map[string]time.Time{}}
	r, err := New(t.Context(), store, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := Handler(r, "s3cr3t", func(check string) bool { return check == "cron" }, slog.New(slog.DiscardHandler))
	ping := func(check, auth string) int {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/heartbeat/"+check, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, tt := range []struct {
		name, check, auth string
		want              int
	}{
		{"no token", "cron", "", http.StatusUnauthorized},
		{"wrong token", "cron", "Bearer guess", http.StatusUnauthorized},
		// The token is checked first, so an unknown name is not revealed.
		{"wrong token, unknown check", "other", "Bearer guess", http.StatusUnauthorized},
		{"unknown check", "other", "Bearer s3cr3t", http.StatusNotFound},
	} {
		if got := ping(tt.check, tt.auth); got != tt.want {
			t.Errorf("%s: status %d, want %d", tt.name, got, tt.want)
		}
	}
	if _, ok := r.LastPing("cron"); ok {
		t.Fatal("a rejected ping was recorded")
	}

	before := time.Now().Truncate(time.Millisecond)
	if got := ping("cron", "Bearer s3cr3t"); got != http.StatusNoContent {
		t.Fatalf("valid ping: status %d, want 204", got)
	}
	if at, ok := r.LastPing("cron"); !ok || at.Before(before) || !store.pings["cron"].Equal(at) {
		t.Errorf("after a valid ping: LastPing %v, %v, saved %v", at, ok, store.pings["cron"])
	}

	// A ping that cannot be saved is refused, so the job can retry.
	store.fail = true
	last, _ := r.LastPing("cron")
	if got := ping("cron", "Bearer s3cr3t"); got != http.StatusServiceUnavailable {
		t.Errorf("unsaveable ping: status %d, want 503", got)
	}
	if at, _ := r.LastPing("cron"); !at.Equal(last) {
		t.Error("an unsaved ping was counted")
	}
}
