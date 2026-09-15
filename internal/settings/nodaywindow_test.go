package settings

import (
	"context"
	"testing"
	"time"
)

// A window whose weekday mask selects no day can never be active, so a schedule
// carrying only such windows parks its feature: latency probing does not run and
// no automatic speedtest fires. That is what the operator wrote down, and it is
// what every door has to keep saying - a direct save, a database an older build
// left behind, and a restored backup - because the other reading turns "never
// run" into "run constantly" on an install nobody touched. The dashboard drops
// such a row before it posts and refuses to save a schedule left with nothing,
// so these doors are the ones that reach the daemon without that refusal in
// front of them. A live window beside a dead one keeps gating as it always did.

// noDay is the well-formed mask with every weekday off.
const noDay = "0000000"

// weekHours walks one hour of every weekday, so a gate that is shut all week
// cannot pass by luck of the clock.
func weekHours() []time.Time {
	start := time.Date(2026, 6, 7, 0, 0, 0, 0, time.Local) // a Sunday
	out := make([]time.Time, 0, 7*24)
	for i := 0; i < 7*24; i++ {
		out = append(out, start.Add(time.Duration(i)*time.Hour))
	}
	return out
}

func TestSanitizeWindowsKeepsNoDayWindow(t *testing.T) {
	dead := Window{Days: noDay, Start: 0, End: 0}
	if got := sanitizeWindows([]Window{dead}); len(got) != 1 || got[0] != dead {
		t.Fatalf("sanitizeWindows dropped a window with no day selected: %+v", got)
	}
	// A dead row beside a live one: both stay, in the order they were sent.
	live := Window{Days: "0111110", Start: 540, End: 1020}
	deadHours := Window{Days: noDay, Start: 540, End: 1020}
	got := sanitizeWindows([]Window{deadHours, live})
	if len(got) != 2 || got[0] != deadHours || got[1] != live {
		t.Fatalf("mixed list = %+v, want %+v then %+v", got, deadHours, live)
	}
	// A malformed mask is not a no-day mask: normDays reads it as every day.
	for _, bad := range []string{"", "xyz", "111111"} {
		if got := sanitizeWindows([]Window{{Days: bad, Start: 60, End: 120}}); len(got) != 1 || got[0].Days != AllDays {
			t.Errorf("malformed mask %q: got %+v, want one window on every day", bad, got)
		}
	}
}

func TestNormalizeKeepsNoDayWindowAndItsFlag(t *testing.T) {
	dead := []Window{{Days: noDay, Start: 540, End: 1020}}
	got := normalize(Values{
		SchedLatEnabled: true, SchedLatWindows: dead,
		SchedSpeedEnabled: true, SchedSpeedWindows: dead,
	})
	if !got.SchedLatEnabled || len(got.SchedLatWindows) != 1 {
		t.Errorf("latency: enabled=%v windows=%+v, want on with the window as written", got.SchedLatEnabled, got.SchedLatWindows)
	}
	if !got.SchedSpeedEnabled || len(got.SchedSpeedWindows) != 1 {
		t.Errorf("speed: enabled=%v windows=%+v, want on with the window as written", got.SchedSpeedEnabled, got.SchedSpeedWindows)
	}
	// The guard for a schedule with no window AT ALL is untouched: nothing on
	// screen would explain that one, so it still reads as no restriction.
	none := normalize(Values{SchedLatEnabled: true, SchedSpeedEnabled: true})
	if none.SchedLatEnabled || none.SchedSpeedEnabled {
		t.Errorf("a schedule with no window at all: lat=%v speed=%v, want both forced off", none.SchedLatEnabled, none.SchedSpeedEnabled)
	}
}

// The boot notice's source of truth: which features are switched on with
// nothing that can ever be active. A live window anywhere in the list clears it.
func TestNeverActiveSchedules(t *testing.T) {
	b := newFileBoot(t)
	c := b.boot()
	if got := c.NeverActiveSchedules(); len(got) != 0 {
		t.Errorf("a fresh store names %v, want nothing parked", got)
	}
	ctx := context.Background()
	if _, err := c.Update(ctx, Patch{
		SchedLatEnabled: pv(true), SchedLatWindows: []Window{{Days: noDay}},
		SchedSpeedEnabled: pv(true), SchedSpeedWindows: []Window{{Days: noDay, Start: 540, End: 1020}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := c.NeverActiveSchedules()
	if len(got) != 2 || got[0] != "latency" || got[1] != "speedtest" {
		t.Errorf("NeverActiveSchedules = %v, want [latency speedtest]", got)
	}
	// One live window is enough to make a schedule reachable again.
	if _, err := c.Update(ctx, Patch{
		SchedSpeedWindows: []Window{{Days: noDay, Start: 540, End: 1020}, {Days: "0000001"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := c.NeverActiveSchedules(); len(got) != 1 || got[0] != "latency" {
		t.Errorf("NeverActiveSchedules = %v, want just [latency] once the speed schedule has a live window", got)
	}
	// Malformed data fails OPEN in the gate (windowActive returns true on a mask
	// that is not seven days), so it must fail open here too - announcing a
	// parked schedule that is in fact wide open would send an operator hunting
	// for a day to pick that is already picked.
	for _, bad := range []string{"", "xyz", "111111"} {
		if !windowsEverActive([]Window{{Days: bad}}) {
			t.Errorf("mask %q reads as never active, but windowActive lets it through", bad)
		}
	}
}

// The save door: a Patch carrying an on toggle and only a no-day window. Both
// gates must stay shut, the snapshot must read back what was sent, and a
// restart must agree with it.
func TestNoDayWindowViaUpdateParksProbing(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	c := b.boot()
	if _, err := c.Update(ctx, Patch{
		SchedLatEnabled: pv(true), SchedLatWindows: []Window{{Days: noDay}},
		SchedSpeedEnabled: pv(true), SchedSpeedWindows: []Window{{Days: noDay, Start: 540, End: 1020}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	v := c.Snapshot()
	if !v.SchedLatEnabled || len(v.SchedLatWindows) != 1 || !v.SchedSpeedEnabled || len(v.SchedSpeedWindows) != 1 {
		t.Errorf("after the save: lat=%v/%+v speed=%v/%+v, want both on with the window as sent",
			v.SchedLatEnabled, v.SchedLatWindows, v.SchedSpeedEnabled, v.SchedSpeedWindows)
	}
	for _, at := range weekHours() {
		if c.LatencyAllowed(at) || c.SpeedAllowed(at) {
			t.Fatalf("%s: latency=%v speed=%v - a schedule with no day selected opened the gate",
				at.Format("Mon 15:04"), c.LatencyAllowed(at), c.SpeedAllowed(at))
		}
	}
	c = b.boot()
	if v := c.get(); !v.SchedLatEnabled || !v.SchedSpeedEnabled || len(v.SchedLatWindows) != 1 || len(v.SchedSpeedWindows) != 1 {
		t.Errorf("after a restart: lat=%v/%+v speed=%v/%+v, want both on with their window",
			v.SchedLatEnabled, v.SchedLatWindows, v.SchedSpeedEnabled, v.SchedSpeedWindows)
	}
}

// The upgrade door: a database from before the window list, holding the old
// single window with every day off. loadSchedule carries it across verbatim, so
// this route needs no operator action at all - only a restart on the newer
// build, and the feature it parked must still be parked. The consumed toggle
// and the window must be written under their own keys as the live-window
// migration's are, so the second boot reads the same thing back.
func TestLegacyNoDayWindowStaysParked(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"schedule_enabled": "1",
		"sched_lat_days":   noDay, "sched_lat_start": "540", "sched_lat_end": "1020",
	})
	c := b.boot()
	if v := c.get(); !v.SchedLatEnabled || len(v.SchedLatWindows) != 1 || v.SchedLatWindows[0].Days != noDay {
		t.Fatalf("legacy no-day window loaded as enabled=%v windows=%+v, want on with the window as stored", v.SchedLatEnabled, v.SchedLatWindows)
	}
	for _, at := range weekHours() {
		if c.LatencyAllowed(at) {
			t.Fatalf("%s: latency probing resumed under a migrated window that selects no day", at.Format("Mon 15:04"))
		}
	}
	all := b.settings()
	if all["sched_lat_enabled"] != "1" || all["sched_lat_windows"] != `[{"days":"0000000","start":540,"end":1020}]` {
		t.Errorf("migrated schedule written as sched_lat_enabled=%q sched_lat_windows=%q, want \"1\" and the window as stored",
			all["sched_lat_enabled"], all["sched_lat_windows"])
	}
	c = b.boot()
	if v := c.get(); !v.SchedLatEnabled || len(v.SchedLatWindows) != 1 {
		t.Errorf("second boot: enabled=%v windows=%+v, want on with the window", v.SchedLatEnabled, v.SchedLatWindows)
	}
}

// The backup door: a config restore writes the windows key straight into the
// table as JSON and reloads, so the list arrives as stored text rather than a
// Patch - once through New on the next boot, and once through Reload on the
// live controller. A live window in the same list must survive and still gate.
func TestStoredNoDayWindowStaysParked(t *testing.T) {
	ctx := context.Background()
	b := newFileBoot(t)
	rows := map[string]string{
		"sched_lat_enabled":   "1",
		"sched_lat_windows":   `[{"days":"0000000","start":0,"end":0}]`,
		"sched_speed_enabled": "1",
		"sched_speed_windows": `[{"days":"0000000","start":540,"end":1020},{"days":"0000001","start":0,"end":0}]`,
	}
	check := func(c *Controller, door string) {
		t.Helper()
		v := c.get()
		if !v.SchedLatEnabled || len(v.SchedLatWindows) != 1 {
			t.Errorf("%s: latency enabled=%v windows=%+v, want on with the stored window", door, v.SchedLatEnabled, v.SchedLatWindows)
		}
		dead, sat := Window{Days: noDay, Start: 540, End: 1020}, Window{Days: "0000001", Start: 0, End: 0}
		if !v.SchedSpeedEnabled || len(v.SchedSpeedWindows) != 2 || v.SchedSpeedWindows[0] != dead || v.SchedSpeedWindows[1] != sat {
			t.Errorf("%s: speed enabled=%v windows=%+v, want on with %+v then %+v", door, v.SchedSpeedEnabled, v.SchedSpeedWindows, dead, sat)
		}
		for _, at := range weekHours() {
			if c.LatencyAllowed(at) {
				t.Fatalf("%s: %s: latency probing resumed under a stored window that selects no day", door, at.Format("Mon 15:04"))
			}
		}
		// The Saturday window still gates speedtests: allowed on Saturday, not on Wednesday.
		if !c.SpeedAllowed(time.Date(2026, 6, 13, 12, 0, 0, 0, time.Local)) {
			t.Errorf("%s: the live Saturday window must allow a Saturday speedtest", door)
		}
		if c.SpeedAllowed(time.Date(2026, 6, 10, 12, 0, 0, 0, time.Local)) {
			t.Errorf("%s: keeping the dead window must not open the speed schedule on a Wednesday", door)
		}
		if got := c.NeverActiveSchedules(); len(got) != 1 || got[0] != "latency" {
			t.Errorf("%s: NeverActiveSchedules = %v, want just [latency]", door, got)
		}
	}
	// Boot over the restored rows, as the next start after a restore does.
	b.seed(rows)
	check(b.boot(), "boot")

	// A restore into a running daemon: rows written by another connection, then Reload.
	b2 := newFileBoot(t)
	c := b2.boot()
	if v := c.get(); v.SchedLatEnabled || v.SchedSpeedEnabled {
		t.Fatal("precondition: a fresh store has no schedule")
	}
	b2.seed(rows)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	check(c, "reload")
}
