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

// Order is judged on the raw parameters. Clamping the start down to the ceiling
// first let a backwards pair that sat wholly past it compare as valid again.
func TestParseRangeParamsRejectsReversedFutureRange(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ceil := now.Add(maxWinMins * time.Minute)
	req := func(from, to time.Time) *http.Request {
		return httptest.NewRequest("GET", fmt.Sprintf("/api/speed?from=%d&to=%d", from.Unix(), to.Unix()), nil)
	}
	if _, _, ok := parseRangeParams(req(now.Add(400*24*time.Hour), now.Add(380*24*time.Hour)), now); ok {
		t.Error("a backwards far-future pair was accepted, want the fallback to ?mins=")
	}
	// Equal ends are refused wherever they sit, the far future included.
	same := now.Add(400 * 24 * time.Hour)
	if _, _, ok := parseRangeParams(req(same, same), now); ok {
		t.Error("a zero-width far-future pair was accepted, want the fallback to ?mins=")
	}
	// The ordered pairs past the ceiling still clamp rather than falling back.
	since, until, ok := parseRangeParams(req(now.Add(380*24*time.Hour), now.Add(400*24*time.Hour)), now)
	if !ok || !since.Equal(ceil) || !until.Equal(ceil) {
		t.Errorf("ordered far-future pair -> (%v, %v, ok=%v), want both clamped to the ceiling %v",
			since, until, ok, ceil)
	}
	from := now.Add(-time.Hour)
	since, until, ok = parseRangeParams(req(from, now.Add(400*24*time.Hour)), now)
	if !ok || !since.Equal(from) || !until.Equal(ceil) {
		t.Errorf("half-future range -> (%v, %v, ok=%v), want (%v, %v, true)", since, until, ok, from, ceil)
	}
}

// The operator-visible half: a backwards window must draw the ?mins= data, not
// a blank chart under a label from 2027.
func TestSpeedHistoryReversedFutureRangeFallsBackToMins(t *testing.T) {
	s := newTestServer(t)
	now := time.Now()
	if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{
		TS: now.Add(-time.Hour).Unix(), DownMbps: 100, UpMbps: 10}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	q := fmt.Sprintf("mins=1440&from=%d&to=%d", now.Add(400*24*time.Hour).Unix(), now.Add(380*24*time.Hour).Unix())
	w := do(t, s.Handler(), "GET", "/api/speed?"+q, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	var out []store.SpeedSample
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("?%s -> %d samples, want the 1 from the mins window", q, len(out))
	}
}
