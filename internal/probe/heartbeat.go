package probe

import (
	"context"
	"fmt"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// Pings reports when heartbeat checks were last pinged.
type Pings interface {
	LastPing(check string) (time.Time, bool)
}

// heartbeatProber passes while the check's last ping is at most every plus
// grace old. It sends nothing: the pings come to the monitor.
type heartbeatProber struct {
	check        string
	every, grace time.Duration
	pings        Pings
	// since stands in for the last ping until there is one, so a job that
	// never pings fails once a period and its grace have passed, rather
	// than never.
	since time.Time
}

func newHeartbeat(c config.Check, opts Options) *heartbeatProber {
	return &heartbeatProber{check: c.Name, every: c.Heartbeat.Every, grace: c.Heartbeat.Grace,
		pings: opts.Pings, since: time.Now()}
}

func (p *heartbeatProber) Probe(context.Context) Result {
	var last time.Time
	pinged := false
	if p.pings != nil {
		last, pinged = p.pings.LastPing(p.check)
	}
	if !pinged {
		last = p.since
	}
	age := time.Since(last)
	if age <= p.every+p.grace {
		return Result{}
	}
	expected := fmt.Sprintf("expected every %v plus %v grace", p.every, p.grace)
	if !pinged {
		return Result{Reason: ReasonHeartbeatMissed, Err: fmt.Errorf("no ping in the %v since the check started, %s", age.Round(time.Second), expected)}
	}
	return Result{Reason: ReasonHeartbeatMissed, Err: fmt.Errorf("last ping %v ago, %s", age.Round(time.Second), expected)}
}
