//go:build !windows

package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// realDBBehind builds a database at target and leaves a symlink at link
// pointing to it - the ordinary "the database moved to the bigger disk and a
// link stayed where the unit file points" arrangement.
func realDBBehind(t *testing.T, target, link, key, value string) {
	t.Helper()
	st, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), key, value); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// settingOf reads one settings row back out of a store.
func settingOf(t *testing.T, st *Store, key string) string {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return all[key]
}

// A -db path that is a symlink to a real database is an install, not a mistake:
// it opens the database the link names, keeps its history, and leaves the link
// exactly where it stood.
func TestOpenFollowsASymlinkToADatabase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	link := filepath.Join(dir, "p.db")
	realDBBehind(t, target, link, "auth_user", "admin")

	st, err := Open(link)
	if err != nil {
		t.Fatalf("Open refused a symlink to a real database: %v", err)
	}
	defer st.Close()
	if got := settingOf(t, st, "auth_user"); got != "admin" {
		t.Fatalf("the store opened through the link does not hold the database's rows: auth_user = %q", got)
	}
	if err := st.SetSetting(context.Background(), "auth_user", "someone"); err != nil {
		t.Fatalf("the store opened through the link is not writable: %v", err)
	}
	if lfi, err := os.Lstat(link); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink at the -db path is gone or replaced (%v)", err)
	}
	if fi, err := os.Stat(target); err != nil || fi.Size() == 0 {
		t.Fatalf("the database the link names was not written through (%v)", err)
	}
	// The WAL belongs to the file that is being written, which is the target.
	if _, err := os.Lstat(link + "-wal"); err == nil {
		t.Fatal("a WAL was created beside the link instead of beside the database it names")
	}
}

// The set-aside is the daemon's escape from a crash loop, and on a linked path
// it has to move the DATABASE. Renaming the link instead leaves the damaged
// file in place under its own name, the arrangement in pieces, and a brand-new
// database sitting where the link stood.
func TestOpenSetsAsideTheDatabaseALinkNames(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	link := filepath.Join(dir, "p.db")
	realDBBehind(t, target, link, "auth_user", "admin")
	tearPastHeader(t, target)

	// Unasked, the damaged database is refused where it stands, and the refusal
	// names the file behind the link: its recovery commands read that file and
	// write the copy beside it, so the copy goes back in that file's place and
	// the link goes on naming it. A message naming the link would have the copy
	// put where the link is, and the link replaced by it.
	if st, err := Open(link); err == nil {
		st.Close()
		t.Fatal("Open started on a damaged database behind a link that nobody asked it to replace")
	} else if resolved, _ := filepath.EvalSymlinks(target); !strings.Contains(err.Error(), "database "+resolved+" is damaged") {
		t.Fatalf("the refusal does not name the file behind the link (%s): %v", resolved, err)
	}
	if lfi, err := os.Lstat(link); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the refusal replaced the symlink at the -db path (%v)", err)
	}

	st, err := Open(link, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open did not recover a torn database behind a link: %v", err)
	}
	defer st.Close()
	if matches, _ := filepath.Glob(link + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the link was set aside as the corrupt database: %v", matches)
	}
	matches, _ := filepath.Glob(target + ".*.corrupt")
	if len(matches) != 1 {
		t.Fatalf("the torn database was not set aside beside itself: %v", matches)
	}
	if lfi, err := os.Lstat(link); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink at the -db path is gone or replaced (%v)", err)
	}
	// The rebuilt store is the one the link names, so the install keeps working.
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	back, err := Open(link)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	if got := settingOf(t, back, "k"); got != "v" {
		t.Fatalf("the link does not name the rebuilt store: k = %q", got)
	}
}

// The -db path is resolved once, and everything after that is about the file it
// resolved to. A link repointed afterwards cannot redirect the writes of a
// store that is already open.
func TestOpenHoldsTheFileItResolvedTo(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	link := filepath.Join(dir, "p.db")
	realDBBehind(t, target, link, "auth_user", "admin")
	decoy := filepath.Join(dir, "decoy.db")
	st, err := Open(decoy)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(decoy, link); err != nil {
		t.Fatal(err)
	}
	// Enough writes to make the pool hand out more than the connection the
	// first statement used.
	for _, v := range []string{"a", "b", "c", "d", "e"} {
		if err := st.SetSetting(context.Background(), "k", v); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	moved, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer moved.Close()
	if got := settingOf(t, moved, "k"); got != "e" {
		t.Fatalf("the writes did not land in the file the -db path resolved to: k = %q", got)
	}
	other, err := Open(decoy)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got := settingOf(t, other, "k"); got != "" {
		t.Fatalf("a link repointed after the open redirected the writes: decoy k = %q", got)
	}
}

// What a link may NOT lead to. Each of these is refused with a message naming
// what was found, and nothing at either end of the link is created, renamed or
// re-permissioned.
func TestOpenRefusesALinkToWhatIsNotADatabaseFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("directory", func(t *testing.T) {
		d := filepath.Join(dir, "adir")
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "todir.db")
		if err := os.Symlink(d, link); err != nil {
			t.Fatal(err)
		}
		st, err := Open(link)
		if err == nil {
			st.Close()
			t.Fatal("Open succeeded through a link to a directory")
		}
		if !strings.Contains(err.Error(), "directory") {
			t.Fatalf("the error does not name the mistake: %v", err)
		}
		if fi, serr := os.Stat(d); serr != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("the directory behind the link was re-permissioned (%v, %v)", fi, serr)
		}
	})

	t.Run("device", func(t *testing.T) {
		link := filepath.Join(dir, "todev.db")
		if err := os.Symlink("/dev/null", link); err != nil {
			t.Fatal(err)
		}
		st, err := Open(link)
		if err == nil {
			st.Close()
			t.Fatal("Open succeeded through a link to a device node")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("the error does not name the mistake: %v", err)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		fifo := filepath.Join(dir, "afifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unavailable here: %v", err)
		}
		link := filepath.Join(dir, "tofifo.db")
		if err := os.Symlink(fifo, link); err != nil {
			t.Fatal(err)
		}
		st, err := Open(link)
		if err == nil {
			st.Close()
			t.Fatal("Open succeeded through a link to a fifo")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("the error does not name the mistake: %v", err)
		}
	})

	t.Run("dangling", func(t *testing.T) {
		gone := filepath.Join(dir, "notmounted", "p.db")
		link := filepath.Join(dir, "dangling.db")
		if err := os.Symlink(gone, link); err != nil {
			t.Fatal(err)
		}
		st, err := Open(link)
		if err == nil {
			st.Close()
			t.Fatal("Open created a database at the far end of a link that leads nowhere")
		}
		if !strings.Contains(err.Error(), link) || !strings.Contains(err.Error(), "names no file") {
			t.Fatalf("the error does not say which link led nowhere: %v", err)
		}
		if _, serr := os.Stat(gone); !os.IsNotExist(serr) {
			t.Fatalf("a database was built where the link pointed (%v)", serr)
		}
		if lfi, lerr := os.Lstat(link); lerr != nil || lfi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the link was replaced (%v)", lerr)
		}
	})

	t.Run("loop", func(t *testing.T) {
		a := filepath.Join(dir, "loopa.db")
		b := filepath.Join(dir, "loopb.db")
		if err := os.Symlink(b, a); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(a, b); err != nil {
			t.Fatal(err)
		}
		st, err := Open(a)
		if err == nil {
			st.Close()
			t.Fatal("Open followed a symlink loop")
		}
		if !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), "names no file") {
			t.Fatalf("the error does not say which link led nowhere: %v", err)
		}
	})
}

// A rebuild behind a link has the same one step between setting the damaged
// database aside and putting the store it finished in its place, and a start can
// die across it the same way. What that leaves is a link to nothing - the file it
// names has just been moved - with the finished store beside where that file
// was. The next start finishes the rebuild at the far end rather than refuse the
// link, and the link is still a link. A link to nothing with no finished store
// beside its far end is still refused, and nothing is made at either end: that is
// the volume that is not mounted yet.
func TestAnInterruptedRebuildBehindALinkIsFinishedByTheNextOpen(t *testing.T) {
	ctx := context.Background()
	refuse := func(string, string) error { return errors.New("the rename was refused") }
	defer func() { renameIntoPlaceFn = os.Rename }()

	for name, linkTo := range map[string]func(t *testing.T, dir string) string{
		"absolute link": func(_ *testing.T, dir string) string { return filepath.Join(dir, "moved", "elsewhere.db") },
		"relative link": func(*testing.T, string) string { return filepath.Join("moved", "elsewhere.db") },
		// A link to a link: the first hop still exists, so the finished store is
		// not beside it - it is beside the file at the end of the chain.
		"link to a link": func(t *testing.T, dir string) string {
			mid := filepath.Join(dir, "mid.db")
			if err := os.Symlink(filepath.Join("moved", "elsewhere.db"), mid); err != nil {
				t.Fatal(err)
			}
			return mid
		},
		// The same chain with its middle link in another directory, relative on
		// both hops: each hop is read from the directory of the link it came from.
		"relative links through another directory": func(t *testing.T, dir string) string {
			if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "moved", "elsewhere.db"), filepath.Join(dir, "sub", "mid.db")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("sub", "mid.db")
		},
		// Its middle link reached through a directory link, with ".." in the target:
		// the kernel climbs out of the directory the link really sits in.
		"relative link behind a directory link": func(t *testing.T, dir string) string {
			deep := filepath.Join(dir, "other", "deep")
			if err := os.MkdirAll(deep, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("other", "deep"), filepath.Join(dir, "sublink")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("..", "..", "moved", "elsewhere.db"), filepath.Join(deep, "mid.db")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("sublink", "mid.db")
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "moved"), 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(dir, "moved", "elsewhere.db")
			link := filepath.Join(dir, "p.db")
			st, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetSetting(ctx, "k", "v"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			if err := os.Symlink(linkTo(t, dir), link); err != nil {
				t.Fatal(err)
			}
			tearTableRoot(t, target, "pauses")

			renameIntoPlaceFn = refuse
			st, err = Open(link, RebuildOnCorruption())
			renameIntoPlaceFn = os.Rename
			if err == nil {
				st.Close()
				t.Fatal("a rebuild whose store could not be put in place started anyway")
			}
			if _, err := os.Lstat(target + rebuiltSuffix); err != nil {
				t.Fatalf("fixture: the cut-short rebuild left no finished store beside the file the link names (%v)", err)
			}

			st, err = Open(link)
			if err != nil {
				t.Fatalf("the next start refused the link its own rebuild had left leading nowhere, with the finished store beside it: %v", err)
			}
			defer st.Close()
			if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || !held {
				t.Fatalf("the next start did not open the store the rebuild finished (held=%v, err=%v): -access network puts any other store on the network with no login", held, err)
			}
			if lfi, err := os.Lstat(link); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the symlink at the -db path is gone or replaced (%v)", err)
			}
			if _, err := os.Lstat(target + rebuiltSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Error("the finished store is still sitting beside the file the link names")
			}
		})
	}

	for name, beside := range map[string]func(far string) error{
		"nothing beside it":     func(string) error { return nil },
		"a directory beside it": func(far string) error { return os.MkdirAll(far+rebuiltSuffix, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			far := filepath.Join(dir, "elsewhere.db")
			link := filepath.Join(dir, "p.db")
			if err := os.Symlink(far, link); err != nil {
				t.Fatal(err)
			}
			if err := beside(far); err != nil {
				t.Fatal(err)
			}
			st, err := Open(link)
			if err == nil {
				st.Close()
				t.Fatal("Open created a database at the far end of a link that leads nowhere, with no finished rebuild beside it")
			}
			if !strings.Contains(err.Error(), "names no file") {
				t.Fatalf("the error does not say the link led nowhere: %v", err)
			}
			if _, err := os.Lstat(far); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("something was made where the link points (%v)", err)
			}
		})
	}

	// A chain with nothing beside its far end is still the volume that is not
	// mounted yet, however many hops lead there.
	t.Run("a chain with nothing beside its far end", func(t *testing.T) {
		dir := t.TempDir()
		far := filepath.Join(dir, "elsewhere.db")
		mid := filepath.Join(dir, "mid.db")
		link := filepath.Join(dir, "p.db")
		if err := os.Symlink(far, mid); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(mid, link); err != nil {
			t.Fatal(err)
		}
		st, err := Open(link)
		if err == nil {
			st.Close()
			t.Fatal("Open created a database at the far end of a chain of links that leads nowhere")
		}
		if !strings.Contains(err.Error(), "names no file") {
			t.Fatalf("the error does not say the link led nowhere: %v", err)
		}
		if _, err := os.Lstat(far); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("something was made where the chain points (%v)", err)
		}
	})
}

// A link into a directory nothing may write is a broken arrangement, not a
// corrupt database: it fails fast and sets nothing aside.
func TestOpenThroughALinkIntoAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through any mode")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(locked, "elsewhere.db")
	link := filepath.Join(dir, "p.db")
	realDBBehind(t, target, link, "auth_user", "admin")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	st, err := Open(link)
	if err == nil {
		st.Close()
		t.Skip("this filesystem let the WAL be created in a read-only directory")
	}
	if matches, _ := filepath.Glob(target + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("a database that only could not be written to was set aside: %v", matches)
	}
	if after, _ := os.ReadFile(target); !bytes.Equal(after, before) {
		t.Fatal("the database behind the link was rewritten")
	}
}

// OpenExisting is what `pingularity reset-auth` uses, and the lockout it exists
// to end can happen on a linked path like any other.
func TestOpenExistingFollowsASymlinkToADatabase(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere.db")
	link := filepath.Join(dir, "p.db")
	realDBBehind(t, target, link, "auth_user", "admin")

	st, err := OpenExisting(link)
	if err != nil {
		t.Fatalf("OpenExisting refused a symlink to a real database: %v", err)
	}
	defer st.Close()
	if got := settingOf(t, st, "auth_user"); got != "admin" {
		t.Fatalf("the store opened through the link does not hold the database's rows: auth_user = %q", got)
	}
}

// A sidecar that is a link is still refused - SQLite opens the write-ahead log
// and the shared-memory file beside the database directly and gets no further
// than "unable to open database file" through a link, on this build and on the
// one before it. What has to be right is the remedy the message names: there is
// no way to point -db at a write-ahead log.
func TestOpenRefusesASymlinkedSidecarWithAdviceThatCanBeFollowed(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "p.db")
	st, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	elsewhere := filepath.Join(dir, "elsewhere-wal")
	if err := os.WriteFile(elsewhere, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, dbPath+"-wal"); err != nil {
		t.Fatal(err)
	}

	st, err = Open(dbPath)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded with a symlinked write-ahead log beside the database")
	}
	if !strings.Contains(err.Error(), dbPath+"-wal") {
		t.Fatalf("the error does not name the sidecar it refused: %v", err)
	}
	if strings.Contains(err.Error(), "points to") || strings.Contains(err.Error(), "-db path") {
		t.Fatalf("the error tells the operator to point -db at a write-ahead log, which is not a thing to do: %v", err)
	}
}

// Only the final component of the -db path is resolved. A symlinked parent
// directory - a data directory moved with a link left at the old name - is not
// the arrangement any of this is about, and goes on working untouched.
func TestOpenThroughASymlinkedParentDirectory(t *testing.T) {
	dir := t.TempDir()
	realDir := filepath.Join(dir, "realvolume")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(dir, "datadir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(linkDir, "p.db")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open refused a database inside a symlinked directory: %v", err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := os.Stat(filepath.Join(realDir, "p.db")); err != nil {
		t.Fatalf("the database was not created in the directory the link names (%v)", err)
	}
	if lfi, err := os.Lstat(linkDir); err != nil || lfi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the directory link was replaced (%v)", err)
	}
	// And a path whose final component is not a link is reported back the way
	// it was typed: an operator reading the message has to recognise the path
	// in their unit file, not a rewritten one.
	mistyped := filepath.Join(linkDir, "adir")
	if err := os.Mkdir(mistyped, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err = Open(mistyped)
	if err == nil {
		st.Close()
		t.Fatal("Open succeeded on a directory")
	}
	if !strings.Contains(err.Error(), mistyped) {
		t.Fatalf("the error renamed the path the operator gave: %v", err)
	}
}
