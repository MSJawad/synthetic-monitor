package probe

import (
	"net"
	"testing"

	"github.com/MSJawad/synthetic-monitor/internal/config"
)

func TestTCPOpenPort(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	wantReason(t, run(t, config.Check{TCP: &config.TCP{Address: ln.Addr().String()}}, Options{}), "")
}

func TestTCPClosedPort(t *testing.T) {
	wantReason(t, run(t, config.Check{TCP: &config.TCP{Address: closedPort(t)}}, Options{}), ReasonConnect)
}
