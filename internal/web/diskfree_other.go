//go:build !linux && !darwin && !freebsd && !windows

package web

// diskFree is unsupported on this platform, so the disk_free gauge is omitted.
func diskFree(string) (uint64, bool) { return 0, false }
