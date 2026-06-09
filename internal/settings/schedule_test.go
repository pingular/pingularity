package settings

import (
	"testing"
	"time"
)

// maskFor returns an all-zero weekday mask with the given weekdays set to '1'.
func maskFor(wds ...time.Weekday) string {
	b := []byte("0000000")
	for _, wd := range wds {
		b[int(wd)] = '1'
	}
	return string(b)
}

func TestWindowActive(t *testing.T) {
	// A known Wednesday and the day before (Tuesday).
	wed := time.Date(2026, 6, 10, 0, 0, 0, 0, time.Local)
	if wed.Weekday() != time.Wednesday {
		t.Fatalf("test date is %v, expected Wednesday", wed.Weekday())
	}
	at := func(h, m int) time.Time { return time.Date(2026, 6, 10, h, m, 0, 0, time.Local) }
	all := AllDays

	cases := []struct {
		name  string
		days  string
		start int
		end   int
		t     time.Time
		want  bool
	}{
		{"all day all days, noon", all, 0, 0, at(12, 0), true},
		{"all day all days, midnight", all, 0, 0, at(0, 0), true},
		{"day not selected", maskFor(time.Monday), 0, 0, at(12, 0), false},
		{"same-day window inside", all, 9 * 60, 17 * 60, at(10, 0), true},
		{"same-day window before", all, 9 * 60, 17 * 60, at(8, 59), false},
		{"same-day window at end (exclusive)", all, 9 * 60, 17 * 60, at(17, 0), false},
		{"same-day window, day off", maskFor(time.Monday), 9 * 60, 17 * 60, at(10, 0), false},
		{"overnight, evening of selected day", maskFor(time.Wednesday), 22 * 60, 6 * 60, at(23, 0), true},
		{"overnight, daytime gap", maskFor(time.Wednesday), 22 * 60, 6 * 60, at(12, 0), false},
		{"overnight, morning belongs to prev day (Tue selected)", maskFor(time.Tuesday), 22 * 60, 6 * 60, at(5, 0), true},
		{"overnight, morning, prev day NOT selected", maskFor(time.Wednesday), 22 * 60, 6 * 60, at(5, 0), false},
		{"malformed days fails open", "xyz", 9 * 60, 17 * 60, at(3, 0), true},
	}
	for _, c := range cases {
		if got := windowActive(c.days, c.start, c.end, c.t); got != c.want {
			t.Errorf("%s: windowActive(%q,%d,%d,%s)=%v want %v",
				c.name, c.days, c.start, c.end, c.t.Format("Mon 15:04"), got, c.want)
		}
	}
}

// windowsActive is the OR of a feature's windows; an empty list is never active.
func TestWindowsActive(t *testing.T) {
	noon := time.Date(2026, 6, 10, 12, 0, 0, 0, time.Local)   // Wednesday
	morning := time.Date(2026, 6, 10, 7, 0, 0, 0, time.Local) // Wednesday
	if windowsActive(nil, noon) {
		t.Fatal("empty window list must never be active")
	}
	// Split day: 06:00-09:00 and 18:00-23:00. Morning matches the first; noon neither.
	split := []Window{{AllDays, 6 * 60, 9 * 60}, {AllDays, 18 * 60, 23 * 60}}
	if !windowsActive(split, morning) {
		t.Fatal("07:00 should fall in the 06:00-09:00 window")
	}
	if windowsActive(split, noon) {
		t.Fatal("noon is in neither split window")
	}
	// Per-day: a Wednesday all-day window matches even alongside a Monday-only one.
	perDay := []Window{{maskFor(time.Monday), 0, 0}, {maskFor(time.Wednesday), 0, 0}}
	if !windowsActive(perDay, noon) {
		t.Fatal("Wednesday all-day window should match Wednesday noon")
	}
}

// loadSchedule migrates the legacy single-window keys and honors the legacy
// master toggle, while preferring the new windows key when present.
func TestLoadScheduleMigration(t *testing.T) {
	keys := []string{"sched_lat_enabled", "sched_lat_windows", "sched_lat_days", "sched_lat_start", "sched_lat_end"}
	call := func(m map[string]string) (bool, []Window) {
		return loadSchedule(m, keys[0], keys[1], keys[2], keys[3], keys[4])
	}
	// Legacy: master toggle seeds enabled; single window migrates.
	en, ws := call(map[string]string{
		"schedule_enabled": "true",
		"sched_lat_days":   maskFor(time.Monday, time.Friday),
		"sched_lat_start":  "540", "sched_lat_end": "1020",
	})
	if !en {
		t.Fatal("legacy master toggle should seed enabled")
	}
	if len(ws) != 1 || ws[0].Start != 540 || ws[0].End != 1020 || ws[0].Days != maskFor(time.Monday, time.Friday) {
		t.Fatalf("legacy migration produced %+v", ws)
	}
	// New windows key wins over legacy day keys.
	en2, ws2 := call(map[string]string{
		"sched_lat_enabled": "true",
		"sched_lat_windows": `[{"days":"1111111","start":0,"end":0},{"days":"0000011","start":600,"end":840}]`,
		"sched_lat_days":    "1000000",
	})
	if !en2 || len(ws2) != 2 || ws2[1].Start != 600 {
		t.Fatalf("new windows not used: en=%v ws=%+v", en2, ws2)
	}
	// Nothing set: disabled, no windows.
	if en3, ws3 := call(map[string]string{}); en3 || ws3 != nil {
		t.Fatalf("empty map should be disabled/nil, got en=%v ws=%+v", en3, ws3)
	}
}

func TestNormDays(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", AllDays}, {"1111111", "1111111"}, {"0111110", "0111110"},
		{"12345", AllDays}, {"111111", AllDays}, {"1111112", AllDays},
	} {
		if got := normDays(c.in); got != c.want {
			t.Errorf("normDays(%q)=%q want %q", c.in, got, c.want)
		}
	}
}
