package store

import (
	"context"
	"testing"
	"time"
)

// The heatmap must not render time that predates the install as healthy.
//
// UptimeSince clamps its window forward to monitoringSince precisely so a fresh
// monitor is not credited for time it never watched. DowntimeByDay had no such
// floor: it walked every local day from `since`, and because days before the
// install have no pause rows either, span-minus-pauses came out equal to the
// whole day. That reads as "watched end to end, nothing wrong" - so it minted no
// row, and a missing row draws as a clean square. Open a 1-year heatmap on a
// two-day-old install and 363 days of green appear for a machine that was not
// running.
func TestHeatmapDoesNotClaimDaysBeforeMonitoringBegan(t *testing.T) {
	s, err := Open(t.TempDir() + "/hf.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()
	// Monitoring began two days ago: the earliest sample IS the anchor.
	began := now.Add(-2 * 24 * time.Hour)
	if err := s.InsertSamples(ctx, []Sample{{
		TS: began, Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Ask for ten days - eight of which predate the install.
	days, err := s.DowntimeByDay(ctx, now.Add(-10*24*time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	byDate := map[string]DowntimeDay{}
	for _, d := range days {
		byDate[d.Date] = d
	}

	// Every day strictly before the install day must be reported, and reported as
	// unobserved - not omitted (which renders clean) and not observed.
	for i := 3; i <= 9; i++ {
		date := now.Add(-time.Duration(i) * 24 * time.Hour).In(time.UTC).Format("2006-01-02")
		d, ok := byDate[date]
		if !ok {
			t.Errorf("%s predates monitoring but has no row, so the heatmap draws it as a clean day", date)
			continue
		}
		if d.Observed() {
			t.Errorf("%s predates monitoring but reports observed_s=%d of window_s=%d", date, d.ObservedS, d.WindowS)
		}
	}
}

// ...and the days the monitor really did watch must still read as observed, or
// the floor has simply blanked the whole heatmap.
func TestHeatmapStillReportsWatchedDaysAsObserved(t *testing.T) {
	s, err := Open(t.TempDir() + "/hf2.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()
	began := now.Add(-3 * 24 * time.Hour)
	if err := s.InsertSamples(ctx, []Sample{{
		TS: began, Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	days, err := s.DowntimeByDay(ctx, now.Add(-5*24*time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	// Yesterday was fully inside the monitored period and had no pause: it must
	// either be absent (the "nothing happened, fully watched" emission rule) or
	// present and observed. It must never be present-and-unobserved.
	yday := now.Add(-24 * time.Hour).In(time.UTC).Format("2006-01-02")
	for _, d := range days {
		if d.Date == yday && !d.Observed() {
			t.Errorf("%s was monitored end to end but reports observed_s=%d of window_s=%d",
				d.Date, d.ObservedS, d.WindowS)
		}
	}
}
