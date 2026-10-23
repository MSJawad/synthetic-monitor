package store

import (
	"math/rand/v2"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

const day = 24 * time.Hour

// today is the current day's midnight, UTC. Tests that reopen a store
// write within the last SummaryDays, which opening it would otherwise
// prune.
func today() time.Time {
	return time.Now().UTC().Truncate(day)
}

func days(t *testing.T, s *Store, check string, since time.Time) []Day {
	t.Helper()
	got, err := s.Days(t.Context(), check, since)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameDays(a, b []Day) bool {
	return slices.EqualFunc(a, b, func(x, y Day) bool {
		return x.Start.Equal(y.Start) && x.Probes == y.Probes && x.Failed == y.Failed
	})
}

// TestDailyTotalsMatchResults writes results in random order and batch
// sizes, many batches spanning a midnight, some exactly at one, and checks
// the daily totals against a count of the results written.
func TestDailyTotalsMatchResults(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // the test must write the same results every run
	first := today().Add(-3 * day)
	want := map[string][]Day{}
	var results []scheduler.Result
	for _, check := range []string{"web", "api"} {
		for d := range 3 {
			midnight := first.Add(time.Duration(d) * day)
			total := Day{Start: midnight}
			for i := range 200 {
				at := midnight.Add(time.Duration(rng.Int64N(int64(day))))
				switch i {
				case 0:
					at = midnight
				case 1:
					at = midnight.Add(day - time.Millisecond)
				}
				reason := probe.Reason("")
				if rng.IntN(4) == 0 {
					reason, total.Failed = probe.ReasonTimeout, total.Failed+1
				}
				results = append(results, result(check, at, reason))
				total.Probes++
			}
			want[check] = append(want[check], total)
		}
	}
	rng.Shuffle(len(results), func(i, j int) { results[i], results[j] = results[j], results[i] })
	for len(results) > 0 {
		n := min(1+rng.IntN(maxBatch), len(results))
		if err := s.write(t.Context(), results[:n]); err != nil {
			t.Fatal(err)
		}
		results = results[n:]
	}

	for check, w := range want {
		if got := days(t, s, check, first); !sameDays(got, w) {
			t.Errorf("%s days = %+v, want %+v", check, got, w)
		}
		// Asking from the middle of a day includes that whole day.
		if got := days(t, s, check, first.Add(day+time.Hour)); !sameDays(got, w[1:]) {
			t.Errorf("%s days since the middle of the second = %+v, want %+v", check, got, w[1:])
		}
	}
}

func TestDailyTotalsOutliveResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	midnight := today()
	s := open(t, path, Options{Retention: 365 * day})
	s.Record(result("web", midnight.Add(-(SummaryDays+5)*day), ""))
	s.Record(result("web", midnight.Add(-60*day), probe.ReasonTimeout))
	s.Record(result("web", midnight, ""))
	closeStore(t, s)

	s = open(t, path, Options{Retention: 30 * day})
	defer closeStore(t, s)
	if total, _ := counts(t, s, "web", time.Time{}); total != 1 {
		t.Errorf("%d results left, want 1: the one within the 30-day retention", total)
	}
	want := []Day{{Start: midnight.Add(-60 * day), Probes: 1, Failed: 1}, {Start: midnight, Probes: 1}}
	if got := days(t, s, "web", time.Time{}); !sameDays(got, want) {
		t.Errorf("days = %+v, want %+v: kept %d days, longer than the results", got, want, SummaryDays)
	}
}

// TestUpgradeCountsExistingResults opens a database written before daily
// totals were kept, as production's was.
func TestUpgradeCountsExistingResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	midnight := today()
	oldDatabase(t, path, 5, `INSERT INTO results VALUES
		('web', ?1, 1000, 1, '', ''),
		('web', ?2, 1000, 0, 'timeout', 'no result within 10s'),
		('web', ?3, 1000, 1, '', ''),
		('api', ?3, 1000, 1, '', '')`,
		midnight.Add(-day).UnixMilli(), midnight.Add(-time.Millisecond).UnixMilli(), midnight.UnixMilli())

	s := open(t, path, Options{})
	s.Record(result("web", midnight.Add(time.Hour), probe.ReasonTimeout))
	closeStore(t, s)
	s = open(t, path, Options{})
	defer closeStore(t, s)
	want := []Day{{Start: midnight.Add(-day), Probes: 2, Failed: 1}, {Start: midnight, Probes: 2, Failed: 1}}
	if got := days(t, s, "web", time.Time{}); !sameDays(got, want) {
		t.Errorf("days = %+v, want %+v: the old results counted, and new ones added", got, want)
	}
}
