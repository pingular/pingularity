package store

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"
)

// backupTables is every table a full backup carries, in a stable order: the
// export whitelist itself, not a list copied into the test. That is deliberate -
// a table missing from the whitelist is missing from these round trips too, so
// the failure surfaces as the restored box disagreeing with its source about
// uptime (what the operator actually sees) instead of as an unknown-table error.
func backupTables() []string {
	out := make([]string, 0, len(exportTables))
	for t := range exportTables {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// restoreBackup copies src into dst the way export + import do: stream every
// whitelisted table out, merge each back in by its key columns. Returns the rows
// applied per table.
func restoreBackup(t *testing.T, src, dst *Store) map[string]int {
	t.Helper()
	ctx := context.Background()
	applied := map[string]int{}
	for _, table := range backupTables() {
		rows, err := src.ExportTable(ctx, table)
		if err != nil {
			t.Fatalf("export %s: %v", table, err)
		}
		n, err := dst.ImportTable(ctx, table, rows)
		if err != nil {
			t.Fatalf("import %s: %v", table, err)
		}
		applied[table] = n
	}
	return applied
}

// pauseRowCount is the number of recorded pause spans (TableCounts doesn't track
// the table).
func pauseRowCount(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count pauses: %v", err)
	}
	return n
}

// A backup must carry the uptime DENOMINATOR, not just the numerator. `events`
// records how many seconds were observed DOWN; `pauses` records which wall
// seconds were observed at all (pausedOverlap feeds UptimeSince's denominator,
// DowntimeByDay's prorate and orphanGapDowntime). Restoring events without pauses
// silently reclassifies every unobserved second as observed-and-up, so the
// restored box reports a different uptime than the machine the file came from -
// on the same data, with nothing to hint at the loss.
//
// The fixture is the ordinary case, not a contrived one: any operator whose
// monitor was stopped for a while has such a row (Monitor.Run books the whole
// process-down gap as one pause span on every restart past startupGapMin).
// Asserted as agreement between source and restore across all three consumers -
// the pill, the heatmap and the digest - rather than as arithmetic.
func TestBackupRestoresTheUptimeDenominator(t *testing.T) {
	now := time.Now()
	const day = 24 * 3600
	src := open(t)
	sampleAt(t, src, now, 7*day, "cf", "ipv4", true) // monitoring anchor: the window start
	pauseAt(t, src, now, 6*day, 5*day)               // the box was off [now-6d, now-1d)
	eventAt(t, src, now, 12*3600, "down", -1)        // a real outage, fully observed
	eventAt(t, src, now, 11*3600, "up", 3600)

	since := now.Add(-7 * day * time.Second)
	before := readAll(t, src, since) // also persists first_seen_ts, so the backup carries the anchor

	// The source sees 5 of the 7 days as unobserved: 1h down out of 2 days observed.
	if math.Abs(before.coverage-2.0/7.0) > 1e-3 {
		t.Fatalf("fixture: source coverage = %.6f, want ~%.6f (5 of 7 days unobserved)",
			before.coverage, 2.0/7.0)
	}

	dst := open(t)
	applied := restoreBackup(t, src, dst)
	// Not fatal: the readings below are the point, and they say what the operator
	// would actually see.
	if applied["pauses"] != 1 {
		t.Errorf("restore applied %d pause rows, want 1: without them the restored box "+
			"counts every unobserved second as observed-and-up", applied["pauses"])
	}

	after := readAll(t, dst, since)
	if math.Abs(after.ratio-before.ratio) > 1e-4 {
		t.Errorf("uptime%% changed across backup/restore: %.6f -> %.6f; the restored box "+
			"divides the same downtime by a denominator that includes unobserved time",
			before.ratio, after.ratio)
	}
	if math.Abs(after.coverage-before.coverage) > 1e-4 {
		t.Errorf("observation coverage changed across backup/restore: %.6f -> %.6f; a restore "+
			"that reports 1.0 defeats the Observation.Defined guard that keeps /metrics from publishing "+
			"an uptime ratio for a window that observed nothing", before.coverage, after.coverage)
	}
	if after.hmDownS != before.hmDownS || after.hmOut != before.hmOut {
		t.Errorf("heatmap changed across backup/restore: %ds/%d outages -> %ds/%d outages",
			before.hmDownS, before.hmOut, after.hmDownS, after.hmOut)
	}
	if after.dgDownS != before.dgDownS || after.dgOut != before.dgOut {
		t.Errorf("digest changed across backup/restore: %ds/%d outages -> %ds/%d outages",
			before.dgDownS, before.dgOut, after.dgDownS, after.dgOut)
	}

	// Restoring the same file twice must not double the denominator's subtrahend:
	// pause rows merge by ts, like every other time-series table. This is also why
	// Prune keeps a straddling span whole instead of splitting it - a split rewrites
	// ts, and the same span would then re-import alongside its unsplit original.
	again := restoreBackup(t, src, dst)
	if again["pauses"] != 0 {
		t.Errorf("re-importing the backup added %d pause rows, want 0 (merged by ts)", again["pauses"])
	}
	if n := pauseRowCount(t, dst); n != 1 {
		t.Errorf("restored box holds %d pause rows after two restores, want 1", n)
	}
}

// The coverage guard has to survive a restore. A window that observed nothing
// reports coverage 0 so callers omit the uptime figure entirely - /metrics skips
// the series (Observation.Defined) rather than publish a misleading 100%. Drop the
// pause rows and that window looks fully observed and flawless, so the restored
// box publishes pingularity_uptime_ratio{window="24h"} 1 for a day it never
// watched, and the source does not publish it at all.
func TestRestoredBackupStillOmitsUptimeForAnUnobservedWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	src := open(t)
	sampleAt(t, src, now, 2*24*3600, "cf", "ipv4", true) // monitoring began 2 days ago
	// Monitoring off for the whole of the last day (master switch, closed schedule
	// window, or the process simply not running). +5s of slack so the span still
	// covers the window's end when UptimeSince reads its own wall clock.
	pauseAt(t, src, now, 24*3600, 24*3600+5)

	since := now.Add(-24 * time.Hour)
	if o, err := src.UptimeSince(ctx, since, 0); err != nil {
		t.Fatalf("UptimeSince: %v", err)
	} else if cov := o.Coverage(); cov != 0 {
		t.Fatalf("fixture: source coverage = %v, want 0 (the whole window is unobserved)", cov)
	}

	dst := open(t)
	restoreBackup(t, src, dst)

	o, err := dst.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince on the restored box: %v", err)
	}
	ratio, cov := o.Ratio(), o.Coverage()
	if cov != 0 {
		t.Fatalf("restored coverage = %v (uptime %.4f), want 0: the restore turned a window "+
			"that observed nothing into a fully-observed, flawless one, and /metrics then "+
			"publishes an uptime ratio the source correctly omits", cov, ratio)
	}
}

// A backup written before pauses were exported has no such rows at all. It must
// restore cleanly into this binary - and, since import only ever merges, it must
// not disturb the pause spans the destination already recorded for itself.
func TestOldBackupWithoutPausesRestoresCleanly(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	src := open(t)
	sampleAt(t, src, now, 3600, "cf", "ipv4", true)
	eventAt(t, src, now, 1800, "down", -1)
	eventAt(t, src, now, 1700, "up", 100)

	dst := open(t)
	pauseAt(t, dst, now, 1200, 300) // the destination's own unobserved stretch

	// The legacy category set: exactly the tables an older export file carried.
	for _, table := range []string{"samples", "dns", "events", "speed", "settings"} {
		rows, err := src.ExportTable(ctx, table)
		if err != nil {
			t.Fatalf("export %s: %v", table, err)
		}
		if _, err := dst.ImportTable(ctx, table, rows); err != nil {
			t.Fatalf("importing an old backup's %s must succeed: %v", table, err)
		}
	}
	// An old file may also carry an explicitly empty array once the key exists.
	if n, err := dst.ImportTable(ctx, "pauses", nil); err != nil || n != 0 {
		t.Fatalf("import of an empty pauses array = (%d, %v), want (0, nil)", n, err)
	}
	if n := pauseRowCount(t, dst); n != 1 {
		t.Fatalf("destination holds %d pause rows after restoring an old backup, want its own 1", n)
	}
	if p, err := dst.pausedOverlap(ctx, now.Add(-3600*time.Second).Unix(), now.Unix()); err != nil {
		t.Fatalf("pausedOverlap: %v", err)
	} else if p != 300 {
		t.Fatalf("the destination's own unobserved seconds = %d, want 300 (an import merges, never deletes)", p)
	}
}

// A backup taken while genuine history sits in quarantine must CARRY it. The
// held rows land back in the destination's quarantine - never directly in
// coverage - and the import arms the deferred re-judgement, so the
// destination's own clock exonerates what it can vouch for and keeps holding
// what no clock ever could. Before pauses_quarantine joined exportTables, a
// backup in that window silently lost the held rows: if the source box then
// died, the restored history reported different coverage than the source.
func TestBackupCarriesQuarantinedHistory(t *testing.T) {
	srcPath := t.TempDir() + "/src.db"
	src, err := Open(srcPath)
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	now := time.Now().Unix()
	seedLegacyPause(t, src, now-3600, 300)                  // genuine, fully past
	seedLegacyPause(t, src, now, int64(maxPauseDuration-1)) // crafted decade-reacher
	if err := src.Close(); err != nil {
		t.Fatalf("close src: %v", err)
	}
	// A stale-clock open quarantines BOTH (every end reads future a year back).
	src, err = openAt(srcPath, now-365*24*3600)
	if err != nil {
		t.Fatalf("stale reopen: %v", err)
	}
	defer src.Close()
	if held := pauseDurations(t, src, "pauses_quarantine"); len(held) != 2 {
		t.Fatalf("premise broken: quarantine holds %v, want both rows", held)
	}

	// The backup, taken in exactly the loss window - and restored elsewhere.
	dst, err := Open(t.TempDir() + "/dst.db")
	if err != nil {
		t.Fatalf("open dst: %v", err)
	}
	defer dst.Close()
	applied := restoreBackup(t, src, dst)
	if applied["pauses_quarantine"] != 2 {
		t.Fatalf("restore carried %d held rows, want 2", applied["pauses_quarantine"])
	}
	if got := pauseDurations(t, dst, "pauses"); len(got) != 0 {
		t.Fatalf("held rows entered coverage directly on import: pauses=%v", got)
	}
	if !dst.pauseRepairArmed() {
		t.Fatal("the import did not arm the re-judgement; held rows would wait for a restart")
	}
	// The destination's next write re-judges under ITS clock: the genuine row
	// is exonerated into coverage, the decade-reacher stays held.
	dst.maybeRepairFuturePausesAt(now)
	if kept := pauseDurations(t, dst, "pauses"); len(kept) != 1 || kept[0] != 300 {
		t.Errorf("pauses hold %v after the re-judgement, want [300]", kept)
	}
	if held := pauseDurations(t, dst, "pauses_quarantine"); len(held) != 1 || held[0] != int64(maxPauseDuration-1) {
		t.Errorf("quarantine holds %v, want only the decade-reacher", held)
	}

	// And a crafted file cannot implant structural garbage as held history.
	// Distinct timestamps on purpose: a ts already present in the destination
	// quarantine is skipped by the destination-wins merge, which would let a
	// structurally VALID row read as "rejected" here and prove nothing.
	n, err := dst.ImportTable(context.Background(), "pauses_quarantine", []map[string]any{
		{"ts": int64(100), "duration_s": int64(60)},                   // pre-epoch anchor
		{"ts": now + 1000, "duration_s": int64(-5)},                   // negative span
		{"ts": now + 2000, "duration_s": int64(maxPauseDuration + 1)}, // past the retention ceiling
	})
	if err != nil || n != 0 {
		t.Fatalf("garbage held rows: applied=%d err=%v, want 0 applied", n, err)
	}
}

// A cancellation landing between a durable chunk commit and the next BeginTx
// returns committed rows WITH an error - and a retry of those rows is pure
// NOT EXISTS no-ops. The re-judgement must therefore arm at every durable
// chunk commit, or held rows imported through exactly that window stay out of
// coverage until a restart or the next detected clock step.
func TestPartialQuarantineImportStillArmsRepair(t *testing.T) {
	dst, err := Open(t.TempDir() + "/partial.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer dst.Close()
	if dst.pauseRepairArmed() {
		t.Fatal("premise broken: fresh plausible store is armed")
	}

	rows := make([]map[string]any, importTxRows+1) // one full chunk + one leftover
	for i := range rows {
		rows[i] = map[string]any{"ts": int64(plausibleEpoch + int64(i)), "duration_s": int64(9 * 365 * 24 * 3600)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	importChunkHook = cancel // the cancellation lands right after the chunk commit
	defer func() { importChunkHook = nil }()

	n, err := dst.ImportTable(ctx, "pauses_quarantine", rows)
	if err == nil {
		t.Fatal("premise broken: the injected cancellation did not surface as an error")
	}
	if n != importTxRows {
		t.Fatalf("committed %d rows, want exactly the durable chunk (%d)", n, importTxRows)
	}
	if !dst.pauseRepairArmed() {
		t.Fatal("a durable chunk of held rows landed but nothing armed the re-judgement; " +
			"a retry is all no-ops, so the rows would wait for a restart")
	}
}
