package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// The future-end pause repair judged rows against a bare time.Now() and DELETED
// what it disbelieved. Any reading at/after plausibleEpoch was trusted, so a
// batteryless RTC booting in 2024 with a valid 2026 database passed the
// plausibility threshold and permanently destroyed real 2025-2026 pause rows
// before NTP synced - and deleting a pause converts unobserved time into observed
// time, so uptime is inflated with nothing left on disk to correct it. The frozen
// 2023 epoch makes that window wider every year.
//
// A judgement this permanent cannot rest on a clock this weak, so the repair MOVES
// the rows aside instead: a later, corrected clock exonerates whatever it can
// reach, and what no clock ever exonerates stays put forever - which keeps the
// coverage-zeroing hole 052b50a closed without trusting the clock at all.

func pauseDurations(t *testing.T, s *Store, table string) []int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT duration_s FROM ` + table + ` ORDER BY ts`)
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		out = append(out, d)
	}
	return out
}

func TestStaleClockHidesPauseHistoryInsteadOfDestroyingIt(t *testing.T) {
	path := t.TempDir() + "/stale.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	// Two genuine, fully-past pauses and the crafted decade-ahead row the repair
	// exists to remove.
	seedLegacyPause(t, s, now-3600, 300)
	seedLegacyPause(t, s, now-30*24*3600, 3600)
	seedLegacyPause(t, s, now, maxPauseDuration-1)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A dead-battery RTC a year behind: implausible as a reading of NOW, yet well
	// past plausibleEpoch, so the old gate trusted it completely.
	staleNow := now - 365*24*3600
	if staleNow < plausibleEpoch {
		t.Fatalf("premise broken: the stale clock %d is pre-epoch, so the plausibility gate would catch it", staleNow)
	}
	stats.ResetForTest()
	s2, err := openAt(path, staleNow)
	if err != nil {
		t.Fatalf("open under stale clock: %v", err)
	}
	// Under that clock every genuine row looks future, so all three go - but they
	// must go ASIDE, not away.
	if got := len(pauseDurations(t, s2, "pauses")); got != 0 {
		t.Fatalf("premise broken: %d row(s) still in pauses under a year-behind clock", got)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// NTP syncs (or the operator fixes the clock) and the store reopens: the two
	// genuine rows are exonerated, the crafted one is not.
	stats.ResetForTest()
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()
	kept := pauseDurations(t, s3, "pauses")
	if len(kept) != 2 || kept[0] != 3600 || kept[1] != 300 {
		t.Errorf("pauses hold %v after a corrected clock reopens the store, want [3600 300]: real "+
			"unobserved time a stale clock could not judge must come back, or uptime stays inflated forever", kept)
	}
	if held := pauseDurations(t, s3, "pauses_quarantine"); len(held) != 1 || held[0] != maxPauseDuration-1 {
		t.Errorf("quarantine holds %v, want [%d]: the row ending a decade ahead is what no plausible "+
			"clock can ever exonerate, and it must stay out of the coverage math", held, int64(maxPauseDuration-1))
	}
	if got := stats.Lifetime().Counters["db.pause_rows_restored"]; got != 2 {
		t.Errorf("db.pause_rows_restored = %d, want 2: a reopen that changed the uptime denominator must not be silent", got)
	}
	if err := s3.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// And a third open changes nothing: the repair is idempotent, and the crafted
	// row is quarantined forever rather than re-judged into the coverage math.
	s4, err := Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s4.Close()
	if kept := pauseDurations(t, s4, "pauses"); len(kept) != 2 {
		t.Errorf("pauses hold %v on the next open, want the same two rows: restoring must not duplicate "+
			"or re-quarantine what it already judged", kept)
	}
	if held := pauseDurations(t, s4, "pauses_quarantine"); len(held) != 1 {
		t.Errorf("quarantine holds %v on the next open, want the one crafted row", held)
	}
}

// The lazy path (an RTC-less board never opens under a plausible clock) shares the
// same judgement, so it must share the same reversibility - and must not resurrect
// a row the live writer has since re-recorded at the same ts, which is the key the
// table is merged on everywhere else.
func TestDeferredRepairQuarantinesAndMergesOnRestore(t *testing.T) {
	path := t.TempDir() + "/lazy_q.db"
	s, err := openAt(path, 120) // service start before NTP
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	seedLegacyPause(t, s, now, maxPauseDuration-1) // crafted: ends a decade ahead
	seedLegacyPause(t, s, now-3600, 300)           // genuine

	// The first write under a plausible clock runs the same judgement.
	if err := s.InsertSamples(context.Background(), []Sample{
		{TS: time.Unix(now, 0), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true},
	}); err != nil {
		t.Fatalf("insert samples: %v", err)
	}
	if kept := pauseDurations(t, s, "pauses"); len(kept) != 1 || kept[0] != 300 {
		t.Errorf("pauses hold %v after the deferred repair, want [300]", kept)
	}
	if held := pauseDurations(t, s, "pauses_quarantine"); len(held) != 1 || held[0] != maxPauseDuration-1 {
		t.Errorf("quarantine holds %v after the deferred repair, want [%d]", held, int64(maxPauseDuration-1))
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Now the merge case: a genuine row sits in quarantine (a stale clock hid it)
	// while the live monitor re-records a span at the same ts. Restoring must not
	// leave two overlapping spans at one ts, which would double-subtract that time
	// from the observed denominator.
	s2, err := openAt(path, now-365*24*3600)
	if err != nil {
		t.Fatalf("stale open: %v", err)
	}
	if got := len(pauseDurations(t, s2, "pauses")); got != 0 {
		t.Fatalf("premise broken: %d row(s) survived the stale-clock open", got)
	}
	seedLegacyPause(t, s2, now-3600, 420) // the live writer records the span again
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()
	if kept := pauseDurations(t, s3, "pauses"); len(kept) != 1 || kept[0] != 420 {
		t.Errorf("pauses hold %v after restoring onto a re-recorded span, want [420]: pause rows are "+
			"merged on ts everywhere else, and two spans at one ts subtract the same seconds twice", kept)
	}
}

// Clearing the downtime dataset must take the quarantined rows too. They are the
// same record (see dataCategories), and a row left behind is restored by the next
// open under a good clock - handing the operator back unobserved time they
// deliberately deleted.
func TestClearDowntimeDropsQuarantinedPauses(t *testing.T) {
	path := t.TempDir() + "/clearq.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	seedLegacyPause(t, s, now-3600, 300)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := openAt(path, now-365*24*3600) // stale clock: the genuine row goes aside
	if err != nil {
		t.Fatalf("stale open: %v", err)
	}
	if got := len(pauseDurations(t, s2, "pauses_quarantine")); got != 1 {
		t.Fatalf("premise broken: quarantine holds %d row(s) after the stale-clock open, want 1", got)
	}
	ctx := context.Background()
	if _, err := s2.Clear(ctx, "downtime"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	cnt, err := s2.TableCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if cnt["pauses_quarantine"] != 0 {
		t.Errorf("quarantined pause rows after clearing downtime = %d, want 0", cnt["pauses_quarantine"])
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s3.Close()
	if kept := pauseDurations(t, s3, "pauses"); len(kept) != 0 {
		t.Errorf("pauses hold %v after a cleared downtime dataset was reopened; a cleared span must not "+
			"come back out of quarantine", kept)
	}
}
