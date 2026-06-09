//go:build !windows

package osperm

import "os"

// SecureFile restricts path to owner read/write (0600).
func SecureFile(path string) error { return os.Chmod(path, 0o600) }

// SecureDir restricts path to owner read/write/execute (0700).
func SecureDir(path string) error { return os.Chmod(path, 0o700) }
