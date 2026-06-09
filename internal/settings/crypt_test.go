package settings

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/secret"
	"github.com/pingular/pingularity/internal/store"
)

func newSealedController(t *testing.T, st *store.Store) *Controller {
	t.Helper()
	box, err := secret.New(":memory:") // ephemeral key, same box for the life of the test
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	c, err := New(context.Background(), st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, Monitoring: true,
	}, WithCrypter(box))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The password must be ciphertext on disk but plaintext in memory - iperf3 needs the
// real thing at test time.
func TestIperfPasswordIsSealedAtRest(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	c := newSealedController(t, st)

	const pw = "SUPERSECRET"
	if _, err := c.Update(ctx, Patch{
		IperfServer:  pv("10.0.0.5:5201"),
		IperfServers: []IperfTarget{{Addr: "10.0.0.5:5201", Auth: true, Username: "bob", Password: pw}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// On disk: sealed.
	raw, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	stored := raw[keyIperfServers]
	if strings.Contains(stored, pw) {
		t.Fatalf("the password is in the clear on disk: %s", stored)
	}
	var onDisk []IperfTarget
	if err := json.Unmarshal([]byte(stored), &onDisk); err != nil {
		t.Fatalf("stored servers are not valid JSON: %v", err)
	}
	if !secret.Sealed(onDisk[0].Password) {
		t.Errorf("stored password is not sealed: %q", onDisk[0].Password)
	}

	// In memory: the real password, or iperf3 can't authenticate.
	if got := c.IperfPassword(); got != pw {
		t.Errorf("IperfPassword() = %q, want the plaintext %q", got, pw)
	}
}

// A restart must be able to read its own passwords back.
func TestSealedPasswordSurvivesReload(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	box, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	base := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}

	c1, err := New(ctx, st, base, WithCrypter(box))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c1.Update(ctx, Patch{
		IperfServer:  pv("10.0.0.5:5201"),
		IperfServers: []IperfTarget{{Addr: "10.0.0.5:5201", Auth: true, Password: "pw"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Same key (same box), fresh controller - what a restart looks like.
	c2, err := New(ctx, st, base, WithCrypter(box))
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	if got := c2.IperfPassword(); got != "pw" {
		t.Errorf("after restart IperfPassword() = %q, want \"pw\"", got)
	}
}

// A password stored before encryption existed must keep working AND be sealed on
// first load - otherwise turning this on leaves the old ones exposed forever.
func TestLegacyPlaintextPasswordIsMigrated(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Simulate a DB written by an older build: plaintext password, no prefix.
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		keyIperfServers: `[{"label":"NAS","addr":"10.0.0.5:5201","auth":true,"password":"OLDPLAINTEXT"}]`,
		keyIperfServer:  "10.0.0.5:5201",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c := newSealedController(t, st)

	// It still works...
	if got := c.IperfPassword(); got != "OLDPLAINTEXT" {
		t.Errorf("legacy password not readable: got %q", got)
	}
	// ...and it is no longer in the clear on disk.
	raw, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if strings.Contains(raw[keyIperfServers], "OLDPLAINTEXT") {
		t.Errorf("legacy plaintext was not migrated; still on disk: %s", raw[keyIperfServers])
	}
}

// Without a crypter (the default in tests) nothing changes - encryption is opt-in
// at construction, so a missing key file degrades to the old behaviour, not a crash.
func TestNoCrypterStoresPlaintext(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	c := newController(t)
	_ = st

	if _, err := c.Update(ctx, Patch{
		IperfServer:  pv("h:5201"),
		IperfServers: []IperfTarget{{Addr: "h:5201", Auth: true, Password: "pw"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := c.IperfPassword(); got != "pw" {
		t.Errorf("IperfPassword() = %q, want \"pw\"", got)
	}
}

// A config import can restore passwords in the clear (an older export still
// carries them). Reload - the path taken after an import - must re-seal them the
// same way New does, so they don't sit unencrypted on disk until the next
// restart or settings save.
func TestReloadReSealsLegacyPlaintext(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	c := newSealedController(t, st)

	// Simulate what a config import writes: plaintext passwords straight into the
	// settings table (no sealing anywhere in the import path).
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		keyIperfServers: `[{"label":"NAS","addr":"10.0.0.5:5201","auth":true,"password":"IMPORTEDPLAINTEXT"}]`,
		keyIperfServer:  "10.0.0.5:5201",
	}); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// Still usable in memory...
	if got := c.IperfPassword(); got != "IMPORTEDPLAINTEXT" {
		t.Errorf("password not readable after reload: got %q", got)
	}
	// ...and no longer in the clear on disk.
	raw, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if strings.Contains(raw[keyIperfServers], "IMPORTEDPLAINTEXT") {
		t.Errorf("Reload left the imported password in the clear on disk: %s", raw[keyIperfServers])
	}
}

// A password sealed under one key must NOT survive being reopened under a different
// key: it can't be decrypted, so the in-memory value is blanked (the tester must
// never send ciphertext, and the UI re-prompts). The other fields stay intact.
func TestUnsealFailureBlanksPassword(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	boxA, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New A: %v", err)
	}
	base := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}

	c1, err := New(ctx, st, base, WithCrypter(boxA))
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	if _, err := c1.Update(ctx, Patch{
		IperfServer:  pv("10.0.0.5:5201"),
		IperfServers: []IperfTarget{{Label: "NAS", Addr: "10.0.0.5:5201", Auth: true, Username: "bob", Password: "SECRET"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Reopen with a DIFFERENT key - the DB restored to a host whose key was regenerated.
	boxB, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New B: %v", err)
	}
	c2, err := New(ctx, st, base, WithCrypter(boxB))
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	servers := c2.Snapshot().IperfServers
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	if servers[0].Password != "" {
		t.Errorf("undecryptable password should be blanked in memory, got %q", servers[0].Password)
	}
	// The non-secret fields must survive the failed decrypt.
	if servers[0].Label != "NAS" || servers[0].Addr != "10.0.0.5:5201" || servers[0].Username != "bob" || !servers[0].Auth {
		t.Errorf("non-secret fields did not survive: %+v", servers[0])
	}
}

// The confirmed erasure finding: when a stored password can't be decrypted (wrong
// key) its ciphertext must NOT be destroyed by an unrelated settings save. It has
// to survive on disk so restoring the original key still recovers the password.
func TestUnrecoverablePasswordSurvivesUnrelatedSave(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	boxA, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New A: %v", err)
	}
	base := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}

	c1, err := New(ctx, st, base, WithCrypter(boxA))
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	if _, err := c1.Update(ctx, Patch{
		IperfServer:  pv("10.0.0.5:5201"),
		IperfServers: []IperfTarget{{Addr: "10.0.0.5:5201", Auth: true, Password: "SECRET"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Capture the exact ciphertext on disk.
	before, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	sealedOnDisk := before[keyIperfServers]

	// Reopen under a wrong key (regenerated pingularity.key), then make an UNRELATED
	// change (the ping interval) - nothing to do with iperf3.
	boxB, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New B: %v", err)
	}
	c2, err := New(ctx, st, base, WithCrypter(boxB))
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	if _, err := c2.Update(ctx, Patch{Latency: pv(9 * time.Second)}); err != nil {
		t.Fatalf("unrelated Update: %v", err)
	}

	// The ciphertext must be untouched on disk...
	after, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings after: %v", err)
	}
	if after[keyIperfServers] != sealedOnDisk {
		t.Fatalf("unrelated save rewrote the iperf servers blob:\n before %s\n after  %s", sealedOnDisk, after[keyIperfServers])
	}
	// ...and restoring the original key must recover the password.
	c3, err := New(ctx, st, base, WithCrypter(boxA))
	if err != nil {
		t.Fatalf("New A (key restored): %v", err)
	}
	if got := c3.IperfPassword(); got != "SECRET" {
		t.Errorf("password not recovered after key restore: got %q, want \"SECRET\"", got)
	}
}

// The same protection with NO crypter at all (the key file was unreadable, so the
// daemon runs without encryption): an unrelated save must not erase the ciphertext.
func TestUnrecoverablePasswordSurvivesUnrelatedSaveNoCrypter(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	boxA, err := secret.New(":memory:")
	if err != nil {
		t.Fatalf("secret.New A: %v", err)
	}
	base := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}

	c1, err := New(ctx, st, base, WithCrypter(boxA))
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	if _, err := c1.Update(ctx, Patch{
		IperfServer:  pv("10.0.0.5:5201"),
		IperfServers: []IperfTarget{{Addr: "10.0.0.5:5201", Auth: true, Password: "SECRET"}},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	before, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	sealedOnDisk := before[keyIperfServers]

	// Reopen WITHOUT a crypter (key file unreadable this boot), unrelated save.
	c2, err := New(ctx, st, base)
	if err != nil {
		t.Fatalf("New no-crypter: %v", err)
	}
	if _, err := c2.Update(ctx, Patch{Latency: pv(9 * time.Second)}); err != nil {
		t.Fatalf("unrelated Update: %v", err)
	}
	after, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings after: %v", err)
	}
	if after[keyIperfServers] != sealedOnDisk {
		t.Fatalf("no-crypter unrelated save rewrote the iperf servers blob:\n before %s\n after  %s", sealedOnDisk, after[keyIperfServers])
	}
	// Key file returns: the password decrypts again.
	c3, err := New(ctx, st, base, WithCrypter(boxA))
	if err != nil {
		t.Fatalf("New A (key restored): %v", err)
	}
	if got := c3.IperfPassword(); got != "SECRET" {
		t.Errorf("password not recovered after key restore: got %q, want \"SECRET\"", got)
	}
}

// If the legacy re-seal WRITE fails during Reload, the imported config must
// still be applied to the live process (not discarded), and Reload must report
// the distinguishable ErrLegacyReseal - mirroring New. Regression test for the
// bug where a transient re-seal write failure dropped an otherwise-valid import.
func TestReloadAppliesConfigWhenResealWriteFails(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	c := newSealedController(t, st)

	// Seed a legacy plaintext password straight into the table, as an import
	// does (keyIperfServer selects the server IperfPassword() reads).
	if _, err := st.SetSettingsDiff(ctx, map[string]string{
		keyIperfServers: `[{"label":"NAS","addr":"10.0.0.5:5201","auth":true,"password":"IMPORTEDPLAINTEXT"}]`,
		keyIperfServer:  "10.0.0.5:5201",
	}); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// Make the store refuse writes but still serve reads (the :memory: pool is a
	// single connection, so this pragma sticks): Reload's read succeeds and only
	// the re-seal write fails, isolating exactly the failure path under test.
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only on: %v", err)
	}

	err = c.Reload(ctx)
	if !errors.Is(err, ErrLegacyReseal) {
		t.Fatalf("Reload err = %v, want it to wrap ErrLegacyReseal", err)
	}
	// The fix: the imported config is live in memory despite the failed re-seal.
	if got := c.IperfPassword(); got != "IMPORTEDPLAINTEXT" {
		t.Errorf("imported config discarded after reseal-write failure: IperfPassword() = %q", got)
	}
	// Restore writes so the deferred Close runs cleanly.
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("query_only off: %v", err)
	}
}
