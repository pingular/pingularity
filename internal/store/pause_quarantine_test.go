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

// A LONG-RUNNING process must not need a restart to give quarantined history
// back. Open under a plausible-but-stale clock (dead RTC battery, a year
// behind) wrongly holds genuine rows aside; when NTP corrects the clock, the
// pruner's hourly step detector notices - and once the corrected clock
// SURVIVES the settle window (a clock pruning does not trust must not re-judge
// history either), a generation arms and the next write restores what the
// corrected clock exonerates. Before this trigger, restoration waited for the
// next restart: coverage stayed inflated and a backup taken meanwhile
// silently omitted the held rows.
func TestClockStepRearmsTheQuarantineRejudgement(t *testing.T) {
	path := t.TempDir() + "/step.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	seedLegacyPause(t, s, now-3600, 300) // genuine, fully in the past
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	staleNow := now - 365*24*3600
	if staleNow < plausibleEpoch {
		t.Fatalf("premise broken: stale clock %d is pre-epoch", staleNow)
	}
	stats.ResetForTest()
	s2, err := openAt(path, staleNow)
	if err != nil {
		t.Fatalf("open under stale clock: %v", err)
	}
	defer s2.Close()
	if got := len(pauseDurations(t, s2, "pauses")); got != 0 {
		t.Fatalf("premise broken: %d row(s) still live under the stale clock", got)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 1 {
		t.Fatalf("premise broken: quarantine holds %v, want the one genuine row", held)
	}
	if s2.pauseRepairArmed() {
		t.Fatal("premise broken: a plausible open-clock must not arm the re-judgement at Open")
	}

	// NTP steps the clock; the pruner's hourly reading detects it. The step
	// alone must NOT arm the re-judgement - the same reading just declared the
	// clock unsettled for destructive pruning, and re-judging history under a
	// possibly-bogus temporary step is the same hazard.
	stepped := time.Now().Add(2 * pruneClockStepSlack)
	if !s2.clockStepped(stepped, 0) {
		t.Fatal("premise broken: the shifted reading was not detected as a step")
	}
	if s2.pauseRepairArmed() {
		t.Fatal("a just-detected step armed the re-judgement before the clock settled")
	}
	s2.maybeRepairFuturePausesAt(now) // a write lands mid-settle: nothing may move
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 1 {
		t.Fatalf("a write during the settle window moved history: quarantine=%v", held)
	}

	// An hour later (wall and uptime advancing together - no further drift)
	// the settle window is still running: still parked.
	if !s2.clockStepped(stepped.Add(time.Hour), time.Hour) {
		t.Fatal("premise broken: mid-settle reading should still report unsettled")
	}
	if s2.pauseRepairArmed() {
		t.Fatal("armed while the settle window was still running")
	}

	// Past the settle window with the clock steady: the step has earned the
	// re-judgement.
	after := pruneClockSettle + time.Hour
	if s2.clockStepped(stepped.Add(after), after) {
		t.Fatal("premise broken: a settled steady clock still reported stepped")
	}
	if !s2.pauseRepairArmed() {
		t.Fatal("a settled step must arm the pause re-judgement; " +
			"without it, restoration waits for a restart that may be months away")
	}

	// The next write re-judges under the corrected clock and gives the row back.
	s2.maybeRepairFuturePausesAt(now)
	if kept := pauseDurations(t, s2, "pauses"); len(kept) != 1 || kept[0] != 300 {
		t.Errorf("pauses hold %v after the step-triggered re-judgement, want [300]", kept)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 0 {
		t.Errorf("quarantine still holds %v after a corrected clock exonerated it", held)
	}
	if got := stats.Lifetime().Counters["db.pause_rows_restored"]; got != 1 {
		t.Errorf("db.pause_rows_restored = %d, want 1", got)
	}
	if s2.pauseRepairArmed() {
		t.Error("still armed after the re-judgement ran; writes must go back to the bare fast path")
	}
}

// A REVERTING step must not burn the trigger. A bogus temporary step (VM
// snapshot resume, hypervisor glitch) that reverts inside the settle window is
// just another step: the deferral extends, and the single judgement that
// eventually runs uses whatever clock actually survives settling.
func TestRevertingStepKeepsDeferringTheRejudgement(t *testing.T) {
	path := t.TempDir() + "/revert.db"
	s, err := openAt(path, time.Now().Unix()-365*24*3600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	stepped := time.Now().Add(2 * pruneClockStepSlack)
	if !s.clockStepped(stepped, 0) {
		t.Fatal("premise broken: first step not detected")
	}
	// The bogus step reverts an hour in: detected as another step, still parked.
	if !s.clockStepped(time.Now().Add(time.Hour), time.Hour) {
		t.Fatal("premise broken: the revert was not detected as a step")
	}
	if s.pauseRepairArmed() {
		t.Fatal("armed between a step and its revert")
	}
	// The reverted (original) clock settles: one arm, under the surviving clock.
	after := pruneClockSettle + 2*time.Hour
	if s.clockStepped(time.Now().Add(after), after) {
		t.Fatal("premise broken: settled reverted clock still reported stepped")
	}
	if !s.pauseRepairArmed() {
		t.Fatal("the surviving clock settled but nothing armed")
	}
}

// The settle window is an ELIGIBILITY gate for every outstanding generation,
// not only the one a settling step would arm - and once a step has settled,
// repair judges in the vetted frame, ignoring whatever clock the consuming
// write happens to carry. The hazard: an implausible Open arms generation 1;
// a bogus plausible step (say three days backward, past the 48h repair skew)
// is detected and starts settling; the next write would otherwise consume
// generation 1 immediately, judging genuine pauses with the clock the pruner
// itself just refused to trust - quarantining real history.
func TestSettleGatesOutstandingGenerationAndVettedFrameWins(t *testing.T) {
	path := t.TempDir() + "/gate.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Now().Unix()
	seedLegacyPause(t, s, now-3600, 300) // genuine, fully in the past
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The RTC-less boot: implausible clock, judgement skipped, generation 1 armed.
	s2, err := openAt(path, 120)
	if err != nil {
		t.Fatalf("open implausible: %v", err)
	}
	defer s2.Close()
	if !s2.pauseRepairArmed() {
		t.Fatal("premise broken: implausible open did not arm the deferred judgement")
	}
	if got := len(pauseDurations(t, s2, "pauses")); got != 1 {
		t.Fatalf("premise broken: %d live rows, want the 1 unjudged genuine pause", got)
	}

	// A bogus step lands - three days backward - and is detected: settling.
	bogus := time.Now().Add(-3 * 24 * time.Hour)
	if !s2.clockStepped(bogus, 0) {
		t.Fatal("premise broken: bogus step not detected")
	}
	// The write that would have consumed generation 1, carrying the bogus
	// machine clock. It must PARK: nothing judged, generation kept.
	s2.maybeRepairFuturePausesAt(bogus.Unix())
	if got := len(pauseDurations(t, s2, "pauses_quarantine")); got != 0 {
		t.Fatal("a settling clock judged history: the genuine pause was quarantined by the bogus reading")
	}
	if !s2.pauseRepairArmed() {
		t.Fatal("the parked write consumed the generation")
	}

	// The clock corrects (another step, back to now's frame) and settles.
	if !s2.clockStepped(time.Now().Add(time.Hour), time.Hour) {
		t.Fatal("premise broken: the correcting step was not detected")
	}
	after := pruneClockSettle + 2*time.Hour
	if s2.clockStepped(time.Now().Add(after), after) {
		t.Fatal("premise broken: settled corrected clock still reported stepped")
	}

	// The consuming write arrives carrying an ABSURD reading (a decade stale).
	// The vetted frame must win: the genuine row is judged with the settled
	// clock and stays live; the stale reading would have quarantined it.
	s2.maybeRepairFuturePausesAt(now - 10*365*24*3600)
	if kept := pauseDurations(t, s2, "pauses"); len(kept) != 1 || kept[0] != 300 {
		t.Errorf("pauses hold %v after the vetted-frame judgement, want [300]", kept)
	}
	if held := pauseDurations(t, s2, "pauses_quarantine"); len(held) != 0 {
		t.Errorf("quarantine holds %v: the write's stale reading was allowed to judge", held)
	}
	if s2.pauseRepairArmed() {
		t.Error("generation not consumed by the vetted-frame judgement")
	}
}
