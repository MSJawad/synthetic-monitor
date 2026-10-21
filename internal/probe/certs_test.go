package probe

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testCA is a certificate authority for tests. An intermediate CA shares its
// root's pool and serves its own certificate as part of every chain it signs.
type testCA struct {
	cert  *x509.Certificate
	key   *ecdsa.PrivateKey
	pool  *x509.CertPool // trusts the root
	chain [][]byte       // intermediate certificates to send after a leaf
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	ca := createCA(t, nil, time.Now().AddDate(10, 0, 0))
	ca.pool = x509.NewCertPool()
	ca.pool.AddCert(ca.cert)
	return ca
}

func createCA(t *testing.T, parent *testCA, notAfter time.Time) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		Subject:               pkix.Name{CommonName: "synthetic-monitor test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key}
}

// issue returns a server certificate for hosts (DNS names or IP addresses)
// valid from notBefore to notAfter.
func (ca *testCA) issue(t *testing.T, notBefore, notAfter time.Time, hosts ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		Subject:     pkix.Name{CommonName: hosts[0]},
		NotBefore:   notBefore,
		NotAfter:    notAfter,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: append([][]byte{der}, ca.chain...), PrivateKey: key}
}

// validFor issues a certificate for hosts valid from an hour ago until d from
// now.
func (ca *testCA) validFor(t *testing.T, d time.Duration, hosts ...string) tls.Certificate {
	t.Helper()
	return ca.issue(t, time.Now().Add(-time.Hour), time.Now().Add(d), hosts...)
}

// startTLSServer serves h over TLS with cert until the test ends.
func startTLSServer(t *testing.T, h http.Handler, cert tls.Certificate) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	// Handshakes that the probe rejects are expected; keep them out of the
	// test output.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}
