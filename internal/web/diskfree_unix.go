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
	// On FreeBSD Statfs_t.Bavail is signed and legitimately goes NEGATIVE once
	// non-root usage eats into the root reserve - i.e. exactly when the disk is
	// effectively full. Converting that straight to uint64 wraps to ~18 EB and the
	// early-warning gauge would report maximal free space at the moment it should
	// fire. Clamp to 0. (Linux/darwin Bavail is unsigned and never trips this.)
	avail := int64(st.Bavail)
	if avail < 0 {
		return 0, true
	}
	return uint64(avail) * uint64(st.Bsize), true
}
