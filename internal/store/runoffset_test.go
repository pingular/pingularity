package store

import (
	"context"
	"testing"
)

// SpeedRunOffset must return the zero-based position of a run within the
// newest-first ordering used by SpeedRuns, so the dashboard can divide it by
// the page size and open the runs table on the row for a clicked chart point.
func TestSpeedRunOffset(t *testing.T) {
	st := open(t)
	ctx := context.Background()

	// Insert out of order; ts values are unique seconds (as real runs are).
	tss := []int64{500, 100, 400, 200, 300}
	for _, ts := range tss {
		if err := st.InsertSpeed(ctx, SpeedSample{TS: ts, DownMbps: 1, Server: "x"}); err != nil {
			t.Fatal(err)
		}
	}

	// Newest-first order is 500,400,300,200,100 -> offsets 0..4.
	want := map[int64]int{500: 0, 400: 1, 300: 2, 200: 3, 100: 4}
	for ts, off := range want {
		got, err := st.SpeedRunOffset(ctx, ts)
		if err != nil {
			t.Fatal(err)
		}
		if got != off {
			t.Fatalf("offset(ts=%d) = %d, want %d", ts, got, off)
		}
		// The page derived from the offset must actually contain that run.
		const perPage = 2
		page := off / perPage // zero-based page
		runs, err := st.SpeedRuns(ctx, perPage, page*perPage)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range runs {
			if r.TS == ts {
				found = true
			}
		}
		if !found {
			t.Fatalf("ts=%d (offset %d) not on page %d (perPage %d)", ts, off, page, perPage)
		}
	}

	// A timestamp newer than every run sits at offset 0; older than all, at the end.
	if got, _ := st.SpeedRunOffset(ctx, 9999); got != 0 {
		t.Fatalf("offset of newest-of-all = %d, want 0", got)
	}
	if got, _ := st.SpeedRunOffset(ctx, 1); got != len(tss) {
		t.Fatalf("offset of oldest-of-all = %d, want %d", got, len(tss))
	}
}
