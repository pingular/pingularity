package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The stop button decides against a run the operator can SEE, and delivers that
// decision later. Nothing in between carries which run was meant.
//
// /api/status publishes a bare `speedtest_running` boolean, the dashboard draws
// "click to stop" from it on a 3s poll, and the click opens a confirm() dialog -
// which blocks that tab's event loop, poll included, for as long as the operator
// takes to read it. The POST that finally arrives says only "abort", and Abort()
// resolves that against whatever holds the flag at the instant it runs.
//
// So a run that started in the meantime is what gets killed. No preemption or
// scheduler race is needed; a few seconds of reading a dialog is enough.
func TestAnAbortDecidedAgainstAFinishedRunDoesNotKillTheNextOne(t *testing.T) {
	release := make(chan struct{})
	parked := make(chan struct{}, 2)
	var runs int
	s, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		runs++
		if runs == 1 {
			<-release // run N: finishes on its own when the test says so
			return Result{DownloadMbps: 100, UploadMbps: 10, PingMS: 5, Server: "t", Engine: "ookla"}, nil
		}
		parked <- struct{}{} // run N+1: sits here so an abort can land on it
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))
	ctx := context.Background()

	// 1. Run N is in flight and the dashboard is showing "click to stop".
	nDone := goRunOnce(ctx, s, "scheduled")
	waitFor(t, func() bool { return s.Running() }, "run N to claim the flag")

	// 2. The operator decides to stop THIS run; capture what identifies it. This is
	//    the moment the decision is made, and the only moment it is unambiguous.
	target := s.RunID()

	// 3. The dialog is still open. Run N finishes on its own and stores its sample.
	close(release)
	if r := <-nDone; r.err != nil {
		t.Fatalf("run N should have completed normally, got %v", r.err)
	}
	waitFor(t, func() bool { return !s.Running() }, "run N to release the flag")

	// 4. A reconnect fires a new run before the operator clicks OK.
	next := goRunOnce(ctx, s, "reconnect")
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("run N+1 never reached the tester")
	}

	// 5. The click lands now, carrying the decision made at step 2.
	s.Abort(target)

	killed := false
	select {
	case r := <-next:
		killed = errors.Is(r.err, ErrAborted)
		if killed {
			t.Error("the reconnect run was killed by a stop the operator decided on for the " +
				"PREVIOUS run, which had already finished and stored its result. The post-outage " +
				"measurement is lost, and because this is ErrAborted rather than ErrBusy the " +
				"reconnect window is not given back either - so no recovery speedtest runs for " +
				"up to a full speed interval")
		}
	case <-time.After(2 * time.Second):
		// Still running: correct. The abort named a run that had already ended.
	}
	if !killed {
		s.Abort(s.RunID()) // release the parked run so the test can finish
		<-next
	}
}

// waitFor spins until cond holds or the test gives up.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
