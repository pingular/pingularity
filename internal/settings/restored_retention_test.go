package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A RESTORE NEVER SHORTENS A RETENTION WINDOW.
//
// A backup carries its install's retention with the rest of its settings. A
// shorter one used to go live at the reload that ends a restore, and the next
// cleanup then deleted the destination's own history down to it. The restore
// now asks RestoredRetention of each retention row before it lands, and leaves
// out one that keeps history for less time than the install does. These tests
// pin how a row is read (the way the reload will) and what counts as shorter.

const day = 24 * time.Hour

// retentionController is a controller on the shipped defaults - 30 days of
// latency, a year of speed and outages - with the given windows in force, so a
// row the reload cannot read falls back to a default shorter than forever, as
// it does in the daemon.
func retentionController(t *testing.T, latency, speed, downtime time.Duration) *Controller {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	c, err := New(context.Background(), st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2,
		Retention: 30 * day, SpeedRetention: 365 * day, DowntimeRetention: 365 * day,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if _, err := c.Update(context.Background(), Patch{Retention: &latency, SpeedRetention: &speed, DowntimeRetention: &downtime}); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	return c
}

// On an install that keeps everything, every window a backup can ask for is
// shorter, except keep forever itself - including the ones it asks for by
// accident: a value the reload cannot read falls back to the default, and one
// past the ceiling is clamped to it, so each is weighed as that.
func TestRestoredRetentionReadsTheRowTheWayTheReloadWill(t *testing.T) {
	c := retentionController(t, 0, 0, 0)
	for _, tc := range []struct {
		key, text string
		asked     time.Duration
		keeps     bool
	}{
		{"retention_s", "3600", time.Hour, false},
		{"retention_s", "0", 0, true},
		{"speed_retention_s", "86400", day, false},
		{"downtime_retention_s", "604800", 7 * day, false},
		{"retention_s", "soon", 30 * day, false},
		{"retention_s", "-5", 30 * day, false},
		{"retention_s", "3600.5", 30 * day, false},
		{"retention_s", "", 30 * day, false},
		{"speed_retention_s", "garbage", 365 * day, false},
		{"downtime_retention_s", "999999999999", MaxDuration, false},
	} {
		asked, here, keeps := c.RestoredRetention(tc.key, tc.text)
		if asked != tc.asked || here != 0 || keeps != tc.keeps {
			t.Errorf("%s=%q: asked %v, here %v, keeps %v; want %v, 0 (forever), %v",
				tc.key, tc.text, asked, here, keeps, tc.asked, tc.keeps)
		}
	}
}

// A window at least as long as the one in force lands, keep forever included;
// only a shorter one is refused.
func TestRestoredRetentionLetsALongerOrEqualWindowLand(t *testing.T) {
	c := retentionController(t, time.Hour, 30*day, 365*day)
	for _, tc := range []struct {
		key, text string
		here      time.Duration
		keeps     bool
	}{
		{"retention_s", "7200", time.Hour, true},
		{"retention_s", "3600", time.Hour, true},
		{"retention_s", "1800", time.Hour, false},
		{"retention_s", "0", time.Hour, true},
		{"speed_retention_s", "2592000", 30 * day, true},
		{"speed_retention_s", "2591999", 30 * day, false},
		{"downtime_retention_s", "3600", 365 * day, false},
		{"downtime_retention_s", "0", 365 * day, true},
	} {
		_, here, keeps := c.RestoredRetention(tc.key, tc.text)
		if here != tc.here || keeps != tc.keeps {
			t.Errorf("%s=%q: here %v, keeps %v; want %v, %v", tc.key, tc.text, here, keeps, tc.here, tc.keeps)
		}
	}
}

// Only the three windows are weighed; every other setting imports as before.
func TestRestoredRetentionLeavesEveryOtherKeyAlone(t *testing.T) {
	c := retentionController(t, 0, 0, 0)
	for key, cat := range map[string]string{
		"retention_s": "latency", "speed_retention_s": "speed", "downtime_retention_s": "downtime",
		"latency_interval_s": "", "save_every_s": "", "": "",
	} {
		if got := RetentionCategory(key); got != cat {
			t.Errorf("RetentionCategory(%q) = %q, want %q", key, got, cat)
		}
		if cat != "" {
			continue
		}
		if _, _, keeps := c.RestoredRetention(key, "1"); !keeps {
			t.Errorf("RestoredRetention refused %q, which is not a retention window", key)
		}
	}
}

// 0 keeps history forever: nothing is longer, and it is longer than anything.
func TestKeepsAtLeastCountsForeverAsTheLongest(t *testing.T) {
	for _, tc := range []struct {
		a, b time.Duration
		want bool
	}{
		{0, 0, true},
		{0, time.Hour, true},
		{time.Hour, 0, false},
		{MaxDuration, 0, false},
		{2 * time.Hour, time.Hour, true},
		{time.Hour, time.Hour, true},
		{time.Hour, 2 * time.Hour, false},
	} {
		if got := KeepsAtLeast(tc.a, tc.b); got != tc.want {
			t.Errorf("KeepsAtLeast(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// The cutoff for keep forever is the epoch, before every row; any other window
// cuts that far back from now.
func TestPruneCutoffKeepsForeverAtTheEpoch(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if got := PruneCutoff(now, 0); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("PruneCutoff(now, 0) = %v, want the epoch", got)
	}
	if got := PruneCutoff(now, -time.Hour); !got.Equal(time.Unix(0, 0)) {
		t.Errorf("PruneCutoff(now, -1h) = %v, want the epoch", got)
	}
	if got := PruneCutoff(now, 30*day); !got.Equal(now.Add(-30 * day)) {
		t.Errorf("PruneCutoff(now, 30d) = %v, want %v", got, now.Add(-30*day))
	}
}
