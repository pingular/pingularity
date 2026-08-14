package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// seedFamilyRows inserts one run carrying the I5/I9 fields and one without,
// mimicking a mixed history (new runs beside pre-field rows / Ookla runs).
func seedFamilyRows(t *testing.T, s *Server) {
	t.Helper()
	ctx := context.Background()
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: 1000, DownMbps: 50, Server: "plain"}); err != nil {
		t.Fatalf("seed plain: %v", err)
	}
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: 2000, DownMbps: 80, Server: "tagged", IPFamily: "6", UDPDirection: "down"}); err != nil {
		t.Fatalf("seed tagged: %v", err)
	}
}

// /api/speed/runs must expose ip_family/udp_direction on runs that recorded
// them and OMIT the keys on runs that didn't - absence is the UI's "don't
// label" signal, so an empty-string key would be a fake value.
func TestSpeedRunsAPIFamilyAndDirectionPresence(t *testing.T) {
	s := newTestServer(t)
	seedFamilyRows(t, s)
	w := do(t, s.Handler(), "GET", "/api/speed/runs", "")
	if w.Code != 200 {
		t.Fatalf("runs %d: %s", w.Code, w.Body)
	}
	var out struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(out.Runs))
	}
	byServer := map[string]map[string]any{}
	for _, r := range out.Runs {
		byServer[r["server"].(string)] = r
	}
	tagged := byServer["tagged"]
	if tagged["ip_family"] != "6" || tagged["udp_direction"] != "down" {
		t.Fatalf("tagged run must carry both fields: %v", tagged)
	}
	plain := byServer["plain"]
	for _, k := range []string{"ip_family", "udp_direction"} {
		if _, ok := plain[k]; ok {
			t.Fatalf("run without a recorded %s must omit the key: %v", k, plain)
		}
	}
}

// The CSV export gains both columns (appended, so positional consumers of the
// existing columns keep working); a run without them exports blank cells.
func TestSpeedRunsCSVFamilyAndDirectionColumns(t *testing.T) {
	s := newTestServer(t)
	seedFamilyRows(t, s)
	w := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if w.Code != 200 {
		t.Fatalf("csv %d: %s", w.Code, w.Body)
	}
	recs, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("csv rows = %d, want header + 2", len(recs))
	}
	head := recs[0]
	famIdx, dirIdx := -1, -1
	for i, c := range head {
		switch c {
		case "ip_family":
			famIdx = i
		case "udp_direction":
			dirIdx = i
		}
	}
	if famIdx < 0 || dirIdx < 0 {
		t.Fatalf("header missing ip_family/udp_direction: %v", head)
	}
	// Newest first: recs[1] is the tagged run, recs[2] the plain one.
	if recs[1][famIdx] != "6" || recs[1][dirIdx] != "down" {
		t.Fatalf("tagged row: family=%q direction=%q, want 6/down", recs[1][famIdx], recs[1][dirIdx])
	}
	if recs[2][famIdx] != "" || recs[2][dirIdx] != "" {
		t.Fatalf("plain row must export blank cells, got family=%q direction=%q", recs[2][famIdx], recs[2][dirIdx])
	}
}
