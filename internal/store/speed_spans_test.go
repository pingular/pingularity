package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// speed_spans is the latency chart's record of when a speedtest was using the
// network. These tests pin the table's reads, its rule, and where it sits in the
// data lifecycle: the latency retention and the latency clear, never the export.

func openSpansFile(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "spans.db"))
	if err != nil {
		t.Fatalf("open file-backed store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func putSpan(t *testing.T, st *Store, ts, dur int64) {
	t.Helper()
	stored, err := st.InsertSpeedSpan(context.Background(), time.Unix(ts, 0), dur)
	if err != nil || !stored {
		t.Fatalf("InsertSpeedSpan(%d, %d) = %v, %v; want stored", ts, dur, stored, err)
	}
}

func spanCount(t *testing.T, st *Store) int64 {
	t.Helper()
	c, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("TableCounts: %v", err)
	}
	return c["speed_spans"]
}

func TestSpeedSpansRoundTripOnAFileStore(t *testing.T) {
	st := openSpansFile(t)
	ctx := context.Background()
	now := time.Now().Unix()
	since := now - 3600
	putSpan(t, st, since-100, 50) // ends before the window: not returned
	putSpan(t, st, since-60, 60)  // ends exactly at since: not returned (half-open)
	putSpan(t, st, since-30, 90)  // straddles since: returned whole
	putSpan(t, st, now-600, 45)   // inside
	putSpan(t, st, now-120, 30)   // inside, near the end
	got, err := st.SpeedSpans(ctx, time.Unix(since, 0), time.Time{}, 0)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	want := []SpeedSpan{{since - 30, since + 60}, {now - 600, now - 555}, {now - 120, now - 90}}
	if len(got) != len(want) {
		t.Fatalf("spans = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// A closed window stops where it says.
	got, err = st.SpeedSpans(ctx, time.Unix(since, 0), time.Unix(now-300, 0), 0)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("closed window: %+v, want the two spans that start before its end", got)
	}
}

func TestSpeedSpansMergeWithinTheBucket(t *testing.T) {
	st := open(t)
	now := time.Now().Unix()
	base := now - 7200
	putSpan(t, st, base, 40)     // [base, base+40)
	putSpan(t, st, base+100, 40) // gap 60 to the first: merges at mergeS 60
	putSpan(t, st, base+201, 40) // gap 61 to the second: stays apart at mergeS 60
	got, err := st.SpeedSpans(context.Background(), time.Unix(base-10, 0), time.Time{}, 60)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	want := []SpeedSpan{{base, base + 140}, {base + 201, base + 241}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("merged = %+v, want %+v", got, want)
	}
	// With no merge width, every span is its own.
	if got, _ := st.SpeedSpans(context.Background(), time.Unix(base-10, 0), time.Time{}, 0); len(got) != 3 {
		t.Errorf("mergeS 0: %+v, want three spans", got)
	}
}

// The endpoint's bound comes from the merge: spans left after it sit more than a
// bucket apart, so a year holds at most about maxSeriesPoints of them. The
// densest year that merges nothing is one-second spans with gaps of a bucket and
// a second, so that is the fill: it puts the answer right at the bound (a looser
// fill would merge into a handful of spans and pass without testing anything).
// Any span added between two of these closes a gap and merges, so no fill can
// answer more.
func TestSpeedSpansBoundedOverAYear(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	const year = 365 * 24 * 3600
	const points = 1500 // web.maxSeriesPoints
	since := now - year
	mergeS := int64(year / points)
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for ts := since; ts+1 <= now; ts += mergeS + 2 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO speed_spans (ts, duration_s) VALUES (?, 1)`, ts); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, err := st.SpeedSpans(ctx, time.Unix(since, 0), time.Time{}, mergeS)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	if len(got) > points+1 {
		t.Errorf("a year answered %d spans, want at most %d", len(got), points+1)
	}
	if len(got) < points-10 {
		t.Errorf("a year answered only %d spans: the fill no longer reaches the bound, so this test proves nothing", len(got))
	}
	// One more span in any gap joins its neighbours instead of adding one.
	putSpan(t, st, since+mergeS/2, 30)
	if again, _ := st.SpeedSpans(ctx, time.Unix(since, 0), time.Time{}, mergeS); len(again) >= len(got) {
		t.Errorf("a span added inside a gap raised the answer from %d to %d", len(got), len(again))
	}
}

func TestInsertSpeedSpanHoldsItsRule(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	for _, c := range []struct {
		name  string
		start time.Time
		dur   int64
		want  bool
	}{
		{"a start before the project existed", time.Unix(plausibleEpoch-10, 0), 30, false},
		{"no length", now.Add(-time.Hour), 0, false},
		{"an end past the present", now.Add(-10 * time.Second), 3600, false},
		{"an ordinary run", now.Add(-time.Minute), 45, true},
	} {
		stored, err := st.InsertSpeedSpan(ctx, c.start, c.dur)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if stored != c.want {
			t.Errorf("%s: stored = %v, want %v", c.name, stored, c.want)
		}
	}
	// An absurd monotonic duration is clamped, not refused: the run did happen.
	start := now.Add(-3 * time.Hour)
	if stored, err := st.InsertSpeedSpan(ctx, start, 99999); err != nil || !stored {
		t.Fatalf("over-long span: stored=%v err=%v, want it clamped and stored", stored, err)
	}
	got, _ := st.SpeedSpans(ctx, start.Add(-time.Minute), start.Add(time.Minute), 0)
	if len(got) != 1 || got[0].End-got[0].Start != maxSpeedSpanS {
		t.Errorf("over-long span read back as %+v, want %d seconds", got, maxSpeedSpanS)
	}
}

// The future allowance is the pause rule's, on purpose: both tables hold wall
// time already gone by, so they answer to the same idea of how far a clock may
// honestly disagree.
func TestSpeedSpanSaneSharesThePauseSkew(t *testing.T) {
	now := time.Now().Unix()
	edge := now + pauseFutureSkew
	if !speedSpanSane(edge-60, 60, now) {
		t.Error("a span ending exactly at the pause skew was refused")
	}
	if speedSpanSane(edge-59, 60, now) {
		t.Error("a span ending past the pause skew was stored")
	}
	if got, want := speedSpanSane(edge-59, 60, now), pauseSpanImportable(edge-59, 60, now); got != want {
		t.Errorf("speed span and pause disagree about an end one second past the skew: %v vs %v", got, want)
	}
	if speedSpanSane(now-7200, maxSpeedSpanS+1, now) {
		t.Error("a span longer than an hour passed the rule")
	}
}

func TestPruneCutsSpeedSpansOnTheLatencyRetention(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	day := int64(24 * 3600)
	n := now.Unix()
	putSpan(t, st, n-40*day, 60)    // older than the 30-day latency window: goes
	putSpan(t, st, n-30*day-30, 60) // starts before the cutoff, ends after it: kept whole
	putSpan(t, st, n-5*day, 60)     // inside: kept, even though speed retention below would cut it
	if _, err := st.db.ExecContext(ctx, `INSERT INTO speed_spans (ts, duration_s) VALUES (?, ?)`, n+3*day, 60); err != nil {
		t.Fatalf("future row: %v", err) // past Prune's future slack, as a clock that ran fast might have written it
	}
	// Speed history kept for a single day: a span that followed it would lose the 5-day one.
	if _, err := st.Prune(ctx, now.Add(-30*24*time.Hour), now.Add(-24*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got, err := st.SpeedSpans(ctx, now.Add(-60*24*time.Hour), time.Unix(n+10*day, 0), 0)
	if err != nil {
		t.Fatalf("SpeedSpans: %v", err)
	}
	if len(got) != 2 || got[0].Start != n-30*day-30 || got[1].Start != n-5*day {
		t.Fatalf("after prune: %+v, want the straddling span and the 5-day one", got)
	}
}

func TestClearLatencyTakesTheSpans(t *testing.T) {
	st := openSpansFile(t)
	ctx := context.Background()
	putSpan(t, st, time.Now().Unix()-600, 40)
	if _, err := st.Clear(ctx, "speed"); err != nil {
		t.Fatalf("Clear speed: %v", err)
	}
	if n := spanCount(t, st); n != 1 {
		t.Fatalf("clearing speed left %d spans, want 1: the speedtest times belong to the latency chart", n)
	}
	if _, err := st.Clear(ctx, "downtime"); err != nil {
		t.Fatalf("Clear downtime: %v", err)
	}
	if n := spanCount(t, st); n != 1 {
		t.Fatalf("clearing downtime left %d spans, want 1", n)
	}
	if _, err := st.Clear(ctx, "latency"); err != nil {
		t.Fatalf("Clear latency: %v", err)
	}
	if n := spanCount(t, st); n != 0 {
		t.Fatalf("clearing latency left %d spans, want 0", n)
	}
}

func TestDeleteSpeedLeavesTheSpans(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	putSpan(t, st, now-100, 40)
	ts, err := st.InsertSpeedTS(ctx, SpeedSample{TS: now - 59, DownMbps: 100, UpMbps: 10})
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if n, err := st.DeleteSpeed(ctx, ts); err != nil || n == 0 {
		t.Fatalf("DeleteSpeed = %d, %v", n, err)
	}
	if n := spanCount(t, st); n != 1 {
		t.Fatalf("deleting the run took its span too (%d left): the link was busy then all the same", n)
	}
}

// Spans only feed the latency chart's hover note and are left out of export,
// like server_health: exporting them would stamp nearly every backup with a
// newer schema, and older releases refuse such a file whole. A restore loses
// the hover notes and nothing else.
func TestSpeedSpansStayOutOfBackups(t *testing.T) {
	if _, ok := exportTables["speed_spans"]; ok {
		t.Fatal("speed_spans is in exportTables; it is meant to stay out of backups")
	}
	ctx := context.Background()
	src, dst := open(t), open(t)
	putSpan(t, src, time.Now().Unix()-600, 40)
	if _, err := src.ExportTable(ctx, "speed_spans"); err == nil {
		t.Error("ExportTable accepted speed_spans")
	}
	if _, err := dst.ImportTable(ctx, "speed_spans", []map[string]any{{"ts": int64(time.Now().Unix() - 600), "duration_s": int64(40)}}); err == nil {
		t.Error("ImportTable accepted speed_spans")
	}
	restoreBackup(t, src, dst)
	if n := spanCount(t, dst); n != 0 {
		t.Errorf("a full restore carried %d spans, want 0", n)
	}
}
