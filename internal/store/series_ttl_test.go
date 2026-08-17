package store

import (
	"context"
	"testing"
	"time"
)

// seriesEntryExpiry returns the single cached entry's expiry. The TTL is not
// observable from the outside - a caller only sees points - so the tests below
// read it here rather than sleeping out a candidate TTL and guessing from
// whether the answer changed.
func seriesEntryExpiry(t *testing.T, st *Store) time.Time {
	t.Helper()
	// Via seriesEntries, so seriesMu is already released before e.mu is taken -
	// see the lock-ordering note there.
	entries := seriesEntries(st)
	if len(entries) != 1 {
		t.Fatalf("seriesCache holds %d entries, want exactly 1 to read a TTL from", len(entries))
	}
	e := entries[0]
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.expires
}

// A live (open-ended) window's TTL is CAPPED at 120s: a young install's wide
// window is one trailing partial bucket that changes every probe round, and the
// natural quarter-bucket TTL would be ~88 minutes at a year - a first-run chart
// pinned near-empty for an hour and a half.
func TestSeriesLiveWideWindowTTLBoundedAt120s(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	// One sample: the young, NON-empty install the bound exists for. (An empty
	// result takes a different path - it is never cached at all.)
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	const yearBucket = 365 * 24 * 3600 / 1500 // 21024s, the width a 1y range asks for
	if _, err := st.Series(ctx, now.Add(-365*24*time.Hour), time.Time{}, yearBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	ttl := time.Until(seriesEntryExpiry(t, st))
	if ttl > 120*time.Second {
		t.Errorf("live 1y window cached for %v, want at most 120s: a young install would pin a near-empty chart for %v", ttl, ttl)
	}
	if ttl < 110*time.Second {
		t.Errorf("live 1y window cached for only %v, want ~120s: the cap is the bound on a wide window, and a shorter one re-runs the scan for nothing", ttl)
	}
}

// The cap is a CAP, not a floor. The 7d preset buckets to 403s (604800/1500,
// seriesBucket web.go:1193) and keeps its own quarter-bucket TTL of 100.75s -
// raising it to 120s would hand the widest preset MORE staleness than its own
// bucket justifies, and clamping it back to 30s is the load the change removes.
func TestSeriesSevenDayPresetKeepsQuarterBucketTTL(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	const weekBucket = 7 * 24 * 3600 / 1500 // 403s
	if _, err := st.Series(ctx, now.Add(-7*24*time.Hour), time.Time{}, weekBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	ttl := time.Until(seriesEntryExpiry(t, st))
	if ttl > time.Duration(weekBucket)*time.Second/4 {
		t.Errorf("7d window cached for %v, want at most its own quarter bucket (100.75s)", ttl)
	}
	if ttl <= 30*time.Second {
		t.Errorf("7d window cached for only %v: the 30s clip is back, and at a 60s chart poll that is a full re-scan every poll", ttl)
	}
	if ttl < 95*time.Second {
		t.Errorf("7d window cached for %v, want ~100.75s (403/4)", ttl)
	}
}

// A window whose end is in the FUTURE is fixed but not historical: new samples
// keep landing inside it, so it is capped like an open-ended one. The UI really
// produces these - a typed range clamps to now+366d, not to now - and without
// the cap a 365-day span would pin fresh samples out of the chart for its full
// quarter bucket, ~88 minutes.
func TestSeriesFutureEndedYearWindowStaysCapped(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 30, "cf", "ipv4", true)
	const yearBucket = 365 * 24 * 3600 / 1500 // 21024s -> 5256s uncapped
	if _, err := st.Series(ctx, now.Add(-335*24*time.Hour), now.Add(30*24*time.Hour), yearBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	ttl := time.Until(seriesEntryExpiry(t, st))
	if ttl > 120*time.Second {
		t.Errorf("future-ended 365d window cached for %v, want at most 120s: samples still arriving inside it would be pinned out of the chart", ttl)
	}
}

// A window that has genuinely ENDED in the past keeps the full quarter-bucket
// TTL: nothing new can enter it, so re-running a multi-second scan for it is
// pure waste. The cap must not leak onto this path.
func TestSeriesHistoricalWindowKeepsFullBucketTTL(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	sampleAt(t, st, now, 100*24*3600, "cf", "ipv4", true) // inside the window below
	const yearBucket = 365 * 24 * 3600 / 1500
	if _, err := st.Series(ctx, now.Add(-400*24*time.Hour), now.Add(-30*24*time.Hour), yearBucket, nil); err != nil {
		t.Fatalf("series: %v", err)
	}
	ttl := time.Until(seriesEntryExpiry(t, st))
	if ttl <= 120*time.Second {
		t.Errorf("past-ended window cached for only %v: a closed window can never change, so it should hold its full quarter bucket (%v)",
			ttl, time.Duration(yearBucket)*time.Second/4)
	}
}

// An empty result is never cached, at any TTL. A fresh install that opens a wide
// range once would otherwise freeze an empty chart until the TTL ran out - and
// raising the cap raises exactly that freeze, so the guard is checked here too.
func TestSeriesEmptyResultLeavesEntryUncached(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	const yearBucket = 365 * 24 * 3600 / 1500
	pts, err := st.Series(ctx, time.Now().Add(-365*24*time.Hour), time.Time{}, yearBucket, nil)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(pts) != 0 {
		t.Fatalf("fresh store returned %d points, want 0", len(pts))
	}
	if exp := seriesEntryExpiry(t, st); !exp.IsZero() {
		t.Errorf("empty result cached until %v (in %v); it must stay uncached so the first samples show at once", exp, time.Until(exp))
	}
}
