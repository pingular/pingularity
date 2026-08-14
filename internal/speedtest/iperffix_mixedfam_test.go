package speedtest

import (
	"context"
	"testing"
)

// A sequential "both" run is two separate iperf3 processes, and with a
// dual-stack hostname plus family "auto" they can genuinely resolve to
// different families (rotating A/AAAA answers, a flapping happy-eyeballs
// path). Fixtures mirror the start.connected block real iperf3 emits.
const (
	// An upload (forward flow) whose control connection landed on IPv6.
	mixedUpV6JSON = `{"start":{"connected":[{"remote_host":"2001:db8::9","remote_port":5201}]},"end":{
		"streams":[{"sender":{"min_rtt":18250}}],
		"sum_sent":    {"bytes":50000000,"bits_per_second":400000000},
		"sum_received":{"bytes":45000000,"bits_per_second":360000000}}}`
)

// mixedFamRun drives a "both" run where the download and upload processes
// report the given canned bodies, and returns the recorded Result.
func mixedFamRun(t *testing.T, downBody, upBody string) Result {
	t.Helper()
	installFakeIperf(t, func(args []string) ([]byte, error) {
		if argvHas(args, "--reverse") {
			return []byte(downBody), nil
		}
		return []byte(upBody), nil
	})
	res, err := newRunIperf("both", false).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// When the two directions measured over DIFFERENT families, recording the
// download's family silently misdescribes the upload. The honest record is
// "mixed": both values are real, neither speaks for the whole row.
func TestIperfBothDirectionsFamilyDisagreementIsMixed(t *testing.T) {
	res := mixedFamRun(t, fakeDownV4JSON, mixedUpV6JSON)
	if res.IPFamily != "mixed" {
		t.Errorf("IPFamily = %q, want mixed (download measured IPv4, upload IPv6 - neither may claim the row)", res.IPFamily)
	}
	// Both directions really measured; mixed must not cost either reading.
	if res.DownloadMbps != 800 || res.UploadMbps != 360 {
		t.Errorf("speeds = %v/%v Mbps, want 800/360 - family bookkeeping must not disturb the measurements", res.DownloadMbps, res.UploadMbps)
	}
}

// Agreement keeps the plain family, exactly as before the mixed vocabulary
// existed - old rows and same-family runs must be untouched by the fix.
func TestIperfBothDirectionsFamilyAgreementUnchanged(t *testing.T) {
	res := mixedFamRun(t, fakeDownV4JSON, fakeUpV4JSON)
	if res.IPFamily != "4" {
		t.Errorf("IPFamily = %q, want 4 when both directions agree", res.IPFamily)
	}
}

// One direction with no start.connected block is UNKNOWN, not a disagreement:
// the known side speaks alone, and mixed is reserved for two known, differing
// families - never inferred from an absence.
func TestIperfUnknownSideIsNotADisagreement(t *testing.T) {
	t.Run("upload unknown, download v4", func(t *testing.T) {
		if res := mixedFamRun(t, fakeDownV4JSON, fakeUpJSON); res.IPFamily != "4" {
			t.Errorf("IPFamily = %q, want 4 (the one recorded family)", res.IPFamily)
		}
	})
	t.Run("download unknown, upload v6", func(t *testing.T) {
		if res := mixedFamRun(t, fakeDownJSON, mixedUpV6JSON); res.IPFamily != "6" {
			t.Errorf("IPFamily = %q, want 6 (the one recorded family)", res.IPFamily)
		}
	})
}
