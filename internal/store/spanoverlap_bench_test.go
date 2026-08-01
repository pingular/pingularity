package store

import (
	"testing"
)

// The heatmap asks spanOverlap the same question once per local day and once per
// outage segment, over the same span list. A year of five-minute checkpoint pause
// rows is ~52k spans (the figure pausedOverlap's own comment uses), and a scan of
// all of them per question is what makes a 60-second poll expensive.
//
// The spans are sorted and merged (see pauseSpans/mergeSpans), so the answer never
// needs a full scan: everything before the window and everything after it can be
// skipped outright.
func benchSpans(n int) [][2]int64 {
	spans := make([][2]int64, n)
	// Five-minute pauses, ten minutes apart - disjoint and ascending, the shape
	// checkpoint flushes actually produce.
	for i := range spans {
		start := int64(i) * 600
		spans[i] = [2]int64{start, start + 300}
	}
	return spans
}

// One day's worth of window against a year of spans - the per-day question.
func BenchmarkSpanOverlapOneDayAgainstAYearOfPauses(b *testing.B) {
	spans := benchSpans(52_000)
	// A window in the middle, so neither end short-circuits trivially.
	from := int64(26_000) * 600
	to := from + 86_400
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := spanOverlap(spans, from, to); got == -1 {
			b.Fatal("unreachable")
		}
	}
}

// And the whole year of days, which is what one heatmap render costs.
func BenchmarkSpanOverlapAYearOfDays(b *testing.B) {
	spans := benchSpans(52_000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var total int64
		for d := 0; d < 366; d++ {
			from := int64(d) * 86_400
			total += spanOverlap(spans, from, from+86_400)
		}
		if total == -1 {
			b.Fatal("unreachable")
		}
	}
}

// naiveSpanOverlap is the previous full-scan implementation, kept as the oracle:
// the optimisation must not change a single answer, and "it looks right" is not
// evidence when the change is a binary search over a precondition.
func naiveSpanOverlap(spans [][2]int64, from, to int64) int64 {
	var n int64
	for _, sp := range spans {
		a, b := sp[0], sp[1]
		if a < from {
			a = from
		}
		if b > to {
			b = to
		}
		if b > a {
			n += b - a
		}
	}
	return n
}

// Exhaustive over a small span set and every window in range, including the
// degenerate and out-of-range ones. Small enough to enumerate completely, which
// beats sampling for a boundary-heavy function.
func TestSpanOverlapMatchesTheFullScanEverywhere(t *testing.T) {
	spanSets := [][][2]int64{
		nil,
		{},
		{{10, 20}},
		{{10, 20}, {30, 40}},
		{{10, 20}, {20, 30}}, // touching
		{{0, 5}, {10, 15}, {20, 25}, {30, 35}},
		{{100, 200}}, // entirely after the probed windows
	}
	for si, spans := range spanSets {
		for from := int64(-5); from <= 45; from++ {
			for to := int64(-5); to <= 45; to++ {
				got := spanOverlap(spans, from, to)
				// The oracle has no to<=from guard, so apply the same one to it.
				var want int64
				if to > from {
					want = naiveSpanOverlap(spans, from, to)
				}
				if got != want {
					t.Fatalf("set %d, window [%d,%d): spanOverlap = %d, full scan = %d",
						si, from, to, got, want)
				}
			}
		}
	}
}
