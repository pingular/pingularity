package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Whatever sits at the -db path is checked BEFORE anything touches it. A
// directory there is a mistyped path (-db /var/lib/pingularity for
// .../pingularity.db); it used to be chmod'ed to 0600 - no execute bit, so
// nothing can traverse it any more - and only then did the open fail.
func TestOpenRefusesADirectoryAtTheDBPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "victimdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // so TempDir cleanup can traverse it either way
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(dir)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded on a directory")
	}
	fi, serr := os.Stat(dir)
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("a failed Open re-permissioned the directory at the -db path to %o (was 755): it is untraversable now", fi.Mode().Perm())
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Fatalf("the error does not name the mistake: %v", err)
	}
}

// A symlink at the -db path used to have its TARGET chmod'ed through the link
// and was then itself renamed away as a corrupt database, with a real database
// created in its place.
func TestOpenRefusesASymlinkAtTheDBPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits and symlinks")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.conf")
	want := []byte("other users config\n")
	if err := os.WriteFile(target, want, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "p.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	st, err := Open(link)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded through a symlink at the -db path")
	}
	fi, serr := os.Stat(target)
	if serr != nil {
		t.Fatal(serr)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("the symlink's target was re-permissioned through the link to %o (was 644)", fi.Mode().Perm())
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, want) {
		t.Fatal("the symlink's target was rewritten")
	}
	if lfi, lerr := os.Lstat(link); lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink at the -db path is gone or replaced (%v)", lerr)
	}
	if matches, _ := filepath.Glob(link + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the symlink was set aside as a corrupt database: %v", matches)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("the error does not name the mistake: %v", err)
	}
}

// OpenExisting is the recovery commands' door: it opens what is there or says
// why it cannot, and creates nothing. Every way of not being a database has to
// come back as an error rather than as a freshly built empty store - `pingularity
// reset-auth -db <path>` reports success off whatever it opens, so a store it
// invented reads as "your password is cleared" on a database that never lost it.
func TestOpenExistingRefusesWhatIsNotThere(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "gone.db")
	if st, err := OpenExisting(missing); err == nil {
		st.Close()
		t.Fatal("OpenExisting created a database where there was none")
	} else if !strings.Contains(err.Error(), "no database") {
		t.Fatalf("the error does not name the mistake: %v", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("OpenExisting left a file behind at a path that had none")
	}

	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := OpenExisting(empty); err == nil {
		st.Close()
		t.Fatal("OpenExisting built a database in an empty file")
	}
	if fi, err := os.Stat(empty); err != nil || fi.Size() != 0 {
		t.Fatalf("the empty file was written to (size %v, err %v)", fi.Size(), err)
	}

	// The file this door exists for: the key file that lives beside the
	// database, one tab-completion away from it. The daemon would rename it and
	// build a store under its name; a command an operator typed must not.
	key := filepath.Join(dir, "pingularity.key")
	keyBytes := []byte("0123456789abcdef0123456789abcdef")
	if err := os.WriteFile(key, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := OpenExisting(key); err == nil {
		st.Close()
		t.Fatal("OpenExisting opened a file that is not a database")
	} else if !strings.Contains(err.Error(), key) {
		t.Fatalf("the error does not name the file it refused: %v", err)
	}
	if got, _ := os.ReadFile(key); !bytes.Equal(got, keyBytes) {
		t.Fatal("the key file was rewritten")
	}
	if matches, _ := filepath.Glob(key + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the key file was set aside as a corrupt database: %v", matches)
	}

	// A torn database: the daemon sets this aside and rebuilds, a recovery
	// command must leave it exactly where it stands.
	torn := filepath.Join(dir, "torn.db")
	st, err := Open(torn)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	tearPastHeader(t, torn)
	before, err := os.ReadFile(torn)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := OpenExisting(torn); err == nil {
		st.Close()
		t.Fatal("OpenExisting opened a database it should not have been able to read")
	}
	if matches, _ := filepath.Glob(torn + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("OpenExisting set the database aside (%v)", matches)
	}
	if after, _ := os.ReadFile(torn); !bytes.Equal(after, before) {
		t.Fatal("OpenExisting rewrote the database it could not open")
	}

	// And it still opens a healthy one.
	good := filepath.Join(dir, "good.db")
	st, err = Open(good)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = OpenExisting(good)
	if err != nil {
		t.Fatalf("OpenExisting refused a healthy database: %v", err)
	}
	defer st.Close()
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatalf("store from OpenExisting is not usable: %v", err)
	}
}
