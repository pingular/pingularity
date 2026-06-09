//go:build windows

package netinfo

import "testing"

// On Windows traceroute-based exit discovery is available (trace_windows.go), so
// the fetch gate must NOT flag it unsupported.
func TestTraceSupportedWindows(t *testing.T) {
	if !traceSupported {
		t.Fatal("traceSupported = false on a windows build")
	}
}
