package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/pingular/pingularity/internal/speedtest"
)

// countOoklaCalls replaces the server-list fetch with a counter, so a test can
// see whether a request actually reached out to the network rather than
// inferring it from a status code.
func countOoklaCalls(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	prev := listOoklaServers
	listOoklaServers = func(ctx context.Context, lat, lon float64) ([]speedtest.ServerInfo, error) {
		n.Add(1)
		return []speedtest.ServerInfo{{ID: "1", Sponsor: "x", Name: "y"}}, nil
	}
	t.Cleanup(func() { listOoklaServers = prev })
	return &n
}

// A CROSS-SITE PAGE MUST NOT BE ABLE TO MAKE THIS DAEMON PHONE HOME.
//
// /api/speedtest/servers reaches out to Ookla (and, for a city query, to a
// geocoder). Any page in the operator's browser can send a form-style GET or
// POST to http://127.0.0.1:9000 without reading the response and without
// tripping CORS - the browser sends it, the daemon acts on it. The loopback
// filter cannot help: the request genuinely comes from loopback.
//
// The handler next door already knows this. handleIperfCheck demands POST plus
// an application/json content type precisely so a simple cross-site request
// cannot forge it - "body-less POST: CSRF guard" is the comment there. That is
// the pattern; this handler was simply never brought in line.
func TestServerListRefusesSimpleCrossSiteRequests(t *testing.T) {
	s := newTestServer(t)
	s.netinfo = stubNetInfo{}
	h := s.Handler()

	for _, tc := range []struct {
		name, method, ct string
	}{
		// The three request shapes a page can send cross-origin with no preflight.
		{"simple GET", "GET", ""},
		{"form POST", "POST", "application/x-www-form-urlencoded"},
		{"text POST", "POST", "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := countOoklaCalls(t)
			r := httptest.NewRequest(tc.method, "/api/speedtest/servers?city=London", nil)
			r.Host = "127.0.0.1:9000"
			if tc.ct != "" {
				r.Header.Set("Content-Type", tc.ct)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if got := n.Load(); got != 0 {
				t.Errorf("%s made %d outbound call(s): a cross-site page can drive this "+
					"daemon's network activity", tc.name, got)
			}
			if w.Code == http.StatusOK {
				t.Errorf("%s answered 200; it must be refused like the iperf check next door", tc.name)
			}
		})
	}
}

// The UI's own call must keep working: POST with a JSON content type, which a
// cross-site page cannot send without a preflight the daemon never approves.
func TestServerListStillServesTheDashboard(t *testing.T) {
	s := newTestServer(t)
	s.netinfo = stubNetInfo{}
	h := s.Handler()
	n := countOoklaCalls(t)

	w := do(t, h, "POST", "/api/speedtest/servers?lat=51.5&lon=-0.12", `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("the dashboard's own request answered %d, want 200: %s", w.Code, w.Body.String())
	}
	if n.Load() != 1 {
		t.Errorf("the dashboard's request made %d outbound calls, want 1", n.Load())
	}
}
