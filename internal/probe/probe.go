// Package probe performs single checks against endpoints and reports whether
// they succeeded, how long they took and, if they failed, why.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// Reason classifies why a probe failed. The set is small and fixed so it can
// be used as a metric label.
type Reason string

// Reasons a probe can fail.
const (
	ReasonTimeout    Reason = "timeout"     // the check's timeout expired
	ReasonCanceled   Reason = "canceled"    // the caller cancelled the probe, e.g. at shutdown; says nothing about the target
	ReasonDNS        Reason = "dns"         // name resolution failed
	ReasonConnect    Reason = "connect"     // the TCP connection was refused or the host was unreachable
	ReasonTLS        Reason = "tls"         // the TLS handshake or certificate verification failed
	ReasonCertExpiry Reason = "cert_expiry" // the certificate chain is valid but expires too soon
	ReasonHTTPStatus Reason = "http_status" // the HTTP status code was not expected
	ReasonHTTPBody   Reason = "http_body"   // the HTTP response body lacked the expected text
	ReasonDNSAnswer  Reason = "dns_answer"  // the name resolved without the expected addresses
	ReasonLatency    Reason = "latency"     // the probe succeeded but took longer than max_latency
	ReasonOther      Reason = "other"       // anything else, such as a connection reset mid-response
)

// Result is the outcome of one probe.
type Result struct {
	Start    time.Time
	Duration time.Duration
	// Reason is empty when the probe succeeded.
	Reason Reason
	// Err describes the failure for logs and the status page.
	Err error
	// Phases breaks down an HTTP probe's duration. It is zero for other types.
	Phases HTTPPhases
	// CertNotAfter is when a TLS check's certificate chain stops verifying.
	// It is zero for other types and when the handshake failed.
	CertNotAfter time.Time
}

// OK reports whether the probe succeeded.
func (r Result) OK() bool { return r.Reason == "" }

// Prober performs one probe. Implementations must return promptly once ctx
// is done.
type Prober interface {
	Probe(ctx context.Context) Result
}

// Options changes how probers verify servers and where they find
// variables. The zero value suits production.
type Options struct {
	// RootCAs verifies server certificates. Nil uses the system roots; tests
	// set it to trust their own certificate authority.
	RootCAs *x509.CertPool
	// Env supplies the variables request templates refer to. Nil uses the
	// process environment.
	Env config.Env
}

// lookupEnv returns a lookup of variables in the environment, for
// config.Expand.
func (o Options) lookupEnv() func(name string) string {
	env := o.Env
	if env == nil {
		env = os.LookupEnv
	}
	return func(name string) string {
		v, _ := env(name)
		return v
	}
}

// New returns the prober for check c, which must have passed config
// validation.
func New(c config.Check, opts Options) Prober {
	var p Prober
	switch {
	case c.HTTP != nil:
		p = newHTTP(c.HTTP, opts)
	case c.TCP != nil:
		p = &tcpProber{address: c.TCP.Address}
	case c.DNS != nil:
		p = newDNS(c.DNS)
	case c.TLS != nil:
		p = newTLS(c.TLS, opts)
	default:
		panic(fmt.Sprintf("probe: check %q has no probe type", c.Name))
	}
	return &limited{next: p, timeout: c.Timeout, maxLatency: c.MaxLatency}
}

// limited wraps a type-specific prober with the behaviour every check shares:
// a timeout, timing, and an optional latency limit.
type limited struct {
	next       Prober
	timeout    time.Duration
	maxLatency time.Duration
}

func (l *limited) Probe(ctx context.Context) Result {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()

	start := time.Now()
	r := l.next.Probe(ctx)
	r.Start, r.Duration = start, time.Since(start)

	// The deadline turns whatever error the probe hit (an i/o timeout, a
	// cancelled read) into a timeout, so every type reports it the same way.
	switch {
	case r.OK():
		if l.maxLatency > 0 && r.Duration > l.maxLatency {
			r.Reason = ReasonLatency
			r.Err = fmt.Errorf("took %v, above max_latency %v", r.Duration.Round(time.Millisecond), l.maxLatency)
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		r.Reason = ReasonTimeout
		r.Err = fmt.Errorf("no result within %v: %w", l.timeout, r.Err)
	case ctx.Err() != nil:
		r.Reason = ReasonCanceled
	}
	return r
}

// classify maps a network error to a Reason. Timeouts are not detected here:
// limited reports those based on the probe's deadline.
func classify(err error) Reason {
	var (
		dnsErr    *net.DNSError
		verifyErr *tls.CertificateVerificationError
		recordErr tls.RecordHeaderError
		opErr     *net.OpError
	)
	switch {
	case errors.As(err, &dnsErr):
		return ReasonDNS
	case errors.As(err, &verifyErr), errors.As(err, &recordErr):
		return ReasonTLS
	case errors.Is(err, http.ErrSchemeMismatch):
		// net/http's replacement for the record error when an https URL
		// reaches a plain HTTP server.
		return ReasonTLS
	case errors.As(err, &opErr) && opErr.Op == "remote error":
		// The server aborted the handshake with a TLS alert.
		return ReasonTLS
	case errors.As(err, &opErr) && opErr.Op == "dial":
		return ReasonConnect
	}
	return ReasonOther
}
