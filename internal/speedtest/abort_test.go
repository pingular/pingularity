package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// newAbortScheduler builds a scheduler over an in-memory store the same way
// newRunOnceScheduler does, but takes the tester itself: every case below turns
// on exactly WHEN the tester returns relative to the cancellation, and on which
// of the two contexts (parent or the run's child) was the one cancelled.
func newAbortScheduler(t *testing.T, tester Tester) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

// runResult carries RunOnce's return off the goroutine the run has to live on -
// Abort can only land while the call is parked inside the tester.
type runResult struct {
	sp  store.SpeedSample
	err error
}

// goRunOnce starts a run on its own goroutine and hands back its outcome.
func goRunOnce(ctx context.Context, s *Scheduler, reason string) <-chan runResult {
	ch := make(chan runResult, 1)
	go func() {
		sp, err := s.RunOnce(ctx, reason)
		ch <- runResult{sp, err}
	}()
	return ch
}

// awaitRun collects the run's outcome, failing rather than hanging when the
// cancellation never reached the tester.
func awaitRun(t *testing.T, ch <-chan runResult) runResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce never returned - the cancellation did not reach the tester")
		return runResult{}
	}
}

// waitRunning spins (bounded) until the single-flight flag is up, which is what
// /api/status shows the RUN button. It is NOT sufficient on its own to make an
// Abort land: RunOnce raises the flag several statements before it publishes the
// cancel func, so spinning only on this would let Abort() slip into that window
// and report false at random. Every case here therefore also waits on a signal
// the tester sends from inside runTester - strictly after abortFn.Store.
func waitRunning(t *testing.T, s *Scheduler) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Running() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("RunOnce never reported Running()")
}

// bestSoFar is the result a best-of-N run has already banked when the user hits
// stop: one server was fully measured, the rest never got their turn.
var bestSoFar = Result{
	DownloadMbps: 420, UploadMbps: 42, PingMS: 11,
	Server: "already-measured", ServerID: "7", Engine: "ookla",
	DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
}

// A user Abort() before any server produced a result is a clean stop, not a
// measurement failure: RunOnce reports ErrAborted (which /api/speedtest turns
// into {"aborted":true}) and nothing is written, because there is nothing to
// write.
func TestAbortBeforeAnyResultReturnsErrAbortedAndStoresNothing(t *testing.T) {
	stats.ResetForTest()
	started := make(chan struct{})
	s, st := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done() // no server finished before the stop click
		return Result{}, ctx.Err()
	}))

	out := goRunOnce(context.Background(), s, "manual")
	<-started // inside runTester, so the cancel func is published
	waitRunning(t, s)

	if !s.Abort(s.RunID()) {
		t.Fatal("Abort() reported no run in flight while one was parked in the tester")
	}
	r := awaitRun(t, out)
	if !errors.Is(r.err, ErrAborted) {
		t.Fatalf("RunOnce = %v, want ErrAborted - a user stop must not surface as a measurement failure (502 + a red toast)", r.err)
	}
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("table counts: %v", err)
	}
	if cnt["speed"] != 0 {
		t.Fatalf("aborted run persisted %d speed rows, want 0 - nothing was measured", cnt["speed"])
	}
}

// The mirror image, and the documented half of the contract: a best-of-N run
// that already measured a server KEEPS that (best) result. The Ookla loop
// returns the winner with a nil error even when the abort cancelled a later
// target, so RunOnce must treat it as an ordinary completed run - persisted, no
// error - and /api/speedtest hands the numbers back instead of {"aborted":true}.
// Throwing it away would spend the user's data and then discard the measurement.
func TestAbortAfterAServerSucceededKeepsTheBestResult(t *testing.T) {
	stats.ResetForTest()
	started := make(chan struct{})
	s, st := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done()          // the stop click lands mid best-of-N...
		return bestSoFar, nil // ...after one server already produced numbers
	}))

	out := goRunOnce(context.Background(), s, "manual")
	<-started
	waitRunning(t, s)
	if !s.Abort(s.RunID()) {
		t.Fatal("Abort() reported no run in flight")
	}

	r := awaitRun(t, out)
	if r.err != nil {
		t.Fatalf("RunOnce = %v, want nil - an already-measured server makes this a completed run, not an abort", r.err)
	}
	if r.sp.Server != bestSoFar.Server || r.sp.DownMbps != bestSoFar.DownloadMbps {
		t.Fatalf("returned sample %+v, want the banked best-so-far %+v", r.sp, bestSoFar)
	}
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("table counts: %v", err)
	}
	if cnt["speed"] != 1 {
		t.Fatalf("speed rows = %d, want 1 - the measurement the user already paid for must be kept", cnt["speed"])
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("speed.fail = %d, want 0 - a kept result is not a failure", got)
	}
}

// Abort() with nothing in flight must report false and leave no trace: the RUN
// button's stop click can always race the run ending, and a stale cancel func
// left behind would then kill the NEXT run instead. RunOnce's deferred
// abortFn.Store(nil) is what makes the post-run case safe.
func TestAbortWithNoRunInFlightIsANoOp(t *testing.T) {
	stats.ResetForTest()
	s, st := newAbortScheduler(t, testerFunc(func(context.Context) (Result, error) {
		return Result{DownloadMbps: 100, UploadMbps: 10, PingMS: 5, Server: "S", DownloadBytes: 1, UploadBytes: 1}, nil
	}))

	if s.Abort(s.RunID()) {
		t.Fatal("Abort() claimed to have signalled a run before any had started")
	}
	if s.Running() {
		t.Fatal("a stray Abort() left the single-flight flag set")
	}

	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if s.Abort(s.RunID()) {
		t.Fatal("Abort() after the run finished still found a cancel func - a late stop click would cancel the next run")
	}
	// And that next run must still complete and persist normally.
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("run after a stray Abort: %v", err)
	}
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("table counts: %v", err)
	}
	if cnt["speed"] != 2 {
		t.Fatalf("speed rows = %d, want 2 - both runs must have been recorded", cnt["speed"])
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("speed.fail = %d, want 0", got)
	}
}

// Two stop clicks during the SAME run (a double-click, or the SPA retrying):
// the second must not panic, must still report that a run was signalled, and
// must not change the outcome. The tester holds the run open past the first
// cancellation so both aborts provably land while it is still in flight.
func TestAbortIsIdempotentWithinOneRun(t *testing.T) {
	stats.ResetForTest()
	started := make(chan struct{})
	release := make(chan struct{})
	s, st := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done()
		<-release // hold the run open so BOTH aborts land inside it
		return Result{}, ctx.Err()
	}))

	out := goRunOnce(context.Background(), s, "manual")
	<-started
	waitRunning(t, s)

	if !s.Abort(s.RunID()) {
		t.Fatal("first Abort() found no run in flight")
	}
	if !s.Abort(s.RunID()) {
		t.Fatal("second Abort() reported false while the same run was still in flight")
	}
	close(release)

	r := awaitRun(t, out)
	if !errors.Is(r.err, ErrAborted) {
		t.Fatalf("RunOnce = %v, want ErrAborted - the repeat click must not change the outcome", r.err)
	}
	cnt, err := st.TableCounts(context.Background())
	if err != nil {
		t.Fatalf("table counts: %v", err)
	}
	if cnt["speed"] != 0 {
		t.Fatalf("speed rows = %d, want 0", cnt["speed"])
	}
	// The flag and the cancel func must both be back down, so the next run is
	// unaffected by either click.
	if s.Running() {
		t.Error("the run is still flagged as in flight after it returned")
	}
	if s.Abort(s.RunID()) {
		t.Error("a cancel func survived the aborted run")
	}
}

// The distinction the child context exists for, and the subtlest part of the
// feature: only the TESTER rides runCtx. A USER abort leaves the parent live, so
// everything after runTester (conninfo, the persist gate, InsertSpeed) still
// runs and the best-so-far is stored. A SHUTDOWN cancels the PARENT, and the
// very same result is deliberately dropped - p.run is about to Close the store,
// and the WAL has to stay crash-consistent. Same tester, same result; only who
// cancels differs.
func TestUserAbortPersistsWhileShutdownDoesNot(t *testing.T) {
	// bankedRun returns a scheduler whose tester parks until its context dies and
	// then reports the server it had already measured - the best-of-N shape.
	bankedRun := func(t *testing.T) (*Scheduler, *store.Store, chan struct{}) {
		started := make(chan struct{})
		s, st := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
			close(started)
			<-ctx.Done()
			return bestSoFar, nil
		}))
		return s, st, started
	}

	t.Run("user abort keeps it", func(t *testing.T) {
		stats.ResetForTest()
		s, st, started := bankedRun(t)
		out := goRunOnce(context.Background(), s, "manual")
		<-started
		waitRunning(t, s)
		if !s.Abort(s.RunID()) {
			t.Fatal("Abort() reported no run in flight")
		}

		r := awaitRun(t, out)
		if r.err != nil {
			t.Fatalf("RunOnce = %v, want nil - the parent ctx is still live, so the run completes normally", r.err)
		}
		cnt, err := st.TableCounts(context.Background())
		if err != nil {
			t.Fatalf("table counts: %v", err)
		}
		if cnt["speed"] != 1 {
			t.Fatalf("speed rows = %d, want 1 - a user abort must not discard an already-measured server", cnt["speed"])
		}
	})

	t.Run("shutdown drops it", func(t *testing.T) {
		stats.ResetForTest()
		s, st, started := bankedRun(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := goRunOnce(ctx, s, "manual")
		<-started
		waitRunning(t, s)
		cancel() // the daemon is going down - not a stop click

		r := awaitRun(t, out)
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("RunOnce = %v, want context.Canceled", r.err)
		}
		cnt, err := st.TableCounts(context.Background())
		if err != nil {
			t.Fatalf("table counts: %v", err)
		}
		if cnt["speed"] != 0 {
			t.Fatalf("speed rows = %d, want 0 - a run must not insert into a store the shutdown is about to Close", cnt["speed"])
		}
	})

	t.Run("shutdown before any result is not an abort", func(t *testing.T) {
		// The same cancellation, but nothing was measured. It must NOT be
		// classified as a user abort: /api/speedtest would then answer a
		// tearing-down daemon with a cheerful {"aborted":true}. It must also not
		// count as a measurement failure.
		stats.ResetForTest()
		started := make(chan struct{})
		s, st := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
			close(started)
			<-ctx.Done()
			return Result{}, ctx.Err()
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		out := goRunOnce(ctx, s, "manual")
		<-started
		waitRunning(t, s)
		cancel()

		r := awaitRun(t, out)
		if errors.Is(r.err, ErrAborted) {
			t.Fatal("a shutdown was reported as a user abort - the parent-ctx half of the guard is gone")
		}
		if r.err == nil {
			t.Fatal("a shutdown mid-run must still report an error")
		}
		cnt, err := st.TableCounts(context.Background())
		if err != nil {
			t.Fatalf("table counts: %v", err)
		}
		if cnt["speed"] != 0 {
			t.Fatalf("speed rows = %d, want 0", cnt["speed"])
		}
		if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
			t.Errorf("speed.fail = %d, want 0 - a shutdown is not a measurement failure either", got)
		}
	})
}

// The metrics contract. speed.fail / speed.fail.<stage> feed the fleet failure
// rate and its stage histogram; a user-initiated stop is not a failure of the
// link or of any server, so neither may move. The run is still counted as an
// ATTEMPT (speed.run.<reason>, speed.duration_n) - it really did start and spend
// time, and hiding that would understate how much the RUN button is used.
func TestAbortDoesNotCountAsASpeedFailure(t *testing.T) {
	stats.ResetForTest()
	started := make(chan struct{})
	s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		close(started)
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))

	out := goRunOnce(context.Background(), s, "manual")
	<-started
	waitRunning(t, s)
	if !s.Abort(s.RunID()) {
		t.Fatal("Abort() reported no run in flight")
	}
	// Not fatal: the counters are the point of this test, and if the abort has
	// been misclassified we want the failure to say WHICH counter it poisoned,
	// not stop at the return value.
	if r := awaitRun(t, out); !errors.Is(r.err, ErrAborted) {
		t.Errorf("RunOnce = %v, want ErrAborted", r.err)
	}

	snap := stats.Lifetime()
	// Scan the whole prefix rather than naming stages: whichever stage the
	// classifier would have picked (context.Canceled lands in "other"), a user
	// abort must reach none of them.
	for name, n := range snap.Counters {
		if strings.HasPrefix(name, "speed.fail") && n != 0 {
			t.Errorf("%s = %d, want 0 - a user abort must not poison the fleet failure rate", name, n)
		}
	}
	for name, want := range map[string]int64{"speed.run.manual": 1, "speed.duration_n": 1} {
		if got := snap.Counters[name]; got != want {
			t.Errorf("%s = %d, want %d - the attempt itself still happened", name, got, want)
		}
	}
}
