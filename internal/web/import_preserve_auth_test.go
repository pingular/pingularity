package web

import (
	"context"
	"net/http"
	"testing"
)

// C1(a): a backup taken from an open box carries auth_enabled=0, but the password
// hash never rides in a backup (settingsExportDeny). Restoring it onto a box that
// owns a working login must NOT disable that login: the destination's own hash is
// still in the store, so re-enabling auth restores full protection. The old repair
// instead forced local-only, which does nothing for a box reached through a
// declared same-host proxy.
func TestImportKeepsDestinationLoginWhenBackupTurnsItOff(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Network access left open (local-only default off), so the LOGIN is the only
	// thing protecting this box - exactly what the backup is about to switch off.
	if !s.settings.AuthActive() {
		t.Fatal("fixture: auth should be active")
	}

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if !s.settings.AuthActive() {
		t.Errorf("restoring an auth-off backup disabled the login on a password-protected box; the "+
			"destination hash never left the store, so the login must be kept active (warnings %v)",
			warningsOf(t, rr))
	}
}

// The fresh-box side of C1(a): a box that never had a login is unchanged. There is
// no destination login to preserve, so an auth-off backup imports cleanly and the
// repair must NOT manufacture an active login (there is no password to enforce it -
// that would be a lockout, not protection).
func TestImportOnAuthlessBoxIsUnchangedByAuthOffBackup(t *testing.T) {
	s := newTestServer(t)
	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if s.settings.AuthActive() {
		t.Errorf("a fresh box became auth-active after importing an auth-off backup: C1(a) preserves an "+
			"EXISTING login, it must never invent one (warnings %v)", warningsOf(t, rr))
	}
}
