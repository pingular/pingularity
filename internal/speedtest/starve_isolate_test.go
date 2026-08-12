package speedtest

import (
	"os"
	"testing"
)

// Isolate: is ONE worker enough on a 2 Mbps link, ignoring the retry path?
// 999490 B / 250000 Bps = 4.0 s against a 15 s window.
//
// This is a MEASUREMENT, not an assertion - it asserts nothing and would pass
// with the starvation rescue deleted. It is kept because its numbers are the
// evidence for a design decision that will be questioned later: the rescue drops
// to a single stream, and this shows why that works (1 worker measures a slow
// link fine where 4 starve). Gated because it costs ~42 s under -race, which CI
// runs, and that is 1/3 of the whole package's increase for zero coverage.
//
//	SPEEDTEST_BENCH=1 go test ./internal/speedtest/ -run TestStarveIsolate -v
func TestStarveIsolateWorkerCounts(t *testing.T) {
	if os.Getenv("SPEEDTEST_BENCH") != "1" {
		t.Skip("set SPEEDTEST_BENCH=1 - measurement, not a behavioural assertion")
	}
	for _, n := range []int{1, 2, 4} {
		t.Run(string(rune('0'+n))+"worker", func(t *testing.T) {
			s := &naServer{rateBPS: 250000, capture: ooklaCaptureTime, threads: n}
			res, err := runNACase(t, s)
			t.Logf("  threads=%d -> UploadMbps=%.2f err=%v", n, res.UploadMbps, err)
		})
	}
}
