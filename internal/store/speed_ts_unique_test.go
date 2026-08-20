package store

import (
	"context"
	"testing"
)

// ts is a speed row's identity everywhere - DeleteSpeed's key, the UI's run
// handle, the backup merge's key (which keeps the FIRST row per key and drops
// the rest). DeleteSpeed's own comment concedes "a same-second collision would
// delete both", resting on "runs are serialized, so ts is effectively unique" -
// but the accounting rows break that premise: a failed or aborted run stamps
// time.Now() with no collision avoidance, so it can land on the same second as
// a measurement (an abort right after a persist, a reconnect burst, a clock
// step), and partial results now write an accounting row on every slow-uplink
// run. These tests pin the fix: the INSERT is what guarantees uniqueness, by
// allocating the next free second.

// TestInsertSpeedNeverSharesASecond: two rows asking for the same second must
// not both land on it - the second one takes the next free second, and the
// caller learns which one it got.
func TestInsertSpeedNeverSharesASecond(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const want = int64(1_700_000_000)

	ts1, err := st.InsertSpeedTS(ctx, SpeedSample{TS: want, DownMbps: 50, Server: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ts2, err := st.InsertSpeedTS(ctx, SpeedSample{TS: want, Server: "b", Failed: true,
		DownBytes: i64p(4096)})
	if err != nil {
		t.Fatal(err)
	}
	if ts1 != want {
		t.Fatalf("first insert moved off a free second: got %d, want %d", ts1, want)
	}
	if ts2 == ts1 {
		t.Fatalf("two rows share second %d - ts is identity (delete, merge, UI) and a shared second makes them one run", ts1)
	}
	if ts2 != want+1 {
		t.Fatalf("second insert took %d, want the NEXT free second %d", ts2, want+1)
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE ts = ?`, want).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows at second %d, want exactly 1", n, want)
	}
}

// TestSameSecondFailedUsageSurvivesRunDelete: deleting a measurement must not
// take an unrelated failed run's accounting row with it. Before insert-time
// uniqueness the two could share a second and DeleteSpeed's
// `ts = ? AND (usage_run_ts IS NULL OR ...)` matched both - un-billing the
// failed run's bytes silently, with nothing ever listing the loss.
func TestSameSecondFailedUsageSurvivesRunDelete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	const ts = int64(1_700_000_100)

	if _, err := st.InsertSpeedTS(ctx, SpeedSample{TS: ts, DownMbps: 80, Server: "run"}); err != nil {
		t.Fatal(err)
	}
	// An unrelated wholly-failed run, stamped the same second by its clock.
	failedTS, err := st.InsertSpeedTS(ctx, SpeedSample{TS: ts, Server: "other", Failed: true,
		UpBytes: i64p(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.DeleteSpeed(ctx, ts); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE ts = ? AND failed = 1`, failedTS).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("deleting the measurement also deleted the unrelated failed run's accounting row - its bytes just got silently un-billed")
	}
}

func i64p(n int64) *int64 { return &n }

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
