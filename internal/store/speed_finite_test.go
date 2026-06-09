package store

import (
	"context"
	"encoding/json"
	"math"
	"testing"
)

// A non-finite stored speed measurement (NaN, which the driver binds as NULL, or
// a real ±Inf) must read back finite, so no single poisoned row can break a
// speed reader: /api/speed json.Encode or CSV export.
func TestScanSpeedCoercesNonFinite(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	inf := math.Inf(1)
	if err := st.InsertSpeed(ctx, SpeedSample{
		TS: 1000, DownMbps: inf, UpMbps: math.Inf(-1), PingMS: math.NaN(),
		JitterMS: &inf, Server: "x",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := st.SpeedRuns(ctx, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	r := got[0]
	if r.DownMbps != 0 || r.UpMbps != 0 || r.PingMS != 0 {
		t.Errorf("non-finite scalars not coerced: down=%v up=%v ping=%v", r.DownMbps, r.UpMbps, r.PingMS)
	}
	if r.JitterMS != nil {
		t.Errorf("non-finite jitter pointer not dropped: %v", *r.JitterMS)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatalf("a row with a non-finite measurement must still json-encode (/api/speed): %v", err)
	}
}

// The export path must sanitize stored non-finite floats too: a raw ±Inf would
// abort json.Encode mid-stream and silently truncate the backup file.
func TestExportCoercesNonFinite(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	inf := math.Inf(1)
	if err := st.InsertSpeed(ctx, SpeedSample{TS: 1000, DownMbps: inf, JitterMS: &inf, Server: "x"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := st.ExportTable(ctx, "speed")
	if err != nil || len(rows) != 1 {
		t.Fatalf("export: err=%v rows=%d", err, len(rows))
	}
	if rows[0]["down_mbps"] != nil {
		t.Errorf("down_mbps = %v, want nil (stored +Inf must export as NULL)", rows[0]["down_mbps"])
	}
	if _, err := json.Marshal(rows[0]); err != nil {
		t.Fatalf("an exported row must json-encode: %v", err)
	}
}
