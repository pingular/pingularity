package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The exact interleaving, forced rather than raced for.
//
// An Abort can land after RunOnce claims the single-flight flag but before it
// publishes its cancel. Abort sees a run in progress (so it reports, correctly,
// that one was signalled) but finds no cancel function to call - the run has to
// notice the request itself, via the store/load pairing described above
// RunOnce's claim: Abort stores the target id (abortFor) before looking for the
// cancel, RunOnce checks abortFor after publishing the cancel, so one side
// always sees the other. Break that ordering and the stop is lost - Abort finds
// nothing to cancel AND the run reads a not-yet-stored abortFor - while the
// user was told it worked.
//
// A 60,000-click stress run did not surface this on one machine, which is exactly
// why it is pinned deterministically here instead.
func TestAStopClickInsideTheClaimWindowIsNotLost(t *testing.T) {
	s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))

	var aborted bool
	afterClaimHook = func() {
		afterClaimHook = nil // once, for this run only
		aborted = s.Abort(s.RunID())
	}
	t.Cleanup(func() { afterClaimHook = nil })

	out := goRunOnce(context.Background(), s, "manual")
	select {
	case r := <-out:
		if !aborted {
			t.Fatal("the hook's Abort() reported no run to stop, so this test drove the wrong window")
		}
		if !errors.Is(r.err, ErrAborted) {
			t.Fatalf("run ended %v, want ErrAborted: Abort() returned true - the user was told the "+
				"test was stopping - but the run was never cancelled", r.err)
		}
	case <-time.After(5 * time.Second):
		s.Abort(s.RunID())
		<-out
		t.Fatal("the run never stopped: the stop click was swallowed by the claim window")
	}
}
