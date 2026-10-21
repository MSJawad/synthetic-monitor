package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func result(check string, reason probe.Reason) scheduler.Result {
	r := scheduler.Result{Check: check, Type: "http", Scheduled: t0}
	r.Start, r.Duration, r.Reason = t0, 120*time.Millisecond, reason
	if reason != "" {
		r.Err = errors.New("failed")
	}
	return r
}

func TestRecordCountsResultsAndLastOutcome(t *testing.T) {
	m := New()
	m.Record(result("web", ""))
	m.Record(result("web", ""))
	m.Record(result("web", probe.ReasonTimeout))

	want := `
# HELP synthetic_probes_total Probes completed, by result: "success" or the reason the probe failed.
# TYPE synthetic_probes_total counter
synthetic_probes_total{check="web",result="success"} 2
synthetic_probes_total{check="web",result="timeout"} 1
# HELP synthetic_probe_success 1 if the check's last probe succeeded, 0 if it failed.
# TYPE synthetic_probe_success gauge
synthetic_probe_success{check="web"} 0
`
	if err := testutil.CollectAndCompare(m.registry, strings.NewReader(want),
		"synthetic_probes_total", "synthetic_probe_success"); err != nil {
		t.Fatal(err)
	}
	if n := testutil.CollectAndCount(m.probeDuration); n != 1 {
		t.Errorf("duration histogram has %d series, want 1", n)
	}
}

func TestRecordHTTPPhasesThatHappened(t *testing.T) {
	m := New()
	r := result("web", "")
	r.Phases = probe.HTTPPhases{Connect: time.Millisecond, Processing: 50 * time.Millisecond, Transfer: time.Millisecond}
	m.Record(r)

	body := scrape(t, m)
	for phase, want := range map[string]bool{"dns": false, "connect": true, "tls": false, "processing": true, "transfer": true} {
		series := `synthetic_probe_http_phase_duration_seconds_count{check="web",phase="` + phase + `"} 1`
		if got := strings.Contains(body, series); got != want {
			t.Errorf("phase %s exported = %v, want %v: only phases that happened are observed", phase, got, want)
		}
	}
}

func TestRecordCertExpiryAndLag(t *testing.T) {
	m := New()
	r := result("cert", "")
	r.Type = "tls"
	r.CertNotAfter = t0.Add(30 * 24 * time.Hour)
	r.Start = t0.Add(250 * time.Millisecond) // waited for a slot
	m.Record(r)

	if got := testutil.ToFloat64(m.certExpiry.WithLabelValues("cert")); got != float64(r.CertNotAfter.Unix()) {
		t.Errorf("cert expiry = %v, want %d", got, r.CertNotAfter.Unix())
	}
	want := `
# HELP synthetic_scheduler_lag_seconds How late probes started after they were due, mostly from waiting for a free slot.
# TYPE synthetic_scheduler_lag_seconds histogram
synthetic_scheduler_lag_seconds_bucket{le="0.001"} 0
synthetic_scheduler_lag_seconds_bucket{le="0.005"} 0
synthetic_scheduler_lag_seconds_bucket{le="0.01"} 0
synthetic_scheduler_lag_seconds_bucket{le="0.05"} 0
synthetic_scheduler_lag_seconds_bucket{le="0.1"} 0
synthetic_scheduler_lag_seconds_bucket{le="0.5"} 1
synthetic_scheduler_lag_seconds_bucket{le="1"} 1
synthetic_scheduler_lag_seconds_bucket{le="5"} 1
synthetic_scheduler_lag_seconds_bucket{le="10"} 1
synthetic_scheduler_lag_seconds_bucket{le="30"} 1
synthetic_scheduler_lag_seconds_bucket{le="+Inf"} 1
synthetic_scheduler_lag_seconds_sum 0.25
synthetic_scheduler_lag_seconds_count 1
`
	if err := testutil.CollectAndCompare(m.lag, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestSetChecksTracksTypesAndTargets(t *testing.T) {
	m := New()
	m.SetChecks([]config.Check{
		{Name: "a", TCP: &config.TCP{}, SLO: &config.SLO{Target: 99.5, WindowDays: 30}},
		{Name: "b", DNS: &config.DNS{}},
	})
	// a becomes an HTTP check without an SLO; b is removed.
	m.SetChecks([]config.Check{{Name: "a", HTTP: &config.HTTP{}}})

	want := `
# HELP synthetic_check_info Always 1 for each configured check, labelled with its type.
# TYPE synthetic_check_info gauge
synthetic_check_info{check="a",type="http"} 1
`
	if err := testutil.CollectAndCompare(m.registry, strings.NewReader(want),
		"synthetic_check_info", "synthetic_slo_target_ratio"); err != nil {
		t.Fatal(err)
	}

	m.SetChecks([]config.Check{{Name: "a", HTTP: &config.HTTP{}, SLO: &config.SLO{Target: 99.9, WindowDays: 7}}})
	if got := testutil.ToFloat64(m.sloTarget.WithLabelValues("a")); got != 0.999 {
		t.Errorf("SLO target = %v, want 0.999", got)
	}
}

func TestSetSLOStatusReplacesSeries(t *testing.T) {
	m := New()
	m.SetSLOStatus(map[string]slo.Status{"a": {Availability: 0.99, BudgetRemaining: 0.5}, "b": {Availability: 1, BudgetRemaining: 1}})
	m.SetSLOStatus(map[string]slo.Status{"a": {Availability: 0.98, BudgetRemaining: -0.2}})

	want := `
# HELP synthetic_slo_availability_ratio Fraction of the check's probes that succeeded over its SLO window.
# TYPE synthetic_slo_availability_ratio gauge
synthetic_slo_availability_ratio{check="a"} 0.98
# HELP synthetic_slo_error_budget_remaining_ratio Fraction of the check's error budget left over its SLO window; negative once overspent.
# TYPE synthetic_slo_error_budget_remaining_ratio gauge
synthetic_slo_error_budget_remaining_ratio{check="a"} -0.2
`
	if err := testutil.CollectAndCompare(m.registry, strings.NewReader(want),
		"synthetic_slo_availability_ratio", "synthetic_slo_error_budget_remaining_ratio"); err != nil {
		t.Fatal(err)
	}
}

func TestForgetDeletesEveryCheckSeries(t *testing.T) {
	m := New()
	for _, name := range []string{"gone", "kept"} {
		r := result(name, "")
		r.Phases.Connect = time.Millisecond
		r.CertNotAfter = t0
		m.Record(r)
		m.Skipped(name, 1)
		m.AlertObserver().Firing(name, true)
	}
	m.SetChecks([]config.Check{
		{Name: "gone", TCP: &config.TCP{}, SLO: &config.SLO{Target: 99, WindowDays: 1}},
		{Name: "kept", TCP: &config.TCP{}},
	})
	m.SetSLOStatus(map[string]slo.Status{"gone": {Availability: 1, BudgetRemaining: 1}})

	m.Forget("gone")
	got, err := testutil.GatherAndCount(m.registry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scrape(t, m), `check="gone"`) {
		t.Errorf("series for the forgotten check remain:\n%s", grep(scrape(t, m), `check="gone"`))
	}
	if !strings.Contains(scrape(t, m), `check="kept"`) || got == 0 {
		t.Error("Forget removed another check's series")
	}
}

func TestConfigLoaded(t *testing.T) {
	m := New()
	m.ConfigLoaded(true)
	m.ConfigLoaded(false)

	if got := testutil.ToFloat64(m.lastReloadOK); got != 0 {
		t.Errorf("last reload successful = %v after a rejected reload, want 0", got)
	}
	if s, f := testutil.ToFloat64(m.reloads.WithLabelValues("success")), testutil.ToFloat64(m.reloads.WithLabelValues("failure")); s != 1 || f != 1 {
		t.Errorf("reloads = %v successful, %v failed; want 1 and 1", s, f)
	}
	if ts := testutil.ToFloat64(m.lastReloadTimeOK); ts == 0 {
		t.Error("last successful reload time not set")
	}
}

type historyStats struct{ dropped, errs uint64 }

func (h historyStats) Dropped() uint64     { return h.dropped }
func (h historyStats) WriteErrors() uint64 { return h.errs }

func TestHandlerServesEverything(t *testing.T) {
	m := New()
	m.TrackInFlight(func() int { return 3 })
	m.TrackHistory(historyStats{dropped: 4, errs: 5})
	m.Record(result("web", ""))

	body := scrape(t, m)
	for _, want := range []string{
		"synthetic_probes_in_flight 3",
		"synthetic_history_dropped_results_total 4",
		"synthetic_history_write_errors_total 5",
		`synthetic_probe_success{check="web"} 1`,
		"go_goroutines ",
		"process_resident_memory_bytes ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func grep(text, substr string) string {
	var lines []string
	for line := range strings.SplitSeq(text, "\n") {
		if strings.Contains(line, substr) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func TestAlertObserver(t *testing.T) {
	m := New()
	o := m.AlertObserver()
	o.Firing("web", true)
	o.Firing("api", true)
	o.Firing("api", false)
	o.Delivered(alert.KindAlert)
	o.DeliveryFailed(alert.KindRecovery, false)
	o.DeliveryFailed(alert.KindRecovery, false)
	o.DeliveryFailed(alert.KindAlert, true)
	o.Pending(2)
	o.DroppedResult()

	body := scrape(t, m)
	for _, want := range []string{
		`synthetic_alert_firing{check="web"} 1`,
		`synthetic_alert_firing{check="api"} 0`,
		`synthetic_notifications_delivered_total{kind="alert"} 1`,
		`synthetic_notification_delivery_failures_total{kind="recovery",permanent="false"} 2`,
		`synthetic_notification_delivery_failures_total{kind="alert",permanent="true"} 1`,
		"synthetic_notifications_pending 2",
		"synthetic_alert_dropped_results_total 1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}
