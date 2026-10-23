package web

import (
	"strconv"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

func TestHistoryBars(t *testing.T) {
	today := midnight(now) // now is 15:00
	day := func(ago int) time.Time { return today.AddDate(0, 0, -ago) }
	days := []store.Day{
		{Start: day(70), Probes: 100},
		{Start: day(5), Probes: 2880, Failed: 2},
		{Start: day(4), Probes: 2880, Failed: 300},
		{Start: day(3), Probes: 2880, Failed: 100},
		{Start: day(2), Probes: 2880},
		{Start: day(1), Probes: 2880},
		{Start: day(0), Probes: 1800},
	}
	incidents := []store.Incident{
		// Open since 14:30 today.
		{Check: "web", Start: now.Add(-30 * time.Minute)},
		// From 23:00 two days ago to 01:30 yesterday.
		{Check: "web", Start: day(1).Add(-time.Hour), End: day(1).Add(90 * time.Minute)},
		// Failing on and off, then down, four days ago.
		{Check: "web", Start: day(4).Add(2 * time.Hour), End: day(4).Add(3 * time.Hour), Burn: true},
		{Check: "web", Start: day(4).Add(5 * time.Hour), End: day(4).Add(5*time.Hour + 10*time.Minute)},
		// Failing on and off three days ago.
		{Check: "web", Start: day(3).Add(time.Hour), End: day(3).Add(3*time.Hour + 5*time.Minute), Burn: true},
	}
	h := buildHistory(days, incidents, now)
	if len(h.Days) != historyDays {
		t.Fatalf("%d days, want %d", len(h.Days), historyDays)
	}
	for ago, want := range map[int]dayView{
		89: {State: statePending, Title: "Jul 24: no data", Older: true},
		70: {State: stateUp, Title: "Aug 12: 100% up, no incidents", Older: true},
		30: {State: statePending, Title: "Sep 21: no data", Older: true},
		29: {State: statePending, Title: "Sep 22: no data"},
		5:  {State: stateUp, Title: "Oct 16: 99.93% up, no incidents"},
		4:  {State: stateDown, Title: "Oct 17: 89.58% up, 2 incidents, 1h 10m in all"},
		3:  {State: stateFailing, Title: "Oct 18: 96.52% up, failing on and off for 2h 5m"},
		2:  {State: stateDown, Title: "Oct 19: 100% up, down for 1h"},
		1:  {State: stateDown, Title: "Oct 20: 100% up, down for 1h 30m"},
		0:  {State: stateDown, Title: "Today: 100% up, down for 30m"},
	} {
		if got := h.Days[historyDays-1-ago]; got != want {
			t.Errorf("%d days ago: %+v, want %+v", ago, got, want)
		}
	}
	// 402 of 16,300 probes failed, and of the 16,200 in the last 30 days,
	// which leave out the 100 from 70 days ago.
	if h.Uptime != "97.53%" || h.NarrowUptime != "97.51%" {
		t.Errorf("uptime %s, last 30 days %s; want 97.53%% and 97.51%%", h.Uptime, h.NarrowUptime)
	}
	if want := "Last 90 days: 97.53% uptime, 5 days with incidents"; h.Summary != want {
		t.Errorf("summary %q, want %q", h.Summary, want)
	}
}

func TestHistoryWithoutProbes(t *testing.T) {
	h := buildHistory(nil, nil, now)
	if h.Uptime != "" || h.NarrowUptime != "" || h.Summary != "Last 90 days: 0 days with incidents" {
		t.Errorf("uptime %q, %q, summary %q; want no uptime without probes", h.Uptime, h.NarrowUptime, h.Summary)
	}
	for i, d := range h.Days {
		if d.State != statePending {
			t.Fatalf("day %d is %s, want pending", i, d.State)
		}
	}
}

func TestIncidentList(t *testing.T) {
	got := buildIncidents([]store.Incident{
		{Check: "web", Start: now.Add(-time.Minute)},
		{Check: "api", Start: now.Add(-3 * time.Hour), End: now.Add(-150 * time.Minute),
			Burn: true, Reason: probe.ReasonHTTPStatus},
		{Check: "db", Start: now.Add(-50 * time.Hour), End: now.Add(-21 * time.Hour), Reason: probe.ReasonConnect},
		{Check: "cdn", Start: now.Add(-5*time.Hour + 7*time.Second), End: now.Add(-5*time.Hour + 27*time.Second)},
	})
	want := []incidentView{
		{Check: "web", State: stateDown, Label: "Down", When: "Since Oct 21, 14:59 UTC", Lasted: "Ongoing"},
		{Check: "api", State: stateFailing, Label: "Failing on and off", When: "Oct 21, 12:00 to 12:30 UTC",
			Lasted: "30m", Cause: "Unexpected HTTP status"},
		{Check: "db", State: stateDown, Label: "Down", When: "Oct 19, 13:00 to Oct 20, 18:00 UTC", Lasted: "1d 5h",
			Cause: "Connection failed"},
		{Check: "cdn", State: stateDown, Label: "Down", When: "Oct 21, 10:00:07 to 10:00:27 UTC", Lasted: "20s"},
	}
	if len(got) != len(want) {
		t.Fatalf("%d incidents, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("incident %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestIncidentListIsCapped(t *testing.T) {
	var in pageInput
	for i := range maxIncidents + 5 {
		in.incidents = append(in.incidents, store.Incident{Check: "web" + strconv.Itoa(i), Start: now.Add(-time.Duration(i) * time.Hour)})
	}
	p := buildPage(in, now)
	if len(p.Incidents) != maxIncidents || p.MoreIncidents != 5 || p.Incidents[0].Check != "web0" {
		t.Errorf("%d incidents listed, %d more, first %s; want the latest %d and 5 more",
			len(p.Incidents), p.MoreIncidents, p.Incidents[0].Check, maxIncidents)
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		400 * time.Millisecond:          "1s",
		59 * time.Second:                "59s",
		time.Minute:                     "1m",
		59*time.Minute + 59*time.Second: "59m",
		time.Hour:                       "1h",
		2*time.Hour + 5*time.Minute:     "2h 5m",
		24 * time.Hour:                  "1d",
		77*time.Hour + 30*time.Minute:   "3d 5h",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%v) = %q, want %q", d, got, want)
		}
	}
}
