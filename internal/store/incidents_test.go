package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

func incidents(t *testing.T, s *Store, since time.Time) []Incident {
	t.Helper()
	got, err := s.Incidents(t.Context(), since)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func sameIncidents(a, b []Incident) bool {
	return slices.EqualFunc(a, b, func(x, y Incident) bool {
		return x.Check == y.Check && x.Start.Equal(y.Start) && x.End.Equal(y.End) && x.Burn == y.Burn && x.Reason == y.Reason
	})
}

// incident saves an alert for check whose failures began at start and, if
// end isn't zero, its recovery once probes succeeded again at end.
func incident(t *testing.T, s *Store, check string, start, end time.Time) {
	t.Helper()
	n := &alert.Notification{Kind: alert.KindAlert, Check: check, At: start.Add(time.Minute), Since: start,
		Probes: 3, Reason: probe.ReasonTimeout}
	if err := s.SaveAlertState(t.Context(), check, alert.State{Firing: true, FiringSince: start}, n); err != nil {
		t.Fatal(err)
	}
	if end.IsZero() {
		return
	}
	n = &alert.Notification{Kind: alert.KindRecovery, Check: check, At: end.Add(time.Minute), Since: start,
		Until: end, Probes: 2}
	if err := s.SaveAlertState(t.Context(), check, alert.State{}, n); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryClosesIncident(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	s := open(t, path, Options{})
	if err := s.SaveAlertState(t.Context(), "web", firing, notice); err != nil {
		t.Fatal(err)
	}
	want := []Incident{{Check: "web", Start: since, Reason: probe.ReasonTimeout}}
	if got := incidents(t, s, since); !sameIncidents(got, want) {
		t.Fatalf("incidents = %+v, want web's open incident %+v", got, want)
	}
	// A restart in the middle keeps the incident open.
	closeStore(t, s)
	s = open(t, path, Options{})
	defer closeStore(t, s)

	// The incident ends when probes began succeeding, not when the
	// recovery notice went out.
	until := since.Add(5 * time.Minute)
	n := &alert.Notification{Kind: alert.KindRecovery, Check: "web", At: until.Add(time.Minute), Since: since,
		Until: until, Probes: 2, Reason: probe.ReasonTimeout}
	if err := s.SaveAlertState(t.Context(), "web", alert.State{}, n); err != nil {
		t.Fatal(err)
	}
	want[0].End = until
	if got := incidents(t, s, since); !sameIncidents(got, want) {
		t.Errorf("incidents = %+v, want %+v", got, want)
	}
}

func TestRemovalClosesBurnIncident(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	burn := *notice
	burn.Probes, burn.Burn = 0, &alert.Burn{Failed: 20, Total: 120, Window: time.Hour, Rate: 33, Since: since}
	if err := s.SaveAlertState(t.Context(), "web", firing, &burn); err != nil {
		t.Fatal(err)
	}
	removed := since.Add(time.Hour)
	if err := s.SaveAlertState(t.Context(), "web", alert.State{}, alert.Removed("web", firing, removed)); err != nil {
		t.Fatal(err)
	}
	want := []Incident{{Check: "web", Start: since, End: removed, Burn: true, Reason: probe.ReasonTimeout}}
	if got := incidents(t, s, since); !sameIncidents(got, want) {
		t.Errorf("incidents = %+v, want %+v: a burn incident that ends with the removal", got, want)
	}
}

func TestIncidentsSince(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "history.db"), Options{})
	defer closeStore(t, s)
	incident(t, s, "ended-before", since.Add(-3*time.Hour), since.Add(-2*time.Hour))
	incident(t, s, "ended-after", since.Add(-90*time.Minute), since.Add(-30*time.Minute))
	incident(t, s, "open", since.Add(-2*time.Hour), time.Time{})

	got := incidents(t, s, since.Add(-time.Hour))
	want := []Incident{
		{Check: "open", Start: since.Add(-2 * time.Hour), Reason: probe.ReasonTimeout},
		{Check: "ended-after", Start: since.Add(-90 * time.Minute), End: since.Add(-30 * time.Minute), Reason: probe.ReasonTimeout},
	}
	if !sameIncidents(got, want) {
		t.Errorf("incidents = %+v, want %+v: those open or ended since, the open one first though it started earlier", got, want)
	}
}

func TestOpenDeletesOldIncidents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	day := 24 * time.Hour
	now := time.Now()
	s := open(t, path, Options{})
	incident(t, s, "ended-91-days-ago", now.Add(-92*day), now.Add(-91*day))
	incident(t, s, "ended-89-days-ago", now.Add(-90*day), now.Add(-89*day))
	incident(t, s, "open-for-100-days", now.Add(-100*day), time.Time{})
	closeStore(t, s)

	s = open(t, path, Options{})
	defer closeStore(t, s)
	var kept []string
	for _, in := range incidents(t, s, time.Time{}) {
		kept = append(kept, in.Check)
	}
	if want := []string{"open-for-100-days", "ended-89-days-ago"}; !slices.Equal(kept, want) {
		t.Errorf("kept %q, want %q: incidents that ended more than %d days ago are deleted, open ones never", kept, want, SummaryDays)
	}
}

// TestUpgradeOpensIncidentsOfFiringChecks opens a database written before
// incidents were recorded, as production's was, while one check fired and
// another was failing below its alert threshold.
func TestUpgradeOpensIncidentsOfFiringChecks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	oldDatabase(t, path, 4, `INSERT INTO alert_state VALUES
		('web', 1, 0, 0, ?1, 'timeout', 'no result within 10s', 1),
		('api', 0, 2, ?1, 0, 'connect', 'connection refused', 0)`, since.UnixMilli())

	s := open(t, path, Options{})
	defer closeStore(t, s)
	want := []Incident{{Check: "web", Start: since, Burn: true, Reason: probe.ReasonTimeout}}
	if got := incidents(t, s, since); !sameIncidents(got, want) {
		t.Errorf("incidents = %+v, want %+v: only the firing check's", got, want)
	}
}
