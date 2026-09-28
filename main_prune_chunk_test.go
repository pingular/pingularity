package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
)

// A BIG CLEANUP IS A PASS THAT CAN BE INTERRUPTED.
//
// Prune deletes in chunks with a wait after every full one, so a pass over a
// big backlog lasts seconds or minutes where it used to be one statement. Two
// things now land inside a pass that used to land beside it: a shutdown, and
// another writer holding the database. Neither is a failure. What went stays
// gone, the rest goes at the next pass, and the log says so in those words.

// seedExpiredSamples writes n latency rows a second apart, all older than any
// window the tests set, in one statement on the second connection.
func (f *prunerStore) seedExpiredSamples(n int) {
	f.t.Helper()
	newest := time.Now().Add(-400 * 24 * time.Hour).Unix()
	if _, err := f.db2.ExecContext(f.ctx, `
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ? - 1)
		INSERT INTO samples (ts, target, latency_ms, success, family)
		SELECT ? - i, 'a', 10.0, 1, 'ipv4' FROM n`, n, newest); err != nil {
		f.t.Fatalf("seed %d samples: %v", n, err)
	}
	if got := f.count("samples"); got != int64(n) {
		f.t.Fatalf("seed: %d samples, want %d", got, n)
	}
}

// tenDayController stores a ten day latency window, keeps the rest forever,
// and boots settings on it the way run() does.
func tenDayController(t *testing.T, f *prunerStore) *settings.Controller {
	t.Helper()
	if err := f.st.SetSettings(f.ctx, map[string]string{
		"retention_s":          "864000",
		"speed_retention_s":    "0",
		"downtime_retention_s": "0",
	}); err != nil {
		t.Fatal(err)
	}
	set, err := settings.New(f.ctx, f.st, defaultSettings(config.Default()))
	if err != nil {
		t.Fatal(err)
	}
	if !set.Loaded() {
		t.Fatal("fixture: the controller must be loaded")
	}
	return set
}

// A shutdown in the middle of a big pass. The pruner has to let go inside the
// shutdown budget, leave the rows it had not reached, and report a stop rather
// than an error. The next start finishes the job.
func TestPrunerStopsMidPassAndFinishesLater(t *testing.T) {
	f := openPrunerStore(t)
	const seeded = 100000 // five full chunks, so most of a second in waits alone
	f.seedExpiredSamples(seeded)
	set := tenDayController(t, f)

	passes := prunePasses()
	var logs bytes.Buffer
	p := &program{store: f.st, log: slog.New(slog.NewTextHandler(&logs, nil))}
	stop, _ := runPrunerFor(t, p, set)
	defer stop()
	if !waitFor(func() bool { return f.count("samples") < seeded }, 5*time.Second) {
		t.Fatal("no chunk was deleted within 5s of the grace")
	}
	began := time.Now()
	stop() // fails the test itself if the pruner takes more than 5s to return
	took := time.Since(began)
	left := f.count("samples")
	if left == 0 {
		t.Fatalf("the pass finished before it could be stopped; %d rows is too small a fixture for this machine", seeded)
	}
	if took > shutdownWorkerGrace {
		t.Errorf("the pruner took %v to stop in the middle of a pass; workers get %v at shutdown, and a chunk or "+
			"a wait has to end inside it", took, shutdownWorkerGrace)
	}
	if got := prunePasses(); got != passes {
		t.Errorf("db.prune_count moved by %d: a stopped pass is not a finished prune", got-passes)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("a stop for shutdown was logged as an error:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "prune stopped for shutdown") {
		t.Errorf("the stopped pass was not reported:\n%s", logs.String())
	}

	// The next start picks up where this one stopped.
	logs.Reset()
	stop2, _ := runPrunerFor(t, p, set)
	defer stop2()
	if !waitFor(func() bool { return prunePasses() > passes }, 30*time.Second) {
		t.Fatalf("the second run did not finish the pass within 30s; %d rows left", f.count("samples"))
	}
	stop2()
	if got := f.count("samples"); got != 0 {
		t.Errorf("%d expired rows left after the second run finished its pass, want 0", got)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("the second run failed:\n%s", logs.String())
	}
}

// Another writer holds the database for longer than a chunk will wait, which
// a restore can do. The pass gives way and says so as a warning, the ticker
// stays, and the pass after the writer lets go removes the rows.
func TestPrunerGivesWayToAnotherWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the 5s busy timeout")
	}
	f := openPrunerStore(t)
	f.seedExpiredSamples(100)
	set := tenDayController(t, f)

	prevTick := pruneInterval
	pruneInterval = 100 * time.Millisecond
	t.Cleanup(func() { pruneInterval = prevTick })

	// One connection, so the transaction that takes the writer is the one
	// that gives it back.
	holder, err := f.db2.Conn(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(f.ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			holder.ExecContext(f.ctx, `ROLLBACK`) //nolint:errcheck // only the failure path
		}
	}()

	passes := prunePasses()
	busy := stats.Lifetime().Counters["db.busy"]
	var logs bytes.Buffer
	p := &program{store: f.st, log: warnOnlyLog(&logs)}
	stop, running := runPrunerFor(t, p, set)
	defer stop()
	if !waitFor(func() bool { return stats.Lifetime().Counters["db.busy"] > busy }, 15*time.Second) {
		t.Fatal("no chunk gave up on the held writer within 15s")
	}
	if got := f.count("samples"); got != 100 {
		t.Errorf("%d samples left while the writer was held, want all 100", got)
	}
	if !running() {
		t.Fatal("runPruner exited after the pass that gave way; nothing would prune once the writer is free")
	}
	if _, err := holder.ExecContext(f.ctx, `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	held = false
	if !waitFor(func() bool { return prunePasses() > passes }, 15*time.Second) {
		t.Fatal("no pass finished within 15s of the writer letting go")
	}
	stop()
	if got := f.count("samples"); got != 0 {
		t.Errorf("%d expired rows left after the writer let go, want 0", got)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "prune gave way to another writer") {
		t.Errorf("the pass that gave way must be reported as a warning:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("giving way to another writer was logged as an error:\n%s", logs.String())
	}
}
