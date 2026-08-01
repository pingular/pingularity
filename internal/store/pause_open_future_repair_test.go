package store

import (
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// pauseSpanImportable falls back to the clock-free bounds when the destination
// clock is implausible (pre-2023), because that clock cannot anchor a judgement.
// The fallback is right - but nothing ever re-judged what it let in: the Open
// repair applied only the clock-free bounds even once a plausible clock WAS
// available, Prune's pause DELETE only removes future-STARTED rows, and
// re-imports merge by ts so a corrected file cannot displace the row. So one
// crafted span (ts=now, duration just under the ceiling: an END a decade ahead)
// imported under an epoch clock was accepted and then kept forever, zeroing
// coverage on every window for ten years. These tests pin the re-judgement: once
// Open runs under a plausible clock, a span reaching further into the future
// than any believable clock disagreement is removed - and everything a
// believable clock could have written survives.

// The verifier's recipe end to end: a row the import door really accepts under
// an epoch clock, persisted, must be GONE after the first reopen under a sane
// clock.
func TestOpenRemovesFutureReachingPauseAcceptedUnderAnEpochClock(t *testing.T) {
	now := time.Now().Unix()
	craftedTS, craftedDur := now, int64(maxPauseDuration-1)
	// First, the door: this is not a hypothetical row - the importer accepts it
	// when the destination clock reads pre-epoch (an RTC-less board before NTP).
	if !pauseSpanImportable(craftedTS, craftedDur, 120) {
		t.Fatalf("premise broken: pauseSpanImportable(%d, %d, epoch clock) = false; "+
			"the crafted row is supposed to enter through the epoch-clock fallback", craftedTS, craftedDur)
	}

	path := t.TempDir() + "/future.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Persist it exactly as the importer's merge would have (raw SQL: the sanity
	// gate already said yes above).
	seedLegacyPause(t, s, craftedTS, craftedDur)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path) // this machine's real clock: plausible
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("pause row ending %d years ahead survived a reopen under a plausible clock; "+
			"nothing else ever re-judges it, so coverage stays zeroed for a decade", craftedDur/(365*24*3600))
	}
	if got := stats.Lifetime().Counters["db.pause_rows_repaired"]; got != 1 {
		t.Errorf("db.pause_rows_repaired = %d, want 1: the removal must be visible, not silent", got)
	}
}

// The epoch-clock trap still holds at REPAIR time: against a pre-epoch clock
// every genuine row looks future, so the future-end criterion must stay off and
// the whole believable history must survive - the same reason the import falls
// back and Prune declines to prune.
func TestOpenRepairKeepsGenuinePausesUnderAnEpochClock(t *testing.T) {
	s := guardStore2(t)
	now := time.Now().Unix()
	seedLegacyPause(t, s, now-3600, 300)        // ordinary checkpointed pause
	seedLegacyPause(t, s, now-30*24*3600, 3600) // a month back
	const bootClock = int64(120)
	if err := repairInsanePausesAt(s.db, bootClock); err != nil {
		t.Fatalf("repair: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("repair under an epoch clock kept %d of 2 genuine rows; a clock that early "+
			"cannot anchor a judgement, and judging with it erases the real history", n)
	}
}

// What a behind-but-plausible destination deliberately accepted must survive the
// clock catching up: the import bounds ends at its own now+skew, so everything
// it let in is PAST once the clock is right - and everything a live writer may
// legitimately persist (an end within pauseFutureSkew of its clock) sits far
// inside the repair's more generous allowance.
func TestOpenKeepsPausesABelievableClockCouldHaveWritten(t *testing.T) {
	path := t.TempDir() + "/behind.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	behindNow := now - 3*3600 // plausible, three hours behind reality
	rows := [][2]int64{
		{now - 4*3600, 1800},                       // checkpointed pause from the backup
		{now - 600, 600 + pauseFutureSkew - 60},    // live write ending just inside its writer's skew
		{now - 24*3600, 24*3600 + pauseFutureSkew}, // ends at the edge of what a live writer may claim
	}
	for _, r := range rows {
		if !pauseSpanImportable(r[0], r[1], behindNow) && !PauseSpanSane(r[0], r[1]) {
			t.Fatalf("premise broken: no writer accepts (%d, %d)", r[0], r[1])
		}
		seedLegacyPause(t, s, r[0], r[1])
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM pauses`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != len(rows) {
		t.Errorf("reopen kept %d of %d rows a believable clock accepted; the repair must only remove "+
			"what no plausible writer could have produced", n, len(rows))
	}
}
