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
	"github.com/pingular/pingularity/internal/osperm"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

// ringSeq separates two Rings built in the same nanosecond; see newEpoch.
var ringSeq atomic.Uint64

// newEpoch names one Ring instance. A viewer's cursor is only meaningful against
// the ring that issued it: LoadFile reseeds a fresh ring from logs.txt with no
// persisted sequence, so after a restart sequence 3000 names a DIFFERENT line
// than the one an already-open tab is holding at 3000. Handing the epoch out
// with every response lets a client notice that and resync instead of silently
// rendering the wrong window. It is a string because a nanosecond timestamp is
// past JavaScript's exact-integer range.
func newEpoch() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatUint(ringSeq.Add(1), 36)
}

// Ring is a concurrency-safe buffer of the last `cap` log entries, additionally
// bounded to maxBytes of retained line text.
type Ring struct {
	mu       sync.Mutex
	lines    []Entry
	cap      int
	maxBytes int
	bytes    int // sum of entrySize over lines (kept in step with the slice)
	// dropped counts the entries this ring has evicted since it was built, and so
	// doubles as the sequence number of lines[0]: the entry at lines[i] is
	// sequence dropped+i. Deriving the sequence from a monotonically increasing
	// evicted-count rather than from a slice index is what makes a reader's cursor
	// survive both eviction paths (the entry cap and the byte ceiling) and trim's
	// compaction, which do move every surviving entry's index.
	dropped uint64
	epoch   string // rotated by Clear (and per Ring); read under mu. See newEpoch
	// saveMu serializes SaveFile calls with each other (NOT with mu, which guards
	// the in-memory ring and must never be held across file I/O). Two concurrent
	// SaveFiles - a periodic checkpoint racing a shutdown save - otherwise share the
	// one fixed ".tmp" path: their O_TRUNC opens and renames interleave and can
	// produce a torn or truncated snapshot.
	saveMu sync.Mutex
	saved  savedSnapshot // what the last successful SaveFile left on disk; guarded by saveMu
}

// savedSnapshot describes the file the last successful SaveFile produced, so a
// SaveFile called when nothing has been logged since can skip the rewrite. It is
// guarded by saveMu rather than mu because only SaveFile touches it, and saveMu
// already serializes those.
type savedSnapshot struct {
	written bool   // false until this Ring has completed one SaveFile, so the first call always writes
	path    string // SaveFile takes its target per call; a different target has nothing memoised
	epoch   string // rotated by Clear, which is the one content change the sequence sum cannot show
	seq     uint64 // dropped+len(lines) at the moment the written entries were copied
	// size of the file we wrote. It notices a snapshot that changed length behind
	// our back, which is the truncate-to-reclaim-space case. (A snapshot that was
	// DELETED is caught before this field is ever compared, by the skip's Lstat
	// returning an error.) It does NOT notice a replacement that keeps the same
	// length: the skip then leaves the foreign bytes in place, so only a content
	// change in the ring will rewrite them.
	size int64
	// mode of the file we wrote, taken from the same Stat as size so the two sides
	// of the comparison always come from the same source (on Windows the mode
	// os.Stat reports is synthetic - access is governed by the DACL SecureFile
	// sets, internal/osperm/perm_windows.go:27-29 - so comparing it against a
	// hardcoded 0600 would never match).
	// Without this the skip would leave a snapshot someone chmodded 0644 readable
	// by every account on the box for as long as the ring stays idle, and this file
	// holds the raw unmasked lines.
	mode os.FileMode
}

// entrySize is one entry's contribution to the byte ceiling: both stored forms.
func entrySize(e Entry) int { return len(e.Raw) + len(e.Masked) }

// New returns a Ring holding at most capacity entries (a non-positive capacity is
// clamped to 1) and at most defaultMaxBytes of retained text.
func New(capacity int) *Ring {
	if capacity < 1 {
		capacity = 1
	}
	return &Ring{cap: capacity, maxBytes: defaultMaxBytes, lines: make([]Entry, 0, capacity), epoch: newEpoch()}
}

// Epoch identifies this Ring instance; see newEpoch.
func (r *Ring) Epoch() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.epoch
}

// Len reports how many entries the ring currently holds. Callers use it to tell a
// truncated read from a complete one: a response of 500 lines means nothing on its
// own, since it could be the whole buffer or the newest tenth of it.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
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
		r.dropped += uint64(drop)
	}
}

// Entries returns a copy of the buffered entries, oldest first. It is the whole
// ring: the bug-report download wants every line it has. A poller wants Since.
func (r *Ring) Entries() []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Entry, len(r.lines))
	copy(out, r.lines)
	return out
}

// window copies lines[lo:hi] and reports the sequence bounds around it. Called
// with r.mu held, so a caller gets its entries and its cursor from one
// consistent snapshot - reading them under two separate locks would let an
// Append land in between and hand back a cursor that skips it.
func (r *Ring) window(lo, hi int) (out []Entry, first, next uint64) {
	out = make([]Entry, hi-lo)
	copy(out, r.lines[lo:hi])
	return out, r.dropped, r.dropped + uint64(hi)
}

// Tail returns the newest limit entries (all of them when limit <= 0), the
// sequence of the oldest entry still buffered, and the sequence one past the
// newest returned - the cursor to pass to Since on the next poll.
func (r *Ring) Tail(limit int) (out []Entry, first, next uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lo := 0
	if limit > 0 && len(r.lines) > limit {
		lo = len(r.lines) - limit
	}
	return r.window(lo, len(r.lines))
}

// Since returns up to limit entries starting at sequence seq (all of them from
// seq on when limit <= 0), the sequence of the oldest entry still buffered, and
// the sequence one past the last entry returned.
//
// It hands back the OLDEST entries after seq, not the newest, so a reader that
// falls behind catches up over successive polls instead of skipping the middle.
// A seq below first (the reader's cursor was evicted while it was away) resumes
// at the oldest line still held rather than reporting a gap it cannot fill; the
// caller compares seq against the returned first to tell the reader how many
// lines it missed. A seq above next (a cursor from another ring, or one held
// across a Clear) is clamped to the end, which reads as "nothing new" - the
// caller notices the epoch changed and resyncs.
func (r *Ring) Since(seq uint64, limit int) (out []Entry, first, next uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end := r.dropped + uint64(len(r.lines))
	if seq < r.dropped {
		seq = r.dropped
	}
	if seq > end {
		seq = end
	}
	lo := int(seq - r.dropped)
	hi := len(r.lines)
	if limit > 0 && hi-lo > limit {
		hi = lo + limit
	}
	return r.window(lo, hi)
}

// Clear empties the buffer.
func (r *Ring) Clear() {
	r.mu.Lock()
	// Rotate the epoch. Advancing the sequence is enough to stop a stale cursor
	// addressing the WRONG lines, but not to tell a viewer that is already
	// caught up that anything happened: its next poll returns no lines and the
	// same next_seq it sent, which is byte-identical to an idle poll. Every other
	// tab therefore went on rendering the lines the operator just wiped, until
	// something else happened to be logged.
	//
	// The epoch already means "your cursor is not meaningful against this buffer,
	// resync" - which is exactly true here - and the client's replace-on-epoch-change
	// path is the one that repaints. Restart is the other event that rotates it.
	r.epoch = newEpoch()
	// Clearing EVICTS the buffered lines, so the sequence keeps climbing past
	// them; resetting it to 0 would make a cursor held from before the Clear look
	// valid again and silently re-render lines the operator just wiped.
	r.dropped += uint64(len(r.lines))
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
//
// A call made when nothing has been logged since the last one does nothing, so a
// caller may snapshot on a fixed timer without paying for it while the ring sits
// idle. What that check compares is the ring's generation token plus the file's
// size and mode - not its content, so a same-length replacement is skipped over
// too; savedSnapshot documents where it stops short.
func (r *Ring) SaveFile(path string) error {
	// Serialize with any other in-flight SaveFile: they share the fixed path+".tmp"
	// scratch file, so overlapping writes/renames would corrupt the snapshot. This
	// lock covers only file I/O, never the logging path (Append takes r.mu, not this).
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	// Snapshot just the slice header under the lock (cheap - entries are string
	// headers, not their bytes), then STREAM each entry to the temp file outside
	// the lock. The old path concatenated the whole ring into one strings.Builder,
	// transiently doubling the ring's bytes in memory; streaming holds one encoded
	// line at a time and never blocks the logging path on file I/O.
	r.mu.Lock()
	snapshot := make([]Entry, len(r.lines))
	copy(snapshot, r.lines)
	// Generation token for the copy just taken, read under the same lock so it
	// always describes exactly these entries. Append strictly increments
	// dropped+len(lines) - trim only moves a line from the second term into the
	// first - and Clear leaves that sum alone but rotates the epoch, so every way
	// the ring's contents can change moves one of the two. Checked over 20,000
	// randomised append/clear operations driving both eviction paths: no content
	// change left the pair unchanged.
	epoch, seq := r.epoch, r.dropped+uint64(len(r.lines))
	r.mu.Unlock()

	// Nothing has been logged since we last wrote this exact file, so writing it
	// again would only rewrite bytes that are already there. The caller snapshots
	// on a one-minute ticker whether or not anything was logged (main.go:462), and
	// the default install runs with logging "off" (main.go:426, main.go:506-507).
	// "Off" is not silence: it sets the level to WARN, and the ring shares that
	// level (applyLogLevel, main.go:2357-2364; buildLogger, main.go:1851). So a
	// default install's ring is IDLE rather than empty - it fills exactly when
	// something has gone wrong, and applyLogLevel's own comment puts real
	// WARN/ERROR volume in normal operation at ~0. Idle is the case this skip is
	// for, and it pays off whatever the ring holds: TestSaveFileSkipsUnchangedRing
	// runs an hour of the ticker over a ring with history and asserts not one
	// rewrite, where before there were 60 - each a file creation plus the fsyncs
	// below (the file, and off Windows its directory too), for content that never
	// changed.
	//
	// The file itself is checked as well as the token, because the memo only says
	// what THIS process wrote and something else may have changed it since. Lstat
	// rather than Stat, for the reason LoadFile gives below: Stat follows a
	// symlink and would describe the TARGET, so a snapshot swapped for a symlink
	// whose target happens to match size and mode would be skipped over and left a
	// symlink - which LoadFile then refuses outright, losing the viewer's history
	// for good rather than for a minute. Lstat sees the link's own mode, declines
	// the skip, and the rename below puts a regular file back, which is what every
	// tick did before this skip existed. On a regular file the two calls agree.
	//   - size, so a snapshot truncated to reclaim space is rewritten on the next
	//     call instead of never coming back (a deleted one fails the Lstat, and is
	//     rewritten for that reason rather than by the size comparison);
	//   - mode, so a snapshot whose permissions were loosened is rebuilt through
	//     osperm.SecureFile below and comes back owner-only. Skipping that would
	//     leave the raw unmasked lines (IPs, hostnames) readable by other accounts
	//     for as long as the ring stays idle - which, per the paragraph above, is a
	//     default install's normal state rather than a brief one.
	if r.saved.written && r.saved.path == path && r.saved.epoch == epoch && r.saved.seq == seq {
		if fi, err := os.Lstat(path); err == nil && fi.Size() == r.saved.size && fi.Mode() == r.saved.mode {
			return nil
		}
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// OpenFile's 0600 is honoured on Unix and IGNORED on Windows, where a new file
	// simply inherits the parent's ACEs - and a supported layout puts this beside the
	// database in C:\ProgramData, whose inherited ACEs grant BUILTIN\Users read. This
	// snapshot holds the unmasked log lines (IPs, hostnames), so it needs its own
	// DACL rather than the directory's. Applied BEFORE the rename, so the file is
	// never briefly readable under its final name; secret.go locks the key file the
	// same way and for the same reason.
	if err := osperm.SecureFile(tmp); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("secure log snapshot: %w", err)
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
	// Size and mode of what we just wrote, taken from the descriptor we wrote it
	// through and after osperm.SecureFile has tightened it. The skip above compares
	// both against the file that is actually there, which is how a truncated or
	// chmodded snapshot gets repaired instead of being assumed good forever.
	fi, err := f.Stat()
	if err != nil {
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
	//
	// Windows has no directory fsync: FlushFileBuffers on a directory handle
	// returns ERROR_ACCESS_DENIED. Rename durability there is the filesystem's
	// responsibility, so skip this step rather than fail the snapshot.
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		if err := dir.Sync(); err != nil {
			dir.Close()
			return err
		}
		if err := dir.Close(); err != nil {
			return err
		}
	}
	// Record what is now on disk, so the next call can skip an identical rewrite.
	// Only after every step succeeded: a save that failed part-way has left
	// something we cannot describe, and must be retried rather than memoised.
	r.saved = savedSnapshot{written: true, path: path, epoch: epoch, seq: seq, size: fi.Size(), mode: fi.Mode()}
	return nil
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
