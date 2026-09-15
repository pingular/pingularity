package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/pingular/pingularity/internal/store"
)

// `pingularity reset-auth` opens the database it is pointed at as it is. The
// daemon's own open sets a torn database aside and rebuilds an empty one, which
// is right for a service that would otherwise crash-loop and wrong for a
// recovery command typed by hand: pointed at the key file beside the database
// (the likeliest slip - same directory, one tab-completion apart) it renamed
// the key to .corrupt, built an empty database under its name, cleared auth on
// that, and reported success.
func TestResetAuthRefusesAFileThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "pingularity.key")
	want := make([]byte, 32)
	if _, err := rand.Read(want); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := resetAuthCmd([]string{"-db", key}); err == nil {
		t.Fatalf("reset-auth reported success on %s, which is not a database", key)
	}
	if got, _ := os.ReadFile(key); !bytes.Equal(got, want) {
		t.Fatal("reset-auth replaced the file it was pointed at")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("reset-auth left %v beside the file it was pointed at; it must create nothing and set nothing aside", names)
	}
}

// On a genuinely torn database the daemon's set-aside is still not this
// command's to take: it would move the history aside as a side effect of a
// password reset and report auth cleared on an empty store, while the running
// service keeps enforcing the old password on the file it still holds open.
func TestResetAuthNeverSetsACorruptDatabaseAside(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pingularity.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSetting(context.Background(), "auth_user", "admin"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	tearDatabase(t, dbPath)
	want, _ := os.ReadFile(dbPath)

	if err := resetAuthCmd([]string{"-db", dbPath}); err == nil {
		t.Fatal("reset-auth reported success on a database it could not open")
	}
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("reset-auth set the database aside (%v): that recovery is the daemon's, and a password reset must never move the history", matches)
	}
	if got, _ := os.ReadFile(dbPath); !bytes.Equal(got, want) {
		t.Fatal("reset-auth rewrote the database it could not open")
	}
}

// The zero-length file a botched copy leaves behind is not a database either.
// SQLite treats an empty file as a brand-new one, so the command used to build a
// full schema inside it and announce that the password had been cleared - on a
// path where no password had ever been stored, while the real database sat
// elsewhere with its login untouched.
func TestResetAuthRefusesAnEmptyFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pingularity.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := resetAuthCmd([]string{"-db", dbPath}); err == nil {
		t.Fatalf("reset-auth reported success on the empty file %s", dbPath)
	}
	if fi, err := os.Stat(dbPath); err != nil || fi.Size() != 0 {
		t.Fatalf("reset-auth built a database in the empty file (size %v, err %v)", fi.Size(), err)
	}
}

// The lockout `reset-auth` exists to end can happen on an install whose -db
// path is a symlink - a database moved to another volume with a link left where
// the unit file points - and that is the install least able to afford a second
// obstacle: the service on it is already not letting anyone in.
func TestResetAuthOpensADatabaseBehindASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	st, err := store.Open(target)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSetting(context.Background(), "auth_user", "admin"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	link := filepath.Join(dir, "pingularity.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := resetAuthCmd([]string{"-db", link}); err != nil {
		t.Fatalf("reset-auth refused a symlinked -db: %v", err)
	}
	if lfi, lerr := os.Lstat(link); lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("reset-auth replaced the symlink at the -db path (%v)", lerr)
	}
	// The password was cleared on the database the link names, not on a store
	// invented somewhere else.
	st, err = store.OpenExisting(target)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if all["auth_enabled"] == "1" || all["auth_hash"] != "" {
		t.Fatalf("auth was not cleared on the database the link names: %v", all)
	}
}
