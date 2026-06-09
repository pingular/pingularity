package monitor

import "testing"

// checkDegraded must debounce (two rounds over the threshold), fire OnDegraded
// once per episode, re-arm on recovery, and never fire while fully offline or
// with detection disabled.
func TestCheckDegraded(t *testing.T) {
	fired := 0
	m := &Monitor{
		DegradedPingFn: func() float64 { return 100 },
		OnDegraded:     func() { fired++ },
	}

	m.checkDegraded(50, true, true) // below threshold
	if fired != 0 || m.degradedStreak != 0 {
		t.Fatalf("below threshold should not arm: fired=%d streak=%d", fired, m.degradedStreak)
	}
	m.checkDegraded(150, true, true) // 1st over - needs 2
	if fired != 0 {
		t.Fatalf("one round over should not fire yet, fired=%d", fired)
	}
	m.checkDegraded(150, true, true) // 2nd over - fire once
	if fired != 1 {
		t.Fatalf("two rounds over should fire once, fired=%d", fired)
	}
	m.checkDegraded(150, true, true) // still over - no re-fire this episode
	if fired != 1 {
		t.Fatalf("should fire once per episode, fired=%d", fired)
	}
	m.checkDegraded(40, true, true) // recover
	if m.degraded || m.degradedStreak != 0 {
		t.Fatalf("recovery should reset the episode")
	}
	m.checkDegraded(150, true, true)
	m.checkDegraded(150, true, true) // a new episode fires again
	if fired != 2 {
		t.Fatalf("a new degraded episode should fire again, fired=%d", fired)
	}
	// A full outage (no reachable anchor) is owned by the up/down machine: reset.
	m.checkDegraded(9999, true, false)
	if m.degraded || m.degradedStreak != 0 {
		t.Fatalf("offline should reset degradation state")
	}

	// Threshold 0 (and a nil fn) mean detection is off: never fires.
	off := &Monitor{DegradedPingFn: func() float64 { return 0 }, OnDegraded: func() { t.Fatal("must not fire when threshold is 0") }}
	off.checkDegraded(9999, true, true)
	nilfn := &Monitor{OnDegraded: func() { t.Fatal("must not fire with a nil DegradedPingFn") }}
	nilfn.checkDegraded(9999, true, true)
}

// A single failed-quorum blip inside a continuous brownout (link stays debounced
// up, so online stays true, but the blip round produces no latency reading) must
// NOT re-arm the once-per-episode latch: OnDegraded fires once and stays quiet
// through the blip and the brownout that follows it.
func TestCheckDegradedBlipDoesNotRefire(t *testing.T) {
	fired := 0
	m := &Monitor{
		DegradedPingFn: func() float64 { return 100 },
		OnDegraded:     func() { fired++ },
	}
	m.checkDegraded(150, true, true)
	m.checkDegraded(150, true, true) // brownout confirmed - fire once
	if fired != 1 {
		t.Fatalf("brownout should fire once, fired=%d", fired)
	}
	// A quorum blip: no family answered (haveReading=false) but the link is still
	// debounced up (online=true). This must hold the episode, not reset it.
	m.checkDegraded(0, false, true)
	if !m.degraded {
		t.Fatal("a blip inside the brownout re-armed the latch")
	}
	// Brownout continues: still no re-fire this episode.
	m.checkDegraded(150, true, true)
	m.checkDegraded(150, true, true)
	if fired != 1 {
		t.Fatalf("brownout re-fired after a blip, fired=%d (want 1)", fired)
	}
	// Only a genuine latency recovery re-arms, so the next brownout fires again.
	m.checkDegraded(40, true, true)
	m.checkDegraded(150, true, true)
	m.checkDegraded(150, true, true)
	if fired != 2 {
		t.Fatalf("a new brownout after recovery should fire again, fired=%d", fired)
	}
}

// resetStreaks (run when a round is skipped while paused) must clear the
// degradation streak too - otherwise a host one round below the threshold before
// a pause would fire OnDegraded on the very first round after resume, violating
// the "a streak can't survive a pause" contract the up/down path enforces.
func TestResetStreaksClearsDegraded(t *testing.T) {
	m := &Monitor{
		DegradedPingFn: func() float64 { return 100 },
		OnDegraded:     func() { t.Fatal("OnDegraded fired across a pause reset") },
	}
	m.checkDegraded(150, true, true) // 1 of 2 over the threshold; armed but not fired
	if m.degradedStreak != 1 {
		t.Fatalf("setup: degradedStreak = %d, want 1", m.degradedStreak)
	}
	m.resetStreaks()
	if m.degradedStreak != 0 || m.degraded {
		t.Fatalf("degraded state survived reset: streak=%d degraded=%v", m.degradedStreak, m.degraded)
	}
	// A single over-threshold round after the reset must NOT fire (debounce needs
	// two again); the OnDegraded above t.Fatals if the streak had survived.
	m.checkDegraded(150, true, true)
}
