package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// M5: the UI's iperf3 loopback-target warning keys on BRIDGED (a container
// without the host's network namespace), never on merely containerized - in a
// host-network container localhost IS the host, so a loopback iperf3 target is
// a working configuration there and must not be warned about. The settings
// payload therefore carries a bridged flag distinct from containerized, on GET
// and on the POST echo (applySettings re-reads every flag after a save), and
// it follows the namespace probe, not the container marker.
func TestSettingsPayloadCarriesBridgedDistinctFromContainerized(t *testing.T) {
	restore := bridgedContainerFn
	t.Cleanup(func() { bridgedContainerFn = restore })

	settingsOf := func(t *testing.T, s *Server, method, body string) map[string]any {
		t.Helper()
		w := do(t, s.Handler(), method, "/api/settings", body)
		if w.Code != http.StatusOK {
			t.Fatalf("%s /api/settings: %d %s", method, w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// Containerized on host networking: containerized true, bridged false - the
	// exact state the loopback warning used to misfire in.
	bridgedContainerFn = func() bool { return false }
	s := newTestServer(t)
	s.InContainer = true
	out := settingsOf(t, s, "GET", "")
	if out["containerized"] != true {
		t.Fatalf("containerized = %v, want true", out["containerized"])
	}
	if v, ok := out["bridged"]; !ok || v != false {
		t.Errorf("host-net container: bridged = %v (present %v), want an explicit false", v, ok)
	}

	// Bridged: the flag flips with the probe while containerized stays put, and
	// the POST echo carries it too (or the trap clears after every save).
	bridgedContainerFn = func() bool { return true }
	if out := settingsOf(t, s, "GET", ""); out["bridged"] != true {
		t.Errorf("bridged container: GET bridged = %v, want true", out["bridged"])
	}
	if out := settingsOf(t, s, "POST", `{"latency_seconds":10}`); out["bridged"] != true {
		t.Errorf("bridged container: POST echo bridged = %v, want true", out["bridged"])
	}
}

// The status payload's bridged_container banner flag rides the same probe (and
// stays absent when not bridged - only sent when true).
func TestStatusBridgedContainerFlagFollowsTheProbe(t *testing.T) {
	restore := bridgedContainerFn
	t.Cleanup(func() { bridgedContainerFn = restore })

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := statusServer(t, st)

	bridgedContainerFn = func() bool { return true }
	if out := getStatus(t, s, ""); out["bridged_container"] != true {
		t.Errorf("bridged: status bridged_container = %v, want true", out["bridged_container"])
	}
	bridgedContainerFn = func() bool { return false }
	if v, ok := getStatus(t, s, "")["bridged_container"]; ok {
		t.Errorf("not bridged: status carries bridged_container = %v, want the key absent", v)
	}
}
