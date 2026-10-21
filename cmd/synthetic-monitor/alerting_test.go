package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

// Target behaviours.
const (
	healthy int32 = iota
	flapping
	down
)

// TestAlerting is the v1 acceptance criterion in miniature: break a
// monitored service on purpose and get exactly one alert and one recovery
// notice, through a flapping phase and a restart of the monitor mid-incident.
func TestAlerting(t *testing.T) {
	t.Parallel()
	var mode atomic.Int32
	var requests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := requests.Add(1)
		if m := mode.Load(); m == down || (m == flapping && n%2 == 0) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer target.Close()
	ntfy := newFakeNtfy(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	writeFile(t, path, "checks:\n  - name: app\n    interval: 1s\n    timeout: 1s\n"+
		"    failure_threshold: 3\n    success_threshold: 2\n    http: {url: \""+target.URL+"\"}\n")
	historyPath := filepath.Join(dir, "history.db")

	monitor := startMonitor(t, path, historyPath, ntfy.URL+"/alerts-topic")
	probesSeen := func() int64 { return requests.Load() }

	// A flapping target never fails three probes in a row.
	mode.Store(flapping)
	waitForProbes(t, probesSeen, 6)
	if got := ntfy.titles(); len(got) != 0 {
		t.Fatalf("notifications while flapping: %q, want none", got)
	}

	// An outage alerts once, however long it lasts.
	mode.Store(down)
	ntfy.waitFor(t, 1)
	waitForProbes(t, probesSeen, 3)

	// Restart the monitor mid-incident.
	monitor.stop(t)
	monitor = startMonitor(t, path, historyPath, ntfy.URL+"/alerts-topic")
	waitForProbes(t, probesSeen, 3)
	if got := ntfy.titles(); len(got) != 1 {
		t.Fatalf("notifications after restarting during the outage: %q, want only the first alert", got)
	}

	// Recovery is announced once.
	mode.Store(healthy)
	ntfy.waitFor(t, 2)
	waitForProbes(t, probesSeen, 4)
	monitor.stop(t)

	want := []string{"app is failing", "app recovered"}
	if got := ntfy.titles(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("notifications = %q, want exactly %q", got, want)
	}
}

type runningMonitor struct {
	cancel context.CancelFunc
	done   chan error
}

func startMonitor(t *testing.T, path, historyPath, ntfyURL string) *runningMonitor {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	watcher, cfg, err := config.NewWatcher(path, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	history, err := store.Open(t.Context(), historyPath, store.Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	notifier, err := alert.NewNtfy(ntfyURL, "")
	if err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := options{maxConcurrent: 4, pollInterval: time.Hour, shutdownTimeout: 10 * time.Second, flushTimeout: 5 * time.Second,
		retentionDays: 1, sloInterval: time.Minute}
	svc := &service{opts: o, logger: logger, watcher: watcher, history: history, notifier: notifier,
		reload: make(chan os.Signal)}
	ctx, cancel := context.WithCancel(t.Context())
	m := &runningMonitor{cancel: cancel, done: make(chan error, 1)}
	go func() { m.done <- svc.serve(ctx, ln, cfg) }()
	return m
}

func (m *runningMonitor) stop(t *testing.T) {
	t.Helper()
	m.cancel()
	select {
	case err := <-m.done:
		if err != nil {
			t.Fatalf("monitor = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("monitor did not stop")
	}
}

// waitForProbes waits until the target has seen n more probes.
func waitForProbes(t *testing.T, seen func() int64, n int64) {
	t.Helper()
	until := seen() + n
	deadline := time.Now().Add(time.Duration(n)*time.Second + 10*time.Second)
	for seen() < until {
		if time.Now().After(deadline) {
			t.Fatalf("target saw %d probes, want %d", seen(), until)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fakeNtfy records the titles of published notifications.
type fakeNtfy struct {
	*httptest.Server
	mu  sync.Mutex
	got []string
}

func newFakeNtfy(t *testing.T) *fakeNtfy {
	t.Helper()
	f := &fakeNtfy{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/alerts-topic" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.mu.Lock()
		f.got = append(f.got, r.Header.Get("Title"))
		f.mu.Unlock()
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeNtfy) titles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

func (f *fakeNtfy) waitFor(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for len(f.titles()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("got notifications %q, want %d", f.titles(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
