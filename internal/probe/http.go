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
	"strings"
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
	req    request
	env    func(name string) string
	client *http.Client
}

func newHTTP(c *config.HTTP, opts Options) *httpProber {
	return &httpProber{req: newRequest(c), env: opts.lookupEnv(), client: newHTTPClient(opts)}
}

// newHTTPClient returns a client for probes.
func newHTTPClient(opts Options) *http.Client {
	return &http.Client{Transport: &http.Transport{
		// A fresh connection per probe measures what a new visitor sees,
		// including DNS, TCP and TLS. Reusing connections would hide
		// failures and latency in those steps.
		DisableKeepAlives: true,
		ForceAttemptHTTP2: true,
		DialContext:       (&net.Dialer{Resolver: systemResolver}).DialContext,
		TLSClientConfig:   &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
		// Proxy is left nil so probes go direct, ignoring HTTP_PROXY.
	}}
}

func (p *httpProber) Probe(ctx context.Context) Result {
	r, _ := p.req.do(ctx, p.client, p.env)
	return r
}

// request is an HTTP request a prober sends and what its response must
// look like. Its header values and body are templates, expanded on every
// send so that secrets exist only for as long as a request needs them.
type request struct {
	url, method  string
	headers      map[string]string
	body         string
	expectStatus []int
	bodyContains []byte
	follow       bool // whether redirects are followed
}

func newRequest(c *config.HTTP) request {
	return request{
		url: c.URL, method: c.Method, headers: c.Headers, body: c.Body,
		expectStatus: c.ExpectStatus, bodyContains: []byte(c.BodyContains), follow: c.Follows(),
	}
}

// do sends the request, with templates expanded by lookup, and checks the
// response. It also returns the body read, for multi-step checks to
// extract values from.
func (q *request) do(ctx context.Context, client *http.Client, lookup func(name string) string) (Result, []byte) {
	var timer phaseTimer
	var body io.Reader
	if q.body != "" {
		body = strings.NewReader(config.Expand(q.body, lookup))
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, timer.trace()), q.method, q.url, body)
	if err != nil {
		return Result{Reason: ReasonOther, Err: err}, nil
	}
	req.Header.Set("User-Agent", userAgent)
	for name, value := range q.headers {
		value = config.Expand(value, lookup)
		if http.CanonicalHeaderKey(name) == "Host" {
			req.Host = value // net/http sends req.Host and ignores a Host header
			continue
		}
		req.Header.Set(name, value)
	}

	if !q.follow {
		c := *client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &c
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Reason: classify(err), Err: err, Phases: timer.phases(time.Now())}, nil
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))

	r := Result{Phases: timer.phases(time.Now())}
	switch {
	case err != nil:
		r.Reason, r.Err = classify(err), fmt.Errorf("reading body: %w", err)
	case !q.statusOK(resp.StatusCode):
		r.Reason, r.Err = ReasonHTTPStatus, fmt.Errorf("status %d, want %s", resp.StatusCode, q.wantStatus())
	case len(q.bodyContains) > 0 && !bytes.Contains(respBody, q.bodyContains):
		r.Reason, r.Err = ReasonHTTPBody, fmt.Errorf("body does not contain %q", q.bodyContains)
	}
	return r, respBody
}

func (q *request) statusOK(code int) bool {
	if len(q.expectStatus) == 0 {
		return code >= 200 && code <= 299
	}
	return slices.Contains(q.expectStatus, code)
}

func (q *request) wantStatus() string {
	if len(q.expectStatus) == 0 {
		return "2xx"
	}
	return fmt.Sprint(q.expectStatus)
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
