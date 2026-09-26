package web

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The runs CSV is the runs table as a file, so it lists the failed tests Record
// failed tests keeps - and it has to say they measured nothing. A failure that
// spent bytes is exactly what csvMbps reads as a measured direction and prints
// as 0.00, so the speed cells are blanked on the row's own marker.
func TestSpeedRunsCSVListsFailureRecordsWithBlankReadings(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b, fb := int64(1000), int64(400)
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 120, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "Real", Engine: "ookla", Trigger: "scheduled", DownBytes: &b, UpBytes: &b}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 60, Server: "Dead", Engine: "iperf3", Trigger: "manual",
		Failed: true, FailStage: "download", DownBytes: &fb, UpBytes: &fb}); err != nil {
		t.Fatal(err)
	}
	// Only a crafted backup can put a formula in the stage; the cell is still text.
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 30, Server: "Dead", Engine: "ookla",
		Failed: true, FailStage: "=cmd|' /C calc'!A0"}); err != nil {
		t.Fatal(err)
	}

	w := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if w.Code != 200 {
		t.Fatalf("csv export %d", w.Code)
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(rows) != 4 {
		t.Fatalf("parse csv (%v): want a header and three rows:\n%s", err, w.Body)
	}
	head := rows[0]
	if head[len(head)-1] != "fail_stage" || head[len(head)-2] != "race_racers" {
		t.Fatalf("header ends %v, want ... race_racers, fail_stage - new columns go at the END", head[len(head)-2:])
	}
	col := map[string]int{}
	for i, name := range head {
		col[name] = i
	}
	crafted, failed, meas := rows[1], rows[2], rows[3]

	for _, c := range []string{"download_mbps", "upload_mbps", "ping_ms", "healthy"} {
		if got := failed[col[c]]; got != "" {
			t.Errorf("failed test's %s = %q, want blank - it measured nothing", c, got)
		}
	}
	if failed[col["download_bytes"]] != "400" || failed[col["upload_bytes"]] != "400" {
		t.Errorf("failed test's bytes = %q/%q, want 400/400 - what it spent is real", failed[col["download_bytes"]], failed[col["upload_bytes"]])
	}
	if failed[col["fail_stage"]] != "download" || failed[col["engine"]] != "iperf3" {
		t.Errorf("failed test's stage/engine = %q/%q, want download/iperf3", failed[col["fail_stage"]], failed[col["engine"]])
	}
	if got := crafted[col["fail_stage"]]; got != "'=cmd|' /C calc'!A0" {
		t.Errorf("crafted stage cell = %q, want it defused with a leading quote", got)
	}
	if meas[col["fail_stage"]] != "" || meas[col["download_mbps"]] != "94.50" {
		t.Errorf("measurement's fail_stage/download = %q/%q, want blank/94.50", meas[col["fail_stage"]], meas[col["download_mbps"]])
	}
}
