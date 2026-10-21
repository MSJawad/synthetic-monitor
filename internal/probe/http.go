package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"sync"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

const (
	userAgent = "synthetic-monitor"
	// maxBodyBytes caps how much of a response is read, so a huge or endless
	// body cannot exhaust memory. body_contains only searches this prefix.
	maxBodyBytes = 1 << 20
)

// HTTPPhases splits an HTTP probe's duration into its steps. A phase is zero
// when it did not happen, such as DNS for an IP address or TLS for plain
// HTTP. After redirects, each phase reflects the last time it happened.
type HTTPPhases struct {
	DNS     time.Duration
	Connect time.Duration
	TLS     time.Duration
	// Processing runs from sending the request to the first response byte:
	// the server's work plus one network round trip.
	Processing time.Duration
	// Transfer runs from the first response byte to the end of the body.
	Transfer time.Duration
}

type httpProber struct {
	url          string
	expectStatus []int
	bodyContains []byte
	client       *http.Client
}

func newHTTP(c *config.HTTP, opts Options) *httpProber {
	return &httpProber{
		url:          c.URL,
		expectStatus: c.ExpectStatus,
		bodyContains: []byte(c.BodyContains),
		client: &http.Client{Transport: &http.Transport{
			// A fresh connection per probe measures what a new visitor sees,
			// including DNS, TCP and TLS. Reusing connections would hide
			// failures and latency in those steps.
			DisableKeepAlives: true,
			ForceAttemptHTTP2: true,
			DialContext:       (&net.Dialer{Resolver: systemResolver}).DialContext,
			TLSClientConfig:   &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
			// Proxy is left nil so probes go direct, ignoring HTTP_PROXY.
		}},
	}
}

func (p *httpProber) Probe(ctx context.Context) Result {
	var timer phaseTimer
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, timer.trace()), http.MethodGet, p.url, nil)
	if err != nil {
		return Result{Reason: ReasonOther, Err: err}
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.client.Do(req)
	if err != nil {
		return Result{Reason: classify(err), Err: err, Phases: timer.phases(time.Now())}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	r := Result{Phases: timer.phases(time.Now())}
	switch {
	case err != nil:
		r.Reason, r.Err = classify(err), fmt.Errorf("reading body: %w", err)
	case !p.statusOK(resp.StatusCode):
		r.Reason, r.Err = ReasonHTTPStatus, fmt.Errorf("status %d, want %s", resp.StatusCode, p.wantStatus())
	case len(p.bodyContains) > 0 && !bytes.Contains(body, p.bodyContains):
		r.Reason, r.Err = ReasonHTTPBody, fmt.Errorf("body does not contain %q", p.bodyContains)
	}
	return r
}

func (p *httpProber) statusOK(code int) bool {
	if len(p.expectStatus) == 0 {
		return code >= 200 && code <= 299
	}
	return slices.Contains(p.expectStatus, code)
}

func (p *httpProber) wantStatus() string {
	if len(p.expectStatus) == 0 {
		return "2xx"
	}
	return fmt.Sprint(p.expectStatus)
}

// phaseTimer records when each step of an HTTP request starts and ends.
// httptrace may call its hooks from several goroutines, hence the mutex.
type phaseTimer struct {
	mu                        sync.Mutex
	dnsStart, dnsDone         time.Time
	connectStart, connectDone time.Time
	tlsStart, tlsDone         time.Time
	wroteRequest, firstByte   time.Time
}

func (pt *phaseTimer) trace() *httptrace.ClientTrace {
	mark := func(t *time.Time) {
		pt.mu.Lock()
		defer pt.mu.Unlock()
		*t = time.Now()
	}
	return &httptrace.ClientTrace{
		DNSStart:             func(httptrace.DNSStartInfo) { mark(&pt.dnsStart) },
		DNSDone:              func(httptrace.DNSDoneInfo) { mark(&pt.dnsDone) },
		ConnectStart:         func(string, string) { mark(&pt.connectStart) },
		ConnectDone:          func(string, string, error) { mark(&pt.connectDone) },
		TLSHandshakeStart:    func() { mark(&pt.tlsStart) },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { mark(&pt.tlsDone) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { mark(&pt.wroteRequest) },
		GotFirstResponseByte: func() { mark(&pt.firstByte) },
	}
}

func (pt *phaseTimer) phases(end time.Time) HTTPPhases {
	pt.mu.Lock()
	defer pt.mu.Unlock()
	return HTTPPhases{
		DNS:        span(pt.dnsStart, pt.dnsDone),
		Connect:    span(pt.connectStart, pt.connectDone),
		TLS:        span(pt.tlsStart, pt.tlsDone),
		Processing: span(pt.wroteRequest, pt.firstByte),
		Transfer:   span(pt.firstByte, end),
	}
}

func span(start, end time.Time) time.Duration {
	if start.IsZero() || end.Before(start) {
		return 0
	}
	return end.Sub(start)
}
