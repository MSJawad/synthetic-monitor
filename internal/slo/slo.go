// Package slo measures checks against their availability objectives.
//
// Availability is counted in probes: the fraction of a check's probes over
// the window that succeeded. Every probe is an equal sample, so a check
// probed every 30 seconds that fails for 10 minutes loses 20 of the 86,400
// samples in a 30-day window. Time when the monitor itself was not running
// produces no probes and so counts neither for nor against a check.
//
// Until a check's history reaches back a whole window, its figures cover
// only the time since its first probe, and its Status says so. An incident
// in a check's first hours therefore weighs as much as it did in the time
// measured, and can overspend the budget until more history dilutes it.
//
// The package also measures how fast each check is spending its budget, for
// alerting: a check failing on and off never fails enough probes in a row
// to alert, but it can burn through its budget all the same.
package slo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// Status is a check's standing against its SLO.
type Status struct {
	Target float64 // e.g. 0.995
	Window time.Duration
	Total  int // probes in the window
	Failed int
	// Availability is the fraction of probes in the window that succeeded.
	Availability float64
	// BudgetRemaining is the fraction of the window's error budget (the
	// failures the target allows) not yet used. It goes negative once the
	// budget is overspent, showing by how much.
	BudgetRemaining float64
	// Since is when the period measured begins: the start of the window or,
	// while the check's history doesn't reach back that far, its first
	// probe, in which case Partial is set.
	Since   time.Time
	Partial bool
	// Rates holds the burn rate over each of BurnWindows: how fast the
	// budget is being spent, as a multiple of the rate that would spend
	// exactly all of it over the SLO window.
	Rates map[time.Duration]float64
	// Burning is the first of BurnRules that fires, or nil.
	Burning *Burn
}

// A BurnRule fires when a check's failures, if they kept up, would spend
// Share of its whole error budget within Long, and are still that frequent
// over Short. The long window keeps one bad minute from paging anyone; the
// short one lets the alert clear soon after the failures stop. These are
// the two paging rules from Google's SRE workbook.
type BurnRule struct {
	Long, Short time.Duration
	Share       float64
}

// BurnRules are tried in order: the fast rule catches heavy failures within
// minutes, the slow one a steadier trickle within hours.
var BurnRules = []BurnRule{
	{Long: time.Hour, Short: 5 * time.Minute, Share: 0.02},
	{Long: 6 * time.Hour, Short: 30 * time.Minute, Share: 0.05},
}

// BurnWindows are the windows BurnRules measure, shortest first.
var BurnWindows = []time.Duration{5 * time.Minute, 30 * time.Minute, time.Hour, 6 * time.Hour}

// Threshold is the burn rate at which r fires for an SLO over window: the
// rate that would spend r.Share of the budget in r.Long. Over 30 days it is
// 14.4 for the fast rule and 6 for the slow one.
func (r BurnRule) Threshold(window time.Duration) float64 {
	return r.Share * float64(window) / float64(r.Long)
}

// Burn describes a burn rule that fired.
type Burn struct {
	Rule BurnRule
	// Rate, Failed and Total are over the rule's long window.
	Rate          float64
	Failed, Total int
	// Since is when the first failure in the long window started.
	Since time.Time
}

// Compute measures failed failures out of total probes against target, a
// fraction below 1. It reports false if there are no probes to measure.
func Compute(target float64, window time.Duration, total, failed int) (Status, bool) {
	if total == 0 {
		return Status{}, false
	}
	// total - target*total rather than (1-target)*total: 1 - 0.995 is
	// 0.0050000000000000044 in floating point, which would allow a sliver
	// more than 5 failures in 1000 and leave a used-up budget at "0% left".
	allowed := float64(total) - target*float64(total)
	return Status{
		Target:          target,
		Window:          window,
		Total:           total,
		Failed:          failed,
		Availability:    float64(total-failed) / float64(total),
		BudgetRemaining: 1 - float64(failed)/allowed,
	}, true
}

// Counts describes a check's probes from some time on.
type Counts struct {
	Total, Failed int
	// FirstFailure is when the earliest failed probe in the period started,
	// zero if none failed.
	FirstFailure time.Time
	// First is when the check's oldest kept probe started, whether in the
	// period or before it; zero if it has none.
	First time.Time
}

// Counter counts a check's probes since a time.
type Counter interface {
	Counts(ctx context.Context, check string, since time.Time) (Counts, error)
}

// burnRate is how fast failed failures out of total probes spend an error
// budget for target: 1 spends exactly the budget, 2 twice as fast.
func burnRate(target float64, c Counts) float64 {
	if c.Total == 0 {
		return 0
	}
	// As in Compute, total - target*total avoids 1-target's rounding.
	return float64(c.Failed) / (float64(c.Total) - target*float64(c.Total))
}

// burning returns the first burn rule that fires for s, or nil. A rule
// also needs minFailures failures in its long window, so that one failed
// probe of a check probed a few times an hour can't page anyone.
func burning(s Status, windows map[time.Duration]Counts, minFailures int) *Burn {
	for _, r := range BurnRules {
		threshold := r.Threshold(s.Window)
		long := windows[r.Long]
		if long.Failed < minFailures || s.Rates[r.Long] < threshold || s.Rates[r.Short] < threshold {
			continue
		}
		return &Burn{Rule: r, Rate: s.Rates[r.Long], Failed: long.Failed, Total: long.Total, Since: long.FirstFailure}
	}
	return nil
}

// Evaluate returns the status of every check that has an SLO and probes in
// its window, keyed by check name. If some checks cannot be counted, it
// returns the others along with the errors.
func Evaluate(ctx context.Context, counter Counter, checks []config.Check, now time.Time) (map[string]Status, error) {
	statuses := make(map[string]Status)
	var errs []error
checks:
	for _, c := range checks {
		if c.SLO == nil {
			continue
		}
		start := now.Add(-c.SLO.Window())
		all, err := counter.Counts(ctx, c.Name, start)
		if err != nil {
			errs = append(errs, fmt.Errorf("counting probes of %s: %w", c.Name, err))
			continue
		}
		s, ok := Compute(c.SLO.Ratio(), c.SLO.Window(), all.Total, all.Failed)
		if !ok {
			continue
		}
		// The check's first probe ever decides, not its first in the window:
		// if the monitor was down when the window began, the window is still
		// covered, and that time counts for neither side.
		s.Since, s.Partial = start, all.First.After(start)
		if s.Partial {
			s.Since = all.First
		}
		windows := make(map[time.Duration]Counts, len(BurnWindows))
		s.Rates = make(map[time.Duration]float64, len(BurnWindows))
		for _, w := range BurnWindows {
			counts, err := counter.Counts(ctx, c.Name, now.Add(-w))
			if err != nil {
				// Left out entirely rather than reported as not burning,
				// which could end an incident the check is still in.
				errs = append(errs, fmt.Errorf("counting probes of %s: %w", c.Name, err))
				continue checks
			}
			windows[w], s.Rates[w] = counts, burnRate(s.Target, counts)
		}
		s.Burning = burning(s, windows, max(c.FailureThreshold, 1))
		statuses[c.Name] = s
	}
	return statuses, errors.Join(errs...)
}

// Run evaluates the current checks immediately and then every interval
// until ctx is done, passing each round's statuses to publish.
func Run(ctx context.Context, interval time.Duration, counter Counter, checks func() []config.Check,
	publish func(map[string]Status), logger *slog.Logger,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		statuses, err := Evaluate(ctx, counter, checks(), time.Now())
		if err != nil && ctx.Err() == nil {
			logger.Error("evaluating SLOs", "err", err)
		}
		publish(statuses)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
