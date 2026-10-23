package web

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

// recentProbes is how many of each check's latest probes the page shows.
const recentProbes = 60

// state is what the page says about a check, or about all of them.
type state string

const (
	statePending    state = "pending"    // no probe yet
	stateUp         state = "up"         // last probe succeeded, no alert
	stateFailing    state = "failing"    // last probe failed, below the alert threshold
	stateDown       state = "down"       // alert firing
	stateRecovering state = "recovering" // alert firing, but probes succeed again
)

func (s state) label() string {
	switch s {
	case statePending:
		return "No data"
	case stateUp:
		return "Up"
	case stateFailing:
		return "Failing"
	case stateDown:
		return "Down"
	case stateRecovering:
		return "Recovering"
	}
	return string(s)
}

// pageInput is everything known when the page renders.
type pageInput struct {
	checks []checkInput
	// incidents are the configured checks' incidents over the days the
	// page shows, newest first.
	incidents            []store.Incident
	incidentsUnavailable bool
}

// checkInput is everything known about one check when the page renders.
type checkInput struct {
	check       config.Check
	samples     []store.Sample // oldest first
	firingSince time.Time      // zero unless its alert is firing
	slo         *slo.Status
	unavailable bool // history could not be read
	lastPing    time.Time
	pinged      bool             // whether a heartbeat check has been pinged
	days        []store.Day      // daily totals over the days the page shows
	incidents   []store.Incident // the check's, newest first
	// daysUnavailable is true if the daily totals could not be read.
	daysUnavailable bool
}

type pageView struct {
	State     state
	Headline  string
	Generated string
	Checks    []checkView
	Incidents []incidentView // newest first, at most maxIncidents
	// MoreIncidents counts the incidents left out of the list.
	MoreIncidents        int
	IncidentsUnavailable bool
	// HistoryDays and NarrowDays are how many days of bars show on a wide
	// and a narrow screen.
	HistoryDays, NarrowDays int
}

type checkView struct {
	Name, Type string
	State      state
	Label      string
	Since      string // for down and recovering checks: when the incident began
	Checked    string // when the last probe ran
	// Heartbeat checks are pinged rather than probed, so they show when
	// the last ping came instead of a latency.
	Heartbeat bool
	Latency   string // the last probe's latency or "Failed"; for a heartbeat, the last ping
	Note      string // why the last probe failed
	Spark     sparkline
	SLO       *sloView
	History   historyView
	Probes    []probeRow // newest first
}

type sloView struct {
	Label        string // "Availability, last 30 days", or "since" while history is shorter
	Availability string // "99.95%"
	Target       string // "99.5%"
	Budget       string // "72% of error budget left"
	Meter        meter
}

// meter is the error budget bar: Fill is the remaining fraction as a
// percentage of its width, Level its severity.
type meter struct {
	Fill  string
	Level string // "ok", "low" or "exhausted"
}

type probeRow struct {
	Time    string
	OK      bool
	Result  string
	Latency string
}

func buildPage(in pageInput, now time.Time) pageView {
	p := pageView{Generated: now.UTC().Format("15:04:05 MST"), HistoryDays: historyDays, NarrowDays: narrowDays,
		IncidentsUnavailable: in.incidentsUnavailable}
	counts := make(map[state]int)
	for _, c := range in.checks {
		v := buildCheck(c, now)
		counts[v.State]++
		p.Checks = append(p.Checks, v)
	}
	p.State, p.Headline = headline(counts, len(in.checks))
	listed := in.incidents[:min(len(in.incidents), maxIncidents)]
	p.Incidents, p.MoreIncidents = buildIncidents(listed), len(in.incidents)-len(listed)
	return p
}

func headline(counts map[state]int, total int) (state, string) {
	degraded := counts[stateFailing] + counts[stateRecovering]
	switch {
	case total == 0:
		return statePending, "No checks configured"
	case counts[stateDown] > 0:
		return stateDown, plural(counts[stateDown], "check is down", "checks are down")
	case degraded > 0:
		return stateFailing, plural(degraded, "check is degraded", "checks are degraded")
	case counts[statePending] == total:
		return statePending, "Waiting for the first probes"
	}
	return stateUp, "All checks are up"
}

// buildCheck returns the view of one check. v is a named result so the
// deferred labelling applies to what is returned, not to a copy.
func buildCheck(in checkInput, now time.Time) (v checkView) {
	v = checkView{Name: in.check.Name, Type: strings.ToUpper(in.check.Type()), State: statePending}
	defer func() { v.Label = v.State.label() }()
	if in.slo != nil && in.check.SLO != nil {
		v.SLO = buildSLO(*in.slo, *in.check.SLO, now)
	}
	v.History = buildHistory(in.days, in.incidents, now)
	v.History.Unavailable = in.daysUnavailable
	if in.unavailable {
		v.Checked = "History unavailable"
		return v
	}
	if len(in.samples) == 0 {
		v.Checked = "Waiting for the first probe"
		return v
	}

	last := in.samples[len(in.samples)-1]
	firing := !in.firingSince.IsZero()
	switch {
	case firing && last.OK:
		v.State = stateRecovering
	case firing:
		v.State = stateDown
	case last.OK:
		v.State = stateUp
	default:
		v.State = stateFailing
	}
	since := clock(in.firingSince, now) + ", " + ago(now.Sub(in.firingSince))
	switch {
	case firing && in.slo != nil && in.slo.Burning != nil:
		// The incident lasts while the budget burns, so each probe's pass or
		// fail says less than the pattern does.
		v.Since = "Failing on and off since " + since
	case v.State == stateDown:
		v.Since = "Down since " + since
	case v.State == stateRecovering:
		v.Since = "Incident began " + since
	}
	v.Checked = "Checked " + ago(now.Sub(last.Start))
	switch {
	case in.check.Heartbeat != nil:
		v.Heartbeat = true
		v.Latency = "None yet"
		if in.pinged {
			v.Latency = ago(now.Sub(in.lastPing))
		}
		if !last.OK {
			v.Note = "overdue"
		}
	case last.OK:
		v.Latency = latency(last.Duration)
		v.Spark = buildSparkline(in.samples)
	default:
		v.Latency, v.Note = "Failed", strings.ToLower(reasonText(last.Reason)[:1])+reasonText(last.Reason)[1:]
		v.Spark = buildSparkline(in.samples)
	}
	for _, s := range slices.Backward(in.samples) {
		row := probeRow{Time: clock(s.Start, now), OK: s.OK, Result: "OK"}
		if !v.Heartbeat {
			row.Latency = latency(s.Duration)
		}
		if !s.OK {
			row.Result = reasonText(s.Reason)
		}
		v.Probes = append(v.Probes, row)
	}
	return v
}

func buildSLO(s slo.Status, c config.SLO, now time.Time) *sloView {
	v := &sloView{
		Label:        "Availability, last " + plural(c.WindowDays, "day", "days"),
		Availability: percent(s.Availability),
		Target:       strconv.FormatFloat(c.Target, 'f', -1, 64) + "%",
	}
	// Until the history covers the window, the figures cover less, and
	// the label says how much.
	if s.Partial {
		v.Label = "Availability since " + clock(s.Since, now)
	}
	// The epsilon keeps binary rounding from showing a used-up budget as
	// "0% left".
	switch b := s.BudgetRemaining; {
	case b <= 1e-9:
		// Counted in probes, which stays meaningful at any scale; a
		// percentage of a budget measured over a few minutes of data can
		// read "overspent by 35966%".
		allowed := int(math.Floor((1-s.Target)*float64(s.Total) + 1e-9))
		v.Budget = fmt.Sprintf("Error budget used up: %s, %d allowed",
			plural(s.Failed, "failed probe", "failed probes"), allowed)
		v.Meter = meter{Fill: "100", Level: "exhausted"}
	default:
		left := int(math.Floor(b*100 + 1e-9))
		v.Budget = fmt.Sprintf("%d%% of error budget left", left)
		v.Meter = meter{Fill: strconv.Itoa(left), Level: "ok"}
		if b < 0.25 {
			v.Meter.Level = "low"
		}
	}
	return v
}

// percent formats a fraction as a percentage, rounded down so imperfect
// availability never shows as 100%, with a third decimal from 99.9% up,
// where the differences that matter are small.
func percent(f float64) string {
	if f >= 1 {
		return "100%"
	}
	scale := 100.0
	if f >= 0.999 {
		scale = 1000
	}
	// The epsilon absorbs binary rounding: 0.995 is stored as
	// 0.99499999..., which must still show as 99.5%.
	v := math.Floor(f*100*scale+1e-6) / scale
	return strconv.FormatFloat(min(v, 99.999), 'f', -1, 64) + "%"
}

func latency(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	switch {
	case ms < 10:
		return strconv.FormatFloat(math.Round(ms*10)/10, 'f', -1, 64) + " ms"
	case ms < 10000:
		return strconv.Itoa(int(math.Round(ms))) + " ms"
	}
	return strconv.FormatFloat(math.Round(ms/100)/10, 'f', -1, 64) + " s"
}

// reasonText describes a failure without the raw error, which can reveal
// internal hostnames and addresses on a public page.
func reasonText(r probe.Reason) string {
	switch r {
	case probe.ReasonTimeout:
		return "Timed out"
	case probe.ReasonCanceled:
		return "Cancelled"
	case probe.ReasonDNS:
		return "DNS lookup failed"
	case probe.ReasonConnect:
		return "Connection failed"
	case probe.ReasonTLS:
		return "TLS error"
	case probe.ReasonCertExpiry:
		return "Certificate expiring"
	case probe.ReasonHTTPStatus:
		return "Unexpected HTTP status"
	case probe.ReasonHTTPBody:
		return "Unexpected response"
	case probe.ReasonDNSAnswer:
		return "Unexpected DNS answer"
	case probe.ReasonLatency:
		return "Too slow"
	case probe.ReasonHeartbeatMissed:
		return "Ping overdue"
	case probe.ReasonOther:
		return "Error"
	}
	return "Error"
}

// clock formats t as a time of day in UTC, with the date if it is not today.
func clock(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04:05 MST")
	}
	return t.Format("Jan 2 15:04 MST")
}

func ago(d time.Duration) string {
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
