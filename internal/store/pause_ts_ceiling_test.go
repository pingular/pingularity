package store

import (
	"math"
	"testing"
)

// The pause door and the at-Open repair must state ONE ts ceiling. They used
// to disagree by a band: pauseSpanBounded accepted ts <= MaxInt64-dur
// (per-row), while the repair's clock-free DELETE removes ts >
// MaxInt64-maxPauseDuration (blanket, constant arguments so no per-row SQL
// arithmetic can overflow). A row in the band - ts=MaxInt64-100, dur=50 -
// was therefore accepted at the door only to be deleted at the next Open and
// mislogged as an older build's unvalidated residue. A start within ten
// years of int64's end is no wall time anyway (year 292 billion), so the
// door refuses the whole band and the two rules agree exactly.

func TestPauseDoorRefusesTheBandTheRepairDeletes(t *testing.T) {
	band := int64(math.MaxInt64) - 100
	if pauseSpanBounded(band, 50) {
		t.Errorf("pauseSpanBounded(MaxInt64-100, 50) = true; the door accepts a row the "+
			"at-Open repair blanket-deletes (ceiling MaxInt64-%d), then mislogs it as residue", int64(maxPauseDuration))
	}
	// The importer inherits the refusal through pauseSpanBounded - including under
	// the epoch-clock fallback, the only door such a row could realistically use.
	if pauseSpanImportable(band, 50, 120) {
		t.Errorf("pauseSpanImportable(MaxInt64-100, 50, epoch clock) = true; the epoch fallback " +
			"must not re-open the band the repair deletes")
	}
	if PauseSpanSane(band, 50) {
		t.Errorf("PauseSpanSane(MaxInt64-100, 50) = true; the live writer must refuse the band too")
	}
}

// The other direction of the agreement: everything the door accepts must
// survive the repair's clock-free half untouched - driven through both rules
// at the exact ceiling.
func TestClockFreeRepairNeverTouchesWhatTheDoorAccepts(t *testing.T) {
	s := guardStore2(t)
	edge := int64(math.MaxInt64) - maxPauseDuration // the shared ceiling, inclusive
	if !pauseSpanBounded(edge, maxPauseDuration) {
		t.Fatalf("premise broken: pauseSpanBounded at the ceiling = false; the bound is meant to be inclusive")
	}
	seedLegacyPause(t, s, edge, maxPauseDuration)
	seedLegacyPause(t, s, edge+1, 50) // one past the ceiling: door refuses, repair deletes
	if pauseSpanBounded(edge+1, 50) {
		t.Fatalf("premise broken: pauseSpanBounded one past the ceiling = true")
	}
	// Epoch clock: only the clock-free half runs (the now-anchored half would
	// remove any far-future endpoint, masking a band disagreement).
	if err := repairInsanePausesAt(s.db, 120); err != nil {
		t.Fatalf("repair: %v", err)
	}
	rows, err := s.db.Query(`SELECT ts FROM pauses ORDER BY ts`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var kept []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		kept = append(kept, ts)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(kept) != 1 || kept[0] != edge {
		t.Errorf("clock-free repair kept %v, want exactly [%d]: the repair must remove precisely "+
			"the rows the door refuses, nothing the door accepted", kept, edge)
	}
}
