package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The gap accumulators (pausedGap, frozenGap) live for exactly one outage and are
// reset by transition(). A pendingGap booked into one outage but reconciled during
// a LATER one - held because its write failed, then refused; or evicted from a full
// retry queue - must not reverse out of the later outage's fresh accumulators. Each
// booking carries the outage generation it was made in, and the reversal fires only
// while that still matches the open incarnation. These tests pin both the fix (a
// cross-outage reconciliation leaves the later outage alone) and the regression it
// must not cause (a same-outage refusal still reverses).

// (i) REGRESSION: a refusal inside the outage that booked the gap must still take
// the deduction and the widen back - the token has to permit reversal while the
// booking's own outage is open, or every refused suspend would strand its half.
func TestSameOutageRefusalStillRevertsWithinTheOutage(t *testing.T) {
	m, _ := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	old := insertPause
	insertPause = func(*store.Store, context.Context, time.Time, int64) (bool, error) {
		return false, nil // the store refuses the span on its first offer
	}
	t.Cleanup(func() { insertPause = old })

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	feed(m, false, down)
	// 1200 wall seconds, 400 seen by the monotonic clock: deduct 400, widen
	// remainder 800 - both taken back by the refusal, because this outage is open.
	if booked, p := m.bookUnobservedGap(ctx, down.Add(600*time.Second), down.Add(1800*time.Second),
		false, m.interval(), 400*time.Second); !booked || p != nil {
		t.Fatalf("a same-outage refusal must settle, not retry: booked=%v pending=%v", booked, p)
	}
	m.mu.RLock()
	pausedGap, frozenGap := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if pausedGap != 0 || frozenGap != 0 {
		t.Errorf("same-outage refusal left (paused %v, frozen %v), want (0, 0): the token must permit "+
			"reversal while the booking's own outage is still open", pausedGap, frozenGap)
	}
}

// (ii) CROSS-OUTAGE refusal: a gap booked in outage A, held because its write
// failed, then refused during outage B, must not be subtracted from B's own
// accumulators - shorting B's widen and its recorded duration with a correction
// that was never B's.
func TestCrossOutageRefusalDoesNotCorruptTheLaterOutage(t *testing.T) {
	m, _ := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	// insertPause is scripted by call order: A's gap FAILS (held for retry), B's gap
	// is STORED, then A's held gap is REFUSED on retry.
	var calls int
	old := insertPause
	insertPause = func(*store.Store, context.Context, time.Time, int64) (bool, error) {
		calls++
		switch calls {
		case 1:
			return false, errors.New("store temporarily unavailable") // A: FAILED -> held
		case 2:
			return true, nil // B: STORED
		default:
			return false, nil // A on retry: REFUSED -> revertGapDeduction
		}
	}
	t.Cleanup(func() { insertPause = old })

	// Outage A. A suspend gap of 1200 wall seconds, 400 seen by the monotonic clock:
	// deduct 400, remainder 800. Its write FAILS, so the record is held.
	downA := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	feed(m, false, downA)
	booked, pA := m.bookUnobservedGap(ctx, downA.Add(600*time.Second), downA.Add(1800*time.Second),
		false, m.interval(), 400*time.Second)
	if !booked || pA == nil {
		t.Fatalf("A's gap should be booked and held (write failed): booked=%v pending=%v", booked, pA)
	}
	held := appendHeldGap(m, nil, pA)

	// A recovers, B opens: two transitions, so A's accumulators are reset/consumed
	// and the outage generation moves past pA's token.
	feed(m, true, downA.Add(1801*time.Second))
	downB := downA.Add(3600 * time.Second)
	feed(m, false, downB)

	// B's own suspend gap: 3000 wall seconds, 1000 seen -> deduct 1000, remainder
	// 2000. Its write is STORED, so B's accumulators legitimately hold these.
	if booked, pB := m.bookUnobservedGap(ctx, downB.Add(60*time.Second), downB.Add(3060*time.Second),
		false, m.interval(), 1000*time.Second); !booked || pB != nil {
		t.Fatalf("B's gap should be booked and stored: booked=%v pending=%v", booked, pB)
	}
	m.mu.RLock()
	wantPaused, wantFrozen := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if wantPaused != 1000*time.Second || wantFrozen != 2000*time.Second {
		t.Fatalf("B's accumulators before the refusal = (paused %v, frozen %v), want (1000s, 2000s)",
			wantPaused, wantFrozen)
	}

	// A's held gap is now refused, during outage B.
	if still := retryHeldGaps(ctx, m, held); len(still) != 0 {
		t.Fatalf("A's refused gap must settle, not retry: %d still held", len(still))
	}
	m.mu.RLock()
	gotPaused, gotFrozen := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if gotPaused != wantPaused || gotFrozen != wantFrozen {
		t.Errorf("outage A's cross-outage refusal corrupted B: pausedGap %v->%v, frozenGap %v->%v; "+
			"a deduction from a closed outage was subtracted from B's fresh accumulators",
			wantPaused, gotPaused, wantFrozen, gotFrozen)
	}
}

// (iii) CROSS-OUTAGE eviction: a held gap from outage A dropped from a full retry
// queue during outage B reverses through the same path - it, too, must leave B's
// accumulators untouched.
func TestCrossOutageEvictionDoesNotCorruptTheLaterOutage(t *testing.T) {
	m, _ := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	// A's gap FAILS (held); B's gap is STORED. Eviction reverses directly, with no
	// further store attempt.
	var calls int
	old := insertPause
	insertPause = func(*store.Store, context.Context, time.Time, int64) (bool, error) {
		calls++
		if calls == 1 {
			return false, errors.New("store temporarily unavailable") // A: held
		}
		return true, nil // B: stored
	}
	t.Cleanup(func() { insertPause = old })

	// Outage A, a held suspend gap (1200s wall / 400s mono).
	downA := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	feed(m, false, downA)
	booked, pA := m.bookUnobservedGap(ctx, downA.Add(600*time.Second), downA.Add(1800*time.Second),
		false, m.interval(), 400*time.Second)
	if !booked || pA == nil {
		t.Fatalf("A's gap should be held: booked=%v pending=%v", booked, pA)
	}

	// A recovers, B opens.
	feed(m, true, downA.Add(1801*time.Second))
	downB := downA.Add(3600 * time.Second)
	feed(m, false, downB)

	// B's own gap (3000s wall / 1000s mono), stored.
	if booked, pB := m.bookUnobservedGap(ctx, downB.Add(60*time.Second), downB.Add(3060*time.Second),
		false, m.interval(), 1000*time.Second); !booked || pB != nil {
		t.Fatalf("B's gap should be stored: booked=%v pending=%v", booked, pB)
	}
	m.mu.RLock()
	wantPaused, wantFrozen := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if wantPaused != 1000*time.Second || wantFrozen != 2000*time.Second {
		t.Fatalf("B's accumulators = (paused %v, frozen %v), want (1000s, 2000s)", wantPaused, wantFrozen)
	}

	// Fill the retry queue with A's gap at the head and enough filler that the NEXT
	// append evicts the head. appendHeldGap drops the oldest (A's gap) and reverses
	// its deduction - which, cross-outage, must leave B alone.
	held := make([]*pendingGap, maxHeldGaps)
	held[0] = pA
	for i := 1; i < maxHeldGaps; i++ {
		held[i] = &pendingGap{start: downB, pause: true} // fillers, no deduction
	}
	held = appendHeldGap(m, held, &pendingGap{start: downB, pause: true})
	if len(held) != maxHeldGaps {
		t.Fatalf("queue length after eviction = %d, want %d", len(held), maxHeldGaps)
	}
	m.mu.RLock()
	gotPaused, gotFrozen := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if gotPaused != wantPaused || gotFrozen != wantFrozen {
		t.Errorf("evicting outage A's held gap during outage B corrupted B: pausedGap %v->%v, "+
			"frozenGap %v->%v", wantPaused, gotPaused, wantFrozen, gotFrozen)
	}
}

// The pause path's counterpart, dropRefusedPauseDeduction, carries the same hazard.
// Its start-only guard caught the ordinary cross-outage case (a later episode's
// anchor is set after the earlier span, so the span sorts before it), but a backward
// wall step between the outages can land the new anchor EARLIER than the old span's
// start - and then only the generation token tells the two apart. A refused span
// from a closed outage must not advance the current episode's pause anchor, or its
// resume fold would wrongly exclude observed-down time that never belonged to it.
func TestCrossOutagePauseRefusalDoesNotMoveTheLaterEpisodeAnchor(t *testing.T) {
	m, _ := newTestMonitor(t, 1, 1)

	// Outage A, then a pause episode inside it. A pause-path span is measured and
	// held (its write failed), carrying A's outage generation.
	downA := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	feed(m, false, downA)
	pauseAnchorA := downA.Add(600 * time.Second)
	m.notePause(pauseAnchorA)
	pA := &pendingGap{start: pauseAnchorA, secs: 300, pause: true, gen: m.outageGen}
	m.noteResume(pauseAnchorA.Add(300 * time.Second)) // probing resumes, fold taken

	// A recovers, B opens. A backward NTP step across the two lands B's pause anchor
	// EARLIER than A's held span start - the one arrangement the start-only guard
	// reads as an in-episode span.
	feed(m, true, downA.Add(1200*time.Second))
	downB := downA.Add(1800 * time.Second)
	feed(m, false, downB)
	pauseAnchorB := pauseAnchorA.Add(-300 * time.Second) // stepped back, before pA.start
	m.notePause(pauseAnchorB)
	before := m.downPausedAt

	// A's held pause span is refused during outage B. Its rowless stretch belongs to
	// the closed outage A, so B's still-open pause anchor must not advance for it.
	m.dropRefusedPauseDeduction(pA)
	if !m.downPausedAt.Equal(before) {
		t.Errorf("outage A's refused pause span moved outage B's pause anchor from %v to %v; B's resume "+
			"fold would then wrongly exclude %v of observed-down time",
			before, m.downPausedAt, m.downPausedAt.Sub(before))
	}
}
