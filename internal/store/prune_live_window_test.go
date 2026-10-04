package store

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// A RUNNING CLEANUP FOLLOWS A WINDOW RAISED WHILE IT RUNS.
//
// A pass was handed its cutoffs as it started and kept them to its end, and a
// pass over a big backlog runs for minutes. A window raised in that time - the
// fix for one lowered by mistake, and what a restore's warning advises for
// restored rows past a window - went on being cut at the old window until the
// pass ended: history the raised window keeps was deleted, restored rows among
// it, after the restore had replied. A pass now asks for the windows in force
// before every chunk it deletes by age (PruneLive), and cuts at the earlier of
// the cutoff it started at and theirs, measured from its start. These tests
// change the window between two chunks through the chunk seam and pin what
// that does to every table: a raise and a switch to keep forever stop the
// deleting at once, and a lowered window waits for the next pass.

// liveWindow is a retention window a test changes while a pass runs, the same
// for all three kinds of history.
type liveWindow struct{ d time.Duration }

// cutoffs is PruneLive's cutoffs function on the window, built the way
// runPruner builds its own from the settings: the start less the window, or
// the epoch for 0, which keeps forever (settings.PruneCutoff, which this
// package cannot import). It reads the window each time it is asked.
func (w *liveWindow) cutoffs(start time.Time) (time.Time, time.Time, time.Time) {
	c := time.Unix(0, 0)
	if w.d > 0 {
		c = start.Add(-w.d)
	}
	return c, c, c
}

// futureArm reports whether a pass statement is a table's future arm rather
// than its retention arm. Every future arm, and no retention arm, asks for a
// time later than a bare parameter: ts > ?.
func futureArm(q string) bool { return strings.Contains(q, "ts > ?") }

// agedRows stores n rows of table, the first as old as age and each two
// minutes younger than the one before; for events, n outages, each a 'down'
// and its 'up' a minute later, so its recovery is as old as the row's age
// less a minute.
func agedRows(t *testing.T, s *Store, table string, now time.Time, age time.Duration, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		ts := now.Add(-age).Unix() + int64(i)*120
		var qs []string
		switch table {
		case "samples":
			qs = []string{`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?1, 'a', 10, 1, 'ipv4')`}
		case "dns":
			qs = []string{`INSERT INTO dns (ts, latency_ms, success) VALUES (?1, 5, 1)`}
		case "speed":
			qs = []string{`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms) VALUES (?1, 100, 10, 5)`}
		case "speed_servers":
			qs = []string{`INSERT INTO speed_servers (run_ts, server_id, selected, measured, winner) VALUES (?1, 's', 1, 1, 0)`}
		case "speed_spans":
			qs = []string{`INSERT INTO speed_spans (ts, duration_s) VALUES (?1, 30)`}
		case "events":
			qs = []string{`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1, 'down', NULL, '')`,
				`INSERT INTO events (ts, type, duration_s, detail) VALUES (?1 + 60, 'up', 60, '')`}
		case "pauses":
			qs = []string{`INSERT INTO pauses (ts, duration_s) VALUES (?1, 60)`}
		default:
			t.Fatalf("no rows for table %s", table)
		}
		for _, q := range qs {
			if _, err := s.db.Exec(q, ts); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
}

// countAged counts the rows of table stamped from age back to age less span,
// the way agedRows lays them down.
func countAged(t *testing.T, s *Store, table string, now time.Time, age, span time.Duration) int64 {
	t.Helper()
	col := "ts"
	if table == "speed_servers" {
		col = "run_ts"
	}
	from := now.Add(-age).Unix()
	return countRows(t, s, `SELECT COUNT(*) FROM `+table+` WHERE `+col+` >= ? AND `+col+` < ?`, from, from+int64(span/time.Second))
}

// The window changes between two chunks of one table's retention arm, for
// every table the cleanup prunes by age. The table holds rows of four ages:
// older than every cutoff but keep forever's, past the window the pass starts
// on (an hour) but inside thirty days, inside the hour but past a minute, and
// stamped past the future horizon. The pass starts on the hour, and the window
// changes once the first chunk of the table's retention arm has deleted rows:
//
//   - raised to thirty days, the pass deletes nothing more that thirty days
//     keep. The rows older than thirty days still go, the ones after the
//     change at the raised cutoff, and a watch opened at the change notes that
//     cutoff, not the hour;
//   - set to keep forever, it deletes nothing more by age;
//   - lowered to a minute, it goes on at the hour, as the next pass would not:
//     the rows inside the hour stay;
//   - left alone, it deletes what the hour lets go.
//
// The future arm deletes what is past the horizon in every case.
func TestAWindowChangedBetweenTwoChunksTakesEffectAtTheNextOne(t *testing.T) {
	const (
		chunk   = 3
		day     = 24 * time.Hour
		ancient = 40 * day      // older than every cutoff but keep forever's
		old     = 3 * time.Hour // past the hour, inside thirty days
		young   = 30 * time.Minute
	)
	for _, table := range []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses"} {
		for _, tc := range []struct {
			name   string
			window time.Duration // the window after the change; -1 leaves it alone
			// What survives: of the ancient rows, all but the first chunk's
			// (keep forever) or none; the old rows; the young rows.
			ancientLeft, oldKept, youngKept bool
			// The cutoff a watch opened at the change notes: 0 for none.
			noted func(start time.Time) int64
		}{
			{"window left alone", -1, false, false, true,
				func(start time.Time) int64 { return start.Add(-time.Hour).Unix() }},
			{"window raised to thirty days", 30 * day, false, true, true,
				func(start time.Time) int64 { return start.Add(-30 * day).Unix() }},
			{"window set to keep forever", 0, true, true, true,
				func(time.Time) int64 { return 0 }},
			{"window lowered to a minute", time.Minute, false, false, true,
				func(start time.Time) int64 { return start.Add(-time.Hour).Unix() }},
		} {
			t.Run(table+"/"+tc.name, func(t *testing.T) {
				s := open(t)
				now := time.Now().Truncate(time.Second)
				agedRows(t, s, table, now, ancient, 2*chunk)
				agedRows(t, s, table, now, old, 3*chunk)
				agedRows(t, s, table, now, young, chunk)
				agedRows(t, s, table, now, -3*day, chunk) // past the future horizon
				rowsPer := int64(1)
				if table == "events" {
					rowsPer = 2 // an outage is two rows, and a chunk takes whole outages
				}
				span := func(n int) time.Duration { return time.Duration(n) * 2 * time.Minute }

				w := &liveWindow{d: time.Hour}
				// Every answer is for the pass's start, however long the pass
				// has run: the windows are measured from there.
				asked := map[int64]int{}
				cutoffs := func(start time.Time) (time.Time, time.Time, time.Time) {
					asked[start.UnixNano()]++
					return w.cutoffs(start)
				}
				pruneChunks(t, chunk, 0)
				var at *PruneWatch
				pruneChunkHook = func(q string, rows int64, _ time.Duration, _ bool) {
					if at != nil || rows == 0 || pruneTableOf(t, q) != table || futureArm(q) {
						return
					}
					if rows != chunk*rowsPer {
						t.Errorf("the first chunk of %s removed %d rows, want %d", table, rows, chunk*rowsPer)
					}
					if tc.window >= 0 {
						w.d = tc.window
					}
					at = s.WatchPrunes()
				}
				clockAt(t, s, now, now, 0)
				if _, err := s.PruneLive(context.Background(), cutoffs); err != nil {
					t.Fatalf("prune: %v", err)
				}
				if at == nil {
					t.Fatalf("the retention arm of %s deleted nothing, so the window never changed", table)
				}
				seen := at.Close()
				if len(asked) != 1 || asked[now.UnixNano()] < 3 {
					t.Errorf("the pass asked for the windows at %v, want only at its start, %d, and once before each "+
						"chunk it deleted by age", asked, now.UnixNano())
				}

				wantAncient := int64(0)
				if tc.ancientLeft {
					wantAncient = chunk * rowsPer // the first chunk went at the hour, before the change
				}
				if got := countAged(t, s, table, now, ancient, span(2*chunk)); got != wantAncient {
					t.Errorf("%d rows older than thirty days left, want %d", got, wantAncient)
				}
				wantOld := int64(0)
				if tc.oldKept {
					wantOld = 3 * chunk * rowsPer
				}
				if got := countAged(t, s, table, now, old, span(3*chunk)); got != wantOld {
					t.Errorf("%d rows past the hour but inside thirty days left, want %d", got, wantOld)
				}
				wantYoung := int64(0)
				if tc.youngKept {
					wantYoung = chunk * rowsPer
				}
				if got := countAged(t, s, table, now, young, span(chunk)); got != wantYoung {
					t.Errorf("%d rows inside the hour but past a minute left, want %d: a pass never cuts later than "+
						"it started at", got, wantYoung)
				}
				if got := countAged(t, s, table, now, -3*day, span(chunk)); got != 0 {
					t.Errorf("%d rows past the future horizon left, want none: no window moves the future arm", got)
				}
				if got, want := seen.Cut[table], tc.noted(now); got != want {
					t.Errorf("a watch opened at the change notes %s cut at %d, want %d (the hour's cutoff is %d)",
						table, got, want, now.Add(-time.Hour).Unix())
				}
			})
		}
	}
}

// copyPruned copies every row of every table a pass can touch from one store
// into another, so a pass's state at one chunk can be finished by the oracle.
func copyPruned(t *testing.T, from, to *Store) {
	t.Helper()
	for _, table := range []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses", "pauses_quarantine"} {
		rows, err := from.db.Query(`SELECT * FROM ` + table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var all [][]any
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			all = append(all, vals)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		marks := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
		for _, vals := range all {
			if _, err := to.db.Exec(`INSERT INTO `+table+` VALUES (`+marks+`)`, vals...); err != nil {
				t.Fatalf("copy %s: %v", table, err)
			}
		}
	}
}

// Over random histories, a window changed at a random point of the pass -
// raised, set to keep forever or lowered, for each kind of history on its own
// - leaves every table as the one-statement prune leaves the rows there were
// at that point, at the cutoffs the pass cuts at from then on: the earlier of
// the one it started at and the new window's, from the pass's start. The
// tables the pass had finished before the change are left as they were, and
// the oracle agrees: what a later cutoff left, an earlier one leaves too.
func TestAWindowChangedMidPassCutsFromThereLikeAPassAtTheNewCutoffs(t *testing.T) {
	const day = 24 * time.Hour
	order := []string{"samples", "dns", "speed", "speed_servers", "speed_spans", "events", "pauses"}
	for trial := 0; trial < randomHistories(60); trial++ {
		rng := rand.New(rand.NewSource(int64(trial)))
		now := time.Now().Truncate(time.Second)
		s := open(t)
		seedPruneHistory(t, s, now, rng)
		pruneChunks(t, int64(1+rng.Intn(5)), 0)

		began := [3]time.Duration{30 * day, 60 * day, 90 * day} // latency, speed, outages
		windows := began
		changed := began
		for i := range changed {
			switch rng.Intn(4) {
			case 0:
				changed[i] = began[i] + time.Duration(1+rng.Intn(40))*day // raised
			case 1:
				changed[i] = 0 // keep forever
			case 2:
				changed[i] = time.Duration(1+rng.Intn(20)) * day // lowered
			default:
				// left alone
			}
		}
		cutoffs := func(start time.Time) (time.Time, time.Time, time.Time) {
			var c [3]time.Time
			for i, w := range windows {
				c[i] = time.Unix(0, 0)
				if w > 0 {
					c[i] = start.Add(-w)
				}
			}
			return c[0], c[1], c[2]
		}
		// The change lands after the j-th chunk of one table's retention arm,
		// or at the first statement past that arm when it has fewer.
		target, j := rng.Intn(len(order)), 1+rng.Intn(3)
		index := func(table string) int {
			for i, tbl := range order {
				if tbl == table {
					return i
				}
			}
			t.Fatalf("a pass deleted from %s, which is not in the list", table)
			return -1
		}
		twin := open(t)
		seen, done := 0, false
		pruneChunkHook = func(q string, _ int64, _ time.Duration, _ bool) {
			if done {
				return
			}
			i := index(pruneTableOf(t, q))
			if i < target {
				return
			}
			if i == target && !futureArm(q) {
				if seen++; seen < j {
					return
				}
			}
			done = true
			copyPruned(t, s, twin)
			windows = changed
		}
		clockAt(t, s, now, now, 0)
		if _, err := s.PruneLive(context.Background(), cutoffs); err != nil {
			t.Fatalf("trial %d: prune: %v", trial, err)
		}
		pruneChunkHook = nil
		if !done {
			t.Fatalf("trial %d: the pass ended before the change", trial)
		}
		var at [3]time.Time
		for i := range at {
			w := changed[i]
			if w <= 0 {
				at[i] = time.Unix(0, 0)
			} else {
				at[i] = now.Add(-w)
			}
			if start := now.Add(-began[i]); start.Before(at[i]) {
				at[i] = start // a lowered window waits for the next pass
			}
		}
		oraclePrune(t, twin, now, at[0], at[1], at[2])
		if got, want := prunedTablesDump(t, s), prunedTablesDump(t, twin); got != want {
			t.Errorf("trial %d: windows %v changed to %v after chunk %d of %s: the pass left\n%s\nthe one-statement "+
				"prune of the rows at the change leaves\n%s", trial, began, changed, j, order[target], got, want)
		}
		s.Close()
		twin.Close()
		if t.Failed() {
			return
		}
	}
}

// The outage history in particular, read after every chunk. The chunk's
// cutoff is one value for the rule and for the recovery it ends on, and it
// can move from one chunk to the next: here the outage window is raised, or
// set to keep forever, after a random chunk of old outages. After every chunk
// the outages the readers report must be ones they reported before the pass,
// or ones a one-statement prune ends on - at the cutoff the pass started at
// from the history before it, or at the new cutoff from the rows left at the
// change. A chunk that ended inside an outage would show one that was never
// there. And the pass ends where that second prune ends.
func TestPruneEventsChunksEndOnWholeOutagesWhenTheWindowIsRaised(t *testing.T) {
	const day = 24 * time.Hour
	checked, changes, trials := 0, 0, randomHistories(80)
	epoch := time.Unix(0, 0)
	for trial := 0; trial < trials; trial++ {
		rng := rand.New(rand.NewSource(int64(trial)))
		now := time.Now().Truncate(time.Second)
		cut := now.Add(-90 * day)
		s, atStart := open(t), open(t)
		seedOutageHistory(t, s, now, cut.Unix(), rand.New(rand.NewSource(int64(trial))))
		seedOutageHistory(t, atStart, now, cut.Unix(), rand.New(rand.NewSource(int64(trial))))
		known := map[string]bool{}
		for _, o := range readOutages(t, s, cut, now).outages {
			known[o] = true
		}
		oraclePrune(t, atStart, now, epoch, epoch, cut)
		for _, o := range readOutages(t, atStart, cut, now).outages {
			known[o] = true
		}
		// Latency and speed are kept forever, so nothing but the outage window
		// moves, and it moves once.
		window := 90 * day
		cutoffs := func(start time.Time) (time.Time, time.Time, time.Time) {
			if window <= 0 {
				return epoch, epoch, epoch
			}
			return epoch, epoch, start.Add(-window)
		}
		raised := time.Duration(91+rng.Intn(30)) * day
		if rng.Intn(4) == 0 {
			raised = 0 // keep forever
		}
		raisedCut := epoch
		if raised > 0 {
			raisedCut = now.Add(-raised)
		}

		pruneChunks(t, int64(1+trial%3), 0)
		chunk, change := 0, 1+rng.Intn(3)
		var fromChange *Store
		pruneChunkHook = func(q string, rows int64, _ time.Duration, _ bool) {
			if !strings.Contains(q, "WITH b(ts)") || rows == 0 {
				return
			}
			chunk++
			checked++
			for _, o := range readOutages(t, s, cut, now).outages {
				if !known[o] {
					t.Errorf("trial %d (chunk size %d), after events chunk %d: the readers report an outage that was "+
						"never there, %s. A chunk ended inside an outage.", trial, pruneChunkRows, chunk, o)
				}
			}
			if chunk != change {
				return
			}
			// The rows at the change, pruned in one statement at the new cutoff:
			// what the pass must end on, and what it may show on the way.
			fromChange = open(t)
			copyPruned(t, s, fromChange)
			oraclePrune(t, fromChange, now, epoch, epoch, raisedCut)
			for _, o := range readOutages(t, fromChange, cut, now).outages {
				known[o] = true
			}
			window = raised
		}
		clockAt(t, s, now, now, 0)
		if _, err := s.PruneLive(context.Background(), cutoffs); err != nil {
			t.Fatal(err)
		}
		pruneChunkHook = nil
		if fromChange != nil {
			changes++
			if got, want := strings.Join(eventRows(t, s), "\n"), strings.Join(eventRows(t, fromChange), "\n"); got != want {
				t.Errorf("trial %d: after the window changed at events chunk %d the events left differ\npass:\n%s\n"+
					"one statement from the change:\n%s", trial, change, got, want)
			}
		}
		if t.Failed() {
			return
		}
	}
	if checked < 2*trials || changes < trials/2 {
		t.Errorf("%d events chunks were checked and %d windows changed over %d histories; they are too small to cut",
			checked, changes, trials)
	}
}

// The pass closes an outage the samples alone say ended (resolveDanglingDowns)
// at the cutoff it starts at, before its first chunk. When the window is then
// raised and the samples that proved the end stay, so does the end it wrote:
// the second the samples prove, with the length they give it. Every figure an
// operator reads - uptime, the digest, the heatmap - reads as before the
// pass, and a later pass on the raised window writes no second end. And when
// the raise is undone while the pass is still on the samples, the pass goes
// back to the cutoff it started at and deletes those samples after all: the
// end written at the start is what keeps the figures as they were.
func TestAnOutageClosedBeforeARaiseReadsAsItDidWithItsSamplesKept(t *testing.T) {
	const day = 24 * time.Hour
	for _, tc := range []struct {
		name string
		// The latency windows the pass is on after its first chunk of samples,
		// and after its second when there is one.
		after []time.Duration
		kept  bool // the samples that prove the end are still there afterwards
	}{
		{"raised to thirty days", []time.Duration{30 * day}, true},
		{"raised part way and put back", []time.Duration{2*time.Hour + 55*time.Minute, time.Hour}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			st := orphanedByARestart(t, now) // a 'down' three hours back, its end only in the samples
			since := ago(now, 4*3600)
			// One reading older than everything else, for the first chunk to
			// take before the window moves.
			mustInsert(t, st, round(ago(now, 5*3600), true)[:1])
			before := readAll(t, st, since)
			if before.dgOut != 1 || before.dgDownS != 600 {
				t.Fatalf("fixture: the digest reads %d outage(s) / %ds, want 1 / 600s", before.dgOut, before.dgDownS)
			}
			provedAt := ago(now, 3*3600-600).Unix() // the first good round after the 'down'

			// The latency window moves; speed and outages are kept forever, so
			// the outage itself stays whatever the samples do.
			latency := time.Hour
			cutoffs := func(start time.Time) (time.Time, time.Time, time.Time) {
				epoch := time.Unix(0, 0)
				return start.Add(-latency), epoch, epoch
			}
			pruneChunks(t, 1, 0)
			moved := 0
			pruneChunkHook = func(q string, rows int64, _ time.Duration, _ bool) {
				if rows > 0 && pruneTableOf(t, q) == "samples" && moved < len(tc.after) {
					latency = tc.after[moved]
					moved++
				}
			}
			clockAt(t, st, now, now, 0)
			if _, err := st.PruneLive(ctx, cutoffs); err != nil {
				t.Fatalf("prune: %v", err)
			}
			pruneChunkHook = nil
			if moved != len(tc.after) {
				t.Fatalf("fixture: the window moved %d times, want %d", moved, len(tc.after))
			}
			proof := countRows(t, st, `SELECT COUNT(*) FROM samples WHERE ts = ?`, provedAt)
			if tc.kept != (proof == 3) || !tc.kept && proof != 0 {
				t.Fatalf("fixture: %d of the 3 readings that prove the end are left, want kept %v", proof, tc.kept)
			}
			want := []string{fmt.Sprintf("%d 600", provedAt)}
			if got := closingEvents(t, st); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("the closing events (ts duration_s) are %q, want the end the samples prove, %q", got, want)
			}
			sameReadings(t, "after a pass that closed the outage and then had its window changed,", before, readAll(t, st, since))

			// The next pass, on the window as it stands, writes no second end.
			if _, err := st.PruneLive(ctx, cutoffs); err != nil {
				t.Fatalf("second prune: %v", err)
			}
			if got := closingEvents(t, st); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("after the next pass the closing events are %q, want %q", got, want)
			}
			sameReadings(t, "after the next pass,", before, readAll(t, st, since))
		})
	}
}
