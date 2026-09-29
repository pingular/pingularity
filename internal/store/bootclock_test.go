package store

import (
	"runtime"
	"testing"
	"time"
)

// The boot clock is what the cleanup guard measures time passing by (see
// sinceOpen), so it has to be a clock: it must move, and while the machine is
// awake it must keep up with Go's own monotonic clock. It may run AHEAD of
// Go's clock - that is the point, the time asleep is added to it and not to
// Go's - but never behind it by more than a slew and rounding.
//
// The two reads of the boot clock enclose the two of Go's, so a scheduling
// delay can only widen the boot clock's interval. What is left is rate and
// resolution: macOS works CLOCK_MONOTONIC out from the wall clock, to the
// microsecond, and time sync slews that by at most a fraction of a percent
// (adjtime(2)). A slew costs its fraction of the whole wait, and a busy
// machine can stretch the 50 ms wait many times over, so the bound is
// relative: a hundredth of the wait, and a millisecond for resolution. That is
// beyond either, and far short of a clock that stands still.
func TestBootClockMovesAndKeepsUpWithGoMonotonicClock(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		// Go's clock counts sleep on Windows already, so there is no boot clock
		// to add there, and an answer here would move the guard off it.
		if _, ok := bootClock(); ok {
			t.Fatalf("bootClock answered on %s, where the guard is meant to keep Go's monotonic clock", runtime.GOOS)
		}
		return
	}
	b1, ok1 := bootClock()
	m1 := time.Now()
	time.Sleep(50 * time.Millisecond)
	m2 := time.Now()
	b2, ok2 := bootClock()
	if !ok1 || !ok2 {
		t.Fatalf("bootClock failed on %s (%v, %v): cleanup falls back to Go's monotonic clock, "+
			"which stands still while the machine sleeps", runtime.GOOS, ok1, ok2)
	}
	boot, mono := b2-b1, m2.Sub(m1)
	if boot <= 0 {
		t.Fatalf("the boot clock did not move over %v awake: it read %v, then %v", mono, b1, b2)
	}
	if boot < mono-mono/100-time.Millisecond {
		t.Errorf("the boot clock fell behind Go's monotonic clock while the machine was awake: %v against %v", boot, mono)
	}
}
