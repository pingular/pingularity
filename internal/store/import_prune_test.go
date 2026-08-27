package store

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"
)

// Prune deletes rows strictly older than each table's cutoff (ts < cutoff, so a
// row exactly at the cutoff is kept) and reports the total deleted.
func TestPrune(t *testing.T) {
	st := open(t)
	now := time.Now().Truncate(time.Second)
	cut := now.Add(-30 * time.Second)
	sampleAt(t, st, now, 100, "a", "ipv4", true) // before cutoff -> deleted
	sampleAt(t, st, now, 50, "b", "ipv4", true)  // before cutoff -> deleted
	sampleAt(t, st, now, 30, "c", "ipv4", true)  // exactly at cutoff -> kept
	sampleAt(t, st, now, 10, "d", "ipv4", true)  // after cutoff -> kept
	// A completed outage (down->up); events retention far in the past keeps both.
	// (A completed outage, so prune's dangling-down resolver leaves it alone.)
	eventAt(t, st, now, 100, "down", -1)
	eventAt(t, st, now, 90, "up", 10)

	n, err := st.Prune(context.Background(), cut, now.Add(-1000*time.Second), now.Add(-1000*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("pruned %d, want 2 (strict < cutoff keeps the at-cutoff row)", n)
	}
	cnt, _ := st.TableCounts(context.Background())
	if cnt["samples"] != 2 {
		t.Fatalf("samples remaining = %d, want 2", cnt["samples"])
	}
	if cnt["events"] != 2 {
		t.Fatalf("events should be untouched, got %d", cnt["events"])
	}
}

// Prune must also remove rows stamped far in the future (a wrong boot clock):
// plain ts < cutoff retention would keep them for decades, pinning every
// newest-row read to them.
func TestPruneRemovesFutureRows(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 10, "ok", "ipv4", true)
	sampleAt(t, st, now, -100*24*3600, "future", "ipv4", true) // stamped ~100 days ahead
	n, err := st.Prune(context.Background(), now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1 (the future-stamped row)", n)
	}
	var cnt int
	st.DB().QueryRow(`SELECT COUNT(*) FROM samples WHERE target='future'`).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("future-stamped row survived prune")
	}
}

// A backup written by a newer binary carries columns this build doesn't know;
// the import must refuse it up front instead of silently dropping those fields
// row by row while reporting success.
func TestImportRejectsUnknownColumns(t *testing.T) {
	st := open(t)
	rows := []map[string]any{
		{"ts": float64(1718900000), "target": "cf", "family": "ipv4", "success": float64(1), "warp_factor": float64(9)},
	}
	if _, err := st.ImportTable(context.Background(), "samples", rows); err == nil {
		t.Fatal("import with an unknown column must fail")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["samples"] != 0 {
		t.Fatalf("samples = %d, want 0 (nothing applied)", cnt["samples"])
	}
}

// A big import is applied in bounded transactions (importTxRows apiece) so the
// single SQLite writer isn't held for the whole restore; no rows may be lost
// across chunk boundaries, and a re-run stays idempotent.
func TestImportChunkedTransactions(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	n := importTxRows + 1
	rows := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, map[string]any{
			"ts": int64(1718900000 + i), "target": "cf", "family": "ipv4",
			"success": int64(1), "latency_ms": 10.0,
		})
	}
	got, err := st.ImportTable(ctx, "samples", rows)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got != n {
		t.Fatalf("imported %d rows, want %d", got, n)
	}
	if cnt, _ := st.TableCounts(ctx); cnt["samples"] != int64(n) {
		t.Fatalf("samples = %d, want %d", cnt["samples"], n)
	}
	if again, err := st.ImportTable(ctx, "samples", rows); err != nil || again != 0 {
		t.Fatalf("re-import = (%d, %v), want (0, nil)", again, err)
	}
}

// ImportTable must drop rows whose ts is NaN/Inf/negative - untrusted JSON that
// would otherwise poison retention pruning and uptime math.
func TestImportRejectsMaliciousRows(t *testing.T) {
	st := open(t)
	ts := float64(time.Now().Unix())
	rows := []map[string]any{
		{"ts": ts, "target": "cf", "family": "ipv4", "success": float64(1), "latency_ms": float64(10)}, // clean
		{"ts": math.NaN(), "target": "x", "family": "ipv4", "success": float64(1)},
		{"ts": math.Inf(1), "target": "y", "family": "ipv4", "success": float64(1)},
		{"ts": float64(-5), "target": "z", "family": "ipv4", "success": float64(1)},
	}
	n, err := st.ImportTable(context.Background(), "samples", rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("imported %d rows, want 1 (NaN/Inf/negative-ts rejected)", n)
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["samples"] != 1 {
		t.Fatalf("samples = %d, want 1", cnt["samples"])
	}
}

// A crafted settings import must not be able to implant a password hash or a
// foreign telemetry identity (the export denylist guards imports too). The
// webhook/heartbeat URLs, by contrast, are part of a backup and DO round-trip.
func TestImportSettingsDenylist(t *testing.T) {
	st := open(t)
	rows := []map[string]any{
		{"key": "auth_hash", "value": "STOLEN-HASH"},
		{"key": "telemetry_install_id", "value": "FOREIGN-ID"},
		{"key": "digest_last_sent", "value": "99999999999"},           // crafted future watermark
		{"key": "webhook_url", "value": "https://example.com/hook"},   // backed up (not denied)
		{"key": "heartbeat_url", "value": "https://example.com/beat"}, // backed up (not denied)
		{"key": "digest_freq", "value": "daily"},                      // an allowed preference
	}
	n, err := st.ImportTable(context.Background(), "settings", rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("imported %d settings, want 3 (auth_hash + telemetry id + digest watermark denied; webhook/heartbeat/digest_freq allowed)", n)
	}
	m, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, bad := m["auth_hash"]; bad {
		t.Error("auth_hash must NOT be importable")
	}
	if _, bad := m["telemetry_install_id"]; bad {
		t.Error("telemetry_install_id must NOT be importable")
	}
	if _, bad := m["digest_last_sent"]; bad {
		t.Error("digest_last_sent must NOT be importable")
	}
	if m["webhook_url"] != "https://example.com/hook" {
		t.Errorf("webhook_url should round-trip through a backup, got %q", m["webhook_url"])
	}
	if m["heartbeat_url"] != "https://example.com/beat" {
		t.Errorf("heartbeat_url should round-trip through a backup, got %q", m["heartbeat_url"])
	}
	if m["digest_freq"] != "daily" {
		t.Errorf("digest_freq should import, got %q", m["digest_freq"])
	}
}

// A crafted row that omits or nulls a NOT NULL column must be skipped on its own
// - not abort the whole import via a failed Exec + Rollback. Covers both the
// keyed-merge path (samples.success) and the INSERT OR REPLACE path
// (settings.value, where the table has no key columns).
func TestImportSkipsNullNotNull(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ts := float64(time.Now().Unix())
	srows := []map[string]any{
		{"ts": ts, "target": "cf", "family": "ipv4", "success": float64(1), "latency_ms": float64(10)}, // clean
		{"ts": ts + 1, "target": "x", "family": "ipv4", "success": nil},                                // explicit null
		{"ts": ts + 2, "target": "y", "family": "ipv4"},                                                // success omitted
	}
	if n, err := st.ImportTable(ctx, "samples", srows); err != nil || n != 1 {
		t.Fatalf("samples import = (%d, %v), want (1, nil): a bad row must skip, not abort", n, err)
	}
	wrows := []map[string]any{
		{"key": "digest_freq", "value": "daily"},
		{"key": "broken", "value": nil},
	}
	if n, err := st.ImportTable(ctx, "settings", wrows); err != nil || n != 1 {
		t.Fatalf("settings import = (%d, %v), want (1, nil)", n, err)
	}
}

// DNS samples ride the latency dataset (same chart, same retention), so they
// must be cleared by Clear("latency") and survive an export/import round-trip -
// dns was previously pruned and charted but absent from both Clear and the
// export whitelist. (TableCounts doesn't track dns, so count via ExportTable.)
func TestDNSClearAndRoundTrip(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.InsertDNS(ctx, time.Unix(1000, 0), 12, true); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDNS(ctx, time.Unix(1001, 0), 0, false); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ExportTable(ctx, "dns")
	if err != nil {
		t.Fatalf("dns must be exportable: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("exported %d dns rows, want 2", len(rows))
	}
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.ExportTable(ctx, "dns"); len(again) != 0 {
		t.Fatalf("dns rows after Clear(latency) = %d, want 0", len(again))
	}
	n, err := st.ImportTable(ctx, "dns", rows)
	if err != nil {
		t.Fatalf("dns import: %v", err)
	}
	if n != 2 {
		t.Fatalf("re-imported %d dns rows, want 2", n)
	}
	if back, _ := st.ExportTable(ctx, "dns"); len(back) != 2 {
		t.Fatalf("dns rows after import = %d, want 2", len(back))
	}
}

// A crafted import packing many rows at ONE ts is capped (maxRowsPerTS) so the
// per-row de-dup probe can't be driven O(N^2) against the ts-only index. Legit
// data (a handful of rows per ts) is well under the cap and unaffected.
func TestImportPerTSCap(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	rows := make([]map[string]any, 0, 300)
	for i := 0; i < 300; i++ {
		rows = append(rows, map[string]any{
			"ts": int64(1000), "target": "t" + strconv.Itoa(i),
			"latency_ms": 10.0, "success": int64(1), "family": "ipv4",
		})
	}
	n, err := st.ImportTable(ctx, "samples", rows)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 256 {
		t.Fatalf("imported %d rows at one ts, want the per-ts cap of 256", n)
	}
	if back, _ := st.ExportTable(ctx, "samples"); len(back) != 256 {
		t.Fatalf("stored %d sample rows, want 256", len(back))
	}
}

// Events paging: EventCount totals, EventsPage is newest-first with limit/offset.
func TestEventsPaging(t *testing.T) {
	st := open(t)
	now := time.Now().Truncate(time.Second)
	eventAt(t, st, now, 300, "down", -1) // oldest
	eventAt(t, st, now, 200, "up", 100)
	eventAt(t, st, now, 100, "down", -1) // newest

	if c, _ := st.EventCount(context.Background()); c != 3 {
		t.Fatalf("EventCount = %d, want 3", c)
	}
	page, err := st.EventsPage(context.Background(), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].TS <= page[1].TS {
		t.Fatalf("EventsPage must be newest-first, len 2; got %+v", page)
	}
	if page[0].TS != now.Add(-100*time.Second).Unix() {
		t.Error("first page row should be the newest event")
	}
	if p2, _ := st.EventsPage(context.Background(), 2, 2); len(p2) != 1 {
		t.Fatalf("offset page len = %d, want 1", len(p2))
	}
}

// Series downsamples to (ts/bucketSec)*bucketSec buckets and takes the min
// latency per bucket. Every other Series test uses bucketSec=1, leaving the
// bucket arithmetic + cross-bucket grouping unexercised.
// Series joins DNS-resolve samples onto the same buckets (mean per bucket), and a
// bucket with no DNS sample (or a failed one) leaves DNSms nil.
func TestSeriesDNS(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	if err := st.InsertSamples(ctx, []Sample{
		{TS: time.Unix(10, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},  // bucket 0
		{TS: time.Unix(310, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 15}, // bucket 300
	}); err != nil {
		t.Fatal(err)
	}
	st.InsertDNS(ctx, time.Unix(10, 0), 20, true)   // bucket 0
	st.InsertDNS(ctx, time.Unix(20, 0), 40, true)   // bucket 0 (mean -> 30)
	st.InsertDNS(ctx, time.Unix(330, 0), 99, false) // bucket 300: failed -> NULL -> nil
	pts, err := st.Series(ctx, time.Unix(0, 0), time.Time{}, 300, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2", len(pts))
	}
	if pts[0].DNSms == nil || *pts[0].DNSms != 30 {
		t.Errorf("bucket0 DNS = %v, want 30 (mean of 20,40)", pts[0].DNSms)
	}
	if pts[1].DNSms != nil {
		t.Errorf("bucket1 DNS = %v, want nil (only a failed sample)", *pts[1].DNSms)
	}
}

func TestSeriesBucketing(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	mk := func(ts int64, lat float64) Sample {
		return Sample{TS: time.Unix(ts, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: lat}
	}
	if err := st.InsertSamples(ctx, []Sample{
		mk(10, 50), mk(20, 30), // bucket [0,300): min 30
		mk(310, 80), mk(320, 40), // bucket [300,600): min 40
	}); err != nil {
		t.Fatal(err)
	}
	pts, err := st.Series(ctx, time.Unix(0, 0), time.Time{}, 300, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2", len(pts))
	}
	if pts[0].TS != 0 || pts[1].TS != 300 {
		t.Fatalf("bucket TS = %d, %d; want 0, 300", pts[0].TS, pts[1].TS)
	}
	if pts[0].LatencyMS == nil || *pts[0].LatencyMS != 30 || pts[1].LatencyMS == nil || *pts[1].LatencyMS != 40 {
		t.Fatalf("bucket mins = %v, %v; want 30, 40", pts[0].LatencyMS, pts[1].LatencyMS)
	}
}

// A speed row without `server` must not wedge the store: the import skips it
// (notNull), and even a NULL that got in another way reads back as "" via the
// COALESCE - one such row used to 500 every speed read and silently empty the
// /metrics speed block.
func TestImportSpeedRowWithoutServerIsSafe(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	n, err := st.ImportTable(ctx, "speed", []map[string]any{
		{"ts": 1000, "down_mbps": 1.0, "up_mbps": 1.0, "ping_ms": 1.0}, // no server: skipped
		{"ts": 1001, "down_mbps": 2.0, "up_mbps": 2.0, "ping_ms": 2.0, "server": "ok"},
	})
	if err != nil || n != 1 {
		t.Fatalf("import = (%d, %v), want (1, nil): serverless row must be skipped", n, err)
	}

	// Belt and braces: force a NULL in directly; reads must still work.
	if _, err := st.db.Exec(`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server) VALUES (999, 1, 1, 1, NULL)`); err != nil {
		t.Fatalf("seed null: %v", err)
	}
	sp, err := st.LatestSpeed(ctx)
	if err != nil || sp == nil {
		t.Fatalf("LatestSpeed after NULL server: (%v, %v)", sp, err)
	}
	rows, err := st.SpeedHistory(ctx, time.Unix(0, 0))
	if err != nil || len(rows) != 2 {
		t.Fatalf("SpeedSince after NULL server: (%d rows, %v), want 2", len(rows), err)
	}
}

// Events prune as whole outages: a 'down' older than the cutoff whose paired 'up'
// straddles it is kept (not orphaned), while a completed outage fully before the
// cutoff is pruned (audit: whole-outage pruning).
func TestPruneWholeOutage(t *testing.T) {
	st := open(t)
	now := time.Now().Truncate(time.Second)
	sampleAt(t, st, now, 10, "a", "ipv4", true) // keep samples out of it
	// Completed outage fully before the cutoff -> both events pruned.
	eventAt(t, st, now, 1000, "down", -1)
	eventAt(t, st, now, 900, "up", 100)
	// Outage straddling the cutoff (down before, up after) -> both kept whole.
	eventAt(t, st, now, 600, "down", -1)
	eventAt(t, st, now, 400, "up", 200)

	eventsCut := now.Add(-500 * time.Second)
	if _, err := st.Prune(context.Background(), now.Add(-time.Hour), now.Add(-time.Hour), eventsCut); err != nil {
		t.Fatal(err)
	}
	cnt, _ := st.TableCounts(context.Background())
	if cnt["events"] != 2 {
		t.Fatalf("events remaining = %d, want 2 (the straddling pair kept whole, the old outage pruned)", cnt["events"])
	}
	var downs int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE type='down' AND ts=?`, now.Add(-600*time.Second).Unix()).Scan(&downs); err != nil {
		t.Fatal(err)
	}
	if downs != 1 {
		t.Fatal("the straddling 'down' (older than cutoff) must be KEPT so its outage isn't split")
	}
}

// A non-integral value in an INTEGER column (e.g. an imported ts=….5) is rejected,
// never stored as a REAL that later breaks int64 reads (audit: fractional timestamps).
func TestImportRejectsFractionalInt(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// Fractional ts -> skipped.
	n, err := st.ImportTableBatch(ctx, "events", []map[string]any{{"ts": 1784765000.5, "type": "down"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fractional ts imported %d rows, want 0 (rejected)", n)
	}
	// A whole-number ts (even as float64, like JSON) is accepted and stored as INTEGER.
	n, err = st.ImportTableBatch(ctx, "events", []map[string]any{{"ts": float64(1784765000), "type": "down"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("whole-number ts imported %d rows, want 1", n)
	}
	var typ string
	if err := st.DB().QueryRow(`SELECT typeof(ts) FROM events LIMIT 1`).Scan(&typ); err != nil {
		t.Fatal(err)
	}
	if typ != "integer" {
		t.Fatalf("stored ts typeof = %q, want integer", typ)
	}
}

// Imported speed byte counts must be clamped like InsertSpeed does, or a crafted
// backup can overflow the int64 SUM in SpeedDataUsage and permanently 500 the
// data-usage reads (audit: import-speed-bytes-unclamped-sum-overflow).
func TestImportClampsSpeedBytes(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	big := float64(9e18) // > int64 max/2; two of them overflow the SUM
	for i, ts := range []int64{1784765000, 1784765100} {
		n, err := st.ImportTableBatch(ctx, "speed",
			[]map[string]any{{"ts": float64(ts), "server": "x", "download_bytes": big}}, nil)
		if err != nil || n != 1 {
			t.Fatalf("import row %d: n=%d err=%v", i, n, err)
		}
	}
	// The data-usage SUM must not overflow (it did before the clamp).
	if _, err := st.SpeedDataUsage(ctx, time.Now()); err != nil {
		t.Fatalf("SpeedDataUsage overflowed on imported bytes: %v", err)
	}
}

// A backup from before Best-of was a count restores the count its on/off
// meant, even on a host that has since stored a count of its own - the count
// wins whenever present, so the on/off alone would never come back.
func TestImportMigratesTheBestOfOnOff(t *testing.T) {
	st := open(t)
	if _, err := st.SetSettingsDiff(context.Background(), map[string]string{"speed_best_of_count": "8"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ImportTable(context.Background(), "settings", []map[string]any{{"key": "speed_best_of", "value": "0"}}); err != nil {
		t.Fatal(err)
	}
	m, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m["speed_best_of_count"] != "1" {
		t.Errorf("count %q after restoring a backup with Best-of off, want 1", m["speed_best_of_count"])
	}
	if _, err := st.ImportTable(context.Background(), "settings", []map[string]any{{"key": "speed_best_of", "value": "true"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.AllSettings(context.Background()); m["speed_best_of_count"] != "3" {
		t.Errorf("count %q after restoring a backup with Best-of on, want 3", m["speed_best_of_count"])
	}
	// A backup that carries its own count is restored as it is.
	if _, err := st.ImportTable(context.Background(), "settings", []map[string]any{{"key": "speed_best_of", "value": "1"}, {"key": "speed_best_of_count", "value": "5"}}); err != nil {
		t.Fatal(err)
	}
	if m, _ = st.AllSettings(context.Background()); m["speed_best_of_count"] != "5" {
		t.Errorf("count %q after restoring a backup with its own count, want 5", m["speed_best_of_count"])
	}
}
