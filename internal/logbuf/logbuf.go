// Package logbuf keeps the most recent log lines in memory so the dashboard can
// show them. Each line is captured in two forms - Raw (full detail) and Masked
// (PII values replaced) - so the viewer can toggle masking at display time while
// the full detail is always retained. The ring is fed by the slog handler and can
// be snapshotted to disk (SaveFile/LoadFile) so the viewer's history survives a
// restart.
package logbuf

import (
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

// Ring is a concurrency-safe buffer of the last `cap` log entries.
type Ring struct {
	mu    sync.Mutex
	lines []Entry
	cap   int
}

// New returns a Ring holding at most capacity entries (a non-positive capacity is
// clamped to 1).
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity, lines: make([]Entry, 0, capacity)}
}

// Append records one captured line (raw + masked) and drops the oldest once over
// capacity.
func (r *Ring) Append(raw, masked string) {
	r.mu.Lock()
	r.lines = append(r.lines, Entry{Raw: raw, Masked: masked})
	if over := len(r.lines) - r.cap; over > 0 {
		r.lines = append(r.lines[:0], r.lines[over:]...)
	}
	r.mu.Unlock()
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
	r.mu.Unlock()
}

// SaveFile snapshots the buffered entries to path (one JSON object per line) so
// the viewer's history survives a restart. It writes a temp file and renames, so
// a crash mid-write can't leave a torn file. Mode 0600: the raw form holds
// IPs/hostnames, so the file is owner-only.
func (r *Ring) SaveFile(path string) error {
	r.mu.Lock()
	var b strings.Builder
	for _, e := range r.lines {
		enc, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	r.mu.Unlock()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
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
			r.lines = append(r.lines, Entry{Raw: ln, Masked: ln})
		}
	}
	if over := len(r.lines) - r.cap; over > 0 {
		r.lines = append(r.lines[:0], r.lines[over:]...)
	}
	return nil
}
