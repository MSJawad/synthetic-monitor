// Package store keeps the history of probe results in SQLite, for the status
// page and SLO calculations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // registers the "sqlite" driver: pure Go, so no cgo

	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

const (
	// maxBatch caps the results written in one transaction.
	maxBatch = 500
	// maxErrorLen caps the stored error text of a failed probe.
	maxErrorLen = 500
	// pruneEvery is how often results older than the retention are deleted.
	pruneEvery = time.Hour
	// pruneChunk bounds each delete, so pruning never holds the write lock
	// for long.
	pruneChunk = 10000
)

// Options configures a Store.
type Options struct {
	// Retention is how long results are kept. It must cover the longest
	// SLO window.
	Retention time.Duration
	// QueueSize bounds the results waiting to be written. Results arriving
	// while it is full are dropped and counted. Defaults to 4096.
	QueueSize int
	// FlushInterval is the longest a result waits before it is written.
	// Defaults to one second.
	FlushInterval time.Duration
	// Logger receives write errors. Nil discards them.
	Logger *slog.Logger
}

// Store records probe results in SQLite.
//
// Results are written by a single goroutine in batched transactions. Record
// only queues a result and never blocks, so a slow disk can never delay a
// probe or distort its timing; if the queue fills up, results are dropped
// and counted instead.
type Store struct {
	db     *sql.DB
	opts   Options
	queue  chan scheduler.Result
	stop   chan struct{} // closed by Close
	done   chan struct{} // closed when the writer has flushed and exited
	cancel context.CancelFunc

	dropped     atomic.Uint64
	writeErrors atomic.Uint64
	closeOnce   sync.Once
	closeErr    error
}

// migrations are applied in order, each once; PRAGMA user_version records
// how many have run.
var migrations = []string{
	`CREATE TABLE results (
		check_name  TEXT    NOT NULL,
		started_at  INTEGER NOT NULL, -- Unix milliseconds
		duration_us INTEGER NOT NULL,
		success     INTEGER NOT NULL, -- 1 or 0
		reason      TEXT    NOT NULL, -- empty on success
		error       TEXT    NOT NULL
	) STRICT;
	-- Covers SLO counts, which then never read the table itself.
	CREATE INDEX results_by_check ON results (check_name, started_at, success);
	CREATE INDEX results_by_time ON results (started_at);`,

	`-- One row per check that is failing or firing; healthy checks have none.
	CREATE TABLE alert_state (
		check_name   TEXT    PRIMARY KEY,
		firing       INTEGER NOT NULL,
		streak       INTEGER NOT NULL,
		streak_start INTEGER NOT NULL, -- Unix milliseconds, 0 if unset
		firing_since INTEGER NOT NULL, -- Unix milliseconds, 0 if unset
		last_reason  TEXT    NOT NULL,
		last_error   TEXT    NOT NULL
	) STRICT, WITHOUT ROWID;
	-- The outbox: notifications are written with the state change that
	-- caused them and deleted once delivered.
	CREATE TABLE notifications (
		id           INTEGER PRIMARY KEY,
		notification TEXT    NOT NULL -- JSON
	) STRICT;`,

	`-- When each heartbeat check was last pinged.
	CREATE TABLE heartbeats (
		check_name TEXT    PRIMARY KEY,
		last_ping  INTEGER NOT NULL -- Unix milliseconds
	) STRICT, WITHOUT ROWID;`,
}

// Open opens or creates the database at path, applies pending migrations,
// deletes results older than the retention and starts the writer.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, fmt.Errorf("history path %q must not contain '?' or '#'", path)
	}
	if opts.Retention <= 0 {
		return nil, errors.New("history retention must be positive")
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = 4096
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	// WAL lets the status page read while the writer writes. synchronous
	// NORMAL is safe with WAL; a power loss can only lose the last few
	// transactions, an acceptable loss for monitoring history. Immediate
	// transactions take the write lock when they begin, so concurrent
	// writers wait their turn under busy_timeout instead of failing with
	// SQLITE_BUSY when two deferred transactions both try to upgrade.
	// _dqs=0 makes a mistyped "identifier" an error, not a string literal.
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000&_txlock=immediate&_dqs=0")
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}

	// Writes outlive the caller's context: Close decides when they stop.
	writeCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &Store{
		db: db, opts: opts, cancel: cancel,
		queue: make(chan scheduler.Result, opts.QueueSize),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if err := s.prune(writeCtx); err != nil {
		s.opts.Logger.Error("deleting old history", "err", err)
	}
	go s.run(writeCtx)
	return s, nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		// PRAGMA does not take parameters; i+1 is an integer we control.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Record queues r to be written. It never blocks: if the queue is full, r
// is dropped and counted.
func (s *Store) Record(r scheduler.Result) {
	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
	}
}

// Dropped returns how many results were dropped because the queue was full.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

// WriteErrors returns how many batches failed to be written.
func (s *Store) WriteErrors() uint64 { return s.writeErrors.Load() }

// Close writes the results still queued and closes the database. If ctx
// ends first, the write in progress is cancelled and the rest are lost.
// Results recorded after Close are never written.
func (s *Store) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.stop)
		select {
		case <-s.done:
		case <-ctx.Done():
			s.cancel()
			<-s.done
			s.closeErr = fmt.Errorf("writing queued history: %w", ctx.Err())
		}
		s.cancel()
		s.closeErr = errors.Join(s.closeErr, s.db.Close())
	})
	return s.closeErr
}

// run is the single writer. It batches queued results into transactions,
// flushing when a batch fills or FlushInterval passes, and prunes old
// results every pruneEvery.
func (s *Store) run(ctx context.Context) {
	defer close(s.done)
	flush := time.NewTicker(s.opts.FlushInterval)
	defer flush.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()

	batch := make([]scheduler.Result, 0, maxBatch)
	write := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.write(ctx, batch); err != nil {
			s.writeErrors.Add(1)
			s.opts.Logger.Error("writing history", "results", len(batch), "err", err)
		}
		batch = batch[:0]
	}
	add := func(r scheduler.Result) {
		batch = append(batch, r)
		if len(batch) == maxBatch {
			write()
		}
	}
	for {
		select {
		case r := <-s.queue:
			add(r)
		case <-flush.C:
			write()
		case <-prune.C:
			write()
			if err := s.prune(ctx); err != nil {
				s.opts.Logger.Error("deleting old history", "err", err)
			}
		case <-s.stop:
			for {
				select {
				case r := <-s.queue:
					add(r)
				default:
					write()
					return
				}
			}
		}
	}
}

func (s *Store) write(ctx context.Context, batch []scheduler.Result) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO results
		(check_name, started_at, duration_us, success, reason, error) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range batch {
		success, errText := 1, ""
		if !r.OK() {
			success, errText = 0, truncate(r.Err.Error(), maxErrorLen)
		}
		if _, err := stmt.ExecContext(ctx, r.Check, r.Start.UnixMilli(), r.Duration.Microseconds(),
			success, string(r.Reason), errText); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prune deletes results older than the retention, a chunk at a time.
func (s *Store) prune(ctx context.Context) error {
	cutoff := time.Now().Add(-s.opts.Retention).UnixMilli()
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM results WHERE rowid IN
			(SELECT rowid FROM results WHERE started_at < ? LIMIT ?)`, cutoff, pruneChunk)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n < pruneChunk {
			return err
		}
	}
}

// Counts returns how many probes of check started at or after since, how
// many of them failed, and when the check's oldest kept probe started (zero
// if it has none).
func (s *Store) Counts(ctx context.Context, check string, since time.Time) (total, failed int, first time.Time, err error) {
	var firstMilli sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(1 - success), 0),
		(SELECT MIN(started_at) FROM results WHERE check_name = ?1)
		FROM results WHERE check_name = ?1 AND started_at >= ?2`, check, since.UnixMilli()).Scan(&total, &failed, &firstMilli)
	if firstMilli.Valid {
		first = time.UnixMilli(firstMilli.Int64)
	}
	return total, failed, first, err
}

// truncate shortens s to at most n bytes without splitting a UTF-8
// character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Sample is one stored probe result.
type Sample struct {
	Start    time.Time
	Duration time.Duration
	OK       bool
	Reason   probe.Reason
}

// Recent returns check's last n results, oldest first.
func (s *Store) Recent(ctx context.Context, check string, n int) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT started_at, duration_us, success, reason FROM results
		WHERE check_name = ? ORDER BY started_at DESC LIMIT ?`, check, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var samples []Sample
	for rows.Next() {
		var (
			startMs, durationUs int64
			sample              Sample
			reason              string
		)
		if err := rows.Scan(&startMs, &durationUs, &sample.OK, &reason); err != nil {
			return nil, err
		}
		sample.Start = time.UnixMilli(startMs).UTC()
		sample.Duration = time.Duration(durationUs) * time.Microsecond
		sample.Reason = probe.Reason(reason)
		samples = append(samples, sample)
	}
	slices.Reverse(samples)
	return samples, rows.Err()
}
