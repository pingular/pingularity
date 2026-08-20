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
// dnsAnsweredErrno reports whether a lookup error means the resolver ANSWERED
// "no such record" at the getaddrinfo level. Go's winError maps
// WSAHOST_NOT_FOUND (11001), DNS_ERROR_RCODE_NAME_ERROR (9003) and
// DNS_INFO_NO_RECORDS (9501) to DNSError.IsNotFound, but not WSANO_DATA
// (11004, "valid name, no data of the requested type") - and that is the
// shape GetAddrInfoW can give the DNS probe's steady-state answer: a NODATA
// for the random name's single requested A type. Only a resolver that
// answered can produce it - a dead, unreachable or SERVFAILing resolver
// cannot - so counting it as answered can never mask a real failure. Mirrors
// the stub in dialerr_other.go.
func dnsAnsweredErrno(err error) bool {
	return errors.Is(err, windows.WSANO_DATA)
}

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
