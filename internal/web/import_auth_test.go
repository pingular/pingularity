package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// A backup carries the login SETTINGS but never the password hash
// (settingsExportDeny). That asymmetry has two consequences the import did not
// handle, and both hand the operator a box they cannot get back into - or one
// anybody can walk into.
//
// The handler already fails CLOSED in one direction: a backup that wants login on
// with no password to enforce it forces local-only and warns. The reverse
// direction, and the username, had nothing.

func importConfig(t *testing.T, s *Server, rows string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"pingularity_export":2,"categories":["config"],"config":[` + rows + `]}`
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(body))
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Content-Type", "application/json")
	// The guard runs first: a non-loopback RemoteAddr is refused outright when
	// local-only is on, and an active login needs credentials. Without both of
	// these the request never reaches the import handler and the test measures a
	// 403 while appearing to measure the import.
	r.RemoteAddr = "127.0.0.1:54321"
	if s.settings.AuthActive() {
		r.SetBasicAuth(s.settings.AuthUser(), testPassword)
	}
	s.Handler().ServeHTTP(rr, r)
	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("request was rejected by the guard (%d %s), so nothing was imported",
			rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	return rr
}

// importConfigWithHook runs an import and fires hook once the request is under
// way, so a concurrent settings write can be placed inside the window between the
// handler's pre-import snapshot and its post-reload repair.
func importConfigWithHook(t *testing.T, s *Server, rows string, hook func()) *httptest.ResponseRecorder {
	t.Helper()
	importMidHook = hook
	t.Cleanup(func() { importMidHook = nil })
	return importConfig(t, s, rows)
}

func warningsOf(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var d struct {
		Warnings []string `json:"warnings"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &d)
	return d.Warnings
}

// Restoring a backup taken from an open box onto a protected one turned the login
// off and reopened LAN access in one step, with no warning - an unauthenticated
// peer could then read the settings document.
func TestImportCannotSilentlyOpenAProtectedBox(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	// SetAuthPassword takes the username and a bcrypt hash together - the pair is
	// what makes auth "active".
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := s.settings.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("local: %v", err)
	}
	if !s.settings.AuthActive() {
		t.Fatal("fixture: auth should be active")
	}

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	// The dangerous combination is "no login" AND "reachable from the LAN". At
	// minimum the box must not end up in both states without saying so.
	if !s.settings.AuthActive() && !s.settings.AccessLocalOnly() {
		t.Errorf("import left the box with no login AND open to the LAN; the handler already "+
			"forces local-only for the mirror-image case (login wanted, no password) - "+
			"warnings were %v", warningsOf(t, rr))
	}
	if len(warningsOf(t, rr)) == 0 {
		t.Error("a config import that removed login protection returned no warning at all")
	}
}

// A backup from another install carries ITS username. Applying it while the
// destination keeps its own password leaves a pair that cannot authenticate: the
// operator's credentials fail, the session cookie is bound to the old username so
// it dies too, and the endpoint that could set a new password is itself behind
// the login.
func TestImportDoesNotRenameTheLoginAccount(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	// SetAuthPassword takes the username and a bcrypt hash together - the pair is
	// what makes auth "active".
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	rr := importConfig(t, s, `{"key":"auth_user","value":"someoneelse"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.settings.AuthUser(); got != "admin" {
		t.Errorf("login account renamed to %q while the password stayed the destination's; "+
			"the operator can no longer authenticate and cannot reach the endpoint that would "+
			"fix it (warnings %v)", got, warningsOf(t, rr))
	}
}

// On a box with no login configured there is nothing to protect, so a backup's
// username must still restore - otherwise the guard breaks ordinary restores.
func TestImportStillRestoresTheUsernameWhenNoLoginIsSet(t *testing.T) {
	s := newTestServer(t)
	rr := importConfig(t, s, `{"key":"auth_user","value":"restored"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.settings.AuthUser(); got != "restored" {
		t.Errorf("auth_user = %q, want restored: with no active password there is no lockout to "+
			"prevent, so a backup's username should apply", got)
	}
}

const testPassword = "hunter2"

func bcryptHashForTest(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// The username guard keys on AuthActive(), which is `AuthEnabled && AuthHash != ""`.
// A box with a password set but login temporarily switched OFF still owns a
// credential pair, and HasPassword() is what says so. Restoring a backup that
// turns login on and carries a foreign username therefore renamed the account
// AND enabled it, leaving the operator's own password paired with a name they
// never chose.
func TestImportDoesNotRenameADisabledAccountThatOwnsAPassword(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	// Login switched OFF, but the password is still stored - a real, reachable
	// state: it is what "turn login off for a moment" leaves behind.
	if err := s.settings.SetAuthEnabled(ctx, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if s.settings.AuthActive() {
		t.Fatal("fixture: auth must be inactive")
	}
	if !s.settings.HasPassword() {
		t.Fatal("fixture: the password must still be stored")
	}

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"1"},{"key":"auth_user","value":"foreign-user"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.settings.AuthUser(); got != "admin" {
		t.Errorf("account renamed to %q while this machine's own password was kept; the guard "+
			"checks AuthActive() (enabled AND a hash) but the thing worth protecting is owning a "+
			"password at all - HasPassword() (warnings %v)", got, warningsOf(t, rr))
	}
}

// A credential rotation landing DURING an import must not be half-overwritten.
// The handler snapshots the username before streaming the body and restores it
// afterwards; if POST /api/access installs a new user+hash pair in between, the
// restore puts the old NAME back beside the new HASH - a pair that was never set
// by anyone, while the response reports the credentials were preserved.
func TestImportDoesNotClobberACredentialRotationInFlight(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	// testPassword, because importConfig authenticates with it - the request has to
	// reach the handler for this to measure anything.
	if err := s.settings.SetAuthPassword(ctx, "before", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Simulate the rotation completing while the import is mid-flight: the import
	// has already taken its snapshot ("before"), and the operator now sets a wholly
	// new pair.
	rotate := func() {
		if err := s.settings.SetAuthPassword(ctx, "after", bcryptHashForTest(t, "new-pass")); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}
	rr := importConfigWithHook(t, s, `{"key":"speedtest_enabled","value":"1"}`, rotate)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	// The rotation must stand whole. A mixed pair authenticates as neither.
	if got := s.settings.AuthUser(); got != "after" {
		t.Errorf("username is %q after a rotation to \"after\" completed during the import; the "+
			"stale pre-import snapshot was written back over one half of a pair the operator had "+
			"just set, so their new credentials no longer authenticate (warnings %v)",
			got, warningsOf(t, rr))
	}
}
