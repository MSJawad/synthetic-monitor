// Package slo measures checks against their availability objectives.
//
// Availability is counted in probes: the fraction of a check's probes over
// the window that succeeded. Every probe is an equal sample, so a check
// probed every 30 seconds that fails for 10 minutes loses 20 of the 86,400
// samples in a 30-day window. Time when the monitor itself was not running
// produces no probes and so counts neither for nor against a check.
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
}

// Compute measures failed failures out of total probes against target, a
// fraction below 1. It reports false if there are no probes to measure.
func Compute(target float64, window time.Duration, total, failed int) (Status, bool) {
	if total == 0 {
		return Status{}, false
	}
	allowed := (1 - target) * float64(total)
	return Status{
		Target:          target,
		Window:          window,
		Total:           total,
		Failed:          failed,
		Availability:    float64(total-failed) / float64(total),
		BudgetRemaining: 1 - float64(failed)/allowed,
	}, true
}

// Counter counts a check's probes, and failed probes, since a time.
type Counter interface {
	Counts(ctx context.Context, check string, since time.Time) (total, failed int, err error)
}

// Evaluate returns the status of every check that has an SLO and probes in
// its window, keyed by check name. If some checks cannot be counted, it
// returns the others along with the errors.
func Evaluate(ctx context.Context, counter Counter, checks []config.Check, now time.Time) (map[string]Status, error) {
	statuses := make(map[string]Status)
	var errs []error
	for _, c := range checks {
		if c.SLO == nil {
			continue
		}
		total, failed, err := counter.Counts(ctx, c.Name, now.Add(-c.SLO.Window()))
		if err != nil {
			errs = append(errs, fmt.Errorf("counting probes of %s: %w", c.Name, err))
			continue
		}
		if s, ok := Compute(c.SLO.Ratio(), c.SLO.Window(), total, failed); ok {
			statuses[c.Name] = s
		}
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
