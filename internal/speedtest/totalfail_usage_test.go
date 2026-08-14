package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/store"
)

// A run in which EVERY candidate fails still moved real bytes - failed
// transfers return their byte counts and RunReason accumulates them - but the
// empty-result return threw the tally away, and the scheduler's error path
// stored nothing: injected traffic recorded as 0/0. Data usage must survive a
// total failure; the traffic lands on the user's bill either way.

// RunReason's total-failure exit must carry the accumulated spent bytes out
// alongside the error, or the scheduler has nothing to record.
func TestRunReasonTotalFailureCarriesSpentBytes(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, _ *ookla.Server, _ string, _ int) (Result, error) {
		return Result{DownloadBytes: 111, UploadBytes: 222}, errors.New("download: dead server")
	})

	o := NewOokla()
	res, err := o.RunReason(context.Background(), "manual")
	if err == nil {
		t.Fatal("want the run to fail - every candidate failed")
	}
	if res.DownloadBytes != 111 || res.UploadBytes != 222 {
		t.Fatalf("total-failure Result carries %d/%d bytes, want the injected 111/222 - the spent traffic vanished",
			res.DownloadBytes, res.UploadBytes)
	}
	// The engine names itself on this exit like it does on every other one: the
	// usage row this Result becomes carries an engine column, and an iperf3
	// failure fills it. Leaving it empty made the two engines' failures
	// inconsistent and left /metrics to guess from a default.
	if res.Engine != "ookla" {
		t.Fatalf("total-failure Result engine = %q, want ookla", res.Engine)
	}
}

// The scheduler's error path must persist the failed run's usage so
// SpeedDataUsage still counts it. Speeds stay zero and no thresholds are
// evaluated - this is accounting, not a measurement.
func TestSchedulerPersistsUsageWhenEveryCandidateFails(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(context.Context) (Result, error) {
		return Result{DownloadBytes: 111, UploadBytes: 222, Engine: "ookla"}, errors.New("download: dead server")
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("RunOnce must still report the failure")
	}

	ctx := context.Background()
	used, err := st.SpeedDataUsageSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpeedDataUsageSince: %v", err)
	}
	if used != 333 {
		t.Fatalf("data usage after a total-failure run = %d bytes, want the injected 333", used)
	}
	// The row is read RAW here, not through SpeedRuns: it is accounting, and
	// every measurement read hides it on purpose (see store.speedNotFailed, and
	// TestFailedRunLeavesNoReadableMeasurement for the consumer-facing half).
	rows, err := st.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("speed rows = %d, want exactly the one usage sample", len(rows))
	}
	row := rows[0]
	if row["download_bytes"] != int64(111) || row["upload_bytes"] != int64(222) {
		t.Fatalf("usage sample bytes = %v/%v, want 111/222", row["download_bytes"], row["upload_bytes"])
	}
	if row["down_mbps"] != float64(0) || row["up_mbps"] != float64(0) || row["healthy"] != nil {
		t.Fatalf("usage sample must carry no measurement or verdict: %+v", row)
	}
	if row["failed"] != int64(1) {
		t.Fatalf("usage sample failed marker = %v, want 1 - without it the row reads as a real 0 Mbps run", row["failed"])
	}
	if row["run_trigger"] != "manual" {
		t.Fatalf("usage sample trigger = %q, want manual", row["run_trigger"])
	}
	// A failed run must NOT read as a completion: the startup latch decides
	// "was the one-per-boot slot served" on this counter, and a failure serving
	// it would silently skip the boot measurement.
	if got := s.completions.Load(); got != 0 {
		t.Fatalf("completions = %d after a failed run, want 0", got)
	}
}

// Shutdown keeps its contract: with the parent context dead the store is about
// to close, so even a byte-carrying failure stores nothing.
func TestSchedulerShutdownStoresNoUsage(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(ctx context.Context) (Result, error) {
		return Result{DownloadBytes: 111, UploadBytes: 222}, ctx.Err()
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RunOnce(ctx, "manual"); err == nil {
		t.Fatal("cancelled run should return an error")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed"] != 0 {
		t.Fatalf("shutdown persisted %d speed rows, want 0 - the WAL must stay crash-consistent", cnt["speed"])
	}
}
