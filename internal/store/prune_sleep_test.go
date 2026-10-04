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

// passClock stands in for the clock pair a cleanup reads (pruneClock): the
// wall clock and the store's uptime. A test moves the two by hand while a pass
// runs, together for a sleep and the wall clock alone for a step.
type passClock struct {
	wall time.Time
	up   time.Duration
}

// passClockAt puts a passClock in place that reads wall, with the guard's
// baseline on the same reading, so a pass that starts now finds no step.
func passClockAt(t *testing.T, st *Store, wall time.Time) *passClock {
	t.Helper()
	c := &passClock{wall: wall}
	st.clockMu.Lock()
	st.clockBase, st.clockBaseUp, st.clockSettleUp = wall.Round(0), 0, 0
	st.clockMu.Unlock()
	prev := pruneClock
	pruneClock = func(*Store) (time.Time, time.Duration) { return c.wall, c.up }
	t.Cleanup(func() { pruneClock = prev })
	return c
}

// writtenAt writes what the monitor writes in its first minute after a wake,
// all of it dated at, or within the minute before: a round and a DNS
// reading, the outage the reconnect made, and a speedtest with its selection
// report and the time it ran. It returns a count of those rows by table.
func writtenAt(t *testing.T, st *Store, at time.Time) func() map[string]int64 {
	t.Helper()
	ctx := context.Background()
	mustInsert(t, st, round(at, true))
	mustDNS(t, st, at, 8.5, true)
	if err := st.InsertEvent(ctx, at.Add(-40*time.Second), "down", -1, ""); err != nil {
		t.Fatalf("insert down: %v", err)
	}
	if err := st.InsertEvent(ctx, at.Add(-10*time.Second), "up", 30, ""); err != nil {
		t.Fatalf("insert up: %v", err)
	}
	if err := st.InsertSpeed(ctx, SpeedSample{TS: at.Unix(), DownMbps: 45, UpMbps: 48, PingMS: 5, Server: "s"}); err != nil {
		t.Fatalf("insert speed: %v", err)
	}
	if err := st.InsertSpeedServers(ctx, []SpeedServerRow{{RunTS: at.Unix(), ServerID: "1", Server: "s",
		Selected: true, Measured: true, DownMbps: 45, UpMbps: 48, PingMS: 5}}); err != nil {
		t.Fatalf("insert the selection report: %v", err)
	}
	if ok, err := st.InsertSpeedSpan(ctx, at.Add(-30*time.Second), 20); err != nil || !ok {
		t.Fatalf("insert the speedtest's time: stored %v, err %v", ok, err)
	}
	from := at.Add(-time.Minute).Unix()
	return func() map[string]int64 {
		got := map[string]int64{}
		for tbl, col := range map[string]string{"samples": "ts", "dns": "ts", "events": "ts", "speed": "ts",
			"speed_servers": "run_ts", "speed_spans": "ts"} {
			got[tbl] = countRows(t, st, `SELECT COUNT(*) FROM `+tbl+` WHERE `+col+` BETWEEN ? AND ?`, from, at.Unix())
		}
		return got
	}
}

// A CLEANUP THAT RUNS ACROSS A LONG SLEEP KEEPS WHAT IS WRITTEN AFTER THE WAKE.
//
// A pass deletes the rows stamped more than 48 hours ahead of the clock, and
// it used to work that horizon out once, as it started. Over a big backlog a
// pass runs for minutes and lets the monitor write between its chunks, and its
// waits stand still while the machine sleeps. A pass caught by the lid closing
// went on after the wake with the horizon of the day it started: after more
// than 48 hours asleep, everything the monitor had written since the wake was
// past it, and every table the pass had not reached yet lost those rows. The
// horizon now moves on with the pass's own uptime, which counts the sleep.
//
// Inside a virtual machine the clocks stop while the host sleeps and time sync
// sets the wall clock forward at wake, so the uptime shows none of it. A pass
// that finds the wall clock and its uptime apart deletes no more rows as ahead
// of the clock, and the next pass finds the step as it starts.
func TestACleanupRunningAcrossALongSleepKeepsWhatIsWrittenAfterTheWake(t *testing.T) {
	const away = 72 * time.Hour
	for _, tc := range []struct {
		name         string
		asleep, step time.Duration
	}{
		{"three days asleep", away, 0},
		{"the clock set three days ahead at wake, as inside a virtual machine", 0, away},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openFileStore(t)
			wake := time.Now()
			start := wake.Add(-away)
			// A backlog of more than one chunk, so the pass is still running
			// when the lid closes: the hook below is its first chunk.
			seedExpiredSamples(t, st, 5, start.Add(-2*time.Hour))
			clock := passClockAt(t, st, start)
			pruneChunks(t, 2, 0)
			var left func() map[string]int64
			var wrote map[string]int64
			tooFar := wake.Add(72 * time.Hour).Unix()
			pruneChunkHook = func(string, int64, time.Duration, bool) {
				if left != nil {
					return
				}
				clock.wall, clock.up = wake, tc.asleep
				left = writtenAt(t, st, wake)
				wrote = left()
				// And one row that is ahead of the clock even after the wake.
				if _, err := st.db.Exec(`INSERT INTO samples (ts, target, latency_ms, success, family)
					VALUES (?, 'a', 10.0, 1, 'ipv4')`, tooFar); err != nil {
					t.Fatal(err)
				}
			}
			cut := start.Add(-time.Hour)
			if _, err := st.Prune(context.Background(), cut, cut, cut); err != nil {
				t.Fatalf("prune: %v", err)
			}
			if left == nil {
				t.Fatal("the pass ran no chunk, so nothing was written in the middle of it")
			}
			for tbl, n := range left() {
				if n != wrote[tbl] || n == 0 {
					t.Errorf("%s holds %d of the %d row(s) written after the wake: the pass deleted them as "+
						"stamped ahead of the clock, by the clock of the day it started", tbl, n, wrote[tbl])
				}
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM samples WHERE ts < ?`, cut.Unix()); got != 0 {
				t.Errorf("%d samples older than the window are left: the backlog goes whatever the clock did", got)
			}
			// After a sleep the pass knows the time, and still deletes what is
			// ahead of it. After a step it does not, and leaves that row to a
			// pass that does.
			want := int64(0)
			if tc.step != 0 {
				want = 1
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM samples WHERE ts = ?`, tooFar); got != want {
				t.Errorf("%d sample(s) stamped three days ahead of the wake are left, want %d: a pass deletes "+
					"what is ahead of the clock after a sleep its uptime counted, and not after a step", got, want)
			}
		})
	}
}

// A clock set while a cleanup runs does not move its horizon. Under the 15
// minutes the clock guard lets pass, a step is not told from the clock's
// ordinary drift, and the pass goes on deleting what is ahead of the clock. It
// measures ahead from the clock it started on and its own uptime, so a clock
// set ten minutes back cannot make it delete ten minutes more.
func TestAClockSetBackUnderARunningCleanupDeletesNoMore(t *testing.T) {
	st := openFileStore(t)
	start := time.Now()
	seedExpiredSamples(t, st, 5, start.Add(-2*time.Hour))
	inside := start.Add(pruneFutureSlack - 5*time.Minute).Unix()
	past := start.Add(pruneFutureSlack + 5*time.Minute).Unix()
	for _, ts := range []int64{inside, past} {
		if _, err := st.db.Exec(`INSERT INTO samples (ts, target, latency_ms, success, family)
			VALUES (?, 'a', 10.0, 1, 'ipv4')`, ts); err != nil {
			t.Fatal(err)
		}
	}
	clock := passClockAt(t, st, start)
	pruneChunks(t, 2, 0)
	pruneChunkHook = func(string, int64, time.Duration, bool) { clock.wall = start.Add(-10 * time.Minute) }
	cut := start.Add(-time.Hour)
	if _, err := st.Prune(context.Background(), cut, cut, cut); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM samples WHERE ts = ?`, inside); got != 1 {
		t.Errorf("the sample stamped 47 h 55 min ahead of the pass's start is gone: the pass measured 48 hours " +
			"from a clock set ten minutes back while it ran")
	}
	if got := countRows(t, st, `SELECT COUNT(*) FROM samples WHERE ts = ?`, past); got != 0 {
		t.Errorf("the sample stamped 48 h 5 min ahead of the pass's start is still there: a step the guard " +
			"lets pass stopped the pass deleting rows ahead of the clock")
	}
}
