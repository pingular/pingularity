//go:build darwin

package netinfo

import "testing"

// On macOS traceroute-based exit discovery is available (trace_darwin.go), so the
// fetch gate must NOT flag it unsupported.
func TestTraceSupportedDarwin(t *testing.T) {
	if !traceSupported {
		t.Fatal("traceSupported = false on a darwin build")
	}
}
