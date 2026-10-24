package probe

import (
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

type fakePings struct {
	mu   sync.Mutex
	last map[string]time.Time
	seen map[string]time.Time
}

func (f *fakePings) LastPing(check string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.last[check]
	return at, ok
}

func (f *fakePings) FirstSeen(check string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.seen[check]
	return at, ok
}

func (f *fakePings) ping(check string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last[check] = time.Now()
}

func TestHeartbeat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pings := &fakePings{last: map[string]time.Time{}}
		c := config.Check{Name: "cron", Timeout: time.Second, Heartbeat: &config.Heartbeat{Every: time.Hour, Grace: 6 * time.Minute}}
		p := New(c, Options{Pings: pings})

		// Before any ping, the clock starts when the check does, if when
		// it was added is not known.
		time.Sleep(time.Hour + 6*time.Minute)
		wantReason(t, p.Probe(t.Context()), "")
		time.Sleep(time.Second)
		r := p.Probe(t.Context())
		wantReason(t, r, ReasonHeartbeatMissed)
		if !strings.Contains(r.Err.Error(), "no ping in the 1h6m1s since the check was added, expected every 1h0m0s plus 6m0s grace") {
			t.Errorf("error = %q", r.Err)
		}

		pings.ping("cron")
		wantReason(t, p.Probe(t.Context()), "")
		time.Sleep(time.Hour + 7*time.Minute)
		r = p.Probe(t.Context())
		wantReason(t, r, ReasonHeartbeatMissed)
		if !strings.Contains(r.Err.Error(), "last ping 1h7m0s ago") {
			t.Errorf("error = %q", r.Err)
		}
	})
}

func TestHeartbeatCountsFromWhenTheCheckWasAdded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Added an hour ago, before the monitor restarted or the check was
		// edited, and never pinged since.
		pings := &fakePings{seen: map[string]time.Time{"cron": time.Now().Add(-time.Hour)}}
		c := config.Check{Name: "cron", Timeout: time.Second, Heartbeat: &config.Heartbeat{Every: time.Hour, Grace: 6 * time.Minute}}
		p := New(c, Options{Pings: pings})

		wantReason(t, p.Probe(t.Context()), "")
		time.Sleep(6*time.Minute + time.Second)
		r := p.Probe(t.Context())
		wantReason(t, r, ReasonHeartbeatMissed)
		if !strings.Contains(r.Err.Error(), "no ping in the 1h6m1s since the check was added") {
			t.Errorf("error = %q", r.Err)
		}
	})
}
