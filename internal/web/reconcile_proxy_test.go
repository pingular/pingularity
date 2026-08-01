package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The reconcile window's "treat everything as local-only" stance is judged on the
// real TCP peer, and behind a same-host reverse proxy the peer IS loopback. So the
// local-only branch takes its proxied case, which only warns, and control falls
// through to auth - which the backup has just switched off. The window that exists
// to fail closed serves the request instead.

// proxiedUnauthenticated issues the request a remote visitor makes through a
// same-host reverse proxy: a loopback peer, the public Host the proxy preserved,
// the forwarded client IP, and no credentials.
func proxiedUnauthenticated(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "ping.example.com"
	r.RemoteAddr = "127.0.0.1:44444"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	s.Handler().ServeHTTP(rr, r)
	return rr
}

// protectedServerBehindProxy is a box with a login, local-only on, and the proxy's
// public name allowed via -allow-host.
func protectedServerBehindProxy(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	s.AllowedHosts = []string{"ping.example.com"}
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := s.settings.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("local: %v", err)
	}
	return s
}

// A proxied visitor must not be served during the restore. Outside the window the
// live settings decide as they always did: with a login on, that is a 401.
func TestRestoreDoesNotServeAProxiedRequestDuringTheWindow(t *testing.T) {
	s := protectedServerBehindProxy(t)

	// Before the restore, with the login active: asked for credentials, not refused
	// wholesale - so the mid-window verdict below is the window's doing.
	if code := proxiedUnauthenticated(t, s, "/api/settings").Code; code != http.StatusUnauthorized {
		t.Fatalf("fixture: a proxied unauthenticated request got %d outside any restore, want 401", code)
	}

	var midCode int
	importReconcileHook = func() {
		midCode = proxiedUnauthenticated(t, s, "/api/settings").Code
	}
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if midCode != http.StatusServiceUnavailable {
		t.Errorf("a proxied unauthenticated request got %d DURING the restore, want 503: locality is "+
			"judged on the TCP peer, so a same-host proxy passes the local-only filter, and the "+
			"backup's settings (no login) were live - the window served what it exists to refuse",
			midCode)
	}
	// And the refusal is only for the window: once it closes, the guard is back to
	// judging on the settings (which the repair has just made safe again).
	if code := proxiedUnauthenticated(t, s, "/api/settings").Code; code == http.StatusServiceUnavailable {
		t.Error("the guard still refuses with 503 after the restore finished; the window's blanket " +
			"deny must not outlive the reconcile")
	}
}

// The window must not take the health probes down with it: a load balancer polls
// them unauthenticated, and a restore is exactly when it is watching.
func TestHealthProbesStillAnswerDuringARestore(t *testing.T) {
	s := protectedServerBehindProxy(t)

	var health, ready *httptest.ResponseRecorder
	importReconcileHook = func() {
		health = proxiedUnauthenticated(t, s, "/healthz")
		ready = proxiedUnauthenticated(t, s, "/readyz")
	}
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if health.Code != http.StatusOK {
		t.Errorf("/healthz got %d during the restore, want 200: the probes carry no data and must "+
			"stay answerable while the window is closed to everything else", health.Code)
	}
	// /readyz may legitimately answer not-ready; what it must never do is report the
	// guard's refusal in place of a readiness verdict.
	if body := ready.Body.String(); !strings.Contains(body, "ready") {
		t.Errorf("/readyz answered %d %q during the restore; the reconcile refusal must not stand in "+
			"for the readiness verdict", ready.Code, strings.TrimSpace(body))
	}
}
