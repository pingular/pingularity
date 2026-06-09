// Package logbuf keeps the most recent log lines in memory so the dashboard can
// show them. Each line is captured in two forms - Raw (full detail) and Masked
// (PII values replaced) - so the viewer can toggle masking at display time while
// the full detail is always retained. The ring is fed by the slog handler and can
// be snapshotted to disk (SaveFile/LoadFile) so the viewer's history survives a
// restart.
package logbuf

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
)

// Entry is one captured log line in both forms. Both are produced at capture
// time; the dashboard chooses which to show, so masking is purely a view concern.
type Entry struct {
	Raw    string `json:"raw"`
	Masked string `json:"masked"`
}

// defaultMaxBytes bounds the ring's total in-memory footprint independent of the
// entry count. The line cap alone bounds memory only if every line is small; a
// burst of oversized lines (a giant Host header echoed into a warning, a stack
// trace) could otherwise pin megabytes per line x the whole cap. This ceiling
// evicts oldest lines until the retained bytes fit, so worst-case memory is
// bounded whatever the line sizes are.
const defaultMaxBytes = 8 << 20

// Ring is a concurrency-safe buffer of the last `cap` log entries, additionally
// bounded to maxBytes of retained line text.
type Ring struct {
	mu       sync.Mutex
	lines    []Entry
	cap      int
	maxBytes int
	bytes    int // sum of entrySize over lines (kept in step with the slice)
}

// entrySize is one entry's contribution to the byte ceiling: both stored forms.
func entrySize(e Entry) int { return len(e.Raw) + len(e.Masked) }

// New returns a Ring holding at most capacity entries (a non-positive capacity is
// clamped to 1) and at most defaultMaxBytes of retained text.
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity, maxBytes: defaultMaxBytes, lines: make([]Entry, 0, capacity)}
}

// Append records one captured line (raw + masked) and drops the oldest until both
// the entry-count and total-byte ceilings are satisfied.
func (r *Ring) Append(raw, masked string) {
	e := Entry{Raw: raw, Masked: masked}
	r.mu.Lock()
	r.lines = append(r.lines, e)
	r.bytes += entrySize(e)
	r.trim()
	r.mu.Unlock()
}

// trim evicts the oldest lines until the ring is within both ceilings, always
// keeping at least the newest line (a single line larger than maxBytes still
// stays - dropping it would discard the very line just recorded). Called with
// r.mu held.
func (r *Ring) trim() {
	drop := 0
	for drop < len(r.lines)-1 && (len(r.lines)-drop > r.cap || r.bytes > r.maxBytes) {
		r.bytes -= entrySize(r.lines[drop])
		drop++
	}
	if drop > 0 {
		r.lines = append(r.lines[:0], r.lines[drop:]...)
	}
}

// Entries returns a copy of the buffered entries, oldest first.
func (r *Ring) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.lines))
	copy(out, r.lines)
	return out
}

// Clear empties the buffer.
func (r *Ring) Clear() {
	r.mu.Lock()
	r.lines = r.lines[:0]
	r.bytes = 0
	r.mu.Unlock()
}

// SaveFile snapshots the buffered entries to path (one JSON object per line) so
// the viewer's history survives a restart. It writes a temp file and renames, so
// a crash mid-write can't leave a torn file. Mode 0600: the raw form holds
// IPs/hostnames, so the file is owner-only.
func (r *Ring) SaveFile(path string) error {
	// Snapshot just the slice header under the lock (cheap - entries are string
	// headers, not their bytes), then STREAM each entry to the temp file outside
	// the lock. The old path concatenated the whole ring into one strings.Builder,
	// transiently doubling the ring's bytes in memory; streaming holds one encoded
	// line at a time and never blocks the logging path on file I/O.
	r.mu.Lock()
	snapshot := make([]Entry, len(r.lines))
	copy(snapshot, r.lines)
	r.mu.Unlock()

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw) // Encode appends its own '\n' per entry
	for _, e := range snapshot {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFile seeds the ring from a file previously written by SaveFile, keeping at
// most cap entries (newest). It also reads the legacy plain-text format (one line
// per entry, treated as raw==masked) so an old snapshot still loads. Intended to
// run once at startup before logging begins; a missing file is not an error.
func (r *Ring) LoadFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var e Entry
		if ln[0] == '{' && json.Unmarshal([]byte(ln), &e) == nil {
			r.lines = append(r.lines, e)
		} else {
			// Legacy snapshot: a bare formatted line. Keep it as-is either way.
			e = Entry{Raw: ln, Masked: ln}
			r.lines = append(r.lines, e)
		}
		r.bytes += entrySize(e)
	}
	// Enforce both ceilings on a loaded snapshot too: an oversized or tampered
	// on-disk file must not be able to seed the ring past its memory bound.
	r.trim()
	return nil
}
