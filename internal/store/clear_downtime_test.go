package store

import (
	"context"
	"testing"
	"time"
)

// "Clear downtime" has to clear the whole downtime dataset, and this branch made
// that two tables rather than one: `events` holds the outage transitions and
// `pauses` holds the spans saying which wall seconds were watched at all. The
// export already treats them as one category (see dataCategories) precisely
// because a restore that took one without the other rewrites uptime.
//
// Clearing only `events` leaves the pause rows behind, and pause rows are the
// uptime DENOMINATOR: the outages vanish from the log and the heatmap, while
// observation coverage stays exactly as depressed as it was - a window that now
// shows nothing wrong and still reports it was only watching half the time, with
// nothing left on any screen to explain why.
func TestClearDowntimeRemovesPauseSpansToo(t *testing.T) {
	s, err := Open(t.TempDir() + "/c.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()
	if err := s.InsertEvent(ctx, now.Add(-2*time.Hour), "down", -1, ""); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := s.InsertEvent(ctx, now.Add(-time.Hour), "up", 3600, ""); err != nil {
		t.Fatalf("up: %v", err)
	}
	if _, err := s.InsertPause(ctx, now.Add(-90*time.Minute), 600); err != nil {
		t.Fatalf("pause: %v", err)
	}

	before, err := s.TableCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if before["events"] == 0 || before["pauses"] == 0 {
		t.Fatalf("fixture did not seed both tables: %+v", before)
	}

	if _, err := s.Clear(ctx, "downtime"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	after, err := s.TableCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if after["events"] != 0 {
		t.Errorf("events rows after clear = %d, want 0", after["events"])
	}
	if after["pauses"] != 0 {
		t.Errorf("pause rows after clear = %d, want 0: the outages are gone but the spans that "+
			"suppress observation coverage remain, so uptime stays depressed with nothing to explain it",
			after["pauses"])
	}
}

// Clearing downtime must not take the other datasets with it.
func TestClearDowntimeLeavesLatencyAndSpeedAlone(t *testing.T) {
	s, err := Open(t.TempDir() + "/c2.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	now := time.Now()
	if err := s.InsertSamples(ctx, []Sample{{
		TS: now.Add(-time.Hour), Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if _, err := s.InsertPause(ctx, now.Add(-30*time.Minute), 60); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if _, err := s.Clear(ctx, "downtime"); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	after, err := s.TableCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if after["samples"] == 0 {
		t.Error("clearing downtime deleted latency samples")
	}
}
