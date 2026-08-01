package store

import (
	"context"
	"testing"
	"time"
)

// The edge-case semantics UptimeSince's callers depend on, pinned against the
// (ratio, coverage) tuple the Observation type replaced. Two of them are sentinels
// rather than measurements and MUST stay exactly where they were: the old code
// returned `1, 0, nil` for both `window <= 0` and `observed <= 0`, so a caller
// rendering "-" reads a defined number and a caller summing gets no NaN.
func TestObservationEdgeSemantics(t *testing.T) {
	for _, tc := range []struct {
		name       string
		o          Observation
		ratio, cov float64
		defined    bool
	}{
		// The zero value is what UptimeSince returns for a non-positive window.
		{"zero value reads as the old `1, 0` sentinel", Observation{}, 1, 0, false},
		{"window with nothing observed", Observation{Window: time.Hour}, 1, 0, false},
		{"fully observed, flawless", Observation{Window: time.Hour, Observed: time.Hour}, 1, 1, true},
		{"half observed, flawless", Observation{Window: 2 * time.Hour, Observed: time.Hour}, 1, 0.5, true},
		{"downtime divides OBSERVED time, not wall time",
			Observation{Window: 4 * time.Hour, Observed: time.Hour, Down: 15 * time.Minute}, 0.75, 0.25, true},
		// Defensive clamps: overlapping or hand-edited rows must not be able to
		// produce a ratio outside [0,1] or a coverage above 1.
		{"downtime past observed clamps to fully down",
			Observation{Window: time.Hour, Observed: time.Hour, Down: 2 * time.Hour}, 0, 1, true},
		{"negative downtime clamps to none",
			Observation{Window: time.Hour, Observed: time.Hour, Down: -time.Hour}, 1, 1, true},
		{"observed past window clamps coverage to 1",
			Observation{Window: time.Hour, Observed: 2 * time.Hour}, 1, 1, true},
		{"negative observed is not a measurement",
			Observation{Window: time.Hour, Observed: -time.Second}, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.o.Ratio(); got != tc.ratio {
				t.Errorf("Ratio() = %v, want %v", got, tc.ratio)
			}
			if got := tc.o.Coverage(); got != tc.cov {
				t.Errorf("Coverage() = %v, want %v", got, tc.cov)
			}
			if got := tc.o.Defined(); got != tc.defined {
				t.Errorf("Defined() = %v, want %v", got, tc.defined)
			}
		})
	}
}

// THE GATE, at its exact boundary. /metrics publishes pingularity_uptime_ratio iff
// Defined(), and Defined() is a DEFINEDNESS check, not a tunable threshold: the
// smallest representable slice of observation publishes, and only literally
// nothing does not.
//
// The boundary is asserted at the smallest positive time.Duration rather than at a
// comfortable value on purpose. A future edit that "tunes the threshold" to any
// non-zero coverage fails here, and it should: a monitor legitimately watched
// 8h/day sits at coverage 0.3333 on every window forever, so such a cutoff would
// delete all six ratio series for it permanently while the coverage series went on
// reporting that data exists.
func TestObservationDefinedAtTheBoundary(t *testing.T) {
	year := 365 * 24 * time.Hour
	if o := (Observation{Window: year, Observed: 1}); !o.Defined() {
		t.Errorf("one nanosecond of observation must be DEFINED (coverage %g); "+
			"a non-zero threshold here silently deletes every ratio series of a part-time monitor", o.Coverage())
	}
	if o := (Observation{Window: year, Observed: 1}); o.Coverage() <= 0 {
		t.Errorf("coverage of the smallest observation = %g, want > 0", o.Coverage())
	}
	if o := (Observation{Window: year, Observed: 0}); o.Defined() {
		t.Error("zero observation must NOT be defined: the ratio there is a sentinel, not a measurement")
	}
	if o := (Observation{Window: year, Observed: -1}); o.Defined() {
		t.Error("a negative (corrupt) observed span must NOT be defined")
	}
}

// A day the monitor never watched must produce a ROW, not silence. DowntimeByDay
// emits rows only for days with an event or an offline second, so before this a
// fully dark day was indistinguishable in the payload - and in the rendered
// heatmap - from a day watched end to end with a flawless link.
func TestDowntimeByDayDisclosesUnobservedDays(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dark := today.AddDate(0, 0, -1)  // yesterday: not watched at all
	part := today.AddDate(0, 0, -2)  // the day before: watched for 20 of its 24 hours
	since := today.AddDate(0, 0, -3) // and the day before THAT: watched end to end

	sampleAt(t, st, now, int(now.Sub(since).Seconds()), "cf", "ipv4", true)
	if _, err := st.InsertPause(ctx, part.Add(20*time.Hour), 4*3600); err != nil {
		t.Fatalf("partial pause: %v", err)
	}
	if _, err := st.InsertPause(ctx, dark, 24*3600); err != nil {
		t.Fatalf("dark pause: %v", err)
	}

	rows, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	byDate := map[string]DowntimeDay{}
	var dates []string
	for _, r := range rows {
		byDate[r.Date] = r
		dates = append(dates, r.Date)
	}

	darkKey := dark.Format("2006-01-02")
	got, ok := byDate[darkKey]
	if !ok {
		t.Fatalf("no row for %s: a day the monitor never watched must be disclosed, "+
			"not omitted - omission renders as a clean day. rows = %+v", darkKey, rows)
	}
	if got.WindowS != 86400 || got.ObservedS != 0 || got.Observed() {
		t.Errorf("%s = %+v, want window_s 86400 / observed_s 0 (nothing watched)", darkKey, got)
	}
	if got.Outages != 0 || got.DowntimeS != 0 {
		t.Errorf("%s = %+v: an unobserved day is neither up nor DOWN; it must not book downtime", darkKey, got)
	}

	partKey := part.Format("2006-01-02")
	if got := byDate[partKey]; got.WindowS != 86400 || got.ObservedS != 20*3600 {
		t.Errorf("%s = %+v, want window_s 86400 / observed_s %d (4h unwatched)", partKey, got, 20*3600)
	}

	// The fully watched day stays absent: a year of healthy 24/7 monitoring must
	// still answer the 60-second heatmap poll with a handful of rows, not 366.
	if _, ok := byDate[since.Format("2006-01-02")]; ok {
		t.Errorf("a fully observed, event-free day must not be emitted: rows = %+v", rows)
	}
	// Rows minted by the observation loop must not break the chronological sort.
	for i := 1; i < len(dates); i++ {
		if dates[i-1] >= dates[i] {
			t.Fatalf("rows out of order after minting unobserved days: %v", dates)
		}
	}
}

// The heatmap's observation must agree with UptimeSince's over the same span - the
// two derive it by different routes (per-day intersection in Go vs one SQL
// aggregate), and this cluster exists because such pairs drift.
func TestDowntimeByDayObservationMatchesUptimeSince(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 3*86400, "cf", "ipv4", true)
	pauseAt(t, st, now, 40*3600, 9*3600) // an unobserved stretch crossing a local midnight

	since := now.Add(-48 * time.Hour)
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	rows, err := st.DowntimeByDay(ctx, since, time.Local)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	var win, obs int
	for _, r := range rows {
		win += r.WindowS
		obs += r.ObservedS
	}
	// Whole days with no pause and no event emit no row, so compare the UNOBSERVED
	// total, which every such day contributes 0 to.
	wantUnobs := int((o.Window - o.Observed).Seconds())
	if gotUnobs := win - obs; gotUnobs < wantUnobs-2 || gotUnobs > wantUnobs+2 {
		t.Fatalf("heatmap says %ds of the window went unobserved, UptimeSince says %ds; "+
			"the pill and the heatmap must describe the same span", gotUnobs, wantUnobs)
	}
}
