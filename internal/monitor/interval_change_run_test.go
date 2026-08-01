package monitor

import (
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The mid-wait interval change, through Run itself. bookUnobservedGap's own
// tests (see TestLoweringTheIntervalDoesNotBookTheWaitItWasServing) pin what it
// does when handed the interval that SIZED the finished wait - but only the
// loop decides what it is handed, and a call site that went back to reading
// m.interval() fresh, the exact shape of the original bug, passed every test.
//
// So: the loop parks on a one-hour cadence, fifty minutes pass, and the
// operator lowers the interval to five seconds. The settings broadcast wakes
// the loop at once, and the gap check that follows must judge those fifty
// minutes against the hour that scheduled them - not against the new cadence,
// which would mint a permanent 50-minute pause row out of a wait the monitor
// was serving exactly as told.
func TestLoweringTheIntervalMidWaitBooksNothingThroughRun(t *testing.T) {
	stats.ResetForTest()
	start := time.Date(2026, 7, 25, 23, 0, 0, 0, time.UTC)
	clk := newWallClock(start)
	swapNow(t, clk)
	snap := capturePauses(t)

	// Observe the loop arming its hour-long wait, so the change below is
	// guaranteed to land mid-wait rather than before the loop first read the
	// interval.
	var hmu sync.Mutex
	armed := 0
	waitArmedHook = func(_, _ time.Duration) {
		hmu.Lock()
		armed++
		hmu.Unlock()
	}
	t.Cleanup(func() { waitArmedHook = nil })

	m, _, _ := newLoopMonitor(t, time.Hour)
	var imu sync.Mutex
	iv := time.Hour
	m.IntervalFn = func() time.Duration { imu.Lock(); defer imu.Unlock(); return iv }

	poke, stop := startLoop(t, m)
	waitFor(t, "the hour-long wait to be armed", func() bool {
		hmu.Lock()
		defer hmu.Unlock()
		return armed >= 1
	})

	// Fifty minutes into the wait, the interval comes down; the broadcast wakes
	// the loop the same instant.
	imu.Lock()
	iv = 5 * time.Second
	imu.Unlock()
	clk.step(50 * time.Minute)
	tick(t, clk, poke)
	tick(t, clk, poke) // later iterations must stay silent about it too
	stop()

	if rows := snap(); len(rows) != 0 {
		t.Fatalf("booked %+v, want no pause row: the 50 minutes were a wait the 1h interval had "+
			"legitimately scheduled, and the row would understate coverage for as long as it is "+
			"retained", rows)
	}
	if got := stats.Lifetime().Counters["monitor.unobserved_gaps"]; got != 0 {
		t.Errorf("monitor.unobserved_gaps = %d, want 0: the operator changing a setting is not an "+
			"observation gap", got)
	}
	if m.pausedGap != 0 {
		t.Errorf("pausedGap = %v, want 0", m.pausedGap)
	}
}
