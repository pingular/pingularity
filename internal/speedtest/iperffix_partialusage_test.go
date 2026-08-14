package speedtest

import (
	"context"
	"errors"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// The failed-run usage salvage must stop at the failure exits. On the SUCCESS
// path a byte count is not accounting, it is the scheduler's "this direction
// ran" predicate (evalThresholds, thresholdsUnmeasured, thresholdsMeasurable all
// key off DownBytes/UpBytes being non-nil) - so parking a failed direction's
// spend in res.UploadBytes would turn every download-only server into a nightly
// "upload 0.0 < N Mbps" breach. This is the counter-test to
// TestIperfRunFailureCarriesSpentBytes: same salvage, opposite requirement.
func TestIperfPartialKeepsFailedDirectionUnmeasured(t *testing.T) {
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") {
			return []byte(fakeDownJSON), nil // the download measures
		}
		// The upload moved real data and THEN died: a body carrying totals
		// alongside a nonzero exit - exactly the case the salvage exists for.
		return []byte(fakeUpJSON), errors.New("exit status 1")
	})
	res, err := newRunIperf("both", false).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v, want nil (the measured download is kept)", err)
	}
	if res.UploadBytes != 0 {
		t.Errorf("kept partial carries %d upload bytes, want 0 - a failed direction must read as unmeasured, never as a real 0 Mbps upload", res.UploadBytes)
	}
	if res.DownloadBytes != 125000000 || res.DownloadMbps != 800 {
		t.Errorf("download = %d bytes / %v Mbps, want 125000000 / 800", res.DownloadBytes, res.DownloadMbps)
	}
	// The consequence, spelled out: the sample RunOnce would build from this
	// result must not be judgeable on upload, or the threshold fires on a
	// direction that never measured.
	sp := store.SpeedSample{
		DownMbps: res.DownloadMbps, UpMbps: res.UploadMbps,
		DownBytes: bytesPtr(res.DownloadBytes), UpBytes: bytesPtr(res.UploadBytes),
	}
	if f := evalThresholds(sp, settings.Thresholds{UpMbps: 5}); len(f) != 0 {
		t.Errorf("kept partial breaches %v; an upload that never measured cannot breach an upload threshold", f)
	}
}
