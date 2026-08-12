package web

import (
	"context"
	"net/http"
	"testing"
)

// During a restore the box runs on whatever the backup said - possibly "no
// login, network open" - so the reconcile window must refuse every non-loopback
// request, even though the imported settings would open the box once applied.
// (Containers get no special case anymore: local-only is enforced for everyone,
// so this holds in a container exactly as on a native host.)
func TestContainerRestoreDoesNotServeAnUnprotectedWindow(t *testing.T) {
	s := newTestServer(t)
	s.InContainer = true
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
			"settings (no login, network open) were live, so the reconcile window's fail-closed promise "+
			"does not hold where the password is the only boundary (got %d)", midCode)
	}
}

// A container with local-only on rejects a remote request - there is no bypass.
// Serving the LAN is an explicit opt-in (local-only OFF, via -access network),
// not something the guard infers from being in a container.
func TestContainerLocalOnlyRejectsRemote(t *testing.T) {
	s := newTestServer(t)
	s.InContainer = true
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("local: %v", err)
	}
	if code := remoteUnauthenticated(t, s, "/api/settings"); code != http.StatusForbidden {
		t.Errorf("container remote request got %d, want 403 (local-only is enforced everywhere now)", code)
	}
	// Explicit network access (local-only off) serves the LAN.
	if err := s.settings.SetAccessLocalOnly(context.Background(), false); err != nil {
		t.Fatalf("network: %v", err)
	}
	if code := remoteUnauthenticated(t, s, "/api/settings"); code != http.StatusOK {
		t.Errorf("with network access (local-only off) remote got %d, want 200", code)
	}
}
