package probe

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

// systemResolver resolves hostnames for HTTP, TCP and TLS probes.
var systemResolver = newResolver("")

// newResolver returns a resolver that sends queries to server (host:port),
// or to the nameservers in /etc/resolv.conf if server is empty.
//
// It uses Go's own DNS client, which behaves the same on every platform and
// has no cache, so every probe sends real queries. Like any resolver it
// answers names listed in /etc/hosts from that file.
func newResolver(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			if server != "" {
				address = server
			}
			var d net.Dialer
			conn, err := d.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			// The resolver bounds each query only by a read deadline (the
			// per-attempt timeout from resolv.conf, 5s by default), so a
			// probe that gives up sooner would leave the query's goroutine
			// and socket waiting until then. Closing the connection when the
			// lookup's context ends releases them at once. conn is returned
			// unwrapped because the resolver checks for net.PacketConn to
			// choose between UDP and TCP message framing.
			context.AfterFunc(ctx, func() { _ = conn.Close() })
			return conn, nil
		},
	}
}

type dnsProber struct {
	host     string
	expect   []netip.Addr
	resolver *net.Resolver
}

func newDNS(c *config.DNS) *dnsProber {
	return &dnsProber{host: c.Host, expect: c.ExpectAddresses, resolver: newResolver(c.Server)}
}

func (p *dnsProber) Probe(ctx context.Context) Result {
	addrs, err := p.resolver.LookupNetIP(ctx, "ip", p.host)
	if err != nil {
		return Result{Reason: classify(err), Err: err}
	}
	// IPv4 answers can come back as IPv4-mapped IPv6 addresses.
	for i := range addrs {
		addrs[i] = addrs[i].Unmap()
	}
	var missing []netip.Addr
	for _, want := range p.expect {
		if !slices.Contains(addrs, want.Unmap()) {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return Result{Reason: ReasonDNSAnswer, Err: fmt.Errorf("answer %v is missing %v", addrs, missing)}
	}
	return Result{}
}
