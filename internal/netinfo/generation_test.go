package netinfo

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"
)

// The refresh-ordering contract. A later-STARTED refresh (higher generation)
// outranks an earlier one, so once it publishes, the older/slower one is refused -
// it can't clobber the newer snapshot when it finally returns. A refresh's own
// several effects (m.info, then the exit patch) still pass while it stays current.
func TestGenerationPublishOrdering(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	g1 := m.nextGen()
	g2 := m.nextGen()
	if g2 <= g1 {
		t.Fatalf("nextGen not monotonic: g1=%d g2=%d", g1, g2)
	}
	if !m.claimPublish(g2) {
		t.Fatal("newer generation g2 should be allowed to publish")
	}
	if m.claimPublish(g1) {
		t.Fatal("older, slower generation g1 must not publish after g2 already did")
	}
	if !m.claimPublish(g2) {
		t.Fatal("current generation must be able to commit a second effect (info, then the exit patch)")
	}
}

// Part two of the in-flight-trace contract: a trace started on the OLD network must not overwrite the
// deliberate IP-change cache-bust when it finally lands. The bust bumps traceGen
// mid-trace; at commit the trace sees the generation moved and drops its result,
// leaving the cache cleared so the next caller re-traces the current network.
func TestCachedExitDropsOutOfGenerationTrace(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		close(entered)
		<-release // hold the trace in flight, against the OLD network
		return []tHop{{TTL: 1, IP: "10.0.0.1"}, {TTL: 2, IP: "not-an-ip"}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(404, "")
	m.ExitTargetFn = func() string { return "1.1.1.1" }

	got := make(chan *ExitInfo, 1)
	go func() { got <- m.cachedExit(context.Background(), "1403") }()
	<-entered
	// The IP-change bust lands mid-trace (as Refresh does): clear the cache and
	// bump the generation the in-flight trace was started under.
	m.traceMu.Lock()
	m.traceAt, m.exit, m.tracedFor = time.Time{}, nil, ""
	m.traceGen++
	m.traceMu.Unlock()
	close(release)
	<-got

	m.traceMu.Lock()
	exit, at := m.exit, m.traceAt
	m.traceMu.Unlock()
	if exit != nil {
		t.Errorf("out-of-generation trace overwrote the cache-bust: %+v", exit)
	}
	if !at.IsZero() {
		t.Error("out-of-generation trace re-stamped traceAt, hiding the bust and suppressing a re-trace")
	}
}

// Part one of the in-flight-trace contract: while a trace toward target A is in flight, a caller wanting a
// newly-selected target B must NOT be handed A's path by the single-flight waiter.
// After waiting it re-validates the cache against its own target and traces B.
func TestCachedExitWaiterRetracesForNewTarget(t *testing.T) {
	var mu sync.Mutex
	var traced [][4]byte
	entered := make(chan struct{})
	release := make(chan struct{})
	stubTrace(t, func(_ context.Context, dst [4]byte, _ int, _ time.Duration) ([]tHop, error) {
		mu.Lock()
		traced = append(traced, dst)
		first := len(traced) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release // hold trace A (target 9.9.9.9) in flight
		}
		return []tHop{{TTL: 1, IP: "10.0.0.1"}, {TTL: 2, IP: "not-an-ip"}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(404, "")
	var tmu sync.Mutex
	target := "9.9.9.9"
	m.ExitTargetFn = func() string {
		tmu.Lock()
		defer tmu.Unlock()
		return target
	}

	go m.cachedExit(context.Background(), "1403") // caller 1 -> trace A (9.9.9.9)
	<-entered
	tmu.Lock()
	target = "8.8.4.4" // the exit target changes while A is in flight
	tmu.Unlock()

	entered2 := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered2)
		m.cachedExit(context.Background(), "1403") // caller 2 -> wants B (8.8.4.4)
		close(done)
	}()
	<-entered2
	for i := 0; i < 200; i++ { // let caller 2 reach the single-flight waiter
		runtime.Gosched()
	}
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	var sawA, sawB bool
	for _, d := range traced {
		sawA = sawA || d == [4]byte{9, 9, 9, 9}
		sawB = sawB || d == [4]byte{8, 8, 4, 4}
	}
	if !sawA || !sawB {
		t.Fatalf("traced %v, want both 9.9.9.9 and 8.8.4.4 - a waiter for the new target must re-trace, not return the in-flight old-target result", traced)
	}
}
