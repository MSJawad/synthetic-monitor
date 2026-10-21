package alert

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/probe"
)

var (
	sampleAlert = Notification{Kind: KindAlert, Check: "web", At: t0.Add(time.Minute), Since: t0,
		Probes: 3, Reason: probe.ReasonHTTPStatus, Error: "status 500, want 2xx"}
	sampleRecovery = Notification{Kind: KindRecovery, Check: "web", At: t0.Add(5 * time.Minute), Since: t0,
		Until: t0.Add(4*time.Minute + 12*time.Second), Probes: 2}
	sampleRemoved = Notification{Kind: KindRemoved, Check: "web", At: t0.Add(time.Hour), Since: t0}
)

type ntfyRequest struct {
	method, path, title, priority, tags, auth, body string
}

// fakeNtfy records requests and answers with status.
func fakeNtfy(t *testing.T, status int) (*httptest.Server, chan ntfyRequest) {
	t.Helper()
	got := make(chan ntfyRequest, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- ntfyRequest{r.Method, r.URL.Path, r.Header.Get("Title"), r.Header.Get("Priority"),
			r.Header.Get("Tags"), r.Header.Get("Authorization"), string(body)}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"code":40101,"http":401,"error":"unauthorized"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestNtfyPublishes(t *testing.T) {
	srv, got := fakeNtfy(t, http.StatusOK)
	nt, err := NewNtfy(srv.URL+"/monitor-x7f3k2", "tk_secret")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		n                     Notification
		title, priority, tags string
		bodyHas1, bodyHas2    string
	}{
		{sampleAlert, "web is failing", "high", "rotating_light",
			"3 probes in a row have failed, the first at 2026-10-21 14:00:00 UTC", "(http_status): status 500, want 2xx"},
		{sampleRecovery, "web recovered", "default", "white_check_mark",
			"Succeeding again since 2026-10-21 14:04:12 UTC", "after 4m12s of failures"},
		{sampleRemoved, "web is no longer monitored", "default", "wastebasket",
			"removed from the config while failing (since 2026-10-21 14:00:00 UTC)", "no recovery notice will follow"},
	}
	for _, tt := range tests {
		t.Run(string(tt.n.Kind), func(t *testing.T) {
			if err := nt.Notify(t.Context(), tt.n); err != nil {
				t.Fatal(err)
			}
			r := <-got
			if r.method != http.MethodPost || r.path != "/monitor-x7f3k2" || r.auth != "Bearer tk_secret" {
				t.Errorf("request %s %s with Authorization %q; want POST to the topic with the token", r.method, r.path, r.auth)
			}
			if r.title != tt.title || r.priority != tt.priority || r.tags != tt.tags {
				t.Errorf("title %q, priority %q, tags %q; want %q, %q, %q", r.title, r.priority, r.tags, tt.title, tt.priority, tt.tags)
			}
			if !strings.Contains(r.body, tt.bodyHas1) || !strings.Contains(r.body, tt.bodyHas2) {
				t.Errorf("body %q lacks %q or %q", r.body, tt.bodyHas1, tt.bodyHas2)
			}
		})
	}
}

func TestNtfyErrors(t *testing.T) {
	tests := []struct {
		status    int
		permanent bool
	}{
		{http.StatusTooManyRequests, false},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, false},
		{http.StatusUnauthorized, true},
		{http.StatusForbidden, true},
		{http.StatusBadRequest, true},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			srv, _ := fakeNtfy(t, tt.status)
			nt, err := NewNtfy(srv.URL+"/topic", "")
			if err != nil {
				t.Fatal(err)
			}
			err = nt.Notify(t.Context(), sampleAlert)
			if err == nil || IsPermanent(err) != tt.permanent {
				t.Fatalf("Notify = %v (permanent %v), want an error with permanent = %v", err, IsPermanent(err), tt.permanent)
			}
			if tt.permanent && !strings.Contains(err.Error(), "unauthorized") {
				t.Errorf("error %q does not include ntfy's explanation", err)
			}
		})
	}
}

func TestNtfyUnreachableIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/topic"
	srv.Close()
	nt, err := NewNtfy(url, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := nt.Notify(t.Context(), sampleAlert); err == nil || IsPermanent(err) {
		t.Fatalf("Notify to a closed server = %v, want a retryable error", err)
	}
}

func TestNewNtfyRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"", "ntfy.sh/topic", "ftp://ntfy.sh/topic", "https://ntfy.sh", "https://ntfy.sh/"} {
		if _, err := NewNtfy(u, ""); err == nil {
			t.Errorf("NewNtfy(%q) succeeded, want an error", u)
		}
	}
}

func TestPermanent(t *testing.T) {
	base := errors.New("bad token")
	err := Permanent(base)
	if !IsPermanent(err) || !errors.Is(err, base) {
		t.Error("Permanent error is not recognised or does not wrap its cause")
	}
	if IsPermanent(base) || IsPermanent(nil) {
		t.Error("an unmarked error counts as permanent")
	}
}

func TestLogNotifier(t *testing.T) {
	var buf bytes.Buffer
	n := LogNotifier{Logger: slog.New(slog.NewTextHandler(&buf, nil))}
	if err := n.Notify(context.Background(), sampleAlert); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, `title="web is failing"`) {
		t.Errorf("log output %q, want a warning with the title", out)
	}
}
