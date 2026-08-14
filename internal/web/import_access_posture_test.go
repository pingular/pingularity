package web

import (
	"context"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// access_local_only decides who may reach the dashboard at all. It is the most
// install-scoped key there is: it describes the network posture of ONE host -
// whether that host's port is safe to answer on - and nothing about the history
// being restored.
//
// The daemon treats it that way everywhere else. warnAmbiguousContainerAccess
// goes out of its way NOT to persist an open posture even when the evidence is
// strong ("a heuristic may advise, never persist"), because getting it wrong
// either exposes a dashboard or locks the operator out of their own container.
// A backup file - which the security model treats as sensitive, i.e. something
// an attacker may hold or craft - must not be the one path that writes it.
//
// Restoring last month's backup, or a backup from another machine, is an
// ordinary thing to do. It must move the DATA, not decide whether this host
// answers the network.
func TestAccessPostureNeverLeavesOrEntersInABackup(t *testing.T) {
	ctx := context.Background()

	// EXPORT: a source that serves the network must not put that in the file.
	src := newTestServer(t)
	if err := src.store.SetSetting(ctx, "access_local_only", "0"); err != nil {
		t.Fatalf("seed access: %v", err)
	}
	rr := do(t, src.Handler(), "GET", "/api/export?config=1", "")
	if rr.Code != 200 {
		t.Fatalf("export: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "access_local_only") {
		t.Errorf("the export carries access_local_only: restoring this backup anywhere else silently decides " +
			"whether that host answers the network")
	}

	// IMPORT: a crafted or older file carrying the key must not move a
	// destination that is closed.
	for _, tc := range []struct {
		name, value string
		harm        string
	}{
		{"opening a closed install", "0",
			"an unauthenticated peer on the LAN can now reach the dashboard, and nothing in the response said so"},
		{"closing an open install", "1",
			"the container now 403s its own published port and the response offers no way back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dstStore, err := store.Open(":memory:")
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { dstStore.Close() })
			// Start from the opposite of what the backup asks for, so a change is
			// unambiguous.
			start := tc.value == "0"
			if err := dstStore.SetSetting(ctx, "access_local_only", map[bool]string{true: "1", false: "0"}[start]); err != nil {
				t.Fatalf("seed access: %v", err)
			}
			dstSet, err := settings.New(ctx, dstStore, settings.Values{
				Latency: 5e9, Speed: 3600e9, Timeout: 2e9,
				DownAfter: 3, UpAfter: 2, AccessLocalOnly: start,
			})
			if err != nil {
				t.Fatalf("new settings: %v", err)
			}
			dst := newTestServerWith(t, dstStore, dstSet)

			backup := `{"pingularity_export":1,"config":[{"key":"access_local_only","value":"` + tc.value + `"}]}`
			if rr := importBackup(t, dst, "config=1", backup); rr.Code != 200 {
				t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
			}

			after, err := dstStore.AllSettings(ctx)
			if err != nil {
				t.Fatalf("AllSettings: %v", err)
			}
			if got := after["access_local_only"]; got == tc.value {
				t.Errorf("a restored backup rewrote this host's access posture to %q: %s", got, tc.harm)
			}
			if got := dstSet.AccessLocalOnly(); got != start {
				t.Errorf("the live access posture moved to %v on a restore; it must be decided by this host, "+
					"not by the file", got)
			}
		})
	}
}
