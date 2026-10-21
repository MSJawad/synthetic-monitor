package config

import (
	"fmt"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// noEnv is an environment with no variables set.
func noEnv(string) (string, bool) { return "", false }

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - name: web
    slo: {target: 99.5}
    http:
      url: https://example.com/healthz
  - name: fast
    interval: 5s
    tcp:
      address: example.com:5432
  - name: resolver
    dns:
      host: example.com.
      server: 1.1.1.1
  - name: cert
    tls:
      address: example.com:443
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{
		{Name: "web", Interval: 30 * time.Second, Timeout: 10 * time.Second, FailureThreshold: 3, SuccessThreshold: 2, SLO: &SLO{Target: 99.5, WindowDays: 30},
			HTTP: &HTTP{URL: "https://example.com/healthz", Method: "GET"}},
		{Name: "fast", Interval: 5 * time.Second, Timeout: 5 * time.Second, FailureThreshold: 3, SuccessThreshold: 2,
			TCP: &TCP{Address: "example.com:5432"}},
		{Name: "resolver", Interval: 30 * time.Second, Timeout: 10 * time.Second, FailureThreshold: 3, SuccessThreshold: 2,
			DNS: &DNS{Host: "example.com.", Server: "1.1.1.1:53"}},
		{Name: "cert", Interval: 30 * time.Second, Timeout: 10 * time.Second, FailureThreshold: 3, SuccessThreshold: 2,
			TLS: &TLS{Address: "example.com:443", ServerName: "example.com", MinDaysRemaining: 14}},
	}
	if !reflect.DeepEqual(cfg.Checks, want) {
		t.Fatalf("checks =\n%+v\nwant\n%+v", cfg.Checks, want)
	}
}

func TestParseKeepsExplicitValues(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - name: api
    interval: 1m
    timeout: 5s
    failure_threshold: 5
    success_threshold: 1
    max_latency: 800ms
    slo: {target: 99.9, window_days: 7}
    http:
      url: http://api.internal:8080/ready
      expect_status: [200, 204]
      body_contains: '"status":"ok"'
  - name: ns
    dns:
      host: example.com
      server: "[2606:4700:4700::1111]:53"
      expect_addresses: [93.184.215.14, "2606:2800:21f:cb07:6820:80da:af6b:8b2c"]
  - name: cert
    tls:
      address: 203.0.113.10:8443
      server_name: example.com
      min_days_remaining: 30
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{
		{Name: "api", Interval: time.Minute, Timeout: 5 * time.Second, MaxLatency: 800 * time.Millisecond, FailureThreshold: 5, SuccessThreshold: 1,
			SLO:  &SLO{Target: 99.9, WindowDays: 7},
			HTTP: &HTTP{URL: "http://api.internal:8080/ready", Method: "GET", ExpectStatus: []int{200, 204}, BodyContains: `"status":"ok"`}},
		{Name: "ns", Interval: 30 * time.Second, Timeout: 10 * time.Second, FailureThreshold: 3, SuccessThreshold: 2,
			DNS: &DNS{Host: "example.com", Server: "[2606:4700:4700::1111]:53", ExpectAddresses: []netip.Addr{
				netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("2606:2800:21f:cb07:6820:80da:af6b:8b2c"),
			}}},
		{Name: "cert", Interval: 30 * time.Second, Timeout: 10 * time.Second, FailureThreshold: 3, SuccessThreshold: 2,
			TLS: &TLS{Address: "203.0.113.10:8443", ServerName: "example.com", MinDaysRemaining: 30}},
	}
	if !reflect.DeepEqual(cfg.Checks, want) {
		t.Fatalf("checks =\n%+v\nwant\n%+v", cfg.Checks, want)
	}
}

func TestParseSupportsAnchorsForSharedSettings(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - &slow
    name: a
    interval: 5m
    timeout: 20s
    tcp: {address: "a.example:22"}
  - <<: *slow
    name: b
    tcp: {address: "b.example:22"}
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if b := cfg.Checks[1]; b.Interval != 5*time.Minute || b.Timeout != 20*time.Second || b.TCP.Address != "b.example:22" {
		t.Fatalf("check b = %+v, want interval and timeout from the anchor and its own address", b)
	}
}

func TestParseRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"empty file", ``, "config is empty"},
		{"unknown field", `
checks:
  - name: a
    timout: 5s
    tcp: {address: "a:1"}`, "field timout not found"},
		{"duration without unit", `
checks:
  - name: a
    interval: 30
    tcp: {address: "a:1"}`, "cannot unmarshal !!int `30` into time.Duration"},
		{"malformed IP", `
checks:
  - name: a
    dns: {host: a, expect_addresses: [999.1.1.1]}`, `ParseAddr("999.1.1.1")`},
		{"missing name", `
checks:
  - tcp: {address: "a:1"}`, "name is required"},
		{"invalid name", `
checks:
  - name: "has space"
    tcp: {address: "a:1"}`, "name must be"},
		{"duplicate name", `
checks:
  - name: a
    tcp: {address: "a:1"}
  - name: a
    tcp: {address: "b:1"}`, `checks[1] "a": duplicate name`},
		{"interval too short", `
checks:
  - name: a
    interval: 500ms
    tcp: {address: "a:1"}`, "interval 500ms is below the minimum"},
		{"timeout above interval", `
checks:
  - name: a
    interval: 5s
    timeout: 10s
    tcp: {address: "a:1"}`, "timeout 10s must be positive and at most the interval 5s"},
		{"max latency not below timeout", `
checks:
  - name: a
    timeout: 2s
    max_latency: 2s
    tcp: {address: "a:1"}`, "max_latency 2s must be less than the timeout 2s"},
		{"no type", `
checks:
  - name: a`, "exactly one of http, tcp, dns, tls, steps or heartbeat must be set, found 0"},
		{"two types", `
checks:
  - name: a
    tcp: {address: "a:1"}
    tls: {address: "a:1"}`, "found 2"},
		{"relative URL", `
checks:
  - name: a
    http: {url: /healthz}`, "must be an absolute http or https URL"},
		{"non-http scheme", `
checks:
  - name: a
    http: {url: "ftp://example.com"}`, "must be an absolute http or https URL"},
		{"bad status code", `
checks:
  - name: a
    http: {url: "http://a", expect_status: [700]}`, "http.expect_status 700"},
		{"address without port", `
checks:
  - name: a
    tcp: {address: example.com}`, `"example.com" must be host:port`},
		{"address without host", `
checks:
  - name: a
    tcp: {address: ":80"}`, "has no host"},
		{"port out of range", `
checks:
  - name: a
    tls: {address: "a:70000"}`, "invalid port"},
		{"dns without host", `
checks:
  - name: a
    dns: {server: 1.1.1.1}`, "dns.host is required"},
		{"SLO target of 100", `
checks:
  - name: a
    slo: {target: 100}
    tcp: {address: "a:1"}`, "slo.target 100 must be a percentage above 0 and below 100"},
		{"SLO target as a ratio", `
checks:
  - name: a
    slo: {target: 0}
    tcp: {address: "a:1"}`, "slo.target 0 must be"},
		{"SLO window too long", `
checks:
  - name: a
    slo: {target: 99, window_days: 400}
    tcp: {address: "a:1"}`, "slo.window_days 400 must be between 1 and 365"},
		{"negative failure threshold", `
checks:
  - name: a
    failure_threshold: -1
    tcp: {address: "a:1"}`, "failure_threshold -1 must be between 1 and 100"},
		{"success threshold too high", `
checks:
  - name: a
    success_threshold: 101
    tcp: {address: "a:1"}`, "success_threshold 101 must be between 1 and 100"},
		{"negative min days", `
checks:
  - name: a
    tls: {address: "a:443", min_days_remaining: -1}`, "must not be negative"},
		{"heartbeat without a period", `
checks:
  - name: a
    heartbeat: {grace: 5m}`, "heartbeat.every 0s is below the minimum"},
		{"negative grace", `
checks:
  - name: a
    heartbeat: {every: 1h, grace: -1m}`, "heartbeat.grace -1m0s must not be negative"},
		{"heartbeat with max latency", `
checks:
  - name: a
    max_latency: 1s
    heartbeat: {every: 1h}`, "max_latency does not apply to heartbeat checks"},
		{"step without a name", `
checks:
  - name: a
    steps: [{url: "http://a"}]`, `steps[0] "": name is required`},
		{"duplicate step name", `
checks:
  - name: a
    steps: [{name: s, url: "http://a"}, {name: s, url: "http://b"}]`, `steps[1] "s": duplicate step name`},
		{"variable used before it is extracted", `
checks:
  - name: a
    steps:
      - {name: one, url: "http://a", headers: {Authorization: "Bearer ${token}"}}
      - {name: two, url: "http://a", extract: {token: data.token}}`, `steps[0] "one": headers["Authorization"] refers to ${token}, which is not set`},
		{"invalid extracted name", `
checks:
  - name: a
    steps: [{name: s, url: "http://a", extract: {my-token: token}}]`, `extract: "my-token" is not a valid variable name`},
		{"empty path segment", `
checks:
  - name: a
    steps: [{name: s, url: "http://a", extract: {token: data..token}}]`, `path "data..token" must be dot-separated`},
		{"invalid step request", `
checks:
  - name: a
    steps: [{name: s, url: "nope"}]`, `steps[0] "s": url "nope" must be an absolute`},
		{"unknown method", `
checks:
  - name: a
    http: {url: "http://a", method: FETCH}`, `http.method "FETCH" must be one of GET, HEAD`},
		{"invalid header name", `
checks:
  - name: a
    http: {url: "http://a", headers: {"Bad Name": x}}`, `http.headers["Bad Name"] is not a valid header name`},
		{"same header twice", `
checks:
  - name: a
    http: {url: "http://a", headers: {content-type: a, Content-Type: b}}`, "are the same header"},
		{"multi-line header", `
checks:
  - name: a
    http: {url: "http://a", headers: {X-A: "a\nb"}}`, `headers["X-A"] must be a single line`},
		{"variable in URL", `
checks:
  - name: a
    http: {url: "http://a/?key=${KEY}"}`, "http.url must not contain variables"},
		{"unset variable", `
checks:
  - name: a
    http: {url: "http://a", headers: {X-Key: "${KEY}"}}`, `http.headers["X-Key"] refers to ${KEY}, which is not set`},
		{"unclosed variable", `
checks:
  - name: a
    http: {url: "http://a", body: "token=${KEY"}`, "http.body: unclosed ${"},
		{"invalid variable name", `
checks:
  - name: a
    http: {url: "http://a", body: "${1X}"}`, "${1X} is not a valid variable name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml), noEnv)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseReportsAllProblemsAtOnce(t *testing.T) {
	_, err := Parse([]byte(`
checks:
  - name: a
    interval: 100ms
    tcp: {address: "a:1"}
  - name: b
    http: {url: nope}
`), noEnv)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{`checks[0] "a": interval`, `checks[1] "b": http.url`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestExampleConfigsAreValid(t *testing.T) {
	if _, _, err := NewWatcher("../../examples/checks.yaml", noEnv); err != nil {
		t.Fatalf("examples/checks.yaml: %v", err)
	}
	// The secrets app-checks.yaml documents, and only those.
	env := func(name string) (string, bool) {
		switch name {
		case "PROBE_EMAIL", "PROBE_PASSWORD", "METRICS_KEY":
			return "x", true
		}
		return "", false
	}
	_, cfg, err := NewWatcher("../../examples/app-checks.yaml", env)
	if err != nil {
		t.Fatalf("examples/app-checks.yaml: %v", err)
	}
	types := make(map[string]bool)
	for _, c := range cfg.Checks {
		types[c.Type()] = true
	}
	for _, want := range []string{"http", "steps", "heartbeat", "tls"} {
		if !types[want] {
			t.Errorf("examples/app-checks.yaml has no %s check", want)
		}
	}
}

func TestSLORatio(t *testing.T) {
	for target, want := range map[float64]float64{99.9: 0.999, 99.5: 0.995, 99.95: 0.9995, 90: 0.9, 0.1: 0.001} {
		if got := (SLO{Target: target}).Ratio(); got != want {
			t.Errorf("Ratio of %v = %v, want %v", target, got, want)
		}
	}
}

func TestParseRequestTemplates(t *testing.T) {
	env := func(name string) (string, bool) {
		v, ok := map[string]string{"KEY": "s3cr3t", "EMPTY": "", "MULTI": "a\nb"}[name]
		return v, ok
	}
	cfg, err := Parse([]byte(`
checks:
  - name: login
    http:
      url: https://api.example.com/signin
      method: post
      headers: {Content-Type: application/json, X-Key: "${KEY}"}
      body: '{"password": "${KEY}", "price": "$5"}'
`), env)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Checks[0].HTTP
	if h.Method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.Method)
	}
	// The config keeps the templates; only the prober sees the values.
	if h.Headers["X-Key"] != "${KEY}" || strings.Contains(h.Body, "s3cr3t") {
		t.Errorf("headers %v, body %q: want the templates unexpanded", h.Headers, h.Body)
	}

	for _, tt := range []struct{ name, want string }{
		{"EMPTY", "${EMPTY}, which is empty in the environment"},
		{"MULTI", "${MULTI}, whose value spans several lines"},
	} {
		_, err := Parse([]byte(`
checks:
  - name: a
    http: {url: "http://a", headers: {X-A: "${`+tt.name+`}"}}
`), env)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("${%s}: error = %v, want it to contain %q", tt.name, err, tt.want)
		}
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"A": "1", "TOKEN": "abc"}
	lookup := func(name string) string { return vars[name] }
	for _, tt := range []struct{ in, want string }{
		{"plain", "plain"},
		{"Bearer ${TOKEN}", "Bearer abc"},
		{"${A}${A}", "11"},
		{"costs $5", "costs $5"},
		{"ends with $", "ends with $"},
		{"literal $${A}", "literal ${A}"},
		{"$$$$", "$$"},
	} {
		if got := Expand(tt.in, lookup); got != tt.want {
			t.Errorf("Expand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseSteps(t *testing.T) {
	env := func(name string) (string, bool) { return "hunter2", name == "PASSWORD" }
	cfg, err := Parse([]byte(`
checks:
  - name: sign-in
    interval: 5m
    steps:
      - name: sign in
        method: POST
        url: https://api.example.com/signin
        body: '{"password": "${PASSWORD}"}'
        extract: {token: data.token}
      - name: profile
        url: https://api.example.com/me
        headers: {Authorization: "Bearer ${token}"}
        body_contains: probe@example.com
`), env)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Checks[0]
	if c.Type() != "steps" || len(c.Steps) != 2 || c.Steps[1].Method != http.MethodGet || c.Steps[0].Extract["token"] != "data.token" {
		t.Fatalf("check = %+v, want 2 steps of type steps, the second a GET", c)
	}

	var many strings.Builder
	many.WriteString("checks:\n  - name: a\n    steps:\n")
	for i := range MaxSteps + 1 {
		fmt.Fprintf(&many, "      - {name: s%d, url: \"http://a\"}\n", i)
	}
	if _, err := Parse([]byte(many.String()), noEnv); err == nil || !strings.Contains(err.Error(), "at most 10 are allowed") {
		t.Errorf("11 steps: error = %v, want a limit of 10", err)
	}
}

func TestParseHeartbeatGrace(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - name: hourly
    heartbeat: {every: 1h}
  - name: frequent
    heartbeat: {every: 2m}
  - name: explicit
    heartbeat: {every: 1h, grace: 20m}
`), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{6 * time.Minute, time.Minute, 20 * time.Minute} {
		c := cfg.Checks[i]
		if c.Type() != "heartbeat" || c.Heartbeat.Grace != want {
			t.Errorf("%s: type %s, grace %v; want heartbeat, %v", c.Name, c.Type(), c.Heartbeat.Grace, want)
		}
	}
}
