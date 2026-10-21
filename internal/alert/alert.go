// Package alert turns probe results into notifications: one alert when a
// check starts failing, and one recovery notice when it is healthy again.
//
// Each check has a small state machine. failure_threshold failed probes in
// a row raise the alert; further failures add nothing. success_threshold
// successful probes in a row then recover it; a failure before that resets
// the count, so a flapping target stays in one alert instead of sending a
// stream of alerts and recoveries.
package alert

import (
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

// Kind is what a notification announces.
type Kind string

// Kinds of notification.
const (
	KindAlert    Kind = "alert"
	KindRecovery Kind = "recovery"
	KindRemoved  Kind = "removed" // the check was removed from the config while its alert was firing
)

// State is a check's alerting state. The zero value is healthy.
type State struct {
	// Firing is true from the alert until the recovery notice.
	Firing bool
	// Streak counts consecutive failed probes while not firing, and
	// consecutive successful probes while firing.
	Streak int
	// StreakStart is when the first probe of the current streak started.
	StreakStart time.Time
	// FiringSince is when the failures that raised the alert began.
	FiringSince time.Time
	// LastReason and LastError describe the most recent failure.
	LastReason probe.Reason
	LastError  string
}

// IsZero reports whether s is the healthy state with nothing to remember.
func (s State) IsZero() bool {
	return !s.Firing && s.Streak == 0
}

// Equal reports whether s and o are the same state.
func (s State) Equal(o State) bool {
	return s.Firing == o.Firing && s.Streak == o.Streak &&
		s.StreakStart.Equal(o.StreakStart) && s.FiringSince.Equal(o.FiringSince) &&
		s.LastReason == o.LastReason && s.LastError == o.LastError
}

// Notification announces a change in a check's alerting state.
type Notification struct {
	// ID identifies the notification in the outbox until it is delivered.
	ID    int64     `json:"-"`
	Kind  Kind      `json:"kind"`
	Check string    `json:"check"`
	At    time.Time `json:"at"` // when the probe that caused it started
	// Since is when the incident's first failure happened.
	Since time.Time `json:"since"`
	// Until is when the first successful probe after the incident started;
	// set on recovery notices only.
	Until time.Time `json:"until,omitzero"`
	// Probes is how many failed (alert) or successful (recovery) probes in
	// a row caused the notification.
	Probes int          `json:"probes,omitempty"`
	Reason probe.Reason `json:"reason,omitempty"`
	Error  string       `json:"error,omitempty"`
}

// Thresholds are the numbers of probes in a row that change a check's state.
type Thresholds struct {
	Failures  int
	Successes int
}

// Next returns the state after r, a result of the check whose state is s,
// and the notification the change calls for, if any.
func Next(s State, r scheduler.Result, t Thresholds) (State, *Notification) {
	ok := r.OK()
	if !ok {
		s.LastReason, s.LastError = r.Reason, errorText(r)
	}
	switch {
	case !s.Firing && ok:
		return State{}, nil

	case !s.Firing: // failed
		if s.Streak == 0 {
			s.StreakStart = r.Start
		}
		s.Streak++
		if s.Streak < t.Failures {
			return s, nil
		}
		n := &Notification{Kind: KindAlert, Check: r.Check, At: r.Start, Since: s.StreakStart,
			Probes: s.Streak, Reason: s.LastReason, Error: s.LastError}
		s.Firing, s.FiringSince = true, s.StreakStart
		s.Streak, s.StreakStart = 0, time.Time{}
		return s, n

	case !ok: // failed while firing: any recovery streak starts over
		s.Streak, s.StreakStart = 0, time.Time{}
		return s, nil

	default: // succeeded while firing
		if s.Streak == 0 {
			s.StreakStart = r.Start
		}
		s.Streak++
		if s.Streak < t.Successes {
			return s, nil
		}
		return State{}, &Notification{Kind: KindRecovery, Check: r.Check, At: r.Start, Since: s.FiringSince,
			Until: s.StreakStart, Probes: s.Streak, Reason: s.LastReason, Error: s.LastError}
	}
}

// Removed returns the notification for check, in state s, being removed from
// the config at the given time, or nil if it was not firing. Without it, an
// alert for a removed check would never be resolved.
func Removed(check string, s State, at time.Time) *Notification {
	if !s.Firing {
		return nil
	}
	return &Notification{Kind: KindRemoved, Check: check, At: at, Since: s.FiringSince,
		Reason: s.LastReason, Error: s.LastError}
}

func errorText(r scheduler.Result) string {
	if r.Err == nil {
		return ""
	}
	return r.Err.Error()
}
