package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// THE WRITE-AHEAD LOG IS BOUNDED, AND THE FILE SAYS HOW MUCH OF IT IS FREE.
//
// SQLite never shrinks the -wal file by itself, so one big cleanup beside an
// export left 80 MB of log behind until the daemon stopped. Two things bound
// it now: a size limit every connection carries, and a trim after a large
// delete. Every test here opens a real file, so it runs the same in both CI
// legs. A :memory: store has no log and one connection, and none of this can
// be seen on it.

// openWALStore opens a store on a real file and returns the file's path too.
func openWALStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wal.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open file-backed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

// fileBytes is the size of a file, or -1 when there is none. It asks an open
// handle, not the path: SQLite has the file open, and on Windows the directory
// entry can lag behind what the file holds.
func fileBytes(name string) int64 {
	f, err := os.Open(name)
	if err != nil {
		return -1
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return -1
	}
	return fi.Size()
}

// walBytes is the size of the -wal file, or -1 when there is none.
func walBytes(path string) int64 { return fileBytes(path + "-wal") }

// seedOldSamples writes n sample rows stamped a year ago, in one transaction.
func seedOldSamples(t *testing.T, st *Store, n int) {
	t.Helper()
	tx, err := st.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, 12.5, 1, 'ipv4')`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	base := time.Now().Add(-365 * 24 * time.Hour).Unix()
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(base+int64(i/6), "t"+string(rune('0'+i%6))); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// pruneYearOld runs a cleanup that removes what seedOldSamples wrote.
func pruneYearOld(ctx context.Context, st *Store) (int64, error) {
	return st.Prune(ctx, time.Now().Add(-24*time.Hour), time.Unix(0, 0), time.Unix(0, 0))
}

// writeRound writes one probe round the way the monitor does.
func writeRound(st *Store, at time.Time) error {
	sms := make([]Sample, 0, 6)
	for i, fam := range []string{"ipv4", "ipv4", "ipv4", "ipv6", "ipv6", "ipv6"} {
		sms = append(sms, Sample{TS: at, Target: "t" + string(rune('0'+i)), Family: fam, LatencyMS: 12.5, Success: true})
	}
	if err := st.InsertSamples(context.Background(), sms); err != nil {
		return err
	}
	return st.InsertDNS(context.Background(), at, 8.5, true)
}

// trimCounters are the three outcomes a trim books, and db.err beside them.
var trimCounters = []string{"db.wal_trim", "db.wal_trim_blocked", "db.wal_trim_failed", "db.err"}

func counter(name string) int64 { return stats.Lifetime().Counters[name] }

// requireCounters checks the four trim counters against want, in the order of
// trimCounters.
func requireCounters(t *testing.T, when string, want ...int64) {
	t.Helper()
	for i, k := range trimCounters {
		if got := counter(k); got != want[i] {
			t.Errorf("%s: %s = %d, want %d", when, k, got, want[i])
		}
	}
}

// trimName says an outcome in words, for a failure message.
func trimName(w walTrim) string {
	switch w {
	case walTrimSkipped:
		return "skipped"
	case walTrimDone:
		return "done"
	case walTrimNotTried:
		return "not tried"
	case walTrimGaveUp:
		return "gave up"
	case walTrimFailed:
		return "failed"
	}
	return "unknown"
}

// lowerTrimRows makes a small delete count as a large one for one test.
func lowerTrimRows(t *testing.T, n int64) {
	t.Helper()
	old := walTrimRows
	walTrimRows = n
	t.Cleanup(func() { walTrimRows = old })
}

// pooled pins every connection of the pool at once and hands each to fn. A
// test that still holds a snapshot has one of the four, and must let it go
// before it calls this, or the fourth pin waits for ever.
func pooled(t *testing.T, st *Store, fn func(i int, c *sql.Conn)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), busyTimeout)
	defer cancel()
	var conns []*sql.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 4; i++ {
		c, err := st.db.Conn(ctx)
		if err != nil {
			t.Fatalf("pin connection %d: %v", i, err)
		}
		conns = append(conns, c)
		fn(i, c)
	}
}

// requirePragmas checks that every pooled connection still has what the DSN
// gave it.
func requirePragmas(t *testing.T, st *Store, when string) {
	t.Helper()
	ctx := context.Background()
	pooled(t, st, func(i int, c *sql.Conn) {
		var busy, limit, sync int64
		var mode string
		for q, dst := range map[string]any{
			`PRAGMA busy_timeout`: &busy, `PRAGMA journal_size_limit`: &limit,
			`PRAGMA synchronous`: &sync, `PRAGMA journal_mode`: &mode,
		} {
			if err := c.QueryRowContext(ctx, q).Scan(dst); err != nil {
				t.Fatalf("%s, connection %d, %s: %v", when, i, q, err)
			}
		}
		if busy != busyTimeout.Milliseconds() {
			t.Errorf("%s: connection %d has busy_timeout %d, want %d: a writer on it gives up early or waits too long", when, i, busy, busyTimeout.Milliseconds())
		}
		if limit != walSizeLimit {
			t.Errorf("%s: connection %d has journal_size_limit %d, want %d: the limit is per connection, and a commit from this one would not cut the log", when, i, limit, walSizeLimit)
		}
		if mode != "wal" || sync != 1 {
			t.Errorf("%s: connection %d has journal_mode %q synchronous %d, want wal and 1 (NORMAL)", when, i, mode, sync)
		}
	})
}

// This is also what holds the number in pragmaConn and walSizeLimit together.
func TestEveryPooledConnectionCarriesThePragmas(t *testing.T) {
	st, _ := openWALStore(t)
	requirePragmas(t, st, "after open")
}

// The limit must sit well above what the log needs anyway. At or below that
// size every checkpoint cycle would cut the file and grow it again.
func TestWALLimitLeavesOrdinaryRunningAlone(t *testing.T) {
	st, path := openWALStore(t)
	var page, every int64
	if err := st.db.QueryRow(`PRAGMA page_size`).Scan(&page); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`PRAGMA wal_autocheckpoint`).Scan(&every); err != nil {
		t.Fatal(err)
	}
	steady := 32 + every*(page+24)
	if walSizeLimit < 2*steady-64 {
		t.Fatalf("walSizeLimit is %d, and the log needs %d in ordinary running (%d pages of %d bytes): keep the limit at twice that or the log is cut and regrown on every cycle",
			walSizeLimit, steady, every, page)
	}
	// Rounds until the log has reached its ordinary size, then twice as many
	// again: two more cycles at whatever a round writes. Not a fixed count,
	// so a round that writes less than today's still gets there.
	const most = 20000
	last, reached := walBytes(path), 0
	at := time.Now().Add(-6 * time.Hour)
	for i := 1; i <= most && (reached == 0 || i <= 3*reached); i++ {
		if err := writeRound(st, at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
		now := walBytes(path)
		if now < last {
			t.Fatalf("round %d: the log shrank from %d to %d bytes in ordinary running", i, last, now)
		}
		last = now
		if reached == 0 && now >= steady {
			reached = i
		}
	}
	if reached == 0 {
		t.Fatalf("the log is %d bytes after %d rounds and never reached the %d of ordinary running: the rounds are not reaching the file", last, most, steady)
	}
	if last > walSizeLimit {
		t.Errorf("the log is %d bytes after ordinary running, want %d at most", last, walSizeLimit)
	}
}

// The limit on its own, with no trim anywhere near: one write bigger than the
// limit, then a few small commits. The first one that restarts the log cuts
// the file.
func TestBigWriteLeavesABoundedLog(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	if _, err := st.db.Exec(`CREATE TABLE wal_filler (b BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO wal_filler VALUES (zeroblob(?))`, walSizeLimit+walSizeLimit/2); err != nil {
		t.Fatalf("filler: %v", err)
	}
	if got := walBytes(path); got <= walSizeLimit {
		t.Fatalf("premise broken: the log is %d bytes after a %d-byte write, want more than the limit of %d", got, walSizeLimit+walSizeLimit/2, walSizeLimit)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.db.Exec(`INSERT INTO wal_filler VALUES (zeroblob(1))`); err != nil {
			t.Fatalf("small commit: %v", err)
		}
	}
	if got := walBytes(path); got > walSizeLimit {
		t.Errorf("the log is %d bytes three commits after a big write, want %d at most", got, walSizeLimit)
	}
	requireCounters(t, "the limit needs no trim", 0, 0, 0, 0)
}

func TestPruneOfManyRowsEmptiesTheLog(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	st, path := openWALStore(t)
	seedOldSamples(t, st, 3000)
	n, err := pruneYearOld(context.Background(), st)
	if err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v; want 3000, nil", n, err)
	}
	if got := walBytes(path); got != 0 {
		t.Errorf("the log is %d bytes after a cleanup that reached walTrimRows, want 0", got)
	}
	requireCounters(t, "after a cleanup that reached walTrimRows", 1, 0, 0, 0)
	requirePragmas(t, st, "after a trim")
}

// The trim counts the whole pass, not its last chunk or its last table.
func TestPruneTrimsOnTheTotalOfItsChunks(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	pruneChunks(t, 700, 0)
	st, path := openWALStore(t)
	seedOldSamples(t, st, 3000)
	biggest := int64(0)
	pruneChunkHook = func(_ string, rows int64, _ time.Duration, _ bool) {
		if rows > biggest {
			biggest = rows
		}
	}
	n, err := pruneYearOld(context.Background(), st)
	if err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v; want 3000, nil", n, err)
	}
	if biggest >= walTrimRows {
		t.Fatalf("premise broken: one chunk removed %d rows, which reaches walTrimRows by itself", biggest)
	}
	if got := walBytes(path); got != 0 {
		t.Errorf("the log is %d bytes after a cleanup of %d rows in chunks of 700, want 0", got, n)
	}
	requireCounters(t, "after a cleanup in chunks", 1, 0, 0, 0)
}

func TestClearOfManyRowsEmptiesTheLog(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	st, path := openWALStore(t)
	seedOldSamples(t, st, 3000)
	n, err := st.Clear(context.Background(), "latency")
	if err != nil || n != 3000 {
		t.Fatalf("Clear = %d, %v; want 3000, nil", n, err)
	}
	if got := walBytes(path); got != 0 {
		t.Errorf("the log is %d bytes after a clear that reached walTrimRows, want 0", got)
	}
	requireCounters(t, "after a clear that reached walTrimRows", 1, 0, 0, 0)
	requirePragmas(t, st, "after a trim")
}

// At the shipped threshold: one hour of rounds on a default install.
func TestSmallDeleteLeavesTheLogAlone(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	seedOldSamples(t, st, 5040)
	before := walBytes(path)
	n, err := pruneYearOld(context.Background(), st)
	if err != nil || n != 5040 {
		t.Fatalf("Prune = %d, %v; want 5040, nil", n, err)
	}
	if got := walBytes(path); got < before || got <= 0 {
		t.Errorf("the log went from %d to %d bytes over an hourly cleanup: it must be left to be overwritten", before, got)
	}
	requireCounters(t, "after an hourly cleanup", 0, 0, 0, 0)
	if 25200 >= walTrimRows {
		t.Errorf("walTrimRows is %d: an hour of rounds at the fastest probe interval is 25200 rows, and the hourly cleanup must stay under it", walTrimRows)
	}
}

// A cleanup leaves the writer free for a moment before its trim, the way it
// does after every full chunk. A clear does not: the wait would be most of
// what a Delete now takes.
func TestACleanupWaitsBeforeItsTrimAndAClearDoesNot(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	st, _ := openWALStore(t)
	// 3000 rows never fill a chunk, so the only wait left is the one before
	// the trim.
	const pause = 400 * time.Millisecond
	pruneChunks(t, pruneChunkRows, pause)
	seedOldSamples(t, st, 3000)
	t0 := time.Now()
	n, err := pruneYearOld(context.Background(), st)
	took := time.Since(t0)
	if err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v; want 3000, nil", n, err)
	}
	if took < pause {
		t.Errorf("a cleanup that trimmed took %v, want %v at least: a probe write that waited out the last chunk must get the writer before the trim can take it", took, pause)
	}
	requireCounters(t, "after the cleanup", 1, 0, 0, 0)

	// A wait nobody could miss. A clear that did wait is stopped, not sat out.
	pruneChunkPause = time.Hour
	seedOldSamples(t, st, 3000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type res struct {
		n   int64
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, err := st.Clear(ctx, "latency")
		done <- res{n, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || r.n != 3000 {
			t.Fatalf("Clear = %d, %v; want 3000, nil", r.n, r.err)
		}
	case <-time.After(busyTimeout / 2):
		cancel()
		<-done
		t.Fatalf("a clear of 3000 rows had not returned after %v: it waits before its trim", busyTimeout/2)
	}
	requireCounters(t, "after the clear", 2, 0, 0, 0)
}

// A cleanup stopped in the wait before its trim still removed its rows. It is
// not a failed cleanup, and stopping is not a blocked or a failed trim.
func TestCleanupStoppedBeforeItsTrimStillSucceeds(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	st, path := openWALStore(t)
	pruneChunks(t, pruneChunkRows, time.Hour)
	seedOldSamples(t, st, 3000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The last statement of a pass is the future arm of the pauses sweep. The
	// wait that follows it here is the trim's.
	pruneChunkHook = func(q string, _ int64, _ time.Duration, _ bool) {
		if strings.Contains(q, "FROM pauses WHERE ts >") {
			time.AfterFunc(50*time.Millisecond, cancel)
		}
	}
	t0 := time.Now()
	n, err := pruneYearOld(ctx, st)
	if err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v when stopped before its trim; want 3000, nil: the rows were removed", n, err)
	}
	if took := time.Since(t0); took >= busyTimeout/2 {
		t.Errorf("a stopped cleanup took %v to return", took)
	}
	if got := walBytes(path); got <= 0 {
		t.Errorf("the log is %d bytes: the trim ran although its caller was stopping", got)
	}
	requireCounters(t, "after a stop", 0, 0, 0, 0)
}

func TestTrimBlockedByAReaderFailsNothing(t *testing.T) {
	stats.ResetForTest()
	lowerTrimRows(t, 3000)
	st, path := openWALStore(t)
	ctx := context.Background()
	seedOldSamples(t, st, 3000)
	// An export in flight: a snapshot from before the cleanup, held across it.
	snap, err := st.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer snap.Rollback()
	var seen int64
	if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM samples`).Scan(&seen); err != nil {
		t.Fatalf("first read: %v", err)
	}
	n, err := pruneYearOld(ctx, st)
	if err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v with a reader open; want 3000, nil: a trim that cannot run must not fail the cleanup", n, err)
	}
	requireCounters(t, "after a trim a reader blocked", 0, 1, 0, 0)
	if got := walBytes(path); got <= 0 {
		t.Errorf("the log is %d bytes while a reader still needs it", got)
	}
	// And it stopped before TRUNCATE. That pass copies while it holds the
	// writer, so it must not run while part of the log is still to be copied.
	if got, err := st.trimWAL(ctx); got != walTrimNotTried || err != nil {
		t.Errorf("trimWAL = %s, %v with a reader on an older snapshot; want not tried, nil: the writer must not be taken", trimName(got), err)
	}
	if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM samples`).Scan(&seen); err != nil || seen != 3000 {
		t.Errorf("the reader sees %d rows, %v; want its own snapshot of 3000", seen, err)
	}
	// The reader has left. No trim comes back for the log: the limit is what
	// bounds it from here, and ordinary writes go on.
	snap.Rollback()
	if err := writeRound(st, time.Now()); err != nil {
		t.Errorf("a round after the reader left: %v", err)
	}
	if got := walBytes(path); got > walSizeLimit {
		t.Errorf("the log is %d bytes after the reader left and a round was written, want %d at most", got, walSizeLimit)
	}
	requirePragmas(t, st, "after a blocked trim")
}

// readerOnTheEnd holds a snapshot of everything the log holds. The PASSIVE
// pass can finish around such a reader. TRUNCATE cannot: it needs the log to
// itself.
func readerOnTheEnd(t *testing.T, st *Store) *sql.Tx {
	t.Helper()
	ctx := context.Background()
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	snap, err := st.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Cleanup(func() { snap.Rollback() })
	var seen int64
	if err := snap.QueryRowContext(ctx, `SELECT COUNT(*) FROM samples`).Scan(&seen); err != nil {
		t.Fatalf("first read: %v", err)
	}
	return snap
}

// The bound that matters is the probe write's: it gives up after busyTimeout.
// With the pool's own timeout a blocked TRUNCATE held the writer for 5.26 s
// and the write beside it waited 5.06 s. Half the timeout is far from both
// figures and from the quarter second a trim may take.
func TestTrimHoldsTheWriterNoLongerThanItsBound(t *testing.T) {
	st, path := openWALStore(t)
	ctx := context.Background()
	snap := readerOnTheEnd(t, st)
	type res struct {
		took time.Duration
		err  error
	}
	wrote := make(chan res, 1)
	go func() {
		time.Sleep(50 * time.Millisecond) // arrive while TRUNCATE waits
		t0 := time.Now()
		err := writeRound(st, time.Now())
		wrote <- res{time.Since(t0), err}
	}()
	t0 := time.Now()
	got, err := st.trimWAL(ctx)
	took := time.Since(t0)
	w := <-wrote
	if got != walTrimGaveUp || err != nil {
		t.Fatalf("trimWAL = %s, %v with a reader on the end of the log; want gave up, nil", trimName(got), err)
	}
	if took < walTrimWait {
		t.Errorf("the trim gave up after %v, want it to wait %v for the reader", took, walTrimWait)
	}
	if took > busyTimeout/2 {
		t.Errorf("the trim took %v, want under %v", took, busyTimeout/2)
	}
	if w.err != nil {
		t.Errorf("a probe write failed beside the trim: %v", w.err)
	}
	if w.took > busyTimeout/2 {
		t.Errorf("a probe write waited %v beside the trim, want under %v", w.took, busyTimeout/2)
	}
	t.Logf("trim %v, write beside it %v, log %d bytes", took, w.took, walBytes(path))
	snap.Rollback()
	requirePragmas(t, st, "after a trim that waited")
}

// A restore commits batch after batch, and a trim can land in the middle of
// one. It must give up on the writer in its own time and leave the batch to
// commit.
func TestTrimBesideALongWriteGivesUp(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	ctx := context.Background()
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	w, err := secondWriter(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (1, 'restored', 1.0, 1, 'ipv4')`); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}
	t0 := time.Now()
	st.trimWALAfter(ctx, walTrimRows, nil)
	if took := time.Since(t0); took > busyTimeout/2 {
		t.Errorf("the trim took %v beside a writer, want under %v", took, busyTimeout/2)
	}
	requireCounters(t, "after a trim beside a long write", 0, 1, 0, 0)
	if got, err := st.trimWAL(ctx); got != walTrimGaveUp || err != nil {
		t.Errorf("trimWAL = %s, %v beside a writer in a transaction; want gave up, nil", trimName(got), err)
	}
	if _, err := w.ExecContext(ctx, `COMMIT`); err != nil {
		t.Errorf("the write beside the trim could not commit: %v", err)
	}
	requirePragmas(t, st, "after a trim beside a long write")
}

// Two checkpoints cannot run at once, and the one that comes second is told
// so with a reply of busy 1, log -1, copied -1. A log of -1 is also what a
// database with no log answers, so the trim has to look at busy first, or it
// takes another checkpoint for an in-memory store and books nothing.
func TestTrimBesideAnotherCheckpointIsBlocked(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	ctx := context.Background()
	snap := readerOnTheEnd(t, st)
	// Another connection's checkpoint, held up by the reader for as long as
	// it is willing to wait. It keeps the checkpoint lock all that time.
	other, err := secondWriter(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, (busyTimeout/2).Milliseconds())); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var busy, logFrames, copied int64
		done <- other.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &copied)
	}()
	// Until the other checkpoint has the lock, the trim's own TRUNCATE meets
	// the reader and gives up. Once it has, the trim must see it.
	saw := false
	for !saw {
		select {
		case err := <-done:
			t.Fatalf("the other checkpoint ended (%v) before a trim ever found it running", err)
		default:
		}
		switch got, err := st.trimWAL(ctx); {
		case err != nil:
			t.Fatalf("trimWAL beside another checkpoint: %v", err)
		case got == walTrimNotTried:
			saw = true
		case got != walTrimGaveUp:
			t.Fatalf("trimWAL = %s beside another checkpoint; want not tried", trimName(got))
		}
	}
	st.trimWALAfter(ctx, walTrimRows, nil)
	requireCounters(t, "after a trim beside another checkpoint", 0, 1, 0, 0)
	snap.Rollback() // lets the other checkpoint finish
	if err := <-done; err != nil {
		t.Errorf("the other checkpoint: %v", err)
	}
	requirePragmas(t, st, "after a trim beside another checkpoint")
}

// Four tabs on wide charts can hold all four connections. The trim waits for
// one no longer than it waits for anything else, and that is a blocked trim,
// not a fault.
func TestTrimWithEveryConnectionBusyIsBlocked(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	ctx := context.Background()
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	pooled(t, st, func(i int, c *sql.Conn) {
		if i < 3 {
			return
		}
		t0 := time.Now()
		got, err := st.trimWAL(ctx)
		if got != walTrimNotTried || err != nil {
			t.Errorf("trimWAL = %s, %v with every connection taken; want not tried, nil", trimName(got), err)
		}
		if took := time.Since(t0); took < walTrimWait || took > busyTimeout/2 {
			t.Errorf("the trim waited %v for a connection, want between %v and %v", took, walTrimWait, busyTimeout/2)
		}
		st.trimWALAfter(ctx, walTrimRows, nil)
	})
	requireCounters(t, "after a trim that found no free connection", 0, 1, 0, 0)
}

// Only running out of time means "busy". A store that cannot hand out a
// connection at all is a fault, and must not read as a reader being in the
// way.
func TestTrimOnABrokenStoreIsAFaultNotABlock(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	ctx := context.Background()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := st.trimWAL(ctx)
	if got != walTrimFailed || err == nil {
		t.Fatalf("trimWAL = %s, %v on a closed store; want failed and the error", trimName(got), err)
	}
	st.trimWALAfter(ctx, walTrimRows, nil)
	requireCounters(t, "after a trim on a closed store", 0, 0, 1, 1)
}

func TestTrimStopsWithItsCaller(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	pruneChunks(t, pruneChunkRows, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t0 := time.Now()
	got, err := st.trimWAL(ctx)
	if got != walTrimSkipped || err != nil {
		t.Errorf("trimWAL = %s, %v under a cancelled context; want skipped, nil", trimName(got), err)
	}
	st.trimWALAfter(ctx, walTrimRows, pruneWait)
	st.trimWALAfter(ctx, walTrimRows, nil)
	if took := time.Since(t0); took > busyTimeout/2 {
		t.Errorf("three trims under a cancelled context took %v", took)
	}
	if got := walBytes(path); got <= 0 {
		t.Errorf("the log is %d bytes: a trim ran although its caller had stopped", got)
	}
	requireCounters(t, "after a shutdown (stopping is not a fault)", 0, 0, 0, 0)
}

// The caller stops while TRUNCATE waits for a reader. SQLite sits out the rest
// of that wait, which is why it is short. The outcome is a stop, and the
// connection goes back to the pool with the timeout it came with.
func TestTrimCancelledWhileItWaitsIsAStop(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	snap := readerOnTheEnd(t, st)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var returned, late atomic.Bool
	stop := time.AfterFunc(50*time.Millisecond, func() {
		late.Store(returned.Load())
		cancel()
	})
	defer stop.Stop()
	t0 := time.Now()
	st.trimWALAfter(ctx, walTrimRows, nil)
	returned.Store(true)
	took := time.Since(t0)
	stop.Stop()
	if took > busyTimeout/2 {
		t.Errorf("a cancelled trim took %v, want under %v", took, busyTimeout/2)
	}
	snap.Rollback()
	requirePragmas(t, st, "after a trim cancelled while it waited")
	if late.Load() || ctx.Err() == nil {
		t.Skipf("the trim returned after %v, before the cancel reached it: nothing to judge on a machine this busy", took)
	}
	requireCounters(t, "after a trim cancelled while it waited", 0, 0, 0, 0)
}

// A store with no log has nothing to trim, and that is not an outcome worth
// a counter.
func TestTrimLeavesAStoreWithNoLogAlone(t *testing.T) {
	stats.ResetForTest()
	t.Setenv("PINGULARITY_TEST_DB_DIR", "") // in memory in both CI legs
	lowerTrimRows(t, 3000)
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	seedOldSamples(t, st, 3000)
	if n, err := pruneYearOld(ctx, st); err != nil || n != 3000 {
		t.Fatalf("Prune = %d, %v; want 3000, nil", n, err)
	}
	if got, err := st.trimWAL(ctx); got != walTrimSkipped || err != nil {
		t.Errorf("trimWAL = %s, %v on an in-memory store; want skipped, nil", trimName(got), err)
	}
	requireCounters(t, "on an in-memory store", 0, 0, 0, 0)
	if got, err := st.ReusableBytes(ctx); err != nil || got < 0 {
		t.Errorf("ReusableBytes = %d, %v on an in-memory store; want a figure", got, err)
	}
}

func TestReusableBytes(t *testing.T) {
	st, path := openWALStore(t)
	ctx := context.Background()
	if got, err := st.ReusableBytes(ctx); err != nil || got != 0 {
		t.Fatalf("ReusableBytes = %d, %v on a new store; want 0, nil", got, err)
	}
	seedOldSamples(t, st, 30_000)
	if _, err := st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	full := fileBytes(path)
	if full <= 0 {
		t.Fatalf("the database file is %d bytes after 30000 rows", full)
	}
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if got := fileBytes(path); got != full {
		t.Errorf("the file went from %d to %d bytes over a clear: the docs say it keeps its size", full, got)
	}
	var page int64
	if err := st.db.QueryRow(`PRAGMA page_size`).Scan(&page); err != nil {
		t.Fatal(err)
	}
	freed, err := st.ReusableBytes(ctx)
	if err != nil || freed <= 0 || freed%page != 0 || freed > full {
		t.Fatalf("ReusableBytes = %d, %v after a clear; want whole pages, more than 0 and at most the file's %d bytes", freed, err, full)
	}
	seedOldSamples(t, st, 15_000)
	if _, err := st.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	after, err := st.ReusableBytes(ctx)
	if err != nil || after >= freed {
		t.Errorf("ReusableBytes = %d, %v after new rows; want less than %d: new rows use the free pages first", after, err, freed)
	}
	if got := fileBytes(path); got != full {
		t.Errorf("the file went from %d to %d bytes: it must neither shrink nor grow while free pages last", full, got)
	}
}

// storeSources reads every non-test Go file of this package.
func storeSources(t *testing.T) map[string]string {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	src := map[string]string{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src[name] = string(b)
	}
	if _, ok := src["store.go"]; !ok {
		t.Fatal("the source scan did not find store.go - it is no longer checking anything")
	}
	return src
}

// Every function that deletes rows either trims the log afterwards or says
// here why it does not. Modelled on TestSpeedFilterCoversEveryMeasurementRead,
// including the staleness half. A function deletes when it holds a DELETE
// statement or hands one to the chunker, so the guard follows the statements
// if they ever move out of PruneLive.
func TestEveryDeleteSaysWhetherItTrimsTheLog(t *testing.T) {
	trims := map[string]string{
		"PruneLive": "retention: a lowered window or a long power-off deletes days of rounds at once",
		"Clear":     "the Data tab's Delete now: whole tables",
	}
	exempt := map[string]string{
		"repairInsanePausesAt":         "at Open, before the pool serves anything; pause rows are few",
		"repairFutureReachingPausesAt": "moves pause rows to the quarantine; few rows",
		"repairUnreadableIntColumns":   "at Open; removes only rows no read can convert",
		"forgetRestoredSplitMarker":    "one settings row",
		"ReleaseAccessHold":            "one settings row",
		"DeleteSpeed":                  "one run and its members",
		"DeleteOutage":                 "one outage",
		"ForgetServerHealth":           "one row",
		"ServerHealth":                 "expired rows of a table that holds a handful",
	}
	del := regexp.MustCompile(`(?i)DELETE\s+FROM|\.pruneChunked\(`)
	decl := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	closing := regexp.MustCompile(`(?m)^\}$`)
	seen := map[string]bool{}
	for file, src := range storeSources(t) {
		locs := decl.FindAllStringSubmatchIndex(src, -1)
		for i, loc := range locs {
			name := src[loc[2]:loc[3]]
			end := len(src)
			if i+1 < len(locs) {
				end = locs[i+1][0]
			}
			if c := closing.FindStringIndex(src[loc[0]:end]); c != nil {
				end = loc[0] + c[1]
			}
			body := stripLineComments(src[loc[0]:end])
			if !del.MatchString(body) {
				continue
			}
			seen[name] = true
			calls := strings.Contains(body, "s.trimWALAfter(")
			_, mustTrim := trims[name]
			_, isExempt := exempt[name]
			switch {
			case mustTrim && !calls:
				t.Errorf("%s (%s) deletes in bulk (%s) and does not call trimWALAfter: the log keeps what the delete left in it until the limit cuts it", name, file, trims[name])
			case calls && !mustTrim:
				t.Errorf("%s (%s) calls trimWALAfter and is not listed in `trims`: add it with the reason", name, file)
			case !mustTrim && !isExempt:
				t.Errorf("%s (%s) deletes rows and is in neither list: call trimWALAfter if it can delete %d rows or more, or add it to `exempt` with the reason it cannot", name, file, walTrimRows)
			}
		}
	}
	var stale []string
	for _, m := range []map[string]string{trims, exempt} {
		for name := range m {
			if !seen[name] {
				stale = append(stale, name)
			}
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		t.Errorf("%s is listed here and no longer deletes anything: drop the entry", name)
	}
	if len(seen) < len(trims)+len(exempt) {
		t.Errorf("the source scan found %d deleting functions; it is no longer checking anything", len(seen))
	}
}

// The store gives no disk space back by itself, by decision: VACUUM holds the
// writer for its whole run and needs about twice the kept data in free disk.
func TestStoreNeverCompactsOnItsOwn(t *testing.T) {
	compacts := regexp.MustCompile(`(?i)\bvacuum\b|auto_vacuum`)
	for file, src := range storeSources(t) {
		if m := compacts.FindString(stripLineComments(src)); m != "" {
			t.Errorf("%s now runs %q: compacting was decided against as an automatic step (it holds the writer and needs free disk). If this is a manual action, scope this guard to it", file, m)
		}
	}
}
