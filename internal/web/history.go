package web

import (
	"strconv"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/store"
)

const (
	// historyDays is how many days of history each check shows, one bar a
	// day; a narrow screen shows only the last narrowDays.
	historyDays = store.SummaryDays
	narrowDays  = 30
	// maxIncidents caps the incidents listed; the API returns them all.
	maxIncidents = 20
)

// historyView is a check's last historyDays days, one bar each.
type historyView struct {
	Days []dayView // oldest first, ending today
	// Uptime is the share of probes that succeeded over all the days, and
	// NarrowUptime over the last narrowDays; empty without any probes.
	Uptime, NarrowUptime string
	Summary              string // the chart's accessible name
	Unavailable          bool   // the totals could not be read
}

// dayView is one day's bar. Its state is down if the check was down at
// any time that day, failing if it was failing on and off, up if it was
// probed without an incident, and pending if nothing is known.
type dayView struct {
	State state
	Title string
	Older bool // before the last narrowDays, so hidden on a narrow screen
}

type incidentView struct {
	Check  string
	State  state  // down, or failing when it failed on and off
	Label  string // "Down" or "Failing on and off"
	When   string // "Oct 21, 14:02 to 14:36 UTC"
	Lasted string // "34m", or "Ongoing"
	Cause  string // the failure that raised it, if known
}

func buildHistory(days []store.Day, incidents []store.Incident, now time.Time) historyView {
	today := midnight(now)
	first := today.AddDate(0, 0, 1-historyDays)
	totals := make(map[int64]store.Day, len(days))
	for _, d := range days {
		totals[d.Start.Unix()] = d
	}
	var (
		h                                historyView
		probes, failed, narrowP, narrowF int
		incidentDays                     int
	)
	for i := range historyDays {
		start := first.AddDate(0, 0, i)
		end := start.AddDate(0, 0, 1)
		d := totals[start.Unix()]
		v := dayView{State: statePending, Older: i < historyDays-narrowDays}
		if d.Probes > 0 {
			v.State = stateUp
		}
		var count int
		var down, flapping, lasted time.Duration
		for _, in := range incidents {
			from, to := in.Start, in.End
			if to.IsZero() {
				to = now
			}
			from, to = later(from, start), earlier(to, end)
			if !to.After(from) {
				continue
			}
			count++
			lasted += to.Sub(from)
			if in.Burn {
				flapping += to.Sub(from)
			} else {
				down += to.Sub(from)
			}
		}
		switch {
		case down > 0:
			v.State = stateDown
		case flapping > 0:
			v.State = stateFailing
		}
		if count > 0 {
			incidentDays++
		}
		v.Title = dayTitle(start, today, d, count, down, flapping, lasted)
		h.Days = append(h.Days, v)

		probes, failed = probes+d.Probes, failed+d.Failed
		if !v.Older {
			narrowP, narrowF = narrowP+d.Probes, narrowF+d.Failed
		}
	}
	if probes > 0 {
		h.Uptime = percent(float64(probes-failed) / float64(probes))
	}
	if narrowP > 0 {
		h.NarrowUptime = percent(float64(narrowP-narrowF) / float64(narrowP))
	}
	h.Summary = "Last " + strconv.Itoa(historyDays) + " days: "
	if h.Uptime != "" {
		h.Summary += h.Uptime + " uptime, "
	}
	h.Summary += plural(incidentDays, "day", "days") + " with incidents"
	return h
}

// dayTitle reads like "Oct 21: 99.93% up, down for 34m".
func dayTitle(start, today time.Time, d store.Day, incidents int, down, flapping, lasted time.Duration) string {
	t := start.Format("Jan 2")
	if start.Equal(today) {
		t = "Today"
	}
	if d.Probes == 0 && incidents == 0 {
		return t + ": no data"
	}
	t += ": "
	if d.Probes > 0 {
		t += percent(float64(d.Probes-d.Failed)/float64(d.Probes)) + " up, "
	}
	switch {
	case incidents == 0:
		return t + "no incidents"
	case incidents > 1:
		return t + strconv.Itoa(incidents) + " incidents, " + span(lasted) + " in all"
	case down > 0:
		return t + "down for " + span(down)
	}
	return t + "failing on and off for " + span(flapping)
}

func buildIncidents(incidents []store.Incident) []incidentView {
	var views []incidentView
	for _, in := range incidents {
		v := incidentView{Check: in.Check, State: stateDown, Label: "Down", Lasted: "Ongoing"}
		if in.Burn {
			v.State, v.Label = stateFailing, "Failing on and off"
		}
		if in.Reason != "" {
			v.Cause = reasonText(in.Reason)
		}
		start, end := in.Start.UTC(), in.End.UTC()
		// To the minute, unless that would make a short incident read
		// "21:54 to 21:54".
		layout := "15:04"
		if start.Truncate(time.Minute).Equal(end.Truncate(time.Minute)) {
			layout = "15:04:05"
		}
		switch {
		case in.End.IsZero():
			v.When = "Since " + start.Format("Jan 2, 15:04 MST")
		case midnight(start).Equal(midnight(end)):
			v.When = start.Format("Jan 2, "+layout) + " to " + end.Format(layout+" MST")
			v.Lasted = span(end.Sub(start))
		default:
			v.When = start.Format("Jan 2, 15:04") + " to " + end.Format("Jan 2, 15:04 MST")
			v.Lasted = span(end.Sub(start))
		}
		views = append(views, v)
	}
	return views
}

// span formats a length of time to its largest two units: "20s", "34m",
// "2h 5m" or "3d 4h". Anything shorter than a second counts as one.
func span(d time.Duration) string {
	m := int(d / time.Minute)
	switch {
	case m < 1:
		return strconv.Itoa(max(1, int(d/time.Second))) + "s"
	case m < 60:
		return strconv.Itoa(m) + "m"
	case m < 24*60:
		return strconv.Itoa(m/60) + "h" + unit(m%60, "m")
	}
	return strconv.Itoa(m/(24*60)) + "d" + unit(m/60%24, "h")
}

func unit(n int, suffix string) string {
	if n == 0 {
		return ""
	}
	return " " + strconv.Itoa(n) + suffix
}

// midnight returns the start of t's day, UTC.
func midnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
