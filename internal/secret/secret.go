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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pingular/pingularity/internal/osperm"
)

// prefix tags a value as sealed by this package, and carries a version so the
// scheme can change later without guessing at the payload. A value WITHOUT it is
// a legacy plaintext password from before encryption existed - Unseal passes it
// through, and the caller re-seals it on the next save.
const prefix = "enc:v1:"

// Prefix is the reserved tag that marks a sealed value. It is exported so callers
// validating user-supplied secrets can reject a plaintext that begins with it (a
// collision Seal cannot distinguish from real ciphertext - see Seal's known limit).
const Prefix = prefix

// keyName is the key file, kept next to the DB so the two travel together for a
// normal restore but not for a stray copy of the DB alone.
const keyName = "pingularity.key"

const keySize = 32 // AES-256

// ErrKeyPermsInsecure reports that an existing, valid key was loaded but its
// file permissions could not be tightened AND the file is (or may be) group- or
// world-accessible. New returns it alongside a USABLE Box: encrypting with a
// loose-perm key still beats storing passwords in plaintext, but the caller
// should warn loudly.
var ErrKeyPermsInsecure = errors.New("secret: key file permissions could not be secured")

// Seams for the perm-failure tests (os.Chmod failures are not portably
// constructible in-process).
var (
	secureFile = osperm.SecureFile
	permCheck  = osperm.GroupOrWorldAccessible
)

// Box seals and unseals secrets with a key held for the life of the process.
type Box struct {
	aead cipher.AEAD
	key  []byte // master key, retained for deriving purpose-separated subkeys (DeriveSubkey)
}

// DeriveSubkey returns a 32-byte key derived from the master key for an
// independent purpose (label), via HMAC-SHA256(masterKey, label). It gives a
// caller a secret bound to the key file (0600, beside the DB but NOT inside it)
// without exposing or directly reusing the master key: a DB-only copy - a backup
// or stray `cp` that carries no key file - cannot reproduce it. Distinct labels
// yield independent keys, so one use can't be substituted for another.
func (b *Box) DeriveSubkey(label string) []byte {
	mac := hmac.New(sha256.New, b.key)
	mac.Write([]byte(label))
	return mac.Sum(nil)
}

// New loads the key beside dbPath, creating it (0600) on first run. An in-memory
// or empty dbPath gets an ephemeral key: nothing is persisted, so nothing needs
// to survive a restart.
func New(dbPath string) (*Box, error) {
	var key []byte
	var err error
	var permErr error // nil, or ErrKeyPermsInsecure with a usable key in hand
	if dbPath == "" || dbPath == ":memory:" {
		key = make([]byte, keySize)
		if _, err = io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("secret: generate ephemeral key: %w", err)
		}
	} else {
		key, err = loadOrCreateKey(filepath.Join(filepath.Dir(dbPath), keyName))
		if err != nil && !errors.Is(err, ErrKeyPermsInsecure) {
			return nil, err
		}
		permErr = err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secret: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secret: gcm: %w", err)
	}
	return &Box{aead: aead, key: key}, permErr
}

// loadOrCreateKey reads the key file, or creates it if it does not yet exist.
//
// The create path never lets a half-written pingularity.key become the final
// key. The old code did O_CREATE|O_EXCL directly on the final name and then
// wrote 32 bytes into it: a crash or short write in that window stranded a
// zero/short file that every later boot hard-rejects (len != keySize), and main
// then continued WITHOUT a crypter, silently storing iperf passwords in
// plaintext forever. The same window was a live race - the O_EXCL loser could
// ReadFile the winner's not-yet-written file and get a short read, hitting the
// same hard error. So instead we write the key to a PRIVATE temp file in the
// same directory, fsync it, and only then publish it under the final name with
// an atomic no-replace link. The final name therefore only ever appears once the
// full 32 bytes are already on disk.
//
// os.Link (not os.Rename) is what preserves concurrent-start safety: link(2)
// refuses to overwrite an existing name, so of two processes starting at once
// exactly one wins and installs its key. The loser gets EEXIST, drops its temp,
// and re-reads the winner's file - which is guaranteed complete, because the
// winner wrote and fsynced its temp in full before linking. os.Link works on
// NTFS (CreateHardLink), so Windows needs no special case here.
func loadOrCreateKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != keySize {
			return nil, fmt.Errorf("secret: %s is %d bytes, want %d - move it aside and re-enter your passwords", path, len(b), keySize)
		}
		// An existing key created before a umask fix (or copied in) may be too open.
		if err := secureFile(path); err != nil {
			// We already hold a valid key. Discarding it here would drop the whole
			// crypter and store iperf3 passwords in PLAINTEXT - strictly worse than a
			// key we could not re-tighten. Proceed if the file is verifiably owner-only
			// already; otherwise proceed in an explicit degraded mode (encrypted with a
			// loose-perm key still beats plaintext), signalled to the caller.
			accessible, known := permCheck(path)
			if known && !accessible {
				return b, nil // chmod was redundant; file is already 0600
			}
			return b, fmt.Errorf("%w (%s): %w", ErrKeyPermsInsecure, path, err)
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

	// The temp lives in the SAME directory as the final key so the later link is
	// a same-filesystem hard link (a cross-device link would fail with EXDEV). The
	// random "*" suffix keeps concurrent creators from colliding on the temp name.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, keyName+".tmp-*")
	if err != nil {
		return nil, fmt.Errorf("secret: create temp key: %w", err)
	}
	tmpName := tmp.Name()
	// From here every error path must remove tmpName: a stranded temp can never
	// become the final key (only a fully written, linked temp does), but leaving
	// truncated temps littering the data dir is still untidy.
	fail := func(format string, err error) ([]byte, error) {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf(format, err)
	}
	if _, err := tmp.Write(key); err != nil {
		return fail("secret: write temp key: %w", err)
	}
	// CreateTemp makes the temp 0600 on Unix but writes no ACL on Windows; lock it
	// down BEFORE it becomes visible under the final name (the link shares this
	// file's inode/ACL) so the key is never briefly world-readable there.
	if err := osperm.SecureFile(tmpName); err != nil {
		return fail("secret: secure temp key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("secret: sync temp key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("secret: close temp key: %w", err)
	}

	// Publish the fully written temp under the final name. Link never overwrites,
	// so the final name can't expose a half-written file and a concurrent starter
	// can't clobber a key that's already in use.
	if err := os.Link(tmpName, path); err != nil {
		os.Remove(tmpName) // the temp has done its job, or never will
		if errors.Is(err, os.ErrExist) {
			// Lost the race: another starter linked its key first. That file is
			// complete (it fsynced its temp in full before linking), so re-reading
			// through the top branch returns the winner's key, not a short read.
			return loadOrCreateKey(path)
		}
		return nil, fmt.Errorf("secret: install key: %w", err)
	}
	os.Remove(tmpName) // unlink the temp name; the final hard link keeps the bytes

	// fsync the directory so the new entry survives a crash right after this call:
	// the key bytes are already durable (temp Sync), this makes the NAME durable.
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("secret: sync key dir: %w", err)
	}
	return key, nil
}

// syncDir fsyncs a directory so a freshly linked entry inside it is durable. On
// Windows a directory handle can't be flushed (FlushFileBuffers rejects it) and
// there is no portable equivalent, so a failure there is tolerated - the file
// bytes were already fsynced via the temp, and NTFS metadata journaling covers
// the directory entry itself.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
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
