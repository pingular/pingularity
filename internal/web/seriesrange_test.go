package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The downsample bucket must come from the window actually being drawn, not
// from ?mins=. A narrow absolute window used to inherit the bucket of whatever
// mins said, so a two-hour window from last year came back bucketed for a year.
func TestSeriesRangeBucketsFromTheSpanNotMins(t *testing.T) {
	s := newTestServer(t)
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Minute)
	for i := 0; i < 120; i++ { // one sample a minute for two hours
		if err := s.store.InsertSamples(t.Context(), []store.Sample{{
			TS: base.Add(time.Duration(i) * time.Minute), Target: "a", Family: "ipv4",
			Success: true, LatencyMS: 10}}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	get := func(q string) []store.SeriesPoint {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/series?"+q, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET ?%s -> %d", q, w.Code)
		}
		var out []store.SeriesPoint
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	from, to := base.Unix(), base.Add(2*time.Hour).Unix()
	// A 2h span buckets at 7200/1500 = 4s, well under the 60s sample spacing, so
	// every sample lands in its own bucket.
	pts := get(fmt.Sprintf("from=%d&to=%d", from, to))
	if len(pts) < 100 {
		t.Errorf("2h absolute window -> %d points, want ~120 (it must bucket by its own span)", len(pts))
	}
	// Sending a huge mins alongside must not coarsen it: from/to wins outright.
	if also := get(fmt.Sprintf("from=%d&to=%d&mins=525600", from, to)); len(also) != len(pts) {
		t.Errorf("mins alongside from/to changed the bucketing: %d vs %d", len(also), len(pts))
	}
}

// The window bounds BOTH aggregates. The DNS line rides a separate subquery
// LEFT JOINed on the bucket, so an out-of-window DNS row only reaches the output
// when it shares a BUCKET with in-window ping data. Buckets are epoch-aligned,
// so that happens exactly when the window end falls MID-bucket: the straddling
// bucket holds in-window ping rows and out-of-window DNS rows at once.
// Both earlier attempts at this test were worthless - one put the stray row ten
// hours out, the other used a bucket-aligned end - and the suite passed with the
// DNS bound deleted. This one is checked against that: remove ` AND ts < ?` from
// the dns subquery and it must fail.
func TestSeriesRangeBoundsTheDNSAggregateToo(t *testing.T) {
	s := newTestServer(t)
	const bucketSec = 60
	base := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Duration(bucketSec) * time.Second)
	// span/1500 == 60, and the end sits 30s INTO its bucket so that bucket
	// straddles the boundary.
	to := base.Add(time.Duration(bucketSec*1500)*time.Second + 30*time.Second)
	straddle := to.Add(-30 * time.Second) // start of the bucket containing `to`
	for _, ts := range []time.Time{base, to.Add(-10 * time.Second)} {
		if err := s.store.InsertSamples(t.Context(), []store.Sample{{
			TS: ts, Target: "a", Family: "ipv4", Success: true, LatencyMS: 10}}); err != nil {
			t.Fatalf("insert sample: %v", err)
		}
	}
	// One DNS row inside the window, and one PAST the end but in the straddling
	// bucket. Unbounded, the second is averaged into a bucket that is returned.
	if err := s.store.InsertDNS(t.Context(), base, 20, true); err != nil {
		t.Fatalf("insert dns: %v", err)
	}
	if err := s.store.InsertDNS(t.Context(), to.Add(10*time.Second), 999, true); err != nil {
		t.Fatalf("insert dns: %v", err)
	}
	w := do(t, s.Handler(), "GET", fmt.Sprintf("/api/series?from=%d&to=%d", base.Unix(), to.Unix()), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var pts []store.SeriesPoint
	if err := json.Unmarshal(w.Body.Bytes(), &pts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var seenStraddle bool
	for _, p := range pts {
		if p.TS == straddle.Unix() {
			seenStraddle = true
		}
		if p.DNSms != nil && *p.DNSms > 100 {
			t.Errorf("bucket %d carries DNS %.0f: the DNS row from past the window end leaked "+
				"in, so the upper bound is missing from the dns aggregate", p.TS, *p.DNSms)
		}
	}
	if !seenStraddle {
		t.Fatalf("setup is wrong: the straddling bucket %d is not in the result, so this test "+
			"would pass even with the bound removed", straddle.Unix())
	}
}

// A window whose end is in the FUTURE must bucket by the part that can hold
// data, not by the whole requested span. Typing a bare year, or "jul 1 to
// dec 31", ends next January, and counting that empty tail coarsened the chart
// by an order of magnitude for the data actually on screen.
func TestSeriesRangeFutureEndDoesNotCoarsenTheBucket(t *testing.T) {
	s := newTestServer(t)
	base := time.Now().Add(-30 * time.Minute).Truncate(time.Minute)
	for i := 0; i < 30; i++ { // one sample a minute for half an hour
		if err := s.store.InsertSamples(t.Context(), []store.Sample{{
			TS: base.Add(time.Duration(i) * time.Minute), Target: "a", Family: "ipv4",
			Success: true, LatencyMS: 10}}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	get := func(q string) int {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/series?"+q, "")
		var out []store.SeriesPoint
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(out)
	}
	open := get(fmt.Sprintf("from=%d", base.Unix()))
	if open < 25 {
		t.Fatalf("setup: open-ended window returned %d points, want ~30", open)
	}
	// The same start, ending a year out: the resolution must not collapse.
	far := get(fmt.Sprintf("from=%d&to=%d", base.Unix(), time.Now().Add(365*24*time.Hour).Unix()))
	if far != open {
		t.Errorf("future-ended window returned %d points against %d for the same data: the empty "+
			"tail is inflating the bucket width", far, open)
	}
}
