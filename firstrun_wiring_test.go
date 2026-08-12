package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/netinfo"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// The production assembly IS the feature: package tests cover injected hooks,
// but deleting `ni.WakeFn = set.Changed` (or miswiring it) would leave every
// package test green while the first speedtest quietly went back to racing an
// empty netinfo. This drives the REAL settings controller's close-and-replace
// broadcast into the REAL netinfo loop, wired exactly as main.go wires them.
func TestFirstRunWiringSettingsBroadcastWakesNetinfo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, settings.Values{Monitoring: false, NetinfoEnabled: true})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}

	ni := netinfo.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ni.EnabledFn = func() bool { return set.Monitoring() && set.NetinfoEnabled() }
	ni.WakeFn = set.Changed // the production wiring under test (main.go)

	loopCtx, loopCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { loopCancel(); <-done }()
	go func() { defer close(done); ni.Loop(loopCtx, time.Hour) }()

	time.Sleep(150 * time.Millisecond)
	if ni.Get().UpdatedAt != 0 {
		t.Fatal("lookup attempted while monitoring was off")
	}

	// The power button. A real mutate -> a real broadcast -> the loop must
	// attempt a refresh well inside its one-minute disabled poll.
	if err := set.SetMonitoring(ctx, true); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for ni.Get().UpdatedAt == 0 {
		if time.Now().After(deadline) {
			t.Fatal("power-on broadcast never reached the netinfo loop; the ni.WakeFn = set.Changed wiring is dead")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newFirstRunReadyFn's truth table, against the real controller and manager -
// this is the composition main.go hands to the scheduler, so a reordered or
// dropped short-circuit here is a 20-second stall (or a defeated wait) on
// every fresh install's first test.
func TestFirstRunReadyFnComposition(t *testing.T) {
	ctx := context.Background()
	mk := func(v settings.Values) (*settings.Controller, *netinfo.Manager) {
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		set, err := settings.New(ctx, st, v)
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		return set, netinfo.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	}

	// Fresh install, netinfo on, nothing published: not ready (the wait is the
	// feature - without it the first run races an empty netinfo).
	set, ni := mk(settings.Values{NetinfoEnabled: true})
	if newFirstRunReadyFn(set, ni)() {
		t.Error("fresh install with netinfo pending must NOT be ready")
	}

	// Pinned server: the race is irrelevant.
	set, ni = mk(settings.Values{NetinfoEnabled: true, SpeedServerID: "12345"})
	if !newFirstRunReadyFn(set, ni)() {
		t.Error("pinned server must be ready immediately")
	}

	// Searched city: overrides the race.
	set, ni = mk(settings.Values{NetinfoEnabled: true, SpeedAutoLoc: "43.65,-79.38"})
	if !newFirstRunReadyFn(set, ni)() {
		t.Error("searched city must be ready immediately")
	}

	// Connection info off: nothing is ever coming.
	set, ni = mk(settings.Values{NetinfoEnabled: false})
	if !newFirstRunReadyFn(set, ni)() {
		t.Error("netinfo disabled must be ready immediately")
	}

	// iperf3 engine: ready only when the binary is actually usable - an
	// unavailable iperf3 falls back to Ookla, which DOES need the race.
	set, ni = mk(settings.Values{NetinfoEnabled: true, SpeedEngine: "iperf3"})
	if got, want := newFirstRunReadyFn(set, ni)(), speedtest.IperfAvailable(); got != want {
		t.Errorf("iperf3 engine readiness = %v, want %v (IperfAvailable on this host)", got, want)
	}
}

// The pending->PUBLISHED half of the readiness truth table. TestFirstRunReadyFnComposition
// only pins the pending (false) side, so a mutation that leaves readiness
// permanently false AFTER netinfo publishes slips past it. This drives a real
// publish (even a failed lookup stamps UpdatedAt) and asserts readiness flips.
func TestFirstRunReadyFnReleasesAfterPublish(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, settings.Values{Monitoring: true, NetinfoEnabled: true})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	ni := netinfo.NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ni.EnabledFn = func() bool { return set.Monitoring() && set.NetinfoEnabled() }
	ready := newFirstRunReadyFn(set, ni)

	if ready() {
		t.Fatal("must NOT be ready before netinfo publishes (UpdatedAt still 0)")
	}
	loopCtx, loopCancel := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { loopCancel(); <-done }()
	go func() { defer close(done); ni.Loop(loopCtx, time.Hour) }()

	deadline := time.Now().Add(15 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("readiness never released after netinfo published (UpdatedAt=%d)", ni.Get().UpdatedAt)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The wiring lives in main.go's run() assembly, which isn't unit-testable, and
// the tests above re-create it locally - so deleting it from main.go leaves them
// green (the exact gap that shipped). This source-presence guard fails if the
// production wiring is removed or renamed. It is deliberately literal.
func TestMainWiresFirstRunAndOptsHooks(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ni.WakeFn = set.Changed",                     // #3: first speedtest must not race an empty netinfo
		"sched.ReadyFn = newFirstRunReadyFn(set, ni)", // #3: selection readiness predicate
		"replayIgnoredOpts(",                          // #7: ignored-PINGULARITY_OPTS warning is replayed at boot
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go no longer wires %q - package tests won't catch it; the behavior is silently dead", want)
		}
	}
}
