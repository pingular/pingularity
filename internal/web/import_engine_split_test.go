package web

import (
	"encoding/json"
	"testing"
)

// A backup from a sparse store carries the Ookla direction and retries and no
// iperf3 pair (the operator never moved it). The restore's per-row upsert lands
// them and reloads; iperf3 must keep its own values rather than take Ookla's,
// which the reload used to do by reading the absent iperf3 keys as a database
// from before the two engines had their own.
func TestImportOfOoklaKnobsLeavesIperfAlone(t *testing.T) {
	s := newTestServer(t)
	type pairs struct {
		SpeedDirection string `json:"speed_direction"`
		IperfDirection string `json:"iperf_direction"`
		SpeedRetries   int    `json:"speed_retries"`
		IperfRetries   int    `json:"iperf_retries"`
	}
	get := func() pairs {
		t.Helper()
		w := do(t, s.Handler(), "GET", "/api/settings", "")
		if w.Code != 200 {
			t.Fatalf("GET /api/settings: HTTP %d", w.Code)
		}
		var p pairs
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}
	before := get()
	if before.SpeedDirection == "down" || before.SpeedRetries == 3 {
		t.Fatalf("precondition: the backup's values must differ from the live ones, got %+v", before)
	}
	rr := importConfig(t, s, `{"key":"speed_direction","value":"down"},{"key":"speed_retries","value":"3"}`)
	if rr.Code != 200 {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	after := get()
	if after.SpeedDirection != "down" || after.SpeedRetries != 3 {
		t.Errorf("imported Ookla pair = %q/%d, want down/3", after.SpeedDirection, after.SpeedRetries)
	}
	if after.IperfDirection != before.IperfDirection || after.IperfRetries != before.IperfRetries {
		t.Errorf("iperf3 pair after the import = %q/%d, want the untouched %q/%d (the backup's Ookla values leaked into iperf3)",
			after.IperfDirection, after.IperfRetries, before.IperfDirection, before.IperfRetries)
	}
	if got := s.settings.IperfDirection(); got != before.IperfDirection {
		t.Errorf("live iperf3 direction = %q, want %q", got, before.IperfDirection)
	}
}
