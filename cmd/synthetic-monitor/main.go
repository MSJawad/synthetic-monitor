// Command synthetic-monitor probes endpoints on a schedule, records their
// availability and latency, and alerts when checks break.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof" //nolint:gosec // served only by debugMux on its own opt-in listener, not on http.DefaultServeMux's
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/heartbeat"
	"github.com/MSJawad/synthetic-monitor/internal/metrics"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
	"github.com/MSJawad/synthetic-monitor/internal/slo"
	"github.com/MSJawad/synthetic-monitor/internal/store"
	"github.com/MSJawad/synthetic-monitor/internal/watchdog"
	"github.com/MSJawad/synthetic-monitor/internal/web"
)

// version identifies the build. The image build sets it to the commit with
// -ldflags "-X main.version=...".
var version = "dev"

func main() {
	err := run(context.Background(), os.Args[1:], os.Stdout)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "synthetic-monitor:", err)
		os.Exit(1)
	}
}

type options struct {
	configPath      string
	listen          string
	debugListen     string
	historyPath     string
	retentionDays   int
	maxConcurrent   int
	pollInterval    time.Duration
	shutdownTimeout time.Duration
	watchdogEvery   time.Duration
	logLevel        slog.Level
	sloInterval     time.Duration // not a flag: tests shorten it
	flushTimeout    time.Duration // not a flag
}

func (o options) retention() time.Duration {
	return time.Duration(o.retentionDays) * 24 * time.Hour
}

func parseFlags(args []string) (options, error) {
	o := options{sloInterval: 30 * time.Second, flushTimeout: 5 * time.Second}
	flags := flag.NewFlagSet("synthetic-monitor", flag.ContinueOnError)
	flags.StringVar(&o.configPath, "config", "", "path to the checks file (required)")
	flags.StringVar(&o.listen, "listen", ":8080", "address of the HTTP server for metrics and health checks")
	flags.StringVar(&o.debugListen, "debug-listen", "",
		"address for pprof profiling endpoints, such as 127.0.0.1:6060; off if empty, never expose publicly")
	flags.StringVar(&o.historyPath, "history", "history.db", "path to the SQLite database of probe results")
	flags.IntVar(&o.retentionDays, "history-retention-days", 35, "days of probe results to keep; must cover every SLO window")
	flags.IntVar(&o.maxConcurrent, "max-concurrent-probes", 64, "maximum number of probes in flight at once")
	flags.DurationVar(&o.pollInterval, "config-poll-interval", 5*time.Second, "how often to check the checks file for changes")
	flags.DurationVar(&o.shutdownTimeout, "shutdown-timeout", 30*time.Second,
		"how long probes in flight get to finish after SIGTERM before they are aborted")
	flags.DurationVar(&o.watchdogEvery, "watchdog-interval", time.Minute, "how often to request WATCHDOG_URL, if it is set")
	flags.TextVar(&o.logLevel, "log-level", slog.LevelInfo, "minimum log level: debug, info, warn or error")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	switch {
	case o.configPath == "":
		return o, errors.New("-config is required")
	case o.maxConcurrent < 1:
		return o, errors.New("-max-concurrent-probes must be at least 1")
	case o.retentionDays < 1:
		return o, errors.New("-history-retention-days must be at least 1")
	case o.pollInterval <= 0 || o.shutdownTimeout <= 0 || o.watchdogEvery <= 0:
		return o, errors.New("-config-poll-interval, -shutdown-timeout and -watchdog-interval must be positive")
	}
	return o, nil
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	o, err := parseFlags(args)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: o.logLevel}))

	// An invalid config at startup is fatal, so a bad deploy fails visibly
	// instead of running with no checks.
	watcher, cfg, err := config.NewWatcher(o.configPath, os.LookupEnv)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	heartbeatToken := os.Getenv("HEARTBEAT_TOKEN")
	if err := checkConfig(cfg, o.retention(), heartbeatToken); err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	notifier, err := newNotifier(os.Getenv("NTFY_URL"), os.Getenv("NTFY_TOKEN"), logger)
	if err != nil {
		return err
	}
	watchdogURL := os.Getenv("WATCHDOG_URL")
	if watchdogURL == "" {
		logger.Warn("WATCHDOG_URL is not set, so nothing outside will notice if the monitor stops")
	} else if err := watchdog.Validate(watchdogURL); err != nil {
		return fmt.Errorf("WATCHDOG_URL: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once shutdown starts, restore default signal handling so a second
	// Ctrl-C kills the process instead of waiting for the graceful path.
	context.AfterFunc(ctx, stop)
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	history, err := store.Open(ctx, o.historyPath, store.Options{Retention: o.retention(), Logger: logger})
	if err != nil {
		return fmt.Errorf("opening history: %w", err)
	}
	defer history.Close(context.WithoutCancel(ctx)) //nolint:errcheck // closed with a deadline during shutdown; this only covers early returns

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if o.debugListen != "" {
		dln, err := lc.Listen(ctx, "tcp", o.debugListen)
		if err != nil {
			return fmt.Errorf("debug listen: %w", err)
		}
		debug := &http.Server{Handler: debugMux(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = debug.Serve(dln) }()
		defer debug.Close()
		logger.Info("debug server listening", "addr", dln.Addr().String())
	}
	svc := &service{opts: o, logger: logger, watcher: watcher, history: history, notifier: notifier, reload: reload,
		heartbeatToken: heartbeatToken, watchdogURL: watchdogURL}
	return svc.serve(ctx, ln, cfg)
}

// newNotifier returns the ntfy notifier for topicURL, or, if it is empty, a
// notifier that only logs. Both settings come from the environment, so the
// topic, which anyone who knows it can read, can be kept in a secret.
func newNotifier(topicURL, token string, logger *slog.Logger) (alert.Notifier, error) {
	if topicURL == "" {
		logger.Warn("NTFY_URL is not set, so notifications will only be logged")
		return alert.LogNotifier{Logger: logger}, nil
	}
	n, err := alert.NewNtfy(topicURL, token)
	if err != nil {
		return nil, fmt.Errorf("NTFY_URL: %w", err)
	}
	return n, nil
}

// checkConfig rejects configs that are valid on their own but not with how
// the monitor runs:
//   - an SLO window longer than the history kept, which would silently
//     compute SLOs over less data than they claim;
//   - a heartbeat check without HEARTBEAT_TOKEN, which nothing could ping.
func checkConfig(cfg *config.Config, retention time.Duration, heartbeatToken string) error {
	var errs []error
	for _, c := range cfg.Checks {
		if c.SLO != nil && c.SLO.Window() > retention {
			errs = append(errs, fmt.Errorf("check %q: slo.window_days %d exceeds the %d days of history kept (-history-retention-days)",
				c.Name, c.SLO.WindowDays, int(retention.Hours()/24)))
		}
		if c.Heartbeat != nil && heartbeatToken == "" {
			errs = append(errs, fmt.Errorf("check %q: heartbeat checks need HEARTBEAT_TOKEN set, or nothing can ping them", c.Name))
		}
	}
	return errors.Join(errs...)
}

// service is the running monitor: what serve needs, opened by run.
type service struct {
	opts     options
	logger   *slog.Logger
	watcher  *config.Watcher
	history  *store.Store
	notifier alert.Notifier
	reload   <-chan os.Signal
	// heartbeatToken authenticates pings; empty disables the endpoint.
	heartbeatToken string
	// watchdogURL is requested regularly while the monitor runs; empty
	// disables it.
	watchdogURL string
}

// serve runs the monitor with cfg until ctx is cancelled, then shuts down in
// an order that loses no results: report not ready, stop config reloads and
// SLO evaluation, let probes in flight finish (aborting them if
// shutdownTimeout runs out), then, within flushTimeout, evaluate their
// results for alerts and deliver what they raise, write the queued history,
// and stop the HTTP server last so health checks keep answering meanwhile.
func (s *service) serve(ctx context.Context, ln net.Listener, cfg *config.Config) error {
	logger := s.logger
	m := metrics.New()
	m.TrackHistory(s.history)
	alerts, err := alert.New(ctx, s.history, cfg.Checks, alert.Options{
		Notifier: s.notifier, Observer: m.AlertObserver(), Logger: logger,
	})
	if err != nil {
		return err
	}
	pings, err := heartbeat.New(ctx, s.history, m.Pinged)
	if err != nil {
		return fmt.Errorf("loading heartbeats: %w", err)
	}
	sched := scheduler.New(sinks{m, s.history, alerts, logSink{logger}}, scheduler.Options{
		MaxConcurrent: s.opts.maxConcurrent,
		Logger:        logger,
		NewProber:     func(c config.Check) probe.Prober { return probe.New(c, probe.Options{Pings: pings}) },
		OnSkip:        m.Skipped,
		OnRemoved: func(check string) {
			m.Forget(check)
			alerts.Forget(check)
			if err := pings.Forget(context.WithoutCancel(ctx), check); err != nil {
				logger.Error("forgetting a removed check's heartbeats", "check", check, "err", err)
			}
		},
	})
	m.TrackInFlight(sched.InFlight)

	var current atomic.Pointer[config.Config]
	apply := func(cfg *config.Config) {
		current.Store(cfg)
		m.SetChecks(cfg.Checks)
		alerts.SetChecks(cfg.Checks) // before probing starts, so new checks' thresholds are known
		sched.Apply(cfg.Checks)
		m.ConfigLoaded(true)
	}
	logger.Info("config loaded", "config_version", cfg.Version, "checks", len(cfg.Checks))
	apply(cfg)

	var slos atomic.Pointer[map[string]slo.Status]
	slos.Store(&map[string]slo.Status{})
	status := web.Handler(web.Source{
		Version:   version,
		Checks:    func() []config.Check { return current.Load().Checks },
		Recent:    s.history.Recent,
		SLOs:      func() map[string]slo.Status { return *slos.Load() },
		Firing:    alerts.Firing,
		LastPing:  pings.LastPing,
		Days:      s.history.Days,
		Incidents: s.history.Incidents,
	}, logger)
	var heartbeats http.Handler
	if s.heartbeatToken != "" {
		heartbeats = heartbeat.Handler(pings, s.heartbeatToken, func(name string) bool {
			for _, c := range current.Load().Checks {
				if c.Name == name {
					return c.Heartbeat != nil
				}
			}
			return false
		}, logger)
	}

	var ready atomic.Bool
	ready.Store(true)
	srv := &http.Server{
		Handler:           newMux(&ready, m.Handler(), status, heartbeats),
		ReadHeaderTimeout: 5 * time.Second, // bounds clients that send headers slowly to hold connections open
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("http server listening", "addr", ln.Addr().String(), "version", version)

	// Background work that must stop before the scheduler and history do.
	bgCtx, stopBackground := context.WithCancel(ctx)
	var background sync.WaitGroup
	background.Go(func() {
		reject := func(err error) {
			m.ConfigLoaded(false)
			logger.Error("config rejected, previous config stays in effect", "err", err)
		}
		s.watcher.Run(bgCtx, s.opts.pollInterval, s.reload,
			func(cfg *config.Config) {
				if err := checkConfig(cfg, s.opts.retention(), s.heartbeatToken); err != nil {
					reject(err)
					return
				}
				logger.Info("config reloaded", "config_version", cfg.Version, "checks", len(cfg.Checks))
				apply(cfg)
			}, reject)
	})
	if s.watchdogURL != "" {
		// Pinging stops as soon as shutdown starts: from then on the
		// monitor is no longer watching anything.
		background.Go(func() {
			watchdog.Run(bgCtx, watchdog.Options{URL: s.watchdogURL, Interval: s.opts.watchdogEvery,
				Client: &http.Client{}, Logger: logger, OnPing: m.WatchdogPinged})
		})
	}
	background.Go(func() {
		slo.Run(bgCtx, s.opts.sloInterval, s.history,
			func() []config.Check { return current.Load().Checks },
			func(statuses map[string]slo.Status) {
				m.SetSLOStatus(statuses)
				slos.Store(&statuses)
				alerts.SetBurns(burns(statuses))
			}, logger)
	})

	select {
	case err = <-serveErr:
		err = fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	ready.Store(false)
	stopBackground()
	background.Wait() // no reload can start checks from here on

	// ctx is already cancelled, so shutdown needs deadlines of its own.
	// WithoutCancel keeps ctx's values but drops its cancellation. Probes get
	// shutdownTimeout; writing results and notifications then gets
	// flushTimeout of its own, so probes that run out the clock do not cost
	// the results of those that finished.
	probesCtx, cancelProbes := context.WithTimeout(context.WithoutCancel(ctx), s.opts.shutdownTimeout)
	defer cancelProbes()
	logger.Info("waiting for probes in flight", "probes", sched.InFlight(), "timeout", s.opts.shutdownTimeout.String())
	if stopErr := sched.Stop(probesCtx); stopErr != nil {
		logger.Warn("shutdown deadline reached", "err", stopErr)
	}
	flushCtx, cancelFlush := context.WithTimeout(context.WithoutCancel(ctx), s.opts.flushTimeout)
	defer cancelFlush()
	if closeErr := alerts.Close(flushCtx); closeErr != nil {
		logger.Warn("shutdown deadline reached", "err", closeErr)
	}
	if closeErr := s.history.Close(flushCtx); closeErr != nil {
		err = errors.Join(err, fmt.Errorf("closing history: %w", closeErr))
	}
	if shutErr := srv.Shutdown(flushCtx); shutErr != nil {
		_ = srv.Close()
		err = errors.Join(err, fmt.Errorf("http shutdown: %w", shutErr))
	}
	logger.Info("shutdown complete")
	return err
}

// newMux routes the server's endpoints. heartbeats is nil when pings are
// disabled.
// burns turns SLO statuses into the burn evaluations the alert manager
// takes: for each check evaluated, the rule that fires, or nil.
func burns(statuses map[string]slo.Status) map[string]*alert.Burn {
	out := make(map[string]*alert.Burn, len(statuses))
	for name, s := range statuses {
		out[name] = nil
		if b := s.Burning; b != nil {
			out[name] = &alert.Burn{Failed: b.Failed, Total: b.Total, Window: b.Rule.Long, Rate: b.Rate, Since: b.Since}
		}
	}
	return out
}

func newMux(ready *atomic.Bool, metrics, status, heartbeats http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", status)
	mux.Handle("GET /metrics", metrics)
	if heartbeats != nil {
		mux.Handle("POST /heartbeat/{check}", heartbeats)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	// Readiness turns off as soon as shutdown starts, so Kubernetes stops
	// routing to the pod while probes in flight finish.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "shutting down\n")
			return
		}
		_, _ = io.WriteString(w, "ready\n")
	})
	return mux
}

// debugMux serves the runtime profiles: CPU, heap, goroutines, and the
// goroutineleak profile (Go 1.27), which lists goroutines blocked forever on
// a channel or lock that nothing can reach any more. It runs on its own
// listener so it is never exposed with the status page.
func debugMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return mux
}

// sinks passes each probe result to every sink in turn.
type sinks []scheduler.Sink

func (ss sinks) Record(r scheduler.Result) {
	for _, s := range ss {
		s.Record(r)
	}
}

// logSink logs probe results: failures at warn, successes at debug.
type logSink struct {
	logger *slog.Logger
}

func (s logSink) Record(r scheduler.Result) {
	if r.OK() {
		s.logger.Debug("probe succeeded", "check", r.Check, "type", r.Type, "duration_ms", r.Duration.Milliseconds())
		return
	}
	s.logger.Warn("probe failed", "check", r.Check, "type", r.Type, "duration_ms", r.Duration.Milliseconds(),
		"reason", r.Reason, "err", r.Err)
}
