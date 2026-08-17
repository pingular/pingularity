package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// A wall-clock measurement of what the Series cache costs at the cadence the
// dashboard ACTUALLY polls, which is the whole reason this file exists. A cache
// figure is a ratio between a TTL and a poll interval, so quoting one without
// the interval it was taken at says nothing. The chart's ladder is 6000ms up to
// a 2h window, 15000 to a day and 60000 beyond (latPollForMins in index.html;
// a6f5778's latPollMs used those same three numbers and sent every absolute
// range to the 6000). Against the 60000 a 30s TTL lands not one hit in half an
// hour; against the 6000 the same 30s TTL misses only one poll in five. Same
// TTL, opposite conclusions - which is why the cadence is required input below
// rather than a default, a default being how a harness ends up quoting a rate
// the product never polled at.
//
// Long and wall-clock-bound, so it is env-gated and never runs in a normal
// `go test ./internal/store/`. Everything it needs is measured, not modelled:
// it drives the real Store.Series and reads the real contract counters.

// seriesCadenceEnvInt reads an int env var, or def when unset.
func seriesCadenceEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s=%q: %v", key, v, err)
	}
	return n
}

// seriesCadenceBucket is seriesBucket (web.go:1193-1203) reimplemented, because
// internal/web imports internal/store and the dependency cannot run the other
// way. It is a copy and copies drift, so the caller below asserts the width it
// produces against the number the shipped ladder is known to give for the
// scenario - a drift in either place fails the run instead of quietly measuring
// a bucket no window asks for. maxSeriesPoints is 1500 (web.go:1177).
func seriesCadenceBucket(since, until, now time.Time) int {
	end := until
	if end.IsZero() || end.After(now) {
		end = now
	}
	b := int(end.Sub(since)/time.Second) / 1500
	if b < 1 {
		b = 1
	}
	return b
}

// clampLiveTTL replays a pre-change live TTL cap on whatever the poll just
// wrote, so the BEFORE arm is the same production code path measured under the
// old policy rather than a separate model of it. The only difference between
// the two trees on this path is the cap: the old code computed
// ttl = bucketSec/4 and clipped it to 30s for an open-ended (or future-ended)
// window, then set expires = now + ttl. Clipping the entry's expiry to now+cap
// immediately after the call reproduces that expiry to within the call's own
// return overhead.
//
// It only ever LOWERS an expiry, so calling it after a cache hit (which leaves
// the expiry alone) cannot extend anything: a hit's expiry was already set at
// an earlier now, so it is at or below this one. Zero expiries - the
// deliberately-uncached empty result - are left alone, since the old code left
// those at zero too.
//
// Entries come from seriesEntries, which hands them over with seriesMu already
// dropped - see the lock-ordering note there.
func clampLiveTTL(st *Store, cap time.Duration) {
	deadline := time.Now().Add(cap)
	for _, e := range seriesEntries(st) {
		e.mu.Lock()
		if !e.expires.IsZero() && e.expires.After(deadline) {
			e.expires = deadline
		}
		e.mu.Unlock()
	}
}

func TestSeriesCadenceWidthOK(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seen      map[int]int
		want      int
		minutes   float64
		rolling   bool
		ok        bool
		wantAllow int
	}{
		{"preset holds one width", map[int]int{403: 30}, 403, 30, true, true, 403},
		{"preset width creeps", map[int]int{403: 20, 404: 10}, 403, 30, true, false, 403},
		{"copy of seriesBucket drifted", map[int]int{432: 60}, 403, 1, true, false, 403},
		{"absolute range steps once in half an hour", map[int]int{1728: 25, 1729: 5}, 1728, 30, false, true, 1730},
		{"absolute range steps twice, both inside the run", map[int]int{1728: 10, 1729: 10, 1730: 10}, 1728, 30, false, true, 1730},
		{"absolute range jumps further than the clock allows", map[int]int{1728: 5, 1732: 5}, 1728, 30, false, false, 1730},
		{"absolute range starts at the wrong width", map[int]int{1729: 30}, 1728, 30, false, false, 1730},
		{"no polls at all", map[int]int{}, 403, 30, true, false, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, allowed, ok := seriesCadenceWidthOK(tc.seen, tc.want, tc.minutes, tc.rolling)
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v for widths %v (want %d, %.0f min, rolling=%v)", ok, tc.ok, tc.seen, tc.want, tc.minutes, tc.rolling)
			}
			if allowed != tc.wantAllow {
				t.Errorf("allowed = %d, want %d", allowed, tc.wantAllow)
			}
		})
	}
}

// The BEFORE arm of every rate figure below is clampLiveTTL, so an inert
// clampLiveTTL would silently report the new cap twice and call the difference
// zero. Pin it behaviourally: it has to actually pull a live entry's expiry
// back to the old cap, it must not push one out again on a later call (that
// would hand the old policy a TTL it never had), and it must leave the
// deliberately-uncached empty result at zero.
func TestClampLiveTTLReplaysTheOldCap(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	const weekBucket = 7 * 24 * 3600 / 1500 // 403s, so the natural TTL is 100.75s
	if _, err := st.Series(ctx, now.Add(-7*24*time.Hour), time.Time{}, weekBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	if ttl := time.Until(seriesEntryExpiry(t, st)); ttl < 95*time.Second {
		t.Fatalf("entry cached for %v before any clamp, want ~100.75s: there is nothing here for the clamp to lower", ttl)
	}
	clampLiveTTL(st, 30*time.Second)
	clamped := seriesEntryExpiry(t, st)
	if ttl := time.Until(clamped); ttl > 30*time.Second || ttl < 29*time.Second {
		t.Fatalf("after replaying the 30s cap the entry lives %v, want ~30s: the BEFORE arm is measuring the new cap", ttl)
	}

	// A second call, this time on a HIT (the 30s has not passed), must not walk
	// the expiry forward: the old code set the expiry once per scan, not once per
	// request, and an expiry that crept forward on every hit would never expire.
	if _, err := st.Series(ctx, now.Add(-7*24*time.Hour), time.Time{}, weekBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	clampLiveTTL(st, 30*time.Second)
	if again := seriesEntryExpiry(t, st); again.After(clamped) {
		t.Errorf("expiry moved from %v to %v across a hit: the clamp is extending TTLs, not replaying a cap", clamped, again)
	}

	// An empty window is never cached; the old code left it at zero too, so the
	// clamp must not turn it into a 30s pin and hide the empty path.
	empty := open(t)
	const yearBucket = 365 * 24 * 3600 / 1500
	if _, err := empty.Series(ctx, now.Add(-365*24*time.Hour), time.Time{}, yearBucket, nil); err != nil {
		t.Fatalf("series on empty store: %v", err)
	}
	clampLiveTTL(empty, 30*time.Second)
	if exp := seriesEntryExpiry(t, empty); !exp.IsZero() {
		t.Errorf("clamp pinned an empty result until %v; it must stay uncached", exp)
	}
}

// seriesCadenceWidthOK judges the bucket widths a run saw against the width the
// ladder gives for its window. Split out of the run so it can be checked in
// milliseconds: the interesting case only shows itself after twenty-five
// minutes of wall clock, which is not a thing anyone re-runs to see whether a
// guard still bites.
//
// A ?mins= preset (rolling=true) re-derives its start from now on every poll, so
// its span - and so its width - is the same on the last poll as on the first.
// Any movement there is drift in this file's copy of seriesBucket.
//
// A live absolute range keeps a FIXED start, so its span grows with the run and
// its width steps up by one every maxSeriesPoints (1500) seconds. That step is
// not drift: it is the product's own behaviour, and because bucketSec is part of
// seriesKey it RE-KEYS the cache - the one source of key churn an absolute range
// has, since its floored start never moves. A run of elapsedMin can cross one
// more boundary than its length divided by 1500 suggests, because it may start
// anywhere inside a step.
func seriesCadenceWidthOK(seen map[int]int, wantBucket int, elapsedMin float64, rolling bool) (lo, hi, allowed int, ok bool) {
	for w := range seen {
		if lo == 0 || w < lo {
			lo = w
		}
		if w > hi {
			hi = w
		}
	}
	allowed = wantBucket
	if !rolling {
		allowed += int(elapsedMin*60)/1500 + 1
	}
	// A run that polled nothing leaves lo at 0, which is not wantBucket, so it
	// fails here rather than needing a length check of its own.
	return lo, hi, allowed, lo == wantBucket && hi <= allowed
}

// TestSeriesPollCadenceRate drives one chart window at one poll cadence for a
// wall-clock stretch and reports the contract counters per minute.
//
// SERIES_CADENCE_POLL_MS has no default ON PURPOSE: a default is a cadence this
// file invented, and a cache rate quoted at an invented cadence is a number
// about nothing. It has to be typed in by whoever quotes the result, read off
// index.html's chart cadence (latPollMs, which defers to latPollDelay), and it
// is echoed in the output so the figure and its cadence travel together.
func TestSeriesPollCadenceRate(t *testing.T) {
	scenario := os.Getenv("SERIES_CADENCE_SCENARIO")
	if scenario == "" {
		t.Skip("set SERIES_CADENCE_SCENARIO=7d|30d-abs (with SERIES_CADENCE_POLL_MS) to measure")
	}
	pollMs := seriesCadenceEnvInt(t, "SERIES_CADENCE_POLL_MS", 0)
	if pollMs <= 0 {
		t.Fatal("SERIES_CADENCE_POLL_MS is required: the cadence must be read off index.html's chart poll (latPollMs/latPollDelay), never invented here")
	}
	minutes := seriesCadenceEnvInt(t, "SERIES_CADENCE_MINUTES", 20)
	seedDays := seriesCadenceEnvInt(t, "SERIES_CADENCE_SEED_DAYS", 31)
	seedInterval := seriesCadenceEnvInt(t, "SERIES_CADENCE_SEED_INTERVAL_SEC", 60)
	legacyCapMs := seriesCadenceEnvInt(t, "SERIES_CADENCE_LEGACY_CAP_MS", 0)
	poll := time.Duration(pollMs) * time.Millisecond

	// The window shapes, exactly as handleSeries builds them (web.go:1768-1791).
	// A preset goes over as ?mins=, so its start is recomputed against now on
	// every poll and slides; a live absolute range goes over as ?from=<fixed> with
	// no ?to=, so its start is a constant and only its END follows now
	// (latWindowQuery index.html, parseRangeParams web.go:1211). That difference
	// is most of the gap between the two numbers: a sliding start re-keys the
	// cache once per bucketSec no matter what the TTL is, while a fixed start
	// re-keys only when its widening span bumps the bucket width - once per 1500s
	// (see seriesCadenceWidthOK), which at these cadences is far rarer.
	var (
		spanMins   int
		wantBucket int
		fixedSince time.Time
	)
	switch scenario {
	case "7d":
		spanMins, wantBucket = 7*24*60, 403 // 604800/1500
	case "30d-abs":
		spanMins, wantBucket = 30*24*60, 1728 // 2592000/1500
		fixedSince = time.Now().Add(-time.Duration(spanMins) * time.Minute).Truncate(time.Second)
	default:
		t.Fatalf("unknown SERIES_CADENCE_SCENARIO %q", scenario)
	}

	// Seeded past the widest window so every poll aggregates real rows: an empty
	// window is never cached at all, and measuring the cache over one would
	// measure the empty path instead of the TTL. Round density does not change
	// which polls hit - only how long a scan takes - so the cheaper 60s seed is
	// used and reported.
	st := seedSeriesDB(t, benchStacks()[1], time.Duration(seedDays)*24*time.Hour, time.Now(), seedInterval)

	ctx := context.Background()
	stats.ResetForTest()
	start := time.Now()
	deadline := start.Add(time.Duration(minutes) * time.Minute)
	polls, empties, bucketSeen := 0, 0, map[int]int{}
	// The trailing sleep is INSIDE the measured stretch. Each poll owns one poll
	// interval; stopping the clock on the last poll instead would divide N polls
	// by the N-1 intervals between them and inflate every rate by 1/N - 5% over a
	// twenty-poll run, which is the size of the effect being measured.
	for time.Now().Before(deadline) {
		now := time.Now()
		since := fixedSince
		if since.IsZero() {
			since = now.Add(-time.Duration(spanMins) * time.Minute)
		}
		bucket := seriesCadenceBucket(since, time.Time{}, now)
		bucketSeen[bucket]++
		pts, err := st.Series(ctx, since, time.Time{}, bucket, nil)
		if err != nil {
			t.Fatalf("series: %v", err)
		}
		if len(pts) == 0 {
			empties++
		}
		polls++
		if legacyCapMs > 0 {
			clampLiveTTL(st, time.Duration(legacyCapMs)*time.Millisecond)
		}
		// loopChart's shape: poll, AWAIT it, then arm the next delay - so the
		// scan's own duration pushes the next poll back, exactly as a slow fetch
		// does in the browser. A fixed schedule would poll faster than the real
		// dashboard can.
		time.Sleep(poll)
	}
	elapsed := time.Since(start).Minutes()

	c := seriesCounters()
	per := func(n int64) float64 { return float64(n) / elapsed }
	out := map[string]any{
		"scenario":          scenario,
		"poll_ms":           pollMs,
		"legacy_cap_ms":     legacyCapMs,
		"bucket_sec":        wantBucket,
		"bucket_widths":     bucketSeen,
		"minutes":           elapsed,
		"polls":             polls,
		"polls_per_min":     per(int64(polls)),
		"query_per_min":     per(c["series.query"]),
		"hit_per_min":       per(c["series.cache.hit"]),
		"expired_per_min":   per(c["series.cache.expired"]),
		"new_per_min":       per(c["series.cache.new"]),
		"empty_per_min":     per(c["series.cache.empty"]),
		"query_seconds":     stats.Lifetime().Histos["series.query.seconds"].Sum,
		"seed_days":         seedDays,
		"seed_interval_sec": seedInterval,
		"raw":               c,
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Emitted BEFORE the guards below, so a run that trips one still prints what
	// it saw. A guard that swallows half an hour of wall clock teaches whoever is
	// measuring to loosen it.
	fmt.Println("SERIESRATE " + string(b))

	if empties > 0 {
		t.Fatalf("%d of %d polls returned no points: the seed does not cover the window, so this measured the empty path and not the cache", empties, polls)
	}
	if _, hi, allowed, ok := seriesCadenceWidthOK(bucketSeen, wantBucket, elapsed, fixedSince.IsZero()); !ok {
		t.Fatalf("bucket widths seen %v over %.1f min: want %d rising no further than %d, got up to %d - seriesBucket's arithmetic and the copy in this file have drifted",
			bucketSeen, elapsed, wantBucket, allowed, hi)
	}
}
