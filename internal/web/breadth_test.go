package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// Bad/missing/negative query params on the read endpoints must default
// gracefully (200), never 500 - the classic param-parsing footgun.
func TestListEndpointParamsDontError(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, p := range []string{
		"/api/events?limit=-1&offset=-5",
		"/api/events?limit=abc",
		"/api/speed/runs?limit=99999999",
		"/api/heatmap?days=abc",
		"/api/heatmap?days=-3",
		"/api/series?mins=-5",
		"/api/series?mins=notanumber",
		"/api/series?exclude=a,b,c,d,e,f,g,h,i,j,k,l,m,n,o", // >12 must truncate, not error
		"/api/speed?mins=999999999",
		"/api/speed/usage?dataMins=-1",
		// the absolute chart window: garbage must fall back to the ?mins= window
		"/api/speed?from=abc",
		"/api/speed?from=-5",
		"/api/speed?from=0",
		"/api/speed?from=1700000000&to=notanumber",
		"/api/speed?from=1700000000&to=1600000000", // reversed
		"/api/speed?from=1700000000&to=1700000000", // zero width
		"/api/series?from=abc",
		"/api/series?from=-5",
		"/api/series?from=1700000000&to=1600000000", // reversed
	} {
		if w := do(t, h, "GET", p, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s -> %d (want 200; bad params must default, not 500)", p, w.Code)
		}
	}
}

// The dashboard page is served with a cache validator and compression: a
// matching If-None-Match gets a 304, a gzip-accepting client gets a compressed
// body that inflates back to the original, and other paths stay 404.
func TestServeUICachingAndCompression(t *testing.T) {
	h := newTestServer(t).Handler()

	w := do(t, h, "GET", "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET / -> %d", w.Code)
	}
	etag := w.Header().Get("ETag")
	if etag == "" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("missing cache validator headers: etag=%q cc=%q", etag, w.Header().Get("Cache-Control"))
	}
	plain := w.Body.Bytes()
	if !strings.Contains(string(plain), "<title>") {
		t.Error("page body doesn't look like the dashboard")
	}

	// Revalidation: same ETag -> 304, no body.
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("If-None-Match revalidation: code=%d len=%d, want 304 empty", rec.Code, rec.Body.Len())
	}

	// Compression: gzip body inflates to the exact same page.
	r = httptest.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Accept-Encoding", "gzip")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("no gzip for an accepting client (Content-Encoding=%q)", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.Len() >= len(plain) {
		t.Errorf("gzip body (%d) not smaller than plain (%d)", rec.Body.Len(), len(plain))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	inflated, err := io.ReadAll(zr)
	if err != nil || string(inflated) != string(plain) {
		t.Errorf("gzip body doesn't inflate to the original page (err=%v, %d vs %d bytes)", err, len(inflated), len(plain))
	}

	if w := do(t, h, "GET", "/nope.html", ""); w.Code != http.StatusNotFound {
		t.Errorf("GET /nope.html -> %d, want 404", w.Code)
	}
}

// Every response - page, API, and guard rejections alike - carries the
// defense-in-depth headers: nosniff and a CSP that permits no external origin.
func TestSecurityHeaders(t *testing.T) {
	h := newTestServer(t).Handler()
	for _, p := range []string{"/", "/api/status", "/metrics"} {
		w := do(t, h, "GET", p, "")
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", p, got)
		}
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
			t.Errorf("%s: unexpected CSP %q", p, csp)
		}
		// frame-ancestors 'none' refuses framing (clickjacking guard); it has no
		// fallback to default-src, so it must be present explicitly.
		if !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP missing frame-ancestors 'none': %q", p, csp)
		}
	}
	// The middleware wraps the whole mux, so a guard REJECTION carries the
	// headers too (a clickjacking frame of a 405/403 page is still a frame).
	w := do(t, h, "GET", "/api/iperf/check?addr=127.0.0.1:1", "") // GET on a POST-only endpoint -> 405
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("setup: want 405 on GET iperf/check, got %d", w.Code)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("405 rejection missing frame-ancestors CSP: %q", csp)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("405 rejection X-Content-Type-Options = %q, want nosniff", got)
	}
}

// /api/iperf/check dials an arbitrary host:port, so like the other
// side-effectful endpoints it must refuse GET and the no-cors POST a
// cross-site page could send.
func TestIperfCheckMethodAndCSRFGuard(t *testing.T) {
	h := newTestServer(t).Handler()
	if w := do(t, h, "GET", "/api/iperf/check?addr=127.0.0.1:1", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET iperf/check: %d, want 405", w.Code)
	}
	// POST without the JSON content-type (do only sets it with a body) -> 415.
	if w := do(t, h, "POST", "/api/iperf/check?addr=127.0.0.1:1", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST iperf/check without content-type: %d, want 415", w.Code)
	}
}

func TestHashPassword(t *testing.T) {
	h, err := hashPassword("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("hunter2")) != nil {
		t.Error("hash must verify the correct password")
	}
	if bcrypt.CompareHashAndPassword([]byte(h), []byte("wrong")) == nil {
		t.Error("hash must reject the wrong password")
	}
}

// Logout (POST + JSON content-type, see TestLogoutMethodAndCSRFGuard) clears
// the session cookie, and the clearing cookie's Secure flag follows the same
// per-request decision as login: on for the public -allow-host domain, off for
// IP-literal Hosts.
func TestHandleLogout(t *testing.T) {
	s := newTestServer(t)
	sessCookie := func(w *http.Response) *http.Cookie {
		for _, c := range w.Cookies() {
			if c.Name == sessionCookie {
				return c
			}
		}
		return nil
	}
	w := do(t, s.Handler(), "POST", "/api/auth/logout", "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("logout %d", w.Code)
	}
	c := sessCookie(w.Result())
	if c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout must clear the session cookie (MaxAge<0), got %+v", c)
	}
	if c.Secure {
		t.Error("IP-literal Host -> logout cookie should not be Secure")
	}
	// Via the public reverse-proxy domain the clearing cookie is Secure.
	s.AllowedHosts = []string{"ping.example.com"}
	r := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "ping.example.com"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if c := sessCookie(rec.Result()); c == nil || !c.Secure {
		t.Error("public -allow-host domain -> logout cookie should be Secure")
	}
}

// A self-hosted dashboard on a public address must never be indexable: it is
// one operator's ISP, exit city, speed history and outage log, and every
// install renders identical titles, so indexing also scatters duplicate
// branded pages. Asserted on the UI, the API and /metrics together, because a
// meta tag would only have covered the first.
func TestDashboardIsNotIndexable(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	for _, path := range []string{"/", "/api/status", "/metrics"} {
		w := do(t, h, "GET", path, "")
		if got := w.Header().Get("X-Robots-Tag"); got != "noindex, nofollow" {
			t.Errorf("%s: X-Robots-Tag = %q, want \"noindex, nofollow\" - a publicly reachable "+
				"install would otherwise be crawlable", path, got)
		}
	}
}
