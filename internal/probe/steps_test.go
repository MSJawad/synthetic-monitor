package probe

import (
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// signInApp is a fake app with a sign-in endpoint that sets a session
// cookie and returns a token, and a profile endpoint that needs both.
func signInApp(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var arrivedWithSession atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signin", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("session"); err == nil {
			arrivedWithSession.Add(1)
		}
		var creds struct{ Password string }
		if err := json.NewDecoder(r.Body).Decode(&creds); err != nil || creds.Password != "hunter2" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		_, _ = io.WriteString(w, `{"data": {"token": "t0k", "id": 12345678901234567890, "admin": false}}`)
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("session")
		if err != nil || c.Value != "s1" || r.Header.Get("Authorization") != "Bearer t0k" {
			http.Error(w, "signed out", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"email": "probe@example.com"}`)
	})
	// HTTPS, so the session cookie can be Secure, as a real one would be.
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &arrivedWithSession
}

func signInSteps(url, path string) []config.Step {
	return []config.Step{
		{
			Name: "sign in", URL: url + "/signin", Method: http.MethodPost, Body: `{"password": "${PASSWORD}"}`,
			Extract: map[string]string{"token": path},
		},
		{
			Name: "profile", URL: url + "/me", Method: http.MethodGet,
			Headers: map[string]string{"Authorization": "Bearer ${token}"}, BodyContains: "probe@example.com",
		},
	}
}

// signInOptions trusts srv's certificate and supplies password as the
// PASSWORD environment variable.
func signInOptions(srv *httptest.Server, password string) Options {
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return Options{RootCAs: pool, Env: func(name string) (string, bool) { return password, name == "PASSWORD" }}
}

func TestStepsSignInThenFetch(t *testing.T) {
	srv, arrivedWithSession := signInApp(t)
	c := config.Check{Steps: signInSteps(srv.URL, "data.token")}
	for range 2 {
		wantReason(t, run(t, c, signInOptions(srv, "hunter2")), "")
	}
	// Each run started with an empty cookie jar, as a new visitor.
	if n := arrivedWithSession.Load(); n != 0 {
		t.Errorf("%d sign-ins arrived with a session cookie, want 0", n)
	}
}

func TestStepsReportTheFailingStep(t *testing.T) {
	srv, _ := signInApp(t)
	r := run(t, config.Check{Steps: signInSteps(srv.URL, "data.token")}, signInOptions(srv, "wrong"))
	wantReason(t, r, ReasonHTTPStatus)
	if !strings.Contains(r.Err.Error(), `step 1 "sign in": status 401`) {
		t.Errorf("error = %q, want it to name step 1", r.Err)
	}
	if strings.Contains(r.Err.Error(), "wrong") {
		t.Errorf("error = %q reveals the password", r.Err)
	}
}

func TestStepsMissingExtractedValue(t *testing.T) {
	srv, _ := signInApp(t)
	r := run(t, config.Check{Steps: signInSteps(srv.URL, "data.session")}, signInOptions(srv, "hunter2"))
	wantReason(t, r, ReasonHTTPBody)
	if !strings.Contains(r.Err.Error(), `step 1 "sign in": extracting token: data.session is not in the response`) {
		t.Errorf("error = %q, want it to name the missing path", r.Err)
	}
}

func TestExtractJSON(t *testing.T) {
	body := []byte(`{"data": {"token": "t0k", "id": 12345678901234567890, "ok": true,
		"items": [{"id": "first"}], "none": null}}`)
	for _, tt := range []struct {
		path, want, wantErr string
	}{
		{"data.token", "t0k", ""},
		{"data.id", "12345678901234567890", ""}, // not rounded through float64
		{"data.ok", "true", ""},
		{"data.items.0.id", "first", ""},
		{"data.items.1.id", "", "data.items.1 is not in the response"},
		{"data.token.length", "", "data.token.length is not in the response"},
		{"data.missing", "", "data.missing is not in the response"},
		{"data.items", "", "data.items is not a string, number or boolean"},
		{"data.none", "", "data.none is not a string, number or boolean"},
	} {
		got, err := extractJSON(body, strings.Split(tt.path, "."))
		if got != tt.want || (err == nil) != (tt.wantErr == "") || (err != nil && err.Error() != tt.wantErr) {
			t.Errorf("extractJSON(%s) = %q, %v; want %q, %q", tt.path, got, err, tt.want, tt.wantErr)
		}
	}
	if _, err := extractJSON([]byte("<html>"), []string{"a"}); err == nil || err.Error() != "the response is not JSON" {
		t.Errorf("non-JSON body: error = %v", err)
	}
}
