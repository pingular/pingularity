package speedtest

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The packet-loss analyzer dials srv.Host - third-party catalogue data - over
// raw TCP and UDP. Its dialers must carry the same SSRF dial guard as every
// other destination in this package: a hostile catalogue entry naming an
// internal host must get neither the TCP sampler connect nor the UDP send.
func TestPacketLossAnalyzerRefusesInternalHost(t *testing.T) {
	clearProxyEnv(t) // an ambient proxy env var must not relax the guard for loopback

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	var accepted int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&accepted, 1)
			_ = c.Close()
		}
	}()

	host := ln.Addr().String()
	plMu.Lock()
	delete(plMap, host) // a stale cooldown entry would skip the dial under test
	plMu.Unlock()
	t.Cleanup(func() { plMu.Lock(); delete(plMap, host); plMu.Unlock() })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if loss := measurePacketLoss(ctx, &ookla.Server{Host: host}); loss != nil {
		t.Fatalf("loss = %v, want nil for a refused internal host", *loss)
	}
	if n := atomic.LoadInt32(&accepted); n != 0 {
		t.Fatalf("SSRF: the loss analyzer's TCP sampler reached the internal listener %d time(s)", n)
	}
}
