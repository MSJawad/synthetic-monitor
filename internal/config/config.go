// Package config loads and validates the YAML file that defines checks.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Defaults applied to fields a check leaves unset.
const (
	DefaultInterval         = 30 * time.Second
	DefaultTimeout          = 10 * time.Second // capped at the check's interval
	DefaultMinDaysRemaining = 14
	DefaultSLOWindowDays    = 30
	DefaultFailureThreshold = 3
	DefaultSuccessThreshold = 2
	MinInterval             = time.Second
)

// Config is the parsed and validated contents of a checks file.
type Config struct {
	Checks []Check `yaml:"checks"`
	// Version identifies the file contents the config was parsed from, so
	// logs can show which version is in effect.
	Version string `yaml:"-"`
}

// Check is one endpoint probed on a schedule. Exactly one of HTTP, TCP, DNS,
// TLS, Steps and Heartbeat is set, and it determines the check's type.
type Check struct {
	// Name identifies the check across config reloads and in metrics,
	// history and alerts, so renaming a check starts it from scratch.
	Name     string        `yaml:"name"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// MaxLatency, if set, fails probes that succeed but take longer.
	MaxLatency time.Duration `yaml:"max_latency"`
	// SLO, if set, is the check's availability objective.
	SLO *SLO `yaml:"slo"`
	// FailureThreshold is how many probes in a row must fail before an
	// alert is sent, so a single blip does not page anyone.
	FailureThreshold int `yaml:"failure_threshold"`
	// SuccessThreshold is how many probes in a row must then succeed before
	// the recovery notice, so a flapping target stays in one alert.
	SuccessThreshold int `yaml:"success_threshold"`

	HTTP *HTTP `yaml:"http"`
	TCP  *TCP  `yaml:"tcp"`
	DNS  *DNS  `yaml:"dns"`
	TLS  *TLS  `yaml:"tls"`
	// Steps, if set, makes the check a sequence of HTTP requests, such as
	// signing in and then fetching a page with the session.
	Steps []Step `yaml:"steps"`
	// Heartbeat, if set, makes the check wait for pings instead of sending
	// probes.
	Heartbeat *Heartbeat `yaml:"heartbeat"`
}

// SLO is an availability objective: at least Target percent of probes
// succeed over the trailing WindowDays days.
type SLO struct {
	Target     float64 `yaml:"target"`
	WindowDays int     `yaml:"window_days"`
}

// Ratio returns Target as a fraction, e.g. 0.999 for 99.9.
func (s SLO) Ratio() float64 {
	// Dividing by 100 rounds twice and gives 0.9990000000000001 for 99.9.
	// Parsing the decimal with an exponent rounds once, to the nearest float.
	r, err := strconv.ParseFloat(strconv.FormatFloat(s.Target, 'f', -1, 64)+"e-2", 64)
	if err != nil {
		return s.Target / 100 // unreachable: FormatFloat output always parses
	}
	return r
}

// Window returns the SLO's window as a duration.
func (s SLO) Window() time.Duration {
	return time.Duration(s.WindowDays) * 24 * time.Hour
}

// HTTP checks that a request to URL succeeds.
type HTTP struct {
	URL string `yaml:"url"`
	// Method defaults to GET.
	Method string `yaml:"method"`
	// Headers and Body are templates (see Expand), so they can carry
	// secrets from the environment. A Host header sets the request's host.
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
	// ExpectStatus lists acceptable status codes. Empty accepts any 2xx.
	ExpectStatus []int `yaml:"expect_status"`
	// BodyContains, if set, must appear in the response body.
	BodyContains string `yaml:"body_contains"`
}

// MaxSteps limits a multi-step check, which shares one timeout.
const MaxSteps = 10

// Step is one request of a multi-step check. Steps run in order with one
// cookie jar, and the check fails at the first step that fails.
type Step struct {
	Name string `yaml:"name"`
	HTTP `yaml:",inline"`
	// Extract sets variables for later steps' templates from this step's
	// JSON response: each maps a variable name to a dotted path such as
	// data.token or items.0.id.
	Extract map[string]string `yaml:"extract"`
}

// Heartbeat checks that something, typically a scheduled job the monitor
// cannot reach, pings the monitor at least every Every, with Grace to
// spare. Each run of the check passes if the last ping is recent enough.
type Heartbeat struct {
	Every time.Duration `yaml:"every"`
	// Grace allows for jobs that start late or run long. It defaults to a
	// tenth of Every, and at least a minute.
	Grace time.Duration `yaml:"grace"`
}

// TCP checks that a TCP connection to Address (host:port) can be opened.
type TCP struct {
	Address string `yaml:"address"`
}

// DNS checks that Host resolves.
type DNS struct {
	Host string `yaml:"host"`
	// Server is the nameserver to query as host:port, or host for port 53.
	// Empty uses the resolvers in /etc/resolv.conf.
	Server string `yaml:"server"`
	// ExpectAddresses, if set, must all be among the resolved addresses.
	ExpectAddresses []netip.Addr `yaml:"expect_addresses"`
}

// TLS checks that Address (host:port) completes a verified TLS handshake and
// that no certificate in the chain expires within MinDaysRemaining days.
type TLS struct {
	Address string `yaml:"address"`
	// ServerName is verified against the certificate. Defaults to the host
	// part of Address.
	ServerName       string `yaml:"server_name"`
	MinDaysRemaining int    `yaml:"min_days_remaining"`
}

// Type returns the name of the check's probe type: "http", "tcp", "dns",
// "tls", "steps" or "heartbeat".
func (c Check) Type() string {
	switch {
	case len(c.Steps) > 0:
		return "steps"
	case c.Heartbeat != nil:
		return "heartbeat"
	case c.HTTP != nil:
		return "http"
	case c.TCP != nil:
		return "tcp"
	case c.DNS != nil:
		return "dns"
	case c.TLS != nil:
		return "tls"
	}
	return ""
}

// Parse decodes a config, applies defaults and validates it, looking up
// the variables that templates refer to in env. Unknown fields are errors,
// so a misspelt option fails loudly instead of being ignored. All
// validation problems are reported together.
func Parse(data []byte, env Env) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, err
	}
	for i := range cfg.Checks {
		cfg.Checks[i].applyDefaults()
	}
	if err := cfg.validate(env); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	cfg.Version = hex.EncodeToString(sum[:6])
	return &cfg, nil
}

func (c *Check) applyDefaults() {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.Timeout == 0 {
		c.Timeout = min(DefaultTimeout, c.Interval)
	}
	if c.FailureThreshold == 0 {
		c.FailureThreshold = DefaultFailureThreshold
	}
	if c.SuccessThreshold == 0 {
		c.SuccessThreshold = DefaultSuccessThreshold
	}
	if c.SLO != nil && c.SLO.WindowDays == 0 {
		c.SLO.WindowDays = DefaultSLOWindowDays
	}
	if c.HTTP != nil {
		c.HTTP.applyDefaults()
	}
	for i := range c.Steps {
		c.Steps[i].applyDefaults()
	}
	if c.Heartbeat != nil && c.Heartbeat.Grace == 0 {
		c.Heartbeat.Grace = max(time.Minute, c.Heartbeat.Every/10)
	}
	if c.DNS != nil && c.DNS.Server != "" {
		if _, _, err := net.SplitHostPort(c.DNS.Server); err != nil {
			c.DNS.Server = net.JoinHostPort(c.DNS.Server, "53")
		}
	}
	if c.TLS != nil {
		if c.TLS.MinDaysRemaining == 0 {
			c.TLS.MinDaysRemaining = DefaultMinDaysRemaining
		}
		if host, _, err := net.SplitHostPort(c.TLS.Address); err == nil && c.TLS.ServerName == "" {
			c.TLS.ServerName = host
		}
	}
}

func (h *HTTP) applyDefaults() {
	if h.Method == "" {
		h.Method = http.MethodGet
	}
	h.Method = strings.ToUpper(h.Method)
}

// validName keeps names safe to use in URLs, metric labels and log lines.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (cfg *Config) validate(env Env) error {
	var errs []error
	seen := make(map[string]bool)
	for i, c := range cfg.Checks {
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("checks[%d] %q: %s", i, c.Name, fmt.Sprintf(format, args...)))
		}
		switch {
		case c.Name == "":
			fail("name is required")
		case !validName.MatchString(c.Name):
			fail("name must be 1-64 letters, digits, '.', '_' or '-', starting with a letter or digit")
		case seen[c.Name]:
			fail("duplicate name")
		}
		seen[c.Name] = true

		if c.Interval < MinInterval {
			fail("interval %v is below the minimum of %v", c.Interval, MinInterval)
		}
		if c.Timeout <= 0 || c.Timeout > c.Interval {
			fail("timeout %v must be positive and at most the interval %v", c.Timeout, c.Interval)
		}
		if c.MaxLatency < 0 || (c.MaxLatency > 0 && c.MaxLatency >= c.Timeout) {
			fail("max_latency %v must be less than the timeout %v", c.MaxLatency, c.Timeout)
		}
		if c.FailureThreshold < 1 || c.FailureThreshold > 100 {
			fail("failure_threshold %d must be between 1 and 100", c.FailureThreshold)
		}
		if c.SuccessThreshold < 1 || c.SuccessThreshold > 100 {
			fail("success_threshold %d must be between 1 and 100", c.SuccessThreshold)
		}
		if c.SLO != nil {
			if c.SLO.Target <= 0 || c.SLO.Target >= 100 {
				fail("slo.target %v must be a percentage above 0 and below 100", c.SLO.Target)
			}
			if c.SLO.WindowDays < 1 || c.SLO.WindowDays > 365 {
				fail("slo.window_days %d must be between 1 and 365", c.SLO.WindowDays)
			}
		}

		types := 0
		for _, set := range []bool{c.HTTP != nil, c.TCP != nil, c.DNS != nil, c.TLS != nil, len(c.Steps) > 0, c.Heartbeat != nil} {
			if set {
				types++
			}
		}
		if types != 1 {
			fail("exactly one of http, tcp, dns, tls, steps or heartbeat must be set, found %d", types)
			continue
		}

		switch {
		case c.HTTP != nil:
			for _, err := range c.HTTP.validate(envVars(env)) {
				fail("http.%v", err)
			}
		case len(c.Steps) > 0:
			for _, err := range validateSteps(c.Steps, env) {
				fail("%v", err)
			}
		case c.Heartbeat != nil:
			if c.Heartbeat.Every < MinInterval {
				fail("heartbeat.every %v is below the minimum of %v", c.Heartbeat.Every, MinInterval)
			}
			if c.Heartbeat.Grace < 0 {
				fail("heartbeat.grace %v must not be negative", c.Heartbeat.Grace)
			}
			if c.MaxLatency != 0 {
				fail("max_latency does not apply to heartbeat checks")
			}
		case c.TCP != nil:
			if err := checkHostPort(c.TCP.Address); err != nil {
				fail("tcp.address: %v", err)
			}
		case c.DNS != nil:
			if c.DNS.Host == "" {
				fail("dns.host is required")
			}
			if c.DNS.Server != "" {
				if err := checkHostPort(c.DNS.Server); err != nil {
					fail("dns.server: %v", err)
				}
			}
		case c.TLS != nil:
			if err := checkHostPort(c.TLS.Address); err != nil {
				fail("tls.address: %v", err)
			}
			if c.TLS.MinDaysRemaining < 0 {
				fail("tls.min_days_remaining %d must not be negative", c.TLS.MinDaysRemaining)
			}
		}
	}
	return errors.Join(errs...)
}

// methods are the HTTP methods a request may use.
var methods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// validate returns the problems with h, each starting with the field it
// concerns. checkVar reports why a variable a template refers to cannot be
// used, or nil if it can.
func (h *HTTP) validate(checkVar func(name string) error) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	template := func(field, s string) {
		names, err := templateRefs(s)
		if err != nil {
			fail("%s: %v", field, err)
			return
		}
		for _, name := range names {
			if err := checkVar(name); err != nil {
				fail("%s refers to ${%s}, %v", field, name, err)
			}
		}
	}

	u, err := url.Parse(h.URL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		fail("url %q must be an absolute http or https URL", h.URL)
	case strings.Contains(h.URL, "${"):
		// URLs appear in error messages, and so in logs and notifications.
		fail("url must not contain variables: send secrets in headers or the body")
	}
	if !slices.Contains(methods, h.Method) {
		fail("method %q must be one of %s", h.Method, strings.Join(methods, ", "))
	}
	canonical := make(map[string]string)
	for name, value := range h.Headers {
		field := fmt.Sprintf("headers[%q]", name)
		if !validHeaderName(name) {
			fail("%s is not a valid header name", field)
			continue
		}
		if prev, dup := canonical[http.CanonicalHeaderKey(name)]; dup {
			fail("%s and headers[%q] are the same header", field, prev)
		}
		canonical[http.CanonicalHeaderKey(name)] = name
		if strings.ContainsAny(value, "\r\n") {
			fail("%s must be a single line", field)
		}
		template(field, value)
	}
	template("body", h.Body)
	for _, code := range h.ExpectStatus {
		if code < 100 || code > 599 {
			fail("expect_status %d is not a valid HTTP status code", code)
		}
	}
	return errs
}

// validateSteps returns the problems with a multi-step check's steps. A
// template in one step may refer to variables extracted by an earlier
// step; any other name must be an environment variable.
func validateSteps(steps []Step, env Env) []error {
	var errs []error
	if len(steps) > MaxSteps {
		errs = append(errs, fmt.Errorf("steps: %d steps, at most %d are allowed", len(steps), MaxSteps))
	}
	extracted := make(map[string]bool)
	names := make(map[string]bool)
	fromEnv := envVars(env)
	for i, st := range steps {
		fail := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("steps[%d] %q: %s", i, st.Name, fmt.Sprintf(format, args...)))
		}
		switch {
		case st.Name == "":
			fail("name is required")
		case names[st.Name]:
			fail("duplicate step name")
		}
		names[st.Name] = true
		for _, err := range st.validate(func(name string) error {
			if extracted[name] {
				return nil
			}
			return fromEnv(name)
		}) {
			fail("%v", err)
		}
		// Extracted variables become visible to the steps after this one.
		for name, path := range st.Extract {
			if !varName.MatchString(name) {
				fail("extract: %q is not a valid variable name", name)
			}
			if extracted[name] {
				fail("extract: %s is already extracted by an earlier step", name)
			}
			if path == "" || slices.Contains(strings.Split(path, "."), "") {
				fail("extract: %s: path %q must be dot-separated keys or indexes, such as data.token", name, path)
			}
		}
		for name := range st.Extract {
			extracted[name] = true
		}
	}
	return errs
}

// envVars returns a checkVar for HTTP.validate that accepts environment
// variables that are set, non-empty and a single line. An empty secret is
// almost always a deployment mistake, so it is caught here rather than
// sent. Values are never quoted in errors: they may be secrets.
func envVars(env Env) func(name string) error {
	return func(name string) error {
		v, ok := env(name)
		switch {
		case !ok:
			return errors.New("which is not set in the environment")
		case v == "":
			return errors.New("which is empty in the environment")
		case strings.ContainsAny(v, "\r\n"):
			return errors.New("whose value spans several lines")
		}
		return nil
	}
}

// validHeaderName reports whether name is an HTTP header field name: one
// or more token characters (RFC 9110, section 5.6.2).
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		letterOrDigit := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !letterOrDigit && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			return false
		}
	}
	return true
}

func checkHostPort(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%q must be host:port", address)
	}
	if host == "" {
		return fmt.Errorf("%q has no host", address)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has an invalid port", address)
	}
	return nil
}
