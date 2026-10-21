package store

import (
	"context"
	"time"
)

// Heartbeats returns when each heartbeat check was last pinged.
func (s *Store) Heartbeats(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT check_name, last_ping FROM heartbeats`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	pings := make(map[string]time.Time)
	for rows.Next() {
		var check string
		var ms int64
		if err := rows.Scan(&check, &ms); err != nil {
			return nil, err
		}
		pings[check] = fromMillis(ms)
	}
	return pings, rows.Err()
}

// SaveHeartbeat records that check was pinged at at. Pings arrive at most a
// few times a minute per check, so each is written straight away rather
// than through the batched results queue.
func (s *Store) SaveHeartbeat(ctx context.Context, check string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeats (check_name, last_ping) VALUES (?, ?)
		ON CONFLICT (check_name) DO UPDATE SET last_ping = excluded.last_ping`, check, toMillis(at))
	return err
}

// DeleteHeartbeat forgets a check's pings, once the check is removed.
func (s *Store) DeleteHeartbeat(ctx context.Context, check string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM heartbeats WHERE check_name = ?`, check)
	return err
}
