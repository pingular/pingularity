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

// A failed iperf3 run still moved bytes, and those bytes are on the user's bill:
// the run's Result must carry them out with the error so the scheduler's usage
// row is honest (the Ookla engine already does - see totalfail_usage_test.go).
// Returning an empty Result recorded 0 bytes for traffic that measurably
// transited the link.

// A body with real totals alongside a nonzero exit (data moved, then the
// control connection dropped) must not throw the transfer away: the failure
// exit carries the bytes, and the Result still names its engine and server so
// the usage row has an engine column to fill.
func TestIperfRunFailureCarriesSpentBytes(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeDownJSON), errors.New("exit status 1")
	})
	res, err := newRunIperf("down", false).Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; a nonzero exit is a failed run")
	}
	if res.DownloadBytes != 125000000 {
		t.Errorf("failed run carries %d download bytes, want 125000000 - the traffic is billed either way", res.DownloadBytes)
	}
	if res.Engine != "iperf3" || res.Server != "iperf3: 127.0.0.1" {
		t.Errorf("failed run engine/server = %q/%q, want iperf3 / iperf3: 127.0.0.1", res.Engine, res.Server)
	}
	// Accounting, never a measurement: a rate here would read as a real 0 Mbps run.
	if res.DownloadMbps != 0 || res.UploadMbps != 0 {
		t.Errorf("failed run reported %v/%v Mbps, want 0/0", res.DownloadMbps, res.UploadMbps)
	}
}

// Same for --bidir, whose single transfer carries both directions.
func TestIperfRunBidirFailureCarriesSpentBytes(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeBidirJSON), errors.New("exit status 1")
	})
	res, err := newRunIperf("bidir", false).Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; a nonzero exit is a failed run")
	}
	if res.DownloadBytes != 505000000 || res.UploadBytes != 199000000 {
		t.Errorf("failed bidir run carries %d/%d bytes, want 505000000/199000000",
			res.DownloadBytes, res.UploadBytes)
	}
}

// Every attempt's traffic counts: a direction retried once spent the link twice,
// so the usage carried out is the SUM, not the last attempt's.
func TestIperfRunFailureSumsRetryBytes(t *testing.T) {
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeDownJSON), errors.New("connection refused") // transient -> retried
	})
	i := newRunIperf("down", false)
	i.RetriesFn = func() int { return 1 } // two attempts
	res, err := i.Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; every attempt failed")
	}
	if res.DownloadBytes != 2*125000000 {
		t.Errorf("two failed attempts carry %d bytes, want %d - each attempt filled the link",
			res.DownloadBytes, 2*125000000)
	}
}

// A remote iperf3 server is untrusted, and a failed run's numbers get no second
// look from the measurement checks: a body whose rate fails the same plausibility
// bound the success path applies contributes nothing to the usage ledger.
func TestIperfFailedRunRejectsImplausibleUsage(t *testing.T) {
	const absurd = `{"end":{
		"streams":[{"sender":{"min_rtt":0}}],
		"sum_received":{"bytes":9000000000000,"bits_per_second":9e18}}}`
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(absurd), errors.New("exit status 1")
	})
	res, err := newRunIperf("down", false).Run(context.Background())
	if err == nil {
		t.Fatal("Run returned nil; a nonzero exit is a failed run")
	}
	if res.DownloadBytes != 0 {
		t.Errorf("implausible body billed %d bytes, want 0 - an absurd total must not inflate data usage", res.DownloadBytes)
	}
}

// End to end through the scheduler: a failed iperf3 run's usage reaches the
// store, tagged as the accounting row it is (failed, no verdict), so
// "data used" counts traffic the link really carried.
func TestSchedulerPersistsFailedIperfUsage(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	installFakeIperf(t, func([]string) ([]byte, error) {
		return []byte(fakeDownJSON), errors.New("exit status 1")
	})
	s := NewScheduler(newRunIperf("down", false), st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("RunOnce must still report the failure")
	}
	ctx := context.Background()
	used, err := st.SpeedDataUsageSince(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpeedDataUsageSince: %v", err)
	}
	if used != 125000000 {
		t.Fatalf("data usage after a failed iperf3 run = %d bytes, want 125000000", used)
	}
	rows, err := st.ExportTable(ctx, "speed")
	if err != nil {
		t.Fatalf("ExportTable: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("speed rows = %d, want exactly the one usage sample", len(rows))
	}
	row := rows[0]
	if row["engine"] != "iperf3" {
		t.Errorf("usage row engine = %v, want iperf3", row["engine"])
	}
	if row["failed"] != int64(1) || row["healthy"] != nil || row["down_mbps"] != float64(0) {
		t.Errorf("usage row must be accounting only, not a 0 Mbps measurement: %+v", row)
	}
}
