package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// A pause row is the uptime DENOMINATOR: pausedOverlap subtracts it from observed
// time, so a bad one does not merely look wrong, it rewrites how much of a window
// the monitor claims to have watched. Import is the one path where these rows
// arrive from outside this process (a restore, or a hand-edited backup), so it is
// the only place that can keep a landmine out of the table.
//
// Each test below corresponds to a value the importer used to accept.

func guardStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/g.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func pauseRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A JSON string in an integer column passed normVal untouched (it is not a
// float64, so the intCols guard never looked at it) and landed as SQLite TEXT.
// Every later read scans duration_s into an int64, so one such row 500s the
// heatmap and the uptime query for the whole retention window.
func TestImportRejectsNonNumericPauseValues(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	for _, row := range []map[string]any{
		{"ts": float64(1000), "duration_s": "oops"},
		{"ts": "1000", "duration_s": float64(60)},
		{"ts": float64(1000), "duration_s": true},
	} {
		if _, err := s.ImportTable(ctx, "pauses", []map[string]any{row}); err != nil {
			t.Fatalf("import %v: %v", row, err)
		}
	}
	if n := pauseRows(t, s); n != 0 {
		t.Errorf("stored %d pause row(s) with a non-numeric value; want 0", n)
	}
}

// The same bypass reached EVERY integer column of every table, not just pauses -
// pauses simply has a second guard (pauseRowSane) that happens to catch it. These
// tables have no such backstop, so the allowlist is the only thing standing
// between a hand-edited backup and a column that 500s its reader.
func TestImportRejectsNonNumericIntColumnsInEveryTable(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		table string
		row   map[string]any
		count string
	}{
		{"events", map[string]any{"ts": float64(1000), "type": "down", "duration_s": "oops"}, "events"},
		{"samples", map[string]any{"ts": "nope", "target": "1.1.1.1", "success": float64(1), "latency_ms": float64(5)}, "samples"},
		{"samples", map[string]any{"ts": float64(1000), "target": "1.1.1.1", "success": true, "latency_ms": float64(5)}, "samples"},
		{"dns", map[string]any{"ts": float64(1000), "success": "yes", "latency_ms": float64(5)}, "dns"},
		{"speed", map[string]any{"ts": float64(1000), "server": "s", "download_bytes": "lots"}, "speed"},
	} {
		s := guardStore(t)
		if _, err := s.ImportTable(ctx, tc.table, []map[string]any{tc.row}); err != nil {
			t.Fatalf("%s import: %v", tc.table, err)
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + tc.count).Scan(&n); err != nil {
			t.Fatalf("%s count: %v", tc.table, err)
		}
		if n != 0 {
			t.Errorf("%s: stored %d row(s) with a non-numeric integer column %v; want 0", tc.table, n, tc.row)
		}
	}
}

// A pause covers [ts, ts+duration_s). A zero or negative duration is not a span,
// and a negative one makes the interval run backwards - pausedOverlap's
// MIN(end)-MAX(start) then subtracts a nonsense quantity from observed time.
func TestImportRejectsNonPositivePauseDuration(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	for _, d := range []float64{0, -1, -5000} {
		if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
			{"ts": float64(1000), "duration_s": d},
		}); err != nil {
			t.Fatalf("import duration %v: %v", d, err)
		}
	}
	if n := pauseRows(t, s); n != 0 {
		t.Errorf("stored %d pause row(s) with duration <= 0; want 0", n)
	}
}

// ts+duration_s is computed in Go (int64) and in SQL. A row near MaxInt64
// overflows the Go side to a huge NEGATIVE endpoint while SQLite promotes the
// same expression to REAL - so the two disagree about where the pause ends, and
// the Go side's end lands before its own start.
func TestImportRejectsOverflowingPauseEndpoint(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	half := float64(math.MaxInt64 / 2)
	for _, row := range []map[string]any{
		{"ts": half, "duration_s": half},
		{"ts": float64(1000), "duration_s": float64(math.MaxInt64 - 10)},
	} {
		if _, err := s.ImportTable(ctx, "pauses", []map[string]any{row}); err != nil {
			t.Fatalf("import %v: %v", row, err)
		}
	}
	if n := pauseRows(t, s); n != 0 {
		t.Errorf("stored %d pause row(s) whose ts+duration_s overflows; want 0", n)
	}
}

// A well-formed pause must still import, or the guards above are just a break.
func TestImportStillAcceptsAWellFormedPause(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	// A plausible timestamp: pauseRowSane now refuses anything before the project
	// existed (plausibleEpoch), so ts=1000 is 1970 and is correctly rejected. This
	// test is about a WELL-FORMED row importing, not about the epoch floor.
	ts := time.Now().Add(-time.Hour).Unix()
	n, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(ts), "duration_s": float64(60)},
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n != 1 || pauseRows(t, s) != 1 {
		t.Fatalf("a valid pause row did not import: n=%d stored=%d", n, pauseRows(t, s))
	}
	var gotTS, dur int64
	if err := s.db.QueryRow(`SELECT ts, duration_s FROM pauses`).Scan(&gotTS, &dur); err != nil {
		t.Fatalf("scan (the row must be integer-typed): %v", err)
	}
	if gotTS != ts || dur != 60 {
		t.Fatalf("got ts=%d dur=%d; want %d/60", gotTS, dur, ts)
	}
}

// pausedOverlap SUMs the spans it finds. Two pauses that overlap therefore
// subtract their shared seconds twice, which pushes observation coverage above
// the wall time actually paused - and nothing downstream re-derives it. Overlap
// is reachable without a crafted import: a checkpoint flush and a resume flush
// can both cover the same stretch.
func TestPausedOverlapUnionsRatherThanSums(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(base), "duration_s": float64(100)},      // [base, base+100)
		{"ts": float64(base + 50), "duration_s": float64(100)}, // [base+50, base+150)
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := s.pausedOverlap(ctx, base, base+150)
	if err != nil {
		t.Fatalf("pausedOverlap: %v", err)
	}
	if got != 150 {
		t.Errorf("pausedOverlap = %ds over the union [base,base+150); want 150 (sum-of-spans is 200)", got)
	}
}

// Containment is the other shape: a long pause with a short one inside it must
// count once, not twice.
func TestPausedOverlapHandlesContainedSpans(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(base), "duration_s": float64(300)},
		{"ts": float64(base + 100), "duration_s": float64(50)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := s.pausedOverlap(ctx, base, base+300)
	if err != nil {
		t.Fatalf("pausedOverlap: %v", err)
	}
	if got != 300 {
		t.Errorf("pausedOverlap = %ds with a contained span; want 300", got)
	}
}

// Disjoint spans must still add up - the union must not over-merge.
func TestPausedOverlapKeepsDisjointSpansSeparate(t *testing.T) {
	s := guardStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	if _, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(base), "duration_s": float64(60)},
		{"ts": float64(base + 200), "duration_s": float64(60)},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := s.pausedOverlap(ctx, base, base+300)
	if err != nil {
		t.Fatalf("pausedOverlap: %v", err)
	}
	if got != 120 {
		t.Errorf("pausedOverlap = %ds for two disjoint 60s pauses; want 120", got)
	}
}
