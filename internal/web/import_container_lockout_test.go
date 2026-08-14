package web

import (
	"net/http"
	"strings"
	"testing"
)

// Importing a login-enabled backup with no password fail-closes access to
// local-only and tells the operator to fix things "in the Access tab". In a
// container that advice can be unreachable the moment it is printed: a bridged
// container's published port now answers 403 - to the very browser that ran
// the restore - and the distroless image has no shell to repair the stored
// setting from. So when the daemon is containerized the warning must also
// state the way back in: restart with -e PINGULARITY_ACCESS=network, which is
// authoritative over the stored setting at boot (reconcileAccess, since 0.62).
// Natively the Access-tab advice works as written, and a container recovery
// hint there would be a guess about how the daemon runs - so it must not
// appear.
func TestImportForcedLocalOnlyWarningCarriesContainerRecovery(t *testing.T) {
	const recovery = "PINGULARITY_ACCESS=network"
	run := func(t *testing.T, inContainer bool) string {
		s := newTestServer(t)
		s.InContainer = inContainer
		// The backup wants login on (no password rides along) AND network
		// access open - the exact combination the repair fail-closes.
		rr := importConfig(t, s, `{"key":"auth_enabled","value":"1"},{"key":"access_local_only","value":"0"}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("import: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		if !s.settings.AccessLocalOnly() {
			t.Fatal("fixture: the repair should have forced local-only")
		}
		return strings.Join(warningsOf(t, rr), " ")
	}
	if w := run(t, true); !strings.Contains(w, recovery) {
		t.Errorf("containerized: the forced-local-only warning must state the env-var way back in "+
			"(a 403d browser cannot reach the Access tab it is pointed at), got %q", w)
	}
	if w := run(t, false); strings.Contains(w, recovery) {
		t.Errorf("native: the warning claims a container recovery for a daemon not in one, got %q", w)
	}
}
