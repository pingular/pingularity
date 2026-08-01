package store

import (
	"context"
	"testing"
	"time"
)

// A BACKUP FILE IS UNTRUSTED INPUT, AND HALF THE DOOR WAS UNGUARDED.
//
// The import door types INTEGER columns (the intCols allowlist) precisely because
// a wrong type there is a landmine: "a non-integral float in one of these ... then
// breaks every read that scans the column into a Go int64". REAL columns - the
// ones every graph actually plots - had no equivalent contract, so a crafted or
// corrupt export could deposit TEXT into latency_ms, ping_ms, jitter, packet loss
// and the bufferbloat fields.
//
// The consequence is worse than a bad point on a chart. A Go scan of TEXT into
// float64 errors, so ONE poisoned row takes out the whole window's read - and
// where SQLite coerces instead (inside AVG) a non-numeric string counts as 0,
// which is not a visible failure at all but a fake perfect measurement.
func TestImportRejectsTextInRealColumns(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ts := time.Now().Add(-time.Minute).Unix()

	poison := []struct {
		table string
		row   map[string]any
	}{
		{"samples", map[string]any{"ts": ts, "target": "a", "family": "ipv4", "latency_ms": "not-a-number", "success": 1}},
		{"dns", map[string]any{"ts": ts, "latency_ms": "NaN-ish", "success": 1}},
		{"speed", map[string]any{"ts": ts, "server": "s", "down_mbps": "fast", "up_mbps": 10.0, "ping_ms": 5.0}},
		{"speed", map[string]any{"ts": ts + 1, "server": "s", "down_mbps": 10.0, "up_mbps": 10.0, "ping_ms": "quick"}},
	}
	for _, p := range poison {
		n, err := st.ImportTable(ctx, p.table, []map[string]any{p.row})
		if err == nil && n > 0 {
			t.Errorf("%s: a non-numeric string was accepted into a REAL column (%v)", p.table, p.row)
		}
	}

	// Nothing may have landed, and every graph read must still work.
	counts, err := st.TableCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"samples", "dns", "speed"} {
		if counts[tbl] != 0 {
			t.Errorf("%s holds %d poisoned row(s)", tbl, counts[tbl])
		}
	}
	if _, err := st.Series(ctx, time.Now().Add(-time.Hour), time.Time{}, 60, nil); err != nil {
		t.Errorf("the latency series read failed after a rejected import: %v", err)
	}
	if _, err := st.SpeedHistory(ctx, time.Unix(1, 0)); err != nil {
		t.Errorf("the speed history read failed after a rejected import: %v", err)
	}
}

// The contract must not turn into "REAL columns reject everything". Real
// exports carry JSON numbers, integers for whole values, and nulls for the
// genuinely unmeasured - all of which must still restore.
func TestImportStillAcceptsRealMeasurements(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	ts := time.Now().Add(-time.Minute).Unix()

	ok := []struct {
		table string
		row   map[string]any
	}{
		{"samples", map[string]any{"ts": ts, "target": "a", "family": "ipv4", "latency_ms": 20.5, "success": 1}},
		{"samples", map[string]any{"ts": ts + 1, "target": "a", "family": "ipv4", "latency_ms": 20, "success": 1}},
		{"samples", map[string]any{"ts": ts + 2, "target": "a", "family": "ipv4", "latency_ms": nil, "success": 0}},
		{"dns", map[string]any{"ts": ts, "latency_ms": 15.25, "success": 1}},
		{"speed", map[string]any{"ts": ts, "server": "s", "down_mbps": 45.5, "up_mbps": 48, "ping_ms": 5.4,
			"jitter_ms": nil, "packet_loss": 0.0, "ping_best_ms": 4.6}},
	}
	for _, r := range ok {
		n, err := st.ImportTable(ctx, r.table, []map[string]any{r.row})
		if err != nil || n != 1 {
			t.Errorf("%s: a legitimate measurement was rejected (n=%d err=%v): %v", r.table, n, err, r.row)
		}
	}
}

// A SINGLE ROW MUST NOT BE ABLE TO BREAK THE USAGE TOTAL.
//
// Data usage sums download+upload across every retained run into a signed int64.
// The per-run cap was 1 PiB per direction, justified in the source as "orders of
// magnitude above any real run yet far below where a SUM over the whole speed
// table could overflow int64" - which was arithmetically false. At 1 PiB per
// direction the margin is 4,095 rows, and a default install keeps 8,760 hourly
// runs for its 365-day speed retention. So a crafted backup could make
// /api/speed/usage error out and stay broken until the rows were found.
func TestSpeedByteCapCannotOverflowTheUsageSum(t *testing.T) {
	if maxSpeedBytesPerRun <= 0 {
		t.Fatal("cap must be positive")
	}
	// Every row a full speed retention can hold, both directions at the cap.
	const maxRows = 366 * 24 * 60 // a leap year of once-a-minute runs
	perRow := 2 * int64(maxSpeedBytesPerRun)
	if perRow/2 != int64(maxSpeedBytesPerRun) {
		t.Fatal("per-row total overflowed on its own")
	}
	if got := int64(maxRows); perRow > (1<<63-1)/got {
		t.Errorf("a full history at the cap overflows int64: %d rows x %d bytes/row exceeds maxint64; "+
			"the cap (%d) leaves only %d rows of headroom",
			maxRows, perRow, maxSpeedBytesPerRun, (1<<63-1)/perRow)
	}
}
