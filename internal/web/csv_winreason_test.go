package web

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The CSV export's win_reason column must carry the same value the JSON runs
// listing shows (the winner row's win_reason from the selection report), and
// the three new free-text cells - win_reason, race_outcome, race_winner_label -
// must go through csvSafe like every other TEXT column: a crafted backup can
// implant a formula in any of them.
func TestSpeedRunsCSVCarriesEscapedWinReason(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ts := time.Now().Unix()
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{
		TS: ts, DownMbps: 100, UpMbps: 20, PingMS: 10,
		// Only reachable through a crafted import, like the IP columns' case.
		RaceOutcome:     "=RACE()",
		RaceWinnerLabel: "@evil, QC",
	}); err != nil {
		t.Fatalf("seed speed run: %v", err)
	}
	if err := s.store.InsertSpeedServers(ctx, []store.SpeedServerRow{{
		RunTS: ts, ServerID: "1234", Server: "Example, ON", Selected: true,
		Measured: true, Winner: true, WinReason: "=challenger_won",
	}}); err != nil {
		t.Fatalf("seed selection report: %v", err)
	}

	// Sanity: the JSON listing resolves the reason - the fixture is real.
	if w := do(t, s.Handler(), "GET", "/api/speed/runs?limit=5", ""); !strings.Contains(w.Body.String(), "challenger_won") {
		t.Fatalf("fixture broken: JSON runs listing has no win_reason:\n%s", w.Body)
	}

	w := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if w.Code != 200 {
		t.Fatalf("csv export %d", w.Code)
	}
	rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
	if err != nil || len(rows) < 2 {
		t.Fatalf("parse csv (%v):\n%s", err, w.Body)
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	for _, name := range []string{"win_reason", "race_outcome", "race_winner_label"} {
		if _, ok := col[name]; !ok {
			t.Fatalf("csv header missing %q: %v", name, rows[0])
		}
	}
	row := rows[1]
	if got := row[col["win_reason"]]; got != "'=challenger_won" {
		t.Errorf("win_reason cell = %q, want %q (populated AND csvSafe-escaped)", got, "'=challenger_won")
	}
	if got := row[col["race_outcome"]]; got != "'=RACE()" {
		t.Errorf("race_outcome cell = %q, want escaped %q", got, "'=RACE()")
	}
	if got := row[col["race_winner_label"]]; got != "'@evil, QC" {
		t.Errorf("race_winner_label cell = %q, want escaped %q", got, "'@evil, QC")
	}
}
