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
		}
		return m.Run()
	}()
	os.Exit(code)
}
