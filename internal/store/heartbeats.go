package store

import (
	"context"
	"time"
)

// Heartbeats returns when each heartbeat check was last pinged.
func (s *Store) Heartbeats(ctx context.Context) (map[string]time.Time, error) {
	return s.heartbeatTimes(ctx, `SELECT check_name, last_ping FROM heartbeats WHERE last_ping != 0`)
}

// HeartbeatRegistrations returns when each heartbeat check was first
// configured, where that is known.
func (s *Store) HeartbeatRegistrations(ctx context.Context) (map[string]time.Time, error) {
	return s.heartbeatTimes(ctx, `SELECT check_name, registered_at FROM heartbeats WHERE registered_at != 0`)
}

// heartbeatTimes returns the times query selects, keyed by check.
func (s *Store) heartbeatTimes(ctx context.Context, query string) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	times := make(map[string]time.Time)
	for rows.Next() {
		var check string
		var ms int64
		if err := rows.Scan(&check, &ms); err != nil {
			return nil, err
		}
		times[check] = fromMillis(ms)
	}
	return times, rows.Err()
}

// RegisterHeartbeat records that check was first configured at at, unless
// that is known already.
func (s *Store) RegisterHeartbeat(ctx context.Context, check string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeats (check_name, last_ping, registered_at) VALUES (?, 0, ?)
		ON CONFLICT (check_name) DO UPDATE SET registered_at = excluded.registered_at WHERE registered_at = 0`,
		check, toMillis(at))
	return err
}

// SaveHeartbeat records that check was pinged at at. Pings arrive at most a
// few times a minute per check, so each is written straight away rather
// than through the batched results queue.
func (s *Store) SaveHeartbeat(ctx context.Context, check string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO heartbeats (check_name, last_ping) VALUES (?, ?)
		ON CONFLICT (check_name) DO UPDATE SET last_ping = excluded.last_ping`, check, toMillis(at))
	return err
}

// DeleteHeartbeat forgets a check's pings and registration, once the check
// is removed.
func (s *Store) DeleteHeartbeat(ctx context.Context, check string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM heartbeats WHERE check_name = ?`, check)
	return err
}
