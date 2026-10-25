package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHeartbeatsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := open(t, path, Options{})
	for _, ping := range []struct {
		check string
		at    time.Time
	}{
		{"cron", since},
		{"cron", since.Add(time.Hour)}, // replaces the first
		{"gone", since},
	} {
		if err := s.SaveHeartbeat(t.Context(), ping.check, ping.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteHeartbeat(t.Context(), "gone"); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)

	s = open(t, path, Options{})
	defer closeStore(t, s)
	pings, err := s.Heartbeats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pings) != 1 || !pings["cron"].Equal(since.Add(time.Hour)) {
		t.Errorf("heartbeats = %v, want only cron's latest ping", pings)
	}
}

func TestHeartbeatRegistrationsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := open(t, path, Options{})
	for _, reg := range []struct {
		check string
		at    time.Time
	}{
		{"cron", since},
		{"cron", since.Add(time.Hour)}, // known already: kept as it was
		{"gone", since},
	} {
		if err := s.RegisterHeartbeat(t.Context(), reg.check, reg.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveHeartbeat(t.Context(), "cron", since.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHeartbeat(t.Context(), "gone"); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)

	s = open(t, path, Options{})
	defer closeStore(t, s)
	registered, err := s.HeartbeatRegistrations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 1 || !registered["cron"].Equal(since) {
		t.Errorf("registrations = %v, want only cron's first", registered)
	}
	pings, err := s.Heartbeats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pings) != 1 || !pings["cron"].Equal(since.Add(2*time.Hour)) {
		t.Errorf("heartbeats = %v, want cron's ping and no other", pings)
	}
}

// TestUpgradeKeepsHeartbeats opens a database written before heartbeat
// registrations were kept, holding a ping.
func TestUpgradeKeepsHeartbeats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	oldDatabase(t, path, 6, `INSERT INTO heartbeats VALUES ('cron', ?)`, since.UnixMilli())

	s := open(t, path, Options{})
	defer closeStore(t, s)
	pings, err := s.Heartbeats(t.Context())
	if err != nil || len(pings) != 1 || !pings["cron"].Equal(since) {
		t.Errorf("heartbeats = %v, %v; want cron's ping", pings, err)
	}
	if err := s.RegisterHeartbeat(t.Context(), "cron", since.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if registered, err := s.HeartbeatRegistrations(t.Context()); err != nil || !registered["cron"].Equal(since.Add(time.Hour)) {
		t.Errorf("registrations = %v, %v; want cron registered once the upgrade made room for it", registered, err)
	}
}
