//go:build !linux

package web

// residentBytes is read from /proc on Linux only, so the resident-memory gauge
// is simply absent elsewhere.
func residentBytes() (uint64, bool) { return 0, false }
