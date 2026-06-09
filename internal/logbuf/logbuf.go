// Package logbuf keeps the most recent log lines in memory so the dashboard can
// show them. Each line is captured in two forms - Raw (full detail) and Masked
// (PII values replaced) - so the viewer can toggle masking at display time while
// the full detail is always retained. The ring is fed by the slog handler and can
// be snapshotted to disk (SaveFile/LoadFile) so the viewer's history survives a
// restart.
package logbuf

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		// Compact the survivors to the front, then zero the slots they vacated.
		// A bare append/copy leaves the backing array's tail still pointing at
		// the evicted entries (and, for a one-line drop, a duplicate of the
		// newest survivor); those references pin the dropped strings until the
		// slot happens to be overwritten by a later Append. Clearing the tail
		// makes the evicted bytes collectable immediately, so the ring's memory
		// actually shrinks when it evicts rather than only when it next grows.
		n := copy(r.lines, r.lines[drop:])
		clear(r.lines[n:])
		r.lines = r.lines[:n]
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
	// Zero the entries before reslicing to length 0. A bare r.lines[:0] keeps
	// the backing array (sized up to the ring's bound) referencing every cleared
	// entry's strings until they are overwritten by future Appends; clearing
	// releases that memory now, so Clear is a predictable memory release.
	clear(r.lines)
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
	// fsync the temp file's bytes to stable storage BEFORE the rename. A rename
	// is atomic only for the directory entry; it makes no promise about the data
	// blocks. Without this fsync a power loss just after the rename could leave
	// the new name pointing at an inode whose bytes never reached disk - a
	// zero-length or partial snapshot.
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// fsync the parent directory so the rename itself is durable. The rename is
	// a directory metadata change; if the file's data survived a crash but the
	// directory entry did not, the snapshot could revert to the old file or
	// disappear entirely. Opening the directory read-only is enough to fsync it.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	return dir.Close()
}

// maxLoadLineBytes caps a single on-disk line before it is turned into an entry.
// The bounded tail read below already caps the TOTAL bytes decoded, but a
// snapshot corrupted into one enormous newline-free "line" could still force a
// single huge allocation; this per-line ceiling keeps that bounded too. It is
// deliberately generous - far larger than any real log line - so it never trims
// legitimate history.
const maxLoadLineBytes = 1 << 20 // 1 MiB

// LoadFile seeds the ring from a file previously written by SaveFile, keeping at
// most cap entries (newest). It also reads the legacy plain-text format (one line
// per entry, treated as raw==masked) so an old snapshot still loads. Intended to
// run once at startup before logging begins; a missing file is not an error.
//
// The read is bounded before any entries are built, so a huge or hostile
// logs.txt cannot OOM startup: only the last maxBytes of the file are read (the
// newest entries, which are all the ring can ever keep), any single line longer
// than maxLoadLineBytes is skipped, and a path that has been swapped for a
// symlink or a device - which could redirect the read to an attacker's target or
// a never-ending stream like /dev/zero - is refused outright.
func (r *Ring) LoadFile(path string) error {
	// Lstat (not Stat) so a symlink is seen as a symlink, and check the type
	// BEFORE opening: opening a fifo or device could block or stream forever, so
	// those must be rejected without ever touching the descriptor. IsRegular is
	// false for symlinks, devices, fifos, directories and sockets alike.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("logbuf: refusing to load %s: not a regular file", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Re-check on the open descriptor to close the tiny window between the Lstat
	// above and this Open, in which the path could have been swapped for a
	// non-regular file.
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("logbuf: refusing to load %s: not a regular file", path)
	}

	// Read at most maxBytes from the TAIL of the file. SaveFile writes
	// oldest->newest and the ring keeps only the newest maxBytes/cap anyway, so
	// the tail is exactly the part worth loading. Bounding the read here means
	// the ring's memory ceiling is honoured before we allocate a single entry,
	// whatever the file's size on disk.
	readCap := int64(r.maxBytes)
	var offset int64
	if fi.Size() > readCap {
		offset = fi.Size() - readCap
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
	}
	b, err := io.ReadAll(io.LimitReader(f, readCap))
	if err != nil {
		return err
	}
	if offset > 0 {
		// We seeked into the middle of the file, so the first "line" is really
		// the tail of an earlier entry; drop up to the first newline so we only
		// parse whole lines and never fabricate an entry from a fragment.
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		} else {
			b = nil
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ln := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if ln == "" || len(ln) > maxLoadLineBytes {
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
