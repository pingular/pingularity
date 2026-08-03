package web

import (
	"context"
	"net/http"
	"testing"
)

// The reconcile window's "treat everything as local-only" guard has a hole in
// bridged containers: the guard's local-only branch hits `case s.Bridged`,
// which only logs a warning and never rejects. Steady-state that is the
// documented stance (a bridged container NATs everyone to the gateway, so
// loopback can't tell a local user from a LAN one), but DURING a restore the
// box is running on whatever the backup said - possibly "no login, network
// open" - and the login password, the container's one real boundary, may be
// exactly what the backup just switched off. For those few seconds fail
// closed, in a container like anywhere else.
func TestContainerRestoreDoesNotServeAnUnprotectedWindow(t *testing.T) {
	s := newTestServer(t)
	s.InContainer, s.Bridged = true, true
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

	// Sample the guard from inside the window the repair has not yet closed,
	// exactly as a NATed LAN peer would hit it.
	var midCode int
	importReconcileHook = func() {
		midCode = remoteUnauthenticated(t, s, "/api/settings")
	}
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if midCode == http.StatusOK {
		t.Errorf("a container served a remote unauthenticated request DURING the restore: the imported "+
			"settings (no login, network open) were live, and the Bridged branch of the guard warns "+
			"instead of rejecting, so the reconcile window's fail-closed promise does not hold where the "+
			"password is the only boundary (got %d)", midCode)
	}
}

// Outside a reconcile the bridged-container stance is unchanged: local-only
// stays a warn-only setting there (it is unenforceable behind NAT, and
// rejecting would just lock the dashboard out), so a remote request is still
// served.
func TestContainerLocalOnlyStillServesOutsideAReconcile(t *testing.T) {
	s := newTestServer(t)
	s.InContainer, s.Bridged = true, true
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("local: %v", err)
	}
	if code := remoteUnauthenticated(t, s, "/api/settings"); code != http.StatusOK {
		t.Errorf("remote request got %d outside any reconcile; the container local-only stance "+
			"(warn, never reject) must not change when no restore is running", code)
	}
}
