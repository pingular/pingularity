package store

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// DELETE NOW ON THE LATENCY DATASET KEEPS THE LENGTH OF EACH OUTAGE THAT HAS ENDED.
//
// A restart in the middle of an outage leaves its 'down' with no 'up': the
// process that wrote it stopped before the link came back, and the next one
// starts out believing the link is up, so it never writes one. Only the latency
// samples say when that outage ended. Clear("latency") deleted them all with no
// step in between, so the readers bounded the outage at whatever the monitor
// wrote next: a ten-minute outage three hours back read as three hours long, in
// the uptime figure, the heatmap and the digest alike, and a later cleanup would
// have written that length down for good. The hourly cleanup never had this
// problem, because it closes such an outage before it prunes the samples
// (resolveDanglingDowns). Clear now does the same, with one difference these
// tests hold as firmly as the fix: it has no retention window between it and
// the present, so it must leave alone any outage the running monitor may still
// close by its own rule.

// restartedStore is the store a new process opens after the last one stopped:
// write runs against a first handle on a file, which is then closed, and the
// same file is opened again. A 'down' written there is one this handle's
// monitor never held, which is what a restart in the middle of an outage leaves.
// File-backed also means the pooled, multi-connection store the daemon runs.
func restartedStore(t *testing.T, write func(first *Store)) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "restarted.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("open the first process's store: %v", err)
	}
	write(first)
	if err := first.Close(); err != nil {
		t.Fatalf("close the first process's store: %v", err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open the store again: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// orphanedByARestart is the history the fix is for: the first process confirms
// an outage three hours ago and stops before the link comes back. The next
// process finds the link up from ten minutes after the outage began, and writes
// a round every ten minutes from then on. Only those rounds say when the outage
// ended.
func orphanedByARestart(t *testing.T, now time.Time) *Store {
	t.Helper()
	st := restartedStore(t, func(first *Store) {
		mustInsert(t, first, round(ago(now, 4*3600), true)) // monitoring anchor
		if err := first.InsertEvent(context.Background(), ago(now, 3*3600), "down", -1, ""); err != nil {
			t.Fatalf("insert down: %v", err)
		}
		mustInsert(t, first, round(ago(now, 3*3600), false))
	})
	for s := 3*3600 - 600; s >= 600; s -= 600 {
		mustInsert(t, st, round(ago(now, s), true))
	}
	return st
}

// closingEvents lists the 'up' rows as "ts duration_s" lines, oldest first.
func closingEvents(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT ts, COALESCE(duration_s, -1) FROM events WHERE type = 'up' ORDER BY ts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ts, dur int64
		if err := rows.Scan(&ts, &dur); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %d", ts, dur))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameReadings fails the test when two snapshots of the three outage readers
// (see readAll) disagree.
func sameReadings(t *testing.T, when string, before, after uptimeReadings) {
	t.Helper()
	if math.Abs(after.ratio-before.ratio) > 1e-4 || after.dgOut != before.dgOut || after.dgDownS != before.dgDownS ||
		after.hmOut != before.hmOut || after.hmDownS != before.hmDownS {
		t.Errorf("%s the outage history reads differently.\n"+
			"before: uptime %.5f, digest %d outage(s) / %ds, heatmap %d outage(s) / %ds\n"+
			"after:  uptime %.5f, digest %d outage(s) / %ds, heatmap %d outage(s) / %ds",
			when, before.ratio, before.dgOut, before.dgDownS, before.hmOut, before.hmDownS,
			after.ratio, after.dgOut, after.dgDownS, after.hmOut, after.hmDownS)
	}
}

// The regression itself, read on all three surfaces an operator sees an outage
// on, and compared with what the hourly cleanup makes of the same samples.
func TestDeleteNowKeepsTheLengthOfAnOutageOnlyTheSamplesEnded(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	since := ago(now, 4*3600)

	before := readAll(t, st, since)
	if before.dgOut != 1 || before.dgDownS != 600 || before.hmDownS != 600 {
		t.Fatalf("fixture: the digest reads %d outage(s) / %ds and the heatmap %ds, want 1 / 600s / 600s",
			before.dgOut, before.dgDownS, before.hmDownS)
	}

	samples := onDisk(t, st, "samples")
	n, err := st.Clear(ctx, "latency")
	if err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if n != samples {
		t.Errorf("Delete now reported %d rows, want the %d samples: closing an outage deletes nothing", n, samples)
	}
	// The monitor goes on probing: its next round lands a few seconds later.
	mustInsert(t, st, round(now, true))

	after := readAll(t, st, since)
	sameReadings(t, "after Delete now and one more round,", before, after)
	if after.dgDownS != 600 {
		t.Errorf("the ten-minute outage now reads as %ds long: with its samples gone the readers "+
			"bounded it at the first round after the delete", after.dgDownS)
	}

	// And the outage's rows are exactly the ones a cleanup of the same samples
	// leaves: its 'down', and one 'up' at the second the samples proved.
	pruned := orphanedByARestart(t, now)
	if _, err := pruned.Prune(ctx, ago(now, 3600), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got, want := dump(t, st, eventsAsStored), dump(t, pruned, eventsAsStored)
	if !slices.Equal(got, want) || len(got) != 2 {
		t.Errorf("after Delete now the outage's rows are\n  %q\nand after a cleanup of the same samples\n  %q\n"+
			"want the same two: the 'down' and one 'up' where the samples proved the link came back", got, want)
	}
}

// The hazard of doing that with no distance from the present. This handle is
// the running monitor's: it confirmed an outage ten minutes ago, and five
// minutes ago one round came back good, a quorum by the samples' rule, before
// the bad rounds resumed. The monitor has not called the link up - it wants
// Up after good rounds in a row - so the outage is still its own. A synthetic
// 'up' at that good round would give the outage two closing events once the
// monitor writes its own, and the readers would count the second as an outage
// of its own.
func TestDeleteNowLeavesAnOutageTheMonitorStillHoldsOpen(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := openFileStore(t)
	mustInsert(t, st, round(ago(now, 3600), true)) // monitoring anchor
	if err := st.InsertEvent(ctx, ago(now, 600), "down", -1, ""); err != nil {
		t.Fatalf("the monitor's down: %v", err)
	}
	mustInsert(t, st, round(ago(now, 600), false))
	mustInsert(t, st, round(ago(now, 300), true))
	mustInsert(t, st, round(ago(now, 295), false))
	since := ago(now, 3600)

	// Meanwhile the readers bound the open outage at that good round, and the
	// recovery memo remembers it.
	if o, err := st.UptimeSince(ctx, since, 0); err != nil || o.Down != 300*time.Second {
		t.Fatalf("before the delete the open outage reads %v down (err %v), want 300s", o.Down, err)
	}

	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if got := dump(t, st, eventsAsStored); len(got) != 1 {
		t.Fatalf("after Delete now the events are %q, want the monitor's 'down' alone: the delete closed "+
			"an outage the running monitor still holds open", got)
	}
	// With the good round gone the outage reads as still going, which is what the
	// monitor says too. A read that still stopped at the deleted round would be a
	// recovery memo the delete failed to drop.
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Down < 600*time.Second {
		t.Errorf("after Delete now the open outage reads %v down, want at least the 600s since its 'down'", o.Down)
	}

	// The monitor sees the link back and writes its own recovery: one outage.
	if err := st.InsertEvent(ctx, time.Now(), "up", int(time.Since(ago(now, 600)).Seconds()), ""); err != nil {
		t.Fatalf("the monitor's up: %v", err)
	}
	ups := closingEvents(t, st)
	if outages, _, err := st.ResolvedOutagesSince(ctx, since.Unix()); err != nil || outages != 1 || len(ups) != 1 {
		t.Errorf("once the monitor closed its outage the history holds the closing events %q and reads %d "+
			"outage(s) (err %v), want one of each", ups, outages, err)
	}
}

// The hourly cleanup has the same hazard once the latency window is short. It
// used to keep its distance from the monitor's outages by the window alone,
// thirty days by default, and the window is the operator's: the Data tab takes
// a fraction of a day, and the API and -retain take any length. Here it is
// under five minutes, on the history above, so the good round five minutes ago
// is past the cutoff and the cleanup's close reaches it. It wrote an 'up'
// there, deleted the round with the rest, and the monitor's own 'up' then made
// two ends of one outage, for good. The cleanup now leaves every outage of the
// running monitor to the monitor, as Delete now does.
func TestACleanupOnAShortWindowLeavesAnOutageTheMonitorStillHoldsOpen(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := openFileStore(t)
	mustInsert(t, st, round(ago(now, 3600), true)) // monitoring anchor
	if err := st.InsertEvent(ctx, ago(now, 600), "down", -1, ""); err != nil {
		t.Fatalf("the monitor's down: %v", err)
	}
	mustInsert(t, st, round(ago(now, 600), false))
	mustInsert(t, st, round(ago(now, 300), true))
	mustInsert(t, st, round(ago(now, 295), false))
	since := ago(now, 3600)

	far := now.Add(-9999 * time.Hour)
	if _, err := st.Prune(ctx, ago(now, 259), far, far); err != nil {
		t.Fatalf("the cleanup: %v", err)
	}
	if got := onDisk(t, st, "samples"); got != 0 {
		t.Fatalf("fixture: the cleanup left %d samples, want none: every round is older than its window", got)
	}
	if got := dump(t, st, eventsAsStored); len(got) != 1 {
		t.Fatalf("after the cleanup the events are %q, want the monitor's 'down' alone: the cleanup closed "+
			"an outage the running monitor still holds open", got)
	}

	// The monitor sees the link back and writes its own recovery: one outage.
	if err := st.InsertEvent(ctx, time.Now(), "up", int(time.Since(ago(now, 600)).Seconds()), ""); err != nil {
		t.Fatalf("the monitor's up: %v", err)
	}
	ups := closingEvents(t, st)
	if outages, _, err := st.ResolvedOutagesSince(ctx, since.Unix()); err != nil || outages != 1 || len(ups) != 1 {
		t.Errorf("once the monitor closed its outage the history holds the closing events %q and reads %d "+
			"outage(s) (err %v), want one of each", ups, outages, err)
	}
}

// ranOnAfterARestart is an orphan whose outage went on into this process: the
// first process confirmed it an hour ago and stopped, and every round since has
// been bad, up to five minutes ago. No good round separates it from an outage
// this process's monitor confirms now.
func ranOnAfterARestart(t *testing.T, now time.Time) *Store {
	t.Helper()
	st := restartedStore(t, func(first *Store) {
		mustInsert(t, first, round(ago(now, 4*3600), true)) // monitoring anchor
		if err := first.InsertEvent(context.Background(), ago(now, 3600), "down", -1, ""); err != nil {
			t.Fatalf("insert down: %v", err)
		}
		mustInsert(t, first, round(ago(now, 3600), false))
	})
	for _, s := range []int{3000, 2000, 1000, 300} {
		mustInsert(t, st, round(ago(now, s), false))
	}
	return st
}

// The monitor's 'down' can fail to land and wait in its retry buffer, and the
// line moves all the same: InsertEvent moves it before the write. The orphan's
// search must then stop at the monitor's 'down' though no such row is on disk,
// because the good round after it is the monitor's recovery in progress, not
// the orphan's.
func TestDeleteNowStopsAtTheMonitorsDownWhileItWaitsToBeWritten(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := ranOnAfterARestart(t, now)
	if _, err := st.db.Exec(`CREATE TRIGGER refuse_outages BEFORE INSERT ON events WHEN NEW.type = 'down'
		BEGIN SELECT RAISE(ABORT, 'the outage is refused'); END`); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertEvent(ctx, ago(now, 300), "down", -1, ""); err == nil {
		t.Fatal("fixture: the monitor's down was written; want it waiting in the retry buffer")
	}
	if _, err := st.db.Exec(`DROP TRIGGER refuse_outages`); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, st, round(ago(now, 250), true))

	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if got := closingEvents(t, st); len(got) != 0 {
		t.Errorf("Delete now wrote the closing events %q: it closed the orphan at a good round that came after "+
			"the monitor's own 'down', so the monitor's outage would read as closed there too", got)
	}
}

// The line is read again after each search, because the monitor can confirm an
// outage while one runs. Here its 'down' lands during the search: the line
// moves from nowhere to five minutes ago between the two reads, and the pairing
// query that ran first never saw the row. The good round after it is the
// monitor's recovery in progress. A close that trusted its first read would end
// the orphan there, and with it the monitor's outage.
func TestTheCloseRereadsTheMonitorsLineAfterEachSearch(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := ranOnAfterARestart(t, now)
	mustInsert(t, st, round(ago(now, 250), true))
	reads := 0
	line := func() int64 {
		reads++
		if reads == 1 {
			return math.MaxInt64 // no 'down' from this monitor yet
		}
		return ago(now, 300).Unix() // and now there is one
	}
	if err := st.resolveDanglingDowns(ctx, now.Unix()+1, now.Unix(), line); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := closingEvents(t, st); len(got) != 0 {
		t.Errorf("the close wrote the closing events %q: that good round came after the 'down' the monitor "+
			"confirmed during the search, and is the monitor's to judge", got)
	}
}

// The monitor's own recovery can be dated ahead of the clock: after a backward
// clock step the widen in Monitor.transition stamps it where the outage's full
// width needs it. That row is the monitor's measurement. A cleanup would move an
// orphan's future-dated 'up' back to the second the samples prove, but this one
// belongs to an outage the monitor opened, and stays as it was written.
func TestDeleteNowKeepsTheMonitorsOwnRecoveryAheadOfTheClock(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("with its outage's down on disk", func(t *testing.T) {
		st := openFileStore(t)
		mustInsert(t, st, round(ago(now, 3600), true))
		if err := st.InsertEvent(ctx, ago(now, 600), "down", -1, ""); err != nil {
			t.Fatalf("the monitor's down: %v", err)
		}
		mustInsert(t, st, round(ago(now, 500), true))
		if err := st.InsertEvent(ctx, now.Add(5*time.Minute), "up", 900, ""); err != nil {
			t.Fatalf("the monitor's widened up: %v", err)
		}
		want := dump(t, st, eventsAsStored)

		if _, err := st.Clear(ctx, "latency"); err != nil {
			t.Fatalf("Delete now: %v", err)
		}
		if got := dump(t, st, eventsAsStored); !slices.Equal(got, want) {
			t.Errorf("Delete now rewrote the monitor's own outage:\n  was %q\n  now %q", want, got)
		}
	})

	// The same 'up', with the monitor's 'down' still in its retry buffer and an
	// orphan before it. Nothing on disk lies between the orphan's 'down' and the
	// monitor's 'up', so the two pair as an outage closed ahead of the clock.
	// The line cuts the orphan's search short at the monitor's 'down', and the
	// 'up' past the line is the monitor's as well: moved back to the orphan's
	// recovery, it closed the orphan, and once the monitor's 'down' landed its
	// outage had no end at all.
	t.Run("with its outage's down waiting to be written", func(t *testing.T) {
		st := orphanedByARestart(t, now) // the orphan's 'down' three hours back, good rounds from ten minutes later
		if _, err := st.db.Exec(`CREATE TRIGGER refuse_outages BEFORE INSERT ON events WHEN NEW.type = 'down'
			BEGIN SELECT RAISE(ABORT, 'the outage is refused'); END`); err != nil {
			t.Fatal(err)
		}
		if err := st.InsertEvent(ctx, ago(now, 300), "down", -1, ""); err == nil {
			t.Fatal("fixture: the monitor's down was written; want it waiting in the retry buffer")
		}
		if _, err := st.db.Exec(`DROP TRIGGER refuse_outages`); err != nil {
			t.Fatal(err)
		}
		monitorsUp := now.Add(5 * time.Minute)
		if err := st.InsertEvent(ctx, monitorsUp, "up", 400, ""); err != nil {
			t.Fatalf("the monitor's widened up: %v", err)
		}

		if _, err := st.Clear(ctx, "latency"); err != nil {
			t.Fatalf("Delete now: %v", err)
		}
		// The monitor's retry lands its 'down'.
		if err := st.InsertEvent(ctx, ago(now, 300), "down", -1, ""); err != nil {
			t.Fatalf("the monitor's down, written again: %v", err)
		}
		got, want := dump(t, st, eventsAsStored), []string{
			fmt.Sprint(ago(now, 3*3600).Unix(), "down", nil, ""),
			fmt.Sprint(ago(now, 3*3600-600).Unix(), "up", int64(600), "recovered while unmonitored"),
			fmt.Sprint(ago(now, 300).Unix(), "down", nil, ""),
			fmt.Sprint(monitorsUp.Unix(), "up", int64(400), ""),
		}
		if !slices.Equal(got, want) {
			t.Errorf("after Delete now and the monitor's retry the events are\n  %q\nwant the orphan closed at its own "+
				"recovery and the monitor's outage with its own 'up' where the monitor wrote it:\n  %q", got, want)
		}
	})
}

// One outage, one closing event, whatever else its history holds. An orphan's
// 'up' can sit ahead of the clock (a process whose clock ran fast, or an import
// from one). Close to the present no reader counts it yet, further out the next
// cleanup deletes it, and either way it is not credible as written. The delete
// moves it back to the second the samples prove, as a cleanup does, rather than
// writing a second closing event beside it.
func TestDeleteNowLeavesEachOutageOneClosingEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	down, rec := ago(now, 3*3600), ago(now, 3*3600-600)
	for _, tc := range []struct {
		name string
		upIn time.Duration // how far ahead of the clock the outage's stray 'up' sits; 0 = there is none
	}{
		{"no up ahead of the clock", 0},
		// Past currentHorizon, so no reader counts it, and inside pruneFutureSlack,
		// so a cleanup keeps it: it pairs with the 'down' on disk.
		{"an up past what the readers count but inside what a cleanup keeps", 10 * time.Minute},
		// Past pruneFutureSlack: the next cleanup deletes it.
		{"an up past what a cleanup keeps", 10 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := restartedStore(t, func(first *Store) {
				mustInsert(t, first, round(ago(now, 4*3600), true))
				if err := first.InsertEvent(ctx, down, "down", -1, ""); err != nil {
					t.Fatalf("insert down: %v", err)
				}
				mustInsert(t, first, round(down, false))
				if tc.upIn > 0 {
					if err := first.InsertEvent(ctx, now.Add(tc.upIn), "up", 3*3600, ""); err != nil {
						t.Fatalf("insert the stray up: %v", err)
					}
				}
			})
			mustInsert(t, st, round(rec, true))

			if _, err := st.Clear(ctx, "latency"); err != nil {
				t.Fatalf("Delete now: %v", err)
			}
			got, want := closingEvents(t, st), []string{fmt.Sprintf("%d %d", rec.Unix(), 600)}
			if !slices.Equal(got, want) {
				t.Errorf("after Delete now the outage's closing events (ts duration_s) are %q, want exactly %q: "+
					"one 'up', at the second the samples proved, ten minutes long", got, want)
			}
		})
	}
}

// Deleting the samples with the recovery unwritten is the damage the close
// exists to prevent, so a close that fails deletes nothing, as a save that fails
// does. Pressed again once the write goes through, the button does the whole job.
func TestDeleteNowThatCannotCloseAnOutageDeletesNothing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	samples := onDisk(t, st, "samples")
	if _, err := st.db.Exec(`CREATE TRIGGER refuse_recoveries BEFORE INSERT ON events WHEN NEW.type = 'up'
		BEGIN SELECT RAISE(ABORT, 'the recovery is refused'); END`); err != nil {
		t.Fatal(err)
	}

	n, err := st.Clear(ctx, "latency")
	if err == nil || n != 0 {
		t.Fatalf("Delete now with the close failing: %d rows, err %v; want 0 and the close's error", n, err)
	}
	if got := onDisk(t, st, "samples"); got != samples {
		t.Errorf("the refused delete left %d samples, want all %d: they are the outage's only record of its end",
			got, samples)
	}
	if got := dump(t, st, eventsAsStored); len(got) != 1 {
		t.Errorf("the refused delete left the events %q, want the 'down' alone", got)
	}

	if _, err := st.db.Exec(`DROP TRIGGER refuse_recoveries`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Clear(ctx, "latency"); err != nil || n != samples {
		t.Fatalf("Delete now once the write goes through: %d rows, err %v; want %d and nil", n, err, samples)
	}
	if got := dump(t, st, eventsAsStored); len(got) != 2 {
		t.Errorf("after Delete now the events are %q, want the 'down' and its 'up'", got)
	}
}

// The close and the delete are two steps, and a crash, or a delete that fails,
// can fall between them. What that leaves is the closing event and every sample.
// The event sits at the second the samples prove, with the observed length, so
// the history reads as it did from the samples, and the next Delete now finds
// the outage closed and writes no second event.
func TestDeleteNowStoppedAfterClosingAnOutageLeavesItsLengthAlone(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	since := ago(now, 4*3600)
	before := readAll(t, st, since)
	samples := onDisk(t, st, "samples")
	if _, err := st.db.Exec(`CREATE TRIGGER keep_samples BEFORE DELETE ON samples
		BEGIN SELECT RAISE(ABORT, 'the delete is refused'); END`); err != nil {
		t.Fatal(err)
	}

	n, err := st.Clear(ctx, "latency")
	if err == nil || n != 0 {
		t.Fatalf("Delete now with the delete failing: %d rows, err %v; want 0 and the delete's error", n, err)
	}
	if got := onDisk(t, st, "samples"); got != samples {
		t.Errorf("the failed delete left %d samples, want all %d: a delete is all or nothing", got, samples)
	}
	closed := dump(t, st, eventsAsStored)
	if len(closed) != 2 {
		t.Fatalf("after the close the events are %q, want the 'down' and its 'up'", closed)
	}
	sameReadings(t, "with the outage closed and its samples still there,", before, readAll(t, st, since))

	if _, err := st.db.Exec(`DROP TRIGGER keep_samples`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.Clear(ctx, "latency"); err != nil || n != samples {
		t.Fatalf("Delete now pressed again: %d rows, err %v; want %d and nil", n, err, samples)
	}
	if got := dump(t, st, eventsAsStored); !slices.Equal(got, closed) {
		t.Errorf("pressed again, Delete now changed the closed outage's rows:\n  was %q\n  now %q", closed, got)
	}
	sameReadings(t, "after Delete now pressed again,", before, readAll(t, st, since))
}

// Only the latency samples prove when an outage ended, so only their delete
// closes outages first. The speed history's delete leaves the outage history as
// it found it, and the downtime delete takes the outage history itself; neither
// touches the samples.
func TestOnlyDeletingLatencyClosesOutagesFirst(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	samples := onDisk(t, st, "samples")
	want := dump(t, st, eventsAsStored)

	if _, err := st.Clear(ctx, "speed"); err != nil {
		t.Fatalf("delete the speed history: %v", err)
	}
	if got := dump(t, st, eventsAsStored); !slices.Equal(got, want) {
		t.Errorf("deleting the speed history changed the events:\n  was %q\n  now %q", want, got)
	}
	if got := onDisk(t, st, "samples"); got != samples {
		t.Errorf("deleting the speed history left %d samples, want %d", got, samples)
	}

	if _, err := st.Clear(ctx, "downtime"); err != nil {
		t.Fatalf("delete the downtime history: %v", err)
	}
	if got := dump(t, st, eventsAsStored); len(got) != 0 {
		t.Errorf("deleting the downtime history left the events %q, want none", got)
	}
	if got := onDisk(t, st, "samples"); got != samples {
		t.Errorf("deleting the downtime history left %d samples, want %d", got, samples)
	}
}

// A close that fails is one failure on /metrics (db.err), whichever statement
// failed and whoever ran the close. The close counts a failed write itself, so
// Delete now and the cleanup do not count it again. A failed read is left to
// the caller, so they count that one. The cleanup used to count every failure
// of its close, and a refused 'up' read as two.
func TestACloseThatFailsIsCountedOnce(t *testing.T) {
	ctx := context.Background()
	for _, by := range []struct {
		name string
		run  func(st *Store, now time.Time) error
	}{
		{"Delete now", func(st *Store, _ time.Time) error { _, err := st.Clear(ctx, "latency"); return err }},
		// A window of an hour: the round that shows the orphan's link back is
		// older, so the cleanup closes the outage before it deletes that round.
		{"a cleanup", func(st *Store, now time.Time) error {
			far := now.Add(-9999 * time.Hour)
			_, err := st.Prune(ctx, ago(now, 3600), far, far)
			return err
		}},
	} {
		for _, tc := range []struct{ name, fail, heal string }{
			{"a write", `CREATE TRIGGER refuse_recoveries BEFORE INSERT ON events WHEN NEW.type = 'up'
				BEGIN SELECT RAISE(ABORT, 'the recovery is refused'); END`, `DROP TRIGGER refuse_recoveries`},
			// The pause spans are read for the outage's observed length.
			{"a read", `ALTER TABLE pauses RENAME TO pauses_gone`, `ALTER TABLE pauses_gone RENAME TO pauses`},
		} {
			t.Run(by.name+"/"+tc.name, func(t *testing.T) {
				now := time.Now()
				st := orphanedByARestart(t, now)
				samples := onDisk(t, st, "samples")
				if _, err := st.db.Exec(tc.fail); err != nil {
					t.Fatal(err)
				}
				stats.ResetForTest()
				if err := by.run(st, now); err == nil {
					t.Fatal("it went through with the close failing")
				}
				if got := counter("db.err"); got != 1 {
					t.Errorf("one failed close counts %d on db.err, want 1", got)
				}
				if got := onDisk(t, st, "samples"); got != samples {
					t.Errorf("with the close failing %d samples are left, want all %d: they are the outage's "+
						"only record of its end", got, samples)
				}
				if _, err := st.db.Exec(tc.heal); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// ONE CHANGE TO THE OUTAGE RECORD AT A TIME.
//
// The close reads which outages have no end and searches the samples for each,
// and only then writes the ends it found. Before it took outageMu nothing kept
// another change out of that gap, and the daemon has several that can run at
// the same moment: a second Delete now from another tab, the hourly cleanup,
// the Downtime tab's Delete now, the delete of one outage from the list, and a
// restore. Each of these holds the close at that point (closeHook), runs the
// other change there, and wants what the two leave when they run one after the
// other.

// closeHold is how long a held close waits for the change beside it. With
// nothing keeping that change out it finishes in under a millisecond, and in
// three at most under the race detector. When outageMu keeps it out it cannot
// finish until the close has, and the close goes on after closeHold. So a slow
// machine can only make one of these tests pass without the lock. It can never
// make one fail with it.
const closeHold = 250 * time.Millisecond

// pauseTheClose holds the first close (resolveDanglingDowns, whoever runs it)
// that gets as far as its writes, before the first one. reached is closed once
// a close is there. It goes on when goOn is called, or after closeHold. Every
// later close goes straight on.
func pauseTheClose(t *testing.T) (reached <-chan struct{}, goOn func()) {
	t.Helper()
	there, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	closeHook = func() {
		if !first.CompareAndSwap(false, true) {
			return
		}
		close(there)
		select {
		case <-release:
		case <-time.After(closeHold):
		}
	}
	t.Cleanup(func() { closeHook = nil })
	var once sync.Once
	return there, func() { once.Do(func() { close(release) }) }
}

// besideTheClose runs change on a goroutine of its own once the next close is
// held (pauseTheClose), and lets the close go on when change has finished. The
// function it returns waits for change and hands back its error.
func besideTheClose(t *testing.T, change func() error) (wait func() error) {
	t.Helper()
	reached, goOn := pauseTheClose(t)
	done, stop, gone := make(chan error, 1), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(gone)
		select {
		case <-reached:
		case <-stop:
			return // the test ended before a close got that far
		}
		err := change()
		goOn()
		done <- err
	}()
	// Registered after pauseTheClose's, so it runs before that one takes the
	// hook away: no goroutine of the test is left running into it.
	t.Cleanup(func() {
		close(stop)
		<-gone
	})
	return func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("the change beside the close never finished")
			return nil
		}
	}
}

// orphanBeforeAnOutageOfItsOwn is the history orphanedByARestart builds, with
// an outage this process's monitor recorded whole after the orphan: a 'down' two
// hours back and its 'up' ten minutes later. The orphan's search then ends at
// that 'down', and the close writes the orphan's end without looking first for
// an 'up' of its own ahead of the clock. That look is only made for the last
// outage on record, and one made after another close had written the end would
// find that end and move it onto itself, which would hide the overlap the tests
// below are about.
func orphanBeforeAnOutageOfItsOwn(t *testing.T, now time.Time) *Store {
	t.Helper()
	ctx := context.Background()
	st := restartedStore(t, func(first *Store) {
		mustInsert(t, first, round(ago(now, 4*3600), true)) // monitoring anchor
		if err := first.InsertEvent(ctx, ago(now, 3*3600), "down", -1, ""); err != nil {
			t.Fatalf("insert the orphan's down: %v", err)
		}
		mustInsert(t, first, round(ago(now, 3*3600), false))
	})
	for s := 3*3600 - 600; s > 2*3600; s -= 600 {
		mustInsert(t, st, round(ago(now, s), true))
	}
	if err := st.InsertEvent(ctx, ago(now, 2*3600), "down", -1, ""); err != nil {
		t.Fatalf("insert the monitor's down: %v", err)
	}
	mustInsert(t, st, round(ago(now, 2*3600), false))
	for s := 2*3600 - 600; s >= 600; s -= 600 {
		mustInsert(t, st, round(ago(now, s), true))
	}
	if err := st.InsertEvent(ctx, ago(now, 2*3600-600), "up", 600, ""); err != nil {
		t.Fatalf("insert the monitor's up: %v", err)
	}
	return st
}

// oneEndEach fails the test unless the closing events are the orphan's one end,
// where its samples proved it, and the monitor's own.
func oneEndEach(t *testing.T, when string, st *Store, now time.Time) {
	t.Helper()
	want := []string{fmt.Sprintf("%d 600", ago(now, 3*3600-600).Unix()), fmt.Sprintf("%d 600", ago(now, 2*3600-600).Unix())}
	if got := closingEvents(t, st); !slices.Equal(got, want) {
		t.Errorf("%s the closing events (ts duration_s) are %q, want the orphan's one and the monitor's: %q", when, got, want)
	}
}

// Two presses of Delete now at once, from two tabs or a retry. Each close read
// the orphan as having no end, and each wrote one: the readers counted the
// second as an outage of its own, and the digest read an outage and ten minutes
// more than there had been.
func TestTwoDeleteNowsAtOnceCloseAnOutageOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanBeforeAnOutageOfItsOwn(t, now)
	since := ago(now, 4*3600)
	before := readAll(t, st, since)

	second := besideTheClose(t, func() error { _, err := st.Clear(ctx, "latency"); return err })
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("the first Delete now: %v", err)
	}
	if err := second(); err != nil {
		t.Fatalf("the second Delete now: %v", err)
	}
	mustInsert(t, st, round(now, true))

	oneEndEach(t, "after two Delete nows at once", st, now)
	sameReadings(t, "after two Delete nows at once and one more round,", before, readAll(t, st, since))
}

// Delete now pressed while the hourly cleanup closes the same outage: the
// first cleanup after a start, or after the window was lowered, reaches a
// recovery this recent. Both closes wrote an end.
func TestDeleteNowBesideACleanupClosesAnOutageOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanBeforeAnOutageOfItsOwn(t, now)
	since := ago(now, 4*3600)
	before := readAll(t, st, since)

	deleteNow := besideTheClose(t, func() error { _, err := st.Clear(ctx, "latency"); return err })
	far := now.Add(-9999 * time.Hour)
	if _, err := st.Prune(ctx, ago(now, 3600), far, far); err != nil {
		t.Fatalf("the cleanup: %v", err)
	}
	if err := deleteNow(); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	mustInsert(t, st, round(now, true))

	oneEndEach(t, "after a cleanup and a Delete now at once", st, now)
	sameReadings(t, "after a cleanup and a Delete now at once and one more round,", before, readAll(t, st, since))
}

// The two Delete now buttons at once, Latency's and Downtime's. The downtime
// delete took the outage while the close was between its read and its write,
// and the close then wrote the end of an outage that was gone. That lone end
// read as an outage in the digest and the uptime figure after the operator had
// deleted every one.
func TestDeleteNowBesideTheDowntimeDeleteLeavesNoEndBehind(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	since := ago(now, 4*3600)
	// One after the other, in either order, the two leave no outage at all.
	apart := orphanedByARestart(t, now)
	for _, kind := range []string{"latency", "downtime"} {
		if _, err := apart.Clear(ctx, kind); err != nil {
			t.Fatalf("delete %s: %v", kind, err)
		}
	}
	mustInsert(t, apart, round(now, true))
	want := readAll(t, apart, since)

	st := orphanedByARestart(t, now)
	downtime := besideTheClose(t, func() error { _, err := st.Clear(ctx, "downtime"); return err })
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("delete latency: %v", err)
	}
	if err := downtime(); err != nil {
		t.Fatalf("delete downtime: %v", err)
	}
	mustInsert(t, st, round(now, true))

	if got := dump(t, st, eventsAsStored); len(got) != 0 {
		t.Errorf("the outage history was deleted, yet the events hold %q: the close wrote the end of an outage "+
			"the downtime delete had taken", got)
	}
	sameReadings(t, "after the two deletes at once and one more round,", want, readAll(t, st, since))
}

// The monitor's own writes never wait for a close: a probe round must not sit
// out a Delete now. Its round and its 'down' go in while a close is held
// between its read and its write, and the close then ends the orphan it read
// and leaves the monitor's outage to the monitor.
func TestTheMonitorsWritesDoNotWaitForAClose(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	held, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	closeHook = func() {
		if first.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
	}
	t.Cleanup(func() { closeHook = nil })
	cleared := make(chan error, 1)
	go func() {
		_, err := st.Clear(ctx, "latency")
		cleared <- err
	}()
	select {
	case <-held:
	case err := <-cleared:
		t.Fatalf("Delete now finished without getting as far as its writes (err %v)", err)
	}

	wrote := make(chan error, 1)
	go func() {
		if err := st.InsertSamples(ctx, round(now, false)); err != nil {
			wrote <- err
			return
		}
		wrote <- st.InsertEvent(ctx, now, "down", -1, "")
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Errorf("the monitor's writes beside a held close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the monitor's round and its 'down' waited for a close to finish")
	}
	close(release)
	if err := <-cleared; err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	got, want := dump(t, st, eventsAsStored), []string{
		fmt.Sprint(ago(now, 3*3600).Unix(), "down", nil, ""),
		fmt.Sprint(ago(now, 3*3600-600).Unix(), "up", int64(600), "recovered while unmonitored"),
		fmt.Sprint(now.Unix(), "down", nil, ""),
	}
	if !slices.Equal(got, want) {
		t.Errorf("after Delete now beside the monitor's 'down' the events are\n  %q\nwant the orphan ended where "+
			"its samples proved it, and the monitor's outage open:\n  %q", got, want)
	}
}

// Nor for a restore, which holds the outage record for as long as it takes to
// bring in its outage history, the time to send it included
// (HoldOutageRecord). The monitor's round and its 'down' go in while a
// restore holds it.
func TestTheMonitorsWritesDoNotWaitForARestoresOutageHistory(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := orphanedByARestart(t, now)
	hold := st.HoldOutageRecord()
	defer hold.Release()
	wrote := make(chan error, 1)
	go func() {
		if err := st.InsertSamples(ctx, round(now, false)); err != nil {
			wrote <- err
			return
		}
		wrote <- st.InsertEvent(ctx, now, "down", -1, "")
	}()
	select {
	case err := <-wrote:
		if err != nil {
			t.Errorf("the monitor's writes beside a restore's outage history: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the monitor's round and its 'down' waited for a restore to bring in its outage history")
	}
}

// Deleting an outage from the list while Delete now closes it. This outage's
// 'up' came from a host whose clock ran fast and sits ten days ahead, past what
// a cleanup keeps, so the close does not pair it with the 'down'. It finds it
// once it has searched, and moves it back to the second the samples prove. A
// delete of that 'up' and its 'down' in between left the end the close then
// wrote with no outage.
func TestDeletingAnOutageBesideDeleteNowLeavesNoEndBehind(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	down, rec, ahead := ago(now, 3*3600), ago(now, 3*3600-600), now.Add(10*24*time.Hour)
	history := func() *Store {
		st := restartedStore(t, func(first *Store) {
			mustInsert(t, first, round(ago(now, 4*3600), true))
			if err := first.InsertEvent(ctx, down, "down", -1, ""); err != nil {
				t.Fatalf("insert down: %v", err)
			}
			mustInsert(t, first, round(down, false))
			if err := first.InsertEvent(ctx, ahead, "up", 3*3600, ""); err != nil {
				t.Fatalf("insert the up ahead of the clock: %v", err)
			}
		})
		mustInsert(t, st, round(rec, true))
		return st
	}
	// One after the other: the close moves the 'up' back, and the delete then
	// finds nothing at the second it was asked for.
	apart := history()
	if _, err := apart.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if _, err := apart.DeleteOutage(ctx, ahead.Unix()); err != nil {
		t.Fatalf("delete the outage: %v", err)
	}
	want := dump(t, apart, eventsAsStored)

	st := history()
	deleted := besideTheClose(t, func() error { _, err := st.DeleteOutage(ctx, ahead.Unix()); return err })
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if err := deleted(); err != nil {
		t.Fatalf("delete the outage: %v", err)
	}
	if got := dump(t, st, eventsAsStored); !slices.Equal(got, want) {
		t.Errorf("after the two deletes at once the events are\n  %q\nand one after the other\n  %q", got, want)
	}
}

// A restore that brings back this very outage's end while Delete now closes it.
// A backup taken after a cleanup or a Delete now on a copy of this history holds
// that end at the second the samples prove, and a restore stores a row only
// where none with its key is stored. The restore stored it between the close's
// read and its write, and the close stored it again.
func TestARestoreBesideDeleteNowStoresEachEndOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	since := ago(now, 4*3600)
	copied := orphanBeforeAnOutageOfItsOwn(t, now)
	if _, err := copied.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now on the copy: %v", err)
	}
	backup, err := copied.ExportTable(ctx, "events")
	if err != nil || len(backup) != 4 {
		t.Fatalf("the backup holds %d outage rows (err %v), want both outages' 'down' and 'up'", len(backup), err)
	}

	st := orphanBeforeAnOutageOfItsOwn(t, now)
	before := readAll(t, st, since)
	restore := besideTheClose(t, func() error { _, err := st.ImportTable(ctx, "events", backup); return err })
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if err := restore(); err != nil {
		t.Fatalf("the restore: %v", err)
	}
	mustInsert(t, st, round(now, true))

	oneEndEach(t, "after a restore and a Delete now at once", st, now)
	sameReadings(t, "after a restore and a Delete now at once and one more round,", before, readAll(t, st, since))
}

// A restore brings a big outage history in batches, 5,000 rows each in the web
// layer, so an outage whose 'down' ends one batch has its 'up' in the next.
// The latency category comes first in a backup, and its samples already prove
// the recovery. With the lock taken batch by batch, a Delete now in between
// read the 'down' as having no end and wrote one at the first good second, and
// the next batch stored the backup's own end, which the monitor had dated at
// the last of its up-after good rounds, a different second: one outage, two
// ends. A restore now holds the outage record across all of its batches, and a
// Delete now pressed in the middle waits for the rest.
func TestARestoreInBatchesBesideDeleteNowStoresEachEndOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	since := ago(now, 4*3600)
	// What the latency category restored: a bad round as the outage began,
	// then a good one every ten minutes from ten minutes later.
	restored := func() *Store {
		st := openFileStore(t)
		mustInsert(t, st, round(ago(now, 4*3600), true))
		mustInsert(t, st, round(ago(now, 3*3600), false))
		for s := 3*3600 - 600; s >= 600; s -= 600 {
			mustInsert(t, st, round(ago(now, s), true))
		}
		return st
	}
	// The backup's outage, its 'up' the monitor's: up-after two good rounds.
	batches := [][]map[string]any{
		{{"ts": ago(now, 3*3600).Unix(), "type": "down", "detail": ""}},
		{{"ts": ago(now, 3*3600-1200).Unix(), "type": "up", "duration_s": int64(1200), "detail": ""}},
	}

	// One after the other: the restore, then Delete now, which finds the
	// outage ended and leaves it be.
	apart := restored()
	hold, perTS := apart.HoldOutageRecord(), map[int64]int{}
	for _, b := range batches {
		if _, err := hold.ImportTableBatch(ctx, "events", b, perTS); err != nil {
			t.Fatalf("restore a batch: %v", err)
		}
	}
	hold.Release()
	if _, err := apart.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	want, wantRead := dump(t, apart, eventsAsStored), readAll(t, apart, since)

	// Delete now pressed after the first batch. With nothing to keep it out it
	// is done within milliseconds; kept out, it cannot finish until the
	// restore lets go, and the restore goes on after closeHold.
	st := restored()
	hold, perTS = st.HoldOutageRecord(), map[int64]int{}
	if _, err := hold.ImportTableBatch(ctx, "events", batches[0], perTS); err != nil {
		t.Fatalf("restore the first batch: %v", err)
	}
	cleared := make(chan error, 1)
	go func() {
		_, err := st.Clear(ctx, "latency")
		cleared <- err
	}()
	var err error
	waited := false
	select {
	case err = <-cleared:
		waited = true
	case <-time.After(closeHold):
	}
	if _, ierr := hold.ImportTableBatch(ctx, "events", batches[1], perTS); ierr != nil {
		t.Fatalf("restore the second batch: %v", ierr)
	}
	hold.Release()
	if !waited {
		select {
		case err = <-cleared:
		case <-time.After(30 * time.Second):
			t.Fatal("Delete now never finished once the restore had let go of the outage record")
		}
	}
	if err != nil {
		t.Fatalf("Delete now: %v", err)
	}
	if got := dump(t, st, eventsAsStored); !slices.Equal(got, want) {
		t.Errorf("after a Delete now between two batches of a restore the events are\n  %q\nand one after the other\n  %q",
			got, want)
	}
	sameReadings(t, "after a Delete now between two batches of a restore,", wantRead, readAll(t, st, since))
}

// The cleanup's sweep of old outages while Delete now closes one of them. Here
// the outage history is kept for less time than the latency samples, so the
// cleanup's own close leaves the outage be (the samples that prove its end stay)
// and its sweep takes it whole, with the complete outage after it. The sweep
// ran between the Delete now's read and its write, and the end the close wrote
// then had no outage.
func TestACleanupSweepBesideDeleteNowLeavesNoEndBehind(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := restartedStore(t, func(first *Store) {
		mustInsert(t, first, round(ago(now, 5*3600), true))
		if err := first.InsertEvent(ctx, ago(now, 3*3600), "down", -1, ""); err != nil {
			t.Fatalf("insert the orphan's down: %v", err)
		}
		mustInsert(t, first, round(ago(now, 3*3600), false))
		for s := 3*3600 - 600; s > 2*3600; s -= 600 {
			mustInsert(t, first, round(ago(now, s), true))
		}
		if err := first.InsertEvent(ctx, ago(now, 2*3600), "down", -1, ""); err != nil {
			t.Fatalf("insert the next outage's down: %v", err)
		}
		mustInsert(t, first, round(ago(now, 2*3600), false))
		if err := first.InsertEvent(ctx, ago(now, 2*3600-600), "up", 600, ""); err != nil {
			t.Fatalf("insert the next outage's up: %v", err)
		}
	})
	pruneChunks(t, pruneChunkRows, pruneChunkPause) // puts the hook back afterwards

	// The first chunk comes after the cleanup's own close. Delete now is
	// pressed then, and its close gets as far as its writes. It goes on once
	// the sweep has run, or after closeHold when the sweep waits for it.
	var cleared chan error
	var goOn func()
	pruneChunkHook = func(q string, _ int64, _ time.Duration, _ bool) {
		if cleared == nil {
			var reached <-chan struct{}
			reached, goOn = pauseTheClose(t)
			cleared = make(chan error, 1)
			go func() {
				_, err := st.Clear(ctx, "latency")
				cleared <- err
			}()
			select {
			case <-reached:
			case <-time.After(30 * time.Second):
				t.Fatal("the Delete now's close never got as far as its writes")
			}
			return
		}
		if strings.Contains(q, "DELETE FROM events") {
			goOn()
		}
	}
	far := now.Add(-9999 * time.Hour)
	_, err := st.Prune(ctx, ago(now, 4*3600), far, ago(now, 3600))
	if cleared == nil {
		t.Fatalf("the cleanup ran no chunk (err %v), so no Delete now ran beside it", err)
	}
	if cerr := <-cleared; err != nil || cerr != nil {
		t.Fatalf("the cleanup: %v; Delete now: %v", err, cerr)
	}
	// One after the other, in either order, the sweep takes both outages
	// whole, the orphan's end with it when the close wrote it first.
	if got := dump(t, st, eventsAsStored); len(got) != 0 {
		t.Errorf("the cleanup took every outage older than its cutoff, yet the events hold %q: the close wrote "+
			"the end of an outage the sweep had taken", got)
	}
}

// The downtime category's tables go in only under the hold on the outage
// record. Store.ImportTableBatch used to take the lock itself, for the outages
// alone and one call at a time, so a caller that streamed an outage history
// through it in batches left every gap between two batches open to a close,
// and had back the second end the hold exists to prevent. It now refuses all
// three tables, names the hold, and stores nothing. ImportTable brings a
// table's rows in one call and takes a hold of its own: the record is held
// between two of its transactions, where a close could otherwise get in, and
// free again once it returns.
func TestTheOutageHistoryGoesInOnlyUnderTheHold(t *testing.T) {
	ctx := context.Background()
	base := time.Now().Add(-30 * 24 * time.Hour).Unix()
	future := int64(4070908800) // 2099-01-01: a held pause reaches past the clock
	rows := func(table string, n int) []map[string]any {
		out := make([]map[string]any, 0, n)
		for i := int64(0); i < int64(n); i++ {
			switch table {
			case "events":
				row := map[string]any{"ts": base + 60*i, "type": "down", "detail": ""}
				if i%2 == 1 {
					row["type"], row["duration_s"] = "up", int64(30)
				}
				out = append(out, row)
			case "pauses":
				out = append(out, map[string]any{"ts": base + 120*i, "duration_s": int64(60)})
			default:
				out = append(out, map[string]any{"ts": future + 120*i, "duration_s": int64(60)})
			}
		}
		return out
	}
	for _, table := range []string{"events", "pauses", "pauses_quarantine"} {
		t.Run(table, func(t *testing.T) {
			st := open(t)
			n, err := st.ImportTableBatch(ctx, table, rows(table, 3), map[int64]int{})
			if err == nil || !strings.Contains(err.Error(), "HoldOutageRecord") || n != 0 {
				t.Errorf("Store.ImportTableBatch on %s = %d, %v; want 0 and an error that names HoldOutageRecord", table, n, err)
			}
			if got := countRows(t, st, `SELECT COUNT(*) FROM `+table); got != 0 {
				t.Errorf("the refused call stored %d rows in %s", got, table)
			}

			one := open(t)
			var held, free int
			prev := importChunkHook
			importChunkHook = func() {
				if one.outageMu.TryLock() {
					one.outageMu.Unlock()
					free++
					return
				}
				held++
			}
			t.Cleanup(func() { importChunkHook = prev })
			want := importTxRows + 1 // two transactions, the hook between them
			if n, err := one.ImportTable(ctx, table, rows(table, want)); err != nil || n != want {
				t.Fatalf("ImportTable on %s = %d, %v; want %d, nil", table, n, err, want)
			}
			if held != 1 || free != 0 {
				t.Errorf("between ImportTable's two transactions on %s the outage record was held %d times and free %d; "+
					"want held once: a close could get in between them", table, held, free)
			}
			if !one.outageMu.TryLock() {
				t.Fatalf("the outage record is still held after ImportTable on %s returned", table)
			}
			one.outageMu.Unlock()
		})
	}
	// The other tables go in through the store's own ImportTableBatch as before.
	st := open(t)
	if n, err := st.ImportTableBatch(ctx, "samples", []map[string]any{{"ts": base, "target": "a", "latency_ms": 12.5,
		"success": int64(1), "family": "ipv4"}}, map[int64]int{}); err != nil || n != 1 {
		t.Errorf("Store.ImportTableBatch on samples = %d, %v; want 1, nil", n, err)
	}
}
