package alert

import (
	"errors"
	"math/rand/v2"
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

var t0 = time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)

// probeAt returns the result of the i-th probe, 30s apart, failing if fail.
func probeAt(i int, fail bool) scheduler.Result {
	r := scheduler.Result{Check: "web", Type: "http", Start: t0.Add(time.Duration(i) * 30 * time.Second)}
	if fail {
		r.Reason, r.Err = probe.ReasonHTTPStatus, errors.New("status 500, want 2xx")
	}
	return r
}

// replay feeds a sequence of results ('F' failed, 'S' succeeded) through Next
// and returns the kinds of notification sent, as a string like "A.R".
func replay(seq string, t Thresholds) (string, []*Notification, State) {
	var s State
	var kinds strings.Builder
	var sent []*Notification
	for i, c := range seq {
		var n *Notification
		s, n = Next(s, probeAt(i, c == 'F'), t)
		switch {
		case n == nil:
			kinds.WriteByte('.')
		case n.Kind == KindAlert:
			kinds.WriteByte('A')
		case n.Kind == KindRecovery:
			kinds.WriteByte('R')
		}
		if n != nil {
			sent = append(sent, n)
		}
	}
	return kinds.String(), sent, s
}

func TestNextSequences(t *testing.T) {
	tests := []struct {
		name      string
		seq       string
		threshold Thresholds
		want      string // per probe: '.' nothing, 'A' alert, 'R' recovery
	}{
		{"healthy", "SSSS", Thresholds{3, 2}, "...."},
		{"blips below the threshold never alert", "SFFSFFSFS", Thresholds{3, 2}, "........."},
		{"third failure in a row alerts", "SFFF", Thresholds{3, 2}, "...A"},
		{"further failures stay quiet", "FFFFFFF", Thresholds{3, 2}, "..A...."},
		{"recovery needs two successes", "FFFSS", Thresholds{3, 2}, "..A.R"},
		{"flapping stays in one alert", "FFFSFSFSFSS", Thresholds{3, 2}, "..A.......R"},
		{"a new incident alerts again", "FFFSSFFF", Thresholds{3, 2}, "..A.R..A"},
		{"thresholds of one", "FSFS", Thresholds{1, 1}, "ARAR"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _, _ := replay(tt.seq, tt.threshold); got != tt.want {
				t.Errorf("replay(%s) = %s, want %s", tt.seq, got, tt.want)
			}
		})
	}
}

func TestNotificationContents(t *testing.T) {
	// Probes 0-1 succeed; 2-4 fail (alert at 4); 5 succeeds; 6 fails; 7-8
	// succeed (recovery at 8).
	_, sent, s := replay("SSFFFSFSS", Thresholds{3, 2})
	if len(sent) != 2 {
		t.Fatalf("sent %d notifications, want 2", len(sent))
	}
	alert, recovery := sent[0], sent[1]

	if alert.Since != probeAt(2, true).Start || alert.At != probeAt(4, true).Start || alert.Probes != 3 {
		t.Errorf("alert = %+v; want since probe 2, at probe 4, after 3 probes", alert)
	}
	if alert.Reason != probe.ReasonHTTPStatus || alert.Error != "status 500, want 2xx" {
		t.Errorf("alert reason %q, error %q; want the last failure's", alert.Reason, alert.Error)
	}
	if recovery.Since != alert.Since || recovery.Until != probeAt(7, false).Start || recovery.Probes != 2 {
		t.Errorf("recovery = %+v; want since the incident began, until probe 7: the success that started the streak", recovery)
	}
	if !s.Equal(State{RecoveredAt: recovery.Until}) {
		t.Errorf("state after recovery = %+v, want healthy, remembering when it recovered", s)
	}
}

func TestRemoved(t *testing.T) {
	if n := Removed("web", State{}, t0); n != nil {
		t.Errorf("Removed of a healthy check = %+v, want nil", n)
	}
	if n := Removed("web", State{Streak: 2}, t0); n != nil {
		t.Errorf("Removed of a check failing below its threshold = %+v, want nil", n)
	}
	firing := State{Firing: true, FiringSince: t0.Add(-time.Hour), LastReason: probe.ReasonTimeout}
	n := Removed("web", firing, t0)
	if n == nil || n.Kind != KindRemoved || n.Since != firing.FiringSince || n.Reason != probe.ReasonTimeout {
		t.Errorf("Removed of a firing check = %+v, want a removal notice for its incident", n)
	}
}

func TestStateEqualIgnoresClockReadings(t *testing.T) {
	now := time.Now() // carries a monotonic clock reading
	a := State{Streak: 1, StreakStart: now}
	b := State{Streak: 1, StreakStart: now.Round(0)} // as loaded from storage
	if !a.Equal(b) {
		t.Error("states differing only in monotonic clock reading are not Equal")
	}
	if a.Equal(State{Streak: 2, StreakStart: now}) {
		t.Error("states with different streaks are Equal")
	}
}

var burn = &Burn{Failed: 20, Total: 120, Window: time.Hour, Rate: 33.3, Since: t0}

func TestNextBurnRaisesAnAlert(t *testing.T) {
	s := State{LastReason: probe.ReasonHTTPStatus, LastError: "status 500, want 2xx"}
	at := t0.Add(10 * time.Minute)
	next, n := NextBurn(s, "web", burn, at, Thresholds{3, 2})
	if !next.Firing || !next.Burning || !next.FiringSince.Equal(t0) {
		t.Errorf("state = %+v; want firing and burning since the first failure", next)
	}
	if n == nil || n.Kind != KindAlert || n.Burn != burn || !n.At.Equal(at) || !n.Since.Equal(t0) ||
		n.Reason != probe.ReasonHTTPStatus {
		t.Errorf("notification = %+v; want an alert carrying the burn and the latest failure", n)
	}

	// Already firing: the burn is noted, and nothing is sent.
	firing := State{Firing: true, FiringSince: t0}
	if next, n := NextBurn(firing, "web", burn, at, Thresholds{3, 2}); n != nil || !next.Burning {
		t.Errorf("firing check: state %+v, notification %+v; want burning and nothing sent", next, n)
	}
	// Not burning, and wasn't: nothing changes.
	if next, n := NextBurn(firing, "web", nil, at, Thresholds{3, 2}); n != nil || !next.Equal(firing) {
		t.Errorf("no burn: state %+v, notification %+v; want no change", next, n)
	}
}

func TestBurningHoldsTheRecoveryUntilItClears(t *testing.T) {
	th := Thresholds{3, 2}
	var s State
	var n *Notification
	for i, fail := range []bool{true, true, true} { // alert on the third failure
		s, _ = Next(s, probeAt(i, fail), th)
	}
	s, _ = NextBurn(s, "web", burn, probeAt(3, false).Start, th)
	for i := 4; i < 8; i++ { // four successes: enough to recover, but burning
		if s, n = Next(s, probeAt(i, false), th); n != nil {
			t.Fatalf("probe %d sent %+v while burning; want the recovery held", i, n)
		}
	}
	at := probeAt(8, false).Start
	s, n = NextBurn(s, "web", nil, at, th)
	if n == nil || n.Kind != KindRecovery || !n.Until.Equal(probeAt(4, false).Start) || !n.At.Equal(at) ||
		!s.Equal(State{RecoveredAt: n.Until}) {
		t.Errorf("burn cleared: state %+v, notification %+v; want the held recovery, back since probe 4", s, n)
	}

	// A burn that clears before the probes recover only lifts the hold.
	s = State{Firing: true, Burning: true, FiringSince: t0}
	if s, n = NextBurn(s, "web", nil, at, th); n != nil || !s.Firing || s.Burning {
		t.Errorf("burn cleared while failing: state %+v, notification %+v; want still firing, nothing sent", s, n)
	}
}

// TestBurnAfterRecovery replays an outage that alerts and recovers before
// the next SLO evaluation, which then finds the burn rule firing on the
// outage's failures.
func TestBurnAfterRecovery(t *testing.T) {
	th := Thresholds{3, 2}
	outage := func() State {
		t.Helper()
		_, sent, s := replay("FFFFSS", th)
		if len(sent) != 2 || sent[1].Kind != KindRecovery {
			t.Fatalf("outage sent %+v; want an alert and a recovery", sent)
		}
		return s
	}
	recoveredAt := probeAt(4, false).Start
	stale := &Burn{Failed: 4, Total: 6, Window: time.Hour, Rate: 50, Since: probeAt(0, true).Start}

	// Only the outage's failures: the incident is over, nothing is sent.
	s := outage()
	at := probeAt(6, false).Start
	next, n := NextBurn(s, "web", stale, at, th)
	if n != nil || next.Firing || next.Burning {
		t.Errorf("stale burn: state %+v, notification %+v; want nothing sent and the check healthy", next, n)
	}
	// Nor when it is still firing on later evaluations.
	if s, n = Next(next, probeAt(6, false), th); n != nil {
		t.Fatalf("success after the stale burn sent %+v", n)
	}
	if next, n = NextBurn(s, "web", stale, probeAt(7, false).Start, th); n != nil || next.Firing {
		t.Errorf("stale burn again: state %+v, notification %+v; want nothing sent", next, n)
	}

	// Failures after the recovery are a new incident, which begins at the
	// first of them rather than inside the last one.
	s = outage()
	for i, fail := range []bool{true, false, true, false} {
		if s, n = Next(s, probeAt(6+i, fail), th); n != nil {
			t.Fatalf("probe %d sent %+v; want failures on and off to stay below the threshold", 6+i, n)
		}
	}
	at = probeAt(10, false).Start
	next, n = NextBurn(s, "web", stale, at, th)
	if n == nil || n.Kind != KindAlert || n.Burn != stale || !n.Since.Equal(probeAt(6, true).Start) ||
		!next.Firing || !next.FiringSince.Equal(n.Since) {
		t.Errorf("burn with new failures: state %+v, notification %+v; want an alert since probe 6", next, n)
	}
	if !n.Since.After(recoveredAt) {
		t.Errorf("new incident since %v overlaps the last, which ended at %v", n.Since, recoveredAt)
	}

	// A burn whose failures all came after the recovery alerts as usual.
	s = outage()
	fresh := &Burn{Failed: 2, Total: 4, Window: time.Hour, Rate: 50, Since: probeAt(6, true).Start}
	if next, n = NextBurn(s, "web", fresh, at, th); n == nil || !n.Since.Equal(fresh.Since) || !next.Firing {
		t.Errorf("fresh burn: state %+v, notification %+v; want an alert since its first failure", next, n)
	}

	// Once the next incident has alerted, its recovery is what counts.
	_, sent, _ := replay("FFFFSSFFFS", th)
	if len(sent) != 3 || sent[2].Kind != KindAlert {
		t.Fatalf("second outage sent %+v; want alert, recovery, alert", sent)
	}
}

// TestFlappingIsOneIncident replays a check failing half its probes at
// random, as one of two replicas would, for 30 minutes, then recovering. On
// failures in a row alone it alerts and recovers over and over; with the
// fast burn rule (2% of a 30-day 99.5% budget in an hour, still burning over
// 5 minutes) it is one incident.
func TestFlappingIsOneIncident(t *testing.T) {
	const (
		flaky     = 60  // probes, 30s apart: 30 minutes failing at random
		after     = 40  // then 20 minutes healthy
		threshold = 7.2 // % of probes failing that burns at 14.4x for 99.5%
	)
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // the replay must be the same every run
	th := Thresholds{3, 2}
	var (
		failed               []bool
		plain, withBurn      State
		plainSent, burnsSent string
	)
	rate := func(last int) float64 { // % of the last probes that failed
		window := failed[max(len(failed)-last, 0):]
		n := 0
		for _, f := range window {
			if f {
				n++
			}
		}
		return 100 * float64(n) / float64(len(window))
	}
	record := func(sent *string, n *Notification) {
		if n != nil {
			*sent += string(n.Kind[0])
		}
	}
	for i := range flaky + after {
		fail := i < flaky && rng.IntN(2) == 0
		failed = append(failed, fail)
		r := probeAt(i, fail)

		var n *Notification
		plain, n = Next(plain, r, th)
		record(&plainSent, n)
		withBurn, n = Next(withBurn, r, th)
		record(&burnsSent, n)

		var b *Burn
		if rate(120) >= threshold && rate(10) >= threshold {
			b = &Burn{Window: time.Hour, Since: t0}
		}
		withBurn, n = NextBurn(withBurn, "web", b, r.Start, th)
		record(&burnsSent, n)
	}
	if len(plainSent) < 4 {
		t.Errorf("failures in a row alone sent %q; the replay should show it flapping", plainSent)
	}
	if burnsSent != "ar" {
		t.Errorf("with the burn rule, sent %q; want one alert and one recovery", burnsSent)
	}
}

func TestRenderBurnAlert(t *testing.T) {
	n := Notification{Kind: KindAlert, Check: "web", Burn: burn, Reason: probe.ReasonHTTPStatus, Error: "status 500, want 2xx"}
	title, body, priority, _ := render(n)
	want := "20 of the 120 probes in the last hour failed, the first at 2026-10-21 14:00:00 UTC. " +
		"That spends the error budget 33 times as fast as the SLO allows.\nLatest failure (http_status): status 500, want 2xx"
	if title != "web is failing on and off" || body != want || priority != "high" {
		t.Errorf("render = %q, %q, %q", title, body, priority)
	}
}
