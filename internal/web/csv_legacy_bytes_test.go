package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A nil byte count means "this direction was not measured" only when the figure
// beside it is zero, which is what a partial run records. It is also what an OLD
// row carries: the byte columns are absent on a database from before they existed,
// on a restored backup whose speed rows had none, and on rows the at-rest integer
// repair strips when it cannot read one. Blanking those exported a run with a real
// measured speed as an empty cell - a spreadsheet then shows a gap where the
// dashboard's own chart draws a line.
func TestSpeedCSVKeepsAMeasuredSpeedWithNoByteCounts(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	i64 := func(v int64) *int64 { return &v }

	// The old row: real throughput both ways, no byte columns at all.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-3 * time.Minute).Unix(), Server: "legacy", ServerID: "1",
		DownMbps: 322.62, UpMbps: 25.04, PingMS: 13.9,
		Trigger: "scheduled", Engine: "ookla",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}
	// A download-only run: the untested direction really was not measured, and
	// must still export blank rather than a made-up "0.00".
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-2 * time.Minute).Unix(), Server: "partial", ServerID: "2",
		DownMbps: 940, UpMbps: 0, PingMS: 5,
		DownBytes: i64(1 << 30),
		Trigger:   "manual", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}

	// The other side of the same condition: a run that really did measure 0.00 Mbps.
	// It has its byte counts (0 of them), so the zero IS the result - a line that
	// stalled outright - and blanking it would hide the worst run there is. The
	// Download tile prints "0 Mbps" for this row; the export must not print nothing.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-time.Minute).Unix(), Server: "stalled", ServerID: "3",
		DownMbps: 0, UpMbps: 0, PingMS: 21,
		DownBytes: i64(0), UpBytes: i64(0),
		Trigger: "manual", Engine: "ookla",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}

	rr := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if rr.Code != 200 {
		t.Fatalf("csv: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(lines) < 4 {
		t.Fatalf("want a header and three rows, got %d lines", len(lines))
	}
	header := strings.Split(lines[0], ",")
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("no %s column in %q", name, lines[0])
		return -1
	}
	dnCol, upCol, srvCol := col("download_mbps"), col("upload_mbps"), col("server")

	var sawLegacy, sawPartial, sawStalled bool
	for _, line := range lines[1:] {
		cells := strings.Split(line, ",")
		if srvCol >= len(cells) || upCol >= len(cells) {
			continue
		}
		switch cells[srvCol] {
		case "legacy":
			sawLegacy = true
			if cells[dnCol] != "322.62" || cells[upCol] != "25.04" {
				t.Errorf("the old row exported download=%q upload=%q; a measured speed with no byte counts is still a measurement\n%s",
					cells[dnCol], cells[upCol], line)
			}
		case "stalled":
			sawStalled = true
			if cells[dnCol] != "0.00" || cells[upCol] != "0.00" {
				t.Errorf("the stalled run exported download=%q upload=%q; it has its byte counts, so the "+
					"zero is the measurement, and the Download tile shows it as \"0 Mbps\"\n%s",
					cells[dnCol], cells[upCol], line)
			}
		case "partial":
			sawPartial = true
			if cells[upCol] != "" {
				t.Errorf("the untested upload direction exported %q; a 0 with no bytes was never measured\n%s", cells[upCol], line)
			}
			if cells[dnCol] != "940.00" {
				t.Errorf("the measured download direction lost its value (%q)\n%s", cells[dnCol], line)
			}
		}
	}
	if !sawLegacy || !sawPartial || !sawStalled {
		t.Fatalf("did not find all three rows in the export (legacy=%v partial=%v stalled=%v)",
			sawLegacy, sawPartial, sawStalled)
	}
}
