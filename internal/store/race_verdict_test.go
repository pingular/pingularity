package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The race verdict rides the speed row: written by InsertSpeed, read back by
// every speed read (scanSpeed), NULL - not "" or 0 - when a run carried none,
// so an old row and an iperf3 row have one spelling of "unrecorded".
func TestRaceVerdictRoundTripsOnTheSpeedRow(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	ms, n, la, lo := 8.4, int64(14), 45.5017, -73.5673
	ts := time.Now().Add(-time.Minute).Unix()
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts, DownMbps: 100, UpMbps: 20, PingMS: 9, Server: "EBOX, Montréal",
		ServerID: "1993", Trigger: "scheduled", Engine: "ookla",
		RaceOutcome: "decided", RaceOrigins: "exit:Montréal(8.4ms),isp:Toronto(15.1ms),geo(-)",
		RaceWinnerKind: "exit", RaceWinnerLabel: "Montréal", RaceWinnerMS: &ms, RaceRacers: &n,
		RaceWinnerLat: &la, RaceWinnerLon: &lo}); err != nil {
		t.Fatalf("InsertSpeed: %v", err)
	}
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts - 60, DownMbps: 90, UpMbps: 20, PingMS: 9, Server: "iperf host",
		Trigger: "scheduled", Engine: "iperf3"}); err != nil {
		t.Fatalf("InsertSpeed (no verdict): %v", err)
	}
	runs, err := st.SpeedRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("SpeedRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	got := runs[0]
	if got.RaceOutcome != "decided" || got.RaceOrigins != "exit:Montréal(8.4ms),isp:Toronto(15.1ms),geo(-)" ||
		got.RaceWinnerKind != "exit" || got.RaceWinnerLabel != "Montréal" ||
		got.RaceWinnerMS == nil || *got.RaceWinnerMS != 8.4 || got.RaceRacers == nil || *got.RaceRacers != 14 ||
		got.RaceWinnerLat == nil || *got.RaceWinnerLat != 45.5017 || got.RaceWinnerLon == nil || *got.RaceWinnerLon != -73.5673 {
		t.Errorf("verdict did not round-trip: %+v", got)
	}
	label, lat, lon, ok, err := st.LastDecidedRace(ctx)
	if err != nil || !ok || label != "Montréal" || lat != 45.5017 || lon != -73.5673 {
		t.Errorf("LastDecidedRace = %q %v,%v ok=%v err=%v; want the decided run's winner", label, lat, lon, ok, err)
	}
	bare := runs[1]
	if bare.RaceOutcome != "" || bare.RaceOrigins != "" || bare.RaceWinnerMS != nil || bare.RaceRacers != nil {
		t.Errorf("a run without a verdict must read back empty, got %+v", bare)
	}
	var nulls int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE ts = ? AND race_outcome IS NULL AND race_racers IS NULL`, ts-60).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 {
		t.Error("an unrecorded verdict must be stored as NULL, the spelling every pre-existing row has")
	}
}

// The race columns are v6 for the export, and the in-use check sees them only
// once a row carries one - the same content-dependent rule the v5 columns
// follow, or a backup taken by an install that never raced would stamp 6 and
// lock v5 readers out for nothing. (The ALTER TABLE migration that adds them
// to a pre-verdict database is exercised by TestOpenMigratesLegacySchema.)
func TestRaceColumnsCountAsInUseOnlyWhenCarried(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for _, c := range []string{"race_outcome", "race_origins", "race_winner_kind", "race_winner_label", "race_winner_ms", "race_racers", "race_winner_lat", "race_winner_lon"} {
		if SpeedColumnSchema(c) != 6 {
			t.Errorf("%s: schema %d, want 6", c, SpeedColumnSchema(c))
		}
	}
	if SpeedColumnSchema("failed") != 5 || SpeedColumnSchema("ts") != 0 {
		t.Error("the v5 columns and the original columns must keep their versions")
	}
	inUse := func() map[string]bool {
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		m, err := st.SpeedColumnsPastSchema4InUse(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	ts := time.Now().Add(-time.Minute).Unix()
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts, DownMbps: 1, UpMbps: 1, PingMS: 1, Server: "s", Engine: "ookla"}); err != nil {
		t.Fatal(err)
	}
	for c, used := range inUse() {
		if used {
			t.Errorf("%s reported in use on a table with no verdict", c)
		}
	}
	if err := st.InsertSpeed(ctx, SpeedSample{TS: ts + 1, DownMbps: 1, UpMbps: 1, PingMS: 1, Server: "s", Engine: "ookla",
		RaceOutcome: "bypassed_pin"}); err != nil {
		t.Fatal(err)
	}
	m := inUse()
	if !m["race_outcome"] {
		t.Error("race_outcome carried by a row must report in use, or the export would drop it and stamp low")
	}
	if m["race_origins"] || m["race_winner_ms"] || m["race_racers"] {
		t.Error("columns no row carries must not report in use: each is dropped and stamped individually")
	}
}

// LastSpeedWinners is the incumbent lookup: winners only, newest first, bounded.
func TestLastSpeedWinnersAreNewestFirstAndBounded(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	for i := 0; i < 5; i++ {
		ts := base + int64(i)*60
		if err := st.InsertSpeedServers(ctx, []SpeedServerRow{
			{RunTS: ts, ServerID: "loser", Server: "L", RankOrder: 2},
			{RunTS: ts, ServerID: "w" + string(rune('0'+i)), Server: "W", RankOrder: 1, Winner: true, WinReason: "fastest_ranked"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.LastSpeedWinners(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].ServerID != "w4" || rows[1].ServerID != "w3" || rows[2].ServerID != "w2" {
		t.Errorf("got %+v, want w4, w3, w2", rows)
	}
	for _, r := range rows {
		if !r.Winner {
			t.Errorf("%s is not a winner", r.ServerID)
		}
	}
	if rows, _ := st.LastSpeedWinners(ctx, 0); rows != nil {
		t.Error("a zero limit asks for nothing")
	}
}

// RecentRankPings is the saved pane's no-network ping: per server, the median
// of its last N answered ranking pings; a server never ranked - or never
// answered - is absent rather than 0.
func TestRecentRankPingsAreAPerServerMedianOfTheLastRuns(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	f := func(v float64) *float64 { return &v }
	// a: newest three 10, 12, 30 (window 3 -> median 12); the older 100 must not count.
	// b: one answered ping; c: ranked but never answered.
	for i, ms := range []*float64{f(100), f(30), f(12), f(10)} {
		ts := base + int64(i)*60
		rows := []SpeedServerRow{{RunTS: ts, ServerID: "a", Server: "A", RankOrder: 1, RankPingMS: ms}}
		if i == 1 {
			rows = append(rows, SpeedServerRow{RunTS: ts, ServerID: "b", Server: "B", RankOrder: 2, RankPingMS: f(7.5)})
		}
		rows = append(rows, SpeedServerRow{RunTS: ts, ServerID: "c", Server: "C", RankOrder: 3})
		if err := st.InsertSpeedServers(ctx, rows); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.RecentRankPings(ctx, []string{"a", "b", "c", "never"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != 12 {
		t.Errorf("a: median %v, want 12 (the newest three of 10, 12, 30; the older 100 is outside the window)", got["a"])
	}
	if got["b"] != 7.5 {
		t.Errorf("b: %v, want 7.5", got["b"])
	}
	if _, ok := got["c"]; ok {
		t.Error("c never answered a ranking ping and must be absent, not 0")
	}
	if _, ok := got["never"]; ok {
		t.Error("a server never ranked must be absent")
	}
	if got, _ := st.RecentRankPings(ctx, nil, 3); len(got) != 0 {
		t.Error("no ids, no answer")
	}
}

// The runs table's tag: the winner row's reason per run, one query per page,
// nothing for a run without a report.
func TestWinReasonsForReadsTheWinnerRowPerRun(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	if err := st.InsertSpeedServers(ctx, []SpeedServerRow{
		{RunTS: base, ServerID: "1", Server: "A", RankOrder: 1, Winner: true, WinReason: "incumbent"},
		{RunTS: base, ServerID: "2", Server: "B", RankOrder: 2},
		{RunTS: base + 60, ServerID: "3", Server: "C", RankOrder: 1, Winner: true, WinReason: "challenger_won"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.WinReasonsFor(ctx, []int64{base, base + 60, base + 120})
	if err != nil {
		t.Fatal(err)
	}
	if got[base] != "incumbent" || got[base+60] != "challenger_won" {
		t.Errorf("got %v", got)
	}
	if _, ok := got[base+120]; ok {
		t.Error("a run without a report must be absent")
	}
	if got, _ := st.WinReasonsFor(ctx, nil); len(got) != 0 {
		t.Error("no runs, no answer")
	}
}

// LastDecidedRace is the newest DECIDED race with a coordinate: a silent race,
// an unanchored winner (no coordinate), or a pinned run in between is skipped,
// and a history with none yields ok=false rather than an error.
func TestLastDecidedRaceSkipsWhatCannotBeEntered(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	if _, _, _, ok, err := st.LastDecidedRace(ctx); ok || err != nil {
		t.Fatalf("empty history: ok=%v err=%v", ok, err)
	}
	f := func(v float64) *float64 { return &v }
	base := time.Now().Add(-time.Hour).Unix()
	rows := []SpeedSample{
		{TS: base, Server: "s", Engine: "ookla", RaceOutcome: "decided", RaceWinnerLabel: "Montréal", RaceWinnerLat: f(45.5), RaceWinnerLon: f(-73.57)},
		{TS: base + 60, Server: "s", Engine: "ookla", RaceOutcome: "decided", RaceWinnerLabel: "your connection"}, // unanchored: no coordinate
		// silent WITH a coordinate: the engine never writes this (the scheduler
		// leaves lat/lon NULL on a silent row) - it pins the outcome filter as
		// defence in depth against an imported row.
		{TS: base + 120, Server: "s", Engine: "ookla", RaceOutcome: "silent", RaceWinnerLabel: "Toronto", RaceWinnerLat: f(43.65), RaceWinnerLon: f(-79.38)},
		{TS: base + 180, Server: "s", Engine: "ookla", RaceOutcome: "bypassed_pin"},
		// decided but unusable (0,0 - an imported backup's column check is a
		// type check only): skipped IN the query, so the walk reaches Montréal
		// instead of stopping here empty-handed.
		{TS: base + 240, Server: "s", Engine: "ookla", RaceOutcome: "decided", RaceWinnerLabel: "nowhere", RaceWinnerLat: f(0), RaceWinnerLon: f(0)},
		{TS: base + 300, Server: "s", Engine: "ookla", RaceOutcome: "decided", RaceWinnerLabel: "off the globe", RaceWinnerLat: f(999), RaceWinnerLon: f(0)},
	}
	for _, r := range rows {
		r.DownMbps, r.UpMbps, r.PingMS = 1, 1, 1
		if err := st.InsertSpeed(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	label, lat, lon, ok, err := st.LastDecidedRace(ctx)
	if err != nil || !ok || label != "Montréal" || lat != 45.5 || lon != -73.57 {
		t.Errorf("got %q %v,%v ok=%v err=%v; want the Montréal race - the newer rows carry nothing a race can enter", label, lat, lon, ok, err)
	}
}

// The per-server reads of the selection reports seek, they do not scan: the
// index they need exists on a fresh and on a migrated database.
func TestSpeedServersHasAPerServerIndex(t *testing.T) {
	st := openTestStore(t)
	rows, err := st.db.Query(`PRAGMA index_list(speed_servers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var seq int
		var name, origin string
		var unique, partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if name == "idx_speed_servers_server" {
			found = true
		}
	}
	if !found {
		t.Fatal("idx_speed_servers_server missing: RecentRankPings and RecentServerScores walk the whole table without it")
	}
	var plan string
	if err := st.db.QueryRow(`EXPLAIN QUERY PLAN SELECT rank_ping_ms FROM speed_servers WHERE server_id = ? AND rank_ping_ms IS NOT NULL AND rank_ping_ms > 0 ORDER BY run_ts DESC LIMIT 20`, "1993").Scan(new(int), new(int), new(int), &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_speed_servers_server") {
		t.Errorf("plan %q, want a seek on idx_speed_servers_server", plan)
	}
}

// A failed run's accounting row is not a measurement: even if it carries a
// decided verdict (reachable only through an imported backup - the scheduler
// never stamps race_* on a failed row) it must not seed the next race.
func TestLastDecidedRaceSkipsFailedRows(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	f := func(v float64) *float64 { return &v }
	base := time.Now().Add(-time.Hour).Unix()
	if err := st.InsertSpeed(ctx, SpeedSample{TS: base, Server: "s", Engine: "ookla", DownMbps: 1, UpMbps: 1, PingMS: 1,
		RaceOutcome: "decided", RaceWinnerLabel: "Montréal", RaceWinnerLat: f(45.5), RaceWinnerLon: f(-73.57)}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSpeed(ctx, SpeedSample{TS: base + 60, Server: "s", Engine: "ookla", Failed: true,
		RaceOutcome: "decided", RaceWinnerLabel: "Toronto", RaceWinnerLat: f(43.65), RaceWinnerLon: f(-79.38)}); err != nil {
		t.Fatal(err)
	}
	label, _, _, ok, err := st.LastDecidedRace(ctx)
	if err != nil || !ok || label != "Montréal" {
		t.Errorf("got %q ok=%v err=%v; the newer row is accounting, not a race", label, ok, err)
	}
}
