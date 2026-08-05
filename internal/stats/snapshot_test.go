package stats

import "testing"

// A SNAPSHOT IS A SNAPSHOT. Snap's comment promises callers cannot mutate live
// state, but the histogram copy shares its Bounds slice with the registry.
func TestSnapshotHistogramBoundsAreCopied(t *testing.T) {
	ResetForTest()
	Observe("review.hist", 0.003)
	snap := Lifetime()
	h, ok := snap.Histos["review.hist"]
	if !ok {
		t.Fatal("histogram missing from snapshot")
	}
	orig := h.Bounds[0]
	h.Bounds[0] = 999
	if got := Lifetime().Histos["review.hist"].Bounds[0]; got != orig {
		t.Errorf("mutating a snapshot's Bounds changed the live registry (%g -> %g)", orig, got)
	}
}
