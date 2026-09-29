package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// PROBE READINGS WAIT IN MEMORY, AND NOTHING THAT READS THEM CAN TELL.
//
// A store with a save interval keeps rounds and DNS readings and writes them
// together. What these tests hold is that the batch is invisible: the same
// rows end up stored in the same order, every read and every ordered write
// sees them, a failed save loses nothing, and a clean stop writes what is
// left. The tests that need more than one connection open a real file. The
// others use open(t) and so run on a file too in CI's file-backed leg.

// batched opens a store that holds rounds for d, the way the daemon sets one
// up once the settings have loaded.
func batched(t *testing.T, d time.Duration) *Store {
	t.Helper()
	st := open(t)
	st.SetSaveEveryFn(func() time.Duration { return d })
	return st
}

// handTimer stands in for time.AfterFunc. It keeps what was asked for and
// runs it when the test says, so no test waits out an interval.
type handTimer struct {
	mu    sync.Mutex
	after []time.Duration
	fns   []func()
}

func setHandTimer(st *Store) *handTimer {
	h := &handTimer{}
	st.held.mu.Lock()
	st.held.after = func(d time.Duration, f func()) *time.Timer {
		h.mu.Lock()
		h.after = append(h.after, d)
		h.fns = append(h.fns, f)
		h.mu.Unlock()
		return time.NewTimer(time.Hour) // something to Stop. It is never waited for
	}
	st.held.mu.Unlock()
	return h
}

func (h *handTimer) armed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.fns)
}

// last is the delay the newest timer was set for.
func (h *handTimer) last() time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.after[len(h.after)-1]
}

// fire runs timer i the way the runtime would, stopped or not: a stopped
// timer that had already fired runs too, and must find nothing to do.
func (h *handTimer) fire(i int) {
	h.mu.Lock()
	f := h.fns[i]
	h.mu.Unlock()
	f()
}

func (h *handTimer) fireLast() { h.fire(h.armed() - 1) }

// handClock is the buffer's clock, moved by the test.
type handClock struct {
	mu sync.Mutex
	t  time.Time
}

func setHandClock(st *Store) *handClock {
	c := &handClock{t: time.Now()}
	st.held.mu.Lock()
	st.held.now = c.now
	st.held.mu.Unlock()
	return c
}

func (c *handClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *handClock) step(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// failSaves makes every save fail with err, and counts the attempts, until
// heal is called.
func failSaves(st *Store, err error) (attempts *atomic.Int64, heal func()) {
	attempts = &atomic.Int64{}
	st.held.mu.Lock()
	st.held.write = func(context.Context, txBeginner, []heldRound, []heldDNS, bool) error {
		attempts.Add(1)
		return err
	}
	st.held.mu.Unlock()
	return attempts, func() {
		st.held.mu.Lock()
		st.held.write = writeHeld
		st.held.mu.Unlock()
	}
}

// saveLines collects what the store reports about its saves.
type saveLines struct {
	mu    sync.Mutex
	lines []string
}

func captureSaveLog(st *Store) *saveLines {
	l := &saveLines{}
	st.SetSaveLogFn(func(level slog.Level, msg string, args ...any) {
		l.mu.Lock()
		l.lines = append(l.lines, level.String()+" "+msg+" "+fmt.Sprint(args...))
		l.mu.Unlock()
	})
	return l
}

func (l *saveLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// onDisk counts a table's rows with plain SQL. Nothing in the store is asked,
// so nothing is saved by looking.
func onDisk(t *testing.T, st *Store, table string) int64 {
	t.Helper()
	var n int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// dump reads whole rows with plain SQL, one string a row, in rowid order.
func dump(t *testing.T, st *Store, query string) []string {
	t.Helper()
	rows, err := st.db.Query(query)
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
			t.Fatalf("scan: %v", err)
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		out = append(out, fmt.Sprint(vals...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

const (
	samplesAsStored = `SELECT rowid, ts, target, latency_ms, typeof(latency_ms), success, family, typeof(family) FROM samples ORDER BY rowid`
	dnsAsStored     = `SELECT rowid, ts, latency_ms, typeof(latency_ms), success FROM dns ORDER BY rowid`
	eventsAsStored  = `SELECT ts, type, duration_s, detail FROM events ORDER BY ts, type`
)

// round is one probe round of three IPv4 targets at ts.
func round(ts time.Time, ok bool) []Sample {
	var sms []Sample
	for _, name := range []string{"a", "b", "c"} {
		sms = append(sms, Sample{TS: ts, Target: name, Family: "ipv4", LatencyMS: 12.5, Success: ok})
	}
	return sms
}

func mustInsert(t *testing.T, st *Store, sms []Sample) {
	t.Helper()
	if err := st.InsertSamples(context.Background(), sms); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
}

func mustDNS(t *testing.T, st *Store, ts time.Time, ms float64, ok bool) {
	t.Helper()
	if err := st.InsertDNS(context.Background(), ts, ms, ok); err != nil {
		t.Fatalf("insert dns: %v", err)
	}
}

// A store nobody gave an interval is every store a test opens for itself, and
// the one reset-auth opens. About fifty test files insert a row and read it
// back with plain SQL on the next line.
func TestUnconfiguredStoreWritesEveryRound(t *testing.T) {
	st := open(t)
	now := time.Now()
	mustInsert(t, st, round(now, true))
	mustDNS(t, st, now, 8.5, true)
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 3 || d != 1 {
		t.Fatalf("right after the inserts the tables hold %d samples and %d dns rows, want 3 and 1: "+
			"a store with no save interval must write every round at once", s, d)
	}
	if n := st.BufferedRows(); n != 0 {
		t.Errorf("%d rows are waiting in a store that was never told to hold any", n)
	}
}

// 0 is the setting for "as before", not a delay of nothing.
func TestSaveEveryZeroWritesEveryRound(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, 0)
	h := setHandTimer(st)
	now := time.Now()
	mustInsert(t, st, round(now, true))
	mustDNS(t, st, now, 8.5, true)
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 3 || d != 1 {
		t.Fatalf("with the interval at 0 the tables hold %d samples and %d dns rows right after the inserts, want 3 and 1", s, d)
	}
	if h.armed() != 0 || st.BufferedRows() != 0 || counter("db.sample_rows_buffered") != 0 {
		t.Errorf("with the interval at 0: %d timers set, %d rows waiting, %d rows booked as held; want none of any",
			h.armed(), st.BufferedRows(), counter("db.sample_rows_buffered"))
	}
	// A negative interval cannot come out of the settings, which clamp it,
	// but the store is handed a function and must not trust it.
	st.SetSaveEveryFn(func() time.Duration { return -time.Second })
	mustInsert(t, st, round(now.Add(time.Second), true))
	if s := onDisk(t, st, "samples"); s != 6 {
		t.Errorf("with a negative interval the table holds %d samples, want 6", s)
	}
	// And one above the maximum is the maximum.
	st.SetSaveEveryFn(func() time.Duration { return time.Hour })
	if got := st.saveEvery(); got != MaxSaveEvery {
		t.Errorf("an interval of an hour is taken as %v, want the maximum %v", got, MaxSaveEvery)
	}
}

// The two paths hold their own copy of the two INSERT statements and of how a
// row's values are bound. This is what keeps the copies the same: fifty rounds
// through each, and the tables compared cell by cell, storage class included.
func TestBatchedRowsMatchDirectRows(t *testing.T) {
	direct, _ := openWALStore(t)
	held, _ := openWALStore(t)
	held.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(held)
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < 50; i++ {
		ts := base.Add(time.Duration(i) * 5 * time.Second)
		sms := []Sample{
			{TS: ts, Target: "a", Family: "ipv4", LatencyMS: 12.5, Success: true},
			{TS: ts, Target: "zero", Family: "ipv4", LatencyMS: 0, Success: true},     // a latency of 0 is a value
			{TS: ts, Target: "failed", Family: "ipv6", LatencyMS: 99, Success: false}, // a failure has no latency, whatever was measured
			{TS: ts, Target: "old-v6", Family: "", LatencyMS: 3.25, Success: true},    // no family is NULL, not ''
		}
		for _, st := range []*Store{direct, held} {
			mustInsert(t, st, sms)
			mustDNS(t, st, ts, 8.5+float64(i), i%7 != 0) // every seventh lookup fails
		}
		if i%6 == 5 {
			if err := held.SaveBuffered(SaveAge); err != nil {
				t.Fatalf("save after round %d: %v", i, err)
			}
		}
	}
	if err := held.SaveBuffered(SaveOrder); err != nil {
		t.Fatalf("last save: %v", err)
	}
	for _, q := range []string{samplesAsStored, dnsAsStored} {
		want, got := dump(t, direct, q), dump(t, held, q)
		if len(want) == 0 {
			t.Fatalf("the direct store holds no rows for %q: the comparison would pass on two empty tables", q)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rows saved in batches differ from rows written one round at a time\n%s\nbatched: %d rows, direct: %d rows", q, len(got), len(want))
			for i := range want {
				if i >= len(got) || got[i] != want[i] {
					g := "(missing)"
					if i < len(got) {
						g = got[i]
					}
					t.Errorf("first difference at row %d:\n  batched %s\n  direct  %s", i, g, want[i])
					break
				}
			}
		}
	}
}

// The interval counts from the oldest waiting row. While monitoring is paused
// no round arrives, so nothing but a timer can save what is waiting.
func TestAgeSaveFiresAtTheBound(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	setHandClock(st)
	mustInsert(t, st, round(time.Now(), true))
	if h.armed() != 1 || h.last() != 30*time.Second {
		t.Fatalf("after the first round %d timers were set, the last for %v; want one, for 30s", h.armed(), h.after)
	}
	if n := onDisk(t, st, "samples"); n != 0 {
		t.Fatalf("%d rows were written before the interval ran out", n)
	}
	h.fireLast()
	if n := onDisk(t, st, "samples"); n != 3 {
		t.Fatalf("the timer ran and the table holds %d rows, want 3", n)
	}
	if got := counter("db.sample_saves.age"); got != 1 {
		t.Errorf("db.sample_saves.age = %d, want 1", got)
	}
	if got := counter("db.sample_rows_buffered"); got != 3 {
		t.Errorf("db.sample_rows_buffered = %d, want 3", got)
	}
	if got := stats.Lifetime().Gauges["db.sample_rows_waiting"]; got != 0 || st.BufferedRows() != 0 {
		t.Errorf("after the save the gauge reads %d and %d rows wait, want 0 and 0", got, st.BufferedRows())
	}
}

// A timer set again by every round would never run out at a 5 s cadence.
func TestAgeCountsFromTheOldestRow(t *testing.T) {
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	c := setHandClock(st)
	now := time.Now()
	mustInsert(t, st, round(now, true))
	for i := 1; i <= 5; i++ {
		c.step(5 * time.Second)
		mustInsert(t, st, round(now.Add(time.Duration(i)*5*time.Second), true))
		mustDNS(t, st, now, 8.5, true)
	}
	if h.armed() != 1 {
		t.Fatalf("six rounds set %d timers (%v), want the one the first round set", h.armed(), h.after)
	}
	h.fireLast()
	if n := onDisk(t, st, "samples"); n != 18 {
		t.Errorf("the one timer saved %d rows, want all 18", n)
	}
	// The next round starts the next batch, and its deadline is its own.
	c.step(time.Second)
	mustInsert(t, st, round(now.Add(31*time.Second), true))
	if h.armed() != 2 || h.last() != 30*time.Second {
		t.Errorf("the first round after a save set timer %d for %v, want a second one for 30s", h.armed(), h.last())
	}
}

// A lowered setting applies to the rows already waiting, from the next round
// on. A raised one lets the deadline that was set stand.
func TestLoweredSettingRearmsTheTimer(t *testing.T) {
	var every atomic.Int64
	every.Store(int64(30 * time.Second))
	st := open(t)
	st.SetSaveEveryFn(func() time.Duration { return time.Duration(every.Load()) })
	h := setHandTimer(st)
	c := setHandClock(st)
	now := time.Now()
	mustInsert(t, st, round(now, true))
	c.step(4 * time.Second)
	every.Store(int64(10 * time.Second))
	mustInsert(t, st, round(now.Add(4*time.Second), true))
	if h.armed() != 2 || h.last() != 6*time.Second {
		t.Fatalf("after the setting went from 30s to 10s, 4s into the batch: %d timers, the last for %v; want a second one for the 6s that are left", h.armed(), h.last())
	}
	every.Store(int64(60 * time.Second))
	mustInsert(t, st, round(now.Add(5*time.Second), true))
	if h.armed() != 2 {
		t.Errorf("a raised setting set a timer of its own (%v): the earlier deadline must stand", h.after)
	}
	// The timer that was replaced had fired already, say. It finds another
	// deadline set and leaves the rows alone.
	h.fire(0)
	if n := onDisk(t, st, "samples"); n != 0 {
		t.Errorf("a replaced timer saved %d rows", n)
	}
	h.fire(1)
	if n := onDisk(t, st, "samples"); n != 9 {
		t.Errorf("the timer in force saved %d rows, want 9", n)
	}
}

// Switching batching off with rows waiting must not let the next round
// overtake them.
func TestSettingSwitchedToZeroSavesAtOnce(t *testing.T) {
	var every atomic.Int64
	every.Store(int64(30 * time.Second))
	st := open(t)
	st.SetSaveEveryFn(func() time.Duration { return time.Duration(every.Load()) })
	setHandTimer(st)
	now := time.Now()
	a, b := round(now, true), round(now.Add(5*time.Second), true)
	for i := range a {
		a[i].Target, b[i].Target = "A", "B"
	}
	mustInsert(t, st, a)
	every.Store(0)
	mustInsert(t, st, b)
	got := dump(t, st, `SELECT target FROM samples ORDER BY rowid`)
	if want := []string{"A", "A", "A", "B", "B", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with the interval set to 0 behind a waiting round, the table holds %v; want %v, both rounds and in the order they were taken", got, want)
	}
	if st.BufferedRows() != 0 {
		t.Errorf("%d rows still wait with the interval at 0", st.BufferedRows())
	}
	// From here every round is written by itself again.
	mustInsert(t, st, round(now.Add(10*time.Second), true))
	if n := onDisk(t, st, "samples"); n != 9 {
		t.Errorf("the next round left the table at %d rows, want 9", n)
	}
}

// The timer is real in the daemon. One test has to run it for real.
func TestTheRealTimerSaves(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return 50 * time.Millisecond })
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := onDisk(t, st, "samples"); n != 0 {
		t.Fatalf("%d rows were written at once, with an interval of 50ms", n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for onDisk(t, st, "samples") != 6 || onDisk(t, st, "dns") != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("5s after a round with a 50ms interval the tables hold %d samples and %d dns rows, want 6 and 1",
				onDisk(t, st, "samples"), onDisk(t, st, "dns"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The round and its DNS reading were handed over microseconds apart. One
	// save takes both, or the second row gets a timer and a save of its own.
	if got := counter("db.sample_saves.age"); got < 1 || got > 2 {
		t.Errorf("db.sample_saves.age = %d, want 1 or 2", got)
	}
}

// readerCase is one way of reading the two tables. saved is on disk before
// batching starts. waiting is handed over while it is on, and is chosen so
// that the answer depends on it.
type readerCase struct {
	name    string
	saved   func(t *testing.T, st *Store, now time.Time)
	waiting func(t *testing.T, st *Store, now time.Time)
	read    func(t *testing.T, st *Store, now time.Time) any
}

func ago(now time.Time, s int) time.Time { return now.Add(-time.Duration(s) * time.Second) }

// openOutage is a link that went down five minutes ago and has failed every
// round since, up to 200 s ago.
func openOutage(t *testing.T, st *Store, now time.Time) {
	t.Helper()
	mustInsert(t, st, round(ago(now, 600), true))
	mustInsert(t, st, round(ago(now, 500), true))
	if err := st.InsertEvent(context.Background(), ago(now, 300), "down", -1, ""); err != nil {
		t.Fatalf("insert down: %v", err)
	}
	for _, s := range []int{300, 250, 200} {
		mustInsert(t, st, round(ago(now, s), false))
	}
}

// recovered is the round that shows the link back, 100 s ago.
func recovered(t *testing.T, st *Store, now time.Time) {
	t.Helper()
	mustInsert(t, st, round(ago(now, 100), true))
	mustDNS(t, st, ago(now, 100), 8.5, true)
}

// redetected is an outage a restart split in two: down, down again, up.
func redetected(t *testing.T, st *Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	mustInsert(t, st, round(ago(now, 600), true))
	for _, e := range []struct {
		s   int
		typ string
		dur int
	}{{400, "down", -1}, {300, "down", -1}, {200, "up", 100}} {
		if err := st.InsertEvent(ctx, ago(now, e.s), e.typ, e.dur, ""); err != nil {
			t.Fatalf("insert %s: %v", e.typ, err)
		}
	}
	mustInsert(t, st, round(ago(now, 400), false))
	mustInsert(t, st, round(ago(now, 250), false))
}

// betweenTheDowns is a round that shows the link up between the two 'down'
// events, which makes them two outages.
func betweenTheDowns(t *testing.T, st *Store, now time.Time) {
	t.Helper()
	mustInsert(t, st, round(ago(now, 350), true))
}

func nothing(*testing.T, *Store, time.Time) {}

func readerCases() []readerCase {
	ctx := context.Background()
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	return []readerCase{
		{"LastObservedTS", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			ts, ok, err := st.LastObservedTS(ctx)
			must(t, err)
			return fmt.Sprint(ts-now.Unix(), ok)
		}},
		{"HasHistory", nothing, recovered, func(t *testing.T, st *Store, now time.Time) any {
			has, err := st.HasHistory(ctx)
			must(t, err)
			return has
		}},
		{"monitoringSince", openOutage, func(t *testing.T, st *Store, now time.Time) {
			mustInsert(t, st, round(ago(now, 900), true)) // older than anything saved
		}, func(t *testing.T, st *Store, now time.Time) any {
			first, err := st.monitoringSince(ctx, now.Unix())
			must(t, err)
			return first - now.Unix()
		}},
		{"UptimeFloor", openOutage, func(t *testing.T, st *Store, now time.Time) {
			mustInsert(t, st, round(ago(now, 900), true))
		}, func(t *testing.T, st *Store, now time.Time) any {
			first, err := st.UptimeFloor(ctx, 0)
			must(t, err)
			return first - now.Unix()
		}},
		{"firstQuorumRecovery", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			rec, ok, err := st.firstQuorumRecovery(ctx, now.Unix()-300, now.Unix())
			must(t, err)
			return fmt.Sprint(rec-now.Unix(), ok)
		}},
		{"newestSampleAt", openOutage, func(t *testing.T, st *Store, now time.Time) {
			mustInsert(t, st, round(ago(now, 50), false))
		}, func(t *testing.T, st *Store, now time.Time) any {
			newest, ok, err := st.newestSampleAt(ctx, now.Unix())
			must(t, err)
			return fmt.Sprint(newest-now.Unix(), ok)
		}},
		{"UptimeSince", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			o, err := st.UptimeSince(ctx, now.Add(-time.Hour), 0)
			must(t, err)
			return o.Down
		}},
		{"UptimeWindows", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			u, err := st.UptimeWindows(ctx, now, 0)
			must(t, err)
			var down []time.Duration
			for _, w := range u.Each() {
				down = append(down, w.Obs.Down)
			}
			return down
		}},
		{"DowntimeByDay", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			days, err := st.DowntimeByDay(ctx, now.Add(-48*time.Hour), time.UTC)
			must(t, err)
			total := 0
			for _, d := range days {
				total += d.DowntimeS
			}
			return total
		}},
		{"ResolvedOutagesSince", redetected, betweenTheDowns, func(t *testing.T, st *Store, now time.Time) any {
			n, down, err := st.ResolvedOutagesSince(ctx, now.Unix()-3600)
			must(t, err)
			return fmt.Sprint(n, down)
		}},
		{"DeleteOutage", redetected, betweenTheDowns, func(t *testing.T, st *Store, now time.Time) any {
			n, err := st.DeleteOutage(ctx, now.Unix()-200)
			must(t, err)
			return fmt.Sprint(n, dump(t, st, eventsAsStored))
		}},
		{"LatestPerTarget", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			got, err := st.LatestPerTarget(ctx, 15*time.Second)
			must(t, err)
			return fmt.Sprintf("%+v", got)
		}},
		{"Series, live", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			pts, err := st.Series(ctx, now.Add(-time.Hour), time.Time{}, 5, nil)
			must(t, err)
			return chartText(pts)
		}},
		{"Series, cached", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			pts, err := st.Series(ctx, now.Add(-time.Hour), time.Time{}, 60, nil)
			must(t, err)
			return chartText(pts)
		}},
		{"TableCounts", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			n, err := st.TableCounts(ctx)
			must(t, err)
			return fmt.Sprint(n["samples"], n["dns"])
		}},
		{"ExportTableRows", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			var rows []string
			for _, table := range []string{"samples", "dns"} {
				must(t, st.ExportTableRows(ctx, table, func(m map[string]any) error {
					rows = append(rows, fmt.Sprint(table, m))
					return nil
				}))
			}
			return rows
		}},
		{"ExportTable", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			rows, err := st.ExportTable(ctx, "samples")
			must(t, err)
			return fmt.Sprint(rows)
		}},
		{"BeginReadSnapshot", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			snap, err := st.BeginReadSnapshot(ctx)
			must(t, err)
			defer snap.Rollback()
			var rows []string
			for _, table := range []string{"samples", "dns"} {
				must(t, st.ExportTableRowsTx(ctx, snap, table, func(m map[string]any) error {
					rows = append(rows, fmt.Sprint(table, m))
					return nil
				}))
			}
			return rows
		}},
		{"Clear", openOutage, recovered, func(t *testing.T, st *Store, now time.Time) any {
			n, err := st.Clear(ctx, "latency")
			must(t, err)
			// What is left after the clear, once anything still waiting has
			// been written. A clear that did not save first leaves rows here.
			must(t, st.SaveBuffered(SaveOrder))
			return fmt.Sprint(n, onDisk(t, st, "samples"), onDisk(t, st, "dns"))
		}},
		{"Prune", func(t *testing.T, st *Store, now time.Time) {
			// A link that went down ten days ago, with nothing to say it came back.
			mustInsert(t, st, round(now.Add(-11*24*time.Hour), true))
			if err := st.InsertEvent(ctx, now.Add(-10*24*time.Hour), "down", -1, ""); err != nil {
				t.Fatalf("insert down: %v", err)
			}
			mustInsert(t, st, round(now.Add(-10*24*time.Hour), false))
			mustInsert(t, st, round(ago(now, 60), true))
		}, func(t *testing.T, st *Store, now time.Time) {
			// The round that shows it back, a day later. The cleanup is about
			// to delete it, and must close the outage from it first.
			mustInsert(t, st, round(now.Add(-9*24*time.Hour), true))
		}, func(t *testing.T, st *Store, now time.Time) any {
			n, err := st.Prune(ctx, now.Add(-24*time.Hour), time.Unix(0, 0), time.Unix(0, 0))
			must(t, err)
			must(t, st.SaveBuffered(SaveOrder))
			return fmt.Sprint(n, onDisk(t, st, "samples"), dump(t, st, eventsAsStored))
		}},
	}
}

func chartText(pts []SeriesPoint) string {
	var b strings.Builder
	for _, p := range pts {
		fmt.Fprintf(&b, "%d online=%v", p.TS, p.Online)
		if p.LatencyMS != nil {
			fmt.Fprintf(&b, " lat=%v", *p.LatencyMS)
		}
		if p.DNSms != nil {
			fmt.Fprintf(&b, " dns=%v", *p.DNSms)
		}
		b.WriteString("; ")
	}
	return b.String()
}

// Every read answers as if each round had been written when it was taken.
//
// Each case runs on three stores. One writes every round at once and holds
// all the rows: its answer is the right one. One holds only what was saved:
// its answer has to differ, or the waiting rows do not decide the answer and
// the case proves nothing. The third has the deciding rows waiting, and has
// to give the right answer. Looking at the buffer afterwards would not do: a
// function that reads first and saves after leaves it just as empty.
//
// A fresh store for every case, because the first read that saves leaves
// nothing waiting for the next.
func TestEveryReaderSeesWaitingRows(t *testing.T) {
	for _, c := range readerCases() {
		t.Run(c.name, func(t *testing.T) {
			now := time.Now()
			full, short, held := open(t), open(t), open(t)
			c.saved(t, full, now)
			c.waiting(t, full, now)
			c.saved(t, short, now)
			c.saved(t, held, now)
			held.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
			setHandTimer(held)
			c.waiting(t, held, now)
			if held.BufferedRows() == 0 {
				t.Fatal("nothing is waiting before the read: the case tests a store that holds no rows")
			}
			want, without := c.read(t, full, now), c.read(t, short, now)
			if reflect.DeepEqual(want, without) {
				t.Fatalf("the answer is %v with and without the waiting rows: they have to decide it", want)
			}
			if got := c.read(t, held, now); !reflect.DeepEqual(got, want) {
				t.Errorf("read with rows waiting: %v\nread with every round written at once: %v\n(and without those rows: %v)", got, want, without)
			}
		})
	}
}

// An open outage ends at the newest sample when nothing shows a recovery.
// The failing rounds still waiting are part of the outage.
func TestOpenOutageEndsAtTheNewestWaitingSample(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	direct, held := open(t), open(t)
	for _, st := range []*Store{direct, held} {
		openOutage(t, st, now)
	}
	held.SetSaveEveryFn(func() time.Duration { return 30 * time.Second })
	setHandTimer(held)
	for _, st := range []*Store{direct, held} {
		mustInsert(t, st, round(ago(now, 150), false))
		mustInsert(t, st, round(ago(now, 100), false))
	}
	want, err := direct.UptimeSince(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := held.UptimeSince(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want.Down != 200*time.Second {
		t.Fatalf("premise: the direct store books %v of downtime, want 200s (down 300s ago, last failed round 100s ago)", want.Down)
	}
	if got.Down != want.Down {
		t.Errorf("downtime with the last two failing rounds waiting: %v, want %v: the open outage was booked short", got.Down, want.Down)
	}
}

// seenAtInsert makes the database itself record how many samples and dns
// rows it held at the moment a row landed in one of the tables that are
// ordered against them. A trigger runs inside the insert, so no read of the
// test's can come between.
func seenAtInsert(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.db.Exec(`CREATE TABLE seen_at_insert (tbl TEXT, samples INTEGER, dns INTEGER)`); err != nil {
		t.Fatalf("create audit table: %v", err)
	}
	for _, tbl := range []string{"events", "pauses", "speed", "speed_spans", "speed_servers"} {
		if _, err := st.db.Exec(`CREATE TRIGGER seen_` + tbl + ` AFTER INSERT ON ` + tbl + ` BEGIN
			INSERT INTO seen_at_insert VALUES ('` + tbl + `', (SELECT COUNT(*) FROM samples), (SELECT COUNT(*) FROM dns));
		END`); err != nil {
			t.Fatalf("create trigger on %s: %v", tbl, err)
		}
	}
}

// Events, pauses and speed rows are never stored before the samples that
// precede them. Each writer is handed a round first, and the database says
// what it held when the writer's row arrived.
func TestOrderSavesBeforeEventPauseAndSpeedRows(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	seenAtInsert(t, st)
	now := time.Now()
	writers := []struct {
		tbl   string
		write func() error
	}{
		{"events", func() error { return st.InsertEvent(ctx, ago(now, 50), "down", -1, "") }},
		{"pauses", func() error { _, err := st.InsertPause(ctx, ago(now, 40), 10); return err }},
		{"speed_spans", func() error { _, err := st.InsertSpeedSpan(ctx, ago(now, 30), 10); return err }},
		{"speed", func() error {
			return st.InsertSpeed(ctx, SpeedSample{TS: now.Unix() - 20, DownMbps: 90, UpMbps: 10, PingMS: 9, Trigger: "manual", Engine: "ookla"})
		}},
		{"speed_servers", func() error {
			return st.InsertSpeedServers(ctx, []SpeedServerRow{{RunTS: now.Unix() - 20, ServerID: "1", Server: "one"}})
		}},
	}
	for i, w := range writers {
		mustInsert(t, st, round(ago(now, 100-i), true))
		mustDNS(t, st, ago(now, 100-i), 8.5, true)
		if st.BufferedRows() != 4 {
			t.Fatalf("%s: %d rows wait before the write, want the round's 4", w.tbl, st.BufferedRows())
		}
		if err := w.write(); err != nil {
			t.Fatalf("%s: %v", w.tbl, err)
		}
		var samples, dns int64
		if err := st.db.QueryRow(`SELECT samples, dns FROM seen_at_insert WHERE tbl = ?`, w.tbl).Scan(&samples, &dns); err != nil {
			t.Fatalf("%s: what the insert saw: %v", w.tbl, err)
		}
		if wantS, wantD := int64(3*(i+1)), int64(i+1); samples != wantS || dns != wantD {
			t.Errorf("the %s row was stored with %d samples and %d dns rows on disk, want %d and %d: "+
				"it went in ahead of the round taken before it", w.tbl, samples, dns, wantS, wantD)
		}
		if got := counter("db.sample_saves.order"); got != int64(i+1) {
			t.Errorf("after the %s write db.sample_saves.order = %d, want %d", w.tbl, got, i+1)
		}
	}
	// A span the table refuses is not a write, and saves nothing.
	mustInsert(t, st, round(ago(now, 10), true))
	if stored, err := st.InsertPause(ctx, ago(now, 5), -1); stored || err != nil {
		t.Fatalf("a pause of -1s: stored %v, err %v; want refused without an error", stored, err)
	}
	if stored, err := st.InsertSpeedSpan(ctx, time.Unix(100, 0), 10); stored || err != nil {
		t.Fatalf("a speedtest in 1970: stored %v, err %v; want refused without an error", stored, err)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("two refused rows left %d rows waiting, want the round's 3 still there", st.BufferedRows())
	}
	if err := st.InsertEvent(ctx, now, "sideways", -1, ""); err == nil || st.BufferedRows() != 3 {
		t.Errorf("an event of an unknown type: err %v, %d rows waiting; want an error and the 3 still there", err, st.BufferedRows())
	}
}

// When the rounds cannot be saved the row that was to follow them is written
// all the same. An outage record is never held back.
func TestOrderWritersGoOnWhenTheSaveFails(t *testing.T) {
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	captureSaveLog(st)
	failSaves(st, errors.New("disk I/O error"))
	now := time.Now()
	mustInsert(t, st, round(ago(now, 100), false))
	if err := st.InsertEvent(ctx, ago(now, 50), "down", -1, ""); err != nil {
		t.Fatalf("the event write failed with the save: %v", err)
	}
	if stored, err := st.InsertPause(ctx, ago(now, 40), 10); !stored || err != nil {
		t.Fatalf("the pause write: stored %v, err %v", stored, err)
	}
	if e, p := onDisk(t, st, "events"), onDisk(t, st, "pauses"); e != 1 || p != 1 {
		t.Errorf("with saves failing the tables hold %d events and %d pauses, want 1 and 1", e, p)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("%d rows wait after the failed saves, want the round's 3", st.BufferedRows())
	}
}

// The count a clear returns goes to the API, and the rows it names are gone.
func TestClearCountsWaitingRows(t *testing.T) {
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	h := setHandTimer(st)
	now := time.Now()
	for i := 0; i < 4; i++ {
		mustInsert(t, st, round(ago(now, 100-i), true))
		mustDNS(t, st, ago(now, 100-i), 8.5, true)
	}
	if err := st.SaveBuffered(SaveAge); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, st, round(ago(now, 10), true))
	mustDNS(t, st, ago(now, 10), 8.5, true)
	n, err := st.Clear(ctx, "latency")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if n != 20 {
		t.Errorf("clear removed %d rows, want 20: 16 saved and the 4 that were waiting", n)
	}
	// Nothing comes back: not by itself, and not when a timer that was set
	// for the cleared rows runs.
	for i := 0; i < h.armed(); i++ {
		h.fire(i)
	}
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 0 || d != 0 || st.BufferedRows() != 0 {
		t.Errorf("after the clear: %d samples, %d dns rows, %d waiting; want none", s, d, st.BufferedRows())
	}
	// The other kinds do not touch the two tables, and leave the rows waiting.
	mustInsert(t, st, round(now, true))
	if _, err := st.Clear(ctx, "speed"); err != nil {
		t.Fatal(err)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("clearing the speed history left %d rows waiting, want 3", st.BufferedRows())
	}
}

// One rule for every writer that deletes: what it cannot first save, it does
// not delete around.
func TestClearDeletesNothingWhenTheSaveFails(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	now := time.Now()
	mustInsert(t, st, round(ago(now, 100), true))
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	captureSaveLog(st)
	mustInsert(t, st, round(ago(now, 50), true))
	failSaves(st, errors.New("database or disk is full"))
	n, err := st.Clear(ctx, "latency")
	if err == nil || n != 0 {
		t.Fatalf("clear with the save failing: %d rows, err %v; want 0 and the save's error", n, err)
	}
	if s := onDisk(t, st, "samples"); s != 3 || st.BufferedRows() != 3 {
		t.Errorf("after the refused clear: %d rows on disk and %d waiting, want 3 and 3", s, st.BufferedRows())
	}
}

// An imported row is stored only where no row with its key is, and a waiting
// row is not where SQL can see it.
func TestImportDoesNotDuplicateWaitingRows(t *testing.T) {
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	ts := time.Now().Add(-time.Minute).Truncate(time.Second)
	mustInsert(t, st, round(ts, true))
	mustDNS(t, st, ts, 8.5, true)
	var samples, dns []map[string]any
	for _, sm := range round(ts, true) {
		samples = append(samples, map[string]any{"ts": float64(ts.Unix()), "target": sm.Target,
			"latency_ms": 12.5, "success": float64(1), "family": "ipv4"})
	}
	dns = append(dns, map[string]any{"ts": float64(ts.Unix()), "latency_ms": 8.5, "success": float64(1)})
	if n, err := st.ImportTable(ctx, "samples", samples); err != nil || n != 0 {
		t.Errorf("importing the waiting round's own rows added %d (err %v), want 0", n, err)
	}
	if n, err := st.ImportTable(ctx, "dns", dns); err != nil || n != 0 {
		t.Errorf("importing the waiting DNS reading added %d (err %v), want 0", n, err)
	}
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 3 || d != 1 || st.BufferedRows() != 0 {
		t.Errorf("after the import: %d samples, %d dns rows, %d waiting; want 3, 1 and 0", s, d, st.BufferedRows())
	}
}

// An import commits in chunks, and rounds arrive while a chunk runs. They are
// saved before the next chunk looks for the keys it is about to insert.
func TestImportSavesBetweenItsChunks(t *testing.T) {
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	live := round(base.Add(time.Duration(importTxRows+10)*time.Second), true)
	chunks := 0
	importChunkHook = func() {
		if chunks++; chunks == 1 {
			mustInsert(t, st, live) // a round taken while the first chunk ran
		}
	}
	t.Cleanup(func() { importChunkHook = nil })
	var rows []map[string]any
	for i := 0; i < importTxRows+20; i++ {
		rows = append(rows, map[string]any{"ts": float64(base.Unix() + int64(i)), "target": "a",
			"latency_ms": 12.5, "success": float64(1), "family": "ipv4"})
	}
	n, err := st.ImportTable(ctx, "samples", rows)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	// The live round's target "a" has the key of imported row importTxRows+10.
	if n != importTxRows+19 {
		t.Errorf("the import added %d rows, want %d: every row but the one whose key a live round took first", n, importTxRows+19)
	}
	if chunks == 0 {
		t.Fatal("the import never committed a chunk: the file is too small for this test")
	}
	if got := onDisk(t, st, "samples"); got != int64(importTxRows+20+2) {
		t.Errorf("the table holds %d rows, want %d: the file's, less the one duplicate, and the live round's 3", got, importTxRows+22)
	}
}

// A restore that stops part way is what the import works hardest to avoid. A
// save that fails does not stop it.
func TestImportGoesOnWhenTheSaveFails(t *testing.T) {
	ctx := context.Background()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	captureSaveLog(st)
	now := time.Now()
	mustInsert(t, st, round(ago(now, 50), true))
	failSaves(st, errors.New("database is locked"))
	n, err := st.ImportTable(ctx, "samples", []map[string]any{{"ts": float64(now.Unix() - 500), "target": "a",
		"latency_ms": 12.5, "success": float64(1), "family": "ipv4"}})
	if err != nil || n != 1 {
		t.Fatalf("import with the save failing: %d rows, err %v; want 1 and no error", n, err)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("%d rows wait after the import, want the round's 3", st.BufferedRows())
	}
}

// A disk too full to take the waiting rows must not stand in the way of the
// export that rescues the rest, nor of the cleanup that makes room.
func TestExportAndPruneGoOnWhenTheSaveFails(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	now := time.Now()
	mustInsert(t, st, round(ago(now, 100), true))
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	captureSaveLog(st)
	mustInsert(t, st, round(ago(now, 50), true))
	failSaves(st, errors.New("database or disk is full"))
	rows := 0
	if err := st.ExportTableRows(ctx, "samples", func(map[string]any) error { rows++; return nil }); err != nil || rows != 3 {
		t.Errorf("export with the save failing: %d rows, err %v; want the 3 that are saved", rows, err)
	}
	snap, err := st.BeginReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot with the save failing: %v", err)
	}
	snap.Rollback()
	if _, err := st.Prune(ctx, now.Add(-time.Hour), time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		t.Errorf("cleanup with the save failing: %v", err)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("%d rows wait, want 3: a failed save keeps its rows", st.BufferedRows())
	}
}

// A failed save loses nothing. The rows stay in order, the next save is five
// seconds on, and it is said once for the whole streak.
func TestFailedSaveKeepsRowsAndRetries(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	c := setHandClock(st)
	log := captureSaveLog(st)
	attempts, heal := failSaves(st, errors.New("disk I/O error"))
	now := time.Now()
	mustInsert(t, st, named(round(now, true), "A"))
	c.step(30 * time.Second) // the timer runs when the interval is over
	h.fireLast()
	if attempts.Load() != 1 || st.BufferedRows() != 3 || onDisk(t, st, "samples") != 0 {
		t.Fatalf("after a failed save: %d attempts, %d rows waiting, %d on disk; want 1, 3 and 0",
			attempts.Load(), st.BufferedRows(), onDisk(t, st, "samples"))
	}
	if got := counter("db.sample_save_failed"); got != 1 {
		t.Errorf("db.sample_save_failed = %d, want 1", got)
	}
	if h.armed() != 2 || h.last() != saveRetryEvery {
		t.Fatalf("after a failed save %d timers were set, the last for %v; want a retry %v on", h.armed(), h.last(), saveRetryEvery)
	}
	// A round that arrives meanwhile waits behind the failed ones, and does
	// not move the retry. The oldest row is past its interval by now, so a
	// timer set for it would run at once, and then again with every round.
	c.step(2 * time.Second)
	mustInsert(t, st, named(round(now.Add(5*time.Second), true), "B"))
	if h.armed() != 2 {
		t.Errorf("a round that arrived during the streak set a timer (%v): the store is tried again with every round, not every %v", h.after, saveRetryEvery)
	}
	c.step(3 * time.Second)
	h.fireLast()
	if attempts.Load() != 2 || h.armed() != 3 || h.last() != saveRetryEvery {
		t.Fatalf("after the retry failed too: %d attempts, %d timers, the last for %v; want 2, 3 and %v",
			attempts.Load(), h.armed(), h.last(), saveRetryEvery)
	}
	heal()
	c.step(5 * time.Second)
	h.fireLast()
	got := dump(t, st, `SELECT target FROM samples ORDER BY rowid`)
	if want := []string{"A", "A", "A", "B", "B", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after the store healed the table holds %v, want %v: every row once, in the order taken", got, want)
	}
	if st.BufferedRows() != 0 {
		t.Errorf("%d rows still wait after the save went through", st.BufferedRows())
	}
	if got := counter("db.sample_saves.age"); got != 1 {
		t.Errorf("db.sample_saves.age = %d, want 1: a failed save is not a save", got)
	}
	lines := log.all()
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ERROR could not save probe readings") ||
		!strings.HasPrefix(lines[1], "INFO saved the probe readings that had waited") {
		t.Errorf("the streak was reported as %q; want one error when it began and one line when it ended", lines)
	}
}

// named gives every row of a round the same target, so a test can read the
// order of whole rounds off the table.
func named(sms []Sample, target string) []Sample {
	for i := range sms {
		sms[i].Target = target
	}
	return sms
}

// duringTheFirstSave runs fn inside the first save, once the save has taken
// its rows and before it writes them. With fail set that save then fails
// with it. Every later save is an ordinary one. fn may hand rows over:
// that takes the buffer's lock and never the gate the save holds.
func duringTheFirstSave(st *Store, fn func(), fail error) {
	var calls atomic.Int64
	st.held.mu.Lock()
	st.held.write = func(ctx context.Context, q txBeginner, rounds []heldRound, dns []heldDNS, brief bool) error {
		if calls.Add(1) == 1 {
			fn()
			if fail != nil {
				return fail
			}
		}
		return writeHeld(ctx, q, rounds, dns, brief)
	}
	st.held.mu.Unlock()
}

// ROWS ARE HANDED OVER WHILE A SAVE IS RUNNING.
//
// That is ordinary in the daemon. The DNS reading is taken off the round's
// goroutine and lands beside the save an outage event starts, and rounds keep
// coming while a request saves. The save has taken its rows by then, so what
// arrives waits in a buffer that was empty a moment ago.

// The save stops the timer when it is done. A row that arrived meanwhile has
// to get a deadline of its own then, or it waits until something reads it:
// with monitoring paused, for as long as nobody looks.
func TestRoundHandedOverDuringASaveKeepsItsDeadline(t *testing.T) {
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	c := setHandClock(st)
	captureSaveLog(st)
	now := time.Now()
	duringTheFirstSave(st, func() {
		c.step(2 * time.Second)
		mustInsert(t, st, named(round(now.Add(30*time.Second), true), "B"))
	}, nil)
	mustInsert(t, st, named(round(now, true), "A"))
	c.step(30 * time.Second)
	h.fireLast()
	got := dump(t, st, `SELECT target FROM samples ORDER BY rowid`)
	if want := []string{"A", "A", "A"}; !reflect.DeepEqual(got, want) || st.BufferedRows() != 3 {
		t.Fatalf("after the save the table holds %v and %d rows wait; want %v and the 3 of the round that arrived during it",
			got, st.BufferedRows(), want)
	}
	// Not the newest timer's delay alone: the round set one for itself when
	// it arrived, and the save stopped that one with its own.
	st.held.mu.Lock()
	due, timer := st.held.due, st.held.timer
	st.held.mu.Unlock()
	if due.IsZero() || timer == nil {
		t.Fatalf("B waits with no deadline set (due %v, timer %v)", due, timer)
	}
	if want := c.now().Add(30 * time.Second); !due.Equal(want) || h.last() != 30*time.Second {
		t.Errorf("B's deadline is %v and its timer runs %v; want %v and 30s, its own interval from when it arrived", due, h.last(), want)
	}
	c.step(30 * time.Second)
	h.fireLast()
	got = dump(t, st, `SELECT target FROM samples ORDER BY rowid`)
	if want := []string{"A", "A", "A", "B", "B", "B"}; !reflect.DeepEqual(got, want) || st.BufferedRows() != 0 {
		t.Errorf("after B's timer the table holds %v and %d rows wait; want %v and 0", got, st.BufferedRows(), want)
	}
}

// A save that fails after a long wait for the writer has the next round
// arrive during it, at the default cadence. Its rows go back in front of that
// round, not behind it.
func TestFailedSaveGoesBackInFrontOfWhatArrivedDuringIt(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	c := setHandClock(st)
	captureSaveLog(st)
	now := time.Now()
	duringTheFirstSave(st, func() {
		c.step(5 * time.Second)
		mustInsert(t, st, named(round(now.Add(5*time.Second), true), "B"))
		mustDNS(t, st, now.Add(5*time.Second), 2.5, true)
	}, errors.New("disk I/O error"))
	mustInsert(t, st, named(round(now, true), "A"))
	mustDNS(t, st, now, 1.5, true)
	c.step(30 * time.Second)
	h.fireLast()
	if st.BufferedRows() != 8 || onDisk(t, st, "samples") != 0 || onDisk(t, st, "dns") != 0 {
		t.Fatalf("after the failed save %d rows wait, with %d samples and %d dns rows on disk; want 8, 0 and 0",
			st.BufferedRows(), onDisk(t, st, "samples"), onDisk(t, st, "dns"))
	}
	if h.last() != saveRetryEvery {
		t.Fatalf("the newest timer runs %v, want the retry %v on: the round that arrived during the save set one for itself", h.last(), saveRetryEvery)
	}
	c.step(saveRetryEvery)
	h.fireLast()
	got := dump(t, st, `SELECT target FROM samples ORDER BY rowid`)
	if want := []string{"A", "A", "A", "B", "B", "B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the retry the table holds %v, want %v", got, want)
	}
	got = dump(t, st, `SELECT latency_ms FROM dns ORDER BY rowid`)
	if want := []string{"1.5", "2.5"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after the retry the dns table holds %v, want %v", got, want)
	}
	if st.BufferedRows() != 0 {
		t.Errorf("%d rows still wait after the retry went through", st.BufferedRows())
	}
}

// And over a full buffer it is the oldest round that goes, not the one that
// arrived last.
func TestFailedSaveOverAFullBufferDropsTheOldestRound(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	captureSaveLog(st)
	const perRound = 64
	const base = 1_700_000_000
	wide := func(i int) []Sample {
		sms := make([]Sample, perRound)
		for j := range sms {
			sms[j] = Sample{TS: time.Unix(base+int64(i), 0), Target: fmt.Sprintf("t%02d", j), Family: "ipv4", LatencyMS: 1, Success: true}
		}
		return sms
	}
	full := maxHeldRows / perRound
	duringTheFirstSave(st, func() { mustInsert(t, st, wide(full)) }, errors.New("disk I/O error"))
	for i := 0; i < full; i++ {
		mustInsert(t, st, wide(i))
	}
	if err := st.SaveBuffered(SaveOrder); err == nil {
		t.Fatal("premise: the save did not fail")
	}
	st.held.mu.Lock()
	rows, rounds := st.held.rows, len(st.held.rounds)
	first := st.held.rounds[0].rows[0].TS.Unix() - base
	last := st.held.rounds[rounds-1].rows[0].TS.Unix() - base
	st.held.mu.Unlock()
	if rows != maxHeldRows || first != 1 || last != int64(full) {
		t.Errorf("the buffer holds %d rows, from round %d to round %d; want %d, from round 1 to round %d: "+
			"round 0 went to make room for the round that arrived during the save", rows, first, last, maxHeldRows, full)
	}
	if got := counter("db.sample_rows_dropped"); got != perRound || st.BufferedRows() != maxHeldRows {
		t.Errorf("db.sample_rows_dropped = %d and %d rows count as waiting, want %d and %d", got, st.BufferedRows(), perRound, maxHeldRows)
	}
}

// Without a logger the lines go where the store's other lines go. With one,
// they reach the log the dashboard shows and nothing is printed twice.
func TestSaveFailuresFallBackToTheStandardLogger(t *testing.T) {
	st := batched(t, 30*time.Second)
	setHandTimer(st)
	failSaves(st, errors.New("disk I/O error"))
	mustInsert(t, st, round(time.Now(), true))
	// Nothing to look at but that it does not panic on a nil logger: the
	// standard logger writes to stderr.
	if err := st.SaveBuffered(SaveOrder); err == nil {
		t.Fatal("a save through a failing store returned no error")
	}
}

// Readers must not hammer a store that cannot be written. Writers that need
// the order try every time.
func TestReadsBackOffAfterAFailedSave(t *testing.T) {
	st := batched(t, 30*time.Second)
	setHandTimer(st)
	c := setHandClock(st)
	captureSaveLog(st)
	attempts, _ := failSaves(st, errors.New("disk I/O error"))
	mustInsert(t, st, round(time.Now(), true))
	if err := st.SaveBuffered(SaveOrder); err == nil {
		t.Fatal("premise: the save did not fail")
	}
	for _, r := range []SaveReason{SaveRead, SaveRequest} {
		if err := st.SaveBuffered(r); !errors.Is(err, errSaveWaiting) {
			t.Errorf("a save for reason %q inside the retry window returned %v, want it to stay away", r, err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("%d attempts after two reads inside the retry window, want the 1 that failed", attempts.Load())
	}
	if err := st.SaveBuffered(SaveOrder); err == nil || attempts.Load() != 2 {
		t.Errorf("a save for the order inside the retry window: err %v, %d attempts; want it to try (2 attempts)", err, attempts.Load())
	}
	c.step(saveRetryEvery + time.Second)
	if err := st.SaveBuffered(SaveRead); err == nil || attempts.Load() != 3 {
		t.Errorf("a read after the retry window: err %v, %d attempts; want it to try (3 attempts)", err, attempts.Load())
	}
}

// The buffer has a size. Past it the oldest rounds go, each of them whole: a
// round missing some of its targets could read as a quorum it never had.
func TestFullBufferDropsOldestWholeRounds(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	log := captureSaveLog(st)
	_, heal := failSaves(st, errors.New("disk I/O error"))
	const perRound = 64
	wide := func(i int) []Sample {
		ts := time.Unix(1_700_000_000+int64(i), 0)
		sms := make([]Sample, perRound)
		for j := range sms {
			sms[j] = Sample{TS: ts, Target: fmt.Sprintf("t%02d", j), Family: "ipv4", LatencyMS: 1, Success: true}
		}
		return sms
	}
	full := maxHeldRows / perRound
	for i := 0; i < full; i++ {
		mustInsert(t, st, wide(i))
	}
	if st.BufferedRows() != maxHeldRows || counter("db.sample_rows_dropped") != 0 {
		t.Fatalf("with the buffer exactly full: %d rows waiting, %d dropped; want %d and 0",
			st.BufferedRows(), counter("db.sample_rows_dropped"), maxHeldRows)
	}
	// A DNS reading is one row and does not fit either.
	mustDNS(t, st, time.Unix(1_700_000_000, 0), 8.5, true)
	mustInsert(t, st, wide(full))
	mustInsert(t, st, wide(full+1))
	st.held.mu.Lock()
	rows, rounds, dns := st.held.rows, len(st.held.rounds), len(st.held.dns)
	first := st.held.rounds[0].rows[0].TS.Unix() - 1_700_000_000
	whole := true
	for _, r := range st.held.rounds {
		whole = whole && len(r.rows) == perRound
	}
	st.held.mu.Unlock()
	if rows > maxHeldRows {
		t.Errorf("the buffer holds %d rows, over its size of %d", rows, maxHeldRows)
	}
	if !whole {
		t.Error("a round in the buffer has lost some of its rows")
	}
	if first != 3 || rounds != full-1 || dns != 1 {
		t.Errorf("the buffer holds %d rounds from round %d on and %d dns rows; want %d rounds from round 3 on and the 1 dns row: "+
			"three rounds went to make room for a DNS reading and two rounds", rounds, first, dns, full-1)
	}
	if got := counter("db.sample_rows_dropped"); got != 3*perRound {
		t.Errorf("db.sample_rows_dropped = %d, want %d", got, 3*perRound)
	}
	// What was dropped waits no longer. Left counted, the short way out that
	// every reader takes when nothing waits would be closed for good.
	if got, gauge := st.BufferedRows(), stats.Lifetime().Gauges["db.sample_rows_waiting"]; got != rows || gauge != int64(rows) {
		t.Errorf("%d rows counted as waiting and a gauge of %d, with %d in the buffer", got, gauge, rows)
	}
	// While the buffer is full every round drops one. The log says it once.
	said := func() (n int) {
		for _, l := range log.all() {
			if strings.HasPrefix(l, "ERROR dropping unsaved probe readings") {
				n++
			}
		}
		return n
	}
	if said() != 1 {
		t.Errorf("%d lines about dropped readings for three drops in one streak, want 1: %q", said(), log.all())
	}
	// A save that goes through ends the streak, and the next one is said again.
	heal()
	if err := st.SaveBuffered(SaveOrder); err != nil {
		t.Fatalf("save: %v", err)
	}
	failSaves(st, errors.New("disk I/O error"))
	for i := 0; i <= full; i++ {
		mustInsert(t, st, wide(full+2+i))
	}
	if said() != 2 {
		t.Errorf("%d lines about dropped readings after a second streak, want 2: %q", said(), log.all())
	}
	if got := counter("db.sample_rows_dropped"); got != 4*perRound {
		t.Errorf("db.sample_rows_dropped = %d after the second streak, want %d", got, 4*perRound)
	}
}

// A full buffer on a healthy store is a reason to save, not to drop.
func TestCapTriggersASave(t *testing.T) {
	stats.ResetForTest()
	st := batched(t, MaxSaveEvery)
	setHandTimer(st)
	sms := make([]Sample, 64)
	for j := range sms {
		sms[j] = Sample{TS: time.Unix(1_700_000_000, 0), Target: fmt.Sprintf("t%02d", j), Family: "ipv4", LatencyMS: 1, Success: true}
	}
	rounds := maxHeldRows/len(sms) + 1
	for i := 0; i < rounds; i++ {
		mustInsert(t, st, sms)
	}
	if got := counter("db.sample_saves.cap"); got != 1 {
		t.Errorf("db.sample_saves.cap = %d, want 1", got)
	}
	if got := counter("db.sample_rows_dropped"); got != 0 {
		t.Errorf("%d rows were dropped on a store that can be written", got)
	}
	if disk, wait := onDisk(t, st, "samples"), st.BufferedRows(); disk != maxHeldRows || wait != len(sms) {
		t.Errorf("%d rows on disk and %d waiting, want %d and %d", disk, wait, maxHeldRows, len(sms))
	}
	// A round bigger than the buffer is written by itself, behind what waited.
	huge := make([]Sample, maxHeldRows+1)
	for j := range huge {
		huge[j] = Sample{TS: time.Unix(1_700_000_100, 0), Target: "huge", Family: "ipv4", LatencyMS: 1, Success: true}
	}
	mustInsert(t, st, huge)
	if disk, wait := onDisk(t, st, "samples"), st.BufferedRows(); disk != int64(2*maxHeldRows+len(sms)+1) || wait != 0 {
		t.Errorf("after a round of %d rows: %d on disk and %d waiting, want %d and 0", len(huge), disk, wait, 2*maxHeldRows+len(sms)+1)
	}
	var lastSmall, firstHuge int64
	if err := st.db.QueryRow(`SELECT MAX(rowid) FROM samples WHERE target <> 'huge'`).Scan(&lastSmall); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow(`SELECT MIN(rowid) FROM samples WHERE target = 'huge'`).Scan(&firstHuge); err != nil {
		t.Fatal(err)
	}
	if firstHuge < lastSmall {
		t.Errorf("the big round was stored from row %d, ahead of a waiting round's row %d", firstHuge, lastSmall)
	}
}

// A store that cannot be written is tried again every saveRetryEvery, by the
// timer. A full buffer must not change that. Every round met it, and started
// a save that put all 8,192 rows through the store again before it failed:
// on the goroutine that paces the rounds, and twice a round where the DNS
// reading met a buffer that was full to the row.
func TestFullBufferLeavesTheRetryToTheTimer(t *testing.T) {
	// With 7 targets and a DNS reading a round the buffer fills exactly.
	for _, targets := range []int{6, 7} {
		t.Run(fmt.Sprintf("%d targets", targets), func(t *testing.T) {
			stats.ResetForTest()
			st := batched(t, MaxSaveEvery)
			h := setHandTimer(st)
			c := setHandClock(st)
			captureSaveLog(st)
			attempts, _ := failSaves(st, errors.New("disk I/O error"))
			handed := 0
			take := func(i int) {
				ts := time.Unix(1_700_000_000+int64(i), 0)
				sms := make([]Sample, targets)
				for j := range sms {
					sms[j] = Sample{TS: ts, Target: fmt.Sprintf("t%02d", j), Family: "ipv4", LatencyMS: 1, Success: true}
				}
				c.step(time.Millisecond)
				mustInsert(t, st, sms)
				c.step(time.Millisecond)
				mustDNS(t, st, ts, 8.5, true)
				handed += targets + 1
			}
			fit := maxHeldRows / (targets + 1)
			for i := 0; i < fit; i++ {
				take(i)
			}
			if attempts.Load() != 0 || st.BufferedRows() != handed {
				t.Fatalf("premise: with %d rounds handed over, %d saves were tried and %d rows wait; want 0 and %d",
					fit, attempts.Load(), st.BufferedRows(), handed)
			}
			// The round that does not fit starts a save, as on a store that
			// can be written. It fails, and the timer is set for the retry.
			take(fit)
			if got := attempts.Load(); got != 1 {
				t.Fatalf("the first round that did not fit tried %d saves, want 1", got)
			}
			armed := h.armed()
			if h.last() != saveRetryEvery {
				t.Fatalf("after the failed save the newest timer runs %v, want the retry %v on", h.last(), saveRetryEvery)
			}
			for i := 1; i <= 10; i++ {
				take(fit + i)
			}
			if got := attempts.Load(); got != 1 {
				t.Errorf("ten more rounds inside %v of the failed save tried %d saves, want none: the retry is the timer's", saveRetryEvery, got-1)
			}
			if h.armed() != armed {
				t.Errorf("the rounds set %d timers of their own, want the retry left as it was", h.armed()-armed)
			}
			st.held.mu.Lock()
			rows, whole := st.held.rows, true
			for _, r := range st.held.rounds {
				whole = whole && len(r.rows) == targets
			}
			st.held.mu.Unlock()
			if rows > maxHeldRows || !whole {
				t.Errorf("the buffer holds %d rows of %d at most, every round whole: %v", rows, maxHeldRows, whole)
			}
			// Nothing was stored, so what was handed over waits or was dropped.
			dropped := counter("db.sample_rows_dropped")
			if dropped == 0 || int64(st.BufferedRows())+dropped != int64(handed) || st.BufferedRows() != rows {
				t.Errorf("%d rows handed over: %d count as waiting, %d are in the buffer and %d were dropped",
					handed, st.BufferedRows(), rows, dropped)
			}
			if got := counter("db.sample_save_failed"); got != 1 {
				t.Errorf("db.sample_save_failed = %d, want 1", got)
			}
			// Once the retry is due a round tries again, and once only.
			c.step(saveRetryEvery)
			take(fit + 11)
			if got := attempts.Load(); got != 2 {
				t.Errorf("a round %v after the failed save brought the saves tried to %d, want 2", saveRetryEvery, got)
			}
		})
	}
}

// A clean stop loses nothing.
func TestCloseSavesWhatIsWaiting(t *testing.T) {
	stats.ResetForTest()
	path := filepath.Join(t.TempDir(), "stop.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := onDisk(t, st, "samples"); n != 0 {
		t.Fatalf("premise: %d rows are on disk before the close", n)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := counter("db.sample_saves.stop"); got != 1 {
		t.Errorf("db.sample_saves.stop = %d, want 1", got)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if s, d := onDisk(t, again, "samples"), onDisk(t, again, "dns"); s != 6 || d != 1 {
		t.Errorf("after a close and a reopen the file holds %d samples and %d dns rows, want 6 and 1", s, d)
	}
	// A second close has nothing to save and must not try.
	st.Close()
	if got := counter("db.sample_saves.stop"); got != 1 {
		t.Errorf("a second close saved again (db.sample_saves.stop = %d)", got)
	}
}

// What the last save cannot write is lost, and the log says how much.
func TestCloseSaysWhatItCouldNotSave(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	h := setHandTimer(st)
	log := captureSaveLog(st)
	failSaves(st, errors.New("database or disk is full"))
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	armed := h.armed()
	st.Close()
	if st.BufferedRows() != 0 {
		t.Errorf("%d rows are still counted as waiting in a closed store", st.BufferedRows())
	}
	if got := counter("db.sample_rows_dropped"); got != 7 {
		t.Errorf("db.sample_rows_dropped = %d, want the 7 rows that were lost", got)
	}
	if h.armed() != armed {
		t.Errorf("the failed last save set a timer to try again, on a store that is closed")
	}
	lines := log.all()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "ERROR probe readings could not be saved at shutdown and are lost") {
		t.Errorf("the log says %q, want the one line about what was lost", lines)
	}
}

// Close waits FinalSaveBudget for a save that is running, and no longer. The
// rows inside that save are lost as far as the store can tell, and it has to
// say so. They were lost without a line in the log or a count, and stayed
// counted as waiting. The save in front comes back after the close, with an
// error or with its rows written after all, and must leave the books as the
// close left them.
func TestCloseWritesOffASaveThatIsStillRunning(t *testing.T) {
	stats.ResetForTest()
	// Both at once, each on a store of its own: a close waits six seconds.
	t.Run("the save in front", func(t *testing.T) {
		for _, fails := range []bool{true, false} {
			name := "goes through"
			if fails {
				name = "fails"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				st, path := openWALStore(t)
				st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
				h := setHandTimer(st)
				log := captureSaveLog(st)
				started, release := make(chan struct{}), make(chan struct{})
				st.held.mu.Lock()
				st.held.write = func(ctx context.Context, q txBeginner, rounds []heldRound, dns []heldDNS, brief bool) error {
					close(started)
					<-release
					if fails {
						return writeHeld(ctx, q, rounds, dns, brief) // on a handle that is closed by now
					}
					// A transaction that was running when the handle was
					// closed. Closing does not stop it.
					return nil
				}
				st.held.mu.Unlock()
				if err := writeRound(st, time.Now()); err != nil {
					t.Fatal(err)
				}
				front := make(chan error, 1)
				go func() { front <- st.SaveBuffered(SaveOrder) }()
				<-started
				armed := h.armed()
				t0 := time.Now()
				if err := st.Close(); err != nil {
					t.Errorf("close: %v", err)
				}
				if took := time.Since(t0); took < FinalSaveBudget || took > FinalSaveBudget+2*time.Second {
					t.Errorf("the close took %v behind a save that was running, want the %v it may wait and little more", took, FinalSaveBudget)
				}
				books := func(when string) {
					t.Helper()
					st.held.mu.Lock()
					rows, rounds, dns := st.held.rows, len(st.held.rounds), len(st.held.dns)
					st.held.mu.Unlock()
					if st.BufferedRows() != 0 || rows != 0 || rounds != 0 || dns != 0 {
						t.Errorf("%s %d rows count as waiting, and the buffer holds %d rows in %d rounds and %d dns readings; want nothing in a closed store",
							when, st.BufferedRows(), rows, rounds, dns)
					}
					if h.armed() != armed {
						t.Errorf("%s a timer was set, on a store that is closed", when)
					}
				}
				books("after the close")
				lines := log.all()
				if len(lines) != 1 || !strings.HasPrefix(lines[0], "ERROR probe readings could not be saved at shutdown and are lost rows7") {
					t.Errorf("after the close the log says %q, want the one line about the 7 rows that were lost", lines)
				}
				close(release)
				err := <-front
				if (err != nil) != fails {
					t.Errorf("the save in front returned %v", err)
				}
				books("after the save in front came back")
				lines = log.all()
				if fails && len(lines) != 1 {
					t.Errorf("the save in front failed on the closed store and the log says %q: the loss was said already", lines)
				}
				if !fails && (len(lines) != 2 || !strings.HasPrefix(lines[1], "INFO the probe readings given up at shutdown were saved after all rows7")) {
					t.Errorf("the save in front went through and the log says %q, want a second line that puts the first one right", lines)
				}
				if !fails {
					return
				}
				again, err := Open(path)
				if err != nil {
					t.Fatalf("reopen: %v", err)
				}
				defer again.Close()
				if s, d := onDisk(t, again, "samples"), onDisk(t, again, "dns"); s != 0 || d != 0 {
					t.Errorf("the reopened file holds %d samples and %d dns rows: premise broken, the rows were not lost", s, d)
				}
			})
		}
	})
	if got := counter("db.sample_rows_dropped"); got != 14 {
		t.Errorf("db.sample_rows_dropped = %d, want 14: the 7 rows each of the two closes gave up", got)
	}
	if got := stats.Lifetime().Gauges["db.sample_rows_waiting"]; got != 0 {
		t.Errorf("the gauge db.sample_rows_waiting reads %d with both stores closed", got)
	}
}

// A stop beside another writer waits for it, as any write does. A restore's
// batch or a cleanup chunk on a slow card can hold the database for seconds
// at the moment of a stop. The budget of the last save has to outlast that
// wait, or the readings of a whole save interval are lost at a clean stop.
func TestStopSavesBesideAnotherWriter(t *testing.T) {
	const held = 2 * time.Second
	for _, how := range []string{"SaveStop", "Close"} {
		t.Run(how, func(t *testing.T) {
			stats.ResetForTest()
			st, path := openWALStore(t)
			st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
			log := captureSaveLog(st)
			if err := writeRound(st, time.Now()); err != nil {
				t.Fatal(err)
			}
			if n := onDisk(t, st, "samples"); n != 0 || st.BufferedRows() != 7 {
				t.Fatalf("premise: %d rows are on disk and %d wait before the stop, want 0 and 7", n, st.BufferedRows())
			}
			release := holdTheWriter(t, path)
			timer := time.AfterFunc(held, release)
			t0 := time.Now()
			var err error
			if how == "Close" {
				err = st.Close()
			} else {
				err = st.SaveBuffered(SaveStop)
			}
			took := time.Since(t0)
			// The timer's goroutine is done with the test before the test is.
			timer.Stop()
			release()
			if err != nil {
				t.Errorf("%s beside a writer that held the database for %v: %v", how, held, err)
			}
			if took < held/2 {
				t.Errorf("%s returned after %v: premise broken, the writer was not held", how, took)
			}
			st.Close()
			again, err := Open(path)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			defer again.Close()
			if s, d := onDisk(t, again, "samples"), onDisk(t, again, "dns"); s != 6 || d != 1 {
				t.Errorf("after the stop the file holds %d samples and %d dns rows, want 6 and 1", s, d)
			}
			for k, want := range map[string]int64{"db.sample_saves.stop": 1, "db.sample_save_failed": 0, "db.sample_rows_dropped": 0} {
				if got := counter(k); got != want {
					t.Errorf("%s = %d, want %d", k, got, want)
				}
			}
			if lines := log.all(); len(lines) != 0 {
				t.Errorf("the store reported %q: a save that waited for the writer and got it is not a fault", lines)
			}
		})
	}
}

// The test above holds the writer for two seconds, so it sees a budget below
// that. This holds the rest, up to the wait itself.
func TestSaveBudgetsOutlastTheWaitForTheWriter(t *testing.T) {
	for name, budget := range map[string]time.Duration{"FinalSaveBudget": FinalSaveBudget, "saveTimeout": saveTimeout} {
		if budget <= busyTimeout {
			t.Errorf("%s is %v and a write waits up to %v for the writer (busy_timeout). SQLite does not look at the context while it waits, "+
				"so a save with a smaller budget comes back failed from a wait that ended well: at a stop its readings are lost",
				name, budget, busyTimeout)
		}
	}
}

// After the close an insert fails as it did, and leaves nothing behind.
func TestInsertAfterCloseHoldsNothing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	h := setHandTimer(st)
	st.Close()
	if err := st.InsertSamples(context.Background(), round(time.Now(), true)); err == nil {
		t.Error("a round inserted into a closed store returned no error")
	}
	if err := st.InsertDNS(context.Background(), time.Now(), 8.5, true); err == nil {
		t.Error("a DNS reading inserted into a closed store returned no error")
	}
	if st.BufferedRows() != 0 || h.armed() != 0 {
		t.Errorf("a closed store holds %d rows and set %d timers", st.BufferedRows(), h.armed())
	}
}

// The monitor's context is cancelled at shutdown. A round that arrives with
// it wrote nothing before, and was counted as a database error for a DNS
// reading and not for a round. Both are compared with a store that writes
// every round at once.
func TestCancelledContextHoldsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now()
	type outcome struct {
		samplesFailed, dnsFailed  bool
		errAfterSamples, errAfter int64
	}
	try := func(st *Store) outcome {
		stats.ResetForTest()
		var o outcome
		o.samplesFailed = st.InsertSamples(ctx, round(now, true)) != nil
		o.errAfterSamples = counter("db.err")
		o.dnsFailed = st.InsertDNS(ctx, now, 8.5, true) != nil
		o.errAfter = counter("db.err")
		return o
	}
	direct := open(t)
	held := batched(t, MaxSaveEvery)
	setHandTimer(held)
	want, got := try(direct), try(held)
	if want != (outcome{true, true, 0, 1}) {
		t.Fatalf("premise: a store that writes every round does %+v with a cancelled context", want)
	}
	if got != want {
		t.Errorf("with a cancelled context a store that batches does %+v, one that does not %+v", got, want)
	}
	if held.BufferedRows() != 0 || onDisk(t, held, "samples") != 0 || onDisk(t, held, "dns") != 0 {
		t.Errorf("a cancelled round left %d rows waiting and %d + %d on disk",
			held.BufferedRows(), onDisk(t, held, "samples"), onDisk(t, held, "dns"))
	}
}

// The repair that Open had to put off runs on the first round under a clock
// that can judge. That round is held now, and must run it all the same.
func TestBatchedInsertStillRunsTheArmedRepair(t *testing.T) {
	now := time.Now().Unix()
	st, err := openAt(t.TempDir()+"/lazy.db", 120) // service start before NTP
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	seedLegacyPause(t, st, now, int64(maxPauseDuration-1))
	if !st.pauseRepairArmed() {
		t.Fatal("premise: the repair is not armed after an open under a clock from 1970")
	}
	mustInsert(t, st, round(time.Unix(now, 0), true))
	if st.pauseRepairArmed() {
		t.Error("the repair is still armed after a round that was held")
	}
	if n := pauseCountWhere(t, st, int64(maxPauseDuration-1)); n != 0 {
		t.Errorf("the pause row the repair removes is still there (%d rows)", n)
	}
	if st.BufferedRows() != 3 {
		t.Errorf("%d rows wait, want the round's 3", st.BufferedRows())
	}
}

// The handle is read when the save runs. A test swaps it (countingStore), and
// a save through a kept copy would write to a handle that is closed.
func TestSaveUsesTheCurrentHandle(t *testing.T) {
	for _, reason := range []SaveReason{SaveOrder, SaveRead} {
		t.Run(string(reason), func(t *testing.T) {
			st, c := countingStore(t)
			st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
			setHandTimer(st)
			if err := writeRound(st, time.Now()); err != nil {
				t.Fatal(err)
			}
			c.reset()
			if err := st.SaveBuffered(reason); err != nil {
				t.Fatalf("save: %v", err)
			}
			wrote := 0
			c.mu.Lock()
			for _, q := range c.seen {
				if strings.Contains(q, "INSERT INTO samples") || strings.Contains(q, "INSERT INTO dns") {
					wrote++
				}
			}
			c.mu.Unlock()
			if wrote != 2 {
				t.Errorf("the handle now in the store prepared %d of the save's two statements", wrote)
			}
			if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 6 || d != 1 {
				t.Errorf("the tables hold %d samples and %d dns rows, want 6 and 1", s, d)
			}
		})
	}
}

// A reader can only have read without the waiting rows while saves failed.
// What it memoized then is dropped by the save that lands them.
func TestSaveAfterFailuresDropsTheReadCaches(t *testing.T) {
	st := batched(t, 30*time.Second)
	setHandTimer(st)
	captureSaveLog(st)
	gen := func() uint64 {
		st.recMu.Lock()
		defer st.recMu.Unlock()
		return st.recGen
	}
	mustInsert(t, st, round(time.Now(), true))
	before := gen()
	if err := st.SaveBuffered(SaveAge); err != nil {
		t.Fatal(err)
	}
	if gen() != before {
		t.Error("an ordinary save dropped the read caches: a wide chart would be scanned again after every save")
	}
	_, heal := failSaves(st, errors.New("disk I/O error"))
	mustInsert(t, st, round(time.Now(), true))
	if err := st.SaveBuffered(SaveAge); err == nil {
		t.Fatal("premise: the save did not fail")
	}
	if gen() != before {
		t.Error("a failed save dropped the read caches, and nothing has changed on disk")
	}
	heal()
	if err := st.SaveBuffered(SaveAge); err != nil {
		t.Fatal(err)
	}
	if gen() == before {
		t.Error("the save that ended a streak of failures left the read caches in place: " +
			"an answer memoized without the waiting rows would be served until the next restart")
	}
}

// A panic while a save writes must not lose the rows it had taken, nor leave
// the gate shut behind it.
func TestPanicInASaveKeepsTheRows(t *testing.T) {
	st := batched(t, 30*time.Second)
	h := setHandTimer(st)
	captureSaveLog(st)
	st.held.mu.Lock()
	st.held.write = func(context.Context, txBeginner, []heldRound, []heldDNS, bool) error { panic("boom") }
	st.held.mu.Unlock()
	mustInsert(t, st, round(time.Now(), true))
	h.fireLast() // on the timer's goroutine in the daemon, where a panic ends the process
	if st.BufferedRows() != 3 {
		t.Fatalf("%d rows wait after a save that panicked, want 3", st.BufferedRows())
	}
	st.held.mu.Lock()
	st.held.write = writeHeld
	st.held.mu.Unlock()
	if err := st.SaveBuffered(SaveOrder); err != nil {
		t.Fatalf("the save after the panic: %v", err)
	}
	if n := onDisk(t, st, "samples"); n != 3 {
		t.Errorf("%d rows on disk, want 3", n)
	}
}

// The mix the daemon runs: rounds, the timer, writers that need the order,
// settings saves and readers, on a real file with the pool of four.
func TestBatchedSavesSurviveConcurrentWriters(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return 20 * time.Millisecond })
	var logged atomic.Int64
	st.SetSaveLogFn(func(level slog.Level, msg string, args ...any) {
		if level >= slog.LevelWarn {
			logged.Add(1)
			t.Errorf("the store reported: %s %v", msg, args)
		}
	})
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	const rounds = 150
	var wg sync.WaitGroup
	errc := make(chan error, 8)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				errc <- err
			}
		}()
	}
	run(func() error {
		for i := 0; i < rounds; i++ {
			ts := base.Add(time.Duration(i) * time.Second)
			if err := st.InsertSamples(ctx, round(ts, true)); err != nil {
				return fmt.Errorf("round %d: %w", i, err)
			}
			if err := st.InsertDNS(ctx, ts, 8.5, true); err != nil {
				return fmt.Errorf("dns %d: %w", i, err)
			}
			time.Sleep(time.Millisecond)
		}
		return nil
	})
	run(func() error {
		for i := 0; i < 40; i++ {
			if err := st.SetSettings(ctx, map[string]string{"k": fmt.Sprintf("v%d", i)}); err != nil {
				return fmt.Errorf("settings %d: %w", i, err)
			}
		}
		return nil
	})
	run(func() error {
		for i := 0; i < 20; i++ {
			if err := st.InsertEvent(ctx, base.Add(time.Duration(i)*time.Second), []string{"down", "up"}[i%2], i%2*5, ""); err != nil {
				return fmt.Errorf("event %d: %w", i, err)
			}
			if _, err := st.InsertPause(ctx, base.Add(time.Duration(i)*time.Second), 1); err != nil {
				return fmt.Errorf("pause %d: %w", i, err)
			}
		}
		return nil
	})
	run(func() error {
		for i := 0; i < 40; i++ {
			if _, err := st.LatestPerTarget(ctx, 15*time.Second); err != nil {
				return fmt.Errorf("latest %d: %w", i, err)
			}
			if _, err := st.Series(ctx, base, time.Time{}, 5, nil); err != nil {
				return fmt.Errorf("series %d: %w", i, err)
			}
		}
		return nil
	})
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	if err := st.SaveBuffered(SaveOrder); err != nil {
		t.Fatalf("last save: %v", err)
	}
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 3*rounds || d != rounds {
		t.Errorf("the tables hold %d samples and %d dns rows, want %d and %d", s, d, 3*rounds, rounds)
	}
	for _, table := range []string{"samples", "dns"} {
		var out int64
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table + ` a JOIN ` + table + ` b ON b.rowid = a.rowid + 1 WHERE b.ts < a.ts`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		if out != 0 {
			t.Errorf("%s: %d rows are stored ahead of a row taken before them", table, out)
		}
	}
	for _, k := range []string{"db.err", "db.busy", "db.sample_save_failed", "db.sample_rows_dropped"} {
		if got := counter(k); got != 0 {
			t.Errorf("%s = %d, want 0", k, got)
		}
	}
	if got := counter("db.sample_rows_buffered"); got != 4*rounds {
		t.Errorf("db.sample_rows_buffered = %d, want %d", got, 4*rounds)
	}
}

// holdTheWriter takes the database's one writer on a connection of its own,
// the way a restore's batch or a big delete has it, and returns the function
// that lets it go.
func holdTheWriter(t *testing.T, path string) (release func()) {
	t.Helper()
	ctx := context.Background()
	w, err := secondWriter(t, path).Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		t.Fatalf("take the writer: %v", err)
	}
	if _, err := w.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ('held', '1')`); err != nil {
		t.Fatalf("write inside the transaction: %v", err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			if _, err := w.ExecContext(ctx, `COMMIT`); err != nil {
				t.Errorf("the long write could not commit: %v", err)
			}
			w.Close()
		})
	}
	t.Cleanup(release)
	return release
}

// readerBound is what a read may take beside a long write. The save it makes
// waits readerSaveWait. The rest is room for a slow machine.
const readerBound = 300 * time.Millisecond

// A REQUEST NEVER STALLS BEHIND A LONG WRITE BECAUSE OF A SAVE.
//
// Before rows waited in memory a read beside a held writer waited for
// nothing, and a write for the whole busy_timeout. A read that saves first is
// a write. So a save made for a reader gives up after a tenth of a second,
// the read goes on with what is saved, and the rows keep waiting for a save
// that nothing waits behind.
func TestReaderSaveGivesUpOnAHeldWriter(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	ctx := context.Background()
	if err := writeRound(st, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	log := captureSaveLog(st)
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	release := holdTheWriter(t, path)

	t0 := time.Now()
	err := st.SaveBuffered(SaveRead)
	took := time.Since(t0)
	if !errors.Is(err, errSaveWaiting) {
		t.Fatalf("a save for a reader beside a held writer returned %v, want it to leave the rows waiting", err)
	}
	if took > readerBound {
		t.Errorf("the save for a reader took %v beside a held writer, want under %v", took, readerBound)
	}
	if took < readerSaveWait/2 {
		t.Errorf("the save for a reader gave up after %v: it is meant to wait %v, which is enough for an ordinary write to finish", took, readerSaveWait)
	}
	// The reads of one request come one after another. Only the first waits.
	t0 = time.Now()
	for i := 0; i < 20; i++ {
		if _, err := st.LatestPerTarget(ctx, 15*time.Second); err != nil {
			t.Fatalf("read beside a held writer: %v", err)
		}
	}
	if took := time.Since(t0); took > readerBound {
		t.Errorf("twenty reads took %v after the first save gave up, want under %v for all of them", took, readerBound)
	}
	if st.BufferedRows() != 7 || onDisk(t, st, "samples") != 6 {
		t.Errorf("%d rows wait and %d are on disk, want 7 and the 6 saved before", st.BufferedRows(), onDisk(t, st, "samples"))
	}
	// Giving up is how it is meant to end, so nothing is booked as a fault.
	for k, want := range map[string]int64{"db.sample_save_deferred": 1, "db.sample_save_failed": 0, "db.err": 0, "db.busy": 0} {
		if got := counter(k); got != want {
			t.Errorf("%s = %d, want %d", k, got, want)
		}
	}
	if lines := log.all(); len(lines) != 0 {
		t.Errorf("the store reported %q: a busy database is not a fault", lines)
	}
	// A save that nothing waits behind waits for the writer like any write.
	done := make(chan error, 1)
	go func() { done <- st.SaveBuffered(SaveOrder) }()
	select {
	case err := <-done:
		t.Fatalf("a save for the order returned %v while the writer was held: it has to wait for it", err)
	case <-time.After(3 * readerSaveWait):
	}
	release()
	if err := <-done; err != nil {
		t.Fatalf("the save after the writer was let go: %v", err)
	}
	if s, d := onDisk(t, st, "samples"), onDisk(t, st, "dns"); s != 12 || d != 2 || st.BufferedRows() != 0 {
		t.Errorf("once the writer was free: %d samples, %d dns rows, %d waiting; want 12, 2 and 0", s, d, st.BufferedRows())
	}
	requirePragmas(t, st, "after a save for a reader gave up")
	// And readers save again at once: the save that went through ended the
	// time they were to stay away.
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveBuffered(SaveRequest); err != nil || st.BufferedRows() != 0 {
		t.Errorf("a save for a request with the writer free: %v, %d rows left waiting", err, st.BufferedRows())
	}
	requirePragmas(t, st, "after a save for a reader went through")
}

// The save in front may be the timer's, waiting for the writer for as long
// as any write. A reader does not wait behind it.
func TestReaderSaveDoesNotWaitBehindASaveThatWaits(t *testing.T) {
	stats.ResetForTest()
	st, path := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	h := setHandTimer(st)
	captureSaveLog(st)
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	release := holdTheWriter(t, path)
	fired := make(chan struct{})
	go func() {
		h.fire(0)
		close(fired)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(st.held.gate) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the timer's save never started")
		}
		time.Sleep(time.Millisecond)
	}
	t0 := time.Now()
	err := st.SaveBuffered(SaveRequest)
	if took := time.Since(t0); took > readerBound || !errors.Is(err, errSaveWaiting) {
		t.Errorf("a save for a request behind a save that waits for the writer: %v after %v; want it to give up inside %v", err, took, readerBound)
	}
	if got := counter("db.sample_save_deferred"); got != 1 {
		t.Errorf("db.sample_save_deferred = %d, want 1", got)
	}
	release()
	<-fired
	if s := onDisk(t, st, "samples"); s != 6 || st.BufferedRows() != 0 {
		t.Errorf("after the writer was let go the timer's save left %d rows on disk and %d waiting, want 6 and 0", s, st.BufferedRows())
	}
	if got := counter("db.sample_saves.age"); got != 1 {
		t.Errorf("db.sample_saves.age = %d, want 1", got)
	}
}

// Four wide charts can hold all four connections. That is a busy database
// too, and not a fault.
func TestReaderSaveWithEveryConnectionTaken(t *testing.T) {
	stats.ResetForTest()
	st, _ := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	log := captureSaveLog(st)
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	pooled(t, st, func(i int, c *sql.Conn) {
		if i < 3 {
			return
		}
		t0 := time.Now()
		err := st.SaveBuffered(SaveRead)
		if took := time.Since(t0); took > readerBound || !errors.Is(err, errSaveWaiting) {
			t.Errorf("a save for a reader with every connection taken: %v after %v; want it to give up inside %v", err, took, readerBound)
		}
	})
	if st.BufferedRows() != 7 || counter("db.sample_save_deferred") != 1 || counter("db.sample_save_failed") != 0 || len(log.all()) != 0 {
		t.Errorf("%d rows wait, %d saves were put off, %d failed, the log says %q; want 7, 1, 0 and nothing",
			st.BufferedRows(), counter("db.sample_save_deferred"), counter("db.sample_save_failed"), log.all())
	}
	if err := st.SaveBuffered(SaveOrder); err != nil || onDisk(t, st, "samples") != 6 {
		t.Errorf("the save once the connections were free: %v, %d rows on disk", err, onDisk(t, st, "samples"))
	}
}

// A reader that went on without the waiting rows may have memoized what it
// read. The save that lands them drops it.
func TestReaderThatMissedRowsDropsTheCachesWhenTheyLand(t *testing.T) {
	st, path := openWALStore(t)
	st.SetSaveEveryFn(func() time.Duration { return MaxSaveEvery })
	setHandTimer(st)
	gen := func() uint64 {
		st.recMu.Lock()
		defer st.recMu.Unlock()
		return st.recGen
	}
	if err := writeRound(st, time.Now()); err != nil {
		t.Fatal(err)
	}
	release := holdTheWriter(t, path)
	before := gen()
	if err := st.SaveBuffered(SaveRead); !errors.Is(err, errSaveWaiting) {
		t.Fatalf("premise: the save for a reader returned %v beside a held writer", err)
	}
	release()
	if err := st.SaveBuffered(SaveOrder); err != nil {
		t.Fatal(err)
	}
	if gen() == before {
		t.Error("the rows a reader went without are on disk now, and what it memoized without them is still served")
	}
}

// samplesSQL matches a statement that names one of the two tables. dynamicSQL
// matches one that builds its table name from a variable, which the first
// cannot see. namedSQL matches the chart's two statements, which are kept
// outside the function that runs them.
var (
	samplesSQL = regexp.MustCompile(`(?i)(FROM|INTO|UPDATE)\s+(samples|dns)\b`)
	dynamicSQL = regexp.MustCompile("(FROM|INTO) `\\s*\\+")
	namedSQL   = regexp.MustCompile(`\bseriesDNSSQL\b|\bseriesSamplesSQL\(`)
	// firstDBUse is where a function first reaches the database, itself or
	// through a helper that reads for it.
	firstDBUse = regexp.MustCompile(`s\.db\.|s\.BeginReadSnapshot\(|s\.resolveDanglingDowns\(|s\.pruneChunked\(|exportRows\(`)
	funcDecl   = regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?([A-Za-z0-9_]+)\(`)
	funcEnd    = regexp.MustCompile(`(?m)^\}$`)
)

// storeFuncs cuts store.go into its functions, comments stripped, the way
// TestSpeedFilterCoversEveryMeasurementRead does and for its reasons: a body
// ends at its closing brace, not where the next function starts, and a
// comment that quotes a call must not answer for code that has none.
func storeFuncs(t *testing.T) (names []string, bodies map[string]string) {
	t.Helper()
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	bodies = map[string]string{}
	locs := funcDecl.FindAllStringSubmatchIndex(string(src), -1)
	for i, loc := range locs {
		name := string(src[loc[2]:loc[3]])
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if c := funcEnd.FindStringIndex(string(src[loc[0]:end])); c != nil {
			end = loc[0] + c[1]
		}
		names = append(names, name)
		bodies[name] = stripLineComments(string(src[loc[0]:end]))
	}
	return names, bodies
}

// savesBeforeItReads reports whether a function calls SaveBuffered, and does
// so before it first reaches the database.
func savesBeforeItReads(body string) bool {
	save := strings.Index(body, "s.SaveBuffered(")
	if save < 0 {
		return false
	}
	use := firstDBUse.FindStringIndex(body)
	return use == nil || save < use[0]
}

// The list of readers is only worth something if it is the whole list. This
// reads store.go and requires every function that runs SQL against samples or
// dns to save first, or to be listed below with the reason it must not.
//
// It reads store.go only, which is the package's one file that is not a test.
// A reader added to another file of the package would go unchecked.
func TestEverySamplesStatementSavesFirst(t *testing.T) {
	exempt := map[string]string{
		"repairInsanePausesAt":       "runs inside Open, before there is a store to hold rows (it names its table by variable, and the table is never one of the two)",
		"repairUnreadableIntColumns": "runs inside Open, before there is a store to hold rows",
		"InsertSamples":              "the writer: what it does not hold it writes itself, after holdRound has said so",
		"InsertDNS":                  "the writer, as InsertSamples",
		"writeHeld":                  "the save itself",
		"exportRows":                 "has no store, only a handle or a snapshot: ExportTableRows and BeginReadSnapshot save for it",
		"seriesSamplesSQL":           "builds the statement and runs nothing: seriesQuery saves",
		"pruneDueCount":              "builds the statement and runs nothing: PruneDue saves",
		"readDNS":                    "reads inside the snapshot seriesQuery opened, where a save would wait for the connection the snapshot holds: its caller saves",
	}
	// Functions that name neither table and reach them through a helper.
	through := map[string]string{
		"ExportTableRows":   "exportRows reads for it",
		"BeginReadSnapshot": "everything read inside the snapshot, an export's tables and a chart's two scans",
	}
	names, bodies := storeFuncs(t)
	seen := map[string]bool{}
	for _, name := range names {
		body := bodies[name]
		_, listed := through[name]
		if !listed && !samplesSQL.MatchString(body) && !dynamicSQL.MatchString(body) && !namedSQL.MatchString(body) {
			continue
		}
		seen[name] = true
		saves := savesBeforeItReads(body)
		why, isExempt := exempt[name]
		switch {
		case isExempt && strings.Contains(body, "s.SaveBuffered("):
			t.Errorf("%s calls SaveBuffered but is listed as a function that must not (%s)", name, why)
		case !isExempt && !saves:
			t.Errorf("%s runs SQL against samples or dns, or against a table it names by variable, and does not call "+
				"s.SaveBuffered before it first reaches the database. Rows that wait in memory are invisible to SQL, "+
				"so it would read or delete around them. Add the call, or add the function to `exempt` with the reason.", name)
		}
	}
	for name := range exempt {
		if !seen[name] {
			t.Errorf("exempt lists %q, which no longer runs SQL against samples or dns", name)
		}
	}
	for name := range through {
		if !seen[name] {
			t.Errorf("through lists %q, which is gone from store.go", name)
		}
	}
	// The scan found the real readers and did not pass on an empty match.
	for _, must := range []string{"LastObservedTS", "monitoringSince", "HasHistory", "firstQuorumRecovery",
		"newestSampleAt", "LatestPerTarget", "seriesQuery", "TableCounts", "Prune", "Clear", "ImportTableBatch"} {
		if !seen[must] {
			t.Errorf("the source scan did not find %s - it is no longer checking anything", must)
		}
	}
}

// The same for the rows that are ordered against the two tables: an event, a
// pause or a speed row stored ahead of the rounds taken before it.
func TestEveryOrderedWriterSavesFirst(t *testing.T) {
	ordered := regexp.MustCompile(`(?i)INSERT(\s+OR\s+\w+)?\s+INTO\s+(events|pauses|speed|speed_spans|speed_servers)\b`)
	exempt := map[string]string{
		"repairFutureReachingPausesAt": "moves pause rows that exist already between the table and its quarantine, and records nothing new. It runs inside a write too, where a save would wait for itself",
	}
	names, bodies := storeFuncs(t)
	seen := map[string]bool{}
	for _, name := range names {
		body := bodies[name]
		if !ordered.MatchString(body) {
			continue
		}
		seen[name] = true
		why, isExempt := exempt[name]
		switch {
		case isExempt && strings.Contains(body, "s.SaveBuffered("):
			t.Errorf("%s calls SaveBuffered but is listed as a function that must not (%s)", name, why)
		case !isExempt && !savesBeforeItReads(body):
			t.Errorf("%s inserts into a table whose rows are ordered against the probe rounds, and does not call "+
				"s.SaveBuffered before it first reaches the database: its row would be stored ahead of the rounds "+
				"taken before it. Add the call, or add the function to `exempt` with the reason.", name)
		}
	}
	for name := range exempt {
		if !seen[name] {
			t.Errorf("exempt lists %q, which no longer inserts into one of those tables", name)
		}
	}
	for _, must := range []string{"InsertEvent", "InsertPause", "InsertSpeedSpan", "InsertSpeedTS", "InsertSpeedServers"} {
		if !seen[must] {
			t.Errorf("the source scan did not find %s - it is no longer checking anything", must)
		}
	}
}

// Every reason is booked under its own counter, and the list the daemon
// seeds from is the list the store books from.
func TestSaveReasonsAreTheOnesBooked(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := regexp.MustCompile(`(?m)^\tSave[A-Za-z]+\s+SaveReason = "([a-z]+)"`).FindAllStringSubmatch(string(src), -1)
	var want []string
	for _, m := range declared {
		want = append(want, m[1])
	}
	var got []string
	for _, r := range SaveReasons() {
		got = append(got, string(r))
	}
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("SaveReasons() is %v, the reasons declared in store.go are %v", got, want)
	}
}
