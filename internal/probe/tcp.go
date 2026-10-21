package probe

import (
	"context"
	"net"
)

type tcpProber struct {
	address string
}

func (p *tcpProber) Probe(ctx context.Context) Result {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", p.address)
	if err != nil {
		return Result{Reason: classify(err), Err: err}
	}
	_ = conn.Close()
	return Result{}
}
