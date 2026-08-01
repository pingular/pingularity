package monitor

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// wallClock is a settable stand-in for the wall clock. Run reads it from the
// monitor goroutine while the test steps it, so every access is mutex-guarded - the
// -race run is part of the point: a seam must not become a data race.
//
// It cannot also model the MONOTONIC clock, and nothing can: Go couples a
// time.Time's two readings - Add moves both, Round(0) drops the monotonic one - so
// no value can exist whose wall has advanced nine hours while its monotonic has
// not. That is precisely the state a suspend leaves a process in, so a freeze is
// modelled here by the half a test CAN hold: the wall clock jumps while the loop's
// real monotonic scheduling (lastRound, time.Until, the timer) does not move with
// it, which is exactly how the loop experiences a suspend in production.
type wallClock struct {
	mu sync.Mutex
	t  time.Time
	n  int // nowFn() calls, so a test can tell the loop has looked at a change
}

func newWallClock(t time.Time) *wallClock { return &wallClock{t: t} }

func (c *wallClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.t
}

// step moves the wall clock by d (negative for a backward NTP step).
func (c *wallClock) step(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *wallClock) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// swapNow stubs the package-level nowFn seam for one test. Mirrors
// swapResolveTime/swapInsertEvent. Call it BEFORE starting the loop: t.Cleanup runs
// LIFO, so the loop's stop must be registered later and therefore run first -
// otherwise the restore would race a monitor goroutine that is still reading it.
func swapNow(t *testing.T, c *wallClock) {
	t.Helper()
	old := nowFn
	nowFn = c.now
	t.Cleanup(func() { nowFn = old })
}

// pauseRec is one captured InsertPause call: the stretch the monitor claims was
// unobserved, as [start, start+dur) WALL seconds.
type pauseRec struct {
	start time.Time
	dur   int64
}

// capturePauses stubs the insertPause seam and returns a snapshot function, so a
// test can assert the exact spans the loop books without going through the
// database. Like swapNow, call it before starting the loop.
func capturePauses(t *testing.T) func() []pauseRec {
	t.Helper()
	var mu sync.Mutex
	var got []pauseRec
	old := insertPause
	insertPause = func(_ *store.Store, _ context.Context, start time.Time, dur int64) (bool, error) {
		mu.Lock()
		got = append(got, pauseRec{start: start, dur: dur})
		mu.Unlock()
		return true, nil // captured, i.e. stored
	}
	t.Cleanup(func() { insertPause = old })
	return func() []pauseRec {
		mu.Lock()
		defer mu.Unlock()
		return append([]pauseRec(nil), got...)
	}
}

// syncBuf is a mutex-guarded log sink. The loop logs from the monitor goroutine
// while the test reads, so a plain bytes.Buffer would be a data race; waiting on a
// log line is how these tests observe an edge the loop takes internally (the resume
// edge has no other visible effect until the next round is due).
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) has(sub string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Contains(s.b.String(), sub)
}

// newLoopMonitor builds the no-network harness the loop tests share: an empty
// prober so rounds touch nothing, DNS off so no resolver lookup runs in the
// background, and a long interval so the only round is the immediate one Run fires
// before entering the loop. Iterations are driven by pokes instead, which keeps the
// debounce streaks still enough to assert on.
func newLoopMonitor(t *testing.T, interval time.Duration) (*Monitor, *store.Store, *syncBuf) {
	t.Helper()
	buf := &syncBuf{}
	m, st := newTestMonitorLog(t, 3, 1, buf)
	m.prober = prober.New(nil, time.Second)
	m.DNSFn = func() bool { return false }
	m.IntervalFn = func() time.Duration { return interval }
	return m, st, buf
}

// startLoop runs m.Run in the background and returns poke (fire a settings wake, so
// the loop re-iterates at once rather than at the next deadline) and stop.
func startLoop(t *testing.T, m *Monitor) (poke func(), stop func()) {
	t.Helper()
	var mu sync.Mutex
	wake := make(chan struct{})
	m.WakeFn = func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return wake }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	poke = func() {
		mu.Lock()
		old := wake
		wake = make(chan struct{})
		close(old)
		mu.Unlock()
	}
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	return poke, stop
}

// tick pokes until the loop has read the clock several more times, proving at least
// one FULL iteration ran after the caller's step. Every iteration reads nowFn at its
// top and advances its wall anchor immediately after, so the margin (a paused
// iteration reads twice) makes "the loop has seen this change and acted on it" an
// observation rather than a hope - the negative assertions below are worthless
// otherwise, and a positive one could measure from a stale anchor.
func tick(t *testing.T, clk *wallClock, poke func()) {
	t.Helper()
	target := clk.reads() + 4
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		poke()
		if clk.reads() >= target {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the monitor loop stopped reading the clock (%d reads, want %d): it is wedged",
		clk.reads(), target)
}

// waitFor polls cond until it holds or the deadline passes, mirroring
// TestPauseDetectedOnMidWaitWake's bounded poll.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The threshold arbitrates between two failures that are not symmetric. MISSING a
// freeze leaves unobserved wall time in UptimeSince's observed denominator, scored
// as up - an error bounded by the freeze itself. INVENTING one subtracts genuinely
// observed time from that denominator, and Store.pausedOverlap SUMs pause rows, so
// invented spans compound, can drive observed time to zero, and nothing ever
// re-derives them away. This pins both edges: every ordinary or clock-corrected
// spacing books nothing, a real freeze books exactly its own span, and an already
// open pause span suppresses the row while the gap is still reported (the caller
// must clear the debounce streaks either way).
func TestUnobservedGapBooksOnlyAnomalousWallJumps(t *testing.T) {
	base := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		interval  time.Duration
		delta     time.Duration
		pauseOpen bool
		wantGap   bool  // a gap was detected, so the streaks must be cleared
		wantSpan  int64 // seconds booked; 0 means no row at all
	}{
		// Ordinary spacing at the default 5s cadence, then the same cadence with a
		// stop-the-world stall on a loaded host. A `2*interval` threshold would be TEN
		// SECONDS here and would book that stall as unobserved time.
		{"default cadence", 5 * time.Second, 5 * time.Second, false, false, 0},
		{"default cadence, GC stall", 5 * time.Second, 12 * time.Second, false, false, 0},
		// A round that burns the maximum dial timeout (config.MaxTimeout, 30s) on top of
		// a minute-long interval, plus scheduler jitter.
		{"maxed-out round", time.Minute, 95 * time.Second, false, false, 0},
		// The longest interval an operator can configure (config.MaxInterval): the
		// threshold has to scale with it, or every ordinary wait becomes a "gap".
		{"hour cadence, one ordinary wait", time.Hour, 61 * time.Minute, false, false, 0},
		// A forward NTP step of a few seconds is a clock correction, not a freeze.
		{"forward NTP step", 5 * time.Second, 8 * time.Second, false, false, 0},
		// Backward steps: the arithmetic goes negative and must book nothing rather than
		// reach InsertPause with a duration that describes time running backwards.
		{"backward NTP step", 5 * time.Second, -30 * time.Second, false, false, 0},
		{"clock set back an hour", 5 * time.Second, -time.Hour, false, false, 0},
		// A laptop closed for the night: the case the whole change exists for.
		{"suspend to RAM", 5 * time.Second, 9 * time.Hour, false, true, 32400},
		// The same freeze while monitoring is already switched off. The open pause span
		// already covers this wall stretch, so a row here would be the SECOND over it,
		// pausedOverlap would subtract those hours twice, and observation coverage would
		// climb past 1.0.
		{"suspend while already paused", 5 * time.Second, 9 * time.Hour, true, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stats.ResetForTest()
			snap := capturePauses(t)
			m, _, _ := newLoopMonitor(t, tc.interval)

			got, _ := m.bookUnobservedGap(context.Background(), base, base.Add(tc.delta), tc.pauseOpen, m.interval(), tc.delta)
			if got != tc.wantGap {
				t.Errorf("bookUnobservedGap = %v, want %v (a %v wall delta at a %v interval)",
					got, tc.wantGap, tc.delta, tc.interval)
			}
			rows := snap()
			if tc.wantSpan == 0 {
				if len(rows) != 0 {
					t.Fatalf("booked %+v, want no pause row: a %v wall delta at a %v interval is not "+
						"unobserved time, and a spurious span is subtracted from the uptime denominator "+
						"for as long as the row lives", rows, tc.delta, tc.interval)
				}
				return
			}
			if len(rows) != 1 {
				t.Fatalf("booked %d pause spans, want exactly 1 for a %v freeze", len(rows), tc.delta)
			}
			if rows[0].start.Unix() != base.Unix() || rows[0].dur != tc.wantSpan {
				t.Errorf("booked [%d, +%ds), want [%d, +%ds): the span runs from the last checkpoint "+
					"the loop is known to have reached",
					rows[0].start.Unix(), rows[0].dur, base.Unix(), tc.wantSpan)
			}
			if got := stats.Lifetime().Floats["monitor.unobserved_s"]; got != float64(tc.wantSpan) {
				t.Errorf("monitor.unobserved_s = %v, want %v", got, float64(tc.wantSpan))
			}
		})
	}
}

// A wall clock that is implausibly early is a boot clock, not a monitor that has
// been asleep since 1970. An RTC-less board comes up near the epoch and jumps
// decades forward the moment NTP answers; booking that step would insert a pause
// span longer than the entire recorded history and zero out UptimeSince's observed
// denominator on its own. Store.Prune refuses to act under the same condition for
// the same reason, and like Prune the guard self-heals: once both readings are
// plausible, a real freeze is booked in full.
func TestUnobservedGapIgnoresImplausibleBootClock(t *testing.T) {
	stats.ResetForTest()
	snap := capturePauses(t)
	m, _, _ := newLoopMonitor(t, 5*time.Second)
	ctx := context.Background()

	epoch := time.Unix(120, 0).UTC()                       // a board that booted with no RTC
	synced := time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC) // ...and then NTP answered

	if got := unobservedGap(epoch, synced, time.Minute); got != 0 {
		t.Errorf("unobservedGap across an NTP boot correction = %ds, want 0: that is the clock being "+
			"set, and the span would exceed the whole history", got)
	}
	if got := unobservedGap(epoch, epoch.Add(9*time.Hour), time.Minute); got != 0 {
		t.Errorf("unobservedGap wholly inside the boot clock = %ds, want 0", got)
	}
	if got := unobservedGap(time.Time{}, synced, time.Minute); got != 0 {
		t.Errorf("unobservedGap from a zero anchor = %ds, want 0", got)
	}
	if booked, _ := m.bookUnobservedGap(ctx, epoch, synced, false, m.interval(), synced.Sub(epoch)); booked {
		t.Error("the boot-clock correction was reported as an observation gap")
	}
	if rows := snap(); len(rows) != 0 {
		t.Fatalf("the boot-clock correction booked %+v, want nothing", rows)
	}

	// Self-healing: with both readings plausible, the same nine hours are booked.
	if booked, _ := m.bookUnobservedGap(ctx, synced, synced.Add(9*time.Hour), false, m.interval(), 9*time.Hour); !booked {
		t.Fatal("a nine-hour freeze on a sane clock was not reported as an observation gap")
	}
	rows := snap()
	if len(rows) != 1 || rows[0].dur != 32400 {
		t.Fatalf("booked %+v, want one span of 32400s once the clock is plausible again", rows)
	}
}

// End to end through Run: the loop itself has to notice the freeze. It cannot see
// it any other way - `wait` comes from the monotonic anchor, and CLOCK_MONOTONIC
// does not advance across suspend-to-RAM - so this check is the only thing standing
// between a nine-hour freeze and nine hours of recorded "up".
//
// Two side effects are pinned with it. The debounce streaks are cleared, because a
// freeze is an observation gap exactly like a pause and a streak from before it must
// not confirm a transition with a round nine hours later. And pausedGap is left
// alone: Monitor.transition measures an outage on the MONOTONIC clock, which is
// frozen for the whole suspend, so the freeze is already absent from the recorded
// outage length - folding it into pausedGap as well would subtract it a second time
// and shrink a real outage below its observed length. The gap belongs in the
// denominator, not the numerator.
func TestRunBooksSuspendGapAndClearsStreaks(t *testing.T) {
	stats.ResetForTest()
	lidClosed := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	clk := newWallClock(lidClosed)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	poke, stop := startLoop(t, m)

	// The immediate round Run fires before entering the loop leaves a bad streak
	// behind (the empty prober reports no quorum, and the hour-long interval means no
	// further round can clear or extend it). The freeze must be what resets it.
	waitFor(t, "the immediate round to leave a debounce streak", func() bool {
		return stats.Lifetime().Counters["monitor.rounds"] >= 1
	})

	clk.step(9 * time.Hour) // lid closed at 23:00, opened at 08:00
	tick(t, clk, poke)
	waitFor(t, "the suspend to be booked", func() bool { return len(snap()) >= 1 })
	tick(t, clk, poke) // later iterations must not re-book the same stretch
	stop()

	rows := snap()
	if len(rows) != 1 {
		t.Fatalf("booked %d pause spans, want exactly 1: the wall anchor has to advance past the "+
			"freeze so the next iteration sees ordinary spacing again (%+v)", len(rows), rows)
	}
	if rows[0].start.Unix() != lidClosed.Unix() {
		t.Errorf("pause span starts at %d, want %d (the last checkpoint before the freeze)",
			rows[0].start.Unix(), lidClosed.Unix())
	}
	if rows[0].dur != 32400 {
		t.Errorf("pause span = %ds, want 32400 (the whole unobserved night)", rows[0].dur)
	}
	if m.badStreak != 0 || m.okStreak != 0 {
		t.Errorf("debounce streaks survived the freeze: ok=%d bad=%d; a pre-freeze streak could then "+
			"confirm a transition on the first round after a nine-hour gap", m.okStreak, m.badStreak)
	}
	if m.pausedGap != 0 || !m.downPausedAt.IsZero() {
		t.Errorf("the gap was folded into the outage accounting too (pausedGap=%v downPausedAt=%v); "+
			"transition() already excludes it via the frozen monotonic clock, so this double-subtracts",
			m.pausedGap, m.downPausedAt)
	}
	if got := stats.Lifetime().Counters["monitor.unobserved_gaps"]; got != 1 {
		t.Errorf("monitor.unobserved_gaps = %d, want 1", got)
	}
}

// The two pause paths must TILE the wall clock, never overlap it.
//
// Phase 1 freezes while monitoring is switched off: the open pause span already
// covers that stretch, so the wall check has to stay silent and exactly one row may
// exist. Store.pausedOverlap SUMs pause rows, so a second span over the same nine
// hours subtracts them twice and reports observation coverage above 1.0.
//
// Phase 2 resumes and freezes again while probing: the pause span is closed, so the
// wall check is now the only thing that can book it, and its span must begin exactly
// where the pause span ended - no second double-counted, none left scored as up.
func TestSuspendGapWhilePausedDoesNotDoubleWrite(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 24, 22, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, buf := newLoopMonitor(t, time.Hour)
	var mu sync.Mutex
	enabled := false // the master switch is off before the loop starts
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return enabled }

	poke, stop := startLoop(t, m)
	waitFor(t, "the pause episode to open", func() bool {
		return stats.Lifetime().Counters["monitor.pauses"] == 1
	})

	// Phase 1: nine hours frozen with monitoring already off.
	clk.step(9 * time.Hour)
	tick(t, clk, poke)
	waitFor(t, "the paused freeze to be checkpointed", func() bool { return len(snap()) >= 1 })
	tick(t, clk, poke)
	rows := snap()
	if len(rows) != 1 {
		t.Fatalf("a freeze while already paused booked %d spans, want exactly 1 (%+v): overlapping "+
			"spans are summed by pausedOverlap and push coverage past 1.0", len(rows), rows)
	}
	if rows[0].start.Unix() != start.Unix() || rows[0].dur != 32400 {
		t.Fatalf("paused-freeze span = [%d, +%ds), want [%d, +32400s)",
			rows[0].start.Unix(), rows[0].dur, start.Unix())
	}

	// Phase 2: monitoring comes back on. Wait for the loop's own resume line before
	// touching the clock - stepping it while the pause span is still open would be a
	// different scenario (phase 1's) and would prove nothing about this one.
	mu.Lock()
	enabled = true
	mu.Unlock()
	poke()
	waitFor(t, "the loop to take the resume edge", func() bool {
		poke()
		return buf.has("monitor recording resumed")
	})

	clk.step(3 * time.Hour) // ...and the host freezes again, this time while probing
	tick(t, clk, poke)
	waitFor(t, "the freeze after the resume to be booked", func() bool { return len(snap()) >= 2 })
	tick(t, clk, poke)
	stop()

	rows = snap()
	if len(rows) != 2 {
		t.Fatalf("booked %d spans over the two freezes, want exactly 2 (%+v)", len(rows), rows)
	}
	if rows[1].start.Unix() != start.Add(9*time.Hour).Unix() || rows[1].dur != 10800 {
		t.Errorf("second span = [%d, +%ds), want [%d, +10800s)",
			rows[1].start.Unix(), rows[1].dur, start.Add(9*time.Hour).Unix())
	}
	if end := rows[0].start.Unix() + rows[0].dur; end != rows[1].start.Unix() {
		t.Errorf("the spans do not abut: the first ends at %d, the second starts at %d",
			end, rows[1].start.Unix())
	}
	if total := rows[0].dur + rows[1].dur; total != 43200 {
		t.Errorf("total unobserved = %ds, want 43200 (twelve wall hours, counted exactly once)", total)
	}
}

// A backward wall step is not unobserved time: NTP correcting a fast RTC, or an
// operator setting the clock, makes the delta negative, and a span minted from it
// would describe time running backwards (Store.InsertPause would drop the row, but
// the arithmetic must not get that far). The loop re-anchors on every iteration
// regardless, so the correction costs exactly one detection window: a freeze that
// follows is measured from the CORRECTED anchor and books its full span, where
// keeping the stale anchor would under-report it by the size of the step.
func TestBackwardClockStepBooksNothingAndReanchors(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 25, 2, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, _ := newLoopMonitor(t, time.Hour)
	poke, stop := startLoop(t, m)
	waitFor(t, "the loop to reach its first iteration", func() bool { return clk.reads() > 0 })

	clk.step(-2 * time.Hour) // the RTC was two hours fast; NTP yanks it back
	tick(t, clk, poke)
	if rows := snap(); len(rows) != 0 {
		t.Fatalf("a backward clock step booked %+v, want nothing: that is a correction, not "+
			"unobserved time", rows)
	}

	// Now a real freeze on top of the corrected clock.
	clk.step(9 * time.Hour)
	tick(t, clk, poke)
	waitFor(t, "the freeze after the correction to be booked", func() bool { return len(snap()) >= 1 })
	tick(t, clk, poke)
	stop()

	rows := snap()
	if len(rows) != 1 {
		t.Fatalf("booked %d spans, want exactly 1 (%+v)", len(rows), rows)
	}
	if want := start.Add(-2 * time.Hour).Unix(); rows[0].start.Unix() != want {
		t.Errorf("span starts at %d, want %d: the anchor must follow the correction, or the freeze "+
			"is measured from a wall time that no longer exists", rows[0].start.Unix(), want)
	}
	if rows[0].dur != 32400 {
		t.Errorf("span = %ds, want 32400 (nine hours measured from the corrected anchor)", rows[0].dur)
	}
}

// suspendFixture writes the history both worlds start from: a day of monitoring
// that began 24h ago, one fully-observed 30-minute outage inside it, and a last
// observation 9h ago - the moment the machine either died or froze. Anchored to the
// real clock, because UptimeSince reads time.Now() itself.
func suspendFixture(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	sample := func(ago time.Duration) {
		t.Helper()
		if err := st.InsertSamples(ctx, []store.Sample{{
			TS: now.Add(-ago), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
		}}); err != nil {
			t.Fatalf("insert sample: %v", err)
		}
	}
	event := func(ago time.Duration, typ string, dur int) {
		t.Helper()
		if err := st.InsertEvent(ctx, now.Add(-ago), typ, dur, ""); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
	sample(24 * time.Hour)                         // monitoring anchor (monitoringSince)
	event(20*time.Hour, "down", -1)                // a real, fully-observed outage...
	event(19*time.Hour+30*time.Minute, "up", 1800) // ...thirty minutes long
	sample(9 * time.Hour)                          // the last thing observed before the gap
}

// countPauses reports how many pause spans are persisted.
func countPauses(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count pauses: %v", err)
	}
	return n
}

// readUptime snapshots the two figures the gap feeds: the uptime ratio over a
// window and the observation coverage of that same window.
func readUptime(t *testing.T, st *store.Store, since time.Time) (ratio, coverage float64) {
	t.Helper()
	o, err := st.UptimeSince(context.Background(), since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	return o.Ratio(), o.Coverage()
}

// THE ASYMMETRY, stated as an equality. One real nine-hour gap, two ways for a host
// to produce it: kill the process (Run's startup-gap check books it, and always
// did) or freeze the host (only the loop's wall check can). Unobserved wall time is
// neither up nor down, so the two worlds must read identically - and must read the
// honest figures rather than the flattering ones.
//
// Unfixed, the freeze world reports uptime 0.979167 against the restart world's
// 0.966667 - always biased optimistic, never pessimistic, and compounding across
// every window - and coverage 1 against 0.625, so the series whose HELP text says "a
// low value means the window was mostly paused/unobserved and its uptime_ratio is
// thin evidence" claims perfect confidence in a night it spent nine hours asleep.
// The assertion is the AGREEMENT rather than the arithmetic, following the store's
// cross-component tests.
func TestSuspendAndRestartWorldsAgreeOnUptime(t *testing.T) {
	stats.ResetForTest()
	now := time.Now()
	since := now.Add(-24 * time.Hour)
	frozeAt := now.Add(-9 * time.Hour)

	// World A - the process was killed at 23:00 and started again at 08:00. Run's
	// startup-gap check books the gap from the last observed row; monitoring stays
	// switched off afterwards, so nothing else is written.
	clkA := newWallClock(now)
	swapNow(t, clkA)
	mA, stA, _ := newLoopMonitor(t, time.Hour)
	suspendFixture(t, stA, now)
	mA.EnabledFn = func() bool { return false }
	_, stopA := startLoop(t, mA)
	waitFor(t, "the restart gap to be booked", func() bool { return countPauses(t, stA) == 1 })
	stopA()
	ratioA, covA := readUptime(t, stA, since)

	// World B - the process never stopped; the HOST froze from 23:00 to 08:00. The
	// startup check sees nothing to book (the last observation IS the current wall
	// second), so only the loop's wall check can account for those hours.
	clkB := newWallClock(frozeAt)
	swapNow(t, clkB)
	mB, stB, _ := newLoopMonitor(t, time.Hour)
	suspendFixture(t, stB, now)
	pokeB, stopB := startLoop(t, mB)
	tick(t, clkB, pokeB)
	if n := countPauses(t, stB); n != 0 {
		t.Fatalf("world B booked %d pause rows before the freeze, want 0", n)
	}
	clkB.step(9 * time.Hour) // the lid opens
	tick(t, clkB, pokeB)
	// Deliberately not a Fatal wait: a world B that books nothing is the defect
	// itself, and the comparison below states it far better than a timeout would.
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if countPauses(t, stB) > 0 {
			break
		}
		pokeB()
		time.Sleep(time.Millisecond)
	}
	stopB()
	ratioB, covB := readUptime(t, stB, since)

	if math.Abs(ratioA-ratioB) > 1e-4 {
		t.Errorf("uptime disagrees across the two worlds: restart %.6f vs freeze %.6f. The same nine "+
			"unobserved hours are excluded from the denominator in one and scored as up in the other",
			ratioA, ratioB)
	}
	if math.Abs(covA-covB) > 1e-4 {
		t.Errorf("observation coverage disagrees: restart %.6f vs freeze %.6f", covA, covB)
	}
	// And both are the honest figures: 30 minutes of outage over 15 OBSERVED hours,
	// not over 24 wall hours.
	if want := 1 - 1800.0/54000.0; math.Abs(ratioB-want) > 1e-3 {
		t.Errorf("uptime = %.6f, want ~%.6f (1800s down over 54000s observed, not over the 86400s "+
			"wall window)", ratioB, want)
	}
	if want := 54000.0 / 86400.0; math.Abs(covB-want) > 1e-3 {
		t.Errorf("coverage = %.6f, want ~%.6f: a window that was nine hours dark must not claim full "+
			"observation - the figure exists to say the uptime beside it is thin evidence", covB, want)
	}
}

// An ordinary NTP correction landing inside an OPEN pause span must not mint a
// span of its own. This is the bound on a tradeoff the code cannot escape: a
// forward wall step and a suspend are the same two readings (wall advanced, the
// monotonic floor did not follow), so flushPause cannot tell them apart and
// deliberately trusts the wall - the suspend it exists to catch is common, and a
// large forward step is not. What keeps that safe in practice is pauseCheckpoint:
// an unforced flush below it returns without writing, so the sub-second-to-seconds
// offsets a real time daemon applies (chrony and ntpd SLEW rather than step unless
// the offset clears makestep, typically only at boot) never reach the table on
// their own. The residual error is exactly the step size, folded into the span the
// resume edge force-writes - here 3s against a span that is otherwise honest.
func TestSmallForwardClockStepDoesNotMintAPauseSpan(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 25, 4, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	m, _, buf := newLoopMonitor(t, time.Hour)
	var mu sync.Mutex
	enabled := false // monitoring off, so a pause span is open for the whole test
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return enabled }

	poke, stop := startLoop(t, m)
	waitFor(t, "the pause episode to open", func() bool {
		return stats.Lifetime().Counters["monitor.pauses"] == 1
	})

	// The correction: three seconds forward, two orders of magnitude under the
	// five-minute checkpoint. Several iterations, so this is the loop declining to
	// write rather than the loop not having looked.
	clk.step(3 * time.Second)
	tick(t, clk, poke)
	tick(t, clk, poke)
	tick(t, clk, poke)
	if rows := snap(); len(rows) != 0 {
		t.Fatalf("a 3s forward step booked %d pause span(s) %+v, want none: an unforced flush under "+
			"pauseCheckpoint must not write, or every NTP correction would over-subtract from the "+
			"observed denominator - the opposite direction of the bug this path fixes", len(rows), rows)
	}

	// Resume force-writes the span. It carries the step, and nothing more.
	mu.Lock()
	enabled = true
	mu.Unlock()
	poke()
	waitFor(t, "the loop to take the resume edge", func() bool {
		poke()
		return buf.has("monitor recording resumed")
	})
	stop()

	rows := snap()
	if len(rows) != 1 {
		t.Fatalf("booked %d spans, want exactly 1 (%+v)", len(rows), rows)
	}
	if rows[0].dur != 3 {
		t.Errorf("span = %ds, want 3s: the residual error from a forward step is the step itself, "+
			"so anything larger means the wall delta is being counted twice", rows[0].dur)
	}
}
