package store

import (
	"context"
	"time"
)

// Day is one check's probe totals for one day, from midnight UTC.
type Day struct {
	Start  time.Time
	Probes int
	Failed int
}

// Days returns check's daily totals from the day containing since,
// oldest first. Days without probes are left out.
func (s *Store) Days(ctx context.Context, check string, since time.Time) ([]Day, error) {
	y, m, d := since.UTC().Date()
	midnight := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	rows, err := s.db.QueryContext(ctx, `SELECT day, probes, failed FROM daily
		WHERE check_name = ? AND day >= ? ORDER BY day`, check, midnight.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []Day
	for rows.Next() {
		var (
			d     Day
			dayMs int64
		)
		if err := rows.Scan(&dayMs, &d.Probes, &d.Failed); err != nil {
			return nil, err
		}
		d.Start = fromMillis(dayMs)
		days = append(days, d)
	}
	return days, rows.Err()
}
