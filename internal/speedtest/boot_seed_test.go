package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Run ids made an abort safe to act on late - within one process. But the
// sequence restarted from 1 on every boot, so the id a tab captured before its
// confirm() dialog blocked could name a DIFFERENT process's run: the daemon
// restarts while the dialog sits open, the new boot's startup run claims id 1
// again three seconds in, and the queued click lands on it. Session cookies
// survive restarts (stateless HMACs - see auth.go), so nothing between the tab
// and Abort notices the process changed.
func TestAStopFromThePreviousBootDoesNotKillTheNewBootsFirstRun(t *testing.T) {
	// Boot A: its first run is on screen and the operator clicks stop; the
	// confirm() dialog blocks the tab with A's run id already captured.
	releaseA := make(chan struct{})
	a, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		<-releaseA
		return Result{DownloadMbps: 100, UploadMbps: 10, PingMS: 5, Server: "t", Engine: "ookla"}, nil
	}))
	aDone := goRunOnce(context.Background(), a, "startup")
	waitFor(t, func() bool { return a.Running() }, "boot A's first run to claim the flag")
	stale := a.RunID()
	close(releaseA)
	if r := <-aDone; r.err != nil {
		t.Fatalf("boot A's run should have completed normally, got %v", r.err)
	}

	// The daemon restarts. Real restarts are seconds apart at their fastest;
	// two milliseconds is enough for the boots to be distinct in time.
	time.Sleep(2 * time.Millisecond)

	// Boot B: a fresh process, its own first run in flight.
	parked := make(chan struct{}, 1)
	b, _ := newAbortScheduler(t, testerFunc(func(ctx context.Context) (Result, error) {
		parked <- struct{}{}
		<-ctx.Done()
		return Result{}, ctx.Err()
	}))
	bDone := goRunOnce(context.Background(), b, "startup")
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("boot B's first run never reached the tester")
	}

	// The queued click lands, carrying the id of a run that ended a process ago.
	if b.Abort(stale) {
		t.Error("Abort accepted a run id captured in the PREVIOUS boot: ids repeat across " +
			"restarts, so a stop decided about one process's run names the next process's run too")
	}
	killed := false
	select {
	case r := <-bDone:
		killed = errors.Is(r.err, ErrAborted)
		if killed {
			t.Error("boot B's startup run - the fresh boot's baseline measurement - was cancelled " +
				"by a stop decided against boot A's run of the same id. Nothing is stored, and the " +
				"operator never asked this process for anything")
		}
	case <-time.After(500 * time.Millisecond):
		// Still running: correct. The stale id names no run of this boot.
	}
	if !killed {
		b.Abort(b.RunID()) // release the parked run so the test can finish
		<-bDone
	}
}

// Run ids ride the JSON status API into a JavaScript client, whose numbers are
// exact only below 2^53 - an id past that would round in the tab and come back
// naming a run that never existed. The seed must also not be zero: 0 is the
// "whatever is running now" wildcard, and a seeded sequence exists to get away
// from exactly that.
func TestBootRunSeedStaysInsideFloat64SafeIntegers(t *testing.T) {
	seed := bootRunSeed()
	if seed == 0 {
		t.Fatal("bootRunSeed() = 0: the first run id would be small again, colliding with a previous boot's")
	}
	// Half the safe range stays in reserve for the increments on top of the
	// seed (bootRunSeed documents the arithmetic).
	if seed >= 1<<52 {
		t.Fatalf("bootRunSeed() = %d, outside the float64-safe budget (>= 2^52): run ids would "+
			"stop surviving the JSON -> JavaScript round trip", seed)
	}
}
