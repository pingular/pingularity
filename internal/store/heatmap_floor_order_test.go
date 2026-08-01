package store

import (
	"context"
	"testing"
	"time"
)

// The heatmap works out a day's observed seconds by subtracting pause spans from
// the day's length, then clamping the result to the part of the day that follows
// the moment monitoring began.
//
// Clamping with a minimum is not the same as clamping the RANGE. On the day
// monitoring started, the pause subtraction is computed against the whole day -
// including the hours before the anchor, which were never watched by anyone - and
// then thrown away wholesale by `min(obs, watchable)` whenever the clamp binds.
// Pauses that fall AFTER the anchor, inside the genuinely watched part of the
// day, disappear with it.
//
// The day then claims it was watched end-to-end from the moment monitoring began,
// while the monitor was demonstrably switched off for part of it - coverage
// overstated on the one day the operator is most likely to be looking at.
func TestHeatmapSubtractsPausesInsideTheFirstWatchedDay(t *testing.T) {
	s, err := Open(t.TempDir() + "/h.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	loc := time.UTC

	// Monitoring begins at noon; the machine is then switched off 14:00-16:00.
	// Watched part of the day = 12h, of which 2h were paused, so 10h observed.
	day := time.Now().In(loc).Truncate(24 * time.Hour).Add(-48 * time.Hour)
	noon := day.Add(12 * time.Hour)
	if err := s.InsertSamples(ctx, []Sample{{
		TS: noon, Target: "1.1.1.1", Family: "ipv4", LatencyMS: 10, Success: true,
	}}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	if _, err := s.InsertPause(ctx, day.Add(14*time.Hour), int64((2 * time.Hour).Seconds())); err != nil {
		t.Fatalf("pause: %v", err)
	}

	days, err := s.DowntimeByDay(ctx, day.Add(-24*time.Hour), loc)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	date := day.Format("2006-01-02")
	var got *DowntimeDay
	for i := range days {
		if days[i].Date == date {
			got = &days[i]
		}
	}
	if got == nil {
		t.Fatalf("no row for %s; rows: %+v", date, days)
	}

	const want = int(10 * 60 * 60) // 12h watchable - 2h paused
	t.Logf("%s: window_s=%d observed_s=%d (want %d)", got.Date, got.WindowS, got.ObservedS, want)
	if got.ObservedS != want {
		t.Errorf("observed_s = %d, want %d: the two paused hours fell inside the watched part of "+
			"the day but were subtracted from the whole day and then discarded by the floor, so "+
			"the day reports being watched every second from the moment monitoring began",
			got.ObservedS, want)
	}
}
