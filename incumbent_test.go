package main

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// The incumbent is the last AUTO winner: a pinned run in between is skipped,
// not treated as "no incumbent", so un-pinning resumes the auto history.
func TestIncumbentFnSkipsPinnedRunsAndStopsAtTheLastAutoWinner(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fn := newIncumbentFn(st)
	if got := fn(); got != "" {
		t.Fatalf("no history: incumbent %q, want none", got)
	}
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	rows := []store.SpeedServerRow{
		{RunTS: base, ServerID: "auto-old", Server: "A", RankOrder: 1, Winner: true, WinReason: "fastest_ranked"},
		{RunTS: base + 60, ServerID: "auto-new", Server: "B", RankOrder: 1, Winner: true, WinReason: "incumbent"},
		{RunTS: base + 120, ServerID: "pin", Server: "P", RankOrder: 0, Winner: true, WinReason: speedtest.WinReasonPinned},
		{RunTS: base + 180, ServerID: "pin-companion", Server: "C", RankOrder: 1, Winner: true, WinReason: speedtest.WinReasonPinnedCompanion},
	}
	if err := st.InsertSpeedServers(ctx, rows); err != nil {
		t.Fatal(err)
	}
	if got := fn(); got != "auto-new" {
		t.Errorf("incumbent %q, want auto-new: the two pinned runs on top are skipped, the newest auto winner stands", got)
	}
}
