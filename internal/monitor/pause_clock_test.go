package monitor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// A pause row says "these wall seconds were not watched", and it is the uptime
// DENOMINATOR - pausedOverlap subtracts it from observed time on every surface.
// One row is therefore enough to blank the product's headline figure, and
// flushPause measures its span on the WALL clock.
//
// The wall clock is not trustworthy at boot. A board without an RTC starts near
// the epoch and stays there until NTP answers; if monitoring is off or outside
// its schedule window at that moment, a pause span is open across the correction,
// and the resulting row claims every second from 1970 to now.
//
// The probing path already knows this: unobservedGap refuses both endpoints below
// plausibleWallEpoch, and TestUnobservedGapIgnoresImplausibleBootClock pins it.
// flushPause - the same scenario with the master switch off instead of on - had
// no such check. Neither did Store.InsertPause: pauseRowSane bounds an IMPORTED
// pause to ten years and refuses one starting before the project existed, but it
// is wired only into the import path, so the monitor could write a row its own
// validator would have rejected.

// The row the monitor writes across an NTP correction must be plausible, or not
// written at all.
func TestBootClockCorrectionDoesNotMintADecadesLongPause(t *testing.T) {
	// Boot at 1970 with monitoring OFF, so a pause span is already open.
	boot := time.Unix(120, 0).UTC()
	clk := newWallClock(boot)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	// Master switch off for the whole test: probing() is false, so a pause span is
	// open across the correction.
	m.EnabledFn = func() bool { return false }
	poke, stop := startLoop(t, m)
	tick(t, clk, poke) // let the loop open the span at the boot instant

	// NTP answers: one forward step of fifty-six years.
	clk.step(time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC).Sub(boot))
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	for _, r := range snap() {
		span := time.Duration(r.dur) * time.Second
		if r.start.Unix() < plausibleWallEpoch {
			t.Errorf("wrote a pause starting %s - before the project existed - for %v; "+
				"a clock being corrected at boot is not unobserved time, and unobservedGap "+
				"already refuses exactly this on the probing path",
				r.start.UTC().Format(time.RFC3339), span)
		}
		if span > 10*365*24*time.Hour {
			t.Errorf("wrote a %v pause; one row this long is subtracted from every uptime "+
				"window, so observation coverage reads ~0 on the pill, /metrics, the digest "+
				"and every heatmap day", span)
		}
	}
}

// The ordinary case this path exists for must keep working: a real switched-off
// stretch, on a sane clock, still has to be recorded.
func TestAnOrdinaryPausedStretchIsStillRecorded(t *testing.T) {
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.EnabledFn = func() bool { return false }
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	clk.step(3 * time.Hour) // three hours switched off
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	var total time.Duration
	for _, r := range snap() {
		total += time.Duration(r.dur) * time.Second
	}
	if total < 3*time.Hour {
		t.Errorf("recorded %v of paused time over a three-hour switch-off; the guard must not "+
			"suppress real unobserved time", total)
	}
}

// A long but believable hibernate must survive. The in-tree comment on flushPause
// rejects a ceiling precisely because "a genuine hibernate can last weeks", so the
// guard has to key on the implausible EPOCH, not on length alone.
func TestALongHibernateIsStillRecorded(t *testing.T) {
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.EnabledFn = func() bool { return false }
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	clk.step(21 * 24 * time.Hour) // three weeks in a drawer
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	var total time.Duration
	for _, r := range snap() {
		total += time.Duration(r.dur) * time.Second
	}
	if total < 20*24*time.Hour {
		t.Errorf("recorded %v for a three-week hibernate; that is real unobserved time and "+
			"truncating it is the error this path was written to fix", total)
	}
}

// A pause row that fails to persist must not be forgotten.
//
// bookUnobservedGap adjusts the outage accounting, attempts one write, and reports
// the gap handled whatever the write did. Run then re-anchors lastWall
// unconditionally, so the interval can never be attempted again: the dark hours
// stay in the observed denominator and are credited as healthy for as long as the
// data is kept. The transition events already have a retry buffer for exactly this
// reason; the pause path had none.
func TestAFailedPauseWriteIsRetried(t *testing.T) {
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	var mu sync.Mutex
	var attempts int
	var stored []int64
	failing := true
	old := insertPause
	insertPause = func(_ *store.Store, _ context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if failing {
			return false, errors.New("disk full")
		}
		stored = append(stored, dur)
		return true, nil
	}
	t.Cleanup(func() { insertPause = old })

	m, _, _ := newLoopMonitor(t, time.Hour)
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	// Nine dark hours the write cannot record.
	clk.step(9 * time.Hour)
	tick(t, clk, poke)
	tick(t, clk, poke)

	mu.Lock()
	firstAttempts := attempts
	mu.Unlock()
	if firstAttempts == 0 {
		t.Fatal("the gap was never offered to the store at all")
	}

	// The store recovers. The interval must still be written.
	mu.Lock()
	failing = false
	mu.Unlock()
	clk.step(time.Minute)
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	mu.Lock()
	defer mu.Unlock()
	var total int64
	for _, d := range stored {
		total += d
	}
	if total < int64((9 * time.Hour).Seconds()) {
		t.Errorf("after the store recovered, %ds of pause was written for a nine-hour gap "+
			"(attempts=%d); the interval was dropped on the first failure and the anchor moved "+
			"past it, so those hours stay in the observed denominator and read as healthy",
			total, attempts)
	}
}

// The SAME invariant on the other path that writes pause rows: an EXPLICIT
// pause (master switch off, schedule window closed) flushes its span through
// flushPause, not bookUnobservedGap - and that path dropped a failed write with
// a Debug line and advanced its anchor past it. A monitor scheduled off
// overnight while the store cannot write (disk full, a long writer lock) lost
// the whole night: those hours stayed in the observed denominator and read as
// observed-and-up for as long as the data was kept.
func TestAFailedExplicitPauseWriteIsRetried(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	var mu sync.Mutex
	var attempts int
	var stored []int64
	failing := true
	old := insertPause
	insertPause = func(_ *store.Store, _ context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if failing {
			return false, errors.New("disk full")
		}
		stored = append(stored, dur)
		return true, nil
	}
	t.Cleanup(func() { insertPause = old })

	m, _, buf := newLoopMonitor(t, time.Hour)
	var emu sync.Mutex
	enabled := false // the master switch is off, so an explicit pause opens at once
	m.EnabledFn = func() bool { emu.Lock(); defer emu.Unlock(); return enabled }
	poke, stop := startLoop(t, m)
	waitFor(t, "the pause episode to open", func() bool {
		return stats.Lifetime().Counters["monitor.pauses"] == 1
	})

	// Nine switched-off hours the checkpoint write cannot record.
	clk.step(9 * time.Hour)
	tick(t, clk, poke)
	tick(t, clk, poke)

	mu.Lock()
	firstAttempts := attempts
	mu.Unlock()
	if firstAttempts == 0 {
		t.Fatal("the span was never offered to the store at all")
	}

	// The store recovers, and monitoring resumes a minute later.
	mu.Lock()
	failing = false
	mu.Unlock()
	clk.step(time.Minute)
	emu.Lock()
	enabled = true
	emu.Unlock()
	waitFor(t, "the loop to take the resume edge", func() bool {
		poke()
		return buf.has("monitor recording resumed")
	})
	tick(t, clk, poke)
	stop()

	mu.Lock()
	defer mu.Unlock()
	var total int64
	for _, d := range stored {
		total += d
	}
	if total < int64((9 * time.Hour).Seconds()) {
		t.Errorf("after the store recovered, %ds of pause was written for a nine-hour switch-off "+
			"(attempts=%d); the slice was dropped on the first failure and the anchor moved past it, "+
			"so those hours stay in the observed denominator and read as healthy", total, attempts)
	}
}

// And the third writer of pause rows, the startup gap: one failed write there
// silently dropped a potentially months-long process-down stretch, with nothing
// to retry it - the immediate first round then advances LastObservedTS past the
// gap, so no future restart re-derives it.
func TestAFailedStartupGapWriteIsRetried(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	clk := newWallClock(now)
	swapNow(t, clk)

	var mu sync.Mutex
	var stored []int64
	failing := true
	old := insertPause
	insertPause = func(_ *store.Store, _ context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return false, errors.New("disk full")
		}
		stored = append(stored, dur)
		return true, nil
	}
	t.Cleanup(func() { insertPause = old })

	m, st, _ := newLoopMonitor(t, time.Hour)
	// The last thing observed before the process died, nine hours ago.
	if err := st.InsertSamples(context.Background(), []store.Sample{{
		TS: now.Add(-9 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	poke, stop := startLoop(t, m)
	tick(t, clk, poke) // the startup write has been attempted (and failed) by now

	// The store recovers; the loop must still get the gap written.
	mu.Lock()
	failing = false
	mu.Unlock()
	clk.step(time.Minute)
	tick(t, clk, poke)
	stop()

	mu.Lock()
	defer mu.Unlock()
	var total int64
	for _, d := range stored {
		total += d
	}
	if total < int64((9 * time.Hour).Seconds()) {
		t.Errorf("after the store recovered, %ds of pause was written for a nine-hour process-down "+
			"gap; the one-shot startup write dropped it, so the whole stretch reads as observed", total)
	}
}

// A pause-path span the store REFUSES must not be silently voided. The mono
// floor in flushPause can compose with PauseSpanSane's end-by-about-now bound
// into a deterministic refusal (a backward wall step larger than the skew
// mid-pause), and the flush discarded the `stored` bool: no log, no counter,
// and the resume edge still deducted the episode from the open outage - a
// deduction paired with a row that does not exist, exactly what the gap path's
// refusal handling was built to prevent.
func TestARefusedPauseSpanIsNotSilentlyVoided(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	// Count offers around the REAL InsertPause, so the refusal is the store's own.
	var mu sync.Mutex
	var offered, stored int
	old := insertPause
	insertPause = func(st *store.Store, ctx context.Context, s time.Time, dur int64) (bool, error) {
		ok, err := old(st, ctx, s, dur)
		mu.Lock()
		offered++
		if ok {
			stored++
		}
		mu.Unlock()
		return ok, err
	}
	t.Cleanup(func() { insertPause = old })

	m, _, buf := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is open, so the resume edge would deduct the episode
	var emu sync.Mutex
	enabled := false
	m.EnabledFn = func() bool { emu.Lock(); defer emu.Unlock(); return enabled }
	poke, stop := startLoop(t, m)
	waitFor(t, "the pause episode to open", func() bool {
		return stats.Lifetime().Counters["monitor.pauses"] == 1
	})

	clk.step(11 * 365 * 24 * time.Hour) // past the ten-year ceiling: refused, deterministically
	tick(t, clk, poke)
	tick(t, clk, poke)

	// Monitoring resumes; the resume edge folds the episode into the outage.
	clk.step(time.Minute)
	emu.Lock()
	enabled = true
	emu.Unlock()
	waitFor(t, "the loop to take the resume edge", func() bool {
		poke()
		return buf.has("monitor recording resumed")
	})
	stop()

	mu.Lock()
	defer mu.Unlock()
	if offered == 0 {
		t.Fatal("no pause was offered to the store; this test drove the wrong path")
	}
	if stored != 0 {
		t.Fatalf("fixture wrong: the store accepted %d of %d spans, so nothing was refused", stored, offered)
	}
	if !buf.has("refused") {
		t.Error("a refused pause span left no log line; the slice is voided with nothing for an " +
			"operator asking \"why is coverage high?\" to find")
	}
	if got := stats.Lifetime().Counters["monitor.unobserved_gap_refused"]; got == 0 {
		t.Error("monitor.unobserved_gap_refused = 0: the refusal was not counted")
	}
	if m.pausedGap != 0 {
		t.Errorf("the outage numerator was corrected by %v for pause spans the store REFUSED to "+
			"store - the row and the deduction must land together or not at all", m.pausedGap)
	}
}

// A pause the store REFUSES must not be treated as a durable write.
//
// Two changes made hours apart collide here. InsertPause gained validation and
// signals refusal by returning nil - the same value as success, matching its
// existing "a non-positive duration is silently dropped" shape. bookUnobservedGap
// gained a retry that decides durability from `err != nil`. Together: a refused
// span looks written, so the anchor advances past it, the outage numerator is
// corrected for a row that does not exist, and nothing is ever retried.
//
// Neither change is wrong alone, which is exactly why this needed a test at the
// seam rather than on either side of it.
func TestARefusedPauseIsNotTreatedAsWritten(t *testing.T) {
	// A clock far enough ahead that the span the monitor computes exceeds
	// maxPauseDuration, so PauseSpanSane refuses it and InsertPause returns nil.
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	var mu sync.Mutex
	var offered, stored int
	old := insertPause
	insertPause = func(st *store.Store, ctx context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		offered++
		mu.Unlock()
		ok, err := old(st, ctx, s, dur) // the real thing, so refusal is the real refusal
		if ok {
			mu.Lock()
			stored++
			mu.Unlock()
		}
		return ok, err
	}
	t.Cleanup(func() { insertPause = old })

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is open, so the numerator correction is in play
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	clk.step(11 * 365 * 24 * time.Hour) // past the ten-year ceiling
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	mu.Lock()
	defer mu.Unlock()
	if offered == 0 {
		t.Fatal("no pause was offered to the store; this test drove the wrong path")
	}
	if stored != 0 {
		t.Fatalf("fixture wrong: the store accepted %d of %d spans, so nothing was refused", stored, offered)
	}
	if m.pausedGap != 0 {
		t.Errorf("the outage numerator was corrected by %v for a pause the store REFUSED to "+
			"store - the correction and the row it is paired with must land together or not at "+
			"all, or uptime is silently rewritten against a record that does not exist", m.pausedGap)
	}
}

// The retry holds only the old wall anchor and recomputes the span later, which
// breaks two ways.
//
// (a) The deduction that removes a sleep from an outage's length is applied only
// once the write lands. If the link recovers first, the outage has already been
// closed and recorded, so the deduction has nothing left to correct - a nine-hour
// sleep during an outage is filed as nine hours of downtime.
func TestASleepIsDeductedEvenIfTheLinkRecoversBeforeTheWriteLands(t *testing.T) {
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	var mu sync.Mutex
	failing := true
	old := insertPause
	insertPause = func(st *store.Store, ctx context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return false, errors.New("disk full")
		}
		return old(st, ctx, s, dur)
	}
	t.Cleanup(func() { insertPause = old })

	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is running
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	clk.step(9 * time.Hour) // asleep, and the write fails
	tick(t, clk, poke)
	tick(t, clk, poke)

	// The link comes back before the store recovers.
	m.mu.Lock()
	m.online = true
	m.mu.Unlock()
	mu.Lock()
	failing = false
	mu.Unlock()
	clk.step(time.Minute)
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	if m.pausedGap == 0 {
		t.Error("the nine-hour sleep was never deducted from the outage it happened inside: the " +
			"deduction waits for the write, the write was retried past the recovery, and by then " +
			"the outage had already been closed with the sleep counted as downtime")
	}
}

// (b) The retry recomputes the span from the original anchor to the CURRENT time,
// so minutes that were observed after waking are swallowed into the pause. The
// recorded unobserved stretch has to be the one that was measured, not one that
// grows every time the write is attempted.
func TestARetriedPauseDoesNotSwallowTimeObservedAfterWaking(t *testing.T) {
	start := time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)

	var mu sync.Mutex
	var written []int64
	failing := true
	old := insertPause
	insertPause = func(_ *store.Store, _ context.Context, s time.Time, dur int64) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return false, errors.New("disk full")
		}
		written = append(written, dur)
		return true, nil
	}
	t.Cleanup(func() { insertPause = old })

	m, _, _ := newLoopMonitor(t, time.Hour)
	poke, stop := startLoop(t, m)
	tick(t, clk, poke)

	clk.step(9 * time.Hour) // the sleep; the write fails
	tick(t, clk, poke)

	// Awake and observed for a further two hours before the store recovers.
	clk.step(2 * time.Hour)
	tick(t, clk, poke)
	mu.Lock()
	failing = false
	mu.Unlock()
	clk.step(time.Minute)
	tick(t, clk, poke)
	tick(t, clk, poke)
	stop()

	mu.Lock()
	defer mu.Unlock()
	for _, d := range written {
		if d > int64((9*time.Hour + 30*time.Minute).Seconds()) {
			t.Errorf("recorded a %v unobserved stretch for a nine-hour sleep; the retry recomputed "+
				"the span up to the time it finally succeeded, so hours the monitor really was "+
				"watching were filed as unwatched", time.Duration(d)*time.Second)
		}
	}
	if len(written) == 0 {
		t.Error("nothing was ever written, so this test measured nothing")
	}
}

// Records for unwritten gaps are held for retry, so the queue needs a ceiling: a
// store that never accepts a write must not turn every future suspend into
// permanently retained memory. Dropping one has to take its deduction back, for
// the same reason a refusal does - the correction and the row are one fact.
func TestHeldGapsAreBoundedAndDroppingOneTakesItsDeductionBack(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false
	held := []*pendingGap{}
	for i := 0; i < maxHeldGaps+8; i++ {
		p := &pendingGap{start: time.Unix(int64(1800000000+i*3600), 0), secs: 60, deduct: time.Minute}
		m.pausedGap += p.deduct
		held = appendHeldGap(m, held, p)
		if len(held) > maxHeldGaps {
			t.Fatalf("queue grew to %d records, past the %d ceiling: an unwritable store would "+
				"retain one record per suspend forever", len(held), maxHeldGaps)
		}
	}
	// 72 gaps queued, 64 retained: the 8 dropped ones must have given their
	// deductions back, leaving 64 minutes standing rather than 72.
	if want := time.Duration(maxHeldGaps) * time.Minute; m.pausedGap != want {
		t.Errorf("pausedGap = %v, want %v: dropped records left their deductions behind, so uptime "+
			"is corrected for unobserved stretches that were never recorded", m.pausedGap, want)
	}
}
