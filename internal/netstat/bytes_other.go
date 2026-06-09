//go:build !linux && !darwin && !windows

package netstat

// readBytes has no implementation off the supported platforms (Linux/macOS/Windows);
// the busy-defer feature is simply unavailable here (Throughput returns ok=false,
// link treated as idle).
func readBytes() (map[string]uint64, bool) { return nil, false }
