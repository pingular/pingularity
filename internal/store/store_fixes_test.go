package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/osperm"
	"github.com/pingular/pingularity/internal/stats"
)

// A corrupt or non-database file at the DB path must not crash-loop the daemon
// (systemd's Restart=always would re-hit the same bad file forever). Open moves
// it aside to a .corrupt file and rebuilds an empty, usable store.
func TestOpenRecoversFromCorruptDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.db")
	// Non-SQLite bytes: the driver reports "file is not a database".
	if err := os.WriteFile(path, []byte("this is not a sqlite database - a power cut truncated it"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open must recover from a corrupt DB, got: %v", err)
	}
	defer st.Close()
	// The rebuilt store is usable.
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatalf("fresh store not usable after recovery: %v", err)
	}
	// The corrupt file was quarantined, not deleted.
	matches, _ := filepath.Glob(path + ".*.corrupt")
	if len(matches) == 0 {
		t.Fatal("corrupt DB was not quarantined to a .corrupt file")
	}
	// A live DB now exists at the original path.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no fresh DB at the original path: %v", err)
	}
}

// dbCorrupt classifies SQLite's corruption/non-database messages and nothing
// else (a busy/locked or plain I/O error must not trip the quarantine path).
func TestDBCorruptClassification(t *testing.T) {
	corrupt := []string{
		"database disk image is malformed",
		"file is not a database",
		"database corruption at line 1",
	}
	for _, m := range corrupt {
		if !dbCorrupt(errors.New(m)) {
			t.Errorf("dbCorrupt(%q) = false, want true", m)
		}
	}
	notCorrupt := []string{"database is locked", "database is busy", "context canceled", ""}
	for _, m := range notCorrupt {
		if dbCorrupt(errors.New(m)) {
			t.Errorf("dbCorrupt(%q) = true, want false", m)
		}
	}
	if dbCorrupt(nil) {
		t.Error("dbCorrupt(nil) = true, want false")
	}
}

// recordDBErr must classify a malformed/corruption message as db.corrupt, not
// db.disk_full: SQLite's "database disk image is malformed" also contains "disk",
// so the corrupt arm has to be tested first.
func TestRecordDBErrCorruptBeatsDisk(t *testing.T) {
	stats.ResetForTest()
	recordDBErr(errors.New("database disk image is malformed"))
	snap := stats.Lifetime()
	if snap.Counters["db.corrupt"] != 1 {
		t.Errorf("db.corrupt = %d, want 1", snap.Counters["db.corrupt"])
	}
	if snap.Counters["db.disk_full"] != 0 {
		t.Errorf("db.disk_full = %d, want 0 (malformed must not read as a full disk)", snap.Counters["db.disk_full"])
	}
	// A genuine full-disk message still lands on db.disk_full.
	stats.ResetForTest()
	recordDBErr(errors.New("disk is full"))
	if got := stats.Lifetime().Counters["db.disk_full"]; got != 1 {
		t.Errorf("db.disk_full = %d, want 1 for a real full-disk error", got)
	}
	// A disk I/O error (failing storage) contains "disk" but is neither corruption
	// nor a full disk: it must land on db.io_err, not db.disk_full.
	stats.ResetForTest()
	recordDBErr(errors.New("disk I/O error"))
	snap = stats.Lifetime()
	if snap.Counters["db.io_err"] != 1 {
		t.Errorf("db.io_err = %d, want 1 for a disk I/O error", snap.Counters["db.io_err"])
	}
	if snap.Counters["db.disk_full"] != 0 {
		t.Errorf("db.disk_full = %d, want 0 (an I/O error must not read as a full disk)", snap.Counters["db.disk_full"])
	}
}

// securingSkippable treats a chmod-hostile filesystem (read-only mount, no unix
// perms) and a missing sidecar as best-effort, but not a real I/O fault.
func TestSecuringSkippable(t *testing.T) {
	skip := []error{
		os.ErrNotExist,
		os.ErrPermission,
		errors.New("chmod /mnt/usb/ping.db: read-only file system"),
		errors.New("operation not supported"),
		errors.New("operation not permitted"),
	}
	for _, e := range skip {
		if !securingSkippable(e) {
			t.Errorf("securingSkippable(%v) = false, want true", e)
		}
	}
	if securingSkippable(errors.New("input/output error")) {
		t.Error("securingSkippable(I/O error) = true, want false (a real fault must still fail)")
	}
}

// B12a: securing a world-readable DB must actually tighten it (and the verify
// step must then confirm it is no longer group/world accessible). A skippable
// securing error that LEFT the file exposed must be fatal, not silently ignored.
func TestSecureDBFileTightensAndVerifies(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only is a DACL on Windows, not Unix mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "ping.db")
	if err := os.WriteFile(path, []byte("x"), 0o666); err != nil { // world-readable
		t.Fatal(err)
	}
	if exposed, known := osperm.GroupOrWorldAccessible(path); !known || !exposed {
		t.Fatalf("setup: fresh 0666 file should read as exposed (known=%v exposed=%v)", known, exposed)
	}
	if err := secureDBFile(path); err != nil {
		t.Fatalf("secureDBFile on a chmod-able file: %v", err)
	}
	if exposed, known := osperm.GroupOrWorldAccessible(path); !known || exposed {
		t.Fatalf("after secureDBFile the DB is still exposed (known=%v exposed=%v)", known, exposed)
	}
	// A missing sidecar reads as unknown, so securing it is a clean no-op.
	if err := secureDBFile(path + "-wal"); err != nil {
		t.Fatalf("securing a missing sidecar should be a no-op, got %v", err)
	}
}

// firstQuorumRecovery requires a STRICT majority (SUM(success)*2 > COUNT(*)):
// exactly half up is not a quorum; two of three is. Series' online flag uses the
// same predicate.
func TestQuorumRequiresStrictMajority(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	after := now.Add(-200 * time.Second).Unix()
	before := now.Unix()

	// Exactly half up (1 of 2) at now-100 is NOT a quorum.
	sampleAt(t, st, now, 100, "a", "ipv4", true)
	sampleAt(t, st, now, 100, "b", "ipv4", false)
	if rec, ok, err := st.firstQuorumRecovery(ctx, after, before); err != nil || ok {
		t.Fatalf("1-up/1-down is exactly half, not a quorum: rec=%d ok=%v err=%v", rec, ok, err)
	}
	pts, err := st.Series(ctx, now.Add(-200*time.Second), time.Time{}, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].Online {
		t.Fatalf("half-up bucket must read offline, got %+v", pts)
	}

	// Two of three up at now-50 IS a strict majority.
	sampleAt(t, st, now, 50, "c", "ipv4", true)
	sampleAt(t, st, now, 50, "d", "ipv4", true)
	sampleAt(t, st, now, 50, "e", "ipv4", false)
	rec, ok, err := st.firstQuorumRecovery(ctx, after, before)
	if err != nil || !ok {
		t.Fatalf("2-of-3 up is a strict majority (quorum): ok=%v err=%v", ok, err)
	}
	if want := now.Add(-50 * time.Second).Unix(); rec != want {
		t.Fatalf("recovery second = %d, want %d (the 2/3 second, not the 1/2 one)", rec, want)
	}
}

// UptimeSince clamps summed downtime to the window: overlapping outage pieces (a
// completed outage plus an orphaned down->down gap) can exceed the window, and
// the `if downtime > window` guard must floor the fraction at 0, never negative.
func TestUptimeSinceClampedToZero(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // window anchor at now-1000
	// Orphaned down->down gap with no recovery between: [now-500, now-100] = 400s.
	eventAt(t, st, now, 500, "down", -1)
	eventAt(t, st, now, 100, "down", -1)
	// A completed outage whose crafted duration, anchored by fallback (its LAG is
	// an 'up', not a 'down'), overlaps the whole window - so the pieces sum past it.
	eventAt(t, st, now, 99, "up", 98)    // closes the now-100 down
	eventAt(t, st, now, 1, "up", 100000) // LAG is the up above -> o_start = ts-dur, far back

	up, err := ratioOf(st.UptimeSince(ctx, now.Add(-1000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	if up < 0 || up > 1 {
		t.Fatalf("uptime %v out of [0,1]: the downtime>window clamp did not fire", up)
	}
	if up != 0 {
		t.Fatalf("uptime = %v, want exactly 0 (summed downtime exceeds the window and clamps)", up)
	}
}

// The wide-window Series cache must not pin an empty first-run result: a fresh
// install opening a wide/open-ended range once would otherwise freeze an empty
// chart for a quarter-bucket. An empty result is never cached, so the first
// samples show immediately.
func TestSeriesDoesNotPinEmptyOpenWindow(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const bucket = 3600
	since := time.Now().Add(-365 * 24 * time.Hour)
	if pts, err := st.Series(ctx, since, time.Time{}, bucket, nil); err != nil || len(pts) != 0 {
		t.Fatalf("first read on an empty table: err=%v pts=%d, want 0", err, len(pts))
	}
	// A probe lands right after.
	if err := st.InsertSamples(ctx, []Sample{{
		TS: time.Now(), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
	}}); err != nil {
		t.Fatal(err)
	}
	pts, err := st.Series(ctx, since, time.Time{}, bucket, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) == 0 {
		t.Fatal("empty open-ended window was cached and pinned the fresh-install chart")
	}
}

// A multi-day outage viewed in a zone whose DST jump lands at midnight
// (America/Havana) must not collapse onto a single day: the boundary is advanced
// to the next date's first EXISTING instant, so every offline day is marked and
// no cell exceeds 24h.
func TestDowntimeByDayDSTSkippedMidnight(t *testing.T) {
	ha, err := time.LoadLocation("America/Havana")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	st := open(t)
	ctx := context.Background()
	down := time.Date(2025, 3, 7, 12, 0, 0, 0, time.UTC)
	up := time.Date(2025, 3, 12, 12, 0, 0, 0, time.UTC)
	if err := st.InsertEvent(ctx, down, "down", -1, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, up, "up", int(up.Sub(down).Seconds()), ""); err != nil {
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, down.Add(-24*time.Hour), ha)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	total := 0
	for _, r := range rows {
		if r.DowntimeS > 24*3600 {
			t.Errorf("day %s books %ds (> 24h): the skipped-midnight boundary collapsed the outage", r.Date, r.DowntimeS)
		}
		seen[r.Date] = r.DowntimeS
		total += r.DowntimeS
	}
	for _, d := range []string{"2025-03-08", "2025-03-09", "2025-03-10", "2025-03-11"} {
		if _, ok := seen[d]; !ok {
			t.Errorf("offline day %s missing (collapsed by the DST boundary bug)", d)
		}
	}
	// Total downtime is preserved regardless of the DST arithmetic (wall seconds).
	if want := int(up.Sub(down).Seconds()); total != want {
		t.Errorf("total downtime = %ds, want %ds (the full outage length)", total, want)
	}
}

// The orphaned down->down gap that UptimeSince counts must also appear in
// ResolvedOutagesSince and DowntimeByDay, so the digest and heatmap don't
// under-report downtime; and a restart re-detecting the SAME outage must not be
// counted as a second outage.
func TestOrphanGapConsistentAcrossAggregators(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 10000, "cf", "ipv4", true) // window anchor
	eventAt(t, st, now, 1000, "down", -1)           // D1
	eventAt(t, st, now, 700, "down", -1)            // D2: restart re-detect, no recovery between
	eventAt(t, st, now, 500, "up", 200)             // closes D2: [now-700, now-500]

	// ResolvedOutagesSince: 200s (closed) + 300s (orphan gap D1->D2) = 500s, one outage.
	c, d, err := st.ResolvedOutagesSince(ctx, now.Add(-10000*time.Second).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if c != 1 || d != 500 {
		t.Fatalf("ResolvedOutagesSince = count %d downtime %d, want 1/500 (orphan gap included)", c, d)
	}

	// DowntimeByDay books the same 500s and counts the restart re-detection once.
	rows, err := st.DowntimeByDay(ctx, now.Add(-10000*time.Second), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var totalDown, totalOut int
	for _, r := range rows {
		totalDown += r.DowntimeS
		totalOut += r.Outages
	}
	if totalDown != 500 || totalOut != 1 {
		t.Fatalf("DowntimeByDay totals = %ds / %d outages, want 500s / 1 outage", totalDown, totalOut)
	}

	// And all three reconcile with the uptime fraction: 500s down of 10000s.
	up, err := ratioOf(st.UptimeSince(ctx, now.Add(-10000*time.Second), 0))
	if err != nil {
		t.Fatal(err)
	}
	approx(t, up, 1-500.0/10000.0)
}

// A dangling 'down' whose outage recovered while the monitor was off is a
// DISTINCT earlier outage (a quorum recovery falls between it and the next
// 'down'), so DowntimeByDay must count it as its own outage - unlike a restart
// re-detection.
func TestDowntimeByDayCountsRecoveredOrphanSeparately(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 10000, "cf", "ipv4", true) // anchor
	eventAt(t, st, now, 5000, "down", -1)           // outage 1: recovered while off
	// quorum recovery (2 of 3) at now-4800
	sampleAt(t, st, now, 4800, "a", "ipv4", true)
	sampleAt(t, st, now, 4800, "b", "ipv4", true)
	sampleAt(t, st, now, 4800, "c", "ipv4", false)
	eventAt(t, st, now, 1000, "down", -1) // outage 2: separate
	eventAt(t, st, now, 800, "up", 200)

	rows, err := st.DowntimeByDay(ctx, now.Add(-10000*time.Second), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	var totalOut int
	for _, r := range rows {
		totalOut += r.Outages
	}
	if totalOut != 2 {
		t.Fatalf("a recovered-while-off orphan is a distinct outage: got %d outages, want 2", totalOut)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// sumDowntimeDays totals the per-day downtime and outage counts.
func sumDowntimeDays(rows []DowntimeDay) (downS, outages int) {
	for _, r := range rows {
		downS += r.DowntimeS
		outages += r.Outages
	}
	return
}

// impliedDowntime is the downtime (seconds) UptimeSince attributes over a window.
func impliedDowntime(t *testing.T, st *Store, since time.Time, windowS float64) int {
	t.Helper()
	up, err := ratioOf(st.UptimeSince(context.Background(), since, 0))
	if err != nil {
		t.Fatal(err)
	}
	return int(((1 - up) * windowS) + 0.5)
}

// B4: an ACTIVE outage (a trailing 'down' with no closing 'up', still writing
// failed samples) must be booked by DowntimeByDay to the last observed sample,
// matching UptimeSince. Before the fix DowntimeByDay ignored the trailing 'down'
// entirely and reported zero downtime for an ongoing outage.
func TestDowntimeByDayActiveOutageMatchesUptime(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true) // window anchor
	eventAt(t, st, now, 500, "down", -1)           // outage begins, never closed
	for _, ago := range []int{480, 300, 100, 2} {  // failed samples up to ~now (still down)
		sampleAt(t, st, now, ago, "cf", "ipv4", false)
	}
	rows, err := st.DowntimeByDay(ctx, now.Add(-1000*time.Second), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	down, out := sumDowntimeDays(rows)
	implied := impliedDowntime(t, st, now.Add(-1000*time.Second), 1000)
	if abs(down-implied) > 3 {
		t.Fatalf("DowntimeByDay active-outage downtime = %ds, want ~%ds (UptimeSince)", down, implied)
	}
	if out != 1 {
		t.Fatalf("active outage should count as 1 outage, got %d", out)
	}
}

// B4: an outage whose leading 'down' predates the window (a boundary-spanning
// restart interruption) must still contribute its in-window downtime. Fetching
// only events strictly inside the window missed the leading 'down', so the
// orphan-gap stretch went unbooked and DowntimeByDay disagreed with UptimeSince.
func TestDowntimeByDayBoundarySpanningOutageMatchesUptime(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 2000, "cf", "ipv4", true) // monitoring anchor (pre-window)
	eventAt(t, st, now, 1000, "down", -1)          // D1: BEFORE the window
	eventAt(t, st, now, 500, "down", -1)           // D2: in window, restart re-detect (no recovery between)
	eventAt(t, st, now, 300, "up", 200)            // closes: [now-500, now-300]

	since := now.Add(-800 * time.Second) // window starts AFTER D1
	rows, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	down, _ := sumDowntimeDays(rows)
	implied := impliedDowntime(t, st, since, 800)
	// In-window downtime = orphan gap [since, D2]=300s + closed [D2, up]=200s = 500s.
	if abs(down-implied) > 3 || abs(down-500) > 3 {
		t.Fatalf("boundary-spanning downtime = %ds, want ~500s and == UptimeSince ~%ds", down, implied)
	}
}

// B4: a monitoring pause mid-outage (failed samples then silence, no closing
// 'up') must bound the active outage at the last observed sample in DowntimeByDay
// too, so an unwatched pause is not booked as live downtime - matching
// TestUptimePausedMidOutageNotCounted's UptimeSince behaviour.
func TestDowntimeByDayPausedMidOutageMatchesUptime(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 1000, "cf", "ipv4", true)
	eventAt(t, st, now, 600, "down", -1)
	sampleAt(t, st, now, 580, "cf", "ipv4", false)
	sampleAt(t, st, now, 560, "cf", "ipv4", false) // last observation before the pause
	rows, err := st.DowntimeByDay(ctx, now.Add(-1000*time.Second), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	down, out := sumDowntimeDays(rows)
	implied := impliedDowntime(t, st, now.Add(-1000*time.Second), 1000)
	// Only [now-600, now-560] = 40s, not the whole [now-600, now].
	if abs(down-40) > 3 || abs(down-implied) > 3 {
		t.Fatalf("paused-mid-outage downtime = %ds, want ~40s and == UptimeSince ~%ds", down, implied)
	}
	if out != 1 {
		t.Fatalf("paused outage should count as 1 outage, got %d", out)
	}
}

// B5 reconciliation fixture: after a restart interruption where the leading
// outage recovered while the monitor was off (a quorum recovery falls between the
// two 'down' events), the digest's resolved-outage COUNT must match the heatmap's
// - both count TWO distinct outages, even though only the second carries a closing
// 'up' event. Before the fix ResolvedOutagesSince counted only the 'up' (one).
func TestDigestOutageCountMatchesHeatmapAfterRestart(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 10000, "cf", "ipv4", true) // anchor
	eventAt(t, st, now, 5000, "down", -1)           // outage 1: recovered while the monitor was off
	sampleAt(t, st, now, 4800, "a", "ipv4", true)   // quorum recovery (2 of 3 up)
	sampleAt(t, st, now, 4800, "b", "ipv4", true)
	sampleAt(t, st, now, 4800, "c", "ipv4", false)
	eventAt(t, st, now, 1000, "down", -1) // outage 2: separate, closed by an 'up'
	eventAt(t, st, now, 800, "up", 200)

	since := now.Add(-10000 * time.Second)
	c, dgDown, err := st.ResolvedOutagesSince(ctx, since.Unix())
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	hmDown, hmOut := sumDowntimeDays(rows)
	if c != 2 {
		t.Fatalf("digest resolved-outage count = %d, want 2 (recovered orphan is a distinct outage)", c)
	}
	if hmOut != 2 {
		t.Fatalf("heatmap outage count = %d, want 2", hmOut)
	}
	if c != hmOut {
		t.Fatalf("digest count %d disagrees with heatmap count %d after a restart interruption", c, hmOut)
	}
	// Downtime agrees too: [now-5000, now-4800]=200s + [now-1000, now-800]=200s = 400s.
	if abs(dgDown-400) > 3 || abs(hmDown-400) > 3 || abs(dgDown-hmDown) > 3 {
		t.Fatalf("downtime disagrees: digest=%ds heatmap=%ds, want ~400s each", dgDown, hmDown)
	}
}

// SpeedDataUsage sums download+upload bytes within each rolling window; the
// windows are strictly nested and 'all' is the greatest.
func TestSpeedDataUsageWindows(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	ins := func(ago time.Duration, bytes int64) {
		t.Helper()
		b := bytes
		if err := st.InsertSpeed(ctx, SpeedSample{TS: now.Add(-ago).Unix(), Server: "s", DownBytes: &b}); err != nil {
			t.Fatalf("insert speed: %v", err)
		}
	}
	// Distinct powers of two so every window sum is unambiguous.
	ins(1*time.Hour, 1)         // in every window
	ins(12*time.Hour, 2)        // 24h and wider
	ins(3*24*time.Hour, 4)      // 7d and wider
	ins(20*24*time.Hour, 8)     // 30d and wider
	ins(100*24*time.Hour, 16)   // 1y and wider
	ins(2*365*24*time.Hour, 32) // all only

	u, err := st.SpeedDataUsage(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := DataUsage{H6: 1, H24: 3, D7: 7, D30: 15, Y1: 31, All: 63}
	if u != want {
		t.Fatalf("SpeedDataUsage = %+v, want %+v", u, want)
	}
	if !(u.All > u.Y1 && u.Y1 > u.D30 && u.D30 > u.D7 && u.D7 > u.H24 && u.H24 > u.H6) {
		t.Fatalf("windows not strictly nested with All greatest: %+v", u)
	}
}

// SpeedDataUsageSince sums download+upload since t inclusively: a run stamped
// exactly at `since` is counted; one a second earlier is not.
func TestSpeedDataUsageSince(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	ins := func(ts int64, down, up int64) {
		t.Helper()
		d, u := down, up
		if err := st.InsertSpeed(ctx, SpeedSample{TS: ts, Server: "s", DownBytes: &d, UpBytes: &u}); err != nil {
			t.Fatalf("insert speed: %v", err)
		}
	}
	since := now.Add(-time.Hour)
	ins(since.Unix(), 10, 5)                      // exactly at since: counted (>= boundary)
	ins(since.Add(-time.Second).Unix(), 100, 100) // one second before since: excluded
	ins(now.Unix(), 3, 4)                         // inside the window

	got, err := st.SpeedDataUsageSince(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	// 15 (boundary run) + 7 (recent run) = 22; the pre-since run is excluded.
	if got != 22 {
		t.Fatalf("SpeedDataUsageSince = %d, want 22 (boundary counted, pre-since excluded)", got)
	}
}
