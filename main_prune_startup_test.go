package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// THE FIRST PRUNE RUNS ON THE WORST CLOCK THE PROCESS WILL EVER HOLD.
//
// Prune's in-process guard compares the wall clock against monotonic time, so it
// sees a clock that STEPS while running. It cannot see a clock that was already
// wrong when the machine booted - there is no step, just a bad reading held
// steadily - and that is exactly the RTC-less/dead-battery case. runPruner used
// to call prune() as its first statement, so on such a host the irreversible
// delete landed before time sync had any chance to correct the reading.
//
// The fix is a wait, so this asserts the absence of work: with the grace still
// running, the database must be untouched.
func TestPrunerWaitsBeforeItsFirstDestructivePass(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// A row far older than any retention window: if a prune runs, it goes.
	old := time.Now().Add(-400 * 24 * time.Hour)
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: old, Target: "a", Family: "ipv4", LatencyMS: 20, Success: true},
	}); err != nil {
		t.Fatal(err)
	}
	count := func() int64 {
		c, err := st.TableCounts(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c["samples"]
	}
	if count() != 1 {
		t.Fatalf("seed: got %d samples, want 1", count())
	}

	set, err := settings.New(ctx, st, settings.Values{Retention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	// Long enough that the test would have to hang to reach the first pass.
	prev := pruneStartupGrace
	pruneStartupGrace = time.Hour
	defer func() { pruneStartupGrace = prev }()

	p := &program{store: st, log: slog.New(slog.DiscardHandler)}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); p.runPruner(runCtx, set) }()

	// Give the goroutine real time to do the wrong thing if it is going to.
	time.Sleep(150 * time.Millisecond)
	if got := count(); got != 1 {
		t.Errorf("the pruner deleted %d row(s) during its startup grace; the first "+
			"destructive pass must not run on an unverified boot clock", 1-got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPruner ignored context cancellation during its startup grace")
	}
}
