package speedtest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A retried direction spends its earlier attempts' bytes too. Only the WINNING
// attempt's count may land on the measurement row - a byte count there is the
// signal that a direction ran - so the rest used to be dropped outright: two
// 125 MB attempts recorded 125 MB, and a metered link was told it had spent half
// what it did.
//
// The remainder is now billed as a usage-only row: counted by the data-usage
// sums, filtered out of every measurement read, exactly like a failed run's.
func TestRetriedRunBillsEveryAttemptItSpent(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	const measured = 125_000_000
	const retried = 125_000_000
	s := &Scheduler{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	res := Result{
		Engine: "iperf3", Server: "lab", ServerID: "1",
		DownloadMbps: 940, DownloadBytes: measured,
		// What the losing attempt spent before the retry succeeded.
		ExtraDownBytes: retried,
	}
	s.recordExtraUsage(ctx, res, "scheduled", time.Now().Unix())

	// Usage counts everything the run cost.
	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != retried {
		t.Fatalf("data usage recorded %d for the retried attempt, want %d: the bytes a failed attempt spent are still bytes off a metered allowance", used.All, retried)
	}

	// ...while no measurement appears for it.
	runs, err := st.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("the usage row surfaced as a measurement (%d runs): a retried attempt measured nothing and must not read as a run", len(runs))
	}
	latest, err := st.LatestSpeed(ctx)
	if err != nil {
		t.Fatalf("LatestSpeed: %v", err)
	}
	if latest != nil {
		t.Fatalf("the usage row became the LATEST speed reading (%.1f Mbps down): it would pin the dashboard tiles and the freshness anchor on a run that measured nothing", latest.DownMbps)
	}
}

// The row must only appear when there is something extra to bill, or every
// ordinary run grows a companion row.
func TestNoExtraUsageRowWhenNothingWasRetried(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	s := &Scheduler{store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.recordExtraUsage(ctx, Result{Engine: "iperf3", DownloadBytes: 1000}, "scheduled", time.Now().Unix())

	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if used.All != 0 {
		t.Fatalf("a clean run wrote a %d-byte usage row; only a retry or a half-failed run has anything extra to bill", used.All)
	}
}

// The iperf engine has to HAND OUT the remainder, or the scheduler has nothing
// to bill. A download that fails once and succeeds on the retry must report the
// winning attempt as the MEASUREMENT and the loser's bytes as extra spend.
func TestIperfReportsSpendBeyondTheMeasuredAttempt(t *testing.T) {
	const bytesPerAttempt = 125_000_000
	ok := `{"end":{"streams":[{"sender":{"min_rtt":12000}}],` +
		`"sum_received":{"bytes":125000000,"bits_per_second":1000000000},` +
		`"sum_sent":{"bytes":125000000,"bits_per_second":1000000000}}}`
	var downCalls int
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") { // download
			downCalls++
			if downCalls == 1 {
				// Real bytes moved, then the connection dropped - a transient
				// failure, so the run retries and the second attempt carries it.
				// The body still reports what the attempt transferred.
				return []byte(ok), errors.New("connection reset by peer")
			}
		}
		return []byte(ok), nil
	})

	i := &Iperf{ServerFn: func() string { return "lab:5201" },
		DirectionFn: func() string { return "down" },
		RetriesFn:   func() int { return 1 },
		UDPFn:       func() bool { return false }}
	res, err := i.Run(context.Background())
	if err != nil {
		t.Fatalf("the retry should have carried the run: %v", err)
	}
	if res.DownloadBytes != bytesPerAttempt {
		t.Fatalf("DownloadBytes = %d, want the WINNING attempt only (%d): the measurement row must describe one transfer", res.DownloadBytes, bytesPerAttempt)
	}
	if res.ExtraDownBytes <= 0 {
		t.Fatalf("ExtraDownBytes = %d: the failed attempt moved %d bytes and they are missing from data usage entirely", res.ExtraDownBytes, bytesPerAttempt)
	}
}

// THE WIRING. The two tests above call recordExtraUsage directly, so deleting
// the call from RunOnce left every one of them green - the fix could be switched
// off entirely without a single failure. This drives the real RunOnce and
// requires both rows: the measurement, and the usage row for what the retry
// spent, at DIFFERENT timestamps so a backup can carry both.
func TestRunOnceRecordsExtraUsageForARetriedRun(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	const measured, extra = 125_000_000, 125_000_000
	s := &Scheduler{
		tester: testerFunc(func(ctx context.Context) (Result, error) {
			return Result{
				Engine: "iperf3", Server: "lab", ServerID: "1",
				DownloadMbps: 940, DownloadBytes: measured,
				ExtraDownBytes: extra,
			}, nil
		}),
		store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil)), interval: time.Hour,
	}
	if _, err := s.RunOnce(ctx, "scheduled"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	used, err := st.SpeedDataUsage(ctx, time.Now())
	if err != nil {
		t.Fatalf("SpeedDataUsage: %v", err)
	}
	if want := int64(measured + extra); used.All != want {
		t.Fatalf("data usage after a retried run = %d, want %d: RunOnce never recorded what the failed attempt spent", used.All, want)
	}
	runs, err := st.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("visible measurements = %d, want exactly 1 (the usage row must not read as a run)", len(runs))
	}
}
