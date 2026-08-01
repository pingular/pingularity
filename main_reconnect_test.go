package main

import (
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
)

// clockBase is an arbitrary fixed instant the gate tests advance by hand. Nothing
// in reconnectGate reads a clock, so the tests are wall-clock-free and can step
// hours in a nanosecond.
var clockBase = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// A flapping link confirms a reconnect every few seconds; m.OnReconnect used to
// dispatch a speedtest and a netinfo refresh for every one of them, so the whole
// value of reconnectGate is what it does on the SECOND and later call. Pin the
// three properties the trigger depends on: the first call is never suppressed
// (the trigger exists to measure the link right after it recovers), a call inside
// the window is, and a suppressed call does not slide the window forward - a link
// flapping faster than the window must still get a run once the window expires.
func TestReconnectGateSpacing(t *testing.T) {
	const min = 15 * time.Minute
	var g reconnectGate

	// Zero value is open: no startup priming, no "hot" gate.
	if !g.allow(clockBase, min) {
		t.Fatal("zero-value gate suppressed the first reconnect; the first one must always run")
	}
	// Inside the window: suppressed, at both ends of it.
	if g.allow(clockBase.Add(time.Second), min) {
		t.Error("a reconnect 1s after a fire was allowed; the flap storm is exactly this case")
	}
	if g.allow(clockBase.Add(min-time.Nanosecond), min) {
		t.Error("a reconnect just short of the window was allowed")
	}
	// Exactly at the window: allowed (the spacing is a minimum, not a strict gap).
	if !g.allow(clockBase.Add(min), min) {
		t.Fatal("a reconnect exactly one window after the fire was suppressed")
	}
	// The window now runs from that fire, not from the suppressed attempts in
	// between: one more window from clockBase+min, not from clockBase.
	if g.allow(clockBase.Add(min).Add(min-time.Nanosecond), min) {
		t.Error("window measured from the wrong fire: a call inside the second window was allowed")
	}
	if !g.allow(clockBase.Add(2*min), min) {
		t.Error("a reconnect two windows in was suppressed; suppressed calls must not slide the window")
	}
}

// A sustained flap must not starve the trigger: every call being suppressed would
// slide the window forward forever if allow recorded suppressed attempts too. Ten
// minutes of reconnects every 15s (the shipped-defaults cadence from the bug
// report) must still produce runs on the window, and only on the window.
func TestReconnectGateFlapStormStillFires(t *testing.T) {
	const min = 5 * time.Minute
	var g reconnectGate
	fires := 0
	for i := 0; i <= 40; i++ { // 41 reconnects over 10 minutes
		if g.allow(clockBase.Add(time.Duration(i)*15*time.Second), min) {
			fires++
		}
	}
	// t=0, t=5m, t=10m: the first plus one per elapsed window.
	if fires != 3 {
		t.Errorf("10min of 15s flapping fired %d times, want 3 (one per %v window)", fires, min)
	}
}

// The two jobs the reconnect callback dispatches are metered separately, and the
// cheap one must not inherit the expensive one's floor: at a point where the
// netinfo gate has reopened the speedtest gate must still be shut, or a flap
// storm gets a speedtest every few minutes after all. This is the check that
// fails if someone later "simplifies" the two gates into one shared instance or
// one shared constant.
func TestReconnectGatesAreIndependent(t *testing.T) {
	if reconnectNetinfoGap >= minReconnectSpeedGap {
		t.Fatalf("netinfo floor %v >= speedtest floor %v; the cheap lookup must reopen sooner than a 30-60s bulk transfer",
			reconnectNetinfoGap, minReconnectSpeedGap)
	}
	var netinfoGate, speedGate reconnectGate
	speedMin := reconnectSpeedGap(time.Hour) // stock schedule -> the hard floor

	if !netinfoGate.allow(clockBase, reconnectNetinfoGap) || !speedGate.allow(clockBase, speedMin) {
		t.Fatal("first reconnect suppressed on one of the gates")
	}
	// One netinfo window later: the lookup is due again, the speedtest is not.
	at := clockBase.Add(reconnectNetinfoGap)
	if !netinfoGate.allow(at, reconnectNetinfoGap) {
		t.Errorf("netinfo refresh suppressed %v after the last one, its own floor", reconnectNetinfoGap)
	}
	if speedGate.allow(at, speedMin) {
		t.Errorf("speedtest allowed %v after the last one; its floor is %v", reconnectNetinfoGap, speedMin)
	}
	// One speedtest window later it is due, and the netinfo gate it shares nothing
	// with is unaffected by either decision.
	if !speedGate.allow(clockBase.Add(speedMin), speedMin) {
		t.Errorf("speedtest suppressed %v after the last one, its own floor", speedMin)
	}
}

// reconnectSpeedGap reads the configured speedtest interval as a data budget: the
// spacing is the larger of the hard floor and the user's own cadence, so a 6h
// schedule does not get reconnect tests every 15 minutes. Pin both halves plus the
// floor's own derivation, since a floor below 5x the smallest schedule the
// settings layer accepts (config.MinSpeedInterval) would put us back in
// back-to-back-transfer territory on a flapping link.
func TestReconnectSpeedGap(t *testing.T) {
	if minReconnectSpeedGap < 5*config.MinSpeedInterval {
		t.Errorf("speedtest floor %v is under 5x the %v interval floor", minReconnectSpeedGap, config.MinSpeedInterval)
	}
	cases := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"unset interval falls back to the floor", 0, minReconnectSpeedGap},
		{"minimum schedule takes the floor", config.MinSpeedInterval, minReconnectSpeedGap},
		{"schedule just under the floor takes the floor", minReconnectSpeedGap - time.Minute, minReconnectSpeedGap},
		{"schedule at the floor takes the floor", minReconnectSpeedGap, minReconnectSpeedGap},
		{"stock hourly schedule wins over the floor", time.Hour, time.Hour},
		{"6h schedule is not tested every 15m", 6 * time.Hour, 6 * time.Hour},
		{"maximum schedule wins", config.MaxSpeedInterval, config.MaxSpeedInterval},
	}
	for _, c := range cases {
		if got := reconnectSpeedGap(c.interval); got != c.want {
			t.Errorf("%s: reconnectSpeedGap(%v) = %v, want %v", c.name, c.interval, got, c.want)
		}
	}
}

// The gate is called from the monitor goroutine today, but its correctness must
// not depend on that: concurrent callers at the same instant must still produce
// exactly one fire. Run under -race, where an unguarded last would report.
func TestReconnectGateConcurrentFiresOnce(t *testing.T) {
	var g reconnectGate
	if !g.allow(clockBase, time.Hour) { // close the gate first; the zero value is open
		t.Fatal("first call suppressed")
	}
	const callers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// All well past the window, all at the same instant: exactly one wins.
			if g.allow(clockBase.Add(2*time.Hour), time.Hour) {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if allowed != 1 {
		t.Errorf("%d of %d concurrent callers were allowed, want exactly 1", allowed, callers)
	}
}

// A dispatch that collides with a run already in progress has measured nothing,
// so it must not consume the reconnect window.
//
// The window was reserved at dispatch and kept regardless, on the reasoning that
// the run we collided with is measuring this same recovery. That holds only if it
// started AFTER the link came back. A scheduled run already in flight when the
// reconnect lands is measuring the link while it was still broken - and keeping
// the window then suppresses the real recovery test for the whole gap, which is
// up to 24h once the interval is read as a data budget.
func TestABusyReconnectDispatchGivesTheWindowBack(t *testing.T) {
	var g reconnectGate
	now := time.Now()
	gap := reconnectSpeedGap(time.Hour)

	if !g.allow(now, gap) {
		t.Fatal("the first reconnect must be allowed through an open gate")
	}
	// That dispatch bounced: nothing was measured.
	g.release(now)

	// The next reconnect, moments later, must still get its test.
	if !g.allow(now.Add(time.Second), gap) {
		t.Error("a reconnect was suppressed by a dispatch that measured nothing; the run it " +
			"collided with may predate the reconnect entirely")
	}
}

// ...but a release must never undo a window some LATER dispatch has since taken,
// or a flap storm could hand out an unbounded number of real tests.
func TestReleaseCannotUndoALaterFire(t *testing.T) {
	var g reconnectGate
	first := time.Now()
	gap := reconnectSpeedGap(time.Hour)

	if !g.allow(first, gap) {
		t.Fatal("first dispatch should pass")
	}
	// A second reconnect an hour later legitimately claims the gate.
	second := first.Add(gap + time.Minute)
	if !g.allow(second, gap) {
		t.Fatal("second dispatch should pass after the gap")
	}
	// The FIRST dispatch now reports it bounced. It must not reopen the gate the
	// second one is holding.
	g.release(first)
	if g.allow(second.Add(time.Second), gap) {
		t.Error("a stale release reopened the gate, so a flap storm could dispatch a real " +
			"speedtest per flap")
	}
}

// A run that actually happened keeps the window, which is the whole point of the
// gate.
func TestASuccessfulReconnectRunKeepsTheWindow(t *testing.T) {
	var g reconnectGate
	now := time.Now()
	gap := reconnectSpeedGap(time.Hour)

	if !g.allow(now, gap) {
		t.Fatal("first dispatch should pass")
	}
	// No release: the run measured something.
	if g.allow(now.Add(time.Minute), gap) {
		t.Error("a second reconnect ran a minute after a completed one; the gate is not spacing")
	}
}
