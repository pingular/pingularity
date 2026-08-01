package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The intCols allowlist stops a TEXT value landing in an INTEGER-affinity column
// at the import door, but it arrived with no at-rest migration - so residue an
// older build already persisted stays forever. SQLite does not enforce column
// types, so `healthy` holding 'yes' makes every read that scans it into a Go
// int64 fail ("converting driver.Value type string" ...), which is not one bad
// row degrading gracefully: LatestSpeed feeds the speed pills and
// LatestPerTarget the status table, and both are permanently broken by one row.
// Re-importing a corrected backup does not heal it either - the merge is by key,
// so the row is updated in place only for the columns the file carries.
//
// The repair mirrors repairInsaneEventDurations rather than deleting outright:
// strip the unreadable FIELD where the column is nullable (the run, the sample,
// the transition is the row's primary content), and delete only where the
// integer column is NOT NULL and there is no value left to keep the row with.

// seedRaw runs a statement the way a pre-allowlist build persisted a row:
// straight SQL, no validation.
func seedRaw(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatalf("seed %q: %v", q, err)
	}
}

func TestOpenRepairsTextResidueInIntegerColumns(t *testing.T) {
	path := t.TempDir() + "/residue.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now().Unix()
	// The verifier's recipe: TEXT in every INTEGER-affinity column the import
	// allowlist guards but nothing repairs.
	seedRaw(t, s, `INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, healthy, download_bytes, upload_bytes)
		VALUES (?, 100, 20, 12, 'srv', 'yes', 'lots', 'lots')`, now-600)
	seedRaw(t, s, `INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, '1.1.1.1', 10, 'yes', 'ipv4')`, now-300)
	seedRaw(t, s, `INSERT INTO dns (ts, latency_ms, success) VALUES (?, 8, 'yes')`, now-300)
	// The residue is what a PRE-gate build left, and such a build never stamped the
	// once-only repair generation - so clear the marker Open just set, or the gate
	// (see intColumnRepairGen) would skip the scan this test exists to exercise.
	seedRaw(t, s, `PRAGMA user_version = 0`)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// The reads the residue wedges. Each one is a whole panel.
	sp, err := s2.LatestSpeed(ctx)
	if err != nil {
		t.Fatalf("LatestSpeed after reopen: %v (one row of legacy TEXT residue must not "+
			"break the speed panel for good)", err)
	}
	if sp == nil {
		t.Fatalf("LatestSpeed returned no run: the speedtest itself is real and must survive")
	}
	if sp.DownMbps != 100 {
		t.Errorf("kept run reports %.0f Mbps down, want 100: only the unreadable fields go", sp.DownMbps)
	}
	if _, err := s2.LatestPerTarget(ctx, time.Hour); err != nil {
		t.Fatalf("LatestPerTarget after reopen: %v (the status table must not stay broken)", err)
	}

	// Nullable columns keep their row and lose the field; a NOT NULL integer column
	// has nothing left to keep the row with, so the row goes.
	var speedRows, strippedHealthy int
	if err := s2.db.QueryRow(`SELECT COUNT(*), SUM(healthy IS NULL) FROM speed`).Scan(&speedRows, &strippedHealthy); err != nil {
		t.Fatalf("count speed: %v", err)
	}
	if speedRows != 1 || strippedHealthy != 1 {
		t.Errorf("speed rows=%d with %d stripped health flags, want 1 and 1: the run is real, "+
			"only the unreadable flag is not", speedRows, strippedHealthy)
	}
	var samples, dnsRows int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&samples); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&dnsRows); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if samples != 0 || dnsRows != 0 {
		t.Errorf("samples=%d dns=%d after reopen, want 0 and 0: success is NOT NULL, so a row whose "+
			"success is unreadable has no meaning left to keep", samples, dnsRows)
	}
	// Counted like the sibling repairs.
	if got := stats.Lifetime().Counters["db.int_columns_repaired"]; got != 3 {
		t.Errorf("db.int_columns_repaired = %d, want 3 (one speed row, one sample, one dns row): "+
			"the repair must be visible on /metrics", got)
	}
}

// Whole numbers already in the column are data, whatever an older build stored
// them as, and a NULL is the real shape of an unmeasured field. Neither may be
// touched, or the repair is itself the data loss.
func TestOpenIntColumnRepairKeepsReadableValues(t *testing.T) {
	path := t.TempDir() + "/readable.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	seedRaw(t, s, `INSERT INTO speed (ts, server, healthy, download_bytes, upload_bytes) VALUES (?, 'srv', 1, 4096, NULL)`, now-900)
	seedRaw(t, s, `INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, '1.1.1.1', 10, 1, 'ipv4')`, now-300)
	seedRaw(t, s, `INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, '1.1.1.1', NULL, 0, 'ipv4')`, now-240)
	seedRaw(t, s, `INSERT INTO dns (ts, latency_ms, success) VALUES (?, 8, 1)`, now-300)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var healthy, down int64
	var up any
	if err := s2.db.QueryRow(`SELECT healthy, download_bytes, upload_bytes FROM speed`).Scan(&healthy, &down, &up); err != nil {
		t.Fatalf("read speed: %v", err)
	}
	if healthy != 1 || down != 4096 || up != nil {
		t.Errorf("speed row came back healthy=%d download_bytes=%d upload_bytes=%v; readable values and "+
			"an honest NULL must survive untouched", healthy, down, up)
	}
	var samples, dnsRows int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&samples); err != nil {
		t.Fatalf("count samples: %v", err)
	}
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&dnsRows); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if samples != 2 || dnsRows != 1 {
		t.Errorf("samples=%d dns=%d, want 2 and 1: a failed probe (success=0) is data too", samples, dnsRows)
	}
	if got := stats.Lifetime().Counters["db.int_columns_repaired"]; got != 0 {
		t.Errorf("db.int_columns_repaired = %d on a clean database, want 0", got)
	}
}
