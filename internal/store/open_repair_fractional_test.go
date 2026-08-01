package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The at-Open repairs replicate the BOUNDS half of the import door, but the
// door also enforces integer typing: the intCols allowlist plus normVal reject
// a fractional float before it can land in an INTEGER-affinity column. A
// fractional REAL already at rest - raw SQL, or a build predating the
// allowlist - is in range, so both repair predicates passed it, and every read
// that scans the column into an int64 (completedOutagesSince behind the uptime
// figure, pauseSpans behind coverage and the heatmap) then errors permanently:
// the reads 500 on every request, and nothing ever heals the row. The repairs
// must hold the typing half of the rule too.

// seedRawPause writes a pause row with driver-typed values (any), the way raw
// SQL or an older build could: float64 lands as SQLite REAL.
func seedRawPause(t *testing.T, s *Store, ts, dur any) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO pauses (ts, duration_s) VALUES (?, ?)`, ts, dur); err != nil {
		t.Fatalf("seed pause (%v, %v): %v", ts, dur, err)
	}
}

func TestOpenRepairsFractionalRowsThatBreakEveryRead(t *testing.T) {
	path := t.TempDir() + "/frac.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	// Genuine observations, so a broken figure can only come from the residue.
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-6 * 24 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// events: an in-range but FRACTIONAL outage length. The bounds half of the
	// repair (duration_s < 0 OR > ceiling) passes it, yet scanning it into int64
	// errors, so uptime and the heatmap 500 forever.
	seedLegacyEvent(t, s, now.Add(-2*time.Hour).Unix(), "down", nil)
	seedLegacyEvent(t, s, now.Add(-time.Hour).Unix(), "up", 3600.5)
	// A genuine pair whose exact length must survive the wider predicate.
	seedLegacyEvent(t, s, now.Add(-30*24*time.Hour).Unix(), "down", nil)
	seedLegacyEvent(t, s, now.Add(-29*24*time.Hour).Unix(), "up", int64(1800))
	// pauses: fractional duration, fractional ts - both in range, both fatal to
	// pauseSpans' int64 scan - and a genuine row that must be kept.
	seedRawPause(t, s, now.Add(-5*time.Hour).Unix(), 1800.5)
	seedRawPause(t, s, float64(now.Add(-6*time.Hour).Unix())+0.5, int64(600))
	seedRawPause(t, s, now.Add(-4*time.Hour).Unix(), int64(300))
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	// The permanent 500s first: these are the verifier's failing reads.
	if _, err := s2.UptimeSince(ctx, now.Add(-7*24*time.Hour), 0); err != nil {
		t.Fatalf("uptime read still fails after reopen: %v", err)
	}
	if _, err := s2.DowntimeByDay(ctx, now.Add(-7*24*time.Hour), time.UTC); err != nil {
		t.Fatalf("heatmap read still fails after reopen: %v", err)
	}

	// events: the fractional length is stripped to NULL exactly like an
	// out-of-bounds one - the transition kept - and the genuine length intact.
	var events, nulls int
	if err := s2.db.QueryRow(`SELECT COUNT(*), COUNT(*) - COUNT(duration_s) FROM events`).Scan(&events, &nulls); err != nil {
		t.Fatalf("event count: %v", err)
	}
	if events != 4 || nulls != 3 {
		t.Errorf("after reopen: %d event rows with %d NULL durations, want 4 with 3 "+
			"(the fractional length stripped, every transition kept)", events, nulls)
	}
	var keptDur int64
	if err := s2.db.QueryRow(`SELECT duration_s FROM events WHERE duration_s IS NOT NULL`).Scan(&keptDur); err != nil {
		t.Fatalf("kept duration (must scan as int64): %v", err)
	}
	if keptDur != 1800 {
		t.Errorf("surviving duration_s = %d, want 1800: a whole-number length is real and must survive intact", keptDur)
	}
	// pauses: the two fractional rows are deleted like any other insane span; the
	// genuine one survives and scans clean.
	var ts, dur int64
	if err := s2.db.QueryRow(`SELECT ts, duration_s FROM pauses`).Scan(&ts, &dur); err != nil {
		t.Fatalf("surviving pause (must be exactly one, integer-typed): %v", err)
	}
	if ts != now.Add(-4*time.Hour).Unix() || dur != 300 {
		t.Errorf("surviving pause = (%d, %d), want (%d, 300)", ts, dur, now.Add(-4*time.Hour).Unix())
	}
	// Counted like every other repair, so the rewrite is visible on /metrics.
	if got := stats.Lifetime().Counters["db.event_durations_repaired"]; got != 1 {
		t.Errorf("db.event_durations_repaired = %d, want 1", got)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_repaired"]; got != 2 {
		t.Errorf("db.pause_rows_repaired = %d, want 2", got)
	}
}
