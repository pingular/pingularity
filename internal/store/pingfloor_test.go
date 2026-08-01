package store

import (
	"context"
	"testing"
	"time"
)

// The floor is written and read through hand-maintained column lists (schema,
// additive migration, SELECT, scan, INSERT, export). Getting one of the six out
// of step does not fail the build - it either misaligns the whole scan or drops
// the value silently, so the round trip is what proves the plumbing.
func TestSpeedPingFloorRoundTrips(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	floor := 4.6
	if err := st.InsertSpeed(ctx, SpeedSample{
		TS: 100, DownMbps: 45, UpMbps: 48, PingMS: 30.14, PingBestMS: &floor,
		Server: "Example ISP, Oldtown", ServerID: "1234", Engine: "ookla",
	}); err != nil {
		t.Fatal(err)
	}
	// A run with no floor at all: iperf3, or any row written before the column
	// existed. It must read back absent, not as a 0 that would pass every
	// latency comparison and threshold.
	if err := st.InsertSpeed(ctx, SpeedSample{
		TS: 200, DownMbps: 900, PingMS: 1.2, Server: "lan", Engine: "iperf3",
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SpeedHistory(ctx, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var withFloor, without *SpeedSample
	for i := range rows {
		switch rows[i].TS {
		case 100:
			withFloor = &rows[i]
		case 200:
			without = &rows[i]
		}
	}
	if withFloor == nil || without == nil {
		t.Fatalf("expected both rows back, got %d", len(rows))
	}
	if withFloor.PingBestMS == nil {
		t.Fatal("the stored ping floor came back nil - a column list is out of step")
	}
	if *withFloor.PingBestMS != 4.6 {
		t.Errorf("ping floor round-tripped as %v, want 4.6", *withFloor.PingBestMS)
	}
	// The mean must survive alongside it, unchanged: the whole point is that the
	// reported number still matches what the engine said.
	if withFloor.PingMS != 30.14 {
		t.Errorf("reported ping came back %v, want the engine's 30.14", withFloor.PingMS)
	}
	if without.PingBestMS != nil {
		t.Errorf("a run with no floor read back %v, want nil", *without.PingBestMS)
	}
}
