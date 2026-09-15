package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The dashboard refuses to save a schedule that is on with no active day, so
// POST /api/settings and a config restore are the two doors that reach the
// daemon without that refusal in front of them. Such a schedule can never be
// on: it parks the feature it gates, and the response has to say back what was
// sent rather than a quietly different schedule the caller never asked for.

func TestSettingsPostNoDayWindowParksProbing(t *testing.T) {
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
	if !got.LatOn || len(got.LatWins) != 1 || !got.SpdOn || len(got.SpdWins) != 1 {
		t.Errorf("echoed lat=%v/%d windows, speed=%v/%d windows; want both on with the one window each was sent",
			got.LatOn, len(got.LatWins), got.SpdOn, len(got.SpdWins))
	}
	now := time.Now()
	if s.settings.LatencyAllowed(now) || s.settings.SpeedAllowed(now) {
		t.Fatalf("latency=%v speed=%v after the save - a schedule with no active day opened both gates",
			s.settings.LatencyAllowed(now), s.settings.SpeedAllowed(now))
	}
}

func TestImportConfigNoDayWindowParksProbing(t *testing.T) {
	s := newTestServer(t)
	payload := `{"pingularity_export":1,"config":[` +
		`{"key":"sched_lat_enabled","value":"1"},` +
		`{"key":"sched_lat_windows","value":"[{\"days\":\"0000000\",\"start\":0,\"end\":0}]"}]}`
	rr := do(t, s.Handler(), "POST", "/api/import?config=1", payload)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: got %d: %s", rr.Code, rr.Body)
	}
	if v := s.settings.Snapshot(); !v.SchedLatEnabled || len(v.SchedLatWindows) != 1 {
		t.Errorf("after the restore: enabled=%v windows=%+v, want on with the restored window", v.SchedLatEnabled, v.SchedLatWindows)
	}
	if s.settings.LatencyAllowed(time.Now()) {
		t.Fatal("latency probing resumed under a restored schedule that selects no day")
	}
	var got struct {
		LatOn bool `json:"sched_lat_enabled"`
	}
	if err := json.Unmarshal(do(t, s.Handler(), "GET", "/api/settings", "").Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.LatOn {
		t.Error("GET /api/settings reports the latency schedule off while the stored row says it is on")
	}
}

// docs/api.md is where a config script's author learns what a window with no
// weekday does, and the sentence it carried before said the opposite of what
// this handler does: saved as off, and the daemon keeps probing. Hold both
// halves of the entry to the handler, so the reference cannot drift back into
// promising measurements a parked install will never take.
func TestNoDayScheduleIsDocumentedAsWhatTheHandlerDoes(t *testing.T) {
	s := newTestServer(t)
	rr := do(t, s.Handler(), "POST", "/api/settings", `{"sched_lat_enabled":true,"sched_lat_windows":[{"days":"0000000","start":0,"end":0}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/settings: got %d: %s", rr.Code, rr.Body)
	}
	var got struct {
		LatOn   bool              `json:"sched_lat_enabled"`
		LatWins []json.RawMessage `json:"sched_lat_windows"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	entry := apiSettingsEntry(t)
	flat := strings.Join(strings.Fields(entry), " ")
	const savedAsSent = "a window that selects no weekday is saved as sent"
	const parks = "parks its feature - latency probing stops, no automatic speedtest fires"
	if kept := got.LatOn && len(got.LatWins) == 1; kept != strings.Contains(flat, savedAsSent) {
		t.Errorf("the handler keeps a no-day window as sent = %v, and docs/api.md's entry says %q = %v:\n%s", kept, savedAsSent, !kept, entry)
	}
	if parked := !s.settings.LatencyAllowed(time.Now()); parked != strings.Contains(flat, parks) {
		t.Errorf("a schedule left with only a no-day window parks probing = %v, and docs/api.md's entry says it %q = %v:\n%s", parked, parks, !parked, entry)
	}
}
