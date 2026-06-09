package netstat

import (
	"context"
	"testing"
	"time"
)

// fakeCounters swaps the sampling seam for a fixed sequence of snapshots; the
// last snapshot repeats if read again.
func fakeCounters(t *testing.T, snaps ...map[string]uint64) {
	t.Helper()
	orig := readBytesFn
	i := 0
	readBytesFn = func() (map[string]uint64, bool) {
		s := snaps[i]
		if i < len(snaps)-1 {
			i++
		}
		return s, true
	}
	t.Cleanup(func() { readBytesFn = orig })
}

// Throughput reports the busiest single interface, not the sum: the same bytes
// showing on eth0 AND its veth must not double-count. Each interface moves
// 125000 bytes (1e6 bits) over a >=100ms window, so the max-over-interfaces rate
// can never exceed 10 Mbps, while a summed implementation would read ~20 Mbps.
func TestThroughputMaxOverInterfaces(t *testing.T) {
	fakeCounters(t,
		map[string]uint64{"eth0": 0, "veth0": 0},
		map[string]uint64{"eth0": 125_000, "veth0": 125_000},
	)
	start := time.Now()
	rate, ok := Throughput(context.Background(), 100*time.Millisecond)
	outer := time.Since(start).Seconds()
	if !ok {
		t.Fatal("Throughput returned ok=false with readable counters")
	}
	// The sampled window is at least the requested 100ms, so one interface's rate
	// tops out at exactly 10 Mbps; the sum of both would have to read above that
	// unless the sample loop stalled to twice the window.
	if rate > 10.0000001 {
		t.Fatalf("rate = %v Mbps, want <= 10 (max over interfaces, not the sum)", rate)
	}
	// Lower bound from our own outer timing: the window elapsed can't exceed it.
	if lo := 1e6 / outer / 1e6 * 0.99; rate < lo {
		t.Fatalf("rate = %v Mbps, want >= %v (~10 Mbps over the sampled window)", rate, lo)
	}
}

// An interface present only in the second snapshot (hotplug, container start)
// has no baseline and must be ignored, not read as a burst from zero.
func TestThroughputIgnoresNewInterface(t *testing.T) {
	fakeCounters(t,
		map[string]uint64{"eth0": 1000},
		map[string]uint64{"eth0": 1000, "wg0": 1 << 40},
	)
	rate, ok := Throughput(context.Background(), time.Millisecond)
	if !ok || rate != 0 {
		t.Fatalf("rate=%v ok=%v, want 0 rate (new interface ignored) and ok", rate, ok)
	}
}

// A counter that went backwards (wrap or driver reset) is skipped rather than
// underflowing into a huge unsigned delta.
func TestThroughputSkipsBackwardsCounter(t *testing.T) {
	fakeCounters(t,
		map[string]uint64{"eth0": 5000},
		map[string]uint64{"eth0": 400},
	)
	rate, ok := Throughput(context.Background(), time.Millisecond)
	if !ok || rate != 0 {
		t.Fatalf("rate=%v ok=%v, want 0 rate (reset counter skipped) and ok", rate, ok)
	}
}

// A context cancelled during the sampling wait must yield ok=false promptly, so
// the caller never treats the link as busy off a half-taken sample.
func TestThroughputCancelledContext(t *testing.T) {
	fakeCounters(t,
		map[string]uint64{"eth0": 0},
		map[string]uint64{"eth0": 1 << 30},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	var rate float64
	var ok bool
	go func() { rate, ok = Throughput(ctx, 30*time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Throughput did not return promptly on a cancelled context")
	}
	if ok {
		t.Fatalf("rate=%v ok=%v, want ok=false on cancellation", rate, ok)
	}
}
