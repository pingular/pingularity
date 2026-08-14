package monitor

import (
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// Bounce reports come back from another goroutine, so they can arrive out of
// order: dispatch 1 bounces, the episode re-arms, dispatch 2 goes out and
// bounces too - and only THEN does dispatch 1's report land.
//
// The pending slot held whatever arrived last, so that late report overwrote
// the live one. The consumer only honours a report matching the CURRENT
// dispatch, so it then matched nothing: the episode sat re-armed-but-not-really
// and the brownout went unmeasured until latency recovered, which is exactly
// when there is nothing left to measure. The slot only ever moves forward now.
func TestStaleBounceReportCannotEraseTheLiveOne(t *testing.T) {
	stats.ResetForTest()
	var fired int
	var ids []uint64
	var m *Monitor
	m = &Monitor{DegradedPingFn: func() float64 { return 100 }}
	m.OnDegraded = degradedDispatcher(m, func(id uint64) {
		fired++
		ids = append(ids, id)
		// Every dispatch bounces off a run already in flight.
		m.RetryDegraded(id)
		// ...and the PREVIOUS dispatch's report arrives late, right after.
		// Out-of-order delivery, not a second bounce: the same collision
		// reported twice by a slower path.
		if len(ids) > 1 {
			m.RetryDegraded(ids[len(ids)-2])
		}
	})

	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true) // debounce satisfied: dispatch 1
	if fired != 1 {
		t.Fatalf("dispatches = %d, want 1", fired)
	}
	m.checkDegraded(500, true, true) // dispatch 1 bounced: dispatch 2
	if fired != 2 {
		t.Fatalf("dispatches = %d, want 2", fired)
	}
	// Dispatch 2 bounced too, and dispatch 1's late report landed after it. The
	// brownout is still live and still unmeasured, so it must dispatch again.
	m.checkDegraded(500, true, true)
	if fired != 3 {
		t.Fatalf("dispatches = %d, want 3: a LATE report for an already-superseded dispatch (%v) erased the live one, "+
			"so the brownout stopped retrying while it was still happening", fired, ids)
	}
	// Still one brownout, however many dispatches it took.
	if got := stats.Lifetime().Counters["monitor.degraded_episodes"]; got != 1 {
		t.Errorf("monitor.degraded_episodes = %d, want 1", got)
	}
}

// The forward-only rule must not swallow a genuine bounce that happens to be
// reported while an older id still sits in the slot unconsumed.
func TestNewerBounceReportStillWinsOverAPendingOlderOne(t *testing.T) {
	stats.ResetForTest()
	m := &Monitor{DegradedPingFn: func() float64 { return 100 }}
	// An old report parked in the slot, never consumed (its episode ended).
	m.RetryDegraded(1)
	// The live dispatch's own report must replace it, not be dropped.
	m.RetryDegraded(7)
	if got := m.degradedRetry.Load(); got != 7 {
		t.Fatalf("pending bounce = %d, want 7: a newer report must take the slot from a stale one", got)
	}
}
