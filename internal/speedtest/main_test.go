package speedtest

import (
	"net"
	"os"
	"testing"
)

// Point the load sampler's fixed anycast target at a local listener so the
// package's tests never depend on (or block on) the real 1.1.1.1 network path.
func TestMain(m *testing.M) {
	code := func() int {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err == nil {
			lulTarget = ln.Addr().String()
			defer ln.Close()
			// Accept and immediately close every dial. The LUL sampler only needs the
			// TCP handshake to complete (it times connect RTT, then closes), so a bare
			// accept-then-close satisfies it. Without draining, the OS accept backlog
			// fills once enough samples run - under -count=N/-shuffle a package can dial
			// this hundreds of times - and later dials then block on an unanswered SYN,
			// hanging the run. The loop ends when the deferred Close makes Accept error.
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					_ = c.Close()
				}
			}()
		}
		return m.Run()
	}()
	os.Exit(code)
}
