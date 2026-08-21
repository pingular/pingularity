package logbuf

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The saved log snapshot holds the RAW lines - unmasked IPs and hostnames - so it
// must carry owner-only permission of its own rather than whatever it inherits.
//
// os.OpenFile's mode argument is honoured on Unix and ignored on Windows, where a
// new file simply takes the parent directory's ACEs. A supported layout puts this
// file beside the database in C:\ProgramData, whose inherited ACEs grant
// BUILTIN\Users read - so on the one platform where the mode is silently dropped,
// the snapshot was readable by any local account. osperm.SecureFile is the
// existing answer to exactly that (secret.go uses it for the key file); this
// checks the snapshot goes through it too.
//
// The permissive leftover planted below no longer makes that call observable: the
// save now creates its own scratch file with os.CreateTemp, already 0600, so
// deleting the securing call leaves the assertion here green. What the leftover
// pins instead is the stronger property that replaced it - a 0644 file at the old
// fixed scratch name is neither adopted nor followed
// (TestSaveFileNeverWritesThroughAPlantedTempPath explains why that matters).
//
// The securing call's own pin moved rather than being dropped, to
// TestSaveFileSecuresTheScratchFileAgainstTheUmask, and on Windows - where the
// mode is ignored and the DACL cannot be read back - to
// TestSaveFileSecuresTheScratchFileBeforePublishingIt, which reads the source.
func TestSavedSnapshotIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	// A permissive parent, which is the case that matters: on Windows the file
	// would inherit these, and on Unix a wrong mode would show up the same way.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "logs.txt")

	// A permissive file at the name a crashed save used to leave behind, and the
	// name anyone with write access to this directory could plant. Chmod after
	// writing, so a restrictive umask cannot quietly tighten the setup.
	const leftoverBody = "torn previous save\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(leftoverBody), 0o644); err != nil {
		t.Fatalf("plant leftover tmp: %v", err)
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		t.Fatalf("chmod leftover tmp: %v", err)
	}

	r := New(16)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		return // no Unix mode to read; SecureFile writes the DACL instead
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("log snapshot is %04o, want 0600: it carries the unmasked log lines "+
			"(addresses and hostnames), so it must not be readable by other accounts", perm)
	}
	// Ignored outright, not reused: neither the leftover's bytes nor its mode may
	// become the snapshot's, and it stays as it was - it is not this save's file
	// to touch.
	if b, err := os.ReadFile(path); err != nil {
		t.Fatalf("read snapshot: %v", err)
	} else if strings.Contains(string(b), "torn previous save") {
		t.Error("the snapshot contains the planted leftover's bytes: a file standing at the " +
			"predictable scratch name was adopted and published under the final name")
	}
	if b, err := os.ReadFile(tmp); err != nil {
		t.Errorf("read the planted leftover: %v", err)
	} else if string(b) != leftoverBody {
		t.Errorf("the planted leftover now holds %q, want %q: the save wrote into a file it "+
			"did not create", string(b), leftoverBody)
	}
}
