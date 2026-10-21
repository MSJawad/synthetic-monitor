package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// proberFunc adapts a function to the Prober interface.
type proberFunc func(context.Context) Result

func (f proberFunc) Probe(ctx context.Context) Result { return f(ctx) }

// blocking fails only once ctx is done, like a probe against a server that
// never answers.
var blocking = proberFunc(func(ctx context.Context) Result {
	<-ctx.Done()
	return Result{Reason: ReasonOther, Err: ctx.Err()}
})

func TestLimitedReportsTimeoutAtDeadline(t *testing.T) {
	const timeout = 50 * time.Millisecond
	start := time.Now()
	r := (&limited{next: blocking, timeout: timeout}).Probe(t.Context())

	if r.Reason != ReasonTimeout {
		t.Fatalf("reason = %q (%v), want %q", r.Reason, r.Err, ReasonTimeout)
	}
	if r.Duration < timeout {
		t.Errorf("duration = %v, want at least the timeout %v", r.Duration, timeout)
	}
	if elapsed := time.Since(start); elapsed > timeout+time.Second {
		t.Errorf("probe returned after %v, want close to the timeout %v", elapsed, timeout)
	}
}

func TestLimitedReportsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := (&limited{next: blocking, timeout: time.Minute}).Probe(ctx)

	if r.Reason != ReasonCanceled {
		t.Fatalf("reason = %q, want %q", r.Reason, ReasonCanceled)
	}
}

func TestLimitedEnforcesMaxLatency(t *testing.T) {
	slowSuccess := proberFunc(func(context.Context) Result {
		time.Sleep(30 * time.Millisecond)
		return Result{}
	})
	r := (&limited{next: slowSuccess, timeout: time.Second, maxLatency: 10 * time.Millisecond}).Probe(t.Context())
	if r.Reason != ReasonLatency {
		t.Fatalf("reason = %q, want %q", r.Reason, ReasonLatency)
	}

	r = (&limited{next: slowSuccess, timeout: time.Second}).Probe(t.Context())
	if !r.OK() {
		t.Fatalf("without max_latency: reason = %q (%v), want success", r.Reason, r.Err)
	}
}

func TestLimitedKeepsProberFailure(t *testing.T) {
	refused := proberFunc(func(context.Context) Result {
		return Result{Reason: ReasonConnect, Err: syscall.ECONNREFUSED}
	})
	before := time.Now()
	r := (&limited{next: refused, timeout: time.Second}).Probe(t.Context())

	if r.Reason != ReasonConnect || !errors.Is(r.Err, syscall.ECONNREFUSED) {
		t.Fatalf("result = (%q, %v), want (%q, %v)", r.Reason, r.Err, ReasonConnect, syscall.ECONNREFUSED)
	}
	if r.Start.Before(before) || r.Duration <= 0 {
		t.Errorf("start = %v, duration = %v; want them set", r.Start, r.Duration)
	}
}

func TestClassify(t *testing.T) {
	// Errors are wrapped the way net/http and net wrap them.
	inURLError := func(err error) error { return &url.Error{Op: "Get", URL: "https://example.com", Err: err} }
	tests := []struct {
		name string
		err  error
		want Reason
	}{
		{"name not found", inURLError(&net.OpError{Op: "dial", Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}}), ReasonDNS},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, ReasonConnect},
		{"untrusted certificate", inURLError(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), ReasonTLS},
		{"not a TLS server", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, ReasonTLS},
		{"https URL on a plain HTTP server", inURLError(http.ErrSchemeMismatch), ReasonTLS},
		{"handshake alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, ReasonTLS},
		{"reset while reading", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, ReasonOther},
		{"truncated body", io.ErrUnexpectedEOF, ReasonOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.err); got != tt.want {
				t.Fatalf("classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// run probes c through New, as the scheduler does, filling in the fields a
// test does not care about.
func run(t *testing.T, c config.Check, opts Options) Result {
	t.Helper()
	c.Name, c.Interval = "test", time.Minute
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Second
	}
	return New(c, opts).Probe(t.Context())
}

// wantReason fails the test unless r failed with want, or succeeded if want
// is empty.
func wantReason(t *testing.T, r Result, want Reason) {
	t.Helper()
	if r.Reason == want {
		return
	}
	if want == "" {
		t.Fatalf("probe failed with %q (%v), want success", r.Reason, r.Err)
	}
	t.Fatalf("reason = %q (err: %v), want %q", r.Reason, r.Err, want)
}
