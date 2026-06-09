package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The iperf3 password lives inside the iperf_servers JSON blob, so the key-level
// denylist can't reach it. An export must carry the servers but never their passwords.
func TestExportStripsIperfPasswords(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	const servers = `[{"label":"NAS","addr":"10.0.0.5:5201","username":"bob","password":"SUPERSECRET","rsa_key":"KEY"},
	                  {"label":"VPS","addr":"vps:5201"}]`
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		"iperf_servers": servers,
		"auth_hash":     "should-never-export",
		"speed_engine":  "iperf3",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var got []map[string]any
	if err := st.ExportTableRows(ctx, "settings", func(m map[string]any) error {
		got = append(got, m)
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	var serversRow string
	for _, r := range got {
		k, _ := r["key"].(string)
		if k == "auth_hash" {
			t.Error("auth_hash was exported")
		}
		if k == "iperf_servers" {
			serversRow, _ = r["value"].(string)
		}
	}
	if serversRow == "" {
		t.Fatal("iperf_servers was not exported at all - the server list should survive, only the password shouldn't")
	}
	if strings.Contains(serversRow, "SUPERSECRET") {
		t.Fatalf("the password leaked into the export: %s", serversRow)
	}

	var list []map[string]any
	if err := json.Unmarshal([]byte(serversRow), &list); err != nil {
		t.Fatalf("exported servers are not valid JSON: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("exported %d servers, want 2 (redaction must not drop servers)", len(list))
	}
	if _, ok := list[0]["password"]; ok {
		t.Error("password field is still present in the export")
	}
	if hp, _ := list[0]["has_password"].(bool); !hp {
		t.Error("has_password marker missing - an import can't tell 'not set' from 'not exported'")
	}
	// Everything else about the server must survive.
	if list[0]["addr"] != "10.0.0.5:5201" || list[0]["username"] != "bob" || list[0]["rsa_key"] != "KEY" {
		t.Errorf("redaction damaged the rest of the server: %+v", list[0])
	}
	if _, ok := list[1]["has_password"]; ok {
		t.Error("a server with no password got a has_password marker")
	}
}

// Restoring your OWN backup must not wipe the passwords this host is still using:
// the export carries none, so the import merges the stored ones back in by address.
func TestImportKeepsExistingIperfPasswords(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		"iperf_servers": `[{"label":"NAS","addr":"10.0.0.5:5201","password":"KEEPME"},{"label":"OLD","addr":"gone:5201","password":"DROPME"}]`,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A password-less export (what we now produce), with one address the host still has
	// and one it doesn't.
	imported := `[{"label":"NAS renamed","addr":"10.0.0.5:5201","has_password":true},{"label":"NEW","addr":"new:5201"}]`
	if _, err := st.ImportTable(ctx, "settings", []map[string]any{
		{"key": "iperf_servers", "value": imported},
	}); err != nil {
		t.Fatalf("import: %v", err)
	}

	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(all["iperf_servers"]), &list); err != nil {
		t.Fatalf("stored servers are not valid JSON: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("stored %d servers, want the 2 that were imported", len(list))
	}
	byAddr := map[string]map[string]any{}
	for _, m := range list {
		byAddr[m["addr"].(string)] = m
	}
	if pw, _ := byAddr["10.0.0.5:5201"]["password"].(string); pw != "KEEPME" {
		t.Errorf("restoring a backup wiped the password this host still uses: got %q", pw)
	}
	if byAddr["10.0.0.5:5201"]["label"] != "NAS renamed" {
		t.Error("the imported label did not apply - the merge should only re-attach the password")
	}
	if _, ok := byAddr["new:5201"]["password"]; ok {
		t.Error("a server this host never had somehow got a password")
	}
	if _, ok := byAddr["10.0.0.5:5201"]["has_password"]; ok {
		t.Error("the export-only has_password marker was stored")
	}
}

// webhook_url and heartbeat_url are part of a backup so a restore is complete -
// they round-trip through the export (which is why an export file must be
// treated as sensitive: the URLs are bearer credentials). auth_hash, by
// contrast, stays denied on both sides (import: TestImportSettingsDenylist).
func TestExportIncludesWebhookAndHeartbeatURLs(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		"webhook_url":   "https://discord.example/api/webhooks/123/SECRET",
		"heartbeat_url": "https://hc-ping.example/UUID",
		"auth_hash":     "HASH",  // still denied
		"digest_freq":   "daily", // a non-secret preference that MUST export
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got := map[string]string{}
	if err := st.ExportTableRows(ctx, "settings", func(m map[string]any) error {
		k, _ := m["key"].(string)
		v, _ := m["value"].(string)
		got[k] = v
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if got["webhook_url"] != "https://discord.example/api/webhooks/123/SECRET" {
		t.Errorf("webhook_url should be backed up, got %q", got["webhook_url"])
	}
	if got["heartbeat_url"] != "https://hc-ping.example/UUID" {
		t.Errorf("heartbeat_url should be backed up, got %q", got["heartbeat_url"])
	}
	if _, ok := got["auth_hash"]; ok {
		t.Error("auth_hash must stay out of the export")
	}
	if got["digest_freq"] != "daily" {
		t.Errorf("a non-secret preference was dropped: digest_freq=%q", got["digest_freq"])
	}
}
