package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// Deleting ONE speed run must invalidate the cached status aggregates, the same
// way the outage delete, the bulk delete and the import do. The data pills and
// the /metrics byte series are computed from the speed table and cached for
// aggTTL (30s); the UI refreshes status the instant the row disappears, so
// without the invalidation the totals and per-run averages keep counting a run
// the operator just deleted for another half minute.
func TestSpeedRunDeleteInvalidatesAggregates(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := statusServer(t, st)

	now := time.Now().Unix()
	const keptTS, goneTS = 120, 60 // seconds ago
	for _, r := range []struct{ ago, down int64 }{{keptTS, 1000}, {goneTS, 3000}} {
		b := r.down
		if err := st.InsertSpeed(ctx, store.SpeedSample{
			TS: now - r.ago, DownMbps: 100, UpMbps: 20, PingMS: 10, DownBytes: &b,
		}); err != nil {
			t.Fatalf("seed run %ds ago: %v", r.ago, err)
		}
	}

	// Warm the cache with both runs in it.
	warm := getStatus(t, s, "")
	if got := warm["speed_avg_down_bytes"]; got != float64(2000) {
		t.Fatalf("warm speed_avg_down_bytes = %v, want 2000", got)
	}
	if got := warm["data_used_bytes"]; got != float64(4000) {
		t.Fatalf("warm data_used_bytes = %v, want 4000", got)
	}

	w := do(t, s.Handler(), "POST", "/api/speed/runs/delete", `{"ts":`+strconv.FormatInt(now-goneTS, 10)+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete: code=%d body=%q", w.Code, w.Body.String())
	}

	// Immediately after - well inside aggTTL - both surfaces must have dropped
	// the deleted run's 3000 bytes.
	after := getStatus(t, s, "")
	if got := after["speed_avg_down_bytes"]; got != float64(1000) {
		t.Errorf("speed_avg_down_bytes = %v, want 1000 (the cache still averages the deleted run)", got)
	}
	if got := after["data_used_bytes"]; got != float64(1000) {
		t.Errorf("data_used_bytes = %v, want 1000 (the cache still counts the deleted run's bytes)", got)
	}
	body := scrape(t, s)
	if !strings.Contains(body, `pingularity_speed_avg_run_bytes{direction="down"} 1000`) {
		t.Errorf("metrics still publish the pre-delete average:\n%s", body)
	}
	if !strings.Contains(body, "pingularity_speed_data_used_bytes 1000\n") {
		t.Errorf("metrics still publish the deleted run's bytes:\n%s", body)
	}
}
