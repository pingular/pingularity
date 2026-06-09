//go:build linux

package netinfo

import "testing"

// On Linux traceroute-based exit discovery is available, so the fetch gate must
// NOT flag it unsupported.
func TestTraceSupportedLinux(t *testing.T) {
	if !traceSupported {
		t.Fatal("traceSupported = false on a Linux build")
	}
}
