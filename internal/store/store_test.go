package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func open(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	if opts.Retention == 0 {
		opts.Retention = 30 * 24 * time.Hour
	}
	s, err := Open(t.Context(), path, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func closeStore(t *testing.T, s *Store) {
	t.Helper()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func result(check string, start time.Time, reason probe.Reason) scheduler.Result {
	r := scheduler.Result{Check: check}
	r.Start, r.Duration, r.Reason = start, 42*time.Millisecond, reason
	if reason != "" {
		r.Err = errors.New("probe failed")
	}
	return r
}

func counts(t *testing.T, s *Store, check string, since time.Time) (int, int) {
	t.Helper()
	total, failed, err := s.Counts(t.Context(), check, since)
	if err != nil {
		t.Fatal(err)
	}
	return total, failed
}

func TestHistorySurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	now := time.Now()
	s := open(t, path, Options{})
	s.Record(result("web", now.Add(-2*time.Minute), ""))
	s.Record(result("web", now.Add(-time.Minute), probe.ReasonTimeout))
	s.Record(result("dns", now, ""))
	closeStore(t, s) // flushes the queue

	s = open(t, path, Options{})
	defer closeStore(t, s)
	if total, failed := counts(t, s, "web", now.Add(-time.Hour)); total != 2 || failed != 1 {
		t.Errorf("web: %d probes, %d failed; want 2 and 1", total, failed)
	}
	if total, failed := counts(t, s, "dns", now.Add(-time.Hour)); total != 1 || failed != 0 {
		t.Errorf("dns: %d probes, %d failed; want 1 and 0", total, failed)
	}
}

func TestCountsOnlyIncludesWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	now := time.Now()
	s := open(t, path, Options{})
	for _, age := range []time.Duration{10 * time.Minute, 5 * time.Minute, time.Minute} {
		s.Record(result("web", now.Add(-age), probe.ReasonConnect))
	}
	closeStore(t, s)
	s = open(t, path, Options{})
	defer closeStore(t, s)

	if total, _ := counts(t, s, "web", now.Add(-6*time.Minute)); total != 2 {
		t.Errorf("%d probes in the last 6 minutes, want 2", total)
	}
	if total, failed := counts(t, s, "missing", now.Add(-time.Hour)); total != 0 || failed != 0 {
		t.Errorf("unknown check: %d probes, %d failed; want 0 and 0", total, failed)
	}
}

func TestResultsAreWrittenWithinFlushInterval(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{FlushInterval: 10 * time.Millisecond})
	defer closeStore(t, s)
	s.Record(result("web", time.Now(), ""))

	deadline := time.Now().Add(5 * time.Second)
	for {
		if total, _ := counts(t, s, "web", time.Now().Add(-time.Hour)); total == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("result not written within 5s with a 10ms flush interval")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenDeletesResultsOlderThanRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	now := time.Now()
	s := open(t, path, Options{Retention: 365 * 24 * time.Hour})
	s.Record(result("web", now.Add(-40*24*time.Hour), ""))
	s.Record(result("web", now.Add(-20*24*time.Hour), ""))
	closeStore(t, s)

	s = open(t, path, Options{Retention: 30 * 24 * time.Hour})
	defer closeStore(t, s)
	if total, _ := counts(t, s, "web", now.Add(-365*24*time.Hour)); total != 1 {
		t.Errorf("%d probes left, want 1: the one within the 30-day retention", total)
	}
}

func TestRecordDropsWhenQueueIsFull(t *testing.T) {
	// No writer is draining this queue.
	s := &Store{queue: make(chan scheduler.Result, 2)}
	for range 5 {
		s.Record(result("web", time.Now(), ""))
	}
	if got := s.Dropped(); got != 3 {
		t.Errorf("dropped %d results, want 3", got)
	}
}

func TestLongErrorsAreTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := open(t, path, Options{})
	r := result("web", time.Now(), probe.ReasonOther)
	r.Err = errors.New(strings.Repeat("é", maxErrorLen)) // 2 bytes per character
	s.Record(r)
	closeStore(t, s)

	s = open(t, path, Options{})
	defer closeStore(t, s)
	var stored string
	if err := s.db.QueryRowContext(t.Context(), "SELECT error FROM results").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) > maxErrorLen || !strings.HasPrefix(strings.Repeat("é", maxErrorLen), stored) {
		t.Errorf("stored error is %d bytes or splits a character; want at most %d whole characters", len(stored), maxErrorLen)
	}
}

func TestReopenDoesNotRerunMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	closeStore(t, open(t, path, Options{}))
	s := open(t, path, Options{}) // would fail with "table results already exists"
	defer closeStore(t, s)

	var version int
	if err := s.db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != len(migrations) {
		t.Errorf("schema version %d, want %d", version, len(migrations))
	}
}

func TestOpenRejectsBadOptions(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(t.Context(), filepath.Join(dir, "a?b.db"), Options{Retention: time.Hour}); err == nil {
		t.Error("Open accepted a path containing '?'")
	}
	if _, err := Open(t.Context(), filepath.Join(dir, "h.db"), Options{}); err == nil {
		t.Error("Open accepted a zero retention")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	closeStore(t, s)
	closeStore(t, s)
}

// BenchmarkWriteBatch measures how many results per second the writer can
// store, in full batches.
func BenchmarkWriteBatch(b *testing.B) {
	s, err := Open(b.Context(), filepath.Join(b.TempDir(), "history.db"), Options{Retention: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close(context.Background())
	batch := make([]scheduler.Result, maxBatch)
	for i := range batch {
		batch[i] = result("web", time.Now(), "")
	}
	for b.Loop() {
		if err := s.write(b.Context(), batch); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*maxBatch)/b.Elapsed().Seconds(), "results/s")
}
