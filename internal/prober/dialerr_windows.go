//go:build windows

package prober

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// dialErrno maps a dial failure to its probe.fail.* class by the underlying
// errno, or "" when it's none of them. On Windows the net stack surfaces the
// Winsock (WSA) errnos, which don't equal syscall's synthetic POSIX constants,
// so match those first; the syscall.E* checks stay as a fallback. Mirrors the
// POSIX twin in dialerr_other.go.
func dialErrno(err error) string {
	switch {
	case errors.Is(err, windows.WSAECONNREFUSED), errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, windows.WSAENETUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "net_unreachable"
	case errors.Is(err, windows.WSAEHOSTUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "host_unreachable"
	}
	return ""
}
