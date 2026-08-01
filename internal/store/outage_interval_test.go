package store

import (
	"context"
	"testing"
	"time"
)

// An outage occupies ONE stretch of clock time, and every surface that reports it
// has to agree about which stretch that is. They did not.
//
// UptimeSince modelled a completed outage as a contiguous [down, down+duration_s),
// where duration_s is the OBSERVED length - so a pause inside the outage pulled
// the modelled end earlier than the real recovery. DowntimeByDay used the real
// [down, up) wall interval, subtracted the pause, and capped at duration_s.
//
// Both report the same TOTAL for a window that contains the whole outage, which
// is why this stayed hidden: the contiguous model sums to duration_s too. It only
// separates when a window boundary lands inside the outage's real wall span -
// which is every "last 6h" pill, every heatmap day boundary, and every digest
// period.
//
//	down at T, pause [T+30, T+90), up at T+120, duration_s = 60
//	  real observed downtime: [T,T+30) + [T+90,T+120)   = 30 + 30
//	  old uptime model:       [T, T+60)                 = 60 contiguous
//	A window starting at T+90 contains 30s of real downtime and 0s of the model's.
func outageFixture(t *testing.T, now time.Time) (*Store, time.Time) {
	t.Helper()
	s, err := Open(t.TempDir() + "/o.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()

	// T is 6h ago so every window under test sits comfortably inside retention.
	base := now.Add(-6 * time.Hour).Truncate(time.Second)
	// A sample before the outage anchors monitoringSince, or UptimeSince clamps
	// the window forward to the first event and the test measures nothing.
	if err := s.InsertSamples(ctx, []Sample{{
		TS: base.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("seed sample: %v", err)
	}
	if err := s.InsertEvent(ctx, base, "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := s.InsertPause(ctx, base.Add(30*time.Second), 60); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// duration_s is the observed length: 30s before the pause + 30s after.
	if err := s.InsertEvent(ctx, base.Add(120*time.Second), "up", 60, ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	return s, base
}

// The whole-outage window is the case that always agreed; keep it, so a fix that
// merely moves the disagreement somewhere else is caught.
func TestUptimeCountsTheWholeOutageOnce(t *testing.T) {
	now := time.Now()
	s, base := outageFixture(t, now)
	o, err := s.UptimeSince(context.Background(), base.Add(-time.Minute), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != 60*time.Second {
		t.Errorf("downtime over the whole outage = %ds, want 60 (its observed length)", o.Down/time.Second)
	}
}

// The window that starts INSIDE the outage, after the pause. The real link was
// down for its final 30 seconds; the contiguous model had already ended.
func TestUptimeSeesDowntimeAfterAPauseInsideTheOutage(t *testing.T) {
	now := time.Now()
	s, base := outageFixture(t, now)
	o, err := s.UptimeSince(context.Background(), base.Add(90*time.Second), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != 30*time.Second {
		t.Errorf("downtime over [pause end, now) = %ds, want 30: the outage really ran to T+120, "+
			"but the contiguous [down, down+duration_s) model ends it at T+60", o.Down/time.Second)
	}
}

// And the mirror: a window ending inside the outage must see only the part of it
// that had already happened.
func TestUptimeSeesOnlyThePrePausePartOfTheOutage(t *testing.T) {
	now := time.Now()
	s, base := outageFixture(t, now)
	// [T, T+30) is the observed downtime before the pause opens.
	o, err := s.UptimeSince(context.Background(), base, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != 60*time.Second {
		t.Errorf("downtime from the outage start = %ds, want 60", o.Down/time.Second)
	}
}

// The two surfaces must place the outage in the same seconds, not merely total the
// same. Compare the windowed uptime figure against the heatmap's own day totals.
func TestUptimeAndHeatmapPlaceTheOutageInTheSameSeconds(t *testing.T) {
	now := time.Now()
	s, base := outageFixture(t, now)
	ctx := context.Background()

	days, err := s.DowntimeByDay(ctx, base.Add(-time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	var hm int
	for _, d := range days {
		hm += d.DowntimeS
	}
	o, err := s.UptimeSince(ctx, base.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if time.Duration(hm)*time.Second != o.Down {
		t.Errorf("heatmap totals %ds of downtime, uptime says %ds - the same outage, two answers", hm, o.Down/time.Second)
	}
	if o.Down != 60*time.Second {
		t.Errorf("both agree on %ds, but the observed outage length is 60s", o.Down/time.Second)
	}
}
