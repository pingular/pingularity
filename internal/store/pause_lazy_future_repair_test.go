package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// 052b50a re-judges the future-reaching pause row at Open - but only when the
// OPEN-time clock is plausible. The exact hardware the epoch-clock import
// fallback serves - an RTC-less board - always opens the store at service
// start, before NTP syncs, so on that hardware the at-Open re-judgement never
// fired and the crafted decade-ahead row it exists to remove survived every
// boot. These tests pin the lazy half: the first WRITE that observes a
// plausible clock runs the same repair once, and while the clock is still
// implausible nothing is touched (the same trap the at-Open gate respects).

// pauseCountWhere counts pause rows matching a duration, so the tests can tell
// the crafted row from the genuine one without depending on ordering.
func pauseCountWhere(t *testing.T, s *Store, dur int64) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pauses WHERE duration_s = ?`, dur).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// The verifier's recipe on the RTC-less boot order: store opened under an
// epoch clock (so the at-Open repair cannot judge), crafted future-reaching
// row present, then the first probe round runs under the synced clock.
func TestFirstWriteUnderAPlausibleClockRemovesFutureReachingPause(t *testing.T) {
	now := time.Now().Unix()
	craftedTS, craftedDur := now, int64(maxPauseDuration-1)
	// The door really admits this row under an epoch clock (the import fallback).
	if !pauseSpanImportable(craftedTS, craftedDur, 120) {
		t.Fatalf("premise broken: pauseSpanImportable(%d, %d, epoch clock) = false; "+
			"the crafted row is supposed to enter through the epoch-clock fallback", craftedTS, craftedDur)
	}
	s, err := openAt(t.TempDir()+"/lazy.db", 120) // service start before NTP
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seedLegacyPause(t, s, craftedTS, craftedDur)
	seedLegacyPause(t, s, now-3600, 300) // genuine checkpointed pause: must survive
	stats.ResetForTest()

	// First probe round after NTP syncs: InsertSamples runs under this machine's
	// real (plausible) clock and must re-judge the survivors.
	if err := s.InsertSamples(context.Background(), []Sample{
		{TS: time.Unix(now, 0), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("insert samples: %v", err)
	}

	if n := pauseCountWhere(t, s, craftedDur); n != 0 {
		t.Errorf("crafted decade-ahead pause row survived the first write under a plausible clock; "+
			"on RTC-less hardware Open never sees such a clock, so nothing else ever removes it (rows=%d)", n)
	}
	if n := pauseCountWhere(t, s, 300); n != 1 {
		t.Errorf("genuine pause rows kept = %d, want 1: the lazy repair must only remove "+
			"what no plausible writer could have produced", n)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_repaired"]; got != 1 {
		t.Errorf("db.pause_rows_repaired = %d, want 1: the deferred removal must be visible, not silent", got)
	}
	if s.pauseRepairArmed() {
		t.Errorf("repair flag still armed after the judgement ran; later writes must be a bare atomic load")
	}
}

// The other per-round write path arms the same re-judgement: a board whose
// monitoring is paused at boot writes pause checkpoints, not samples.
func TestPauseWriteAlsoTriggersTheDeferredRepair(t *testing.T) {
	now := time.Now().Unix()
	craftedTS, craftedDur := now, int64(maxPauseDuration-1)
	s, err := openAt(t.TempDir()+"/lazypause.db", 120)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seedLegacyPause(t, s, craftedTS, craftedDur)
	stats.ResetForTest()

	stored, err := s.InsertPause(context.Background(), time.Unix(now-600, 0), 60)
	if err != nil || !stored {
		t.Fatalf("insert pause = %v,%v want stored,nil", stored, err)
	}
	if n := pauseCountWhere(t, s, craftedDur); n != 0 {
		t.Errorf("crafted future-reaching row survived an InsertPause under a plausible clock (rows=%d)", n)
	}
	if n := pauseCountWhere(t, s, 60); n != 1 {
		t.Errorf("the pause just written was lost (rows=%d, want 1); the repair must run before, "+
			"and never against, the live write", n)
	}
}

// The epoch-clock trap holds on the lazy path exactly as it does at Open:
// while the clock is still implausible every genuine row looks future, so the
// re-judgement must stay armed and touch nothing.
func TestDeferredRepairStaysOffWhileTheClockIsImplausible(t *testing.T) {
	now := time.Now().Unix()
	craftedTS, craftedDur := now, int64(maxPauseDuration-1)
	s, err := openAt(t.TempDir()+"/notyet.db", 120)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	seedLegacyPause(t, s, craftedTS, craftedDur)

	s.maybeRepairFuturePausesAt(500) // a write lands, clock still pre-epoch
	if n := pauseCountWhere(t, s, craftedDur); n != 1 {
		t.Errorf("a pre-epoch clock judged the future-end rule (rows=%d, want 1); "+
			"a clock that early cannot anchor the judgement", n)
	}
	if !s.pauseRepairArmed() {
		t.Errorf("repair flag disarmed under an implausible clock; the re-judgement would never run")
	}

	// And a store opened under a plausible clock never arms the lazy path at all.
	s2, err := Open(t.TempDir() + "/sane.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s2.Close()
	if s2.pauseRepairArmed() {
		t.Errorf("store opened under a plausible clock armed the lazy repair; Open already judged")
	}
}
