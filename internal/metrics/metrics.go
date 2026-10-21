// Package metrics exposes probe results and the service's own health as
// Prometheus metrics.
package metrics

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
)

const namespace = "synthetic"

// durationBuckets cover a local probe (milliseconds) up to the longest
// sensible timeout.
var durationBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60}

// Metrics holds every metric the service exports. Per-check series carry
// only a check label; the check's type is on synthetic_check_info, so a
// reload that changes a check's type does not leave stale series behind.
type Metrics struct {
	registry *prometheus.Registry

	probeDuration *prometheus.HistogramVec
	probes        *prometheus.CounterVec
	success       *prometheus.GaugeVec
	httpPhase     *prometheus.HistogramVec
	certExpiry    *prometheus.GaugeVec
	checkInfo     *prometheus.GaugeVec

	lag     prometheus.Histogram
	skipped *prometheus.CounterVec

	reloads          *prometheus.CounterVec
	lastReloadOK     prometheus.Gauge
	lastReloadTimeOK prometheus.Gauge

	sloTarget       *prometheus.GaugeVec
	sloAvailability *prometheus.GaugeVec
	sloBudget       *prometheus.GaugeVec

	alertFiring          *prometheus.GaugeVec
	notificationsSent    *prometheus.CounterVec
	notificationFailures *prometheus.CounterVec
	notificationsPending prometheus.Gauge
	alertDropped         prometheus.Counter

	mu    sync.Mutex
	types map[string]string // check name to type, as last set on checkInfo
	slos  map[string]bool   // checks with SLO status series
}

// New returns Metrics registered on their own registry, along with Go
// runtime and process metrics.
func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		probeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "probe_duration_seconds",
			Help: "How long probes took, successful or not.", Buckets: durationBuckets,
		}, []string{"check"}),
		probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "probes_total",
			Help: `Probes completed, by result: "success" or the reason the probe failed.`,
		}, []string{"check", "result"}),
		success: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "probe_success",
			Help: "1 if the check's last probe succeeded, 0 if it failed.",
		}, []string{"check"}),
		httpPhase: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "probe_http_phase_duration_seconds",
			Help: "Time HTTP probes spent in each phase: dns, connect, tls, processing, transfer.", Buckets: durationBuckets,
		}, []string{"check", "phase"}),
		certExpiry: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "tls_cert_expiry_timestamp_seconds",
			Help: "When the certificate chain seen by the check's last successful handshake stops verifying, as a Unix timestamp.",
		}, []string{"check"}),
		checkInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "check_info",
			Help: "Always 1 for each configured check, labelled with its type.",
		}, []string{"check", "type"}),
		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "scheduler_lag_seconds",
			Help:    "How late probes started after they were due, mostly from waiting for a free slot.",
			Buckets: []float64{.001, .005, .01, .05, .1, .5, 1, 5, 10, 30},
		}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "scheduler_skipped_runs_total",
			Help: "Scheduled probes skipped because an earlier probe of the check overran them.",
		}, []string{"check"}),
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "config_reloads_total",
			Help: `Config loads and reloads, by result: "success" or "failure".`,
		}, []string{"result"}),
		lastReloadOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "config_last_reload_successful",
			Help: "1 if the last config load or reload succeeded, 0 if it was rejected.",
		}),
		lastReloadTimeOK: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "config_last_reload_success_timestamp_seconds",
			Help: "When the config was last loaded successfully, as a Unix timestamp.",
		}),
		sloTarget: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "slo_target_ratio",
			Help: "The check's SLO: the fraction of probes that must succeed over its window.",
		}, []string{"check"}),
		sloAvailability: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "slo_availability_ratio",
			Help: "Fraction of the check's probes that succeeded over its SLO window.",
		}, []string{"check"}),
		sloBudget: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "slo_error_budget_remaining_ratio",
			Help: "Fraction of the check's error budget left over its SLO window; negative once overspent.",
		}, []string{"check"}),
		alertFiring: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "alert_firing",
			Help: "1 while the check's alert is firing: from the alert until the recovery notice.",
		}, []string{"check"}),
		notificationsSent: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "notifications_delivered_total",
			Help: "Notifications the notifier accepted, by kind: alert, recovery or removed.",
		}, []string{"kind"}),
		notificationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "notification_delivery_failures_total",
			Help: `Failed notification deliveries, by kind and whether the failure was permanent ("true": given up on) or will be retried ("false").`,
		}, []string{"kind", "permanent"}),
		notificationsPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "notifications_pending",
			Help: "Notifications saved but not yet delivered.",
		}),
		alertDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "alert_dropped_results_total",
			Help: "Probe results not evaluated for alerting because the queue was full.",
		}),
		types: make(map[string]string),
		slos:  make(map[string]bool),
	}
	m.registry.MustRegister(
		m.probeDuration, m.probes, m.success, m.httpPhase, m.certExpiry, m.checkInfo,
		m.lag, m.skipped, m.reloads, m.lastReloadOK, m.lastReloadTimeOK,
		m.sloTarget, m.sloAvailability, m.sloBudget,
		m.alertFiring, m.notificationsSent, m.notificationFailures, m.notificationsPending, m.alertDropped,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the metrics in the Prometheus exposition format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Record updates the metrics for one probe result.
func (m *Metrics) Record(r scheduler.Result) {
	m.probeDuration.WithLabelValues(r.Check).Observe(r.Duration.Seconds())
	result, success := "success", 1.0
	if !r.OK() {
		result, success = string(r.Reason), 0
	}
	m.probes.WithLabelValues(r.Check, result).Inc()
	m.success.WithLabelValues(r.Check).Set(success)

	for phase, d := range map[string]time.Duration{
		"dns": r.Phases.DNS, "connect": r.Phases.Connect, "tls": r.Phases.TLS,
		"processing": r.Phases.Processing, "transfer": r.Phases.Transfer,
	} {
		if d > 0 { // skip phases that did not happen, such as DNS for an IP address
			m.httpPhase.WithLabelValues(r.Check, phase).Observe(d.Seconds())
		}
	}
	if !r.CertNotAfter.IsZero() {
		m.certExpiry.WithLabelValues(r.Check).Set(float64(r.CertNotAfter.Unix()))
	}
	if !r.Scheduled.IsZero() {
		m.lag.Observe(max(r.Start.Sub(r.Scheduled), 0).Seconds())
	}
}

// SetChecks records the configured checks, their types and SLO targets.
func (m *Metrics) SetChecks(checks []config.Check) {
	m.mu.Lock()
	defer m.mu.Unlock()
	types := make(map[string]string, len(checks))
	for _, c := range checks {
		types[c.Name] = c.Type()
		m.checkInfo.WithLabelValues(c.Name, c.Type()).Set(1)
		if c.SLO != nil {
			m.sloTarget.WithLabelValues(c.Name).Set(c.SLO.Ratio())
		} else {
			m.sloTarget.DeleteLabelValues(c.Name)
		}
	}
	for name, typ := range m.types {
		if types[name] != typ {
			m.checkInfo.DeleteLabelValues(name, typ)
		}
	}
	m.types = types
}

// Forget deletes every series of a check that no longer exists.
func (m *Metrics) Forget(check string) {
	labels := prometheus.Labels{"check": check}
	for _, vec := range []interface{ DeletePartialMatch(prometheus.Labels) int }{
		m.probeDuration, m.probes, m.success, m.httpPhase, m.certExpiry, m.checkInfo,
		m.skipped, m.sloTarget, m.sloAvailability, m.sloBudget, m.alertFiring,
	} {
		vec.DeletePartialMatch(labels)
	}
}

// Skipped counts scheduled runs a check skipped.
func (m *Metrics) Skipped(check string, runs int) {
	m.skipped.WithLabelValues(check).Add(float64(runs))
}

// ConfigLoaded records the outcome of a config load or reload.
func (m *Metrics) ConfigLoaded(ok bool) {
	if !ok {
		m.reloads.WithLabelValues("failure").Inc()
		m.lastReloadOK.Set(0)
		return
	}
	m.reloads.WithLabelValues("success").Inc()
	m.lastReloadOK.Set(1)
	m.lastReloadTimeOK.SetToCurrentTime()
}

// SetSLOStatus replaces the SLO status series with statuses, keyed by check.
// Checks absent from statuses, such as those without data yet, lose their
// series.
func (m *Metrics) SetSLOStatus(statuses map[string]slo.Status) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, s := range statuses {
		m.sloAvailability.WithLabelValues(name).Set(s.Availability)
		m.sloBudget.WithLabelValues(name).Set(s.BudgetRemaining)
	}
	for name := range m.slos {
		if _, ok := statuses[name]; !ok {
			m.sloAvailability.DeleteLabelValues(name)
			m.sloBudget.DeleteLabelValues(name)
		}
	}
	m.slos = make(map[string]bool, len(statuses))
	for name := range statuses {
		m.slos[name] = true
	}
}

// AlertObserver returns an alert.Observer that records alerting activity.
func (m *Metrics) AlertObserver() alert.Observer { return alertObserver{m} }

type alertObserver struct{ m *Metrics }

func (o alertObserver) Firing(check string, firing bool) {
	v := 0.0
	if firing {
		v = 1
	}
	o.m.alertFiring.WithLabelValues(check).Set(v)
}

func (o alertObserver) Delivered(kind alert.Kind) {
	o.m.notificationsSent.WithLabelValues(string(kind)).Inc()
}

func (o alertObserver) DeliveryFailed(kind alert.Kind, permanent bool) {
	o.m.notificationFailures.WithLabelValues(string(kind), strconv.FormatBool(permanent)).Inc()
}

func (o alertObserver) Pending(n int) { o.m.notificationsPending.Set(float64(n)) }

func (o alertObserver) DroppedResult() { o.m.alertDropped.Inc() }

// TrackInFlight exports the number of probes running, read from f at
// scrape time.
func (m *Metrics) TrackInFlight(f func() int) {
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: namespace, Name: "probes_in_flight",
		Help: "Probes currently running.",
	}, func() float64 { return float64(f()) }))
}

// HistoryStats reports problems writing probe history.
type HistoryStats interface {
	Dropped() uint64
	WriteErrors() uint64
}

// TrackHistory exports h's counters, read at scrape time.
func (m *Metrics) TrackHistory(h HistoryStats) {
	m.registry.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: namespace, Name: "history_dropped_results_total",
			Help: "Probe results not written to history because the write queue was full.",
		}, func() float64 { return float64(h.Dropped()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Namespace: namespace, Name: "history_write_errors_total",
			Help: "Failed writes of batches of probe results to history.",
		}, func() float64 { return float64(h.WriteErrors()) }),
	)
}
