package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Uptime has to stay cheap as history accumulates, because it is not on a rare
// path: the shared status/metrics aggregate refreshes every 30 seconds and a
// saved custom uptime window puts an uncached UptimeSince on every 3-second poll.
//
// The shape that matters is a year of five-minute pause checkpoints - what any
// install using the monitoring schedule accumulates - crossed with a couple of
// outages a day. Reading the pause table once per outage makes the cost the
// PRODUCT of those two counts, so it looks fine on a fresh install and falls over
// on a year-old one, which is exactly the install nobody tests on.

// yearFixture seeds ~52k pause spans and n outages across one year.
func yearFixture(t testing.TB, s *Store, outages int) time.Time {
	t.Helper()
	ctx := context.Background()
	// Anchor the whole fixture to a 600-second boundary. Without this the pause
	// grid starts at an arbitrary second while the outages below align to absolute
	// multiples of 600, so their phase relative to the pauses - and therefore how
	// much downtime is observed - drifted with the wall clock and the fixture
	// reported a different answer on different runs.
	now := time.Unix(time.Now().Unix()/600*600, 0)
	yearAgo := now.Add(-365 * 24 * time.Hour)

	// Five-minute pause spans, ten minutes apart: the checkpoint cadence.
	var pauses []map[string]any
	for ts := yearAgo.Unix(); ts < now.Unix(); ts += 600 {
		pauses = append(pauses, map[string]any{"ts": float64(ts), "duration_s": float64(300)})
	}
	for i := 0; i < len(pauses); i += 5000 {
		end := i + 5000
		if end > len(pauses) {
			end = len(pauses)
		}
		if _, err := s.ImportTable(ctx, "pauses", pauses[i:end]); err != nil {
			t.Fatalf("seed pauses: %v", err)
		}
	}

	// Outages spread evenly, each 120s, landing in observed time.
	step := (365 * 24 * 3600) / int64(outages+1)
	var events []map[string]any
	for i := 1; i <= outages; i++ {
		down := yearAgo.Unix() + int64(i)*step
		down -= down % 600 // align to the pause grid, in the observed half
		down += 310        // just after a pause ends
		events = append(events,
			map[string]any{"ts": float64(down), "type": "down"},
			map[string]any{"ts": float64(down + 120), "type": "up", "duration_s": float64(120)})
	}
	if _, err := s.ImportTable(ctx, "events", events); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	if err := s.InsertSamples(ctx, []Sample{{
		TS: yearAgo, Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("seed sample: %v", err)
	}
	return now
}

// The regression guard. Wall-clock budgets are blunt, but the failure being
// guarded is two orders of magnitude, not a few percent - main answered the same
// question in ~11ms and the N+1 version took ~2.6s, so a one-second ceiling
// separates them without being sensitive to machine speed.
func TestUptimeStaysCheapOverAYearOfHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("year-scale fixture")
	}
	if raceEnabled {
		// Measured: 18ms becomes 732ms under instrumentation, so these ceilings would
		// fail on the tool rather than on the code. TestUptimeCostDoesNotScaleWith-
		// OutageCount still guards the same regression here, by ratio rather than by
		// clock, which is why it is the one that matters.
		t.Skip("wall-clock budgets are not meaningful under the race detector")
	}
	s, err := Open(t.TempDir() + "/scale.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	now := yearFixture(t, s, 730)

	start := time.Now()
	o, err := s.UptimeSince(ctx, now.Add(-365*24*time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	took := time.Since(start)
	t.Logf("UptimeSince(1y) over 52k pauses / 730 outages took %v (down=%v ratio=%.6f)", took, o.Down, o.Ratio())
	if took > time.Second {
		t.Errorf("UptimeSince(1y) took %v; reading the pause table once per outage makes this the "+
			"PRODUCT of the two counts, and it runs every 30s behind /api/status and /metrics", took)
	}

	start = time.Now()
	if _, err := s.UptimeWindows(ctx, now, 0); err != nil {
		t.Fatalf("UptimeWindows: %v", err)
	}
	took = time.Since(start)
	t.Logf("UptimeWindows (6 windows) took %v", took)
	if took > 2*time.Second {
		t.Errorf("UptimeWindows took %v; this is the call the 30s aggregate refresh makes, and "+
			"past its own 30s context budget the figures freeze and never recover", took)
	}
}

// Cost must not scale with the outage count. Measuring the RATIO between two
// outage counts on the same pause table isolates the N+1 from machine speed.
func TestUptimeCostDoesNotScaleWithOutageCount(t *testing.T) {
	if testing.Short() {
		t.Skip("year-scale fixture")
	}
	ctx := context.Background()
	timeAt := func(outages int) time.Duration {
		s, err := Open(t.TempDir() + fmt.Sprintf("/scale%d.db", outages))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer s.Close()
		now := yearFixture(t, s, outages)
		start := time.Now()
		if _, err := s.UptimeSince(ctx, now.Add(-365*24*time.Hour), 0); err != nil {
			t.Fatalf("UptimeSince: %v", err)
		}
		return time.Since(start)
	}
	few, many := timeAt(50), timeAt(800)
	t.Logf("UptimeSince(1y): 50 outages %v, 800 outages %v", few, many)
	// 16x the outages must not cost anything like 16x the time.
	if many > 4*few+50*time.Millisecond {
		t.Errorf("16x the outages cost %v vs %v - the pause table is being re-read per outage", many, few)
	}
}

// The union path must be EQUIVALENT to asking the database per outage, not merely
// faster. This compares them directly on a fixture where outages deliberately
// straddle pause spans - the case the per-outage query existed for, and the only
// one where the two could differ.
func TestSpansInMatchesAPerOutageQuery(t *testing.T) {
	s, err := Open(t.TempDir() + "/eq.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	base := time.Unix(time.Now().Unix()/600*600, 0).Add(-30 * 24 * time.Hour).Unix()
	// Overlapping and duplicated pause rows on purpose: the union merges them, so
	// this also proves the merge does not change what a window sees.
	var pauses []map[string]any
	for i := int64(0); i < 400; i++ {
		ts := base + i*600
		pauses = append(pauses,
			map[string]any{"ts": float64(ts), "duration_s": float64(300)},
			map[string]any{"ts": float64(ts + 200), "duration_s": float64(250)}) // overlaps the previous
	}
	if _, err := s.ImportTable(ctx, "pauses", pauses); err != nil {
		t.Fatalf("pauses: %v", err)
	}

	union, err := s.pauseSpans(ctx, base-3600, base+400*600+3600)
	if err != nil {
		t.Fatalf("union: %v", err)
	}
	// Probe every phase within the cycle, including spans that start inside a
	// pause, end inside one, contain one, and sit entirely between two.
	for i := int64(0); i < 600; i += 7 {
		for _, width := range []int64{1, 60, 120, 300, 600, 1500} {
			from := base + 137*600 + i
			to := from + width
			want, err := s.pauseSpans(ctx, from, to)
			if err != nil {
				t.Fatalf("pauseSpans: %v", err)
			}
			got := spansIn(union, from, to)
			if len(got) != len(want) {
				t.Fatalf("window [%d,%d): spansIn gave %v, the query gave %v", from, to, got, want)
			}
			for k := range want {
				if got[k] != want[k] {
					t.Fatalf("window [%d,%d): span %d = %v, query says %v", from, to, k, got[k], want[k])
				}
			}
		}
	}
}
