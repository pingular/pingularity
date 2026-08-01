package store

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// PauseSpanSane's future-end check is anchored to the clock CHECKING the row, not
// the one that wrote it. On a restore that is the destination's clock, and when it
// runs behind the source every pause span ending in the gap is genuine by the
// source's clock and "future" by ours. The events and samples in the same file
// land; the uptime DENOMINATOR does not - the exact silent divergence the export
// format stamp exists to make loud. These tests pin the two halves of the fix:
// the drop is counted and logged, and an implausible destination clock (an
// RTC-less board before NTP sync) does not veto the whole pause history.

// A destination 40 minutes behind the source is arithmetically a span ending 40
// minutes ahead of the local clock. It is still refused - the future-end check is
// a deliberate tradeoff, because an accepted future-reaching span clamps into
// every queried window and Prune never repairs it - but the refusal must be
// visible: the file still holds the rows, and a re-run once the clock has synced
// lands them (the merge by ts is idempotent), so the operator has to be told.
func TestImportCountsDroppedPauseRows(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	stats.ResetForTest()

	// The event in the same backup restores fine...
	if n, err := s.ImportTable(ctx, "events", []map[string]any{
		{"ts": float64(now.Add(-30 * time.Minute).Unix()), "type": "down"},
	}); err != nil || n != 1 {
		t.Fatalf("events import = (%d, %v), want (1, nil)", n, err)
	}
	// ...while the pause span ending 40 minutes past our clock does not.
	n, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(now.Add(-20 * time.Minute).Unix()), "duration_s": float64(3600)},
	})
	if err != nil {
		t.Fatalf("pauses import: %v", err)
	}
	if n != 0 {
		t.Fatalf("pauses import applied %d row(s); the future-end tradeoff was supposed to refuse this span", n)
	}
	if got := stats.Lifetime().Counters["import.pause_dropped"]; got != 1 {
		t.Errorf("import.pause_dropped = %d, want 1: a dropped denominator row must never be silent - "+
			"the restored box otherwise publishes a different uptime from the machine the backup came from, "+
			"and nothing tells the operator to re-run the import after the clock syncs", got)
	}
}

// An accepted row must not touch the counter.
func TestImportDoesNotCountAcceptedPauseRows(t *testing.T) {
	s := guardStore2(t)
	ctx := context.Background()
	now := time.Now()
	stats.ResetForTest()

	if n, err := s.ImportTable(ctx, "pauses", []map[string]any{
		{"ts": float64(now.Add(-2 * time.Hour).Unix()), "duration_s": float64(600)},
	}); err != nil || n != 1 {
		t.Fatalf("import = (%d, %v), want (1, nil)", n, err)
	}
	if got := stats.Lifetime().Counters["import.pause_dropped"]; got != 0 {
		t.Errorf("import.pause_dropped = %d, want 0", got)
	}
}

// On an RTC-less board restored before NTP syncs, "now" is 1970 and now+skew
// predates every believable ts, so the destination-anchored check fails EVERY
// genuine span in the file: outages and samples restore, the whole denominator
// vanishes, and the box reports higher uptime and full coverage for windows the
// source knew were unobserved. A clock that early cannot anchor a judgement -
// Prune already declines to prune under it - so the importer must fall back to
// the clock-free bounds rather than veto the table.
func TestImportKeepsGenuinePausesUnderAnEpochClock(t *testing.T) {
	realNow := time.Now().Unix()
	const bootClock = int64(120) // seconds after the 1970 epoch, pre-NTP
	for _, tc := range []struct {
		name    string
		ts, dur int64
		want    bool
	}{
		{"an ordinary checkpointed pause from the backup", realNow - 3600, 300, true},
		{"a three-week hibernate from the backup", realNow - 30*24*3600, 21 * 24 * 3600, true},
		{"still refused: not a span", realNow - 3600, 0, false},
		{"still refused: longer than any history", realNow - 3600, maxPauseDuration + 1, false},
		{"still refused: starts before the project existed", plausibleEpoch - 1, 600, false},
	} {
		if got := pauseSpanImportable(tc.ts, tc.dur, bootClock); got != tc.want {
			t.Errorf("%s: pauseSpanImportable(%d, %d, epoch clock) = %v, want %v",
				tc.name, tc.ts, tc.dur, got, tc.want)
		}
	}
}

// Under a PLAUSIBLE clock the importer keeps the live rule's future-end
// tradeoff: the two writers still agree wherever both can judge.
func TestImportKeepsTheFutureEndCheckUnderAPlausibleClock(t *testing.T) {
	now := time.Now().Unix()
	for _, tc := range []struct {
		name    string
		ts, dur int64
	}{
		{"ends 40 minutes ahead", now - 1200, 3600},
		{"reaches a decade ahead", now - 7200, maxPauseDuration},
	} {
		if pauseSpanImportable(tc.ts, tc.dur, now) {
			t.Errorf("%s: accepted, but a span that means to reach forward is not a measurement", tc.name)
		}
		if PauseSpanSane(tc.ts, tc.dur) {
			t.Errorf("%s: the live rule accepted it too", tc.name)
		}
	}
}
