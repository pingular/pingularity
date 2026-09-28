//go:build linux

package web

import "os"

// residentBytes is the memory this process holds in RAM (its resident set
// size). The Go runtime's own figures leave out what SQLite allocates for
// itself. Returns ok=false if /proc is not mounted.
func residentBytes() (uint64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	return statmResident(b, os.Getpagesize())
}
