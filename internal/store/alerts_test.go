package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

var (
	since  = time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	firing = alert.State{Firing: true, Burning: true, FiringSince: since, LastReason: probe.ReasonTimeout, LastError: "no result within 10s"}
	notice = &alert.Notification{Kind: alert.KindAlert, Check: "web", At: since.Add(time.Minute), Since: since,
		Probes: 3, Reason: probe.ReasonTimeout, Error: "no result within 10s"}
)

func TestAlertStateAndOutboxSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := open(t, path, Options{})
	if err := s.SaveAlertState(t.Context(), "web", firing, notice); err != nil {
		t.Fatal(err)
	}
	closeStore(t, s)

	s = open(t, path, Options{})
	defer closeStore(t, s)
	states, err := s.AlertStates(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := states["web"]; len(states) != 1 || !got.Equal(firing) {
		t.Errorf("states = %+v, want web firing as saved", states)
	}
	pending, err := s.PendingNotifications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID == 0 || pending[0].Kind != alert.KindAlert ||
		!pending[0].Since.Equal(since) || pending[0].Error != notice.Error {
		t.Errorf("pending = %+v, want the saved alert with an ID", pending)
	}
}

func TestHealthyStateDeletesRow(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	if err := s.SaveAlertState(t.Context(), "web", firing, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAlertState(t.Context(), "web", alert.State{}, nil); err != nil {
		t.Fatal(err)
	}
	if states, err := s.AlertStates(t.Context()); err != nil || len(states) != 0 {
		t.Errorf("states = %+v (%v), want none once healthy", states, err)
	}
}

func TestStateIsNotSavedWithoutItsNotification(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	// Make adding to the outbox fail.
	if _, err := s.db.ExecContext(t.Context(), "DROP TABLE notifications"); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveAlertState(t.Context(), "web", firing, notice); err == nil {
		t.Fatal("SaveAlertState succeeded without an outbox")
	}
	if states, err := s.AlertStates(t.Context()); err != nil || len(states) != 0 {
		t.Errorf("states = %+v (%v): the state change was saved without its notification", states, err)
	}
}

func TestPendingNotificationsInOrder(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	for _, kind := range []alert.Kind{alert.KindAlert, alert.KindRecovery, alert.KindAlert} {
		n := *notice
		n.Kind = kind
		if err := s.SaveAlertState(t.Context(), "web", firing, &n); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := s.PendingNotifications(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 || pending[0].Kind != alert.KindAlert || pending[1].Kind != alert.KindRecovery {
		t.Fatalf("pending = %+v, want alert, recovery, alert in that order", pending)
	}

	if err := s.DeleteNotification(t.Context(), pending[0].ID); err != nil {
		t.Fatal(err)
	}
	if rest, _ := s.PendingNotifications(t.Context()); len(rest) != 2 || rest[0].ID != pending[1].ID {
		t.Errorf("after delivering the first: %+v, want the other two", rest)
	}
}

// TestUpgradeKeepsAlertState opens a database written before alert state
// had a burning column, as production's was, holding a firing alert.
func TestUpgradeKeepsAlertState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err := db.ExecContext(t.Context(), m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 3;
		INSERT INTO alert_state VALUES ('web', 1, 0, 0, ?, 'timeout', 'no result within 10s')`, since.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := open(t, path, Options{})
	defer closeStore(t, s)
	states, err := s.AlertStates(t.Context())
	want := alert.State{Firing: true, FiringSince: since, LastReason: probe.ReasonTimeout, LastError: "no result within 10s"}
	if err != nil || !states["web"].Equal(want) {
		t.Errorf("states = %+v, %v; want web still firing, not burning", states, err)
	}
}
