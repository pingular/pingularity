package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const heartbeatTestPath = "/api/notify/heartbeat/test"

// Like the webhook test, this dials a URL from the request body, so the pure
// guards (method, empty URL, bad JSON) must reject before any network use.
func TestHandleNotifyHeartbeatTestGuards(t *testing.T) {
	h := newTestServer(t).Handler()
	if w := do(t, h, "GET", heartbeatTestPath, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "POST", heartbeatTestPath, `{"url":"   "}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty URL want 400, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no heartbeat URL set") {
		t.Errorf("empty URL body = %q, want it to name the missing heartbeat URL", w.Body.String())
	}
	if w := do(t, h, "POST", heartbeatTestPath, `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
	// The content-type gate is the CSRF guard, and this endpoint reaches the
	// network: a cross-site page can post text/plain with no preflight.
	r := httptest.NewRequest("POST", heartbeatTestPath, strings.NewReader(`{"url":"http://127.0.0.1:1/ping"}`))
	r.Header.Set("Content-Type", "text/plain")
	r.Host = "127.0.0.1:9000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content-type want 415, got %d: %s", rec.Code, rec.Body)
	}
}

// Pressing Test is a real check-in, not a simulation: the watchdog must receive
// the request, and the UI needs the {"sent":true} its success path renders.
func TestHandleNotifyHeartbeatTestChecksIn(t *testing.T) {
	got := make(chan string, 4)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Method + " " + r.URL.Path
	}))
	defer recv.Close()

	w := do(t, newTestServer(t).Handler(), "POST", heartbeatTestPath, `{"url":"`+recv.URL+`/ping/token"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("test ping %d: %s", w.Code, w.Body)
	}
	var out map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	if !out["sent"] {
		t.Errorf("body = %q, want sent=true", w.Body.String())
	}
	select {
	case hit := <-got:
		if hit != "GET /ping/token" {
			t.Errorf("watchdog saw %q, want GET /ping/token", hit)
		}
	default:
		t.Error("the watchdog was never pinged - Test must be a real check-in")
	}
}

// The point of the button is to catch a URL that does not work, so a refusing
// watchdog must reach the user instead of being reported as sent.
func TestHandleNotifyHeartbeatTestSurfacesFailure(t *testing.T) {
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer recv.Close()

	w := do(t, newTestServer(t).Handler(), "POST", heartbeatTestPath, `{"url":"`+recv.URL+`"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("failing ping want 502, got %d: %s", w.Code, w.Body)
	}
	if body := w.Body.String(); !strings.Contains(body, "ping failed") || !strings.Contains(body, "500") {
		t.Errorf("body = %q, want it to say the ping failed and name the status", body)
	}
}

// Healthchecks.io redirects, so the endpoint must use the heartbeat client
// (which follows) rather than the webhook client (which refuses): the wrong one
// reports a perfectly good URL as broken.
func TestHandleNotifyHeartbeatTestFollowsRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer final.Close()
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/ping", http.StatusFound)
	}))
	defer front.Close()

	w := do(t, newTestServer(t).Handler(), "POST", heartbeatTestPath, `{"url":"`+front.URL+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("redirected ping %d: %s (heartbeat client must follow redirects)", w.Code, w.Body)
	}
}

// A heartbeat URL is a credential: the browser must be told the ping was
// refused without being handed the URL or its token back.
func TestHandleNotifyHeartbeatTestBlockedURLIsNotEchoed(t *testing.T) {
	const token = "s3cr3t-t0k3n"
	const url = "http://169.254.169.254/ping/" + token

	w := do(t, newTestServer(t).Handler(), "POST", heartbeatTestPath, `{"url":"`+url+`"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("blocked destination want 502, got %d: %s", w.Code, w.Body)
	}
	body := w.Body.String()
	for _, leak := range []string{token, url, "169.254.169.254"} {
		if strings.Contains(body, leak) {
			t.Errorf("error body leaked %q: %s", leak, body)
		}
	}
}

// The endpoint waits on someone else's server, so it must be exempt from
// baselineWriteWindow like /api/notify/test. The deadline is armed before the
// handler runs, so the exemption is only visible through the write deadline the
// middleware does (or does not) set.
func TestHeartbeatTestIsSelfPaced(t *testing.T) {
	s := newTestServer(t)
	sets := func(method, path, body string) int {
		inner := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		r := httptest.NewRequest(method, path, rd)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		r.Host = "127.0.0.1:9000"
		s.Handler().ServeHTTP(inner, r)
		return inner.sets
	}
	if n := sets("POST", heartbeatTestPath, `{"url":"   "}`); n != 0 {
		t.Errorf("write deadline armed %d times on the heartbeat test; it must be self-paced", n)
	}
	if n := sets("GET", "/api/monitoring", ""); n == 0 {
		t.Fatal("no deadline on an ordinary route either - this test cannot tell the exemption apart")
	}
}

// The Test button exists to say why a heartbeat URL did not work, and a dead
// host or a typo'd scheme is the usual reason - a branch no other test drives.
// The message must name the heartbeat, never the webhook: both fields sit in
// the same Notifications group, so the wrong noun sends people to fix the wrong
// one. It must also survive the trip without carrying the check UUID.
func TestHandleNotifyHeartbeatTestFailureNamesTheRightField(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	addr := dead.URL
	dead.Close() // nothing is listening now, so the dial fails

	h := newTestServer(t).Handler()
	for _, url := range []string{addr + "/ping/s3cr3t-uuid", "ftp://example.invalid/ping/s3cr3t-uuid"} {
		w := do(t, h, "POST", heartbeatTestPath, `{"url":"`+url+`"}`)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("%s: want 502, got %d: %s", url, w.Code, w.Body)
		}
		body := w.Body.String()
		if !strings.Contains(body, "ping failed") {
			t.Errorf("%s: body = %q, want it to say the ping failed", url, body)
		}
		if strings.Contains(body, "webhook") {
			t.Errorf("%s: body = %q calls a heartbeat failure a webhook one", url, body)
		}
		if strings.Contains(body, "s3cr3t-uuid") {
			t.Errorf("%s: body = %q leaked the check UUID", url, body)
		}
	}
}
