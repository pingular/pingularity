// Package store is Pingularity's SQLite-backed time-series persistence layer.
//
// Tables:
//
//   - samples:  one row per target per probe round (raw latency time series).
//   - dns:      one DNS-resolve-latency sample per probe round.
//   - events:   up/down transitions with outage durations - the authoritative
//     outage record behind the heatmap, event log, and uptime % (see UptimeSince).
//   - speed:    speedtest results plus the connection context each ran in.
//   - settings: UI-adjustable key/values that survive restarts.
//
// Uses the pure-Go modernc.org/sqlite driver (no cgo) so the binary stays
// statically linkable. See Open for the WAL/pragma rationale.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingular/pingularity/internal/osperm"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/util"
	_ "modernc.org/sqlite" // pure-Go driver, registers "sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS samples (
    ts         INTEGER NOT NULL,   -- unix seconds
    target     TEXT    NOT NULL,
    latency_ms REAL,               -- NULL when the probe failed
    success    INTEGER NOT NULL,   -- 0 / 1
    family     TEXT               -- "ipv4" | "ipv6"; NULL on pre-family rows
);
CREATE INDEX IF NOT EXISTS idx_samples_ts ON samples(ts);

CREATE TABLE IF NOT EXISTS dns (
    ts         INTEGER NOT NULL,   -- unix seconds (one row per probe round)
    latency_ms REAL,               -- cache-busted resolve time; NULL when DNS failed
    success    INTEGER NOT NULL    -- 0 / 1 (1 = resolver answered, incl. NXDOMAIN)
);
CREATE INDEX IF NOT EXISTS idx_dns_ts ON dns(ts);

CREATE TABLE IF NOT EXISTS events (
    ts         INTEGER NOT NULL,   -- unix seconds of the transition
    type       TEXT    NOT NULL,   -- 'down' | 'up'
    duration_s INTEGER,            -- outage length, set on 'up'
    detail     TEXT
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);

CREATE TABLE IF NOT EXISTS pauses (
    ts         INTEGER NOT NULL,   -- unix seconds a monitoring pause / unobserved span began
    duration_s INTEGER NOT NULL    -- observed length of the pause span (seconds)
);
CREATE INDEX IF NOT EXISTS idx_pauses_ts ON pauses(ts);

CREATE TABLE IF NOT EXISTS speed (
    ts           INTEGER NOT NULL,
    down_mbps    REAL,
    up_mbps      REAL,
    ping_ms      REAL,
    server       TEXT,
    server_id    TEXT,
    public_ipv4  TEXT,
    public_ipv6  TEXT,
    isp          TEXT,
    isp_location TEXT,
    dns_ip       TEXT,
    dns_provider TEXT,
    dns_location TEXT,
    packet_loss  REAL,                -- percent (0..100); NULL when unmeasurable
    healthy      INTEGER,             -- 1/0 vs thresholds; NULL when no thresholds set
    jitter_ms    REAL,                -- ping variation (ms); NULL on pre-jitter rows
    download_bytes INTEGER,           -- bytes received during the test; NULL on old rows
    upload_bytes   INTEGER,           -- bytes sent during the test; NULL on old rows
    cf_colo        TEXT,              -- Cloudflare PoP at run time
    exit_summary   TEXT,              -- exit router → handoff path at run time
    run_trigger    TEXT,              -- what started the test: scheduled|manual|reconnect|startup|degraded ("trigger" is reserved in SQLite)
    idle_ms        REAL,              -- latency under load: idle baseline (median, ms)
    loaded_down_ms REAL,              -- ... during the download phase; NULL when unmeasurable
    loaded_up_ms   REAL,              -- ... during the upload phase
    loaded_down_max_ms REAL,          -- worst single sample (spike) during the download phase
    loaded_up_max_ms   REAL,          -- ... during the upload phase
    engine             TEXT           -- test backend: ookla|iperf3 (NULL on old rows = ookla)
);
CREATE INDEX IF NOT EXISTS idx_speed_ts ON speed(ts);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Store wraps the database handle.
type Store struct {
	db *sql.DB

	// recMu guards recCache, the per-'down' quorum-recovery scan memo (see
	// firstQuorumRecovery), and recGen, the invalidation generation that lets a
	// long unlocked scan detect a concurrent cache invalidation and decline to
	// publish a now-stale result.
	recMu    sync.Mutex
	recCache map[int64]recScan
	recGen   uint64

	// seriesMu guards seriesCache, the wide-window chart aggregate cache (see
	// Series).
	seriesMu    sync.Mutex
	seriesCache map[seriesKey]*seriesEntry
}

// pragmaConn is the per-connection pragma query appended to every file-backed
// DSN: WAL + synchronous=NORMAL cuts fsync overhead on the write-heavy probe
// path and lets status/chart reads run without blocking the writer, and
// busy_timeout makes a rare write-write collision retry instead of failing. The
// params apply to every pooled connection (modernc/sqlite).
const pragmaConn = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

// buildDSN turns a filesystem path into a modernc/sqlite DSN. A path containing
// a character the driver parses as DSN syntax ('?', '#', or '%') must not be
// concatenated raw: modernc splits a plain DSN at the FIRST '?' and opens only
// the prefix, so -db ".../a?b/x.db" silently creates ".../a" (outside the
// secured data dir, at the driver's default 0644) while the securing loop stats
// the intended path, finds nothing, and skips it. Escape those bytes and open a
// file: URI, which modernc keeps intact and SQLite (SQLITE_OPEN_URI) decodes
// back to the real filename. Ordinary paths - including Windows drive/UNC paths,
// which contain none of these characters - keep the historical plain form
// byte-for-byte, so this changes behaviour only for paths that are otherwise
// broken today.
func buildDSN(path string) string {
	if path == ":memory:" {
		return path
	}
	if !strings.ContainsAny(path, "?#%") {
		return path + pragmaConn
	}
	// A single-pass replacer: "%" -> "%25" is applied to the original input, so
	// the "%" it introduces is not re-scanned into the "?"/"#" rules.
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + esc + pragmaConn
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists.
func Open(path string) (*Store, error) {
	// Create the parent dir for file-backed databases (skips ":memory:" and bare
	// filenames, whose dir is "."). 0o700 because the DB holds secrets at rest
	// (bcrypt auth hash, webhook URLs) - not readable by other local users.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		// Whether the directory already existed decides if we may tighten it.
		// os.MkdirAll gives no "did I create it" signal, so probe first (Lstat,
		// not Stat, so a symlinked dir is seen as pre-existing rather than
		// followed).
		_, statErr := os.Lstat(dir)
		dirExisted := statErr == nil
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", dir, err)
		}
		if !dirExisted {
			// We just created this directory, so tightening it cannot disturb a
			// pre-existing system or shared directory. This is the safe case:
			// MkdirAll's 0o700 is honoured on Unix but ignored on Windows (no
			// ACL), so apply the owner-only protection explicitly either way.
			if err := osperm.SecureDir(dir); err != nil && !securingSkippable(err) {
				return nil, fmt.Errorf("secure data dir %s: %w", dir, err)
			}
		} else if accessible, known := osperm.GroupOrWorldAccessible(dir); known && accessible {
			// A pre-existing directory we did NOT create must never be
			// re-permissioned: a supported -db like /var/lib/pingularity.db or
			// C:\ProgramData\pingularity.db would otherwise chmod/DACL-lock a
			// shared system directory and break every other user of it. The DB
			// and its WAL/SHM sidecars are still locked to the owner below, so
			// the data stays private; warn that the parent is world/group
			// reachable so an operator can move to a dedicated directory.
			log.Printf("pingularity: data directory %s is group/world-accessible and was not created by pingularity; leaving its permissions unchanged (the database file itself is owner-only). Consider a dedicated -db directory.", dir)
		}
		// A database that predates the secured directory (a restored backup, a
		// db copied in from elsewhere) may hold only ACEs inherited from its old
		// parent - and on Windows, protecting the directory strips inherited
		// ACEs from existing children, leaving such a file unreadable
		// (SQLITE_CANTOPEN). Give any existing database and its WAL sidecars the
		// explicit owner-only protection too; on Unix this tightens a copied-in
		// db to 0600, closing the same gap from the other direction.
		for _, sfx := range []string{"", "-wal", "-shm"} {
			if _, err := os.Stat(path + sfx); err == nil {
				if err := secureDBFile(path + sfx); err != nil {
					return nil, fmt.Errorf("secure db %s: %w", path+sfx, err)
				}
			}
		}
	}
	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite has a single writer; a small pool lets concurrent readers (status
	// poll, charts) proceed under WAL, while writers serialize via busy_timeout.
	// A bare ":memory:" DSN gives each pooled connection its OWN private database,
	// fragmenting schema/data across connections - so pin the pool to one there.
	// File-backed paths share on-disk state and keep the pool.
	if path == ":memory:" {
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(4)
	}
	// Applying the base schema, the additive column migrations, and the index
	// cleanup is one operation we may need to run twice: once on the existing
	// file, then again on a freshly rebuilt one if the first attempt reveals the
	// file is corrupt (see the recovery below).
	if err := applySchema(db); err != nil {
		// A malformed or truncated main DB (typically a hard power-off mid-
		// checkpoint) makes the first statement fail with "database disk image is
		// malformed" or "file is not a database". Under systemd's Restart=always
		// this crash-loops forever on the same bad file and monitoring never
		// recovers. For a file-backed store, move the corrupt DB (and its WAL/SHM
		// sidecars) ASIDE - never delete user data - and rebuild an empty store so
		// the daemon comes back up monitoring. A :memory: store or a non-corruption
		// error still fails fast.
		if path == ":memory:" || !dbCorrupt(err) {
			db.Close()
			return nil, err
		}
		db.Close()
		stats.Inc("db.corrupt")
		moved, qerr := quarantineCorruptDB(path)
		if qerr != nil {
			return nil, qerr
		}
		log.Printf("pingularity: database %s is corrupt (%v); moved aside to %s and rebuilt an empty store", path, err, moved)
		if db, err = sql.Open("sqlite", buildDSN(path)); err != nil {
			return nil, fmt.Errorf("open db (post-recovery): %w", err)
		}
		db.SetMaxOpenConns(4) // file-backed: recovery only runs for path != ":memory:"
		if err := applySchema(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("rebuild after corruption: %w", err)
		}
	}
	// Tighten on-disk permissions to owner-only: the driver creates files 0644
	// (umask-subject) but this DB stores secrets. Covers the main file plus the
	// WAL/SHM sidecars; best-effort (a sidecar missing pre-first-write, or a
	// filesystem that can't express owner-only perms, is not fatal - see
	// securingSkippable).
	if path != ":memory:" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := secureDBFile(path + suffix); err != nil {
				db.Close()
				return nil, fmt.Errorf("secure db perms %s: %w", path+suffix, err)
			}
		}
	}
	return &Store{
		db:          db,
		recCache:    map[int64]recScan{},
		seriesCache: map[seriesKey]*seriesEntry{},
	}, nil
}

// applySchema creates the base tables, runs the additive column migrations, and
// drops a retired index. Idempotent: re-running an ALTER on an already-migrated
// table fails with "duplicate column name", the expected no-op. Any other error
// (locked, disk full, corrupt) is real and returned, because continuing would
// leave a column missing and every statement naming it failing forever. Open
// runs this twice when it has to rebuild a corrupt file.
func applySchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Migrate older DBs: per-run speed connection columns (TEXT), numeric per-run
	// columns (packet loss %, health flag, jitter, data volumes), latency under
	// load (idle baseline + per-phase loaded medians, then maxes), and the
	// per-sample address family (so series/uptime group by the family the monitor
	// assigned rather than inferring it from the target name).
	migrations := []struct{ table, col, typ string }{
		{"speed", "server_id", "TEXT"}, {"speed", "public_ipv4", "TEXT"},
		{"speed", "public_ipv6", "TEXT"}, {"speed", "isp", "TEXT"},
		{"speed", "isp_location", "TEXT"}, {"speed", "dns_ip", "TEXT"},
		{"speed", "dns_location", "TEXT"}, {"speed", "dns_provider", "TEXT"},
		{"speed", "cf_colo", "TEXT"}, {"speed", "exit_summary", "TEXT"},
		{"speed", "run_trigger", "TEXT"}, {"speed", "engine", "TEXT"},
		{"speed", "packet_loss", "REAL"}, {"speed", "healthy", "INTEGER"},
		{"speed", "jitter_ms", "REAL"},
		{"speed", "download_bytes", "INTEGER"}, {"speed", "upload_bytes", "INTEGER"},
		{"speed", "idle_ms", "REAL"}, {"speed", "loaded_down_ms", "REAL"},
		{"speed", "loaded_up_ms", "REAL"}, {"speed", "loaded_down_max_ms", "REAL"},
		{"speed", "loaded_up_max_ms", "REAL"},
		{"samples", "family", "TEXT"},
	}
	for _, m := range migrations {
		if _, err := db.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.col + ` ` + m.typ); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.col, err)
		}
	}
	// Drop the unused (target, ts) index from existing DBs: no query filters by
	// target, and the one per-target query pins idx_samples_ts - so it was pure
	// write/storage cost on the hot probe path.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_samples_target_ts`); err != nil {
		return fmt.Errorf("migrate: drop idx_samples_target_ts: %w", err)
	}
	return nil
}

// dbCorrupt reports whether err is SQLite signalling on-disk corruption or a
// non-database file - the unrecoverable class Open quarantines and rebuilds from,
// as opposed to a transient busy/locked or a real I/O fault.
func dbCorrupt(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "corrupt") ||
		strings.Contains(msg, "disk image")
}

// quarantineCorruptDB moves a corrupt database and its WAL/SHM sidecars aside to
// timestamped ".corrupt" files so a torn/truncated file can't crash-loop the
// daemon forever. Nothing is deleted - the operator can inspect or recover the
// quarantined data. Returns the path the main file was moved to (for the log).
func quarantineCorruptDB(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	moved := ""
	for _, sfx := range []string{"", "-wal", "-shm"} {
		src := path + sfx
		if _, err := os.Stat(src); err != nil {
			continue // sidecar may not exist
		}
		dst := src + "." + stamp + ".corrupt"
		if err := os.Rename(src, dst); err != nil {
			return "", fmt.Errorf("quarantine corrupt db %s: %w", src, err)
		}
		if sfx == "" {
			moved = dst
		}
	}
	return moved, nil
}

// secureDBFile tightens a database file (or WAL/SHM sidecar) to owner-only,
// tolerating a filesystem that genuinely cannot express perms (vfat/exFAT/CIFS,
// read-only mounts) - BUT only after verifying the file was not actually left
// group/world accessible. A permission error that leaves an existing DB readable
// by other local users (e.g. a root-created DB the daemon now runs too
// unprivileged to chmod, per the service hardening) must NOT be silently skipped:
// the DB holds the auth hash and webhook secrets, so an unfixable exposure is
// fatal and the operator has to correct ownership/perms. A missing sidecar or a
// mount that reports tight perms passes cleanly.
func secureDBFile(path string) error {
	err := osperm.SecureFile(path)
	if err == nil {
		return nil
	}
	if !securingSkippable(err) {
		return err
	}
	if exposed, known := osperm.GroupOrWorldAccessible(path); known && exposed {
		return fmt.Errorf("cannot secure %s to owner-only and it is still group/world accessible (holds the auth hash and webhook secrets): %w", path, err)
	}
	return nil
}

// securingSkippable reports whether a permission-securing error just means the
// filesystem cannot express owner-only permissions (a read-only mount, or an FS
// with no unix perms like vfat/exFAT/CIFS), or the target does not exist yet (a
// sidecar not written before first use). Securing is best-effort on such a
// filesystem: the DB still works, it just can't be tightened to 0600/0700. A
// real error (a genuine I/O fault) is not skippable and still fails startup.
func securingSkippable(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "read-only") ||
		strings.Contains(msg, "not supported") ||
		strings.Contains(msg, "not permitted")
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle for ad-hoc queries (used by tests).
func (s *Store) DB() *sql.DB { return s.db }

// Sample is one target's result within a probe round.
type Sample struct {
	TS        time.Time
	Target    string
	Family    string  // "ipv4" | "ipv6"
	LatencyMS float64 // ignored when Success is false
	Success   bool
}

// famExpr is the SQL for a sample's address family: the stored family column,
// falling back to the legacy "-v6" target-name suffix on rows written before
// the column existed. Yields "ipv4"/"ipv6".
const famExpr = `COALESCE(NULLIF(family,''), CASE WHEN target LIKE '%-v6' THEN 'ipv6' ELSE 'ipv4' END)`

// InsertSamples records one probe round's results in a single transaction (one
// commit/fsync instead of one per target).
func (s *Store) InsertSamples(ctx context.Context, sms []Sample) error {
	if len(sms) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, sm := range sms {
		var lat any
		if sm.Success {
			lat = sm.LatencyMS
		}
		var fam any
		if sm.Family != "" {
			fam = sm.Family
		}
		if _, err := stmt.ExecContext(ctx, sm.TS.Unix(), sm.Target, lat, util.B2I(sm.Success), fam); err != nil {
			tx.Rollback()
			recordDBErr(err)
			return err
		}
	}
	err = tx.Commit()
	recordDBErr(err)
	return err
}

// InsertDNS records one DNS-resolve-latency sample (one per probe round). ms is
// the cache-busted lookup time; ok=false stores a NULL latency with success=0.
func (s *Store) InsertDNS(ctx context.Context, ts time.Time, ms float64, ok bool) error {
	var lat any
	if ok {
		lat = ms
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO dns (ts, latency_ms, success) VALUES (?, ?, ?)`,
		ts.Unix(), lat, util.B2I(ok))
	recordDBErr(err)
	return err
}

// InsertPause records a monitoring-pause span [start, start+durationS): wall time
// during which no probe rounds ran - the master switch off, the latency toggle
// off, a closed schedule window, all address families off, or the process itself
// being down. UptimeSince subtracts these spans from its denominator so paused and
// unobserved time counts as neither up nor down; only OBSERVED time is scored.
// durationS <= 0 is a no-op. The monitor flushes spans incrementally (see Run) so
// a pause that never resumes is still reflected.
func (s *Store) InsertPause(ctx context.Context, start time.Time, durationS int64) error {
	if durationS <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pauses (ts, duration_s) VALUES (?, ?)`, start.Unix(), durationS)
	recordDBErr(err)
	return err
}

// LastObservedTS returns the newest plausible samples/events timestamp - the last
// moment the monitor is known to have been observing. The monitor uses it on
// startup to book the gap since then (process downtime) as an unobserved pause. ok
// is false when nothing has been recorded yet (a fresh install).
//
// It also considers the END of the newest persisted pause span (ts + duration_s):
// if the last thing recorded before shutdown was a pause (monitoring switched off,
// then the process exited), anchoring the startup gap at the older last sample/event
// would re-book the interval the pause span already covers - double-counting it out
// of the observed denominator.
func (s *Store) LastObservedTS(ctx context.Context) (int64, bool, error) {
	now := time.Now().Unix()
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT MAX(t) FROM (
			SELECT MAX(ts) AS t FROM samples WHERE ts >= ? AND ts <= ?
			UNION ALL
			SELECT MAX(ts) AS t FROM events WHERE ts >= ? AND ts <= ?
			UNION ALL
			SELECT MAX(ts + duration_s) AS t FROM pauses WHERE ts >= ? AND ts <= ?)`,
		plausibleEpoch, now, plausibleEpoch, now, plausibleEpoch, now).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// InsertEvent records an up/down transition. For 'up' events, durationS is the
// length of the outage that just ended; pass a negative value to store NULL.
func (s *Store) InsertEvent(ctx context.Context, ts time.Time, typ string, durationS int, detail string) error {
	var dur any
	if durationS >= 0 {
		dur = durationS
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (ts, type, duration_s, detail) VALUES (?, ?, ?, ?)`,
		ts.Unix(), typ, dur, detail)
	recordDBErr(err)
	return err
}

// plausibleEpoch is the earliest believable monitoring timestamp (2023-01-01
// UTC, before the project existed). RTC-less boards can boot with a near-epoch
// clock until NTP syncs; rows stamped that way must not anchor the uptime
// window, or the "all" window stretches to decades and reads ~100%.
const plausibleEpoch = 1672531200

// firstSeenKey is the settings key persisting when monitoring began (see
// monitoringSince).
const firstSeenKey = "first_seen_ts"

// monitoringSince returns when monitoring began - the anchor UptimeSince
// clamps window starts to. It takes the earliest plausible samples/events
// timestamp (cheap index seeks) and remembers the lowest value ever seen in
// settings: sample retention (default 30 days) keeps moving MIN(samples.ts)
// forward, which would silently turn every longer uptime window into a
// ~30-day one even though the year-long events retention still shows older
// outages on the heatmap. Timestamps outside [plausibleEpoch, now] (a wrong
// boot clock) are ignored and never persisted, so a bad anchor self-heals.
func (s *Store) monitoringSince(ctx context.Context, nowU int64) (int64, error) {
	anchor := nowU
	var first sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(t) FROM (
			SELECT MIN(ts) AS t FROM samples WHERE ts >= ?
			UNION ALL
			SELECT MIN(ts) AS t FROM events WHERE ts >= ?)`,
		plausibleEpoch, plausibleEpoch).Scan(&first); err != nil {
		return 0, err
	}
	if first.Valid && first.Int64 < anchor {
		anchor = first.Int64
	}
	var stored int64
	var v string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, firstSeenKey).Scan(&v); {
	case err == sql.ErrNoRows:
	case err != nil:
		return 0, err
	default:
		if n, e := strconv.ParseInt(v, 10, 64); e == nil && n >= plausibleEpoch && n <= nowU {
			stored = n
		}
	}
	if stored != 0 && stored < anchor {
		anchor = stored
	}
	// Remember a new lowest (or first valid) anchor. Best-effort: a write lost
	// to lock contention just retries on the next call.
	if anchor >= plausibleEpoch && anchor < nowU && (stored == 0 || anchor < stored) {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			firstSeenKey, strconv.FormatInt(anchor, 10)); err != nil {
			recordDBErr(err)
		}
	}
	return anchor, nil
}

// InstallBornAt returns the persisted install anchor (first_seen_ts) as a unix
// second, or 0 if it has not been recorded yet. A cheap single-key lookup; used as
// a stable per-install id (a fresh database gets a new one) to scope first-run UI.
func (s *Store) InstallBornAt(ctx context.Context) int64 {
	var v string
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = ?`, firstSeenKey).Scan(&v); err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// recScan memoizes one quorum-recovery search. Samples older than
// recFrontierSlack are immutable, so both outcomes past that margin are stable:
// a found recovery second after a given 'down', and a nothing-found-up-to-
// frontier scan.
type recScan struct {
	frontier int64 // scanned (down, frontier] finding nothing (when rec == 0)
	rec      int64 // first quorum-recovery second after the 'down'; 0 = none found yet
}

// recFrontierSlack keeps the recovery memo behind wall-now by this many seconds.
// A round is stamped at its start but committed up to a probe timeout later, and
// the clock can step back, so samples within this window may still appear or
// shift; they are re-scanned rather than memoized. Generous enough to cover both
// while keeping the re-scanned tail small.
const recFrontierSlack = 300

// firstQuorumRecovery returns the first samples second in (after, before]
// where any address family had a strict majority of its targets up - the live
// monitor's recovery rule. It bounds outages that never got a closing 'up' (a
// restart mid-outage leaves a dangling 'down'). The scan walks forward in
// growing chunks because recovery is almost always near the 'down', and both
// results and progress are memoized per `after`: UptimeWindows makes six
// UptimeSince calls per status refresh, and without the memo a months-old
// dangling 'down' would re-aggregate every newer sample on each one.
func (s *Store) firstQuorumRecovery(ctx context.Context, after, before int64) (int64, bool, error) {
	s.recMu.Lock()
	memo := s.recCache[after]
	gen := s.recGen
	s.recMu.Unlock()
	if memo.rec != 0 {
		if memo.rec <= before {
			return memo.rec, true, nil
		}
		return 0, false, nil
	}
	lo := after
	if memo.frontier > lo {
		lo = memo.frontier
	}
	// Keep the persisted frontier (and any cached recovery) behind this margin of
	// `before`: a probe round is stamped at its start but committed up to a probe
	// timeout later, and the clock can step back, so a sample with ts near `now`
	// may still appear or move. Rows within the margin are always re-scanned; the
	// cost is bounded (a few minutes of samples) and it self-heals write lag and
	// modest backward clock steps that an all-the-way-to-now memo would skip.
	safe := before - recFrontierSlack
	for chunk := int64(3600); lo < before; chunk *= 2 {
		hi := lo + chunk
		if hi > before {
			hi = before
		}
		var rec sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `
			SELECT MIN(sec) FROM (
				SELECT ts AS sec FROM samples WHERE ts > ? AND ts <= ?
				GROUP BY ts, `+famExpr+`
				HAVING SUM(success) * 2 > COUNT(*))`, lo, hi).Scan(&rec); err != nil {
			return 0, false, err
		}
		s.recMu.Lock()
		// If the read caches were invalidated (import/clear/prune) while this scan
		// ran unlocked, this result may reflect pre-change data: return it to the
		// caller but do NOT publish it, so the next call recomputes against the new
		// data instead of re-memoizing a stale answer that would then survive until
		// the next invalidation, cache reset, or restart.
		publish := s.recGen == gen
		if publish && len(s.recCache) > 256 {
			s.recCache = map[int64]recScan{}
		}
		if rec.Valid {
			if publish && rec.Int64 <= safe { // stable enough to remember
				s.recCache[after] = recScan{frontier: hi, rec: rec.Int64}
			}
			s.recMu.Unlock()
			return rec.Int64, true, nil
		}
		if publish {
			if f := hi; f <= safe {
				s.recCache[after] = recScan{frontier: f}
			} else if safe > memo.frontier {
				s.recCache[after] = recScan{frontier: safe}
			}
		}
		s.recMu.Unlock()
		lo = hi
	}
	return 0, false, nil
}

// newestSampleAt returns the newest samples timestamp, clamped to nowU so a
// future-stamped row (a boot clock years ahead) can't push it past now. ok is
// false when the table is empty. Used to bound a dangling outage at the last
// time monitoring actually observed the link (a pause writes no samples).
func (s *Store) newestSampleAt(ctx context.Context, nowU int64) (int64, bool, error) {
	var newest sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(ts) FROM samples`).Scan(&newest); err != nil {
		return 0, false, err
	}
	if !newest.Valid {
		return 0, false, nil
	}
	v := newest.Int64
	if v > nowU {
		v = nowU
	}
	return v, true, nil
}

// orphanGapDowntime sums the downtime from orphaned down->down gaps overlapping
// [sinceU, nowU]. Two 'down' events with no 'up' between them happen when the
// monitor restarts mid-outage: it comes back optimistically "online", re-detects
// the same outage, and writes a second 'down'. The link was down for the whole
// gap but no 'up' carries that duration, so it must be added explicitly.
//
// The gap is not always all downtime: when the link recovered while the monitor
// was off, the leading 'down' dangles over proven-up samples until the NEXT real
// outage writes the second 'down'. So each gap is bounded at the first quorum-
// recovery second after its leading 'down'.
//
// The LEAD scan is bounded to events from the last one at/before sinceU
// (idx_events_ts makes that MAX lookup O(log n)): a down->down pair straddling
// sinceU is still caught (its leading 'down' is that boundary event), while pairs
// wholly before the window - 0 overlap anyway - are skipped. UptimeSince and
// ResolvedOutagesSince share this so both report the same downtime.
//
// recovered is the count of gaps whose leading outage PROVABLY ended (a quorum
// recovery fell between the two 'down' events) and resolved within the window
// (rec > sinceU). Such a gap is a DISTINCT resolved outage with no closing 'up'
// event of its own, so ResolvedOutagesSince adds it to the outage count to agree
// with DowntimeByDay's heatmap tally; a gap with NO recovery between is the same
// outage re-detected after a restart and is not counted. UptimeSince ignores this.
func (s *Store) orphanGapDowntime(ctx context.Context, sinceU, nowU int64) (downtime int64, recovered int, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, next_ts
		FROM (
			SELECT ts, type,
			       LEAD(ts)   OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_ts,
			       LEAD(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_type
			FROM events WHERE ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE ts <= ?))
		WHERE type='down' AND next_type='down'`, sinceU)
	if err != nil {
		return 0, 0, err
	}
	// Collect the pairs before running the per-gap recovery lookups: the pool can
	// be pinned to one connection (":memory:"), where a query inside an open rows
	// scan would deadlock.
	var gaps [][2]int64
	for rows.Next() {
		var g [2]int64
		if err := rows.Scan(&g[0], &g[1]); err != nil {
			rows.Close()
			return 0, 0, err
		}
		gaps = append(gaps, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, g := range gaps {
		end := g[1]
		if rec, ok, err := s.firstQuorumRecovery(ctx, g[0], g[1]); err != nil {
			return 0, 0, err
		} else if ok && rec < end {
			end = rec
			if rec > sinceU { // the leading outage resolved inside the window: a distinct outage
				recovered++
			}
		}
		if end > nowU {
			end = nowU
		}
		start := g[0]
		if start < sinceU {
			start = sinceU
		}
		if end > start {
			// A pause inside this down->down gap is unobserved time, not downtime: the
			// link may have recovered with nobody watching. Subtract the pause overlap so
			// the gap contributes only OBSERVED downtime - matching how a completed
			// outage's duration_s already excludes paused time, and how UptimeSince
			// removes the same pause span from the denominator (else it double-counts).
			paused, err := s.pausedOverlap(ctx, start, end)
			if err != nil {
				return 0, 0, err
			}
			if dt := end - start - paused; dt > 0 {
				downtime += dt
			}
		}
	}
	return downtime, recovered, nil
}

// pausedOverlap returns the total recorded pause-span seconds overlapping [from,to].
func (s *Store) pausedOverlap(ctx context.Context, from, to int64) (int64, error) {
	if to <= from {
		return 0, nil
	}
	var p sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(MAX(0, MIN(ts + duration_s, ?) - MAX(ts, ?))), 0) FROM pauses`,
		to, from).Scan(&p); err != nil {
		return 0, err
	}
	return p.Int64, nil
}

// UptimeSince returns the fraction of time (0..1) the link was up over
// [since, now], derived from the debounced outage events so it matches the
// heatmap and outage table - not a per-target-probe success rate (which would,
// e.g., halve when one address family is down though the link stayed online by
// quorum). The window is clamped to when monitoring began (monitoringSince) so
// a freshly-started monitor isn't credited for time it never watched. Also far
// cheaper than scanning every sample row.
// UptimeSince returns the up-fraction over [since, now] and the observation
// coverage of that window (observed / wall, 0..1). The ratio's denominator is
// OBSERVED time - wall time minus recorded pause spans - so paused, scheduled-off,
// families-off and process-down time is neither up nor down (it would otherwise be
// credited as up). coverage == 0 means the window observed nothing (e.g. a monitor
// launched with probing off); callers should omit the uptime figure then. retention
// (> 0) clamps the window start to now-retention so a window can't reach past the
// point where outage events are pruned - beyond which downtime is lost but the wall
// window would persist, drifting the figure optimistic (the "all" window especially).
func (s *Store) UptimeSince(ctx context.Context, since time.Time, retention time.Duration) (ratio, coverage float64, err error) {
	nowU := time.Now().Unix()
	sinceU := since.Unix()

	// Clamp the window start to when monitoring actually began.
	first, err := s.monitoringSince(ctx, nowU)
	if err != nil {
		return 0, 0, err
	}
	if first > sinceU {
		sinceU = first
	}
	// And no earlier than the outage-retention horizon: events older than this are
	// pruned, so their downtime is gone; counting that wall time in the denominator
	// with no matching numerator is exactly the F3 drift. Within the horizon every
	// event survives, so numerator and denominator cover the same period.
	if retention > 0 {
		if floor := nowU - int64(retention.Seconds()); floor > sinceU {
			sinceU = floor
		}
	}
	window := nowU - sinceU
	if window <= 0 {
		return 1, 0, nil
	}

	// Downtime from completed outages overlapping the window. Each 'up' records
	// its outage's observed length in duration_s; anchor the span at the paired
	// 'down' event's timestamp (the true wall-clock start) and run it for
	// duration_s. Anchoring at up.ts-duration_s instead would mis-place an outage
	// whose observed length is shorter than its wall-clock gap because a suspend
	// or monitoring pause fell inside it. Fall back to up.ts-duration_s when no
	// 'down' precedes the 'up'. The scan is bounded to events from the last one
	// at/before sinceU, like the orphan scan below.
	var down sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(MAX(0, MIN(o_start + duration_s, ?) - MAX(o_start, ?))), 0)
		FROM (
			SELECT type, duration_s,
			       CASE WHEN LAG(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) = 'down'
			            THEN LAG(ts) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END)
			            ELSE ts - duration_s END AS o_start
			FROM events WHERE ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE ts <= ?))
		WHERE type='up' AND duration_s IS NOT NULL`,
		nowU, sinceU, sinceU).Scan(&down); err != nil {
		return 0, 0, err
	}
	downtime := down.Int64

	// Orphaned down->down gaps (a restart mid-outage wrote a second 'down' with no
	// closing 'up') carry no duration_s, so add them explicitly, each bounded at
	// its first quorum-recovery second. Shared with ResolvedOutagesSince (which also
	// uses the recovered-gap count; uptime needs only the downtime).
	gap, _, err := s.orphanGapDowntime(ctx, sinceU, nowU)
	if err != nil {
		return 0, 0, err
	}
	downtime += gap

	// Ongoing outage: if the latest event is a 'down', the link may still be
	// offline - OR it recovered without a closing 'up' (a restart mid-outage
	// leaves a dangling 'down'). The first second quorum returned (a strict
	// majority of either family's targets up - the live monitor's rule) proves
	// recovery; bound the outage there rather than running it to now and pinning
	// uptime low.
	var lastType string
	var lastTS int64
	switch err := s.db.QueryRowContext(ctx,
		`SELECT type, ts FROM events ORDER BY ts DESC, CASE type WHEN 'up' THEN 0 ELSE 1 END LIMIT 1`).Scan(&lastType, &lastTS); {
	case err == sql.ErrNoRows: // no events recorded → no outages
	case err != nil:
		return 0, 0, err
	default:
		if lastType == "down" {
			end := nowU
			if rec, ok, err := s.firstQuorumRecovery(ctx, lastTS, nowU); err != nil {
				return 0, 0, err
			} else if ok && rec < end {
				end = rec // quorum recovered here; not still down
			} else if newest, ok, err := s.newestSampleAt(ctx, nowU); err != nil {
				return 0, 0, err
			} else if ok && newest < end && newest > lastTS {
				// No quorum recovery in samples: either the link is still down (a
				// real outage keeps writing failed samples, so newest stays ~now and
				// this cap is a no-op) or monitoring was paused mid-outage (no samples
				// are written while paused). Bound the outage at the last observed
				// sample so an unwatched pause isn't booked as live downtime - the
				// eventual 'up' event excludes that paused stretch too, so the
				// transient figure now matches the final record.
				end = newest
			}
			start := lastTS
			if start < sinceU {
				start = sinceU
			}
			if end > start {
				// Subtract any pause overlapping the still-open outage: a pause bracketed
				// by observed down-samples (e.g. a schedule window closing while down) is
				// unobserved, not downtime - same rule as the completed/orphan branches.
				paused, err := s.pausedOverlap(ctx, start, end)
				if err != nil {
					return 0, 0, err
				}
				if dt := end - start - paused; dt > 0 {
					downtime += dt
				}
			}
		}
	}

	// Observed time = wall window minus recorded pause spans overlapping it. Pause
	// spans are disjoint episodes, each clamped to the window, so their sum never
	// exceeds the window. Downtime is already observed-only (every branch above
	// excludes the pause overlap), so it can't exceed observed time.
	pausedWindow, err := s.pausedOverlap(ctx, sinceU, nowU)
	if err != nil {
		return 0, 0, err
	}
	observed := window - pausedWindow
	if observed <= 0 {
		return 1, 0, nil // nothing observed in this window; coverage 0 tells the caller to omit
	}
	if downtime > observed {
		downtime = observed
	}
	return 1 - float64(downtime)/float64(observed), float64(observed) / float64(window), nil
}

// Event is a recorded up/down transition.
type Event struct {
	TS          int64  `json:"ts"` // unix seconds (matches the other endpoints)
	Type        string `json:"type"`
	DurationS   int    `json:"duration_s"`
	HasDuration bool   `json:"has_duration"`
}

// EventCount returns the total number of recorded transition events.
func (s *Store) EventCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n)
	return n, err
}

// EventsPage returns a page of transitions, newest first.
func (s *Store) EventsPage(ctx context.Context, limit, offset int) ([]Event, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, type, duration_s FROM events ORDER BY ts DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var ts int64
		var typ string
		var dur sql.NullInt64
		if err := rows.Scan(&ts, &typ, &dur); err != nil {
			return nil, err
		}
		out = append(out, Event{
			TS:          ts,
			Type:        typ,
			DurationS:   int(dur.Int64),
			HasDuration: dur.Valid,
		})
	}
	return out, rows.Err()
}

// TargetLatency is the most recent reading for a single target.
type TargetLatency struct {
	Target    string  `json:"target"`
	LatencyMS float64 `json:"latency_ms"`
	Success   bool    `json:"success"`
	TS        int64   `json:"ts"`
}

// LatestPerTarget returns the newest sample per recently-probed target: those
// whose newest sample is within grace of the newest sample overall. Anchoring
// the cutoff to the table's own max (not wall clock) means pausing monitoring
// keeps showing the last round, while a target that stopped being probed (e.g.
// IPv6 toggled off live) drops out after grace instead of showing a frozen
// reading until retention prunes it. The anchor is clamped to wall now so rows
// stamped in the future (a boot clock years ahead, later stepped back) can't
// pin the window and freeze the pills on pre-step readings. grace <= 0
// disables the cutoff.
//
// A future-dated row is also excluded outright (ts <= now + metricsFutureSkew):
// the wall-now clamp above only stops a future row shifting the window START, but
// within the window a future row for a still-live target would still win ORDER BY
// ts DESC and show a not-yet-real reading until the clock caught up. The small
// skew tolerance keeps a slightly-fast importer's just-now rows visible.
func (s *Store) LatestPerTarget(ctx context.Context, grace time.Duration) ([]TargetLatency, error) {
	now := time.Now().Unix()
	cut := `0`
	args := []any{}
	if grace > 0 {
		cut = `MIN((SELECT COALESCE(MAX(ts), 0) FROM samples), ?) - ?`
		args = append(args, now, int64(grace.Seconds()))
	}
	args = append(args, now+int64(metricsFutureSkew.Seconds()))
	// Range-seek the recent window via idx_samples_ts, then take the newest row
	// per target from that small slice. The old form scanned the whole table for
	// MAX(ts) per target on every status poll (~130ms at a week of samples); this
	// is flat (~130µs) regardless of table size. A target last probed before the
	// cutoff has no row in the window and correctly drops out.
	//
	// INDEXED BY pins the ts-range index: without table stats the planner can't
	// tell the range scan beats reading every row. It is a hard constraint, not a
	// hint - if a migration renames or drops idx_samples_ts without updating this
	// query, SQLite errors at plan time and the status endpoint fails. Keep the
	// index name in sync.
	rows, err := s.db.QueryContext(ctx, `
		SELECT target, COALESCE(latency_ms, 0), success, ts FROM (
			SELECT target, latency_ms, success, ts,
			       ROW_NUMBER() OVER (PARTITION BY target ORDER BY ts DESC) AS rn
			FROM samples INDEXED BY idx_samples_ts WHERE ts >= `+cut+` AND ts <= ?
		) WHERE rn = 1
		ORDER BY target`, args...)
	if err != nil {
		recordDBErr(err) // status/metrics swallow this; surface it on the db.* health counters
		return nil, err
	}
	defer rows.Close()
	var out []TargetLatency
	for rows.Next() {
		var tl TargetLatency
		var succ int
		if err := rows.Scan(&tl.Target, &tl.LatencyMS, &succ, &tl.TS); err != nil {
			return nil, err
		}
		tl.Success = succ == 1
		out = append(out, tl)
	}
	return out, rows.Err()
}

// SeriesPoint is one bucket collapsed across targets: the best (lowest) latency
// among successful targets, plus the quorum online flag.
type SeriesPoint struct {
	TS        int64    `json:"t"`
	LatencyMS *float64 `json:"lat"` // nil when the bucket was offline
	Online    bool     `json:"online"`
	DNSms     *float64 `json:"dns,omitempty"` // mean DNS-resolve time in the bucket; nil when not measured
}

// seriesKey identifies one cacheable Series aggregate: the window start and end
// normalized to their bucket, the bucket width, and the exclude set.
// untilBkt is part of the key and must stay that way: without it a fixed
// historical window and a rolling one that happen to share a start bucket and
// bucket width collide in the map and serve each other data.
type seriesKey struct {
	sinceBkt  int64
	untilBkt  int64 // 0 = open ended
	bucketSec int
	exclude   string
}

// seriesEntry caches one computed aggregate; mu single-flights concurrent
// viewers of the same key so only one runs the scan.
type seriesEntry struct {
	mu      sync.Mutex
	expires time.Time
	pts     []SeriesPoint
}

// Series returns latency points since the given time, oldest first, bucketed
// into bucketSec-wide windows so long ranges stay small. Each point is the
// lowest latency in the bucket plus an online flag. Online uses per-family
// quorum (any address family with a majority of its targets up) - matching the
// monitor/heatmap - so one address family being down doesn't paint a false
// outage band on a dual-stack host. Family comes from famExpr.
//
// Wide windows are cached: they re-aggregate the raw samples table (seconds of
// work at months of retention) on every chart poll of every open dashboard,
// yet the result only changes materially once per bucket. Buckets of a minute
// or more are served from a quarter-bucket TTL cache, and concurrent viewers
// of the same window share one scan. Narrow (cheap) windows stay fully live.
// Callers must not mutate the returned slice.
func (s *Store) Series(ctx context.Context, since, until time.Time, bucketSec int, excludeTargets []string) ([]SeriesPoint, error) {
	if bucketSec < 1 {
		bucketSec = 1
	}
	if bucketSec < 60 {
		return s.seriesQuery(ctx, since, until, bucketSec, excludeTargets)
	}
	ex := append([]string(nil), excludeTargets...)
	sort.Strings(ex)
	// Floor the start to its bucket boundary and run the aggregate from THERE, so a
	// cache miss computes exactly the window the key names. The key already floors
	// the start (below), so two exact starts in the same bucket share one entry -
	// but the SQL aggregate keyed off the caller's exact start would build a
	// different leading (partial) bucket for each, serving whichever raced in first.
	// Aligning the query to the same boundary makes cached and fresh agree; buckets
	// are ts/bucket-aligned anyway, so this just completes the leading bucket.
	sinceBkt := since.Unix() / int64(bucketSec)
	since = time.Unix(sinceBkt*int64(bucketSec), 0)
	// The END is keyed EXACTLY, not floored to its bucket like the start. The
	// start is floored because a rolling window slides every second and flooring
	// is what makes it cacheable at all; a fixed end never moves, so flooring it
	// buys no hits and instead aliases two windows whose ends fall in the same
	// bucket onto one entry - and their trailing partial bucket aggregates a
	// different set of rows, so the second caller is served the first ones data.
	untilBkt := int64(0)
	if !until.IsZero() {
		untilBkt = until.Unix()
	}
	key := seriesKey{
		sinceBkt:  sinceBkt,
		untilBkt:  untilBkt,
		bucketSec: bucketSec,
		exclude:   strings.Join(ex, ","),
	}
	s.seriesMu.Lock()
	e, ok := s.seriesCache[key]
	if !ok {
		if len(s.seriesCache) > 32 {
			s.seriesCache = map[seriesKey]*seriesEntry{}
		}
		e = &seriesEntry{}
		s.seriesCache[key] = e
	}
	s.seriesMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	if time.Now().Before(e.expires) {
		return e.pts, nil
	}
	pts, err := s.seriesQuery(ctx, since, until, bucketSec, excludeTargets)
	if err != nil {
		return nil, err
	}
	e.pts = pts
	// The quarter-bucket TTL rests on "the aggregate only changes materially once
	// per bucket" - true for a mature window, but on a young install a wide window
	// is one trailing partial bucket that changes every probe round, so a long TTL
	// would pin a near-empty first-run chart (and a missing DNS line) for up to
	// ~87min on a 1y range. Cap the trailing (open-ended) window at 30s, matching
	// the status pills' aggregate TTL; a fixed historical window is safe to cache
	// for its full bucket. Never pin an empty result at all, so a fresh DB re-
	// aggregates as soon as its first samples land.
	ttl := time.Duration(bucketSec) * time.Second / 4
	// A window whose end is still in the FUTURE is fixed but NOT historical: new
	// samples keep entering it, so treat it like an open-ended window and cap the
	// TTL. The UI accepts future-ended spans (a typed range like "jul 1 to dec 31"
	// clamps to now+366d, not now), so without this a future-ended range would
	// pin new samples out of the chart for up to bucketSec/4 (~88 min at 1y). Once
	// the end passes into the past the window becomes genuinely historical and the
	// full bucket TTL is valid again.
	if (until.IsZero() || until.After(time.Now())) && ttl > 30*time.Second {
		ttl = 30 * time.Second
	}
	if len(pts) == 0 {
		e.expires = time.Time{}
	} else {
		e.expires = time.Now().Add(ttl)
	}
	return pts, nil
}

// seriesQuery runs the actual Series aggregate (see Series for semantics).
func (s *Store) seriesQuery(ctx context.Context, since, until time.Time, bucketSec int, excludeTargets []string) ([]SeriesPoint, error) {
	// excludeTargets drops targets from the latency MIN (the "lowest" line) only;
	// online/outage detection below still counts every target, since connectivity
	// is global truth, not a per-user display filter. Placeholders keep it
	// injection-safe.
	latFilter := ""
	args := []any{bucketSec, bucketSec}
	for _, t := range excludeTargets {
		latFilter += "?,"
		args = append(args, t)
	}
	if latFilter != "" {
		latFilter = " AND target NOT IN (" + latFilter[:len(latFilter)-1] + ")"
	}
	// An absolute window bounds BOTH aggregates below. Bounding only the samples
	// one would leave the DNS line on a different window than the latency line it
	// is drawn beside - wrong data rather than a visible break.
	upper := ""
	if !until.IsZero() {
		upper = " AND ts < ?"
	}
	args = append(args, since.Unix())
	if !until.IsZero() {
		args = append(args, until.Unix())
	}
	// The DNS line rides the same buckets via a LEFT JOIN on a parallel aggregate
	// of the dns table (mean resolve time per bucket), so the chart plots ping +
	// DNS on one axis.
	args = append(args, bucketSec, bucketSec, since.Unix())
	if !until.IsZero() {
		args = append(args, until.Unix())
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ping.bts, MIN(ping.lat) AS lat, MAX(ping.fam_online) AS online, d.dns
		FROM (
			SELECT (ts / ?) * ? AS bts,
			       `+famExpr+` AS fam,
			       MIN(CASE WHEN success = 1`+latFilter+` THEN latency_ms END) AS lat,
			       CASE WHEN SUM(success) * 2 > COUNT(*) THEN 1 ELSE 0 END AS fam_online
			FROM samples
			WHERE ts >= ?`+upper+`
			GROUP BY bts, fam
		) ping
		LEFT JOIN (
			SELECT (ts / ?) * ? AS bts, AVG(CASE WHEN success = 1 THEN latency_ms END) AS dns
			FROM dns WHERE ts >= ?`+upper+` GROUP BY bts
		) d ON d.bts = ping.bts
		GROUP BY ping.bts
		ORDER BY ping.bts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		var lat, dns sql.NullFloat64
		var online int
		if err := rows.Scan(&p.TS, &lat, &online, &dns); err != nil {
			return nil, err
		}
		if lat.Valid {
			v := lat.Float64
			p.LatencyMS = &v
		}
		if dns.Valid {
			v := dns.Float64
			p.DNSms = &v
		}
		p.Online = online == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// SpeedSample is one completed speedtest plus the connection context it ran in.
type SpeedSample struct {
	TS          int64    `json:"ts"`
	DownMbps    float64  `json:"down_mbps"`
	UpMbps      float64  `json:"up_mbps"`
	PingMS      float64  `json:"ping_ms"`
	JitterMS    *float64 `json:"jitter_ms,omitempty"`
	Server      string   `json:"server"`
	ServerID    string   `json:"server_id,omitempty"`
	PublicIPv4  string   `json:"public_ipv4,omitempty"`
	PublicIPv6  string   `json:"public_ipv6,omitempty"`
	ISP         string   `json:"isp,omitempty"`
	ISPLocation string   `json:"isp_location,omitempty"`
	DNSIP       string   `json:"dns_ip,omitempty"`
	DNSProvider string   `json:"dns_provider,omitempty"`
	DNSLocation string   `json:"dns_location,omitempty"`
	// PacketLoss is the loss percentage (0..100); nil when unmeasurable. Healthy
	// is the verdict against the speed thresholds in effect at run time; nil when
	// no thresholds were configured, or when the run measured nothing the active
	// thresholds cover (so the verdict could not be evaluated).
	PacketLoss *float64 `json:"packet_loss,omitempty"`
	Healthy    *bool    `json:"healthy,omitempty"`
	// DownBytes/UpBytes are the data volumes the test transferred; nil on rows
	// recorded before data-usage tracking existed.
	DownBytes *int64 `json:"download_bytes,omitempty"`
	UpBytes   *int64 `json:"upload_bytes,omitempty"`
	// CFColo is the Cloudflare PoP and ExitSummary the exit-router → handoff path,
	// captured at run time (empty on older rows).
	CFColo      string `json:"cf_colo,omitempty"`
	ExitSummary string `json:"exit_summary,omitempty"`
	// Trigger is what started the test: scheduled|manual|reconnect|startup|degraded (empty
	// on older rows).
	Trigger string `json:"trigger,omitempty"`
	// Engine is the test backend: ookla|iperf3 (empty on older rows = ookla).
	// Lets the chart mark engine switchovers.
	Engine string `json:"engine,omitempty"`
	// Latency under load: idle baseline and per-phase loaded medians (ms), same
	// method/target so loaded-minus-idle is bufferbloat. nil when unmeasurable or
	// on older rows. The Max fields are the worst single sample per phase (the
	// spike, not the typical).
	IdleMS          *float64 `json:"idle_ms,omitempty"`
	LoadedDownMS    *float64 `json:"loaded_down_ms,omitempty"`
	LoadedUpMS      *float64 `json:"loaded_up_ms,omitempty"`
	LoadedDownMaxMS *float64 `json:"loaded_down_max_ms,omitempty"`
	LoadedUpMaxMS   *float64 `json:"loaded_up_max_ms,omitempty"`
}

const speedCols = `ts, down_mbps, up_mbps, ping_ms, COALESCE(server,''), COALESCE(server_id,''),
	COALESCE(public_ipv4,''), COALESCE(public_ipv6,''), COALESCE(isp,''),
	COALESCE(isp_location,''), COALESCE(dns_ip,''), COALESCE(dns_provider,''), COALESCE(dns_location,''),
	packet_loss, healthy, jitter_ms, download_bytes, upload_bytes,
	COALESCE(cf_colo,''), COALESCE(exit_summary,''), COALESCE(run_trigger,''),
	idle_ms, loaded_down_ms, loaded_up_ms, loaded_down_max_ms, loaded_up_max_ms, COALESCE(engine,'')`

// nzFinite returns a float column's value, or 0 when NULL or non-finite
// (NaN/±Inf) - the safe sentinel for always-present scalars.
func nzFinite(n sql.NullFloat64) float64 {
	if !n.Valid || math.IsNaN(n.Float64) || math.IsInf(n.Float64, 0) {
		return 0
	}
	return n.Float64
}

// ptrFinite returns a pointer to a float column's value, or nil when NULL or
// non-finite - so an optional metric reads as "unknown" rather than poisoning a
// later json.Encode.
func ptrFinite(n sql.NullFloat64) *float64 {
	if !n.Valid || math.IsNaN(n.Float64) || math.IsInf(n.Float64, 0) {
		return nil
	}
	v := n.Float64
	return &v
}

func scanSpeed(sc interface{ Scan(...any) error }) (SpeedSample, error) {
	var sp SpeedSample
	// Every float column scans through NullFloat64 and is sanitized: a non-finite
	// measurement (a sub-ms speedtest sample can divide to NaN/±Inf) lands in the
	// DB as NULL (NaN, coerced by the driver) or a real ±Inf. Scanning NULL into a
	// plain float64 errors, and a stored ±Inf later breaks json.Encode on
	// /api/speed and emits "+Inf" in CSV - either way one poisoned row would wedge
	// EVERY speed read. nzFinite/ptrFinite collapse both to 0 / nil.
	var down, up, ping sql.NullFloat64
	var ploss, jitter, idle, loadDown, loadUp, loadDownMax, loadUpMax sql.NullFloat64
	var healthy, downB, upB sql.NullInt64
	err := sc.Scan(&sp.TS, &down, &up, &ping, &sp.Server, &sp.ServerID,
		&sp.PublicIPv4, &sp.PublicIPv6, &sp.ISP, &sp.ISPLocation, &sp.DNSIP, &sp.DNSProvider, &sp.DNSLocation,
		&ploss, &healthy, &jitter, &downB, &upB, &sp.CFColo, &sp.ExitSummary, &sp.Trigger,
		&idle, &loadDown, &loadUp, &loadDownMax, &loadUpMax, &sp.Engine)
	sp.DownMbps, sp.UpMbps, sp.PingMS = nzFinite(down), nzFinite(up), nzFinite(ping)
	sp.IdleMS = ptrFinite(idle)
	sp.LoadedDownMS = ptrFinite(loadDown)
	sp.LoadedUpMS = ptrFinite(loadUp)
	sp.LoadedDownMaxMS = ptrFinite(loadDownMax)
	sp.LoadedUpMaxMS = ptrFinite(loadUpMax)
	sp.PacketLoss = ptrFinite(ploss)
	sp.JitterMS = ptrFinite(jitter)
	if healthy.Valid {
		b := healthy.Int64 == 1
		sp.Healthy = &b
	}
	if downB.Valid {
		v := downB.Int64
		sp.DownBytes = &v
	}
	if upB.Valid {
		v := upB.Int64
		sp.UpBytes = &v
	}
	return sp, err
}

// InsertSpeed records a speedtest result and the connection it ran in.
// maxSpeedBytesPerRun bounds a single speedtest's recorded byte count: 1 PiB is
// orders of magnitude above any real run yet far below where a SUM over the whole
// speed table could overflow int64 (see InsertSpeed).
const maxSpeedBytesPerRun = 1 << 50

// clampSpeedBytes bounds an optional byte count to [0, maxSpeedBytesPerRun]. nil
// (direction not measured) passes through unchanged.
func clampSpeedBytes(v *int64) *int64 {
	if v == nil {
		return nil
	}
	switch {
	case *v < 0:
		z := int64(0)
		return &z
	case *v > maxSpeedBytesPerRun:
		m := int64(maxSpeedBytesPerRun)
		return &m
	default:
		return v
	}
}

func (s *Store) InsertSpeed(ctx context.Context, sp SpeedSample) error {
	// Optional measurements unwrap *T -> any (NULL when nil) via ptrArg below;
	// healthy is the exception (bool -> 1/0).
	var healthy any
	if sp.Healthy != nil {
		healthy = util.B2I(*sp.Healthy)
	}
	// A remote/malicious iperf3 server (or a corrupt result) can report a byte count
	// unrelated to its rate - bytes and bits_per_second are independent JSON fields.
	// Left unbounded it flows into SpeedDataUsage's integer SUM, where SQLite raises
	// "integer overflow" and HARD-BREAKS the whole data-usage query (not merely skews
	// it). Clamp each row far above any real speedtest yet far below an int64 SUM
	// overflow.
	downBytes := clampSpeedBytes(sp.DownBytes)
	upBytes := clampSpeedBytes(sp.UpBytes)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, server_id,
			public_ipv4, public_ipv6, isp, isp_location, dns_ip, dns_provider, dns_location,
			packet_loss, healthy, jitter_ms, download_bytes, upload_bytes, cf_colo, exit_summary, run_trigger,
			idle_ms, loaded_down_ms, loaded_up_ms, loaded_down_max_ms, loaded_up_max_ms, engine)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sp.TS, sp.DownMbps, sp.UpMbps, sp.PingMS, sp.Server, sp.ServerID,
		sp.PublicIPv4, sp.PublicIPv6, sp.ISP, sp.ISPLocation, sp.DNSIP, sp.DNSProvider, sp.DNSLocation,
		ptrArg(sp.PacketLoss), healthy, ptrArg(sp.JitterMS), ptrArg(downBytes), ptrArg(upBytes), sp.CFColo, sp.ExitSummary, sp.Trigger,
		ptrArg(sp.IdleMS), ptrArg(sp.LoadedDownMS), ptrArg(sp.LoadedUpMS), ptrArg(sp.LoadedDownMaxMS), ptrArg(sp.LoadedUpMaxMS), sp.Engine)
	recordDBErr(err)
	return err
}

// ResolvedOutagesSince returns the number of outages that resolved after since
// and their total downtime in seconds. An outage resolves with an 'up' event
// carrying its duration; one still ongoing has no 'up' yet and isn't counted
// (its duration wouldn't be final). A direct aggregate, so it can't undercount
// when a window holds very many outages. The downtime is clamped to the [since,
// now] window the same way UptimeSince clamps it, so an outage that began before
// `since` contributes only its in-window portion - keeping the digest's downtime
// figure consistent with the uptime% beside it.
func (s *Store) ResolvedOutagesSince(ctx context.Context, since int64) (count, downtimeS int, err error) {
	nowU := time.Now().Unix()
	var c, d sql.NullInt64
	if err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(MAX(0, MIN(o_start + duration_s, ?) - MAX(o_start, ?))), 0)
		FROM (
			SELECT ts AS up_ts, type, duration_s,
			       CASE WHEN LAG(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) = 'down'
			            THEN LAG(ts) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END)
			            ELSE ts - duration_s END AS o_start
			FROM events)
		WHERE type='up' AND duration_s IS NOT NULL AND up_ts > ?`, nowU, since, since).Scan(&c, &d); err != nil {
		return 0, 0, err
	}
	// Add the orphaned down->down gap downtime UptimeSince also books (a restart
	// mid-outage leaves a dangling 'down' whose pre-restart stretch carries no
	// duration_s), so the digest's downtime stays consistent with the uptime%
	// beside it. A gap whose leading outage PROVABLY recovered (a quorum recovery
	// fell between the two downs) is a distinct resolved outage with no closing 'up'
	// of its own, so add it to the count too - otherwise the digest under-reports
	// versus the heatmap after a restart interruption (see DowntimeByDay, which
	// counts that re-detection as its own outage). A gap with no recovery between is
	// the SAME outage re-detected and is not counted.
	gap, gapOutages, err := s.orphanGapDowntime(ctx, since, nowU)
	if err != nil {
		return 0, 0, err
	}

	// A trailing 'down' that recovered WITHOUT a closing 'up' (the monitor
	// restarts optimistically online, so an outage that cleared while it was
	// stopped never gets its 'up' written) is a real resolved outage. UptimeSince
	// already books it as downtime via the same firstQuorumRecovery probe, but the
	// count/duration query above is keyed on 'up' rows and would miss it entirely -
	// leaving the digest reading "no outages" on a day the heatmap shows one (see
	// DowntimeByDay, which reconciles the same trailing down). Mirror UptimeSince's
	// trailing-down branch: if the newest event is a 'down' with a provable quorum
	// recovery in (down, now] that lands inside the window, count it once and add
	// its in-window downtime.
	var lastType string
	var lastTS int64
	switch e := s.db.QueryRowContext(ctx,
		`SELECT type, ts FROM events ORDER BY ts DESC, CASE type WHEN 'up' THEN 0 ELSE 1 END LIMIT 1`).Scan(&lastType, &lastTS); {
	case e == sql.ErrNoRows: // no events → nothing to reconcile
	case e != nil:
		return 0, 0, e
	default:
		if lastType == "down" {
			if rec, ok, e := s.firstQuorumRecovery(ctx, lastTS, nowU); e != nil {
				return 0, 0, e
			} else if ok && rec > since {
				gapOutages++
				start := lastTS
				if start < since {
					start = since
				}
				if rec > start {
					gap += rec - start
				}
			}
		}
	}
	return int(c.Int64) + gapOutages, int(d.Int64) + int(gap), nil
}

// TableCounts reports the row count of each data table.
func (s *Store) TableCounts(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []string{"samples", "speed", "events", "settings"} {
		var n int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t).Scan(&n); err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// LatestConnInfo returns the most recent speedtest run that recorded a public
// IP or ISP - a persisted last-known connection context the Connection panel
// falls back on when a live lookup is failing (e.g. an outage). nil when none.
func (s *Store) LatestConnInfo(ctx context.Context) (*SpeedSample, error) {
	sp, err := scanSpeed(s.db.QueryRowContext(ctx,
		`SELECT `+speedCols+` FROM speed
		 WHERE COALESCE(isp,'') <> '' OR COALESCE(public_ipv4,'') <> ''
		 ORDER BY ts DESC LIMIT 1`))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sp, nil
}

// LatestSpeed returns the most recent speedtest, or nil if none exist.
func (s *Store) LatestSpeed(ctx context.Context) (*SpeedSample, error) {
	// ts <= now + skew: a future-dated import must not win "latest" and pin the
	// speed pills/metrics on a not-yet-real run (see metricsFutureSkew).
	sp, err := scanSpeed(s.db.QueryRowContext(ctx,
		`SELECT `+speedCols+` FROM speed WHERE ts <= ? ORDER BY ts DESC LIMIT 1`,
		time.Now().Add(metricsFutureSkew).Unix()))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		recordDBErr(err) // status/metrics swallow this; surface it on the db.* health counters
		return nil, err
	}
	return &sp, nil
}

// SpeedHistory returns speedtests since the given time, oldest first, with no
// upper bound.
func (s *Store) SpeedHistory(ctx context.Context, since time.Time) ([]SpeedSample, error) {
	return s.SpeedHistoryRange(ctx, since, time.Time{})
}

// SpeedHistoryRange returns speedtests in [since, until), oldest first. A zero
// until means no upper bound, which is what every caller outside the chart
// wants. The interval is half-open so a run stamped exactly on a boundary
// belongs to one side only - speedtests land on schedule, so a run at local
// midnight is ordinary, and a closed bound would show it in both neighbouring
// day ranges.
func (s *Store) SpeedHistoryRange(ctx context.Context, since, until time.Time) ([]SpeedSample, error) {
	q := `SELECT ` + speedCols + ` FROM speed WHERE ts >= ?`
	args := []any{since.Unix()}
	if !until.IsZero() {
		q += ` AND ts < ?`
		args = append(args, until.Unix())
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY ts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpeedSample
	for rows.Next() {
		sp, err := scanSpeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SpeedHistoryDescFunc streams every recorded speedtest newest-first to fn, one
// row at a time, without materializing the whole table - so the CSV export runs
// at O(1) memory over a full history instead of building the entire slice. fn is
// called per row; returning an error stops the scan and propagates it.
func (s *Store) SpeedHistoryDescFunc(ctx context.Context, fn func(SpeedSample) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT `+speedCols+` FROM speed ORDER BY ts DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		sp, err := scanSpeed(rows)
		if err != nil {
			return err
		}
		if err := fn(sp); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SpeedCount returns the total number of recorded speedtests.
func (s *Store) SpeedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed`).Scan(&n)
	return n, err
}

// SpeedRunOffset returns the zero-based position of the run with the given
// timestamp within the newest-first ordering (i.e. how many runs are newer),
// which the UI divides by page size to jump straight to a run's row when a
// chart point is clicked.
func (s *Store) SpeedRunOffset(ctx context.Context, ts int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE ts > ?`, ts).Scan(&n)
	return n, err
}

// DeleteSpeed removes a single speedtest run by its timestamp - the run's
// identity throughout the UI/API (the same key SpeedRunOffset and the
// chart<->table link use). Returns rows removed: 0 when no run matched (already
// gone), which the caller treats as an idempotent no-op, not an error. Runs are
// serialized, so ts is effectively unique; a same-second collision would delete
// both, consistent with ts-as-identity everywhere else.
func (s *Store) DeleteSpeed(ctx context.Context, ts int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM speed WHERE ts = ?`, ts)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteOutage removes one resolved outage. ts identifies it by its closing
// 'up' event - the row the outages table and the uptime windows key off. The
// 'down' events that belong to it go too: leaving one behind would strand a
// dangling 'down' that the uptime math re-derives as an orphaned outage
// (making downtime look the same or WORSE after the delete). The sweep walks
// back from the 'up' the same way the uptime windows pair events: a restart
// mid-outage re-detects it and writes a second 'down' (same outage - swept),
// but a down->down gap containing a quorum RECOVERY is a distinct earlier
// outage that ended while the monitor was off - kept. The sweep is decided
// BEFORE the transaction: reads inside it would either self-deadlock the
// single-connection :memory: pool (firstQuorumRecovery queries the pool) or
// recreate the deferred read-then-write snapshot upgrade that fails with
// SQLITE_BUSY_SNAPSHOT under the daemon's sample commits (see
// SetSettingsDiff). Events are append-only (the monitor never rewrites them),
// and every delete below targets exact rows with its own guard, so the
// pre-computed list can't touch anything it shouldn't. Returns rows removed;
// 0 means no such resolved outage, an idempotent no-op.
func (s *Store) DeleteOutage(ctx context.Context, ts int64) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts FROM events WHERE type = 'down' AND ts <= ?1
		  AND ts > COALESCE((SELECT MAX(ts) FROM events WHERE type = 'up' AND ts < ?1), 0)
		ORDER BY ts DESC`, ts)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	var downs []int64
	for rows.Next() {
		var d int64
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			recordDBErr(err)
			return 0, err
		}
		downs = append(downs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return 0, err
	}
	var sweep []int64
	for i, d := range downs {
		if i > 0 {
			// A quorum recovery between this down and the next-newer one means
			// this down's outage ended on its own - it isn't part of the one
			// being deleted (mirrors UptimeSince's down->down gap bounding).
			rec, ok, err := s.firstQuorumRecovery(ctx, d, downs[i-1])
			if err != nil {
				recordDBErr(err)
				return 0, err
			}
			if ok && rec < downs[i-1] {
				break
			}
		}
		sweep = append(sweep, d)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	defer tx.Rollback()
	// First statement is the guarded write; no such 'up' = idempotent no-op
	// (and nothing else is touched).
	res, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE ts = ? AND type = 'up' AND duration_s IS NOT NULL AND duration_s >= 0`, ts)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, nil
	}
	for _, d := range sweep {
		res, err := tx.ExecContext(ctx, `DELETE FROM events WHERE type = 'down' AND ts = ?`, d)
		if err != nil {
			recordDBErr(err)
			return 0, err
		}
		nd, _ := res.RowsAffected()
		n += nd
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return 0, err
	}
	return n, nil
}

// SpeedAvgBytes returns the average download and upload bytes per speedtest over
// the most recent runs that recorded data volumes (0 when none) - used to
// estimate ongoing data usage at a given interval. The error lets the caller
// tell a real DB failure from a genuine zero, so it won't cache the zero.
func (s *Store) SpeedAvgBytes(ctx context.Context) (avgDown, avgUp int64, err error) {
	// Average each direction INDEPENDENTLY over the most recent runs that recorded
	// that direction's byte volume. Requiring BOTH columns non-NULL (the previous
	// form) dropped every download-only or upload-only run entirely, so for the
	// data-conserving users who run a single direction the estimate fell back to a
	// fabricated constant. A per-direction average uses each partial run for the
	// direction it actually measured.
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COALESCE(CAST(AVG(download_bytes) AS INTEGER), 0)
		     FROM (SELECT download_bytes FROM speed
		           WHERE download_bytes IS NOT NULL ORDER BY ts DESC LIMIT 20)),
		  (SELECT COALESCE(CAST(AVG(upload_bytes) AS INTEGER), 0)
		     FROM (SELECT upload_bytes FROM speed
		           WHERE upload_bytes IS NOT NULL ORDER BY ts DESC LIMIT 20))`).Scan(&avgDown, &avgUp)
	if err != nil {
		recordDBErr(err)
		return 0, 0, err
	}
	return avgDown, avgUp, nil
}

// Uptime holds the up-fraction (0..1) over several time windows (mirrors the
// DataUsage windows, so the uptime pill offers the same set as the data pill).
type Uptime struct {
	H6  float64 `json:"6h"`
	H24 float64 `json:"24h"`
	D7  float64 `json:"7d"`
	D30 float64 `json:"30d"`
	Y1  float64 `json:"1y"`
	All float64 `json:"all"`
}

// UptimeWindows computes the up-fraction for each window relative to now via
// UptimeSince (each clamps to the observed period); "all" runs from epoch so it
// clamps to when monitoring began. Returns the first error encountered.
// UptimeWindows computes the up-fraction for each window relative to now, plus the
// matching observation coverage (0..1) of each window - a window whose coverage is 0
// observed nothing and its ratio should be omitted. retention clamps the long windows
// to retained-event coverage (see UptimeSince).
func (s *Store) UptimeWindows(ctx context.Context, now time.Time, retention time.Duration) (u, cov Uptime, err error) {
	at := func(d time.Duration) (float64, float64) {
		if err != nil {
			return 0, 0
		}
		r, c, e := s.UptimeSince(ctx, now.Add(-d), retention)
		if e != nil {
			err = e
		}
		return r, c
	}
	u.H6, cov.H6 = at(6 * time.Hour)
	u.H24, cov.H24 = at(24 * time.Hour)
	u.D7, cov.D7 = at(7 * 24 * time.Hour)
	u.D30, cov.D30 = at(30 * 24 * time.Hour)
	u.Y1, cov.Y1 = at(365 * 24 * time.Hour)
	if err == nil { // "all": from the epoch, clamped inside UptimeSince to first-seen / retention
		u.All, cov.All, err = s.UptimeSince(ctx, time.Unix(0, 0), retention)
	}
	return u, cov, err
}

// UptimeFloor returns the earliest timestamp the uptime figures can vouch for: the
// later of when monitoring began and the outage-retention horizon. Exported as
// pingularity_uptime_since_timestamp_seconds so a consumer knows how far "all" and
// the long windows actually reach.
func (s *Store) UptimeFloor(ctx context.Context, retention time.Duration) (int64, error) {
	nowU := time.Now().Unix()
	first, err := s.monitoringSince(ctx, nowU)
	if err != nil {
		return 0, err
	}
	if retention > 0 {
		if floor := nowU - int64(retention.Seconds()); floor > first {
			first = floor
		}
	}
	return first, nil
}

// DataUsage holds cumulative speedtest data (bytes) over several time windows.
type DataUsage struct {
	H6  int64 `json:"6h"`
	H24 int64 `json:"24h"`
	D7  int64 `json:"7d"`
	D30 int64 `json:"30d"`
	Y1  int64 `json:"1y"`
	All int64 `json:"all"`
}

// SpeedDataUsage returns the bytes transferred by speedtests within each window,
// relative to now, computed in a single pass.
func (s *Store) SpeedDataUsage(ctx context.Context, now time.Time) (DataUsage, error) {
	cut := func(d time.Duration) int64 { return now.Add(-d).Unix() }
	var u DataUsage
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN ts>=? THEN b END),0),
		       COALESCE(SUM(CASE WHEN ts>=? THEN b END),0),
		       COALESCE(SUM(CASE WHEN ts>=? THEN b END),0),
		       COALESCE(SUM(CASE WHEN ts>=? THEN b END),0),
		       COALESCE(SUM(CASE WHEN ts>=? THEN b END),0),
		       COALESCE(SUM(b),0)
		FROM (SELECT ts, COALESCE(download_bytes,0)+COALESCE(upload_bytes,0) AS b FROM speed WHERE ts <= ?)`,
		cut(6*time.Hour), cut(24*time.Hour), cut(7*24*time.Hour),
		cut(30*24*time.Hour), cut(365*24*time.Hour),
		now.Add(metricsFutureSkew).Unix()).
		Scan(&u.H6, &u.H24, &u.D7, &u.D30, &u.Y1, &u.All)
	return u, err
}

// SpeedDataUsageSince returns the speedtest bytes (download+upload) transferred
// since t - backs the dashboard data bubble's custom (arbitrary-window) range.
func (s *Store) SpeedDataUsageSince(ctx context.Context, since time.Time) (int64, error) {
	var b int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(COALESCE(download_bytes,0)+COALESCE(upload_bytes,0)),0) FROM speed WHERE ts>=?`,
		since.Unix()).Scan(&b)
	return b, err
}

// SpeedRuns returns a page of speedtests, newest first.
func (s *Store) SpeedRuns(ctx context.Context, limit, offset int) ([]SpeedSample, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+speedCols+` FROM speed ORDER BY ts DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpeedSample
	for rows.Next() {
		sp, err := scanSpeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// DowntimeDay aggregates outages for a single calendar day in the requested
// timezone.
type DowntimeDay struct {
	Date      string `json:"date"`       // "YYYY-MM-DD" (in the requested zone)
	Outages   int    `json:"outages"`    // number of LINK DOWN events
	DowntimeS int    `json:"downtime_s"` // total seconds offline (from recoveries)
}

// DowntimeByDay returns per-day outage counts and total downtime since the
// given time, for days with at least one event or offline second. Day
// boundaries are taken in loc (the viewer's timezone; nil = the server's local
// zone). Bucketing is done in Go rather than SQLite's 'localtime' so any IANA
// zone works. A 'down' counts as an outage on the day it happened; a recovery's
// outage duration is prorated across the local days the outage actually spanned
// (clamped to the window), so a multi-day outage marks every offline day instead
// of booking days of downtime on the recovery day alone. Orphaned down->down
// gaps (a restart mid-outage) are prorated too, so the heatmap's downtime and
// outage count match UptimeSince/ResolvedOutagesSince.
func (s *Store) DowntimeByDay(ctx context.Context, since time.Time, loc *time.Location) ([]DowntimeDay, error) {
	if loc == nil {
		loc = time.Local
	}
	// Start the scan from the last event AT/BEFORE the window, not strictly inside
	// it, exactly as UptimeSince/orphanGapDowntime do - otherwise an outage whose
	// leading 'down' predates the window (an ongoing or restart-interrupted outage
	// spanning the boundary) is invisible here and its in-window downtime is lost,
	// disagreeing with the uptime%. A pre-window boundary event is used only to
	// anchor proration; it is never counted as an in-window outage (guarded below).
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, type, COALESCE(duration_s, 0)
		FROM events
		WHERE ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE ts <= ?)
		ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END`, since.Unix())
	if err != nil {
		return nil, err
	}
	// Materialize the events before the per-gap recovery lookups below: a query
	// inside an open rows scan deadlocks the single-connection (":memory:") pool.
	type evt struct {
		ts  int64
		typ string
		dur int
	}
	var events []evt
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.ts, &e.typ, &e.dur); err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []DowntimeDay
	byDay := map[string]int{} // date -> index into out
	day := func(date string) *DowntimeDay {
		i, ok := byDay[date]
		if !ok {
			i = len(out)
			byDay[date] = i
			out = append(out, DowntimeDay{Date: date})
		}
		return &out[i]
	}
	sinceU := since.Unix()
	nowU := time.Now().Unix()
	// prorate splits [start, end] across local days, crediting each its own OBSERVED
	// offline seconds (a recorded pause overlapping a day's segment is subtracted, so
	// the heatmap agrees with the uptime% - both exclude paused time). start is clamped
	// to sinceU and end to nowU (a crafted/corrupt duration_s that imports don't
	// range-check must not run the loop for millennia). When limit >= 0 the total
	// credited is bounded to limit (the authoritative pause-adjusted duration_s),
	// crediting from start forward, so an outage whose WALL gap exceeds its observed
	// length books only its observed seconds: for an explicit monitoring pause (also
	// recorded in `pauses`) wall-minus-pause already equals duration_s so the limit is
	// a no-op and the pause is never subtracted twice; for a system suspend (monotonic
	// clock frozen, no pause row) the limit trims the unobserved wall time. limit < 0
	// credits all observed time - used for spans carrying no separate duration_s (orphan
	// restart gaps and the trailing ongoing outage).
	prorate := func(start, end, limit int64) {
		if start < sinceU {
			start = sinceU
		}
		if end > nowU {
			end = nowU
		}
		remaining := limit
		for start < end {
			d := time.Unix(start, 0).In(loc)
			// Advance to the NEXT local date's first existing instant. Do not
			// rebuild midnight of the current day and add 24h: in zones whose DST
			// jump lands at midnight (America/Havana) that midnight does not exist
			// and Go normalizes it BACKWARDS, which would collapse the whole
			// remaining outage onto one day.
			nd := d.AddDate(0, 0, 1)
			b := time.Date(nd.Year(), nd.Month(), nd.Day(), 0, 0, 0, 0, loc)
			for b.Day() != nd.Day() { // local midnight skipped by a DST jump
				b = b.Add(time.Hour)
			}
			next := b.Unix()
			if next > end {
				next = end
			} else if next <= start {
				next = start + 1 // defensive: a boundary must advance, never swallow the rest
			}
			seg := next - start
			if paused, err := s.pausedOverlap(ctx, start, next); err == nil {
				if seg -= paused; seg < 0 {
					seg = 0
				}
			}
			if limit >= 0 {
				if seg > remaining {
					seg = remaining // reconcile to the observed length; trim unobserved wall time
				}
				remaining -= seg
			}
			day(d.Format("2006-01-02")).DowntimeS += int(seg)
			start = next
			if limit >= 0 && remaining <= 0 {
				break
			}
		}
	}

	prevDownTS := int64(-1)
	for _, e := range events {
		switch e.typ {
		case "down":
			// A 'down' whose predecessor is an un-recovered 'down' is a restart
			// re-detecting the SAME ongoing outage; its pre-restart stretch carries
			// no duration_s, so credit it here, bounded at the first quorum recovery
			// after the leading 'down' (the link may have recovered while the
			// monitor was off). Count this 'down' as a NEW outage only when such a
			// recovery proves the leading outage already ended - otherwise it is the
			// same outage and would double the heatmap's outage tally.
			newOutage := true
			if prevDownTS >= 0 {
				end := e.ts
				rec, ok, err := s.firstQuorumRecovery(ctx, prevDownTS, e.ts)
				if err != nil {
					return nil, err
				}
				if ok && rec < end {
					end = rec
				} else {
					newOutage = false // no recovery between: same outage re-detected
				}
				prorate(prevDownTS, end, -1)
			}
			// Count the outage on the day it began - but only when that 'down' falls
			// inside the window. A boundary 'down' pulled in from before `since` (to
			// anchor proration) must not add a phantom outage dot on a pre-window day.
			if newOutage && e.ts >= sinceU {
				day(time.Unix(e.ts, 0).In(loc).Format("2006-01-02")).Outages++
			}
			prevDownTS = e.ts
		case "up":
			// Anchor the outage at the paired 'down' event (its true start) and run
			// to the recovery ('up') time - the real WALL interval - so proration
			// splits it across the actual local days and subtracts any recorded pause
			// ONCE. The credited total is capped at duration_s, the observed length:
			// for an explicit monitoring pause (also in `pauses`) wall-minus-pause
			// already equals duration_s, so the cap is a no-op and the pause is not
			// double-counted; for a system suspend (no pause row) the cap trims the
			// unobserved wall time. Fall back to up.ts-duration_s when the 'down'
			// predates the window (no paired start to anchor on; [up-dur, up] still
			// has length duration_s, so the cap stays a no-op there too).
			start := e.ts - int64(e.dur)
			if prevDownTS >= 0 && prevDownTS < e.ts {
				start = prevDownTS
			}
			prevDownTS = -1
			prorate(start, e.ts, int64(e.dur))
		}
	}
	// A trailing 'down' with no closing 'up' is an outage still in progress (or one
	// that recovered while the monitor was off). UptimeSince books its downtime to
	// now, bounded at the first quorum recovery / last observed sample; mirror that
	// here so the heatmap's downtime matches the uptime%. The outage's own day was
	// already counted when the 'down' was processed, so this only credits downtime.
	if prevDownTS >= 0 {
		end := nowU
		if rec, ok, err := s.firstQuorumRecovery(ctx, prevDownTS, nowU); err != nil {
			return nil, err
		} else if ok && rec < end {
			end = rec // quorum recovered here; not still down
		} else if newest, ok, err := s.newestSampleAt(ctx, nowU); err != nil {
			return nil, err
		} else if ok && newest < end && newest > prevDownTS {
			// No quorum recovery in samples: either still down (newest stays ~now, a
			// no-op) or monitoring was paused mid-outage - bound at the last observed
			// sample so an unwatched pause isn't booked as live downtime, matching
			// UptimeSince and the eventual 'up' event.
			end = newest
		}
		prorate(prevDownTS, end, -1)
	}
	// Proration can back-fill days out of order (an 'up' credits earlier days),
	// so sort; "YYYY-MM-DD" sorts chronologically.
	sort.Slice(out, func(i, j int) bool { return out[i].Date < out[j].Date })
	return out, nil
}

// AllSettings returns every persisted setting as a key/value map.
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetSetting upserts a single setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	return s.SetSettings(ctx, map[string]string{key: value})
}

// SetSettings upserts several settings in one transaction.
func (s *Store) SetSettings(ctx context.Context, kv map[string]string) error {
	_, err := s.SetSettingsDiff(ctx, kv)
	return err
}

// SetSettingsDiff upserts several settings in one transaction and reports which
// keys actually changed value (or were newly created) - the input to the
// settings.changed.* counters.
//
// The old values are read OUTSIDE the write transaction on purpose: a deferred
// read→write transaction can't upgrade its snapshot when another connection
// commits in between (SQLITE_BUSY_SNAPSHOT, which busy_timeout does NOT absorb),
// and the daemon writes probe samples every few seconds - so a read-then-write
// tx here made settings saves fail sporadically. A write-only tx takes the lock
// with its first statement, where busy_timeout works. The cost: the changed-keys
// diff is best-effort under a concurrent settings write - fine for a usage
// counter (callers serialize via the controller's writer lock anyway).
func (s *Store) SetSettingsDiff(ctx context.Context, kv map[string]string) ([]string, error) {
	if len(kv) == 0 {
		return nil, nil
	}
	old, err := s.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	defer stmt.Close()
	var changed []string
	for k, v := range kv {
		if _, err := stmt.ExecContext(ctx, k, v); err != nil {
			tx.Rollback()
			recordDBErr(err)
			return nil, err
		}
		if prev, ok := old[k]; !ok || prev != v {
			changed = append(changed, k)
		}
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return changed, nil
}

// pruneFutureSlack is how far ahead of the wall clock a row's timestamp may
// sit before Prune removes it. Rows stamped further out can only come from a
// wrong boot clock (e.g. a garbage RTC years ahead, later stepped back by
// NTP); plain ts < cutoff retention would keep them for decades, and every
// newest-row read (LatestSpeed, LatestConnInfo, chart windows) stays pinned to
// them. Generous on purpose: a modest backward clock step must never delete
// real rows.
const pruneFutureSlack = 48 * time.Hour

// metricsFutureSkew is how far ahead of wall-now a row may sit and still count as
// "current" in the latest-reading and usage reads (LatestPerTarget, LatestSpeed,
// SpeedDataUsage). Prune's 48h slack is deliberately generous so a stepped-back
// clock never deletes real rows - but that same slack lets a future-dated import
// win a "latest" read and freeze current metrics until wall time catches up. This
// tight bound excludes such rows from the live reads while still tolerating a
// slightly-fast importer's just-now timestamps. Rows beyond it stay in the DB
// (Prune owns deletion); they are only hidden from the current-state reads.
const metricsFutureSkew = 2 * time.Minute

// resolveDanglingDowns makes the events log self-sufficient BEFORE the evidence
// samples are pruned. An orphaned or dangling 'down' (a restart mid-outage never
// wrote the closing 'up') is bounded at its first quorum-recovery second, found
// only in the samples table. Once those samples pass the shorter sample
// retention while the year-long events survive, that scan finds nothing and
// UptimeSince re-books the whole gap as downtime - phantom downtime in the
// 1y/all windows, growing over time and surviving restarts (recCache is
// in-memory). So, for each such 'down' whose recovery falls in the window about
// to be pruned (rec < sampleCutoff), persist a synthetic closing 'up' at that
// second: the outage becomes a completed event and no longer needs the samples.
func (s *Store) resolveDanglingDowns(ctx context.Context, sampleCutoff, nowU int64) error {
	// Dangling downs: a 'down' immediately followed by another 'down' (orphan gap)
	// or the final event being a 'down'. Mirrors UptimeSince's pairing.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, next_ts, next_type FROM (
			SELECT ts, type,
			       LEAD(ts)   OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_ts,
			       LEAD(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_type
			FROM events)
		WHERE type='down' AND (next_type='down' OR next_type IS NULL)`)
	if err != nil {
		return err
	}
	// Collect the pairs before the per-gap recovery lookups: a query inside an
	// open rows scan deadlocks the single-connection (":memory:") pool.
	type gap struct{ down, end int64 }
	var gaps []gap
	for rows.Next() {
		var down int64
		var nextTS sql.NullInt64
		var nextType sql.NullString
		if err := rows.Scan(&down, &nextTS, &nextType); err != nil {
			rows.Close()
			return err
		}
		end := nowU
		if nextType.Valid && nextType.String == "down" && nextTS.Valid {
			end = nextTS.Int64
		}
		gaps = append(gaps, gap{down: down, end: end})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type synthUp struct{ ts, dur int64 }
	var synth []synthUp
	for _, g := range gaps {
		rec, ok, err := s.firstQuorumRecovery(ctx, g.down, g.end)
		if err != nil {
			return err
		}
		// Only persist when the recovery evidence is itself being pruned; if it
		// survives this prune, the samples still prove it and no synthetic event is
		// needed (avoids re-scanning the recent, still-shifting tail too).
		if ok && rec > g.down && rec < sampleCutoff {
			synth = append(synth, synthUp{ts: rec, dur: rec - g.down})
		}
	}
	for _, u := range synth {
		if err := s.InsertEvent(ctx, time.Unix(u.ts, 0), "up", int(u.dur), "recovered while unmonitored"); err != nil {
			return err
		}
	}
	if len(synth) > 0 {
		s.invalidateReadCaches()
	}
	return nil
}

// Prune deletes old rows - and future-stamped ones beyond pruneFutureSlack.
// Each table has its own cutoff so latency samples, speed history, and outage
// events can be retained for different windows. Returns total rows removed.
func (s *Store) Prune(ctx context.Context, samplesBefore, speedBefore, eventsBefore time.Time) (int64, error) {
	start := time.Now()
	// Refuse to prune while the wall clock is implausibly early: an RTC-less device
	// (a Pi without a battery) boots near 1970, at which point EVERY real row looks
	// "future" (> now + slack) and the future-row DELETE below would erase the whole
	// history before NTP corrects the clock. The pruner retries hourly, so once the
	// clock is sane the backlog is caught up - nothing is lost by waiting, everything
	// is lost by pruning now (audit: startup clock skew).
	if start.Unix() < plausibleEpoch {
		stats.Inc("db.prune_skipped_clock")
		return 0, nil
	}
	cuts := []struct {
		table  string
		before time.Time
	}{
		{"samples", samplesBefore},
		{"dns", samplesBefore}, // DNS samples share the latency retention
		{"speed", speedBefore},
		{"pauses", eventsBefore}, // pause spans share the outage retention (uptime denominator)
	}
	horizon := start.Add(pruneFutureSlack).Unix()
	// Close any dangling 'down' whose recovery lives only in the samples about to
	// be deleted, so pruning can't turn it into phantom downtime.
	if err := s.resolveDanglingDowns(ctx, samplesBefore.Unix(), start.Unix()); err != nil {
		recordDBErr(err)
		return 0, err
	}
	var total int64
	for _, c := range cuts {
		res, err := s.db.ExecContext(ctx, `DELETE FROM `+c.table+` WHERE ts < ? OR ts > ?`,
			c.before.Unix(), horizon)
		if err != nil {
			recordDBErr(err)
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			recordDBErr(err)
		}
		total += n
	}
	// Events (outage transitions) prune as WHOLE outages: a 'down' older than the
	// cutoff whose paired 'up' is at/after it straddles the boundary, so keep it -
	// deleting only the 'down' would orphan the 'up' and split one outage into
	// phantom downtime the uptime math then miscounts (audit: whole-outage pruning).
	// Everything else < cutoff (complete past outages) and any future-stamped event
	// still goes. A dangling 'down' (no 'up' yet) is kept, like an in-progress outage.
	eventsCut := eventsBefore.Unix()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE ts > ?
		   OR (ts < ? AND NOT (
		         type = 'down'
		         AND (SELECT MIN(u.ts) FROM events u WHERE u.type = 'up' AND u.ts > events.ts) >= ?))`,
		horizon, eventsCut, eventsCut)
	if err != nil {
		recordDBErr(err)
		return total, err
	}
	if n, e := res.RowsAffected(); e == nil {
		total += n
	}
	// Pruning deleted rows (and resolveDanglingDowns may have written a synthetic
	// 'up'), so drop the memoized quorum scans and cached chart aggregates. A
	// fixed-window Series cache would otherwise keep serving pruned rows for up to
	// bucketSec/4 (~88 min on the 1-year bucket).
	s.invalidateReadCaches()
	stats.Inc("db.prune_count")
	stats.AddF("db.prune_ms_sum", util.DurMS(time.Since(start)))
	return total, nil
}

// recordDBErr feeds the db.* health counters (surfaced on /metrics), splitting
// out the two failure modes worth distinguishing: write contention and a full
// disk. Errors still propagate to callers unchanged.
func recordDBErr(err error) {
	if err == nil {
		return
	}
	stats.Inc("db.err")
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "busy") || strings.Contains(msg, "locked"):
		stats.Inc("db.busy")
	case strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") || strings.Contains(msg, "corrupt"):
		// Tested before the disk arm: the corruption message "database disk image
		// is malformed" also contains "disk", so matching disk first would misfile
		// it as a full disk and never flag corruption.
		stats.Inc("db.corrupt") // unrecoverable, distinct from transient busy/full
	case strings.Contains(msg, "i/o"):
		// SQLite's "disk I/O error" (failing storage, fsync failure, flaky mount)
		// also contains "disk", so it must be tested before the disk-full arm - a
		// full disk and failing hardware need different fixes, so paging on the
		// wrong one sends the operator down the wrong runbook.
		stats.Inc("db.io_err")
	case strings.Contains(msg, "disk") || strings.Contains(msg, "full") || strings.Contains(msg, "no space"):
		stats.Inc("db.disk_full")
	}
}

// invalidateReadCaches drops memoized read results (quorum-recovery scans,
// cached chart aggregates) after bulk data changes: an import can back-fill
// samples that change a memoized answer, and a clear must not keep serving
// deleted data.
func (s *Store) invalidateReadCaches() {
	s.recMu.Lock()
	s.recGen++
	s.recCache = map[int64]recScan{}
	s.recMu.Unlock()
	s.seriesMu.Lock()
	s.seriesCache = map[seriesKey]*seriesEntry{}
	s.seriesMu.Unlock()
}

// Clear deletes all rows of one data kind ("latency" -> samples + dns, "speed"
// -> speed, "downtime" -> events) and returns rows removed. DNS rides the
// latency dataset (same chart, same retention), so clearing latency clears it
// too - otherwise orphaned DNS rows keep feeding the chart after a clear.
func (s *Store) Clear(ctx context.Context, kind string) (int64, error) {
	var tables []string
	switch kind {
	case "latency":
		tables = []string{"samples", "dns"}
	case "speed":
		tables = []string{"speed"}
	case "downtime":
		tables = []string{"events"}
	default:
		return 0, fmt.Errorf("unknown data kind %q", kind)
	}
	// One transaction so a multi-table clear ("latency" -> samples + dns) is
	// all-or-nothing: a cancellation or a failure on the second table must not
	// leave the first emptied and the dataset half-cleared. Caches are dropped
	// only after the commit succeeds - a rolled-back clear deleted nothing.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, table := range tables {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table)
		if err != nil {
			recordDBErr(err)
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return 0, err
	}
	s.invalidateReadCaches()
	return total, nil
}

// ptrArg unwraps an optional *T into the `any` the SQLite driver binds, mapping
// a nil pointer to a NULL column. The write-side mirror of the read-side
// ptrFinite/nzFinite helpers.
func ptrArg[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// exportTables whitelists the tables export/import works on, with the column
// list (export order) and the natural key used to merge on import. settings has
// no key cols → imported rows override by PRIMARY KEY. notNull lists the NOT NULL
// columns not already in keyCols, so ImportTable can skip a crafted row that
// omits one (or supplies an explicit null) instead of failing the Exec and
// rolling back the whole restore. Together keyCols + notNull are every NOT NULL
// column of the table.
// intCols lists the INTEGER-affinity columns of each table. A non-integral float in
// one of these (e.g. an imported ts=1784765000.5) is stored by SQLite as a REAL and
// then breaks every read that scans the column into a Go int64 (the outage log and
// heatmap 500 on it). Import rejects a row whose intCol value isn't a whole number
// rather than persist a landmine (audit: fractional timestamps poison reads).
var exportTables = map[string]struct {
	cols    []string
	keyCols []string
	notNull []string
	intCols []string
}{
	"samples": {cols: []string{"ts", "target", "latency_ms", "success", "family"}, keyCols: []string{"ts", "target"}, notNull: []string{"success"}, intCols: []string{"ts", "success"}},
	"dns":     {cols: []string{"ts", "latency_ms", "success"}, keyCols: []string{"ts"}, notNull: []string{"success"}, intCols: []string{"ts", "success"}},
	"events":  {cols: []string{"ts", "type", "duration_s", "detail"}, keyCols: []string{"ts", "type"}, intCols: []string{"ts", "duration_s"}},
	"speed": {cols: []string{"ts", "down_mbps", "up_mbps", "ping_ms", "server", "server_id",
		"public_ipv4", "public_ipv6", "isp", "isp_location", "dns_ip", "dns_provider", "dns_location",
		"packet_loss", "healthy", "jitter_ms", "download_bytes", "upload_bytes", "cf_colo", "exit_summary", "run_trigger",
		"idle_ms", "loaded_down_ms", "loaded_up_ms", "loaded_down_max_ms", "loaded_up_max_ms", "engine"},
		keyCols: []string{"ts"},
		// server is read as a plain string everywhere (and COALESCEd on read as a
		// second belt) - a crafted row without one must be skipped, not inserted
		// as NULL, which once wedged every speed read with a Scan error.
		notNull: []string{"server"},
		intCols: []string{"ts", "healthy", "download_bytes", "upload_bytes"}},
	"settings": {cols: []string{"key", "value"}, keyCols: nil, notNull: []string{"key", "value"}},
}

// settingsExportDeny lists settings keys never included in an export - things
// that must not leak into or be implanted from a backup file.
var settingsExportDeny = map[string]bool{
	"auth_hash":          true, // the password hash - a restore must not implant a foreign login
	"auth_session_epoch": true, // logout revocation counter - session state, not config; a restore must not roll it back and un-revoke tokens
	// NOTE: webhook_url and heartbeat_url are deliberately NOT denied - a backup
	// includes them so a restore is complete. They are bearer credentials (the URL
	// itself is the secret), so an export file must be treated as sensitive: anyone
	// holding it can post to the alert channel or fake the dead-man's-switch.
	// Legacy telemetry identity/state keys. Telemetry was removed, but an export
	// from an older build can still carry these - keep them denied so a restore
	// can't implant a foreign install id/salt or replay stale local watermarks.
	// (telemetry_enabled was never denied.)
	"telemetry_install_id": true, "telemetry_salt": true,
	"telemetry_id_born_at": true, "telemetry_consent_version": true,
	"telemetry_last_speed_ts": true, "telemetry_last_event_ts": true,
	"telemetry_last_send_ts": true, "telemetry_clean_shutdown": true,
	// Digest delivery state (when the last summary went out): local-only, and a
	// stale/crafted value could suppress or force a digest, so it's neither
	// exported nor accepted. The cadence preference (digest_freq) stays exportable.
	"digest_last_sent": true,
}

// settingsExportRedact rewrites a settings VALUE on its way into an export. The
// denylist above works on whole keys, but each iperf3 server's password lives inside
// the iperf_servers JSON blob, so the key can't be denied without dropping the whole
// server list. Strip the secret out of the blob instead: an export carries the servers
// (address, label, bind, username, RSA key) but never their passwords.
var settingsExportRedact = map[string]func(string) string{
	"iperf_servers": redactIperfPasswords,
}

// redactIperfPasswords removes every "password" from the saved-server JSON, leaving a
// has_password marker so an import can tell "not set" from "not exported". Anything
// unparseable exports as an empty list rather than leak a blob we don't understand.
func redactIperfPasswords(raw string) string {
	if raw == "" {
		return raw
	}
	var list []map[string]any
	if json.Unmarshal([]byte(raw), &list) != nil {
		return "[]"
	}
	for _, m := range list {
		if pw, _ := m["password"].(string); pw != "" {
			m["has_password"] = true
		}
		delete(m, "password")
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// mergeImportedIperfPasswords keeps the passwords THIS host already has when a config
// import replaces the server list. Exports carry none (see above), so without this,
// restoring your own backup would silently wipe the passwords you are still using.
// Matched by address - the same key the settings layer merges on.
func mergeImportedIperfPasswords(incoming, existing string) string {
	var in, ex []map[string]any
	if json.Unmarshal([]byte(incoming), &in) != nil {
		return incoming
	}
	saved := map[string]string{}
	if existing != "" && json.Unmarshal([]byte(existing), &ex) == nil {
		for _, m := range ex {
			addr, _ := m["addr"].(string)
			if pw, _ := m["password"].(string); pw != "" && addr != "" {
				saved[strings.TrimSpace(addr)] = pw
			}
		}
	}
	for _, m := range in {
		delete(m, "has_password") // export-only marker; never store it
		if pw, _ := m["password"].(string); pw != "" {
			continue // an older export that still carries one: keep it
		}
		addr, _ := m["addr"].(string)
		if pw, ok := saved[strings.TrimSpace(addr)]; ok {
			m["password"] = pw
		}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return incoming
	}
	return string(b)
}

// ExportTableRows streams each row of a whitelisted table to fn in a single pass,
// without materializing the whole table - so a huge table (millions of samples)
// exports at O(1) memory instead of buffering every row. fn is called per row;
// returning an error stops the scan and propagates it. Denied settings rows (the
// secrets/state denylist) are skipped, same as ExportTable.
func (s *Store) ExportTableRows(ctx context.Context, table string, fn func(map[string]any) error) error {
	return exportRows(ctx, s.db, table, fn)
}

// BeginReadSnapshot opens a read-only transaction. In WAL mode this pins a single
// consistent database snapshot from its first read, so exporting several tables
// within it (via ExportTableRowsTx) can't interleave rows from different commits or
// pick up rows newer than the export started (audit: export is not one snapshot).
// The caller MUST Rollback it when done; it only prevents WAL CHECKPOINT past its
// mark (a bounded WAL growth for the export's duration), never live writes.
func (s *Store) BeginReadSnapshot(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

// ExportTableRowsTx is ExportTableRows scoped to a caller-provided read snapshot.
func (s *Store) ExportTableRowsTx(ctx context.Context, tx *sql.Tx, table string, fn func(map[string]any) error) error {
	return exportRows(ctx, tx, table, fn)
}

// rowQuerier is satisfied by both *sql.DB and *sql.Tx, so exportRows can run against
// the plain handle or inside a read snapshot.
type rowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func exportRows(ctx context.Context, q rowQuerier, table string, fn func(map[string]any) error) error {
	t, ok := exportTables[table]
	if !ok {
		return fmt.Errorf("export: unknown table %q", table)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+strings.Join(t.cols, ", ")+` FROM `+table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]any, len(t.cols))
		ptrs := make([]any, len(t.cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		m := make(map[string]any, len(t.cols))
		for i, c := range t.cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			// A stored non-finite float (a real ±Inf speed measurement) would
			// abort json.Encode mid-stream and silently truncate the backup;
			// export it as NULL like the ptrFinite read paths do - the import
			// side drops NaN/Inf values anyway.
			if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
				v = nil
			}
			m[c] = v
		}
		if table == "settings" {
			k, _ := m["key"].(string)
			if settingsExportDeny[k] {
				continue // never export secrets
			}
			if redact := settingsExportRedact[k]; redact != nil {
				if v, ok := m["value"].(string); ok {
					m["value"] = redact(v)
				}
			}
		}
		if err := fn(m); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportTable returns all rows of a whitelisted table as a slice. Prefer
// ExportTableRows for large tables - this buffers the whole table in memory.
func (s *Store) ExportTable(ctx context.Context, table string) ([]map[string]any, error) {
	out := []map[string]any{}
	if err := s.ExportTableRows(ctx, table, func(m map[string]any) error {
		out = append(out, m)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// importTxRows bounds how many rows one import transaction applies. SQLite has
// a single writer, and a restore holding it for the whole file (~a minute at a
// large backup) would starve the live monitor's inserts past their 5s
// busy_timeout, silently dropping probe rounds and transition events. The
// per-key merge below makes a partially applied import safe to re-run.
const importTxRows = 5000

// ImportTable merges exported rows into a whitelisted table in bounded
// transactions (importTxRows apiece, so a big restore doesn't starve live
// writers). settings rows override by key (INSERT OR REPLACE); time-series
// tables insert only rows whose key columns aren't already present, preserving
// existing (e.g. newer) local rows. Returns rows added/replaced - on error,
// the rows already durably committed. Rows carrying columns this binary does
// not know (a backup from a newer version) fail the whole import up front
// rather than silently dropping the newer fields.
//
// The per-timestamp flood cap is scoped to this single call; a streaming
// importer that splits one logical import across batches must use
// ImportTableBatch with a shared counter so the cap stays global.
func (s *Store) ImportTable(ctx context.Context, table string, rows []map[string]any) (int, error) {
	return s.ImportTableBatch(ctx, table, rows, map[int64]int{})
}

// ImportTableBatch is ImportTable with a caller-owned per-timestamp counter
// (perTS). A streaming restore decodes the file in fixed-size batches and calls
// this once per batch with the SAME map, so the maxRowsPerTS flood cap counts
// across all batches. A per-call counter would reset each batch, letting a
// crafted file pack maxRowsPerTS rows per batch at one ts and restore the O(N^2)
// dedup cost the cap exists to bound.
func (s *Store) ImportTableBatch(ctx context.Context, table string, rows []map[string]any, perTS map[int64]int) (int, error) {
	t, ok := exportTables[table]
	if !ok {
		return 0, fmt.Errorf("import: unknown table %q", table)
	}
	known := map[string]bool{}
	for _, c := range t.cols {
		known[c] = true
	}
	intCol := map[string]bool{}
	for _, c := range t.intCols {
		intCol[c] = true
	}
	for _, k := range t.keyCols {
		known[k] = true
	}
	for _, r := range rows {
		for c := range r {
			if !known[c] {
				return 0, fmt.Errorf("import %s: unknown column %q (backup from a newer version?)", table, c)
			}
		}
	}
	// An import can replace the iperf3 server list, and exports no longer carry passwords
	// (see redactIperfPasswords), so we re-attach the ones this host already has. Read them
	// HERE, before the write transaction opens: a read on s.db while the tx holds the single
	// SQLite writer would wait on itself forever.
	var curIperfServers string
	if table == "settings" {
		if err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'iperf_servers'`).Scan(&curIperfServers); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if tx != nil {
			tx.Rollback()
		}
	}()
	committed, pending, inTx := 0, 0, 0
	// A chunked import commits durably as it goes, so a later failure still leaves
	// earlier rows in place - invalidate the read caches whenever any chunk
	// committed, not only on full success, or memoized answers contradict the
	// back-filled data.
	durable := false
	defer func() {
		if durable {
			s.invalidateReadCaches()
		}
	}()
	// Bound rows per ts during import: the per-row de-dup probe filters by ts via
	// idx_samples_ts then scans for the rest of the key, so a crafted file packing
	// huge numbers of rows at ONE ts would be O(N^2) inside this write transaction
	// and pin the single SQLite writer. Real data has only a handful of rows per ts;
	// cap far above that and skip the excess (like the other defensive skips here).
	const maxRowsPerTS = 256
	if perTS == nil { // single-shot callers may pass nil; batch callers share one map
		perTS = map[int64]int{}
	}
	for _, r := range rows {
		// The export denylist guards imports too: a crafted file must not implant
		// a password hash, a legacy telemetry identity, or delivery state.
		if table == "settings" {
			k, _ := r["key"].(string)
			if settingsExportDeny[k] {
				continue
			}
			// Exports carry no iperf3 passwords, so a straight REPLACE here would wipe the
			// ones this host is still using. Fold them back in by address (read before the tx).
			if k == "iperf_servers" {
				if v, ok := r["value"].(string); ok {
					r["value"] = mergeImportedIperfPasswords(v, curIperfServers)
				}
			}
		}
		// Imported IP columns are untrusted operator input (security-model.md frames a
		// backup as hostile), yet organically-stored IPs are validated at write time and
		// the CSV export path skips csvSafe on them precisely because it assumes they are
		// well-formed. Blank any value that isn't a parseable IP so a crafted backup can't
		// implant a spreadsheet formula (=cmd, @HYPERLINK, ...) that later rides a .csv
		// export out un-defused.
		if table == "speed" {
			for _, c := range []string{"public_ipv4", "public_ipv6", "dns_ip"} {
				if v, ok := r[c].(string); ok && strings.TrimSpace(v) != "" {
					if _, err := netip.ParseAddr(strings.TrimSpace(v)); err != nil {
						r[c] = ""
					}
				}
			}
			// Clamp byte counts to [0, maxSpeedBytesPerRun], exactly as InsertSpeed does:
			// an unbounded imported value flows into SpeedDataUsage's int64 SUM, which
			// SQLite fails with "integer overflow", 500-ing the data panel until the row
			// is removed (audit: import-speed-bytes overflow). normVal has already turned
			// these into int64.
			for _, c := range []string{"download_bytes", "upload_bytes"} {
				var n int64 // JSON gives float64, a direct export->import gives int64
				switch x := r[c].(type) {
				case int64:
					n = x
				case float64:
					if math.IsInf(x, 0) || math.IsNaN(x) || x != math.Trunc(x) || x < float64(math.MinInt64) || x >= float64(math.MaxInt64) {
						continue // normVal/intCol guard rejects the row later
					}
					n = int64(x)
				default:
					continue
				}
				r[c] = *clampSpeedBytes(&n)
			}
		}
		// Reject a row whose ts isn't a finite number >= 0: imported JSON is
		// untrusted, and a NaN/Inf/negative ts silently corrupts retention pruning
		// (DELETE ... WHERE ts < cutoff) and the event-derived uptime math.
		var tsKey int64
		hasTS := false
		if tsv, ok := r["ts"]; ok {
			// ts is float64 from a JSON import but int64 from a direct
			// export->import; accept both numeric forms, reject anything else.
			var f float64
			switch n := tsv.(type) {
			case float64:
				f = n
			case int64:
				f = float64(n)
			case int:
				f = float64(n)
			default:
				continue
			}
			if math.IsInf(f, 0) || math.IsNaN(f) || f < 0 || f >= float64(math.MaxInt64) {
				continue
			}
			tsKey, hasTS = int64(f), true
		}
		// Per-ts cap (keyed time-series tables only; settings has no key/ts).
		if hasTS && len(t.keyCols) > 0 {
			if perTS[tsKey] >= maxRowsPerTS {
				continue
			}
			perTS[tsKey]++
		}
		// Every non-key NOT NULL column must be present and non-null. A missing or
		// null value would bind NULL into a NOT NULL column, fail the Exec, and (via
		// the deferred Rollback) discard the ENTIRE import - one corrupt row in an
		// untrusted backup becoming a total restore failure. Skip just this row,
		// like the key-column guard below. (keyCols cover the rest.)
		badNN := false
		for _, c := range t.notNull {
			if v, present := r[c]; !present || v == nil {
				badNN = true
				break
			}
		}
		if badNN {
			continue
		}
		var cols, ph []string
		var args []any
		bad := false
		for _, c := range t.cols {
			if v, ok := r[c]; ok {
				nv, ok := normVal(v)
				if !ok {
					bad = true
					break
				}
				// An INTEGER-affinity column must hold a whole number: normVal turns a
				// whole float into int64, so a value still float64 here was fractional
				// (e.g. ts=…​.5) and would land as a REAL that breaks int64 reads. Skip it.
				if intCol[c] {
					if _, isFloat := nv.(float64); isFloat {
						bad = true
						break
					}
				}
				cols = append(cols, c)
				ph = append(ph, "?")
				args = append(args, nv)
			}
		}
		// A non-finite float anywhere in the row (NaN/Inf) is rejected rather than
		// stored: SQLite has no NaN/Inf literal and such values poison aggregates.
		if bad || len(cols) == 0 {
			continue
		}
		var q string
		if len(t.keyCols) == 0 {
			q = `INSERT OR REPLACE INTO ` + table + ` (` + strings.Join(cols, ", ") + `) VALUES (` + strings.Join(ph, ", ") + `)`
		} else {
			conds := make([]string, len(t.keyCols))
			for i, k := range t.keyCols {
				conds[i] = k + "=?"
				// Key columns are the table's NOT NULL identity columns. A row that
				// omits one (or supplies a JSON null) would drop it from the INSERT
				// and send NULL into a NOT NULL column, failing the Exec and (via the
				// deferred Rollback) discarding the ENTIRE import. Skip just this row,
				// like the other defensive skips in this loop.
				kc, present := r[k]
				if !present || kc == nil {
					bad = true
					break
				}
				kv, ok := normVal(kc)
				if !ok {
					bad = true
					break
				}
				args = append(args, kv)
			}
			if bad {
				continue
			}
			q = `INSERT INTO ` + table + ` (` + strings.Join(cols, ", ") + `) SELECT ` + strings.Join(ph, ", ") +
				` WHERE NOT EXISTS (SELECT 1 FROM ` + table + ` WHERE ` + strings.Join(conds, " AND ") + `)`
		}
		res, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			recordDBErr(err)
			return committed, err
		}
		aff, _ := res.RowsAffected()
		pending += int(aff)
		if inTx++; inTx >= importTxRows {
			if err := tx.Commit(); err != nil {
				recordDBErr(err)
				return committed, err
			}
			durable = true
			// This sub-chunk's rows are now durable and visible to other
			// connections, but the final invalidation is deferred to function exit.
			// Drop the read caches now so a concurrent reader in the (long, for a big
			// import) remaining-rows window can't keep serving memoized answers that
			// the just-committed rows have already contradicted.
			s.invalidateReadCaches()
			committed, pending, inTx = committed+pending, 0, 0
			if tx, err = s.db.BeginTx(ctx, nil); err != nil {
				return committed, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return committed, err
	}
	durable = true
	return committed + pending, nil
}

// normVal turns JSON-decoded whole-number float64s into int64 so INTEGER columns
// and key comparisons stay exact (e.g. ts, success). Reports ok=false for a
// non-finite float (NaN/Inf): SQLite has no NaN/Inf literal and storing one
// would poison aggregates - the caller drops the row.
func normVal(v any) (any, bool) {
	switch f := v.(type) {
	case float64:
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, false
		}
		if f == math.Trunc(f) {
			// Reject whole numbers outside int64 range: int64(f) would silently wrap
			// to a garbage value (a crafted import ts could poison pruning/uptime math).
			if f < float64(math.MinInt64) || f >= float64(math.MaxInt64) {
				return nil, false
			}
			return int64(f), true
		}
	case []any, map[string]any:
		// A JSON array/object can't bind to a scalar column; the driver would
		// reject it and abort the whole import. Skip the row like the other
		// corrupt-value guards rather than half-applying the restore.
		return nil, false
	}
	return v, true
}
