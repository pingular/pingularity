package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The absolute chart window is half-open [from, to): a run stamped exactly on
// the upper bound belongs to the NEXT range, not this one. Speedtests land on a
// schedule, so a run at local midnight is ordinary and a closed bound would
// show it in both neighbouring day ranges.
func TestSpeedHistoryRangeHalfOpen(t *testing.T) {
	s := newTestServer(t)
	base := time.Now().Add(-72 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 4; i++ {
		sp := store.SpeedSample{TS: base.Add(time.Duration(i) * time.Hour).Unix(), DownMbps: float64(100 + i), UpMbps: 10}
		if err := s.store.InsertSpeed(t.Context(), sp); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	get := func(q string) []store.SpeedSample {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/speed?"+q, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET ?%s -> %d", q, w.Code)
		}
		var out []store.SpeedSample
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	from, to := base.Unix(), base.Add(2*time.Hour).Unix()
	got := get(fmt.Sprintf("from=%d&to=%d", from, to))
	if len(got) != 2 {
		t.Fatalf("half-open [t0,t0+2h) = %d samples, want 2 (the run AT the upper bound must be excluded)", len(got))
	}
	if got[0].TS != from {
		t.Errorf("lower bound is inclusive: first ts %d, want %d", got[0].TS, from)
	}
	// An open upper bound keeps today's since-only behaviour.
	if all := get(fmt.Sprintf("from=%d", from)); len(all) != 4 {
		t.Errorf("open end = %d samples, want all 4", len(all))
	}
}

// ?mins= is shipped public API and must keep working untouched, and a bad or
// reversed range must fall back to it rather than erroring or emptying.
func TestSpeedHistoryRangeFallsBackToMins(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{TS: now.Add(-time.Hour).Unix(), DownMbps: 100, UpMbps: 10}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	for _, q := range []string{"mins=1440", "from=abc&mins=1440", "from=0&mins=1440", "mins=1440&from=200&to=100"} {
		w := do(t, s.Handler(), "GET", "/api/speed?"+q, "")
		var out []store.SpeedSample
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("?%s decode: %v", q, err)
		}
		if len(out) != 1 {
			t.Errorf("?%s -> %d samples, want the 1 from the mins window", q, len(out))
		}
	}
}

// An absolute window must never reach further back than ?mins= already allows.
func TestParseRangeParamsClampsToTheMinsCeiling(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	floor := now.Add(-maxWinMins * time.Minute)
	r := httptest.NewRequest("GET", "/api/speed?from=1", nil)
	since, until, ok := parseRangeParams(r, now)
	if !ok {
		t.Fatal("a far-past from should clamp, not reject")
	}
	if !since.Equal(floor) {
		t.Errorf("since %v, want it raised to the floor %v", since, floor)
	}
	if !until.IsZero() {
		t.Errorf("until %v, want zero (open) when ?to is absent", until)
	}
	// Beyond the future ceiling clamps rather than refusing: refusing would fall
	// back to the default window and draw the last 7 days under a 2030 label.
	far := fmt.Sprintf("/api/speed?from=%d", now.Add(400*24*time.Hour).Unix())
	fs, _, ok := parseRangeParams(httptest.NewRequest("GET", far, nil), now)
	if !ok || !fs.Equal(now.Add(maxWinMins*time.Minute)) {
		t.Errorf("far-future from -> (%v, ok=%v), want it clamped to the ceiling", fs, ok)
	}
}

// An open-start span ("until 2020") must select nothing rather than falling
// back to the default window. The client sends from=1 so the floor below can
// raise it without ever overtaking the end and reading as a reversed pair.
func TestSpeedHistoryRangeOpenStartOlderThanTheCap(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{
		TS: time.Now().Add(-time.Hour).Unix(), DownMbps: 100, UpMbps: 10}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	to := time.Date(2020, 1, 5, 0, 0, 0, 0, time.UTC).Unix()
	w := do(t, s.Handler(), "GET", fmt.Sprintf("/api/speed?from=1&to=%d", to), "")
	var out []store.SpeedSample
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d samples for an open start ending in 2020, want 0", len(out))
	}
}

// A span entirely older than retention is a legitimate request for pruned
// history: it must answer with no rows, NOT fall back to the default window.
// Clamping the start up to the floor makes it overtake the end, and reading
// that as reversed input is how asking for January 2020 silently drew this week.
func TestSpeedHistoryRangeBelowFloorIsEmptyNotFallback(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{
		TS: time.Now().Add(-time.Hour).Unix(), DownMbps: 100, UpMbps: 10}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	to := time.Date(2020, 1, 5, 0, 0, 0, 0, time.UTC).Unix()
	w := do(t, s.Handler(), "GET", fmt.Sprintf("/api/speed?from=%d&to=%d", from, to), "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	var out []store.SpeedSample
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d samples for a 2020 span, want 0 (it must not fall back to the mins window)", len(out))
	}
}
