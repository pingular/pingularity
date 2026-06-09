package secret

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealUnsealRoundTrip(t *testing.T) {
	b, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const pw = "hunter2 with spaces & symbols ✓"
	sealed, err := b.Seal(pw)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if strings.Contains(sealed, pw) {
		t.Fatalf("the plaintext survives in the sealed value: %q", sealed)
	}
	if !Sealed(sealed) {
		t.Errorf("Sealed(%q) = false", sealed)
	}
	got, err := b.Unseal(sealed)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if got != pw {
		t.Errorf("round trip = %q, want %q", got, pw)
	}
}

// Sealing the same password twice must produce different ciphertext (fresh nonce),
// or a settings diff would leak "these two servers share a password".
func TestSealIsNonDeterministic(t *testing.T) {
	b, _ := New(":memory:")
	a, _ := b.Seal("same")
	c, _ := b.Seal("same")
	if a == c {
		t.Error("two seals of the same plaintext are identical - nonce is not fresh")
	}
}

// Empty stays empty (no password set); an already-sealed value must not be sealed
// twice, or a save would leave it undecryptable.
func TestSealIdempotentAndEmpty(t *testing.T) {
	b, _ := New(":memory:")
	if v, _ := b.Seal(""); v != "" {
		t.Errorf("Seal(\"\") = %q, want empty", v)
	}
	once, _ := b.Seal("pw")
	twice, _ := b.Seal(once)
	if twice != once {
		t.Error("re-sealing an already-sealed value changed it (double encryption)")
	}
	got, err := b.Unseal(twice)
	if err != nil || got != "pw" {
		t.Errorf("Unseal after double Seal = %q, %v; want \"pw\"", got, err)
	}
}

// A password stored before encryption existed has no prefix: pass it through so the
// user's saved auth keeps working, and let the caller re-seal it.
func TestUnsealPassesThroughLegacyPlaintext(t *testing.T) {
	b, _ := New(":memory:")
	got, err := b.Unseal("legacy-plaintext")
	if err != nil {
		t.Fatalf("Unseal(legacy): %v", err)
	}
	if got != "legacy-plaintext" {
		t.Errorf("legacy passthrough = %q", got)
	}
	if Sealed("legacy-plaintext") {
		t.Error("Sealed() said a legacy plaintext value was encrypted")
	}
}

// Tampering (or the wrong key) must fail loudly, never yield a wrong password.
func TestUnsealRejectsTamperedAndForeignCiphertext(t *testing.T) {
	b1, _ := New(":memory:")
	sealed, _ := b1.Seal("pw")

	// flip a byte inside the payload
	bad := []byte(sealed)
	bad[len(bad)-1] ^= 'a' ^ 'b'
	if _, err := b1.Unseal(string(bad)); err == nil {
		t.Error("tampered ciphertext decrypted without error")
	}

	// a different box (different key) must not decrypt it
	b2, _ := New(":memory:")
	if _, err := b2.Unseal(sealed); err == nil {
		t.Error("a foreign key decrypted the ciphertext")
	}
}

// Malformed sealed inputs (bad base64, too short to hold a nonce, empty payload)
// must return an error and never panic - a corrupt DB row or crafted import must
// not crash the daemon or yield a bogus password.
func TestUnsealRejectsMalformedSealedValues(t *testing.T) {
	b, err := New(":memory:")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ns := b.aead.NonceSize()
	nonceOnly := prefix + base64.RawStdEncoding.EncodeToString(make([]byte, ns)) // valid nonce, no ciphertext/tag
	cases := []string{
		prefix,                 // empty payload after the prefix
		prefix + "AAAA",        // decodes to 3 bytes, far short of a nonce
		prefix + "!!!",         // not valid base64 at all
		prefix + "====",        // base64 padding only
		nonceOnly,              // nonce-length payload with no auth tag
		prefix + "not base64!", // spaces/punctuation
	}
	for _, in := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Unseal(%q) panicked: %v", in, r)
				}
			}()
			got, err := b.Unseal(in)
			if err == nil {
				t.Errorf("Unseal(%q) = %q, want a non-nil error", in, got)
			}
		}()
	}
}

// The key file is created 0600 beside the DB and reused on the next start, so a
// restart doesn't strand every stored password.
func TestKeyFileIsPersistedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "pingularity.db")

	b1, err := New(db)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sealed, _ := b1.Seal("pw")

	fi, err := os.Stat(filepath.Join(dir, keyName))
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if runtime.GOOS != "windows" { // Mode().Perm() is synthetic on Windows
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode = %o, want 600", perm)
		}
	}

	b2, err := New(db) // simulate a restart
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	got, err := b2.Unseal(sealed)
	if err != nil || got != "pw" {
		t.Errorf("after restart, Unseal = %q, %v; want \"pw\" (key was not reused)", got, err)
	}
}

func TestCorruptKeyFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "pingularity.db")
	if err := os.WriteFile(filepath.Join(dir, keyName), []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(db); err == nil {
		t.Error("a truncated key file was accepted")
	}
}
