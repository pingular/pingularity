// Package secret encrypts the few at-rest secrets Pingularity has to keep in
// recoverable form - today just each saved iperf3 server's password, which
// iperf3 needs in the clear at test time (it encrypts it with the server's RSA
// key itself), so it cannot be hashed the way the dashboard login is.
//
// WHAT THIS DOES AND DOES NOT BUY YOU. The key lives in a 0600 file beside the
// database, so anyone who can read the database file can normally read the key
// too. This is NOT protection against someone with access to the host. What it
// does protect is the database travelling on its own: a backup, a VM snapshot,
// a stray `cp pingularity.db` onto a share - all of which now carry ciphertext
// instead of your password. Treat it as defence in depth, not as a vault.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pingular/pingularity/internal/osperm"
)

// prefix tags a value as sealed by this package, and carries a version so the
// scheme can change later without guessing at the payload. A value WITHOUT it is
// a legacy plaintext password from before encryption existed - Unseal passes it
// through, and the caller re-seals it on the next save.
const prefix = "enc:v1:"

// keyName is the key file, kept next to the DB so the two travel together for a
// normal restore but not for a stray copy of the DB alone.
const keyName = "pingularity.key"

const keySize = 32 // AES-256

// Box seals and unseals secrets with a key held for the life of the process.
type Box struct{ aead cipher.AEAD }

// New loads the key beside dbPath, creating it (0600) on first run. An in-memory
// or empty dbPath gets an ephemeral key: nothing is persisted, so nothing needs
// to survive a restart.
func New(dbPath string) (*Box, error) {
	var key []byte
	var err error
	if dbPath == "" || dbPath == ":memory:" {
		key = make([]byte, keySize)
		if _, err = io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("secret: generate ephemeral key: %w", err)
		}
	} else if key, err = loadOrCreateKey(filepath.Join(filepath.Dir(dbPath), keyName)); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Box{aead: aead}, nil
}

// loadOrCreateKey reads the key file, or creates it with O_EXCL so two starting
// processes can't both write one and leave the loser's secrets undecryptable.
func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != keySize {
			return nil, fmt.Errorf("secret: %s is %d bytes, want %d - move it aside and re-enter your passwords", path, len(b), keySize)
		}
		// An existing key created before a umask fix (or copied in) may be too open.
		if err := osperm.SecureFile(path); err != nil {
			return nil, fmt.Errorf("secret: secure key: %w", err)
		}
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("secret: read key: %w", err)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("secret: generate key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) { // lost the race - read the winner's key
			return loadOrCreateKey(path)
		}
		return nil, fmt.Errorf("secret: create key: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(key); err != nil {
		return nil, fmt.Errorf("secret: write key: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	// O_EXCL's 0600 mode is honoured on Unix but ignored on Windows (no ACL is
	// written); apply the owner-only protection explicitly either way.
	if err := osperm.SecureFile(path); err != nil {
		return nil, fmt.Errorf("secret: secure key: %w", err)
	}
	return key, nil
}

// Seal encrypts plain. An empty string stays empty (no password set), and an
// already-sealed value is returned as-is so re-saving can't double-encrypt.
// Known limit: a real password that itself starts with "enc:v1:" is
// indistinguishable from a sealed value, passes through in the clear, and then
// fails to unseal at use time. Accepted: the collision is contrived, and
// guessing wrong the other way would double-encrypt every re-save.
func (b *Box) Seal(plain string) (string, error) {
	if plain == "" || strings.HasPrefix(plain, prefix) {
		return plain, nil
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secret: nonce: %w", err)
	}
	ct := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(ct), nil
}

// Unseal decrypts a sealed value. A value without the prefix is a legacy
// plaintext password and is returned unchanged (the caller re-seals it on the
// next save). A sealed value that won't decrypt - wrong or lost key, tampered
// file - returns an error rather than a wrong password.
func (b *Box) Unseal(sealed string) (string, error) {
	if sealed == "" || !strings.HasPrefix(sealed, prefix) {
		return sealed, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, prefix))
	if err != nil {
		return "", fmt.Errorf("secret: decode: %w", err)
	}
	ns := b.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("secret: sealed value too short")
	}
	pt, err := b.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", fmt.Errorf("secret: cannot decrypt (wrong or lost key file?): %w", err)
	}
	return string(pt), nil
}

// Sealed reports whether v is already encrypted - used to spot legacy plaintext
// that still needs migrating.
func Sealed(v string) bool { return strings.HasPrefix(v, prefix) }
