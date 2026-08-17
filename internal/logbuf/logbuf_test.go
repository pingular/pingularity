package logbuf

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func rawsOf(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Raw
	}
	return out
}

// The ring keeps only the last `cap` entries, oldest first.
func TestRingCapAndOrder(t *testing.T) {
	r := New(3)
	for i := 1; i <= 5; i++ {
		r.Append(fmt.Sprintf("line%d", i), fmt.Sprintf("line%d", i))
	}
	got := rawsOf(r.Entries())
	want := []string{"line3", "line4", "line5"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
}

// Append stores both forms; Clear empties the ring.
func TestAppendBothFormsAndClear(t *testing.T) {
	r := New(10)
	r.Append("ip=1.2.3.4 msg=hi", "ip=[redacted] msg=hi")
	got := r.Entries()
	if len(got) != 1 || got[0].Raw != "ip=1.2.3.4 msg=hi" || got[0].Masked != "ip=[redacted] msg=hi" {
		t.Fatalf("append kept wrong entry: %+v", got)
	}
	r.Clear()
	if got := r.Entries(); len(got) != 0 {
		t.Fatalf("after Clear: %v, want empty", got)
	}
}

// SaveFile/LoadFile round-trips both forms so the viewer's history (and its
// masked/full split) survives a restart; a missing file loads as empty.
func TestSaveLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	r := New(10)
	r.Append("ip=1.2.3.4 one", "ip=[redacted] one")
	r.Append("isp=Acme two", "isp=[redacted] two")
	if err := r.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	r2 := New(10)
	if err := r2.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	got := r2.Entries()
	if len(got) != 2 || got[0].Raw != "ip=1.2.3.4 one" || got[0].Masked != "ip=[redacted] one" || got[1].Masked != "isp=[redacted] two" {
		t.Fatalf("round-trip lost a form: %+v", got)
	}
	// Load respects cap (keep newest).
	r3 := New(1)
	if err := r3.LoadFile(path); err != nil {
		t.Fatalf("LoadFile cap: %v", err)
	}
	if got := r3.Entries(); len(got) != 1 || got[0].Raw != "isp=Acme two" {
		t.Fatalf("cap on load = %+v, want newest only", got)
	}
	// Missing file is not an error.
	if err := New(5).LoadFile(filepath.Join(dir, "nope.txt")); err != nil {
		t.Errorf("missing file: %v, want nil", err)
	}
}

// A legacy plain-text snapshot (pre-dual-form) still loads: each line becomes an
// entry whose raw and masked forms are identical.
func TestLoadLegacyPlainText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.txt")
	if err := os.WriteFile(path, []byte("plain line one\nplain line two\n"), 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}
	r := New(10)
	if err := r.LoadFile(path); err != nil {
		t.Fatalf("LoadFile legacy: %v", err)
	}
	got := r.Entries()
	if len(got) != 2 || got[0].Raw != "plain line one" || got[0].Masked != "plain line one" {
		t.Fatalf("legacy load wrong: %+v", got)
	}
}

// The byte ceiling bounds total retained memory even when the line count is well
// under cap: a burst of oversized lines is evicted down to the ceiling, but the
// single newest line is always kept (dropping it would discard what was just
// appended). Regression for the oversized-Host memory exhaustion (B1).
func TestRingByteCeiling(t *testing.T) {
	r := New(1000)            // a generous line cap...
	r.maxBytes = 4096         // ...but a tight byte ceiling for the test
	big := make([]byte, 2000) // ~2000 bytes raw+masked ⇒ ~4000 per Append
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 100; i++ {
		r.Append(string(big), string(big))
	}
	if r.bytes > r.maxBytes {
		t.Fatalf("retained %d bytes, want <= %d (ceiling)", r.bytes, r.maxBytes)
	}
	if got := len(r.Entries()); got == 0 || got > 2 {
		t.Fatalf("kept %d entries; the ceiling should hold ~1-2 oversized lines", got)
	}

	// A single line larger than the whole ceiling must still be retained (never
	// drop the line just recorded).
	r2 := New(10)
	r2.maxBytes = 8
	r2.Append(string(big), string(big))
	if got := len(r2.Entries()); got != 1 {
		t.Fatalf("an over-ceiling lone line must be kept, got %d entries", got)
	}
}

// After eviction the backing array's tail must no longer reference the dropped
// entries: trim compacts survivors to the front and zeros the vacated slots, so
// the evicted strings become collectable instead of being pinned by the backing
// array until a later Append overwrites the slot. Covers both the one-line
// duplicate-survivor case and larger drops that retained evicted strings.
func TestTrimReleasesEvictedSlots(t *testing.T) {
	r := New(3)
	for i := 1; i <= 6; i++ {
		s := fmt.Sprintf("line%d", i)
		r.Append(s, s)
	}
	// Inspect the whole backing array via a cap-length reslice: every slot past
	// the live length must be a zero Entry, i.e. hold no reference at all.
	r.mu.Lock()
	full := r.lines[:cap(r.lines)]
	for i := len(r.lines); i < len(full); i++ {
		if full[i] != (Entry{}) {
			r.mu.Unlock()
			t.Fatalf("backing slot %d still references an evicted entry: %+v", i, full[i])
		}
	}
	r.mu.Unlock()
}

// Clear must release the retained entries, not merely reslice to length 0 (which
// would leave the backing array pinning every cleared string).
func TestClearReleasesReferences(t *testing.T) {
	r := New(4)
	for i := 1; i <= 4; i++ {
		s := fmt.Sprintf("line%d", i)
		r.Append(s, s)
	}
	r.Clear()
	r.mu.Lock()
	full := r.lines[:cap(r.lines)]
	for i := 0; i < len(full); i++ {
		if full[i] != (Entry{}) {
			r.mu.Unlock()
			t.Fatalf("backing slot %d still references a cleared entry: %+v", i, full[i])
		}
	}
	r.mu.Unlock()
}

// LoadFile must refuse a symlink rather than following it to an attacker-chosen
// target, and must not seed the ring from it.
func TestLoadFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(real, []byte(`{"raw":"a","masked":"a"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed real file: %v", err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	r := New(10)
	if err := r.LoadFile(link); err == nil {
		t.Fatalf("LoadFile followed a symlink; want rejection")
	}
	if got := len(r.Entries()); got != 0 {
		t.Fatalf("symlink load seeded %d entries, want 0", got)
	}
}

// LoadFile must refuse a device file rather than reading from it forever (a
// never-ending stream like /dev/zero would otherwise OOM/hang startup).
func TestLoadFileRejectsDevice(t *testing.T) {
	const dev = "/dev/zero"
	if _, err := os.Lstat(dev); err != nil {
		t.Skipf("%s not available: %v", dev, err)
	}
	r := New(10)
	if err := r.LoadFile(dev); err == nil {
		t.Fatalf("LoadFile(%s) returned nil; want rejection (would read forever)", dev)
	}
	if got := len(r.Entries()); got != 0 {
		t.Fatalf("device load seeded %d entries, want 0", got)
	}
}

// A file far larger than the ring's byte ceiling must not be read whole: only a
// bounded tail is loaded (honouring the memory bound before allocation), the
// newest entries are kept, and the partial line at the seek boundary is dropped
// rather than fabricated into a torn entry.
func TestLoadFileReadsBoundedNewestTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")

	// Build a snapshot far larger than the read cap we will impose below.
	src := New(1 << 20)
	const n = 5000
	for i := 0; i < n; i++ {
		s := fmt.Sprintf("entry-%05d-%s", i, strings.Repeat("x", 100))
		src.Append(s, s)
	}
	if err := src.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}

	r := New(1 << 20)    // a huge entry cap so only the byte ceiling can bind
	r.maxBytes = 8 << 10 // an 8 KiB read cap, far below the file size
	if int64(r.maxBytes) >= fi.Size() {
		t.Fatalf("test setup: file %d bytes is not larger than read cap %d", fi.Size(), r.maxBytes)
	}
	if err := r.LoadFile(path); err != nil {
		t.Fatalf("LoadFile: %v", err)
	}

	got := r.Entries()
	if len(got) == 0 {
		t.Fatalf("loaded nothing from a large file")
	}
	// Bounded: retained bytes stay within the ceiling (nothing near the file's
	// full size was ever allocated into the ring).
	if r.bytes > r.maxBytes {
		t.Fatalf("retained %d bytes, want <= %d (read cap)", r.bytes, r.maxBytes)
	}
	// Newest kept: the last loaded entry is the last one written.
	wantLast := fmt.Sprintf("entry-%05d-%s", n-1, strings.Repeat("x", 100))
	if last := got[len(got)-1].Raw; last != wantLast {
		t.Fatalf("last loaded = %q, want newest %q", last, wantLast)
	}
	// No torn fragment leaked in as a bogus entry: every loaded raw is a whole,
	// well-formed line.
	for _, e := range got {
		if !strings.HasPrefix(e.Raw, "entry-") {
			t.Fatalf("loaded a torn/partial line as an entry: %q", e.Raw)
		}
	}
}

// Concurrent appends must not race or lose the invariant len(entries) <= cap.
func TestConcurrent(t *testing.T) {
	r := New(100)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Append("x", "x")
			}
		}()
	}
	wg.Wait()
	if got := len(r.Entries()); got != 100 {
		t.Fatalf("len = %d, want 100 (cap)", got)
	}
}

// SaveFile must be safe to call concurrently: a periodic checkpoint can race a
// shutdown save, and both use the same fixed path+".tmp" scratch file. Without
// the save mutex their O_TRUNC opens and renames interleave into a torn snapshot;
// with it every load sees the complete ring (#21).
//
// Each goroutine appends its own line before saving. SaveFile skips the rewrite
// when the ring has not changed since it last wrote this file, so against a
// static ring only the FIRST of these 160 calls would reach the tmp+rename path
// the test exists to race, and the other 159 would return at the skip:
// instrumenting the loop to stat the snapshot after every call saw exactly one
// (inode, mtime) file identity for the whole test that way, against many with
// the append.
func TestSaveFileConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logs.txt")
	const N = 500
	r := New(N)
	for i := 0; i < N; i++ {
		r.Append(fmt.Sprintf("raw-%03d", i), fmt.Sprintf("msk-%03d", i))
	}
	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				// The ring is already at its entry cap, so this evicts one line and
				// adds one: every snapshot is still exactly N entries, whenever it
				// is taken.
				line := fmt.Sprintf("round-%02d-g-%d", round, g)
				r.Append(line, line)
				if err := r.SaveFile(path); err != nil {
					t.Errorf("SaveFile: %v", err)
				}
			}(g)
		}
		wg.Wait()
		got := New(N)
		if err := got.LoadFile(path); err != nil {
			t.Fatalf("round %d LoadFile: %v", round, err)
		}
		if n := len(got.Entries()); n != N {
			t.Fatalf("round %d: loaded %d entries, want %d (torn snapshot)", round, n, N)
		}
	}
}
