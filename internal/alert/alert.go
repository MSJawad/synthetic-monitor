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
//
// An incident starts when enough probes fail in a row, or when the check's
// error budget is burning too fast, as it does when probes fail on and off.
// It ends only once enough probes in a row have succeeded and the budget
// has stopped burning, so a check failing on and off stays in one incident
// rather than alerting and recovering over and over.
type State struct {
	// Firing is true from the alert until the recovery notice.
	Firing bool
	// Burning is true while an error budget burn rule fires for the check,
	// which holds back the recovery.
	Burning bool
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
	return !s.Firing && !s.Burning && s.Streak == 0
}

// Equal reports whether s and o are the same state.
func (s State) Equal(o State) bool {
	return s.Firing == o.Firing && s.Burning == o.Burning && s.Streak == o.Streak &&
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
	// Burn is set on an alert that a burn rule raised.
	Burn *Burn `json:"burn,omitempty"`
}

// Burn describes an error budget burn rule that fires for a check: Failed
// of the Total probes over the last Window failed, the first at Since,
// spending the budget Rate times as fast as the SLO allows.
type Burn struct {
	Failed int           `json:"failed"`
	Total  int           `json:"total"`
	Window time.Duration `json:"window"`
	Rate   float64       `json:"rate"`
	Since  time.Time     `json:"since"`
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
		if s.Streak < t.Successes || s.Burning {
			return s, nil
		}
		return State{}, recovery(r.Check, r.Start, s)
	}
}

// NextBurn returns the state after an SLO evaluation of check, whose state
// is s: b is the burn rule that fires, or nil if none does. A burn raises
// an alert if the check isn't firing already, and once it clears, a
// recovery it was holding back goes out.
func NextBurn(s State, check string, b *Burn, at time.Time, t Thresholds) (State, *Notification) {
	switch {
	case b != nil && !s.Firing:
		n := &Notification{Kind: KindAlert, Check: check, At: at, Since: b.Since,
			Reason: s.LastReason, Error: s.LastError, Burn: b}
		return State{Firing: true, Burning: true, FiringSince: b.Since, LastReason: s.LastReason, LastError: s.LastError}, n
	case b != nil:
		s.Burning = true
		return s, nil
	case !s.Burning:
		return s, nil
	}
	s.Burning = false
	if s.Firing && s.Streak >= t.Successes {
		return State{}, recovery(check, at, s)
	}
	return s, nil
}

// recovery returns the recovery notice ending s's incident.
func recovery(check string, at time.Time, s State) *Notification {
	return &Notification{Kind: KindRecovery, Check: check, At: at, Since: s.FiringSince,
		Until: s.StreakStart, Probes: s.Streak, Reason: s.LastReason, Error: s.LastError}
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
