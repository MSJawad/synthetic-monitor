package config

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestParseAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(`
checks:
  - name: web
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
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{
		{Name: "web", Interval: 30 * time.Second, Timeout: 10 * time.Second,
			HTTP: &HTTP{URL: "https://example.com/healthz"}},
		{Name: "fast", Interval: 5 * time.Second, Timeout: 5 * time.Second,
			TCP: &TCP{Address: "example.com:5432"}},
		{Name: "resolver", Interval: 30 * time.Second, Timeout: 10 * time.Second,
			DNS: &DNS{Host: "example.com.", Server: "1.1.1.1:53"}},
		{Name: "cert", Interval: 30 * time.Second, Timeout: 10 * time.Second,
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
    max_latency: 800ms
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
`))
	if err != nil {
		t.Fatal(err)
	}
	want := []Check{
		{Name: "api", Interval: time.Minute, Timeout: 5 * time.Second, MaxLatency: 800 * time.Millisecond,
			HTTP: &HTTP{URL: "http://api.internal:8080/ready", ExpectStatus: []int{200, 204}, BodyContains: `"status":"ok"`}},
		{Name: "ns", Interval: 30 * time.Second, Timeout: 10 * time.Second,
			DNS: &DNS{Host: "example.com", Server: "[2606:4700:4700::1111]:53", ExpectAddresses: []netip.Addr{
				netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("2606:2800:21f:cb07:6820:80da:af6b:8b2c"),
			}}},
		{Name: "cert", Interval: 30 * time.Second, Timeout: 10 * time.Second,
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
`))
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
  - name: a`, "exactly one of http, tcp, dns or tls must be set, found 0"},
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
		{"negative min days", `
checks:
  - name: a
    tls: {address: "a:443", min_days_remaining: -1}`, "must not be negative"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
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
`))
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	for _, want := range []string{`checks[0] "a": interval`, `checks[1] "b": http.url`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
