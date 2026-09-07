package web

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The dashboard refuses to save a schedule that is on with no active day, so
// POST /api/settings and a config restore are the two doors that reach the
// daemon without that refusal in front of them. A schedule that could never be
// on must leave the daemon probing, read back as off, and say so in the
// response the caller gets.

func TestSettingsPostNoDayWindowKeepsProbing(t *testing.T) {
	s := newTestServer(t)
	body := `{"sched_lat_enabled":true,"sched_lat_windows":[{"days":"0000000","start":0,"end":0}],` +
		`"sched_speed_enabled":true,"sched_speed_windows":[{"days":"0000000","start":540,"end":1020}]}`
	rr := do(t, s.Handler(), "POST", "/api/settings", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: got %d: %s", rr.Code, rr.Body)
	}
	var got struct {
		LatOn   bool              `json:"sched_lat_enabled"`
		LatWins []json.RawMessage `json:"sched_lat_windows"`
		SpdOn   bool              `json:"sched_speed_enabled"`
		SpdWins []json.RawMessage `json:"sched_speed_windows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LatOn || len(got.LatWins) != 0 || got.SpdOn || len(got.SpdWins) != 0 {
		t.Errorf("echoed lat=%v/%d windows, speed=%v/%d windows; want both off with none", got.LatOn, len(got.LatWins), got.SpdOn, len(got.SpdWins))
	}
	now := time.Now()
	if !s.settings.LatencyAllowed(now) || !s.settings.SpeedAllowed(now) {
		t.Fatalf("latency=%v speed=%v after the save - the daemon accepted a schedule that shuts both gates for good",
			s.settings.LatencyAllowed(now), s.settings.SpeedAllowed(now))
	}
}

func TestImportConfigNoDayWindowKeepsProbing(t *testing.T) {
	s := newTestServer(t)
	payload := `{"pingularity_export":1,"config":[` +
		`{"key":"sched_lat_enabled","value":"1"},` +
		`{"key":"sched_lat_windows","value":"[{\"days\":\"0000000\",\"start\":0,\"end\":0}]"}]}`
	rr := do(t, s.Handler(), "POST", "/api/import?config=1", payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: got %d: %s", rr.Code, rr.Body)
	}
	if v := s.settings.Snapshot(); v.SchedLatEnabled || len(v.SchedLatWindows) != 0 {
		t.Errorf("after the restore: enabled=%v windows=%+v, want off with no windows", v.SchedLatEnabled, v.SchedLatWindows)
	}
	if !s.settings.LatencyAllowed(time.Now()) {
		t.Fatal("latency probing gated off by a restored schedule that selects no day")
	}
	var got struct {
		LatOn bool `json:"sched_lat_enabled"`
	}
	if err := json.Unmarshal(do(t, s.Handler(), "GET", "/api/settings", "").Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.LatOn {
		t.Error("GET /api/settings still reports the latency schedule on")
	}
}
