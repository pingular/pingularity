package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
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
	// Hermetic: the seams above cover the egress lookup, but rDNS/Cymru ride
	// net.DefaultResolver - on a networked machine those real lookups ran this
	// test to ~2s of its 3s timer-wait, and under full-suite load it tipped
	// over (flaked Aug 2026). Fail them instantly instead.
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	defer func() { net.DefaultResolver = oldRes }()

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

// The first-boot empty-Exit gap (Aug 2026): the boot refresh succeeds on the
// fast fields (snapshot healthy, Error=="") but the exit trace fails - and a
// trace-only failure used to leave the Loop on the full maxStale cadence, so
// the Exit row stayed empty for an hour unless the user clicked refresh. The
// Loop must treat "exit expected, attempted, missing" like an error and arm
// the short errRetryStale-based timer. Deleting exitMissing from the staleness
// caps makes wait ~maxStale (this test fails).
func TestLoopArmsFastRetryWhileExitMissing(t *testing.T) {
	oldV4, oldV6, oldRE, oldAfter := ipv4Client, ipv6Client, resolverEgress, afterFn
	defer func() { ipv4Client, ipv6Client, resolverEgress, afterFn = oldV4, oldV6, oldRE, oldAfter }()
	ipv4Client = canned(200, "203.0.113.5") // the echo works: identity healthy, trace allowed
	ipv6Client = canned(500, "")
	resolverEgress = func(context.Context) string { return "" }
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		return nil, errors.New("no raw socket")
	})

	waits := make(chan time.Duration, 4)
	block := make(chan time.Time)
	afterFn = func(d time.Duration) <-chan time.Time { waits <- d; return block }

	// Hermetic: fail every real DNS lookup (Cymru ASN, rDNS) instantly, so the
	// test neither touches the network nor races its 5s timer-wait against a
	// slow or blackholed resolver.
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	defer func() { net.DefaultResolver = oldRes }()

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The geo/ISP lookup must succeed too - an "isp lookup failed" Error would
	// put the Loop on the fast cadence via the EXISTING error cap, and this
	// test must isolate the exit-missing cap. The ISP rides the speed-history
	// fallback (Cymru fails instantly above), same as fetch's own design for a
	// blank Cymru answer.
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	m.LastKnownFn = func() *Info { return &Info{PublicIP: "203.0.113.5", ISP: "AS64496 Example"} }

	const maxStale = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Loop(ctx, maxStale) }()

	var wait time.Duration
	select {
	case wait = <-waits:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("Loop never armed its backstop timer")
	}
	cancel()
	<-done

	// Preconditions: this is exactly the gap state - healthy snapshot, trace
	// allowed, attempted, no exit. If any of these fail the setup drifted and
	// the wait assertion below would be testing something else.
	if got := m.Get().Error; got != "" {
		t.Fatalf("boot refresh flagged an error (%q); this test needs the healthy-snapshot state where only the trace failed", got)
	}
	if got := m.Get().ExitUnavailable; got != "" {
		t.Fatalf("ExitUnavailable=%q; this test needs a host where the trace is expected to work", got)
	}
	if !m.exitMissing() {
		t.Fatal("exitMissing()=false after the boot refresh; the failed trace did not leave the missing-exit state")
	}
	if wait > 2*errRetryStale {
		t.Fatalf("Loop armed %v with the Exit row empty; want the errRetryStale-based cap (~%v), not maxStale (%v) - the Exit row would sit empty for an hour", wait, errRetryStale, maxStale)
	}
}

// A refresh driven from OUTSIDE the Loop (reconnect, post-speedtest, manual)
// whose trace attempt leaves the Exit row missing must nudge the Loop - it is
// mid-sleep on a timer armed back when the row was fine, up to a full maxStale
// away, and without the nudge the first-boot fix only covers the Loop's own
// refreshes.
func TestRefreshNudgesLoopWhenExitMissing(t *testing.T) {
	oldV4, oldV6, oldRE := ipv4Client, ipv6Client, resolverEgress
	defer func() { ipv4Client, ipv6Client, resolverEgress = oldV4, oldV6, oldRE }()
	ipv4Client = canned(200, "203.0.113.5")
	ipv6Client = canned(500, "")
	resolverEgress = func(context.Context) string { return "" }
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	defer func() { net.DefaultResolver = oldRes }()
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		return nil, errors.New("no raw socket")
	})

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	m.LastKnownFn = func() *Info { return &Info{PublicIP: "203.0.113.5", ISP: "AS64496 Example"} }

	m.Refresh(context.Background())

	if !m.exitMissing() {
		t.Fatal("refresh with a failing trace did not leave the missing-exit state; the nudge assertion below would be vacuous")
	}
	select {
	case <-m.nudge:
	default:
		t.Fatal("refresh left the Exit row missing but did not nudge the Loop: an externally-driven failure would sleep out the previously-armed wait, up to a full maxStale")
	}
}
