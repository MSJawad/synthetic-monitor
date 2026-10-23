package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

// AlertStates returns the alerting state of every check that is not
// healthy.
func (s *Store) AlertStates(ctx context.Context) (map[string]alert.State, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_name, firing, burning, streak, streak_start, firing_since,
		last_reason, last_error FROM alert_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := make(map[string]alert.State)
	for rows.Next() {
		var (
			name                     string
			st                       alert.State
			streakStart, firingSince int64
			reason                   string
		)
		if err := rows.Scan(&name, &st.Firing, &st.Burning, &st.Streak, &streakStart, &firingSince, &reason, &st.LastError); err != nil {
			return nil, err
		}
		st.StreakStart, st.FiringSince = fromMillis(streakStart), fromMillis(firingSince)
		st.LastReason = probe.Reason(reason)
		states[name] = st
	}
	return states, rows.Err()
}

// SaveAlertState stores check's alerting state, deleting it if st is
// healthy, and adds n, if not nil, to the outbox. Both happen in one
// transaction, so a crash cannot record a state change without the
// notification it calls for, or the other way round.
func (s *Store) SaveAlertState(ctx context.Context, check string, st alert.State, n *alert.Notification) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	if st.IsZero() {
		_, err = tx.ExecContext(ctx, `DELETE FROM alert_state WHERE check_name = ?`, check)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO alert_state
			(check_name, firing, burning, streak, streak_start, firing_since, last_reason, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (check_name) DO UPDATE SET firing = excluded.firing, burning = excluded.burning,
				streak = excluded.streak, streak_start = excluded.streak_start, firing_since = excluded.firing_since,
				last_reason = excluded.last_reason, last_error = excluded.last_error`,
			check, st.Firing, st.Burning, st.Streak, toMillis(st.StreakStart), toMillis(st.FiringSince),
			string(st.LastReason), truncate(st.LastError, maxErrorLen))
	}
	if err != nil {
		return err
	}
	if n != nil {
		payload, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notifications (notification) VALUES (?)`, string(payload)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PendingNotifications returns the undelivered notifications, oldest first.
func (s *Store) PendingNotifications(ctx context.Context) ([]alert.Notification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, notification FROM notifications ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []alert.Notification
	for rows.Next() {
		var (
			id      int64
			payload string
			n       alert.Notification
		)
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payload), &n); err != nil {
			return nil, err
		}
		n.ID = id
		pending = append(pending, n)
	}
	return pending, rows.Err()
}

// DeleteNotification removes a delivered notification from the outbox.
func (s *Store) DeleteNotification(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notifications WHERE id = ?`, id)
	return err
}

func toMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
