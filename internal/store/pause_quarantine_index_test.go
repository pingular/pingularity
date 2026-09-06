package store

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// pauses_quarantine is keyed on ts like every other exportable time-series
// table, and two hot paths seek it by ts: the import's per-row NOT-EXISTS dedup,
// and the future-pause repair's twin lookups between it and pauses - which run
// inside ONE write transaction at every Open and on the first write after a
// generation arms. Without an index every seek is a full scan: importing N held
// rows is O(N^2), and the repair is O(pauses x quarantine) with the single
// SQLite writer held for the whole product. Every other pooled writer waits on
// busy_timeout and then FAILS - the monitor's probe round and outage transition,
// the scheduler's speed row, the operator's settings save - and none of them
// retries. A year of five-minute checkpoints is ~52k live pauses; a board whose
// clock ran fast for a couple of weeks before NTP corrected it holds thousands
// of rows aside, so the product is reached without any crafted file.

// seedPauseTables restores P live pauses and Q held rows through the import
// door, the way a backup does - it is the import that arms the re-judgement,
// so the next write runs the repair exactly as it would after a restore. Held
// rows end in 2099: no plausible clock exonerates them, so they stay for the
// repair to walk on every Open.
func seedPauseTables(t *testing.T, s *Store, P, Q int) (importPauses, importHeld time.Duration) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	live := make([]map[string]any, 0, P)
	base := now - int64(P+10)*300
	for i := 0; i < P; i++ {
		live = append(live, map[string]any{"ts": base + int64(i)*300, "duration_s": int64(299)})
	}
	start := time.Now()
	if n, err := s.ImportTableBatch(ctx, "pauses", live, map[int64]int{}); err != nil || n != P {
		t.Fatalf("import pauses = %d, %v; want %d, nil", n, err, P)
	}
	importPauses = time.Since(start)
	held := make([]map[string]any, 0, Q)
	future := int64(4070908800) // 2099-01-01
	for i := 0; i < Q; i++ {
		held = append(held, map[string]any{"ts": future + int64(i)*300, "duration_s": int64(3600)})
	}
	start = time.Now()
	if n, err := s.ImportTableBatch(ctx, "pauses_quarantine", held, map[int64]int{}); err != nil || n != Q {
		t.Fatalf("import pauses_quarantine = %d, %v; want %d, nil", n, err, Q)
	}
	importHeld = time.Since(start)
	return importPauses, importHeld
}

// queryPlan joins every detail line of EXPLAIN QUERY PLAN for q.
func queryPlan(t *testing.T, s *Store, q string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(new(int), new(int), new(int), &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		lines = append(lines, detail)
	}
	return strings.Join(lines, " | ")
}

// A database written by a build before the index existed gets it at its next
// Open, the way every other index in the base schema reaches an existing file.
func TestOpenGivesQuarantineTheTsIndex(t *testing.T) {
	path := t.TempDir() + "/qidx.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Model the file an older build left behind: the table, no index.
	if _, err := s.db.Exec(`DROP INDEX IF EXISTS idx_pauses_quarantine_ts`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'pauses_quarantine' AND name = 'idx_pauses_quarantine_ts'`).Scan(&n); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if n != 1 {
		t.Fatal("idx_pauses_quarantine_ts missing after Open: the import dedup and the future-pause repair scan the whole quarantine per row without it")
	}
	// The two seeks that matter, in the shape the code issues them: the import's
	// per-row dedup probe, and the repair's merge - one held-twin lookup per live
	// pause, inside the transaction that holds the writer.
	for _, c := range []struct {
		name string
		q    string
		args []any
	}{
		{"import dedup", `SELECT 1 FROM pauses_quarantine WHERE ts=?`, []any{1}},
		{"repair merge", `UPDATE pauses SET duration_s = max(duration_s,
			(SELECT MAX(q.duration_s) FROM pauses_quarantine q
			 WHERE q.ts = pauses.ts AND q.ts + q.duration_s <= ?))
		WHERE EXISTS (SELECT 1 FROM pauses_quarantine q
			WHERE q.ts = pauses.ts AND q.ts + q.duration_s <= ?)`, []any{1, 1}},
	} {
		plan := queryPlan(t, s2, c.q, c.args...)
		t.Logf("%s plan: %s", c.name, plan)
		if !strings.Contains(plan, "idx_pauses_quarantine_ts") || strings.Contains(plan, "SCAN q") || strings.Contains(plan, "SCAN pauses_quarantine") {
			t.Errorf("%s plan %q: want a seek on idx_pauses_quarantine_ts, not a scan of the quarantine per row", c.name, plan)
		}
	}
}

// busyTimeout is the wait the DSN gives a pooled writer before it gives up
// (pragmaConn). Any single write transaction longer than this fails every other
// writer that arrives during it.
const busyTimeout = 5 * time.Second

// The defect itself. File-backed on purpose: a :memory: store has one
// connection, so no second writer can ever be seen waiting.
func TestQuarantineRepairDoesNotStarveOtherWriters(t *testing.T) {
	if testing.Short() {
		t.Skip("20k x 20k fixture")
	}
	s, err := Open(t.TempDir() + "/starve.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	const P, Q = 20000, 20000
	dP, dQ := seedPauseTables(t, s, P, Q)
	t.Logf("import: pauses(%d)=%v pauses_quarantine(%d)=%v", P, dP.Round(time.Millisecond), Q, dQ.Round(time.Millisecond))
	if !s.pauseRepairArmed() {
		t.Fatal("premise broken: importing held rows did not arm the re-judgement")
	}
	ctx := context.Background()
	now := time.Now()
	type result struct {
		name string
		took time.Duration
		err  error
	}
	results := make(chan result, 5)
	var wg sync.WaitGroup
	run := func(name string, f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			err := f()
			results <- result{name, time.Since(start), err}
		}()
	}
	// The probe round after a restore: the first write runs the armed repair.
	run("InsertSamples (ran the armed repair)", func() error {
		return s.InsertSamples(ctx, []Sample{{TS: now, Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true}})
	})
	// Let it take the writer first, so the four below arrive while the repair
	// holds it - the scheduler, the monitor's DNS sample and outage transition,
	// and an operator saving settings, all during one probe round.
	time.Sleep(500 * time.Millisecond)
	run("InsertSpeed", func() error {
		return s.InsertSpeed(ctx, SpeedSample{TS: now.Unix(), DownMbps: 100, UpMbps: 10, PingMS: 5, Server: "srv"})
	})
	run("InsertEvent", func() error { return s.InsertEvent(ctx, now, "down", 0, "probe") })
	run("InsertDNS", func() error { return s.InsertDNS(ctx, now, 12.5, true) })
	run("SetSettings", func() error { return s.SetSettings(ctx, map[string]string{"speed_interval_min": "60"}) })
	wg.Wait()
	close(results)
	for r := range results {
		t.Logf("%-38s %-10v err=%v", r.name, r.took.Round(time.Millisecond), r.err)
		if r.err != nil {
			t.Errorf("%s failed while the pause repair held the writer: %v - the probe round, the outage "+
				"transition and the speed row are logged and dropped, never retried", r.name, r.err)
		}
		if strings.HasPrefix(r.name, "InsertSamples") && r.took >= busyTimeout {
			t.Errorf("the write carrying the repair held the writer for %v, at or past busy_timeout (%v): "+
				"every other writer that arrives during it gives up", r.took, busyTimeout)
		}
	}
	if s.pauseRepairArmed() {
		t.Errorf("repair still armed after the probe round: the judgement did not run")
	}
}

// Importing N held rows must cost N seeks, not N scans, and the repair that the
// import arms must cost a seek per row, not the product of the two tables. The
// budgets are blunt on purpose: the failure guarded is seconds against
// milliseconds, not a few percent.
func TestQuarantineImportAndRepairAreNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("20k x 20k fixture")
	}
	if raceEnabled {
		t.Skip("wall-clock budgets are not meaningful under the race detector")
	}
	s, err := Open(t.TempDir() + "/cost.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	const P, Q = 20000, 20000
	dP, dQ := seedPauseTables(t, s, P, Q)
	t.Logf("import: pauses(%d)=%v pauses_quarantine(%d)=%v", P, dP.Round(time.Millisecond), Q, dQ.Round(time.Millisecond))
	if dQ > 2*time.Second {
		t.Errorf("importing %d held rows took %v; the same rows into the indexed pauses table took %v - "+
			"the per-row dedup probe is scanning the whole quarantine", Q, dQ, dP)
	}
	start := time.Now()
	if err := repairFutureReachingPausesAt(s.db, time.Now().Unix()); err != nil {
		t.Fatalf("repair: %v", err)
	}
	took := time.Since(start)
	t.Logf("repairFutureReachingPausesAt over %d pauses x %d held: %v", P, Q, took.Round(time.Millisecond))
	if took > time.Second {
		t.Errorf("the future-pause repair took %v over %d x %d rows; it runs inside one write transaction at "+
			"every Open and on the first write after a restore, and past busy_timeout (%v) it fails every other writer", took, P, Q, busyTimeout)
	}
	// The walk changed nothing: held rows ending in 2099 are never exonerated, and
	// no live row reaches the horizon.
	var np, nq int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM pauses), (SELECT COUNT(*) FROM pauses_quarantine)`).Scan(&np, &nq); err != nil {
		t.Fatalf("count: %v", err)
	}
	if np != P || nq != Q {
		t.Errorf("after the repair pauses=%d quarantine=%d, want %d and %d unchanged", np, nq, P, Q)
	}
}
