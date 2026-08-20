//go:build !windows

package prober

import (
	"errors"
	"syscall"
)

// dialErrno maps a dial failure to its probe.fail.* class by the underlying
// POSIX errno, or "" when it's none of them. The Windows twin
// (dialerr_windows.go) mirrors this against the WSA errnos.
// dnsAnsweredErrno is the POSIX stub of the Windows check (see
// dialerr_windows.go): only there does the system resolver surface an
// answered "no such record" as an errno Go does not map to IsNotFound.
func dnsAnsweredErrno(error) bool { return false }

func dialErrno(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, syscall.ENETUNREACH):
		return "net_unreachable"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host_unreachable"
	}
	return ""
}
