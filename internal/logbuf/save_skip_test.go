package logbuf

import (
	"bytes"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// markerTime is stamped onto a saved snapshot so a later SaveFile that rewrites
// the file can be told from one that skipped it. Comparing inodes would work on
// Unix only, and comparing content cannot detect a rewrite that reproduces the
// same bytes - which is exactly the case under test.
var markerTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// markSaved stamps the snapshot with markerTime. Size is untouched, so the
// snapshot still matches what SaveFile believes it wrote.
func markSaved(t *testing.T, path string) {
	t.Helper()
	if err := os.Chtimes(path, markerTime, markerTime); err != nil {
		t.Fatalf("stamp marker: %v", err)
	}
}

// rewritten reports whether the snapshot has been replaced since markSaved.
func rewritten(t *testing.T, path string) bool {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	return !fi.ModTime().Equal(markerTime)
}

// The daemon snapshots the ring on a one-minute ticker whether or not anything
// was logged (main.go:462), and the default install runs with logging "off"
// (main.go:426, main.go:506-507) - which sets the level to WARN rather than
// silencing it, so that install's ring is idle rather than empty and fills only
// when something goes wrong (applyLogLevel, main.go:2357-2364). What the skip
// turns on is therefore idleness, not emptiness, so both states are covered
// below: rewriting an unchanged snapshot every minute cost 60 file creations an
// hour either way, each one fsyncing the file and (off Windows) its parent
// directory, for content that never changed.
func TestSaveFileSkipsUnchangedRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.txt")

	for _, tc := range []struct {
		name  string
		lines int
	}{
		{"empty ring (nothing logged yet)", 0},
		{"ring with history, idle", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logs.txt")
			r := New(2000)
			for i := 0; i < tc.lines; i++ {
				r.Append("raw line", "masked line")
			}
			if err := r.SaveFile(path); err != nil {
				t.Fatalf("first save: %v", err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat after first save: %v", err)
			}
			markSaved(t, path)

			// An hour of the daemon's ticker with nothing logged in between.
			for i := 0; i < 60; i++ {
				if err := r.SaveFile(path); err != nil {
					t.Fatalf("save %d: %v", i+1, err)
				}
			}
			if rewritten(t, path) {
				t.Error("SaveFile rewrote the snapshot although the ring had not changed")
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat after idle saves: %v", err)
			}
			if !os.SameFile(before, after) {
				t.Error("SaveFile replaced the snapshot file although the ring had not changed")
			}
		})
	}

	// A single appended line must still reach disk: skipping is only ever allowed
	// when the ring is byte-for-byte what was already written.
	r := New(2000)
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}
	markSaved(t, path)
	r.Append("something happened", "something happened")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("save after append: %v", err)
	}
	if !rewritten(t, path) {
		t.Fatal("SaveFile skipped the write after a line was appended")
	}
	if got := readEntries(t, path); len(got) != 1 || got[0].Raw != "something happened" {
		t.Fatalf("snapshot does not hold the appended line: %#v", got)
	}
}

// Clear is the one content change the sequence sum cannot show, because it moves
// the buffered lines from len(lines) into dropped and leaves the total alone. It
// rotates the epoch instead, and the skip must honour that: main.go re-saves on
// clear precisely so a restart cannot resurrect lines the operator just wiped.
func TestSaveFileWritesAfterClear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs.txt")
	r := New(2000)
	for i := 0; i < 5; i++ {
		r.Append("secret host", "masked host")
	}
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}
	markSaved(t, path)

	r.Clear()
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("save after clear: %v", err)
	}
	if !rewritten(t, path) {
		t.Fatal("SaveFile skipped the write after Clear, so a restart would resurrect the cleared lines")
	}
	if got := readEntries(t, path); len(got) != 0 {
		t.Fatalf("cleared snapshot still holds %d entries", len(got))
	}
}

// The skip must not survive the snapshot being deleted or truncated by something
// outside this process, or an operator who removes logs.txt to reclaim space
// would find it never comes back.
func TestSaveFileRewritesWhenSnapshotIsGone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		disturb func(t *testing.T, path string)
	}{
		{"deleted", func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatalf("remove snapshot: %v", err)
			}
		}},
		{"truncated", func(t *testing.T, path string) {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatalf("truncate snapshot: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logs.txt")
			r := New(2000)
			r.Append("keep me", "keep me")
			if err := r.SaveFile(path); err != nil {
				t.Fatalf("first save: %v", err)
			}

			tc.disturb(t, path)

			// The ring has not changed, so only the state of the file on disk can
			// make this call write.
			if err := r.SaveFile(path); err != nil {
				t.Fatalf("save after %s: %v", tc.name, err)
			}
			got := readEntries(t, path)
			if len(got) != 1 || got[0].Raw != "keep me" {
				t.Fatalf("snapshot was not restored after being %s: %#v", tc.name, got)
			}
		})
	}
}

// A snapshot swapped for a symlink must be rewritten back into a regular file.
// This is the one case where the skip's stat call has to disagree with what a
// reader would see through the path: os.Stat FOLLOWS the link and reports the
// target's size and mode, so a link pointed at a file that matches both would
// satisfy the skip and be left in place - and LoadFile refuses a non-regular
// path outright (logbuf.go, "not a regular file"), so the viewer's history would
// be gone for good instead of for the one minute the pre-skip code took to
// rebuild it. os.Lstat reports the link itself, whose mode never equals the
// saved regular-file mode.
func TestSaveFileRewritesASymlinkedSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege the test runner may not hold")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	target := filepath.Join(dir, "elsewhere.txt")

	r := New(2000)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first save: %v", err)
	}

	// Point the snapshot at a file matching it in the two things the skip checks,
	// so nothing but the path's own type can make the next call write. Chmod after
	// writing, so a restrictive umask cannot make the target differ by mode.
	decoy := bytes.Repeat([]byte("Z"), int(fi.Size()))
	decoy[len(decoy)-1] = '\n'
	if err := os.WriteFile(target, decoy, 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Chmod(target, fi.Mode().Perm()); err != nil {
		t.Fatalf("chmod symlink target: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("symlink over the snapshot path: %v", err)
	}
	// Guard the premise: if the swap did not match size and mode through the link,
	// the skip would decline for a reason that has nothing to do with Lstat.
	through, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat through the symlink: %v", err)
	}
	if through.Size() != fi.Size() || through.Mode() != fi.Mode() {
		t.Fatalf("symlink target does not match the snapshot through the link (size %d/%d, mode %v/%v): "+
			"this test would pass without exercising Lstat at all",
			through.Size(), fi.Size(), through.Mode(), fi.Mode())
	}

	// The ring is untouched, so only the path having become a symlink can make
	// this call write.
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("save over the symlink: %v", err)
	}
	li, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat after save: %v", err)
	}
	if li.Mode()&os.ModeSymlink != 0 {
		t.Error("SaveFile skipped over a symlinked snapshot and left the link in place")
	}
	if !li.Mode().IsRegular() {
		t.Errorf("snapshot is %v after a save, want a regular file", li.Mode())
	}
	// The point of rewriting it: LoadFile must be able to read the history back.
	r2 := New(2000)
	if err := r2.LoadFile(path); err != nil {
		t.Fatalf("LoadFile after the repair save: %v", err)
	}
	if got := readEntries(t, path); len(got) != 1 || got[0].Raw != "probe 203.0.113.7 ok" {
		t.Fatalf("snapshot holds %#v, want the one buffered line", got)
	}
	// The rewrite must replace the LINK, not write through it: the rename swaps the
	// path itself, so the file the link pointed at keeps its own bytes. Writing
	// through would mean a symlink could redirect the unmasked lines anywhere the
	// daemon can write.
	if after, err := os.ReadFile(target); err != nil {
		t.Fatalf("read symlink target: %v", err)
	} else if !bytes.Equal(after, decoy) {
		t.Errorf("the symlink's target was written through: it now holds %q", after)
	}
}

// The memo describes one file, so pointing SaveFile at a different path must
// write rather than assume that path already holds the same snapshot.
func TestSaveFileWritesToADifferentPath(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "logs.txt")
	second := filepath.Join(dir, "logs-elsewhere.txt")
	r := New(2000)
	r.Append("a line", "a line")
	if err := r.SaveFile(first); err != nil {
		t.Fatalf("save to first path: %v", err)
	}
	fi, err := os.Stat(first)
	if err != nil {
		t.Fatalf("stat first path: %v", err)
	}

	// Plant a decoy at the second path that matches the memo in every respect
	// EXCEPT the path: same byte length and same owner-only mode as the snapshot
	// just written. Without it the memo's stat of the second path simply fails
	// (no such file) and the write happens for a reason that has nothing to do
	// with the path field, so dropping the path check from the skip leaves this
	// test green. Chmod after writing, so a restrictive umask cannot make the
	// decoy differ by mode instead.
	stale := bytes.Repeat([]byte("Z"), int(fi.Size()))
	if len(stale) > 0 {
		stale[len(stale)-1] = '\n'
	}
	if err := os.WriteFile(second, stale, 0o600); err != nil {
		t.Fatalf("plant decoy at second path: %v", err)
	}
	if err := os.Chmod(second, fi.Mode().Perm()); err != nil {
		t.Fatalf("chmod decoy: %v", err)
	}

	if err := r.SaveFile(second); err != nil {
		t.Fatalf("save to second path: %v", err)
	}
	onDisk, err := os.ReadFile(second)
	if err != nil {
		t.Fatalf("read second path: %v", err)
	}
	if bytes.Equal(onDisk, stale) {
		t.Fatalf("second path still holds the decoy bytes %q: the memo for %s was applied to %s",
			stale, first, second)
	}
	if got := readEntries(t, second); len(got) != 1 || got[0].Raw != "a line" {
		t.Fatalf("second path holds %#v, want the one buffered line", got)
	}
}

// A snapshot whose mode was loosened must be repaired rather than skipped over.
// The file holds the RAW unmasked lines, which is why it is owner-only in the
// first place (SaveFile's doc comment, TestSavedSnapshotIsOwnerOnly), and the
// only thing that puts a loosened mode back is a rewrite through
// osperm.SecureFile. Before the skip existed every tick rebuilt the file, so a
// chmod healed within a minute; a skip that compared only size would instead
// leave the file readable by other accounts for as long as the ring stays idle,
// and on a default install idle is the normal state - logging "off" sets the
// level to WARN, whose volume in normal operation is ~0 (applyLogLevel,
// main.go:2357-2364).
func TestSaveFileRepairsALoosenedSnapshotMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix mode to loosen: SecureFile writes a DACL there and os.Stat's mode is synthetic")
	}
	path := filepath.Join(t.TempDir(), "logs.txt")
	r := New(2000)
	r.Append("probe 203.0.113.7 ok", "probe [ip] ok")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("loosen the snapshot mode: %v", err)
	}

	// The ring is untouched and the file's length is untouched, so the mode is the
	// only thing that can make this call write.
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("save after chmod: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after repair save: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("snapshot left at %04o after a save, want 0600: it carries the unmasked log "+
			"lines (addresses and hostnames), and the ring may never change again", perm)
	}
	if got := readEntries(t, path); len(got) != 1 || got[0].Raw != "probe 203.0.113.7 ok" {
		t.Fatalf("repairing the mode lost the snapshot contents: %#v", got)
	}

	// The repair must also refresh the memo, or every later tick would rewrite the
	// file forever and the skip would be dead for the life of the process.
	markSaved(t, path)
	for i := 0; i < 3; i++ {
		if err := r.SaveFile(path); err != nil {
			t.Fatalf("idle save %d after repair: %v", i+1, err)
		}
	}
	if rewritten(t, path) {
		t.Error("SaveFile kept rewriting after repairing the mode, so the repair did not refresh the memo")
	}
}

// The skip is only sound if every change to the ring's contents moves the
// (epoch, dropped+len(lines)) pair SaveFile compares. Append is meant to
// increment the sum by exactly one whichever eviction path trim takes, and Clear
// is meant to leave the sum alone but rotate the epoch. This drives both
// eviction paths - the entry cap and the byte ceiling - and fails if any content
// change ever leaves the pair untouched.
func TestSaveTokenMovesOnEveryContentChange(t *testing.T) {
	r := New(8) // small cap so eviction runs constantly
	type token struct {
		epoch string
		seq   uint64
	}
	read := func() (token, string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var sb strings.Builder
		enc := json.NewEncoder(&sb)
		for _, e := range r.lines {
			if err := enc.Encode(e); err != nil {
				t.Fatalf("encode: %v", err)
			}
		}
		return token{r.epoch, r.dropped + uint64(len(r.lines))}, sb.String()
	}

	rnd := rand.New(rand.NewSource(1))
	prevTok, prevContent := read()
	for i := 0; i < 20000; i++ {
		switch {
		case rnd.Intn(20) == 0:
			r.Clear()
		default:
			s := strings.Repeat("x", 1+rnd.Intn(64))
			if rnd.Intn(500) == 0 {
				// Bigger than defaultMaxBytes on its own, to drive the byte ceiling.
				s = strings.Repeat("y", (defaultMaxBytes)+1)
			}
			r.Append(s, s)
			cur, _ := read()
			if cur.epoch == prevTok.epoch && cur.seq != prevTok.seq+1 {
				t.Fatalf("op %d: Append moved the sequence sum %d -> %d, want +1", i, prevTok.seq, cur.seq)
			}
		}
		curTok, curContent := read()
		if curTok == prevTok && curContent != prevContent {
			t.Fatalf("op %d: ring contents changed while the token stayed %+v, so SaveFile would skip a real change", i, curTok)
		}
		prevTok, prevContent = curTok, curContent
	}
}

// readEntries decodes a snapshot written by SaveFile.
func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var out []Entry
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("decode snapshot line %q: %v", ln, err)
		}
		out = append(out, e)
	}
	return out
}
