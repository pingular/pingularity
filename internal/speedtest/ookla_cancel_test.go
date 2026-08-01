package speedtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// fakeCaptureWindow stands in for the library's ooklaCaptureTime: the span a
// speedtest-go transfer keeps running for after its context dies, because only
// its own timer can stop it. Shortened from 15s so a transfer nobody releases
// bounds the test instead of hanging it.
const fakeCaptureWindow = 3 * time.Second

// promptRelease is how long a cancelled transfer may take to hand the caller
// back. The two behaviours it separates are nowhere near each other - before
// runTransfer the caller waited out the entire capture window - so this is a
// wide margin, not a tight timing assertion.
const promptRelease = 500 * time.Millisecond

// fakeTransfer is a stand-in for a speedtest-go transfer that fails the same way
// the real one does: it never watches ctx, and returns only when its capture
// window closes - either because the test closed it early or because the window
// ran out. It writes the server's result fields on the way out exactly as
// downloadTestContext does, so -race polices the ownership rule: nothing may
// touch srv once runTransfer has reported the transfer abandoned.
func fakeTransfer(window <-chan struct{}, returned chan<- struct{}) ooklaTransfer {
	return func(_ context.Context, srv *ookla.Server) error {
		select {
		case <-window:
		case <-time.After(fakeCaptureWindow):
		}
		srv.DLSpeed, srv.ULSpeed = 812.5, 96.25
		returned <- struct{}{}
		return nil
	}
}

// Cancelling the context must release the caller NOW, not at the end of the
// transfer's capture window. That wait is what an abort used to sit through: the
// UI spinner kept turning, the scheduler's single-flight flag stayed set so every
// new run answered ErrBusy, and the shutdown drain gave up with a worker still
// live. The orphaned library goroutine finishing later is expected and fine -
// what must not happen is anyone waiting for it.
func TestRunTransferReleasesOnCancel(t *testing.T) {
	window := make(chan struct{})
	returned := make(chan struct{}, 1)

	// Wire the fake through the production seam and put it back afterwards (the
	// package's swap-a-var idiom - see iperfExec, iperfRetryDelay).
	orig := ooklaDownload
	defer func() { ooklaDownload = orig }()
	ooklaDownload = fakeTransfer(window, returned)

	srv := &ookla.Server{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(10*time.Millisecond, cancel) // the abort click, mid-transfer

	start := time.Now()
	finished, err := runTransfer(ctx, srv, ooklaDownload)
	elapsed := time.Since(start)

	if elapsed > promptRelease {
		t.Fatalf("runTransfer held the caller for %v after cancellation, want under %v "+
			"(the transfer's own capture window is %v in production)", elapsed, promptRelease, ooklaCaptureTime)
	}
	if finished {
		t.Fatalf("finished = true while the transfer is still running - the caller would read srv from under it")
	}
	// The error shape the callers already expect: RunReason ends the run on
	// errTransferAbandoned, the scheduler reads a context error as an abort (no
	// failure stat, no store insert), and speedFailStage still bins it by the
	// stage prefix measure wraps it in.
	if !errors.Is(err, errTransferAbandoned) {
		t.Errorf("err = %v, want errTransferAbandoned so RunReason ends the run", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if got := speedFailStage(fmt.Errorf("download: %w", err)); got != "download" {
		t.Errorf("speedFailStage = %q, want download", got)
	}

	// The orphan must be able to deliver its result and exit: done is buffered
	// precisely so a transfer nobody listens to any more never parks forever.
	close(window)
	select {
	case <-returned:
	case <-time.After(fakeCaptureWindow + time.Second):
		t.Fatal("the abandoned transfer never returned; its result channel must not block it")
	}
	// Only now, having joined it, may the test read what it wrote.
	if srv.DLSpeed != 812.5 {
		t.Errorf("DLSpeed = %v, want the orphan's own write (812.5)", srv.DLSpeed)
	}
}

// An abandoned transfer must end the attempt loop on the spot: a retry would call
// srv.Context.Reset() while the orphan's workers still read that manager - an
// unlocked swap of the snapshot and both directions. This is measure's exact
// composition (withRetryPred over runTransfer) with the network swapped out.
func TestAbandonedTransferIsNotRetried(t *testing.T) {
	window := make(chan struct{})
	returned := make(chan struct{}, 1)

	orig := ooklaUpload
	defer func() { ooklaUpload = orig }()
	ooklaUpload = fakeTransfer(window, returned)

	srv := &ookla.Server{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	attempts := 0
	start := time.Now()
	err := withRetryPred(ctx, 2, func(error) bool { return true }, func() error {
		attempts++
		finished, e := runTransfer(ctx, srv, ooklaUpload)
		if finished {
			t.Error("finished = true on a cancelled context")
		}
		return e
	})
	if elapsed := time.Since(start); elapsed > promptRelease {
		t.Fatalf("the retry loop took %v to give up, want under %v", elapsed, promptRelease)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: another attempt would Reset the manager under the orphan", attempts)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
}

// The uncancelled path is untouched: the transfer's own return value comes
// straight back with finished set, which is what tells measure it still owns srv
// and may read the speeds and byte totals off it.
func TestRunTransferReturnsTransferResult(t *testing.T) {
	want := errors.New("server refused the transfer")
	finished, err := runTransfer(context.Background(), &ookla.Server{},
		func(context.Context, *ookla.Server) error { return want })
	if !finished {
		t.Error("finished = false for a transfer that completed on its own")
	}
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want the transfer's own error", err)
	}
	if finished, err := runTransfer(context.Background(), &ookla.Server{},
		func(context.Context, *ookla.Server) error { return nil }); !finished || err != nil {
		t.Errorf("successful transfer = (%v, %v), want (true, nil)", finished, err)
	}
}
