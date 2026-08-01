package logbuf

import (
	"os"
	"path/filepath"
	"runtime"
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
// What the test can observe is Unix-side, and only because a permissive scratch
// file is planted first: O_CREATE keeps an existing file's mode, so the 0600 in
// SaveFile's open cannot repair the leftover and only the securing call brings
// the snapshot to owner-only - remove that call and this fails. The Windows half
// of the guarantee (the DACL) is not asserted here; it rests on both platforms
// sharing the one securing call whose effect this test proves.
func TestSavedSnapshotIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	// A permissive parent, which is the case that matters: on Windows the file
	// would inherit these, and on Unix a wrong mode would show up the same way.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "logs.txt")

	// The scratch file a crashed save left behind, sitting at a permissive mode.
	// SaveFile reuses the fixed path+".tmp" name, and opening an existing file
	// keeps its mode whatever O_CREATE asks for - so without the securing call
	// the snapshot ships at 0644 under its final name. Chmod after writing, so a
	// restrictive umask cannot quietly tighten the setup and blunt the assertion.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("torn previous save\n"), 0o644); err != nil {
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
	if _, err := os.Stat(tmp); err == nil {
		t.Error("the temp file was left behind")
	}
}
