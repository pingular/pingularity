package speedtest

import (
	"context"
	"testing"
)

// RunOnce must persist the winner's selection report next to the speed row,
// keyed by the SAME ts - and treat a run without one (iperf3, every plain-Run
// fake) as normal, not as an error or an empty insert.
func TestRunOnceStoresTheSelectionReportWithTheRun(t *testing.T) {
	ping := 7.5
	s, st := newRunOnceScheduler(t, Result{
		DownloadMbps: 500, UploadMbps: 480, PingMS: 8, Server: "S2", ServerID: "2",
		DownloadBytes: 6000, UploadBytes: 600,
		Selection: &SelectionReport{Candidates: []CandidateReport{
			{ServerID: "1", Server: "S1", RankOrder: 1, RankPingMS: &ping, Selected: true, Measured: true,
				DownMbps: 100, UpMbps: 100, PingMS: 10, DownloadBytes: 1000, UploadBytes: 100,
				CapacityMbps: 100, BelievedCapacityMbps: 100, Score: 90},
			{ServerID: "2", Server: "S2", RankOrder: 2, Selected: true, Measured: true,
				DownMbps: 500, UpMbps: 480, PingMS: 8, DownloadBytes: 2000, UploadBytes: 200,
				CapacityMbps: 493, BelievedCapacityMbps: 493, Score: 458, Winner: true, WinReason: "score"},
			{ServerID: "3", Server: "S3", RankOrder: 3},
		}},
	})
	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows, err := st.SpeedServers(context.Background(), sp.TS)
	if err != nil {
		t.Fatalf("SpeedServers: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("persisted rows = %d, want 3 (keyed by the run's own ts %d)", len(rows), sp.TS)
	}
	var winner *int
	for i, r := range rows {
		if r.Winner {
			winner = &i
		}
	}
	if winner == nil || rows[*winner].ServerID != sp.ServerID {
		t.Fatalf("winner row missing or mismatched: rows=%+v speed server_id=%s", rows, sp.ServerID)
	}
	w := rows[*winner]
	if w.DownloadBytes != 2000 || w.WinReason != "score" || w.Score == 0 {
		t.Errorf("winner row lost data across persistence: %+v", w)
	}
	if r := rows[0]; r.RankPingMS == nil || *r.RankPingMS != ping {
		t.Errorf("rank ping did not round-trip: %+v", r)
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed_servers"] != 3 {
		t.Errorf("TableCounts[speed_servers] = %d, want 3 (the count every clear/prune test trusts)", cnt["speed_servers"])
	}
}

// Engines without server selection (iperf3, plain fakes) produce no report;
// the run must persist exactly as before with zero selection rows.
func TestRunOnceWithoutAReportStoresNoSelectionRows(t *testing.T) {
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, Server: "S", ServerID: "1"})
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	cnt, _ := st.TableCounts(context.Background())
	if cnt["speed"] != 1 || cnt["speed_servers"] != 0 {
		t.Errorf("speed=%d speed_servers=%d, want 1/0", cnt["speed"], cnt["speed_servers"])
	}
}
