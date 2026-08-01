package store

import (
	"context"
	"testing"
	"time"
)

// evAt inserts a transition event at an absolute unix-second timestamp
// (store_test.go's eventAt is relative to now; these tests need fixed stamps).
func evAt(t *testing.T, st *Store, ts int64, typ string, durationS int) {
	t.Helper()
	if err := st.InsertEvent(context.Background(), time.Unix(ts, 0), typ, durationS, ""); err != nil {
		t.Fatalf("insert event %s@%d: %v", typ, ts, err)
	}
}

func TestDeleteOutageRemovesThePair(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	evAt(t, st, base+1000, "down", -1)
	evAt(t, st, base+1100, "up", 100)
	evAt(t, st, base+2000, "down", -1)
	evAt(t, st, base+2300, "up", 300)
	evAt(t, st, base+5000, "down", -1) // in-progress / orphan, untouched throughout

	n, err := st.DeleteOutage(ctx, base+2300)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("deleted = %d, want 2 (the up and its down)", n)
	}
	if c, _ := st.EventCount(ctx); c != 3 {
		t.Fatalf("events left = %d, want 3", c)
	}
	// The earlier outage's pair and the trailing down are all still there.
	ev, err := st.EventsPage(ctx, 10, 0)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	want := map[int64]string{base + 1000: "down", base + 1100: "up", base + 5000: "down"}
	for _, e := range ev {
		if want[e.TS] != e.Type {
			t.Fatalf("unexpected survivor %s@%d", e.Type, e.TS)
		}
		delete(want, e.TS)
	}
	if len(want) != 0 {
		t.Fatalf("missing survivors: %v", want)
	}

	// Idempotent: the outage is already gone.
	if n, err := st.DeleteOutage(ctx, base+2300); err != nil || n != 0 {
		t.Fatalf("re-delete = %d,%v want 0,nil", n, err)
	}
}

func TestDeleteOutageNeverReachesAnEarlierOutage(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	// Two back-to-back outages: the second starts 50s after the first ends.
	evAt(t, st, base+1000, "down", -1)
	evAt(t, st, base+1100, "up", 100)
	evAt(t, st, base+1150, "down", -1)
	evAt(t, st, base+1200, "up", 50)

	if n, _ := st.DeleteOutage(ctx, base+1200); n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}
	// The first pair must be intact - the delete matched down@+1150, not down@+1000.
	ev, _ := st.EventsPage(ctx, 10, 0)
	if len(ev) != 2 || ev[0].TS != base+1100 || ev[1].TS != base+1000 {
		t.Fatalf("first outage disturbed: %+v", ev)
	}
}

func TestDeleteOutageRejectsNonOutageRows(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	evAt(t, st, base+1000, "down", -1) // an open outage has no closing up yet

	if n, err := st.DeleteOutage(ctx, base+1000); err != nil || n != 0 {
		t.Fatalf("down ts: deleted = %d,%v want 0,nil", n, err)
	}
	if c, _ := st.EventCount(ctx); c != 1 {
		t.Fatalf("events left = %d, want 1 (nothing deleted)", c)
	}
}

// An 'up' with a NULL duration is a FINISHED outage whose length was never
// measured (or was stripped by a repair) - not a live one - so it is
// deletable, and its 'down' markers are swept with it like any other
// outage's. It used to be refused, which left the repair-nulled residue row
// permanently undeletable (see repaired_outage_delete_test.go).
func TestDeleteOutageAcceptsANullDurationUp(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	evAt(t, st, base+1000, "down", -1)
	evAt(t, st, base+2000, "up", -1) // recovery recorded without a measured length

	if n, err := st.DeleteOutage(ctx, base+2000); err != nil || n != 2 {
		t.Fatalf("null-duration up: deleted = %d,%v want 2,nil (the up and its down)", n, err)
	}
	if c, _ := st.EventCount(ctx); c != 0 {
		t.Fatalf("events left = %d, want 0", c)
	}
}

func TestDeleteOutageUpdatesResolvedCounts(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now().Unix()
	evAt(t, st, now-3600, "down", -1)
	evAt(t, st, now-3300, "up", 300)

	if c, d, _ := st.ResolvedOutagesSince(ctx, now-7200); c != 1 || d != 300 {
		t.Fatalf("before delete: count=%d downtime=%d, want 1/300", c, d)
	}
	if n, _ := st.DeleteOutage(ctx, now-3300); n != 2 {
		t.Fatalf("deleted = %d, want 2", n)
	}
	if c, d, _ := st.ResolvedOutagesSince(ctx, now-7200); c != 0 || d != 0 {
		t.Fatalf("after delete: count=%d downtime=%d, want 0/0", c, d)
	}
}

func TestDeleteOutageSpanningAMonitoringPause(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	// The monitor excludes paused time from duration_s, so the recorded length
	// (300s) is far shorter than the wall gap (1000s). The down must still go.
	evAt(t, st, base+1000, "down", -1)
	evAt(t, st, base+2000, "up", 300)

	n, err := st.DeleteOutage(ctx, base+2000)
	if err != nil || n != 2 {
		t.Fatalf("deleted = %d,%v want 2,nil (pause-shortened duration must not strand the down)", n, err)
	}
	if c, _ := st.EventCount(ctx); c != 0 {
		t.Fatalf("events left = %d, want 0", c)
	}
}

func TestDeleteOutageSplitByMonitorRestart(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	base := int64(plausibleEpoch) + 1000
	// A restart mid-outage re-detects the same outage and writes a second
	// 'down'; the closing up's duration covers only the re-detection. Deleting
	// the outage must sweep BOTH downs or the survivor keeps the downtime alive.
	evAt(t, st, base+1000, "down", -1)
	evAt(t, st, base+1500, "down", -1)
	evAt(t, st, base+2000, "up", 500)

	n, err := st.DeleteOutage(ctx, base+2000)
	if err != nil || n != 3 {
		t.Fatalf("deleted = %d,%v want 3 (the up and both downs)", n, err)
	}
	if c, _ := st.EventCount(ctx); c != 0 {
		t.Fatalf("events left = %d, want 0", c)
	}
}

// A dangling 'down' whose outage RECOVERED while the monitor was off is a
// distinct outage (UptimeSince bounds it at the quorum recovery); deleting a
// later outage must not sweep it away.
func TestDeleteOutageKeepsDistinctRecoveredOutage(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	now := time.Now()
	eventAt(t, st, now, 3600, "down", -1) // outage 1 opens; never closed
	// quorum recovery (2 of 3 targets) while the monitor was off
	sampleAt(t, st, now, 3000, "a", "ipv4", true)
	sampleAt(t, st, now, 3000, "b", "ipv4", true)
	sampleAt(t, st, now, 3000, "c", "ipv4", false)
	eventAt(t, st, now, 1800, "down", -1) // outage 2
	eventAt(t, st, now, 1500, "up", 300)

	n, err := st.DeleteOutage(ctx, now.Add(-1500*time.Second).Unix())
	if err != nil || n != 2 {
		t.Fatalf("deleted = %d,%v want 2 (outage 2 only)", n, err)
	}
	ev, _ := st.EventsPage(ctx, 10, 0)
	if len(ev) != 1 || ev[0].Type != "down" || ev[0].TS != now.Add(-3600*time.Second).Unix() {
		t.Fatalf("outage 1's dangling down must survive, got %+v", ev)
	}
}
