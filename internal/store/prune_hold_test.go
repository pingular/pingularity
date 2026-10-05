package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// NO CLEANUP STARTS WHILE A RESTORE IS BRINGING ROWS IN.
//
// A pass that landed in the middle of a restore cut at the windows in force at
// that moment. A backup's own windows arrive with its config, which our exports
// write last, so a backup that keeps history for longer than the install did
// lost the rows it had brought so far, a moment before the window that keeps
// them came into force. A restore now holds the cleanup off (HoldCleanup) from
// before its first row until it has replied: a pass that comes due meanwhile
// is skipped and counted, and the next one removes what it would have.

// skippedForRestore reads how many passes have been skipped for a restore.
func skippedForRestore() int64 { return stats.Lifetime().Counters["db.prune_skipped_restore"] }

// A pass that comes due under a hold deletes nothing, says why, and is counted.
// The pass after the hold is let go removes the rows. Two restores never run
// at once, but the hold counts them anyway, and letting go twice lets go once:
// a second call must not end somebody else's hold.
func TestAPassThatComesDueDuringARestoreIsSkipped(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	clockAt(t, s, now, now, 0)
	sampleAt(t, s, now, 48*3600, "a", "ipv4", true)
	cut := now.Add(-time.Hour)
	before := skippedForRestore()
	passes := stats.Lifetime().Counters["db.prune_count"]

	release := s.HoldCleanup()
	n, err := s.Prune(ctx, cut, cut, cut)
	if !errors.Is(err, ErrCleanupHeld) || n != 0 {
		t.Fatalf("a pass under a hold returned %d rows, %v; want 0 and ErrCleanupHeld", n, err)
	}
	if got := countRows(t, s, `SELECT COUNT(*) FROM samples`); got != 1 {
		t.Errorf("a pass under a hold left %d of 1 samples", got)
	}
	if got := skippedForRestore() - before; got != 1 {
		t.Errorf("db.prune_skipped_restore moved by %d for one skipped pass, want 1", got)
	}
	if got := stats.Lifetime().Counters["db.prune_count"] - passes; got != 0 {
		t.Errorf("db.prune_count moved by %d: a skipped pass is not a finished prune", got)
	}

	other := s.HoldCleanup()
	release()
	release() // a second call lets go of nothing
	if _, err := s.Prune(ctx, cut, cut, cut); !errors.Is(err, ErrCleanupHeld) {
		t.Fatalf("with one of two holds let go, twice, a pass returned %v; want ErrCleanupHeld: the other still holds", err)
	}
	other()
	if n, err := s.Prune(ctx, cut, cut, cut); err != nil || n != 1 {
		t.Fatalf("the pass after the holds were let go returned %d rows, %v; want the 1 row the skipped ones left", n, err)
	}
	if got := skippedForRestore() - before; got != 2 {
		t.Errorf("db.prune_skipped_restore moved by %d over two skipped passes and one that ran, want 2", got)
	}
}

// A pass that was already running when the hold was taken goes on to its end:
// the hold stops a pass from starting, and nothing waits on it. Here the hold
// is taken once the pass's first chunk has gone, and the pass still removes
// every row it set out to.
func TestAPassAlreadyRunningGoesOnUnderAHold(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	clockAt(t, s, now, now, 0)
	for i := 0; i < 5; i++ {
		sampleAt(t, s, now, 48*3600+i, "a", "ipv4", true)
	}
	pruneChunks(t, 2, 0) // three chunks; puts the hook back afterwards
	var release func()
	pruneChunkHook = func(_ string, rows int64, _ time.Duration, _ bool) {
		if rows > 0 && release == nil {
			release = s.HoldCleanup()
		}
	}
	cut := now.Add(-time.Hour)
	n, err := s.Prune(ctx, cut, cut, cut)
	if release == nil {
		t.Fatal("fixture: no chunk deleted a row, so the hold was never taken")
	}
	defer release()
	if err != nil || n != 5 {
		t.Errorf("a pass that was running when the hold was taken returned %d rows, %v; want all 5 and no error", n, err)
	}
}

// A store that is not there holds nothing, like a watch on one.
func TestAHoldOnNoStoreIsSafe(t *testing.T) {
	var s *Store
	s.HoldCleanup()()
}
