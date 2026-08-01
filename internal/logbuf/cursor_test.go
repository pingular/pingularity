package logbuf

import (
	"fmt"
	"testing"
)

// A reader's cursor has to survive the ring evicting under it. The sequence is
// derived from the count of entries EVICTED, not from a slice index, so the
// index every survivor gets when trim() compacts them to the front is
// irrelevant to it - which is the whole reason an offset (what /api/events
// pages by) is the wrong cursor for this endpoint.
func TestSinceSurvivesEviction(t *testing.T) {
	r := New(4)
	for i := 0; i < 4; i++ {
		r.Append(fmt.Sprintf("line %d", i), fmt.Sprintf("masked %d", i))
	}
	_, first, next := r.Tail(0)
	if first != 0 || next != 4 {
		t.Fatalf("fresh ring: first=%d next=%d, want 0/4", first, next)
	}
	// Read everything, then let the ring turn over completely.
	for i := 4; i < 10; i++ {
		r.Append(fmt.Sprintf("line %d", i), fmt.Sprintf("masked %d", i))
	}
	got, first, next := r.Since(4, 0)
	if first != 6 {
		t.Fatalf("after 6 evictions first=%d, want 6 (the sequence must count evictions, not restart at the slice head)", first)
	}
	if next != 10 {
		t.Fatalf("next=%d, want 10", next)
	}
	// The cursor is below first: those lines are gone. Resume at the oldest still
	// held rather than returning nothing or - worse - the wrong four lines.
	if len(got) != 4 || got[0].Raw != "line 6" || got[3].Raw != "line 9" {
		t.Fatalf("evicted cursor returned %d entries starting %q; want the 4 still held, line 6..line 9", len(got), raw0(got))
	}
	// A live cursor gets exactly what arrived after it, oldest first.
	got, _, next = r.Since(8, 0)
	if len(got) != 2 || got[0].Raw != "line 8" || got[1].Raw != "line 9" {
		t.Fatalf("Since(8) = %d entries starting %q; want line 8, line 9", len(got), raw0(got))
	}
	if next != 10 {
		t.Fatalf("Since(8) next=%d, want 10", next)
	}
	// Caught up: nothing new, and the cursor does not move.
	got, _, next = r.Since(10, 0)
	if len(got) != 0 || next != 10 {
		t.Fatalf("Since(10) = %d entries, next=%d; want 0 and 10 (the steady state is an empty response)", len(got), next)
	}
}

// A reader that has fallen far behind catches up over successive polls instead
// of skipping the middle: a limited window returns the OLDEST entries after the
// cursor, never the newest.
func TestSinceReturnsOldestFirstUnderLimit(t *testing.T) {
	r := New(100)
	for i := 0; i < 20; i++ {
		r.Append(fmt.Sprintf("line %d", i), "")
	}
	got, _, next := r.Since(0, 5)
	if len(got) != 5 || got[0].Raw != "line 0" || got[4].Raw != "line 4" {
		t.Fatalf("Since(0,5) = %d entries starting %q; want line 0..line 4", len(got), raw0(got))
	}
	if next != 5 {
		t.Fatalf("Since(0,5) next=%d, want 5 - a reader that jumped to the newest window would silently lose lines 0..14", next)
	}
	// Tail is the other direction: the NEWEST limit entries, for a cold open.
	got, _, next = r.Tail(5)
	if len(got) != 5 || got[0].Raw != "line 15" || got[4].Raw != "line 19" {
		t.Fatalf("Tail(5) = %d entries starting %q; want line 15..line 19", len(got), raw0(got))
	}
	if next != 20 {
		t.Fatalf("Tail(5) next=%d, want 20", next)
	}
}

// Clear EVICTS the buffered lines, so the sequence must keep climbing past them.
// Resetting it to zero would make a cursor held from before the Clear look
// valid, and the viewer would silently re-render lines the operator just wiped.
func TestClearAdvancesTheSequence(t *testing.T) {
	r := New(100)
	for i := 0; i < 6; i++ {
		r.Append(fmt.Sprintf("line %d", i), "")
	}
	_, _, next := r.Tail(0)
	if next != 6 {
		t.Fatalf("next=%d, want 6", next)
	}
	r.Clear()
	for i := 6; i < 9; i++ {
		r.Append(fmt.Sprintf("line %d", i), "")
	}
	got, first, _ := r.Since(next, 0)
	if first != 6 {
		t.Fatalf("after Clear first=%d, want 6 (Clear must advance the sequence, not reset it)", first)
	}
	if len(got) != 3 || got[0].Raw != "line 6" {
		t.Fatalf("a cursor held across Clear returned %d entries starting %q; want only the 3 lines recorded after it", len(got), raw0(got))
	}
}

// Two rings are two different sequence spaces, and a client cannot tell them
// apart from the numbers alone - which is exactly what a restart produces, since
// LoadFile reseeds from logs.txt with no persisted sequence.
func TestEpochDistinguishesRings(t *testing.T) {
	a, b := New(10), New(10)
	if a.Epoch() == "" || b.Epoch() == "" {
		t.Fatal("a ring must always name itself")
	}
	if a.Epoch() == b.Epoch() {
		t.Fatalf("two rings share epoch %q; a cursor from one would look valid against the other", a.Epoch())
	}
}

func raw0(e []Entry) string {
	if len(e) == 0 {
		return ""
	}
	return e[0].Raw
}
