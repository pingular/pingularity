package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// Zero is the "not probed" sentinel for latency, not a measurement: a
// successful iperf3 run that moved real bytes can report ping_ms=0. The tiles,
// the runs table, the charts and /metrics all treat it as absent. The CSV was
// the last surface still writing a literal "0.0", which reads as a perfect
// round trip - and is worse in an export than on screen, because a spreadsheet
// will average it in without anyone noticing.
func TestSpeedCSVBlanksAnUnprobedPing(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	i64 := func(v int64) *int64 { return &v }

	// A real iperf3 run: bytes moved in both directions, no latency probe.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-2 * time.Minute).Unix(), Server: "lab", ServerID: "1",
		DownMbps: 940, UpMbps: 910, PingMS: 0,
		DownBytes: i64(1 << 30), UpBytes: i64(1 << 30),
		Trigger: "scheduled", Engine: "iperf3",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}
	// ...and an ordinary run that DID measure latency, so the column is not
	// simply always blank.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Add(-time.Minute).Unix(), Server: "ookla", ServerID: "2",
		DownMbps: 100, UpMbps: 20, PingMS: 12.5,
		DownBytes: i64(1 << 20), UpBytes: i64(1 << 19),
		Trigger: "scheduled", Engine: "ookla",
	}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}

	rr := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if rr.Code != 200 {
		t.Fatalf("csv: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(rr.Body.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("want a header and two rows, got %d lines", len(lines))
	}
	header := strings.Split(lines[0], ",")
	pingCol := -1
	for i, h := range header {
		if h == "ping_ms" {
			pingCol = i
		}
	}
	if pingCol < 0 {
		t.Fatalf("no ping_ms column in %q", lines[0])
	}
	var sawBlank, sawReal bool
	for _, line := range lines[1:] {
		cells := strings.Split(line, ",")
		if pingCol >= len(cells) {
			continue
		}
		switch cells[pingCol] {
		case "":
			sawBlank = true
		case "12.5":
			sawReal = true
		case "0.0":
			t.Errorf("the unprobed run exported ping_ms=0.0: a spreadsheet reads that as a perfect round trip and averages it in\n%s", line)
		}
	}
	if !sawBlank {
		t.Error("the run that never probed latency did not export a blank ping_ms")
	}
	if !sawReal {
		t.Error("the run that DID measure latency lost its value; the fix must not blank real readings")
	}
}
