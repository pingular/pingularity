package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// seriesCounters is the Series accounting after a call, as a map, so a test can
// assert the WHOLE picture: the interesting failures are miscounts (a hit booked
// as a miss), and asserting one key at a time cannot see them.
func seriesCounters() map[string]int64 {
	c := stats.Lifetime().Counters
	out := map[string]int64{}
	for _, k := range []string{"series.bypass", "series.cache.hit", "series.cache.expired", "series.cache.new", "series.cache.empty", "series.query"} {
		out[k] = c[k]
	}
	return out
}

// wantCounters is EXHAUSTIVE: a key the caller left out must be zero. Naming
// only the keys under test is what let a miscount hide - the whole failure mode
// here is one outcome being booked under another's name, and that moves two
// counters, so a check that reads only the one you were thinking about sees
// half of it and passes.
func wantCounters(t *testing.T, got map[string]int64, want map[string]int64, why string) {
	t.Helper()
	for k, v := range got {
		w := want[k]
		if v != w {
			t.Errorf("%s = %d, want %d (%s); full picture %v", k, v, w, why, got)
		}
	}
	for k := range want {
		if _, known := got[k]; !known {
			t.Errorf("want names %q, which seriesCounters does not collect - the assertion is inert", k)
		}
	}
}

// seriesEntries snapshots the cache's entries under seriesMu and returns them
// unlocked, so a caller that needs e.mu takes it with seriesMu already
// released. Series takes seriesMu only to find its entry and drops it before
// locking e.mu, so nothing in the package holds both at once; a helper that
// walked the map with seriesMu held and reached for e.mu inside the loop would
// be the sole place that ordered the two, which is exactly the ordering nobody
// remembers when the next lock is added.
func seriesEntries(st *Store) []*seriesEntry {
	st.seriesMu.Lock()
	defer st.seriesMu.Unlock()
	out := make([]*seriesEntry, 0, len(st.seriesCache))
	for _, e := range st.seriesCache {
		out = append(out, e)
	}
	return out
}

// expireSeriesEntries ages every cached entry out without waiting for its TTL:
// the distinction under test is "entry existed and expired" vs "no entry", and
// sleeping out a real quarter-bucket TTL would take a minute at the smallest
// cacheable bucket.
func expireSeriesEntries(st *Store) {
	for _, e := range seriesEntries(st) {
		e.mu.Lock()
		e.expires = time.Now().Add(-time.Second)
		e.mu.Unlock()
	}
}

// Sub-minute buckets return before the cache is ever consulted, so hit/expired/
// new can never see them - and four of the five range presets are sub-minute
// (1/2/14/57s for 5m/1h/6h/1d). Without its own counter that traffic is
// invisible and a busy dashboard reads as an idle cache.
func TestSeriesSubMinuteBucketCountsAsBypassNotCache(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	stats.ResetForTest()
	// The 1d preset's width: 86400/1500 = 57s (seriesBucket web.go:1193).
	for i := 0; i < 2; i++ {
		if _, err := st.Series(ctx, now.Add(-24*time.Hour), time.Time{}, 57, nil); err != nil {
			t.Fatalf("series: %v", err)
		}
	}
	got := seriesCounters()
	wantCounters(t, got, map[string]int64{
		"series.bypass":        2,
		"series.query":         2,
		"series.cache.hit":     0,
		"series.cache.new":     0,
		"series.cache.expired": 0,
	}, "a 57s bucket never reaches the cache, so every poll is a real scan and only bypass can show it")
	if len(st.seriesCache) != 0 {
		t.Errorf("seriesCache holds %d entries after sub-minute calls; the bypass return must not populate it", len(st.seriesCache))
	}
}

// First sight of a key is "new", a second call inside the TTL is a "hit", and a
// hit runs no query. Separating new from expired is the point: a rolling window's
// key rotates once per bucket on its own (the start is floored to the bucket),
// so churn that no TTL can fix must not read as TTL expiry.
func TestSeriesCacheNewThenHitThenExpired(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 120, "cf", "ipv4", true)
	since := now.Add(-time.Hour)
	stats.ResetForTest()
	if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.hit": 0, "series.cache.expired": 0, "series.query": 1, "series.bypass": 0,
	}, "first sight of a key")

	if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.hit": 1, "series.cache.expired": 0, "series.query": 1,
	}, "served from the live entry, so no second scan")

	expireSeriesEntries(st)
	if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.hit": 1, "series.cache.expired": 1, "series.query": 2,
	}, "the entry was still there, it had just aged out - that is expiry, not a new key")
}

// A window with no rows in it is never cached, so its entry sits in the map
// holding nothing - and the poll after that must NOT be booked as an expiry,
// because nothing expired. This is not a corner: on a fresh install EVERY wide
// window is empty, so before this was separated a new install reported a steady
// stream of expiries that never happened, and any before/after TTL comparison
// read off .expired was reading pure fiction.
func TestSeriesEmptyWindowBooksEmptyNotExpired(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const yearBucket = 365 * 24 * 3600 / 1500 // 21024s, the width a 1y range asks for
	stats.ResetForTest()
	for i := 0; i < 3; i++ {
		pts, err := st.Series(ctx, time.Now().Add(-365*24*time.Hour), time.Time{}, yearBucket, nil)
		if err != nil {
			t.Fatalf("series poll %d: %v", i, err)
		}
		if len(pts) != 0 {
			t.Fatalf("poll %d returned %d points from an empty store", i, len(pts))
		}
	}
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new":   1, // poll 1: the key really had no entry
		"series.cache.empty": 2, // polls 2 and 3: the entry was there, holding nothing
		"series.query":       3, // an empty result is never pinned, so all three scan
	}, "three polls of an empty window: one new key, then two re-scans of an entry that never held anything")
}

// The same key, walked through every outcome in turn. Booking .empty for a real
// expiry is the mirror of the bug above and would be just as wrong - the whole
// point of splitting them is that .expired is the ONE outcome a TTL change
// moves, so it has to still fire when a TTL really does run out. Each step also
// pins the arithmetic that makes the counters readable at all: new + empty +
// expired is exactly the number of scans, and a hit runs none.
func TestSeriesCacheOutcomesWalkedOnOneKey(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-time.Hour)
	stats.ResetForTest()

	poll := func(step string) {
		t.Helper()
		if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		c := seriesCounters()
		if scans := c["series.cache.new"] + c["series.cache.empty"] + c["series.cache.expired"]; scans != c["series.query"] {
			t.Errorf("after %s: new+empty+expired = %d but series.query = %d; a scan ran with no outcome booked for it (or an outcome was booked with no scan): %v",
				step, scans, c["series.query"], c)
		}
	}

	poll("first sight of an empty window")
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.query": 1,
	}, "no entry existed for this key")

	poll("second poll, still empty")
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.empty": 1, "series.query": 2,
	}, "the entry exists but has never held a result")

	// The first samples land. THIS poll still finds the entry empty - the rows
	// arrived after it was last written - so it is one more .empty, and it is the
	// poll that finally caches something.
	sampleAt(t, st, now, 120, "cf", "ipv4", true)
	poll("first poll after samples land")
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.empty": 2, "series.query": 3,
	}, "the entry was still empty when this poll read it; its result is the first thing cached")

	poll("inside the TTL")
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.empty": 2, "series.cache.hit": 1, "series.query": 3,
	}, "a live entry serves without scanning")

	expireSeriesEntries(st)
	poll("after the TTL runs out")
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 1, "series.cache.empty": 2, "series.cache.hit": 1,
		"series.cache.expired": 1, "series.query": 4,
	}, "a populated entry aged out - the only outcome a TTL change moves, and it must not be swallowed by .empty")
}

// A caller that arrives while another is mid-scan blocks on the per-entry mutex
// and is handed the fresh result without running a query. It must be counted
// where that is decided - after the lock - because the map lookup it did on the
// way in saw a stale (or missing) entry. Counting what the lookup saw would book
// a miss for every request the single-flight absorbed, reporting the query load
// the cache exists to prevent as though it happened.
func TestSeriesConcurrentCallersCountAsHitsNotMisses(t *testing.T) {
	ctx := context.Background()
	end := time.Now().Truncate(time.Hour)
	// A week of minute rounds: the scan has to take long enough that the other
	// callers really are blocked on the mutex rather than arriving after the fact.
	st := seedSeriesDB(t, benchStacks()[1], 7*24*time.Hour, end, 60)
	const callers = 16
	stats.ResetForTest()
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := st.Series(ctx, end.Add(-7*24*time.Hour), time.Time{}, 403, nil); err != nil {
				t.Errorf("series: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.query":         1,
		"series.cache.new":     1,
		"series.cache.hit":     callers - 1,
		"series.cache.expired": 0,
	}, "one scan served all 16 callers, so 15 of them are hits")
}

// The other half of the concurrent case, pinned without a race. Series inserts
// a zero-value entry and releases seriesMu before anyone locks the entry, so a
// second caller can find that entry in the map and still be the first to look
// inside it - and what it finds is an entry with no result and no expiry. That
// is a first sight of the key, not an empty result: booking .empty says "this
// window has no rows in it", a claim about the data that is false here, since
// the scan this caller is about to run returns rows. Classifying off the map
// lookup instead of the entry is what got this wrong, and it is why the
// concurrent test above was flaky: with that classification restored (and
// nothing else changed) it fails intermittently, at a rate that varies with the
// machine and how much of it is busy - measured here at both 4.8% and 0.7% of
// runs on the same code. No figure is quoted as a property of the code, because
// it is a property of the race window, and a reader who fails to reproduce one
// would wrongly conclude the bug is gone. This test rebuilds the state directly
// so it fails every time instead.
//
// The state is rebuilt by hand rather than raced for: a test that has to win a
// race to see the bug is the flake this fix removed, and the entry is put back
// into exactly the state the insert publishes - no stored result, no expiry,
// nobody holding e.mu.
func TestSeriesUnpopulatedEntryBooksNewNotEmpty(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 120, "cf", "ipv4", true)
	since := now.Add(-time.Hour)
	stats.ResetForTest()
	if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	ents := seriesEntries(st)
	if len(ents) != 1 {
		t.Fatalf("cache holds %d entries after one call, want 1", len(ents))
	}
	e := ents[0]
	e.mu.Lock()
	e.scanned, e.expires, e.pts = false, time.Time{}, nil
	e.mu.Unlock()

	pts, err := st.Series(ctx, since, time.Time{}, 60, nil)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("the window returned no points, so .empty would be the right name for it and this test cannot tell the two states apart")
	}
	wantCounters(t, seriesCounters(), map[string]int64{
		"series.cache.new": 2, "series.query": 2,
	}, "an entry nobody has filled yet is a first sight, not a window with no rows in it")
}

// The duration histogram must observe every real scan and nothing else: a hit
// that recorded a near-zero duration would drag the mean down and make an
// expensive aggregate look cheap.
func TestSeriesQueryHistogramObservesOnlyRealScans(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 120, "cf", "ipv4", true)
	since := now.Add(-time.Hour)
	stats.ResetForTest()
	for i := 0; i < 3; i++ { // one scan, then two hits
		if _, err := st.Series(ctx, since, time.Time{}, 60, nil); err != nil {
			t.Fatalf("series: %v", err)
		}
	}
	h, ok := stats.Lifetime().Histos["series.query.seconds"]
	if !ok {
		t.Fatal("no series.query.seconds histogram: the scan duration is not being observed at all")
	}
	if h.Count != 1 {
		t.Errorf("histogram counted %d observations over 1 scan + 2 hits, want 1", h.Count)
	}
	if got := seriesCounters()["series.query"]; int64(h.Count) != got {
		t.Errorf("histogram count %d != series.query %d; the two must move together or the mean is over the wrong denominator", h.Count, got)
	}
}
