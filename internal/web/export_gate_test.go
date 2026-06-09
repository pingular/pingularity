package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A saturated export gate must refuse a new export with 429 rather than open
// another SQLite read cursor and risk pinning the pool. Regression for B8.
func TestExportGateRefusesWhenFull(t *testing.T) {
	s := newTestServer(t)
	gate := s.exportGate()
	for i := 0; i < cap(gate); i++ { // fill every slot
		gate <- struct{}{}
	}

	call := func(path string, h http.HandlerFunc) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "127.0.0.1:9000"
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	if code := call("/api/speed/runs.csv", s.handleSpeedRunsCSV); code != http.StatusTooManyRequests {
		t.Fatalf("CSV export with a full gate: got %d, want 429", code)
	}
	if code := call("/api/export?speed=1", s.handleExport); code != http.StatusTooManyRequests {
		t.Fatalf("data export with a full gate: got %d, want 429", code)
	}

	// Freeing a slot lets an export proceed again.
	<-gate
	if code := call("/api/speed/runs.csv", s.handleSpeedRunsCSV); code != http.StatusOK {
		t.Fatalf("CSV export after freeing the gate: got %d, want 200", code)
	}
}
