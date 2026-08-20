package speedtest

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// RunOnce writes up to three records for one run: the measurement row, an
// extra-usage accounting row naming it (usage_run_ts), and the selection
// report keyed to it. This test pins the pairing under same-second pressure -
// the seconds around now are pre-occupied, the way a failed run's or another
// run's accounting row can occupy them:
//
//  1. The measurement's second is ITS OWN: ts is the run's identity for
//     delete, merge and the UI, and DeleteSpeed's first statement removes
//     every unreferenced row on that second - so a shared second silently
//     un-bills whatever else lived there.
//  2. The extra-usage row names the second the measurement actually LANDED
//     on, and exists only once the measurement is durable - an accounting row
//     referencing a measurement that never landed would inflate the usage
//     total forever, with nothing to sweep it.
//
// Asserted through the surfaces the bug corrupts: DeleteSpeed's removed-count
// and the SpeedDataUsage billing total.
func TestRunOnceUsagePairingUnderSameSecondPressure(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Occupy the seconds RunOnce is about to stamp: 6 accounting rows of 1024
	// bytes each. They never show in listings, so nothing visibly collides -
	// only delete, merge and the usage total would corrupt.
	now := time.Now()
	const occupierBytes = 6 * 1024
	for ts := now.Unix() - 1; ts <= now.Unix()+4; ts++ {
		if _, err := st.InsertSpeedTS(ctx, store.SpeedSample{TS: ts, Server: "occupier", Failed: true,
			DownBytes: bytesPtr(1024)}); err != nil {
			t.Fatal(err)
		}
	}

	tester := testerFunc(func(ctx context.Context) (Result, error) {
		return Result{DownloadMbps: 42, Server: "fake", Engine: "ookla",
			DownloadBytes: 1 << 20, ExtraUpBytes: 2 << 20}, nil
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp, err := s.RunOnce(ctx, "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// The run's full bill is present: measurement + extra spend + occupiers.
	after := now.Add(48 * time.Hour) // usage windows are relative to now; look back over everything
	u, err := st.SpeedDataUsage(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	const runBytes = 1<<20 + 2<<20
	if u.All != occupierBytes+runBytes {
		t.Fatalf("usage total = %d, want %d - the run's extra spend is unbilled or double-billed (pairing broke)", u.All, occupierBytes+runBytes)
	}

	// Deleting the run removes exactly ONE measurement row - a count of 2
	// means the run shared its second with an unrelated row and took it along.
	removed, err := st.DeleteSpeed(ctx, sp.TS)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("DeleteSpeed removed %d rows at the run's second, want exactly 1 - the run's identity is ambiguous", removed)
	}

	// And the billing total returns to exactly the occupiers': the run's
	// measurement AND its referenced extra row are gone, nothing else is.
	u, err = st.SpeedDataUsage(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if u.All != occupierBytes {
		t.Fatalf("usage total after delete = %d, want %d - either the extra row was orphaned (stale reference) or unrelated rows were un-billed", u.All, occupierBytes)
	}
}

// TestPartialBreachesItsUploadThreshold: a kept partial whose upload FAILED
// must trip a configured upload minimum, not silence it - the alternative
// read every such run as healthy and actively reset an in-progress breach
// streak at the exact moment the uplink fell below the rescue cliff.
func TestPartialBreachesItsUploadThreshold(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	tester := testerFunc(func(ctx context.Context) (Result, error) {
		return Result{DownloadMbps: 200, PingMS: 12, Server: "fake", Engine: "ookla",
			DownloadBytes: 1 << 20, ExtraUpBytes: 2 << 20, UploadFailed: true}, nil
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100, UpMbps: 10} }
	var alerted []string
	s.OnUnhealthy = func(sp store.SpeedSample, failures []string) { alerted = append(alerted, failures...) }

	sp, err := s.RunOnce(ctx, "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.Healthy == nil || *sp.Healthy {
		t.Fatal("a partial whose upload FAILED under a configured upload minimum was judged healthy - the alert is silenced by the very failure it watches, and the breach streak resets")
	}
	if len(alerted) == 0 {
		t.Fatal("OnUnhealthy never fired for the failed direction")
	}
	found := false
	for _, f := range alerted {
		if strings.Contains(f, "upload unmeasured") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the failure list %v does not name the failed upload", alerted)
	}
}

// TestPartialWithoutThresholdStaysUnjudged: with no upload minimum configured,
// a kept partial keeps its previous nil verdict - the breach exists only where
// an operator asked to watch the direction.
func TestPartialWithoutThresholdStaysUnjudged(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	tester := testerFunc(func(ctx context.Context) (Result, error) {
		return Result{DownloadMbps: 200, PingMS: 12, Server: "fake", Engine: "ookla",
			DownloadBytes: 1 << 20, ExtraUpBytes: 2 << 20, UploadFailed: true}, nil
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} }
	fired := false
	s.OnUnhealthy = func(store.SpeedSample, []string) { fired = true }

	sp, err := s.RunOnce(ctx, "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.Healthy == nil || !*sp.Healthy {
		t.Fatal("with no threshold on the failed direction, the measured download passing its own minimum is an ordinary healthy verdict")
	}
	if fired {
		t.Fatal("OnUnhealthy fired although no configured threshold was breached")
	}
}
