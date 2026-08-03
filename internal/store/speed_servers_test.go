package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func seedRun(t *testing.T, st *Store, ts int64) {
	t.Helper()
	ctx := context.Background()
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts, DownMbps: 100, UpMbps: 100, PingMS: 10, Server: "S1, N1", ServerID: "1"}); err != nil {
		t.Fatalf("insert speed: %v", err)
	}
	if err := st.InsertSpeedServers(ctx, []SpeedServerRow{
		{RunTS: ts, ServerID: "1", Server: "S1, N1", RankOrder: 1, Selected: true, Measured: true, Winner: true, WinReason: "score"},
		{RunTS: ts, ServerID: "2", Server: "S2, N2", RankOrder: 2, Selected: true, Measured: true},
	}); err != nil {
		t.Fatalf("insert speed_servers: %v", err)
	}
}

func serverRowCount(t *testing.T, st *Store) int64 {
	t.Helper()
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("table counts: %v", err)
	}
	return cnt["speed_servers"]
}

// The selection report has no foreign key (nothing here does); every deletion
// path must cascade by hand, or explanations outlive their runs. DeleteSpeed
// is a single run, Clear("speed") is the operator wipe, and both must take the
// report rows with them.
func TestSelectionRowsFollowTheirRunThroughDeleteAndClear(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	seedRun(t, st, 1000)
	seedRun(t, st, 2000)
	if n := serverRowCount(t, st); n != 4 {
		t.Fatalf("seeded rows = %d, want 4", n)
	}
	if _, err := st.DeleteSpeed(ctx, 1000); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := serverRowCount(t, st); n != 2 {
		t.Errorf("after DeleteSpeed(1000): rows = %d, want 2 - a deleted run left its report behind", n)
	}
	if rows, _ := st.SpeedServers(ctx, 2000); len(rows) != 2 {
		t.Errorf("the surviving run lost its report: %d rows", len(rows))
	}
	if _, err := st.Clear(ctx, "speed"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n := serverRowCount(t, st); n != 0 {
		t.Errorf("after Clear(speed): rows = %d, want 0 - 'clear speed data' must mean all of it", n)
	}
}

// Prune must apply BOTH arms of the speed cut to the report rows - the
// retention cutoff and the future horizon - keyed on run_ts, or old and
// future-dated reports accumulate forever under an index nothing reads.
func TestSelectionRowsRideTheSpeedRetention(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()

	old := now.Add(-100 * time.Hour).Unix()
	fresh := now.Add(-1 * time.Hour).Unix()
	future := now.Add(1000 * time.Hour).Unix()
	for _, ts := range []int64{old, fresh, future} {
		seedRun(t, st, ts)
	}
	// Speed cutoff at -50h: the -100h run and the far-future run must go, the
	// -1h run must stay. Sample/event cutoffs far in the past so only speed
	// (and its report) is exercised.
	if _, err := st.Prune(ctx, now.Add(-9999*time.Hour), now.Add(-50*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rows, _ := st.SpeedServers(ctx, fresh); len(rows) != 2 {
		t.Errorf("in-retention run lost its report: %d rows", len(rows))
	}
	for name, ts := range map[string]int64{"old": old, "future": future} {
		if rows, _ := st.SpeedServers(ctx, ts); len(rows) != 0 {
			t.Errorf("%s run's report survived the prune: %d rows", name, len(rows))
		}
	}
}

// applySchema runs on every Open - twice on the corruption-recovery path - so
// the new DDL must be as idempotent as the rest of the schema const.
func TestSchemaWithSelectionTableIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	seedRun(t, st, 1000)
	st.Close()
	st, err = Open(path)
	if err != nil {
		t.Fatalf("second open on the same file: %v", err)
	}
	defer st.Close()
	if n := serverRowCount(t, st); n != 2 {
		t.Errorf("rows after reopen = %d, want 2", n)
	}
}

// A backup is hostile input (security model): a crafted or truncated row can
// carry NULL in any nullable TEXT column, and one such row must not wedge the
// whole run's report read - the exact Scan-error class that once broke every
// speed read (see the speed exportTables notNull comment).
func TestSelectionReadSurvivesImportedNullTextColumns(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	seedRun(t, st, 3000)
	// Bypass the import guards the way a pre-guard backup already on disk would:
	// raw insert with every guarded TEXT column NULL.
	if _, err := st.DB().Exec(`INSERT INTO speed_servers (run_ts, selected, measured, winner) VALUES (3000, 1, 0, 0)`); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	rows, err := st.SpeedServers(ctx, 3000)
	if err != nil {
		t.Fatalf("one NULL-text row wedged the whole report: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("rows = %d, want 3 (the NULL row reads as empty strings, not an error)", len(rows))
	}
}

// The import flood cap keys on the time column; speed_servers is the one keyed
// table whose column is run_ts, not ts - without the fallback a crafted backup
// packs unbounded rows at one run_ts and the cap silently never engages.
func TestSelectionImportHonoursThePerTimestampCap(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	rows := make([]map[string]any, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, map[string]any{
			"run_ts": float64(4000), "server_id": fmt.Sprintf("s%d", i), "server": "S, N",
			"selected": float64(1), "measured": float64(0), "winner": float64(0),
		})
	}
	n, err := st.ImportTableBatch(context.Background(), "speed_servers", rows, map[int64]int{})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n > 256 {
		t.Errorf("imported %d rows at one run_ts; the flood cap (256) never engaged", n)
	}
}
