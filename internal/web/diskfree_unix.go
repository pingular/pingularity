//go:build linux || darwin || freebsd

package web

import "syscall"

// diskFree returns the bytes available to a non-root user on the filesystem
// containing path (Bavail, the figure that actually matters for the service).
// ok=false on error.
func diskFree(path string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
