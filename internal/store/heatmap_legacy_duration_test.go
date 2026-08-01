package store

import (
	"context"
	"testing"
	"time"
)

// A duration_s of -1 on an 'up' row is corrupt-backup residue: InsertEvent writes
// NULL for a negative and eventRowSane strips one from new imports, but a row that
// landed during the window when imports did not range-check is still on disk, and
// no migration cleans it. observedOutageSpans refuses the value (observed <= 0
// books nothing), so UptimeSince and the digest read the outage as 0s - while
// DowntimeByDay passed the same -1 straight into prorate's limit, where negative
// is the internal "no recorded length, credit everything" sentinel, and booked the
// whole down-to-up wall gap as red heatmap days. One row, three surfaces, two
// answers: the exact cross-surface disagreement this seam exists to prevent.
func TestHeatmapAgreesWithUptimeOnALegacyNegativeDuration(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	downTS := now.Add(-30 * time.Minute).Unix()
	upTS := now.Add(-20 * time.Minute).Unix()

	// Seed the legacy shape by straight SQL - today's writers both refuse it.
	if _, err := s.db.Exec(`INSERT INTO events (ts, type, duration_s) VALUES (?, 'down', NULL), (?, 'up', -1)`,
		downTS, upTS); err != nil {
		t.Fatalf("seed: %v", err)
	}

	since := now.Add(-time.Hour)
	o, err := s.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	_, digestDownS, err := s.ResolvedOutagesSince(ctx, since.Unix())
	if err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	days, err := s.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	heatmapDownS := 0
	for _, d := range days {
		heatmapDownS += d.DowntimeS
	}

	// The three surfaces must give ONE answer, and the agreed reading of a length
	// that is not a measurement is zero - a completed outage with no recorded
	// observed length, exactly how observedOutageSpans already treats it.
	if uptimeDownS := int(o.Down / time.Second); heatmapDownS != uptimeDownS || heatmapDownS != digestDownS {
		t.Errorf("one legacy duration_s=-1 row, three answers: heatmap %ds, uptime %ds, digest %ds; "+
			"prorate's negative limit means \"no limit\" and must never be fed from a stored value",
			heatmapDownS, uptimeDownS, digestDownS)
	}
}

// The repaired shape a modern import produces - 'up' with the duration stripped
// (NULL) - stays agreed on zero too; the outage is still counted.
func TestHeatmapAgreesWithUptimeOnAStrippedDuration(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	downTS := now.Add(-30 * time.Minute).Unix()
	upTS := now.Add(-20 * time.Minute).Unix()

	if _, err := s.db.Exec(`INSERT INTO events (ts, type, duration_s) VALUES (?, 'down', NULL), (?, 'up', NULL)`,
		downTS, upTS); err != nil {
		t.Fatalf("seed: %v", err)
	}

	since := now.Add(-time.Hour)
	o, err := s.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	days, err := s.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	heatmapDownS := 0
	outages := 0
	for _, d := range days {
		heatmapDownS += d.DowntimeS
		outages += d.Outages
	}
	if uptimeDownS := int(o.Down / time.Second); heatmapDownS != uptimeDownS {
		t.Errorf("no-duration 'up': heatmap %ds vs uptime %ds", heatmapDownS, uptimeDownS)
	}
	if outages != 1 {
		t.Errorf("outage count = %d, want 1: dropping the length must not drop the outage", outages)
	}
}
