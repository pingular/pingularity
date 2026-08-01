package logbuf

import "testing"

// Clearing the log has to reach every viewer, not just the tab that clicked it.
//
// A caught-up viewer polls with the cursor it already holds. Clear evicts the
// lines and advances `dropped` past them, so the question is what that poll gets
// back and whether it can tell the difference between "nothing new" and
// "everything you are showing is gone".
func TestClearIsVisibleToACaughtUpCursor(t *testing.T) {
	r := New(100)
	for i := 0; i < 5; i++ {
		r.Append("line", "line")
	}
	// A viewer that has read everything.
	_, _, next := r.Since(0, 0)
	if next != 5 {
		t.Fatalf("cursor after reading 5 lines = %d, want 5", next)
	}

	before := r.Epoch()
	r.Clear()

	// The lines and cursor alone cannot carry the news: a caught-up viewer's poll
	// returns nothing and the same next_seq it sent, which is exactly what an idle
	// poll returns. Confirm that, so the epoch check below is understood as the
	// only signal available rather than a belt-and-braces extra.
	out, first, after := r.Since(next, 0)
	if len(out) != 0 || after != next || first != next {
		t.Fatalf("fixture assumption broken: after Clear a caught-up cursor got lines=%d first=%d next=%d",
			len(out), first, after)
	}

	// So the epoch has to move. It already means "your cursor is not meaningful
	// against this buffer, resync", and the viewer repaints whole on seeing it
	// change - the same path a restart uses.
	if got := r.Epoch(); got == before {
		t.Errorf("epoch is still %q after Clear; a caught-up viewer's next poll is "+
			"indistinguishable from an idle one, so every other tab keeps rendering the "+
			"lines the operator wiped", got)
	}
}

// A viewer holding a PRE-clear cursor (mid-scroll, or just behind) must not be
// handed lines that no longer exist, and must not be told to append to a view
// whose contents are gone.
func TestClearInvalidatesAStaleCursor(t *testing.T) {
	r := New(100)
	for i := 0; i < 5; i++ {
		r.Append("line", "line")
	}
	r.Clear()
	for i := 0; i < 2; i++ {
		r.Append("fresh", "fresh")
	}

	// A viewer that was two lines behind when the clear happened.
	out, first, _ := r.Since(3, 0)
	for _, e := range out {
		if e.Raw != "fresh" {
			t.Errorf("a stale cursor was served %q, which was cleared", e.Raw)
		}
	}
	if first < 5 {
		t.Errorf("first_seq = %d, want >= 5: the cleared lines' sequence range must not be "+
			"reusable, or a stale cursor silently addresses different content", first)
	}
}
