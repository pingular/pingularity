package logbuf

import (
	"fmt"
	"os"
	"path/filepath"
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
