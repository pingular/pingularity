package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// SpeedAllowed / LatencyAllowed are the scheduler-facing probe gates. Each must
// short-circuit to true when its schedule is disabled, deny when enabled with a
// window that doesn't cover now, and the two features must gate independently. An
// enabled schedule with NO windows is normalized off (Update runs normalize), so
// it reads as unscheduled = allowed rather than a silent "never" (audit #15).
func TestSpeedAndLatencyAllowed(t *testing.T) {
	mk := func(sched Values) *Controller {
		t.Helper()
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		c, err := New(context.Background(), st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2, Monitoring: true})
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		// Set the schedule via the real Update path (New's overlay re-derives the
		// schedule from the persisted map, so it can't be seeded through def).
		if _, err := c.Update(context.Background(), Patch{
			SchedSpeedEnabled: &sched.SchedSpeedEnabled, SchedSpeedWindows: sched.SchedSpeedWindows,
			SchedLatEnabled: &sched.SchedLatEnabled, SchedLatWindows: sched.SchedLatWindows,
		}); err != nil {
			t.Fatalf("update schedule: %v", err)
		}
		return c
	}
	noon := time.Date(2026, 6, 10, 12, 0, 0, 0, time.Local) // Wednesday
	allDay := []Window{{Days: AllDays, Start: 0, End: 0}}
	earlyOnly := []Window{{Days: AllDays, Start: 0, End: 60}} // 00:00-01:00 only

	// Speed gate.
	if !mk(Values{SchedSpeedEnabled: false}).SpeedAllowed(noon) {
		t.Error("speed: disabled schedule must allow (24/7)")
	}
	if !mk(Values{SchedSpeedEnabled: true}).SpeedAllowed(noon) {
		t.Error("speed: enabled with no windows is normalized off, must allow")
	}
	if !mk(Values{SchedSpeedEnabled: true, SchedSpeedWindows: allDay}).SpeedAllowed(noon) {
		t.Error("speed: all-day window must allow")
	}
	if mk(Values{SchedSpeedEnabled: true, SchedSpeedWindows: earlyOnly}).SpeedAllowed(noon) {
		t.Error("speed: a window not covering noon must deny")
	}

	// Latency gate.
	if !mk(Values{SchedLatEnabled: false}).LatencyAllowed(noon) {
		t.Error("latency: disabled schedule must allow")
	}
	if !mk(Values{SchedLatEnabled: true}).LatencyAllowed(noon) {
		t.Error("latency: enabled with no windows is normalized off, must allow")
	}
	if !mk(Values{SchedLatEnabled: true, SchedLatWindows: allDay}).LatencyAllowed(noon) {
		t.Error("latency: all-day window must allow")
	}

	// Independence: scheduling speed must not gate latency.
	if !mk(Values{SchedSpeedEnabled: true, SchedSpeedWindows: earlyOnly}).LatencyAllowed(noon) {
		t.Error("latency must stay 24/7 when only the speed schedule is enabled")
	}
}
