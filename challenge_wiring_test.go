package main

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

func seedWinners(t *testing.T, st *store.Store, base int64, reasons ...string) {
	t.Helper()
	for i, r := range reasons {
		row := store.SpeedServerRow{RunTS: base + int64(i)*60, ServerID: "s" + string(rune('a'+i)), Server: "S", RankOrder: 1, Winner: true, WinReason: r}
		if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{row}); err != nil {
			t.Fatal(err)
		}
	}
}

// A challenge is due once N automatic winners have passed with no challenge
// among them; pinned runs neither count nor reset; 0 turns it off.
func TestChallengeFnIsDueEveryNAutoRunsFromHistory(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{SpeedChallengeEvery: 3, SpeedChallengeMargin: 15,
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	fn := newChallengeFn(st, set)
	if fn() {
		t.Fatal("no history: nothing to challenge yet")
	}
	base := time.Now().Add(-2 * time.Hour).Unix()
	seedWinners(t, st, base, "fastest_ranked", "incumbent")
	if fn() {
		t.Error("two auto runs, N=3: not due")
	}
	seedWinners(t, st, base+1000, speedtest.WinReasonPinned) // a pinned run does not count
	if fn() {
		t.Error("a pinned run must not count toward N")
	}
	seedWinners(t, st, base+2000, "incumbent")
	if !fn() {
		t.Error("three auto runs without a challenge: due")
	}
	seedWinners(t, st, base+3000, speedtest.WinReasonChallenger) // the challenge happened (and lost)
	if fn() {
		t.Error("just challenged: not due")
	}
	seedWinners(t, st, base+4000, "incumbent", "incumbent")
	if fn() {
		t.Error("two auto runs since the challenge: not due")
	}
	seedWinners(t, st, base+5000, "incumbent")
	if !fn() {
		t.Error("three auto runs since the challenge: due again")
	}
	// A pinned stretch longer than N must not start the count over.
	seedWinners(t, st, base+6000, speedtest.WinReasonPinned, speedtest.WinReasonPinnedBestOf, speedtest.WinReasonPinnedCompanion, speedtest.WinReasonPinned)
	if !fn() {
		t.Error("four pinned runs in a row: still due - pinned runs neither count nor reset")
	}
	off, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { off.Close() })
	setOff, err := settings.New(context.Background(), off, settings.Values{SpeedChallengeEvery: 0,
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	seedWinners(t, off, base, "incumbent", "incumbent", "incumbent", "incumbent")
	if newChallengeFn(off, setOff)() {
		t.Error("0 = never challenge")
	}
}

// The incumbent lookup skips a challenger that lost, and takes one that won.
func TestIncumbentFnSkipsLosingChallengers(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fn := newIncumbentFn(st)
	base := time.Now().Add(-time.Hour).Unix()
	row := func(ts int64, id, reason string) store.SpeedServerRow {
		return store.SpeedServerRow{RunTS: ts, ServerID: id, Server: "S", RankOrder: 1, Winner: true, WinReason: reason}
	}
	if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{row(base, "seat", "incumbent")}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{row(base+60, "loser", speedtest.WinReasonChallenger)}); err != nil {
		t.Fatal(err)
	}
	if got := fn(); got != "seat" {
		t.Errorf("incumbent %q, want seat: a challenger that lost must not take the seat", got)
	}
	if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{row(base+120, "winner", speedtest.WinReasonChallengerWon)}); err != nil {
		t.Fatal(err)
	}
	if got := fn(); got != "winner" {
		t.Errorf("incumbent %q, want winner: a challenger that won holds the seat", got)
	}
}

func TestIncumbentScoresFnReadsTheRecord(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	base := time.Now().Add(-time.Hour).Unix()
	rows := []store.SpeedServerRow{
		{RunTS: base, ServerID: "1993", Server: "EBOX", RankOrder: 1, Measured: true, Winner: true, Score: 50, DownMbps: 100, UpMbps: 20},
		{RunTS: base + 60, ServerID: "1993", Server: "EBOX", RankOrder: 1, Measured: true, Winner: true, Score: 60, DownMbps: 100, UpMbps: 20},
		{RunTS: base + 120, ServerID: "1993", Server: "EBOX", RankOrder: 1, Measured: true, Winner: true, Score: 70, DownMbps: 100, UpMbps: 20},
		// A download-only run: judged under another direction, not comparable to a "both" record.
		{RunTS: base + 180, ServerID: "1993", Server: "EBOX", RankOrder: 1, Measured: true, Winner: true, Score: 90, DownMbps: 100},
		// A partial run whose upload failed scores 0: not a record, and must not eat a window slot.
		{RunTS: base + 240, ServerID: "1993", Server: "EBOX", RankOrder: 1, Measured: true, Winner: true, Score: 0, DownMbps: 100, UpMbps: 20},
		{RunTS: base + 240, ServerID: "2", Server: "X", RankOrder: 2},
	}
	for _, r := range rows {
		if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{r}); err != nil {
			t.Fatal(err)
		}
	}
	got := newIncumbentScoresFn(st)("1993", "both")
	if len(got) != 3 || got[0] != 70 || got[2] != 50 {
		t.Errorf("both: scores %v, want 70, 60, 50 newest first - the download-only and the zero rows excluded", got)
	}
	if got := newIncumbentScoresFn(st)("1993", "down"); len(got) != 1 || got[0] != 90 {
		t.Errorf("down: scores %v, want the one download-only row", got)
	}
	if got := newIncumbentScoresFn(st)("2", "both"); len(got) != 0 {
		t.Errorf("a server never measured has no record, got %v", got)
	}
}

// The recent-winner origin: the newest decided race's city, as a candidate
// for the next race; nil with no such race.
func TestRecentWinnerFnNamesTheLastDecidedRace(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fn := newRecentWinnerFn(st)
	if fn() != nil {
		t.Fatal("no history: no recent winner")
	}
	la, lo := 45.5017, -73.5673
	if err := st.InsertSpeed(context.Background(), store.SpeedSample{TS: time.Now().Add(-time.Hour).Unix(), Server: "s", Engine: "ookla",
		DownMbps: 1, UpMbps: 1, PingMS: 1, RaceOutcome: "decided", RaceWinnerKind: "exit", RaceWinnerLabel: "Montréal, CA",
		RaceWinnerLat: &la, RaceWinnerLon: &lo}); err != nil {
		t.Fatal(err)
	}
	got := fn()
	if got == nil || got.Kind != "recent" || got.Label != "Montréal, CA" || got.Lat != la || got.Lon != lo || !got.Anchored {
		t.Errorf("got %+v, want the Montréal race as an anchored 'recent' origin", got)
	}
}
