package web

import (
	"net/http"
	"testing"
)

// C-06: a backup cut off partway (config/data applied, later data silently
// missing) or one with trailing junk must be a 400, not a false "Imported". The
// category loop stops at dec.More()==false, which is also what EOF looks like, so
// the handler now requires the closing '}' and then EOF.
func TestImportRejectsTruncatedAndTrailingJunk(t *testing.T) {
	s := newTestServer(t)

	// Complete latency array but the top-level object is cut off (no closing '}').
	truncated := `{"pingularity_export":1,"latency":[{"ts":1000,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]`
	if w := do(t, s.Handler(), "POST", "/api/import?latency=1", truncated); w.Code != http.StatusBadRequest {
		t.Fatalf("truncated import: got %d, want 400: %s", w.Code, w.Body)
	}

	// A valid object followed by trailing junk.
	trailing := `{"pingularity_export":1,"latency":[]} garbage`
	if w := do(t, s.Handler(), "POST", "/api/import?latency=1", trailing); w.Code != http.StatusBadRequest {
		t.Fatalf("trailing-junk import: got %d, want 400: %s", w.Code, w.Body)
	}

	// A well-formed export still imports (guard against over-rejection).
	ok := `{"pingularity_export":1,"latency":[{"ts":1000,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]}`
	if w := do(t, s.Handler(), "POST", "/api/import?latency=1", ok); w.Code != http.StatusOK {
		t.Fatalf("well-formed import: got %d, want 200: %s", w.Code, w.Body)
	}
}
