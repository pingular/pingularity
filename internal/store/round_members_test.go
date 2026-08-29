package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A round member (round_ts set) is history the runs table and the exports
// show, but never a run's result: the reads that decide something skip it,
// deleting its winner removes it, and deleting it alone removes only it.
func TestRoundMembersAreHistoryNotTheResult(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix() - 10
	b := func(n int64) *int64 { return &n }
	winTS, err := st.InsertSpeedTS(ctx, SpeedSample{TS: now, DownMbps: 300, UpMbps: 30, PingMS: 8, Server: "w", DownBytes: b(1000), UpBytes: b(100)})
	if err != nil {
		t.Fatal(err)
	}
	for i, ts := range []int64{now - 120, now - 60} {
		if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: ts, DownMbps: 50, UpMbps: 5, PingMS: 12, Server: "l", DownBytes: b(500), UpBytes: b(50), RoundTS: &winTS}); err != nil {
			t.Fatalf("member %d: %v", i, err)
		}
	}
	// A plain earlier run, to prove the reads still see ordinary rows.
	if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: now - 3600, DownMbps: 200, UpMbps: 20, PingMS: 9, Server: "old", DownBytes: b(700), UpBytes: b(70)}); err != nil {
		t.Fatal(err)
	}
	if latest, err := st.LatestSpeed(ctx); err != nil || latest == nil || latest.TS != winTS {
		t.Errorf("latest %+v (%v), want the winner", latest, err)
	}
	if ci, err := st.LatestConnInfo(ctx); err != nil || (ci != nil && ci.RoundTS != nil) {
		t.Errorf("latest conn info %+v (%v): never a member", ci, err)
	}
	hist, err := st.SpeedHistoryRange(ctx, time.Unix(now-7200, 0), time.Time{}, 0)
	if err != nil || len(hist) != 2 {
		t.Errorf("chart rows %d (%v), want 2: the two results, no members", len(hist), err)
	}
	// The chart's read shows the members too (drawn off the line), so its
	// count and rows include them; the digest's (SpeedHistoryRange above) does not.
	if rows, total, err := st.SpeedHistoryBudget(ctx, time.Unix(now-7200, 0), time.Time{}, 100); err != nil || total != 4 || len(rows) != 4 {
		t.Errorf("chart rows %d of %d (%v), want all 4 with the members", len(rows), total, err)
	} else {
		members := 0
		for _, r := range rows {
			if r.RoundTS != nil {
				members++
			}
		}
		if members != 2 {
			t.Errorf("%d members on the chart, want 2", members)
		}
	}
	if runs, err := st.SpeedRuns(ctx, 10, 0); err != nil || len(runs) != 4 {
		t.Errorf("runs table rows %d (%v), want all 4", len(runs), err)
	} else {
		members := 0
		for _, r := range runs {
			if r.RoundTS != nil && *r.RoundTS == winTS {
				members++
			}
		}
		if members != 2 {
			t.Errorf("%d members read back with their round, want 2", members)
		}
	}
	if n, _ := st.SpeedCount(ctx); n != 4 {
		t.Errorf("count %d, want 4", n)
	}
	// The estimate's per-run average sums each round: (1000+500+500 + 700)/2.
	if d, u, err := st.SpeedAvgBytes(ctx); err != nil || d != 1350 || u != 135 {
		t.Errorf("average bytes %d/%d (%v), want 1350/135", d, u, err)
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inUse, err := st.SpeedColumnsPastSchema4InUse(ctx, tx)
	tx.Rollback()
	if err != nil || !inUse["round_ts"] {
		t.Errorf("round_ts in use = %v (%v): a backup carrying members must stamp schema %d", inUse["round_ts"], err, SpeedColumnSchema("round_ts"))
	}
	if SpeedColumnSchema("round_ts") != 7 {
		t.Errorf("round_ts needs schema %d, want 7", SpeedColumnSchema("round_ts"))
	}
	// Deleting a member alone removes only it.
	if n, err := st.DeleteSpeed(ctx, now-120); err != nil || n != 1 {
		t.Errorf("delete a member: %d rows (%v), want 1", n, err)
	}
	if n, _ := st.SpeedCount(ctx); n != 3 {
		t.Errorf("count %d after deleting one member, want 3", n)
	}
	// Deleting the winner takes the rest of its round - and SAYS how many rows
	// went, so the page can tell the operator what it just destroyed rather
	// than reporting one deletion for a whole round.
	if n, err := st.DeleteSpeed(ctx, winTS); err != nil || n != 2 {
		t.Errorf("delete the winner: reported %d rows (%v), want 2 - the winner and the member still in its round", n, err)
	}
	if n, _ := st.SpeedCount(ctx); n != 1 {
		t.Errorf("count %d after deleting the winner, want the old run alone", n)
	}
}

// The data estimate needs to know how big the recorded rounds were, and the
// Best-of setting cannot say: it describes the next run, not the ones already
// measured. SpeedAvgServers counts them from the selection reports.
func TestSpeedAvgServersCountsWhatTheRunsMeasured(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix() - 100
	b := func(n int64) *int64 { return &n }
	run := func(ts int64, measured int) {
		if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: ts, DownMbps: 100, UpMbps: 10, PingMS: 9,
			Server: "s", DownBytes: b(1000), UpBytes: b(100)}); err != nil {
			t.Fatal(err)
		}
		rows := []SpeedServerRow{}
		for i := 0; i < measured; i++ {
			rows = append(rows, SpeedServerRow{RunTS: ts, ServerID: fmt.Sprint(i), Server: "S", RankOrder: int64(i), Measured: true})
		}
		rows = append(rows, SpeedServerRow{RunTS: ts, ServerID: "ranked-only", Server: "R", RankOrder: 9}) // ranked, never measured
		if len(rows) > 0 {
			if err := st.InsertSpeedServers(ctx, rows); err != nil {
				t.Fatal(err)
			}
		}
	}
	if avg, err := st.SpeedAvgServers(ctx); err != nil || avg != 1 {
		t.Errorf("empty store: %v (%v), want 1 - nothing to divide by", avg, err)
	}
	run(now-60, 3) // a Best-of-3 round
	run(now-30, 1) // a single-server run
	avg, err := st.SpeedAvgServers(ctx)
	if err != nil || avg != 2 {
		t.Errorf("avg servers %v (%v), want 2: only the MEASURED rows count, not the ranked ones", avg, err)
	}
	// A run with no report at all (iperf3, or a row older than the reports)
	// measured the one server it recorded.
	if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: now, DownMbps: 100, UpMbps: 10, PingMS: 9,
		Server: "iperf", Engine: "iperf3", DownBytes: b(1000), UpBytes: b(100)}); err != nil {
		t.Fatal(err)
	}
	if avg, err = st.SpeedAvgServers(ctx); err != nil || avg != 5.0/3.0 {
		t.Errorf("avg servers %v (%v), want (3+1+1)/3: a run without a report counts as one", avg, err)
	}
}

// The upload conviction has to outlive the process that earned it: the cheap
// health probe cannot see a server whose upload endpoint refuses everything,
// so re-learning it costs a whole measurement turn.
func TestServerHealthOutlivesTheProcess(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	if rows, err := st.ServerHealth(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("fresh store: %v (%v), want none", rows, err)
	}
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "16045", Expires: now + 12*3600, Fails: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "expired", Expires: now - 60, Fails: 2}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ServerHealth(ctx)
	if err != nil || len(rows) != 1 || rows[0].ServerID != "16045" || rows[0].Fails != 2 {
		t.Fatalf("read back %+v (%v), want the live conviction alone - the lapsed one is the server's second chance", rows, err)
	}
	// The sweep is not just a filter: the lapsed row is gone from the table.
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM server_health`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d rows left (%v), want the expired one dropped", n, err)
	}
	// A re-conviction refreshes rather than duplicating.
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "16045", Expires: now + 24*3600, Fails: 3}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ServerHealth(ctx)
	if len(rows) != 1 || rows[0].Expires != now+24*3600 || rows[0].Fails != 3 {
		t.Errorf("after a second conviction: %+v, want one refreshed row", rows)
	}
}
