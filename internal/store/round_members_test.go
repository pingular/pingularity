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
// so re-learning it costs a whole measurement turn. A LAPSED conviction
// outlives it too, for `keep`: it excludes nobody, but how many chances a
// server has already had decides how long its next exclusion runs.
func TestServerHealthOutlivesTheProcess(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const keep = 7 * 24 * time.Hour
	now := time.Now().Unix()
	if rows, err := st.ServerHealth(ctx, keep); err != nil || len(rows) != 0 {
		t.Fatalf("fresh store: %v (%v), want none", rows, err)
	}
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "16045", Expires: now + 12*3600, Fails: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "lapsed", Expires: now - 60, Fails: 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "ancient", Expires: now - int64(keep.Seconds()) - 60, Fails: 3}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ServerHealth(ctx, keep)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ServerHealth{}
	for _, r := range rows {
		got[r.ServerID] = r
	}
	if len(got) != 2 || got["16045"].Fails != 2 || got["lapsed"].Fails != 2 {
		t.Fatalf("read back %+v, want the live conviction AND the recently lapsed one - the count is what escalates the next exclusion", rows)
	}
	if _, ok := got["ancient"]; ok {
		t.Errorf("a conviction older than keep is not evidence of anything: %+v", rows)
	}
	// The sweep is not just a filter: the ancient row is gone from the table.
	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM server_health`).Scan(&n); err != nil || n != 2 {
		t.Errorf("%d rows left (%v), want only the one past keep dropped", n, err)
	}
	// A re-conviction refreshes rather than duplicating.
	if err := st.SaveServerHealth(ctx, ServerHealth{ServerID: "16045", Expires: now + 24*3600, Fails: 3}); err != nil {
		t.Fatal(err)
	}
	rows, _ = st.ServerHealth(ctx, keep)
	for _, r := range rows {
		if r.ServerID == "16045" && (r.Expires != now+24*3600 || r.Fails != 3) {
			t.Errorf("after a second conviction: %+v, want one refreshed row", r)
		}
	}
	if len(rows) != 2 {
		t.Errorf("after a second conviction: %d rows, want the same two", len(rows))
	}
}

// Two runs can share a second in a database that was imported, or written
// before InsertSpeedTS allocated the first FREE second. The UI's confirm names
// one run and its toast reports one, so exactly one has to go - keyed on ts
// alone, this statement destroyed every row on that second.
func TestDeletingOneRunLeavesTheOtherRunThatSecond(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const ts = 1787926012
	for _, id := range []string{"58956", "1993"} {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, server_id) VALUES (?,?,?,?,?,?)`,
			ts, 100.0, 20.0, 9.0, "srv "+id, id); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.DeleteSpeed(ctx, ts)
	if err != nil {
		t.Fatal(err)
	}
	var left int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE ts = ?`, ts).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if n != 1 || left != 1 {
		t.Errorf("deleted %d rows leaving %d on that second, want exactly one deleted and one left - the confirm said 'this run'", n, left)
	}
	// The one still there is a real run, not a husk: deleting it works too.
	if n2, err := st.DeleteSpeed(ctx, ts); err != nil || n2 != 1 {
		t.Errorf("second delete removed %d (%v), want the remaining run", n2, err)
	}
}

// A window under budget was never thinned, so nothing may tell the dashboard it
// was: `total` has to count what the query can actually return, and pass 2
// collapses same-second rows. One legacy collision put a dashed "sampled"
// border and a "mean of N charted, of M in range" caveat on every average.
func TestASharedSecondIsNotASampledWindow(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	ins := func(ts int64, id string) {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, server_id) VALUES (?,?,?,?,?,?)`,
			ts, 100.0, 20.0, 9.0, "srv "+id, id); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 15; i++ {
		ins(now-int64(i)*600, "a")
	}
	ins(now-5*600, "b") // the collision: two runs, one second

	rows, total, err := st.SpeedHistoryBudget(ctx, time.Unix(now-86400, 0), time.Time{}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if total != len(rows) {
		t.Errorf("%d rows returned against a total of %d under a 1500 budget: the dashboard reads that gap as thinning and labels every average a sample", len(rows), total)
	}
	if len(rows) != 15 {
		t.Errorf("returned %d rows, want the 15 distinct seconds", len(rows))
	}
}

// The chart's sampling caveat compares the runs it charted against the runs in
// range. The total that drives X-Sampled counts every row, round members
// included, so comparing one against the other described a coverage that was
// never true - with the shipped Best-of settings, off by about threefold.
func TestSpeedRunCountCountsRunsNotRoundMembers(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	ins := func(ts int64, round *int64) {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, server_id, round_ts) VALUES (?,?,?,?,?,?,?)`,
			ts, 100.0, 20.0, 9.0, "srv", "1", round); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 4; i++ { // four runs, each with two losers kept
		w := now - int64(i)*600
		ins(w, nil)
		ins(w-1, &w)
		ins(w-2, &w)
	}
	runs, err := st.SpeedRunCount(ctx, time.Unix(now-86400, 0), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 4 {
		t.Errorf("counted %d, want the 4 runs - the other 8 rows are the servers those rounds measured", runs)
	}
	_, total, err := st.SpeedHistoryBudget(ctx, time.Unix(now-86400, 0), time.Time{}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if total != 12 {
		t.Errorf("the row total is %d, want all 12 - it is what decides whether the window was thinned", total)
	}
}
