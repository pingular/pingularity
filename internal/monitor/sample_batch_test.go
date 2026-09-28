package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// THE MONITOR DOES NOT KNOW THAT ITS ROUNDS WAIT IN MEMORY.
//
// Nothing in this package changed when the store began to save probe rounds
// in batches. These tests hold what that rests on: a round goes through the
// monitor the same way and ends up stored the same way, and an outage event
// is never on disk ahead of the rounds that confirmed it, because the store
// saves what is waiting before it writes the event.
//
// The rounds here dial real loopback sockets. A prober with no targets, which
// the other tests of this package use, makes a round with no rows, and
// InsertSamples returns on an empty round before it holds anything.

// batchedMonitor is a monitor on a file-backed store. every is the store's
// save interval, and 0 is a store that writes every round at once.
func batchedMonitor(t *testing.T, downAfter, upAfter int, every time.Duration) (*Monitor, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "monitor.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if every > 0 {
		st.SetSaveEveryFn(func() time.Duration { return every })
	}
	m := New(config.Config{DownAfter: downAfter, UpAfter: upAfter}, nil, st,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.since = time.Unix(0, 0)
	// The DNS probe runs for real, off the round's goroutine as in the daemon,
	// with the lookup itself stubbed.
	old := resolveTime
	resolveTime = func(context.Context) (time.Duration, bool, error) { return 3 * time.Millisecond, true, nil }
	t.Cleanup(func() { resolveTime = old })
	return m, st
}

// listening returns a prober whose three targets answer.
func listening(t *testing.T) *prober.Prober {
	t.Helper()
	var targets []config.Target
	for _, name := range []string{"cloudflare", "google", "quad9"} {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
		targets = append(targets, config.Target{Name: name, Network: "tcp4", Address: ln.Addr().String(), Family: config.IPv4})
	}
	return prober.New(targets, 2*time.Second)
}

// refusing returns a prober whose three targets are closed loopback ports:
// every dial is refused straight away.
func refusing() *prober.Prober {
	var targets []config.Target
	for _, name := range []string{"cloudflare", "google", "quad9"} {
		targets = append(targets, config.Target{Name: name, Network: "tcp4", Address: "127.0.0.1:1", Family: config.IPv4})
	}
	return prober.New(targets, 500*time.Millisecond)
}

// oneRound runs a round and waits for its DNS reading, so every round has
// handed over all it has before the next begins.
func oneRound(m *Monitor) {
	m.round(context.Background())
	m.dnsWG.Wait()
}

// onDisk counts a table's rows with plain SQL, which saves nothing.
func onDisk(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// samplesSeenByEvents makes the database record how many samples it held at
// the moment each event landed. The trigger runs inside the event's own
// insert, so nothing the test does can come between the two.
func samplesSeenByEvents(t *testing.T, st *store.Store) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE seen_by_event (type TEXT, samples INTEGER)`,
		`CREATE TRIGGER seen_by_event AFTER INSERT ON events BEGIN
			INSERT INTO seen_by_event VALUES (NEW.type, (SELECT COUNT(*) FROM samples));
		END`,
	} {
		if _, err := st.DB().Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func seenByEvents(t *testing.T, st *store.Store) []string {
	t.Helper()
	rows, err := st.DB().Query(`SELECT type, samples FROM seen_by_event ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var typ string
		var n int
		if err := rows.Scan(&typ, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s after %d samples", typ, n))
	}
	return out
}

// An outage record is never on disk ahead of the rounds that confirmed it.
// Two good rounds, then two that fail: the second confirms the outage, and
// the 'down' lands behind all four. Then one good round confirms the
// recovery, and the 'up' lands behind all five. Nothing else saves here: the
// interval is two minutes and no read is made.
func TestTransitionSavesReadingsBeforeTheEvent(t *testing.T) {
	stats.ResetForTest()
	m, st := batchedMonitor(t, 2, 1, store.MaxSaveEvery)
	samplesSeenByEvents(t, st)
	up, down := listening(t), refusing()

	m.prober = up
	oneRound(m)
	oneRound(m)
	if got := onDisk(t, st, "samples"); got != 0 || st.BufferedRows() != 8 {
		t.Fatalf("after two quiet rounds %d samples are on disk and %d rows wait, want 0 and 8: the rounds are not being held", got, st.BufferedRows())
	}
	m.prober = down
	oneRound(m)
	if got := onDisk(t, st, "events"); got != 0 {
		t.Fatalf("one failed round wrote %d events, with Down after at 2", got)
	}
	oneRound(m)
	m.prober = up
	oneRound(m)

	want := []string{"down after 12 samples", "up after 15 samples"}
	if got := seenByEvents(t, st); !reflect.DeepEqual(got, want) {
		t.Errorf("the events were stored as %v, want %v: an event went in ahead of the rounds that confirmed it", got, want)
	}
	if got := counter("db.sample_saves.order"); got != 2 {
		t.Errorf("db.sample_saves.order = %d, want 2, one for each event", got)
	}
	// The confirming round is saved with the event. Only its DNS reading,
	// which the monitor takes off the round's goroutine, may come after.
	if got := onDisk(t, st, "samples"); got != 15 {
		t.Errorf("%d samples are on disk after the recovery, want all 15", got)
	}
	if got := st.BufferedRows(); got > 1 {
		t.Errorf("%d rows still wait after the recovery, want the last DNS reading at most", got)
	}
}

// A failed event write leaves the event with the monitor, which tries again
// on the next round. The rounds were saved before the write was tried, so
// they are on disk whatever became of it.
func TestTransitionSavesEvenWhenTheEventWriteFails(t *testing.T) {
	stats.ResetForTest()
	m, st := batchedMonitor(t, 2, 1, store.MaxSaveEvery)
	if _, err := st.DB().Exec(`CREATE TRIGGER refuse_events BEFORE INSERT ON events BEGIN
		SELECT RAISE(ABORT, 'no events today');
	END`); err != nil {
		t.Fatal(err)
	}
	m.prober = refusing()
	oneRound(m)
	oneRound(m)
	if m.online {
		t.Fatal("two failed rounds did not take the link down")
	}
	if got := onDisk(t, st, "events"); got != 0 {
		t.Fatalf("premise: %d events were stored through a trigger that refuses them", got)
	}
	if got := onDisk(t, st, "samples"); got != 6 {
		t.Errorf("%d samples are on disk after the event write failed, want the 6 of the two rounds", got)
	}
	m.mu.Lock()
	pending := len(m.pendingEvents)
	m.mu.Unlock()
	if pending != 1 {
		t.Fatalf("%d events are kept for a retry, want the 'down'", pending)
	}
	// The store takes events again. The next round begins by writing the one
	// that was kept.
	if _, err := st.DB().Exec(`DROP TRIGGER refuse_events`); err != nil {
		t.Fatal(err)
	}
	samplesSeenByEvents(t, st)
	oneRound(m)
	if got, want := seenByEvents(t, st), []string{"down after 6 samples"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the retried event was stored as %v, want %v", got, want)
	}
}

// shape is a table without what two runs cannot share: when the rounds ran
// and how long each dial took.
func shape(t *testing.T, st *store.Store, query string) []string {
	t.Helper()
	rows, err := st.DB().Query(query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, fmt.Sprint(vals...))
	}
	return out
}

// The same rounds through the monitor leave the same rows, held or not: the
// targets in the order they were dialled, a failed dial without a latency,
// one DNS reading a round, and every round's rows behind those of the round
// before. The mix has rounds that fail, so both kinds of row are in it.
func TestBatchedRoundsStoreTheSameRows(t *testing.T) {
	const (
		samples = `SELECT rowid, target, success, family, typeof(latency_ms), typeof(ts) FROM samples ORDER BY rowid`
		dns     = `SELECT rowid, success, typeof(latency_ms), typeof(ts) FROM dns ORDER BY rowid`
		// A round's rows share its second, and rounds do not overtake each other.
		outOfOrder = `SELECT COUNT(*) FROM samples a JOIN samples b ON b.rowid = a.rowid + 1 WHERE b.ts < a.ts`
		perRound   = `SELECT COUNT(*) FROM (SELECT ts FROM samples GROUP BY ts HAVING COUNT(*) % 3 <> 0)`
	)
	run := func(every time.Duration) *store.Store {
		m, st := batchedMonitor(t, 9, 9, every) // no transition: nothing here is saved for an event
		up, down := listening(t), refusing()
		for i := 0; i < 12; i++ {
			m.prober = up
			if i%4 == 3 {
				m.prober = down
			}
			oneRound(m)
		}
		return st
	}
	direct, held := run(0), run(store.MaxSaveEvery)
	if got := onDisk(t, held, "samples"); got != 0 || held.BufferedRows() != 48 {
		t.Fatalf("premise: after twelve rounds the batching store has %d samples on disk and %d rows waiting, want 0 and 48", got, held.BufferedRows())
	}
	if err := held.SaveBuffered(store.SaveOrder); err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, q := range []string{samples, dns} {
		want, got := shape(t, direct, q), shape(t, held, q)
		if len(want) == 0 {
			t.Fatalf("the rounds stored nothing (%s): the comparison would pass on two empty tables", q)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rounds held and saved together left other rows than rounds written one by one\n%s\nheld:   %v\ndirect: %v", q, got, want)
		}
	}
	for _, st := range []*store.Store{direct, held} {
		for _, q := range []string{outOfOrder, perRound} {
			var n int
			if err := st.DB().QueryRow(q).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%d rows break the order of the rounds (%s)", n, q)
			}
		}
	}
}
