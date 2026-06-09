//go:build linux

package netinfo

// traceSupported reports whether traceroute-based exit discovery can run on this
// platform. Linux has the raw/ping ICMP sockets + IP_RECVERR it needs (see
// trace_linux.go); other platforms gate on this and explain the blank instead.
const traceSupported = true
