package speedtest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// abortTester moves bytes, then waits to be cancelled - a user pressing Abort
// part-way through a transfer. It reports what it spent alongside the error,
// which is the contract every engine follows on a failure path.
type abortTester struct {
	started chan struct{}
	spent   int64
}

func (a *abortTester) Run(ctx context.Context) (Result, error) {
	close(a.started)
	<-ctx.Done()
	return Result{Engine: "ookla", DownloadBytes: a.spent}, ctx.Err()
}

// ErrAborted's own documentation promises that data an aborted run had already
// moved still lands in a usage-only row. An abort is a user action, not a
// measurement failure, so nothing is stored as a reading - but the bytes were
// spent off a metered allowance either way, and the row is the only place they
// can be recorded.
func TestAbortedRunStillBillsWhatItMoved(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	const spent = 40 << 20 // 40 MiB pushed before the user gave up
	tester := &abortTester{started: make(chan struct{}), spent: spent}
	s := &Scheduler{
		tester: tester, store: st,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		interval: time.Hour,
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.RunOnce(ctx, "manual")
		done <- err
	}()
	<-tester.started
	// Abort the run that is holding the claim.
	if id := s.curID.Load(); !s.Abort(id) {
		t.Fatalf("Abort(%d) did not take", id)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an aborted run reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce did not return after the abort")
	}

	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != spent {
		t.Fatalf("data usage after an aborted run = %d, want %d: ErrAborted's contract says the bytes it already moved are still recorded", used.All, spent)
	}
	// ...and it must not read as a measurement.
	runs, err := st.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("the aborted run surfaced as a measurement (%d rows)", len(runs))
	}
}
