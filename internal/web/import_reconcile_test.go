package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Restoring a backup is not one step. The imported settings go live the moment the
// controller reloads, and the safety repair - the thing that stops a backup taking
// your login away or opening the box to the network - runs afterwards. In between,
// the destination is running on whatever the backup said, and the request guard
// reads those live values on every request.
//
// So the guarantee "a restore cannot leave you with no login AND network access"
// is true at the end and false in the middle, which for an access control is the
// same as false.

// remoteUnauthenticated issues the request a LAN peer would: not loopback, no
// credentials.
func remoteUnauthenticated(t *testing.T, s *Server, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "192.168.1.50:40000"
	s.Handler().ServeHTTP(rr, r)
	return rr.Code
}

// A protected box restoring a backup taken from an open one must never, at any
// instant, serve a remote unauthenticated request.
func TestRestoreNeverPublishesAnUnprotectedWindow(t *testing.T) {
	s := newTestServer(t)
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

	// Sample the guard from inside the window the repair has not yet closed.
	var midCode int
	var midAuthActive, midLocalOnly bool
	importReconcileHook = func() {
		midAuthActive = s.settings.AuthActive()
		midLocalOnly = s.settings.AccessLocalOnly()
		midCode = remoteUnauthenticated(t, s, "/api/settings")
	}
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	t.Logf("inside the window: auth_active=%v local_only=%v; remote unauthenticated GET -> %d",
		midAuthActive, midLocalOnly, midCode)

	if midCode == http.StatusOK {
		t.Errorf("a remote unauthenticated request was served DURING the restore, while the "+
			"imported settings were live and the safety repair had not run yet (auth_active=%v "+
			"local_only=%v). The protection is applied after the fact, so the guarantee holds "+
			"only once the request that broke it has already been answered",
			midAuthActive, midLocalOnly)
	}
}

// A password-only change keeps the username by design (SetAuthPassword with an
// empty user). The repair infers "the operator rotated the pair" from the password
// HASH changing - so a password-only rotation during a restore looks like a
// deliberate pair change, and the backup's username is allowed to stand beside the
// operator's new password. Nobody chose that combination and neither credential
// set opens it.
func TestRestoreKeepsTheUsernameWhenOnlyThePasswordRotates(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "before", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Mid-import, the operator changes ONLY their password. This is what
	// handleAccess does for a password-only change: it passes the CURRENT username
	// alongside the new hash, so the name is kept (SetAuthPassword itself defaults a
	// genuinely empty name to "admin", which is first-time setup, not a rotation).
	rotate := func() {
		if err := s.settings.SetAuthPassword(ctx, s.settings.AuthUser(), bcryptHashForTest(t, "brand-new")); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	rr := importConfigWithHook(t, s, `{"key":"auth_user","value":"from-backup"}`, rotate)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}

	if got := s.settings.AuthUser(); got != "before" {
		t.Errorf("username is %q after a PASSWORD-ONLY rotation during the restore; the operator "+
			"never changed their username, so the backup's name has been paired with their new "+
			"password and neither of their credential sets works (warnings %v)",
			got, warningsOf(t, rr))
	}
}

// Once a category has committed, the reconcile must finish even if the client
// hangs up. Running it on the request context leaves the persisted settings and
// the live controller disagreeing until a restart.
func TestRestoreReconcilesEvenIfTheClientDisconnects(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "before", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	importReconcileHook = func() { cancel() } // the client goes away mid-reconcile
	t.Cleanup(func() { importReconcileHook = nil })

	body := `{"pingularity_export":2,"categories":["config"],"config":[{"key":"auth_user","value":"from-backup"}]}`
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(body)).WithContext(reqCtx)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	r.SetBasicAuth(s.settings.AuthUser(), testPassword)
	s.Handler().ServeHTTP(rr, r)

	live := s.settings.AuthUser()
	if err := s.settings.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	persisted := s.settings.AuthUser()
	t.Logf("after a cancelled restore: live=%q persisted=%q", live, persisted)

	if live != persisted {
		t.Errorf("live username is %q but the stored one is %q - the reconcile ran on the "+
			"client's context, so hanging up left the two disagreeing and a restart would adopt "+
			"the backup's name", live, persisted)
	}
	if persisted != "before" {
		t.Errorf("stored username is %q, want \"before\": the reconcile must complete once "+
			"config has committed, whatever the client does", persisted)
	}
}
