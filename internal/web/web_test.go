package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

var now = time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC)

func sample(ago time.Duration, ok bool) store.Sample {
	s := store.Sample{Start: now.Add(-ago), Duration: 120 * time.Millisecond, OK: ok}
	if !ok {
		s.Reason = probe.ReasonTimeout
	}
	return s
}

func httpCheck(name string) config.Check {
	return config.Check{Name: name, HTTP: &config.HTTP{URL: "https://internal.example:8443/healthz"}}
}

func TestBuildCheckStates(t *testing.T) {
	tests := []struct {
		name       string
		in         checkInput
		want       state
		latency    string
		hasSince   bool
		checkedHas string
	}{
		{"no probes yet", checkInput{}, statePending, "", false, "Waiting for the first probe"},
		{"history unreadable", checkInput{unavailable: true}, statePending, "", false, "History unavailable"},
		{"up", checkInput{samples: []store.Sample{sample(40*time.Second, false), sample(10*time.Second, true)}},
			stateUp, "120 ms", false, "Checked 10s ago"},
		{"failing below threshold", checkInput{samples: []store.Sample{sample(10*time.Second, false)}},
			stateFailing, "Failed", false, "Checked 10s ago"},
		{"down", checkInput{samples: []store.Sample{sample(5*time.Second, false)}, firingSince: now.Add(-12 * time.Minute)},
			stateDown, "Failed", true, "Checked 5s ago"},
		{"recovering", checkInput{samples: []store.Sample{sample(5*time.Second, true)}, firingSince: now.Add(-time.Hour)},
			stateRecovering, "120 ms", true, "Checked 5s ago"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.in.check = httpCheck("web")
			v := buildCheck(tt.in, now)
			if v.State != tt.want || v.Latency != tt.latency || (v.Since != "") != tt.hasSince || v.Checked != tt.checkedHas {
				t.Errorf("view = state %q, latency %q, since %q, checked %q; want %q, %q, since set %v, %q",
					v.State, v.Latency, v.Since, v.Checked, tt.want, tt.latency, tt.hasSince, tt.checkedHas)
			}
			if v.Label == "" || v.Type != "HTTP" {
				t.Errorf("label %q, type %q; want a label and HTTP", v.Label, v.Type)
			}
			if failed := v.Latency == "Failed"; failed != (v.Note == "timed out") {
				t.Errorf("note %q for latency %q, want the reason exactly when the last probe failed", v.Note, v.Latency)
			}
		})
	}
}

func TestDownSince(t *testing.T) {
	in := checkInput{check: httpCheck("web"), samples: []store.Sample{sample(time.Second, false)}, firingSince: now.Add(-12 * time.Minute)}
	if got := buildCheck(in, now).Since; got != "Down since 14:48:00 UTC, 12m ago" {
		t.Errorf("Since = %q", got)
	}
	in.firingSince = now.Add(-30 * time.Hour)
	if got := buildCheck(in, now).Since; got != "Down since Oct 20 09:00 UTC, 30h ago" {
		t.Errorf("Since for an incident from yesterday = %q", got)
	}
	in.samples = []store.Sample{sample(time.Second, true)}
	if got := buildCheck(in, now).Since; got != "Incident began Oct 20 09:00 UTC, 30h ago" {
		t.Errorf("Since while recovering = %q", got)
	}
}

func TestHeadline(t *testing.T) {
	tests := []struct {
		states []state
		want   state
		text   string
	}{
		{nil, statePending, "No checks configured"},
		{[]state{stateUp, stateUp}, stateUp, "All checks are up"},
		{[]state{statePending, statePending}, statePending, "Waiting for the first probes"},
		{[]state{stateUp, statePending}, stateUp, "All checks are up"},
		{[]state{stateUp, stateFailing, stateRecovering}, stateFailing, "2 checks are degraded"},
		{[]state{stateDown, stateFailing}, stateDown, "1 check is down"},
		{[]state{stateDown, stateDown}, stateDown, "2 checks are down"},
	}
	for _, tt := range tests {
		counts := make(map[state]int)
		for _, s := range tt.states {
			counts[s]++
		}
		if got, text := headline(counts, len(tt.states)); got != tt.want || text != tt.text {
			t.Errorf("headline(%v) = %q %q, want %q %q", tt.states, got, text, tt.want, tt.text)
		}
	}
}

func TestPercentNeverRoundsUpToPerfect(t *testing.T) {
	for in, want := range map[float64]string{
		1: "100%", 0.9995: "99.95%", 0.99999: "99.999%", 0.999999999: "99.999%",
		0.995: "99.5%", 0.896551: "89.65%", 0: "0%",
	} {
		if got := percent(in); got != want {
			t.Errorf("percent(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestErrorBudget(t *testing.T) {
	// 1000 probes against a 99.5% target allow 5 failures.
	tests := []struct {
		failed int
		text   string
		level  string
		fill   string
	}{
		{0, "100% of error budget left", "ok", "100"},
		{1, "80% of error budget left", "ok", "80"},
		{4, "20% of error budget left", "low", "20"},
		{5, "Error budget used up: 5 failed probes, 5 allowed", "exhausted", "100"},
		{40, "Error budget used up: 40 failed probes, 5 allowed", "exhausted", "100"},
	}
	for _, tt := range tests {
		status, _ := slo.Compute(0.995, 30*24*time.Hour, 1000, tt.failed)
		v := buildSLO(status, config.SLO{Target: 99.5, WindowDays: 30}, now)
		if v.Budget != tt.text || v.Meter.Level != tt.level || v.Meter.Fill != tt.fill {
			t.Errorf("%d failed: %q, level %q, fill %q; want %q, %q, %q",
				tt.failed, v.Budget, v.Meter.Level, v.Meter.Fill, tt.text, tt.level, tt.fill)
		}
		if v.Label != "Availability, last 30 days" || v.Target != "99.5%" {
			t.Errorf("label %q, target %q; want the last 30 days and 99.5%%", v.Label, v.Target)
		}
	}

	// A short history allows no failures at all yet.
	early, _ := slo.Compute(0.999, 7*24*time.Hour, 29, 1)
	if v := buildSLO(early, config.SLO{Target: 99.9, WindowDays: 7}, now); v.Budget != "Error budget used up: 1 failed probe, 0 allowed" {
		t.Errorf("early budget = %q", v.Budget)
	}
}

func TestSLOLabelWhileHistoryIsShorterThanTheWindow(t *testing.T) {
	status, _ := slo.Compute(0.995, 30*24*time.Hour, 100, 0)
	status.Partial = true
	for since, want := range map[time.Time]string{
		now.Add(-2 * time.Hour):  "Availability since 13:00:00 UTC",
		now.Add(-48 * time.Hour): "Availability since Oct 19 15:00 UTC",
	} {
		status.Since = since
		if got := buildSLO(status, config.SLO{Target: 99.5, WindowDays: 30}, now).Label; got != want {
			t.Errorf("history since %v: label %q, want %q", since, got, want)
		}
	}
}

func TestLatencyFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{
		400 * time.Microsecond: "0.4 ms", 3250 * time.Microsecond: "3.3 ms", 123400 * time.Microsecond: "123 ms",
		9999 * time.Millisecond: "9999 ms", 12340 * time.Millisecond: "12.3 s",
	} {
		if got := latency(d); got != want {
			t.Errorf("latency(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestSparkline(t *testing.T) {
	samples := []store.Sample{
		{Start: now.Add(-4 * time.Second), Duration: 50 * time.Millisecond, OK: true},
		{Start: now.Add(-3 * time.Second), Duration: 100 * time.Millisecond, OK: true},
		{Start: now.Add(-2 * time.Second), Duration: time.Second, Reason: probe.ReasonTimeout},
		{Start: now.Add(-time.Second), Duration: 100 * time.Millisecond, OK: true},
	}
	sp := buildSparkline(samples)
	slot := 100.0 / recentProbes

	if sp.Count != 4 || sp.Caption != "Last 4 probes, peak 100 ms, 1 failed" {
		t.Errorf("count %d, caption %q; want 4 and a caption with the successful peak", sp.Count, sp.Caption)
	}
	// Four probes take the last four of 60 equal slots, each centred in its own.
	if len(sp.Segments) != 1 || sp.Segments[0].X1 != pct(56.5*slot) || sp.Segments[0].X2 != pct(57.5*slot) {
		t.Errorf("segments = %+v, want one, joining the first two probes in slots 56 and 57", sp.Segments)
	}
	if len(sp.Failures) != 1 || sp.Failures[0].X != pct(58*slot) || sp.Failures[0].Width != pct(slot) {
		t.Errorf("failures = %+v, want one band filling slot 58", sp.Failures)
	}
	if sp.End == nil || sp.End.X != pct(59.5*slot) || sp.End.Y != px(sparkPad) {
		t.Errorf("end = %+v, want a dot in the last slot at peak height", sp.End)
	}
	if len(sp.Hits) != 4 || sp.Hits[3].X != pct(59*slot) || !strings.Contains(sp.Hits[2].Title, "failed, Timed out") {
		t.Errorf("hits = %+v, want one per slot, describing the failure", sp.Hits)
	}
}

func TestSparklineMergesConsecutiveFailures(t *testing.T) {
	var samples []store.Sample
	for i, ok := range []bool{false, false, true, false, false, false} {
		samples = append(samples, store.Sample{Start: now.Add(time.Duration(i) * time.Second), Duration: time.Millisecond, OK: ok})
	}
	sp := buildSparkline(samples)
	slot := 100.0 / recentProbes
	want := []band{{X: pct(54 * slot), Width: pct(2 * slot)}, {X: pct(57 * slot), Width: pct(3 * slot)}}
	if len(sp.Failures) != 2 || sp.Failures[0] != want[0] || sp.Failures[1] != want[1] {
		t.Errorf("failures = %+v, want two full-width bands %+v", sp.Failures, want)
	}
}

func TestSparklineCaption(t *testing.T) {
	for _, tt := range []struct {
		count, failed int
		want          string
	}{
		{1, 0, "Last probe, peak 5 ms"},
		{1, 1, "Last probe, failed"},
		{60, 60, "Last 60 probes, all failed"},
		{60, 2, "Last 60 probes, peak 5 ms, 2 failed"},
	} {
		if got := caption(tt.count, tt.failed, 5*time.Millisecond); got != tt.want {
			t.Errorf("caption(%d, %d) = %q, want %q", tt.count, tt.failed, got, tt.want)
		}
	}
}

func TestSparklineKeepsLatestProbes(t *testing.T) {
	samples := make([]store.Sample, recentProbes+10)
	for i := range samples {
		samples[i] = store.Sample{Start: now.Add(time.Duration(i) * time.Second), Duration: time.Millisecond, OK: true}
	}
	if sp := buildSparkline(samples); sp.Count != recentProbes || len(sp.Segments) != recentProbes-1 {
		t.Errorf("count %d with %d segments, want the latest %d probes", sp.Count, len(sp.Segments), recentProbes)
	}
}

// fakeSource returns fixed data, as the service would after some probes.
func fakeSource() Source {
	withSLO := httpCheck("apartment-api")
	withSLO.SLO = &config.SLO{Target: 99.9, WindowDays: 7}
	samples := map[string][]store.Sample{
		"apartment-api": {sample(3*time.Second, true), sample(2*time.Second, false), sample(time.Second, false)},
		"cache":         {sample(time.Second, true)},
	}
	return Source{
		Version: "4b825dc",
		Checks: func() []config.Check {
			return []config.Check{withSLO, {Name: "cache", TCP: &config.TCP{Address: "10.0.0.7:6379"}}, httpCheck("broken")}
		},
		Recent: func(_ context.Context, check string, _ int) ([]store.Sample, error) {
			if check == "broken" {
				return nil, errors.New("database is locked")
			}
			return samples[check], nil
		},
		SLOs: func() map[string]slo.Status {
			return map[string]slo.Status{"apartment-api": {Target: 0.999, Total: 1000, Failed: 2, Availability: 0.998, BudgetRemaining: -1,
				Since: now.Add(-3 * time.Hour), Partial: true}}
		},
		Firing: func() map[string]time.Time {
			// The alert manager keeps local times; the JSON must not.
			return map[string]time.Time{"apartment-api": now.Add(-2 * time.Second).In(time.FixedZone("CEST", 2*60*60))}
		},
	}
}

type response struct {
	status int
	header http.Header
	body   string
}

func serve(t *testing.T, path string) response {
	t.Helper()
	rec := httptest.NewRecorder()
	h := newHandler(fakeSource(), slog.New(slog.DiscardHandler), func() time.Time { return now })
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
	return response{rec.Code, rec.Header(), rec.Body.String()}
}

func TestPage(t *testing.T) {
	resp := serve(t, "/")
	page := resp.body
	if resp.status != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET / = %d %s", resp.status, resp.header.Get("Content-Type"))
	}
	for _, want := range []string{
		"<title>1 check is down · Status</title>",
		`<p class="headline" data-state="down">`,
		`<h2 class="check-name">apartment-api</h2>`,
		`<span class="pill" data-state="down">`,
		"Down since 14:59:58 UTC, 2s ago",
		"Error budget used up: 2 failed probes, 1 allowed",
		"Availability since 12:00:00 UTC",
		`Failed<span class="metric-aside">timed out</span>`,
		"<summary>Recent probes</summary>",
		"History unavailable",
		`href="` + cssURL + `"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Targets and raw errors can reveal internal hosts; the page shows neither.
	for _, leak := range []string{"internal.example", "10.0.0.7", "database is locked", "<script"} {
		if strings.Contains(page, leak) {
			t.Errorf("page contains %q", leak)
		}
	}
	if csp := resp.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
}

func TestAPI(t *testing.T) {
	var got apiStatus
	if err := json.Unmarshal([]byte(serve(t, "/api/status").body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != stateDown || len(got.Checks) != 3 {
		t.Fatalf("status %q with %d checks, want down with 3", got.Status, len(got.Checks))
	}
	if got.Version != "4b825dc" {
		t.Errorf("version = %q, want the source's", got.Version)
	}
	api := got.Checks[0]
	if api.State != stateDown || api.LastProbe == nil || api.LastProbe.OK || api.LastProbe.Reason != probe.ReasonTimeout ||
		api.SLO == nil || api.SLO.FailedProbes != 2 || api.FailingSince.IsZero() {
		t.Errorf("apartment-api = %+v, want down with its last probe and SLO", api)
	}
	if api.SLO != nil && !api.SLO.Since.Equal(now.Add(-3*time.Hour)) {
		t.Errorf("SLO since = %v, want the check's first probe 3 hours ago", api.SLO.Since)
	}
	if api.FailingSince.Location() != time.UTC {
		t.Errorf("failing_since is in %v, want UTC like every other timestamp", api.FailingSince.Location())
	}
	if cache := got.Checks[1]; cache.State != stateUp || cache.SLO != nil || !cache.FailingSince.IsZero() {
		t.Errorf("cache = %+v, want up without an SLO", cache)
	}
}

func TestStylesheetIsCachedForGood(t *testing.T) {
	resp := serve(t, cssURL)
	if resp.status != http.StatusOK || !strings.HasPrefix(resp.header.Get("Content-Type"), "text/css") || !strings.Contains(resp.body, "--surface") {
		t.Fatalf("GET %s = %d %s", cssURL, resp.status, resp.header.Get("Content-Type"))
	}
	if cc := resp.header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", cc)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	if resp := serve(t, "/nope"); resp.status != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", resp.status)
	}
}
