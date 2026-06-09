//go:build !linux && !darwin && !windows

package netinfo

import (
	"context"
	"errors"
	"time"
)

// traceSupported is false on this fallback platform (neither Linux, darwin, nor
// Windows - e.g. the *BSDs or plan9): traceroute needs raw/ping ICMP sockets, so
// exit discovery is skipped - fetch sets ExitUnavailable (internal/API-visible;
// the dashboard hides the Exit row entirely) so Refresh never starts the doomed
// trace (see fetch's traceSupported gate). Linux/darwin/windows each set true in
// their own trace_support_*.go.
const traceSupported = false

// errUnsupportedPlatform is what the stub traceroute returns. Callers gate on
// traceSupported before ever reaching it, so it should not surface in practice -
// it's the sentinel that keeps the doomed trace and its trace_fail metric at bay.
var errUnsupportedPlatform = errors.New("traceroute: exit discovery is unsupported on this platform")

// traceroute has no native implementation on this platform; it fails immediately
// with the platform sentinel and only the Cloudflare PoP shows.
func traceroute(ctx context.Context, dst [4]byte, maxTTL int, probeTimeout time.Duration) ([]tHop, error) {
	return nil, errUnsupportedPlatform
}
