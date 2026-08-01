package speedtest

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// Running() and Abort() have to describe the same run.
//
// RunOnce claims the single-flight flag first and only publishes its cancel
// function several statements later - after the deferred cleanup, the current
// server reset, the counters and two log lines. In that window Running() is
// already true, so /api/status reports a run in progress and the dashboard offers
// "click to stop", while Abort() looked up a cancel that was still nil, did
// nothing, and reported that it had done nothing. The stop button is wired to
// exactly that, so a click landing in the window failed silently.
//
// The window is short, which is what makes it worth a test rather than an
// eyeball: it works every time it is tried by hand and fails for a user whose
// machine stalls between two statements.

// While a run holds the single-flight flag, Abort() must report that it signalled
// it - published cancel or not.
func TestAbortReportsSignalledDuringTheStartupWindow(t *testing.T) {
	s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		return Result{}, nil
	}))
	// Exactly the state RunOnce occupies between claiming the flag and publishing
	// its cancel: the claim taken under an id, with nothing to cancel yet.
	s.curID.Store(s.runSeq.Add(1))
	s.cur.Store(nil)
	defer s.curID.Store(0)

	if !s.Running() {
		t.Fatal("fixture wrong: Running() must be true for this to be the window under test")
	}
	if !s.Abort(s.RunID()) {
		t.Error("Abort() reported no run to stop while Running() was true - the dashboard offers a " +
			"stop button on precisely that state, so this is a button that does nothing")
	}
}

// The scenario as a user produces it: the status poll reports a run in progress,
// the dashboard draws "click to stop", and the user clicks ONCE. If that single
// click lands before RunOnce published its cancel, the click has to still count.
//
// Repeated, because the window is a handful of statements wide: any single
// iteration usually misses it, and the point is that a user eventually will not.
func TestASingleStopClickTheMomentARunAppearsAlwaysStopsIt(t *testing.T) {
	const rounds = 300
	// Bounded so a dropped click costs milliseconds, not seconds: with the bug this
	// fires on a good fraction of rounds, and 300 x seconds would outlast any CI job.
	const stopWait = 200 * time.Millisecond
	missed := 0
	for i := 0; i < rounds; i++ {
		s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
			<-ctx.Done() // parked until something cancels it
			return Result{}, ctx.Err()
		}))
		clicked := make(chan bool, 1)
		go func() {
			// Exactly what the dashboard does: watch for a run, then click once.
			for !s.Running() {
				runtime.Gosched()
			}
			clicked <- s.Abort(s.RunID())
		}()
		out := goRunOnce(context.Background(), s, "manual")

		dropped := false
		select {
		case r := <-out:
			if !errors.Is(r.err, ErrAborted) {
				t.Fatalf("round %d: run ended %v, want ErrAborted", i, r.err)
			}
		case <-time.After(stopWait):
			// The run never stopped: the click was dropped, and the user is looking at
			// a stop button that did nothing.
			dropped = true
			s.Abort(s.RunID()) // release the parked goroutine so the test can exit
			<-out
		}
		// One click, so at most one failure per round - counting the timeout AND the
		// click's own return would report more dropped clicks than clicks made.
		if ok := <-clicked; !ok {
			dropped = true
		}
		if dropped {
			missed++
		}
	}
	if missed > 0 {
		t.Errorf("%d of %d stop clicks were dropped: Abort() landed between RunOnce claiming the "+
			"single-flight flag and publishing its cancel", missed, rounds)
	}
}

// ...but a request belonging to a run that already ended must not leak into the
// next one: a stop click has to stop the test it was clicked on, not the one
// after it.
func TestAStaleAbortDoesNotCancelTheNextRun(t *testing.T) {
	// Run 1: parked in the tester, then properly aborted.
	s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))
	out := goRunOnce(context.Background(), s, "manual")
	waitRunning(t, s)
	if !s.Abort(s.RunID()) {
		t.Fatal("Abort() found no run in flight")
	}
	if r := awaitRun(t, out); !errors.Is(r.err, ErrAborted) {
		t.Fatalf("first run = %v, want ErrAborted", r.err)
	}

	// Run 2 must be untouched by run 1's abort.
	s.TesterFn = func() Tester {
		return testerFunc(func(ctx context.Context) (Result, error) {
			return Result{DownloadMbps: 100}, nil
		})
	}
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Errorf("second run = %v, want success: the previous run's abort leaked forward", err)
	}
}
