package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// pausedIn is the recorded unobserved seconds overlapping [from, to] - exactly
// the quantity UptimeSince removes from its denominator, DowntimeByDay's prorate
// subtracts per day, and orphanGapDowntime removes from a restart gap.
func pausedIn(t *testing.T, st *Store, from, to time.Time) int64 {
	t.Helper()
	p, err := st.pausedOverlap(context.Background(), from.Unix(), to.Unix())
	if err != nil {
		t.Fatalf("pausedOverlap: %v", err)
	}
	return p
}

// Prune must be an accounting no-op INSIDE the retained window. A pause span has
// length, so one can begin before the retention cutoff and run well into the
// window that is being kept; deleting it whole (`ts < cutoff`) erases the record
// that those in-window seconds were unobserved, and they silently become
// observed-and-up. Nothing else changes - no data arrives, no data the operator
// can see leaves - yet coverage jumps toward 1.0 and the uptime% moves.
//
// The dangerous row is the startup-gap span: Monitor.Run books an entire
// process-down stretch as ONE row, so a single delete can hand back months.
// (Live pauses checkpoint every ~5 minutes, so they lose at most that - covered
// by the sibling test below.) And this is not an occasional alignment: runPruner
// ticks hourly and the cutoff sweeps forward continuously, so every pause row met
// that DELETE at the first tick after the cutoff crossed its ts - i.e. while
// almost all of its span was still inside the retained window.
//
// The invariant asserted here is pausedOverlap(cutoff, now) surviving the prune,
// plus the readings of all three consumers; the row bookkeeping is checked so the
// fix can't be a prune that simply stopped deleting.
func TestPruneKeepsPauseSpanStraddlingTheCutoff(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	const day = 24 * 3600
	st := open(t)

	sampleAt(t, st, now, 100*day, "cf", "ipv4", true) // monitoring anchor at the window start
	// The startup-gap row: the monitor was stopped from 150 to 30 days ago and
	// booked the whole gap as one span, so 70 of its days lie inside the window
	// that survives this prune.
	pauseAt(t, st, now, 150*day, 120*day)
	// A completed outage well inside the window: one day of OBSERVED downtime.
	eventAt(t, st, now, 20*day, "down", -1)
	eventAt(t, st, now, 19*day, "up", day)
	// Rows the prune must still remove, so "keep the straddler" can't degrade into
	// "keep everything": one span wholly past the cutoff, one stamped in the future
	// by a wrong boot clock.
	pauseAt(t, st, now, 300*day, day)
	pauseAt(t, st, now, -60*day, 3600)

	cutoff := now.Add(-100 * day * time.Second) // outage/pause retention horizon
	since := cutoff
	before := readAll(t, st, since)
	beforePaused := pausedIn(t, st, cutoff, now)
	if beforePaused != 70*day {
		t.Fatalf("fixture: %ds unobserved inside the window, want %ds (70 of the span's 120 days)",
			beforePaused, 70*day)
	}
	if math.Abs(before.coverage-0.3) > 1e-3 {
		t.Fatalf("fixture: coverage = %.6f, want ~0.3 (30 observed days of 100)", before.coverage)
	}

	// Sample retention is deliberately wider than the cutoff under test: this is
	// about the pause sweep, and losing the anchor sample would move the window too.
	if _, err := st.Prune(ctx, now.Add(-101*day*time.Second), now.Add(-101*day*time.Second), cutoff); err != nil {
		t.Fatalf("prune: %v", err)
	}
	st.invalidateReadCaches() // cold caches, as after a restart

	if got := pausedIn(t, st, cutoff, now); got != beforePaused {
		t.Errorf("unobserved seconds inside the retained window: %ds -> %ds across a Prune; "+
			"the straddling span was deleted whole and %ds of unobserved time became "+
			"observed-and-up", beforePaused, got, beforePaused-got)
	}
	after := readAll(t, st, since)
	if math.Abs(after.ratio-before.ratio) > 1e-4 {
		t.Errorf("uptime ratio moved across the prune: %.6f -> %.6f (no data the operator "+
			"can see was removed from this window)", before.ratio, after.ratio)
	}
	if math.Abs(after.coverage-before.coverage) > 1e-4 {
		t.Errorf("observation coverage moved across the prune: %.6f -> %.6f", before.coverage, after.coverage)
	}
	if after.hmDownS != before.hmDownS || after.hmOut != before.hmOut {
		t.Errorf("heatmap moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.hmDownS, before.hmOut, after.hmDownS, after.hmOut)
	}
	if after.dgDownS != before.dgDownS || after.dgOut != before.dgOut {
		t.Errorf("digest moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.dgDownS, before.dgOut, after.dgDownS, after.dgOut)
	}
	// Only the straddler survives: the wholly-expired span and the future-stamped
	// one are gone, so retention still bites.
	if n := pauseRowCount(t, st); n != 1 {
		t.Errorf("pause rows after prune = %d, want 1 (the straddling span only - the expired "+
			"and future-stamped rows must still be deleted)", n)
	}
}

// The live-pause shape: monitoring off writes contiguous spans of at most
// pauseCheckpoint (~5 min), so exactly one of them straddles the cutoff at any
// tick and the old sweep lost only that one. Small, but it is the common case -
// it recurs on every pruner tick for as long as the operator keeps pausing - and
// the same rule fixes it, so it is pinned here alongside the months-long span.
func TestPruneKeepsCheckpointPauseStraddlingTheCutoff(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	const day = 24 * 3600
	st := open(t)
	sampleAt(t, st, now, 60*day, "cf", "ipv4", true)

	// Five 5-minute checkpoint rows around the cutoff. The middle one begins 150s
	// before it and so contributes 150 unobserved seconds to the retained window.
	cutoffAgo := 30 * day
	for _, offset := range []int{750, 450, 150, -150, -450} {
		pauseAt(t, st, now, cutoffAgo+offset, 300)
	}
	cutoff := now.Add(-time.Duration(cutoffAgo) * time.Second)
	beforePaused := pausedIn(t, st, cutoff, now)
	if beforePaused != 750 {
		t.Fatalf("fixture: %ds unobserved inside the window, want 750 (150 from the straddler, "+
			"600 from the two spans after the cutoff)", beforePaused)
	}

	if _, err := st.Prune(ctx, now.Add(-61*day*time.Second), now.Add(-61*day*time.Second), cutoff); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := pausedIn(t, st, cutoff, now); got != beforePaused {
		t.Errorf("unobserved seconds inside the retained window: %ds -> %ds across a Prune "+
			"(the checkpoint span straddling the cutoff was deleted whole)", beforePaused, got)
	}
	if n := pauseRowCount(t, st); n != 3 {
		t.Errorf("pause rows after prune = %d, want 3 (the straddler and the two later spans; "+
			"the two that ended before the cutoff must go)", n)
	}
}
