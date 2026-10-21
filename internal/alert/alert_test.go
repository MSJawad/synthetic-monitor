package alert

import (
	"errors"
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
	if !s.IsZero() {
		t.Errorf("state after recovery = %+v, want the zero state", s)
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
