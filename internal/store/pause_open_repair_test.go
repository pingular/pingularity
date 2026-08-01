package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Before PauseSpanSane, InsertPause validated nothing but a positive duration, so
// an RTC-less board that booted near the epoch persisted spans today's writers
// refuse - and the fix guarded only NEW writes. applySchema migrates columns, not
// data; Prune's straddle rule deliberately keeps any row whose END is still inside
// retention. So the exact rows the guard declares meaningless survive the upgrade
// and keep zeroing coverage for up to a retention year. Open must repair them.

// seedLegacyPause writes a pause row the way an older build did: straight SQL,
// no validation.
func seedLegacyPause(t *testing.T, s *Store, ts, dur int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO pauses (ts, duration_s) VALUES (?, ?)`, ts, dur); err != nil {
		t.Fatalf("seed pause (%d, %d): %v", ts, dur, err)
	}
}

func TestOpenRemovesPauseRowsTodaysWritersRefuse(t *testing.T) {
	path := t.TempDir() + "/repair.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()

	// The rows the old InsertPause really accepted (its only check was dur > 0).
	seedLegacyPause(t, s, 100, now-30*24*3600-100)           // epoch boot: span from 1970 to now-30d
	seedLegacyPause(t, s, plausibleEpoch-1000, 600)          // starts before the project existed
	seedLegacyPause(t, s, now-48*3600, maxPauseDuration+1)   // longer than any history ever held
	keepReal := now - 24*3600                                // yesterday, an hour off: genuine
	seedLegacyPause(t, s, keepReal, 3600)                    //
	seedLegacyPause(t, s, now-7200, int64(maxPauseDuration)) // ends a decade ahead: only a NOW-anchored
	//                                                          rule can say so, and this reopen has one
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	rows, err := s2.db.Query(`SELECT ts FROM pauses ORDER BY ts`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		kept = append(kept, ts)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	// Only the genuine row survives. The future-ending row goes too, because THIS
	// reopen runs under the test machine's plausible clock: the now-anchored
	// criterion applies exactly when a clock exists that can anchor it (and only
	// then - see TestOpenRepairKeepsGenuinePausesUnderAnEpochClock for the
	// RTC-less board, where every real row looks future and the criterion must
	// stay off).
	want := fmt.Sprintf("%v", []int64{keepReal})
	if got := fmt.Sprintf("%v", kept); got != want {
		t.Errorf("pause rows after reopen = %s, want %s: the clock-free insane rows must go, the rest must stay", got, want)
	}
}

// The end-to-end harm: months of genuinely watched history rendering as
// unmonitored because one pre-upgrade row subtracts the whole window.
func TestOpenRepairRestoresPoisonedCoverage(t *testing.T) {
	path := t.TempDir() + "/coverage.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertSamples(ctx, []Sample{
		{TS: now.Add(-6 * 24 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
		{TS: now.Add(-2 * time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	seedLegacyPause(t, s, 100, now.Unix()-3600-100) // 1970 -> an hour ago, all of it "unobserved"
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	o, err := s2.UptimeSince(ctx, now.Add(-7*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Observed < o.Window/2 || !o.Defined() {
		t.Errorf("one pre-upgrade pause row still zeroes coverage after reopen (window=%v observed=%v defined=%v); "+
			"a span the writers refuse must not survive in the database they guard", o.Window, o.Observed, o.Defined())
	}
}
