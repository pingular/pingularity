package web

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A ~1MiB (rejected) Host must not flow verbatim into the logs and thence the
// in-memory ring behind the viewer: the DNS-rebinding rejection warning truncates
// it via capForLog. Regression for the oversized-Host memory exhaustion (B1).
func TestOversizedHostTruncatedInLog(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServerLog(t, &buf)
	h := s.Handler()

	// A valid-looking public FQDN of ~1MiB: it fails the rebinding guard (not an
	// IP/localhost/reserved suffix), so it takes the warn-and-reject path.
	bigHost := strings.Repeat("a", 1<<20) + ".example.com"
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Host = bigHost
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("oversized public Host should be rejected by the rebinding guard, got %d", w.Code)
	}
	if buf.Len() > 4096 {
		t.Fatalf("log retained %d bytes for a ~1MiB Host; the value must be truncated before logging", buf.Len())
	}
	if strings.Contains(buf.String(), strings.Repeat("a", logCap+1)) {
		t.Fatal("the full oversized Host leaked into the log line")
	}
}

// A burst of rejected Hosts must not emit a warning per request: the warning is
// coalesced to at most one per rejectWarnEvery. Regression for the log-ring flood
// half of B1.
func TestHostRejectionWarningCoalesced(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServerLog(t, &buf)
	h := s.Handler()

	for i := 0; i < 50; i++ {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = "attacker-controlled.example.com"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if n := strings.Count(buf.String(), "DNS-rebinding guard"); n != 1 {
		t.Fatalf("expected the rejection warning coalesced to 1 line, got %d", n)
	}
}
