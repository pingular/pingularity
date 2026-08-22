package main

import (
	"net"
	"testing"
)

// Case and the DNS root dot are spellings of the same name, and Go binds 127.0.0.1
// for all of them - so none may fire the startup "authentication OFF" warning. Each
// case binds the address first, because what the kernel binds is what "reachable"
// means here.
func TestNonLoopbackListenLocalhostSpellings(t *testing.T) {
	for _, addr := range []string{"localhost:9000", "LOCALHOST:9000", "Localhost:9000", "localhost.:9000"} {
		t.Run(addr, func(t *testing.T) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				t.Fatalf("SplitHostPort(%q): %v", addr, err)
			}
			ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("cannot bind %q on this host: %v", host, err)
			}
			bound, _, _ := net.SplitHostPort(ln.Addr().String())
			ln.Close()
			if ip := net.ParseIP(bound); ip == nil || !ip.IsLoopback() {
				t.Skipf("%q bound %s on this host, not loopback", host, bound)
			}
			if nonLoopbackListen(addr) {
				t.Errorf("nonLoopbackListen(%q) = true, want false (socket bound %s)", addr, bound)
			}
		})
	}
}
