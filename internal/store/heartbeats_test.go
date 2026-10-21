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
