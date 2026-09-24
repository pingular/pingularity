package web

import (
	"net/http"
	"testing"
)

// The two probes are the routes an outside checker reads through whatever
// sits in front of the daemon. A front told to cache everything had no
// instruction from the origin against storing "ok", so an uptime check could
// stay green after the process died. They carry no-store now, like /api and
// /metrics already do; the page keeps its validator (no-cache + ETag) and is
// untouched.
// Through the real chain (Server.Handler), so the header is what a peer sees.
func TestTheProbesAreNeverCached(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, p := range []string{"/healthz", "/readyz"} {
		w := do(t, h, "GET", p, "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d, want 200", p, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store", p, cc)
		}
	}
	if w := do(t, h, "GET", "/", ""); w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("GET /: Cache-Control = %q, want the page's no-cache validator untouched", w.Header().Get("Cache-Control"))
	}
}
