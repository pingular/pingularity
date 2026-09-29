package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// fakeBoot stands in for the boot clock (bootClockFn). Where it starts means
// nothing; only how far it moves does.
type fakeBoot struct {
	at time.Duration
	ok bool
}

// fakeBootClock puts a fakeBoot in place of the boot clock. Call it before the
// store opens, since Open reads the boot clock it measures uptime from.
func fakeBootClock(t *testing.T) *fakeBoot {
	t.Helper()
	f := &fakeBoot{at: 1000 * time.Hour, ok: true}
	prev := bootClockFn
	bootClockFn = func() (time.Duration, bool) { return f.at, f.ok }
	t.Cleanup(func() { bootClockFn = prev })
	return f
}

// sleepFor makes the store's clocks read the way they do after the machine
// slept for asleep: the wall clock and the boot clock moved on by the same
// amount, and Go's monotonic clock did not move at all. step is a jump of the
// wall clock on top, the way NTP setting the clock at wake would add one; 0
// for a plain sleep, and asleep 0 for a step while awake.
//
// No test can move the real wall clock, so the guard's wall baseline moves
// back instead. The guard only ever reads the wall clock against that
// baseline, so to it the two are the same thing.
func sleepFor(st *Store, boot *fakeBoot, asleep, step time.Duration) {
	st.clockMu.Lock()
	st.clockBase = st.clockBase.Add(-asleep - step)
	st.clockMu.Unlock()
	boot.at += asleep
}

// stepSeen reads pauseStepSeen under its lock: the flag a detected step sets,
// which parks the deferred pause repair and later arms a re-judgement.
func stepSeen(st *Store) bool {
	st.clockMu.Lock()
	defer st.clockMu.Unlock()
	return st.pauseStepSeen
}

func skippedForClock() int64 { return stats.Lifetime().Counters["db.prune_skipped_clock"] }

// A LID CLOSED FOR THE NIGHT IS NOT A CLOCK STEP.
//
// The guard used to measure time passing by Go's monotonic clock. On macOS and
// Linux that clock stands still while the machine sleeps, and the wall clock
// does not, so a night asleep read as the wall clock jumping nine hours ahead:
// cleanup parked for six more AWAKE hours, counted a skip at every hourly try,
// and noted a step that later armed a re-judgement of the pauses for nothing.
// The boot clock counts the night too, so both sides of the comparison move
// together.
func TestPruneKeepsCleaningUpAfterTheMachineSleeps(t *testing.T) {
	boot := fakeBootClock(t)
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	count := seedHistory(t, st, now) // an hour old
	skipped := skippedForClock()

	sleepFor(st, boot, 9*time.Hour, 0)

	// A window the hour-old rows are past, as the first cleanup after the night finds them.
	cut := now.Add(-time.Minute)
	if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for tbl, n := range count() {
		if n != 0 {
			t.Errorf("%s kept %d row(s) past retention: the cleanup after a night asleep "+
				"parked, as if the clock had been set nine hours ahead", tbl, n)
		}
	}
	if got := skippedForClock() - skipped; got != 0 {
		t.Errorf("db.prune_skipped_clock moved by %d over a night asleep, want 0", got)
	}
	if stepSeen(st) || st.pauseRepairArmed() {
		t.Errorf("a night asleep left the pauses owed a re-judgement (step seen %v, armed %v); "+
			"nothing about the clock changed", stepSeen(st), st.pauseRepairArmed())
	}
}

// Naps add up. The baseline the guard judges against moves only when it finds
// a step, so on Go's clock every nap since the last one stayed in the drift:
// two ten-minute naps were a twenty-minute step, and a laptop that dozes
// through the day parked its cleanup as surely as one shut for the night.
func TestPruneKeepsCleaningUpAcrossNapsThatAddUp(t *testing.T) {
	boot := fakeBootClock(t)
	st := open(t)
	ctx := context.Background()
	skipped := skippedForClock()
	for nap := 1; nap <= 4; nap++ {
		now := time.Now()
		count := seedHistory(t, st, now)
		sleepFor(st, boot, 10*time.Minute, 0)
		cut := now.Add(-time.Minute)
		if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
			t.Fatalf("prune after nap %d: %v", nap, err)
		}
		for tbl, n := range count() {
			if n != 0 {
				t.Errorf("after nap %d (%v asleep in all) %s kept %d row(s) past retention: "+
					"the naps added up to a clock step", nap, time.Duration(nap)*10*time.Minute, tbl, n)
			}
		}
		if t.Failed() {
			break
		}
	}
	if got := skippedForClock() - skipped; got != 0 {
		t.Errorf("db.prune_skipped_clock moved by %d over four naps, want 0", got)
	}
	if stepSeen(st) {
		t.Error("four naps were taken for a clock step")
	}
}

// THE BOOT CLOCK MUST NOT HIDE A REAL STEP.
//
// Setting the clock moves the wall clock and leaves the boot clock where it
// was: nothing can set CLOCK_BOOTTIME, and macOS moves kern.boottime along
// with the wall clock so CLOCK_MONOTONIC stays put. So the guard still sees a
// step while the machine is awake, and one made at wake from a sleep, in
// either direction.
//
// The last case is one Go's clock could not see at all. Set back at wake by as
// long as the night was, the wall clock reads what it read at bedtime, and Go's
// clock, stopped for the night, agrees with it: the old guard found no step
// and cleaned up by a clock just set nine hours back.
func TestPruneStillRefusesAClockStepTheBootClockDidNotTake(t *testing.T) {
	for _, tc := range []struct {
		name         string
		asleep, step time.Duration
	}{
		{"two years ahead while awake", 0, 2 * 365 * 24 * time.Hour},
		{"an hour back while awake", 0, -time.Hour},
		{"two hours ahead at wake from a night asleep", 9 * time.Hour, 2 * time.Hour},
		{"an hour back at wake from a night asleep", 9 * time.Hour, -time.Hour},
		{"as far back at wake as the night was long", 9 * time.Hour, -9 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			boot := fakeBootClock(t)
			st := open(t)
			ctx := context.Background()
			now := time.Now()
			count := seedHistory(t, st, now)
			skipped := skippedForClock()

			sleepFor(st, boot, tc.asleep, tc.step)

			cut := now.Add(-time.Minute) // would take the hour-old rows on a trusted clock
			if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
				t.Fatalf("prune: %v", err)
			}
			for tbl, n := range count() {
				if n == 0 {
					t.Errorf("%s was emptied across a clock step of %v: the guard's clock hid it", tbl, tc.step)
				}
			}
			if got := skippedForClock() - skipped; got != 1 {
				t.Errorf("db.prune_skipped_clock moved by %d, want 1", got)
			}
			if !stepSeen(st) {
				t.Error("the step was not noted for the pause re-judgement")
			}
		})
	}
}

// THE WAIT AFTER A REAL STEP COUNTS THE TIME ASLEEP, AS IT DOES ON WINDOWS.
//
// The six-hour settle runs on the same boot clock as the step check, so a step
// found before the lid shuts is believed at the first cleanup after a longer
// sleep, not after six more awake hours. That is kept on purpose (see
// pruneClockSettle): the next pass comes an awake hour after the one that
// found the step, and time sync at wake normally lands well inside it. This
// pins it. A wait that counted awake time alone would still be parked here,
// and so would a guard on Go's clock, which took the sleep itself for a second
// step.
func TestTheWaitAfterAClockStepCountsTheTimeAsleep(t *testing.T) {
	boot := fakeBootClock(t)
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	count := seedHistory(t, st, now)
	skipped := skippedForClock()
	armed := st.pauseRepairArm.Load()
	cut := now.Add(-time.Minute) // would take the hour-old rows on a trusted clock

	// NTP sets the clock two hours ahead while the machine is awake, and the
	// next cleanup finds the step and waits.
	sleepFor(st, boot, 0, 2*time.Hour)
	if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("prune at the step: %v", err)
	}
	for tbl, n := range count() {
		if n == 0 {
			t.Fatalf("premise broken: %s was emptied by the cleanup that found the step", tbl)
		}
	}
	if got := skippedForClock() - skipped; got != 1 || !stepSeen(st) {
		t.Fatalf("premise broken: the step was not found (skips %d, step seen %v)", got, stepSeen(st))
	}

	// Then the lid shuts for seven hours, longer than the wait, and Go's
	// monotonic clock stands still through them. The first cleanup after the
	// wake finds the clock settled.
	sleepFor(st, boot, 7*time.Hour, 0)
	if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("prune after the sleep: %v", err)
	}
	for tbl, n := range count() {
		if n != 0 {
			t.Errorf("%s kept %d row(s) past retention: seven hours asleep did not count towards "+
				"the six-hour wait after the step", tbl, n)
		}
	}
	if got := skippedForClock() - skipped; got != 1 {
		t.Errorf("db.prune_skipped_clock moved by %d over the step and the sleep, want 1: "+
			"the pass that found the step, and none after the wake", got)
	}
	if got := st.pauseRepairArm.Load() - armed; got != 1 {
		t.Errorf("the settled step armed %d pause re-judgement(s), want 1", got)
	}
	if stepSeen(st) || st.pauseRepairArmed() {
		t.Errorf("the settled step is still owed its re-judgement after the cleanup (step seen %v, armed %v)",
			stepSeen(st), st.pauseRepairArmed())
	}

	// Once per step: the cleanup an hour on arms nothing more.
	sleepFor(st, boot, time.Hour, 0)
	if _, err := st.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("prune an hour on: %v", err)
	}
	if got := st.pauseRepairArm.Load() - armed; got != 1 {
		t.Errorf("one step armed the pause re-judgement %d times, want once", got)
	}
}

// THE PAUSE REPAIR'S CLOCK MUST KEEP COUNTING WHILE THE MACHINE SLEEPS.
//
// Once a clock step has settled, the deferred pause repair judges in the frame
// the settle vouched for, moved on by the time since. That time was Go's
// monotonic clock, which stands still while macOS and Linux sleep, so after a
// long weekend with the lid shut the frame sat the whole weekend in the past.
// Anything ending more than 48 hours past it looked like a row from the
// future: the pause that recorded the weekend was held aside, and the weekend
// counted as watched.
func TestPauseRepairFrameKeepsCountingWhileTheMachineSleeps(t *testing.T) {
	boot := fakeBootClock(t)
	st := open(t)

	// The clock was running seven hours fast; NTP sets it back and the guard
	// sees the step. Seven hours later it has settled, and the settle arms a
	// re-judgement that the next write runs at once.
	if !st.clockStepped(time.Now().Add(-7*time.Hour), 0) {
		t.Fatal("premise broken: the correction was not seen as a step")
	}
	boot.at += 7 * time.Hour
	vetted := time.Now()
	if st.clockStepped(vetted, 7*time.Hour) {
		t.Fatal("premise broken: the corrected clock had not settled after seven hours")
	}
	if !st.pauseRepairArmed() {
		t.Fatal("premise broken: the settled step armed no re-judgement")
	}
	st.maybeRepairFuturePausesAt(vetted.Unix())
	if st.pauseRepairArmed() {
		t.Fatal("premise broken: the settle's own re-judgement did not run")
	}

	// Then the lid shuts for a long weekend. The monitor books it as time
	// nobody watched, ending at the wake.
	const weekend = 60 * time.Hour
	boot.at += weekend
	wake := vetted.Add(weekend)
	seedLegacyPause(t, st, vetted.Add(5*time.Minute).Unix(), int64((weekend - 5*time.Minute).Seconds()))

	// A restore that brought held rows arms a re-judgement, and the next write
	// runs it. The reading that write carries is ignored: the vetted frame
	// judges, moved on by the weekend as the wall clock was. So the write
	// carries the vetted reading itself, plausible but stale by the weekend. A
	// repair that judged by it would hold the weekend's pause aside, as a frame
	// that stood still through the weekend would.
	if got, ok := st.repairReading(func() int64 { return vetted.Unix() }); !ok || got != wake.Unix() {
		t.Errorf("the repair would judge at %v (trusted %v), want the wake at %v: the frame moves on "+
			"by the time since it was vetted, the weekend asleep included", time.Unix(got, 0), ok, time.Unix(wake.Unix(), 0))
	}
	st.pauseRepairArm.Add(1)
	st.maybeRepairFuturePausesAt(vetted.Unix())

	if held := pauseDurations(t, st, "pauses_quarantine"); len(held) != 0 {
		t.Errorf("the repair held aside %v: it judged by a clock that stood still through the weekend, "+
			"or by the write's stale reading, so the pause that recorded the weekend looked like it "+
			"ended %v in the future", held, weekend)
	}
	if live := pauseDurations(t, st, "pauses"); len(live) != 1 {
		t.Errorf("pauses hold %v after the repair, want the weekend's one row", live)
	}
	if st.pauseRepairArmed() {
		t.Error("the restore's re-judgement did not run")
	}
}

// Without a boot clock the guard keeps Go's monotonic clock, as it did before
// there was one. A store whose Open could not read the boot clock never mixes
// the two clocks, and a reading that fails after Open falls back for that
// reading alone.
func TestUptimeFallsBackToGoMonotonicClockWithoutABootClock(t *testing.T) {
	t.Run("none at open", func(t *testing.T) {
		boot := fakeBootClock(t)
		boot.ok = false
		st := open(t)
		boot.ok, boot.at = true, boot.at+100*time.Hour // it answers later, a long way on
		before := time.Since(st.opened)
		up := st.sinceOpen()
		if up < before || up > time.Since(st.opened) {
			t.Errorf("uptime %v is not Go's clock since Open (between %v and %v): "+
				"a boot clock that came up after Open was mixed in", up, before, time.Since(st.opened))
		}
	})
	t.Run("lost after open", func(t *testing.T) {
		boot := fakeBootClock(t)
		st := open(t)
		boot.at += 100 * time.Hour
		if up := st.sinceOpen(); up != 100*time.Hour {
			t.Fatalf("uptime %v, want the boot clock's 100h", up)
		}
		boot.ok = false
		before := time.Since(st.opened)
		up := st.sinceOpen()
		if up < before || up > time.Since(st.opened) {
			t.Errorf("uptime %v with the boot clock gone is not Go's clock since Open (between %v and %v)",
				up, before, time.Since(st.opened))
		}
	})
}
