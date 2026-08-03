package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

func seedSelectionRun(t *testing.T, s *Server, ts int64) {
	t.Helper()
	ctx := context.Background()
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: ts, DownMbps: 500, UpMbps: 480, PingMS: 8, Server: "S2, N2", ServerID: "2"}); err != nil {
		t.Fatalf("seed speed: %v", err)
	}
	if err := s.store.InsertSpeedServers(ctx, []store.SpeedServerRow{
		{RunTS: ts, ServerID: "1", Server: "S1, N1", RankOrder: 1, Selected: true, Measured: true, Score: 90},
		{RunTS: ts, ServerID: "2", Server: "S2, N2", RankOrder: 2, Selected: true, Measured: true, Score: 458, Winner: true, WinReason: "score"},
	}); err != nil {
		t.Fatalf("seed speed_servers: %v", err)
	}
}

// The report's only reachable surface in production (the DB usually sits
// inside a Docker volume): a known run answers with its rows, an unknown ts is
// a 404, and a run that predates the feature answers 200 with an empty list -
// missing explanation is normal, missing run is the error.
func TestSpeedRunServersAPIServesTheReport(t *testing.T) {
	s := newTestServer(t)
	seedSelectionRun(t, s, 5000)

	w := do(t, s.Handler(), "GET", "/api/speed/runs/servers?ts=5000", "")
	if w.Code != http.StatusOK {
		t.Fatalf("known run: HTTP %d: %s", w.Code, w.Body)
	}
	var resp struct {
		TS      int64                  `json:"ts"`
		Servers []store.SpeedServerRow `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TS != 5000 || len(resp.Servers) != 2 {
		t.Fatalf("resp = ts %d with %d rows, want 5000 with 2", resp.TS, len(resp.Servers))
	}
	if !resp.Servers[1].Winner || resp.Servers[1].WinReason != "score" {
		t.Errorf("winner row lost across the API: %+v", resp.Servers[1])
	}

	if w := do(t, s.Handler(), "GET", "/api/speed/runs/servers?ts=99999", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown run: HTTP %d, want 404", w.Code)
	}
	if w := do(t, s.Handler(), "GET", "/api/speed/runs/servers?ts=bogus", ""); w.Code != http.StatusBadRequest {
		t.Errorf("bad ts: HTTP %d, want 400", w.Code)
	}

	// A run with no report (pre-feature history, an iperf3 run, an old backup):
	// 200 with [], never a 404 and never null.
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{TS: 6000, DownMbps: 1, Server: "S", ServerID: "9"}); err != nil {
		t.Fatalf("seed bare run: %v", err)
	}
	w = do(t, s.Handler(), "GET", "/api/speed/runs/servers?ts=6000", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"servers":[]`) {
		t.Errorf("report-less run: HTTP %d body %s, want 200 with an empty servers list", w.Code, w.Body)
	}
}

// The speed category now spans two tables; a backup must carry the selection
// reports and a restore must land them - and a LEGACY export (no speed_servers
// key) must import cleanly, because every backup taken before this feature is
// one.
func TestSelectionReportSurvivesExportImport(t *testing.T) {
	s := newTestServer(t)
	seedSelectionRun(t, s, 5000)

	w := do(t, s.Handler(), "GET", "/api/export?speed=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export: HTTP %d: %s", w.Code, w.Body)
	}
	exported := w.Body.String()
	if !strings.Contains(exported, `"speed_servers":[`) {
		t.Fatalf("speed export must carry the selection reports:\n%s", exported)
	}

	s2 := newTestServer(t)
	if w := do(t, s2.Handler(), "POST", "/api/import?speed=1", exported); w.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", w.Code, w.Body)
	}
	rows, err := s2.store.SpeedServers(context.Background(), 5000)
	if err != nil || len(rows) != 2 {
		t.Fatalf("restored rows = %d (err %v), want 2", len(rows), err)
	}
	if !rows[1].Winner || rows[1].WinReason != "score" {
		t.Errorf("winner row corrupted by the round-trip: %+v", rows[1])
	}
	// Idempotent like the other time-series merges: importing the same file
	// twice must not double the report.
	if w := do(t, s2.Handler(), "POST", "/api/import?speed=1", exported); w.Code != http.StatusOK {
		t.Fatalf("re-import: HTTP %d: %s", w.Code, w.Body)
	}
	if rows, _ := s2.store.SpeedServers(context.Background(), 5000); len(rows) != 2 {
		t.Errorf("re-import doubled the report: %d rows", len(rows))
	}

	// The legacy shape: a v1 speed-only file from a build that predates the
	// table. Import must succeed and simply have no reports.
	legacy := fmt.Sprintf(`{"pingularity_export":1,"categories":["speed"],"speed":[{"ts":7000,"down_mbps":1,"up_mbps":1,"ping_ms":9,"server":"L","server_id":"7"}]}`)
	s3 := newTestServer(t)
	if w := do(t, s3.Handler(), "POST", "/api/import?speed=1", legacy); w.Code != http.StatusOK {
		t.Fatalf("legacy import: HTTP %d: %s", w.Code, w.Body)
	}
	if cnt, _ := s3.store.TableCounts(context.Background()); cnt["speed"] != 1 || cnt["speed_servers"] != 0 {
		t.Errorf("legacy import: speed=%d speed_servers=%d, want 1/0", cnt["speed"], cnt["speed_servers"])
	}
}
