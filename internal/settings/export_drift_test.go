package settings

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// The store export/import path (ExportTableRows / ImportTable) hard-codes the
// secret semantics that this package owns: which settings keys to deny and how
// to strip iperf3 passwords out of the iperf_servers blob. store can't import
// settings (settings->store is the dependency direction), so those tests live in
// store and seed hand-written JSON literals - nothing there marshals a real
// settings.IperfTarget, so a new secret field added to IperfTarget would flow
// into /api/export in the clear with every store test still green.
//
// This package DOES import store, so it can drive the real export path against
// the real settings types. The two tests below are the drift guard the design
// review asked for: adding a field to IperfTarget, or dropping a settings key
// from the store denylist, fails here instead of silently leaking a credential.

func jsonFieldName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	return strings.Split(tag, ",")[0]
}

// TestExportRedactsEveryIperfSecretField populates every field of IperfTarget
// with a sentinel, runs the list through store's real export path, and checks
// that secret fields are stripped and public fields survive. classify must list
// every JSON field: a new, unclassified field fails the test, forcing whoever
// adds it to decide whether it is safe to carry in a shared backup ("public") or
// a credential the export must drop ("secret", which then also fails until the
// store redactor learns to strip it).
func TestExportRedactsEveryIperfSecretField(t *testing.T) {
	const (
		public = "public"
		secret = "secret"
	)
	classify := map[string]string{
		"label":    public,
		"addr":     public,
		"bind":     public,
		"ipver":    public,
		"auth":     public,
		"username": public,
		"rsa_key":  public,
		"pkcs1":    public,
		"password": secret,
	}

	rt := reflect.TypeOf(IperfTarget{})
	var tgt IperfTarget
	rv := reflect.ValueOf(&tgt).Elem()
	sentinels := map[string]string{} // json name -> sentinel, string fields only
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		name := jsonFieldName(f)
		if name == "" {
			t.Fatalf("IperfTarget.%s has no json tag; the export path can't reason about it", f.Name)
		}
		if _, ok := classify[name]; !ok {
			t.Fatalf("IperfTarget has a new field %q not classified in this test. "+
				"Decide whether it is safe to include in a shared /api/export backup "+
				"(mark it %q) or a credential the store export redactor must strip "+
				"(mark it %q, and teach redactIperfPasswords to drop it).", name, public, secret)
		}
		switch f.Type.Kind() {
		case reflect.String:
			s := "SENTINEL_" + strings.ToUpper(name)
			rv.Field(i).SetString(s)
			sentinels[name] = s
		case reflect.Bool:
			rv.Field(i).SetBool(true)
		default:
			t.Fatalf("IperfTarget.%s has unhandled kind %s; extend this test", f.Name, f.Type.Kind())
		}
	}

	raw, err := json.Marshal([]IperfTarget{tgt})
	if err != nil {
		t.Fatalf("marshal target: %v", err)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.SetSettingsDiff(ctx, map[string]string{keyIperfServers: string(raw)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var exported string
	if err := st.ExportTableRows(ctx, "settings", func(m map[string]any) error {
		if k, _ := m["key"].(string); k == keyIperfServers {
			exported, _ = m["value"].(string)
		}
		return nil
	}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if exported == "" {
		t.Fatal("iperf_servers was not exported at all")
	}

	for name, sentinel := range sentinels {
		present := strings.Contains(exported, sentinel)
		switch classify[name] {
		case secret:
			if present {
				t.Errorf("secret IperfTarget field %q leaked into the export (value %q found in %s)", name, sentinel, exported)
			}
		case public:
			if !present {
				t.Errorf("public IperfTarget field %q was dropped by export redaction (value %q missing from %s)", name, sentinel, exported)
			}
		}
	}
}

// TestExportDeniesSettingsSecretKeys asserts the store denylist still drops the
// secret-bearing settings keys this package defines. If a future secret key is
// added, list it here and in store's settingsExportDeny together - dropping one
// side lights up this test instead of leaking the credential into a backup.
func TestExportDeniesSettingsSecretKeys(t *testing.T) {
	// webhook_url / heartbeat_url are intentionally exportable now (part of a
	// backup). The quick-setup pair is denied for a different reason than the
	// hash: the answer and the offer clock belong to the INSTALL - a restored
	// mid-offer backup must not reopen the dialog on (or hold monitoring of)
	// an established destination.
	secretKeys := []string{keyAuthHash, keyQuickSetup, keyQuickSetupOffer}

	seed := map[string]string{
		keyDigestFreq: "daily", // a non-secret control that MUST survive export
	}
	for _, k := range secretKeys {
		seed[k] = "SENTINEL_" + strings.ToUpper(k)
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if _, err := st.SetSettingsDiff(ctx, seed); err != nil {
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

	for _, k := range secretKeys {
		if _, ok := got[k]; ok {
			t.Errorf("secret settings key %q was exported; store denylist must drop it", k)
		}
	}
	if got[keyDigestFreq] != "daily" {
		t.Errorf("non-secret key %q was dropped by export: got %q", keyDigestFreq, got[keyDigestFreq])
	}
}
