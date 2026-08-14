package speedtest

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// Deleting a retried run must clear what that run really cost - and nothing
// else. The scheduler writes the retried attempt's bytes as a usage-only row one
// second after the measurement, so the two never share the backup merge key, and
// stamps it with the run it belongs to so the delete does not have to guess from
// that position. The whole chain has to hold: a row the scheduler leaves
// unstamped is one the delete can never find, and the bytes stay on the
// data-usage total for a speedtest that is no longer in the history.
func TestDeletingARetriedRunClearsWhatItSpent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	measuredTS := time.Now().Add(-time.Hour).Unix()
	s := &Scheduler{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.recordExtraUsage(ctx, Result{
		Engine: "iperf3", Server: "lab", ServerID: "1",
		DownloadMbps: 940, DownloadBytes: 125_000_000,
		ExtraDownBytes: 125_000_000,
	}, "scheduled", measuredTS)

	if _, err := st.DeleteSpeed(ctx, measuredTS); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 0 {
		t.Errorf("the deleted run still bills %d bytes: the usage row the scheduler wrote for its retried attempt "+
			"does not say which run it belongs to, so deleting the run left it behind - and no table, chart or CSV "+
			"ever shows that row for the operator to remove it by hand.", used.All)
	}
}

// A wholly failed run's usage row is the only record that run has, so deleting a
// DIFFERENT run must not take it. It carries no run reference precisely because
// there is no measurement to reference - and that is what keeps it out of the
// path of the neighbouring run's delete, even when it lands on the very next
// second.
func TestAFailedRunsUsageSurvivesDeletingTheRunBeforeIt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// The failed run first, because recordFailedUsage stamps the row with its own
	// clock: read back where it landed and place the scheduled measurement one
	// second EARLIER, so the failed row sits exactly where a positional cascade
	// would sweep it. Deriving the measurement's ts from the row rather than
	// predicting the row's ts from the clock keeps the arrangement exact even if
	// the second ticks mid-test.
	s := &Scheduler{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.recordFailedUsage(ctx, Result{
		Engine: "iperf3", Server: "lab", ServerID: "1",
		DownloadBytes: 40_000_000,
	}, "manual")
	rows, err := st.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the failed run wrote %d rows, want 1 (its usage)", len(rows))
	}
	failedTS, ok := rows[0]["ts"].(int64)
	if !ok {
		t.Fatalf("failed run's ts is %T, want int64", rows[0]["ts"])
	}

	i64 := func(v int64) *int64 { return &v }
	measuredTS := failedTS - 1
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: measuredTS, DownMbps: 940, UpMbps: 910, PingMS: 8,
		DownBytes: i64(125_000_000), UpBytes: i64(125_000_000),
		Server: "lab", ServerID: "1", Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed(measurement): %v", err)
	}

	if _, err := st.DeleteSpeed(ctx, measuredTS); err != nil {
		t.Fatalf("DeleteSpeed: %v", err)
	}
	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 40_000_000 {
		t.Errorf("data usage reads %d bytes after deleting the scheduled run, want 40000000: the failed manual run "+
			"that followed it a second later spent those bytes, and deleting somebody else's run destroyed its only "+
			"record of them.", used.All)
	}
}
