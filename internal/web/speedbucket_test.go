package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// seedRuns writes n speedtests ending at now, spaced stepMin apart.
func seedRuns(t *testing.T, s *Server, n int, step time.Duration) []int64 {
	t.Helper()
	now := time.Now().Truncate(time.Minute)
	var out []int64
	for i := 0; i < n; i++ {
		ts := now.Add(-time.Duration(n-1-i) * step).Unix()
		out = append(out, ts)
		sp := store.SpeedSample{TS: ts, DownMbps: float64(100 + i%37), UpMbps: 10, PingMS: 12, Server: "srv"}
		if err := s.store.InsertSpeed(t.Context(), sp); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	return out
}

func getSpeed(t *testing.T, s *Server, q string) []store.SpeedSample {
	t.Helper()
	w := do(t, s.Handler(), "GET", "/api/speed?"+q, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/speed?%s -> %d", q, w.Code)
	}
	var out []store.SpeedSample
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// The response size has to follow what the canvas can draw, not how long the
// daemon has been recording. Before this the endpoint returned every run in the
// window: a year of the DEFAULT hourly schedule is 8,759 runs and 6.4 MB,
// re-fetched every 60s by every visible tab, and a year of 5-minute LAN tests is
// 105k runs and 74 MB held in memory per in-flight request.
func TestSpeedHistoryIsDownsampledToTheSeriesCap(t *testing.T) {
	s := newTestServer(t)
	// 4000 runs across ~2.8 days: comfortably more points than the cap.
	all := seedRuns(t, s, 4000, time.Minute)
	got := getSpeed(t, s, "mins=10080")
	if len(got) == 0 || len(got) > maxSeriesPoints {
		t.Fatalf("4000 runs in the window returned %d points, want at most the %d /api/series uses", len(got), maxSeriesPoints)
	}
	// Every point must be a REAL row. The dashboard turns a clicked point's ts
	// into /api/speed/runs?locate=<ts>, and a synthetic bucket-midpoint timestamp
	// would scroll the runs table to a row that does not exist.
	real := map[int64]bool{}
	for _, ts := range all {
		real[ts] = true
	}
	for _, p := range got {
		if !real[p.TS] {
			t.Fatalf("point ts=%d is not one of the recorded runs; click-to-row and the tooltips read this as a real run", p.TS)
		}
	}
	// Strictly increasing, one point per bucket.
	for i := 1; i < len(got); i++ {
		if got[i].TS <= got[i-1].TS {
			t.Fatalf("points are not strictly ordered at %d: %d then %d", i, got[i-1].TS, got[i].TS)
		}
	}
	// The final point must be the NEWEST run. syncSpeedPanel paints the seven stat
	// tiles and the "last in range" caption from the last point; a representative
	// picked any other way would silently start showing some earlier run there.
	if got[len(got)-1].TS != all[len(all)-1] {
		t.Fatalf("last point ts=%d, want the newest run %d - the stat tiles read this as the latest run",
			got[len(got)-1].TS, all[len(all)-1])
	}
	// The first point must still be inside the window (no fabricated leading edge).
	if got[0].TS < all[0] {
		t.Fatalf("first point ts=%d predates the oldest run %d", got[0].TS, all[0])
	}
}

// Downsampling must be invisible for any window with fewer runs than the cap -
// which is every window a real install draws except a multi-month one. 1d, 7d
// and 30d come back exactly as they did.
func TestSpeedHistoryNarrowWindowIsUnchanged(t *testing.T) {
	s := newTestServer(t)
	all := seedRuns(t, s, 400, time.Hour) // ~16 days, well under the cap
	got := getSpeed(t, s, "mins=43200")
	if len(got) != len(all) {
		t.Fatalf("a %d-run window returned %d points; below the cap every run must be returned", len(all), len(got))
	}
	for i := range got {
		if got[i].TS != all[i] {
			t.Fatalf("point %d ts=%d, want %d", i, got[i].TS, all[i])
		}
	}
}

// The absolute ?from/&to window buckets off the same rule, and stays half-open.
func TestSpeedHistoryAbsoluteWindowBuckets(t *testing.T) {
	s := newTestServer(t)
	all := seedRuns(t, s, 4000, time.Minute)
	from, to := all[0], all[len(all)-1] // half-open: excludes the newest run
	got := getSpeed(t, s, fmt.Sprintf("from=%d&to=%d", from, to))
	// One point per bucket. The bucket width is FLOORED (span/maxSeriesPoints,
	// the arithmetic /api/series has always used), so the window can hold a
	// handful more buckets than maxSeriesPoints; the bound is the bucket count,
	// not the constant.
	bucket := seriesBucket(time.Unix(from, 0), time.Unix(to, 0), time.Now())
	maxPts := int(to-from)/bucket + 1
	if len(got) == 0 || len(got) > maxPts {
		t.Fatalf("absolute window returned %d points, want at most one per bucket (%d)", len(got), maxPts)
	}
	if len(got) >= len(all) {
		t.Fatalf("absolute window returned %d of %d runs; it must downsample", len(got), len(all))
	}
	if last := got[len(got)-1].TS; last >= to {
		t.Fatalf("last point ts=%d is at or past the exclusive upper bound %d", last, to)
	}
	if last := got[len(got)-1].TS; last != all[len(all)-2] {
		t.Fatalf("last point ts=%d, want the newest run inside the window (%d)", last, all[len(all)-2])
	}
}

// seriesBucket is the single rule both chart endpoints size themselves by, so it
// has to keep measuring the part of the window that can HOLD data.
func TestSeriesBucketRule(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// A window with fewer seconds than points is not bucketed at all.
	if b := seriesBucket(now.Add(-20*time.Minute), time.Time{}, now); b != 1 {
		t.Fatalf("20-minute window bucket=%d, want 1 (off)", b)
	}
	// A year lands on the same width /api/series would use.
	year := 366 * 24 * time.Hour
	want := int(year/time.Second) / maxSeriesPoints
	if b := seriesBucket(now.Add(-year), time.Time{}, now); b != want {
		t.Fatalf("366-day window bucket=%d, want %d", b, want)
	}
	// An end in the future must not coarsen the window: most typed ranges ("jul 1
	// to dec 31") end after now, and measuring to that end would bucket a
	// two-week window as if it were a year.
	near := seriesBucket(now.Add(-2*time.Hour), now.Add(year), now)
	if near != seriesBucket(now.Add(-2*time.Hour), time.Time{}, now) {
		t.Fatalf("a future end changed the bucket (%d); width must come from the part that can hold data", near)
	}
	// A window entirely in the past is measured by its own span, not by now: ten
	// minutes from a year ago must bucket like ten minutes, not like a year.
	if b := seriesBucket(now.Add(-year), now.Add(-year).Add(10*time.Minute), now); b != 1 {
		t.Fatalf("a ten-minute window from a year ago bucket=%d, want 1", b)
	}
}

// What the positional stride actually guarantees, pinned - because the README
// claimed something stronger that was never true and never tested: that a wider
// window returns a SUPERSET of a narrower one. It does not. The stride re-picks
// from scratch at the new length, so at the production budget a window of 3001
// runs and one of 3000 share a single point out of 1500.
//
// These are the properties the chart and the stat tiles actually rely on.
func TestSpeedHistoryStridePropertiesThatDoHold(t *testing.T) {
	s := newTestServer(t)
	const runs = maxSeriesPoints + 501
	ts := seedRuns(t, s, runs, time.Second)

	got := getSpeed(t, s, "mins=100000")
	if len(got) > maxSeriesPoints {
		t.Errorf("returned %d points, over the %d budget", len(got), maxSeriesPoints)
	}
	// Every point is a REAL recorded run - never an average - so clicking one can
	// locate its row.
	real := map[int64]bool{}
	for _, v := range ts {
		real[v] = true
	}
	for _, p := range got {
		if !real[p.TS] {
			t.Fatalf("point ts=%d is not a recorded run", p.TS)
		}
	}
	// The newest run is always the last point: the stat tiles and the "last in
	// range" caption read it.
	if len(got) == 0 || got[len(got)-1].TS != ts[len(ts)-1] {
		t.Errorf("last point is not the newest run")
	}
	// Ascending, so the chart can draw it without sorting.
	for i := 1; i < len(got); i++ {
		if got[i].TS <= got[i-1].TS {
			t.Fatalf("points are not strictly ascending at %d", i)
		}
	}
}

// And the property that does NOT hold, asserted so nobody re-adds the claim to
// the README without noticing. If someone later makes the stride anchored so
// containment DOES hold, this test fails and they update the docs deliberately.
func TestWiderSpeedWindowIsNotASupersetOfANarrowerOne(t *testing.T) {
	s := newTestServer(t)
	const runs = maxSeriesPoints + 501
	ts := seedRuns(t, s, runs, time.Second)

	// Absolute windows differing by exactly one run at the old end - `mins` values
	// both wide enough to cover everything select the same rows and prove nothing.
	wide := getSpeed(t, s, fmt.Sprintf("from=%d&to=%d", ts[0], ts[len(ts)-1]+1))
	narrow := getSpeed(t, s, fmt.Sprintf("from=%d&to=%d", ts[1], ts[len(ts)-1]+1))

	inWide := map[int64]bool{}
	for _, p := range wide {
		inWide[p.TS] = true
	}
	missing := 0
	for _, p := range narrow {
		if !inWide[p.TS] {
			missing++
		}
	}
	if missing == 0 {
		t.Skip("the two windows happened to select the same rows; widen the difference to re-test")
	}
	t.Logf("%d of %d points in the narrower window are absent from the wider one - "+
		"the stride re-picks, it does not nest", missing, len(narrow))
}
