package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// The settle-completing Prune arms the deferred pause re-judgement and then, in
// the SAME call, sweeps future-reaching pause rows with a hard DELETE. If the
// sweep runs first, the rows the judgement would have moved to pauses_quarantine
// (restorable when the clock is corrected) are gone for good. Prune must run the
// armed judgement BEFORE the sweep. This drives the ordering directly: a
// future-starting pause row, the repair armed, a trusted (non-stepping) Prune -
// the row must land in quarantine, not vanish.
func TestPruneRunsPauseRepairBeforeDeletingFutureRows(t *testing.T) {
	path := t.TempDir() + "/order.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now()
	// A future-starting pause: ts more than the 48h slack ahead of the prune
	// clock (the shape a backward clock step leaves behind). The prune sweep's
	// `ts > horizon` catches it; the repair's `ts + duration_s > horizon` also
	// catches it - so whichever runs first decides delete-vs-quarantine.
	futureTS := base.Add(72 * time.Hour).Unix()
	seedLegacyPause(t, s, futureTS, 300)

	// Arm the deferred re-judgement, as a settle-completing clockStepped would,
	// and leave pauseStepSeen false so repairReading trusts the fallback clock.
	s.clockMu.Lock()
	s.pauseStepSeen = false
	s.clockMu.Unlock()
	s.pauseRepairArm.Add(1)
	if !s.pauseRepairArmed() {
		t.Fatal("premise: repair did not arm")
	}

	// A trusted clock reading NOW (no step): clockStepped returns false and Prune
	// proceeds to the sweep - after the repair, with the fix.
	clockAt(t, s, base, base, 0)

	// Retention cutoffs in the past so nothing ELSE is deleted and the future
	// row is not caught by the `ts + duration_s < eventsCut` end-of-retention
	// clause - only the `ts > horizon` future clause is in play.
	cut := base.Add(-365 * 24 * time.Hour)
	if _, err := s.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := pauseDurations(t, s, "pauses"); len(got) != 0 {
		t.Errorf("pauses holds %v after prune, want empty (the future row must have MOVED, not stayed)", got)
	}
	if held := pauseDurations(t, s, "pauses_quarantine"); len(held) != 1 || held[0] != 300 {
		t.Errorf("pauses_quarantine holds %v, want [300]: the settle-completing prune deleted the "+
			"future-reaching row before the armed repair could hold it aside", held)
	}
}

// If the deferred repair is armed but has NOT healed (it parked because a clock
// step is still settling, or its transaction failed), Prune must NOT delete the
// future-STARTED pause rows this cycle - deleting them destroys exactly what the
// repair would later quarantine. The retention-floor sweep still runs; the
// future rows wait for a prune where the repair succeeds first.
func TestPruneDefersFutureDeleteWhenRepairUnhealed(t *testing.T) {
	path := t.TempDir() + "/deferred.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Now()
	futureTS := base.Add(72 * time.Hour).Unix()
	seedLegacyPause(t, s, futureTS, 300)

	// Arm the repair and force it to FAIL through the seam: the transaction rolls
	// back (row stays in pauses), the error is swallowed, and the generation
	// stays armed - the unhealed state Prune must not delete into.
	prev := pauseRepairFn
	pauseRepairFn = func(*sql.DB, int64) error { return errors.New("injected repair failure") }
	t.Cleanup(func() { pauseRepairFn = prev })
	s.pauseRepairArm.Add(1)

	clockAt(t, s, base, base, 0)
	cut := base.Add(-365 * 24 * time.Hour)
	if _, err := s.Prune(ctx, cut, cut, cut); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// The future-started row must still be in pauses (deferred), NOT deleted and
	// NOT quarantined (the repair could not run).
	if got := pauseDurations(t, s, "pauses"); len(got) != 1 || got[0] != 300 {
		t.Errorf("pauses holds %v after a prune with an unhealed repair, want [300]: the future row "+
			"was deleted instead of deferred for a later prune", got)
	}
	if held := len(pauseDurations(t, s, "pauses_quarantine")); held != 0 {
		t.Errorf("quarantine holds %d rows, want 0 (the parked repair moved nothing)", held)
	}
}
