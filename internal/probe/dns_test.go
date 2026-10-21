package probe

import (
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

func TestDNS(t *testing.T) {
	server := startFakeDNS(t, map[string]netip.Addr{"app.test.": netip.MustParseAddr("192.0.2.10")})
	tests := []struct {
		name   string
		host   string
		expect []netip.Addr
		want   Reason
	}{
		{"resolves", "app.test.", nil, ""},
		{"expected address present", "app.test.", []netip.Addr{netip.MustParseAddr("192.0.2.10")}, ""},
		{"expected address missing", "app.test.", []netip.Addr{netip.MustParseAddr("192.0.2.99")}, ReasonDNSAnswer},
		{"name does not exist", "missing.test.", nil, ReasonDNS},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := config.Check{DNS: &config.DNS{Host: tt.host, Server: server, ExpectAddresses: tt.expect}}
			wantReason(t, run(t, c, Options{}), tt.want)
		})
	}
}

func TestDNSUnresponsiveServer(t *testing.T) {
	// The resolver must not keep waiting for an answer after the probe
	// has given up.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	// A socket nobody reads from: queries arrive and are never answered.
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	c := config.Check{Timeout: 100 * time.Millisecond, DNS: &config.DNS{Host: "app.test.", Server: pc.LocalAddr().String()}}
	wantReason(t, run(t, c, Options{}), ReasonTimeout)
}

// startFakeDNS runs a minimal DNS server over UDP until the test ends and
// returns its address. It answers A queries for names in records, answers
// other query types for those names with no records, and returns NXDOMAIN
// for every other name.
func startFakeDNS(t *testing.T, records map[string]netip.Addr) string {
	t.Helper()
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return // closed by cleanup
			}
			if resp := dnsResponse(buf[:n], records); resp != nil {
				_, _ = pc.WriteTo(resp, from)
			}
		}
	}()
	t.Cleanup(func() {
		_ = pc.Close()
		<-done
	})
	return pc.LocalAddr().String()
}

// dnsResponse builds the reply to a DNS query in wire format (RFC 1035
// section 4), or returns nil if the query is malformed.
func dnsResponse(query []byte, records map[string]netip.Addr) []byte {
	const headerLen = 12
	if len(query) < headerLen {
		return nil
	}
	// The question name is a series of length-prefixed labels ending in a
	// zero byte, followed by a 2-byte type and a 2-byte class.
	var labels []string
	i := headerLen
	for i < len(query) && query[i] != 0 {
		n := int(query[i])
		if i+1+n > len(query) {
			return nil
		}
		labels = append(labels, string(query[i+1:i+1+n]))
		i += 1 + n
	}
	i++ // the zero byte
	if i+4 > len(query) {
		return nil
	}
	qtype := binary.BigEndian.Uint16(query[i:])
	i += 4
	name := strings.ToLower(strings.Join(labels, ".")) + "."
	addr, known := records[name]

	const (
		typeA   = 1
		classIN = 1
	)
	flags := uint16(0x8180) // response, recursion desired and available, no error
	if !known {
		flags |= 3 // NXDOMAIN
	}
	var answers uint16
	if known && qtype == typeA {
		answers = 1
	}

	resp := append([]byte{}, query[0], query[1]) // the query's ID
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1) // question count
	resp = binary.BigEndian.AppendUint16(resp, answers)
	resp = append(resp, 0, 0, 0, 0)            // authority and additional counts
	resp = append(resp, query[headerLen:i]...) // the question, echoed back
	if answers == 1 {
		resp = append(resp, 0xC0, headerLen) // name: pointer to the question's name
		resp = binary.BigEndian.AppendUint16(resp, typeA)
		resp = binary.BigEndian.AppendUint16(resp, classIN)
		resp = binary.BigEndian.AppendUint32(resp, 60) // TTL in seconds
		resp = binary.BigEndian.AppendUint16(resp, 4)  // address length
		ip := addr.As4()
		resp = append(resp, ip[:]...)
	}
	return resp
}
