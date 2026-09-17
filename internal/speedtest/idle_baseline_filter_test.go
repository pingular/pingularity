package speedtest

import (
	"context"
	"math"
	"testing"
	"time"
)

// A POLLUTED BASELINE SILENTLY ZEROES BLOAT. Bufferbloat is max(0, loaded -
// idle), so a SYN retransmit landing in the IDLE burst inflates the baseline
// by ~1s and erases real bloat from the result - the wrong-zero failure
// observed in production (idle_ms of ~510 and ~1013 on a 7 ms link). An idle
// burst whose middle is retransmits has no honest median: the filtered count
// falls under lulIdleMin and the baseline must be nil, never a number a
// retransmit helped choose.
func TestIdleBaselineDiscardsRetransmits(t *testing.T) {
	// Half the burst lost its first SYN: the field shape on a path dropping
	// ~44% of SYNs. The unfiltered even-count median averages a clean 7.1 with
	// a retransmitted 1013.5 into ~510 - a latency the link never produced,
	// stored as the baseline every loaded phase is judged against.
	bimodal := []float64{6.8, 1013.9, 7.1, 1014.2, 6.9, 1013.5}
	if got := summarizeIdle(bimodal); got != nil {
		t.Fatalf("idle baseline = %v ms from a burst that was half retransmits; "+
			"want nil - only 3 samples survive the retransmit filter, under lulIdleMin=%d, "+
			"and a ~510 ms baseline here silently zeroes real bufferbloat", *got, lulIdleMin)
	}
	// Mostly retransmits: the unfiltered median is ~1013 - pure RTO timer,
	// zero latency information. Also nil.
	rto := []float64{7.0, 1013.2, 1013.9, 1014.2, 1014.8, 1013.5, 7.2, 1014.1}
	if got := summarizeIdle(rto); got != nil {
		t.Fatalf("idle baseline = %v ms from a burst that was mostly retransmits; want nil", *got)
	}
	// A minority of retransmits leaves enough honest samples: the median is
	// taken over the six clean ones alone.
	ok := []float64{7.0, 6.8, 1013.9, 7.2, 7.1, 6.9, 1014.4, 7.3}
	got := summarizeIdle(ok)
	if got == nil {
		t.Fatalf("idle baseline = nil; six clean samples survive the filter, over lulIdleMin=%d", lulIdleMin)
	}
	if want := 7.05; math.Abs(*got-want) > 1e-9 {
		t.Fatalf("idle baseline = %v, want %v: the median of the six clean samples "+
			"with both retransmits discarded", *got, want)
	}
}

// THE GUARD IS RELATIVE TO THE BURST MINIMUM, NOT AN ABSOLUTE CUTOFF. A lost
// SYN retransmits on an RTO of at least one second on every OS we ship, so on
// ANY link the retransmits sit >= 1s above the honest samples - a 600 ms
// satellite path clusters near 600 with its retransmits near 1600. An absolute
// threshold would discard that link's genuine latency wholesale and report no
// baseline forever.
func TestIdleFilterKeepsHighLatencyLinks(t *testing.T) {
	sat := []float64{598, 602, 611, 1601, 605, 1608, 600, 603}
	got := summarizeIdle(sat)
	if got == nil {
		t.Fatal("idle baseline = nil for a 600 ms link with two retransmits; the filter " +
			"ate genuine high latency - the guard must be relative to the burst minimum")
	}
	if want := 602.5; math.Abs(*got-want) > 1e-9 {
		t.Fatalf("idle baseline = %v, want %v: the median of the six ~600 ms samples "+
			"with the ~1600 ms retransmits discarded", *got, want)
	}
	// The pure filter at its boundary: exactly guard-above-minimum stays (an
	// RTO adds >= 1s, so it cannot be one), just beyond goes.
	ms := []float64{100, 100 + lulRetransmitGuardMS, 100 + lulRetransmitGuardMS + 0.1}
	kept := dropRetransmits(ms)
	if len(kept) != 2 || kept[0] != 100 || kept[1] != 100+lulRetransmitGuardMS {
		t.Fatalf("dropRetransmits(%v) = %v, want only the two samples within min+%v kept",
			ms, kept, lulRetransmitGuardMS)
	}
	if kept := dropRetransmits(nil); len(kept) != 0 {
		t.Fatalf("dropRetransmits(nil) = %v, want empty", kept)
	}
}

// AN ALL-RETRANSMIT BURST HAS NO HONEST SAMPLE TO ANCHOR ON. The min-relative
// rule measures each sample against the burst's own minimum, so a burst where
// every SYN was retransmitted anchors on a retransmit: min+guard then covers
// all the rest and nothing is filtered at all. That is the production failure
// this filter exists to kill - an idle baseline of ~1013 ms, pure RTO timer,
// which zeroes every bit of real bloat measured against it. On a path dropping
// ~44% of SYNs an all-retransmit burst is ordinary, not a corner case.
func TestIdleBaselineRejectsAllRetransmitBurst(t *testing.T) {
	allRTO := []float64{1013.2, 1013.9, 1014.2, 1013.1, 1014.0, 1012.8, 1013.6, 1014.4}
	if got := summarizeIdle(allRTO); got != nil {
		t.Fatalf("idle baseline = %v ms from a burst that was ENTIRELY retransmits; want nil - "+
			"every sample is one RTO above the path, so the burst holds no latency at all and "+
			"a baseline near 1013 ms silently zeroes real bufferbloat", *got)
	}
	if kept := dropRetransmits(allRTO); len(kept) != 0 {
		t.Fatalf("dropRetransmits(%v) = %v, want nothing kept: the minimum is itself a "+
			"retransmit, so there is no honest reference point in this burst", allRTO, kept)
	}
}

// THE BACKSTOP MUST NOT EAT A SLOW LINK. It is keyed on the burst MINIMUM
// against lulRetransmitFloorMS - one RTO above a zero-RTT path - so a real
// 600 ms satellite link stays whole whether or not its burst also carries
// retransmits near 1600 ms. Named exception: a link whose genuine idle RTT is
// at or above the floor reports no baseline, which beats reporting one that
// cannot be told from an RTO ladder.
func TestRetransmitBackstopCases(t *testing.T) {
	for _, c := range []struct {
		name string
		ms   []float64
		want []float64
	}{
		{"satellite, clean", []float64{598, 602, 611, 605, 600}, []float64{598, 602, 611, 605, 600}},
		{"satellite, two retransmits", []float64{598, 1601, 611, 1608, 600}, []float64{598, 611, 600}},
		{"satellite, all retransmits", []float64{1601, 1608, 1605}, nil},
		{"single honest sample", []float64{7.1}, []float64{7.1}},
		{"single retransmit", []float64{1013.9}, nil},
		{"single sample just under the floor", []float64{lulRetransmitFloorMS - 0.1}, []float64{lulRetransmitFloorMS - 0.1}},
		{"whole burst at the floor", []float64{lulRetransmitFloorMS, lulRetransmitFloorMS + 1}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := dropRetransmits(c.ms)
			if len(got) != len(c.want) {
				t.Fatalf("dropRetransmits(%v) = %v, want %v", c.ms, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("dropRetransmits(%v) = %v, want %v", c.ms, got, c.want)
				}
			}
		})
	}
}

// THE ANCHOR IS THE BURST MINIMUM, NOT THE FIRST SAMPLE. Nothing says the
// retransmit arrives late in the burst - the observed field shape starts with
// one about as often as not. Anchoring on ms[0] instead of the minimum keeps
// every sample within min+guard of a retransmit, and this burst then publishes
// a 510.3 ms baseline: the exact wrong number seen in production, the average
// of a 7 ms link and its own RTO.
func TestIdleFilterAnchorsOnBurstMinimum(t *testing.T) {
	leading := []float64{1013.9, 6.8, 1014.2, 7.1, 1013.5, 6.9}
	kept := dropRetransmits(leading)
	want := []float64{6.8, 7.1, 6.9}
	if len(kept) != len(want) {
		t.Fatalf("dropRetransmits(%v) = %v, want the three honest samples %v: the filter "+
			"anchored on the leading retransmit instead of the burst minimum", leading, kept, want)
	}
	for i := range kept {
		if kept[i] != want[i] {
			t.Fatalf("dropRetransmits(%v) = %v, want %v", leading, kept, want)
		}
	}
	if got := summarizeIdle(leading); got != nil {
		t.Fatalf("idle baseline = %v ms from a burst whose FIRST sample is a retransmit; "+
			"want nil - only 3 honest samples survive, under lulIdleMin=%d (anchoring on "+
			"ms[0] instead publishes 510.3 ms, the production wrong number)", *got, lulIdleMin)
	}
	// Same shape with enough honest samples to keep: the median is theirs alone,
	// 7.05 - anchoring on the leading 1013.9 would keep the whole burst instead.
	leadingOK := []float64{1013.9, 6.8, 7.1, 7.0, 6.9, 7.2, 7.3, 1014.2}
	got := summarizeIdle(leadingOK)
	if got == nil {
		t.Fatal("idle baseline = nil; six honest samples survive a leading retransmit, over lulIdleMin")
	}
	if want := 7.05; math.Abs(*got-want) > 1e-9 {
		t.Fatalf("idle baseline = %v, want %v: the median of the six honest samples, "+
			"with the leading and trailing retransmits both discarded", *got, want)
	}
}

// THE SAMPLER MUST REDUCE WITH summarizeIdle, NOT A RAW MEDIAN. Asserting the
// filter's arithmetic proves nothing about what a run stores if
// measureIdleLatency still medians the unfiltered burst. Loopback cannot drop
// SYNs, so the burst seam hands back the field shapes and the assertions pin
// which reduction ran on them.
func TestIdleMeasurementFiltersRetransmits(t *testing.T) {
	origBurst := lulProbeBurst
	t.Cleanup(func() { lulProbeBurst = origBurst })

	lulProbeBurst = func(_ context.Context, _ string, n int, _ time.Duration) ([]float64, int) {
		if n != lulIdleProbes {
			t.Errorf("idle burst asked for %d probes, want lulIdleProbes=%d", n, lulIdleProbes)
		}
		return []float64{6.8, 1013.9, 7.1, 1014.2, 6.9, 1013.5}, 0
	}
	if got := measureIdleLatency(context.Background(), "192.0.2.1:443"); got != nil {
		t.Fatalf("measureIdleLatency = %v ms from a half-retransmit burst; want nil - the "+
			"sampler medianed the unfiltered burst, storing a baseline fabricated by SYN retransmits", *got)
	}

	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		return []float64{7.0, 6.8, 1013.9, 7.2, 7.1, 6.9, 1014.4, 7.3}, 0
	}
	got := measureIdleLatency(context.Background(), "192.0.2.1:443")
	if got == nil {
		t.Fatal("measureIdleLatency = nil; six clean samples survive the filter, over lulIdleMin")
	}
	// 7.15 here means the two retransmits were still in the medianed slice.
	if want := 7.05; math.Abs(*got-want) > 1e-9 {
		t.Fatalf("measureIdleLatency = %v, want %v: the median of the clean samples only", *got, want)
	}
}

// THE IDLE BURST IS BOUNDED AND SPACED. The cap is what keeps a manual Run Now
// from looking hung on a firewalled LAN, where every probe can sit out
// lulConnTimeout; the spacing is what makes the probes independent looks at the
// queue rather than one look repeated. And the cap has to hold an honest burst
// on a slow link, or the baseline comes back short by exactly the probes a
// satellite path needed.
func TestIdleBurstBudgetAndSpacing(t *testing.T) {
	origBurst := lulProbeBurst
	t.Cleanup(func() { lulProbeBurst = origBurst })

	var gotN int
	var gotGap, left time.Duration
	bounded := false
	lulProbeBurst = func(ctx context.Context, _ string, n int, gap time.Duration) ([]float64, int) {
		gotN, gotGap = n, gap
		if d, ok := ctx.Deadline(); ok {
			bounded, left = true, time.Until(d)
		}
		return nil, n
	}
	measureIdleLatency(context.Background(), "192.0.2.1:443")

	if !bounded {
		t.Fatalf("the idle burst ran unbounded: every probe of it can take lulConnTimeout=%v on "+
			"a firewalled LAN, so %d of them make a manual run look hung", lulConnTimeout, lulIdleProbes)
	}
	if left > lulIdleBudget+100*time.Millisecond {
		t.Errorf("the idle burst has %v to run, past lulIdleBudget=%v", left.Round(time.Millisecond), lulIdleBudget)
	}
	if gotN != lulIdleProbes || gotGap != lulIdleGap {
		t.Errorf("idle burst = %d probes %v apart, want lulIdleProbes=%d probes lulIdleGap=%v apart: "+
			"back-to-back probes sample one instant of one queue", gotN, gotGap, lulIdleProbes, lulIdleGap)
	}
	// A 600 ms satellite link - slow, and well inside what this metric measures.
	if slow := lulIdleProbes*600*time.Millisecond + (lulIdleProbes-1)*lulIdleGap; lulIdleBudget < slow {
		t.Errorf("lulIdleBudget = %v where a 600 ms link needs %v for a full burst: the cap cuts "+
			"the baseline short on exactly the links whose bloat matters most", lulIdleBudget, slow)
	}
}
