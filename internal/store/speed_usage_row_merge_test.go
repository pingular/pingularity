package store

import (
	"context"
	"testing"
	"time"
)

// A run that retried a direction writes TWO speed rows in the same second: the
// measurement, and a usage-only row carrying what the failed attempt spent. They
// are different KINDS of record that merely share a timestamp.
//
// With a merge key of ts alone they collided on import, and the first-wins guard
// kept whichever was exported first - which was the usage row, so restoring a
// backup silently threw away the MEASUREMENT and halved the data-usage figure it
// was supposed to preserve. A backup that loses the reading is worse than no
// backup, because it looks like it worked.
func TestRetriedRunSurvivesABackupRoundTrip(t *testing.T) {
	ctx := context.Background()
	src, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()

	ts := time.Now().Add(-time.Hour).Unix()
	i64 := func(v int64) *int64 { return &v }
	// The measurement.
	if err := src.InsertSpeed(ctx, SpeedSample{
		TS: ts, DownMbps: 940, UpMbps: 910, PingMS: 8,
		DownBytes: i64(125_000_000), UpBytes: i64(125_000_000),
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed(measurement): %v", err)
	}
	// The retried attempt's spend: the scheduler writes it one second later so the
	// two rows never share the backup merge key.
	if err := src.InsertSpeed(ctx, SpeedSample{
		TS: ts + 1, Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		Failed: true, DownBytes: i64(125_000_000),
	}); err != nil {
		t.Fatalf("InsertSpeed(usage): %v", err)
	}

	rows, err := src.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("exported %d speed rows, want 2 (the measurement and its usage row)", len(rows))
	}

	dst, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	n, err := dst.ImportTable(ctx, "speed", rows)
	if err != nil {
		t.Fatalf("ImportTable: %v", err)
	}
	if n != 2 {
		t.Fatalf("imported %d of 2 rows: the two kinds of record collided on their shared timestamp", n)
	}

	// The measurement must be readable as a run...
	runs, err := dst.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("restored %d measurements, want 1: the reading itself was dropped by the restore", len(runs))
	}
	if runs[0].DownMbps != 940 {
		t.Fatalf("restored measurement reads %.0f Mbps, want 940", runs[0].DownMbps)
	}
	// ...and the usage must be whole: both halves of what the run spent.
	used, err := dst.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if want := int64(375_000_000); used.All != want {
		t.Fatalf("restored data usage = %d, want %d: a restore must not lose what the run cost", used.All, want)
	}
	// Re-importing the same backup must stay idempotent.
	again, err := dst.ImportTable(ctx, "speed", rows)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if again != 0 {
		t.Fatalf("re-importing the same backup inserted %d more rows; the merge key no longer dedupes", again)
	}
}
