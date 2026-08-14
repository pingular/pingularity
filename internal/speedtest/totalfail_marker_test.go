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

// The usage row a FAILED run leaves behind (see recordFailedUsage) must be
// accounting and NOTHING else. It used to be indistinguishable from a real
// 0 Mbps measurement: every consumer's "was this direction measured?" predicate
// is exactly `bytes != nil`, and the row carries real bytes with zero speeds -
// so /metrics emitted pingularity_speed_download_mbps 0 (a permanent false
// "below threshold" alert), advanced the last-run freshness anchor (defeating
// staleness alerts), and the dashboard drew a 0 point in the chart, the history
// table and the averages. store.SpeedSample.Failed is the discriminator, and
// the store hides marked rows from every MEASUREMENT read while the data-usage
// sums keep counting them.

// A total-failure run must leave usage behind WITHOUT leaving a measurement:
// nothing that reads "the last speedtest" may see it.
func TestFailedRunLeavesNoReadableMeasurement(t *testing.T) {
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
	// The bytes are on the user's bill either way - accounting still counts them.
	if used, err := st.SpeedDataUsageSince(ctx, time.Now().Add(-time.Hour)); err != nil || used != 333 {
		t.Fatalf("SpeedDataUsageSince = %d, %v; want the injected 333 bytes", used, err)
	}

	// ...and NOTHING that renders or alerts on a measurement may see the row.
	// LatestSpeed is what /metrics and the dashboard tiles read: a row here
	// emits pingularity_speed_download_mbps 0 and stamps the freshness anchor.
	sp, err := st.LatestSpeed(ctx)
	if err != nil {
		t.Fatalf("LatestSpeed: %v", err)
	}
	if sp != nil {
		t.Fatalf("LatestSpeed returned the failed run as a measurement: %+v", *sp)
	}
	runs, err := st.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("the history table shows %d row(s) for a run that measured nothing", len(runs))
	}
	if n, err := st.SpeedCount(ctx); err != nil || n != 0 {
		t.Fatalf("SpeedCount = %d, %v; want 0 - the paging total must match the rows", n, err)
	}
	hist, err := st.SpeedHistory(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpeedHistory: %v", err)
	}
	if len(hist) != 0 {
		t.Fatalf("the chart would draw %d zero point(s) for a failed run", len(hist))
	}
}

// The total-failure Result must name the engine that spent the bytes, or the
// usage row's engine column is empty and the fleet cannot tell an Ookla
// failure from an iperf3 one (web.go's metrics path defaults empty to "ookla",
// which would be a guess that happens to be right for exactly one engine).
func TestRunReasonTotalFailureCarriesEngine(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, _ *ookla.Server, _ string, _ int) (Result, error) {
		// A failed measure never reaches the Result-building tail of measure(),
		// so the engine is not set on the way out - only the bytes are.
		return Result{DownloadBytes: 111, UploadBytes: 222}, errors.New("download: dead server")
	})

	res, err := NewOokla().RunReason(context.Background(), "manual")
	if err == nil {
		t.Fatal("want the run to fail - every candidate failed")
	}
	if res.Engine != "ookla" {
		t.Fatalf("total-failure Result carries engine %q, want ookla - the usage row cannot say who spent the bytes", res.Engine)
	}
}
