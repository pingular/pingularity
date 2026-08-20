package main

import (
	"context"
	"testing"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
)

// TestAnsweredInstallIsNotHeldByAStoreReadError: "answered" is permanent and
// lives in the loaded settings - QuickSetupHold(true, ...) is false for every
// clock value - so no store fault can make it need re-asking. The hold used to
// read the offer clock from the store FIRST and return held on any read error;
// the right direction for a fresh install, but on an install that consented
// long ago it paused monitoring (probe rounds skipped, pause rows written) and
// printed a 'first run: monitoring is on hold' boot notice, off one transient
// disk hiccup.
func TestAnsweredInstallIsNotHeldByAStoreReadError(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	set, err := settings.New(ctx, st, testDefaultsFor(config.Config{}))
	if err != nil {
		t.Fatalf("settings load: %v", err)
	}
	if err := set.SetQuickSetupDone(ctx, true); err != nil {
		t.Fatalf("mark answered: %v", err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsReleased {
		t.Fatalf("healthy answered install reads %v, want released - fixture fault", got)
	}

	// The transient fault: every store read now errors. The in-memory answer
	// is untouched.
	st.Close()

	if got := quickSetupHoldState(ctx, set); got != qsReleased {
		t.Fatalf("a store read error put an ANSWERED install on the first-run hold (%v, want released): monitoring pauses and the boot notice claims a first run, on an install that consented long ago", got)
	}
}
