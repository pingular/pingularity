package web

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// gaugeValue reads an unlabelled gauge out of a scrape, and says whether the
// scrape carried it at all - absence being the thing these tests are about.
func gaugeValue(body, name string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, name+" ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, name+" ")), 64)
		return v, err == nil
	}
	return 0, false
}

// A nil byte count means "this direction was not measured" only when the figure
// beside it is zero. It is also what an OLD row carries: the byte columns are
// absent on a database from before they existed, on a restored backup whose speed
// rows had none, and on rows the at-rest integer repair strips. The dashboard
// cards and the CSV export both show such a run; /metrics used to be the one
// surface that hid it, so a scrape said "no speedtest data" beside a dashboard
// reading 323 Mbps. The byte gauge itself still goes absent - that number really
// is missing - and a skipped direction still emits nothing.
func TestMetricsKeepsAMeasuredSpeedWithNoByteCounts(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), Server: "legacy", ServerID: "1",
		DownMbps: 322.62, UpMbps: 25.04, PingMS: 13.9,
		Trigger: "scheduled", Engine: "ookla",
	}); err != nil {
		t.Fatalf("seed legacy speed: %v", err)
	}
	body := scrape(t, s)

	for _, c := range []struct {
		name string
		want float64
	}{
		{"pingularity_speed_download_mbps", 322.62},
		{"pingularity_speed_upload_mbps", 25.04},
		{"pingularity_speed_download_bytes_per_second", 322.62 * 1e6 / 8},
		{"pingularity_speed_upload_bytes_per_second", 25.04 * 1e6 / 8},
	} {
		got, ok := gaugeValue(body, c.name)
		if !ok {
			t.Errorf("%s is absent for a run with a real throughput and no byte counts; the dashboard "+
				"and the CSV both show that run\n--- body ---\n%s", c.name, body)
			continue
		}
		if math.Abs(got-c.want) > 0.5 {
			t.Errorf("%s = %g, want %g", c.name, got, c.want)
		}
	}
	// The byte counter itself has nothing to report, and says so by staying away.
	if strings.Contains(body, "pingularity_speed_last_run_bytes") {
		t.Errorf("last_run_bytes must stay absent when the row has no byte columns\n%s", body)
	}

	// A direction the engine skipped is still not a measurement: no bytes AND a
	// zeroed figure. Emitting 0.0 there is a permanent false "below threshold".
	s2 := newMetricsServer(t)
	dn := int64(1_000_000)
	if err := s2.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 300, UpMbps: 0, PingMS: 8, DownBytes: &dn,
	}); err != nil {
		t.Fatalf("seed down-only speed: %v", err)
	}
	partial := scrape(t, s2)
	for _, name := range []string{"pingularity_speed_upload_mbps", "pingularity_speed_upload_bytes_per_second"} {
		if _, ok := gaugeValue(partial, name); ok {
			t.Errorf("%s must be ABSENT when upload was skipped\n%s", name, partial)
		}
	}

	// The two unit systems describe the same reading, and docs/metrics.md tells a
	// dashboard to pick whichever it wants - so they must never disagree about
	// whether the reading exists.
	for _, b := range []string{body, partial} {
		for _, pair := range [][2]string{
			{"pingularity_speed_download_mbps", "pingularity_speed_download_bytes_per_second"},
			{"pingularity_speed_upload_mbps", "pingularity_speed_upload_bytes_per_second"},
		} {
			_, human := gaugeValue(b, pair[0])
			_, base := gaugeValue(b, pair[1])
			if human != base {
				t.Errorf("%s present=%v but %s present=%v; they are the same number in two units\n%s",
					pair[0], human, pair[1], base, b)
			}
		}
	}
}
