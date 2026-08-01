package monitor

import (
	"sync"
	"testing"
	"time"
)

// A suspend is spotted by comparing how much wall time passed against how long the
// loop expected to be asleep. That expectation has to be the wait actually armed.
//
// Every mid-wait wake - any settings broadcast - sends the loop round again, and
// it re-arms only what REMAINS of the interval against the same round anchor. The
// expectation was being refreshed to the whole interval each time, so it drifted
// above the real sleep by however long the loop had already been waiting. On a
// long interval the threshold ends up most of an interval too generous, and a
// suspend inside that margin is never written down - coverage then reads higher
// than the machine can actually vouch for.
func TestTheGapThresholdMatchesTheWaitTheLoopActuallyArmed(t *testing.T) {
	const iv = 10 * time.Second

	var mu sync.Mutex
	type arm struct{ armed, threshold time.Duration }
	var arms []arm
	waitArmedHook = func(armed, threshold time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		arms = append(arms, arm{armed, threshold})
	}
	t.Cleanup(func() { waitArmedHook = nil })

	m, _, _ := newLoopMonitor(t, iv)
	poke, stop := startLoop(t, m)

	// Let the loop sit in its wait for a while, then wake it the way a settings
	// change would. It re-arms the REMAINDER; the threshold must follow it down.
	time.Sleep(1500 * time.Millisecond)
	poke()
	time.Sleep(200 * time.Millisecond)
	stop()

	mu.Lock()
	defer mu.Unlock()
	if len(arms) < 2 {
		t.Fatalf("the loop armed %d waits; the test needs at least the initial one and one "+
			"after the wake", len(arms))
	}
	for i, a := range arms {
		t.Logf("arm %d: armed=%v threshold=%v", i, a.armed.Round(time.Millisecond),
			a.threshold.Round(time.Millisecond))
		if a.threshold > a.armed {
			t.Errorf("arm %d: the next gap check is judged against %v but the loop only sleeps for "+
				"%v. A suspend of up to %v inside that wait falls under the threshold and is never "+
				"recorded as unobserved time", i, a.threshold.Round(time.Millisecond),
				a.armed.Round(time.Millisecond), (a.threshold - a.armed).Round(time.Millisecond))
		}
	}
	last := arms[len(arms)-1]
	if last.armed >= iv-time.Second {
		t.Errorf("the wait re-armed after the wake was %v, not a remainder of the %v interval - "+
			"the test did not reproduce a mid-wait wake, so it proved nothing", last.armed, iv)
	}
}
