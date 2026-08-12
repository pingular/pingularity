package netinfo

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// Loop sizes its backstop timer from the staleness cap, and the cap is recomputed
// AFTER the refresh (netinfo.go:435-443): a boot/refresh can flip the snapshot
// healthy->error, and the error path wants the fast errRetryStale retry, not the
// full maxStale the pre-refresh healthy read implied. This drives that exact flip
// with no network - the boot snapshot is empty (Error==""), and the first refresh
// fails every lookup deterministically ("ip lookup failed"), which CarriedIdentity
// short-circuits before any traceroute. The armed wait must be the short
// errRetryStale-based cap, not maxStale. Deleting the post-refresh recompute makes
// wait ~maxStale (this test fails).
func TestLoopArmsFastRetryWhenRefreshFlipsToError(t *testing.T) {
	oldV4, oldV6, oldRE, oldAfter := ipv4Client, ipv6Client, resolverEgress, afterFn
	defer func() { ipv4Client, ipv6Client, resolverEgress, afterFn = oldV4, oldV6, oldRE, oldAfter }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(500, "")
	resolverEgress = func(context.Context) string { return "" } // no DNS egress -> no real lookups

	waits := make(chan time.Duration, 4)
	block := make(chan time.Time) // never fires: Loop parks in the select until ctx cancel
	afterFn = func(d time.Duration) <-chan time.Time { waits <- d; return block }

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(500, "") // cfColo / geo answer nothing

	const maxStale = time.Hour // >> errRetryStale (5m), so the two caps are unmistakable
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Loop(ctx, maxStale) }()

	var wait time.Duration
	select {
	case wait = <-waits:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("Loop never armed its backstop timer")
	}
	cancel()
	<-done

	// The refresh flipped the snapshot to an error; the timer must be armed off
	// that post-refresh error state (errRetryStale ~5m), not the pre-refresh
	// healthy maxStale (1h).
	if got := m.Get().Error; got == "" {
		t.Fatalf("boot refresh did not flip the snapshot to an error; got Error=%q", got)
	}
	if wait > 10*time.Minute {
		t.Fatalf("post-error retry armed at %v; want the short errRetryStale-based cap (~%v), not the pre-refresh maxStale (%v) - the post-refresh cap recompute (netinfo.go:440-443) is gone", wait, errRetryStale, maxStale)
	}
}
