package store

import (
	"context"
	"maps"
	"math/rand"
	"slices"
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

// rowIDs lists a table's rowids.
func rowIDs(t *testing.T, s *Store, table string) map[int64]bool {
	t.Helper()
	rows, err := s.db.Query(`SELECT rowid FROM ` + table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// goneFrom counts the rows of before that the table no longer holds, all of
// them or only the ones on one side of a mark. Counted by rowid, because a
// cleanup also writes rows: the ends its close gives outages that had none.
func goneFrom(t *testing.T, s *Store, table string, before map[int64]bool, keep func(id int64) bool) int64 {
	t.Helper()
	now := rowIDs(t, s, table)
	var n int64
	for id := range before {
		if !now[id] && (keep == nil || keep(id)) {
			n++
		}
	}
	return n
}

// Over random histories on both sides of every cutoff - straddling outages,
// open ones, outages whose end only old samples show, pauses running into the
// window, rows ahead of the clock - the rows PruneDue counts in each table are
// the rows the next Prune removes from it by age, and the rows PruneDueAhead
// counts are the ones it removes for being past the future horizon: together,
// every row the pass removes. The close Prune begins with is part of that
// Prune: the outages it ends past the cutoff go in the same pass, and PruneDue
// counts them, and an 'up' past the horizon that it moves back is not deleted
// for being ahead of the clock, and PruneDueAhead leaves it out.
//
// In two trials of three the running monitor holds an outage open, its 'down'
// written through the monitor's door at some second of the history. The close
// leaves every outage from that second on to the monitor and stops an earlier
// one's search there, so the cleanup ends fewer outages and moves fewer ends
// back, and both counts must leave out the same ones.
func TestPruneDueCountsWhatPruneRemovesByAge(t *testing.T) {
	ctx := context.Background()
	tables := []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses"}
	var counted, closedAndGone, leftToTheMonitor, ahead int64
	for trial := 0; trial < randomHistories(30); trial++ {
		now := time.Now()
		s := open(t)
		seedPruneHistory(t, s, now, rand.New(rand.NewSource(int64(trial))))
		sb, pb, eb := now.Add(-30*24*time.Hour), now.Add(-60*24*time.Hour), now.Add(-90*24*time.Hour)
		if trial%3 != 0 {
			// The ends the close records with no line at all: the ones past the
			// outage cutoff are the ones the monitor's line can take away.
			past := func(line func() int64) (downs []int64) {
				var plan []plannedClose
				if err := s.closeDanglingDowns(ctx, sb.Unix(), now.Unix(), line, &plan); err != nil {
					t.Fatal(err)
				}
				for _, c := range plan {
					if c.at < eb.Unix() {
						downs = append(downs, c.down)
					}
				}
				return downs
			}
			without := past(nil)
			// The monitor's 'down' at any second of the history, from a source of
			// its own so the histories stay the ones the seed gives. Every other
			// time it is put just before an outage the close would have ended.
			at := now.Add(-time.Duration(rand.New(rand.NewSource(int64(1000+trial))).Intn(120*86400)) * time.Second)
			if trial%3 == 2 && len(without) > 0 {
				at = time.Unix(slices.Min(without)-1, 0)
			}
			if err := s.InsertEvent(ctx, at, "down", -1, ""); err != nil {
				t.Fatalf("trial %d: the monitor's down: %v", trial, err)
			}
			leftToTheMonitor += int64(len(without) - len(past(s.live.line)))
		}
		clockAt(t, s, now, now, 0)
		// What the rules alone count, before the close is asked: the difference
		// is what the close adds, and some history must have some.
		rulesAlone := countRows(t, s, pruneDueCount("events"), eb.Unix(), int64(0))
		since := map[string]int64{}
		for _, tbl := range tables {
			since[tbl] = 0 // every row counts
		}
		due, err := s.PruneDue(ctx, sb, pb, eb, since)
		if err != nil {
			t.Fatalf("trial %d: PruneDue: %v", trial, err)
		}
		closedAndGone += due["events"] - rulesAlone
		future, err := s.PruneDueAhead(ctx, sb, since)
		if err != nil {
			t.Fatalf("trial %d: PruneDueAhead: %v", trial, err)
		}
		before := map[string]map[int64]bool{}
		for _, tbl := range tables {
			before[tbl] = rowIDs(t, s, tbl)
			ahead += future[tbl]
		}
		if _, err := s.Prune(ctx, sb, pb, eb); err != nil {
			t.Fatalf("trial %d: prune: %v", trial, err)
		}
		for _, tbl := range tables {
			removed := goneFrom(t, s, tbl, before[tbl], nil)
			if removed != due[tbl]+future[tbl] {
				t.Errorf("trial %d: %s: prune removed %d rows, but PruneDue counted %d by age and PruneDueAhead %d "+
					"past the future horizon", trial, tbl, removed, due[tbl], future[tbl])
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
	if closedAndGone == 0 {
		t.Fatal("no trial had an outage the close ended past the cutoff; the histories no longer test the close")
	}
	if leftToTheMonitor <= 0 {
		t.Fatal("no trial had the monitor's line keep the close from an outage it would have ended past the cutoff; " +
			"the histories no longer test the line")
	}
	if ahead == 0 {
		t.Fatal("no trial had a row past the future horizon; the histories no longer test the future arms")
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
// each row's recovery. The rows stamped past the future horizon are found the
// same way, table by table. The count of this install's own outage records that a
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
	for table := range pruneAheadCol {
		q := pruneDueAheadCount(table)
		plan := queryPlan(t, s, q, int64(1700000000), int64(5))
		if strings.Contains(plan, "SCAN") || !strings.Contains(plan, "SEARCH "+table) {
			t.Errorf("%s\nplan %q: want a SEARCH of %s and no SCAN", strings.Join(strings.Fields(q), " "), plan, table)
		}
	}
	// The event that followed an own 'down' before the restore: a seek of the ts
	// index to that second and a walk from there, never the whole table.
	plan = queryPlan(t, s, pruneDueOwnNext, int64(1700000000), int64(5))
	if strings.Contains(plan, "SCAN") || !strings.Contains(plan, "SEARCH events USING INDEX idx_events_ts (ts>?)") {
		t.Errorf("%s\nplan %q: want a SEARCH of events from that second on its ts index, and no SCAN",
			strings.Join(strings.Fields(pruneDueOwnNext), " "), plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") && !strings.Contains(plan, "FOR LAST TERM OF ORDER BY") {
		t.Errorf("plan %q: want the rows read in ts order, with a sort only among the events of one second", plan)
	}
	// The outages the cleanup's close ends past the cutoff: the 'down' rows on
	// one side of the mark that the rule keeps today, in the range of seconds
	// the ends reach, with the rule's seek for each one's recovery.
	for _, own := range []bool{false, true} {
		q := pruneDueKeptDowns(own)
		plan := queryPlan(t, s, q, int64(1700000000), int64(5), int64(1600000000), int64(1650000000))
		if strings.Contains(plan, "SCAN") || !strings.Contains(plan, "SEARCH events") ||
			!strings.Contains(plan, "INDEX idx_events_ts (ts>?)") {
			t.Errorf("%s\nplan %q: want a SEARCH of events, a seek of its ts index for each recovery, and no SCAN",
				strings.Join(strings.Fields(q), " "), plan)
		}
	}
}

// A cleanup begins by giving each outage that never got its end the second the
// old samples show (resolveDanglingDowns), and where that second is past the
// outage cutoff too, its sweep deletes the outage in the same pass. PruneDue
// used to count such an outage as the open one it still is, which the cleanup
// keeps, so a restore was told nothing of an outage the next cleanup deleted.
// Each case here is an install's own rows, then a restore's, under a latency
// window of thirty days and an outage window of a year. The restored rows
// PruneDue counts are the ones the next Prune removes from the rows after the
// marks, and the own records PruneDueClosedOutages counts are the ones it
// removes from the rows before them and would not have removed without the
// restore, which a second store holding the install's rows alone shows.
func TestPruneDueCountsTheOutagesTheCleanupsCloseEnds(t *testing.T) {
	ctx := context.Background()
	const day = int64(86400)
	now := time.Now()
	nowU := now.Unix()
	samplesCut, eventsCut := now.Add(-30*24*time.Hour), now.Add(-365*24*time.Hour)
	type row struct {
		q    string
		args []any
	}
	down := func(ago int64) row {
		return row{`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, 'down', NULL, '')`, []any{nowU - ago}}
	}
	up := func(ago int64) row {
		return row{`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, 'up', 60, '')`, []any{nowU - ago}}
	}
	// round is one probe round of three targets, all good or all bad.
	round := func(ago int64, ok bool) []row {
		var out []row
		for _, tg := range []string{"a", "b", "c"} {
			out = append(out, row{`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, 10, ?, 'ipv4')`,
				[]any{nowU - ago, tg, ok}})
		}
		return out
	}
	rows := func(parts ...any) []row {
		var out []row
		for _, p := range parts {
			switch p := p.(type) {
			case row:
				out = append(out, p)
			case []row:
				out = append(out, p...)
			}
		}
		return out
	}
	// An outage that began 400 days ago, and the rounds that show it over ten
	// minutes later.
	const began = 400 * day
	ended := rows(round(began, false), round(began-600, true), round(began-1200, true))
	for _, tc := range []struct {
		name           string
		held           int64 // how long ago the running monitor opened an outage it still holds; 0 for none
		mine, theirs   []row
		wantDue        int64 // restored outage rows the next cleanup deletes
		wantOwn        int64 // own outage rows it deletes only because of the restore
		wantDueSamples int64
	}{
		{"a restored outage with no end, which the restored readings show ended",
			0, nil, rows(ended, down(began)), 1, 0, 9},
		{"an outage of the install's own, which the restored readings show ended",
			0, rows(down(began)), rows(ended), 0, 1, 9},
		{"a restored outage whose end is inside the outage window",
			0, nil, rows(round(200*day, false), round(200*day-600, true), down(200*day)), 0, 0, 6},
		{"a restored outage whose end the readings inside the latency window show",
			0, nil, rows(round(10*day, false), round(10*day-600, true), down(10*day)), 0, 0, 0},
		{"an outage and the readings that end it, both the install's own",
			0, rows(ended, down(began)), rows(down(20*day), up(20*day-60)), 0, 0, 0},
		{"a restored outage whose own end is dated a day ahead",
			0, nil, rows(ended, down(began), up(-day)), 2, 0, 9},
		{"an outage of the install's own with a restored one after it, which the restored readings show ended",
			0, rows(down(began + 3600)), rows(ended, down(began)), 1, 1, 9},
		// Past the future horizon the 'up' would go for being ahead of the clock,
		// but the close moves it back first: it goes by age with its outage...
		{"a restored outage whose own end is dated three days ahead",
			0, nil, rows(ended, down(began), up(-3*day)), 2, 0, 9},
		// ...or stays with it, where the end the readings show is inside the
		// outage window.
		{"a restored outage inside the outage window whose own end is dated three days ahead",
			0, nil, rows(round(200*day, false), round(200*day-600, true), down(200*day), up(-3*day)), 0, 0, 6},
		// With no readings to show an earlier end it is only a row from the future.
		{"a restored outage whose only end is dated three days ahead",
			0, nil, rows(down(began), up(-3*day)), 0, 0, 0},
		// No restored reading here, and no restored end: a restored 'down' an hour
		// into an outage of the install's own, which straddles the outage cutoff.
		// It takes that outage's 'up' for its own, the install's outage reads as
		// having no end, and the close ends it from the install's own readings,
		// ten minutes in and past the cutoff.
		{"an outage of the install's own that straddles the cutoff, with a restored down inside it",
			0, rows(ended, down(began), up(200*day)), rows(down(began - 3600)), 0, 1, 0},
		// The same restored 'down' after an outage of the install's own that never
		// had an end, a later outage of the install's following it: the cleanup
		// would have ended that one anyway.
		{"an outage of the install's own with no end, with a restored down after it",
			0, rows(ended, down(began), down(300*day), up(300*day-60)), rows(down(began - 3600)), 0, 0, 0},
		// An outage the running monitor holds open is the monitor's to end. The
		// cleanup's close leaves it, whatever the restored readings show, so
		// the count names nothing...
		{"an outage the running monitor holds open, in which the restored readings show a good round",
			began, nil, rows(ended), 0, 0, 9},
		// ...and an end a restore brings for it, dated three days ahead, is
		// not moved back: it goes as a row ahead of the clock.
		{"an outage the running monitor holds open, with a restored end dated three days ahead",
			began, nil, rows(ended, up(-3*day)), 0, 0, 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			with, alone := open(t), open(t)
			put := func(s *Store, rs []row) {
				t.Helper()
				for _, r := range rs {
					if _, err := s.db.Exec(r.q, r.args...); err != nil {
						t.Fatalf("%s: %v", r.q, err)
					}
				}
			}
			put(with, tc.mine)
			put(alone, tc.mine)
			if tc.held != 0 {
				for _, s := range []*Store{with, alone} {
					if err := s.InsertEvent(ctx, time.Unix(nowU-tc.held, 0), "down", -1, ""); err != nil {
						t.Fatalf("the monitor's down: %v", err)
					}
				}
			}
			since := map[string]int64{}
			for _, tbl := range []string{"samples", "events"} {
				mark, err := with.RowMark(ctx, tbl)
				if err != nil {
					t.Fatal(err)
				}
				since[tbl] = mark
			}
			put(with, tc.theirs)
			clockAt(t, with, now, now, 0)
			due, err := with.PruneDue(ctx, samplesCut, samplesCut, eventsCut, since)
			if err != nil {
				t.Fatalf("PruneDue: %v", err)
			}
			own, err := with.PruneDueClosedOutages(ctx, samplesCut, eventsCut, since)
			if err != nil {
				t.Fatalf("PruneDueClosedOutages: %v", err)
			}
			ahead, err := with.PruneDueAhead(ctx, samplesCut, since)
			if err != nil {
				t.Fatalf("PruneDueAhead: %v", err)
			}
			if got := countRows(t, with, `SELECT COUNT(*) FROM events WHERE type = 'up' AND detail = 'recovered while unmonitored'`); got != 0 {
				t.Fatalf("the count wrote %d outage ends; it must write nothing", got)
			}
			if due["events"] != tc.wantDue || own != tc.wantOwn || due["samples"] != tc.wantDueSamples {
				t.Errorf("counted %d restored outage rows, %d of the install's own and %d restored readings; want %d, %d and %d",
					due["events"], own, due["samples"], tc.wantDue, tc.wantOwn, tc.wantDueSamples)
			}
			before, aloneBefore := rowIDs(t, with, "events"), rowIDs(t, alone, "events")
			for _, s := range []*Store{with, alone} {
				clockAt(t, s, now, now, 0)
				if _, err := s.Prune(ctx, samplesCut, samplesCut, eventsCut); err != nil {
					t.Fatalf("prune: %v", err)
				}
			}
			mark := since["events"]
			theirsGone := goneFrom(t, with, "events", before, func(id int64) bool { return id > mark })
			mineGone := goneFrom(t, with, "events", before, func(id int64) bool { return id <= mark })
			aloneGone := goneFrom(t, alone, "events", aloneBefore, nil)
			if theirsGone != due["events"]+ahead["events"] {
				t.Errorf("the next cleanup removed %d of the restored outage rows, but PruneDue counted %d and "+
					"PruneDueAhead %d ahead of the clock", theirsGone, due["events"], ahead["events"])
			}
			if mineGone-aloneGone != own {
				t.Errorf("the next cleanup removed %d of the install's own outage rows, %d of them only because of the "+
					"restore, but PruneDueClosedOutages counted %d", mineGone, mineGone-aloneGone, own)
			}
		})
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
		if err := s.resolveDanglingDowns(ctx, sb.Unix(), now.Unix(), s.live.line); err != nil {
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

	// The future arm's chunks note no cutoff: what they delete is stamped past
	// the horizon, not old, and no restored row younger than the cutoff is
	// among it for being young. A pass on an hour that finds nothing past its
	// cutoff and a row of every table three days ahead of the clock counts the
	// rows and notes no cutoff.
	ahead := s.WatchPrunes()
	oldRows(-3 * day)
	prune(ago(time.Hour), ago(time.Hour), ago(time.Hour))
	if got := ahead.Close(); len(got.Cut) != 0 || got.Rows["samples"] != 1 || got.Rows["events"] != 2 {
		t.Errorf("a pass that deleted only rows ahead of the clock left the watch holding %v; want a row of each "+
			"table, both of the outage's, and no cutoff", got)
	}
}

// A watch also notes a Delete now: the tables one has emptied while the watch
// was open, and the tables one is on as the watch is read. A restore tells its
// rows from the install's by rowid, and a table emptied under it hands its
// rowids out again (RowMark). A Delete now that deleted nothing - refused, or
// stopped before its delete - empties nothing and notes nothing, and neither
// does one from before the watch opened.
func TestAWatchNotesADeleteNow(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	sampleAt(t, s, now, 60, "a", "ipv4", true)
	if _, err := s.Clear(ctx, "speed"); err != nil { // before the watch
		t.Fatal(err)
	}
	w := s.WatchPrunes()
	if got := w.Tally(); len(got.Cleared) != 0 || len(got.Clearing) != 0 {
		t.Fatalf("no Delete now has run since the watch opened, but it holds %v", got)
	}

	// Read from inside a latency Delete now, between its close of the outages
	// and its delete: its tables are being emptied, and not emptied yet.
	var during PruneTally
	closeHook = func() { during = w.Tally() }
	t.Cleanup(func() { closeHook = nil })
	if n, err := s.Clear(ctx, "latency"); err != nil || n != 1 {
		t.Fatalf("Delete now: %d rows, %v; want the 1 sample", n, err)
	}
	closeHook = nil
	latency := map[string]bool{"samples": true, "dns": true, "speed_spans": true}
	if !maps.Equal(during.Clearing, latency) || len(during.Cleared) != 0 {
		t.Errorf("read in the middle of a latency Delete now the watch holds %v; want its three tables as being "+
			"emptied and none as emptied yet", during)
	}
	after := w.Tally()
	if want := map[string]int64{"samples": 1, "dns": 1, "speed_spans": 1}; !maps.Equal(after.Cleared, want) || len(after.Clearing) != 0 {
		t.Errorf("after the Delete now the watch holds %v; want each of its tables emptied once and none being emptied", after)
	}

	// One that deletes nothing notes nothing.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Clear(gone, "downtime"); err == nil {
		t.Fatal("fixture: a Delete now on a cancelled context succeeded")
	}
	if _, err := s.Clear(ctx, "everything"); err == nil {
		t.Fatal("fixture: a Delete now of a kind that does not exist succeeded")
	}
	if got := w.Tally(); !maps.Equal(got.Cleared, after.Cleared) || len(got.Clearing) != 0 {
		t.Errorf("two Delete nows that deleted nothing left the watch holding %v, want %v as before", got, after)
	}

	// A second one counts again, and a closed watch holds what it held.
	if _, err := s.Clear(ctx, "latency"); err != nil {
		t.Fatal(err)
	}
	closed := w.Close()
	if closed.Cleared["samples"] != 2 {
		t.Errorf("after two latency Delete nows the watch counts %d for samples, want 2", closed.Cleared["samples"])
	}
	if _, err := s.Clear(ctx, "latency"); err != nil {
		t.Fatal(err)
	}
	if again := w.Close(); !maps.Equal(again.Cleared, closed.Cleared) || !maps.Equal(w.Tally().Cleared, closed.Cleared) {
		t.Errorf("a closed watch moved from %v to %v with a later Delete now", closed, again)
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
		closed, err := with.PruneDueClosedOutages(ctx, epoch, cut, map[string]int64{"events": mark})
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
