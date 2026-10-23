package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

// Incident is one stored incident: a check's alert and its recovery.
type Incident struct {
	Check string
	Start time.Time // when the failures began
	// End is when probes began succeeding again, or when the check was
	// removed; zero while the incident is open.
	End time.Time
	// Burn is true if an error budget burn rule raised the incident, which
	// happens when probes fail on and off, rather than failures in a row.
	Burn   bool
	Reason probe.Reason // the latest failure's reason when it was raised
}

// Incidents returns the incidents that are open or ended at or after
// since, newest first.
func (s *Store) Incidents(ctx context.Context, since time.Time) ([]Incident, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_name, started_at, ended_at, burn, reason FROM incidents
		WHERE ended_at IS NULL OR ended_at >= ? ORDER BY started_at DESC, rowid DESC`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []Incident
	for rows.Next() {
		var (
			in      Incident
			startMs int64
			endMs   sql.NullInt64
			reason  string
		)
		if err := rows.Scan(&in.Check, &startMs, &endMs, &in.Burn, &reason); err != nil {
			return nil, err
		}
		in.Start, in.End, in.Reason = fromMillis(startMs), fromMillis(endMs.Int64), probe.Reason(reason)
		incidents = append(incidents, in)
	}
	return incidents, rows.Err()
}

// recordIncident opens an incident for check on an alert, and closes its
// open incident on a recovery or removal notice.
func recordIncident(ctx context.Context, tx *sql.Tx, check string, n *alert.Notification) error {
	var err error
	switch n.Kind {
	case alert.KindAlert:
		_, err = tx.ExecContext(ctx, `INSERT INTO incidents (check_name, started_at, burn, reason) VALUES (?, ?, ?, ?)`,
			check, n.Since.UnixMilli(), n.Burn != nil, string(n.Reason))
	case alert.KindRecovery, alert.KindRemoved:
		// A removal notice has no Until: the incident ends with the removal.
		end := n.Until
		if end.IsZero() {
			end = n.At
		}
		_, err = tx.ExecContext(ctx, `UPDATE incidents SET ended_at = ? WHERE check_name = ? AND ended_at IS NULL`,
			end.UnixMilli(), check)
	}
	return err
}
