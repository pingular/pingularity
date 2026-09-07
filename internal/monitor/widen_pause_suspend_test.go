package monitor

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The widen (see transition) stretches a stepped-back recovery's stored pair to
// down + elapsed + frozenGap, so that once the read model subtracts the outage's
// pause rows the pair still holds duration_s. frozenGap was fed only by the
// wall-gap check - a suspend seen while probing - on the premise that an explicit
// pause advances both clocks and so needs no widen of its own. A host that
// suspends while monitoring is switched OFF is the case flushPause was written
// for, and it breaks that premise from inside the pause: the row is measured on
// the WALL clock, so it holds the freeze, while the resume fold (noteResume) is
// MONOTONIC and does not. The row was wider than the fold by the freeze, nothing
// credited the difference, and after a backward wall step the read model
// subtracted the whole row from a pair that never held it - a fifteen-minute
// outage booked as two on uptime, the outage list and the heatmap while its
// duration_s still said fifteen.
//
// The credit is an accounting of the whole outage, not of each slice. A pause
// episode is written as one row per checkpoint and folded by a single monotonic
// subtraction at the resume edge, so the difference is accumulated exactly and
// rounded once, where the widen consumes it - a slice-by-slice rounding turns
// each row's sub-second phase into a whole second and drifts a host that never
// slept clean out of its own recovery second.
//
// Every test here holds the invariant widen_suspend_test.go states: stored pair
// width minus its pause-row overlap == duration_s == what UptimeSince,
// ResolvedOutagesSince and DowntimeByDay book.

// bookFrozenPauseSlice books one switched-off slice the way Run's flushPause
// leaves it: a row of `span` WALL seconds, of which the monotonic clock saw only
// `mono` - the remainder is a lid-close inside the pause. Booked through the
// monitor's own seam, because no synthesized time.Time can carry a wall span its
// monotonic reading did not (Add moves both), which is the same reason
// bookUnobservedGap takes monoAdvance from its caller.
func bookFrozenPauseSlice(t *testing.T, m *Monitor, start time.Time, span, mono time.Duration) {
	t.Helper()
	if p := m.bookPauseSpan(context.Background(), start, span, mono); p != nil {
		t.Fatalf("pause slice not settled: pending=%v", p)
	}
}

// replaySwitchedOffEpisode books one whole switched-off episode the way Run
// leaves it on a host that never sleeps: flushPause checkpoints every `slice` of
// WALL time, sizing each row off the Unix stamps (whole seconds) and floored at
// the monotonic reading, and noteResume folds the episode's monotonic span in
// one subtraction. Returns the episode's total row seconds. Nothing here is
// asleep and nothing steps the clock - only the sub-second phase between a
// slice's two endpoints, which is what the rows and the fold round differently.
func replaySwitchedOffEpisode(t *testing.T, m *Monitor, start time.Time, slice time.Duration, n int) int64 {
	t.Helper()
	var rows int64
	at := start
	m.notePause(at)
	for i := 0; i < n; i++ {
		end := at.Add(slice)
		span := time.Duration(end.Unix()-at.Unix()) * time.Second
		mono := end.Sub(at)
		if mono > span {
			span = mono
		}
		bookFrozenPauseSlice(t, m, at, span, mono)
		rows += int64(span.Seconds())
		at = end
	}
	m.noteResume(at)
	return rows
}

// seedOutage opens the outage every test here closes: one earlier sample so the
// uptime window has a monitoring-since anchor, then DOWN at wall `down`.
func seedOutage(t *testing.T, m *Monitor, st *store.Store, down time.Time) {
	t.Helper()
	if err := st.InsertSamples(context.Background(), []store.Sample{{
		TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
	feed(m, false, down)
}

// recoverAt closes the outage at wall second `wall` after `elapsed` monotonic
// seconds. No synthesized time.Time can hold those two readings apart (Add moves
// both), so the halves are fabricated separately, exactly as production leaves
// them: m.since anchors the MONOTONIC measurement, the fed ts carries the wall.
func recoverAt(t *testing.T, m *Monitor, wall time.Time, elapsed time.Duration) {
	t.Helper()
	m.mu.Lock()
	m.since = wall.Add(-elapsed)
	m.mu.Unlock()
	feed(m, true, wall)
	if !m.online {
		t.Fatal("expected UP after recovery")
	}
}

// storedPair reads the one outage's stored pair and its duration_s.
func storedPair(t *testing.T, st *store.Store) (downTS, upTS, dur int64) {
	t.Helper()
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	return downTS, upTS, dur
}

// assertSurfacesBook checks the three read surfaces that share the interval
// model against the outage's own duration_s.
func assertSurfacesBook(t *testing.T, st *store.Store, since time.Time, dur int64) {
	t.Helper()
	ctx := context.Background()
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := time.Duration(dur) * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v of downtime, want %v", o.Down, want)
	}
	if n, downS, err := st.ResolvedOutagesSince(ctx, since.Unix()); err != nil || n != 1 || int64(downS) != dur {
		t.Errorf("ResolvedOutagesSince = (%d outages, %ds, err=%v), want (1, %ds, nil)", n, downS, err, dur)
	}
	if got := sumDowntimeByDay(t, st, since); int64(got) != dur {
		t.Errorf("DowntimeByDay booked %ds, want %ds", got, dur)
	}
}

// frozenGap reads the accumulator under the lock that guards it.
func frozenGap(m *Monitor) time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.frozenGap
}

// The lid closes while monitoring is switched off inside an outage, and NTP steps
// the wall back before the link recovers. Two minutes in the operator switches
// monitoring off for ten wall minutes; the host is asleep for five of them, so the
// monotonic clock saw only 300 of the row's 600 seconds. 1200 monotonic seconds
// elapse in all (900 observed down, 300 awake inside the pause); the recovery's
// wall second reads down+300.
func TestWidenAddsBackASuspendThatFellInsideAPause(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	bookFrozenPauseSlice(t, m, pauseStart, 600*time.Second, 300*time.Second)
	m.noteResume(pauseStart.Add(300 * time.Second)) // the fold is the monotonic half only
	if m.pausedGap != 300*time.Second {
		t.Fatalf("pausedGap = %v, want 300s: noteResume folds the monotonic half of the episode", m.pausedGap)
	}
	recoverAt(t, m, down.Add(300*time.Second), 1200*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900 (1200s elapsed minus the 300s fold)", dur)
	}
	if got := (upTS - downTS) - 600; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide): the pair was widened to the elapsed alone, and the read model "+
			"subtracts the freeze the row holds from seconds the pair does not", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// The loss is min(backward step, freeze), so no dramatic clock excursion is
// needed: the same outage with the wall stepped back a hundred seconds - a
// resume-from-suspend re-sync against a fast RTC - lost exactly a hundred. 1500
// wall seconds really passed (1200 monotonic plus the 300s freeze), and the
// recovery reads down+1400.
func TestAFreezeInsideAPauseSurvivesASmallBackwardStep(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	bookFrozenPauseSlice(t, m, pauseStart, 600*time.Second, 300*time.Second)
	m.noteResume(pauseStart.Add(300 * time.Second))
	recoverAt(t, m, down.Add(1400*time.Second), 1200*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900", dur)
	}
	if got := (upTS - downTS) - 600; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"after a hundred-second backward step (pair %ds wide)", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// A freeze long enough for the wall-gap check to notice (past one wait plus
// suspendGapSlack) is seen twice: by bookUnobservedGap, which leaves the row to
// the open pause, and by flushPause, which writes it. It must be credited to the
// widen exactly once, at the row. The switched-off stretch is 2400 wall seconds
// of which the clock saw 600; the loop woke mid-pause after an 1800s freeze.
func TestAFreezeSeenByTheWallGapCheckInsideAPauseIsCreditedOnce(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	if booked, p := m.bookUnobservedGap(ctx, pauseStart.Add(300*time.Second),
		pauseStart.Add(2100*time.Second), true, m.interval(), 0); !booked || p != nil {
		t.Fatalf("in-pause freeze: booked=%v pending=%v, want booked with no row of its own", booked, p)
	}
	bookFrozenPauseSlice(t, m, pauseStart, 2400*time.Second, 600*time.Second)
	m.noteResume(pauseStart.Add(600 * time.Second))
	if got := frozenGap(m); got != 1800*time.Second {
		t.Errorf("frozenGap = %v, want 1800s: the freeze the row holds, credited once", got)
	}
	recoverAt(t, m, down.Add(300*time.Second), 1200*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 600 {
		t.Fatalf("outage duration = %d, want 600 (1200s elapsed minus the 600s fold)", dur)
	}
	if got := (upTS - downTS) - 2400; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide)", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// The whole switched-off stretch asleep, which is what a closed lid does: the
// pause opens two minutes into the outage, the host is away for 1200 wall
// seconds the monotonic clock saw none of, thirteen more minutes are observed
// down, and the wall steps back half an hour so the recovery reads down+300
// with 900 monotonic seconds elapsed. The row is then wider than the whole
// unwidened pair.
func TestAFullyFrozenPauseKeepsTheOutageItsLength(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	bookFrozenPauseSlice(t, m, pauseStart, 1200*time.Second, 0)
	m.noteResume(pauseStart) // the monotonic clock stood still for the episode
	if m.pausedGap != 0 {
		t.Fatalf("pausedGap = %v, want 0: the fold is monotonic and the clock did not move", m.pausedGap)
	}
	recoverAt(t, m, down.Add(300*time.Second), 900*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900 (900s elapsed, nothing folded)", dur)
	}
	if got := (upTS - downTS) - 1200; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide)", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// Both feeders of frozenGap inside one outage, in either order: a suspend the
// wall-gap check booked while probing (1200 wall seconds, none monotonic) and a
// switched-off slice that hid a freeze (600 wall, 300 monotonic). The widen must
// add back the mono-absent seconds of both rows, whichever came first.
func TestBothKindsOfFrozenRowWidenThePairInEitherOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pauseFirst bool
	}{
		{"suspend while probing, then a pause that hid a freeze", false},
		{"pause that hid a freeze, then a suspend while probing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, st := newTestMonitor(t, 1, 1)
			ctx := context.Background()
			down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
			seedOutage(t, m, st, down)

			pausedFreeze := func(at time.Time) {
				m.notePause(at)
				bookFrozenPauseSlice(t, m, at, 600*time.Second, 300*time.Second)
				m.noteResume(at.Add(300 * time.Second))
			}
			suspend := func(at time.Time) {
				if booked, p := m.bookUnobservedGap(ctx, at, at.Add(1200*time.Second), false, m.interval(), 0); !booked || p != nil {
					t.Fatalf("suspend gap not booked cleanly: booked=%v pending=%v", booked, p)
				}
			}
			if tc.pauseFirst {
				pausedFreeze(down.Add(120 * time.Second)) // row [down+120, down+720)
				suspend(down.Add(900 * time.Second))      // row [down+900, down+2100)
			} else {
				suspend(down.Add(120 * time.Second))       // row [down+120, down+1320)
				pausedFreeze(down.Add(1500 * time.Second)) // row [down+1500, down+2100)
			}
			// 1200 monotonic seconds elapsed (900 observed, 300 awake inside the
			// pause); the wall stepped back so the recovery reads down+300.
			recoverAt(t, m, down.Add(300*time.Second), 1200*time.Second)

			downTS, upTS, dur := storedPair(t, st)
			if dur != 900 {
				t.Fatalf("outage duration = %d, want 900", dur)
			}
			if got := (upTS - downTS) - 1200 - 600; got != dur {
				t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
					"(pair %ds wide)", got, dur, upTS-downTS)
			}
			assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
		})
	}
}

// The credit is the row's seconds minus the monotonic time they cover, exactly
// and with its sign, and only while an outage is open. Nothing is rounded or
// clamped here: the slice is a fraction of an episode, and only the episode's
// total is a whole number of seconds anyone can act on.
func TestAPauseSliceCreditsItsExactWallMinusMonotonicDifference(t *testing.T) {
	for _, tc := range []struct {
		name       string
		online     bool
		span, mono time.Duration
		want       time.Duration
	}{
		// Both clocks ran through it: the fold already covers the whole row.
		{"awake pause inside an outage", false, 600 * time.Second, 600 * time.Second, 0},
		// The link was up, so there is no outage whose pair needs widening.
		{"freeze inside a pause while the link is up", true, 600 * time.Second, 300 * time.Second, 0},
		// flushPause's monotonic floor sized the row (a backward step mid-pause),
		// and the row is whole seconds, so it holds a fraction of a second LESS
		// than the clock saw. Carried NEGATIVE: it has to cancel the fractions
		// other slices of the same episode hold in surplus, or a long pause drifts
		// a second at a time away from its own rows.
		{"slice sized by the monotonic floor", false, 1000*time.Second + 400*time.Millisecond, 1000*time.Second + 400*time.Millisecond, -400 * time.Millisecond},
		// The loop was awake for half a second of a slice whose row claims twenty
		// minutes: all but that half second is a lid-close.
		{"awake for a fraction of a second", false, 1200 * time.Second, 500 * time.Millisecond, 1200*time.Second - 500*time.Millisecond},
		{"a lid-close inside the pause", false, 600 * time.Second, 300 * time.Second, 300 * time.Second},
		// flushPause floors the span at the monotonic reading, so this cannot
		// arrive from the loop. Should it ever, the answer is still the signed
		// difference: a fold larger than the rows is a pair that needs LESS than
		// the elapsed, not more.
		{"monotonic reading past the wall span", false, 600 * time.Second, 900 * time.Second, -300 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newTestMonitor(t, 1, 1)
			down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
			if !tc.online {
				feed(m, false, down)
			}
			at := down.Add(120 * time.Second)
			m.notePause(at)
			bookFrozenPauseSlice(t, m, at, tc.span, tc.mono)
			if got := frozenGap(m); got != tc.want {
				t.Errorf("frozenGap = %v after a %v slice of which the monotonic clock saw %v (online=%v), want %v",
					got, tc.span, tc.mono, tc.online, tc.want)
			}
		})
	}
}

// A REFUSED slice never gets a row, and the pause path reconciles that by
// moving the resume fold's anchor past the slice's WALL seconds
// (dropRefusedPauseDeduction) - the rowless stretch, freeze included, then
// counts as observed downtime in duration_s. So its credit stays: the pair has
// to hold those seconds too, and taking the credit back, as revertGapDeduction
// does for a refused suspend row, would leave the pair short by the freeze.
func TestARefusedPauseSliceKeepsItsWidenCredit(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	// The first slice lands: 600 wall seconds, 300 of them monotonic.
	bookFrozenPauseSlice(t, m, pauseStart, 600*time.Second, 300*time.Second)
	// The second, the same shape, is refused by the store. Restored on the way out
	// as well as here, the way the other seam swaps in this package are: a t.Fatal
	// while the stub is installed would otherwise refuse every pause row the rest
	// of the package writes.
	old := insertPause
	t.Cleanup(func() { insertPause = old })
	insertPause = func(*store.Store, context.Context, time.Time, int64) (bool, error) {
		return false, nil
	}
	bookFrozenPauseSlice(t, m, pauseStart.Add(600*time.Second), 600*time.Second, 300*time.Second)
	insertPause = old
	if got := frozenGap(m); got != 600*time.Second {
		t.Errorf("frozenGap = %v, want 600s: the refused slice's freeze is in duration_s, so it stays in the widen", got)
	}
	if want := pauseStart.Add(600 * time.Second); !m.downPausedAt.Equal(want) {
		t.Fatalf("downPausedAt = %v, want %v: the refused slice's wall seconds leave the fold", m.downPausedAt, want)
	}
	// The episode's monotonic span is 600s and the refusal took 600 wall seconds
	// back out of the fold, so nothing is folded: the resume reading sits on the
	// advanced anchor.
	m.noteResume(pauseStart.Add(600 * time.Second))
	if m.pausedGap != 0 {
		t.Fatalf("pausedGap = %v, want 0", m.pausedGap)
	}
	// 1500 monotonic seconds elapsed (900 observed, 600 awake inside the pause);
	// the wall stepped back so the recovery reads down+300.
	recoverAt(t, m, down.Add(300*time.Second), 1500*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 1500 {
		t.Fatalf("outage duration = %d, want 1500 (the refused slice counts as observed)", dur)
	}
	if got := (upTS - downTS) - 600; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide)", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// Through the loop itself: Run hands each slice to the credit with the slice's
// own two readings. This harness's wall clock cannot freeze the monotonic half
// (see wallClock), so the only pause it can model is one on a host that stayed
// awake - which must write its wall row and credit nothing to the widen.
func TestRunCreditsNothingForAnAwakePauseInsideAnOutage(t *testing.T) {
	// Anchored to the real clock: the resume edge folds time.Now() against the
	// pause edge's reading, and a fabricated date would fold the distance to it.
	start := time.Now().Round(0)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is in progress, so the pause opens inside it
	var mu sync.Mutex
	enabled := false
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return enabled }
	poke, stop := startLoop(t, m)
	// A reader on the accumulator while the loop books the slice: the credit is
	// written under mu, like transition's reset of it, and -race is the check.
	done := make(chan struct{})
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-done:
				return
			default:
				frozenGap(m)
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	tick(t, clk, poke) // the pause opens at `start`

	clk.step(20 * time.Minute) // twenty switched-off minutes, both clocks running
	tick(t, clk, poke)         // the checkpoint flush books the slice
	mu.Lock()
	enabled = true
	mu.Unlock()
	tick(t, clk, poke) // the resume edge
	stop()
	close(done)
	readers.Wait()

	var total int64
	for _, r := range snap() {
		total += r.dur
	}
	if total != 1200 {
		t.Errorf("recorded %ds of paused time, want 1200: the row is still the wall span", total)
	}
	if got := frozenGap(m); got != 0 {
		t.Errorf("frozenGap = %v after an awake pause, want 0: the slice's wall and monotonic "+
			"spans agree, so there is nothing the fold does not already cover", got)
	}
}

// Through the loop again, with the slice's two readings held apart by the only
// distance this harness can put between them. The row is whole seconds off the
// Unix stamps while the monotonic span is the exact distance, so a slice that
// opens half a second into one second and closes on another is a second wider
// than the clock saw. That second is a lid-close in miniature - the same
// arithmetic, in the only size a test can build - and it has to reach the widen,
// which it can only do if the flush hands each slice its OWN monotonic reading
// rather than the wall span it just measured.
func TestRunHandsEachPauseSliceItsMonotonicReading(t *testing.T) {
	// Half a second past a whole second, and anchored to the real clock for the
	// reason the awake-pause test above gives.
	start := time.Now().Round(0).Truncate(time.Second).Add(500 * time.Millisecond)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is in progress, so the pause opens inside it
	var mu sync.Mutex
	enabled := false
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return enabled }
	poke, stop := startLoop(t, m)
	tick(t, clk, poke) // the pause opens at `start`

	// Closes on a whole second: 1200 seconds by the Unix stamps the row is made
	// of, 1199.5 by the clock.
	clk.step(1200*time.Second - 500*time.Millisecond)
	tick(t, clk, poke) // the checkpoint flush books the slice
	mu.Lock()
	enabled = true
	mu.Unlock()
	tick(t, clk, poke) // the resume edge
	stop()

	var total int64
	for _, r := range snap() {
		total += r.dur
	}
	if total != 1200 {
		t.Errorf("recorded %ds of paused time, want 1200: the row is the wall span", total)
	}
	if got := frozenGap(m); got != 500*time.Millisecond {
		t.Errorf("frozenGap = %v, want 500ms: the row claims half a second the monotonic clock "+
			"did not see, which it can only know if the flush handed it both readings", got)
	}
	// And what the widen will do with it: the row is a whole second wider than
	// the fold once transition floors that fold, so the pair owes that second.
	if got := ceilSeconds(frozenGap(m)); got != 1 {
		t.Errorf("the widen would add %ds, want 1s", got)
	}
}

// ceilSeconds is the whole of the widen's rounding, so it is asserted on its
// own: whole seconds up, for either sign, and no second invented for a value
// that is already whole.
func TestCeilSecondsRoundsUpForEitherSign(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want int64
	}{
		{0, 0},
		{time.Nanosecond, 1},
		{500 * time.Millisecond, 1},
		{time.Second, 1},
		{time.Second + time.Nanosecond, 2},
		{1799*time.Second + 999*time.Millisecond, 1800},
		{-time.Nanosecond, 0},
		{-999 * time.Millisecond, 0},
		{-time.Second, -1},
		{-1500 * time.Millisecond, -1},
	} {
		if got := ceilSeconds(tc.in); got != tc.want {
			t.Errorf("ceilSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A pause on a host that never slept, written as the many slices a long
// switched-off stretch really takes, must leave the recovery exactly where the
// clock put it. Each row is whole Unix seconds while the fold is the exact
// monotonic distance, so every slice carries a fraction; charged one whole
// second per slice they add up to roughly half a second each, and the widen
// then fires on a host with nothing to widen for - fifty minutes switched off
// moved the stored recovery nine seconds into the future and left the pair nine
// seconds wider than its own duration_s plus rows.
func TestManySwitchedOffSlicesOnAnAwakeHostDoNotMoveTheRecovery(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	// Every instant here sits 900ms into its second, and the checkpoint is
	// 300.9s, so each slice opens and closes on a different phase of the second -
	// the ordinary case, not a contrived one.
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC).Add(900 * time.Millisecond)
	seedOutage(t, m, st, down)

	start := down.Add(120 * time.Second)
	rows := replaySwitchedOffEpisode(t, m, start, 300*time.Second+900*time.Millisecond, 10)
	if got := frozenGap(m); got < -time.Second || got > time.Second {
		t.Errorf("frozenGap = %v after ten awake slices, want under a second either way: "+
			"the whole episode can owe only the second its rows and its fold round differently", got)
	}
	// Ten more observed minutes and the link comes back. The wall never stepped,
	// so the elapsed is simply the distance from the down to the recovery.
	up := start.Add(10 * (300*time.Second + 900*time.Millisecond)).Add(600 * time.Second)
	recoverAt(t, m, up, up.Sub(down))

	downTS, upTS, dur := storedPair(t, st)
	if upTS != up.Unix() {
		t.Errorf("stored recovery = %d, want %d (%+ds): nothing slept and nothing stepped, so "+
			"there was nothing for the widen to add", upTS, up.Unix(), upTS-up.Unix())
	}
	if got := (upTS - downTS) - rows; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide)", got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// The same drift, compounded until it changes an answer: an outage that spans an
// overnight schedule window is written as one row per checkpoint, and a credit
// charged per slice grows with the window. Past two minutes (the store's
// metricsFutureSkew) the widened recovery sits beyond the horizon
// completedOutagesSince reads, and the outage stops being resolved at all -
// UptimeSince still books its downtime while the outage list says none ended.
func TestALongSwitchedOffStretchKeepsItsOutageResolved(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	const slices = 200
	slice := 300*time.Second + 900*time.Millisecond
	// Anchored to the real clock, which is what the horizon is measured against:
	// a fabricated date would put the whole outage safely in the past and prove
	// nothing. Recovery lands at `now`; the window is under seventeen hours.
	now := time.Now().UTC().Truncate(time.Second).Add(900 * time.Millisecond)
	down := now.Add(-(120*time.Second + slices*slice + 210*time.Second))
	seedOutage(t, m, st, down)

	start := down.Add(120 * time.Second)
	replaySwitchedOffEpisode(t, m, start, slice, slices)
	up := start.Add(slices * slice).Add(210 * time.Second)
	recoverAt(t, m, up, up.Sub(down))

	if _, upTS, dur := storedPair(t, st); upTS > up.Unix() {
		t.Errorf("stored recovery is %ds past the true one (duration_s %ds): %d slices of "+
			"switched-off time each moved it", upTS-up.Unix(), dur, slices)
	}
	n, downS, err := st.ResolvedOutagesSince(context.Background(), down.Add(-time.Hour).Unix())
	if err != nil || n != 1 || downS != 330 {
		t.Errorf("ResolvedOutagesSince = (%d outages, %ds, err=%v), want (1, 330s, nil): "+
			"a recovery stamped past the future horizon is invisible to it", n, downS, err)
	}
}

// The rounding direction, where it decides an answer. Real readings are
// fractional, so a fold usually is too, and transition FLOORS it out of
// duration_s while the rows it has to line up with are whole seconds. The
// half-second the floor drops is still inside the outage's length, so the widen
// has to round its credit the other way: a pause row of 600 wall seconds, of
// which the monotonic clock saw 300.5, inside a 1200-second elapsed leaves
// duration_s at 900 - and a pair only 1499 wide has 899 of it left once the row
// comes off, one second of downtime nobody can find.
func TestAFractionalFoldStillLeavesThePairHoldingItsRows(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	seedOutage(t, m, st, down)

	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	bookFrozenPauseSlice(t, m, pauseStart, 600*time.Second, 300500*time.Millisecond)
	m.noteResume(pauseStart.Add(300500 * time.Millisecond))
	if m.pausedGap != 300500*time.Millisecond {
		t.Fatalf("pausedGap = %v, want 300.5s", m.pausedGap)
	}
	// The wall stepped back, so the recovery reads down+300 for 1200 monotonic
	// seconds elapsed.
	recoverAt(t, m, down.Add(300*time.Second), 1200*time.Second)

	downTS, upTS, dur := storedPair(t, st)
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900 (1200s elapsed minus the floored 300.5s fold)", dur)
	}
	if got := (upTS - downTS) - 600; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds "+
			"(pair %ds wide): the half second the fold's flooring dropped is still in the outage",
			got, dur, upTS-downTS)
	}
	assertSurfacesBook(t, st, down.Add(-time.Hour), dur)
}

// The claim transition now makes about the widened width - that it is exactly
// duration_s plus the outage's rows, whatever the sub-second phases - swept over
// every combination of them. Real readings land anywhere inside their second,
// and the three quantities that have to agree round differently: the row is
// whole Unix seconds, the fold is exact and floored out of duration_s, and the
// elapsed is floored too. A rounding that is right on the whole second and wrong
// a tenth past it is the kind that hides until an operator asks why a fifteen
// minute outage reads as fourteen fifty-nine.
func TestTheWidenedPairHoldsItsRowsAtEveryPhaseOfTheSecond(t *testing.T) {
	phases := []time.Duration{0, 100 * time.Millisecond, 500 * time.Millisecond, 900 * time.Millisecond}
	for _, ps := range phases {
		for _, pe := range phases {
			for _, el := range phases {
				m, st := newTestMonitor(t, 1, 1)
				down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
				seedOutage(t, m, st, down)

				// A switched-off stretch of about ten minutes inside the outage,
				// opening and closing at arbitrary phases of their seconds, with
				// both clocks running through it.
				pauseStart := down.Add(120*time.Second + ps)
				pauseEnd := down.Add(720*time.Second + pe)
				m.notePause(pauseStart)
				span := time.Duration(pauseEnd.Unix()-pauseStart.Unix()) * time.Second
				mono := pauseEnd.Sub(pauseStart)
				if mono > span {
					span = mono
				}
				bookFrozenPauseSlice(t, m, pauseStart, span, mono)
				m.noteResume(pauseEnd)
				// The wall stepped back, so the widen is what sizes the pair.
				recoverAt(t, m, down.Add(300*time.Second), 1200*time.Second+el)

				downTS, upTS, dur := storedPair(t, st)
				if got := (upTS - downTS) - int64(span.Seconds()); got != dur {
					t.Errorf("pause [+%v, +%v), elapsed +%v: stored pair width minus its "+
						"pause-row overlap = %ds for a duration_s of %ds (pair %ds wide, row %ds)",
						ps, pe, el, got, dur, upTS-downTS, int64(span.Seconds()))
				}
			}
		}
	}
}
