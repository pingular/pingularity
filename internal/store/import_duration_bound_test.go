package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// eventRowSane strips a NEGATIVE duration_s, but a positive one of any magnitude
// sailed through: completedOutagesSince anchors an unpaired 'up' at ts-duration_s,
// so one crafted row claiming 1e15 seconds books an outage reaching back thirty
// million years, and every window anyone asks about is inside it. The guard was
// added to stop a single backup row rewriting every published uptime figure, and
// this is that same failure through the door it left open.
func TestImportBoundsAnAbsurdOutageDuration(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	stats.ResetForTest()

	// A window with genuine observations, so a collapsed uptime can only come
	// from the crafted row.
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-6 * 24 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-time.Hour).Unix()), "type": "up", "duration_s": float64(1e15)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	o, err := s.UptimeSince(ctx, now.Add(-7*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down > time.Hour {
		t.Errorf("a single imported row with duration_s=1e15 booked %v of downtime (ratio %.6f); "+
			"a length no history could ever hold is not a measurement and must not reach the interval maths",
			o.Down, o.Ratio())
	}
	// The repair must be COUNTED: a restore that quietly rewrote rows is how the
	// last silent uptime divergence went unnoticed.
	if got := stats.Lifetime().Counters["import.event_duration_dropped"]; got != 1 {
		t.Errorf("import.event_duration_dropped = %d, want 1: a repaired row must be visible on /metrics", got)
	}
}

// The negative strip shares the counter: both are the same repair.
func TestImportCountsANegativeDurationStripToo(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	stats.ResetForTest()

	if _, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-time.Minute).Unix()), "type": "up", "duration_s": float64(-1)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if got := stats.Lifetime().Counters["import.event_duration_dropped"]; got != 1 {
		t.Errorf("import.event_duration_dropped = %d, want 1", got)
	}
}

// A long but believable outage - a week offline - must keep its duration, or the
// bound is just a break.
func TestImportKeepsAPlausiblyLongOutageDuration(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	stats.ResetForTest()

	weekS := int64(7 * 24 * 3600)
	n, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-30 * 24 * time.Hour).Unix()), "type": "down"},
		{"ts": float64(now.Add(-23 * 24 * time.Hour).Unix()), "type": "up", "duration_s": float64(weekS)},
	})
	if err != nil || n != 2 {
		t.Fatalf("import = (%d, %v), want (2, nil)", n, err)
	}
	var dur int64
	if err := s.db.QueryRow(`SELECT duration_s FROM events WHERE type = 'up'`).Scan(&dur); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if dur != weekS {
		t.Errorf("duration_s = %d, want %d: a week-long outage is real and must survive intact", dur, weekS)
	}
	if got := stats.Lifetime().Counters["import.event_duration_dropped"]; got != 0 {
		t.Errorf("import.event_duration_dropped = %d, want 0: nothing was repaired", got)
	}
}
