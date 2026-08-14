package store

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// open returns an in-memory store for testing.
func open(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// sampleAt inserts one target's probe result at secsAgo seconds before now.
func sampleAt(t *testing.T, st *Store, now time.Time, secsAgo int, target, family string, success bool) {
	t.Helper()
	if err := st.InsertSamples(context.Background(), []Sample{{
		TS: now.Add(-time.Duration(secsAgo) * time.Second), Target: target,
		Family: family, Success: success, LatencyMS: 10,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
}

func eventAt(t *testing.T, st *Store, now time.Time, secsAgo int, typ string, durationS int) {
	t.Helper()
	if err := st.InsertEvent(context.Background(), now.Add(-time.Duration(secsAgo)*time.Second), typ, durationS, ""); err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func approx(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.02 {
		t.Fatalf("got %.4f, want ~%.4f", got, want)
	}
}

// ratioOf keeps the many tests that assert only the up-fraction to one line:
//
//	up, err := ratioOf(st.UptimeSince(ctx, since, 0))
//
// It is TEST-ONLY sugar and deliberately not exported: production hands the whole
// Observation around so a renderer cannot end up holding a ratio whose coverage
// was dropped, which is the defect the type exists to make unspellable. A test
// that cares about coverage takes the Observation itself (see the coverage tests
// below).
func ratioOf(o Observation, err error) (float64, error) { return o.Ratio(), err }

// A clean run with no outage events is fully up.
func TestUptimeNoOutages(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1.0)
}

// A single completed outage (recorded as an 'up' event carrying its duration)
// is subtracted from the window.
func TestUptimeCompletedOutage(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	// Outage spanned [now-600, now-500] (100s), closed by an 'up' at now-500.
	eventAt(t, st, now, 600, "down", -1)
	eventAt(t, st, now, 500, "up", 100)
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 0.9) // 100s down out of 1000s
}

// A restart mid-outage writes a second 'down' with no 'up' between the two.
// The gap between consecutive downs must still count as downtime (#5).
func TestUptimeOrphanedDoubleDown(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 600, "down", -1) // outage begins
	eventAt(t, st, now, 300, "down", -1) // restart re-detects: orphaned gap 600->300 = 300s
	eventAt(t, st, now, 100, "up", 200)  // recovery closes the second down: 300->100 = 200s
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// 300 (orphan gap) + 200 (closed outage) = 500s down out of 1000s.
	approx(t, up, 0.5)
}

// A dangling final 'down' is bounded at the first second the *quorum* recovered
// (a strict majority of a family's targets), not at the first single successful
// target (#4).
func TestUptimeDanglingDownQuorumRecovery(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // baseline, sets the window start
	eventAt(t, st, now, 300, "down", -1)           // last event is a down, never closed

	// now-200: only 1 of 3 ipv4 targets up - NOT a quorum (must not count as recovery).
	sampleAt(t, st, now, 200, "a", "ipv4", true)
	sampleAt(t, st, now, 200, "b", "ipv4", false)
	sampleAt(t, st, now, 200, "c", "ipv4", false)
	// now-100: 2 of 3 up - quorum recovered here.
	sampleAt(t, st, now, 100, "a", "ipv4", true)
	sampleAt(t, st, now, 100, "b", "ipv4", true)
	sampleAt(t, st, now, 100, "c", "ipv4", false)

	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// Down from now-300 to quorum recovery at now-100 = 200s out of 1000s.
	// (The single-target heuristic would have stopped at now-200 → 0.9.)
	approx(t, up, 0.8)
}

// A 'down' left dangling by a recovery that happened while the monitor was off
// must not turn the healthy gap up to the NEXT outage into downtime: the
// orphan down->down gap is bounded at the first quorum-recovery second after
// its leading 'down', like the dangling-final-'down' branch.
func TestUptimeOrphanGapBoundedByQuorumRecovery(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 900, "down", -1) // dangles: link recovered while the monitor was off
	// Monitor back at now-850: quorum immediately (the link is up).
	sampleAt(t, st, now, 850, "cf", "ipv4", true)
	sampleAt(t, st, now, 500, "cf", "ipv4", true)
	eventAt(t, st, now, 200, "down", -1) // the next real outage
	eventAt(t, st, now, 100, "up", 100)
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// 50s (dangling down at now-900, bounded at the quorum sample at now-850)
	// + 100s (closed outage) = 150s down out of 1000s. The unbounded orphan
	// heuristic would have charged the whole 700s gap.
	approx(t, up, 0.85)
}

// The dangling-'down' recovery scan walks forward in growing chunks; a
// recovery beyond the first chunk (>1h after the down) must still be found,
// and a repeat call (memoized) must agree.
func TestUptimeDanglingDownRecoveryBeyondFirstChunk(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 10000, "cf", "ipv4", true)
	eventAt(t, st, now, 8000, "down", -1)
	sampleAt(t, st, now, 3000, "cf", "ipv4", true) // first quorum second, 5000s after the down
	for i := 0; i < 2; i++ {
		up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-10000*time.Second), 0))
		if err != nil {
			t.Fatal(err)
		}
		approx(t, up, 0.5) // down now-8000 -> recovery now-3000 = 5000s of 10000s
	}
}

// The recovery memo must not advance its frontier all the way to `before`: a
// probe round is stamped at its start but committed a little later, so a quorum
// sample can appear just behind the scan head. Memoizing up to `before` would
// skip it forever (the outage would read as ongoing); the frontier stays
// recFrontierSlack behind so the recent tail is re-scanned.
func TestFirstQuorumRecoveryReScansRecentTail(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	down := time.Now().Add(-24 * time.Hour).Unix()
	before := down + 4000

	// First scan: no quorum sample in (down, before] yet -> not found.
	if _, found, err := st.firstQuorumRecovery(ctx, down, before); err != nil || found {
		t.Fatalf("scan1 found=%v err=%v, want not found", found, err)
	}
	// A quorum sample lands late, within recFrontierSlack of `before` (so an
	// advance-to-before memo would have skipped it).
	w := before - 100
	if err := st.InsertSamples(ctx, []Sample{{
		TS: time.Unix(w, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	rec, found, err := st.firstQuorumRecovery(ctx, down, before)
	if err != nil || !found {
		t.Fatalf("scan2 found=%v err=%v, want found (frontier skipped the late sample)", found, err)
	}
	if rec != w {
		t.Fatalf("recovery second = %d, want %d", rec, w)
	}
}

// The uptime window anchor must survive sample pruning: retention removes old
// samples (default 30d) long before events (365d), and without a persisted
// anchor every longer window silently degrades to the retained-samples span,
// zeroing out older outages the heatmap still shows.
func TestUptimeAnchorSurvivesSamplePruning(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 10000, "cf", "ipv4", true)
	eventAt(t, st, now, 6000, "down", -1)
	eventAt(t, st, now, 5000, "up", 1000)
	// The first call persists the anchor (now-10000).
	up, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 0.9)
	// Prune every sample (sample retention shorter than the outage age); the
	// outage events survive, and so must the window anchor.
	if _, err := st.Prune(ctx, now, now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatal(err)
	}
	up, err = ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 0.9)
}

// A near-epoch sample recorded under a wrong boot clock must not anchor the
// "all" uptime window decades back and dilute real downtime to ~100%.
func TestUptimeIgnoresEpochGarbageAnchor(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.InsertSamples(ctx, []Sample{{
		TS: time.Unix(1000, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 600, "down", -1)
	eventAt(t, st, now, 500, "up", 100)
	up, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 0.9) // window anchored at now-1000, not 1970
}

// Series marks a bucket online when any family has a quorum, using the stored
// family column - even when target names lack the legacy "-v6" suffix (#6).
func TestSeriesUsesFamilyColumn(t *testing.T) {
	st := open(t)
	now := time.Now()
	// One ipv4 target up (1/1 quorum); two ipv6 targets down (0/2). Names carry
	// no "-v6" suffix, so a name-based guess would lump all three as ipv4 (1/3,
	// offline). The family column must drive it → ipv4 online → bucket online.
	sampleAt(t, st, now, 10, "x", "ipv4", true)
	sampleAt(t, st, now, 10, "y", "ipv6", false)
	sampleAt(t, st, now, 10, "z", "ipv6", false)

	pts, err := st.Series(context.Background(), now.Add(-60*time.Second), time.Time{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("expected 1 point, got %d", len(pts))
	}
	if !pts[0].Online {
		t.Fatal("bucket should be online via ipv4 quorum (family column), got offline")
	}
}

// Series drops the named targets from the latency MIN (the "lowest" line) but
// still counts every target for online/outage detection.
func TestSeriesExcludeTargets(t *testing.T) {
	st := open(t)
	now := time.Now()
	ins := func(target string, lat float64) {
		t.Helper()
		if err := st.InsertSamples(context.Background(), []Sample{{
			TS: now, Target: target, Family: "ipv4", Success: true, LatencyMS: lat,
		}}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	ins("cloudflare", 50)
	ins("google", 10) // the lowest
	ins("quad9", 30)
	get := func(exclude ...string) SeriesPoint {
		t.Helper()
		pts, err := st.Series(context.Background(), now.Add(-60*time.Second), time.Time{}, 1, exclude)
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) != 1 {
			t.Fatalf("want 1 point, got %d", len(pts))
		}
		return pts[0]
	}
	want := func(p SeriesPoint, lat float64) {
		t.Helper()
		if p.LatencyMS == nil || *p.LatencyMS != lat {
			t.Fatalf("want min %g, got %v", lat, p.LatencyMS)
		}
	}
	want(get(), 10)                  // no exclude -> overall lowest
	want(get("google"), 30)          // drop the lowest -> next lowest
	want(get("google", "quad9"), 50) // drop two -> remaining
	if p := get("google", "quad9"); !p.Online {
		t.Fatal("excluding from the MIN must not change online (all targets still up)")
	}
}

// Export then import (after a clear) preserves rows including the family column,
// and re-importing the same rows adds nothing (merge-by-key is idempotent).
func TestExportImportSamplesRoundTrip(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	sampleAt(t, st, now, 20, "cf6", "ipv6", false)

	rows, err := st.ExportTable(context.Background(), "samples")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("export: expected 2 rows, got %d", len(rows))
	}

	if _, err := st.Clear(context.Background(), "latency"); err != nil {
		t.Fatal(err)
	}
	n, err := st.ImportTable(context.Background(), "samples", rows)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("import: expected 2 rows added, got %d", n)
	}

	// The family column survived the round trip.
	var fam string
	if err := st.DB().QueryRow(`SELECT family FROM samples WHERE target='cf6'`).Scan(&fam); err != nil {
		t.Fatal(err)
	}
	if fam != "ipv6" {
		t.Fatalf("family not preserved: got %q", fam)
	}

	// Re-importing the same rows is a no-op (existing keys are skipped).
	again, err := st.ImportTable(context.Background(), "samples", rows)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("re-import should add 0 rows, added %d", again)
	}
}

// End-to-end: ImportTable must drop a row whose ts is out of int64 range (a
// crafted import) rather than store a wrapped/garbage timestamp.
func TestImportRejectsOverflowTimestamp(t *testing.T) {
	st := open(t)
	sampleAt(t, st, time.Now(), 30, "cf", "ipv4", true)
	rows, err := st.ExportTable(context.Background(), "samples")
	if err != nil || len(rows) != 1 {
		t.Fatalf("export: err=%v rows=%d", err, len(rows))
	}
	bad := map[string]any{}
	for k, v := range rows[0] {
		bad[k] = v
	}
	bad["ts"] = float64(1e20) // JSON numbers decode to float64; out of int64 range
	bad["target"] = "evil"
	if _, err := st.Clear(context.Background(), "latency"); err != nil {
		t.Fatal(err)
	}
	n, err := st.ImportTable(context.Background(), "samples", []map[string]any{rows[0], bad})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("crafted overflow-ts row should be dropped: want 1 imported, got %d", n)
	}
	var total, evil, neg int
	st.DB().QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&total)
	st.DB().QueryRow(`SELECT COUNT(*) FROM samples WHERE target='evil'`).Scan(&evil)
	st.DB().QueryRow(`SELECT COUNT(*) FROM samples WHERE ts < 0`).Scan(&neg)
	if total != 1 || evil != 0 || neg != 0 {
		t.Fatalf("overflow not contained: total=%d evil=%d negTs=%d", total, evil, neg)
	}
}

// normVal must reject whole-number floats outside int64 range so a crafted import
// (e.g. {"ts": 1e20}) can't silently wrap to a garbage timestamp that poisons
// pruning/uptime math. In-range whole numbers convert; non-whole pass through.
func TestNormValBounds(t *testing.T) {
	cases := []struct {
		in     any
		wantOK bool
		want   any // only checked when wantOK
	}{
		{float64(1e20), false, nil},   // > MaxInt64: would wrap
		{float64(-1e20), false, nil},  // < MinInt64: would wrap
		{float64(9.3e18), false, nil}, // just over MaxInt64 (~9.22e18)
		{math.Inf(1), false, nil},
		{math.NaN(), false, nil},
		{float64(1718900000), true, int64(1718900000)}, // a real unix ts
		{float64(0), true, int64(0)},
		{float64(123.5), true, float64(123.5)}, // non-whole: passes through unchanged
	}
	for _, c := range cases {
		got, ok := normVal(c.in)
		if ok != c.wantOK {
			t.Errorf("normVal(%v) ok=%v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if c.wantOK && got != c.want {
			t.Errorf("normVal(%v) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}

// A target that stopped being probed (e.g. IPv6 toggled off) must drop out of
// LatestPerTarget once it lags more than grace behind the newest round, while
// still-probed targets keep reporting. grace <= 0 disables the cutoff.
func TestLatestPerTargetDropsStaleTargets(t *testing.T) {
	st := open(t)
	now := time.Now()
	// v6 last probed 100s ago; v4 probed in the two most recent rounds.
	sampleAt(t, st, now, 100, "cf-v6", "ipv6", true)
	sampleAt(t, st, now, 10, "cf", "ipv4", true)
	sampleAt(t, st, now, 5, "cf", "ipv4", true)

	got, err := st.LatestPerTarget(context.Background(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Target != "cf" {
		t.Fatalf("expected only the live target, got %+v", got)
	}
	if got[0].TS != now.Add(-5*time.Second).Unix() {
		t.Fatal("expected the newest sample for the live target")
	}

	// Cutoff disabled: the stale target reappears.
	got, err = st.LatestPerTarget(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected both targets with grace disabled, got %+v", got)
	}
}

// DowntimeByDay buckets events by calendar day in the requested timezone, so
// the same event lands on different dates for viewers in different zones.
func TestDowntimeByDayTimezone(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	// 2026-01-02 00:30 UTC: still 2026-01-01 in New York (UTC-5).
	ts := time.Date(2026, 1, 2, 0, 30, 0, 0, time.UTC)
	// Anchor monitoring before the requested window so the monitoring floor is a
	// no-op here: this test is about which local day an outage lands on, not about
	// how the heatmap discloses time that predates the install (see
	// heatmap_floor_test.go). Without a sample, monitoringSince falls back to the
	// first EVENT, which in a fixture seeded only with events is the outage
	// itself - so every hour before it would correctly read as unobserved and
	// bury what this test is actually checking.
	if err := st.InsertSamples(ctx, []Sample{{
		TS: ts.Add(-48 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := st.InsertEvent(ctx, ts.Add(-90*time.Second), "down", -1, ""); err != nil {
		t.Fatalf("insert down: %v", err)
	}
	if err := st.InsertEvent(ctx, ts, "up", 90, ""); err != nil {
		t.Fatalf("insert up: %v", err)
	}

	since := ts.Add(-24 * time.Hour)
	utc, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay UTC: %v", err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	nyRows, err := st.DowntimeByDay(ctx, since, ny)
	if err != nil {
		t.Fatalf("DowntimeByDay NY: %v", err)
	}

	// In UTC both events fall on Jan 2; in New York both fall on Jan 1
	// (the down at 19:28:30, the up at 19:30 local).
	if len(utc) != 1 || utc[0].Date != "2026-01-02" || utc[0].Outages != 1 || utc[0].DowntimeS != 90 {
		t.Errorf("UTC rows = %+v, want one 2026-01-02 row with 1 outage / 90s", utc)
	}
	if len(nyRows) != 1 || nyRows[0].Date != "2026-01-01" || nyRows[0].Outages != 1 || nyRows[0].DowntimeS != 90 {
		t.Errorf("NY rows = %+v, want one 2026-01-01 row with 1 outage / 90s", nyRows)
	}

	// nil location falls back to the server's local zone (today's behavior).
	localRows, err := st.DowntimeByDay(ctx, since, nil)
	if err != nil {
		t.Fatalf("DowntimeByDay nil loc: %v", err)
	}
	wantDate := ts.In(time.Local).Format("2006-01-02")
	if len(localRows) == 0 || localRows[0].Date != wantDate {
		t.Errorf("nil-loc rows = %+v, want date %s", localRows, wantDate)
	}
}

// A multi-day outage must mark every offline day: the duration is prorated
// across the local days it spanned, not booked entirely on the recovery day.
func TestDowntimeByDayProratesAcrossDays(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// Down Mon 2026-01-05 20:00 UTC, back Thu 2026-01-08 08:00 UTC (60h).
	down := time.Date(2026, 1, 5, 20, 0, 0, 0, time.UTC)
	up := time.Date(2026, 1, 8, 8, 0, 0, 0, time.UTC)
	// Anchor monitoring before the window so the monitoring floor is a no-op: this
	// test is about splitting one outage across local days, not about how the heatmap discloses
	// pre-install time (see heatmap_floor_test.go). monitoringSince otherwise falls
	// back to the first EVENT, which here is the outage itself.
	if err := st.InsertSamples(ctx, []Sample{{
		TS: down.Add(-48 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", int(up.Sub(down).Seconds()), ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, down.Add(-24*time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// Every day here is a whole UTC day inside the window and has no pause row, so
	// each is fully observed: WindowS == ObservedS == 86400.
	want := []DowntimeDay{
		{Date: "2026-01-05", Outages: 1, DowntimeS: 4 * 3600, WindowS: 86400, ObservedS: 86400},
		{Date: "2026-01-06", DowntimeS: 24 * 3600, WindowS: 86400, ObservedS: 86400},
		{Date: "2026-01-07", DowntimeS: 24 * 3600, WindowS: 86400, ObservedS: 86400},
		{Date: "2026-01-08", DowntimeS: 8 * 3600, WindowS: 86400, ObservedS: 86400},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("day %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

// An outage whose observed duration is shorter than its wall-clock gap (a
// suspend or monitoring pause fell inside it) is placed at the paired 'down'
// event's start, not at up.ts-duration_s. Its 1h of downtime lands on the day
// it began, not the recovery day.
func TestDowntimeByDayAnchorsOnDownEvent(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// Down 2026-01-05 22:00 UTC, recovered 2026-01-06 03:00 UTC (5h wall gap),
	// but only 1h was observed down - a 4h suspend fell inside.
	down := time.Date(2026, 1, 5, 22, 0, 0, 0, time.UTC)
	up := time.Date(2026, 1, 6, 3, 0, 0, 0, time.UTC)
	// Anchor monitoring before the window so the monitoring floor is a no-op: this
	// test is about which day an outage is anchored to, not about how the heatmap discloses
	// pre-install time (see heatmap_floor_test.go). monitoringSince otherwise falls
	// back to the first EVENT, which here is the outage itself.
	if err := st.InsertSamples(ctx, []Sample{{
		TS: down.Add(-48 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", 3600, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, down.Add(-24*time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	// One day only: 01-05, carrying both the outage marker and the full 1h.
	// up.ts-duration_s would have booked the hour on 01-06 instead.
	want := []DowntimeDay{{Date: "2026-01-05", Outages: 1, DowntimeS: 3600, WindowS: 86400, ObservedS: 86400}}
	if len(rows) != len(want) || rows[0] != want[0] {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

// An EXPLICIT monitoring pause inside a completed outage is folded into the recorded
// duration_s by the monitor AND written to the `pauses` table. The heatmap must credit
// the observed downtime ONCE: subtracting the pause a second time during proration made
// DowntimeByDay disagree with the uptime% (which trusts duration_s directly). Regression
// for the pause double-subtraction.
func TestDowntimeByDayReconcilesPauseWithinOutage(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// Observed down 30s, monitoring paused 60s, observed down 30s more, then recover:
	// wall gap 120s, observed length (== stored duration_s) 60s.
	down := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	up := down.Add(120 * time.Second)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", 60, ""); err != nil { // duration_s already pause-adjusted
		t.Fatal(err)
	}
	if _, err := st.InsertPause(ctx, down.Add(30*time.Second), 60); err != nil { // pause [+30s, +90s)
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, down.Add(-time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, r := range rows {
		total += r.DowntimeS
	}
	// Must equal duration_s (60s) - exactly what the uptime% books. The old code
	// credited 30s (the pause removed once by the monitor, then again in prorate).
	if total != 60 {
		t.Fatalf("heatmap downtime total = %ds, want 60s (== duration_s / uptime); pause double-subtracted?", total)
	}
}

// The same reconciliation, but the pause-bearing outage straddles local midnight: the
// observed seconds must split onto the correct days and still total duration_s.
func TestDowntimeByDayPauseAcrossMidnight(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// Down 23:59:00, pause 60s at 23:59:30 (spans midnight, ends 00:00:30), recover
	// 00:03:00: wall 240s, pause 60s, observed (duration_s) 180s. Observed splits as
	// 30s on the down day (23:59:00-23:59:30) and 150s on the next (00:00:30-00:03:00).
	down := time.Date(2026, 3, 10, 23, 59, 0, 0, time.UTC)
	up := time.Date(2026, 3, 11, 0, 3, 0, 0, time.UTC)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", 180, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertPause(ctx, down.Add(30*time.Second), 60); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, down.Add(-time.Hour), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	total := 0
	for _, r := range rows {
		got[r.Date] = r.DowntimeS
		total += r.DowntimeS
	}
	if total != 180 {
		t.Fatalf("total downtime = %ds, want 180s (== duration_s)", total)
	}
	if got["2026-03-10"] != 30 || got["2026-03-11"] != 150 {
		t.Fatalf("per-day = {03-10:%d, 03-11:%d}, want {30, 150}", got["2026-03-10"], got["2026-03-11"])
	}
}

// A corrupt or crafted duration_s (a backup import does not range-check it, and
// normVal accepts any whole number up to ~9.2e18) must not drive the per-day loop
// outside the queried window: unclamped it appends one row per day of the
// duration - eleven million of them in the first seconds here - hanging the
// request and exhausting memory. prorate clamps every span to [since, now], so
// the call returns promptly with a handful of rows.
func TestDowntimeByDayClampsCraftedDuration(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	// Seeded as a legacy row, like its UptimeSince sibling: InsertEvent now drops a
	// duration this large (maxPauseDuration), so routing the fixture through it
	// leaves duration_s NULL and the scenario unbuilt. The residue this guards
	// against is already on disk, written by a build that had no such door.
	//
	// Unpaired on purpose. A preceding 'down' anchors the outage at ITS timestamp,
	// the crafted duration never reaches the day loop, and the clamp under test is
	// unreachable - so the shape that exercises it is the stray 'up' a corrupt
	// import leaves behind, which the loop starts at ts-duration_s.
	// ~31,700 years of "downtime": pre-clamp this is ~1.16e10 loop iterations.
	seedLegacyEvent(t, st, now.Add(-time.Minute).Unix(), "up", int64(1_000_000_000_000))
	type res struct {
		rows []DowntimeDay
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		rows, err := st.DowntimeByDay(ctx, now.Add(-24*time.Hour), time.UTC)
		ch <- res{rows, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("DowntimeByDay: %v", r.err)
		}
		// The queried window is 24h, so at most today plus a midnight-straddle: a
		// tiny, bounded result rather than tens of billions.
		if len(r.rows) > 2 {
			t.Fatalf("got %d day rows, want <= 2 - the day loop is not clamped to the window", len(r.rows))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DowntimeByDay did not return within 5s - the loop is unbounded")
	}
}

// The same suspend-shortened outage is counted only in windows overlapping its
// real (down-anchored) interval, not a later window that only the
// up.ts-duration_s placement would have touched; the total stays 1h.
func TestUptimeOutagePlacementAcrossSuspend(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	// Monitoring began 100h ago; outage confirmed 50h ago, recovered 45h ago
	// (5h wall gap) but only 1h observed down (4h suspend inside).
	sampleAt(t, st, now, 100*3600, "cf", "ipv4", true)
	eventAt(t, st, now, 50*3600, "down", -1)
	eventAt(t, st, now, 45*3600, "up", 3600)

	// A window starting 47h ago is entirely after the down-anchored interval
	// [50h, 49h ago], so it sees no downtime. The old up-anchored placement
	// [46h, 45h ago] would have fallen inside this window.
	narrow, err := ratioOf(st.UptimeSince(ctx, now.Add(-47*time.Hour), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, narrow, 1.0)

	// A window covering the whole outage still counts exactly the observed 1h,
	// so the total is preserved: 1 - 3600/(90*3600).
	wide, err := ratioOf(st.UptimeSince(ctx, now.Add(-90*time.Hour), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, wide, 1-1.0/90)
}

// Future-stamped samples (wrong boot clock, later stepped back) must not pin
// LatestPerTarget's freshness window: currently-probed targets keep reporting.
func TestLatestPerTargetIgnoresFutureAnchor(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, -3600, "ghost", "ipv4", true) // stamped 1h in the future
	sampleAt(t, st, now, 5, "cf", "ipv4", true)
	got, err := st.LatestPerTarget(context.Background(), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range got {
		if tl.Target == "cf" {
			return
		}
	}
	t.Fatalf("current target missing (future row pinned the window), got %+v", got)
}

// Wide-window Series results are cached briefly (they re-aggregate the raw
// table but change only once per bucket); a Clear must drop the cache so
// deleted data disappears immediately.
func TestSeriesCacheInvalidatedByClear(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ins := func(ts int64) {
		t.Helper()
		if err := st.InsertSamples(ctx, []Sample{{
			TS: time.Unix(ts, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
		}}); err != nil {
			t.Fatal(err)
		}
	}
	ins(100)
	pts, err := st.Series(ctx, time.Unix(0, 0), time.Time{}, 300, nil)
	if err != nil || len(pts) != 1 {
		t.Fatalf("first read: err=%v pts=%d", err, len(pts))
	}
	// Cached: an insert inside the TTL isn't visible yet for the same key.
	ins(400)
	if pts, _ = st.Series(ctx, time.Unix(0, 0), time.Time{}, 300, nil); len(pts) != 1 {
		t.Fatalf("expected the cached result (1 bucket), got %d", len(pts))
	}
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatal(err)
	}
	if pts, _ = st.Series(ctx, time.Unix(0, 0), time.Time{}, 300, nil); len(pts) != 0 {
		t.Fatalf("cache must be dropped on Clear, got %d buckets", len(pts))
	}
}

// Re-opening an existing DB re-runs the add-column migrations; the expected
// "duplicate column name" errors are tolerated, so Open must succeed and the
// data must remain usable.
func TestOpenMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.InsertSamples(context.Background(), []Sample{{
		TS: time.Now(), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 1,
	}}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	st.Close()
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-open must tolerate duplicate-column migrations: %v", err)
	}
	defer st2.Close()
	if cnt, _ := st2.TableCounts(context.Background()); cnt["samples"] != 1 {
		t.Fatalf("samples after re-open = %d, want 1", cnt["samples"])
	}
}

// The on-disk DB holds secrets (auth hash, tokens, webhook URLs), so the data
// dir and the file must be owner-only.
func TestOpenSecuresPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Mode().Perm() is synthetic on Windows; protection is via a DACL, not mode bits")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "pingularity.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	// A write to exercise the DB; the owner-only perms are set in Open, not here.
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatalf("write: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("data dir perms = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("db file perms = %o, want 600", fi.Mode().Perm())
	}
}

// A long-lived DB created at an OLD schema (before later ADD COLUMN migrations)
// must upgrade cleanly on Open, and reads/writes naming the added columns must
// then work - guarding against drift between the CREATE TABLE schema const and
// the migrations list, which would ship a broken upgrade that fresh installs
// (and the whole test suite, all starting from the current schema) never hit.
func TestOpenMigratesLegacySchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Hand-create tables at a pre-migration schema: speed without the per-run
	// connection / latency-under-load columns, samples without the family column.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE speed (ts INTEGER NOT NULL, down_mbps REAL, up_mbps REAL, ping_ms REAL, server TEXT);
		CREATE TABLE samples (ts INTEGER NOT NULL, target TEXT NOT NULL, latency_ms REAL, success INTEGER NOT NULL);
		INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server) VALUES (100, 50, 10, 20, 'old');
		INSERT INTO samples (ts, target, latency_ms, success) VALUES (100, 'cf', 12, 1);`); err != nil {
		t.Fatalf("seed legacy schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// A write naming migrated columns must succeed (they now exist).
	if err := st.InsertSpeed(ctx, SpeedSample{TS: 200, DownMbps: 99, Engine: "iperf3", ISP: "EBOX"}); err != nil {
		t.Fatalf("insert speed after migrate: %v", err)
	}
	sp, err := st.LatestSpeed(ctx)
	if err != nil || sp == nil {
		t.Fatalf("latest speed: %v (nil=%v)", err, sp == nil)
	}
	if sp.Engine != "iperf3" || sp.ISP != "EBOX" {
		t.Fatalf("migrated columns not round-tripping: engine=%q isp=%q", sp.Engine, sp.ISP)
	}
	// The legacy row must still read (its new columns default NULL/empty).
	if _, err := st.SpeedHistory(ctx, time.Unix(0, 0)); err != nil {
		t.Fatalf("read legacy speed rows: %v", err)
	}
	// samples.family migration: an insert naming family and a family-grouped read.
	if err := st.InsertSamples(ctx, []Sample{{TS: time.Unix(300, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}}); err != nil {
		t.Fatalf("insert sample after migrate: %v", err)
	}
	if _, err := st.Series(ctx, time.Unix(0, 0), time.Time{}, 1, nil); err != nil {
		t.Fatalf("series after migrate: %v", err)
	}
}

// The most important retention-correctness case: an orphaned 'down' whose only
// recovery evidence is samples must survive those samples being pruned. At prune
// time the store synthesizes the closing 'up', so the 1y/all uptime windows stay
// correct even with recCache cold (as after a restart past the sample horizon).
func TestUptimeOrphanGapSurvivesSamplePrune(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 100000, "cf", "ipv4", true) // baseline / window anchor
	eventAt(t, st, now, 90000, "down", -1)           // recovered while the monitor was off
	sampleAt(t, st, now, 89000, "cf", "ipv4", true)  // quorum recovery, 1000s after the down
	eventAt(t, st, now, 1000, "down", -1)            // next real outage -> the 90000 down is an orphan
	eventAt(t, st, now, 500, "up", 500)

	// Warm: orphan gap bounded at the recovery (1000s) + closed outage (500s).
	up, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1-1500.0/100000.0)

	// Prune every sample older than an hour: the recovery evidence is deleted.
	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st.invalidateReadCaches() // simulate a restart: cold recCache

	up, err = ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	// Still 1500s down, NOT the whole ~89000s gap that would appear without the
	// synthetic recovery.
	approx(t, up, 1-1500.0/100000.0)
}

// A dangling FINAL 'down' whose recovery is proven only by samples is the worst
// case (its phantom downtime grows as retention advances): the prune-time
// synthetic recovery must close it too.
func TestUptimeDanglingFinalDownSurvivesSamplePrune(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 100000, "cf", "ipv4", true)
	eventAt(t, st, now, 90000, "down", -1)          // last event stays a 'down'
	sampleAt(t, st, now, 89000, "cf", "ipv4", true) // quorum recovery 1000s later

	up, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1-1000.0/100000.0)

	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st.invalidateReadCaches()

	up, err = ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1-1000.0/100000.0)
}

// Monitoring paused mid-outage writes no samples, so the dangling-'down' branch
// must not book the unwatched pause as live downtime (the eventual 'up' event
// excludes it too): the outage is bounded at the last observed sample, not now.
func TestUptimePausedMidOutageNotCounted(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // baseline / window start
	eventAt(t, st, now, 600, "down", -1)           // outage confirmed
	sampleAt(t, st, now, 580, "cf", "ipv4", false) // observed down
	sampleAt(t, st, now, 560, "cf", "ipv4", false) // last observation before the pause
	// ...then monitoring is paused: no samples after now-560, no closing 'up'.

	up, err := ratioOf(st.UptimeSince(ctx, now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// Only the observed-down stretch [now-600, now-560] = 40s counts, NOT the whole
	// [now-600, now] = 600s that would accrue if the pause were counted as down.
	approx(t, up, 1-40.0/1000.0)
}

// A genuinely ongoing outage keeps writing failed samples, so bounding at the
// newest sample must still count downtime through ~now (no regression from the
// pause fix).
func TestUptimeOngoingOutageStillCountsToNow(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 500, "down", -1)
	// Failed samples continue right up to ~now (the monitor is still probing).
	for _, ago := range []int{480, 300, 100, 2} {
		sampleAt(t, st, now, ago, "cf", "ipv4", false)
	}
	up, err := ratioOf(st.UptimeSince(ctx, now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// Down from now-500 to ~now (newest failed sample at now-2) ~= 500s of 1000s.
	approx(t, up, 0.5)
}

// A settings import row whose value decoded to a JSON object/array must be
// skipped like any other corrupt row - not bubble an unsupported-type driver
// error that aborts the whole (half-applied) restore.
func TestImportSkipsJSONContainerValue(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ts := float64(time.Now().Unix())
	rows := []map[string]any{
		{"ts": ts, "type": "down", "detail": map[string]any{"note": "x"}}, // object value -> skip
		{"ts": ts + 1, "type": "up", "detail": []any{1, 2, 3}},            // array value -> skip
		{"ts": ts + 2, "type": "up", "duration_s": float64(30), "detail": "clean"},
	}
	n, err := st.ImportTable(ctx, "events", rows)
	if err != nil {
		t.Fatalf("import must not abort on a container value: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported %d events, want 1 (object/array rows skipped, clean row kept)", n)
	}
	if cnt, _ := st.TableCounts(ctx); cnt["events"] != 1 {
		t.Fatalf("events = %d, want 1", cnt["events"])
	}
}

// UptimeSince clamps computed downtime to the [since, now] window: a corrupt
// duration_s must not blow the fraction far negative. The SQL uses
// MIN(o_start + duration_s, now); this proves it.
//
// Seeded with raw SQL because that is now the only way such a row EXISTS: every
// door bounds the length (eventRowSane on import, repairInsaneEventDurations at
// rest, InsertEvent on the live path). The clamp is still the thing under test -
// it is what stands between the readers and a row that predates those doors, and
// it must keep standing whether or not anything can add a new one.
func TestUptimeSinceClampsCraftedDuration(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	// Down 120s ago, "recovered" 60s ago but with an absurd ~31,700-year duration.
	eventAt(t, st, now, 120, "down", -1)
	seedLegacyEvent(t, st, now.Add(-60*time.Second).Unix(), "up", int64(1_000_000_000_000))
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	// ~120s down out of 1000s -> ~0.88, clamped to now. Unclamped this would be
	// a massively negative fraction.
	if up < 0.80 || up > 0.95 {
		t.Fatalf("uptime = %v, want ~0.88 (downtime clamped to now); a wild value means duration_s escaped the clamp", up)
	}
}

// The Series cache key includes the window END. Without it a fixed historical
// window and a rolling one that share a start bucket and bucket width collide
// in the 32-entry map and serve each other data - silently wrong, with no
// visible break to give it away.
func TestSeriesCacheKeyedOnWindowEnd(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	// Two clusters an hour apart, so a bounded and an unbounded window over the
	// same start must differ.
	for i := 0; i < 3; i++ {
		sampleAt(t, st, base.Add(time.Duration(i)*time.Minute), 10, "a", "ipv4", true)
		sampleAt(t, st, base.Add(2*time.Hour+time.Duration(i)*time.Minute), 20, "a", "ipv4", true)
	}
	// bucketSec >= 60 is the cached path (below that Series queries live).
	const bucket = 60
	bounded, err := st.Series(ctx, base, base.Add(time.Hour), bucket, nil)
	if err != nil {
		t.Fatal(err)
	}
	open, err := st.Series(ctx, base, time.Time{}, bucket, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounded) == 0 || len(open) == 0 {
		t.Fatalf("empty result: bounded=%d open=%d", len(bounded), len(open))
	}
	if len(bounded) >= len(open) {
		t.Errorf("bounded window returned %d points and the open one %d: the bounded "+
			"result must be smaller, so the two are not sharing a cache entry",
			len(bounded), len(open))
	}
	// And the bounded one really stops at its end.
	for _, p := range bounded {
		if p.TS >= base.Add(time.Hour).Unix() {
			t.Errorf("point %d is past the window end %d", p.TS, base.Add(time.Hour).Unix())
		}
	}
}

// The cache keys the window END exactly, not floored to its bucket. Flooring it
// aliased two windows whose ends fall in the same bucket onto one entry, and
// their trailing partial bucket aggregates different rows - so the second caller
// was served the first one's data.
func TestSeriesCacheDoesNotAliasNearbyEnds(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const bucket = 60
	base := time.Now().Add(-30 * 24 * time.Hour).Truncate(bucket * time.Second)
	// Two samples in the SAME bucket, the later one much faster, so a window that
	// reaches it reports a different MIN for that bucket.
	for _, x := range []struct {
		off time.Duration
		ms  float64
	}{{0, 50}, {40 * time.Second, 5}} {
		if err := st.InsertSamples(ctx, []Sample{{
			TS: base.Add(x.off), Target: "a", Family: "ipv4", Success: true, LatencyMS: x.ms,
		}}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	minOf := func(until time.Duration) float64 {
		t.Helper()
		pts, err := st.Series(ctx, base, base.Add(until), bucket, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(pts) == 0 || pts[0].LatencyMS == nil {
			t.Fatalf("no latency for window ending +%v", until)
		}
		return *pts[0].LatencyMS
	}
	early := minOf(20 * time.Second) // stops before the fast sample
	late := minOf(50 * time.Second)  // includes it
	if early != 50 || late != 5 {
		t.Errorf("window ending +20s reports %.0f ms and +50s reports %.0f ms; want 50 and 5 "+
			"(equal values mean the two ends fell in one bucket and shared a cache entry)", early, late)
	}
}

// pauseAt records a pause span [now-secsAgo, +durS) - unobserved wall time.
func pauseAt(t *testing.T, st *Store, now time.Time, secsAgo int, durS int64) {
	t.Helper()
	if _, err := st.InsertPause(context.Background(), now.Add(-time.Duration(secsAgo)*time.Second), durS); err != nil {
		t.Fatalf("insert pause: %v", err)
	}
}

// Paused time is excluded from the denominator: a 1000s window with 500s paused
// and no outages is 100% up over the 500s OBSERVED, with 0.5 coverage (F2).
func TestUptimePausedExcludedFromDenominator(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // anchors monitoringSince at now-1000
	pauseAt(t, st, now, 800, 500)                  // paused [now-800, now-300]: 500s unobserved
	o, err := st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	up, cov := o.Ratio(), o.Coverage()
	approx(t, up, 1.0)  // no outages; the paused span is neither up nor down
	approx(t, cov, 0.5) // 500 observed / 1000 wall
}

// A paused span around an outage cancels in both numerator and denominator: the
// outage's recorded duration already excludes paused time, and the denominator
// drops the same span, so the ratio stays honest.
func TestUptimeOutageWithPause(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 600, "down", -1)
	eventAt(t, st, now, 500, "up", 100) // 100s observed down
	pauseAt(t, st, now, 400, 400)       // 400s paused while up
	o, err := st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	up, cov := o.Ratio(), o.Coverage()
	approx(t, up, 1-100.0/600.0) // 100s down / 600s observed (1000 wall - 400 paused)
	approx(t, cov, 0.6)
}

// A window that observed nothing (fully paused, or probing never ran) reports 0
// coverage so the caller omits the uptime figure instead of a misleading 100% (F2).
func TestUptimeFullyPausedZeroCoverage(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 3600, "cf", "ipv4", true) // monitoringSince = now-3600
	pauseAt(t, st, now, 3600, 3600)                // entire window paused
	o, err := st.UptimeSince(context.Background(), now.Add(-3600*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	up, cov := o.Ratio(), o.Coverage()
	approx(t, cov, 0.0)
	_ = up // ratio is meaningless at 0 coverage; the caller omits it
}

// retention clamps the window start so it can't reach past where outage events are
// pruned: an outage older than the retention horizon is excluded from BOTH the
// denominator and numerator, preventing the all-window optimistic drift (F3).
func TestUptimeRetentionClamp(t *testing.T) {
	st := open(t)
	now := time.Now()
	// Activity 10 days ago (install anchor) and an outage 8 days ago.
	sampleAt(t, st, now, 10*86400, "cf", "ipv4", true)
	sampleAt(t, st, now, 60, "cf", "ipv4", true)
	eventAt(t, st, now, 8*86400+100, "down", -1)
	eventAt(t, st, now, 8*86400, "up", 100) // 100s outage, 8 days ago

	// With no retention clamp, "all" runs from the 10-day anchor and includes the outage.
	full, err := ratioOf(st.UptimeSince(context.Background(), time.Unix(0, 0), 0))
	if err != nil {
		t.Fatal(err)
	}
	// With a 7-day retention clamp, the window starts 7 days ago - the 8-day-old
	// outage is now outside it, so uptime reads 100% over the retained period.
	co, err := st.UptimeSince(context.Background(), time.Unix(0, 0), 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clamped, cov := co.Ratio(), co.Coverage()
	if !(full < clamped) {
		t.Fatalf("retention clamp should drop the old outage: full=%.5f clamped=%.5f", full, clamped)
	}
	approx(t, clamped, 1.0)
	if cov <= 0 {
		t.Fatalf("clamped window should have observation coverage, got %.3f", cov)
	}
}

// A pause INSIDE a restart-mid-outage gap (down->down, no recovery samples) must be
// counted once: excluded from the observed denominator AND not booked as downtime.
// Before the fix, orphanGapDowntime booked the whole gap (incl. paused seconds) as
// downtime while the same seconds were subtracted from the denominator - double-count
// (audit: orphan-gap-pause-double-count).
// A 'down' and 'up' sharing a timestamp (a sub-second outage rounded to one
// second, or colliding imported events) must pair as a completed 0s outage, not
// leave the 'down' dangling. The ORDER BY tiebreaker (down sorts before up at a
// tie) makes this deterministic regardless of insertion/rowid order — here the
// 'up' is inserted FIRST, so a plain ORDER BY ts would return it before the
// 'down' and count the outage as un-recovered through to now.
func TestUptimeEventTiebreakerSameTS(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 500, "up", 0)    // recovery inserted first (lower rowid)
	eventAt(t, st, now, 500, "down", -1) // outage start at the SAME second
	up, err := ratioOf(st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1.0) // 0s downtime; the bug leaves the down dangling → ~0.5
}

// When the last thing recorded before shutdown is a pause span, LastObservedTS
// must anchor at the pause's END, not the older last sample — otherwise the
// startup gap re-books time the pause already covers.
func TestLastObservedTSConsidersPauseEnd(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // last real activity: now-1000
	pauseAt(t, st, now, 500, 400)                  // pause [now-500, now-100]
	last, ok, err := st.LastObservedTS(context.Background())
	if err != nil || !ok {
		t.Fatalf("LastObservedTS: ok=%v err=%v", ok, err)
	}
	want := now.Add(-100 * time.Second).Unix() // the pause END, not now-1000
	if last < want-2 || last > want+2 {
		t.Fatalf("LastObservedTS=%d, want ~%d (pause end); bug returns the older sample at %d",
			last, want, now.Add(-1000*time.Second).Unix())
	}
}

func TestUptimeOrphanGapExcludesPause(t *testing.T) {
	st := open(t)
	now := time.Now()
	sampleAt(t, st, now, 1000, "a", "ipv4", true) // anchor monitoringSince at now-1000
	eventAt(t, st, now, 600, "down", -1)          // outage starts
	eventAt(t, st, now, 300, "down", -1)          // restart mid-outage re-detects it (2nd down)
	eventAt(t, st, now, 100, "up", 200)           // recovers; completed portion [-300,-100]=200s
	pauseAt(t, st, now, 500, 200)                 // pause [-500,-300] sits INSIDE the [-600,-300] gap
	o, err := st.UptimeSince(context.Background(), now.Add(-1000*time.Second), 0)
	if err != nil {
		t.Fatal(err)
	}
	up, cov := o.Ratio(), o.Coverage()
	// observed = 1000 - 200 paused = 800; observed downtime = 200 (completed) + (300-200) (gap
	// minus its interior pause) = 300; ratio = 1 - 300/800 = 0.625. The bug gives 0.375.
	approx(t, cov, 0.8)
	approx(t, up, 0.625)
}
