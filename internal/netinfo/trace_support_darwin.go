//go:build darwin

package netinfo

// traceSupported reports whether traceroute-based exit discovery can run on this
// platform. macOS has the raw/datagram ICMP sockets it needs (see trace_darwin.go);
// the fetch gate reads this to decide whether to trace or explain the blank.
const traceSupported = true
