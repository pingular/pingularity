package main

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"

	_ "modernc.org/sqlite"
)

// THE PRUNER MUST NOT DELETE ON WINDOWS NOBODY CHOSE.
//
// When the boot-time settings read fails the controller keeps answering, but
// with the compiled-in defaults: a stored "keep forever" reads as thirty days.
// The daemon already refuses to WRITE settings and refuses to SERVE the UI on
// such a controller; the pruner is the one consumer whose reading drives an
// irreversible DELETE, so it has to refuse too - and refuse the PASS, not the
// job: the ticker has to keep re-checking, so the first pass after the read
// finally succeeds prunes on the stored windows. The store has to be one whose
// settings read fails while its DELETEs still succeed - a damaged page under
// the settings table, not a locked database. A second connection renames the
// table away, which is exactly that shape.

// prunerStore is a FILE-BACKED store plus an independent connection to the same
// file. The second connection is what makes the fault injectable (and lets the
// test count rows without touching the table it broke).
type prunerStore struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	db2 *sql.DB
}

func openPrunerStore(t *testing.T) *prunerStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	return &prunerStore{t: t, ctx: context.Background(), st: st, db2: db2}
}

func (f *prunerStore) count(table string) int64 {
	f.t.Helper()
	var n int64
	if err := f.db2.QueryRowContext(f.ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func (f *prunerStore) exec(q string) {
	f.t.Helper()
	if _, err := f.db2.ExecContext(f.ctx, q); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

// rows is the samples/speed/events row count triple the assertions compare.
func (f *prunerStore) rows() [3]int64 {
	f.t.Helper()
	return [3]int64{f.count("samples"), f.count("speed"), f.count("events")}
}

// seedHistory writes the rows the compiled-in windows (30d latency, 1y speed
// and outages) would delete and the operator's stored windows would keep, plus
// one the other way round: a 20-day-old speed run that a stored 10-day window
// deletes and the 1-year default keeps. The second is how a later test tells
// "pruned on the stored windows" from "pruned on nothing".
func (f *prunerStore) seedHistory() {
	f.t.Helper()
	old := time.Now().Add(-400 * 24 * time.Hour)
	if err := f.st.SetSettings(f.ctx, map[string]string{
		"retention_s":          "0",      // keep forever
		"speed_retention_s":    "864000", // 10 days
		"downtime_retention_s": "0",      // keep forever
	}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.InsertSamples(f.ctx, []store.Sample{
		{TS: old, Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
	}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.InsertSpeed(f.ctx, store.SpeedSample{
		TS: time.Now().Add(-20 * 24 * time.Hour).Unix(), DownMbps: 100, UpMbps: 20, PingMS: 5, Server: "s",
	}); err != nil {
		f.t.Fatal(err)
	}
	// A completed outage: both rows fall under the outage window's cutoff.
	if err := f.st.InsertEvent(f.ctx, old, "down", 0, ""); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.InsertEvent(f.ctx, old.Add(time.Hour), "up", 3600, ""); err != nil {
		f.t.Fatal(err)
	}
	if got := f.rows(); got != [3]int64{1, 1, 2} {
		f.t.Fatalf("seed: samples/speed/events = %v, want [1 1 2]", got)
	}
}

// unloadedController boots settings the way run() does on a store whose
// settings read fails: New errors, the controller comes back on defaults.
func unloadedController(t *testing.T, f *prunerStore) *settings.Controller {
	t.Helper()
	f.exec(`ALTER TABLE settings RENAME TO settings_unreadable`)
	set, err := settings.New(f.ctx, f.st, defaultSettings(config.Default()))
	if err == nil {
		t.Fatal("fixture: the settings read must FAIL")
	}
	if set.Loaded() {
		t.Fatal("fixture: the controller must start unloaded")
	}
	return set
}

// prunePasses counts the passes Prune has completed. Incrementing it is the
// last thing Prune does before returning, so a wait on it is a wait for the
// whole sweep - Prune deletes from each table in its own statement, and a
// pruner cancelled between two of them reports a cancellation, not a prune.
func prunePasses() int64 { return stats.Lifetime().Counters["db.prune_count"] }

// waitFor polls cond until it holds or d has passed, and reports which.
func waitFor(cond func() bool, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
	return true
}

// warnOnlyLog captures the log at the level a stock install runs at: log_level
// "off" pins WARN, so a skip reported any quieter than that would be exactly
// the silence the guard exists to end.
func warnOnlyLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// runPrunerFor starts runPruner with a short startup grace. It returns a stop
// function that cancels the loop and waits for it to exit, and a running
// report for asserting that the loop has NOT exited on its own.
func runPrunerFor(t *testing.T, p *program, set *settings.Controller) (stop func(), running func() bool) {
	t.Helper()
	prev := pruneStartupGrace
	pruneStartupGrace = 50 * time.Millisecond
	t.Cleanup(func() { pruneStartupGrace = prev })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.runPruner(ctx, set) }()
	running = func() bool {
		select {
		case <-done:
			return false
		default:
			return true
		}
	}
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runPruner ignored context cancellation")
		}
	}
	return stop, running
}

func TestPrunerSkipsWhileSettingsNeverLoaded(t *testing.T) {
	f := openPrunerStore(t)
	f.seedHistory()
	set := unloadedController(t, f)

	var logs bytes.Buffer
	p := &program{store: f.st, log: warnOnlyLog(&logs)}
	stop, running := runPrunerFor(t, p, set)
	defer stop()
	// Well past the grace: the first pass has had every chance to run.
	time.Sleep(400 * time.Millisecond)
	if got := f.rows(); got != [3]int64{1, 1, 2} {
		t.Errorf("the pruner deleted history while settings were unloaded: samples/speed/events = %v, want [1 1 2] "+
			"(stored retention_s=0 and downtime_retention_s=0 mean keep forever; the compiled-in 30d/1y windows are not the operator's)", got)
	}
	// Skipping the pass must not end the job: settings that load later need
	// the ticker still there, or the process never prunes again.
	if !running() {
		t.Error("runPruner exited after the skipped pass; it must stay on its ticker for the pass after settings load")
	}
	stop()
	// The skip must be visible at the stock log level: one warning for the pass.
	if n := strings.Count(logs.String(), "prune skipped"); n != 1 {
		t.Errorf("want exactly one 'prune skipped' warning for the skipped pass, got %d in:\n%s", n, logs.String())
	}
}

// Once the retry loop's Reload wins, the SAME loop's next tick acts on the
// stored windows: the speed run the 10-day stored window covers goes, and the
// keep-forever rows stay. The tick is shortened so that the pass after the
// load is a real tick of the loop that skipped, not a restarted one - a guard
// that only covered the first pass, or one that ended the loop, would both
// pass a restart.
func TestPrunerPrunesOnStoredWindowsOnceSettingsLoad(t *testing.T) {
	f := openPrunerStore(t)
	f.seedHistory()
	set := unloadedController(t, f)

	prevTick := pruneInterval
	pruneInterval = 100 * time.Millisecond
	t.Cleanup(func() { pruneInterval = prevTick })

	var logs bytes.Buffer
	cfg := config.Default()
	p := &program{cfg: cfg, store: f.st, log: warnOnlyLog(&logs)}
	// run() registers the post-load hook before spawning the retry loop.
	p.registerSettingsLoadedHook(f.ctx, set)

	before := prunePasses()
	stop, running := runPrunerFor(t, p, set)
	defer stop()
	// The grace and then several ticks, every one of them unloaded.
	time.Sleep(400 * time.Millisecond)
	if got := f.rows(); got != [3]int64{1, 1, 2} {
		t.Fatalf("an unloaded pass deleted history: samples/speed/events = %v, want [1 1 2]", got)
	}
	if !running() {
		t.Fatal("runPruner exited while settings were unloaded; nothing would prune once they load")
	}

	// The fault clears; the retry loop's first attempt (5s) now succeeds and
	// flips Loaded on the controller the running pruner holds.
	f.exec(`ALTER TABLE settings_unreadable RENAME TO settings`)
	rctx, rcancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer rcancel()
	p.retrySettingsLoad(rctx, set)
	if !set.Loaded() {
		t.Fatal("retry loop returned without loading settings - harness fault, not the bug")
	}
	if r, s, d := set.Retention(), set.SpeedRetention(), set.DowntimeRetention(); r != 0 || s != 10*24*time.Hour || d != 0 {
		t.Fatalf("loaded controller reports retention=%v speed=%v downtime=%v, want 0/240h/0 (the stored windows)", r, s, d)
	}

	// The next tick prunes. Wait for the pass to complete, not for a table to
	// empty, so the assertions below read a finished sweep.
	if !waitFor(func() bool { return prunePasses() > before }, 5*time.Second) {
		t.Error("no prune pass completed within 5s of settings loading; the ticker must re-check Loaded on every pass")
	}
	if got := f.rows(); got != [3]int64{1, 0, 2} {
		t.Errorf("after settings loaded: samples/speed/events = %v, want [1 0 2] "+
			"(the 20-day speed run is past the stored 10-day window; the keep-forever rows stay)", got)
	}
	stop()
	if !strings.Contains(logs.String(), "prune skipped") {
		t.Errorf("the unloaded passes must have been reported:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a pass failed:\n%s", logs.String())
	}
}

// A controller whose boot read succeeded prunes exactly as it always has: each
// finite window deletes past its cutoff and keeps inside it, a zero window
// keeps everything, and nothing is skipped or warned about.
func TestPrunerPrunesALoadedControllerAsBefore(t *testing.T) {
	f := openPrunerStore(t)
	if err := f.st.SetSettings(f.ctx, map[string]string{
		"retention_s":          "864000", // 10 days
		"speed_retention_s":    "864000", // 10 days
		"downtime_retention_s": "0",      // keep forever
	}); err != nil {
		t.Fatal(err)
	}
	ago := func(days int) time.Time { return time.Now().Add(-time.Duration(days) * 24 * time.Hour) }
	if err := f.st.InsertSamples(f.ctx, []store.Sample{
		{TS: ago(20), Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
		{TS: ago(5), Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []int{20, 5} {
		if err := f.st.InsertSpeed(f.ctx, store.SpeedSample{TS: ago(d).Unix(), DownMbps: 100, UpMbps: 20, PingMS: 5, Server: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.InsertEvent(f.ctx, ago(400), "down", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.st.InsertEvent(f.ctx, ago(400).Add(time.Hour), "up", 3600, ""); err != nil {
		t.Fatal(err)
	}

	set, err := settings.New(f.ctx, f.st, defaultSettings(config.Default()))
	if err != nil {
		t.Fatal(err)
	}
	if !set.Loaded() {
		t.Fatal("fixture: the controller must be loaded")
	}

	before := prunePasses()
	var logs bytes.Buffer
	p := &program{store: f.st, log: slog.New(slog.NewTextHandler(&logs, nil))}
	stop, _ := runPrunerFor(t, p, set)
	defer stop()
	if !waitFor(func() bool { return prunePasses() > before }, 5*time.Second) {
		t.Error("no prune pass completed within 5s of the grace")
	}
	stop()
	if got := f.rows(); got != [3]int64{1, 1, 2} {
		t.Errorf("loaded pass: samples/speed/events = %v, want [1 1 2] (10-day windows drop the 20-day rows and keep the 5-day ones; the zero outage window keeps both events)", got)
	}
	if strings.Contains(logs.String(), "prune skipped") {
		t.Errorf("a loaded controller must not skip a pass:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "pruned old data") {
		t.Errorf("the pass must report what it pruned:\n%s", logs.String())
	}
}
