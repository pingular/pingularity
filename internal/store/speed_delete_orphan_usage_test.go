package store

import (
	"context"
	"testing"
	"time"
)

// Deleting a run must take its usage-accounting row with it.
//
// The two rows are one event to the user - one speedtest, which retried a
// direction - but they are two rows one second apart, and the accounting row is
// invisible: every listing read carries speedNotFailed, so it appears in no
// table, no chart, no CSV, and no API response. The ONLY thing it shows up in is
// the data-usage total.
//
// So if the delete misses it, the bytes it carries become unremovable. The run
// is gone from the history while the "Data used" figure and
// pingularity_speed_data_used_bytes keep billing for it, and there is no
// timestamp the operator could pass to the delete endpoint to clear it, because
// nothing ever tells them one exists. It only ages out with retention, and every
// subsequent deletion of a retried run adds more.
//
// This is a seam between two fixes that were each correct alone: the accounting
// row moved to measuredTS+1 so a backup's merge key would stop collapsing it
// into the measurement, and that moved it out of the reach of a delete that
// matches on ts. The row now carries the ts of the run it bills for
// (UsageRunTS), and the delete follows that - a row's own statement of what it
// belongs to, rather than an inference from where it sits.
func TestDeletingARetriedRunRemovesItsUsageRowToo(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ts := time.Now().Add(-time.Hour).Unix()
	i64 := func(v int64) *int64 { return &v }
	// The measurement: 125 MB down + 125 MB up.
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: ts, DownMbps: 940, UpMbps: 910, PingMS: 8,
		DownBytes: i64(125_000_000), UpBytes: i64(125_000_000),
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed(measurement): %v", err)
	}
	// What the abandoned first attempt spent, written one second later and
	// pointing back at the run it belongs to - exactly what the scheduler stores
	// (see speedtest.Scheduler.recordExtraUsage).
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: ts + 1, Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		Failed: true, UsageRunTS: i64(ts), DownBytes: i64(125_000_000),
	}); err != nil {
		t.Fatalf("InsertSpeed(usage): %v", err)
	}
	if used, err := s.SpeedDataUsage(ctx, time.Now()); err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	} else if used.All != 375_000_000 {
		t.Fatalf("setup: usage = %d, want 375000000", used.All)
	}

	// The operator deletes the run - the only timestamp any surface has ever
	// shown them.
	n, err := s.DeleteSpeed(ctx, ts)
	if err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteSpeed reported %d rows removed, want 1 (the measurement); the caller reads this as "+
			"\"was there a run\"", n)
	}

	runs, err := s.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("%d runs left after deleting the only one", len(runs))
	}
	used, err := s.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 0 {
		t.Errorf("the deleted run still bills %d bytes of data usage. Its accounting row sits at ts+1 and no "+
			"listing will ever show that timestamp, so the operator cannot delete it either - the figure is wrong "+
			"until retention rolls past it.", used.All)
	}
}

// The adjacency rule must not reach past the run it belongs to. A MEASUREMENT
// one second later is a different run and must survive - the delete is scoped to
// flagged rows precisely so it cannot eat a reading.
func TestDeletingARunSparesAMeasurementOneSecondLater(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ts := time.Now().Add(-time.Hour).Unix()
	i64 := func(v int64) *int64 { return &v }
	for i, at := range []int64{ts, ts + 1} {
		if err := s.InsertSpeed(ctx, SpeedSample{
			TS: at, DownMbps: float64(900 + i), UpMbps: 900, PingMS: 8,
			DownBytes: i64(1 << 20), UpBytes: i64(1 << 20),
			Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		}); err != nil {
			t.Fatalf("InsertSpeed(%d): %v", at, err)
		}
	}
	if _, err := s.DeleteSpeed(ctx, ts); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	runs, err := s.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs left, want 1: deleting one run took a real measurement a second later with it", len(runs))
	}
	if runs[0].TS != ts+1 {
		t.Errorf("the surviving run is at %d, want %d", runs[0].TS, ts+1)
	}
}
