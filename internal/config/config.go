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
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"go.yaml.in/yaml/v3"
)

// Defaults applied to fields a check leaves unset.
const (
	DefaultInterval         = 30 * time.Second
	DefaultTimeout          = 10 * time.Second // capped at the check's interval
	DefaultMinDaysRemaining = 14
	MinInterval             = time.Second
)

// Config is the parsed and validated contents of a checks file.
type Config struct {
	Checks []Check `yaml:"checks"`
	// Version identifies the file contents the config was parsed from, so
	// logs can show which version is in effect.
	Version string `yaml:"-"`
}

// Check is one endpoint probed on a schedule. Exactly one of HTTP, TCP, DNS
// and TLS is set, and it determines the check's type.
type Check struct {
	// Name identifies the check across config reloads and in metrics,
	// history and alerts, so renaming a check starts it from scratch.
	Name     string        `yaml:"name"`
	Interval time.Duration `yaml:"interval"`
	Timeout  time.Duration `yaml:"timeout"`
	// MaxLatency, if set, fails probes that succeed but take longer.
	MaxLatency time.Duration `yaml:"max_latency"`

	HTTP *HTTP `yaml:"http"`
	TCP  *TCP  `yaml:"tcp"`
	DNS  *DNS  `yaml:"dns"`
	TLS  *TLS  `yaml:"tls"`
}

// HTTP checks that a GET request to URL succeeds.
type HTTP struct {
	URL string `yaml:"url"`
	// ExpectStatus lists acceptable status codes. Empty accepts any 2xx.
	ExpectStatus []int `yaml:"expect_status"`
	// BodyContains, if set, must appear in the response body.
	BodyContains string `yaml:"body_contains"`
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

// Type returns the name of the check's probe type: "http", "tcp", "dns" or
// "tls".
func (c Check) Type() string {
	switch {
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

// Parse decodes a config, applies defaults and validates it. Unknown fields
// are errors, so a misspelt option fails loudly instead of being ignored.
// All validation problems are reported together.
func Parse(data []byte) (*Config, error) {
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
	if err := cfg.validate(); err != nil {
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

// validName keeps names safe to use in URLs, metric labels and log lines.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (cfg *Config) validate() error {
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

		types := 0
		for _, set := range []bool{c.HTTP != nil, c.TCP != nil, c.DNS != nil, c.TLS != nil} {
			if set {
				types++
			}
		}
		if types != 1 {
			fail("exactly one of http, tcp, dns or tls must be set, found %d", types)
			continue
		}

		switch {
		case c.HTTP != nil:
			u, err := url.Parse(c.HTTP.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				fail("http.url %q must be an absolute http or https URL", c.HTTP.URL)
			}
			for _, code := range c.HTTP.ExpectStatus {
				if code < 100 || code > 599 {
					fail("http.expect_status %d is not a valid HTTP status code", code)
				}
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
