package monitor

import (
	"context"
	"testing"
	"time"
)

// A suspend that falls inside an outage must be excluded from that outage's
// recorded length on every platform.
//
// transition() measures elapsed with ts.Sub(m.since) - a MONOTONIC subtraction -
// and the monitor used to take it on faith that the monotonic clock freezes while
// the machine sleeps, so the suspend could never be inside the measurement. Go
// only promises that on "some systems", and Windows is not one of them: the
// runtime reads _INTERRUPT_TIME, the biased interrupt time, which has the sleep
// added back on wake.
//
// Where that left Windows: the suspend was still inside the outage's numerator,
// while the pause row written for the same gap removed it from the observed
// denominator. One stretch of clock counted as downtime AND as never-watched -
// an outage reported hours longer than the link was actually down, against a
// window that says it was not watching.

// The decision itself, over the cases each platform produces.
func TestUnobservedInOutageDeductsOnlyWhatTheClockSaw(t *testing.T) {
	const wall = 9 * time.Hour
	for _, tc := range []struct {
		name string
		mono time.Duration
		want time.Duration
	}{
		{"monotonic clock stopped for the whole sleep (macOS, Linux)", 0, 0},
		{"clock ran straight through the sleep (Windows)", wall, wall},
		{"clock stopped for most of it, ran for the rest", 2 * time.Hour, 2 * time.Hour},
		{"awake but stalled - no sleep at all, every second counted", wall, wall},
		{"clock reports more than the wall gap: never deduct past the gap", wall + time.Hour, wall},
		{"nonsense negative reading deducts nothing", -time.Hour, 0},
	} {
		if got := unobservedInOutage(wall, tc.mono); got != tc.want {
			t.Errorf("%s: unobservedInOutage(%v, %v) = %v, want %v", tc.name, wall, tc.mono, got, tc.want)
		}
	}
}

// And the wiring: with an outage open and a clock that ran through the gap - what
// a Windows suspend looks like, and what every fake clock in these tests produces
// since a synthesized time.Time carries no monotonic reading - the gap must land
// in pausedGap, which transition() subtracts.
func TestSuspendInsideAnOutageIsDeductedWhenTheClockKeptRunning(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is in progress

	prev := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	now := prev.Add(9 * time.Hour) // synthesized: Sub() sees the full 9h
	if booked, _ := m.bookUnobservedGap(context.Background(), prev, now, false, m.interval(), now.Sub(prev)); !booked {
		t.Fatal("a nine-hour gap was not booked at all")
	}
	if m.pausedGap != 9*time.Hour {
		t.Errorf("pausedGap = %v, want 9h: the clock advanced through the gap, so transition() "+
			"counted it as downtime and it has to be subtracted back out", m.pausedGap)
	}
}

// The mirror: while the link is UP there is no outage to correct, so the gap must
// not accumulate into the next one.
func TestSuspendWhileOnlineDoesNotTouchOutageAccounting(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = true

	prev := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	if booked, _ := m.bookUnobservedGap(context.Background(), prev, prev.Add(9*time.Hour), false, m.interval(), 9*time.Hour); !booked {
		t.Fatal("a nine-hour gap was not booked at all")
	}
	if m.pausedGap != 0 {
		t.Errorf("pausedGap = %v, want 0: nothing was down, so there is no outage length to correct", m.pausedGap)
	}
}

// A gap too short to be anomalous is ordinary loop spacing and must not be
// deducted from anything.
func TestOrdinarySpacingIsNotDeductedFromAnOutage(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false

	prev := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	if booked, _ := m.bookUnobservedGap(context.Background(), prev, prev.Add(time.Minute), false, m.interval(), time.Minute); booked {
		t.Fatal("one minute of ordinary spacing was booked as an unobserved gap")
	}
	if m.pausedGap != 0 {
		t.Errorf("pausedGap = %v, want 0", m.pausedGap)
	}
}

// A suspend DURING an explicit monitoring pause must be deducted from the outage
// once, not twice.
//
// Two paths adjust the same accumulator. bookUnobservedGap folds in the monotonic
// advance whenever the link is offline; the resume edge then calls noteResume,
// which folds in the whole explicit-pause episode. Both cover the SAME wall
// stretch when the suspend happened inside the pause, and bookUnobservedGap was
// ignoring the pauseOpen flag it is handed - the flag it already uses to suppress
// the duplicate pause ROW, for exactly the same reason.
//
// The consequence is not a cosmetic over-count: transition() subtracts the
// accumulator from the outage length, so a doubled value clamps a real outage to
// zero and the downtime disappears from the log, the heatmap and the digest.
func TestASuspendInsideAnExplicitPauseIsDeductedOnce(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false // an outage is in progress

	start := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	// The pause opened while the link was already down.
	m.notePause(start)
	// Nine hours pass unobserved, and the wall check sees it while the pause is
	// STILL open (pauseOpen=true) - the lid-closed-during-a-pause case.
	if booked, _ := m.bookUnobservedGap(context.Background(), start, start.Add(9*time.Hour), true, m.interval(), 9*time.Hour); !booked {
		t.Fatal("the nine-hour gap was not booked at all")
	}
	// Then monitoring resumes, closing the explicit pause over the same stretch.
	m.noteResume(start.Add(9 * time.Hour))

	if m.pausedGap != 9*time.Hour {
		t.Errorf("pausedGap = %v, want 9h: the suspend and the explicit pause cover the same "+
			"nine hours, and transition() subtracts this from the outage - at %v a real outage "+
			"is clamped to zero and vanishes from every surface", m.pausedGap, m.pausedGap)
	}
}

// With NO explicit pause open, the wall gap is the only record of the unwatched
// stretch, so it must still be deducted - the fix must not silence the case the
// accumulator was added for.
func TestASuspendWithNoPauseOpenIsStillDeducted(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	m.online = false

	start := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)
	if booked, _ := m.bookUnobservedGap(context.Background(), start, start.Add(9*time.Hour), false, m.interval(), 9*time.Hour); !booked {
		t.Fatal("the nine-hour gap was not booked at all")
	}
	if m.pausedGap != 9*time.Hour {
		t.Errorf("pausedGap = %v, want 9h", m.pausedGap)
	}
}

// Lowering the probe interval must not retroactively score the wait that was
// already in progress as unobserved time.
//
// The loop parks for the configured interval, then checks on waking whether the
// wall gap was anomalous. That check used to read the interval FRESH, so an
// operator moving the interval from 1h to 5s made the loop wake at once and judge
// the 50 minutes it had been correctly sleeping against the new 5s cadence. It
// booked a 50-minute pause row: permanent, subtracted from observed time for as
// long as it is retained, and describing a stretch during which the monitor was
// doing exactly what it had been told to do.
func TestLoweringTheIntervalDoesNotBookTheWaitItWasServing(t *testing.T) {
	m, _, _ := newLoopMonitor(t, 5*time.Second) // the NEW, lower interval
	start := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)

	// The wait that just finished was sized by the OLD one-hour cadence, and 50
	// minutes of it elapsed before the settings change woke the loop.
	if booked, _ := m.bookUnobservedGap(context.Background(), start, start.Add(50*time.Minute), false, time.Hour, 50*time.Minute); booked {
		t.Error("booked a pause row for a 50-minute wait that a 1h interval had legitimately " +
			"scheduled; the operator lowering the interval is not an observation gap")
	}
	if m.pausedGap != 0 {
		t.Errorf("pausedGap = %v, want 0", m.pausedGap)
	}
}

// ...but a genuine freeze longer than the cadence that sized the wait is still a
// gap, whatever the interval is now.
func TestAGapBeyondTheServingIntervalIsStillBooked(t *testing.T) {
	m, _, _ := newLoopMonitor(t, time.Hour)
	start := time.Date(2026, 7, 24, 23, 0, 0, 0, time.UTC)

	if booked, _ := m.bookUnobservedGap(context.Background(), start, start.Add(9*time.Hour), false, time.Hour, 9*time.Hour); !booked {
		t.Error("a nine-hour freeze under a one-hour cadence must still book an unobserved gap")
	}
}
