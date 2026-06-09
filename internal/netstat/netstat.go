// Package netstat samples the host's network-interface byte counters to estimate
// current link throughput. It exists so a scheduled speedtest can be deferred
// while the link is already moving real traffic - the test then neither competes
// with that traffic nor measures an already-saturated link. Supported on Linux
// (/proc/net/dev), macOS (routing sysctl), and Windows (GetIfTable2); other
// platforms report "unsupported" and the caller treats the link as not-busy.
package netstat

import (
	"context"
	"time"
)

// readBytesFn is the counter-sampling seam Throughput reads through: production
// uses the platform's readBytes; tests substitute canned snapshots.
var readBytesFn = readBytes

// Supported reports whether per-interface byte counters are readable here (at
// least one non-loopback interface with readable counters on the platforms that
// have an implementation - Linux/macOS/Windows). Only used to gate the UI's
// busy-defer toggle; the runtime path no-ops on its own when unsupported, so this
// is purely a UX hint.
func Supported() bool {
	m, ok := readBytes()
	return ok && len(m) > 0
}

// Throughput samples per-interface (rx+tx) byte counters, waits d, samples again,
// and returns the busiest single interface's average rate in Mbps. ok is false
// when counters are unavailable (unsupported platform, read error) or ctx is
// cancelled during the wait, in which case the caller must not treat the link as
// busy.
//
// It takes the max over interfaces, NOT the sum: a container/VPN download shows on
// the veth, the bridge, AND the physical NIC, so summing would multiply-count it.
// The busiest interface is the closest single-number proxy for "the link is moving
// data" without that inflation.
func Throughput(ctx context.Context, d time.Duration) (float64, bool) {
	b0, ok := readBytesFn()
	if !ok || len(b0) == 0 {
		return 0, false
	}
	start := time.Now()
	select {
	case <-ctx.Done():
		return 0, false
	case <-time.After(d):
	}
	b1, ok := readBytesFn()
	if !ok {
		return 0, false
	}
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	var maxRate float64
	for name, end := range b1 {
		begin, seen := b0[name]
		if !seen || end < begin {
			continue // interface appeared mid-sample, or a counter wrapped/reset
		}
		if rate := float64(end-begin) * 8 / elapsed / 1e6; rate > maxRate {
			maxRate = rate
		}
	}
	return maxRate, true
}
