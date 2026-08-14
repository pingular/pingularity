package store

import (
	"context"
	"testing"
	"time"
)

// Deleting one speedtest must not touch a DIFFERENT run's usage record.
//
// A run that retried a direction writes its usage-accounting row one second
// after the measurement, and DeleteSpeed used to find that row by guessing at
// the position: any flagged row sitting at ts+1 was assumed to belong to the run
// being deleted. Nothing in the row said so.
//
// The guess is wrong as soon as two runs land a second apart, which needs no
// exotic timing at all - a manual run that fails one second after a scheduled
// measurement finished is exactly it. A whole-run failure IS a flagged row (it
// records the bytes a failed run still spent), so deleting the scheduled run
// took the manual run's entire record with it: bytes that were really spent
// disappear from "Data used" and from
// pingularity_speed_data_used_bytes, and because no listing ever shows a flagged
// row, the operator is never told the second run existed, let alone that
// deleting the first destroyed it.
func TestDeletingOneRunKeepsAnotherRunsUsageRecord(t *testing.T) {
	ctx := context.Background()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	ts := time.Now().Add(-time.Hour).Unix()
	i64 := func(v int64) *int64 { return &v }
	// The scheduled run: a plain measurement, 125 MB each way.
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: ts, DownMbps: 940, UpMbps: 910, PingMS: 8,
		DownBytes: i64(125_000_000), UpBytes: i64(125_000_000),
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed(scheduled run): %v", err)
	}
	// A second run, triggered by hand one second later, which failed outright:
	// its record is the usage-accounting row for the 40 MB it spent before
	// giving up. A different run, with its own timestamp and its own bytes.
	if err := s.InsertSpeed(ctx, SpeedSample{
		TS: ts + 1, Server: "lab", ServerID: "1", Trigger: "manual", Engine: "iperf3",
		Failed: true, DownBytes: i64(40_000_000),
	}); err != nil {
		t.Fatalf("InsertSpeed(failed manual run): %v", err)
	}
	if used, err := s.SpeedDataUsage(ctx, time.Now()); err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	} else if used.All != 290_000_000 {
		t.Fatalf("setup: usage = %d, want 290000000", used.All)
	}

	// The operator deletes the scheduled run - the only one of the two any
	// surface has ever shown them.
	n, err := s.DeleteSpeed(ctx, ts)
	if err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteSpeed reported %d rows removed, want 1 (the measurement)", n)
	}

	var left int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE ts = ?`, ts+1).Scan(&left); err != nil {
		t.Fatalf("count rows at ts+1: %v", err)
	}
	if left != 1 {
		t.Errorf("deleting the scheduled run destroyed the record of the manual run that ran a second later "+
			"(%d rows left at its timestamp, want 1). That run's bytes were really spent and are gone from the "+
			"data-usage history for good: a failed run appears in no table, chart or CSV, so nothing ever told "+
			"the operator it existed and nothing can bring it back.", left)
	}
	used, err := s.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 40_000_000 {
		t.Errorf("after deleting the scheduled run the data-usage total reads %d bytes, want 40000000 - what the "+
			"other run spent. Deleting one speedtest silently un-billed a second one.", used.All)
	}
}

// A restored run must still take its usage row with it. The reference the delete
// follows lives in a column, so it only survives a backup if the export carries
// it: dropped, every restored accounting row would be unreachable, billing bytes
// for runs the operator can delete from the history and never stop paying for.
func TestARestoredRunStillTakesItsUsageRowWhenDeleted(t *testing.T) {
	ctx := context.Background()
	src, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()

	ts := time.Now().Add(-time.Hour).Unix()
	i64 := func(v int64) *int64 { return &v }
	if err := src.InsertSpeed(ctx, SpeedSample{
		TS: ts, DownMbps: 940, UpMbps: 910, PingMS: 8,
		DownBytes: i64(125_000_000), UpBytes: i64(125_000_000),
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed(measurement): %v", err)
	}
	if err := src.InsertSpeed(ctx, SpeedSample{
		TS: ts + 1, Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
		Failed: true, UsageRunTS: i64(ts), DownBytes: i64(125_000_000),
	}); err != nil {
		t.Fatalf("InsertSpeed(usage): %v", err)
	}

	rows, err := src.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	dst, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	if n, err := dst.ImportTable(ctx, "speed", rows); err != nil {
		t.Fatalf("ImportTable: %v", err)
	} else if n != 2 {
		t.Fatalf("imported %d of 2 speed rows", n)
	}

	if _, err := dst.DeleteSpeed(ctx, ts); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	used, err := dst.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 0 {
		t.Errorf("the restored run was deleted but still bills %d bytes: its usage row came back from the backup "+
			"without the reference that says which run it belongs to, so nothing can ever remove it - and no "+
			"listing shows that row for the operator to try.", used.All)
	}
}
