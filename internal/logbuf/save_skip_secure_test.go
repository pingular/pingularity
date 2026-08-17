//go:build unix

package logbuf

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ctimeOf reports path's inode change time. A chmod advances it even when it
// sets the mode the file already has, which makes it the one thing a test can
// observe about a securing call that changed nothing else: the file's bytes,
// length, mode and modification time all stay exactly as they were.
//
// golang.org/x/sys/unix rather than the standard syscall package because its
// Stat_t names the field Ctim on every unix (syscall calls it Ctimespec on
// darwin and Ctim on linux, so the standard package cannot be read portably);
// the repo already depends on x/sys and uses unix elsewhere
// (internal/netstat/bytes_darwin.go:9, internal/netinfo/trace_linux.go:11).
func ctimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		t.Fatalf("stat %s for its change time: %v", path, err)
	}
	return time.Unix(st.Ctim.Unix())
}

// The skip at logbuf.go must re-apply the snapshot's protection even when it
// declines to rewrite the file, because on Windows the mode it compares cannot
// report a loosened DACL: os.Lstat derives the permission bits there from
// FILE_ATTRIBUTE_READONLY alone, and SaveFile's 0600 open never sets that
// attribute, so both sides of that comparison are the same constant whatever the
// DACL says. osperm exposes nothing that can read a DACL back either -
// GroupOrWorldAccessible reports known=false on Windows
// (internal/osperm/perm_windows.go:27-30) - so the skip cannot detect the
// loosening and must simply re-secure. Before the skip existed the every-minute
// rewrite re-ran osperm.SecureFile and repaired a loosened DACL within a minute.
//
// What this asserts on a unix host is the securing call itself, which is the
// half that is shared with Windows: the change time advances (chmod ran) while
// the inode and the modification time hold (nothing was rewritten). The
// optimisation has to survive - on this build tag the skip must still cost one
// chmod instead of a file creation, a write, that same chmod on the temp file,
// an fsync of the file, an fsync of its directory and a rename - so both halves
// are asserted together. Those counts are the unix ones and do not carry to
// Windows, where SecureFile is a token lookup, an SDDL parse and a
// SetNamedSecurityInfo, and the rewrite it replaces fsyncs the file but not the
// directory; the per-platform accounting is in logbuf.go beside the securing
// call. A test that watched the DACL itself would need a reader for it in
// osperm and a Windows host; TestSaveFileRepairsALoosenedSnapshotMode covers the
// unix-only mode comparison that makes the OTHER path (rewrite) fire.
func TestSaveFileResecuresOnTheSkipPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.txt")
	r := New(2000)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Guard the premise: the assertion below reads a chmod out of the change
	// time, so first check that this filesystem records one at all. If its
	// timestamps are too coarse to separate two calls, the test would report a
	// missing securing call that did happen.
	control := ctimeOf(t, path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("control chmod: %v", err)
	}
	if !ctimeOf(t, path).After(control) {
		t.Skip("this filesystem's change time does not separate two chmods, so a securing call cannot be observed here")
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first save: %v", err)
	}
	markSaved(t, path)
	beforeCtime := ctimeOf(t, path)

	// One tick of the daemon's one-minute ticker with nothing logged since
	// (main.go:462). The ring and the file are both untouched, so this is the
	// skip.
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("idle save: %v", err)
	}

	if !ctimeOf(t, path).After(beforeCtime) {
		t.Error("SaveFile skipped without re-securing the snapshot, so a protection loosened behind " +
			"its back stands for as long as the ring stays idle - which on a default install is its normal state")
	}
	// The optimisation itself: skipping still means no rewrite, so the file keeps
	// its inode and its modification time. On this build tag the securing call is
	// one chmod (internal/osperm/perm_unix.go:11); the rewrite it replaces is a
	// file creation, a write, that same chmod on the temp file, two fsyncs (the
	// file and its directory) and a rename. Windows counts differently - see the
	// header.
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after idle save: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("SaveFile replaced the snapshot file although the ring had not changed")
	}
	if rewritten(t, path) {
		t.Error("SaveFile rewrote the snapshot although the ring had not changed")
	}
	if got := readEntries(t, path); len(got) != 1 || got[0].Raw != "probe 203.0.113.7 ok" {
		t.Fatalf("snapshot holds %#v, want the one buffered line", got)
	}
}

// The securing call above must stay BELOW the mode comparison. os.Lstat reports
// the LINK, so a snapshot swapped for a symlink fails that comparison and takes
// the rewrite path instead - and that is the only thing keeping the securing
// call off the link's target. Unix SecureFile is os.Chmod
// (internal/osperm/perm_unix.go:11) and chmod follows a symlink, so securing
// before the comparison declines would let whoever plants the link choose which
// file the daemon chmods 0600.
//
// TestSaveFileRewritesASymlinkedSnapshot reads as though it covers this and does
// not: it asserts the target's BYTES, and a chmod rewrites no bytes. Measured
// with the securing call lifted above the mode comparison, that test passes
// while the target comes back 0600. This test watches the target's MODE, which
// is the only thing that moves.
func TestSaveFileNeverSecuresThroughASymlinkedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	// A file that is none of the daemon's business, standing in for whatever the
	// planted link is aimed at.
	bystander := filepath.Join(dir, "bystander.txt")

	r := New(2000)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// 0644, not 0600: SecureFile sets 0600, so a target that already held it
	// would leave the chmod under test with nothing to change. Chmod after
	// writing, so a restrictive umask cannot take the mode below 0644 either.
	if err := os.WriteFile(bystander, []byte("not the daemon's file\n"), 0o644); err != nil {
		t.Fatalf("write the bystander file: %v", err)
	}
	if err := os.Chmod(bystander, 0o644); err != nil {
		t.Fatalf("chmod the bystander file: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Symlink(bystander, path); err != nil {
		t.Fatalf("symlink over the snapshot path: %v", err)
	}

	// Nothing was logged in between, so this call reaches the skip's memo branch:
	// the path, the epoch and the sequence sum all still match, and the Lstat'd
	// mode is what declines the skip.
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("save over the symlink: %v", err)
	}

	if fi, err := os.Lstat(bystander); err != nil {
		t.Fatalf("lstat the bystander file: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("the symlink's target was chmodded to %04o: the securing call ran before the mode "+
			"comparison declined the skip, so whoever plants the link chooses which file the daemon "+
			"chmods 0600", perm)
	}
	// Guard the premise: the mode above also survives a SaveFile that did nothing
	// at all, so check the rewrite this test assumes actually ran.
	li, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat the snapshot: %v", err)
	}
	if !li.Mode().IsRegular() {
		t.Fatalf("snapshot is %v after the save, so the rewrite did not run and the bystander's "+
			"mode proves nothing about the securing call's placement", li.Mode())
	}
}
