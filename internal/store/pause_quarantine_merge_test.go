package store

import (
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The quarantine restore keyed collisions on ts with a bare NOT-EXISTS skip: a held
// row sharing a ts with a live pauses row was NOT re-inserted, then deleted from the
// quarantine anyway. When the held span was the LONGER of the two, that silently
// destroyed the longer span - a 7000s hold behind a 60s live row lost 6940s of
// unobserved time out of the uptime denominator. pauses has no unique constraint on
// ts, so the fix must keep exactly one row per ts holding the LONGER span: insert an
// exonerated held row with no live twin, merge the longer duration into a twin where
// one exists, and only then drop the held copies.

func seedQuarantine(t *testing.T, s *Store, ts, dur int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO pauses_quarantine (ts, duration_s) VALUES (?, ?)`, ts, dur); err != nil {
		t.Fatalf("seed quarantine (%d, %d): %v", ts, dur, err)
	}
}

func TestOpenMergesLongerQuarantinedSpanOntoLiveTwin(t *testing.T) {
	path := t.TempDir() + "/merge.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	ts := now - 7200
	// A long held span waiting in quarantine and a short live span the monitor has
	// since recorded at the same ts - both fully in the past, so a plausible clock
	// exonerates the hold.
	seedQuarantine(t, s, ts, 7000)
	seedLegacyPause(t, s, ts, 60)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := openAt(path, now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	kept := pauseDurations(t, s2, "pauses")
	if len(kept) != 1 || kept[0] != 7000 {
		t.Errorf("pauses hold %v after restoring a longer held span onto a live twin, want [7000]: the "+
			"exonerated 7000s hold must merge into the 60s live row keeping the LONGER span, not be dropped "+
			"by a NOT-EXISTS skip and then deleted (losing 6940s of unobserved time)", kept)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 0 {
		t.Errorf("quarantine holds %v after the merge, want empty: an exonerated held row left behind is "+
			"invisible limbo - not in coverage, not in any export", held)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_merged"]; got != 1 {
		t.Errorf("db.pause_rows_merged = %d, want 1: a merge that changed the uptime denominator must be visible on /metrics", got)
	}

	// Second open changes nothing: the merged span is fully past, so it neither moves
	// out nor is re-restored.
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := openAt(path, now)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s3.Close()
	if kept := pauseDurations(t, s3, "pauses"); len(kept) != 1 || kept[0] != 7000 {
		t.Errorf("second open changed the merged span to %v, want the same [7000]: the restore must be idempotent", kept)
	}
	if held := pauseDurations(t, s3, "pauses_quarantine"); len(held) != 0 {
		t.Errorf("second open re-populated quarantine with %v, want empty", held)
	}
}

// The move-out direction must dedup against the quarantine too: a future-reaching
// pauses row at a ts already held (a re-import, or the live writer re-recording the
// span while the first sits in quarantine) must not pile a second row at that ts on
// every Open, and must keep the longer span if the two disagree.
func TestOpenDeduplicatesQuarantineOnRepeatedMoveOut(t *testing.T) {
	path := t.TempDir() + "/moveout.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	ts := now
	dur := int64(maxPauseDuration - 1) // ends a decade ahead: always future-reaching
	seedLegacyPause(t, s, ts, dur)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// First open holds the future-reaching row aside: one quarantine row, pauses clear.
	s2, err := openAt(path, now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 1 {
		t.Fatalf("first open held %v, want a single row", held)
	}
	// The span reappears in pauses at the SAME ts (a re-import, or the live writer
	// re-recording it) while the first copy sits in quarantine.
	seedLegacyPause(t, s2, ts, dur)
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Second open must not create a second quarantine row at that ts.
	s3, err := openAt(path, now)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s3.Close()
	if held := pauseDurations(t, s3, "pauses_quarantine"); len(held) != 1 || held[0] != dur {
		t.Errorf("quarantine holds %v after a repeated move-out at one ts, want a single [%d]: the move-out "+
			"must dedup against the quarantine so repeated Opens never pile up rows at one ts nor shrink a held span", held, dur)
	}
	if kept := pauseDurations(t, s3, "pauses"); len(kept) != 0 {
		t.Errorf("pauses hold %v after the second move-out, want empty: the future-reaching row belongs in quarantine", kept)
	}
}

// The merge must not land on a twin that is itself future-reaching. Such a twin's
// span is the longer by construction - it ends past the horizon, the exonerated
// held row ends inside it - so max() kept the twin, the held copy was then deleted
// as "represented", and the move-out held the twin aside: the span the clock had
// just vouched for was gone from both tables, and only the row no plausible clock
// will ever exonerate remained. An exonerated held row is inserted BESIDE such a
// twin; the twin alone moves out.
func TestOpenRestoresExoneratedSpanBesideFutureReachingTwin(t *testing.T) {
	path := t.TempDir() + "/twin.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	ts := now - 7200
	future := int64(maxPauseDuration - 1) // a live row at the same second, reaching a decade ahead
	seedQuarantine(t, s, ts, 7000)        // fully past: a plausible clock exonerates it
	seedLegacyPause(t, s, ts, future)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := openAt(path, now)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if kept := pauseDurations(t, s2, "pauses"); len(kept) != 1 || kept[0] != 7000 {
		t.Errorf("pauses hold %v after restoring beside a future-reaching twin, want [7000]: the exonerated "+
			"span must be inserted next to the twin, not merged into it with max() and then lost when the twin moves out", kept)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 1 || held[0] != future {
		t.Errorf("quarantine holds %v, want only the future-reaching twin [%d]", held, future)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_merged"]; got != 0 {
		t.Errorf("db.pause_rows_merged = %d, want 0: a twin no clock can vouch for is not a span to merge into", got)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_restored"]; got != 1 {
		t.Errorf("db.pause_rows_restored = %d, want 1: the exonerated span went back into coverage", got)
	}

	// Second open changes nothing: the restored span is past, the twin stays held.
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := openAt(path, now)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s3.Close()
	if kept := pauseDurations(t, s3, "pauses"); len(kept) != 1 || kept[0] != 7000 {
		t.Errorf("second open changed pauses to %v, want the same [7000]", kept)
	}
	if held := pauseDurations(t, s3, "pauses_quarantine"); len(held) != 1 || held[0] != future {
		t.Errorf("second open changed the quarantine to %v, want the same [%d]", held, future)
	}
}
