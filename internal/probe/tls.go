package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

type tlsProber struct {
	address      string
	minRemaining time.Duration
	minDays      int
	dialer       *tls.Dialer
}

func newTLS(c *config.TLS, opts Options) *tlsProber {
	return &tlsProber{
		address:      c.Address,
		minRemaining: time.Duration(c.MinDaysRemaining) * 24 * time.Hour,
		minDays:      c.MinDaysRemaining,
		dialer: &tls.Dialer{
			NetDialer: &net.Dialer{Resolver: systemResolver},
			Config:    &tls.Config{ServerName: c.ServerName, RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
		},
	}
}

func (p *tlsProber) Probe(ctx context.Context) Result {
	conn, err := p.dialer.DialContext(ctx, "tcp", p.address)
	if err != nil {
		return Result{Reason: classify(err), Err: err}
	}
	defer conn.Close()

	notAfter := chainExpiry(conn.(*tls.Conn).ConnectionState().VerifiedChains)
	r := Result{CertNotAfter: notAfter}
	if left := time.Until(notAfter); left < p.minRemaining {
		r.Reason = ReasonCertExpiry
		r.Err = fmt.Errorf("certificate chain expires in %.1f days on %s, below the minimum of %d days",
			left.Hours()/24, notAfter.UTC().Format(time.DateOnly), p.minDays)
	}
	return r
}

// chainExpiry returns when the connection would stop verifying. Any
// certificate in a chain, including an intermediate, ends that chain when it
// expires, so a chain lasts until its earliest expiry. A server can present
// several valid chains (for example through a cross-signed intermediate),
// and verification keeps working while any of them is valid, so the result
// is the latest of the chains' expiries.
func chainExpiry(chains [][]*x509.Certificate) time.Time {
	var latest time.Time
	for _, chain := range chains {
		var earliest time.Time
		for _, cert := range chain {
			if earliest.IsZero() || cert.NotAfter.Before(earliest) {
				earliest = cert.NotAfter
			}
		}
		if earliest.After(latest) {
			latest = earliest
		}
	}
	return latest
}
