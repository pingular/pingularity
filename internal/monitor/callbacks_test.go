package monitor

import (
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The user-facing alert path: a confirmed down->up must fire OnTransition with
// the right online flag and recovery downtime, and OnReconnect exactly once.
func TestMonitorAlertCallbacks(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1) // downAfter=2, upAfter=1; starts online
	type call struct {
		online bool
		dur    int
	}
	var calls []call
	reconnects := 0
	m.OnTransition = func(online bool, durationS int) { calls = append(calls, call{online, durationS}) }
	m.OnReconnect = func() { reconnects++ }

	feed(m, false, time.Unix(1000, 0)) // bad round 1 of 2 - not yet confirmed
	feed(m, false, time.Unix(1002, 0)) // confirms DOWN
	feed(m, true, time.Unix(1010, 0))  // confirms UP; downtime = 1010-1002 = 8s

	if len(calls) != 2 || calls[0].online || !calls[1].online {
		t.Fatalf("OnTransition calls = %+v, want [{false} {true}]", calls)
	}
	if calls[1].dur != 8 {
		t.Fatalf("reconnect downtime = %ds, want 8s", calls[1].dur)
	}
	if reconnects != 1 {
		t.Fatalf("OnReconnect fired %d times, want 1", reconnects)
	}
}
