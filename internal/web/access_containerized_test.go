package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The sign-in overlay's "locked out?" hint names the machine the recovery
// command has to run on, and that machine is different in a container: the
// binary is in the image and the database on the volume, so "run pingularity
// reset-auth on the host" opens some other database, reports success, and
// leaves the operator locked out. The page took that flag from /api/settings,
// which is gated - so a locked-out operator, the one person who needs the
// advice, is the one person who could not get it. GET /api/access is the
// endpoint the overlay renders from and the one that answers without a
// session, so the flag has to be there, WITH auth active and no credentials.
func TestAccessStatusCarriesContainerizedWithoutASession(t *testing.T) {
	get := func(t *testing.T, s *Server) map[string]any {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/access", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /api/access: %d %s", w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Locked out of a containerized daemon: no cookie, auth active.
	container := newTestServer(t)
	container.InContainer = true
	setPassword(t, container, "admin", "secret")
	out := get(t, container)
	if out["authed"] != false || out["auth_active"] != true {
		t.Fatalf("test set-up: want an unauthenticated caller against active auth, got authed=%v auth_active=%v", out["authed"], out["auth_active"])
	}
	if out["containerized"] != true {
		t.Errorf("containerized = %v (present %v), want true - the overlay's recovery hint still sends a container operator to the host",
			out["containerized"], out["containerized"] != nil)
	}

	// And an explicit false on a native host: the page must not guess a container
	// from a missing field and send a native operator into one.
	native := newTestServer(t)
	setPassword(t, native, "admin", "secret")
	if v, ok := get(t, native)["containerized"]; !ok || v != false {
		t.Errorf("native host: containerized = %v (present %v), want an explicit false", v, ok)
	}
}
