//go:build !windows

package web

import "os"

// openFDs counts this process's open file descriptors: /proc/self/fd on Linux,
// /dev/fd elsewhere on Unix. A slow FD leak (unclosed sockets/files) shows here
// long before "too many open files". Returns ok=false if neither path exists.
func openFDs() (int, bool) {
	for _, p := range []string{"/proc/self/fd", "/dev/fd"} {
		if d, err := os.Open(p); err == nil {
			names, err := d.Readdirnames(-1)
			d.Close()
			if err == nil {
				n := len(names) - 1 // discount the fd opened for the directory read itself
				if n < 0 {
					n = 0
				}
				return n, true
			}
		}
	}
	return 0, false
}
