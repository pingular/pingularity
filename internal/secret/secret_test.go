package secret

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// A second loadOrCreateKey on an existing file must return the SAME key bytes,
// not mint a fresh one - otherwise a restart would strand every password sealed
// under the first key.
func TestLoadOrCreateKeyIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), keyName)
	first, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("first loadOrCreateKey: %v", err)
	}
	if len(first) != keySize {
		t.Fatalf("minted key is %d bytes, want %d", len(first), keySize)
	}
	second, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("second loadOrCreateKey: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("second call returned a different key - the existing file was not reused")
	}
}

// The create path must never promote a half-written temp to the final key: a
// crash mid-write strands a short temp, and if that ever became pingularity.key
// every later boot would hard-reject it (len != keySize) and the daemon would
// silently run WITHOUT encryption. A stray short temp left in the data dir must
// be ignored, and a full-length key minted instead.
func TestStrandedShortTempDoesNotBecomeKey(t *testing.T) {
	dir := t.TempDir()
	// Simulate a crash that left a truncated temp behind. The create path names
	// its temps with the keyName+".tmp-*" pattern os.CreateTemp expands.
	if err := os.WriteFile(filepath.Join(dir, keyName+".tmp-crash"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, keyName)
	key, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatalf("loadOrCreateKey: %v", err)
	}
	if len(key) != keySize {
		t.Fatalf("minted key is %d bytes, want %d", len(key), keySize)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("final key not created: %v", err)
	}
	if fi.Size() != keySize {
		t.Fatalf("final key file is %d bytes, want %d - a short temp was promoted", fi.Size(), keySize)
	}
	// The final key must round-trip, proving it is real key material and not the
	// stray temp's bytes reinterpreted.
	b, err := New(filepath.Join(dir, "pingularity.db"))
	if err != nil {
		t.Fatalf("New over minted key: %v", err)
	}
	sealed, _ := b.Seal("pw")
	if got, err := b.Unseal(sealed); err != nil || got != "pw" {
		t.Fatalf("round trip under minted key = %q, %v; want \"pw\"", got, err)
	}
}

// Concurrency guarantee: many processes (here, goroutines) starting at once must
// all end up with the SAME key. Exactly one wins the os.Link; the losers get
// EEXIST and re-read the winner's fully written file. If any racer minted or read
// a different (or short) key, some server's stored secrets would be undecryptable
// for that process's lifetime.
func TestLoadOrCreateKeyConcurrentStartsAgree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyName)
	const n = 16
	keys := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			keys[i], errs[i] = loadOrCreateKey(path)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if len(keys[i]) != keySize {
			t.Fatalf("racer %d got a %d-byte key, want %d", i, len(keys[i]), keySize)
		}
		if !bytes.Equal(keys[i], keys[0]) {
			t.Fatalf("racer %d got a different key than racer 0 - concurrent starts disagree", i)
		}
	}
	// Every loser must have cleaned up its temp; only the final key should remain.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file after concurrent create: %s", e.Name())
		}
	}
}
