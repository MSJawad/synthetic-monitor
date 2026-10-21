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
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
)

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
	maxConcurrent   int
	pollInterval    time.Duration
	shutdownTimeout time.Duration
	logLevel        slog.Level
}

func parseFlags(args []string) (options, error) {
	var o options
	flags := flag.NewFlagSet("synthetic-monitor", flag.ContinueOnError)
	flags.StringVar(&o.configPath, "config", "", "path to the checks file (required)")
	flags.StringVar(&o.listen, "listen", ":8080", "address of the HTTP server for health checks")
	flags.IntVar(&o.maxConcurrent, "max-concurrent-probes", 64, "maximum number of probes in flight at once")
	flags.DurationVar(&o.pollInterval, "config-poll-interval", 5*time.Second, "how often to check the checks file for changes")
	flags.DurationVar(&o.shutdownTimeout, "shutdown-timeout", 30*time.Second,
		"how long probes in flight get to finish after SIGTERM before they are aborted")
	flags.TextVar(&o.logLevel, "log-level", slog.LevelInfo, "minimum log level: debug, info, warn or error")
	if err := flags.Parse(args); err != nil {
		return o, err
	}
	switch {
	case o.configPath == "":
		return o, errors.New("-config is required")
	case o.maxConcurrent < 1:
		return o, errors.New("-max-concurrent-probes must be at least 1")
	case o.pollInterval <= 0 || o.shutdownTimeout <= 0:
		return o, errors.New("-config-poll-interval and -shutdown-timeout must be positive")
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
	watcher, cfg, err := config.NewWatcher(o.configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Once shutdown starts, restore default signal handling so a second
	// Ctrl-C kills the process instead of waiting for the graceful path.
	context.AfterFunc(ctx, stop)
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", o.listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	return monitor(ctx, ln, watcher, cfg, reload, o, logger)
}

// monitor runs the service until ctx is cancelled, then shuts down in an
// order that loses no results: report not ready, stop config reloads, let
// probes in flight finish (aborting them if o.shutdownTimeout runs out),
// and stop the HTTP server last so health checks keep answering meanwhile.
func monitor(ctx context.Context, ln net.Listener, watcher *config.Watcher, cfg *config.Config,
	reload <-chan os.Signal, o options, logger *slog.Logger,
) error {
	sched := scheduler.New(logSink{logger}, scheduler.Options{MaxConcurrent: o.maxConcurrent, Logger: logger})
	logger.Info("config loaded", "version", cfg.Version, "checks", len(cfg.Checks))
	sched.Apply(cfg.Checks)

	var ready atomic.Bool
	ready.Store(true)
	srv := &http.Server{
		Handler:           newMux(&ready),
		ReadHeaderTimeout: 5 * time.Second, // bounds clients that send headers slowly to hold connections open
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("http server listening", "addr", ln.Addr().String())

	watchCtx, stopWatching := context.WithCancel(ctx)
	var watching sync.WaitGroup
	watching.Go(func() {
		watcher.Run(watchCtx, o.pollInterval, reload,
			func(cfg *config.Config) {
				logger.Info("config reloaded", "version", cfg.Version, "checks", len(cfg.Checks))
				sched.Apply(cfg.Checks)
			},
			func(err error) {
				logger.Error("config rejected, previous config stays in effect", "err", err)
			})
	})

	var err error
	select {
	case err = <-serveErr:
		err = fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	ready.Store(false)
	stopWatching()
	watching.Wait() // no reload can start checks from here on

	// ctx is already cancelled, so shutdown needs its own deadline.
	// WithoutCancel keeps ctx's values but drops its cancellation.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.shutdownTimeout)
	defer cancel()
	if stopErr := sched.Stop(shutdownCtx); stopErr != nil {
		logger.Warn("shutdown deadline reached", "err", stopErr)
	}
	if shutErr := srv.Shutdown(shutdownCtx); shutErr != nil {
		_ = srv.Close()
		err = errors.Join(err, fmt.Errorf("http shutdown: %w", shutErr))
	}
	logger.Info("shutdown complete")
	return err
}

func newMux(ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()
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
