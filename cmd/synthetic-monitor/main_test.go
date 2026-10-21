package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParseFlags(t *testing.T) {
	o, err := parseFlags([]string{"-config", "checks.yaml", "-log-level", "debug"})
	if err != nil {
		t.Fatal(err)
	}
	want := options{configPath: "checks.yaml", listen: ":8080", historyPath: "history.db", retentionDays: 35,
		maxConcurrent: 64, pollInterval: 5 * time.Second, shutdownTimeout: 30 * time.Second,
		logLevel: slog.LevelDebug, sloInterval: 30 * time.Second, flushTimeout: 5 * time.Second}
	if o != want {
		t.Errorf("options = %+v, want %+v", o, want)
	}

	for _, args := range [][]string{
		{},
		{"-config", "c.yaml", "-max-concurrent-probes", "0"},
		{"-config", "c.yaml", "-history-retention-days", "0"},
		{"-config", "c.yaml", "-shutdown-timeout", "0s"},
		{"-config", "c.yaml", "-log-level", "loud"},
	} {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("parseFlags(%q) succeeded, want an error", args)
		}
	}
}

func TestHealthAndReadiness(t *testing.T) {
	var ready atomic.Bool
	mux := newMux(&ready, http.NotFoundHandler(), http.NotFoundHandler())
	status := func(path string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		return rec.Code
	}

	ready.Store(true)
	if h, r := status("/healthz"), status("/readyz"); h != http.StatusOK || r != http.StatusOK {
		t.Errorf("while running: healthz %d, readyz %d; want 200, 200", h, r)
	}
	ready.Store(false)
	if h, r := status("/healthz"), status("/readyz"); h != http.StatusOK || r != http.StatusServiceUnavailable {
		t.Errorf("while shutting down: healthz %d, readyz %d; want 200, 503", h, r)
	}
}

func TestDebugMuxServesLeakProfile(t *testing.T) {
	rec := httptest.NewRecorder()
	debugMux().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/debug/pprof/goroutineleak?debug=1", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "goroutineleak profile: total 0") {
		t.Fatalf("goroutineleak profile = %d %q, want 200 with no leaks", rec.Code, rec.Body.String())
	}
}

func TestCheckRetention(t *testing.T) {
	cfg := &config.Config{Checks: []config.Check{
		{Name: "short", SLO: &config.SLO{Target: 99, WindowDays: 30}},
		{Name: "long", SLO: &config.SLO{Target: 99, WindowDays: 60}},
		{Name: "none"},
	}}
	err := checkRetention(cfg, 35*24*time.Hour)
	if err == nil || !strings.Contains(err.Error(), `"long"`) || strings.Contains(err.Error(), `"short"`) {
		t.Fatalf("checkRetention = %v, want an error about check long only", err)
	}
}

// TestMonitor runs the service against a real HTTP target and observes it
// only through its logs, HTTP endpoints and history file, as an operator
// would.
func TestMonitor(t *testing.T) {
	t.Parallel()
	var holding atomic.Bool
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if holding.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer target.Close()
	targetAddr := strings.TrimPrefix(target.URL, "http://")

	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	web := "  - name: web\n    interval: 2s\n    timeout: 2s\n    slo: {target: 99}\n    http: {url: \"" + target.URL + "\"}\n"
	writeFile(t, path, "checks:\n"+web)
	watcher, cfg, err := config.NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}

	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	reload := make(chan os.Signal, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	historyPath := filepath.Join(dir, "history.db")
	retention := 35 * 24 * time.Hour
	history, err := store.Open(t.Context(), historyPath, store.Options{Retention: retention, FlushInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// Poll rarely so that reloads in this test happen only on SIGHUP.
	o := options{maxConcurrent: 4, pollInterval: time.Hour, shutdownTimeout: 10 * time.Second, flushTimeout: 5 * time.Second,
		retentionDays: 35, sloInterval: 50 * time.Millisecond}
	svc := &service{opts: o, logger: logger, watcher: watcher, history: history,
		notifier: alert.LogNotifier{Logger: logger}, reload: reload}
	done := make(chan error, 1)
	go func() { done <- svc.serve(ctx, ln, cfg) }()

	waitForStatus(t, base+"/readyz", http.StatusOK)
	logs.waitFor(t, "a successful probe of web", func(e []logEntry) bool { return count(e, "probe succeeded", "web") > 0 })

	// A probe flows through history and SLO evaluation into the metrics.
	waitForMetrics(t, base, "web's SLO status", func(m string) bool {
		return strings.Contains(m, `synthetic_probes_total{check="web",result="success"}`) &&
			strings.Contains(m, `synthetic_slo_availability_ratio{check="web"} 1`) &&
			strings.Contains(m, `synthetic_slo_error_budget_remaining_ratio{check="web"} 1`) &&
			strings.Contains(m, `synthetic_check_info{check="web",type="http"} 1`)
	})

	// The status page and its JSON show the check, without its target.
	status, page, err := get(t.Context(), base+"/")
	if err != nil || status != http.StatusOK || !strings.Contains(page, `<h2 class="check-name">web</h2>`) ||
		strings.Contains(page, target.URL) {
		t.Errorf("GET / = %d, %v; want the status page naming web but not its URL", status, err)
	}
	status, body, err := get(t.Context(), base+"/api/status")
	if err != nil || status != http.StatusOK || !strings.Contains(body, `"name":"web","type":"http","state":"up"`) {
		t.Errorf("GET /api/status = %d %s, %v; want web up", status, body, err)
	}

	// SIGHUP applies an edited config without a restart.
	writeFile(t, path, "checks:\n"+web+"  - name: port\n    interval: 1s\n    tcp: {address: \""+targetAddr+"\"}\n")
	reload <- syscall.SIGHUP
	logs.waitFor(t, "a successful probe of the added check", func(e []logEntry) bool { return count(e, "probe succeeded", "port") > 0 })

	// An invalid edit is rejected and the running checks carry on.
	writeFile(t, path, "checks:\n  - name: web\n    timout: 2s\n")
	reload <- syscall.SIGHUP
	logs.waitFor(t, "the invalid config to be rejected", func(e []logEntry) bool {
		return count(e, "config rejected, previous config stays in effect", "") > 0
	})
	before := count(logs.entries(), "probe succeeded", "port")
	logs.waitFor(t, "probes to continue after the rejection", func(e []logEntry) bool { return count(e, "probe succeeded", "port") > before })
	waitForMetrics(t, base, "the rejection to be reported", func(m string) bool {
		return strings.Contains(m, "synthetic_config_last_reload_successful 0")
	})

	// Removing a check removes its series once it has stopped.
	writeFile(t, path, "checks:\n"+web)
	reload <- syscall.SIGHUP
	waitForMetrics(t, base, "the removed check's series to go", func(m string) bool {
		return !strings.Contains(m, `check="port"`) && strings.Contains(m, "synthetic_config_last_reload_successful 1")
	})

	// Shutdown reports not ready and waits for the probe in flight.
	holding.Store(true)
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no probe reached the target")
	}
	cancel()
	waitForStatus(t, base+"/readyz", http.StatusServiceUnavailable)
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("monitor did not return after the probe in flight finished")
	}
	entries := logs.entries()
	shutdown := index(entries, "shutting down")
	if shutdown < 0 || count(entries[shutdown:], "probe succeeded", "web") != 1 {
		t.Errorf("the probe in flight at shutdown was not recorded after shutdown began")
	}
	if i := index(entries, "waiting for probes in flight"); i < 0 || entries[i].Probes != 1 {
		t.Errorf("shutdown did not log that it was waiting for the 1 probe in flight")
	}
	if index(entries, "shutdown deadline reached") >= 0 {
		t.Errorf("shutdown aborted probes, want them to finish")
	}

	// Every logged result, including the one in flight at shutdown, was
	// written to history.
	history, err = store.Open(t.Context(), historyPath, store.Options{Retention: retention})
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close(context.Background())
	for _, check := range []string{"web", "port"} {
		total, _, err := history.Counts(t.Context(), check, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if logged := count(entries, "probe succeeded", check); total != logged {
			t.Errorf("history holds %d results for %s, want the %d logged", total, check, logged)
		}
	}
}

// TestShutdownDeadline stops the service while a probe hangs past the
// shutdown timeout: that probe is aborted, but the results of the others
// are still written and shutdown succeeds.
func TestShutdownDeadline(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	hangs := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done() // until the probe is aborted
	}))
	defer hangs.Close()
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer ok.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "checks.yaml")
	writeFile(t, path, "checks:\n"+
		"  - name: hangs\n    interval: 10s\n    http: {url: \""+hangs.URL+"\"}\n"+
		"  - name: web\n    interval: 1s\n    http: {url: \""+ok.URL+"\"}\n")
	watcher, cfg, err := config.NewWatcher(path)
	if err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(dir, "history.db")
	history, err := store.Open(t.Context(), historyPath, store.Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	o := options{maxConcurrent: 4, pollInterval: time.Hour, shutdownTimeout: 200 * time.Millisecond,
		flushTimeout: 5 * time.Second, retentionDays: 1, sloInterval: time.Minute}
	svc := &service{opts: o, logger: logger, watcher: watcher, history: history,
		notifier: alert.LogNotifier{Logger: logger}, reload: make(chan os.Signal)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.serve(ctx, ln, cfg) }()

	select {
	case <-entered:
	case <-time.After(15 * time.Second): // the first probe starts within one interval
		t.Fatal("no probe reached the hanging target")
	}
	logs.waitFor(t, "a successful probe of web", func(e []logEntry) bool { return count(e, "probe succeeded", "web") > 0 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("monitor = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("monitor did not stop")
	}
	entries := logs.entries()
	if n := count(entries, "shutdown deadline reached", ""); n != 1 {
		t.Errorf("logged %d shutdown deadlines, want 1, for the hanging probe only", n)
	}

	history, err = store.Open(t.Context(), historyPath, store.Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer history.Close(context.Background())
	total, _, err := history.Counts(t.Context(), "web", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if logged := count(entries, "probe succeeded", "web"); total != logged {
		t.Errorf("history holds %d results for web, want the %d logged", total, logged)
	}
}

// waitForMetrics polls /metrics until done reports true.
func waitForMetrics(t *testing.T, base, what string, done func(string) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body, err := get(t.Context(), base+"/metrics")
		if err == nil && status == http.StatusOK && done(body) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; last /metrics:\n%s", what, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitForStatus polls url until it returns want.
func waitForStatus(t *testing.T, url string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, _, err := get(t.Context(), url)
		if err == nil && status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s = (%d, %v), want %d", url, status, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// get performs a GET on a connection that is closed afterwards, so no idle
// keep-alive goroutines outlive the test.
func get(ctx context.Context, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

// logBuffer collects JSON log lines and is safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

type logEntry struct {
	Msg    string `json:"msg"`
	Check  string `json:"check"`
	Probes int    `json:"probes"`
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) entries() []logEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	var entries []logEntry
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		var e logEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

// waitFor polls the logs until done reports true.
func (b *logBuffer) waitFor(t *testing.T, what string, done func([]logEntry) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done(b.entries()) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; logs:\n%s", what, b.buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// count returns how many entries have msg and, if check is not empty, check.
func count(entries []logEntry, msg, check string) int {
	n := 0
	for _, e := range entries {
		if e.Msg == msg && (check == "" || e.Check == check) {
			n++
		}
	}
	return n
}

func index(entries []logEntry, msg string) int {
	for i, e := range entries {
		if e.Msg == msg {
			return i
		}
	}
	return -1
}
