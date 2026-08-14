package store

import (
	"context"
	"testing"
	"time"
)

// seedRun writes a measurement, and seedUsage writes the accounting row that
// bills for it. Raw SQL on purpose: several of these shapes are ones the daemon
// never produces and only a crafted or corrupted backup could deliver, which is
// exactly what the guards below exist for.
func seedMeasuredRun(t *testing.T, s *Store, ts int64, down, up int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO speed (ts, server, down_mbps, up_mbps, ping_ms, download_bytes, upload_bytes, run_trigger, engine)
		VALUES (?, 'lab', 900, 900, 8, ?, ?, 'scheduled', 'iperf3')`, ts, down, up); err != nil {
		t.Fatalf("seed run %d: %v", ts, err)
	}
}

func seedUsageRow(t *testing.T, s *Store, ts, runTS, down int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO speed (ts, server, download_bytes, run_trigger, engine, failed, usage_run_ts)
		VALUES (?, 'lab', ?, 'manual', 'iperf3', 1, ?)`, ts, down, runTS); err != nil {
		t.Fatalf("seed usage %d: %v", ts, err)
	}
}

// Deleting a run must not take the NEIGHBOUR's usage with it.
//
// The accounting row lands one second after the run it bills for, so the row
// sitting at any given second may belong to the run before it. Deleting by
// timestamp alone therefore reaches across into another run's record - the same
// destruction the positional sweep used to cause, arriving from the other
// direction. A manual run that fails one second after a scheduled one finished is
// all it takes.
func TestDeletingARunSparesTheNeighboursUsageRowAtTheSameSecond(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	base := time.Now().Add(-time.Hour).Unix()

	seedMeasuredRun(t, s, base, 1_000_000, 2_000_000)   // the earlier run...
	seedUsageRow(t, s, base+1, base, 500_000)           // ...and its usage, one second later
	seedMeasuredRun(t, s, base+1, 3_000_000, 4_000_000) // a different run, at that same second

	if _, err := s.DeleteSpeed(ctx, base+1); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	used, err := s.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	// The earlier run (3 MB) and its usage row (0.5 MB) must both survive; only
	// the deleted run's 7 MB goes.
	if want := int64(3_500_000); used.All != want {
		t.Errorf("data used = %d, want %d: deleting one run destroyed the previous run's usage record, "+
			"which no screen lists and nothing can restore", used.All, want)
	}
}

// The cascade is scoped to accounting rows as well as to the reference, and that
// belt matters: the daemon only ever puts usage_run_ts on an accounting row, but
// a crafted backup can put one on a real measurement. Destroying a reading is the
// worst outcome available here, so the reference alone must not be enough.
func TestTheCascadeWillNotDeleteAMeasurementThatNamesTheRun(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	base := time.Now().Add(-time.Hour).Unix()

	seedMeasuredRun(t, s, base, 1_000_000, 2_000_000)
	// A MEASUREMENT (not flagged) that points at the run being deleted.
	if _, err := s.db.Exec(`INSERT INTO speed (ts, server, down_mbps, up_mbps, ping_ms, download_bytes, upload_bytes, run_trigger, engine, usage_run_ts)
		VALUES (?, 'lab', 940, 910, 8, 5000, 6000, 'scheduled', 'iperf3', ?)`, base+5, base); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.DeleteSpeed(ctx, base); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	runs, err := s.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].TS != base+5 {
		t.Errorf("after deleting one run the history holds %d run(s); a real measurement was destroyed "+
			"because it carried a reference the daemon would never have written on it", len(runs))
	}
}

// A backup has to carry the reference, or a restore silently unlinks every
// accounting row from its run and the usage becomes undeletable again. The export
// only carries a post-schema-4 column when some row uses it, so the in-use check
// has to count this column too - not just the marker beside it.
func TestABackupKnowsTheReferenceIsInUse(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	base := time.Now().Add(-time.Hour).Unix()
	seedMeasuredRun(t, s, base, 1_000_000, 2_000_000)
	seedUsageRow(t, s, base+1, base, 500_000)

	tx, err := s.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer tx.Rollback()
	inUse, err := s.SpeedColumnsPastSchema4InUse(ctx, tx)
	if err != nil {
		t.Fatalf("SpeedColumnsPastSchema4InUse: %v", err)
	}
	if !inUse["usage_run_ts"] {
		t.Error("the reference is reported as unused while a row carries one, so the backup would drop it: " +
			"every restored accounting row comes back orphaned, and deleting its run no longer removes it")
	}
}

// The reference has to arrive as a number. A text value matches no
// `usage_run_ts = ?` comparison, so the row it lands on is an accounting row
// nothing can ever reach - the exact stranding the reference was added to end.
func TestATextReferenceIsRefusedAtTheImportDoor(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	base := time.Now().Add(-time.Hour).Unix()

	n, err := s.ImportTable(ctx, "speed", []map[string]any{{
		"ts": base + 1, "server": "lab", "download_bytes": int64(500_000),
		"run_trigger": "manual", "engine": "iperf3", "failed": int64(1),
		"usage_run_ts": "not-a-number",
	}})
	if err != nil {
		t.Fatalf("ImportTable: %v", err)
	}
	var stored int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE typeof(usage_run_ts) = 'text'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 0 {
		t.Errorf("a text reference was stored on %d row(s) (import applied %d): no delete can ever match it, "+
			"so that row bills bytes for a run it can no longer be attached to", stored, n)
	}
}
