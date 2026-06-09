//go:build windows

package netinfo

// traceSupported reports whether traceroute-based exit discovery can run on this
// platform. Windows traces via the iphlpapi IcmpSendEcho API (see
// trace_windows.go); the fetch gate reads this to decide whether to trace or
// explain the blank.
const traceSupported = true
