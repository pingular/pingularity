package speedtest

import (
	"context"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// An aborted transfer is abandoned rather than stopped: the library's transfers
// do not honour a context, so runTransfer walks away and lets the goroutine die
// on its own capture timer. That is a deliberate trade (see runTransfer), but it
// leaves the link busy for up to ooklaCaptureTime AFTER the scheduler has already
// released its single-flight guard - which is the moment the user is free to
// start another test.
//
// A run started in that window measures a link the abandoned transfer is still
// pulling bytes through: the new result reads slower than the link really is, and
// the data is spent twice. Repeating abort/start stacks them.

// requireQuiet makes each test independent of the last. liveTransfers is a
// package global and an abandoned goroutine outlives the test that created it by
// however long its work takes, so asserting "it must be zero right now" makes
// these tests order-dependent - which is precisely the flakiness the counter
// exists to describe. Drain instead, before and after.
func requireQuiet(t *testing.T) {
	t.Helper()
	if !awaitQuietTransfers(context.Background(), 10*time.Second) {
		t.Fatalf("transfers from an earlier test never drained")
	}
	t.Cleanup(func() { awaitQuietTransfers(context.Background(), 10*time.Second) })
}

// An abandoned transfer must remain visible as in-flight until it really ends.
func TestAnAbandonedTransferIsStillCountedAsInFlight(t *testing.T) {
	requireQuiet(t)
	release := make(chan struct{})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	// A transfer that ignores its context, exactly as the library's do.
	finished, _ := runTransfer(ctx, &ookla.Server{}, func(context.Context, *ookla.Server) error {
		<-release
		return nil
	})
	if finished {
		t.Fatal("the transfer reported finishing; this test needs the abandoned path")
	}
	if n := liveTransfers.Load(); n != 1 {
		t.Errorf("liveTransfers = %d after abandoning a transfer, want 1: nothing tracks the "+
			"goroutine still using the link, so the next run cannot know to wait", n)
	}
}

// The wait must actually block while one is running, and clear once it ends.
func TestAwaitQuietTransfersWaitsForAnOrphanThenProceeds(t *testing.T) {
	requireQuiet(t)
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	runTransfer(ctx, &ookla.Server{}, func(context.Context, *ookla.Server) error {
		<-release
		return nil
	})

	// While the orphan runs, the wait must not return quiet.
	if awaitQuietTransfers(context.Background(), 100*time.Millisecond) {
		t.Error("awaitQuietTransfers reported a quiet link while an abandoned transfer was running")
	}
	// Once it ends, the wait clears promptly.
	close(release)
	if !awaitQuietTransfers(context.Background(), 2*time.Second) {
		t.Error("awaitQuietTransfers never went quiet after the orphan finished")
	}
	if n := liveTransfers.Load(); n != 0 {
		t.Errorf("liveTransfers = %d after the orphan exited, want 0", n)
	}
}

// It must be bounded: a genuinely stuck transfer delays a run, it does not cancel
// it. A monitor that stops measuring because of one bad transfer is worse than
// one that measures alongside it.
func TestAwaitQuietTransfersGivesUpRatherThanHanging(t *testing.T) {
	requireQuiet(t)
	stuck := make(chan struct{})
	defer close(stuck)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	runTransfer(ctx, &ookla.Server{}, func(context.Context, *ookla.Server) error {
		<-stuck
		return nil
	})

	start := time.Now()
	if awaitQuietTransfers(context.Background(), 150*time.Millisecond) {
		t.Error("reported quiet while a transfer was still stuck")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("waited %v for a stuck transfer; the bound must cap it", el)
	}
}

// A caller's own cancellation must break the wait immediately - shutdown must not
// sit behind someone else's straggler.
func TestAwaitQuietTransfersHonoursItsContext(t *testing.T) {
	requireQuiet(t)
	stuck := make(chan struct{})
	defer close(stuck)
	tctx, tcancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); tcancel() }()
	runTransfer(tctx, &ookla.Server{}, func(context.Context, *ookla.Server) error {
		<-stuck
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	start := time.Now()
	awaitQuietTransfers(ctx, time.Hour)
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("waited %v after its context was cancelled", el)
	}
}

// A run whose context is already dead must not start a transfer at all.
//
// runTransfer counts the transfer in and launches the goroutine before it looks at
// ctx, so a cancelled run still handed the library a transfer it cannot stop -
// speedtest-go ignores the context and runs to its own fifteen-second capture
// deadline. That orphan then costs twice: this run reports errTransferAbandoned
// and gives up its remaining servers, and the NEXT run waits for the link to go
// quiet before it can measure anything.
func TestACancelledRunStartsNoTransfer(t *testing.T) {
	requireQuiet(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already dead before the call

	// A channel, not a bool: the goroutine is asynchronous, so reading a flag right
	// after the call can miss a launch that simply has not been scheduled yet - the
	// test would then pass for the wrong reason.
	started := make(chan struct{}, 1)
	finished, err := runTransfer(ctx, &ookla.Server{}, func(context.Context, *ookla.Server) error {
		started <- struct{}{}
		return nil
	})
	launched := false
	select {
	case <-started:
		launched = true
	case <-time.After(250 * time.Millisecond):
	}
	if launched {
		t.Error("launched the library transfer on an already-cancelled run; it ignores the " +
			"context, so it runs to its own capture deadline and the next run waits for it")
	}
	if finished {
		t.Error("reported the transfer as finished; nothing ran")
	}
	if err == nil {
		t.Error("returned no error for a cancelled run")
	}
	if n := liveTransfers.Load(); n != 0 {
		t.Errorf("liveTransfers = %d after declining to start; the counter makes the NEXT run "+
			"wait, so a transfer that never ran must not be counted", n)
	}
}

// A live context must still run, or the guard has simply broken transfers.
func TestALiveRunStillStartsItsTransfer(t *testing.T) {
	requireQuiet(t)
	var started bool
	finished, err := runTransfer(context.Background(), &ookla.Server{}, func(context.Context, *ookla.Server) error {
		started = true
		return nil
	})
	if !started || !finished || err != nil {
		t.Errorf("live run: started=%v finished=%v err=%v; want true/true/nil", started, finished, err)
	}
	if n := liveTransfers.Load(); n != 0 {
		t.Errorf("liveTransfers = %d after a completed transfer, want 0", n)
	}
}
