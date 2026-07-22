//go:build !windows

package osperm

import "os"

// SecureFile restricts path to owner read/write (0600).
func SecureFile(path string) error { return os.Chmod(path, 0o600) }

// SecureDir restricts path to owner read/write/execute (0700).
func SecureDir(path string) error { return os.Chmod(path, 0o700) }

// GroupOrWorldAccessible reports whether path grants any permission to group or
// other (mode & 0o077 != 0). known is false when the mode can't be read (e.g. the
// file does not exist). Callers use it to VERIFY that a securing attempt actually
// left the file owner-only - so a chmod that failed skippably (an existing DB owned
// by another user, an FS that can't express perms) can't silently leave it exposed.
func GroupOrWorldAccessible(path string) (accessible, known bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, false
	}
	return fi.Mode().Perm()&0o077 != 0, true
}
