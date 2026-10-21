package probe

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// BenchmarkProbe runs each probe type against a server on the same machine,
// 64 probes at a time, the service's default -max-concurrent-probes.
// cpu-µs/probe is the process's CPU time per probe, including the server's
// share of the work: the figure that decides how many probes a machine can
// run (cores ÷ CPU per probe). allocs/op shows the garbage each probe
// creates. probes/s is wall-clock throughput, which an occasional dropped
// SYN, resent after a second, can distort; CPU time does not count that
// wait. Run it with a fixed count:
//
//	go test -run '^$' -bench BenchmarkProbe -benchtime 5000x ./internal/probe
//
// Every probe opens a new connection, and whichever side closes first holds
// its port in TIME_WAIT for 2×MSL (30s on macOS, 60s on Linux). TCP and TLS
// probes always close first; HTTP probes do in about one connection in
// eight, when both sides close at once. macOS has 16,384 ephemeral ports
// for the whole machine, so a longer benchmark runs out of local ports
// ("can't assign requested address") long before it runs out of CPU.
// 5000 probes per type stays inside that limit.
//
// Each case first sends one burst of probes before the timer starts, since
// on Linux the opening burst of connections to a server that has not
// accepted any yet tends to overflow its accept queue (TcpExt
// ListenOverflows) and wait out that one-second retransmission.
func BenchmarkProbe(b *testing.B) {
	const inFlight = 64
	ca := newTestCA(b)
	cert := ca.validFor(b, 90*24*time.Hour, "127.0.0.1", "app.test")
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	plain := httptest.NewServer(ok)
	b.Cleanup(plain.Close)
	secure := startTLSServer(b, ok, cert)
	dns := startFakeDNS(b, map[string]netip.Addr{"app.test.": netip.MustParseAddr("192.0.2.10")})

	cases := []struct {
		name  string
		check config.Check
	}{
		{"http", config.Check{HTTP: &config.HTTP{URL: plain.URL}}},
		{"https", config.Check{HTTP: &config.HTTP{URL: secure.URL}}},
		{"tcp", config.Check{TCP: &config.TCP{Address: strings.TrimPrefix(plain.URL, "http://")}}},
		{"tls", config.Check{TLS: &config.TLS{Address: strings.TrimPrefix(secure.URL, "https://"), ServerName: "app.test", MinDaysRemaining: 14}}},
		{"dns", config.Check{DNS: &config.DNS{Host: "app.test.", Server: dns}}},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			c.check.Name, c.check.Interval, c.check.Timeout = "bench", time.Minute, 10*time.Second
			p := New(c.check, Options{RootCAs: ca.pool})
			var warm sync.WaitGroup
			for range inFlight {
				warm.Go(func() { p.Probe(b.Context()) })
			}
			warm.Wait()
			b.ResetTimer()
			cpuBefore := cpuTime(b)
			b.ReportAllocs()
			b.SetParallelism(max(1, inFlight/runtime.GOMAXPROCS(0)))
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if r := p.Probe(b.Context()); !r.OK() {
						b.Error(fmt.Errorf("probe failed: %s: %w", r.Reason, r.Err))
						return
					}
				}
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "probes/s")
			b.ReportMetric(float64(cpuTime(b)-cpuBefore)/float64(time.Microsecond)/float64(b.N), "cpu-µs/probe")
		})
	}
}

// cpuTime returns the user and system CPU time the process has used.
func cpuTime(b *testing.B) time.Duration {
	b.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		b.Fatal(err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
