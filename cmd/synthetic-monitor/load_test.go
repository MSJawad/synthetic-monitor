package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/alert"
	"github.com/MSJawad/synthetic-monitor/internal/config"
	"github.com/MSJawad/synthetic-monitor/internal/metrics"
	"github.com/MSJawad/synthetic-monitor/internal/probe"
	"github.com/MSJawad/synthetic-monitor/internal/scheduler"
	"github.com/MSJawad/synthetic-monitor/internal/store"
)

var (
	loadFlag    = flag.Bool("load", false, "run TestLoad, which measures how many checks one instance sustains")
	loadProfile = flag.String("load-profile", "", "directory to write a CPU profile of each TestLoad level to")
)

// Load test timing: each level runs warmup, then is measured for window.
const (
	loadWarmup = 3 * time.Second
	loadWindow = 10 * time.Second
)

// TestLoad measures how many checks one instance sustains. It wires the
// service's pipeline (scheduler, real probes, metrics, SQLite history and
// alerting) and runs N checks, each probing every second against a server
// in the same process, stepping N up. A level is sustained if at least 99%
// of the scheduled probes ran, none failed, and p99 scheduler lag stayed
// under 100ms. CPU figures include the target server's share.
//
//	go test -run TestLoad -v -timeout 30m ./cmd/synthetic-monitor -args -load
func TestLoad(t *testing.T) {
	if !*loadFlag {
		t.Skip("run with -load")
	}
	cert, pool := selfSigned(t)
	levels := []struct {
		kind   string
		checks []int
	}{
		{"http", []int{1000, 2000, 4000, 8000, 12000, 16000}},
		{"https", []int{500, 1000, 2000, 3000, 4000}},
	}
	t.Logf("%s/%s, %d CPUs, Go %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	t.Log("| probe | checks | probes/s | ran | failed | skipped | lag p50 | lag p99 | CPU cores | CPU per probe | heap | sustained |")
	t.Log("|---|---|---|---|---|---|---|---|---|---|---|---|")
	first := true
	for _, l := range levels {
		for _, n := range l.checks {
			if !first && runtime.GOOS == "darwin" {
				// macOS shares its ephemeral ports across the machine, and the
				// last level left some in TIME_WAIT for 30s.
				time.Sleep(35 * time.Second)
			}
			first = false
			r := runLoad(t, l.kind, n, cert, pool)
			t.Log(r.row())
		}
	}
}

type loadResult struct {
	kind              string
	checks            int
	expected          int
	ran, failed       int
	skipped           int64
	lagP50, lagP99    time.Duration
	cpu               time.Duration
	heapMB            float64
	lastFailureReason probe.Reason
}

func (r loadResult) row() string {
	ranPct := 100 * float64(r.ran) / float64(r.expected)
	sustained := ranPct >= 99 && r.failed == 0 && r.lagP99 < 100*time.Millisecond
	perProbe := time.Duration(0)
	if r.ran > 0 {
		perProbe = r.cpu / time.Duration(r.ran)
	}
	failed := fmt.Sprint(r.failed)
	if r.failed > 0 {
		failed += " (" + string(r.lastFailureReason) + ")"
	}
	return fmt.Sprintf("| %s | %d | %.0f | %.1f%% | %s | %d | %v | %v | %.2f | %v | %.0f MB | %v |",
		r.kind, r.checks, float64(r.ran)/loadWindow.Seconds(), ranPct, failed, r.skipped,
		r.lagP50.Round(10*time.Microsecond), r.lagP99.Round(10*time.Microsecond),
		r.cpu.Seconds()/loadWindow.Seconds(), perProbe.Round(time.Microsecond), r.heapMB, map[bool]string{true: "yes", false: "no"}[sustained])
}

func runLoad(t *testing.T, kind string, n int, cert tls.Certificate, pool *x509.CertPool) loadResult {
	t.Helper()
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	target := httptest.NewUnstartedServer(ok)
	if kind == "https" {
		target.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		target.StartTLS()
	} else {
		target.Start()
	}
	defer target.Close()

	logger := slog.New(slog.DiscardHandler)
	history, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "history.db"), store.Options{Retention: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	var checks []config.Check
	for i := range n {
		checks = append(checks, config.Check{Name: fmt.Sprintf("c%05d", i), Interval: time.Second, Timeout: time.Second,
			FailureThreshold: 3, SuccessThreshold: 2, HTTP: &config.HTTP{URL: target.URL}})
	}
	m := metrics.New()
	alerts, err := alert.New(t.Context(), history, checks, alert.Options{Notifier: alert.LogNotifier{Logger: logger}, Observer: m.AlertObserver()})
	if err != nil {
		t.Fatal(err)
	}
	c := &countingSink{}
	var skipped atomic.Int64
	sched := scheduler.New(sinks{m, history, alerts, logSink{logger}, c}, scheduler.Options{
		MaxConcurrent: 64,
		Logger:        logger,
		NewProber:     func(c config.Check) probe.Prober { return probe.New(c, probe.Options{RootCAs: pool}) },
		OnSkip:        func(_ string, runs int) { skipped.Add(int64(runs)) },
	})
	sched.Apply(checks)

	time.Sleep(loadWarmup)
	var profile *os.File
	if *loadProfile != "" {
		if profile, err = os.Create(filepath.Join(*loadProfile, fmt.Sprintf("cpu-%s-%d.pprof", kind, n))); err != nil {
			t.Fatal(err)
		}
		if err := pprof.StartCPUProfile(profile); err != nil {
			t.Fatal(err)
		}
	}
	skippedBefore, cpuBefore := skipped.Load(), cpuTime(t)
	c.measure(true)
	time.Sleep(loadWindow)
	c.measure(false)
	r := loadResult{kind: kind, checks: n, expected: n * int(loadWindow/time.Second),
		skipped: skipped.Load() - skippedBefore, cpu: cpuTime(t) - cpuBefore}
	if profile != nil {
		pprof.StopCPUProfile()
		_ = profile.Close()
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	r.heapMB = float64(mem.HeapInuse) / (1 << 20)
	r.ran, r.failed, r.lagP50, r.lagP99, r.lastFailureReason = c.results()

	ctx := t.Context()
	if err := sched.Stop(ctx); err != nil {
		t.Error(err)
	}
	if err := alerts.Close(ctx); err != nil {
		t.Error(err)
	}
	if err := history.Close(ctx); err != nil {
		t.Error(err)
	}
	return r
}

// countingSink records the results that complete while it is measuring.
type countingSink struct {
	on     atomic.Bool
	mu     sync.Mutex
	ran    int
	failed int
	reason probe.Reason
	lags   []time.Duration
}

func (c *countingSink) measure(on bool) { c.on.Store(on) }

func (c *countingSink) Record(r scheduler.Result) {
	if !c.on.Load() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ran++
	if !r.OK() {
		c.failed++
		c.reason = r.Reason
	}
	c.lags = append(c.lags, r.Start.Sub(r.Scheduled))
}

func (c *countingSink) results() (ran, failed int, p50, p99 time.Duration, reason probe.Reason) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slices.Sort(c.lags)
	pct := func(p float64) time.Duration {
		if len(c.lags) == 0 {
			return 0
		}
		return c.lags[int(p*float64(len(c.lags)-1))]
	}
	return c.ran, c.failed, pct(0.50), pct(0.99), c.reason
}

// cpuTime returns the user and system CPU time the process has used.
func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// selfSigned returns an ECDSA certificate for 127.0.0.1 and a pool that
// trusts it. ECDSA, as most certificate authorities now issue, keeps the
// handshake cost realistic; httptest's built-in certificate is RSA-2048,
// several times slower to sign with.
func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}
