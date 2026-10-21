package probe

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

const day = 24 * time.Hour

func TestTLS(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	tests := []struct {
		name       string
		cert       tls.Certificate
		serverName string
		trust      bool
		want       Reason
	}{
		{"valid", ca.validFor(t, 90*day, "app.test"), "app.test", true, ""},
		{"expires within minimum", ca.validFor(t, 5*day, "app.test"), "app.test", true, ReasonCertExpiry},
		{"expired", ca.issue(t, now.Add(-2*day), now.Add(-day), "app.test"), "app.test", true, ReasonTLS},
		{"not yet valid", ca.issue(t, now.Add(day), now.Add(90*day), "app.test"), "app.test", true, ReasonTLS},
		{"wrong server name", ca.validFor(t, 90*day, "app.test"), "other.test", true, ReasonTLS},
		{"unknown issuer", ca.validFor(t, 90*day, "app.test"), "app.test", false, ReasonTLS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTLSServer(t, http.NotFoundHandler(), tt.cert)
			var opts Options
			if tt.trust {
				opts.RootCAs = ca.pool
			}
			r := run(t, tlsCheck(srv, tt.serverName), opts)
			wantReason(t, r, tt.want)

			leaf, err := x509.ParseCertificate(tt.cert.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			if r.Reason == "" || r.Reason == ReasonCertExpiry {
				if !r.CertNotAfter.Equal(leaf.NotAfter) {
					t.Errorf("CertNotAfter = %v, want the leaf's %v", r.CertNotAfter, leaf.NotAfter)
				}
			} else if !r.CertNotAfter.IsZero() {
				t.Errorf("CertNotAfter = %v after a failed handshake, want zero", r.CertNotAfter)
			}
		})
	}
}

func TestTLSIntermediateExpiringFirst(t *testing.T) {
	root := newTestCA(t)
	inter := root.intermediate(t, time.Now().Add(5*day))
	srv := startTLSServer(t, http.NotFoundHandler(), inter.validFor(t, 90*day, "app.test"))

	r := run(t, tlsCheck(srv, "app.test"), Options{RootCAs: root.pool})
	wantReason(t, r, ReasonCertExpiry)
	if !r.CertNotAfter.Equal(inter.cert.NotAfter) {
		t.Errorf("CertNotAfter = %v, want the intermediate's %v", r.CertNotAfter, inter.cert.NotAfter)
	}
}

func TestTLSAgainstPlainTCPServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()

	wantReason(t, run(t, tlsCheck(srv, "app.test"), Options{}), ReasonTLS)
}

func TestTLSConnectionRefused(t *testing.T) {
	c := config.Check{TLS: &config.TLS{Address: closedPort(t), ServerName: "app.test", MinDaysRemaining: 14}}
	wantReason(t, run(t, c, Options{}), ReasonConnect)
}

func TestChainExpiryUsesLongestLivedChain(t *testing.T) {
	at := func(days int) *x509.Certificate {
		return &x509.Certificate{NotAfter: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, days)}
	}
	// Both chains share a leaf. The first ends when its cross-signed
	// intermediate expires on day 10; the second stays valid until the leaf
	// expires on day 90.
	got := chainExpiry([][]*x509.Certificate{
		{at(90), at(10), at(3000)},
		{at(90), at(400), at(5000)},
	})
	if want := at(90).NotAfter; !got.Equal(want) {
		t.Fatalf("chainExpiry = %v, want %v", got, want)
	}
	if got := chainExpiry(nil); !got.IsZero() {
		t.Fatalf("chainExpiry(nil) = %v, want zero", got)
	}
}

// tlsCheck targets srv's address, verifying the certificate against
// serverName, with the default 14-day minimum.
func tlsCheck(srv *httptest.Server, serverName string) config.Check {
	return config.Check{TLS: &config.TLS{
		Address:          strings.TrimPrefix(strings.TrimPrefix(srv.URL, "https://"), "http://"),
		ServerName:       serverName,
		MinDaysRemaining: config.DefaultMinDaysRemaining,
	}}
}
