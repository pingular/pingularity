package settings

import (
	"context"
	"testing"
	"time"
)

// A window whose weekday mask selects no day can never be active, so it is the
// "no windows" state wearing a row. normalize's empty-list guard was written for
// that state, and a schedule switched on with only such a window slipped past
// it: latency probing stopped and no automatic speedtest fired, with the toggle
// still reading on and nothing to say why. The dashboard drops such a row
// before it posts; these pin that the daemon does the same at every door of
// its own - a direct save, a database an older build left behind, and a
// restored backup - and that a live window beside a dead one is untouched.

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

func TestSanitizeWindowsDropsNoDayWindow(t *testing.T) {
	if got := sanitizeWindows([]Window{{Days: noDay, Start: 0, End: 0}}); len(got) != 0 {
		t.Fatalf("a window with no day selected survived sanitizeWindows: %+v", got)
	}
	// A dead row beside a live one goes; the live one stays exactly as it was.
	live := Window{Days: "0111110", Start: 540, End: 1020}
	got := sanitizeWindows([]Window{{Days: noDay, Start: 540, End: 1020}, live})
	if len(got) != 1 || got[0] != live {
		t.Fatalf("mixed list = %+v, want just %+v", got, live)
	}
	// A malformed mask is not a no-day mask: normDays reads it as every day, and
	// that window stays.
	for _, bad := range []string{"", "xyz", "111111"} {
		if got := sanitizeWindows([]Window{{Days: bad, Start: 60, End: 120}}); len(got) != 1 || got[0].Days != AllDays {
			t.Errorf("malformed mask %q: got %+v, want one window on every day", bad, got)
		}
	}
}

func TestNormalizeNoDayWindowForcesFlagOff(t *testing.T) {
	dead := []Window{{Days: noDay, Start: 540, End: 1020}}
	got := normalize(Values{
		SchedLatEnabled: true, SchedLatWindows: dead,
		SchedSpeedEnabled: true, SchedSpeedWindows: dead,
	})
	if got.SchedLatEnabled || len(got.SchedLatWindows) != 0 {
		t.Errorf("latency: enabled=%v windows=%+v, want off with no windows", got.SchedLatEnabled, got.SchedLatWindows)
	}
	if got.SchedSpeedEnabled || len(got.SchedSpeedWindows) != 0 {
		t.Errorf("speed: enabled=%v windows=%+v, want off with no windows", got.SchedSpeedEnabled, got.SchedSpeedWindows)
	}
}

// The save door: a Patch carrying an on toggle and only a no-day window. Both
// gates must keep answering "allowed", the snapshot must read as off, and a
// restart must agree with it.
func TestNoDayWindowViaUpdateKeepsProbing(t *testing.T) {
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
	if v.SchedLatEnabled || len(v.SchedLatWindows) != 0 || v.SchedSpeedEnabled || len(v.SchedSpeedWindows) != 0 {
		t.Errorf("after the save: lat=%v/%+v speed=%v/%+v, want both off with no windows",
			v.SchedLatEnabled, v.SchedLatWindows, v.SchedSpeedEnabled, v.SchedSpeedWindows)
	}
	for _, at := range weekHours() {
		if !c.LatencyAllowed(at) || !c.SpeedAllowed(at) {
			t.Fatalf("%s: latency=%v speed=%v - an on toggle with no day selected shut the gate",
				at.Format("Mon 15:04"), c.LatencyAllowed(at), c.SpeedAllowed(at))
		}
	}
	c = b.boot()
	if v := c.get(); v.SchedLatEnabled || v.SchedSpeedEnabled || len(v.SchedLatWindows) != 0 || len(v.SchedSpeedWindows) != 0 {
		t.Errorf("after a restart: lat=%v/%+v speed=%v/%+v, want both off with no windows",
			v.SchedLatEnabled, v.SchedLatWindows, v.SchedSpeedEnabled, v.SchedSpeedWindows)
	}
}

// The upgrade door: a database from before the window list, holding the old
// single window with every day off. loadSchedule carries it across verbatim,
// so this route needs no operator action at all - only a restart on the newer
// build. It must load off, and the consumed toggle and the (now empty) list
// must be written under their own keys as the live-window migration's are, so
// the second boot reads the same thing back.
func TestLegacyNoDayWindowLoadsOff(t *testing.T) {
	b := newFileBoot(t)
	b.seed(map[string]string{
		"schedule_enabled": "1",
		"sched_lat_days":   noDay, "sched_lat_start": "540", "sched_lat_end": "1020",
	})
	c := b.boot()
	if v := c.get(); v.SchedLatEnabled || len(v.SchedLatWindows) != 0 {
		t.Fatalf("legacy no-day window loaded as enabled=%v windows=%+v, want off with no windows", v.SchedLatEnabled, v.SchedLatWindows)
	}
	for _, at := range weekHours() {
		if !c.LatencyAllowed(at) {
			t.Fatalf("%s: latency probing gated off by a migrated window that selects no day", at.Format("Mon 15:04"))
		}
	}
	all := b.settings()
	if all["sched_lat_enabled"] != "0" || all["sched_lat_windows"] != "[]" {
		t.Errorf("migrated schedule written as sched_lat_enabled=%q sched_lat_windows=%q, want \"0\" and \"[]\"",
			all["sched_lat_enabled"], all["sched_lat_windows"])
	}
	c = b.boot()
	if v := c.get(); v.SchedLatEnabled || len(v.SchedLatWindows) != 0 {
		t.Errorf("second boot: enabled=%v windows=%+v, want off with no windows", v.SchedLatEnabled, v.SchedLatWindows)
	}
}

// The backup door: a config restore writes the windows key straight into the
// table as JSON and reloads, so the list arrives as stored text rather than a
// Patch - once through New on the next boot, and once through Reload on the
// live controller. A live window in the same list must survive and still gate.
func TestStoredNoDayWindowLoadsOff(t *testing.T) {
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
		if v.SchedLatEnabled || len(v.SchedLatWindows) != 0 {
			t.Errorf("%s: latency enabled=%v windows=%+v, want off with no windows", door, v.SchedLatEnabled, v.SchedLatWindows)
		}
		sat := Window{Days: "0000001", Start: 0, End: 0}
		if !v.SchedSpeedEnabled || len(v.SchedSpeedWindows) != 1 || v.SchedSpeedWindows[0] != sat {
			t.Errorf("%s: speed enabled=%v windows=%+v, want on with just %+v", door, v.SchedSpeedEnabled, v.SchedSpeedWindows, sat)
		}
		for _, at := range weekHours() {
			if !c.LatencyAllowed(at) {
				t.Fatalf("%s: %s: latency probing gated off by a stored window that selects no day", door, at.Format("Mon 15:04"))
			}
		}
		// The Saturday window still gates speedtests: allowed on Saturday, not on Wednesday.
		if !c.SpeedAllowed(time.Date(2026, 6, 13, 12, 0, 0, 0, time.Local)) {
			t.Errorf("%s: the live Saturday window must allow a Saturday speedtest", door)
		}
		if c.SpeedAllowed(time.Date(2026, 6, 10, 12, 0, 0, 0, time.Local)) {
			t.Errorf("%s: dropping the dead window must not open the speed schedule on a Wednesday", door)
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
