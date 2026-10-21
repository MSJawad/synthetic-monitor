package probe

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

func TestHTTPSuccess(t *testing.T) {
	userAgents := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userAgents <- r.UserAgent()
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	r := run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, BodyContains: `"status":"ok"`}}, Options{})
	wantReason(t, r, "")
	if ua := <-userAgents; ua != userAgent {
		t.Errorf("User-Agent = %q, want %q", ua, userAgent)
	}
}

func TestHTTPStatus(t *testing.T) {
	tests := []struct {
		name   string
		status int
		expect []int
		want   Reason
	}{
		{"any 2xx accepted by default", http.StatusNoContent, nil, ""},
		{"5xx rejected by default", http.StatusInternalServerError, nil, ReasonHTTPStatus},
		{"listed code accepted", http.StatusServiceUnavailable, []int{503}, ""},
		{"unlisted code rejected", http.StatusOK, []int{204}, ReasonHTTPStatus},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, ExpectStatus: tt.expect}}, Options{}), tt.want)
		})
	}
}

func TestHTTPFollowsRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("GET /old", http.RedirectHandler("/new", http.StatusFound))
	mux.HandleFunc("GET /new", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "moved") })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL + "/old", BodyContains: "moved"}}, Options{}), "")
}

func TestHTTPBodyMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"starting"}`)
	}))
	defer srv.Close()

	r := run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, BodyContains: `"status":"ok"`}}, Options{})
	wantReason(t, r, ReasonHTTPBody)
}

func TestHTTPBodySearchStopsAtLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("a", maxBodyBytes)+"needle")
	}))
	defer srv.Close()

	wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL, BodyContains: "needle"}}, Options{}), ReasonHTTPBody)
}

func TestHTTPTimeoutOnSlowServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the probe gives up
	}))
	defer srv.Close()

	const timeout = 100 * time.Millisecond
	start := time.Now()
	r := run(t, config.Check{Timeout: timeout, HTTP: &config.HTTP{URL: srv.URL}}, Options{})
	wantReason(t, r, ReasonTimeout)
	if elapsed := time.Since(start); elapsed > timeout+time.Second {
		t.Errorf("probe returned after %v, want close to the timeout %v", elapsed, timeout)
	}
}

func TestHTTPMaxLatency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := run(t, config.Check{MaxLatency: 10 * time.Millisecond, HTTP: &config.HTTP{URL: srv.URL}}, Options{})
	wantReason(t, r, ReasonLatency)
}

func TestHTTPTruncatedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Promise 100 bytes, send 7, then drop the connection.
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			panic(err)
		}
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 100\r\n\r\npartial")
		_ = buf.Flush()
		_ = conn.Close()
	}))
	defer srv.Close()

	wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL}}, Options{}), ReasonOther)
}

func TestHTTPConnectionRefused(t *testing.T) {
	wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: "http://" + closedPort(t)}}, Options{}), ReasonConnect)
}

func TestHTTPSCertificates(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	tests := []struct {
		name  string
		cert  tls.Certificate
		trust bool
		want  Reason
	}{
		{"trusted", ca.validFor(t, 90*24*time.Hour, "127.0.0.1"), true, ""},
		{"unknown issuer", ca.validFor(t, 90*24*time.Hour, "127.0.0.1"), false, ReasonTLS},
		{"expired", ca.issue(t, now.Add(-48*time.Hour), now.Add(-24*time.Hour), "127.0.0.1"), true, ReasonTLS},
		{"wrong host", ca.validFor(t, 90*24*time.Hour, "other.example"), true, ReasonTLS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTLSServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), tt.cert)
			var opts Options
			if tt.trust {
				opts.RootCAs = ca.pool
			}
			wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL}}, opts), tt.want)
		})
	}
}

func TestHTTPSAgainstPlainHTTPServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	url := strings.Replace(srv.URL, "http://", "https://", 1)
	wantReason(t, run(t, config.Check{HTTP: &config.HTTP{URL: url}}, Options{}), ReasonTLS)
}

func TestHTTPSPhases(t *testing.T) {
	ca := newTestCA(t)
	const serverTime = 20 * time.Millisecond
	srv := startTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(serverTime)
		_, _ = io.WriteString(w, "ok")
	}), ca.validFor(t, 90*24*time.Hour, "127.0.0.1"))

	r := run(t, config.Check{HTTP: &config.HTTP{URL: srv.URL}}, Options{RootCAs: ca.pool})
	wantReason(t, r, "")
	p := r.Phases
	if p.DNS != 0 {
		t.Errorf("DNS = %v, want 0 for an IP address", p.DNS)
	}
	if p.Connect <= 0 || p.TLS <= 0 {
		t.Errorf("connect = %v, TLS = %v, want both positive", p.Connect, p.TLS)
	}
	if p.Processing < serverTime {
		t.Errorf("processing = %v, want at least the server's %v", p.Processing, serverTime)
	}
	if sum := p.Connect + p.TLS + p.Processing + p.Transfer; sum > r.Duration {
		t.Errorf("phases add up to %v, more than the probe's duration %v", sum, r.Duration)
	}
}

// closedPort returns a local address that refuses connections.
func closedPort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
