package watchdog

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunPingsUntilCancelled(t *testing.T) {
	var hits atomic.Int32
	failing := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	logs := &syncBuffer{}
	var oks, fails atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	secretURL := srv.URL + "/ping/0b5c4ea1-secret"
	go func() {
		defer close(done)
		Run(ctx, Options{URL: secretURL, Interval: 20 * time.Millisecond, Client: srv.Client(),
			Logger: slog.New(slog.NewTextHandler(logs, nil)),
			OnPing: func(ok bool) {
				if ok {
					oks.Add(1)
				} else {
					fails.Add(1)
				}
			}})
	}()

	waitFor(t, "three successful pings", func() bool { return oks.Load() >= 3 })
	failing.Store(true)
	waitFor(t, "a failed ping", func() bool { return fails.Load() >= 1 })
	srv.CloseClientConnections()
	srv.Close() // now pings fail to connect, and the error quotes the URL
	waitFor(t, "a connection failure", func() bool { return fails.Load() >= 3 })
	cancel()
	<-done

	if strings.Contains(logs.String(), "0b5c4ea1-secret") {
		t.Errorf("the logs reveal the ping URL:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "status 500") {
		t.Errorf("the logs do not say why a ping failed:\n%s", logs)
	}
}

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://hc-ping.com/uuid", true},
		{"hc-ping.com/uuid", false},
		{"ftp://hc-ping.com/uuid", false},
	} {
		if err := Validate(tt.url); (err == nil) != tt.ok {
			t.Errorf("Validate(%q) = %v, want ok %v", tt.url, err, tt.ok)
		}
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
