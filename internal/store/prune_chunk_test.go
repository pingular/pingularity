package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// PRUNE DELETES IN BOUNDED CHUNKS.
//
// Prune ran one DELETE per table. Over a big backlog (retention lowered, a box
// that was off for weeks, a restore of old rows) one of those held SQLite's
// single writer for seconds, and a probe write that waits 5 s for the writer is
// logged and dropped. The deletes now run in chunks of pruneChunkRows with a
// wait after every full one. These tests pin what that must not change (the
// rows that go, the count returned, whole outages) and what it must add (a
// bound between waits, a free writer in the gaps, a clean stop).

// pruneChunks sets the chunk bound and the wait for one test and puts all
// three seams back afterwards. Store tests never run in parallel, so swapping
// package variables is safe.
func pruneChunks(t *testing.T, rows int64, pause time.Duration) {
	t.Helper()
	pr, pp, ph := pruneChunkRows, pruneChunkPause, pruneChunkHook
	pruneChunkRows, pruneChunkPause = rows, pause
	t.Cleanup(func() { pruneChunkRows, pruneChunkPause, pruneChunkHook = pr, pp, ph })
}

// cacheDrops reads how many times the read caches have been dropped.
func cacheDrops(s *Store) uint64 {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	return s.recGen
}

// oraclePrune is the sweep as Prune ran it before chunking: the same steps in
// the same order, one statement per table. It stays here as the definition of
// which rows a prune removes.
func oraclePrune(t *testing.T, s *Store, start, samplesBefore, speedBefore, eventsBefore time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	s.maybeRepairFuturePauses()
	horizon := start.Add(pruneFutureSlack).Unix()
	if err := s.resolveDanglingDowns(ctx, samplesBefore.Unix(), start.Unix()); err != nil {
		t.Fatal(err)
	}
	var total int64
	run := func(q string, args ...any) {
		res, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		n, _ := res.RowsAffected()
		total += n
	}
	run(`DELETE FROM samples WHERE ts < ? OR ts > ?`, samplesBefore.Unix(), horizon)
	run(`DELETE FROM dns WHERE ts < ? OR ts > ?`, samplesBefore.Unix(), horizon)
	run(`DELETE FROM speed WHERE ts < ? OR ts > ?`, speedBefore.Unix(), horizon)
	run(`DELETE FROM speed_servers WHERE run_ts < ? OR run_ts > ?`, speedBefore.Unix(), horizon)
	run(`DELETE FROM speed_spans WHERE ts > ? OR (ts < ? AND ts + duration_s < ?)`,
		horizon, samplesBefore.Unix(), samplesBefore.Unix())
	ec := eventsBefore.Unix()
	run(`
		DELETE FROM events
		WHERE ts > ?
		   OR (ts < ? AND NOT (
		         type = 'down'
		         AND (SELECT MIN(u.ts) FROM events u WHERE u.type = 'up' AND u.ts > events.ts) >= ?))`,
		horizon, ec, ec)
	if s.pauseRepairArmed() {
		run(`DELETE FROM pauses WHERE ts + duration_s < ?`, ec)
	} else {
		run(`DELETE FROM pauses WHERE ts > ? OR ts + duration_s < ?`, horizon, ec)
	}
	return total
}

// prunedTablesDump lists every row of every table a prune can touch, in a
// fixed order, so two stores can be compared as text.
func prunedTablesDump(t *testing.T, s *Store) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT 'samples', ts, target, 0 FROM samples ORDER BY ts, target`,
		`SELECT 'dns', ts, '', 0 FROM dns ORDER BY ts`,
		`SELECT 'speed', ts, '', 0 FROM speed ORDER BY ts`,
		`SELECT 'speed_servers', run_ts, COALESCE(server_id,''), 0 FROM speed_servers ORDER BY run_ts, server_id`,
		`SELECT 'speed_spans', ts, '', duration_s FROM speed_spans ORDER BY ts, duration_s`,
		`SELECT 'events', ts, type, COALESCE(duration_s,-1) FROM events ORDER BY ts, type, duration_s`,
		`SELECT 'pauses', ts, '', duration_s FROM pauses ORDER BY ts, duration_s`,
		`SELECT 'pauses_quarantine', ts, '', duration_s FROM pauses_quarantine ORDER BY ts, duration_s`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for rows.Next() {
			var tbl, txt string
			var ts, d int64
			if err := rows.Scan(&tbl, &ts, &txt, &d); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %d %s %d\n", tbl, ts, txt, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// seedPruneHistory fills all seven pruned tables with rows on both sides of
// every cutoff: old, kept, ahead of the clock inside the slack, and past the
// future horizon. Spans and outages straddle the cutoffs by chance.
func seedPruneHistory(t *testing.T, s *Store, now time.Time, rng *rand.Rand) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; this is only the failure path
	ex := func(q string, args ...any) {
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	const day = int64(86400)
	nowU := now.Unix()
	pick := func() int64 {
		switch rng.Intn(20) {
		case 0:
			return nowU + 3*day + int64(rng.Intn(1000)) // past the future horizon
		case 1:
			return nowU + int64(rng.Intn(40*3600)) // ahead, inside the slack
		}
		return nowU - int64(rng.Intn(int(120*day)))
	}
	for i := 0; i < 60+rng.Intn(60); i++ {
		ts := pick()
		ok := rng.Intn(3) > 0
		for _, tg := range []string{"a", "b", "c"} {
			ex(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, ?, ?, 'ipv4')`, ts, tg, 10.0, ok)
		}
		ex(`INSERT INTO dns (ts, latency_ms, success) VALUES (?, 5.0, 1)`, ts)
	}
	for i := 0; i < 30+rng.Intn(30); i++ {
		ts := pick()
		ex(`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?, 100, 10, 5)`, ts)
		for k := 0; k < 3; k++ {
			ex(`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?, ?, 1, 1, 0)`, ts, fmt.Sprint(k))
		}
		ex(`INSERT INTO speed_spans (ts, duration_s) VALUES (?, ?)`, pick(), 1+rng.Intn(3600))
	}
	for i := 0; i < 20+rng.Intn(60); i++ {
		typ := "down"
		if rng.Intn(5) < 2 {
			typ = "up"
		}
		var dur any
		if typ == "up" {
			dur = rng.Intn(600)
		}
		ex(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, ?, ?, '')`, pick(), typ, dur)
	}
	for i := 0; i < 20+rng.Intn(40); i++ {
		ex(`INSERT INTO pauses (ts, duration_s) VALUES (?, ?)`, pick(), 1+rng.Intn(int(40*day)))
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// randomHistories is how many random histories a comparison runs. The race
// detector makes each one many times slower and has nothing to find in them,
// since they run on one goroutine, so it gets a third.
func randomHistories(n int) int {
	if raceEnabled || testing.Short() {
		return n / 3
	}
	return n
}

// The rows a chunked prune removes, and the count it returns, are the rows and
// the count of the one-statement prune, whatever the chunk size.
func TestPruneChunkedMatchesOneStatement(t *testing.T) {
	for trial := 0; trial < randomHistories(60); trial++ {
		now := time.Now()
		a, b := open(t), open(t)
		seedPruneHistory(t, a, now, rand.New(rand.NewSource(int64(trial))))
		seedPruneHistory(t, b, now, rand.New(rand.NewSource(int64(trial))))
		if prunedTablesDump(t, a) != prunedTablesDump(t, b) {
			t.Fatal("premise: the two stores differ before the prune")
		}
		pruneChunks(t, int64(1+trial%7), 0)
		chunks := 0
		pruneChunkHook = func(_ string, rows int64, _ time.Duration, _ bool) {
			if rows > 0 {
				chunks++
			}
		}
		clockAt(t, a, now, now, 0)
		sb, pb, eb := now.Add(-30*24*time.Hour), now.Add(-60*24*time.Hour), now.Add(-90*24*time.Hour)
		got, err := a.Prune(context.Background(), sb, pb, eb)
		if err != nil {
			t.Fatal(err)
		}
		want := oraclePrune(t, b, now, sb, pb, eb)
		if got != want {
			t.Errorf("trial %d (chunk %d): chunked prune removed %d rows, one statement per table removed %d",
				trial, pruneChunkRows, got, want)
		}
		if da, db := prunedTablesDump(t, a), prunedTablesDump(t, b); da != db {
			t.Errorf("trial %d (chunk %d): rows left differ\nchunked:\n%s\none statement:\n%s", trial, pruneChunkRows, da, db)
		}
		if chunks < 20 {
			t.Errorf("trial %d: only %d chunks removed rows; the history is too small to test chunking", trial, chunks)
		}
		a.Close()
		b.Close()
		if t.Failed() {
			return
		}
	}
}

// eventRows lists the events table as "ts type" lines.
func eventRows(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT ts, type FROM events ORDER BY ts, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts int64
		var typ string
		if err := rows.Scan(&ts, &typ); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s", ts, typ))
	}
	return out
}

// The final state. A 'down' may go only with an 'up' that closes it under the
// cutoff, and the sub-select that checks this reads the table as earlier
// chunks left it. Taking the 'up' first would leave the 'down' looking at the
// NEXT recovery, the one past the cutoff, and keep it as a false straddler.
func TestPruneKeepsWholeOutagesAcrossChunks(t *testing.T) {
	s := open(t)
	now := time.Now()
	cut := now.Add(-90 * 24 * time.Hour).Unix()
	for _, e := range []struct {
		ts  int64
		typ string
		dur any
	}{
		{cut - 1000 + 10, "down", nil},
		{cut - 1000 + 20, "up", 10},
		{cut + 5, "up", 3},
	} {
		if _, err := s.db.Exec(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, ?, ?, '')`, e.ts, e.typ, e.dur); err != nil {
			t.Fatal(err)
		}
	}
	pruneChunks(t, 1, 0)
	clockAt(t, s, now, now, 0)
	epoch := time.Unix(0, 0)
	n, err := s.Prune(context.Background(), epoch, epoch, time.Unix(cut, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{fmt.Sprintf("%d up", cut+5)}
	if got := eventRows(t, s); n != 2 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("prune removed %d rows and left %v; want 2 removed and %v left: the 'down' goes with the 'up' that closed it", n, got, want)
	}
}

// outageReadings is what the readers of the events table answer: every closed
// outage there is as one line, and the totals for the window from since on.
type outageReadings struct {
	outages []string
	window  string
}

func readOutages(t *testing.T, s *Store, since, now time.Time) outageReadings {
	t.Helper()
	ctx := context.Background()
	var r outageReadings
	all, err := s.completedOutagesSince(ctx, 0, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range all {
		r.outages = append(r.outages, fmt.Sprintf("start=%d end=%d observed=%d", o.start, o.end, o.observed))
	}
	var b strings.Builder
	kept, err := s.completedOutagesSince(ctx, since.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	union, err := s.pauseUnionFor(ctx, kept, since.Unix(), now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "observed downtime %d\n", observedDowntimeIn(kept, union, since.Unix(), now.Unix()))
	c, d, err := s.ResolvedOutagesSince(ctx, since.Unix())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "resolved %d, down %d s\n", c, d)
	days, err := s.downtimeByDayAt(ctx, since, time.UTC, now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	for _, dd := range days {
		if dd.Outages != 0 || dd.DowntimeS != 0 {
			fmt.Fprintf(&b, "%s outages %d, down %d s\n", dd.Date, dd.Outages, dd.DowntimeS)
		}
	}
	r.window = b.String()
	return r
}

// seedOutageHistory writes about forty outages across the events cutoff, with
// the shapes that have gone wrong before mixed in: a 'down' written twice, a
// recovery with no 'down', a 'down' and an 'up' in the same second. Every
// recovery's duration_s is HALF the wall span, the way a pause inside the
// outage leaves it. That is what makes a cut visible: an 'up' that lost its
// 'down' is read as starting at ts - duration_s, which is then not the start.
// Some future-dated pairs go in too, for the future arm.
func seedOutageHistory(t *testing.T, s *Store, now time.Time, cut int64, rng *rand.Rand) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; this is only the failure path
	ev := func(ts int64, typ string, dur any) {
		if _, err := tx.Exec(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, ?, ?, '')`, ts, typ, dur); err != nil {
			t.Fatal(err)
		}
	}
	const day = int64(86400)
	ts := cut - 110*day
	for i := 0; i < 40; i++ {
		ts += int64(1 + rng.Intn(int(5*day)))
		switch rng.Intn(6) {
		case 0:
			ev(ts, "down", nil)
			ev(ts+5, "down", nil)
		case 1:
			ev(ts, "up", 30)
		case 2:
			ev(ts, "up", 30)
			ev(ts, "down", nil)
			continue
		default:
			ev(ts, "down", nil)
		}
		d := int64(10 + rng.Intn(3000))
		ev(ts+d, "up", d/2)
		ts += d
	}
	if ts >= now.Unix() {
		t.Fatalf("premise: the history ran past now (%d >= %d)", ts, now.Unix())
	}
	fts := now.Unix() + 3*day
	for i := 0; i < 4; i++ {
		ev(fts, "down", nil)
		ev(fts+100, "up", 50)
		fts += 1000
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// Every state a reader can meet, not only the last. A pass can be stopped
// after any chunk, and the wait after a full one is long enough for a request.
// So after EVERY events chunk each outage the readers report must be one they
// reported before the pass, or one the one-statement prune ends on. The second
// half is for a quirk that statement has always had: readers pair a 'down' and
// an 'up' written in the same second, its predicate does not, so at the cutoff
// it can end on a pairing that was not there before.
func TestPruneEventsChunksEndOnWholeOutages(t *testing.T) {
	const day = 24 * time.Hour
	checked, trials := 0, randomHistories(100)
	for trial := 0; trial < trials; trial++ {
		now := time.Now().Truncate(time.Second)
		cut := now.Add(-90 * day)
		s, twin := open(t), open(t)
		seedOutageHistory(t, s, now, cut.Unix(), rand.New(rand.NewSource(int64(trial))))
		seedOutageHistory(t, twin, now, cut.Unix(), rand.New(rand.NewSource(int64(trial))))
		epoch := time.Unix(0, 0)
		oraclePrune(t, twin, now, epoch, epoch, cut)
		final := readOutages(t, twin, cut, now)

		pruneChunks(t, int64(1+trial%5), 0)
		var before outageReadings
		known := map[string]bool{}
		for _, o := range final.outages {
			known[o] = true
		}
		first, chunk := true, 0
		pruneChunkHook = func(q string, rows int64, _ time.Duration, _ bool) {
			if first {
				// The first call is for the samples arm: the dangling step has
				// run and no event has gone yet.
				first = false
				before = readOutages(t, s, cut, now)
				for _, o := range before.outages {
					known[o] = true
				}
			}
			if !strings.Contains(q, "FROM events") || rows == 0 {
				return
			}
			chunk++
			checked++
			got := readOutages(t, s, cut, now)
			for _, o := range got.outages {
				if !known[o] {
					t.Errorf("trial %d (chunk size %d), after events chunk %d: the readers report an outage that was "+
						"never there, %s. A chunk ended inside an outage.", trial, pruneChunkRows, chunk, o)
				}
			}
			if got.window != before.window && got.window != final.window {
				t.Errorf("trial %d (chunk size %d), after events chunk %d: the kept window reads\n%s"+
					"before the pass it read\n%sand the one-statement prune ends on\n%s",
					trial, pruneChunkRows, chunk, got.window, before.window, final.window)
			}
		}
		clockAt(t, s, now, now, 0)
		if _, err := s.Prune(context.Background(), epoch, epoch, cut); err != nil {
			t.Fatal(err)
		}
		pruneChunkHook = nil
		if got, want := strings.Join(eventRows(t, s), "\n"), strings.Join(eventRows(t, twin), "\n"); got != want {
			t.Errorf("trial %d: events left differ\nchunked:\n%s\none statement:\n%s", trial, got, want)
		}
		s.Close()
		twin.Close()
		if t.Failed() {
			return
		}
	}
	if checked < 5*trials {
		t.Errorf("only %d events chunks were checked over %d histories; they are too small to cut", checked, trials)
	}
}

// No more than the bound goes between two waits, and the bound is the PASS's:
// a chunk gets what the tables before it left over. Reset per table, a run of
// nearly full tables adds up to one long hold with no wait in it.
//
// The events rule is the one exception and it is pinned here too. Its limit
// counts recoveries, so with a 'down' for every 'up' it removes twice the rows
// it names.
func TestPruneDeletesNoMoreThanTheBoundBetweenWaits(t *testing.T) {
	s := open(t)
	now := time.Now().Truncate(time.Second)
	const bound = 10
	old := now.Add(-200 * 24 * time.Hour).Unix()
	far := now.Add(100 * time.Hour).Unix()
	ex := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// Sizes that are not multiples of the bound, so partial chunks meet at
	// every table border.
	for i := int64(0); i < 25; i++ {
		ex(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, 'a', 10, 1, 'ipv4')`, old+i)
	}
	for i := int64(0); i < 3; i++ {
		ex(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, 'a', 10, 1, 'ipv4')`, far+i)
	}
	for i := int64(0); i < 7; i++ {
		ex(`INSERT INTO dns (ts, latency_ms, success) VALUES (?, 5.0, 1)`, old+i)
	}
	for i := int64(0); i < 13; i++ {
		ex(`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?, 100, 10, 5)`, old+i)
	}
	for i := int64(0); i < 9; i++ {
		ex(`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?, 's', 1, 1, 0)`, old+i)
	}
	for i := int64(0); i < 4; i++ {
		ex(`INSERT INTO speed_spans (ts, duration_s) VALUES (?, 30)`, old+i)
	}
	const outages = 17
	for i := int64(0); i < outages; i++ {
		ex(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, 'down', NULL, '')`, old+100*i)
		ex(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, 'up', 50, '')`, old+100*i+50)
	}
	for i := int64(0); i < 3; i++ {
		ex(`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, 'up', 50, '')`, far+i)
	}
	for i := int64(0); i < 12; i++ {
		ex(`INSERT INTO pauses (ts, duration_s) VALUES (?, 60)`, old+i)
	}
	const seeded = 25 + 3 + 7 + 13 + 9 + 4 + 2*outages + 3 + 12

	pruneChunks(t, bound, 0)
	upsLeft := func() int64 {
		var n int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type = 'up' AND ts < ?`, now.Unix()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	ups := upsLeft()
	// limit mirrors the helper's budget: what is left of the bound, and the
	// whole bound again after a full chunk.
	limit := int64(bound)
	var between, waits int64
	outageChunk, tables := false, map[string]bool{}
	mixed := 0
	pruneChunkHook = func(q string, rows int64, _ time.Duration, full bool) {
		byOutage := strings.Contains(q, "WITH b(ts)")
		if byOutage {
			left := upsLeft()
			if took := ups - left; took > limit {
				t.Errorf("an events chunk took %d recoveries, its limit was %d", took, limit)
			}
			ups = left
			if rows > 2*limit {
				t.Errorf("an events chunk removed %d rows, more than a 'down' and an 'up' for each of its %d recoveries", rows, limit)
			}
			outageChunk = outageChunk || rows > 0
		} else if rows > limit {
			t.Errorf("a chunk removed %d rows, its limit was %d: %s", rows, limit, strings.Join(strings.Fields(q), " "))
		}
		between += rows
		if rows > 0 {
			tables[pruneTableOf(t, q)] = true
		}
		most := int64(bound)
		if outageChunk {
			most = 2 * bound
		}
		if between > most {
			t.Errorf("%d rows went between two waits, the bound is %d (%d where an events chunk is part of it)", between, bound, 2*bound)
		}
		if full != (rows >= limit) {
			t.Errorf("a chunk of %d rows with a limit of %d reported full = %v", rows, limit, full)
		}
		if !full {
			limit -= rows
			return
		}
		if len(tables) > 1 {
			mixed++
		}
		waits++
		limit, between, outageChunk, tables = bound, 0, false, map[string]bool{}
	}
	clockAt(t, s, now, now, 0)
	cut := now.Add(-24 * time.Hour)
	n, err := s.Prune(context.Background(), cut, cut, cut)
	if err != nil {
		t.Fatal(err)
	}
	if n != seeded {
		t.Errorf("prune removed %d rows, want all %d seeded", n, seeded)
	}
	if mixed < 3 {
		t.Errorf("only %d stretches between waits crossed a table border; the fixture no longer tests the shared bound", mixed)
	}
	if waits < seeded/(2*bound) {
		t.Errorf("%d waits for %d rows at a bound of %d", waits, seeded, bound)
	}
}

// pruneTableOf names the table a prune statement deletes from.
func pruneTableOf(t *testing.T, q string) string {
	t.Helper()
	m := regexp.MustCompile(`DELETE FROM (\w+)`).FindStringSubmatch(q)
	if m == nil {
		t.Fatalf("no DELETE FROM in %q", q)
	}
	return m[1]
}

// secondWriter opens its own connection to the store's file, with the pragmas
// a store connection gets. A write through it is a real commit by another
// writer, whatever the store's own inserts come to do before they reach disk.
func secondWriter(t *testing.T, path string) *sql.DB {
	t.Helper()
	w, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	t.Cleanup(func() { w.Close() })
	return w
}

// seedExpiredSamples writes n sample rows a second apart, the newest one at
// newest, in one statement.
func seedExpiredSamples(t *testing.T, s *Store, n int, newest time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < ? - 1)
		INSERT INTO samples (ts, target, latency_ms, success, family)
		SELECT ? - i, 'a', 10.0, 1, 'ipv4' FROM n`, n, newest.Unix()); err != nil {
		t.Fatalf("seed %d samples: %v", n, err)
	}
}

func sampleCount(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// File-backed on purpose: a :memory: store has one connection, so no second
// writer can ever be seen waiting. After every chunk another connection must
// get the writer at once. A transaction left open across chunks would make it
// wait out its deadline.
func TestPruneReleasesTheWriterBetweenChunks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	now := time.Now()
	seedExpiredSamples(t, s, 35, now.Add(-time.Hour))
	w := secondWriter(t, path)
	pruneChunks(t, 10, 0)
	wrote := 0
	pruneChunkHook = func(string, int64, time.Duration, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := w.ExecContext(ctx,
			`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, 'live', 1.0, 1, 'ipv4')`, now.Unix()); err != nil {
			t.Errorf("a write between two chunks failed: %v", err)
			return
		}
		wrote++
	}
	clockAt(t, s, now, now, 0)
	epoch := time.Unix(0, 0)
	n, err := s.Prune(context.Background(), now.Add(-10*time.Minute), epoch, epoch)
	if err != nil || n != 35 {
		t.Fatalf("prune = %d, %v; want 35, nil", n, err)
	}
	if wrote < 4 {
		t.Errorf("%d writes ran between chunks, want one after each of the 4 sample chunks at least", wrote)
	}
	if got := sampleCount(t, s); got != int64(wrote) {
		t.Errorf("%d samples left, want the %d written between chunks", got, wrote)
	}
}

// The defect itself, at the shipped chunk size and wait, with a writer that
// never stops. Letting go of the lock between chunks is not enough: a writer
// that found it taken sleeps before it looks again, so chunks run back to back
// keep winning it. The wait is what lets the writer in, and the proof is that
// it commits in EVERY gap, not in some.
func TestPruneLetsProbeWritesInterleave(t *testing.T) {
	if testing.Short() {
		t.Skip("100k row fixture")
	}
	path := filepath.Join(t.TempDir(), "interleave.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	const rows = 100000
	now := time.Now()
	seedExpiredSamples(t, s, rows, now.Add(-time.Hour))
	w := secondWriter(t, path)
	pruneChunks(t, pruneChunkRows, pruneChunkPause) // the shipped values; this only restores the hook

	var commits atomic.Int64
	var longest time.Duration
	var werr error
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			began := time.Now()
			_, err := w.Exec(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, 'live', 1.0, 1, 'ipv4')`, now.Unix())
			if err != nil {
				werr = err
				return
			}
			if d := time.Since(began); d > longest {
				longest = d
			}
			commits.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	var marks []int64 // the writer's commits so far, read as each full chunk lands
	pruneChunkHook = func(_ string, _ int64, _ time.Duration, full bool) {
		if full {
			marks = append(marks, commits.Load())
		}
	}
	clockAt(t, s, now, now, 0)
	epoch := time.Unix(0, 0)
	n, err := s.Prune(context.Background(), now.Add(-10*time.Minute), epoch, epoch)
	marks = append(marks, commits.Load())
	close(stop)
	wg.Wait()
	if err != nil || n != rows {
		t.Fatalf("prune = %d, %v; want %d, nil", n, err, rows)
	}
	if werr != nil {
		t.Fatalf("the writer beside the prune failed: %v", werr)
	}
	if want := int(rows / pruneChunkRows); len(marks)-1 != want {
		t.Fatalf("%d full chunks, want %d", len(marks)-1, want)
	}
	t.Logf("commits at each full chunk %v, longest write %v", marks, longest.Round(time.Millisecond))
	if raceEnabled {
		t.Skip("wall-clock budgets are not meaningful under the race detector")
	}
	for i := 1; i < len(marks); i++ {
		if marks[i] <= marks[i-1] {
			t.Errorf("the writer committed nothing between full chunk %d and what followed it (commits %v): "+
				"the wait after a full chunk is what lets a waiting writer in", i, marks)
		}
	}
	if longest > time.Second {
		t.Errorf("a write waited %v beside the prune, against a busy_timeout of %v", longest, busyTimeout)
	}
}

// A stop between two chunks ends the pass at once, even with the wait set to
// an hour. What went stays gone and is counted, what is left is left for the
// next pass, the read caches drop once, and the pass is not counted as a prune.
func TestPruneStopsBetweenChunks(t *testing.T) {
	s := open(t)
	now := time.Now()
	seedExpiredSamples(t, s, 25, now.Add(-time.Hour))
	pruneChunks(t, 10, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pruneChunkHook = func(string, int64, time.Duration, bool) { cancel() }
	drops := cacheDrops(s)
	passes := stats.Lifetime().Counters["db.prune_count"]
	clockAt(t, s, now, now, 0)
	began := time.Now()
	epoch := time.Unix(0, 0)
	n, err := s.Prune(ctx, now.Add(-10*time.Minute), epoch, epoch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the stop took %v: the wait ignored the context", took)
	}
	if n != 10 {
		t.Errorf("prune reported %d rows, want the 10 of the one chunk that committed", n)
	}
	if got := sampleCount(t, s); got != 15 {
		t.Errorf("%d samples left, want 15", got)
	}
	if got := cacheDrops(s); got != drops+1 {
		t.Errorf("the read caches dropped %d times, want once: rows are gone, so a stopped pass drops them too", got-drops)
	}
	if got := stats.Lifetime().Counters["db.prune_count"]; got != passes {
		t.Errorf("db.prune_count moved by %d on a stopped pass", got-passes)
	}
	// The next pass removes the rest by the same rule.
	pruneChunkHook = nil
	pruneChunkPause = 0
	n, err = s.Prune(context.Background(), now.Add(-10*time.Minute), epoch, epoch)
	if err != nil || n != 15 {
		t.Fatalf("second pass = %d, %v; want 15, nil", n, err)
	}
	if got := cacheDrops(s); got != drops+2 {
		t.Errorf("the read caches dropped %d times over two passes, want 2", got-drops)
	}
	if got := stats.Lifetime().Counters["db.prune_count"]; got != passes+1 {
		t.Errorf("db.prune_count moved by %d over a stopped pass and a finished one, want 1", got-passes)
	}
}

// The same stop landing inside the wait, which is where a pass over a big
// backlog spends most of its time.
func TestPruneStopsDuringTheWait(t *testing.T) {
	s := open(t)
	now := time.Now()
	seedExpiredSamples(t, s, 25, now.Add(-time.Hour))
	pruneChunks(t, 10, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	pruneChunkHook = func(_ string, _ int64, _ time.Duration, full bool) {
		if full {
			once.Do(func() { time.AfterFunc(50*time.Millisecond, cancel) })
		}
	}
	drops := cacheDrops(s)
	clockAt(t, s, now, now, 0)
	began := time.Now()
	epoch := time.Unix(0, 0)
	n, err := s.Prune(ctx, now.Add(-10*time.Minute), epoch, epoch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Fatalf("the stop took %v: the wait ignored the context", took)
	}
	if n != 10 || sampleCount(t, s) != 15 {
		t.Errorf("prune reported %d rows and left %d, want 10 and 15", n, sampleCount(t, s))
	}
	if got := cacheDrops(s); got != drops+1 {
		t.Errorf("the read caches dropped %d times, want once", got-drops)
	}
}

// A chunk that fails after others committed. The rows that went are reported
// and the caches drop, the same as for a stop.
func TestPruneDropsCachesWhenAChunkFails(t *testing.T) {
	s := open(t)
	now := time.Now()
	seedExpiredSamples(t, s, 25, now.Add(-time.Hour))
	pruneChunks(t, 10, 0)
	broke := false
	pruneChunkHook = func(string, int64, time.Duration, bool) {
		if !broke {
			broke = true
			// The table the next arm but one deletes from, moved out of reach.
			if _, err := s.db.Exec(`ALTER TABLE dns RENAME TO dns_moved`); err != nil {
				t.Errorf("rename: %v", err)
			}
		}
	}
	drops := cacheDrops(s)
	passes := stats.Lifetime().Counters["db.prune_count"]
	clockAt(t, s, now, now, 0)
	epoch := time.Unix(0, 0)
	n, err := s.Prune(context.Background(), now.Add(-10*time.Minute), epoch, epoch)
	if err == nil || !strings.Contains(err.Error(), "dns") {
		t.Fatalf("err = %v, want the failure on the moved table", err)
	}
	if n != 25 || sampleCount(t, s) != 0 {
		t.Errorf("prune reported %d rows and left %d samples, want 25 and 0", n, sampleCount(t, s))
	}
	if got := cacheDrops(s); got != drops+1 {
		t.Errorf("the read caches dropped %d times, want once", got-drops)
	}
	if got := stats.Lifetime().Counters["db.prune_count"]; got != passes {
		t.Errorf("db.prune_count moved by %d on a failed pass", got-passes)
	}
}

// Once per pass, however many chunks it took. A drop per chunk would throw the
// chart cache away every 150 ms for the length of a big cleanup.
func TestPruneDropsCachesOnce(t *testing.T) {
	s := open(t)
	now := time.Now()
	seedExpiredSamples(t, s, 45, now.Add(-time.Hour))
	pruneChunks(t, 10, 0)
	chunks := 0
	pruneChunkHook = func(_ string, rows int64, _ time.Duration, _ bool) {
		if rows > 0 {
			chunks++
		}
	}
	drops := cacheDrops(s)
	clockAt(t, s, now, now, 0)
	epoch := time.Unix(0, 0)
	if n, err := s.Prune(context.Background(), now.Add(-10*time.Minute), epoch, epoch); err != nil || n != 45 {
		t.Fatalf("prune = %d, %v; want 45, nil", n, err)
	}
	if chunks != 5 {
		t.Fatalf("premise: %d chunks removed rows, want 5", chunks)
	}
	if got := cacheDrops(s); got != drops+1 {
		t.Errorf("the read caches dropped %d times over a pass of %d chunks, want once", got-drops, chunks)
	}
}

// The armed state is asked before every chunk of the future pauses arm. Read
// once, a judgement armed while the pass was running (a restore does that)
// would find the rows it was armed to hold aside already deleted.
func TestPruneStopsTheFutureArmWhenRepairArmsMidPass(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "arm.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	base := time.Now()
	for i := int64(0); i < 5; i++ {
		seedLegacyPause(t, s, base.Add(72*time.Hour).Unix()+i, 300)
	}
	prev := pauseRepairFn
	pauseRepairFn = func(*sql.DB, int64) error { return errors.New("injected repair failure") }
	t.Cleanup(func() { pauseRepairFn = prev })
	pruneChunks(t, 2, 0)
	pruneChunkHook = func(q string, rows int64, _ time.Duration, _ bool) {
		if strings.Contains(q, "FROM pauses WHERE ts > ?") && rows > 0 && !s.pauseRepairArmed() {
			s.pauseRepairArm.Add(1)
		}
	}
	deferred := stats.Lifetime().Counters["db.prune_pauses_future_deferred"]
	clockAt(t, s, base, base, 0)
	cut := base.Add(-365 * 24 * time.Hour)
	n, err := s.Prune(context.Background(), cut, cut, cut)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if left := len(pauseDurations(t, s, "pauses")); left != 3 || n != 2 {
		t.Errorf("prune removed %d rows and left %d future pauses, want 2 and 3: the arm must stop at the "+
			"chunk after the judgement was armed", n, left)
	}
	if got := stats.Lifetime().Counters["db.prune_pauses_future_deferred"]; got != deferred+1 {
		t.Errorf("db.prune_pauses_future_deferred moved by %d, want 1", got-deferred)
	}
}

// pruneStatements runs one pass over an empty store and returns every
// statement it ran, in order. Every arm runs at least once whatever the
// tables hold, so this is the whole list.
func pruneStatements(t *testing.T, s *Store) []string {
	t.Helper()
	pruneChunks(t, pruneChunkRows, 0)
	var qs []string
	pruneChunkHook = func(q string, _ int64, _ time.Duration, _ bool) { qs = append(qs, q) }
	now := time.Now()
	clockAt(t, s, now, now, 0)
	cut := now.Add(-24 * time.Hour)
	if _, err := s.Prune(context.Background(), cut, cut, cut); err != nil {
		t.Fatalf("prune: %v", err)
	}
	pruneChunkHook = nil
	return qs
}

// A chunk must find its rows by a seek. Each statement runs once per chunk,
// so a scan or a sort in one is paid hundreds of times over a big backlog.
// The ended pauses arm scans, as the statement it replaces did.
func TestPruneChunksSeekTheTsIndex(t *testing.T) {
	s := open(t)
	qs := pruneStatements(t, s)
	if len(qs) != 14 {
		t.Fatalf("a pass ran %d statements, want 14 (two arms for each of seven tables)", len(qs))
	}
	scans := 0
	for _, q := range qs {
		table := pruneTableOf(t, q)
		args := make([]any, strings.Count(q, "?"))
		if strings.Contains(q, "?1") {
			args = make([]any, 2) // numbered: the cutoff and the limit
		}
		for i := range args {
			args[i] = int64(1700000000)
		}
		plan := queryPlan(t, s, q, args...)
		flat := strings.Join(strings.Fields(q), " ")
		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("%s\nsorts: %s", flat, plan)
		}
		if strings.Contains(flat, "FROM pauses WHERE ts + duration_s") {
			scans++
			if !strings.Contains(plan, "SCAN pauses") {
				t.Errorf("%s\nplan %q: the ended pauses arm was a scan; if it seeks now, say so here", flat, plan)
			}
			continue
		}
		if !strings.Contains(plan, "idx_"+table+"_ts") {
			t.Errorf("%s\nplan %q: want a seek on idx_%s_ts", flat, plan, table)
		}
		if strings.Contains(plan, "SCAN "+table) {
			t.Errorf("%s\nplan %q: scans %s", flat, plan, table)
		}
	}
	if scans != 1 {
		t.Errorf("%d statements matched the ended pauses arm, want 1", scans)
	}
}

// Every table is either pruned in chunks or listed here with the reason it is
// never pruned. A table added later has to land in one of the two.
func TestPruneChunksCoverEveryPrunedTable(t *testing.T) {
	exempt := map[string]string{
		"settings":          "configuration, not history",
		"server_health":     "one row per convicted speedtest server, keyed by its id; its own read drops the expired ones",
		"pauses_quarantine": "rows the pause repair holds aside for a later clock correction; Prune must never delete them",
	}
	s := open(t)
	pruned := map[string]int{}
	for _, q := range pruneStatements(t, s) {
		pruned[pruneTableOf(t, q)]++
	}
	want := []string{"dns", "events", "pauses", "samples", "speed", "speed_servers", "speed_spans"}
	var got []string
	for tbl, arms := range pruned {
		got = append(got, tbl)
		if arms != 2 {
			t.Errorf("%s has %d arms, want 2 (its cutoff and the future horizon)", tbl, arms)
		}
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("prune deletes from %v, want %v", got, want)
	}
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	inSchema := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		inSchema[name] = true
		_, isExempt := exempt[name]
		switch {
		case pruned[name] > 0 && isExempt:
			t.Errorf("%s is pruned and also listed as never pruned", name)
		case pruned[name] == 0 && !isExempt:
			t.Errorf("table %s is neither pruned nor listed as exempt: decide whether retention applies to it, "+
				"and if it does, delete from it through pruneChunked", name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for name := range exempt {
		if !inSchema[name] {
			t.Errorf("exempt lists %q, which is not a table any more", name)
		}
	}
	if len(inSchema) < len(want)+len(exempt) {
		t.Errorf("the schema scan found %d tables; it is no longer checking anything", len(inSchema))
	}
}

// The chunker is only a bound if every DELETE in Prune goes through it. This
// reads Prune's body, sliced the way TestEventTypeFilterCoversEveryEventRead
// slices a function, and refuses a statement run any other way.
func TestPruneRunsEveryDeleteThroughTheChunker(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	start := regexp.MustCompile(`(?m)^func \(s \*Store\) Prune\(`).FindIndex(src)
	if start == nil {
		t.Fatal("the source scan did not find Prune - it is no longer checking anything")
	}
	end := regexp.MustCompile(`(?m)^\}$`).FindIndex(src[start[0]:])
	if end == nil {
		t.Fatal("the source scan did not find the end of Prune")
	}
	body := stripLineComments(string(src[start[0] : start[0]+end[1]]))
	for _, direct := range []string{"ExecContext(", ".Exec(", "BeginTx(", ".Begin("} {
		if strings.Contains(body, direct) {
			t.Errorf("Prune calls %s itself: a statement run outside pruneChunked has no row bound and no wait, "+
				"and holds the writer for as long as its backlog is big", direct)
		}
	}
	if n := strings.Count(body, "s.pruneChunked("); n < 8 {
		t.Errorf("Prune calls pruneChunked %d times; the scan is reading the wrong text", n)
	}
	if n := strings.Count(body, "DELETE FROM"); n < 8 {
		t.Errorf("Prune holds %d DELETE statements; the scan is reading the wrong text", n)
	}
}

// An hour of rows is what the hourly pass of a settled install removes. At
// the default cadence (a round every 5 s, six targets and a DNS sample) that
// is 5040 rows, and the pass must go through without filling a chunk, so
// without a wait. A faster cadence can fill one: at a round a second the hour
// is 25200 rows and the pass waits once, which costs nothing.
func TestPruneHourlyPassNeverFillsAChunk(t *testing.T) {
	s := open(t)
	now := time.Now().Truncate(time.Second)
	cut := now.Add(-30 * 24 * time.Hour)
	var dual benchStack
	for _, st := range benchStacks() {
		if st.name == "dual" {
			dual = st
		}
	}
	seedSeriesInto(t, s, dual, time.Hour, cut, 5)
	// What else an hour leaves behind: a speedtest with its report and its
	// span, and an outage.
	old := cut.Add(-30 * time.Minute).Unix()
	for _, q := range []string{
		`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?1, 100, 10, 5)`,
		`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?1, 'a', 1, 1, 1)`,
		`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?1, 'b', 1, 1, 0)`,
		`INSERT INTO speed_spans (ts, duration_s) VALUES (?1, 30)`,
		`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1, 'down', NULL, '')`,
		`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1 + 60, 'up', 60, '')`,
		`INSERT INTO pauses (ts, duration_s) VALUES (?1, 300)`,
	} {
		if _, err := s.db.Exec(q, old); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	// The shipped bound, and a wait long enough that a pass which did wait
	// could not be missed. The hook stops such a pass instead of sitting it out.
	pruneChunks(t, pruneChunkRows, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	filled := 0
	pruneChunkHook = func(_ string, _ int64, _ time.Duration, full bool) {
		if full {
			filled++
			cancel()
		}
	}
	clockAt(t, s, now, now, 0)
	n, err := s.Prune(ctx, cut, cut, cut)
	const hour = 720*7 + 7
	if filled != 0 {
		t.Fatalf("an hourly pass of %d rows filled a chunk of %d after %d rows, and would wait after it", hour, pruneChunkRows, n)
	}
	if err != nil || n != hour {
		t.Errorf("prune = %d, %v; want %d, nil", n, err, hour)
	}
}
