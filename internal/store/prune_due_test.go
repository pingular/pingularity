package store

import (
	"context"
	"maps"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// A RESTORE IS TOLD WHAT THE NEXT CLEANUP REALLY REMOVES.
//
// PruneDue counts the rows of a table the next Prune removes by retention,
// among the rows stored after a mark (RowMark) - the rows a restore brought.
// Its rules are Prune's own (pruneAge), so these tests pin the one thing that
// matters about it: the count and the cleanup agree, row for row, whatever the
// history looks like.

// countRows answers a COUNT(*) query.
func countRows(t *testing.T, s *Store, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// Over random histories on both sides of every cutoff - straddling outages,
// open ones, pauses running into the window, rows ahead of the clock - the rows
// PruneDue counts in each table are the rows the next Prune removes from it,
// less the ones past the future horizon, which it does not count.
func TestPruneDueCountsWhatPruneRemovesByAge(t *testing.T) {
	ctx := context.Background()
	tables := []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses"}
	var counted int64
	for trial := 0; trial < randomHistories(30); trial++ {
		now := time.Now()
		s := open(t)
		seedPruneHistory(t, s, now, rand.New(rand.NewSource(int64(trial))))
		sb, pb, eb := now.Add(-30*24*time.Hour), now.Add(-60*24*time.Hour), now.Add(-90*24*time.Hour)
		// The one repair Prune makes that PruneDue leaves out, closing an outage
		// from the samples about to go (see PruneDue). Made first, so what follows
		// compares the rules alone; Prune's own call then finds nothing to close.
		if err := s.resolveDanglingDowns(ctx, sb.Unix(), now.Unix()); err != nil {
			t.Fatal(err)
		}
		since := map[string]int64{}
		for _, tbl := range tables {
			since[tbl] = 0 // every row counts
		}
		due, err := s.PruneDue(ctx, sb, pb, eb, since)
		if err != nil {
			t.Fatalf("trial %d: PruneDue: %v", trial, err)
		}
		horizon := now.Add(pruneFutureSlack).Unix()
		before, future := map[string]int64{}, map[string]int64{}
		for _, tbl := range tables {
			col := "ts"
			if tbl == "speed_servers" {
				col = "run_ts"
			}
			before[tbl] = countRows(t, s, `SELECT COUNT(*) FROM `+tbl)
			future[tbl] = countRows(t, s, `SELECT COUNT(*) FROM `+tbl+` WHERE `+col+` > ?`, horizon)
		}
		clockAt(t, s, now, now, 0)
		if _, err := s.Prune(ctx, sb, pb, eb); err != nil {
			t.Fatalf("trial %d: prune: %v", trial, err)
		}
		for _, tbl := range tables {
			removed := before[tbl] - countRows(t, s, `SELECT COUNT(*) FROM `+tbl)
			if removed != due[tbl]+future[tbl] {
				t.Errorf("trial %d: %s: prune removed %d rows, %d of them past the future horizon, but PruneDue counted %d",
					trial, tbl, removed, future[tbl], due[tbl])
			}
			counted += due[tbl]
		}
		if t.Failed() {
			return
		}
	}
	if counted == 0 {
		t.Fatal("no trial had a row past a cutoff; the histories no longer test anything")
	}
}

// A mark splits a table in two: PruneDue counts only the rows stored after it,
// however old the ones before it are, and not the ones inside the window.
func TestPruneDueCountsOnlyTheRowsAfterTheMark(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	sampleAt(t, s, now, 48*3600, "here-before", "ipv4", true) // before the mark, past the window
	mark, err := s.RowMark(ctx, "samples")
	if err != nil {
		t.Fatal(err)
	}
	sampleAt(t, s, now, 48*3600+5, "restored-old", "ipv4", true) // after the mark, past the window
	sampleAt(t, s, now, 60, "restored-new", "ipv4", true)        // after the mark, inside the window
	cut := now.Add(-time.Hour)
	due, err := s.PruneDue(ctx, cut, cut, cut, map[string]int64{"samples": mark})
	if err != nil {
		t.Fatal(err)
	}
	if due["samples"] != 1 {
		t.Errorf("PruneDue counted %d samples, want 1: the row stored before the mark is this install's own, and the new one is inside the window", due["samples"])
	}
}

// The mark is the table's highest rowid, 0 for an empty table, and a name that
// is not a table a restore writes is refused rather than put into SQL.
func TestRowMarkIsWhereTheTableStands(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if m, err := s.RowMark(ctx, "events"); err != nil || m != 0 {
		t.Fatalf("empty table: mark %d, %v; want 0, nil", m, err)
	}
	now := time.Now()
	eventAt(t, s, now, 300, "down", 0)
	eventAt(t, s, now, 200, "up", 100)
	want := countRows(t, s, `SELECT MAX(rowid) FROM events`)
	if m, err := s.RowMark(ctx, "events"); err != nil || m != want {
		t.Fatalf("mark %d, %v; want %d, nil", m, err, want)
	}
	if _, err := s.RowMark(ctx, "sqlite_master"); err == nil {
		t.Error("RowMark took a table no restore writes")
	}
}

// A restore is counted once, but on an install of any size, so each count must
// find its rows by a seek, never a scan. As EXPLAIN QUERY PLAN has them: the
// latency and speed rules go through their table's ts index, which holds only
// what is past the cutoff; the outage and span rules go through the rowid
// range after the mark, the outage one with a seek of the events ts index for
// each row's recovery. The count of this install's own outage records that a
// restored recovery closes reads the rowid range up to the mark - every outage
// record the install had, two small rows an outage - with the same seeks.
func TestPruneDueCountsSeek(t *testing.T) {
	s := open(t)
	for table := range pruneAge {
		q := pruneDueCount(table)
		plan := queryPlan(t, s, q, int64(1700000000), int64(5))
		if strings.Contains(plan, "SCAN") || !strings.Contains(plan, "SEARCH "+table) {
			t.Errorf("%s\nplan %q: want a SEARCH of %s and no SCAN", strings.Join(strings.Fields(q), " "), plan, table)
		}
	}
	q := pruneDueClosedCount()
	plan := queryPlan(t, s, q, int64(1700000000), int64(5))
	if strings.Contains(plan, "SCAN") || !strings.Contains(plan, "SEARCH events") ||
		strings.Count(plan, "INDEX idx_events_ts (ts>?)") != 2 {
		t.Errorf("%s\nplan %q: want a SEARCH of events, a seek of its ts index for each recovery in both halves, "+
			"and no SCAN", strings.Join(strings.Fields(q), " "), plan)
	}
}

// A watch on the cleanup is kept table by table, and what it counts for each
// table is what Prune removed from it while the watch was open, from both
// arms: a restore reads it to tell a pass that deleted the kind of rows it
// brought from one that deleted another kind (see WatchPrunes). Each pass
// here has a watch of its own, opened after the pass before it, and counts
// nothing of that one.
func TestAWatchCountsWhatPruneRemovesFromEachTable(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if got := s.WatchPrunes().Close().Rows; len(got) != 0 {
		t.Fatalf("no pass has run, but a watch holds %v", got)
	}
	tables := []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses"}
	for pass := 0; pass < 2; pass++ {
		now := time.Now()
		seedPruneHistory(t, s, now, rand.New(rand.NewSource(int64(pass))))
		sb, pb, eb := now.Add(-30*24*time.Hour), now.Add(-60*24*time.Hour), now.Add(-90*24*time.Hour)
		// The synthetic recoveries Prune can write first, written now, so that
		// what a table loses is what Prune deleted from it.
		if err := s.resolveDanglingDowns(ctx, sb.Unix(), now.Unix()); err != nil {
			t.Fatal(err)
		}
		before, watch := map[string]int64{}, s.WatchPrunes()
		for _, tbl := range tables {
			before[tbl] = countRows(t, s, `SELECT COUNT(*) FROM `+tbl)
		}
		clockAt(t, s, now, now, 0)
		if _, err := s.Prune(ctx, sb, pb, eb); err != nil {
			t.Fatalf("pass %d: prune: %v", pass, err)
		}
		after := watch.Close()
		var moved int64
		for _, tbl := range tables {
			removed := before[tbl] - countRows(t, s, `SELECT COUNT(*) FROM `+tbl)
			if got := after.Rows[tbl]; got != removed {
				t.Errorf("pass %d: %s: the watch counted %d rows, and Prune removed %d", pass, tbl, got, removed)
			}
			moved += after.Rows[tbl]
		}
		if moved == 0 {
			t.Fatalf("pass %d removed nothing; the history no longer tests anything", pass)
		}
		if n, ok := after.Rows["pauses_quarantine"]; ok {
			t.Errorf("the watch holds %d rows for pauses_quarantine, which Prune never deletes from", n)
		}
	}
}

// A restore asks whether a pass could have reached the rows it brought, and a
// pass deletes by age only what is older than the cutoff it cuts at. So a
// watch keeps, table by table, the latest cutoff at which a chunk deleted rows
// while it was open. It is noted with the chunk that deleted them, so a
// restore never reads the rows a pass deleted without the cutoff they went
// at. A chunk that deletes nothing notes nothing: neither does a window kept
// forever, which cuts at the epoch, nor a pass the clock guard stops. A later
// pass on a longer window cuts at an earlier second and does not lower it.
// And what a pass deleted before the watch opened, or after it closed, is not
// in it.
func TestAWatchNotesTheCutoffsTheCleanupDeletedAt(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	clockAt(t, s, now, now, 0)
	// oldRows stores a row of every table the cleanup prunes by age, and an
	// outage whole, all as old as age.
	oldRows := func(age time.Duration) {
		t.Helper()
		ts := now.Add(-age).Unix()
		for _, q := range []string{
			`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?1, 'a', 10, 1, 'ipv4')`,
			`INSERT INTO dns (ts, latency_ms, success) VALUES (?1, 5, 1)`,
			`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?1, 100, 10, 5)`,
			`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?1, 's', 1, 1, 0)`,
			`INSERT INTO speed_spans (ts, duration_s) VALUES (?1, 30)`,
			`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1, 'down', NULL, '')`,
			`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1 + 60, 'up', 60, '')`,
			`INSERT INTO pauses (ts, duration_s) VALUES (?1, 60)`,
		} {
			if _, err := s.db.Exec(q, ts); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	cuts := func(sb, pb, eb time.Time) map[string]int64 {
		return map[string]int64{"samples": sb.Unix(), "dns": sb.Unix(), "speed_spans": sb.Unix(),
			"speed": pb.Unix(), "speed_servers": pb.Unix(), "events": eb.Unix(), "pauses": eb.Unix()}
	}
	prune := func(sb, pb, eb time.Time) {
		t.Helper()
		if _, err := s.Prune(ctx, sb, pb, eb); err != nil {
			t.Fatalf("prune: %v", err)
		}
	}
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const day = 24 * time.Hour

	// What a pass deleted before the watch opened is not in it.
	oldRows(4 * day)
	prune(ago(time.Hour), ago(time.Hour), ago(time.Hour))
	if got := s.WatchPrunes().Close(); len(got.Rows) != 0 || len(got.Cut) != 0 {
		t.Fatalf("a watch opened after the only pass holds %v", got)
	}

	// A pass on windows of an hour, two and three. Its cutoff for a table is
	// in the watch as soon as a chunk has deleted rows of it: the pass's first
	// such chunk takes the samples.
	oldRows(4*day + time.Minute)
	sb, pb, eb := ago(time.Hour), ago(2*time.Hour), ago(3*time.Hour)
	early, whole := s.WatchPrunes(), s.WatchPrunes()
	var first PruneTally
	pruneChunks(t, pruneChunkRows, pruneChunkPause) // puts the hook back afterwards
	pruneChunkHook = func(_ string, rows int64, _ time.Duration, _ bool) {
		if rows > 0 && first.Rows == nil {
			first = early.Close()
		}
	}
	prune(sb, pb, eb)
	pruneChunkHook = nil
	if want := map[string]int64{"samples": sb.Unix()}; !maps.Equal(first.Cut, want) {
		t.Errorf("after the pass's first chunk that deleted rows the watch holds cutoffs %v, want %v", first.Cut, want)
	}
	want := cuts(sb, pb, eb)
	got := whole.Close()
	if !maps.Equal(got.Cut, want) {
		t.Errorf("after the pass the watch holds cutoffs %v, want the pass's own %v", got.Cut, want)
	}

	// A closed watch holds what it held when it closed.
	oldRows(4*day + 2*time.Minute)
	prune(ago(time.Minute), ago(time.Minute), ago(time.Minute))
	if again := whole.Close(); !maps.Equal(again.Cut, got.Cut) || !maps.Equal(again.Rows, got.Rows) {
		t.Errorf("a closed watch moved from %v to %v with a later pass", got, again)
	}

	// Nothing deleted, nothing noted: a window of thirty days keeps the rows,
	// keeping forever cuts at the epoch, and the clock guard stops a pass.
	oldRows(4*day + 3*time.Minute)
	quiet := s.WatchPrunes()
	prune(ago(30*day), ago(30*day), ago(30*day))
	prune(time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, 0))
	clockAt(t, s, now, now.Add(time.Hour), 0) // the wall clock an hour ahead of the store's uptime
	prune(now.Add(time.Hour), now.Add(time.Hour), now.Add(time.Hour))
	if got := quiet.Close(); len(got.Rows) != 0 || len(got.Cut) != 0 {
		t.Errorf("no pass deleted a row, but the watch holds %v", got)
	}

	// A pass that cuts at an earlier second, on a longer window, after one
	// that cut later does not lower the later one's cutoff.
	clockAt(t, s, now, now, 0)
	both := s.WatchPrunes()
	prune(ago(time.Hour), ago(time.Hour), ago(time.Hour)) // the rows the quiet passes kept
	oldRows(40 * day)
	prune(ago(30*day), ago(30*day), ago(30*day))
	later := cuts(ago(time.Hour), ago(time.Hour), ago(time.Hour))
	if got := both.Close(); !maps.Equal(got.Cut, later) || got.Rows["samples"] != 2 {
		t.Errorf("after a pass on an hour and one on thirty days, each deleting a row of every table, the watch "+
			"holds %v; want the hour's cutoffs %v and two samples", got, later)
	}
}

// outageRow is one row of a random outage history.
type outageRow struct {
	ts  int64
	typ string
}

// randomOutages is an outage history of the last ten days, 'down' and 'up'
// in no particular order: some outages are open, some straddle any cutoff,
// and some have two 'down' rows in a row, as a restart mid-outage leaves.
func randomOutages(rng *rand.Rand, now time.Time) []outageRow {
	rows := make([]outageRow, 5+rng.Intn(30))
	for i := range rows {
		rows[i].ts, rows[i].typ = now.Unix()-int64(rng.Intn(10*86400)), "down"
		if rng.Intn(5) < 2 {
			rows[i].typ = "up"
		}
	}
	return rows
}

// addOutages stores rows in events, in order.
func addOutages(t *testing.T, s *Store, rows []outageRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := s.db.Exec(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, ?, 60, '')`, r.ts, r.typ); err != nil {
			t.Fatalf("insert event: %v", err)
		}
	}
}

// Over random outage histories - this install's own, then a restore's, the two
// interleaved in time - the own records PruneDueClosedOutages counts are the
// ones the next Prune removes from the rows up to the mark and would not have
// removed had the restore not happened, which a second store holding the
// install's rows alone shows. And the restored rows PruneDue counts are the
// ones it removes from the rows after the mark. Together the two are
// everything that restore adds to the cleanup's list.
func TestPruneDueClosedOutagesCountsWhatARestoreAddsToTheCleanup(t *testing.T) {
	ctx := context.Background()
	epoch := time.Unix(0, 0) // no other table holds a row
	var closedSeen int64
	for trial := 0; trial < randomHistories(40); trial++ {
		now := time.Now()
		rng := rand.New(rand.NewSource(int64(trial)))
		mine, theirs := randomOutages(rng, now), randomOutages(rng, now)
		with, alone := open(t), open(t)
		addOutages(t, with, mine)
		mark, err := with.RowMark(ctx, "events")
		if err != nil {
			t.Fatal(err)
		}
		addOutages(t, with, theirs)
		addOutages(t, alone, mine)
		cut := now.Add(-time.Duration(1+rng.Intn(9*24)) * time.Hour)
		due, err := with.PruneDue(ctx, epoch, epoch, cut, map[string]int64{"events": mark})
		if err != nil {
			t.Fatalf("trial %d: PruneDue: %v", trial, err)
		}
		closed, err := with.PruneDueClosedOutages(ctx, cut, mark)
		if err != nil {
			t.Fatalf("trial %d: PruneDueClosedOutages: %v", trial, err)
		}
		const upTo, after = `SELECT COUNT(*) FROM events WHERE rowid <= ?`, `SELECT COUNT(*) FROM events WHERE rowid > ?`
		mineBefore, theirsBefore := countRows(t, with, upTo, mark), countRows(t, with, after, mark)
		aloneBefore := countRows(t, alone, `SELECT COUNT(*) FROM events`)
		for _, s := range []*Store{with, alone} {
			clockAt(t, s, now, now, 0)
			if _, err := s.Prune(ctx, epoch, epoch, cut); err != nil {
				t.Fatalf("trial %d: prune: %v", trial, err)
			}
		}
		mineGone, theirsGone := mineBefore-countRows(t, with, upTo, mark), theirsBefore-countRows(t, with, after, mark)
		aloneGone := aloneBefore - countRows(t, alone, `SELECT COUNT(*) FROM events`)
		if theirsGone != due["events"] {
			t.Errorf("trial %d: prune removed %d of the restored rows, but PruneDue counted %d", trial, theirsGone, due["events"])
		}
		if mineGone-aloneGone != closed {
			t.Errorf("trial %d: prune removed %d of the install's own rows, %d of them only because of the restore, "+
				"but PruneDueClosedOutages counted %d", trial, mineGone, mineGone-aloneGone, closed)
		}
		closedSeen += closed
		if t.Failed() {
			return
		}
	}
	if closedSeen == 0 {
		t.Fatal("no trial had a restored recovery end one of the install's outages; the histories no longer test anything")
	}
}
