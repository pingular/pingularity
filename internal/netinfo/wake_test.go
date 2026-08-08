package netinfo

import (
	"context"
	"sync"
	"testing"
	"time"
)

// wakeSource mimics settings.Controller.Changed's contract: hand out the
// CURRENT channel; a broadcast closes it and installs a replacement. The old
// version of this file closed one fixed channel, which both hot-spun the loop
// and could never exercise the lost-generation hazard the contract carries.
type wakeSource struct {
	mu sync.Mutex
	ch chan struct{}
}

func newWakeSource() *wakeSource { return &wakeSource{ch: make(chan struct{})} }
func (w *wakeSource) changed() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ch
}
func (w *wakeSource) broadcast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	close(w.ch)
	w.ch = make(chan struct{})
}

// A settings wake must make Loop re-evaluate IMMEDIATELY - the disabled poll
// ticks only once a minute, and the first speedtest's server selection
// (scheduler ReadyFn) waits on the refresh this triggers. Pinned precisely:
// this proves the WAKE reaches Loop's evaluation well inside the minute tick;
// which branch then refreshes (the resume one-shot, or staleness - a
// zero-info cache is maximally stale) is not distinguished here. Offline-safe
// by the package convention: a refresh ATTEMPT stamps UpdatedAt even when the
// fetch fails, so the assertion is on the stamp, not on lookup success.
func TestLoopWakeRefreshesOnResume(t *testing.T) {
	m := quietManager()
	var mu sync.Mutex
	cur := false
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return cur }
	src := newWakeSource()
	m.WakeFn = src.changed

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); m.Loop(ctx, time.Hour) }()

	// Disabled boot: no attempt.
	time.Sleep(100 * time.Millisecond)
	if m.Get().UpdatedAt != 0 {
		t.Fatal("lookup attempted while disabled")
	}

	// Power-on + broadcast: well inside the old one-minute poll latency.
	mu.Lock()
	cur = true
	mu.Unlock()
	src.broadcast()
	deadline := time.Now().Add(15 * time.Second)
	for m.Get().UpdatedAt == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no refresh attempt within 15s of the wake; resume still rides the minute tick")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Nudge must reach the same evaluation with NO broadcast at all - it exists
// for the Quick Setup 48h expiry, a pure clock edge that flips no settings.
func TestLoopNudgeRefreshesOnResume(t *testing.T) {
	m := quietManager()
	var mu sync.Mutex
	cur := false
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return cur }
	src := newWakeSource()
	m.WakeFn = src.changed

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); m.Loop(ctx, time.Hour) }()

	time.Sleep(100 * time.Millisecond)
	if m.Get().UpdatedAt != 0 {
		t.Fatal("lookup attempted while disabled")
	}

	mu.Lock()
	cur = true
	mu.Unlock()
	m.Nudge() // no broadcast - the 48h-expiry shape
	deadline := time.Now().Add(15 * time.Second)
	for m.Get().UpdatedAt == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no refresh attempt within 15s of the nudge")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The lost-wake ordering pin. Changed() hands out a channel a broadcast closes
// AND REPLACES, so the loop must subscribe before reading the state it sleeps
// on. This drives the adversarial interleaving deterministically: the enable
// flips - and the broadcast fires - from INSIDE the loop's own enabled() read,
// i.e. strictly after this iteration's subscription under the fixed ordering
// (and strictly before it under the old fetch-after-check ordering, where the
// loop then slept out its full minute on the replacement channel).
func TestLoopWakeNotLostAcrossEnableRace(t *testing.T) {
	m := quietManager()
	src := newWakeSource()
	m.WakeFn = src.changed

	var mu sync.Mutex
	cur := false
	armed := false
	m.EnabledFn = func() bool {
		mu.Lock()
		defer mu.Unlock()
		if armed {
			armed = false
			cur = true
			src.broadcast() // lands between this read and the (old) fetch
			return false    // the stale answer the caller acts on
		}
		return cur
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); m.Loop(ctx, time.Hour) }()

	time.Sleep(150 * time.Millisecond) // loop is asleep on its disabled timer
	mu.Lock()
	armed = true
	mu.Unlock()
	m.Nudge() // wake it so the next evaluation hits the armed read

	deadline := time.Now().Add(15 * time.Second)
	for m.Get().UpdatedAt == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the enable's broadcast was lost; the loop slept on the replacement channel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
