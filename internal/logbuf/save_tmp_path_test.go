//go:build unix

package logbuf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// SaveFile's scratch file must never be a name someone else can pre-empt.
//
// It used to be path+".tmp", opened O_WRONLY|O_CREATE|O_TRUNC, which follows a
// symlink: whoever could create that one predictable name in the data directory
// chose which file the daemon truncated, wrote the raw log lines into, and
// chmodded 0600 - and the rename then published the LINK as logs.txt, which
// LoadFile refuses on the next start, losing the viewer's history for good. No
// race needed: the name was fixed, so a link planted once was followed on the
// very next save. SaveFile's create has the rest of the reasoning, including why
// the data directory is not always ours alone.
func TestSaveFileNeverWritesThroughAPlantedTempPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")

	// A file that is none of the daemon's business, standing in for whatever the
	// link is aimed at. Chmod after writing so a restrictive umask cannot
	// pre-tighten it and blunt the mode assertion below.
	const bystanderBody = "not the daemon's business\n"
	bystander := filepath.Join(dir, "bystander.txt")
	if err := os.WriteFile(bystander, []byte(bystanderBody), 0o644); err != nil {
		t.Fatalf("write the bystander file: %v", err)
	}
	if err := os.Chmod(bystander, 0o644); err != nil {
		t.Fatalf("chmod the bystander file: %v", err)
	}
	// The whole attack: one symlink at the name the save path used to derive.
	if err := os.Symlink(bystander, path+".tmp"); err != nil {
		t.Fatalf("plant the link at the scratch name: %v", err)
	}

	r := New(16)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	if b, err := os.ReadFile(bystander); err != nil {
		t.Fatalf("read the bystander file: %v", err)
	} else if string(b) != bystanderBody {
		t.Errorf("the planted link's target now holds %q, want %q: the save opened its scratch "+
			"file by a predictable name with O_TRUNC and followed the link, so anyone who can "+
			"create that name in the data directory picks which file the daemon overwrites",
			string(b), bystanderBody)
	}
	if fi, err := os.Lstat(bystander); err != nil {
		t.Fatalf("lstat the bystander file: %v", err)
	} else if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Errorf("the planted link's target was chmodded to %04o, want 0644: osperm.SecureFile ran "+
			"on the scratch file by name and chmod follows a symlink, so the same plant also picks "+
			"which file the daemon locks to 0600", perm)
	}

	// The save must still have done its job: refusing the plant is only a fix if
	// the snapshot is written anyway, as a regular file - a symlink here means the
	// rename published the link, which LoadFile then refuses outright.
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat the snapshot: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("the snapshot is %v, want a regular file: the scratch file was the planted link, "+
			"so the rename published it and LoadFile will refuse this path on the next start",
			fi.Mode())
	}
	got := New(16)
	if err := got.LoadFile(path); err != nil {
		t.Fatalf("LoadFile the snapshot back: %v", err)
	}
	if e := got.Entries(); len(e) != 1 || e[0].Raw != "probe 203.0.113.7 ok" {
		t.Errorf("the snapshot round-tripped to %v, want the one appended line: the save wrote "+
			"somewhere other than the snapshot", e)
	}
}

// An ordinary every-minute save must leave the data directory as it found it: the
// old fixed scratch name was self-cleaning (the next tick's O_TRUNC reused it), a
// random one is not. This drives the success path only, where os.Rename consumes
// the temp whatever SaveFile does about it, so it pins none of the cleanup - the
// test below does that.
//
// Saves that change nothing return at the skip and never create a temp at all,
// so the ring is appended to between calls to make each one take the write path.
func TestSaveFileLeavesNoScratchFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	r := New(16)
	for i := 0; i < 8; i++ {
		r.Append("probe ok", "probe ok")
		if err := r.SaveFile(path); err != nil {
			t.Fatalf("SaveFile %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("scratch file %s left in the data directory: a save that leaves its temp "+
				"behind litters the data directory with one more file every minute, forever",
				e.Name())
		}
	}
}

// A save that fails AFTER its scratch file exists must still take that file with
// it. SaveFile runs on a one-minute ticker, so a persistent fault - a full disk on
// the flush, an EPERM from the securing call - would otherwise strand one more
// logs.txt.tmp-NNNNNNN in the data directory every minute for as long as it lasts.
//
// A directory standing at the snapshot's final name is the one failure this
// package can force without a seam for injecting I/O errors: every step up to the
// publish succeeds, and a rename cannot put a file where a directory already is
// (the Fatalf below says so if some filesystem allows it). That reaches the bare
// os.Remove after os.Rename; the `fail` closure's legs need a filesystem that can
// be made to fail mid-write, so this test does not claim them.
func TestSaveFileRemovesItsScratchFileWhenTheSaveFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("plant a directory at the snapshot path: %v", err)
	}

	r := New(16)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err == nil {
		t.Fatalf("SaveFile reported success renaming its scratch file over a directory, so the " +
			"failing save this test needs never happened")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data dir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("scratch file %s survived a failed save: the randomised name is not "+
				"self-cleaning the way the old fixed one was, so a persistent fault on the "+
				"one-minute ticker strands one more file in the data directory every minute, "+
				"forever", e.Name())
		}
	}
}

// The securing call on the scratch file has to RUN, and os.CreateTemp hides it: it
// creates at 0600 already, so on Unix deleting that call changes nothing an
// ordinary save can show. Masking the owner-write bit off the create buys the
// observation back - CreateTemp only ASKS for 0600 and open(2) subtracts the
// umask, so the scratch file is born 0400 and only the securing call can bring the
// published snapshot to exactly 0600. The mode is a proxy for what matters, that
// the call runs at all: on Windows it is the only protection there is, and with no
// umask to lean on TestSaveFileSecuresTheScratchFileBeforePublishingIt reads the
// source there instead.
//
// umask is process-wide: the directory is made before it changes (0400 would leave
// a temp dir this test cannot write into) and restored as the test returns.
// Nothing in this package calls t.Parallel.
func TestSaveFileSecuresTheScratchFileAgainstTheUmask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")

	old := unix.Umask(0o200)
	defer unix.Umask(old)

	// Guard the premise: the assertion below reads the securing call out of a bit
	// the umask would otherwise have taken, so check first that this filesystem
	// subtracts the umask at all. Where it does not, the snapshot would come back
	// 0600 either way and the test would pass while pinning nothing.
	control, err := os.CreateTemp(dir, "umask-control-*")
	if err != nil {
		t.Fatalf("create the control file: %v", err)
	}
	controlPath := control.Name()
	control.Close()
	ci, err := os.Stat(controlPath)
	if err != nil {
		t.Fatalf("stat the control file: %v", err)
	}
	if err := os.Remove(controlPath); err != nil {
		t.Fatalf("remove the control file: %v", err)
	}
	if perm := ci.Mode().Perm(); perm != 0o400 {
		t.Skipf("os.CreateTemp came out %04o under a 0200 umask, so this filesystem does not "+
			"subtract it and the securing call cannot be observed here", perm)
	}

	r := New(16)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the snapshot: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("the snapshot is %04o, want 0600: os.CreateTemp only asks for 0600 and the umask "+
			"took the owner-write bit out of it, so osperm.SecureFile is the only thing left that "+
			"can pin the mode - and on Windows that call is the only thing that writes the "+
			"snapshot's own DACL at all, over inherited ACEs that grant BUILTIN\\Users read of "+
			"the unmasked log lines", perm)
	}
}
