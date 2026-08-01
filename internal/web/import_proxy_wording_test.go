package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// C1(b): when a same-host reverse proxy is declared (-allow-host), limiting access
// to the local machine CANNOT block visitors arriving through the proxy, so any
// message claiming the box is "restricted to this machine" is a false promise. The
// warning wording must be conditional on len(AllowedHosts) > 0.

// The import repair for an unenforceable imported login (backup wants login on, no
// password rides with it) forces local-only and warns. That warning must tell the
// truth about what local-only can and cannot do.
func TestImportUnenforceableAuthWarningIsConditionalOnDeclaredProxy(t *testing.T) {
	const proxyPhrase = "CANNOT block"                                 // only truthful when a proxy is declared
	const noProxyPhrase = "access was restricted to this machine only" // only truthful when none is

	run := func(hosts []string) string {
		s := newTestServer(t)
		s.AllowedHosts = hosts
		rr := importConfig(t, s, `{"key":"auth_enabled","value":"1"},{"key":"access_local_only","value":"0"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		return strings.Join(warningsOf(t, rr), " ")
	}

	// No proxy declared: local-only is a real restriction, keep the truthful wording.
	if w := run(nil); !strings.Contains(w, noProxyPhrase) {
		t.Errorf("no proxy declared: warning should keep the truthful local-only wording, got %q", w)
	} else if strings.Contains(w, proxyPhrase) {
		t.Errorf("no proxy declared: warning must not raise a proxy caveat, got %q", w)
	}

	// Proxy declared: the message must state local-only CANNOT block proxied
	// visitors and NOT claim the box was restricted/protected.
	if w := run([]string{"ping.example.com"}); !strings.Contains(w, proxyPhrase) {
		t.Errorf("proxy declared: warning must state local-only CANNOT block proxied visitors, got %q", w)
	} else if strings.Contains(w, noProxyPhrase) {
		t.Errorf("proxy declared: warning falsely claims access was restricted to this machine, got %q", w)
	}
}

// POST /api/access enabling local-only used to record the proxy caveat only in the
// server log. The operator saving the change never saw it. The caveat must be
// surfaced in the response, and only when a proxy is declared.
func TestAccessLocalOnlyResponseWarnsOnlyWhenProxyDeclared(t *testing.T) {
	post := func(s *Server) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/access", strings.NewReader(`{"local_only":true}`))
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rr, r)
		return rr
	}

	// No proxy declared: enabling local-only is a real restriction, no caveat.
	s := newTestServer(t)
	rr := post(s)
	if rr.Code != http.StatusOK {
		t.Fatalf("no-proxy POST /api/access: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if w := strings.Join(warningsOf(t, rr), " "); w != "" {
		t.Errorf("no proxy declared: local-only response should carry no proxy caveat, got %q", w)
	}

	// Proxy declared: the response must carry the caveat, stating local-only CANNOT
	// block proxied visitors.
	s = newTestServer(t)
	s.AllowedHosts = []string{"ping.example.com"}
	rr = post(s)
	if rr.Code != http.StatusOK {
		t.Fatalf("proxy POST /api/access: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if w := strings.Join(warningsOf(t, rr), " "); !strings.Contains(w, "CANNOT block") {
		t.Errorf("proxy declared: local-only response must warn that local-only CANNOT block proxied "+
			"visitors, got %q", w)
	}
}
