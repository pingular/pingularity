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
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"maps"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

CREATE TABLE IF NOT EXISTS pauses_quarantine (
    ts         INTEGER NOT NULL,   -- a pause row held aside because its END reaches further past the repairing clock than any believable disagreement
    duration_s INTEGER NOT NULL    -- see repairFutureReachingPausesAt: held, not deleted, so a corrected clock can give it back
);
-- Sought by ts on two paths that hold the single writer: the import's per-row
-- dedup probe, and the future-pause repair's twin lookup for every live pause,
-- inside one transaction at every Open. Without it each seek scans the table -
-- importing N held rows costs N squared, the repair costs pauses times
-- quarantine, and every other writer arriving meanwhile waits out busy_timeout
-- and drops its row.
CREATE INDEX IF NOT EXISTS idx_pauses_quarantine_ts ON pauses_quarantine(ts);

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
    ping_best_ms   REAL,              -- fastest of the ping samples (ping_ms is their mean); what the run's decisions use
    idle_ms        REAL,              -- latency under load: idle baseline (median, ms)
    loaded_down_ms REAL,              -- ... during the download phase; NULL when unmeasurable
    loaded_up_ms   REAL,              -- ... during the upload phase
    loaded_down_p95_ms REAL,          -- p95 of the samples taken during the download phase
    loaded_up_p95_ms   REAL,          -- ... during the upload phase
    engine             TEXT,          -- test backend: ookla|iperf3 (NULL on old rows = ookla)
    ip_family          TEXT,          -- address family the transfer actually used: 4|6|mixed (NULL = unrecorded, never guessed)
    udp_direction      TEXT,          -- which way the UDP loss/jitter probe sampled: down|up (NULL = unrecorded)
    -- 1 = this row is ACCOUNTING, not a measurement: a run where every candidate
    -- failed still moved real bytes, and they land on the user's bill, so the
    -- usage is recorded (see speedtest.Scheduler.recordFailedUsage) - but nothing
    -- was measured. NULL/0 = a real run (every row written before this existed).
    -- A separate column rather than NULL speeds ON PURPOSE, twice over: every
    -- consumer's "was this direction measured?" predicate is "bytes != nil", and
    -- a usage row has real bytes; and scanSpeed reads down_mbps/up_mbps through
    -- nzFinite, which collapses SQL NULL to 0 - so NULL speeds would come back
    -- from the DB indistinguishable from a genuine 0 Mbps reading. Every
    -- MEASUREMENT query filters this out (see speedNotFailed); the data-usage
    -- sums deliberately do not. The runs table lists such a row only when it
    -- also carries fail_stage (a failure record, see speedIsListed), and even
    -- then as a failed test, never as a reading.
    failed             INTEGER,
    -- The run whose spend an ACCOUNTING row bills: joins speed.ts, the same
    -- manual-cascade shape as speed_servers.run_ts (no FK anywhere in this
    -- schema). NULL on every measurement row, and on the accounting row a
    -- WHOLLY failed run leaves behind - that run produced no measurement, so
    -- there is nothing for it to point at (see
    -- speedtest.Scheduler.recordFailedUsage).
    --
    -- Named for the usage rather than called plain 'run_ts' because this table
    -- holds both kinds of row: beside its own 'ts', a bare 'run_ts' would read
    -- as "when this run ran" on the very rows it is NULL for.
    --
    -- DeleteSpeed cascades on this. It used to find the row positionally - the
    -- flagged row at ts+1 - which is a GUESS: a manual run that fails one second
    -- after a scheduled measurement writes its own flagged row at exactly that
    -- second, and deleting the scheduled run then destroyed the manual run's
    -- record. With Record failed tests off no listing shows a flagged row, so
    -- nothing told the operator that run existed or that its bytes had just
    -- been un-billed.
    usage_run_ts       INTEGER,
    -- How the run's centre was chosen (speedtest.RaceVerdict), so a surprising
    -- city is explainable from the DB the way speed_servers explains the server
    -- inside it. race_outcome: decided|silent|unanchored|failed|skipped|
    -- bypassed_pin (NULL on rows before this existed and on
    -- iperf3 runs); race_origins: the field in one line, per origin with its
    -- pool's fastest answer ("exit:Montréal, QC(8.4ms) | isp:Toronto(15.1ms) | geo(-)");
    -- race_winner_kind/label: the origin the run centred on, and lat/lon its
    -- coordinate when the race was decided on an anchored origin (NULL on
    -- silent rows: the stand-in is named but not re-entered), so the next run
    -- can enter it as the "recent" origin; race_winner_ms: the fastest racer's
    -- floor; race_racers: servers pinged.
    race_outcome       TEXT,
    race_origins       TEXT,
    race_winner_kind   TEXT,
    race_winner_label  TEXT,
    race_winner_ms     REAL,
    race_racers        INTEGER,
    race_winner_lat    REAL,
    race_winner_lon    REAL,
    -- The round this row was measured in and did not win: the ts of the
    -- winner's row, written when the operator keeps a Best-of round's losers
    -- (speedtest.Scheduler.recordLosers). NULL on every row that is a run's
    -- own result. A real measurement - every column above is that server's
    -- own reading - that is history but not the line's record: the reads
    -- that decide something (charts, the latest run, thresholds, baselines,
    -- the data estimate, the next race's origin) carry speedIsResult and
    -- skip it; the runs table, its counts and the exports show it.
    -- DeleteSpeed cascades on it, so a deleted run takes its round along.
    round_ts           INTEGER,
    -- The stage a WHOLLY failed run stopped at (speedtest.speedFailStage:
    -- server_list|server_fetch|no_servers|ping|na|download|upload|bidir|other),
    -- written beside failed=1 by the scheduler only when the operator turned on
    -- Record failed tests. It is what turns the run's accounting row into a
    -- FAILURE RECORD the runs table lists (see speedIsFailure). NULL on every
    -- measurement, on every accounting row written with the switch off or for a
    -- user's stop, and on every row older than the column.
    fail_stage         TEXT
);
CREATE INDEX IF NOT EXISTS idx_speed_ts ON speed(ts);

CREATE TABLE IF NOT EXISTS speed_servers (
    run_ts INTEGER NOT NULL,              -- joins speed.ts (no FK anywhere in this schema; cascades are manual)
    server_id TEXT,
    server TEXT,                          -- "Sponsor, Name" label, as on speed.server
    distance_km REAL,
    rank_order INTEGER,                   -- 1-based ping-race rank; 0 = never ranked (a pin resolved outside the list)
    rank_ping_ms REAL,                    -- ranking ping (the floor of ~10 samples; rows before 2026-08-27 hold the library mean); NULL when the ranking ping went unanswered
    selected INTEGER NOT NULL,            -- became a measurement target
    measured INTEGER NOT NULL,            -- produced a result
    error TEXT,                           -- failure text; selected+unmeasured+EMPTY = never attempted (the app writes '', not NULL)
    down_mbps REAL, up_mbps REAL, ping_ms REAL, ping_best_ms REAL, jitter_ms REAL,
    download_bytes INTEGER, upload_bytes INTEGER, -- this candidate's OWN traffic (speed.download_bytes is the round total)
    capacity_mbps REAL,                   -- raw weighted-geomean capacity
    believed_capacity_mbps REAL,          -- capacity after the implausibility guard
    capped_direction TEXT,                -- direction(s) the guard held down for this row: down|up|down,up
    score REAL,                           -- what the winner decision compared
    winner INTEGER NOT NULL,
    win_reason TEXT                       -- score|ping_bootstrap|fastest_ranked|incumbent|on_net|challenger|challenger_won|challenger_failed|pinned|pinned_bestof|pinned_companion (vocabulary: speedtest/selection.go)
);
CREATE INDEX IF NOT EXISTS idx_speed_servers_ts ON speed_servers(run_ts);

-- When a speedtest that produced a result was using the network: one row per
-- such run, written once its result row is stored (speedtest.Scheduler's
-- recordSpan). Hovering a latency chart point in one of these stretches says
-- "During a speedtest", because a test fills the line and the probe rounds taken
-- meanwhile read slow, or fail, for that reason alone. Nothing is drawn for them.
-- Not a result: no outcome is stored. It only feeds that one chart's tooltip, so
-- it follows the chart's data: pruned on the latency retention, cleared with the
-- latency data, and left out of export like server_health. DeleteSpeed leaves
-- it, because the link was busy then whatever became of the run's row.
CREATE TABLE IF NOT EXISTS speed_spans (
    ts         INTEGER NOT NULL,   -- unix seconds the run started using the network
    duration_s INTEGER NOT NULL    -- how long it did (monotonic), so ts + duration_s is its end rounded up; 1..3600
);
CREATE INDEX IF NOT EXISTS idx_speed_spans_ts ON speed_spans(ts);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Servers convicted of refusing every speedtest upload, so the exclusion
-- outlives a restart. The cheap health check is a GET of the server's latency
-- file, which a host whose upload endpoint answers 500 to everything still
-- passes - so that conviction costs a whole measurement turn to earn, and
-- before this table every restart threw it away and paid again. Only
-- convictions live here; verdicts the GET probe reaches on its own are one
-- request to re-derive and are not worth storing. An expired row is kept for a
-- while longer (see ServerHealth): the TTL is the server's second chance, but
-- how many chances it has already had decides how long the NEXT one lasts.
CREATE TABLE IF NOT EXISTS server_health (
    server_id TEXT PRIMARY KEY,
    expires   INTEGER NOT NULL,   -- unix seconds; the exclusion stands until then
    fails     INTEGER NOT NULL
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

	// opened carries the wall and MONOTONIC readings from store open, and
	// openedBoot the boot clock read with them (openedBootOK false when there
	// was none); the three are set once, in openAtClock, and only read after,
	// so they need no lock. sinceOpen turns them into the store's uptime.
	// clockBase/clockBaseUp are the wall/uptime pair destructive pruning judges
	// the clock against, and clockSettleUp parks pruning until that much uptime
	// has passed. See clockStepped - the only thing that reads or writes them.
	clockMu       sync.Mutex
	opened        time.Time
	openedBoot    time.Duration
	openedBootOK  bool
	clockBase     time.Time
	clockBaseUp   time.Duration
	clockSettleUp time.Duration
	// pauseStepSeen (under clockMu): a wall-clock step was detected and the
	// pause re-judgement is owed - once the clock survives the settle window.
	// While set, ALL deferred pause repair is parked, including generations
	// armed earlier (an implausible Open's) - the same reading that parked
	// destructive pruning must not judge history either.
	pauseStepSeen bool
	// pauseVetted (under clockMu): the wall reading that survived the most
	// recent settle window, and pauseVettedUp the uptime read with it; zero
	// until a step has settled. Once present, every deferred repair judges in
	// THIS frame, advanced by the uptime since (see repairReading) - never a
	// fresh wall reading, which a step after arming (but before the consuming
	// write) could have made untrustworthy again.
	pauseVetted   time.Time
	pauseVettedUp time.Duration

	// pauseRepairArm/pauseRepairDone are the deferred pause re-judgement
	// trigger, as a GENERATION pair rather than a boolean: arm != done means a
	// judgement is owed. Open seeds generation 1 when its clock is implausible
	// (an RTC-less board opens at service start, before NTP syncs); a detected
	// clock step arms a fresh generation once it settles (see clockStepped).
	// A generation - with the judging clock read only AFTER observing it -
	// closes the race a boolean had: a writer that captured time.Now() before
	// an NTP correction could consume the one trigger and re-judge with the
	// pre-correction clock, restoring nothing. See maybeRepairFuturePausesFn.
	pauseRepairArm  atomic.Uint64
	pauseRepairDone atomic.Uint64
	pauseRepairMu   sync.Mutex // serializes the judgement transaction itself

	// pruneWatches are the watches open on the cleanup (WatchPrunes), and
	// pruneWatchMu guards the set and what each watch has counted. Every chunk
	// of every pass that deletes rows adds them to each open watch
	// (pruneChunked).
	pruneWatchMu sync.Mutex
	pruneWatches map[*PruneWatch]struct{}
	// clearing counts, table by table, the Delete nows that have begun and not
	// yet ended (Clear), under pruneWatchMu. A watch reads it as it answers
	// (PruneTally.Clearing).
	clearing map[string]int

	// cleanupHolds counts the restores that are bringing rows in (HoldCleanup).
	// While it is above zero no cleanup pass starts.
	cleanupHolds atomic.Int32

	// rebuilt records that this Open found the database damaged, set it aside
	// and started over on an empty one. Fixed for the life of the handle - a
	// later Open of the file it just built finds a healthy database and says
	// no. See RebuiltAfterCorruption for who asks.
	rebuilt bool

	// held is the probe rounds and DNS readings that wait to be saved (see
	// SaveBuffered).
	held sampleBuf

	// outageMu is held for the whole of every change to the outage record
	// (events) that does not come from the running monitor: the close of the
	// outages whose end only the samples show (resolveDanglingDowns, which a
	// cleanup and a latency Delete now both run), the delete of one outage
	// (DeleteOutage), the downtime Delete now (Clear), a cleanup's sweep of
	// old outages (Prune) and a restore's outage history (HoldOutageRecord,
	// which every import of the downtime category's tables goes through). The
	// close reads which outages have no end, searches the samples for each,
	// and only then writes the ends it found. With nothing to keep the others
	// out, two closes at once each wrote an end for the same outage, and the
	// readers count the second one as an outage of its own; and a delete that
	// landed between a close's read and its write left an end with no outage.
	// Under the lock each of these finds the record as the one before it left
	// it. A restore that streams a backup holds it for the whole of its
	// downtime category, every batch of the outages and the paused time after
	// them, not batch by batch: a close in a gap between two batches found
	// half an outage, or an outage without the pauses it reads
	// (HoldOutageRecord). A restore's count of what the next cleanup deletes
	// takes it as well, for the dry run of the close it asks for (PruneDue,
	// closeDanglingDowns): that writes nothing, and reads the record between
	// two changes to it, never in the middle of one.
	//
	// The monitor's own writes never take it: InsertEvent from a probe round
	// or from the monitor's retry buffer. A probe round must not wait out a
	// close, only, as beside any other writer, the one statement in progress.
	// The close keeps away from the monitor's outages by the line those draw
	// (monitorOutages) instead. Open's repairs run before anything else can
	// reach the store.
	//
	// It is taken with no other lock of this package held and no transaction
	// or cursor of the holder's open. (A restore can take it with the web
	// layer's importMu held, when the file carries its config ahead of its
	// outages, and again for its count once every row is in. It lets go before
	// it moves on to another category, the count's dry run lets go as it
	// returns, and nothing that holds outageMu takes importMu.) Under it the
	// holder saves the
	// waiting rows (the save gate, then held.mu, and the daemon's save
	// interval and save log functions, which lock only their own state), reads
	// and drops the read caches (recMu, seriesMu), asks a cleanup's cutoffs
	// function for the windows in force before each chunk of its sweep
	// (PruneLive: the daemon's reads the settings controller, whose lock is
	// held only to copy or replace its values), adds what a cleanup's chunk
	// deleted to the watches on the cleanup, or that a downtime Delete now has
	// emptied its tables (pruneWatchMu), and waits for a
	// pooled connection and for SQLite's writer. A restore also reads the rest
	// of its downtime category from the request under it, which makes it the
	// one holder that can keep the lock for long (see HoldOutageRecord for
	// what waits).
	// A close also reads and tidies the list of the monitor's outages under it
	// (monitorOutages, whose own lock is held for a few lines with nothing
	// taken inside it; the monitor's writes take that one, never this).
	// No goroutine holding any of those goes on to take outageMu, and clockMu
	// and pauseRepairMu are never held with it (Prune takes and lets go of
	// both before its close), so no two goroutines can each wait for the
	// other. Taking it with nothing open also keeps the in-memory pool, which
	// has one connection, from waiting on itself: a holder never waits for a
	// connection it holds.
	outageMu sync.Mutex

	// live is the outages the running monitor has opened and is not done with.
	// The earliest of them is the line a close of outages keeps away from, for
	// a Delete now (Clear) and for a cleanup (PruneLive) alike: see
	// monitorOutages for what is on the list, and resolveDanglingDowns for why
	// both need the line.
	live monitorOutages
}

// monitorOutages is the outages the running monitor opened on this handle and
// may still write to: the ones a close of outages must leave to it.
// InsertEvent is the live monitor's door for its transitions, and the daemon
// opens one handle when it starts and runs one monitor on it for the life of
// that handle. The monitor only ever closes an outage it opened itself, and
// every 'up' it writes lands at or after its own 'down'. So an outage is put
// here when its 'down' comes through that door, and is the monitor's until
// both of its events are on disk.
//
// The line (line) is the earliest 'down' on the list, math.MaxInt64 with none.
// A close leaves every 'down' at or past it to the monitor and stops an
// earlier outage's search there.
//
// The simpler line, the earliest 'down' the handle has ever written, never
// moving later, would be safe and too wide. On a daemon that had run for
// months it would sit at the first outage since the start, and every outage
// dated after that one which was not the monitor's would be left with no end:
// one a restore brought from another install, cut short by a restart there.
// The cleanup would delete the samples that showed its end without writing
// the end down, and a ten-minute outage would then read as lasting until the
// next one on record, for good. So the line moves on as the monitor finishes
// its outages, and an outage behind it is closed like any other.
//
// What counts as finished is kept narrow, because an end written for an outage
// the monitor goes on to end itself is a second end, and the readers count
// that as an outage of its own:
//
//   - Both events are on disk, whichever landed first. A write that fails
//     waits in the monitor's retry buffer, and the monitor writes a later
//     transition straight to the store while an earlier one still waits, so
//     an 'up' can land before its own 'down'. An 'up' is taken as the end of
//     the latest outage on the list that began at or before it and has none:
//     the monitor's transitions alternate, and each 'down' is dated after the
//     'up' before it (Monitor.transition).
//   - The clock has reached the 'up'. After a backward clock step the monitor
//     dates its 'up' ahead of the clock on purpose (the widen in
//     Monitor.transition), and a close takes an 'up' dated ahead of the clock
//     it runs at (currentHorizon) for one to move back. Until the clock gets
//     there the outage stays on the list and the row stays as the monitor
//     wrote it. A close judges that by its own clock, as it begins. The
//     monitor's door judges it by the bare clock, with none of the two
//     minutes a close allows, so that a close whose reading is a little
//     behind the door's still finds the 'up' within its horizon. A clock set
//     back further after that is not followed: the row is then like any other
//     dated ahead of the clock.
//   - No close is part way through. A close reads which outages have no end
//     and only later reads the line, so an outage that came off the list in
//     between would look like one nobody holds. Outages come off as a close
//     begins, before it reads anything, or when the monitor opens its next
//     outage with no close running. One that finishes during a close stays on
//     until then, so for a close the line only ever moves earlier.
//   - No close is waiting for its turn. A close reads its clock and may then
//     wait for outageMu, behind a restore's outage history for as long as
//     the client takes to send it. It judges "reached" by the reading it
//     took before the wait. An outage the monitor ended during the wait, at
//     an 'up' more than the close's two minutes past that reading, and that
//     the door then dropped, would look to the close like one nobody holds
//     with an 'up' dated ahead of the clock, and the close would move the
//     monitor's own 'up' back. So the door drops nothing while a close waits
//     (waiting), and the close itself drops only what its own reading has
//     reached.
//
// An outage whose 'down' or 'up' the monitor had to drop (its retry buffer
// was full: monitor.event_dropped) never finishes, and the line stays at it
// for as long as the process runs. The list
// is short: finished outages leave it, and past maxMonitorOutages the oldest
// is dropped and the line stays at or before it for good (floor).
//
// mu is held for a few lines and nothing is taken under it. The monitor's
// goroutine takes it in InsertEvent and a close takes it with outageMu held.
type monitorOutages struct {
	mu      sync.Mutex
	open    []monitorOutage
	floor   int64 // the line never moves later than this; math.MaxInt64 until an outage is dropped from a full list
	closing bool  // a close is running: nothing comes off the list until the next one begins
	waiting int   // closes that have read their clock and wait for outageMu: the door drops nothing meanwhile
}

// monitorOutage is one outage on the list: the second of its 'down', which is
// what names it, and what has reached the disk.
type monitorOutage struct {
	down, up             int64 // up is the second of its 'up', once that has landed
	downLanded, upLanded bool
}

// maxMonitorOutages bounds the list. Only outages that never finish add up,
// one for each event the monitor drops.
const maxMonitorOutages = 64

// opened puts the outage whose 'down' is at second at on the list, before the
// row is written: a close that finds the row then finds it behind the line. A
// write that fails leaves it there, and the monitor writes that 'down' again,
// at the same second, from its retry buffer. nowU is the clock, for dropping
// the outages that have finished since the last close: the ones whose 'up' the
// clock itself has reached.
func (m *monitorOutages) opened(at, nowU int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, o := range m.open {
		if o.down == at {
			return // written again from the retry buffer
		}
	}
	if !m.closing && m.waiting == 0 {
		m.forget(nowU)
	}
	if len(m.open) >= maxMonitorOutages {
		m.floor = min(m.floor, m.open[0].down)
		m.open = m.open[1:]
	}
	m.open = append(m.open, monitorOutage{down: at})
}

// landed notes that an event of the monitor's is on disk.
func (m *monitorOutages) landed(typ string, at int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if typ == "down" {
		for i := range m.open {
			if m.open[i].down == at {
				m.open[i].downLanded = true
			}
		}
		return
	}
	// An 'up' ends the latest outage that began at or before it and has no
	// end yet. One that finds none closes nothing on the list.
	latest := -1
	for i, o := range m.open {
		if !o.upLanded && o.down <= at && (latest < 0 || o.down > m.open[latest].down) {
			latest = i
		}
	}
	if latest >= 0 {
		m.open[latest].up, m.open[latest].upLanded = at, true
	}
}

// forget drops the outages the monitor is done with: both events on disk, and
// the 'up' at or before reached. Called with mu held.
func (m *monitorOutages) forget(reached int64) {
	m.open = slices.DeleteFunc(m.open, func(o monitorOutage) bool {
		return o.downLanded && o.upLanded && o.up <= reached
	})
}

// wait is called by a close before it waits for outageMu, with the clock
// reading it will run at already taken. begin takes it back.
func (m *monitorOutages) wait() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waiting++
}

// begin is called by a close as it starts, under outageMu and before it reads
// the outage record: the finished outages come off the list here, and none
// does again until the close has ended (end). nowU is the clock the close runs
// at, and an 'up' counts as reached where the close's pairing reads it as one
// the clock has reached (currentHorizon).
func (m *monitorOutages) begin(nowU int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waiting--
	m.closing = true
	m.forget(currentHorizon(nowU))
}

// end is called by a close as it returns.
func (m *monitorOutages) end() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closing = false
}

// line is the second a close keeps away from: the earliest 'down' on the list.
func (m *monitorOutages) line() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	at := m.floor
	for _, o := range m.open {
		at = min(at, o.down)
	}
	return at
}

// pragmaConn is the per-connection pragma query appended to every file-backed
// DSN: WAL + synchronous=NORMAL cuts fsync overhead on the write-heavy probe
// path and lets status/chart reads run without blocking the writer, and
// busy_timeout makes a rare write-write collision retry instead of failing. The
// params apply to every pooled connection (modernc/sqlite).
//
// journal_size_limit is walSizeLimit, written out because a constant string
// cannot be built from a number. SQLite never shrinks the -wal file by itself:
// it writes it again from the start after a checkpoint, so a log that grew
// once kept that size until the daemon stopped (78 to 87 MB after a cleanup of
// 15 days of rows). With the limit, the first commit after the log restarts
// cuts the file back to the limit. That commit is usually a save of probe
// readings, and the cut is one truncate call inside it. The limit does not keep the log from
// growing past it while a reader holds the log. It is a property of the
// connection, not of the file, which is why it rides the DSN.
const pragmaConn = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(8388608)&_pragma=synchronous(NORMAL)"

// walSizeLimit is the journal_size_limit in pragmaConn, in bytes (8 MiB). It
// is twice what the log needs in ordinary running: SQLite checkpoints at 1000
// pages, which is 4,120,032 bytes of log at the 4096-byte page size, and a
// running install measured 4,124,152 to 4,136,512. A limit at or below that
// size would cut the file on every cycle, about 81 times a day, and make
// SQLite grow it again each time. At twice the size ordinary running never
// reaches it.
const walSizeLimit = 8 << 20

// walTrimRows is how many deleted rows make a cleanup or a clear worth
// emptying the log afterwards. A cleanup writes about 40 bytes of log for
// every row it deletes (38 to 43 measured). It deletes in chunks and the log
// restarts between them, so left alone it leaves the log at its ordinary 4 MB
// however many rows it took. A reader that holds its place across the cleanup
// (an export, a wide chart) keeps the log from restarting, and the log then
// keeps every chunk: at this many rows that comes to walSizeLimit. So this is
// the smallest cleanup that can leave more log behind than the limit allows.
// The hourly cleanup of a running install removes 5,040 rows, and 25,200 at
// the fastest probe interval, and must never reach it: emptying a log that is
// about to be refilled is work for nothing. A variable so a test can lower
// it: a fixture of this many rows is slow under the race detector.
var walTrimRows int64 = 200_000

// walTrimWait bounds the two waits a trim can make: for a free pooled
// connection, and inside SQLite for the writer and for readers still on the
// log. The second wait holds the writer, so this is the longest a trim can
// keep a probe round's write waiting. A probe write gives up after 5 s.
const walTrimWait = 250 * time.Millisecond

// walTrim is what a trim did.
type walTrim int

const (
	walTrimSkipped  walTrim = iota // nothing to do: no log (an in-memory database), or the caller is stopping
	walTrimDone                    // the log is empty
	walTrimNotTried                // stopped before TRUNCATE, so the writer was never taken: no free connection, another checkpoint running, or a reader that still needs part of the log
	walTrimGaveUp                  // TRUNCATE ran and something was still on the log, or writing, after walTrimWait
	walTrimFailed                  // the connection or SQLite refused
)

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

// OpenOption adjusts how Open treats a database it cannot read.
type OpenOption func(*openOpts)

// openOpts is what the OpenOptions set. Zero value = the default open: a
// damaged file is refused and left where it is.
type openOpts struct {
	rebuildOnCorruption bool
}

// RebuildOnCorruption arms the set-aside recovery: a database Open cannot read
// is renamed to <db>.<UTC>.corrupt (with its WAL/SHM sidecars) and an empty one
// is built in its place, so a service on Restart=always comes back monitoring
// instead of crash-looping on the same bad file. It is the daemon's -on-corrupt
// rebuild, and it is off by default for the reason recorded where the recovery
// runs: the history and every saved setting - login included - leave with the
// old file, and that is not a decision to take on an operator's behalf.
func RebuildOnCorruption() OpenOption {
	return func(o *openOpts) { o.rebuildOnCorruption = true }
}

// Open opens (creating if needed) the SQLite database at path and ensures the
// schema exists.
func Open(path string, opts ...OpenOption) (*Store, error) {
	// ONE reading: the clock the pause rows are judged with (nowU) and the
	// step detector's baseline are the same time.Time. Two separate readings -
	// even nanoseconds apart, and previously a whole slow Open apart - leave a
	// seam where an NTP correction lands between them: rows judged with the
	// stale clock, the corrected one installed as the baseline, and no step
	// ever visible afterwards to trigger the re-judgement.
	now := time.Now()
	return openAtClock(path, now.Unix(), now, false, opts...)
}

// OpenExisting opens the database at path as it is. It never creates one and
// never sets one aside: nothing at the path, an empty file, or a file that is
// not a database is refused, and a database that will not open is left exactly
// where it stood. The set-aside recovery Open performs is the daemon's - the
// alternative there is a service crash-looping on the same file - and a
// command an operator runs by hand must never take it on their behalf:
// `pingularity reset-auth` pointed at the key file beside the database used to
// rename the key to .corrupt, build an empty store under its name, clear the
// password on that, and report success.
func OpenExisting(path string) (*Store, error) {
	now := time.Now()
	return openAtClock(path, now.Unix(), now, true)
}

// containerDataDir is the image's own data directory - the Dockerfile VOLUME,
// with the ENTRYPOINT pinning -db inside it. The container carve-out below
// fires only for exactly this path. Var so tests can point it at a temp dir.
var containerDataDir = "/var/lib/pingularity"

// inContainerFn is util.InContainer, a var so tests can force either answer.
var inContainerFn = util.InContainer

// ownedByThisUserFn is osperm.OwnedByThisUser, a var so tests can model a
// directory owned by another user (root:<fsGroup> on a PVC) without root.
var ownedByThisUserFn = osperm.OwnedByThisUser

// realDirNoSymlink reports whether path is an actual directory and not a
// symlink - the carve-out must never chmod THROUGH a link (the pre-existence
// probe above deliberately Lstats for the same reason).
func realDirNoSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
}

// imageLineageMarker reports whether dir carries our image's data-dir marker:
// both Dockerfiles plant .pingularity-image-dir, and Docker's volume copy-up
// carries it into a fresh named volume, while an empty PVC or a plain bind mount
// does not. It is a strong heuristic, not a proof of volume TYPE: a bind mount
// restored or copied FROM a marker-bearing volume would carry the marker too and
// so be tightened. That is an acceptable outcome (the content is genuinely ours,
// owned by our uid, at our path); the marker exists to exclude the COMMON unsafe
// cases - a host directory or PVC that never held our data - not to prove the
// mount is a named volume.
func imageLineageMarker(dir string) bool {
	fi, err := os.Lstat(filepath.Join(dir, ".pingularity-image-dir"))
	return err == nil && fi.Mode().IsRegular()
}

// fsGroupShape reports whether a directory's mode and ownership look the way
// the kubelet's fsGroup ownership management leaves a PVC root: writable
// through its group bits, closed to world write, and not owned by our euid
// (the kubelet chowns the volume root to root:<fsGroup> and grants the group
// rwx). Mode and ownership only - proving our process is actually IN that
// group would take platform-specific Stat_t plumbing, and this only picks a
// log message: a miss falls back to the generic warning, never changes what
// gets tightened or left alone.
func fsGroupShape(perm os.FileMode, ownedByUs bool) bool {
	return !ownedByUs && perm&0o020 != 0 && perm&0o002 == 0
}

// looseDataDirWarning picks the boot warning for a pre-existing group/world-
// accessible data directory we refuse to re-permission. The generic advice
// ("use a dedicated -db directory") is useless on a Kubernetes PVC - the mount
// point is fixed by the pod spec and the looseness is the kubelet's fsGroup
// ownership, applied again on every mount - so when the directory has exactly
// that shape inside a container, name the fixes that exist there instead.
// Message selection only: the never-repermission rule above is identical for
// both messages.
func looseDataDirWarning(dir string) string {
	if fi, err := os.Lstat(dir); err == nil && inContainerFn() &&
		fsGroupShape(fi.Mode().Perm(), ownedByThisUserFn(dir)) {
		return fmt.Sprintf("pingularity: data directory %s is group-writable and owned by another user - the shape Kubernetes fsGroup ownership leaves on a PVC root, where group access is how this pod writes at all; leaving it unchanged (the database file itself is kept owner-only). If fsGroup is intentional, keep it (fsGroupChangePolicy: OnRootMismatch skips the per-mount re-chown); to make the directory owner-only and end this warning, chown it to this image's user (uid 65532) once and drop fsGroup.", dir)
	}
	return fmt.Sprintf("pingularity: data directory %s is group/world-accessible and was not created by pingularity; leaving its permissions unchanged (the database file itself is owner-only). Consider a dedicated -db directory.", dir)
}

// openAtClock is the full seam: the judging clock AND the wall reading the step
// detector baselines on (opened), as one pair. The detector measures time
// passing by the boot clock, read first thing below; opened's monotonic half is
// only its fallback, for a machine whose boot clock cannot be read (see
// sinceOpen). existing is OpenExisting's contract: the file must already be a
// database, and a database that will not open is never set aside.
func openAtClock(path string, nowU int64, opened time.Time, existing bool, opts ...OpenOption) (*Store, error) {
	// The boot clock is read first, before anything below can take time, so it
	// pairs with opened, which the caller read an instant ago. Read after a slow
	// Open - a migration, a rebuild - it would start the uptime late by that
	// long, and the first cleanup would see the wall clock that much ahead: a
	// step, once an Open takes longer than pruneClockStepSlack.
	openedBoot, openedBootOK := bootClockFn()
	var oo openOpts
	for _, opt := range opts {
		opt(&oo)
	}
	// PINGULARITY_TEST_DB_DIR redirects a ":memory:" open to a unique file
	// under the named directory - CI's file-backed matrix leg, and nothing
	// else. The in-memory pool is pinned to ONE connection (see Open below),
	// which makes every multi-connection failure mode structurally invisible
	// to the ordinary test run: the WAL snapshot-upgrade class busy_timeout
	// cannot absorb (SQLITE_BUSY_SNAPSHOT) shipped a measurement-loss bug past
	// this entire suite exactly that way, and had broken settings saves once
	// before. With the variable set, the SAME tests run against the
	// four-connection file-backed pool the daemon actually uses in production.
	// Production is unaffected: real deployments never pass ":memory:", and
	// the variable does nothing for a file path.
	if path == ":memory:" {
		if dir := os.Getenv("PINGULARITY_TEST_DB_DIR"); dir != "" {
			f, err := os.CreateTemp(dir, "pingularity-testdb-*.db")
			if err != nil {
				return nil, fmt.Errorf("test db dir: %w", err)
			}
			path = f.Name()
			_ = f.Close()
		}
	}
	// What KIND of thing sits at the path is settled BEFORE anything below
	// touches it. The securing chmod further down follows symlinks and the
	// set-aside recovery renames whatever it finds, and both used to act on
	// things that were never a file of ours: a directory typed for the file
	// inside it (-db /var/lib/pingularity) was left 0600 and untraversable, and
	// a symlink had its target chmod'ed through the link and was itself renamed
	// away. Only a regular file goes any further. What is IN that file is not
	// asked here on purpose - a database whose first page a power cut zeroed no
	// longer looks like one, and refusing it would take away the very recovery
	// below.
	//
	// A link is resolved rather than refused, and everything from here down -
	// the check, the chmod, the DSN, the set-aside - is about the file it
	// resolved to. See resolveDBPath: the harm was never that the -db path was
	// a link, it was that the repair acted on the link and the chmod acted on
	// its target, so they were about two different files.
	//
	// The one thing still judged by the path as typed is the data directory
	// below. pingularity.key and logs.txt are placed beside the -db flag, not
	// beside what it resolves to (secret.New and ringPath are handed the flag),
	// so behind a link that directory is where they sit: it is the one whose
	// looseness is worth a warning, the one an older build judged, and - when
	// the link sits in the image's own volume - the one the container carve-out
	// below exists for. The database file itself is secured where it really is.
	asTyped := path
	if path != ":memory:" {
		resolved, err := resolveDBPath(path)
		if err != nil {
			return nil, err
		}
		path = resolved
		if err := checkDBPath(path, existing); err != nil {
			return nil, err
		}
		// A rebuild that a start left between setting the damaged file aside and
		// putting its replacement in place is finished here, before anything
		// below can create a brand-new store at the empty path instead (see
		// finishInterruptedRebuild).
		if !existing {
			if err := finishInterruptedRebuild(path); err != nil {
				return nil, err
			}
		}
	}
	// Create the parent dir for file-backed databases (skips ":memory:" and bare
	// filenames, whose dir is "."). 0o700 because the DB holds secrets at rest
	// (bcrypt auth hash, webhook URLs) - not readable by other local users.
	if dir := filepath.Dir(asTyped); dir != "" && dir != "." {
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
			//
			// ONE narrow exception, all three conditions required: inside a
			// container, at exactly the image's own data path, owned by our
			// user. The image ships /var/lib/pingularity locked 0700, but
			// Linux Docker engines recreate a fresh named volume's root
			// group/world-accessible during copy-up - so every Linux docker
			// install warned on every start about a directory that IS ours by
			// construction. The shared-system-directory hazard this rule
			// guards against cannot apply there: nothing else lives at that
			// path in a single-process container. Tighten it and say so; if
			// the chmod fails, fall through to the honest warning.
			tightened := false
			if inContainerFn() && dir == containerDataDir && realDirNoSymlink(dir) &&
				ownedByThisUserFn(dir) && imageLineageMarker(dir) {
				// Verify the chmod TOOK before claiming it: on a filesystem that
				// accepts-but-ignores chmod the honest warning must survive, and
				// must not be replaced by a false "tightened" every boot.
				if err := osperm.SecureDir(dir); err == nil {
					if still, known := osperm.GroupOrWorldAccessible(dir); known && !still {
						tightened = true
						log.Printf("pingularity: tightened data directory %s to owner-only (the image ships it that way; the build or volume setup had loosened it)", dir)
					}
				}
			}
			if !tightened {
				log.Printf("%s", looseDataDirWarning(dir))
			}
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
	// Applying the base schema, the additive column migrations, the index
	// cleanup and the at-Open data repairs is one operation we may need to run
	// twice: once on the existing file, then again on a freshly rebuilt one if
	// the first attempt reveals the file is corrupt (see the recovery below).
	initialize := func(db *sql.DB) error {
		// First, while the schema still shows whether a build that records the
		// engine split has had this file (see the function). This one stays
		// outside the transaction below: its delete is allowed to meet damage and
		// leave its row, and damage met inside a transaction fails everything the
		// transaction tries after it.
		if err := forgetRestoredSplitMarker(db); err != nil {
			return err
		}
		// The migration is ONE transaction: the base schema, the added columns
		// and the new indexes. Building an index is the first read of a table an
		// older release never read at startup, so that is where damage that sat
		// dormant under the older release surfaces on the upgrade. Committed
		// statement by statement, the steps before it stayed behind in a file the
		// start then refused - server_health among them, which is how the marker
		// check above tells a file this release has opened from one it has not -
		// so a marker restored into that file later, under the older release, was
		// believed at the next upgrade. Rolled back, a file refused on damage the
		// migration meets keeps nothing of it; the one change a start can already
		// have made is that delete of a marker restored from another database, a
		// row no older release reads. Damage only the repairs below read is met
		// after the commit, and a file refused on that keeps the migration.
		if err := migrateInOneTransaction(db); err != nil {
			return err
		}
		// Repair what an OLDER build's unvalidated InsertPause left behind. The
		// PauseSpanSane guards hold every NEW write and import to one rule, but a row
		// already on disk answers to nobody: applySchema migrates columns, not data,
		// and Prune's straddle rule deliberately keeps any pause whose END is still
		// inside retention - so an epoch-boot span persisted before the guard existed
		// keeps zeroing coverage for up to a retention year after the upgrade.
		if err := repairInsanePausesAt(db, nowU); err != nil {
			return err
		}
		// And the events-table twin: outage lengths a pre-guard import let in, plus a
		// count of the unreadable types sitting beside them (reported, never deleted -
		// see the function).
		if err := reportUnreadableEventTypes(db); err != nil {
			return err
		}
		if err := repairInsaneEventDurations(db); err != nil {
			return err
		}
		// And the typing door's own residue: values no read can convert, in the
		// whole-number columns the import allowlist guards but nothing migrated. Unlike
		// the two repairs above (cheap point deletes on small tables), this one full-scans
		// samples/dns/speed with typeof() on every Open before the listener binds, so it
		// is gated on a persisted, versioned generation marker: run it once per DB, stamp
		// the generation on success, and skip it forever after. The clock-dependent future-
		// pause repair below is deliberately NOT gated - its verdict changes with the clock.
		if gen, err := repairGeneration(db); err != nil {
			return err
		} else if gen < intColumnRepairGen {
			if err := repairUnreadableIntColumns(db); err != nil {
				return err
			}
			if err := stampRepairGeneration(db, intColumnRepairGen); err != nil {
				return err
			}
		}
		return nil
	}
	rebuilt := false // this Open set a damaged database aside and started over on an empty one
	if err := initialize(db); err != nil {
		// A malformed or truncated main DB (typically a hard power-off mid-
		// checkpoint) makes a statement fail with "database disk image is
		// malformed" or "file is not a database". Under systemd's Restart=always
		// this crash-loops forever on the same bad file and monitoring never
		// recovers. For a file-backed store, move the corrupt DB (and its WAL/SHM
		// sidecars) ASIDE - never delete user data - and rebuild an empty store so
		// the daemon comes back up monitoring. The net covers every step of
		// initialize, not only the schema: that step reads just sqlite_master, so
		// a torn page in a table the repairs scan sailed past it and surfaced as
		// "malformed" from the repair instead - the same file, the same crash
		// loop, but with nothing set aside. A :memory: store, an OpenExisting
		// caller or a non-corruption error still fails fast.
		//
		// So does damage itself, unless the operator armed the recovery. The crash
		// loop is real, but as a DEFAULT this trades a fault the operator can still
		// act on for one they cannot: the file is replaced, the dashboard comes back
		// empty, and nothing about a running daemon says so. Damage sitting in a
		// table an earlier release never read at startup is dormant until some
		// release reads it - an added index does, and an upgrade is precisely when
		// nobody is watching the logs. A refusal keeps every option open - the file
		// stays where it is, sqlite3's .recover can try to read it back, and a build
		// whose startup never reads the damaged table still opens it - while a
		// rebuild closes them. (How much .recover gets back depends on where the
		// damage sits: most of the file for a torn table page, and anything from
		// all of it to nothing for a torn first page - a header wiped alone reads
		// back whole, a zeroed first page comes back as lost_and_found rows with no
		// settings table, random bytes over it as an empty file - which is why the
		// message below says to check the copy before it replaces anything. The
		// migration is one transaction, so damage it meets - the new indexes are
		// what reach a table an older release never read - is refused with none of
		// it committed; damage only the repairs after it read is met once it has
		// committed, and that file keeps this release's tables, columns and indexes
		// (see initialize). Nothing is moved, replaced or deleted, which is the part
		// that cannot be taken back.) None of this restores an older release's
		// behaviour: those replaced a database they could not open too, and the only
		// reason this damage is new is that their startup never read the table it
		// sits in. So a box that a power cut would have silently restarted empty now
		// stops until somebody looks at it, which is the trade this default is
		// making on purpose.
		if path == ":memory:" || existing || !oo.rebuildOnCorruption || !dbCorrupt(err) {
			db.Close()
			if existing && dbCorrupt(err) {
				// Name the FILE. This door is what the hand-typed recovery
				// commands use, and the driver's own "file is not a database
				// (26)" says nothing about which path produced it - the key
				// file beside the database is one tab-completion away from it.
				return nil, fmt.Errorf("%s is not a pingularity database, or is damaged past opening: %w", path, err)
			}
			if path != ":memory:" && dbCorrupt(err) {
				// The whole refusal in one message: nothing was touched, here
				// is how to try to get the data back, and here is the switch
				// that takes the other road. An operator reads this in
				// `systemctl status`/`docker logs` with the daemon down, so it
				// has to carry the commands rather than point at a manual.
				//
				// And it must not promise what .recover cannot do. For a torn
				// table page it reads nearly everything back; random bytes over
				// the first page leave an empty file and exit 0, and a zeroed
				// first page or damage just past the header a copy with no
				// settings table. Put in place, either is a brand-new install - no login,
				// and on the network if -access network says so - which is the
				// open dashboard a refusal exists to prevent, reached by doing
				// what the message said. Which shape this is cannot be told from
				// the error (the same "not a database" comes back for a header
				// .recover reads straight past), so the check is the operator's,
				// and the message hands them the command for it and says where
				// to go when it comes back empty.
				q := shellQuotePath(runtime.GOOS, path)
				rq := shellQuotePath(runtime.GOOS, path+".recovered")
				return nil, fmt.Errorf("database %s is damaged (%w); it has not been moved or replaced - try to recover it with `sqlite3 %s .recover | sqlite3 %s`, then check what came back before it replaces anything: `sqlite3 %s \"SELECT key FROM settings\"` lists the install's saved settings, auth_hash among them if it had a password. How much comes back depends on where the damage is, and a torn first page can leave nothing at all. If the list is there, put the copy in the damaged file's place and start again; if the copy is empty or has no settings, do not put it in place - start with -on-corrupt rebuild instead, which sets the damaged file aside, begins again on an empty store that has lost the history and every saved setting (login, network access, thresholds, notifications), and holds network access to this machine until a password is set on it and the daemon restarted or reloaded", path, err, q, rq, rq)
			}
			return nil, err
		}
		// Ask the doomed file, while it is still open, whether the install it
		// belonged to had got past its first run. See firstRunInDoomedDB.
		firstRun := firstRunInDoomedDB(db)
		db.Close()
		stats.Inc("db.corrupt")
		// The replacement is built whole, beside the damaged file, BEFORE that
		// file moves, and only then do the two change places. Building it where
		// the damaged file had been, after moving that file aside, left every
		// failure in between - a full disk while the schema was written, an I/O
		// error, a crash or a power cut - holding an empty store with no hold row
		// in it at the database's path. The next start found that store healthy
		// and brand new, and -access network put it on the network with no login:
		// the outcome the hold row below exists to prevent, reached by failing to
		// write it. Built first, a failure leaves the damaged file exactly where
		// it was, to be refused or rebuilt again; and the one step left between
		// the set-aside and the replacement is a rename, which the next Open
		// finishes if a start dies across it (finishInterruptedRebuild).
		built, berr := buildRebuiltStore(path, nowU, firstRun, initialize)
		if berr != nil {
			return nil, fmt.Errorf("database %s is damaged (%v), and the empty store to replace it could not be built, so the damaged file has not been moved: rebuild after corruption: %w", path, err, berr)
		}
		moved, qerr := quarantineCorruptDB(path)
		if qerr != nil {
			return nil, qerr
		}
		if rerr := renameIntoPlaceFn(built, path); rerr != nil {
			return nil, fmt.Errorf("put the rebuilt store in place of %s: %w", path, rerr)
		}
		lost := "monitoring continues on it, but the history and every saved setting (login, network access, thresholds, notifications) are gone with the old file"
		if firstRun {
			lost = "the old file was still inside its first-run offer, so the rebuilt store starts over as one: nothing is being measured until Quick Setup is answered or its 48h grace runs out"
		}
		log.Printf("pingularity: database %s is corrupt (%v); moved aside to %s and rebuilt an empty store - %s", path, err, moved, lost)
		if db, err = sql.Open("sqlite", buildDSN(path)); err != nil {
			return nil, fmt.Errorf("open db (post-recovery): %w", err)
		}
		db.SetMaxOpenConns(4) // file-backed: recovery only runs for path != ":memory:"
		rebuilt = true
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
	st := &Store{
		db:           db,
		recCache:     map[int64]recScan{},
		seriesCache:  map[seriesKey]*seriesEntry{},
		opened:       opened, // the top-of-Open reading: the wall baseline, and sinceOpen's fallback
		openedBoot:   openedBoot,
		openedBootOK: openedBootOK,
		rebuilt:      rebuilt,
	}
	st.clockBase, st.clockBaseUp = st.opened.Round(0), 0
	st.held.init()
	st.live.floor = math.MaxInt64 // no outage of this handle's monitor dropped from its list
	// When the clock could not anchor the future-end pause repair above, arm the
	// lazy re-judgement: the first write under a plausible clock runs it instead.
	if nowU < plausibleEpoch {
		st.pauseRepairArm.Store(1)
	}
	return st, nil
}

// rebuiltSuffix names the replacement a rebuild builds beside a damaged
// database before either file moves (see buildRebuiltStore).
const rebuiltSuffix = ".rebuilt"

// renameIntoPlaceFn is os.Rename, for the step that puts a rebuilt store at the
// database's path - a var so tests can make that one rename fail, which nothing
// in a real directory does just after the damaged file left the same one.
var renameIntoPlaceFn = os.Rename

// buildRebuiltStore builds, beside the damaged database at path, the whole store
// a rebuild puts in its place - the schema and at-Open repairs, the install
// anchor, the access hold - closes it, which folds its WAL into the one file,
// and returns that file's path (<path>.rebuilt). Nothing about the damaged
// database is touched. A build that fails part-way is removed and its error
// returned, so what it leaves behind is what was there before it started.
func buildRebuiltStore(path string, nowU int64, firstRun bool, initialize func(*sql.DB) error) (string, error) {
	built := path + rebuiltSuffix
	remove := func() error {
		for _, sfx := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(built + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	// A build an earlier start abandoned, or a finished one it never put in
	// place over a damaged file that is still here: this start builds its own.
	if err := remove(); err != nil {
		return "", fmt.Errorf("clear an earlier rebuild: %w", err)
	}
	db, err := sql.Open("sqlite", buildDSN(built))
	if err != nil {
		return "", err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (string, error) {
		db.Close()
		_ = remove()
		return "", err
	}
	if err := initialize(db); err != nil {
		return fail(err)
	}
	// The rebuilt store belongs to an install that already existed - the
	// database just moved aside was its - and it has to say so. An empty
	// store reads as a brand-new install to the first-run decision, which
	// then seeded a fresh offer clock and held monitoring for the whole
	// consent grace: two days of measuring nothing on a box that consented
	// long ago, the boot notice claiming a first run, and the health
	// endpoint green throughout. The install anchor is the evidence that
	// decision reads (settings.EstablishedInStore): as far as this store can
	// vouch for it, monitoring began at the rebuild.
	//
	// Unless the old file said otherwise while it could still be read. An
	// install still INSIDE its first-run offer has consented to nothing
	// yet, and anchoring one would start it probing with the defaults and
	// retire the Quick Setup dialog it was never shown. So the anchor is
	// withheld only on that positive evidence, and a file too damaged to
	// answer keeps monitoring: a database ruined past reading is far more
	// often one that had been written to for months than one three hours
	// old, holding wrongly costs two days of the measurement this daemon
	// exists to do, and releasing wrongly costs what the offer's own 48h
	// expiry would have done anyway - minus the dialog, which is the part
	// worth avoiding when it can be.
	if !firstRun {
		if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
			firstSeenKey, strconv.FormatInt(nowU, 10)); err != nil {
			return fail(fmt.Errorf("anchor rebuilt store: %w", err))
		}
	}
	// And the store records, in itself, that nobody has a login on it. The
	// password left with the file set aside, and a daemon told -access
	// network would otherwise answer the network over an empty store with
	// nothing to check a visitor against - not only at this start but at
	// every later one, because the next start finds this database healthy
	// and has no other way to know. See AccessHoldAfterRebuild.
	if _, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		accessHoldKey, strconv.FormatInt(nowU, 10)); err != nil {
		return fail(fmt.Errorf("hold access on rebuilt store: %w", err))
	}
	if err := db.Close(); err != nil {
		_ = remove()
		return "", fmt.Errorf("close the rebuilt store: %w", err)
	}
	// One file, whole: the rename that puts it in place moves the main file only,
	// and a WAL left beside the old name - the hold row in it, if the checkpoint
	// the close runs did not finish - would not follow it.
	if fi, err := os.Stat(built + "-wal"); err == nil && fi.Size() > 0 {
		_ = remove()
		return "", fmt.Errorf("the rebuilt store's WAL still holds %d bytes after it was closed", fi.Size())
	}
	return built, nil
}

// finishInterruptedRebuild completes a rebuild that a start left between
// setting a damaged database aside and putting its finished replacement in
// place: nothing at path, and <path>.rebuilt beside it. Left alone, this Open
// would create a brand-new store at the empty path - no install anchor, and no
// hold on network access, so -access network would put it on the network with
// no login - while the store the rebuild finished, carrying both, sat unused
// beside it. Sidecars the damaged file still has at path (a WAL the set-aside
// did not reach) are set aside first, as the rebuild would have: renamed into
// place beside them, the rebuilt store would be read through the old WAL. The
// build finishes before the damaged file moves, so a build a start died in the
// middle of sits beside a damaged database still at path, is never put in place
// here, and is cleared by the next rebuild.
func finishInterruptedRebuild(path string) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	built := path + rebuiltSuffix
	if fi, err := os.Lstat(built); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	if _, err := quarantineCorruptDB(path); err != nil {
		return err
	}
	if err := renameIntoPlaceFn(built, path); err != nil {
		return fmt.Errorf("put the rebuilt store in place of %s: %w", path, err)
	}
	log.Printf("pingularity: database %s had been set aside as damaged by a start that stopped before putting the store it rebuilt in its place; that store is in place now", path)
	return nil
}

// migrator is what the migration steps write through: the one connection Open
// runs them on (see migrateInOneTransaction), or a database handle.
type migrator interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// migrateInOneTransaction runs applySchema as one transaction, on a connection of
// its own that is handed back before anything else runs - a rebuild caps the pool
// at one. The transaction takes the write lock before its first read (BEGIN
// IMMEDIATE), so a writer busy on the same file makes it wait out the busy
// timeout, as the statements it replaced did. Begun as an ordinary transaction,
// its first read took a shared lock that SQLite will not upgrade while another
// connection writes, and it does not call the busy handler for that: the first
// start on an older release's file failed "database is locked" at once whenever
// something else was writing to it, and so did reset-auth.
func migrateInOneTransaction(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if err := applySchema(connMigrator{ctx, conn}); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// connMigrator is a migrator over one pinned connection.
type connMigrator struct {
	ctx  context.Context
	conn *sql.Conn
}

func (m connMigrator) Exec(query string, args ...any) (sql.Result, error) {
	return m.conn.ExecContext(m.ctx, query, args...)
}

func (m connMigrator) QueryRow(query string, args ...any) *sql.Row {
	return m.conn.QueryRowContext(m.ctx, query, args...)
}

// applySchema creates the base tables, runs the additive column migrations, and
// drops a retired index. Idempotent: re-running an ALTER on an already-migrated
// table fails with "duplicate column name", the expected no-op. Any other error
// (locked, disk full, corrupt) is real and returned, because continuing would
// leave a column missing and every statement naming it failing forever. Open
// runs this twice when it has to rebuild a corrupt file.
func applySchema(db migrator) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Migrate older DBs: per-run speed connection columns (TEXT), numeric per-run
	// columns (packet loss %, health flag, jitter, data volumes), latency under
	// load (idle baseline + per-phase loaded medians, then p95 tails), and the
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
		{"speed", "loaded_up_ms", "REAL"}, {"speed", "loaded_down_p95_ms", "REAL"},
		{"speed", "loaded_up_p95_ms", "REAL"},
		{"speed", "ping_best_ms", "REAL"},
		{"speed", "ip_family", "TEXT"}, {"speed", "udp_direction", "TEXT"},
		{"speed", "failed", "INTEGER"},
		{"speed", "usage_run_ts", "INTEGER"},
		{"speed", "race_outcome", "TEXT"}, {"speed", "race_origins", "TEXT"},
		{"speed", "race_winner_kind", "TEXT"}, {"speed", "race_winner_label", "TEXT"},
		{"speed", "race_winner_ms", "REAL"}, {"speed", "race_racers", "INTEGER"},
		{"speed", "race_winner_lat", "REAL"}, {"speed", "race_winner_lon", "REAL"},
		{"speed", "round_ts", "INTEGER"},
		{"speed", "fail_stage", "TEXT"},
		{"samples", "family", "TEXT"},
	}
	for _, m := range migrations {
		if _, err := db.Exec(`ALTER TABLE ` + m.table + ` ADD COLUMN ` + m.col + ` ` + m.typ); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.col, err)
		}
	}
	// An index naming a MIGRATED column has to be created here, after the loop
	// above has added it: a legacy database reaches this point with a speed table
	// that has no usage_run_ts, and naming it in the base schema fails the whole
	// migration with "no such column".
	//
	// This one carries the accounting row's reference to the run it bills for.
	// DeleteSpeed cascades on it once per delete; without it that cascade scans
	// every speedtest ever recorded. Partial, because only accounting rows carry a
	// value and they are a small minority of the table.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_speed_usage_run_ts ON speed(usage_run_ts) WHERE usage_run_ts IS NOT NULL`); err != nil {
		return fmt.Errorf("migrate index idx_speed_usage_run_ts: %w", err)
	}
	// round_ts likewise: DeleteSpeed cascades on it, and SpeedAvgBytes sums a
	// round's members per run - forty correlated lookups per refresh, each a
	// full scan without this. Partial for the same reason: only a kept
	// Best-of round's losers carry a value.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_speed_round_ts ON speed(round_ts) WHERE round_ts IS NOT NULL`); err != nil {
		return fmt.Errorf("migrate index idx_speed_round_ts: %w", err)
	}
	// The per-server reads of the selection reports (RecentRankPings for the
	// picker's saved pane, RecentServerScores for the challenge verdict) ask
	// "this server's last N rows": without this they walk the whole table
	// through idx_speed_servers_ts - measured 55 ms and ~100k rows shipped to
	// Go per settings-drawer open at a year of hourly runs, 675 ms at a
	// five-minute cadence - and a kept server that was never ranked walks it
	// to the end. With it each is a seek and a LIMIT. Twelve index inserts
	// per run, ~30 bytes a row.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_speed_servers_server ON speed_servers(server_id, run_ts)`); err != nil {
		return fmt.Errorf("migrate index idx_speed_servers_server: %w", err)
	}
	// Drop the unused (target, ts) index from existing DBs: no query filters by
	// target, and the one per-target query pins idx_samples_ts - so it was pure
	// write/storage cost on the hot probe path.
	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_samples_target_ts`); err != nil {
		return fmt.Errorf("migrate: drop idx_samples_target_ts: %w", err)
	}
	return nil
}

// repairInsanePausesAt deletes pause rows that today's writers refuse but an older
// InsertPause accepted (its only check was a positive duration) - above all the
// epoch-boot span an RTC-less board recorded across its NTP correction, half a
// million hours subtracted from every uptime window. Runs at every Open:
// idempotent, and the pauses table is small (one row per episode).
//
// The clock-free half of the pause rule (pauseSpanBounded, in SQL) always
// applies - non-positive, starting before plausibleEpoch, longer than
// maxPauseDuration, or placed where ts+duration_s would overflow. The
// now-anchored future-end half applies ONLY when the clock reading it is
// plausible: at open time the clock may be the very epoch-boot clock this repair
// cleans up after, and against such a clock every genuine row looks future - the
// same trap Prune's plausibleEpoch skip and pauseSpanImportable's fallback exist
// to avoid. But that import fallback is also how a future-reaching row gets IN:
// under an epoch clock the importer can only apply the clock-free bounds, so a
// crafted span ending a decade ahead is accepted - and nothing else ever
// re-judges it (Prune's pause DELETE removes only future-STARTED rows, and
// re-imports merge by ts, so a corrected file cannot displace it). Once a
// plausible clock IS available, such a row is exactly what PauseSpanSane refuses
// from a live writer and cannot be checkpointed history, so it goes here.
//
// Split out with a caller-supplied clock so tests can prove the epoch-clock
// behaviour without faking time. Open passes its own reading; when that reading
// cannot anchor the future-end half, maybeRepairFuturePausesAt re-runs that
// half on the first write that observes a plausible clock.
func repairInsanePausesAt(db *sql.DB, nowU int64) error {
	// The ts ceiling implies no surviving row can overflow: with duration_s capped
	// at maxPauseDuration, ts <= MaxInt64-maxPauseDuration keeps ts+duration_s in
	// range - and a start beyond year 292 billion describes nothing anyway. Kept as
	// constant arguments so no per-row arithmetic can itself overflow (SQLite errors
	// on integer overflow rather than wrapping).
	//
	// The CAST clauses are the TYPING half of the import door (the intCols
	// allowlist + normVal), applied at rest: a value that is not whole integer
	// seconds - a fractional REAL from raw SQL or a pre-allowlist build, in range
	// so the bounds above pass it - makes every read that scans the column into an
	// int64 (pauseSpans, behind coverage and the heatmap) error permanently. CAST
	// truncates, so a whole number compares equal whatever its storage class and
	// is untouched; anything else is as unreadable as an out-of-range span and
	// goes the same way.
	//
	// Applied to the quarantine table too, on the same rule and in the same breath:
	// a row waiting there can be handed BACK to pauses (see
	// repairFutureReachingPausesAt), so it must already satisfy every clock-free
	// bound - including the ts ceiling, which is what lets the restore compute
	// ts + duration_s in SQL without risking the overflow SQLite errors on.
	var repaired int64
	for _, tbl := range []string{"pauses", "pauses_quarantine"} {
		res, err := db.Exec(`DELETE FROM `+tbl+` WHERE duration_s <= 0 OR duration_s > ? OR ts < ? OR ts > ?
			OR CAST(duration_s AS INTEGER) != duration_s OR CAST(ts AS INTEGER) != ts`,
			maxPauseDuration, plausibleEpoch, int64(math.MaxInt64)-maxPauseDuration)
		if err != nil {
			recordDBErr(err)
			return fmt.Errorf("repair pauses: %w", err)
		}
		if n, e := res.RowsAffected(); e == nil {
			repaired += n
		}
	}
	if repaired > 0 {
		stats.Add("db.pause_rows_repaired", repaired)
		log.Printf("pingularity: removed %d pause row(s) an older build stored without validation (not a span, not whole integer seconds, longer than any retainable history, or starting before the project existed); they were subtracted from every uptime window as unobserved time", repaired)
	}
	// The future-end half, only under a clock that can anchor it.
	if nowU >= plausibleEpoch {
		return repairFutureReachingPausesAt(db, nowU)
	}
	return nil
}

// repairFutureReachingPausesAt is the now-anchored half of the pause repair: hold
// aside spans ending further past the present than any believable clock
// disagreement. The clock-free DELETE in repairInsanePausesAt has already removed
// every row that could make ts+duration_s overflow - in both tables - so the
// endpoints are safe to compute in SQL. The skew is deliberately more generous
// than the live writer's pauseFutureSkew: a plausible clock can still be wrong by
// whole timezones or stepped back hours after running fast, while everything a
// believable writer produced ends within pauseFutureSkew of ITS clock, hours (not
// years) from ours at worst. Rows a behind-but-plausible destination deliberately
// imported are safe by construction: the importer bounds their ends at its own
// now+skew, which is already the past of any later reading of the same clock.
//
// It MOVES rows rather than deleting them, because the clock it judges with is not
// good enough for a permanent verdict. Any reading at/after plausibleEpoch used to
// be trusted, so a batteryless RTC booting in 2024 with a valid 2026 database
// cleared the threshold and destroyed real 2025-2026 pause rows before NTP synced
// - and a deleted pause turns unobserved time into observed time, inflating uptime
// with nothing left on disk to correct it. The frozen 2023 epoch widens that window
// every year. So each run is one transaction that:
//
//   - FIRST restores quarantined rows whose end is within nowU+skew. A later,
//     corrected clock exonerates them; the row was never disbelieved on its own
//     merits, only against a clock that could not judge it. One row per ts is the
//     invariant the rest of the table assumes (export/import merge pause rows on ts),
//     so an exonerated held row with no live twin is inserted, and one that collides
//     with a live row at that ts is merged into it - keeping the LONGER of the two
//     spans. That merge is the point: the old NOT-EXISTS skip dropped a held row
//     whenever a live row shared its ts and then deleted it from the quarantine
//     anyway, silently destroying the longer span (a 7000s hold behind a 60s live
//     row lost 6940s). Overlapping same-ts spans at read time are already unioned by
//     mergeSpans (de13222), so this is not about double-counting a denominator - it
//     is about not losing a span and not leaving a duplicate ts.
//   - THEN moves rows ending beyond nowU+skew out of pauses, deduped against the
//     quarantine (keeping the longer span) so a re-import or a re-recorded span at a
//     ts already held does not pile up a second held row every Open. Order matters:
//     doing it the other way round would quarantine and immediately re-restore the
//     same row on a clock sitting near the horizon.
//
// A crafted decade-ahead row is therefore still out of the coverage math on the
// first plausible open - the hole 052b50a closed stays closed - and stays out
// forever, since no plausible clock ever reaches its end. Nothing trusts the clock
// to be RIGHT any more, only to be a clock.
//
// Restoration happens here and on a bounded trigger: the lazy re-judgement
// consumes its generation once it has run (see maybeRepairFuturePausesFn), and
// a DETECTED clock step arms a fresh one - after the step survives the
// pruner's settle window (clockStepped, read hourly by the pruner), because a
// clock pruning does not yet trust must not re-judge history either. A process
// whose clock syncs after Open therefore re-judges within roughly the settle
// window plus an hour plus one write, not at the next restart. Arming is per
// correction, never per write: re-arming while the quarantine is occupied
// would otherwise run this transaction on every probe round for as long as one
// crafted row sits there. And a backup taken between a wrong judgement and
// the step-triggered repair still carries the held rows: pauses_quarantine
// exports under its own key, lands back in the destination's quarantine, and
// the import arms a re-judgement so the destination's clock decides what to
// exonerate.
//
// Shared by the at-Open path and the lazy first-plausible-write path
// (maybeRepairFuturePausesAt): both must apply the identical judgement, or the
// hardware that never Opens under a plausible clock keeps the rows forever.
// pauseRepairFn is the deferred-repair call the Prune path routes through, a var
// so a test can force the repair to fail and exercise Prune's defer-the-delete
// guard. Production always uses the real implementation.
var pauseRepairFn = repairFutureReachingPausesAt

func repairFutureReachingPausesAt(db *sql.DB, nowU int64) error {
	horizon := nowU + pauseRepairFutureSkew
	// One transaction: a half-applied move either loses a row (deleted from pauses,
	// not yet held) or duplicates it (held and still live), and both are wrong in the
	// uptime denominator.
	tx, err := db.Begin()
	if err != nil {
		recordDBErr(err)
		return fmt.Errorf("repair future pauses: %w", err)
	}
	defer tx.Rollback()
	// RESTORE. Merge FIRST into any live twin, so the INSERT below (which skips a ts
	// already live) never re-touches a row it just created. max() keeps the LONGER
	// span; MAX() collapses several held rows at one ts. Only exonerated held rows
	// (end within the horizon) participate, in the merge and the insert alike.
	//
	// A twin that itself reaches past the horizon is neither merged into nor
	// counted as "already live": its span is the longer by construction (it ends
	// beyond the horizon, the exonerated row ends inside it), so max() would keep
	// the twin, the held copy would be deleted below as represented, and the MOVE
	// OUT would then hold the twin aside - the span the clock just vouched for gone
	// from both tables, and only the row no clock will ever exonerate left. The
	// exonerated row is inserted beside such a twin, and the twin alone moves out.
	resMerge, err := tx.Exec(`UPDATE pauses SET duration_s = max(duration_s,
			(SELECT MAX(q.duration_s) FROM pauses_quarantine q
			 WHERE q.ts = pauses.ts AND q.ts + q.duration_s <= ?))
		WHERE ts + duration_s <= ?
		  AND EXISTS (SELECT 1 FROM pauses_quarantine q
			WHERE q.ts = pauses.ts AND q.ts + q.duration_s <= ?)`, horizon, horizon, horizon)
	if err != nil {
		recordDBErr(err)
		return fmt.Errorf("merge quarantined pauses: %w", err)
	}
	resIn, err := tx.Exec(`INSERT INTO pauses (ts, duration_s)
		SELECT q.ts, MAX(q.duration_s) FROM pauses_quarantine q
		WHERE q.ts + q.duration_s <= ?
		  AND NOT EXISTS (SELECT 1 FROM pauses p WHERE p.ts = q.ts AND p.ts + p.duration_s <= ?)
		GROUP BY q.ts`, horizon, horizon)
	if err != nil {
		recordDBErr(err)
		return fmt.Errorf("restore quarantined pauses: %w", err)
	}
	// Every exonerated held row is now represented in pauses - inserted if it had no
	// live twin, merged into the twin (keeping the longer span) if it did - so the
	// held copies can go. Leaving any behind would re-offer it on every open, and an
	// exonerated row stranded in quarantine is invisible limbo: out of coverage and
	// out of every export.
	if _, err := tx.Exec(`DELETE FROM pauses_quarantine WHERE ts + duration_s <= ?`, horizon); err != nil {
		recordDBErr(err)
		return fmt.Errorf("restore quarantined pauses: %w", err)
	}
	// MOVE OUT, deduped against the quarantine. Merge FIRST into a held row already at
	// that ts (keeping the longer span), then insert only future-reaching pauses rows
	// whose ts is not already held, so repeated Opens neither pile a second row at one
	// ts nor shrink a span already held there. GROUP BY collapses several future rows
	// at one ts to the longer one.
	if _, err := tx.Exec(`UPDATE pauses_quarantine SET duration_s = max(duration_s,
			(SELECT MAX(p.duration_s) FROM pauses p
			 WHERE p.ts = pauses_quarantine.ts AND p.ts + p.duration_s > ?))
		WHERE EXISTS (SELECT 1 FROM pauses p
			WHERE p.ts = pauses_quarantine.ts AND p.ts + p.duration_s > ?)`, horizon, horizon); err != nil {
		recordDBErr(err)
		return fmt.Errorf("quarantine future pauses: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO pauses_quarantine (ts, duration_s)
		SELECT p.ts, MAX(p.duration_s) FROM pauses p
		WHERE p.ts + p.duration_s > ?
		  AND NOT EXISTS (SELECT 1 FROM pauses_quarantine q WHERE q.ts = p.ts)
		GROUP BY p.ts`, horizon); err != nil {
		recordDBErr(err)
		return fmt.Errorf("quarantine future pauses: %w", err)
	}
	resOut, err := tx.Exec(`DELETE FROM pauses WHERE ts + duration_s > ?`, horizon)
	if err != nil {
		recordDBErr(err)
		return fmt.Errorf("quarantine future pauses: %w", err)
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return fmt.Errorf("repair future pauses: %w", err)
	}
	if n, e := resOut.RowsAffected(); e == nil && n > 0 {
		stats.Add("db.pause_rows_repaired", n)
		log.Printf("pingularity: held aside %d pause row(s) ending further past the present than any believable clock disagreement (accepted earlier, when this machine's clock could not judge them); they were subtracting unobserved time from windows that have not happened yet, and a later open under a corrected clock gives back any that turn out to be real", n)
	}
	if n, e := resIn.RowsAffected(); e == nil && n > 0 {
		stats.Add("db.pause_rows_restored", n)
		log.Printf("pingularity: restored %d pause row(s) held aside earlier, now that the clock can see their span is in the past (a stale clock at the previous start could not); the unobserved time they record is back out of the uptime denominator", n)
	}
	if n, e := resMerge.RowsAffected(); e == nil && n > 0 {
		stats.Add("db.pause_rows_merged", n)
		log.Printf("pingularity: merged %d held pause row(s) back into a live span recorded at the same second, keeping the longer of the two; the previous restore dropped the held span whenever a live row shared its ts, losing the longer unobserved span from the uptime denominator", n)
	}
	return nil
}

// maybeRepairFuturePauses runs the deferred future-end pause repair when a
// trigger generation is armed and the clock is plausible: Open's skipped
// judgement (implausible boot clock - the RTC-less board the epoch-clock
// import fallback serves) and the settled-step re-judgement both arrive here,
// from the per-round write paths (InsertSamples, InsertPause). Cheap by
// design: the disarmed fast path is two atomic loads and no query runs until
// a plausible clock is actually observed.
//
// The wall clock is read INSIDE, only after observing the armed generation -
// never taken from the caller. A caller-supplied timestamp could be captured
// before the very correction that armed the trigger (the writer preempted
// between time.Now() and the flag check), consuming the only trigger and
// re-judging with the pre-correction clock: nothing restored, nothing left
// armed. Reading after the observation orders the clock after the event.
func (s *Store) maybeRepairFuturePauses() {
	s.maybeRepairFuturePausesFn(func() int64 { return time.Now().Unix() })
}

func (s *Store) maybeRepairFuturePausesFn(nowFn func() int64) {
	if !s.pauseRepairArmed() {
		return // the every-write fast path
	}
	s.pauseRepairMu.Lock()
	defer s.pauseRepairMu.Unlock()
	arm := s.pauseRepairArm.Load()
	if arm == s.pauseRepairDone.Load() {
		return // another writer already judged this generation
	}
	nowU, trusted := s.repairReading(nowFn)
	if !trusted {
		return // a step is settling: stays armed, no clock may judge yet
	}
	if nowU < plausibleEpoch {
		return // stays armed; the first plausible write judges
	}
	if err := pauseRepairFn(s.db, nowU); err != nil {
		// Stays armed (done not advanced) - the DELETE is idempotent and a
		// later write retries. The write itself is never failed for it.
		log.Printf("pingularity: deferred pause repair failed (a later write retries): %v", err)
		return
	}
	s.pauseRepairDone.Store(arm)
}

// pauseRepairArmed reports whether a re-judgement generation is owed.
func (s *Store) pauseRepairArmed() bool {
	return s.pauseRepairArm.Load() != s.pauseRepairDone.Load()
}

// pauseRepairFutureSkew is how far past the repairing clock a stored pause may
// end and still be believed at Open. Wider than pauseFutureSkew on purpose: the
// live skew judges a writer against its own clock five minutes at a time, while
// this one judges rows at rest that may have been written under a different
// clock than the one now reading them - and deletion, unlike an import refusal,
// cannot be re-run after the clock syncs. Two days covers every civil-time
// misconfiguration (UTC-offset mistakes reach 26 hours) and any plausible NTP
// step-back, and is still four orders of magnitude short of the decade-reaching
// spans this repair exists to remove.
const pauseRepairFutureSkew = 48 * 3600

// repairInsaneEventDurations strips outage lengths today's import refuses but a
// pre-guard build already persisted - the events-table twin of
// repairInsanePauses. eventRowSane bounds duration_s at the import door only;
// a row that entered before the bound existed answers to nobody:
// completedOutagesSince anchors an unpaired 'up' at ts-duration_s with no bound,
// so one on-disk row claiming 1e15 seconds is an outage reaching back thirty
// million years that every queried window lands inside, and re-importing a
// corrected backup merges by (ts, type) and changes nothing.
//
// The repair is eventRowSane's, applied at rest: strip the impossible length
// (to NULL, what InsertEvent stores for "no measurement"), never the row - its
// primary content is the transition, and deleting it would leave the preceding
// 'down' dangling, which every reader treats as an outage still running. Same
// predicate as the import door, no type filter: only 'up' rows carry a length
// today, but the rule is about what a duration can MEAN, and holding both doors
// to one rule is the point. Clock-free by nature (a bound, not a now-anchored
// judgement), so it runs unconditionally; idempotent, and the events table is
// small (two rows per outage).
func repairInsaneEventDurations(db *sql.DB) error {
	// The CAST clause is the typing half of the import door (intCols + normVal)
	// applied at rest, exactly as in the pause repair: an in-range fractional
	// REAL passes both bounds yet errors every int64 scan of the column
	// (completedOutagesSince, behind uptime and the heatmap), 500ing the reads
	// permanently. It is normalized to NULL like an out-of-bounds length - the
	// transition is the row's primary content and stays.
	res, err := db.Exec(`UPDATE events SET duration_s = NULL WHERE duration_s < 0 OR duration_s > ?
		OR CAST(duration_s AS INTEGER) != duration_s`,
		maxPauseDuration)
	if err != nil {
		recordDBErr(err)
		return fmt.Errorf("repair event durations: %w", err)
	}
	if n, e := res.RowsAffected(); e == nil && n > 0 {
		stats.Add("db.event_durations_repaired", n)
		log.Printf("pingularity: stripped the recorded length from %d outage event(s) an older build imported without validation (negative, not whole integer seconds, or longer than any retainable history); the transitions are kept, the impossible lengths were rewriting every uptime window they touched", n)
	}
	return nil
}

// reportUnreadableEventTypes counts rows whose `type` this build cannot
// interpret - older versions accepted any string, and today's import door
// refuses one, but a door is not a migration and nothing else revisits such a
// row (applySchema migrates columns, not data, and re-importing a corrected
// backup merges by (ts, type), so the poisoned key is never touched).
//
// It only COUNTS. This pass used to delete, which read as harmless tidy-up while
// no third type existed - and the reasoning against it was already written here:
// a build that deletes every type it does not recognise destroys a NEWER
// build's events on a downgrade, which is precisely the data loss a repair pass
// must not cause. The first release to add an event type would have made every
// downgrade silently drop those rows at startup, with this log line calling it a
// repair.
//
// Deleting bought nothing, because the fix was made in the QUERIES: every read
// of this table selects only the two types it understands, so an uninterpretable
// row is inert whether or not it is still on disk. Deliberately not enumerated here - the list went stale the
// first time it was extended, and an enumeration in a doc comment is exactly the
// kind of audit trail that rots quietly. TestEventTypeFilterCoversEveryEventRead
// derives the real list from the source on every run, and fails when a reader
// forgets the filter or an exemption names a function that no longer exists.
//
// What is left is worth saying out loud - a database holding
// rows this build cannot read is usually a downgrade, and the operator should
// know the rows are there and are being ignored, not silently removed.
func reportUnreadableEventTypes(db *sql.DB) error {
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE type NOT IN ('down', 'up')`).Scan(&n); err != nil {
		recordDBErr(err)
		return fmt.Errorf("count unreadable event types: %w", err)
	}
	if n > 0 {
		stats.Add("db.event_types_unreadable", n)
		log.Printf("pingularity: %d outage event(s) carry a type this build does not recognise (only 'down' and 'up' mean anything here); they are ignored by every uptime read and left untouched on disk - if this database was last used by a newer version, its events are still there for it", n)
	}
	return nil
}

// repairUnreadableIntColumns is the intCols allowlist applied at rest: a value in
// an INTEGER-affinity column that no read can convert to a Go int64. The import
// door has rejected such a row since the allowlist landed, but the door came with
// no migration, so residue an older build persisted stays forever - SQLite does
// not enforce column types, so `healthy` holding 'yes' is stored happily and then
// fails every scan of the column ("converting driver.Value type string ... to a
// int64"). That is not one bad row degrading gracefully: LatestSpeed is the speed
// panel and LatestPerTarget is the status table, and one row breaks each of them
// permanently. Re-importing a corrected backup does not heal it either - the
// merge is by key and only rewrites the columns the file carries.
//
// Which action per column follows repairInsaneEventDurations' philosophy: strip
// the unreadable FIELD where the column is NULLable, because the row's primary
// content is the run or the transition and NULL is a shape the readers already
// handle; delete only where the integer column is NOT NULL, because there the
// unreadable value IS what the row says (a probe result is its success flag) and
// no honest row is left to keep. Every aggregate would otherwise read such a flag
// as a failure - 'yes' = 1 is false in SQL - so leaving it mints downtime out of
// a successful probe.
//
// Only non-ts columns are listed: an imported ts has been range-checked as a
// number for every keyed table since long before the type allowlist, so there is
// no door residue could have come through, and a ts is also the one column whose
// removal is not recoverable by re-import. There is nothing to normalise, either:
// SQLite's INTEGER affinity converts numeric-looking TEXT on the way in, so
// anything still unreadable at rest is not a number in any lossless sense.
//
// Runs unconditionally at Open, like the sibling repairs: clock-free, idempotent,
// and one full scan of samples costs ~0.2s per 2.6M rows - paid once per start,
// against reads that are otherwise broken for the whole retention window.
func repairUnreadableIntColumns(db *sql.DB) error {
	// typeof() is the test, not CAST: the pause/event repairs compare CAST against
	// the value because they are judging NUMBERS (a fractional REAL), while here the
	// question is only whether the storage class is one a scan can read. 'null'
	// counts as readable - it is the honest shape of an unmeasured field.
	var stripped, deleted int64
	for _, r := range []struct {
		dst  *int64
		stmt string
	}{
		// One UPDATE rather than one per column, so a run with two unreadable fields
		// counts as the one repaired row it is. A CASE with no ELSE yields NULL, which
		// is exactly the strip.
		{&stripped, `UPDATE speed SET
			healthy        = CASE WHEN typeof(healthy)        IN ('integer','null') THEN healthy        END,
			download_bytes = CASE WHEN typeof(download_bytes) IN ('integer','null') THEN download_bytes END,
			upload_bytes   = CASE WHEN typeof(upload_bytes)   IN ('integer','null') THEN upload_bytes   END
			WHERE typeof(healthy)        NOT IN ('integer','null')
			   OR typeof(download_bytes) NOT IN ('integer','null')
			   OR typeof(upload_bytes)   NOT IN ('integer','null')`},
		{&deleted, `DELETE FROM samples WHERE typeof(success) != 'integer'`},
		{&deleted, `DELETE FROM dns WHERE typeof(success) != 'integer'`},
	} {
		res, err := db.Exec(r.stmt)
		if err != nil {
			recordDBErr(err)
			return fmt.Errorf("repair integer columns: %w", err)
		}
		if n, e := res.RowsAffected(); e == nil && n > 0 {
			*r.dst += n
		}
	}
	if n := stripped + deleted; n > 0 {
		stats.Add("db.int_columns_repaired", n)
		log.Printf("pingularity: repaired %d row(s) holding a value no read can convert in a whole-number column, stored before the import door checked types: stripped the unreadable field from %d speedtest run(s) and removed %d probe row(s) whose success flag is the whole row's meaning", n, stripped, deleted)
	}
	return nil
}

// intColumnRepairGen is the generation of the at-rest int-column repair
// (repairUnreadableIntColumns) that has run against a database. A fresh or pre-gate
// DB carries generation 0; Open runs the scan only when the stored generation is
// below this, then stamps it, so the full typeof() scan of samples/dns/speed is paid
// once per DB rather than on every start. Bump this when the repair's coverage
// changes so an older DB re-scans exactly once.
//
// ACCEPTED LIMITATION: stamping means a raw-SQL write that plants unreadable residue
// AFTER the generation is recorded goes unrepaired. This is sound because the import
// door (the intCols allowlist + normVal) has rejected that poison since it landed, so
// the only doors left open are direct raw-SQL writes - which are out of scope for a
// migration whose whole job was to clean up what a pre-allowlist build let through.
// The forthcoming storage/clock redesign will tie the generation to a door guarantee
// rather than to "the scan ran once".
const intColumnRepairGen = 1

// repairGeneration reads the stored int-column repair generation. It lives in PRAGMA
// user_version - a per-database header field, so every pooled connection sees the same
// value and a write on any connection persists to the file - chosen precisely because
// it is NOT a table: export/import only touch the exportTables whitelist, so the marker
// is never carried into a backup. That matters: were it exportable, restoring a backup
// from an already-stamped machine onto a DB with its OWN unscanned residue would skip
// the repair the destination still needs. Keeping it per-DB means each database's own
// first Open governs its own scan.
func repairGeneration(db *sql.DB) (int64, error) {
	var gen int64
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&gen); err != nil {
		recordDBErr(err)
		return 0, fmt.Errorf("read repair generation: %w", err)
	}
	return gen, nil
}

// stampRepairGeneration records that the int-column repair through generation gen has
// run. PRAGMA does not accept a bound parameter; gen is a compile-time constant, so the
// formatted literal cannot inject.
func stampRepairGeneration(db *sql.DB, gen int64) error {
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, gen)); err != nil {
		recordDBErr(err)
		return fmt.Errorf("stamp repair generation: %w", err)
	}
	return nil
}

// forgetRestoredSplitMarker deletes the settings layer's split marker
// (engine_split_done, see settings.recordMigrations) from a file no build that
// records the split has opened, and it runs before applySchema because the
// schema is what tells. server_health reached the base schema ahead of the
// split, in the same release, and every build that writes the marker creates
// that table here at Open, before its settings load writes a row. So a settings
// table holding the marker with no server_health beside it was handed the
// marker by something other than that load - and every release before 0.100 is
// such a thing: none of them leaves the key out of an export or refuses it on
// restore, so a backup one takes of a database this release has started carries
// the marker onto a machine that never ran this release. That machine goes on
// running iperf3 on Ookla's direction and retries wherever iperf3 has no row of
// its own, as those releases always did. Kept, the marker told the split's
// first load here that the carry had already been made, and iperf3 dropped to
// its defaults at the upgrade with nothing said: upload-only with three retries
// became both directions with one, roughly twice the transfer per run. Deleted,
// it leaves that load to carry the pair as on any upgrade, say so, and write
// the marker back itself. The row goes before the table is created, so an Open
// cut short between the two leaves the next Open the same answer.
//
// The query reads only the schema, which applySchema reads first, and the
// delete is a write like the ones applySchema makes next, so what fails either
// of them - a file that is not a database, one locked or read-only - fails
// applySchema too, which used to meet it first. It is worded as applySchema
// words its own and counted as it is: not here, but once, by Open's recovery
// when the file is corrupt.
//
// Damage only the delete reads is the exception. The delete finds its row
// through the settings table's key index, and nothing else at Open reads that
// page: applySchema never touches the table, and the settings load reads it
// whole. So a file whose only damage is that index opened on the releases
// before this step, which ran on the settings it held. Failing Open here would
// set such a file aside, or refuse to start on it, at its first start after
// the upgrade - the history and every saved setting out of reach over one
// damaged page nothing needed. A delete that meets corruption is left undone
// instead: the marker stays, as it did before this step, and a machine that
// also restored one drops iperf3 to its defaults at the upgrade, as it did
// then.
//
// This leans on server_health staying in the base schema: a build that retires
// that table owes this check another one that arrived with it.
func forgetRestoredSplitMarker(db *sql.DB) error {
	var haveSettings, haveHealth int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'settings'),
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'server_health')`).Scan(&haveSettings, &haveHealth); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	if haveSettings == 0 || haveHealth != 0 {
		return nil // a new file, or one a build that records the split has opened
	}
	if _, err := db.Exec(`DELETE FROM settings WHERE key = 'engine_split_done'`); err != nil {
		if dbCorrupt(err) {
			return nil // damage only the delete reads: see above
		}
		return fmt.Errorf("migrate: %w", err)
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

// firstRunInDoomedDB asks a database that is about to be set aside whether the
// install it belonged to was still inside its first-run offer - the window in
// which nothing has been measured and nobody has answered Quick Setup. Only a
// positive answer counts: the offer clock is on disk, and neither the answered
// marker nor the install anchor is. Everything else - an install that answered,
// one old enough to carry an anchor, a settings table too torn to read, a file
// SQLite cannot make sense of at all - reads as false, because the rebuild's
// question is "may I keep monitoring", and only proof that consent was never
// given may stop it.
//
// One read of the smallest table in the file, on the handle that just failed:
// the damage sits somewhere, but rarely everywhere, and asking the file beats
// assuming for it.
func firstRunInDoomedDB(db *sql.DB) bool {
	var offered, settled int
	if err := db.QueryRow(`SELECT
		EXISTS(SELECT 1 FROM settings WHERE key = 'quick_setup_offer_since'),
		EXISTS(SELECT 1 FROM settings WHERE key IN ('quick_setup_done', ?))`,
		firstSeenKey).Scan(&offered, &settled); err != nil {
		return false
	}
	return offered != 0 && settled == 0
}

// resolveDBPath answers the question a -db path actually poses: which file does
// it lead to. A link there is an ordinary arrangement - the database moved to
// the bigger disk and a link stayed where the unit file points - and refusing
// it stops installs that had been running for years. What must not happen is
// what used to: the securing chmod followed the link while the set-aside
// renamed the link itself, so a repair re-permissioned one file and renamed
// another, left the damaged database sitting under its own name, and put a
// brand-new empty store where the link had been. Resolving once, here, is what
// keeps every step below talking about the same file - and it also means a link
// swapped after this point cannot redirect any of them, because nothing opens
// the link's name again.
//
// A link that leads nowhere, or round in a circle, is refused instead. It names
// no file to resolve to, and the alternative is worse than an error: the open
// below would create a database at the far end of it, which on the usual cause
// - the volume the link points into is not mounted yet - means monitoring
// starting over into an empty file that the mount then hides.
//
// One link to nothing is not that: the one a rebuild leaves when its start dies
// between setting the damaged database aside and putting the store it finished
// in that file's place (see finishInterruptedRebuild). The link - or the last
// of a chain of them - names a file the rebuild has just moved, and the
// finished store sits beside where it was.
// Refused, that store stayed unused and the daemon stayed down on a message
// about a link, for want of the one rename the next Open exists to make. So
// the far end, through as many as maxLinkHops links, is handed on when a
// finished store is beside it, and only then - nothing sits beside a file on a
// volume that is not mounted.
func resolveDBPath(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		// Nothing there yet (a first run creates the file) or not a link:
		// the path is already the file's own name. A path that cannot be
		// Lstat'ed at all is left to checkDBPath, which reports why.
		return path, nil
	}
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		if far, ok := farEndOfLinks(path); ok {
			if bfi, berr := os.Lstat(far + rebuiltSuffix); berr == nil && bfi.Mode().IsRegular() {
				return far, nil
			}
		}
		return "", fmt.Errorf("%s is a symlink that names no file: %w", path, err)
	}
	return target, nil
}

// maxLinkHops bounds farEndOfLinks the way the kernel bounds a path lookup
// (Linux gives up after 40 links, macOS after 32): a chain longer than that is a
// loop, or near enough to be treated as one.
const maxLinkHops = 40

// farEndOfLinks follows the chain of links that starts at path - each relative
// hop read from the directory its link really sits in, as the kernel reads it -
// to the first name that is not itself a link, and reports whether it got there.
// That name need not exist: a chain that ends nowhere is what it is asked about.
func farEndOfLinks(path string) (string, bool) {
	cur := path
	for hop := 0; hop < maxLinkHops; hop++ {
		next, err := os.Readlink(cur)
		if err != nil {
			return "", false
		}
		if !filepath.IsAbs(next) {
			// A directory on the way to the link can itself be a link, and a ".." in
			// the target climbs out of the directory the kernel reached, not out of
			// the one the name spells.
			dir := filepath.Dir(cur)
			if real, err := filepath.EvalSymlinks(dir); err == nil {
				dir = real
			}
			next = filepath.Join(dir, next)
		}
		if fi, err := os.Lstat(next); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return next, true
		}
		cur = next
	}
	return "", false
}

// checkDBPath is what Open establishes about the -db path and its WAL/SHM
// sidecars before anything touches them: each is absent or a regular file and
// not a directory. The path itself has been resolved through any link by then
// (see resolveDBPath), so what is judged here is the real file the -db flag
// leads to - a directory typed for the file inside it, a device node, a fifo -
// and it is refused with an error that names it, never re-permissioned,
// renamed or replaced.
//
// It deliberately does NOT judge the file's CONTENTS. A regular file that is
// not a database reaches SQLite, fails to open, and is set aside by the
// recovery below - which is what that recovery is for, because the shapes are
// indistinguishable: a torn database whose header page was lost to a bad
// sector reads exactly like a file that was never a database at all, and
// refusing those bytes would leave the daemon crash-looping on precisely the
// fault the set-aside exists to end. The price is an unrelated file at a
// mistyped -db being renamed to .corrupt; nothing is deleted, and OpenExisting
// (what the hand-typed commands use) refuses rather than rename.
//
// mustExist is OpenExisting's contract: nothing at the path, or an empty file,
// is refused as well.
func checkDBPath(path string, mustExist bool) error {
	for _, sfx := range []string{"", "-wal", "-shm"} {
		p := path + sfx
		fi, err := os.Lstat(p)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				if sfx == "" && mustExist {
					return fmt.Errorf("no database at %s", path)
				}
				continue
			}
			return fmt.Errorf("check db path %s: %w", p, err)
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			// A sidecar, almost always: the -db path itself arrives here
			// already resolved, and only a link planted between the resolve
			// and this Lstat could still be one. Neither can be followed -
			// SQLite opens the write-ahead log and the shared-memory file
			// beside the database directly and gets no further than "unable
			// to open database file" through a link - so the remedy named is
			// the one that exists for all three, and never "pass -db the file
			// it points to", which is not a thing to do with a WAL.
			return fmt.Errorf("%s is a symlink; the database and its -wal/-shm sidecars are opened directly, never through a link - put the file it names in its place", p)
		case fi.IsDir():
			return fmt.Errorf("%s is a directory, not a database file; pass -db the file inside it", p)
		case !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", p)
		}
		if sfx == "" && fi.Size() == 0 && mustExist {
			return fmt.Errorf("%s is empty, not a database", path)
		}
	}
	return nil
}

// shellQuotePath wraps a path for the recovery commands the refusal above
// prints, quoted for the shell an operator on goos pastes them into - the
// refusal runs on the machine that holds the database, so the caller passes
// runtime.GOOS. Those commands are read with the daemon down and pasted
// straight into a shell, and on macOS the default database directory is
// "Application Support": pasted bare, the space splits the command and sqlite3
// reports a SQL syntax error about the second half rather than opening
// anything, so the one instruction an operator has fails and recovers nothing.
// (The Windows default, under %ProgramData%, has no space in it; a -db pointed
// somewhere else can.)
//
// A POSIX shell gets single quotes, because they are also the only quoting that
// survives a $ or a backtick in a directory name - inside double quotes the
// shell would expand those, and a recovery command that quietly names a
// different file is worse than one that fails.
//
// Windows gets its own. cmd.exe does not treat a single quote as quoting at
// all, so the POSIX form handed sqlite3 a file name that began with one - and on
// every Windows path, since a backslash is outside the POSIX safe set. Double
// quotes are what cmd.exe and PowerShell both strip, a Windows file name cannot
// contain one, and a backslash is an ordinary character to both shells, so the
// default path stays bare and a spaced one comes through either shell whole.
// What double quotes do not stop is PowerShell expanding a $ or a backtick
// inside them, or cmd.exe a %NAME%. A path carrying one of those gets
// PowerShell's single quotes instead (a quote inside doubled): literal in the
// shell this project's Windows instructions use, and in cmd.exe a name no file
// has, so the command fails there rather than opening a different one.
//
// A path that needs none is left bare so the common message reads as prose
// rather than as a script.
func shellQuotePath(goos, p string) string {
	safeChars := "_-./:@+=,"
	if goos == "windows" {
		if strings.ContainsAny(p, "$`%") {
			return "'" + strings.ReplaceAll(p, "'", "''") + "'"
		}
		safeChars = `\/:._-`
	}
	safe := p != ""
	for _, r := range p {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			strings.ContainsRune(safeChars, r) {
			continue
		}
		safe = false
		break
	}
	if safe {
		return p
	}
	if goos == "windows" {
		return `"` + p + `"`
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
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

// RebuiltAfterCorruption reports that this store is the empty one Open built
// after setting a damaged database aside - so everything the install had is in
// the .corrupt file beside it, not in here. The daemon is monitoring again,
// which is what the recovery is for, but it is not the install anyone
// configured: the callers are the ones that must not carry on as though
// nothing happened (readiness, for the life of this handle; the access posture
// the lost password was standing behind outlives it - see
// AccessHoldAfterRebuild).
func (s *Store) RebuiltAfterCorruption() bool { return s.rebuilt }

// accessHoldKey is the settings row a store rebuilt after corruption carries
// until a login stands behind it again; its value is when the rebuild happened.
const accessHoldKey = "access_hold_after_rebuild"

// AccessHoldAfterRebuild reports whether network access must stay held to this
// machine because this is a store Open rebuilt after finding the database
// damaged, and no login has been set on it since. The hold lives in the rebuilt
// store rather than on the handle because the handle is gone at the next start
// and the store is not: that start opens a healthy, empty database, and without
// the row an explicit -access network would put it on the network with no
// password behind it - the outcome the hold exists to prevent, one restart
// later.
//
// The first call that finds a login releases the hold, so a login switched off
// later, on purpose, is the operator's decision and not the rebuild's
// consequence. A store whose settings cannot be read answers held: this decides
// who can reach the dashboard, and it fails closed.
func (s *Store) AccessHoldAfterRebuild(ctx context.Context) (bool, error) {
	all, err := s.AllSettings(ctx)
	if err != nil {
		return true, err
	}
	if all[accessHoldKey] == "" {
		return false, nil
	}
	if all["auth_hash"] == "" || (all["auth_enabled"] != "1" && all["auth_enabled"] != "true") {
		return true, nil
	}
	// Said, because it need not be anyone's doing at this machine: an older build
	// the store was rolled back to does not know the hold, and a login set there -
	// by whoever reached its open dashboard first - ends it just the same. This
	// line is the only record of that.
	released, err := s.ReleaseAccessHold(ctx)
	if released {
		log.Printf("pingularity: the hold a rebuilt store kept on network access is released: a login is set on it")
	}
	return false, err
}

// ReleaseAccessHold ends the hold AccessHoldAfterRebuild reports, and says
// whether there was one to end. `pingularity reset-auth` calls it: that command
// is how an operator says they know the store has no login, and it is the way
// back for an install that cannot reach its own loopback address to set one - a
// bridged container with no shell, whose published port the hold refuses.
func (s *Store) ReleaseAccessHold(ctx context.Context) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, accessHoldKey)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Close writes the probe readings that are still waiting and releases the
// underlying database handle. The save comes first, before anything else a
// close may come to do: it needs the handle, and the rows are lost without it.
func (s *Store) Close() error {
	s.finalSave()
	return s.db.Close()
}

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
//
// On a store with a save interval the round waits in memory instead, and is
// written with the rounds around it (see SaveBuffered). The error then says
// only whether the round was taken. A save that fails later is logged and
// counted by the store.
func (s *Store) InsertSamples(ctx context.Context, sms []Sample) error {
	// A probe round means the process is alive and reading its clock: the moment
	// that clock turns plausible, run the future-end pause repair Open had to
	// skip (an atomic no-op on every store that was judged at Open).
	s.maybeRepairFuturePauses()
	if len(sms) == 0 {
		return nil
	}
	if held, err := s.holdRound(ctx, sms); held {
		return err
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
// It waits in memory like a probe round on a store with a save interval.
func (s *Store) InsertDNS(ctx context.Context, ts time.Time, ms float64, ok bool) error {
	if held, err := s.holdDNS(ctx, ts, ms, ok); held {
		return err
	}
	var lat any
	if ok {
		lat = ms
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO dns (ts, latency_ms, success) VALUES (?, ?, ?)`,
		ts.Unix(), lat, util.B2I(ok))
	recordDBErr(err)
	return err
}

// Batched saves.
//
// Probe rounds and DNS readings wait in memory and are written together, one
// transaction for every save. A round written alone costs about 4.7 pages of
// write-ahead log, which at the default cadence is 333 MB of writes a day.
// Six rounds to a save bring that to 85 MB. On an SD card or eMMC that is the
// difference that matters.
//
// SQL cannot see a waiting row, so whatever reads the two tables, or has to
// come after the rows in them, saves first: every store function that reads
// samples or dns, every writer of an event, a pause or a speed row, a
// cleanup, a clear, an import and Close. Two more things start a save: the
// oldest waiting row reaching the chosen age, and a full buffer. What is
// still waiting when the process is killed or the power goes is lost. That
// is the whole cost, and outage records are never part of it: an event is
// written at once and takes the waiting rows with it.
//
// A store that was given no interval (SetSaveEveryFn) writes every round at
// once, as it always did. That is every test that opens its own store, and
// reset-auth.

// MaxSaveEvery is the longest a reading may be left waiting, whatever the
// setting says. It is the most history a crash or a power cut can cost. It is
// also as long as a restart after one may take before the monitor books the
// stretch it cannot account for as unobserved (startupGapMin in
// internal/monitor).
const MaxSaveEvery = 120 * time.Second

// FinalSaveBudget is what a save at shutdown may take: one whole wait for the
// writer (busy_timeout in pragmaConn) and a second to commit. It must stay
// above that wait, and so must saveTimeout. SQLite does not look at the
// context while it waits for the writer: under a deadline of 300 ms a write
// beside a held writer still took 5.06 s to come back, and came back as
// "context deadline exceeded", which db.busy does not count.
const FinalSaveBudget = 6 * time.Second

const (
	// maxHeldRows is how many rows may wait. A round that would go over it
	// starts a save, and only if that save fails are the oldest rounds
	// dropped. A save that failed within the last saveRetryEvery counts as
	// that failure, and the round starts none of its own: a store that
	// cannot be written would be handed every waiting row again by every
	// round, and the retry is the timer's. It covers the longest wait at the
	// fastest cadence: 120 s of rounds a second apart, with 64 targets and a
	// DNS reading each, is 7,800 rows. 64 is where /metrics stops listing
	// targets. Full, the buffer holds about 0.6 MB.
	maxHeldRows = 8192
	// saveRetryEvery is how long after a failed save the next one is tried.
	// It is the default probe interval: a store that could not be written
	// was tried again once a round before, too.
	saveRetryEvery = 5 * time.Second
	// saveTimeout bounds one save. It is longer than busy_timeout, so that
	// SQLite gives up first and the error names what was in the way.
	saveTimeout = 10 * time.Second
	// readerSaveWait is as long as a save made for a reader waits: for a
	// save that is already running, for a free connection, and for the
	// writer. A request must never stall behind a long write because of a
	// save. Measured before this bound, a read beside a held writer waited
	// nothing at all and a write waited the whole 5 s.
	readerSaveWait = 100 * time.Millisecond
	// readerSaveQuiet is how long saves for readers stay away after one of
	// them found the writer taken. One request reads through many store
	// functions, and each of them would wait readerSaveWait again.
	readerSaveQuiet = time.Second
)

// SaveReason says what started a save. It is the last part of the counter a
// save is booked under, db.sample_saves.<reason>.
type SaveReason string

const (
	SaveAge     SaveReason = "age"     // the oldest waiting row reached the save interval, or a failed save is tried again
	SaveRequest SaveReason = "request" // an HTTP request that may read the rows is about to be served
	SaveRead    SaveReason = "read"    // a store function is about to read samples or dns
	SaveOrder   SaveReason = "order"   // a row that has to come after the waiting ones is about to be written, or rows are about to be deleted or merged
	SaveCap     SaveReason = "cap"     // the buffer is full
	SaveStop    SaveReason = "stop"    // the monitor has stopped, or the store is closing
)

// SaveReasons lists every reason a save can have, so the counters can be
// seeded from the same list they are booked from.
func SaveReasons() []SaveReason {
	return []SaveReason{SaveAge, SaveRequest, SaveRead, SaveOrder, SaveCap, SaveStop}
}

// forReader reports whether a save is made on behalf of a reader. Such a save
// gives up after readerSaveWait. Every other save keeps the ordinary wait of
// busy_timeout: nothing is waiting for an answer behind it.
func (r SaveReason) forReader() bool { return r == SaveRead || r == SaveRequest }

// errSaveWaiting is what a save made for a reader returns when it left the
// rows waiting: the writer was taken, or a save failed moments ago. The read
// goes on with what is saved, and the rows are written by the next save that
// is not made for a reader.
var errSaveWaiting = errors.New("probe readings are waiting to be saved")

// heldRound is one probe round waiting to be saved.
type heldRound struct {
	// at is when the store was handed the round. The save interval counts
	// from here, on the monotonic clock: the rows' own timestamps are wall
	// time, which can step, and a test's rows can carry any time at all.
	at   time.Time
	rows []Sample
}

// heldDNS is one DNS reading waiting to be saved.
type heldDNS struct {
	at time.Time
	ts time.Time
	ms float64
	ok bool
}

// txBeginner is satisfied by *sql.DB and by *sql.Conn: a save runs on the
// pool, or on one connection it took out for itself.
type txBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// sampleBuf holds the rows that wait. Rounds are held and dropped whole: a
// round was all or nothing when it was one transaction, and recovery dating
// counts a majority of each round's targets, so part of a round could read as
// a quorum the round never had.
type sampleBuf struct {
	everyFn atomic.Pointer[func() time.Duration]
	logFn   atomic.Pointer[func(level slog.Level, msg string, args ...any)]
	// waiting counts the rows held plus the rows inside a save that is
	// running. Zero means a reader has nothing to wait for, and reading it
	// is all a read pays then.
	waiting atomic.Int64

	// gate lets one save run at a time, and is held across its transaction.
	// A channel and not a mutex, because a save made for a reader has to be
	// able to stop waiting for it. Order: gate, then mu.
	gate chan struct{}

	mu     sync.Mutex
	rounds []heldRound
	dns    []heldDNS
	rows   int
	timer  *time.Timer
	due    time.Time // what the timer is set for. Zero: not set
	// quietUntil keeps saves made for readers away: for saveRetryEvery after
	// a save failed, so readers do not hammer a store that cannot be
	// written, and for readerSaveQuiet after one of them found the writer
	// taken.
	quietUntil time.Time
	failing    bool // the last save failed. Logged once for the whole streak
	// dropSaid: the log has said that rows are being dropped. While the
	// buffer is full every round drops one, and one line says so for all of
	// them until a save goes through. The counter counts every row.
	dropSaid bool
	// missed: a reader went on without the waiting rows. What it read may
	// be memoized, so the save that lands them drops the read caches.
	missed bool
	closed bool
	// writtenOff: Close waited for a save that was still running for as
	// long as a close may take, and booked the rows that save holds as lost
	// (finalSave). When the save comes back it puts nothing into the buffer
	// and counts nothing a second time.
	writtenOff bool

	// Test seams. They are fields and not package variables, because a
	// timer goroutine reads them, and a test's cleanup would put a package
	// variable back while another store's timer is still running. Set before
	// the first insert and read under mu.
	write func(ctx context.Context, q txBeginner, rounds []heldRound, dns []heldDNS, brief bool) error
	after func(d time.Duration, f func()) *time.Timer
	now   func() time.Time
}

// init makes a buffer ready. The one place a Store is built calls it.
func (b *sampleBuf) init() {
	b.gate = make(chan struct{}, 1)
	b.write = writeHeld
	b.after = time.AfterFunc
	b.now = time.Now
}

// SetSaveEveryFn gives the store the save interval, asked for again on every
// insert so a changed setting applies to the next round. The function must
// not call back into the store. With none set, or with nil, every round is
// written at once.
func (s *Store) SetSaveEveryFn(fn func() time.Duration) {
	if fn == nil {
		s.held.everyFn.Store(nil)
		return
	}
	s.held.everyFn.Store(&fn)
}

// SetSaveLogFn gives the store the daemon's logger for what happens to
// waiting rows. A round that waits is a round the monitor was told is stored,
// so a failed save can only be reported from here, and it has to reach the
// log the dashboard shows. With none set the lines go to the standard logger.
func (s *Store) SetSaveLogFn(fn func(level slog.Level, msg string, args ...any)) {
	if fn == nil {
		s.held.logFn.Store(nil)
		return
	}
	s.held.logFn.Store(&fn)
}

func (s *Store) saveLog(level slog.Level, msg string, args ...any) {
	if fn := s.held.logFn.Load(); fn != nil {
		(*fn)(level, msg, args...)
		return
	}
	var kv strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&kv, " %v=%v", args[i], args[i+1])
	}
	log.Printf("pingularity: %s%s", msg, kv.String())
}

// saveEvery is the save interval in force: 0 when every round is to be
// written at once, and never more than MaxSaveEvery.
func (s *Store) saveEvery() time.Duration {
	fn := s.held.everyFn.Load()
	if fn == nil {
		return 0
	}
	d := (*fn)()
	if d <= 0 {
		return 0
	}
	return min(d, MaxSaveEvery)
}

// BufferedRows is how many rows are waiting to be saved, those inside a
// running save included.
func (s *Store) BufferedRows() int { return int(s.held.waiting.Load()) }

// holdRound keeps a probe round for the next save. held false means the
// caller writes it itself, now.
func (s *Store) holdRound(ctx context.Context, sms []Sample) (held bool, err error) {
	// Asked before any lock of the buffer is taken: the function is the
	// settings controller's, which has a lock of its own.
	every := s.saveEvery()
	if every == 0 && s.held.waiting.Load() == 0 {
		return false, nil
	}
	if len(sms) > maxHeldRows {
		// More than the buffer holds. What waits goes first, to keep the order.
		s.SaveBuffered(SaveCap)
		return false, nil
	}
	// A round cancelled before it was written opened no transaction and was
	// not counted as a database error. It is not held either.
	if err := ctx.Err(); err != nil {
		return true, err
	}
	r := heldRound{rows: append([]Sample(nil), sms...)}
	return s.hold(len(sms), every, func(b *sampleBuf, at time.Time) {
		r.at = at
		b.rounds = append(b.rounds, r)
	})
}

// holdDNS is holdRound for one DNS reading.
func (s *Store) holdDNS(ctx context.Context, ts time.Time, ms float64, ok bool) (held bool, err error) {
	every := s.saveEvery()
	if every == 0 && s.held.waiting.Load() == 0 {
		return false, nil
	}
	// InsertDNS counts every error, a cancelled context's too.
	if err := ctx.Err(); err != nil {
		recordDBErr(err)
		return true, err
	}
	return s.hold(1, every, func(b *sampleBuf, at time.Time) {
		b.dns = append(b.dns, heldDNS{at: at, ts: ts, ms: ms, ok: ok})
	})
}

// hold puts n rows into the buffer through put and sets the timer for them.
func (s *Store) hold(n int, every time.Duration, put func(b *sampleBuf, at time.Time)) (held bool, err error) {
	b := &s.held
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false, nil // the caller's own write fails on the closed store, as before
	}
	dropped, say := 0, false
	if b.rows+n > maxHeldRows {
		// A save that failed moments ago is this round's failed save too.
		// Trying again here handed a store that cannot be written all the
		// waiting rows with every round, and twice a round when the DNS
		// reading met a full buffer as well, on the goroutine that paces the
		// rounds. The price: if the store healed inside those few seconds,
		// this round still drops the oldest one to make room.
		if !b.failing || !b.now().Before(b.quietUntil) {
			b.mu.Unlock()
			s.SaveBuffered(SaveCap)
			b.mu.Lock()
			if b.closed {
				b.mu.Unlock()
				return false, nil
			}
		}
		dropped = b.dropOldest(maxHeldRows - n)
		say = b.sayDrop(dropped)
	}
	put(b, b.now())
	b.rows += n
	stats.Set("db.sample_rows_waiting", b.waiting.Add(int64(n)))
	// While saves fail the timer is the retry's, and a new round must not
	// pull it forward. With the interval just set to 0 the save below is
	// this round's.
	if !b.failing && every > 0 {
		b.arm(s, b.oldest().Add(every), false)
	}
	b.mu.Unlock()
	stats.Add("db.sample_rows_buffered", int64(n))
	s.noteDropped(dropped, say)
	if every == 0 {
		// The interval was set to 0 while rows were waiting. They go first,
		// and this round with them.
		return true, s.SaveBuffered(SaveAge)
	}
	return true, nil
}

// oldest is when the oldest waiting row was handed over. Called with mu held
// and rows waiting.
func (b *sampleBuf) oldest() time.Time {
	switch {
	case len(b.rounds) == 0:
		return b.dns[0].at
	case len(b.dns) == 0 || !b.dns[0].at.Before(b.rounds[0].at):
		return b.rounds[0].at
	}
	return b.dns[0].at
}

// arm sets the timer for at. An earlier deadline that is already set stands,
// unless replace says otherwise: a lowered setting applies to the next round,
// and a raised one lets the deadline that was set run out once. Called with
// mu held.
func (b *sampleBuf) arm(s *Store, at time.Time, replace bool) {
	if !replace && !b.due.IsZero() && !b.due.After(at) {
		return
	}
	b.disarm()
	b.due = at
	b.timer = b.after(max(at.Sub(b.now()), 0), func() { s.timedSave(at) })
}

// disarm stops the timer. Called with mu held.
func (b *sampleBuf) disarm() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.due = time.Time{}
}

// timedSave is what the timer runs. A timer that was replaced after it fired
// finds another deadline set and does nothing.
func (s *Store) timedSave(at time.Time) {
	b := &s.held
	// A panic on a timer's goroutine would take the daemon down, and its job
	// is to keep watching the link.
	defer func() {
		if r := recover(); r != nil {
			s.saveLog(slog.LevelError, "saving probe readings panicked; the save is retried", "panic", r)
			b.mu.Lock()
			if !b.closed && b.rows > 0 {
				b.arm(s, b.now().Add(saveRetryEvery), true)
			}
			b.mu.Unlock()
		}
	}()
	b.mu.Lock()
	mine := b.due.Equal(at)
	if mine {
		b.due, b.timer = time.Time{}, nil
	}
	b.mu.Unlock()
	if mine {
		s.SaveBuffered(SaveAge)
	}
}

// dropOldest drops the oldest waiting rounds and DNS readings, each of them
// whole, until no more than keep rows are left, and returns how many rows
// went. Called with mu held.
func (b *sampleBuf) dropOldest(keep int) (dropped int) {
	for b.rows > keep && b.rows > 0 {
		if len(b.rounds) == 0 || (len(b.dns) > 0 && b.dns[0].at.Before(b.rounds[0].at)) {
			b.dns = b.dns[1:]
			b.rows--
			dropped++
			continue
		}
		n := len(b.rounds[0].rows)
		b.rounds[0] = heldRound{}
		b.rounds = b.rounds[1:]
		b.rows -= n
		dropped += n
	}
	if dropped > 0 {
		// Dropped rows wait no longer. Left counted, no reader would ever
		// take the short way out again.
		stats.Set("db.sample_rows_waiting", b.waiting.Add(-int64(dropped)))
	}
	return dropped
}

// putBack returns the rows a save could not write to the front of the
// buffer, in the order they had. Rounds that arrived meanwhile stay behind
// them. Called with mu held.
func (b *sampleBuf) putBack(rounds []heldRound, dns []heldDNS, n int) (dropped int) {
	if b.writtenOff {
		return 0 // booked as lost by the close, and nothing reads a closed buffer
	}
	b.rounds = append(rounds, b.rounds...)
	b.dns = append(dns, b.dns...)
	b.rows += n
	return b.dropOldest(maxHeldRows)
}

// sayDrop reports whether the log is to say that rows were dropped: for the
// first of them since a save last went through. Called with mu held.
func (b *sampleBuf) sayDrop(dropped int) bool {
	if dropped == 0 || b.dropSaid {
		return false
	}
	b.dropSaid = true
	return true
}

func (s *Store) noteDropped(n int, say bool) {
	if n == 0 {
		return
	}
	stats.Add("db.sample_rows_dropped", int64(n))
	if say {
		s.saveLog(slog.LevelError, "dropping unsaved probe readings, oldest first: saves have failed for so long that no more can wait in memory", "rows", n)
	}
}

// What enter found.
const (
	gateTaken    = iota // the save may run, and must leave the gate when done
	gateKeptAway        // a save for a reader, while such saves are to stay away
	gateTimedOut        // the save in front ran for longer than this one waits
)

// enter takes the gate. A save made for a reader waits readerSaveWait for it
// and no longer, and does not wait at all while such saves are to stay away.
func (b *sampleBuf) enter(ctx context.Context, brief bool) int {
	if brief {
		b.mu.Lock()
		quiet := b.now().Before(b.quietUntil)
		if quiet {
			b.missed = true
		}
		b.mu.Unlock()
		if quiet {
			return gateKeptAway
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, readerSaveWait)
		defer cancel()
	}
	select {
	case b.gate <- struct{}{}:
		return gateTaken
	case <-ctx.Done():
		return gateTimedOut
	}
}

func (b *sampleBuf) leave() { <-b.gate }

// readerGaveUp books a save made for a reader that left the rows waiting.
// Called with mu held.
func (b *sampleBuf) readerGaveUp() {
	b.quietUntil = b.now().Add(readerSaveQuiet)
	b.missed = true
}

// SaveBuffered writes the waiting rows, in one transaction. It is safe on a
// nil store, and costs one atomic read when nothing waits.
//
// A save runs under its own context and never the caller's. The buffer is
// shared, and one client that hangs up must not cancel a save that others
// wait for.
//
// A save made for a reader (SaveRead, SaveRequest) that cannot start within
// readerSaveWait leaves the rows waiting and returns an error its caller
// ignores. The read then serves what is saved. Every other save waits for the
// writer as any write does.
//
// It must never be called with a transaction or a cursor of the caller's
// open. The in-memory pool has one connection, and a save would wait for the
// one its caller holds.
func (s *Store) SaveBuffered(reason SaveReason) error {
	if s == nil {
		return nil
	}
	b := &s.held
	if b.waiting.Load() == 0 {
		return nil
	}
	brief := reason.forReader()
	every := s.saveEvery() // before mu, as in holdRound
	budget := saveTimeout
	if reason == SaveStop {
		budget = FinalSaveBudget
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	switch b.enter(ctx, brief) {
	case gateKeptAway:
		return errSaveWaiting
	case gateTimedOut:
		b.mu.Lock()
		if brief {
			b.readerGaveUp()
		} else if !b.closed && b.rows > 0 && b.due.IsZero() {
			// The save in front ran for longer than a save may. The rows it
			// took are its own to settle. The ones here are tried again.
			b.arm(s, b.now().Add(saveRetryEvery), true)
		}
		b.mu.Unlock()
		if brief {
			stats.Inc("db.sample_save_deferred")
			return errSaveWaiting
		}
		return fmt.Errorf("save probe readings: the save in front did not finish: %w", ctx.Err())
	}
	defer b.leave()

	b.mu.Lock()
	if b.rows == 0 {
		b.mu.Unlock()
		return nil // the save in front took them
	}
	if brief && b.now().Before(b.quietUntil) {
		// The save in front failed while this one waited for it.
		b.missed = true
		b.mu.Unlock()
		return errSaveWaiting
	}
	rounds, dns, n := b.rounds, b.dns, b.rows
	b.rounds, b.dns, b.rows = nil, nil, 0
	write := b.write
	b.mu.Unlock()

	// s.db is read here, at the time of the save, and never kept: a test
	// swaps the handle (countingStore).
	err := s.runSave(ctx, write, rounds, dns, brief)

	b.mu.Lock()
	var dropped int
	var recovered, missed, first, say bool
	closed := b.closed
	// The close did not wait for this save any longer and booked its rows as
	// lost. They are counted as waiting no more, and putBack leaves them out.
	writtenOff := b.writtenOff
	switch {
	case err == nil:
		if !writtenOff {
			stats.Set("db.sample_rows_waiting", b.waiting.Add(-int64(n)))
		}
		b.quietUntil = time.Time{}
		recovered, b.failing = b.failing, false
		missed, b.missed = b.missed, false
		b.dropSaid = false
		b.disarm()
		if b.rows > 0 && !b.closed {
			b.arm(s, b.oldest().Add(every), true)
		}
	case brief && (IsBusy(err) || errors.Is(err, errNoConnection)):
		// The writer is taken, or every connection is. Nothing has failed.
		// The rows keep the deadline they had, and the save that meets it
		// waits for the writer as long as any write.
		dropped = b.putBack(rounds, dns, n)
		say = b.sayDrop(dropped)
		b.readerGaveUp()
		if !b.closed && b.due.IsZero() {
			b.arm(s, b.oldest().Add(every), false)
		}
		err = errSaveWaiting
	default:
		dropped = b.putBack(rounds, dns, n)
		say = b.sayDrop(dropped)
		b.quietUntil = b.now().Add(saveRetryEvery)
		first, b.failing = !b.failing, true
		b.missed = b.missed || brief
		if !b.closed {
			b.arm(s, b.quietUntil, true)
		}
	}
	b.mu.Unlock()

	// Counters and log lines come last, with no lock held.
	switch {
	case err == nil:
		stats.Inc("db.sample_saves." + string(reason))
		if writtenOff {
			// Closing the handle does not stop a transaction that is
			// running. The log has said these rows are lost, and they are
			// counted as dropped. The count stays, the log is put right.
			s.saveLog(slog.LevelInfo, "the probe readings given up at shutdown were saved after all", "rows", n)
		}
		if recovered {
			s.saveLog(slog.LevelInfo, "saved the probe readings that had waited through failed saves", "rows", n)
		}
		// A reader can only have missed rows while saves failed or while
		// the writer was taken. Whatever it memoized then is dropped now
		// that the rows are in.
		if recovered || missed {
			s.invalidateReadCaches()
		}
	case err == errSaveWaiting:
		stats.Inc("db.sample_save_deferred")
	default:
		stats.Inc("db.sample_save_failed")
		// Close says what was lost in its own words (finalSave).
		if first && !closed {
			s.saveLog(slog.LevelError, "could not save probe readings; they stay in memory and the save is retried", "rows", n, "err", err)
		}
	}
	s.noteDropped(dropped, say)
	return err
}

// errNoConnection is a save made for a reader finding every pooled
// connection taken for longer than it waits.
var errNoConnection = errors.New("no free database connection")

// runSave runs one save's transaction. A panic in it comes back as an error,
// so the rows return to the buffer like those of any failed save.
func (s *Store) runSave(ctx context.Context, write func(context.Context, txBeginner, []heldRound, []heldDNS, bool) error,
	rounds []heldRound, dns []heldDNS, brief bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("save probe readings: panic: %v", r)
		}
	}()
	if !brief {
		return write(ctx, s.db, rounds, dns, false)
	}
	// A save made for a reader runs on a connection of its own, taken out of
	// the pool for the one transaction, because the wait for the writer
	// belongs to the connection. A context cannot shorten it (see
	// FinalSaveBudget).
	wait, cancel := context.WithTimeout(ctx, readerSaveWait)
	conn, err := s.db.Conn(wait)
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return errNoConnection
		}
		return err
	}
	defer conn.Close()
	restore, err := lowerBusyTimeout(ctx, conn, readerSaveWait)
	if err != nil {
		return err
	}
	defer restore()
	return write(ctx, conn, rounds, dns, true)
}

// writeHeld writes rounds and DNS readings in one transaction, in the order
// they were handed over. It only writes. A transaction that reads first and
// writes after fails with SQLITE_BUSY_SNAPSHOT as soon as another connection
// commits in between, and busy_timeout does not cover that (see
// InsertSpeedTS).
//
// The two statements and the way a row's values are bound are InsertSamples'
// and InsertDNS's, and so is what counts as a database error: a transaction
// that would not open or prepare does not, a row or a commit that failed
// does. TestBatchedRowsMatchDirectRows holds the two paths together.
//
// brief is a save made for a reader. The writer being taken is how such a
// save is meant to end when a long write is running, so it is not counted as
// a database error.
func writeHeld(ctx context.Context, q txBeginner, rounds []heldRound, dns []heldDNS, brief bool) error {
	record := func(err error) {
		if !brief || !IsBusy(err) {
			recordDBErr(err)
		}
	}
	tx, err := q.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// On every way out, a panic's too. A transaction left open keeps its
	// connection, and a save made for a reader could not hand its own back.
	// After the commit this does nothing.
	defer tx.Rollback()
	if len(rounds) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rounds {
			for _, sm := range r.rows {
				var lat any
				if sm.Success {
					lat = sm.LatencyMS
				}
				var fam any
				if sm.Family != "" {
					fam = sm.Family
				}
				if _, err := stmt.ExecContext(ctx, sm.TS.Unix(), sm.Target, lat, util.B2I(sm.Success), fam); err != nil {
					record(err)
					return err
				}
			}
		}
	}
	for _, d := range dns {
		var lat any
		if d.ok {
			lat = d.ms
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO dns (ts, latency_ms, success) VALUES (?, ?, ?)`,
			d.ts.Unix(), lat, util.B2I(d.ok)); err != nil {
			record(err)
			return err
		}
	}
	err = tx.Commit()
	record(err)
	return err
}

// finalSave is Close's save. Nothing is held after it: what could not be
// written is dropped and said to be lost, because nothing will try again.
//
// That goes for the rows inside a save that is still running when the wait
// for it is over. The close does not wait longer: Stop gives a shutdown two
// saves of FinalSaveBudget each (stopWait in main.go) and then lets the
// process go. Left alone, those rows were lost without a line in the log or
// a count, and stayed counted as waiting.
func (s *Store) finalSave() {
	b := &s.held
	b.mu.Lock()
	b.closed = true
	b.disarm()
	b.mu.Unlock()
	err := s.SaveBuffered(SaveStop)
	b.mu.Lock()
	lost := b.dropOldest(0)
	if n := b.waiting.Load(); n > 0 {
		// The buffer is empty, so what still counts as waiting is inside the
		// save in front.
		b.writtenOff = true
		lost += int(n)
		b.waiting.Store(0)
		stats.Set("db.sample_rows_waiting", 0)
	}
	b.mu.Unlock()
	if lost > 0 {
		stats.Add("db.sample_rows_dropped", int64(lost))
		s.saveLog(slog.LevelError, "probe readings could not be saved at shutdown and are lost", "rows", lost, "err", err)
	}
}

// InsertPause records a monitoring-pause span [start, start+durationS): wall time
// during which no probe rounds ran - the master switch off, the latency toggle
// off, a closed schedule window, all address families off, or the process itself
// being down. UptimeSince subtracts these spans from its denominator so paused and
// unobserved time counts as neither up nor down; only OBSERVED time is scored.
// durationS <= 0 is a no-op. The monitor flushes spans incrementally (see Run) so
// a pause that never resumes is still reflected.
// Returns whether the row was STORED. A refusal is not an error - the span is
// simply not something this table can hold - but it is not a write either, and
// callers that pair a pause row with other state (the monitor deducts the same
// span from an outage's length) must be able to tell the two apart. Reporting
// refusal as `nil, nil` made a refused span look durable: the deduction was
// applied against a row that did not exist.
func (s *Store) InsertPause(ctx context.Context, start time.Time, durationS int64) (stored bool, err error) {
	// The other per-round write path (a board with monitoring paused at boot
	// checkpoints pauses, not samples): same deferred re-judgement as
	// InsertSamples, before the live row lands.
	s.maybeRepairFuturePauses()
	// The same rule the importer applies (PauseSpanSane). It used to check only for
	// a positive duration, which made this the one way into the table that a
	// crafted backup file could not have taken: the monitor measures a pause on the
	// wall clock, so a host whose clock jumps forward at boot - no RTC, waiting on
	// NTP - handed it a span reaching back to 1970 and it was stored without
	// question. Dropped rather than returned as an error, matching the existing
	// non-positive case: the caller cannot do anything useful with the failure, and
	// the monitor now declines to offer such a span in the first place.
	if !PauseSpanSane(start.Unix(), durationS) {
		return false, nil
	}
	// The rounds before the pause are stored before it. If they cannot be,
	// the pause is written all the same: it is the uptime denominator.
	s.SaveBuffered(SaveOrder)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO pauses (ts, duration_s) VALUES (?, ?)`, start.Unix(), durationS)
	recordDBErr(err)
	return err == nil, err
}

// SpeedSpan is one stretch in which a speedtest was using the network, as the
// latency chart draws it (unix seconds, End exclusive).
type SpeedSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// maxSpeedSpanS bounds one span. Both engines bound a run far below an hour (a
// sixteen-server Best-of round is budgeted at under half of one); a longer
// duration is clamped by InsertSpeedSpan. It is also the look-back that lets
// SpeedSpans find a span straddling the window's start through the ts index.
const maxSpeedSpanS = 3600

// speedSpanSane is the rule a span must meet to be stored: a believable start,
// a positive bounded length, and an end no later than the present. The future
// allowance is pauseFutureSkew, the same one PauseSpanSane uses, on purpose: a
// span is a stretch of wall time already gone by, exactly like a pause, so the
// two tables answer to one idea of how far a clock may honestly disagree.
func speedSpanSane(ts, dur, nowU int64) bool {
	return ts >= plausibleEpoch && dur >= 1 && dur <= maxSpeedSpanS && ts+dur <= nowU+pauseFutureSkew
}

// InsertSpeedSpan records that a speedtest used the network for durationS
// seconds from start. It mirrors InsertPause: a span the rule refuses (a wall
// clock that was not yet believable when the run started) is dropped and
// reported as stored=false, not as an error. A durationS above maxSpeedSpanS is
// clamped first, because a monotonic duration is honest even when absurd.
func (s *Store) InsertSpeedSpan(ctx context.Context, start time.Time, durationS int64) (stored bool, err error) {
	if durationS > maxSpeedSpanS {
		durationS = maxSpeedSpanS
	}
	if !speedSpanSane(start.Unix(), durationS, time.Now().Unix()) {
		return false, nil
	}
	s.SaveBuffered(SaveOrder) // the rounds taken before the test ended come first
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO speed_spans (ts, duration_s) VALUES (?, ?)`, start.Unix(), durationS)
	recordDBErr(err)
	return err == nil, err
}

// SpeedSpans returns the spans overlapping [since, until), oldest first, with
// any two that sit mergeS seconds apart or closer merged into one. A zero until
// means up to currentHorizon, like every open-ended read of the present.
// mergeS is the caller's bucket width: merged that way, the spans left are more
// than a bucket apart, so a window holds about as many as it holds chart points
// (maxSeriesPoints) however many tests it covers.
func (s *Store) SpeedSpans(ctx context.Context, since, until time.Time, mergeS int64) ([]SpeedSpan, error) {
	lo := since.Unix()
	hi := until.Unix()
	if until.IsZero() {
		hi = currentHorizon(time.Now().Unix())
	}
	// ts >= lo - maxSpeedSpanS keeps the scan on the index while still finding a
	// span that began before the window and runs into it.
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, duration_s FROM speed_spans
		 WHERE ts >= ? AND ts < ? AND ts + duration_s > ?
		 ORDER BY ts`, lo-maxSpeedSpanS, hi, lo)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	var out []SpeedSpan
	for rows.Next() {
		var ts, dur int64
		if err := rows.Scan(&ts, &dur); err != nil {
			return nil, err
		}
		sp := SpeedSpan{Start: ts, End: ts + dur}
		if n := len(out); n > 0 && sp.Start <= out[n-1].End+mergeS {
			out[n-1].End = max(out[n-1].End, sp.End)
			continue
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return out, nil
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
	s.SaveBuffered(SaveRead)
	now := time.Now().Unix()
	var v sql.NullInt64
	// The events arm counts only the two types this build reads. The answer is
	// "when was this monitor last known to be observing", and a row it cannot
	// interpret is not an observation it can vouch for - taking one as the anchor
	// shortens the startup gap booked as unobserved, so the stretch between the
	// last real evidence and that row is silently credited as watched time in the
	// uptime denominator.
	if err := s.db.QueryRowContext(ctx, `
		SELECT MAX(t) FROM (
			SELECT MAX(ts) AS t FROM samples WHERE ts >= ? AND ts <= ?
			UNION ALL
			SELECT MAX(ts) AS t FROM events WHERE ts >= ? AND ts <= ? AND type IN ('down','up')
			UNION ALL
			SELECT MAX(ts + duration_s) AS t FROM pauses WHERE ts >= ? AND ts <= ?)`,
		plausibleEpoch, now, plausibleEpoch, now, plausibleEpoch, now).Scan(&v); err != nil {
		return 0, false, err
	}
	return v.Int64, v.Valid, nil
}

// InsertEvent records an up/down transition. For 'up' events, durationS is the
// length of the outage that just ended; pass a negative value to store NULL.
//
// The live door holds eventRowSane's rule, and holds it the SAME WAY - one rule,
// two doors (this one and the import guard). At rest there is no third door: a
// row already on disk is only counted and reported, never deleted, because this
// build cannot tell "corruption an old version let in" from "an event a newer
// version understands" (see reportUnreadableEventTypes):
//
//   - an unreadable TYPE makes the whole row meaningless to this build, so it is
//     refused at the door. Every reader selects exactly 'down'/'up', so a third
//     value is inert once it is on disk - but keeping it OUT is still right,
//     because nothing downstream can act on what it does not understand.
//   - an impossible LENGTH does not make the row meaningless: its primary content
//     is the transition. Refusing it would drop a recovery and leave the preceding
//     'down' dangling, which every reader treats as an outage still running - a
//     caller whose clock produced one absurd number would have its outage
//     "corrected" into an unbounded one. The length alone is dropped.
//
// This is the running monitor's door, and only the monitor's. A 'down' through
// it puts an outage on the list a close of outages keeps away from, and an
// 'up' that lands ends one (monitorOutages). Anything else that writes an
// event goes through insertEvent.
func (s *Store) InsertEvent(ctx context.Context, ts time.Time, typ string, durationS int, detail string) error {
	// On the list before the row can be on disk, so a close that finds the row
	// also finds the line. A write that fails leaves the outage there: the
	// monitor keeps that 'down' and writes it again.
	if typ == "down" {
		s.live.opened(ts.Unix(), time.Now().Unix())
	}
	err := s.insertEvent(ctx, ts, typ, durationS, detail)
	if err == nil {
		s.live.landed(typ, ts.Unix())
	}
	return err
}

// insertEvent is InsertEvent's write, without the monitor's bookkeeping: the
// door for an event the monitor did not decide, which is the end a close of
// outages records.
func (s *Store) insertEvent(ctx context.Context, ts time.Time, typ string, durationS int, detail string) error {
	if typ != "down" && typ != "up" {
		return fmt.Errorf("insert event: unknown type %q (want \"down\" or \"up\")", typ)
	}
	var dur any
	if durationS >= 0 {
		if int64(durationS) > maxPauseDuration {
			// Counted, never silent: completedOutagesSince anchors an unpaired 'up' at
			// ts-duration_s, so the row we are declining to write would have been an
			// outage reaching back further than any retainable history, landing inside
			// every window that asks.
			stats.Inc("db.event_duration_dropped")
		} else {
			dur = durationS
		}
	}
	// The rounds that confirmed the change are stored before the event is, so
	// an outage record is never on disk ahead of the readings it rests on.
	// This is also the save a transition makes: every one of them writes an
	// event. If the rounds cannot be saved the event is written all the same.
	s.SaveBuffered(SaveOrder)
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
	s.SaveBuffered(SaveRead)
	anchor := nowU
	var first sql.NullInt64
	// Only events this build can read anchor the window FROM THE TABLE. This is
	// the denominator behind every uptime figure, and MIN takes whatever is
	// earliest, so one row carrying a type nothing here interprets drags the
	// anchor back to its timestamp and every percentage is then computed over a
	// span that includes time this install has no evidence it watched.
	//
	// It does not heal an anchor already PERSISTED from such a row. The stored
	// first_seen_ts read below only ever ratchets down, and it carries no
	// provenance - a build without this filter that ever ran an uptime read with
	// an unreadable row on disk wrote that row's timestamp there, and it stays.
	// Nothing can distinguish it from a legitimately old anchor after the fact,
	// and discarding stored anchors to be safe would re-introduce the exact bug
	// the stored anchor exists to prevent (sample retention walking MIN forward
	// and silently shortening every long window). So: this stops new poisoning,
	// and an install already carrying one keeps it.
	//
	// The type is not
	// indexed, so it is tested row by row while the query walks idx_events_ts in
	// ts order (EXPLAIN QUERY PLAN still reports SEARCH events USING INDEX
	// idx_events_ts, not a table scan), and the table holds two rows per outage.
	if err := s.db.QueryRowContext(ctx, `
		SELECT MIN(t) FROM (
			SELECT MIN(ts) AS t FROM samples WHERE ts >= ?
			UNION ALL
			SELECT MIN(ts) AS t FROM events WHERE ts >= ? AND type IN ('down','up'))`,
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

// HasHistory reports whether the database holds ANY measurement history -
// samples, events, or speed runs. This, not the install anchor's age, is what
// separates an upgrade from a fresh install: the anchor (first_seen_ts) only
// persists once a dashboard or digest computes uptime, so a headless install
// or a speedtest-only one can carry months of rows and no anchor at all.
// Cheap: three LIMIT-1 existence probes.
//
// The speed probe carries speedNotFailed like every other measurement read:
// accounting rows are the one kind of speed row that measured nothing (see the
// failed column), so an install whose every speedtest has failed has recorded
// no history, however many bytes it spent doing it. EstablishedInStore keys on
// this answer, and through it the ambiguous-container-access warning and the
// first-run consent flow - all three ask "has this install ever measured
// anything", not "has it ever written a row".
//
// The events probe names the two types for the same reason: a transition this
// build cannot interpret is not history it can show, so an install holding
// nothing else would answer "established" and skip the very flows that exist
// for a fresh install.
func (s *Store) HasHistory(ctx context.Context) (bool, error) {
	s.SaveBuffered(SaveRead)
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM samples LIMIT 1)
		OR EXISTS(SELECT 1 FROM events WHERE type IN ('down','up') LIMIT 1)
		OR EXISTS(SELECT 1 FROM speed WHERE `+speedNotFailed+` LIMIT 1)`).Scan(&n)
	return n != 0, err
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
	// Before the memo is read, not before the scan: a save can drop the memo,
	// and one that did so between the read of the generation below and the
	// publish would make this scan's answer look stale for nothing.
	s.SaveBuffered(SaveRead)
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
				HAVING SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END) * 2 > COUNT(*))`, lo, hi).Scan(&rec); err != nil {
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
	// An open outage ends at the newest sample. Read without the waiting
	// rounds it would be booked short.
	s.SaveBuffered(SaveRead)
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
// It stops at currentHorizon at the top for the same reason, and it MUST be the
// same bound their newest-event decision uses: both callers book the trailing
// 'down' as an open outage once a future-dated row stops answering "what happened
// most recently?", so a scan that still sees that row books the SAME seconds a
// second time - as an orphan gap here and as an open outage there. Bounding one
// end without the other is how the fix for the newest-event read doubled the
// downtime it was meant to restore.
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
			FROM events WHERE type IN ('down','up')
			  AND ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE type IN ('down','up') AND ts <= ?)
			                    AND ts <= ?)
		WHERE type='down' AND next_type='down'`, sinceU, currentHorizon(nowU))
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

// nextLocalDay returns the unix second at which the local date after d begins -
// the first instant whose local date is no longer d's. Shared by DowntimeByDay's
// proration and its observation loop so the two cannot bucket the same second into
// different days.
//
// Both of those loops walk the window one local day at a time by asking for the
// next boundary, so the answer has to be strictly later than d, it has to be the
// FIRST second the clock no longer reads d's date, and it has to be the SAME second
// whichever second of that run of the date it is asked from - the observation loop
// enters at the run's first second, proration enters wherever an outage happens to
// begin. Run, and not day: where the clock falls back across midnight a date comes
// back later on, and the two loops still have to agree on where each run of it ends.
//
// CONSTRUCTING that second is what does not work. Where a DST jump lands exactly on
// midnight, the next local midnight either does not happen at all (America/Havana,
// America/Santiago and Atlantic/Azores each spring) or happens twice (Asia/Gaza and
// Asia/Hebron each autumn), and time.Date answers the first case by normalizing to a
// neighbouring instant and the second by picking whichever of the two its offset
// lookup lands in - the later one wherever the zone is east of Greenwich. Adding a
// day to d first made it worse rather than better: AddDate keeps d's time of day, so
// asked from inside the very hour the next date skips, its own result normalized
// BACKWARDS onto d's date, the repair loop underneath it never ran because the day
// numbers already matched, and the boundary came back equal to its own input. Both
// callers then fell through to their "must advance" fallback and walked that hour one
// second at a time. The downtime arithmetic survived - a second is a second whichever
// tile holds it - but the observation loop mints a day's row on the first tile that is
// not fully watched and ASSIGNS that tile's span to window_s, so the hour of
// fully-watched one-second tiles ahead of it was never in the number: on the eve of
// the jump the heatmap offered "monitored 19h 0m of 23h 0m" for an ordinary 24-hour
// Saturday.
//
// SEARCHING for the next date is no better, and it fails somewhere rarer and quieter.
// A fall-back can carry the clock backwards ACROSS midnight: Newfoundland and
// Labrador changed the clocks at 00:01 until 2011, so on 2010-11-06 the local date
// reached the 7th, dropped back to the 6th a minute later, and only stayed on the 7th
// an hour after that. The local date does not increase with time there, so nothing may
// bisect on it - "has the date reached the next one yet" lands in the second stretch
// and answers an hour late from 85,440 of that date's 89,940 seconds, and answers
// differently depending on which second it was asked from, which is exactly what
// sharing this function between the two loops exists to rule out.
//
// So read the zone's own clock forward instead of guessing at a date. Between two
// clock changes the offset is fixed, so the local date turns only at local midnight,
// and local midnight is d's own time of day subtracted from d - a count of seconds, so
// no rule can normalize it anywhere. The only other instant the date can turn is a
// clock change, and the zone names its own. Whichever comes first wins: midnight if
// nothing changes before it, otherwise the change if the date turned with it, and
// otherwise the walk goes on from there, because an ordinary autumn fall-back puts the
// clock back inside the same date. That answers every shape - a midnight that never
// happens, a midnight that happens twice (the first one wins), a calendar date the
// zone skips outright, and a date that comes back - and it answers from the run of
// seconds d belongs to rather than from d itself, so every second of one run of a
// date gets the same boundary. Both candidates are later than d, so the result
// always is.
//
// The changes are read out of ZoneBounds' START, never its end. The end goes stale
// for the last day before a zone's stored transitions run out and its rule takes
// over: on the tzdata here that is 2040-12-31, where 192 of the 598 zones report an
// end EARLIER than the instant asked about. A walk that steps onto such an end never
// moves again.
func nextLocalDay(d time.Time, loc *time.Location) int64 {
	t := d.In(loc)
	y, m, day := t.Date()
	for {
		// Midnight, counted rather than built: whatever is left of today.
		h, mi, s := t.Clock()
		mid := t.Unix() + int64(86400-(h*3600+mi*60+s))
		// The FIRST clock change in (t, midnight], if the zone makes one. ZoneBounds
		// names the change that began the stretch an instant belongs to, so asking
		// about midnight and then about the second before each answer walks back to
		// the earliest one - the zone's own list read backwards, never a search over
		// dates.
		chg, changed := int64(0), false
		for probe := mid; probe > t.Unix(); probe = chg - 1 {
			// A zone that never changes reports the zero Time here, which is year one
			// and so falls under the same test as a change that is already behind us.
			// The last clause only keeps the walk moving: ZoneBounds' start has never
			// been observed past the instant it was asked about, in any zone, but a
			// probe that climbed would never come back.
			st, _ := time.Unix(probe, 0).In(loc).ZoneBounds()
			if st.Unix() <= t.Unix() || st.Unix() > probe {
				break
			}
			chg, changed = st.Unix(), true
		}
		if !changed {
			return mid // the offset holds until midnight, so midnight is the boundary
		}
		// The offset is fixed from t up to that change, so the date cannot have turned
		// before it. Either it turns AT it, or the day carries on past it.
		if ey, em, ed := time.Unix(chg, 0).In(loc).Date(); ey != y || em != m || ed != day {
			return chg
		}
		t = time.Unix(chg, 0).In(loc)
	}
}

// pauseSpans returns the recorded pause spans overlapping [from, to), each already
// clamped to it, oldest first.
//
// DowntimeByDay needs the pause overlap of EVERY day in its range, and the range
// is a year: asking pausedOverlap per day would be 366 round trips on every
// 60-second heatmap poll. Pause rows are few (one per episode, not per second), so
// one query and an in-Go intersection is both cheaper and exact.
func (s *Store) pauseSpans(ctx context.Context, from, to int64) ([][2]int64, error) {
	if to <= from {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, duration_s FROM pauses WHERE ts < ? AND ts + duration_s > ? ORDER BY ts`, to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]int64
	for rows.Next() {
		var ts, dur int64
		if err := rows.Scan(&ts, &dur); err != nil {
			return nil, err
		}
		a, b := ts, ts+dur
		if a < from {
			a = from
		}
		if b > to {
			b = to
		}
		if b > a {
			out = append(out, [2]int64{a, b})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Merge here, once, so every caller is handed a canonical union rather than raw
	// rows it has to defend against. Overlap is not only the crafted-import case: a
	// checkpoint flush and the resume flush that follows it can both cover the same
	// wall stretch, and two rows over one stretch used to subtract it from the
	// observed denominator TWICE - pushing coverage past the time actually paused,
	// which nothing downstream re-derives.
	return mergeSpans(out), nil
}

// observedOutageSpans returns the stretches of a completed outage that were
// actually WATCHED: its real wall interval [start,end) minus the recorded pause
// union, trimmed so the total never exceeds observed (the outage's duration_s).
//
// This is the canonical answer to "when was the link down?", and it exists
// because two surfaces used to answer it differently. UptimeSince modelled the
// outage as a contiguous [start, start+observed) - correct in TOTAL, but placed
// in the wrong seconds whenever a pause fell inside the outage, because the real
// recovery is later than start+observed by exactly the paused stretch. The
// heatmap meanwhile used the real interval. Both then reported the same figure
// for a window containing the whole outage and different figures for any window
// whose boundary cut through it - which is every uptime pill, every heatmap day
// boundary and every digest period.
//
// The trim runs from the END, matching DowntimeByDay's proration: wall time in
// excess of the observed length is unobserved time we have no pause row for (a
// system suspend), and the outage's leading edge is the part we are surest of.
// For an explicit pause the trim is a no-op, since wall-minus-pause already
// equals observed.
func observedOutageSpans(pauses [][2]int64, start, end, observed int64) [][2]int64 {
	if end <= start {
		return nil
	}
	var out [][2]int64
	cur := start
	for _, p := range mergeSpans(pauses) {
		if p[1] <= cur {
			continue
		}
		if p[0] >= end {
			break
		}
		if p[0] > cur {
			out = append(out, [2]int64{cur, p[0]})
		}
		if p[1] > cur {
			cur = p[1]
		}
	}
	if cur < end {
		out = append(out, [2]int64{cur, end})
	}
	// Trim the tail down to the observed length. A negative length is not a
	// measurement and must never mean "no limit": this used to return the whole
	// wall interval on `observed < 0`, and the only way a negative reaches here is
	// a corrupt or crafted backup row - InsertEvent writes NULL, never a negative,
	// so the product cannot produce one. One imported `up` with duration_s:-1
	// therefore booked its entire down-to-up gap as downtime and drove the uptime
	// ratio on every surface to near zero. Import rejects it now too (see
	// eventRowSane); this is the second lock on the same door, because the value
	// arrives from a file and the blast radius is every published figure.
	if observed <= 0 {
		return nil
	}
	remaining := observed
	for i := range out {
		width := out[i][1] - out[i][0]
		if width <= remaining {
			remaining -= width
			continue
		}
		out[i][1] = out[i][0] + remaining
		if out[i][1] == out[i][0] {
			return out[:i]
		}
		return out[:i+1]
	}
	return out
}

// completedOutage is one closed outage: the wall interval it really occupied and
// the length that was actually observed within it (its `up` row's duration_s).
type completedOutage struct{ start, end, observed int64 }

// completedOutagesSince returns the closed outages overlapping [from,to), each
// anchored at its paired 'down' (the true wall start) and running to its 'up'.
//
// It is shared rather than duplicated because it was duplicated once already, in
// SQL, and the copies drifted: UptimeSince moved to the observed-span model while
// ResolvedOutagesSince - the digest's source - kept modelling every outage as a
// contiguous [down, down+duration_s). Both then described the same outage in the
// same digest line and disagreed, because a pause inside an outage puts the real
// recovery later than the contiguous end.
//
// `to` is the present in both callers, and the scan stops at its currentHorizon:
// an 'up' the clock has not reached has not closed anything yet, and counting it
// here while the newest-event decision beside it (already bounded) still sees the
// outage as open books the same seconds twice. Same bound, same answer, or the
// uptime% and the heatmap disagree.
func (s *Store) completedOutagesSince(ctx context.Context, from, to int64) ([]completedOutage, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT o_start, ts, duration_s FROM (
			SELECT type, ts, duration_s,
			       CASE WHEN LAG(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) = 'down'
			            THEN LAG(ts) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END)
			            ELSE ts - duration_s END AS o_start
			FROM events WHERE type IN ('down','up')
			  AND ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE type IN ('down','up') AND ts <= ?)
			                    AND ts <= ?)
		WHERE type='up' AND duration_s IS NOT NULL AND o_start < ? AND ts > ?`,
		from, currentHorizon(to), to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []completedOutage
	for rows.Next() {
		var o completedOutage
		if err := rows.Scan(&o.start, &o.end, &o.observed); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// pauseUnionFor loads the merged pause spans covering the window AND every outage
// in it - one read, whatever the outage count (see spansIn). An outage that began
// before the window still contributes its in-window part, so the union has to
// reach back to the earliest outage start rather than stopping at `from`.
func (s *Store) pauseUnionFor(ctx context.Context, outages []completedOutage, from, to int64) ([][2]int64, error) {
	lo, hi := from, to
	for _, o := range outages {
		if o.start < lo {
			lo = o.start
		}
		if o.end > hi {
			hi = o.end
		}
	}
	return s.pauseSpans(ctx, lo, hi)
}

// observedDowntimeIn sums how much of [from,to) these outages were OBSERVED to be
// down, using the one interval model: the real wall span minus the pause union,
// trimmed to each outage's recorded observed length.
func observedDowntimeIn(outages []completedOutage, union [][2]int64, from, to int64) int64 {
	var n int64
	for _, o := range outages {
		spans := observedOutageSpans(spansIn(union, o.start, o.end), o.start, o.end, o.observed)
		n += spanOverlap(spans, from, to)
	}
	return n
}

// spansIn clips an already-merged, ts-sorted span list to [from,to), returning
// exactly what a fresh pauseSpans(from,to) would have returned - without a query.
//
// It exists because asking the database per outage made uptime cost the PRODUCT
// of the outage count and the pause count. Only `ts < ?` is indexable in that
// query's predicate, so every per-outage read walked the whole pause history
// preceding it to return the nought-or-one spans that actually overlap: measured
// at a year of five-minute checkpoints and two outages a day, UptimeSince went
// from ~11ms to ~2.6s and the six-window refresh to ~5.9s. Loading the union once
// and slicing it here is the same answer for one query instead of N.
func spansIn(spans [][2]int64, from, to int64) [][2]int64 {
	if to <= from || len(spans) == 0 {
		return nil
	}
	i := sort.Search(len(spans), func(i int) bool { return spans[i][1] > from })
	var out [][2]int64
	for ; i < len(spans); i++ {
		a, b := spans[i][0], spans[i][1]
		if a >= to {
			break
		}
		if a < from {
			a = from
		}
		if b > to {
			b = to
		}
		if b > a {
			out = append(out, [2]int64{a, b})
		}
	}
	return out
}

// mergeSpans returns the union of a ts-sorted, half-open span list: overlapping
// and touching spans are coalesced, so summing the result is the union's length
// rather than the sum of its parts. Input must be sorted by start, which is what
// pauseSpans' ORDER BY guarantees (clipping preserves it - both endpoints move
// toward the window, never past each other).
func mergeSpans(spans [][2]int64) [][2]int64 {
	if len(spans) < 2 {
		return spans
	}
	out := spans[:1]
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp[0] <= last[1] { // overlapping or adjacent: extend
			if sp[1] > last[1] {
				last[1] = sp[1]
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

// spanOverlap sums how many seconds of spans fall inside [from, to). Spans are
// normally disjoint episodes; duplicated or overlapping rows (the re-import hazard
// Prune's own comment flags) can oversum, so every caller clamps the result to the
// segment length exactly as UptimeSince clamps its downtime.
// PRECONDITION: spans are sorted by start and disjoint, which is what mergeSpans
// produces and what every caller passes (pauseSpans merges before returning, and
// observedOutageSpans emits the gaps between merged spans in order). That is what
// lets this skip rather than scan.
//
// The heatmap asks this once per local day AND once per outage segment over the
// same list, so a full scan per question is the cost that matters: a year of
// five-minute checkpoint pauses is ~52k spans, and one render was measured at
// 9.2ms of pure comparison before the search below. Everything ending before the
// window and everything starting after it is skipped outright.
func spanOverlap(spans [][2]int64, from, to int64) int64 {
	if to <= from || len(spans) == 0 {
		return 0
	}
	// First span that reaches into the window at all.
	i := sort.Search(len(spans), func(i int) bool { return spans[i][1] > from })
	var n int64
	for ; i < len(spans); i++ {
		a, b := spans[i][0], spans[i][1]
		if a >= to {
			break // sorted, so nothing after this one can overlap either
		}
		if a < from {
			a = from
		}
		if b > to {
			b = to
		}
		if b > a {
			n += b - a
		}
	}
	return n
}

// pausedOverlap returns the total recorded pause-span seconds overlapping [from,to].
//
// The WHERE clause is the same predicate pauseSpans uses and changes no result:
// a row outside [from,to) contributes MAX(0, negative) = 0 to the sum either way.
// It is there for the cost. `pauses` is a year of five-minute checkpoint rows for
// anyone using the monitoring schedule (~52k rows), and unfiltered this was a full
// SCAN doing the interval arithmetic on every one of them: 5.7ms a call, on the
// path /api/status and the digest take. Filtered, an early window becomes SEARCH
// pauses USING INDEX idx_pauses_ts (ts<?) and a recent one still scans but skips
// the arithmetic - 3.1ms measured either way. Only the `ts < ?` side can use the
// index: a pause span may be arbitrarily long (Prune deliberately keeps a
// startup-gap row that straddles the cutoff, see
// TestPruneKeepsPauseSpanStraddlingTheCutoff), so there is no safe lower bound on
// `ts` to hand the planner.
func (s *Store) pausedOverlap(ctx context.Context, from, to int64) (int64, error) {
	if to <= from {
		return 0, nil
	}
	// Via pauseSpans so this shares ONE union with the heatmap. The SQL aggregate
	// this replaced summed each row's clipped length independently, so two pause
	// rows overlapping by 50s reported 200s for a 150s union - more paused time
	// than the window contains. Both surfaces now derive paused time the same way,
	// which is the property the cross-render agreement tests assert.
	spans, err := s.pauseSpans(ctx, from, to)
	if err != nil {
		return 0, err
	}
	return spanOverlap(spans, from, to), nil
}

// Observation is one uptime window's evidence: the wall span it describes, how
// much of that span was actually watched, and how much of the watched time the
// link was down. Ratio and Coverage are derived from the same three fields, so a
// consumer that has one has necessarily been handed the other.
//
// It exists because UptimeSince used to return (ratio, coverage, err) and two of
// its four consumers dropped the coverage with a single `_`: /api/status and the
// digest both reported a confident percentage for windows that observed nothing,
// while /metrics - the one consumer that honoured it - correctly published no
// ratio at all. Two surfaces of one process told opposite stories in the same
// second (reachable on the documented `-latency=false` speedtest-only mode, and
// on `-ipv4=off -ipv6=off`, where coverage is permanently 0). Dropping coverage
// must not be a one-character mistake, so there is no longer a second return
// value to drop.
type Observation struct {
	// Window is the wall span the figure describes, AFTER UptimeSince's
	// monitoringSince and retention clamps - not necessarily the span asked for.
	Window time.Duration
	// Observed is Window minus the recorded pause spans overlapping it: paused,
	// scheduled-off, families-off, suspended and process-down time. It is the
	// denominator of Ratio and the only span this figure can speak for.
	Observed time.Duration
	// Down is the OBSERVED downtime in the window. Every branch of UptimeSince
	// already excludes the pause overlap, so it never exceeds Observed.
	Down time.Duration
}

// Ratio is the up-fraction (0..1) of OBSERVED time - not of wall time, so a
// stretch nobody watched is neither up nor down rather than silently credited as
// up.
//
// A window that observed nothing has no measurement to report and returns 1: a
// SENTINEL, not a reading, preserved bit-for-bit from the (ratio, coverage) tuple
// this type replaced (`observed <= 0` returned `1, 0, nil`) because a caller that
// renders "-" reads it and a caller that sums it must not see a NaN. Gate on
// Defined before presenting it; a bare 1 here means "nothing to say", not "perfect".
func (o Observation) Ratio() float64 {
	if o.Observed <= 0 {
		return 1
	}
	down := o.Down
	if down < 0 {
		down = 0
	}
	if down > o.Observed {
		down = o.Observed // corrupt/overlapping rows must not produce a negative ratio
	}
	return 1 - float64(down)/float64(o.Observed)
}

// Coverage is the fraction of the window that was actually observed (0..1). A low
// value means the ratio beside it is thin evidence; 0 means there is no ratio at
// all (see Defined). Exported as pingularity_uptime_coverage_ratio.
func (o Observation) Coverage() float64 {
	if o.Window <= 0 || o.Observed <= 0 {
		return 0
	}
	if o.Observed >= o.Window {
		return 1 // pause rows are clamped to the window, so this is the ordinary "nothing paused" case
	}
	return float64(o.Observed) / float64(o.Window)
}

// Defined reports whether the window holds a measurement AT ALL - the question
// /metrics asks before publishing pingularity_uptime_ratio, and the reason that
// series is absent rather than 1 for an unwatched window.
//
// This is deliberately not a threshold and takes no argument. At zero observed
// time Ratio is the hard-coded sentinel above, not a thin measurement, so there is
// nothing here to tune: a monitor legitimately watched 8 hours a day sits at
// coverage 0.3333 on every window forever, and any non-zero cutoff would delete
// all six uptime_ratio series for it while the coverage series kept saying "I have
// data". Consumers who want a cutoff apply their own, which PromQL lets them do
// (`and on(window) pingularity_uptime_coverage_ratio > 0.95`, the pattern the
// README already teaches) - and which nothing lets them do to a value the exporter
// refused to emit. "Thin" is the shipped word for LOW-BUT-NONZERO coverage (see
// the coverage HELP text and README); it is not this.
func (o Observation) Defined() bool { return o.Observed > 0 }

// UptimeSince returns the up-fraction over [since, now] and the evidence behind
// it, derived from the debounced outage events so it matches the heatmap and
// outage table - not a per-target-probe success rate (which would, e.g., halve
// when one address family is down though the link stayed online by quorum). Also
// far cheaper than scanning every sample row.
//
// The window is clamped to when monitoring began (monitoringSince) so a freshly
// started monitor isn't credited for time it never watched, and the ratio's
// denominator is OBSERVED time - wall time minus recorded pause spans - so
// paused, scheduled-off, families-off and process-down time is neither up nor
// down (it would otherwise be credited as up). An Observation whose Coverage is 0
// observed nothing (e.g. a monitor launched with probing off) and has no uptime
// figure to render; see Defined. retention (> 0) clamps the window start to
// now-retention so a window can't reach past the point where outage events are
// pruned - beyond which downtime is lost but the wall window would persist,
// drifting the figure optimistic (the "all" window especially).
func (s *Store) UptimeSince(ctx context.Context, since time.Time, retention time.Duration) (Observation, error) {
	nowU := time.Now().Unix()
	sinceU := since.Unix()

	// Clamp the window start to when monitoring actually began.
	first, err := s.monitoringSince(ctx, nowU)
	if err != nil {
		return Observation{}, err
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
		// Nothing to describe: the zero Observation reads as Ratio 1 / Coverage 0,
		// exactly what the (ratio, coverage) tuple returned here.
		return Observation{}, nil
	}

	// Downtime from completed outages overlapping the window. Each 'up' records
	// its outage's observed length in duration_s; anchor the span at the paired
	// 'down' event's timestamp (the true wall-clock start) and run it for
	// duration_s. Anchoring at up.ts-duration_s instead would mis-place an outage
	// whose observed length is shorter than its wall-clock gap because a suspend
	// or monitoring pause fell inside it. Fall back to up.ts-duration_s when no
	// 'down' precedes the 'up'. The scan is bounded to events from the last one
	// at/before sinceU, like the orphan scan below.
	// One model, one read. See completedOutagesSince / observedDowntimeIn.
	outages, err := s.completedOutagesSince(ctx, sinceU, nowU)
	if err != nil {
		return Observation{}, err
	}
	pauseUnion, err := s.pauseUnionFor(ctx, outages, sinceU, nowU)
	if err != nil {
		return Observation{}, err
	}
	downtime := observedDowntimeIn(outages, pauseUnion, sinceU, nowU)

	// Orphaned down->down gaps (a restart mid-outage wrote a second 'down' with no
	// closing 'up') carry no duration_s, so add them explicitly, each bounded at
	// its first quorum-recovery second. Shared with ResolvedOutagesSince (which also
	// uses the recovered-gap count; uptime needs only the downtime).
	gap, _, err := s.orphanGapDowntime(ctx, sinceU, nowU)
	if err != nil {
		return Observation{}, err
	}
	downtime += gap

	// Ongoing outage: if the latest event is a 'down', the link may still be
	// offline - OR it recovered without a closing 'up' (a restart mid-outage
	// leaves a dangling 'down'). The first second quorum returned (a strict
	// majority of either family's targets up - the live monitor's rule) proves
	// recovery; bound the outage there rather than running it to now and pinning
	// uptime low.
	//
	// "Latest" means latest AT THE PRESENT (currentHorizon). This decision is the
	// one place a single future-dated row does not merely add noise but SUBTRACTS a
	// real outage: an 'up' stamped ahead of the clock (a clock-ahead import, an RTC
	// years out) wins the ordering, answers "not down", and the ongoing outage
	// vanishes from the uptime figure for as long as the clock takes to catch up.
	var lastType string
	var lastTS int64
	switch err := s.db.QueryRowContext(ctx,
		`SELECT type, ts FROM events WHERE ts <= ? AND type IN ('down','up')
		 ORDER BY ts DESC, CASE type WHEN 'up' THEN 0 ELSE 1 END LIMIT 1`,
		currentHorizon(nowU)).Scan(&lastType, &lastTS); {
	case err == sql.ErrNoRows: // no events recorded → no outages
	case err != nil:
		return Observation{}, err
	default:
		if lastType == "down" {
			end := nowU
			if rec, ok, err := s.firstQuorumRecovery(ctx, lastTS, nowU); err != nil {
				return Observation{}, err
			} else if ok && rec < end {
				end = rec // quorum recovered here; not still down
			} else if newest, ok, err := s.newestSampleAt(ctx, nowU); err != nil {
				return Observation{}, err
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
				// From the union loaded above; the open outage runs to `end`, which is
				// bounded by nowU, so it is already inside it.
				paused := spanOverlap(pauseUnion, start, end)
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
	pausedWindow := spanOverlap(pauseUnion, sinceU, nowU)
	observed := window - pausedWindow
	if observed < 0 {
		observed = 0 // overlapping/imported pause rows can oversum; never a negative span
	}
	// Keep the clamp here rather than only inside Ratio: Down is a published field
	// (the digest and the agreement test read it), so it must be the same observed
	// downtime the ratio divides by, not a raw sum that can exceed the window.
	if downtime > observed {
		downtime = observed
	}
	return Observation{
		Window:   time.Duration(window) * time.Second,
		Observed: time.Duration(observed) * time.Second,
		Down:     time.Duration(downtime) * time.Second,
	}, nil
}

// Event is a recorded up/down transition.
type Event struct {
	TS          int64  `json:"ts"` // unix seconds (matches the other endpoints)
	Type        string `json:"type"`
	DurationS   int    `json:"duration_s"`
	HasDuration bool   `json:"has_duration"`
}

// EventCount returns the total number of recorded transition events.
//
// Same type filter as EventsPage, and it has to be: this is the TOTAL the pager
// divides into pages, so counting rows the page hides offers the operator a
// final page that comes back empty.
func (s *Store) EventCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type IN ('down','up')`).Scan(&n)
	return n, err
}

// EventsPage returns a page of transitions, newest first.
//
// Only the two types this build understands, like every other read of this
// table. A row carrying some other string in `type` used to be impossible to
// meet here because Open deleted it before anything ran; that deletion is gone
// (it destroyed a newer build's events on a downgrade - see
// reportUnreadableEventTypes), so the rows older versions accepted are now on
// disk while the reads run. Unfiltered, this one hands them to two consumers
// that cannot do anything sane with them: the outages table renders a
// transition it has no meaning for, and monitor.go asks for the single newest
// event to seed its event-clock guard - so an unreadable row that happens to be
// newest becomes the floor under every transition that process then records.
func (s *Store) EventsPage(ctx context.Context, limit, offset int) ([]Event, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, type, duration_s FROM events WHERE type IN ('down','up')
		 ORDER BY ts DESC LIMIT ? OFFSET ?`, limit, offset)
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
// A future-dated row is also excluded outright (ts <= currentHorizon):
// the wall-now clamp above only stops a future row shifting the window START, but
// within the window a future row for a still-live target would still win ORDER BY
// ts DESC and show a not-yet-real reading until the clock caught up. The small
// skew tolerance keeps a slightly-fast importer's just-now rows visible.
func (s *Store) LatestPerTarget(ctx context.Context, grace time.Duration) ([]TargetLatency, error) {
	s.SaveBuffered(SaveRead)
	now := time.Now().Unix()
	cut := `0`
	args := []any{}
	if grace > 0 {
		cut = `MIN((SELECT COALESCE(MAX(ts), 0) FROM samples), ?) - ?`
		args = append(args, now, int64(grace.Seconds()))
	}
	args = append(args, currentHorizon(now))
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

// liveTTLCap bounds how long a still-growing window (open-ended, or ending in
// the future) may be served from cache. It is a CAP, not a floor: it clips a
// TTL above it and raises nothing, so it engages only above bucket 480 - a span
// wider than about 8.3 days (480s buckets x the 1500 the server aims for,
// maxSeriesPoints web.go:1177). The 7d preset buckets to 403s (seriesBucket,
// web.go:1193) and keeps its own natural 100.75s; 5m/1h/6h/1d bucket to
// 1/2/14/57s and never reach the cache at all (the sub-minute return in Series
// below).
//
// The cost of raising this from 30s is chart-versus-reality lag: up to
// bucketSec/4 - 30s of extra staleness, which is 0 for every preset up to 1d,
// +70.75s at 7d, and reaches the full +90s only past that ~8.3-day span, where
// one bucket already averages eight minutes or more of probes.
//
// The old 30s was chosen "matching the status pills aggregate TTL" - a wrong
// reason, and the reason the number was wrong. That TTL is aggTTL
// (web.go:246), which guards aggregates() (web.go:250): uptime, data-usage and
// speed averages, read by /api/status, /readyz and /metrics. It never calls
// Series, whose only production caller is the lone non-test `.Series(` in the
// tree, in handleSeries (web.go:1791), so the two caches share no data and no
// figure was ever kept in step by giving them the same number.
const liveTTLCap = 120 * time.Second

// seriesEntry caches one computed aggregate; mu single-flights concurrent
// viewers of the same key so only one runs the scan.
type seriesEntry struct {
	mu sync.Mutex
	// scanned records that a scan has completed and stored its result in this
	// entry, INCLUDING a result of no points. It exists because two different
	// states both leave expires at the zero time - "nobody has filled this entry
	// yet" and "a scan filled it with nothing" - and the outcome switch in Series
	// has to tell them apart. Written and read only under mu.
	scanned bool
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
		// Counted separately because this return is the ONLY place the sub-minute
		// buckets are visible: they never consult the cache, so none of the
		// hit/new/empty/expired counters below can account for them and a dashboard
		// doing nothing but this would read as an idle, perfectly-behaved cache.
		// Four of the five UI range presets land here: the chart's preset ladder is
		// [[5,'5m'],[60,'1h'],[360,'6h'],[1440,'1d'],[10080,'7d']] minutes
		// (index.html, grep that literal), which seriesBucket (web.go:1193) turns
		// into 1/2/14/57/403s over maxSeriesPoints = 1500 (web.go:1177) - only 7d
		// clears 60.
		stats.Inc("series.bypass")
		return s.seriesQuery(ctx, since, until, bucketSec, excludeTargets)
	}
	ex := append([]string(nil), excludeTargets...)
	sort.Strings(ex)
	// Floor the start to its bucket boundary and run the aggregate from THERE, so a
	// cache miss computes exactly the window the key names. The key already floors
	// the start (below), so two exact starts in the same bucket share one entry -
	// but an aggregate keyed off the caller's exact start would build a
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
	e := s.seriesCache[key]
	if e == nil {
		if len(s.seriesCache) > 32 {
			s.seriesCache = map[seriesKey]*seriesEntry{}
		}
		e = &seriesEntry{}
		s.seriesCache[key] = e
	}
	s.seriesMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	// Freshness is judged - and counted - only after e.mu is held, never from the
	// map lookup above. A caller that arrived while another was mid-seriesQuery
	// found the entry stale (or absent), then BLOCKED on this mutex and is handed
	// the fresh result right here without running a query of its own. Counting
	// what the lookup saw would book a miss for every request the single-flight
	// just absorbed, overstating query load by exactly the concurrency the lock
	// is saving.
	if time.Now().Before(e.expires) {
		stats.Inc("series.cache.hit")
		return e.pts, nil
	}
	// Three outcomes, because they have three different causes and only one of
	// them is a TTL's fault. Every request that gets past the return above runs a
	// scan, so the three have to partition the misses exactly: hit + new + empty
	// + expired is every request that reached the cache, and new + empty +
	// expired is every scan the cache path ran (series.query counts those plus
	// the sub-minute bypasses).
	//
	// The switch reads the ENTRY's state, never whether the map lookup above
	// found one. The lookup ran before e.mu was taken and describes the cache as
	// it was then, not as it is here: the insert publishes a zero-value entry and
	// drops seriesMu before anyone locks e.mu, so a second caller can find that
	// entry in the map and still be the first through this switch. Reading the
	// lookup made that caller book .empty for a key nobody had filled yet.
	switch {
	case !e.scanned:
		// Nothing has ever been stored in this entry, so there was nothing to
		// serve and the scan below is compulsory. Three ways to get here: no entry
		// existed and the insert above made this one (first sight of the key, or
		// the over-32-entry wipe dropped the old one); another caller published an
		// entry and this caller reached e.mu before it stored anything; or every
		// scan on this entry so far returned an error, which returns below without
		// storing. Kept apart from expiry because a rolling window's key rotates
		// every bucketSec on its own (the start is floored to the bucket, above):
		// 403s at the 7d preset. That churn is compulsory and no TTL change can
		// touch it, so a single "miss" counter would credit or blame a TTL for
		// turnover it never controlled.
		stats.Inc("series.cache.new")
	case e.expires.IsZero():
		// A scan completed on this entry and stored no points: the empty-result
		// branch at the bottom of this function leaves the expiry at the zero time
		// on purpose. Two other things also leave a zero expiry - the zero-value
		// entry the insert above publishes, and a scan that errored before
		// reaching the assignment - but neither sets scanned, so both are caught
		// by the case above and only a real empty result reaches here.
		// Nothing expired, so .expired here would be a metric that lies - and it
		// would lie loudest where it does the most damage: on a fresh install
		// every wide window is empty, so every poll after the first would report
		// an expiry that never happened and a TTL change would look like it had
		// made things worse. It is not .new either - the entry is not a fresh one,
		// this is a key being re-scanned because its last scan found nothing to
		// serve. Its own counter, because its cause is its own and its cure is
		// too: not a short TTL and not key churn, but a window with no rows in
		// it, which no TTL can fix and which stops on its own once samples land.
		stats.Inc("series.cache.empty")
	default:
		// A populated entry whose TTL ran out - the only miss a TTL change can
		// remove. .new is key churn and .empty is a window with no rows, and
		// neither moves when liveTTLCap or the quarter-bucket TTL does, so a
		// before/after TTL figure is a claim about THIS counter and the hits it
		// turns into.
		stats.Inc("series.cache.expired")
	}
	pts, err := s.seriesQuery(ctx, since, until, bucketSec, excludeTargets)
	if err != nil {
		return nil, err
	}
	e.pts = pts
	// This entry now holds a result - even if that result is no points at all -
	// so the next caller through the switch above reads .empty or .expired rather
	// than .new. An errored scan returns above without getting here, leaving the
	// entry exactly as it found it.
	e.scanned = true
	// The quarter-bucket TTL rests on "the aggregate only changes materially once
	// per bucket" - true for a mature window, but on a young install a wide window
	// is one trailing partial bucket that changes every probe round, so a long TTL
	// would pin a near-empty first-run chart (and a missing DNS line) for up to
	// ~87min on a 1y range. Cap the trailing (open-ended) window at liveTTLCap; a
	// fixed historical window is safe to cache for its full bucket. Never pin an
	// empty result at all, so a fresh DB re-aggregates as soon as its first
	// samples land.
	ttl := time.Duration(bucketSec) * time.Second / 4
	// A window whose end is still in the FUTURE is fixed but NOT historical: new
	// samples keep entering it, so treat it like an open-ended window and cap the
	// TTL. The UI accepts future-ended spans (a typed range like "jul 1 to dec 31"
	// clamps to now+366d, not now), so without this a future-ended range would
	// pin new samples out of the chart for up to bucketSec/4 (~88 min at 1y). Once
	// the end passes into the past the window becomes genuinely historical and the
	// full bucket TTL is valid again.
	if (until.IsZero() || until.After(time.Now())) && ttl > liveTTLCap {
		ttl = liveTTLCap
	}
	if len(pts) == 0 {
		// Left at the zero time, which - paired with the scanned flag set just
		// above - is what the next poll reads to book series.cache.empty rather
		// than a false .expired (the switch above). Assigned rather than assumed:
		// an entry that held points before and scans empty now has to be reset to
		// the empty state, not left pinned on its old expiry.
		e.expires = time.Time{}
	} else {
		e.expires = time.Now().Add(ttl)
	}
	return pts, nil
}

// seriesScanHook runs at four points of seriesQuery, with the number of rows
// read so far: "samples" once the samples statement is open and before its
// first row is read, "dns" between the two statements, "dnsrows" once the dns
// statement is open and before its first row is read, and "end" on every way
// out of a scan that got its read snapshot. A zero width and a snapshot that
// would not open return before that, with nothing read. Test seam only (nil in
// production), like importChunkHook: a write or a cancellation landing between
// the two scans or inside one of them is the interesting case, and nothing
// else can put one there.
var seriesScanHook func(stage string, rows int64)

// seriesFamCode is a sample's family the way the fold wants it: 0 for ipv4, 1
// for ipv6, and the stored value itself for anything else (an import can carry
// any text). The driver hands a small integer over without allocating; a text
// value per row costs two allocations.
const seriesFamCode = `CASE ` + famExpr + ` WHEN 'ipv4' THEN 0 WHEN 'ipv6' THEN 1 ELSE ` + famExpr + ` END`

// seriesSamplesSQL reads one row per sample in ts order: the timestamp, the
// latency if the row counts toward the lowest line, whether it succeeded, and
// its family. INDEXED BY is a hard constraint, as in LatestPerTarget: without
// the index SQLite would sort the window to satisfy ORDER BY, which is the
// cost this query exists to avoid, so it fails at plan time instead.
func seriesSamplesSQL(latFilter string) string {
	return `
		SELECT ts,
		       CASE WHEN success = 1` + latFilter + ` THEN latency_ms END,
		       CASE WHEN success = 1 THEN 1 ELSE 0 END,
		       ` + seriesFamCode + `
		FROM samples INDEXED BY idx_samples_ts
		WHERE ts >= ? AND ts < ?
		ORDER BY ts`
}

// seriesDNSSQL reads the successful DNS readings in ts order. The CAST is what
// AVG did to a value before adding it, so text left in the column by an old
// import still counts the way it did.
const seriesDNSSQL = `
		SELECT ts, CASE WHEN success = 1 THEN CAST(latency_ms AS REAL) END
		FROM dns INDEXED BY idx_dns_ts
		WHERE ts >= ? AND ts < ?
		ORDER BY ts`

// seriesInt takes an INTEGER cell. Anything else is an error. For a sample's ts
// that is a fraction left behind by an import of an older build, and it fails
// the read as it did when the bucket made from it went through database/sql.
// (Not in every last case: see the known difference at seriesSQLOracle.)
type seriesInt int64

func (c *seriesInt) Scan(src any) error {
	v, ok := src.(int64)
	if !ok {
		return fmt.Errorf("series: %T where an integer belongs", src)
	}
	*c = seriesInt(v)
	return nil
}

// What a latency cell held. The order is SQLite's: a number sorts below text,
// and text below a blob.
const (
	seriesNull = iota
	seriesNumber
	seriesText
	seriesBlob
)

// seriesCell takes a latency cell as stored. The column should hold a number or
// NULL; text and blobs from old imports can still be at rest, and MIN ranked
// them rather than failing, so they are kept here with their kind.
type seriesCell struct {
	kind uint8
	num  float64
	raw  []byte // text or blob bytes, reused from row to row
}

func (c *seriesCell) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		c.kind = seriesNull
	case float64:
		c.kind, c.num = seriesNumber, v
	case int64:
		c.kind, c.num = seriesNumber, float64(v)
	case string:
		c.kind, c.raw = seriesText, append(c.raw[:0], v...)
	case []byte:
		c.kind, c.raw = seriesBlob, append(c.raw[:0], v...)
	default:
		return fmt.Errorf("series: %T in a latency cell", src)
	}
	return nil
}

// seriesFamCell takes seriesFamCode: 0 or 1, or another family's own value.
type seriesFamCell struct {
	code int // 0 ipv4, 1 ipv6, 2 anything else
	// The other value was stored as a blob. That is a different family from the
	// same bytes stored as text, as it was to GROUP BY.
	blob bool
	raw  []byte
}

func (c *seriesFamCell) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		if v != 0 && v != 1 {
			return fmt.Errorf("series: family code %d", v)
		}
		c.code = int(v)
	case string:
		c.code, c.blob, c.raw = 2, false, append(c.raw[:0], v...)
	case []byte:
		c.code, c.blob, c.raw = 2, true, append(c.raw[:0], v...)
	default:
		return fmt.Errorf("series: %T in a family cell", src)
	}
	return nil
}

// seriesDNSTime takes a dns row's ts: a whole number, or a fraction left behind
// by an import of an older build. The fraction is kept as it is, because the
// row's bucket is worked out differently (fracBucket) and no dns row ever
// failed the read.
type seriesDNSTime struct {
	whole int64
	frac  float64
	odd   bool // the ts is in frac
}

func (c *seriesDNSTime) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		c.whole, c.odd = v, false
	case float64:
		c.frac, c.odd = v, true
	default:
		return fmt.Errorf("series: %T where a timestamp belongs", src)
	}
	return nil
}

// seriesFamCount is one family's tally inside one bucket.
type seriesFamCount struct{ n, ok int64 }

// online is the quorum rule: a strict majority of the family's rows succeeded.
// It is the rule the prober judges a round by (aggregate, internal/prober).
func (f seriesFamCount) online() bool { return f.ok*2 > f.n }

// seriesMean adds numbers up the way SQLite's AVG does (Kahan-Babuska-Neumaier
// summation), in the same order, so the mean comes out the same to the last
// bit. A plain sum does not. There are only additions here, so the compiler has
// nothing to fuse.
type seriesMean struct {
	sum, comp float64
	n         int64
}

func (m *seriesMean) add(r float64) {
	t := m.sum + r
	if math.Abs(m.sum) > math.Abs(r) {
		m.comp += (m.sum - t) + r
	} else {
		m.comp += (r - t) + m.sum
	}
	m.sum = t
	m.n++
}

// value is the mean, or false when there is none: nothing was added, or the
// result is not a number (Inf plus -Inf), which SQLite returns as NULL.
func (m *seriesMean) value() (float64, bool) {
	if m.n == 0 {
		return 0, false
	}
	r := m.sum
	if !math.IsInf(m.comp, 0) && !math.IsNaN(m.comp) {
		r += m.comp
	}
	r /= float64(m.n)
	return r, !math.IsNaN(r)
}

// seriesAgg folds ts-ordered rows into SeriesPoints. One per scan; nothing in
// it is shared, and the points it returns are never reused (Series caches them).
type seriesAgg struct {
	width int64
	out   []SeriesPoint
	rows  int64 // rows read so far, samples and dns; for the hook
	hook  func(stage string, rows int64)

	open bool // a bucket is being filled
	bts  int64
	fam  [2]seriesFamCount // ipv4, ipv6
	// Families other than the two, keyed by their bytes: [0] stored as text, [1]
	// as a blob. A map, not a list: an import may carry any number of them.
	other [2]map[string]*seriesFamCount

	hasLat bool
	lat    float64
	// The smallest text or blob latency of the bucket. It only matters when the
	// bucket has no number at all, because a number always ranks lower.
	oddKind uint8
	odd     []byte
}

// bucket is the start of ts's bucket, by the same integer arithmetic SQLite
// used ((ts / width) * width, truncating toward zero), so timestamps below
// zero land where they did.
func (a *seriesAgg) bucket(ts int64) int64 { return (ts / a.width) * a.width }

// fracBucket is the bucket of a dns row whose ts is a fraction, and false when
// the row has none. SQLite ran the same (ts / width) * width on it in floating
// point, where nothing truncates: the bucket came out as about the ts itself, a
// value no sample's bucket has, and the row went nowhere. It was counted only
// where the arithmetic gave the start of a bucket exactly, which in practice
// takes a ts so close to zero that dividing it leaves nothing. Both are kept
// as they were. A division and a multiplication give the compiler nothing to
// fuse, so this rounds as SQLite did.
func (a *seriesAgg) fracBucket(ts float64) (int64, bool) {
	w := float64(a.width)
	r := (ts / w) * w
	if r != math.Trunc(r) || r < -1<<63 || r >= 1<<63 {
		return 0, false
	}
	b := int64(r)
	return b, b%a.width == 0
}

// dnsBucket is the bucket a dns row belongs to, and false when it has none.
func (a *seriesAgg) dnsBucket(ts *seriesDNSTime) (int64, bool) {
	if ts.odd {
		return a.fracBucket(ts.frac)
	}
	return a.bucket(ts.whole), true
}

func (a *seriesAgg) at(stage string) {
	if a.hook != nil {
		a.hook(stage, a.rows)
	}
}

func (a *seriesAgg) readSamples(ctx context.Context, q rowQuerier, query string, args []any) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.at("samples")
	var (
		ts, ok seriesInt
		lat    seriesCell
		fam    seriesFamCell
	)
	dest := []any{&ts, &lat, &ok, &fam}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		a.rows++
		if err := a.addSample(int64(ts), &lat, int64(ok), &fam); err != nil {
			return err
		}
	}
	// A cancelled request surfaces here: database/sql closes the rows when the
	// context ends, Next returns false, and Err is the context's error.
	if err := rows.Err(); err != nil {
		return err
	}
	return a.closeBucket()
}

func (a *seriesAgg) addSample(ts int64, lat *seriesCell, ok int64, fam *seriesFamCell) error {
	if bts := a.bucket(ts); !a.open || bts != a.bts {
		if err := a.closeBucket(); err != nil {
			return err
		}
		a.open, a.bts = true, bts
	}
	f := &a.fam[0]
	switch {
	case fam.code < 2:
		f = &a.fam[fam.code]
	default:
		k := 0
		if fam.blob {
			k = 1
		}
		if f = a.other[k][string(fam.raw)]; f == nil {
			if a.other[k] == nil {
				a.other[k] = map[string]*seriesFamCount{}
			}
			f = &seriesFamCount{}
			a.other[k][string(fam.raw)] = f
		}
	}
	f.n++
	f.ok += ok
	switch lat.kind {
	case seriesNumber:
		if !a.hasLat || lat.num < a.lat {
			a.hasLat, a.lat = true, lat.num
		}
	case seriesText, seriesBlob:
		if a.oddKind == seriesNull || lat.kind < a.oddKind ||
			(lat.kind == a.oddKind && bytes.Compare(lat.raw, a.odd) < 0) {
			a.oddKind, a.odd = lat.kind, append(a.odd[:0], lat.raw...)
		}
	}
	return nil
}

// closeBucket turns the bucket being filled into a point.
func (a *seriesAgg) closeBucket() error {
	if !a.open {
		return nil
	}
	p := SeriesPoint{TS: a.bts}
	switch {
	case a.hasLat:
		v := a.lat
		p.LatencyMS = &v
	case a.oddKind != seriesNull:
		// Only text or blobs succeeded here. The old query handed the smallest one
		// to database/sql, which parsed it or failed the whole read. Same here.
		v, err := strconv.ParseFloat(string(a.odd), 64)
		if err != nil {
			return fmt.Errorf("series: latency in bucket %d is not a number: %w", a.bts, err)
		}
		p.LatencyMS = &v
	}
	p.Online = a.fam[0].online() || a.fam[1].online()
	for k := range a.other {
		for _, f := range a.other[k] {
			if f.online() {
				p.Online = true
			}
		}
		// Forget this bucket's families. Dropped, not cleared: clearing a map costs
		// the size it once grew to, at every bucket after.
		if len(a.other[k]) > 0 {
			a.other[k] = nil
		}
	}
	a.out = append(a.out, p)
	a.open = false
	a.fam = [2]seriesFamCount{}
	a.hasLat, a.oddKind = false, seriesNull
	return nil
}

// readDNS gives each point the mean of its bucket's DNS readings. A reading in
// a bucket without samples has no point to go to and is dropped, as the LEFT
// JOIN dropped it.
func (a *seriesAgg) readDNS(ctx context.Context, q rowQuerier, sinceU, upperU int64) error {
	rows, err := q.QueryContext(ctx, seriesDNSSQL, sinceU, upperU)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.at("dnsrows")
	var (
		ts   seriesDNSTime
		lat  seriesCell
		open bool
		bts  int64
		mean seriesMean
		i    int // the first point not yet passed; both lists are in ts order
	)
	put := func() {
		if !open {
			return
		}
		for i < len(a.out) && a.out[i].TS < bts {
			i++
		}
		if i < len(a.out) && a.out[i].TS == bts {
			if v, ok := mean.value(); ok {
				a.out[i].DNSms = &v
			}
		}
	}
	dest := []any{&ts, &lat}
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		a.rows++
		b, ok := a.dnsBucket(&ts)
		if !ok {
			continue
		}
		if !open || b != bts {
			put()
			open, bts, mean = true, b, seriesMean{}
		}
		if lat.kind == seriesNumber {
			mean.add(lat.num)
		}
	}
	// As in readSamples. Without this a scan that was cut short would end as
	// a chart with part of its DNS line, or none, and be kept as one.
	if err := rows.Err(); err != nil {
		return err
	}
	put()
	return nil
}

// seriesQuery runs the actual Series aggregate (see Series for semantics).
//
// It reads the window's rows in time order and folds them into buckets here,
// in Go. Asking SQLite for the buckets (GROUP BY bucket, family) made it sort
// every row of the window first, although the ts index already hands them over
// in order. That sort was over half the time of a wide scan, and from about
// 100,000 rows up it spilled into temp files on every chart redraw. The two
// statements below walk their ts index and nothing else, so nothing is sorted.
//
// The points must not change by a bit. The statement this replaced is kept as
// seriesSQLOracle in series_stream_test.go, and the tests there compare the two.
func (s *Store) seriesQuery(ctx context.Context, since, until time.Time, bucketSec int, excludeTargets []string) ([]SeriesPoint, error) {
	// Counted here, not at the two call sites (the sub-minute bypass and the cache
	// miss in Series above), so "a scan really ran" cannot drift from the code that
	// runs one. An error still counts: the work was done and the DB paid for it.
	// The duration goes into a histogram rather than a running mean because the
	// spread is the whole story - a 5m window and a 30d window run the same code
	// and differ by orders of magnitude (see BenchmarkSeriesQuery,
	// series_bench_test.go), so a mean over both describes neither. Which bounds
	// it lands in is internal/stats' business (stats.Observe picks them by metric
	// name), not this call site's.
	stats.Inc("series.query")
	queryStart := time.Now()
	defer func() { stats.Observe("series.query.seconds", time.Since(queryStart).Seconds()) }()
	// The fold divides by the width. Series never passes less than 1; this is for
	// a direct caller, which gets an error where the division would panic.
	if bucketSec == 0 {
		return nil, errors.New("series: bucket width is zero")
	}
	// excludeTargets drops targets from the latency MIN (the "lowest" line) only;
	// online/outage detection below still counts every target, since connectivity
	// is global truth, not a per-user display filter. Placeholders keep it
	// injection-safe. The test stays in SQL: doing it here would need every row's
	// target as a Go string.
	latFilter := ""
	var args []any
	for _, t := range excludeTargets {
		latFilter += "?,"
		args = append(args, t)
	}
	if latFilter != "" {
		latFilter = " AND target NOT IN (" + latFilter[:len(latFilter)-1] + ")"
	}
	// An absolute window bounds BOTH scans below. Bounding only the samples one
	// would leave the DNS line on a different window than the latency line it is
	// drawn beside - wrong data rather than a visible break.
	//
	// An open-ended window still ends: at the present (currentHorizon), not at
	// whatever the newest row on disk claims. Left unbounded, a sample stamped
	// ahead of the clock draws a latency point - and, through the quorum, an
	// outage band - in a bucket the wall clock has not reached, and it stays there
	// until it does.
	upperTS := currentHorizon(time.Now().Unix())
	if !until.IsZero() {
		upperTS = until.Unix()
	}
	args = append(args, since.Unix(), upperTS)
	// One read snapshot for both scans, so the two lines of a chart come from the
	// same commit, as they did when one statement read both tables. Two plain
	// pool queries could each see a different one. Everything below runs on the
	// transaction, never on s.db: the in-memory pool has one connection and this
	// holds it. The waiting rounds are saved before it opens, not by a cache
	// hit in Series, which reads nothing.
	s.SaveBuffered(SaveRead)
	tx, err := s.BeginReadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	agg := seriesAgg{width: int64(bucketSec), hook: seriesScanHook}
	defer agg.at("end")
	if err := agg.readSamples(ctx, tx, seriesSamplesSQL(latFilter), args); err != nil {
		return nil, err
	}
	// A bucket exists only where a sample does, so with none there is nothing for
	// a DNS reading to attach to. nil, not an empty slice, as before.
	if len(agg.out) == 0 {
		return nil, nil
	}
	agg.at("dns")
	if err := agg.readDNS(ctx, tx, since.Unix(), upperTS); err != nil {
		return nil, err
	}
	return agg.out, nil
}

// SpeedSample is one completed speedtest plus the connection context it ran in.
type SpeedSample struct {
	TS       int64    `json:"ts"`
	DownMbps float64  `json:"down_mbps"`
	UpMbps   float64  `json:"up_mbps"`
	PingMS   float64  `json:"ping_ms"`
	JitterMS *float64 `json:"jitter_ms,omitempty"`
	// PingBestMS is the fastest of the ping samples PingMS averages. The engine
	// reports a MEAN, which one stalled sample drags upward without limit (a
	// single ~225ms handshake among nine ~4.6ms ones reads as 30ms, and lands in
	// Jitter as a ~66ms standard deviation). The floor is what the link can
	// actually do, so the run's DECISIONS - server choice and the ping threshold -
	// judge on this, while PingMS stays the engine's own number so the reported
	// figure still matches what speedtest.net would show. nil on iperf3 rows and
	// on rows written before this existed; fall back to PingMS there.
	PingBestMS  *float64 `json:"ping_best_ms,omitempty"`
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
	// IPFamily is the address family the transfer actually used ("4"/"6", or
	// "mixed" when the run's connections really did split across both and no
	// single family describes the row) - family "auto" resolves invisibly, so
	// it's read back from the run itself (see speedtest.Result.IPFamily). Empty
	// on rows recorded before it was captured and on engines that don't report
	// it; never backfilled.
	IPFamily string `json:"ip_family,omitempty"`
	// Failed marks a row that is ACCOUNTING, not a measurement: a run whose every
	// candidate failed still moved DownBytes/UpBytes real bytes onto the user's
	// bill, so the usage is recorded while no speed, ping or verdict was ever
	// produced. Set only by the scheduler's failure path. Rows carrying it are
	// invisible to every measurement read on this type (see speedNotFailed) and
	// counted by the data-usage sums; a consumer that reads one out of an export
	// must treat it as "nothing was measured", NOT as a 0 Mbps reading. The one
	// place such a row is listed is the runs table (and its CSV), and only as a
	// failure record: Failed plus FailStage, see speedIsListed.
	Failed bool `json:"failed,omitempty"`
	// FailStage is the stage a wholly failed run stopped at (the closed
	// speedtest.speedFailStage vocabulary), set only on a failure record - the
	// usage row of a failed run the operator asked to keep, with Record failed
	// tests on. Failed is set too, so every measurement read hides the row; its
	// speeds and ping were never measured and read back as 0, which means
	// nothing (check Failed first). Empty on every other row, and never read
	// back from one: scanSpeed drops a stage that is not beside the marker.
	FailStage string `json:"fail_stage,omitempty"`
	// UsageRunTS is the ts of the measurement row an accounting row bills for -
	// the reference DeleteSpeed cascades on, so removing a run takes the spend of
	// its abandoned attempts with it and NOTHING else. nil on measurements, on
	// the row a wholly failed run leaves (no measurement exists to point at), and
	// on accounting rows written before the column existed.
	//
	// WRITE-SIDE ONLY: InsertSpeed stores it, the cascade compares it in SQL, and
	// no read scans it back - speedCols does not carry it. Nothing in the app or
	// the API needs it (the rows holding it are hidden from every measurement
	// read by design), and a column absent from the scan list cannot wedge every
	// speed read the way an unreadable `healthy` once did - see
	// repairUnreadableIntColumns, which exists because one such row broke the
	// speed panel and the status table permanently.
	UsageRunTS *int64 `json:"usage_run_ts,omitempty"`
	// Race is how the run's centre was chosen - the city race's outcome, or
	// why it did not run (see speedtest.RaceVerdict for the vocabulary). All
	// empty/nil on rows written before it existed and on iperf3 runs; the
	// pair of pointers stays nil rather than 0 so "unrecorded" has one spelling.
	RaceOutcome     string   `json:"race_outcome,omitempty"`
	RaceOrigins     string   `json:"race_origins,omitempty"`
	RaceWinnerKind  string   `json:"race_winner_kind,omitempty"`
	RaceWinnerLabel string   `json:"race_winner_label,omitempty"`
	RaceWinnerMS    *float64 `json:"race_winner_ms,omitempty"`
	RaceRacers      *int64   `json:"race_racers,omitempty"`
	RaceWinnerLat   *float64 `json:"race_winner_lat,omitempty"`
	RaceWinnerLon   *float64 `json:"race_winner_lon,omitempty"`
	// RoundTS is the run this row was measured in and did not win - the
	// winner row's ts - on a loser a Best-of round kept (Discard losers off);
	// nil on every row that is a run's own result. See speedIsResult.
	RoundTS *int64 `json:"round_ts,omitempty"`
	// WinReason is why the run's server was the one measured - the winner
	// row's win_reason from the selection report (speed_servers). Never stored
	// on the speed row: every reader RESOLVES it, so a new one must too, or the
	// column it fills reads empty. Two do - the runs listing, through
	// WinReasonsFor (one query per page), and the CSV export, through
	// SpeedHistoryDescFunc's own subquery (the streaming shape has nowhere to
	// batch a page). Empty on every other read. Free text out of the DB, so a
	// reader that writes it into a document escapes it: the export runs it
	// through csvSafe.
	WinReason string `json:"win_reason,omitempty"`
	// UDPDirection is which way the UDP loss/jitter probe sampled ("down"/"up");
	// empty when loss/jitter went unmeasured or the row predates the field. Loss
	// on an asymmetric path differs by direction, so a sample without it stays
	// ambiguous rather than being guessed at.
	UDPDirection string `json:"udp_direction,omitempty"`
	// Latency under load: idle baseline and per-phase loaded medians (ms), same
	// method/target so loaded-minus-idle is bufferbloat. nil when unmeasurable or
	// on older rows. The P95 fields are the tail of each phase's samples - not the
	// maximum, which on a third of phases is a lost SYN retransmitted on the OS's
	// fixed 1s timer rather than anything about the link (see speedtest.loadStat).
	IdleMS          *float64 `json:"idle_ms,omitempty"`
	LoadedDownMS    *float64 `json:"loaded_down_ms,omitempty"`
	LoadedUpMS      *float64 `json:"loaded_up_ms,omitempty"`
	LoadedDownP95MS *float64 `json:"loaded_down_p95_ms,omitempty"`
	LoadedUpP95MS   *float64 `json:"loaded_up_p95_ms,omitempty"`
}

const speedCols = `ts, down_mbps, up_mbps, ping_ms, COALESCE(server,''), COALESCE(server_id,''),
	COALESCE(public_ipv4,''), COALESCE(public_ipv6,''), COALESCE(isp,''),
	COALESCE(isp_location,''), COALESCE(dns_ip,''), COALESCE(dns_provider,''), COALESCE(dns_location,''),
	packet_loss, healthy, jitter_ms, download_bytes, upload_bytes,
	COALESCE(cf_colo,''), COALESCE(exit_summary,''), COALESCE(run_trigger,''),
	idle_ms, loaded_down_ms, loaded_up_ms, loaded_down_p95_ms, loaded_up_p95_ms, COALESCE(engine,''),
	ping_best_ms, COALESCE(ip_family,''), COALESCE(udp_direction,''), failed,
	COALESCE(race_outcome,''), COALESCE(race_origins,''), COALESCE(race_winner_kind,''), COALESCE(race_winner_label,''),
	race_winner_ms, race_racers, race_winner_lat, race_winner_lon, round_ts, COALESCE(fail_stage,'')`

// speedNotFailed is the predicate every MEASUREMENT read of the speed table
// carries, and the reason the accounting rows recordFailedUsage writes cannot
// reach a chart, an average, a threshold verdict or a /metrics gauge: this
// package holds all the SQL, so filtering here covers every consumer at once.
// (The runs table is a LISTING, not a measurement read: it carries
// speedIsListed, which adds the failure records Record failed tests keeps and
// nothing else.)
//
// Written positively (NULL or 0 is a real run) rather than as `failed IS NOT 1`
// so an unreadable value - only reachable through a crafted import - HIDES the
// row instead of showing it. Hiding a row we cannot classify loses a
// measurement; showing it invents one, and inventing a 0 Mbps reading is the
// exact failure this column exists to prevent.
//
// The usage sums (SpeedDataUsage, SpeedDataUsageSince), Prune, DeleteSpeed and
// export/import deliberately do NOT carry it: the row exists for those.
const speedNotFailed = `(failed IS NULL OR failed = 0)`

// speedIsResult narrows speedNotFailed to the rows that are a run's OWN
// result. A Best-of round's kept losers (round_ts set, see SpeedSample.RoundTS)
// are measurements, but of servers the run did not choose, so the reads that
// decide something - the charts, the latest run, thresholds and baselines,
// the data estimate, the next race's origin - read this; the runs table, the
// counts behind its paging, and its CSV read speedIsListed (every row
// speedNotFailed passes, plus failure records) and show them.
const speedIsResult = speedNotFailed + ` AND round_ts IS NULL`

// speedIsAccounting is the same question from the other side - "is this row
// accounting rather than a measurement?" - DERIVED from the predicate above
// instead of restated, so the two can never drift apart. DeleteSpeed's usage
// cascade is the caller: it must reach exactly the rows every measurement read
// hides, no more (a row it wrongly matches is a reading destroyed with no way
// back) and no fewer (a row it misses bills bytes for a speedtest that is gone,
// and no listing shows it for the operator to remove by hand - a row carrying
// usage_run_ts is never a failure record, see speedIsFailure).
//
// Two spellings of this complement have already disagreed in this file: written
// `failed = 1`, the export's in-use check reported a hand-edited marker of 2 as
// "no row uses this column" while every read treated that row as accounting.
// (SpeedColumnsPastSchema4InUse still spells the complement out inline - it
// tests one column among several in a single SELECT - so it is the place to
// check first if this predicate ever changes.)
const speedIsAccounting = `NOT ` + speedNotFailed

// speedHasStage is the ONE spelling of "fail_stage is set": NULL and the empty
// string both mean unset (an empty one is reachable through an import). The
// listing and the export's in-use check share it so they cannot disagree about
// which rows carry one.
const speedHasStage = `(fail_stage IS NOT NULL AND fail_stage <> '')`

// speedIsFailure is the failure record Record failed tests leaves: a wholly
// failed run's usage row, carrying the stage it stopped at. Derived from
// speedIsAccounting so "non-zero marker" means what it means everywhere else,
// then narrowed three ways:
//   - typeof(failed) = 'integer', because this predicate SHOWS rows and
//     scanSpeed reads `failed` through NullInt64: a hand-edited text marker
//     must hide the row, not fail the whole page with a Scan error.
//   - speedHasStage, because the stage is what makes it a record. The same
//     row without one (the switch was off, or the user stopped the run) is
//     plain accounting and stays hidden, as it always was.
//   - no usage_run_ts and no round_ts, because an extra-usage row or a round
//     member is never a run of its own, even if something stamped a stage on it.
//
// The marker AND the stage, never the stage alone: a crafted stage on a
// measurement lists that row as the measurement it is (scanSpeed drops the
// stage), not as a failure.
const speedIsFailure = `(` + speedIsAccounting + ` AND typeof(failed) = 'integer' AND ` +
	speedHasStage + ` AND usage_run_ts IS NULL AND round_ts IS NULL)`

// speedIsListed is what the runs TABLE shows: every row speedNotFailed passes,
// plus failure records. A LISTING, never a reading - nothing that feeds a
// chart, an average, a verdict, a gauge or the digest may carry it, and
// TestSpeedFilterCoversEveryMeasurementRead allows it in exactly the functions
// behind the table (the page, its total, the chart-to-table jump, the CSV and
// the run-exists check), so the pager, the jump and the file cannot disagree
// about which rows exist.
const speedIsListed = `(` + speedNotFailed + ` OR ` + speedIsFailure + `)`

// speedWindow renders the half-open [since, until) ts filter the speed-history
// reads share, with its bound args. Shared so the two cannot answer the same
// question differently: SpeedHistoryBudget's `total` is the disclosure that a
// response was thinned, and it is only true if the counting pass and the reading
// pass agree on where the window ENDS.
//
// A zero `until` does not mean "no upper bound" - it means "up to the present",
// and the present is currentHorizon, never whatever the newest row on disk
// claims. An explicit end is honoured exactly as given (see currentHorizon).
func speedWindow(since, until time.Time) (string, []any) {
	upper := currentHorizon(time.Now().Unix())
	if !until.IsZero() {
		upper = until.Unix()
	}
	return ` AND ts >= ? AND ts < ?`, []any{since.Unix(), upper}
}

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

// extra receives any columns a caller appended after speedCols in its own
// query (SpeedHistoryDescFunc's win_reason), in the same order.
func scanSpeed(sc interface{ Scan(...any) error }, extra ...any) (SpeedSample, error) {
	var sp SpeedSample
	// Every float column scans through NullFloat64 and is sanitized: a non-finite
	// measurement (a sub-ms speedtest sample can divide to NaN/±Inf) lands in the
	// DB as NULL (NaN, coerced by the driver) or a real ±Inf. Scanning NULL into a
	// plain float64 errors, and a stored ±Inf later breaks json.Encode on
	// /api/speed and emits "+Inf" in CSV - either way one poisoned row would wedge
	// EVERY speed read. nzFinite/ptrFinite collapse both to 0 / nil.
	var down, up, ping sql.NullFloat64
	var ploss, jitter, idle, loadDown, loadUp, loadDownP95, loadUpP95, pingBest, raceMS, raceLat, raceLon sql.NullFloat64
	var healthy, downB, upB, failed, racers, roundTS sql.NullInt64
	var stage string
	err := sc.Scan(append([]any{&sp.TS, &down, &up, &ping, &sp.Server, &sp.ServerID,
		&sp.PublicIPv4, &sp.PublicIPv6, &sp.ISP, &sp.ISPLocation, &sp.DNSIP, &sp.DNSProvider, &sp.DNSLocation,
		&ploss, &healthy, &jitter, &downB, &upB, &sp.CFColo, &sp.ExitSummary, &sp.Trigger,
		&idle, &loadDown, &loadUp, &loadDownP95, &loadUpP95, &sp.Engine, &pingBest,
		&sp.IPFamily, &sp.UDPDirection, &failed,
		&sp.RaceOutcome, &sp.RaceOrigins, &sp.RaceWinnerKind, &sp.RaceWinnerLabel, &raceMS, &racers, &raceLat, &raceLon, &roundTS,
		&stage}, extra...)...)
	if roundTS.Valid {
		v := roundTS.Int64
		sp.RoundTS = &v
	}
	sp.RaceWinnerMS = ptrFinite(raceMS)
	sp.RaceWinnerLat, sp.RaceWinnerLon = ptrFinite(raceLat), ptrFinite(raceLon)
	if racers.Valid {
		v := racers.Int64
		sp.RaceRacers = &v
	}
	sp.DownMbps, sp.UpMbps, sp.PingMS = nzFinite(down), nzFinite(up), nzFinite(ping)
	sp.IdleMS = ptrFinite(idle)
	sp.LoadedDownMS = ptrFinite(loadDown)
	sp.LoadedUpMS = ptrFinite(loadUp)
	sp.LoadedDownP95MS = ptrFinite(loadDownP95)
	sp.LoadedUpP95MS = ptrFinite(loadUpP95)
	sp.PacketLoss = ptrFinite(ploss)
	sp.JitterMS = ptrFinite(jitter)
	sp.PingBestMS = ptrFinite(pingBest)
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
	// Non-zero, not == 1: the meaning speedNotFailed and the import clamp give
	// the marker. A listed failure record with a hand-edited marker of 2 must
	// still render as a failed test, never as a 0 Mbps measurement.
	sp.Failed = failed.Valid && failed.Int64 != 0
	// A stage means something only beside the marker. A crafted one on a
	// measurement never reaches the JSON or the CSV, so that row cannot be
	// drawn as a failure it was not.
	if sp.Failed {
		sp.FailStage = stage
	}
	return sp, err
}

// nullStr stores an empty string as NULL, so a column that has "unrecorded"
// as its only meaning of empty (the race_* text columns) has one spelling of
// it in the table, the same one every row written before the column existed
// carries.
func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// InsertSpeed records a speedtest result and the connection it ran in.
// maxSpeedBytesPerRun bounds a single speedtest's recorded byte count. The cap
// exists to protect SpeedDataUsage, which SUMs download+upload across every
// retained run inside SQLite: an int64 overflow there does not skew the total,
// it raises "integer overflow" and hard-breaks the query, and the data-usage
// endpoint stays broken until the offending rows are found and removed.
//
// So the cap has to be derived from that sum rather than eyeballed. It was 1 PiB
// (2^50), justified in this comment as "far below where a SUM over the whole
// speed table could overflow int64" - which was simply false arithmetic: two
// directions at 2^50 is 2^51 per row, so int64 runs out after 4,095 rows, and a
// default install keeps 8,760 of them (hourly runs, 365-day speed retention). A
// crafted backup needed only a few thousand rows to wedge the usage query.
//
// 4 TiB per direction is the largest power of two that keeps a WORST-CASE
// history - once-a-minute runs for a leap year, ~527k rows, far denser than any
// real schedule - inside int64 with room to spare, while still sitting ~16,000x
// above what a real speedtest moves. Enforced on the way in by clampSpeedBytes,
// for both live inserts and imports; TestSpeedByteCapCannotOverflowTheUsageSum
// re-derives the bound so changing either side fails rather than silently
// re-opening this.
const maxSpeedBytesPerRun = 1 << 42

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

// InsertSpeed stores a speed row; see InsertSpeedTS for the second it lands on.
func (s *Store) InsertSpeed(ctx context.Context, sp SpeedSample) error {
	_, err := s.InsertSpeedTS(ctx, sp)
	return err
}

// InsertSpeedTS stores a speed row and returns the second it actually landed
// on. ts is a speed row's IDENTITY - DeleteSpeed's key (whose first statement
// removes every unreferenced row on that second), the backup merge's key
// (keep-first: two rows sharing a second means a restore silently drops one),
// and the UI's run handle. Runs are serialized, but the accounting rows are
// not clock-spaced: a wholly-failed or aborted run stamps time.Now() and can
// land on a measurement's second, an extra-usage row's +1 sentinel can land
// on a neighbouring run's, and a stepped clock can re-visit an occupied
// second. So uniqueness is enforced where the row is born: the insert walks
// forward to the first free second and reports which one it took, so a caller
// can key dependent rows (usage_run_ts, speed_servers.run_ts) to the second
// the row really sits on. The walk is bounded by the length of the contiguous
// occupied stretch starting at the requested second - a handful at worst in
// real data, each probe an indexed point lookup - and all speed writers are
// serialized (the scheduler's single flight), which is what makes
// check-then-insert inside one transaction sufficient.
func (s *Store) InsertSpeedTS(ctx context.Context, sp SpeedSample) (int64, error) {
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
	// NULL rather than 0 for a real run: identical to every row written before
	// the column existed, so "not an accounting row" has ONE representation in
	// the table instead of two.
	var failed any
	if sp.Failed {
		failed = int64(1)
	}
	// A stage is only ever stored beside the marker: on its own it would claim
	// a failure for a row every read treats as a measurement.
	stage := nullStr(sp.FailStage)
	if !sp.Failed {
		stage = nil
	}
	// ONE autocommit statement, deliberately: a deferred read-then-write
	// transaction here fails with SQLITE_BUSY_SNAPSHOT the moment another
	// pooled connection commits between the read and the write - the daemon
	// commits probe samples every few seconds, busy_timeout does NOT absorb a
	// snapshot upgrade, and this exact shape already broke settings saves once
	// (see SetSettingsDiff) and the outage repair once (see the pre-computed
	// list above deleteResolvedOutage). Measured here: a hammer test lost a
	// measurement within ~5-50 inserts. A single INSERT..SELECT computes the
	// first free second and inserts it atomically - no window even against a
	// concurrent import - and lock acquisition is the plain kind busy_timeout
	// has always covered. The landed second is read back by rowid.
	//
	// The free-second expression: the requested second if nothing occupies it,
	// else the smallest successor of an occupied second >= the request whose
	// successor is free (the first gap after the contiguous occupied stretch).
	//
	// The rounds taken while the test ran are stored before its result.
	s.SaveBuffered(SaveOrder)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO speed (ts, down_mbps, up_mbps, ping_ms, server, server_id,
			public_ipv4, public_ipv6, isp, isp_location, dns_ip, dns_provider, dns_location,
			packet_loss, healthy, jitter_ms, download_bytes, upload_bytes, cf_colo, exit_summary, run_trigger,
			idle_ms, loaded_down_ms, loaded_up_ms, loaded_down_p95_ms, loaded_up_p95_ms, engine,
			ping_best_ms, ip_family, udp_direction, failed, usage_run_ts,
			race_outcome, race_origins, race_winner_kind, race_winner_label, race_winner_ms, race_racers,
			race_winner_lat, race_winner_lon, round_ts, fail_stage)
		 SELECT COALESCE((SELECT MIN(t) FROM (
					SELECT ?1 AS t WHERE NOT EXISTS (SELECT 1 FROM speed WHERE ts = ?1)
					UNION ALL
					SELECT ts + 1 FROM speed WHERE ts >= ?1
					  AND NOT EXISTS (SELECT 1 FROM speed s2 WHERE s2.ts = speed.ts + 1)
				)), ?1),
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?`,
		sp.TS, sp.DownMbps, sp.UpMbps, sp.PingMS, sp.Server, sp.ServerID,
		sp.PublicIPv4, sp.PublicIPv6, sp.ISP, sp.ISPLocation, sp.DNSIP, sp.DNSProvider, sp.DNSLocation,
		ptrArg(sp.PacketLoss), healthy, ptrArg(sp.JitterMS), ptrArg(downBytes), ptrArg(upBytes), sp.CFColo, sp.ExitSummary, sp.Trigger,
		ptrArg(sp.IdleMS), ptrArg(sp.LoadedDownMS), ptrArg(sp.LoadedUpMS), ptrArg(sp.LoadedDownP95MS), ptrArg(sp.LoadedUpP95MS), sp.Engine,
		ptrArg(sp.PingBestMS), sp.IPFamily, sp.UDPDirection, failed, ptrArg(sp.UsageRunTS),
		nullStr(sp.RaceOutcome), nullStr(sp.RaceOrigins), nullStr(sp.RaceWinnerKind), nullStr(sp.RaceWinnerLabel),
		ptrArg(sp.RaceWinnerMS), ptrArg(sp.RaceRacers), ptrArg(sp.RaceWinnerLat), ptrArg(sp.RaceWinnerLon), ptrArg(sp.RoundTS),
		stage)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	rowid, err := res.LastInsertId()
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	var ts int64
	if err := s.db.QueryRowContext(ctx, `SELECT ts FROM speed WHERE rowid = ?`, rowid).Scan(&ts); err != nil {
		recordDBErr(err)
		return 0, err
	}
	return ts, nil
}

// SpeedServerRow is one candidate's row of a run's server-selection report
// (table speed_servers): who was considered, what was measured, how it scored,
// and why the winner won. Written once per run right after the speed row;
// exists so a surprising winner is explainable from the DB alone at any log
// level. Pointer fields are NULL when unmeasured, mirroring SpeedSample.
type SpeedServerRow struct {
	RunTS      int64    `json:"run_ts"` // joins speed.ts
	ServerID   string   `json:"server_id"`
	Server     string   `json:"server"`
	DistanceKM float64  `json:"distance_km"`
	RankOrder  int64    `json:"rank_order"`
	RankPingMS *float64 `json:"rank_ping_ms,omitempty"`
	Selected   bool     `json:"selected"`
	Measured   bool     `json:"measured"`
	Err        string   `json:"error,omitempty"`

	DownMbps      float64  `json:"down_mbps"`
	UpMbps        float64  `json:"up_mbps"`
	PingMS        float64  `json:"ping_ms"`
	PingBestMS    *float64 `json:"ping_best_ms,omitempty"`
	JitterMS      *float64 `json:"jitter_ms,omitempty"`
	DownloadBytes int64    `json:"download_bytes"`
	UploadBytes   int64    `json:"upload_bytes"`

	CapacityMbps         float64 `json:"capacity_mbps"`
	BelievedCapacityMbps float64 `json:"believed_capacity_mbps"`
	CappedDirection      string  `json:"capped_direction,omitempty"`
	Score                float64 `json:"score"`
	Winner               bool    `json:"winner"`
	WinReason            string  `json:"win_reason,omitempty"`
}

const speedServerCols = `run_ts, server_id, server, distance_km, rank_order, rank_ping_ms,
	selected, measured, error, down_mbps, up_mbps, ping_ms, ping_best_ms, jitter_ms,
	download_bytes, upload_bytes, capacity_mbps, believed_capacity_mbps, capped_direction,
	score, winner, win_reason`

// InsertSpeedServers persists one run's selection report. One transaction so a
// run's rows land all-or-nothing - a partial report would read as "the other
// candidates were never considered", which is exactly the ambiguity the table
// exists to remove.
func (s *Store) InsertSpeedServers(ctx context.Context, rows []SpeedServerRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.SaveBuffered(SaveOrder) // before the transaction opens, never inside it
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordDBErr(err)
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO speed_servers (`+speedServerCols+`)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.RunTS, r.ServerID, r.Server, r.DistanceKM, r.RankOrder, ptrArg(r.RankPingMS),
			util.B2I(r.Selected), util.B2I(r.Measured), r.Err,
			r.DownMbps, r.UpMbps, r.PingMS, ptrArg(r.PingBestMS), ptrArg(r.JitterMS),
			r.DownloadBytes, r.UploadBytes, r.CapacityMbps, r.BelievedCapacityMbps, r.CappedDirection,
			r.Score, util.B2I(r.Winner), r.WinReason); err != nil {
			recordDBErr(err)
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return err
	}
	return nil
}

// SpeedRunExists reports whether a speed row with this exact ts exists - the
// read API's "was there ever such a run" check, distinct from "the run has no
// selection report" (which is normal for pre-feature and iperf3 history).
func (s *Store) SpeedRunExists(ctx context.Context, ts int64) (bool, error) {
	var n int
	// Plain accounting rows are not runs: /api/speed/runs/servers must 404 for
	// one, exactly as it does for a ts that never existed - and it can only be
	// asked about a ts the paged/charted history handed out, which those rows
	// are not in. A failure record IS in the runs table, so it is a run here: a
	// run with no selection report, like an iperf3 one (a failed run writes
	// none), which answers 200 with an empty list.
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE `+speedIsListed+` AND ts = ?`, ts).Scan(&n)
	if err != nil {
		recordDBErr(err)
		return false, err
	}
	return n > 0, nil
}

// SpeedServers returns one run's selection report rows in rank order (the
// unranked pin row first). Empty is a normal answer, not an error: runs
// restored from pre-feature backups, iperf3 runs, and runs whose report
// insert failed all have a speed row with no companions.
func (s *Store) SpeedServers(ctx context.Context, runTS int64) ([]SpeedServerRow, error) {
	return s.speedServerRows(ctx,
		`SELECT `+speedServerCols+` FROM speed_servers WHERE run_ts = ? ORDER BY rank_order`, runTS)
}

// LastSpeedWinners returns the winners of the most recent runs that have a
// selection report, newest first, at most limit of them. It is the incumbent
// lookup (see speedtest.Ookla.IncumbentFn): the caller skips the pinned ones
// itself, since which win reasons name a pin is the speedtest package's
// vocabulary and this package cannot import it. Walks idx_speed_servers_ts
// backwards and stops at the limit, so it costs a handful of index steps, not
// a scan of the history.
func (s *Store) LastSpeedWinners(ctx context.Context, limit int) ([]SpeedServerRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.speedServerRows(ctx,
		`SELECT `+speedServerCols+` FROM speed_servers WHERE winner = 1 ORDER BY run_ts DESC LIMIT ?`, limit)
}

// RecentServerScores returns one server's last n measured scores (the
// selection report's ping-discounted capacity, see speedtest.roundScore),
// newest first, for the challenge verdict: a rival takes the incumbent's seat
// only by beating the median of these. The record is per test DIRECTION - a
// score is download-only, upload-only or a weighted mean of both depending on
// how the run was configured, so only same-direction rows are comparable; the
// row's own readings say which it was. Unmeasured rows and unusable scores
// (NULL, zero - a failed direction - or non-finite) are skipped BEFORE the
// window of n is counted; a server with no history yields none.
func (s *Store) RecentServerScores(ctx context.Context, serverID, dir string, n int) ([]float64, error) {
	if serverID == "" || n <= 0 {
		return nil, nil
	}
	sameDir := `down_mbps > 0 AND up_mbps > 0`
	switch dir {
	case "down":
		sameDir = `down_mbps > 0 AND up_mbps = 0`
	case "up":
		sameDir = `up_mbps > 0 AND down_mbps = 0`
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT score FROM speed_servers WHERE server_id = ? AND measured = 1 AND score > 0 AND `+sameDir+
			` ORDER BY run_ts DESC LIMIT ?`, serverID, n)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	var out []float64
	for rows.Next() {
		var sc sql.NullFloat64
		if err := rows.Scan(&sc); err != nil {
			recordDBErr(err)
			return nil, err
		}
		if sc.Valid && !math.IsNaN(sc.Float64) && !math.IsInf(sc.Float64, 0) && sc.Float64 > 0 {
			out = append(out, sc.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return out, nil
}

// WinReasonsFor returns the winner row's win_reason for each of the given
// runs (keyed by run ts), for the runs table: a run absent from the map has no
// selection report (pre-feature history, an import, an iperf3 run). One query
// for the whole page rather than one per row.
func (s *Store) WinReasonsFor(ctx context.Context, runTS []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(runTS) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(runTS))
	marks := make([]string, 0, len(runTS))
	for _, ts := range runTS {
		args = append(args, ts)
		marks = append(marks, "?")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_ts, COALESCE(win_reason,'') FROM speed_servers WHERE winner = 1 AND run_ts IN (`+strings.Join(marks, ",")+`)`, args...)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var reason string
		if err := rows.Scan(&ts, &reason); err != nil {
			recordDBErr(err)
			return nil, err
		}
		if reason != "" {
			out[ts] = reason
		}
	}
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return out, nil
}

// LastSpeedWinnersExcluding is LastSpeedWinners without the rows whose
// win_reason is one of reasons, so a caller counting AUTOMATIC winners is not
// starved by a long pinned stretch: the window is measured in the rows that
// count, not in rows. The vocabulary is the speedtest package's, which this
// package cannot import, so the caller names the reasons.
func (s *Store) LastSpeedWinnersExcluding(ctx context.Context, limit int, reasons ...string) ([]SpeedServerRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	if len(reasons) == 0 {
		return s.LastSpeedWinners(ctx, limit)
	}
	args := make([]any, 0, len(reasons)+1)
	marks := make([]string, 0, len(reasons))
	for _, r := range reasons {
		args = append(args, r)
		marks = append(marks, "?")
	}
	args = append(args, limit)
	return s.speedServerRows(ctx,
		`SELECT `+speedServerCols+` FROM speed_servers WHERE winner = 1 AND COALESCE(win_reason,'') NOT IN (`+strings.Join(marks, ",")+`)
		 ORDER BY run_ts DESC LIMIT ?`, args...)
}

// RecentRankPings is the median of each server's last `runs` ranking pings
// (rank_ping_ms, unanswered ones excluded), keyed by server ID, for the IDs
// asked about. A server never ranked - or never answered - is absent. It is
// what the picker shows for a kept server before anyone re-measures it: what
// the daemon's own runs measured, which costs no network at all. The window
// is per server, not per run, so a server that is ranked only when its city
// wins still gets its own last `runs` answers.
//
// One bounded query per ID, not one IN query for all: the window has to be
// per server, and a single ordered query can only apply it after every
// matching row has crossed the driver - measured at ~100k rows and 55 ms per
// call at a year of hourly runs. Each query here is a seek on
// idx_speed_servers_server plus LIMIT: a fraction of a millisecond, however
// long the history (and the index is what keeps a never-ranked kept server
// from walking the table to prove it).
func (s *Store) RecentRankPings(ctx context.Context, ids []string, runs int) (map[string]float64, error) {
	out := map[string]float64{}
	if len(ids) == 0 || runs <= 0 {
		return out, nil
	}
	for _, id := range ids {
		v, err := s.recentRankPingsFor(ctx, id, runs)
		if err != nil {
			return nil, err
		}
		if n := len(v); n > 0 {
			sort.Float64s(v)
			if n%2 == 1 {
				out[id] = v[n/2]
			} else {
				out[id] = (v[n/2-1] + v[n/2]) / 2
			}
		}
	}
	return out, nil
}

// recentRankPingsFor is one server's last `runs` answered ranking pings,
// newest first (see RecentRankPings).
func (s *Store) recentRankPingsFor(ctx context.Context, id string, runs int) ([]float64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT rank_ping_ms FROM speed_servers
		 WHERE server_id = ? AND rank_ping_ms IS NOT NULL AND rank_ping_ms > 0
		 ORDER BY run_ts DESC LIMIT ?`, id, runs)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	var v []float64
	for rows.Next() {
		var ms sql.NullFloat64
		if err := rows.Scan(&ms); err != nil {
			recordDBErr(err)
			return nil, err
		}
		if ms.Valid && !math.IsNaN(ms.Float64) && !math.IsInf(ms.Float64, 0) && ms.Float64 > 0 {
			v = append(v, ms.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return v, nil
}

// speedServerRows runs one speed_servers query and scans its rows.
func (s *Store) speedServerRows(ctx context.Context, query string, args ...any) ([]SpeedServerRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	var out []SpeedServerRow
	for rows.Next() {
		var r SpeedServerRow
		// Floats through NullFloat64 + nzFinite/ptrFinite, TEXT through
		// NullString, for the same reason scanSpeed does it: one NULL or ±Inf
		// row - and an imported backup CAN carry NULL in any nullable column -
		// must not wedge every read of the run's report. The speed table
		// learned this the hard way (see its exportTables notNull comment).
		var serverID, server, errText, cappedDir, winReason sql.NullString
		var dist, rankPing, down, up, ping, pingBest, jitter, capacity, believed, score sql.NullFloat64
		var rank, downB, upB, sel, meas, win sql.NullInt64
		if err := rows.Scan(&r.RunTS, &serverID, &server, &dist, &rank, &rankPing,
			&sel, &meas, &errText, &down, &up, &ping, &pingBest, &jitter,
			&downB, &upB, &capacity, &believed, &cappedDir,
			&score, &win, &winReason); err != nil {
			recordDBErr(err)
			return nil, err
		}
		r.ServerID, r.Server, r.Err = serverID.String, server.String, errText.String
		r.CappedDirection, r.WinReason = cappedDir.String, winReason.String
		r.DistanceKM, r.DownMbps, r.UpMbps, r.PingMS = nzFinite(dist), nzFinite(down), nzFinite(up), nzFinite(ping)
		r.CapacityMbps, r.BelievedCapacityMbps, r.Score = nzFinite(capacity), nzFinite(believed), nzFinite(score)
		r.RankPingMS, r.PingBestMS, r.JitterMS = ptrFinite(rankPing), ptrFinite(pingBest), ptrFinite(jitter)
		r.RankOrder, r.DownloadBytes, r.UploadBytes = rank.Int64, downB.Int64, upB.Int64
		r.Selected, r.Measured, r.Winner = sel.Int64 != 0, meas.Int64 != 0, win.Int64 != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		recordDBErr(err)
		return nil, err
	}
	return out, nil
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
	// The SAME interval model UptimeSince uses, not a second copy of it in SQL.
	// This query used to place every outage at a contiguous [down, down+duration_s)
	// while uptime placed it at its real span minus pauses, so a pause inside an
	// outage made the digest print an outage total that contradicted the percentage
	// printed beside it - "Uptime 85.71% - 1 outage - 0s down" - in one line.
	outages, err := s.completedOutagesSince(ctx, since, nowU)
	if err != nil {
		return 0, 0, err
	}
	union, err := s.pauseUnionFor(ctx, outages, since, nowU)
	if err != nil {
		return 0, 0, err
	}
	// The COUNT is of outages that RESOLVED in the window (keyed on the 'up'), which
	// is a different question from how much of their downtime falls inside it - an
	// outage that began before `since` still resolved within it.
	var resolved int
	for _, o := range outages {
		if o.end > since {
			resolved++
		}
	}
	c := sql.NullInt64{Int64: int64(resolved), Valid: true}
	d := sql.NullInt64{Int64: observedDowntimeIn(outages, union, since, nowU), Valid: true}
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
	// Bounded at currentHorizon exactly as UptimeSince's twin is, and it has to be
	// the same bound: these two print the outage count and the uptime% in one
	// sentence, so a future row hiding the trailing outage from one of them is how
	// that sentence contradicts itself.
	var lastType string
	var lastTS int64
	switch e := s.db.QueryRowContext(ctx,
		`SELECT type, ts FROM events WHERE ts <= ? AND type IN ('down','up')
		 ORDER BY ts DESC, CASE type WHEN 'up' THEN 0 ELSE 1 END LIMIT 1`,
		currentHorizon(nowU)).Scan(&lastType, &lastTS); {
	case e == sql.ErrNoRows: // no events → nothing to reconcile
	case e != nil:
		return 0, 0, e
	default:
		if lastType == "down" {
			if rec, ok, e := s.firstQuorumRecovery(ctx, lastTS, nowU); e != nil {
				return 0, 0, e
			} else if ok && rec > since {
				// Count the outage on the same evidence DowntimeByDay and
				// orphanGapDowntime use - a quorum recovery landed in the window, so an
				// outage began - and INDEPENDENTLY of how much of it was observed. A
				// stretch nobody watched end to end is still an outage we saw begin and
				// then lost sight of; suppressing the count would hide an event the
				// heatmap still draws a dot for, trading this function's old
				// disagreement with the uptime% for a new one with the heatmap. Only
				// the DURATION is pause-excluded, below.
				gapOutages++
				start := lastTS
				if start < since {
					start = since
				}
				if rec > start {
					// Subtract the pause overlap, exactly as UptimeSince's trailing-down
					// branch and DowntimeByDay's prorate do for this same stretch: the
					// wall gap [start, rec] can be mostly unobserved (the process was
					// down, or monitoring was switched off mid-outage) and unobserved
					// time is neither up nor down. Without this the digest sent an
					// arithmetically impossible sentence - "Uptime 100.00% · 1 outage ·
					// 168h 0m down" - because the uptime% beside it divides observed
					// downtime by OBSERVED time while this counted raw wall seconds; the
					// operator then opens the dashboard to a spotless heatmap. Keeping
					// the digest consistent with the uptime% beside it is this
					// function's whole stated purpose.
					paused, e := s.pausedOverlap(ctx, start, rec)
					if e != nil {
						return 0, 0, e
					}
					// Only OBSERVED seconds are downtime. A trailing stretch that was
					// unobserved end to end contributes 0 here and still counts above:
					// "1 outage · 0m down" is the honest reading of an outage we watched
					// begin and could not time, and it matches what the heatmap shows.
					if dt := rec - start - paused; dt > 0 {
						gap += dt
					}
				}
			}
		}
	}
	return int(c.Int64) + gapOutages, int(d.Int64) + int(gap), nil
}

// TableCounts reports the row count of each data table.
func (s *Store) TableCounts(ctx context.Context) (map[string]int64, error) {
	s.SaveBuffered(SaveRead)
	out := map[string]int64{}
	// Every table Clear/import/prune can touch, so a test asserting "this dataset
	// is empty now" cannot pass by looking at a table the operation never had.
	// dns and pauses were both absent, and pauses is the one that matters: it is
	// the uptime denominator, so a row left behind changes the numbers while being
	// invisible to any count.
	for _, t := range []string{"samples", "dns", "speed_spans", "speed", "speed_servers", "events", "pauses", "pauses_quarantine", "settings"} {
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
//
// Bounded at currentHorizon for the same reason LatestSpeed is: this is the
// fallback shown precisely when the live lookup fails, so a future-dated import
// winning it would replace the operator's last KNOWN ISP/IP with one the link has
// never actually had, at the one moment nothing else can contradict it.
func (s *Store) LatestConnInfo(ctx context.Context) (*SpeedSample, error) {
	sp, err := scanSpeed(s.db.QueryRowContext(ctx,
		`SELECT `+speedCols+` FROM speed
		 WHERE `+speedIsResult+` AND ts <= ?
		   AND (COALESCE(isp,'') <> '' OR COALESCE(public_ipv4,'') <> '')
		 ORDER BY ts DESC LIMIT 1`, currentHorizon(time.Now().Unix())))
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
	// ts <= currentHorizon: a future-dated import must not win "latest" and pin the
	// speed pills/metrics on a not-yet-real run.
	sp, err := scanSpeed(s.db.QueryRowContext(ctx,
		`SELECT `+speedCols+` FROM speed WHERE `+speedIsResult+` AND ts <= ? ORDER BY ts DESC LIMIT 1`,
		currentHorizon(time.Now().Unix())))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		recordDBErr(err) // status/metrics swallow this; surface it on the db.* health counters
		return nil, err
	}
	return &sp, nil
}

// SpeedHistory returns every speedtest from the given time up to the present,
// oldest first, with no downsampling - the digest reads each run, so it wants
// them all. "The present" is speedWindow's, not the newest row's: the digest
// summarises what has happened, and a run stamped ahead of the clock has not.
func (s *Store) SpeedHistory(ctx context.Context, since time.Time) ([]SpeedSample, error) {
	return s.SpeedHistoryRange(ctx, since, time.Time{}, 1)
}

// SpeedHistoryBudget returns at most budget speedtests from [since, until),
// oldest first, by selecting every k-th RUN rather than one run per k SECONDS.
//
// Thinning by rank instead of by time is what makes the point count depend on
// how many runs there are rather than on how they are spread: a time bucket has
// to be sized from some span, and any span - the requested one, or the populated
// extent - collapses to a handful of points as soon as the runs inside it are
// clustered. Rank has no such failure mode; the answer is exactly
// min(rows-in-window, budget), for every window, on every distribution.
//
// Two passes, both cheap. The first reads ts alone - idx_speed_ts drives the
// range, with a row visit only to read the accounting marker - and yields both
// the count and the timestamps to keep, so `total` counts what a caller can
// actually be handed rather than every row in the window (the two differing is
// what tells a client its history was thinned, so it must not include rows no
// pass could ever return). The second re-reads only the chosen rows whole,
// carrying the same filter so a marker written between the passes cannot slip a
// non-measurement into the result. The kept rows are REAL
// rows and the newest run in the window is always among them - the dashboard's
// stat tiles read the final point, and clicking a point looks its ts up with
// SpeedRunOffset, so a synthetic timestamp would scroll the runs table to a row
// that does not exist.
//
// total is how many runs the window actually holds, which the first pass already
// counted. It is returned rather than discarded so the API can tell a client that
// what it received was thinned: a bare array of a thousand points looks exactly
// like a complete history of a thousand runs, and a consumer summing bytes or
// counting outages off it would be quietly wrong.
func (s *Store) SpeedHistoryBudget(ctx context.Context, since, until time.Time, budget int) (out []SpeedSample, total int, err error) {
	if budget < 1 {
		budget = 1
	}
	clause, winArgs := speedWindow(since, until)
	// speedNotFailed, not speedIsResult: this is the chart's read, and a
	// Best-of round's kept losers (round_ts) are plotted there like any other
	// run - and are rows to the thinning like any other.
	win := ` WHERE ` + speedNotFailed + clause
	// Pass 1: timestamps only, over the ts index.
	rows, err := s.db.QueryContext(ctx, `SELECT ts FROM speed`+win+` ORDER BY ts`, winArgs...)
	if err != nil {
		return nil, 0, err
	}
	var all []int64
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			rows.Close()
			return nil, 0, err
		}
		// DISTINCT seconds, because that is what pass 2 can return: it collapses
		// same-second rows with GROUP BY. Counting them separately here made
		// `total` one higher than the rows delivered, and the caller reads that
		// gap as "this window was thinned" - so a single legacy collision
		// anywhere in range put a dashed "sampled" border and a mean-of caveat
		// on every average, in a window that was never thinned at all. The query
		// is ORDER BY ts, so neighbours are the only possible duplicates.
		if len(all) > 0 && all[len(all)-1] == ts {
			continue
		}
		all = append(all, ts)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if len(all) == 0 {
		return nil, 0, nil
	}
	keep := all
	if len(all) > budget {
		keep = make([]int64, 0, budget)
		for i := 0; i < budget; i++ {
			keep = append(keep, all[int(int64(i)*int64(len(all))/int64(budget))])
		}
		// Pin the newest run as the final point regardless of where the stride
		// landed: the "last in range" caption and the stat tiles read it.
		keep[len(keep)-1] = all[len(all)-1]
	}
	// Pass 2: the chosen rows, whole. budget is far below SQLite's 32766-variable
	// ceiling. GROUP BY ts collapses the duplicate that two runs sharing a second
	// would otherwise produce, so the response can never exceed budget.
	ph := strings.TrimSuffix(strings.Repeat("?,", len(keep)), ",")
	args := make([]any, 0, len(keep))
	for _, ts := range keep {
		args = append(args, ts)
	}
	rows2, err := s.db.QueryContext(ctx,
		`SELECT `+speedCols+` FROM speed WHERE `+speedNotFailed+` AND ts IN (`+ph+`) GROUP BY ts ORDER BY ts`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows2.Close()
	for rows2.Next() {
		sp, err := scanSpeed(rows2)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sp)
	}
	return out, len(all), rows2.Err()
}

// SpeedRunCount counts the RUNS in a window - rows that are a run in their own
// right, not the other servers a Best-of round measured. The chart's sampling
// caveat needs it: it compares the runs it charted against the runs in range,
// and the total that drives X-Sampled counts every row, members included. With
// the shipped Best-of settings roughly two thirds of rows are members, so
// comparing one against the other understated the mean's coverage about
// threefold - on the very number the caveat exists to qualify.
func (s *Store) SpeedRunCount(ctx context.Context, since, until time.Time) (int, error) {
	q := `SELECT COUNT(*) FROM speed WHERE ` + speedNotFailed + ` AND round_ts IS NULL AND ts >= ?`
	args := []any{since.Unix()}
	if !until.IsZero() {
		q += ` AND ts < ?`
		args = append(args, until.Unix())
	}
	var n int
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		recordDBErr(err)
		return 0, err
	}
	return n, nil
}

// SpeedHistoryRange returns speedtests in [since, until), oldest first. A zero
// until means "up to the present" (see speedWindow), which is what every caller
// outside the chart wants. The interval is half-open so a run stamped exactly on
// a boundary belongs to one side only - speedtests land on schedule, so a run at
// local midnight is ordinary, and a closed bound would show it in both
// neighbouring day ranges.
//
// bucketSec downsamples the result the way Series does, and for the same reason:
// a chart window is wide but a canvas is not, so the response has to be bounded
// by the number of points that can be drawn rather than by how long the daemon
// has been recording. A bucket of 1 (or less) is off and every row is returned;
// a wider bucket keeps ONE run per bucketSec-wide window, which caps a year of
// history at the same size as a week of it.
//
// The kept run is the NEWEST in its bucket, and it is a REAL row, not a computed
// average. Both matter downstream: the dashboard's stat tiles and "last in
// range" caption read the final point as the most recent run, and clicking a
// point looks its ts up in the runs table (SpeedRunOffset) - a synthetic
// bucket-midpoint timestamp would scroll that table to a row that does not
// exist. Picking the newest per bucket makes the final point the newest run by
// construction, so neither behaviour needs a special case.
func (s *Store) SpeedHistoryRange(ctx context.Context, since, until time.Time, bucketSec int) ([]SpeedSample, error) {
	// The filter rides `win`, so both the outer select and the bucket subquery
	// carry it: a bucket whose newest row is an accounting row must yield the
	// newest MEASUREMENT in that bucket, not an empty bucket.
	clause, winArgs := speedWindow(since, until)
	// Results only (speedIsResult): the digest summarises the runs, and a
	// kept loser is not one; the chart, which does show them, reads
	// SpeedHistoryBudget.
	win := ` WHERE ` + speedIsResult + clause
	q := `SELECT ` + speedCols + ` FROM speed` + win
	args := append([]any(nil), winArgs...)
	if bucketSec > 1 {
		// Two passes over idx_speed_ts rather than one aggregate with bare columns:
		// SQLite would let `SELECT ts, down_mbps ... GROUP BY ts/?` pick the other
		// columns off the MAX(ts) row, but that is a SQLite-specific extension and
		// silently returns an arbitrary row if the aggregate is ever reshaped. The
		// subquery names the timestamps and the outer select re-reads those rows
		// whole, which is plain SQL and can't drift.
		q += ` AND ts IN (SELECT MAX(ts) FROM speed` + win + ` GROUP BY ts / ?)`
		args = append(args, winArgs...)
		args = append(args, bucketSec)
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
// called per row; returning an error stops the scan and propagates it. The CSV
// is the runs table as a file, so it lists the same rows (speedIsListed),
// failure records included.
func (s *Store) SpeedHistoryDescFunc(ctx context.Context, fn func(SpeedSample) error) error {
	// The scalar subquery resolves each row's win_reason from its selection
	// report (winner row of speed_servers, walked via idx_speed_servers_ts) so
	// the CSV can carry the same value the JSON listing shows - the streaming
	// shape leaves nowhere to batch a WinReasonsFor page the way the listing
	// does. LIMIT 1 because a crafted import can hold two winner rows for one
	// run, and a scalar subquery must not turn that into a duplicated CSV row.
	rows, err := s.db.QueryContext(ctx, `SELECT `+speedCols+`,
		COALESCE((SELECT ss.win_reason FROM speed_servers ss WHERE ss.run_ts = speed.ts AND ss.winner = 1 LIMIT 1), '')
		FROM speed WHERE `+speedIsListed+` ORDER BY ts DESC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var winReason string
		sp, err := scanSpeed(rows, &winReason)
		if err != nil {
			return err
		}
		sp.WinReason = winReason
		if err := fn(sp); err != nil {
			return err
		}
	}
	return rows.Err()
}

// SpeedCount returns the number of recorded MEASUREMENTS - "has anything ever
// been measured here?", which newPriorDataFn asks. A failure record is not
// one, however many the runs table lists; the table pages against
// SpeedListedCount instead.
func (s *Store) SpeedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE `+speedNotFailed).Scan(&n)
	return n, err
}

// SpeedListedCount is the total the runs TABLE pages against. It must carry
// the same predicate as SpeedRuns and SpeedRunOffset, or the pager and the
// jump disagree about which rows exist. SpeedCount is a different question
// ("has anything been measured", newPriorDataFn) that a failure record must
// not answer yes to.
func (s *Store) SpeedListedCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE `+speedIsListed).Scan(&n)
	return n, err
}

// SpeedRunOffset returns the zero-based position of the run with the given
// timestamp within the runs table's newest-first ordering (i.e. how many
// listed rows are newer, failure records included), which the UI divides by
// page size to jump straight to a run's row when a chart point is clicked.
func (s *Store) SpeedRunOffset(ctx context.Context, ts int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM speed WHERE `+speedIsListed+` AND ts > ?`, ts).Scan(&n)
	return n, err
}

// DeleteSpeed removes a single speedtest run by its timestamp - the run's
// identity throughout the UI/API (the same key SpeedRunOffset and the
// chart<->table link use). Returns rows removed: 0 when no run matched (already
// gone), which the caller treats as an idempotent no-op, not an error. ts is
// GUARANTEED unique per row: InsertSpeedTS allocates each row the first free
// second, precisely because this delete (and the backup merge, and the UI)
// treat ts as identity - "runs are serialized" was never enough once the
// accounting rows started stamping their own clocks.
//
// A run that retried a direction also has a usage-accounting row one second
// later (the scheduler writes it at measuredTS+1 so a backup's ts merge key
// cannot collapse the two records into one), and that row goes too. It has to:
// it is invisible to every listing - the runs table's speedIsListed never lists
// a row that carries usage_run_ts - so the only place it appears is the
// data-usage total, and the only timestamp any surface ever shows the operator
// is the measurement's. Left behind it would bill bytes for a run that no
// longer exists, with nothing to delete it by.
//
// A failure record (Record failed tests) is removed by rowid like any run. It
// has nothing to cascade to: a failed run writes no selection report and keeps
// no round, and its own usage is the row itself.
//
// That row is found by the reference it CARRIES (speed.usage_run_ts), not by its
// position. The position - "the flagged row at ts+1" - was a guess, and it was
// wrong whenever a second run landed on that second: a manual run that fails one
// second after a scheduled measurement writes its own flagged row at exactly
// ts+1, and deleting the scheduled run then destroyed the manual run's whole
// record, silently, since with Record failed tests off nothing lists a flagged
// row.
//
// The positional sweep is GONE rather than kept as a fallback for accounting
// rows written before the column existed. Keeping it would reintroduce the
// defect in full - it is the same blind DELETE, and a row it wrongly matches is
// unrecoverable - to clean up after rows that exist in no released build: the
// accounting row and the column that references it are both unreleased work. An
// unreferenced row in a developer database is simply not swept, which costs a
// stale usage total there and nothing anywhere else.
//
// The run's time (speed_spans, behind the latency chart's hover note) is
// deliberately left. It says the link was busy then, which stays true whatever
// became of the row, and it is what explains the slow or failed probe rounds
// taken during it.
func (s *Store) DeleteSpeed(ctx context.Context, ts int64) (int64, error) {
	// One transaction with the run's selection report (speed_servers has no FK;
	// the cascade is manual): a run deleted without its report rows would leave
	// candidates explaining a run that no longer exists. The returned count is
	// every SPEED row removed - the measurement plus the members of its round -
	// so a caller can tell the operator what actually went, rather than
	// reporting one deletion for a round of sixteen. Still the "was there a
	// run" signal: it is only ever 0 when nothing matched.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	defer tx.Rollback()
	// `ts` identifies the run, but it does not identify EVERY row at that second.
	// An accounting row lands one second after the run it bills for, so the row at
	// this exact timestamp may belong to the PREVIOUS run - a manual run that
	// failed a second after a scheduled one finished is enough. Deleting it here
	// would destroy that run's usage the same way the positional sweep this
	// method used to end with did, only from the other direction.
	//
	// So a row that names a DIFFERENT run is left alone. A row naming this one, or
	// naming none at all (every measurement), is the run being deleted.
	// One row, by rowid. The uniqueness this method's comment asserts is real for
	// anything the daemon writes (InsertSpeedTS allocates the first free second
	// atomically, losers included), but it is NOT enforced by the schema, and a
	// database restored from an import - or written before that allocation
	// existed - can hold two runs on one second. Keyed on ts alone this
	// statement deleted both, behind a confirm that says "this run" and a toast
	// that says one went. Deleting exactly what was named is the promise the UI
	// makes; the round and accounting cascades below are keyed correctly already.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM speed WHERE rowid IN (
		     SELECT rowid FROM speed
		      WHERE ts = ? AND (usage_run_ts IS NULL OR usage_run_ts = ?)
		      ORDER BY (usage_run_ts IS NULL) DESC, rowid
		      LIMIT 1)`, ts, ts)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM speed_servers WHERE run_ts = ?`, ts); err != nil {
		recordDBErr(err)
		return 0, err
	}
	// A kept Best-of round goes with its winner: the losers' rows name this
	// run as their round (round_ts), and a member row without its round would
	// be a measurement nothing explains. Deleting a member row alone (its own
	// ts) takes just that row, like any other.
	members, err := tx.ExecContext(ctx, `DELETE FROM speed WHERE round_ts = ?`, ts)
	if err != nil {
		recordDBErr(err)
		return 0, err
	}
	// Still scoped to a FLAGGED row, belt to the reference's braces, so this can
	// never eat a reading: the daemon sets usage_run_ts only on accounting rows,
	// but a crafted import could put one on a measurement, and destroying a
	// reading is the worse outcome by far. speedIsAccounting rather than a
	// hand-written `failed = 1` so this and the reads that hide these rows judge
	// every marker the same way.
	//
	// The guard cannot strand a row the reference would have found, because the
	// two columns travel together through a backup: the export drops a
	// post-schema-4 column only when NO row uses it, and any row carrying
	// usage_run_ts carries `failed` as well.
	//
	// The count returned stays the measurement's - the caller's "was there a run"
	// signal - so removing the accounting row cannot read as a second run.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM speed WHERE usage_run_ts = ? AND `+speedIsAccounting, ts); err != nil {
		recordDBErr(err)
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		recordDBErr(err)
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	m, err := members.RowsAffected()
	if err != nil {
		return n, err
	}
	return n + m, nil
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
//
// The whole of it runs under outageMu. The close of outages whose end only the
// samples show (resolveDanglingDowns) is the one writer that adds ends and
// moves an 'up' back, and it decides both from rows it read first: run in
// between, it could write an end after this delete took that outage's 'down',
// or change the rows this sweep was decided on (see outageMu).
func (s *Store) DeleteOutage(ctx context.Context, ts int64) (int64, error) {
	s.outageMu.Lock()
	defer s.outageMu.Unlock()
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
	// First statement is the guarded write; no such 'up' = idempotent no-op (and
	// nothing else is touched). The guard asks only "is this row a closing
	// 'up'?" - 'up' IS the resolution, so the type check alone keeps a live
	// outage (a dangling 'down') undeletable. It used to require a non-NULL,
	// non-negative duration_s too, but a NULL length is a real shape of a
	// FINISHED outage - InsertEvent stores NULL for "no measurement",
	// eventRowSane strips an impossible imported length to NULL, and
	// repairInsaneEventDurations does the same at Open - and that last one made
	// the row undeletable exactly when deletion is the operator's only remedy
	// left. The negative check goes with it: nothing negative survives Open
	// anymore, and refusing it here only stranded the row the same way.
	res, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE ts = ? AND type = 'up'`, ts)
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

// ServerHealth is one server's persisted upload conviction.
type ServerHealth struct {
	ServerID string
	Expires  int64
	Fails    int
}

// SaveServerHealth records (or refreshes) one server's conviction. Called from
// a run, so it must not be slow or fatal: the caller logs and carries on.
func (s *Store) SaveServerHealth(ctx context.Context, h ServerHealth) error {
	if h.ServerID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO server_health (server_id, expires, fails) VALUES (?, ?, ?)`,
		h.ServerID, h.Expires, h.Fails)
	if err != nil {
		recordDBErr(err)
	}
	return err
}

// ForgetServerHealth drops one server's conviction outright. Called when a run
// has MEASURED that server's upload endpoint without being refused, which is
// the one piece of evidence strong enough to say the fault is gone - see
// noteUploadAccepted. Leaving the row would let the next restart read it back
// and charge the server for a fault it has since been measured not to have.
func (s *Store) ForgetServerHealth(ctx context.Context, serverID string) error {
	if serverID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM server_health WHERE server_id = ?`, serverID)
	if err != nil {
		recordDBErr(err)
	}
	return err
}

// ServerHealth returns every conviction worth remembering: the ones still in
// force, and the ones that have lapsed within `keep`. The caller decides what
// a lapsed row means - here it is only retention, and rows older than `keep`
// are swept. Read once at startup, which is also the only sweep the table
// needs: nothing is kept beyond `keep`, so it cannot grow past the servers
// convicted inside one.
func (s *Store) ServerHealth(ctx context.Context, keep time.Duration) ([]ServerHealth, error) {
	now := time.Now().Unix()
	cutoff := now - int64(keep.Seconds())
	if _, err := s.db.ExecContext(ctx, `DELETE FROM server_health WHERE expires <= ?`, cutoff); err != nil {
		recordDBErr(err) // a failed sweep is not a reason to skip the read
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT server_id, expires, fails FROM server_health WHERE expires > ?`, cutoff)
	if err != nil {
		recordDBErr(err)
		return nil, err
	}
	defer rows.Close()
	var out []ServerHealth
	for rows.Next() {
		var h ServerHealth
		if err := rows.Scan(&h.ServerID, &h.Expires, &h.Fails); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// SpeedAvgServers is how many servers the recent runs actually measured, over
// the same window and the same rows SpeedAvgBytes averages bytes across. It is
// the divisor that takes a recorded round back to one server's worth, so the
// data estimate can say what a round of a DIFFERENT size would cost. 1 when
// nothing is recorded, and never below 1.
//
// Counted from the selection report (speed_servers.measured), because that is
// the only place a round's size is written down: with losers discarded the
// winner's row carries the whole round's bytes and says nothing about how many
// servers produced them, and the Best-of SETTING cannot answer it either -
// changing that changes what the next run will cost, not what the recorded
// ones did. A run with no report (iperf3, or a row from before the reports
// existed) counts as the one server it measured.
func (s *Store) SpeedAvgServers(ctx context.Context) (float64, error) {
	var avg float64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(AVG(n), 1) FROM (
		  SELECT MAX(1, (SELECT COUNT(*) FROM speed_servers ss WHERE ss.run_ts = s.ts AND ss.measured = 1)) AS n
		    FROM speed s
		   WHERE `+speedIsResult+` AND download_bytes IS NOT NULL AND ts <= ?
		   ORDER BY ts DESC LIMIT 20)`, currentHorizon(time.Now().Unix())).Scan(&avg)
	if err != nil {
		recordDBErr(err)
		return 1, err
	}
	if !(avg >= 1) { // NULL/NaN/0 all mean "nothing to divide by"
		return 1, nil
	}
	return avg, nil
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
	// Accounting rows are excluded even though they carry bytes: this average is
	// a PROJECTION of what the next run will cost ("at this interval you'll use
	// X/month"), and a total failure aborts partway - its bytes are a fraction of
	// a real run's, so counting them predicts a bill no schedule produces. The
	// bytes still count in what was ACTUALLY used (SpeedDataUsage), which is the
	// number they belong to.
	// Bounded at currentHorizon like SpeedDataUsage beside it - /api/status prints
	// both in one object, and two answers off two different presents read as a
	// contradiction the operator cannot resolve. ORDER BY ts DESC makes a
	// future-dated run (an import, a clock jump) sort FIRST, so it would not merely
	// join the newest 20, it would take a slot and EVICT a real run: one row an hour
	// ahead owns a twentieth of a mean whose other members it displaced, and Prune
	// keeps it for pruneFutureSlack. The predicate belongs INSIDE each derived table,
	// before ORDER BY/LIMIT - wrapped around it, the row still spends its slot and
	// the average is silently taken over 19 runs.
	// One horizon captured for both directions, not one time.Now() each: two calls
	// can straddle a second boundary and answer download and upload off presents a
	// second apart, which is the same disagreement in miniature.
	horizon := currentHorizon(time.Now().Unix())
	err = s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COALESCE(CAST(AVG(b) AS INTEGER), 0)
		     FROM (SELECT s.download_bytes + COALESCE((SELECT SUM(l.download_bytes) FROM speed l WHERE l.round_ts = s.ts), 0) AS b
		           FROM speed s
		           WHERE `+speedIsResult+` AND download_bytes IS NOT NULL AND ts <= ?
		           ORDER BY ts DESC LIMIT 20)),
		  (SELECT COALESCE(CAST(AVG(b) AS INTEGER), 0)
		     FROM (SELECT s.upload_bytes + COALESCE((SELECT SUM(l.upload_bytes) FROM speed l WHERE l.round_ts = s.ts), 0) AS b
		           FROM speed s
		           WHERE `+speedIsResult+` AND upload_bytes IS NOT NULL AND ts <= ?
		           ORDER BY ts DESC LIMIT 20))`, horizon, horizon).Scan(&avgDown, &avgUp)
	if err != nil {
		recordDBErr(err)
		return 0, 0, err
	}
	return avgDown, avgUp, nil
}

// Uptime holds one Observation per dashboard time window (mirrors the DataUsage
// windows, so the uptime pill offers the same set as the data pill). Every field
// carries its own coverage, so no window can reach a renderer as a bare ratio.
type Uptime struct {
	H6  Observation
	H24 Observation
	D7  Observation
	D30 Observation
	Y1  Observation
	All Observation
}

// NamedObservation is one window's label ("6h", "24h", … - the label /metrics and
// /api/status both key on) beside its Observation.
type NamedObservation struct {
	Window string
	Obs    Observation
}

// Each returns the six windows in dashboard order.
//
// This is the single enumeration of the window set: /metrics and /api/status both
// iterate it, so a window cannot exist on one surface and not the other, and a
// renderer that writes a ratio writes the coverage from the same loop variable in
// the same loop body - there is no second collection to forget. Adding a window
// here adds it, with its coverage, everywhere at once.
func (u Uptime) Each() []NamedObservation {
	return []NamedObservation{
		{"6h", u.H6}, {"24h", u.H24}, {"7d", u.D7},
		{"30d", u.D30}, {"1y", u.Y1}, {"all", u.All},
	}
}

// UptimeWindows computes the Observation for each window relative to now via
// UptimeSince (each clamps to the observed period); "all" runs from epoch so it
// clamps to when monitoring began. retention clamps the long windows to
// retained-event coverage (see UptimeSince). Returns the first error encountered.
func (s *Store) UptimeWindows(ctx context.Context, now time.Time, retention time.Duration) (u Uptime, err error) {
	at := func(d time.Duration) Observation {
		if err != nil {
			return Observation{}
		}
		o, e := s.UptimeSince(ctx, now.Add(-d), retention)
		if e != nil {
			err = e
		}
		return o
	}
	u.H6 = at(6 * time.Hour)
	u.H24 = at(24 * time.Hour)
	u.D7 = at(7 * 24 * time.Hour)
	u.D30 = at(30 * 24 * time.Hour)
	u.Y1 = at(365 * 24 * time.Hour)
	if err == nil { // "all": from the epoch, clamped inside UptimeSince to first-seen / retention
		u.All, err = s.UptimeSince(ctx, time.Unix(0, 0), retention)
	}
	return u, err
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
		currentHorizon(now.Unix())).
		Scan(&u.H6, &u.H24, &u.D7, &u.D30, &u.Y1, &u.All)
	return u, err
}

// SpeedDataUsageSince returns the speedtest bytes (download+upload) transferred
// since t - backs the dashboard data bubble's custom (arbitrary-window) range.
// Bounded at currentHorizon like the preset windows beside it: the custom range
// is the same question over a different span, so a future-dated run must not
// inflate one bubble while the presets exclude it.
func (s *Store) SpeedDataUsageSince(ctx context.Context, since time.Time) (int64, error) {
	var b int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(COALESCE(download_bytes,0)+COALESCE(upload_bytes,0)),0)
		 FROM speed WHERE ts>=? AND ts<=?`,
		since.Unix(), currentHorizon(time.Now().Unix())).Scan(&b)
	return b, err
}

// SpeedResults returns the newest limit runs that are a test's own result -
// SpeedRuns without a kept Best-of round's members (speedIsResult). For a
// scan that walks back over RUNS with a fixed budget: on SpeedRuns the
// members would spend it, and a single kept round of a dozen servers could
// then hide the run the scan is after.
func (s *Store) SpeedResults(ctx context.Context, limit int) ([]SpeedSample, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+speedCols+` FROM speed WHERE `+speedIsResult+` ORDER BY ts DESC LIMIT ?`, limit)
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

// SpeedRuns returns a page of the runs table, newest first: every measurement,
// a kept Best-of round's members, and the failure records Record failed tests
// keeps (speedIsListed). Read Failed before any figure on a row.
func (s *Store) SpeedRuns(ctx context.Context, limit, offset int) ([]SpeedSample, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+speedCols+` FROM speed WHERE `+speedIsListed+` ORDER BY ts DESC LIMIT ? OFFSET ?`, limit, offset)
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
	// WindowS/ObservedS are the day's disclosure, the same one coverage_ratio makes
	// on /metrics: how many of the day's seconds fall inside the requested range at
	// all (the first and last day are partial, and a DST day is 23h or 25h), and how
	// many of those the monitor actually watched. Without them the heatmap subtracts
	// paused time from its numerator perfectly and then tells the operator nothing:
	// a day nobody watched and a day watched end to end with a flawless link render
	// as the identical empty square. Correct arithmetic and honest disclosure are
	// different properties, and this seam has now drifted on the second one six times.
	WindowS   int `json:"window_s"`
	ObservedS int `json:"observed_s"`
}

// Observed reports whether the day holds a measurement at all - the DowntimeDay
// spelling of Observation.Defined, and equally not a threshold. A day with
// WindowS 0 (no in-range seconds) is treated as observed so an empty row can
// never render as an alarming "unmonitored" square.
func (d DowntimeDay) Observed() bool { return d.WindowS <= 0 || d.ObservedS > 0 }

// DowntimeByDay returns per-day outage counts, total downtime and observation
// span since the given time. Day boundaries are taken in loc (the viewer's
// timezone; nil = the server's local zone). Bucketing is done in Go rather than
// SQLite's 'localtime' so any IANA zone works. A 'down' counts as an outage on the
// day it happened; a recovery's outage duration is prorated across the local days
// the outage actually spanned (clamped to the window), so a multi-day outage marks
// every offline day instead of booking days of downtime on the recovery day alone.
// Orphaned down->down gaps (a restart mid-outage) are prorated too, so the
// heatmap's downtime and outage count match UptimeSince/ResolvedOutagesSince.
//
// A row is emitted for a day with at least one event, one offline second, OR one
// unobserved second - the last of those is why the day loop at the end exists:
// without it a fully dark day produces no row at all and its ObservedS field would
// have nowhere to live.
func (s *Store) DowntimeByDay(ctx context.Context, since time.Time, loc *time.Location) ([]DowntimeDay, error) {
	return s.downtimeByDayAt(ctx, since, loc, time.Now().Unix())
}

// downtimeByDayAt is DowntimeByDay against a caller-supplied clock (the same seam
// openAt gives Open), so a test can stand the heatmap on a chosen day - the eve of
// a DST jump, say - without faking time for the whole process. Everything this
// function decides about a day is measured against nowU: the event horizon, the
// end of the observation walk, and the bound on a still-open outage.
func (s *Store) downtimeByDayAt(ctx context.Context, since time.Time, loc *time.Location, nowU int64) ([]DowntimeDay, error) {
	if loc == nil {
		loc = time.Local
	}
	// Start the scan from the last event AT/BEFORE the window, not strictly inside
	// it, exactly as UptimeSince/orphanGapDowntime do - otherwise an outage whose
	// leading 'down' predates the window (an ongoing or restart-interrupted outage
	// spanning the boundary) is invisible here and its in-window downtime is lost,
	// disagreeing with the uptime%. A pre-window boundary event is used only to
	// anchor proration; it is never counted as an in-window outage (guarded below).
	//
	// The scan stops at currentHorizon for the same reason UptimeSince's
	// newest-event decision does, and it MUST be the same bound: the trailing-down
	// branch at the end of this function is what credits an ongoing outage, and it
	// fires only when no 'up' followed. A future-dated 'up' read as the closing one
	// would silently switch that branch off here while uptime and the digest still
	// credit the outage - the heatmap drawing a clean square beside a percentage
	// that is not.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, type, COALESCE(duration_s, 0)
		-- type IN ('down','up') here and in the anchor: consistency with the
		-- newest-event probes, which DO depend on it (see UptimeSince). The Go
		-- loop below already ignores an unrecognised type - its switch has no
		-- default - so this filter is belt-and-braces rather than the thing the
		-- tally rests on, and no fixture I could build makes it change an answer.
		-- It is here so every reader of this table agrees about what it reads,
		-- which is what stopped being true when only the probes were filtered.
		FROM events
		WHERE type IN ('down','up')
		  AND ts >= (SELECT COALESCE(MAX(ts), 0) FROM events WHERE type IN ('down','up') AND ts <= ?) AND ts <= ?
		ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END`, since.Unix(), currentHorizon(nowU))
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
	// The window's pause spans, fetched ONCE for both loops below. prorate used to
	// ask pausedOverlap per day-segment, which is the round trip per day that this
	// function's own pauseSpans comment argues against - and it is worse than that
	// comment's 366, because it is one query per outage DAY-SEGMENT: a year on the
	// monitoring schedule (52k pause rows, ~700 outages) spent 4.1 of its 4.2
	// seconds inside those ~705 unbounded SUMs, on every 60-second heatmap poll.
	// One bounded query and an in-Go intersection is the same arithmetic: both
	// paths clamp each span to the segment and clamp the result at 0, and
	// pauseSpans has already clamped to [since, now], inside which every segment
	// prorate builds already lies.
	// Reach back to the earliest event pulled in, not just to sinceU: the scan
	// deliberately starts at the last event AT/BEFORE the window so an outage
	// straddling the boundary is visible, and its observed-span arithmetic needs the
	// pauses over its REAL interval, not only the part inside the window.
	pauseFrom := sinceU
	if len(events) > 0 && events[0].ts < pauseFrom {
		pauseFrom = events[0].ts
	}
	spans, err := s.pauseSpans(ctx, pauseFrom, nowU)
	if err != nil {
		return nil, err
	}
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
	// prorate splits ONE outage across the local days it touches. The seconds it
	// splits come from observedOutageSpans - the same function uptime and the digest
	// use - so all three now describe an outage with one piece of arithmetic rather
	// than three that resemble each other.
	//
	// The old version clamped to the window and only THEN started spending the
	// observed-length budget, which quietly refilled it at the window edge: seconds
	// already spent before `since` became available again inside it. That was
	// invisible while a pause row explained the gap between an outage's wall span
	// and its observed length, because subtracting the pause left nothing for the
	// budget to trim. It showed up on the suspend-shaped record - wall gap longer
	// than duration_s with NO pause row, which is what a system sleep and every
	// outage restored from an older build look like - where uptime reported 0s for a
	// window the heatmap filled with 30s of the same outage.
	prorate := func(start, end, limit int64) {
		if end <= start {
			return
		}
		// limit < 0 means "no recorded observed length" (the orphan/dangling paths);
		// the whole interval is then creditable, so hand the budget its own width.
		budget := limit
		if budget < 0 {
			budget = end - start
		}
		for _, sp := range observedOutageSpans(spansIn(spans, start, end), start, end, budget) {
			a, b := sp[0], sp[1]
			if a < sinceU {
				a = sinceU
			}
			if b > nowU {
				b = nowU
			}
			for a < b {
				d := time.Unix(a, 0).In(loc)
				next := nextLocalDay(d, loc)
				if next > b {
					next = b
				} else if next <= a {
					next = a + 1 // defensive: a boundary must advance, never swallow the rest
				}
				day(d.Format("2006-01-02")).DowntimeS += int(next - a)
				a = next
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
			//
			// A NEGATIVE duration_s is corrupt-backup residue (InsertEvent writes
			// NULL, eventRowSane strips it from new imports) - but prorate's limit
			// uses negative as its internal "no recorded length, credit everything"
			// sentinel, so passing the stored value through booked the whole wall gap
			// as heatmap downtime while observedOutageSpans books ZERO for the same
			// row (observed <= 0 means nothing was measured). Zero is the reading
			// uptime and the digest agree on; normalize before the call.
			dur := int64(e.dur)
			if dur < 0 {
				dur = 0
			}
			start := e.ts - dur
			if prevDownTS >= 0 && prevDownTS < e.ts {
				start = prevDownTS
			}
			prevDownTS = -1
			prorate(start, e.ts, dur)
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

	// Per-day observation. Every local day in [since, now] gets its wall span and
	// its watched span; a day that was not watched end to end gets a ROW MINTED for
	// it, because DowntimeByDay otherwise emits nothing at all for a day with no
	// event and no offline second - so the field alone would have had nowhere to
	// live on exactly the days it exists to describe. Fully-watched days keep the
	// old emission rule (no row unless something happened) so a year of healthy
	// 24/7 monitoring still returns a handful of rows to the 60-second heatmap poll
	// rather than 366.
	// Time before monitoring began is UNOBSERVED, and saying so is the whole point
	// of these two fields. Pauses cannot express it: a machine that was not running
	// wrote no pause rows, so span-minus-pauses came out equal to the full day, the
	// day read as watched-end-to-end-and-clean, no row was minted, and a missing row
	// draws as a clean square. UptimeSince has clamped to this same anchor all
	// along - which is exactly why the heatmap could show a year of green beside an
	// uptime pill that correctly described two days.
	began, err := s.monitoringSince(ctx, nowU)
	if err != nil {
		return nil, err
	}
	for start := sinceU; start < nowU; {
		d := time.Unix(start, 0).In(loc)
		next := nextLocalDay(d, loc)
		if next > nowU {
			next = nowU
		} else if next <= start {
			next = start + 1 // defensive: a boundary must advance (mirrors prorate)
		}
		span := next - start
		// Nothing before the anchor was watched, whatever the pause rows say, so narrow
		// the RANGE to the watchable part before measuring anything inside it. Taking
		// the pauses off the whole day first and flooring afterwards discarded the
		// subtraction entirely whenever the floor bound: pauses lying after the anchor
		// - inside the part that really was watched - vanished with it, and the first
		// day of monitoring claimed unbroken coverage from the moment it began.
		lo := start
		if began > lo {
			lo = began
		}
		var obs int64
		if lo < next {
			// Clamp exactly as UptimeSince clamps its downtime: duplicated or
			// overlapping pause rows must read as "the whole segment was unobserved",
			// never as negative observed time.
			obs = (next - lo) - spanOverlap(spans, lo, next)
			if obs < 0 {
				obs = 0
			}
		}
		date := d.Format("2006-01-02")
		if i, ok := byDay[date]; ok {
			out[i].WindowS += int(span)
			out[i].ObservedS += int(obs)
		} else if obs < span {
			rec := day(date)
			rec.WindowS, rec.ObservedS = int(span), int(obs)
		}
		start = next
	}

	// Proration can back-fill days out of order (an 'up' credits earlier days), and
	// the observation loop above mints unobserved days after them, so sort;
	// "YYYY-MM-DD" sorts chronologically.
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

// FutureSlack publishes the bound above, so a caller outside the package can ask
// the same question Prune asks - "is this row's ts still plausibly future?" -
// without restating the 48h and drifting from it.
const FutureSlack = pruneFutureSlack

// metricsFutureSkew is how far ahead of wall-now a row may sit and still count as
// "current" in a read of the present. Read it through currentHorizon below, never
// by restating the sum. Prune's 48h slack is deliberately generous so a stepped-back
// clock never deletes real rows - but that same slack lets a future-dated import
// win a "latest" read and freeze current metrics until wall time catches up. This
// tight bound excludes such rows from the live reads while still tolerating a
// slightly-fast importer's just-now timestamps. Rows beyond it stay in the DB
// (Prune owns deletion); they are only hidden from the current-state reads.
const metricsFutureSkew = 2 * time.Minute

// currentHorizon is the newest timestamp a read of the PRESENT may consider: the
// wall clock plus the skew above. Every such read goes through this one function
// rather than restating the sum, because the defect it exists to stop was never a
// missing bound in general - it was the bound holding on SOME surfaces and not
// others, so a single future-dated row answered for "now" on the pills, the
// charts, the digest and the uptime figures in different combinations depending
// on which read you asked.
//
// It bounds the OPEN-ENDED reads only. A caller naming an absolute end gets
// exactly the window it named, future or not: the UI accepts a typed range that
// ends ahead of now (it clamps at now+366d), and quietly answering a narrower
// question than the one asked is its own wrong answer.
func currentHorizon(nowU int64) int64 { return nowU + int64(metricsFutureSkew.Seconds()) }

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
//
// Clear runs it too, before a Delete now takes every sample. Both pass liveFrom,
// which reads the line the running monitor's outages draw (monitorOutages; nil
// is no line at all, and nothing in the daemon passes it). The line is there
// for the running monitor, which holds an outage open in memory from its own
// 'down' and closes it by a stricter rule than the scan below: up-after good
// rounds in a row, dated at the last of them, its length measured on the
// monotonic clock with paused time taken out. The scan takes the first good
// second. So while the monitor is still counting, or after one good round
// between bad ones, the samples already show a recovery the monitor has not
// accepted - and after a failed write its 'up' can be waiting in its retry
// buffer, with the 'down' still dangling on disk. A synthetic 'up' for that outage is one closing event too many once the
// monitor writes its own: the readers book the second 'up' as an outage of its
// own, anchored at ts minus its duration, and with the samples gone nothing
// re-derives the truth.
//
// So every 'down' at or past liveFrom's line is left to the monitor, and an
// earlier outage's search stops at that line, as it stops at a 'down' on disk.
// The line is read again after each search, because the monitor can confirm an
// outage while one runs, and a recovery found past the line as it then stands
// is the monitor's to judge. The line is the earliest outage the monitor is
// not done with, so it moves later as the monitor ends its outages, but never
// while a close runs: the outages the monitor has finished come off its list
// as a close begins, before the close reads which outages have no end, and
// the ones it finishes after that stay on until the next close
// (monitorOutages). A line that moved later between those two reads let the
// close take an outage the monitor had just ended for one with no end.
//
// A cleanup used to go without the line and keep its distance through its
// cutoff alone. It only writes a recovery older than the sample retention,
// thirty days by default, so an outage the monitor still holds would have to
// have stayed open, with the monitor running, for that whole window after a
// good second the monitor never accepted. But the retention is the operator's
// to set: the Data tab takes a fraction of a day, and the API and -retain any
// length. At a window of a few minutes the cleanup ended a ten-minute-old
// outage at one good round between bad ones and deleted that round in the same
// pass, and the monitor's own 'up' then made two ends of it. At twelve hours
// the same happened to an outage that had flapped for longer than that. A
// Delete now never had any distance: every sample goes, so its cutoff is the
// present.
//
// What the line costs a cleanup is an outage of the monitor's that the monitor
// never finishes: its 'down' is on disk and its 'up' is lost for good. The
// monitor keeps an 'up' the store refused and writes it again every round,
// so that takes a store that goes on refusing until the retry buffer is full
// and drops its oldest record (maxPendingEvents in the monitor, logged and
// counted as monitor.event_dropped). Without the line a cleanup ended that
// outage at the first good second in its samples as they aged out, and it kept
// its length. With it the outage stays open on disk for as long as this
// process runs, and the readers bound it at the first good round still kept.
// So once the rounds that showed the recovery have aged out it reads longer at
// every pass, as far as the next 'down' on disk, the monitor's next outage
// that did get written. The line stays at that outage too, so from then on
// every later outage with no end is left alone with it, the monitor's or not:
// one a restore brings, dated after it, gets no end from a cleanup either. The
// same goes for an outage whose 'down' was the record dropped. A restart lifts
// the line. One that comes while those rounds are still kept lets the cleanup
// that reaches them end the outage where they prove it, and after a later one
// the longer reading stays. That is accepted: it takes a store that refuses
// writes across dozens of transitions, and it is said on /metrics and in the
// log. The second end took a short window and a link that flapped, and nothing
// said so.
//
// Two other ways to draw that line were weighed and left. Reading the monitor's
// rule back out of the samples cannot be done from here: this package does not
// know up-after, a live setting, nor which rounds a pause or a suspend cut out of
// the monitor's streak, and no sample shows an 'up' still waiting to be written.
// Asking the monitor (the snapshot /api/status reads) says whether the link is
// down now, not whether an 'up' of the monitor's own is still to be written, and
// it is read before the delete runs, so a transition in between goes unseen.
//
// The whole of it runs under outageMu, whoever calls it. What it writes is
// decided by what it read first: which outages have no end on disk, and the
// second the samples prove for each. A second close in between wrote the same
// end again, and a delete of outages in between left the end it wrote with no
// outage (see outageMu for who else takes the lock, and why the monitor does
// not).
//
// It counts the failures of its writes on /metrics itself, and of the lookup
// that goes with them, and returns those wrapped in recordedErr. The rest are
// its caller's to count.
func (s *Store) resolveDanglingDowns(ctx context.Context, sampleCutoff, nowU int64, liveFrom func() int64) error {
	return s.closeDanglingDowns(ctx, sampleCutoff, nowU, liveFrom, nil)
}

// plannedClose is one end a close of the outages would record
// (closeDanglingDowns), as its dry run reports it.
type plannedClose struct {
	down  int64 // the 'down' it ends
	at    int64 // the second the samples prove the link came back
	moved int64 // the 'up' it would move back to at, by its ts; 0 when it would write a new one
}

// closeDanglingDowns is resolveDanglingDowns, with a dry run: when plan is not
// nil it reads which outages have no end and searches the samples for each, as
// the close does, fills plan with the ends it would record, and writes
// nothing. PruneDue asks for one, to count what the close a cleanup begins
// with puts on that cleanup's list. It is this function and not a second
// reading of the same rules, so the count cannot come to disagree with the
// close about which outages get an end, or where.
func (s *Store) closeDanglingDowns(ctx context.Context, sampleCutoff, nowU int64, liveFrom func() int64, plan *[]plannedClose) error {
	// nowU was read before this wait, which can be long behind a restore: the
	// monitor's door keeps every outage on its list until the close has its
	// turn (monitorOutages, "No close is waiting").
	s.live.wait()
	s.outageMu.Lock()
	defer s.outageMu.Unlock()
	// The outages the monitor has finished come off its list here, before the
	// first read, and none does again until this returns: the line may move
	// earlier under a close, never later (monitorOutages).
	s.live.begin(nowU)
	defer s.live.end()
	if liveFrom == nil {
		liveFrom = func() int64 { return math.MaxInt64 } // no line: every 'down' is reached
	}
	// Dangling downs: a 'down' immediately followed by another 'down' (orphan gap)
	// or the final event being a 'down'. Mirrors UptimeSince's pairing, because
	// this is the write side of that agreement and a disagreement here is
	// permanent - but on the FUTURE side it deliberately reaches further than the
	// readers do, for the reason below.
	//
	// The horizon: the question here is not "what answers for now?" but "will
	// anything still be on disk to close this outage after the sweep below runs?",
	// so the bound is this prune's own future horizon (nowU + pruneFutureSlack),
	// the same one the events sweep deletes at, and not currentHorizon.
	//
	//   - An 'up' beyond pruneFutureSlack is deleted by that sweep a few lines
	//     below, so it closes nothing. Pairing a 'down' with one would skip the
	//     synthetic recovery while every reader - which ignores it too, see
	//     currentHorizon - still sees the outage open, and the samples that were
	//     its only other proof go in this same call. The outage would be left open
	//     forever and uptime would collapse toward zero.
	//   - An 'up' inside pruneFutureSlack SURVIVES, and closes the outage by itself
	//     once the wall clock reaches it. Bounding at currentHorizon hid exactly
	//     those rows (past two minutes ahead, inside 48 hours) and wrote a
	//     synthetic recovery beside one, leaving TWO closing events for one outage:
	//     the readers then report one outage as two and book its downtime twice,
	//     permanently, because the samples are gone and every later prune sees two
	//     complete outages and leaves them alone.
	//
	// Derived from nowU, the clock reading the sweep's horizon starts from. The
	// sweep's only moves later from there, by the time its pass runs (PruneLive),
	// so an 'up' counted here as one the sweep keeps is kept. One just past this
	// bound can be kept as well, when the pass runs on across a sleep. A
	// recovery the samples prove was written all the same, by moving that 'up'
	// back when it is this outage's own, so the outage still has one end.
	//
	// The type filter: same reason the readers grew one. An event type this build
	// cannot interpret must not silently close an outage.
	pruneHorizon := nowU + int64(pruneFutureSlack/time.Second)
	// Selected alongside the dangling shapes: a 'down' PAIRED with an 'up' the
	// readers cannot see yet (past currentHorizon, inside pruneFutureSlack).
	// Such a pair is closed on disk but reads as an open outage for up to 48h,
	// and its only other proof - the recovery samples - is being pruned in
	// this same call; a later backward clock step could then delete the future
	// 'up' and leave the outage open forever (the exact hazard the correction
	// block below documents as tried-and-rejected). When the samples prove an
	// earlier recovery, that specific 'up' is moved back to the proven second
	// - correction by identity, so it can never seize a different outage's row.
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, next_ts, next_type FROM (
			SELECT ts, type,
			       LEAD(ts)   OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_ts,
			       LEAD(type) OVER (ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END) AS next_type
			FROM events WHERE type IN ('down','up') AND ts <= ?)
		WHERE type='down' AND (next_type='down' OR next_type IS NULL
			OR (next_type='up' AND next_ts > ?))`, pruneHorizon, currentHorizon(nowU))
	if err != nil {
		return err
	}
	// Collect the pairs before the per-gap recovery lookups: a query inside an
	// open rows scan deadlocks the single-connection (":memory:") pool.
	type gap struct {
		down, end int64
		// final: nothing follows this 'down' inside the bound at all, as opposed
		// to another 'down' following it. Only a final gap can be the one a
		// future-dated 'up' belongs to, because any 'up' the clock has reached
		// would have paired with this 'down' and it would not be dangling.
		final bool
		// pairUp: this 'down' is already paired with an 'up' the readers cannot
		// see yet - the row named here, to be corrected BY IDENTITY when the
		// samples prove an earlier recovery. 0 for the dangling shapes.
		pairUp int64
	}
	var gaps []gap
	for rows.Next() {
		var down int64
		var nextTS sql.NullInt64
		var nextType sql.NullString
		if err := rows.Scan(&down, &nextTS, &nextType); err != nil {
			rows.Close()
			return err
		}
		end, final, pairUp := nowU, true, int64(0)
		if nextType.Valid && nextTS.Valid {
			switch nextType.String {
			case "down":
				end, final = nextTS.Int64, false
			case "up": // the future-paired shape; recovery can only lie in the observed past
				final, pairUp = false, nextTS.Int64
			}
		}
		gaps = append(gaps, gap{down: down, end: end, final: final, pairUp: pairUp})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	type synthUp struct {
		ts, dur, down int64
		final         bool
		pairUp        int64 // non-zero: correct THIS 'up' to ts instead of searching or inserting
	}
	var synth []synthUp
	for _, g := range gaps {
		// Read after the pairing query, never before it: InsertEvent puts a
		// 'down' on the monitor's list before its row lands, so a monitor
		// 'down' the query found is behind the line by now.
		live := liveFrom()
		if g.down >= live {
			continue // the monitor's own outage, or one after it: the monitor closes those
		}
		if g.end > live {
			// The 'down' of the earliest outage the monitor is not done with
			// ends this search, as a 'down' on disk would: the samples after it
			// belong to the monitor's outage. And like one on disk it means
			// this gap is not the last, even when that 'down' is not on disk
			// yet (its write failed and waits in the monitor's buffer).
			g.end, g.final = live, false
			// For the same reason an 'up' past the line is the monitor's to
			// judge, not this outage's to move. This 'down' pairs with one
			// only while the monitor's 'down' at the line is not on disk: it
			// waits in the retry buffer, or the buffer was full and dropped it.
			// The monitor dates its own 'up' ahead of the clock on purpose
			// (the widen in Monitor.transition). Moved back to this outage's
			// recovery, it closed this outage with the monitor's measurement,
			// and the monitor's outage had no end once its 'down' landed. This
			// outage gets an end of its own instead, at a price accepted with
			// it: an 'up' that an import brought from a clock that ran fast,
			// which the close now leaves alone here too, can read as an outage
			// of its own once the monitor's waiting 'down' lands and the clock
			// passes it. Once the monitor's outage has both its events on disk
			// the line moves past it and this branch is not reached for it.
			if g.pairUp > live {
				g.pairUp = 0
			}
		}
		rec, ok, err := s.firstQuorumRecovery(ctx, g.down, g.end)
		if err != nil {
			return err
		}
		if ok && rec >= liveFrom() {
			// The monitor confirmed an outage while the search ran, and this
			// recovery came after that 'down': it is the monitor's to judge.
			continue
		}
		// Only persist when the recovery evidence is itself being pruned; if it
		// survives this prune, the samples still prove it and no synthetic event is
		// needed (avoids re-scanning the recent, still-shifting tail too).
		if ok && rec > g.down && rec < sampleCutoff {
			// duration_s is OBSERVED seconds everywhere, never the raw wall gap: the
			// monitor subtracts m.pausedGap before writing an 'up' (see
			// Monitor.transition, "so only time actually observed down ... is
			// recorded"), and every consumer is built on that. UptimeSince runs
			// duration_s from the paired 'down' for the numerator and removes pause
			// spans from the DENOMINATOR only - so a synthetic 'up' carrying wall
			// time would book the unobserved stretch as downtime AND drop it from
			// observed time: double-booked, and permanently, because after this
			// prune it is an ordinary completed outage that nothing re-derives. The
			// case that motivated this: a 'down' is written, the host loses power for
			// weeks, and the restart books the whole gap as one pause span (see
			// Monitor.Run's startup-gap pause) - correctly 0 downtime until an
			// hourly prune minted a weeks-long phantom outage out of it.
			dur := rec - g.down
			paused, err := s.pausedOverlap(ctx, g.down, rec)
			if err != nil {
				return err
			}
			// Clamp: pause spans are normally disjoint, but an import or an
			// overlapping startup-gap span could sum past the wall gap, and a
			// negative duration_s would read as un-clamped downtime downstream.
			if dur -= paused; dur < 0 {
				dur = 0
			}
			synth = append(synth, synthUp{ts: rec, dur: dur, down: g.down, final: g.final, pairUp: g.pairUp})
		}
	}
	if plan != nil {
		// The dry run ends here, before the first write. What the writes below
		// decide as they go is decided here the same way: an outage's own 'up'
		// dated ahead of the clock is moved back rather than joined by a second
		// end, the one the pairing named or, for the last outage, the next
		// event when that is an 'up'.
		for _, u := range synth {
			moved := u.pairUp
			if moved == 0 && u.final {
				var at int64
				var kind string
				switch err := s.db.QueryRowContext(ctx, closeNextEvent, u.down).Scan(&at, &kind); {
				case err == nil && kind == "up":
					moved = at
				case err == nil, errors.Is(err, sql.ErrNoRows):
				default:
					return err
				}
			}
			*plan = append(*plan, plannedClose{down: u.down, at: u.ts, moved: moved})
		}
		return nil
	}
	if closeHook != nil {
		closeHook()
	}
	for _, u := range synth {
		// An outage gets ONE closing event. A 'down' can look dangling while an
		// 'up' for it already exists, dated PAST pruneFutureSlack - an import
		// from a host whose clock ran fast, or a clock excursion here. The
		// pairing above stops at the slack (a row inside it survives the sweep
		// and closes the outage by itself once the clock arrives), so anything
		// the final-gap lookup finds here is a row the sweep below is about to
		// DELETE - and the samples are about to be pruned too, so something
		// must record the recovery before both proofs go.
		//
		// But INSERTING beside it leaves two closing events for one outage. Once
		// the clock passes the future row, the second one anchors its own outage
		// at ts-duration_s (completedOutagesSince) and the history reports an
		// outage that never happened, permanently, with the samples that would
		// disprove it already gone.
		//
		// So the future row is CORRECTED rather than duplicated. It is not
		// credible as written - it claims a recovery later than the samples prove,
		// at a time that has not arrived - and moving it to the proven second
		// keeps the operator's event, keeps the count at one, and puts the row in
		// the past where no future sweep can remove it. That last part matters:
		// declining to write anything at all was tried, and a backward clock step
		// then deleted the future row and left the outage open forever.
		//
		// Only an 'up' that is NEXT is this outage's, though. If a 'down' sits
		// between (a complete future pair - the same fast-clock import), the
		// 'up' beyond it is THAT outage's recovery: seizing it would rewrite a
		// different outage's closing event into the past and leave its 'down'
		// open forever once the clock reaches it - the exact phantom this block
		// exists to prevent, manufactured instead of avoided. The dangling
		// 'down' takes a synthetic recovery of its own, and the future pair is
		// left to the sweep's judgement.
		moved := false
		if u.pairUp != 0 {
			// The outage's own future 'up', already identified by the pairing -
			// moved to the second the samples prove, by its identity, so no
			// search can hand it a different outage's recovery.
			if _, err := s.db.ExecContext(ctx,
				`UPDATE events SET ts = ?, duration_s = ? WHERE type = 'up' AND ts = ?`, u.ts, u.dur, u.pairUp); err != nil {
				recordDBErr(err)
				return recordedErr{err}
			}
			log.Printf("pingularity: an outage recovery was recorded %ds in the future (a clock that ran fast, or an import from one); moved it back to the second the probe history proves the link returned", u.pairUp-u.ts)
			moved = true
		}
		if u.final {
			var at int64
			var kind string
			switch err := s.db.QueryRowContext(ctx, closeNextEvent, u.down).Scan(&at, &kind); {
			case err == nil && kind != "up":
				// A different outage begins first - nothing here to correct.
			case err == nil:
				if _, err := s.db.ExecContext(ctx,
					`UPDATE events SET ts = ?, duration_s = ? WHERE type = 'up' AND ts = ?`, u.ts, u.dur, at); err != nil {
					recordDBErr(err)
					return recordedErr{err}
				}
				log.Printf("pingularity: an outage recovery was recorded %ds in the future (a clock that ran fast, or an import from one); moved it back to the second the probe history proves the link returned", at-u.ts)
				moved = true
			case errors.Is(err, sql.ErrNoRows):
			default:
				recordDBErr(err)
				return recordedErr{err}
			}
		}
		if moved {
			continue
		}
		// Not through InsertEvent: that door is the monitor's, and an 'up'
		// through it ends an outage on the monitor's list.
		if err := s.insertEvent(ctx, time.Unix(u.ts, 0), "up", int(u.dur), "recovered while unmonitored"); err != nil {
			return recordedErr{err} // insertEvent counted it
		}
	}
	if len(synth) > 0 {
		s.invalidateReadCaches()
	}
	return nil
}

// closeNextEvent finds the event that follows a 'down' (?1), a 'down' first
// where the two share a second. The close asks it of the last outage with no
// end: an 'up' there is that outage's own, dated past the future horizon, and
// is moved back instead of joined by a second end. One text for the close and
// for its dry run.
const closeNextEvent = `SELECT ts, type FROM events WHERE type IN ('down','up') AND ts > ?
	ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END LIMIT 1`

// closeHook runs inside resolveDanglingDowns once the close has read which
// outages have no end and searched the samples for each, before it writes the
// first end it found. Test seam only (nil in production), like pruneChunkHook:
// it holds a close at the one point where another change to the outage record
// would make its writes wrong.
var closeHook func()

// pruneClock reads the wall clock and the store's uptime (sinceOpen) as one
// pair. A single seam so a test can make the two disagree, which is the whole
// point: no wall reading can be checked against itself. The deferred pause
// repair takes its uptime from here too (repairReading), so the frame it
// judges in and the guard that vets it read the same clock.
var pruneClock = func(s *Store) (time.Time, time.Duration) {
	return time.Now(), s.sinceOpen()
}

// bootClockFn is bootClock, a var so a test can stand in for a machine that
// slept: the boot clock moving on while Go's monotonic clock stands still is
// the whole subject, and no test can put a real computer to sleep.
var bootClockFn = bootClock

// sinceOpen is the store's uptime as the clock guard reads it: how long since
// Open, by a clock that nothing can set and that keeps counting while the
// machine sleeps (bootClock: CLOCK_BOOTTIME on Linux, CLOCK_MONOTONIC on macOS).
//
// It used to be time.Since(s.opened), Go's monotonic clock, and on macOS and
// Linux that clock stands still while the machine sleeps (mach_absolute_time
// and CLOCK_MONOTONIC; `go doc time` warns of it). The wall clock does not: a
// wake puts the time asleep back into it. So clockStepped read a lid closed
// for the night as the wall clock jumping ahead by the night. Any sleep longer
// than pruneClockStepSlack - or several shorter ones adding up, because the
// baseline moves only when a step is found - parked cleanup until another
// pruneClockSettle of AWAKE time had passed, counted db.prune_skipped_clock at
// every hourly try, parked the deferred pause repair, and then re-judged the
// pauses for a step that never happened. The next sleep started the wait over,
// so a laptop that never stayed awake six hours at a stretch did not clean up
// again until the daemon restarted. The monitor notes the same blind spot: "a
// step and a suspend leave identical evidence" - on Go's clock they do. On
// this one they do not: a wake moves the wall clock and the boot clock by the
// same amount, and setting the clock moves the wall clock alone.
//
// So a real step still shows: NTP stepping the clock, a VM resumed from a
// snapshot on its old time until the guest agent sets it, a hypervisor's
// time-sync glitch, a bad RTC corrected after boot. Each sets the wall clock
// and leaves the boot clock where it was. What this cannot see is a wake that
// reports the wrong length of sleep, which moves both. The kernel takes that
// length from the hardware clock that keeps time through the sleep, and the
// monitor books the same length as unobserved time.
//
// Windows needs none of it: Go's clock there counts sleep already (see
// bootclock_other.go). If the boot clock cannot be read at Open, the store
// uses Go's clock throughout, as the guard did before. If it was read at Open
// and a later reading fails, that one reading uses Go's clock, which lacks the
// sleep since Open: for that reading the guard is as blind to sleep as it used
// to be, and can take a sleep for a step, or miss a backward step about as
// long as the sleep. The ways these calls are known to fail - a kernel without
// the clock, a sandbox that refuses the call - fail every time, not now and
// then, and land in the first case.
//
// Inside a virtual machine the boot clock is the guest's, and the guest does
// not sleep when its host does: its clocks, this one among them, just stop
// while the host sleeps, and at wake its time sync sets the wall clock forward
// by the time lost. To this guard that is a step, and it is one: the clock was
// set. So pingularity under Docker Desktop, WSL2 or Lima on a laptop still
// parks cleanup after each sleep of the laptop, for pruneClockSettle of the
// laptop's awake time, and counts db.prune_skipped_clock for it, as every
// sleep did before. Running directly on the laptop is what avoids it.
func (s *Store) sinceOpen() time.Duration {
	if s.openedBootOK {
		if now, ok := bootClockFn(); ok {
			return now - s.openedBoot
		}
	}
	return time.Since(s.opened)
}

// pruneClockStepSlack is how far the wall clock may run ahead of the store's
// uptime before destructive pruning parks itself, and pruneClockSettle is how
// much steady uptime it then wants before believing the clock again.
//
// The slack is sized to swallow ordinary hygiene - NTP slew, a leap second, the
// scheduling gap between reading the clock and running the DELETE - while being
// two orders of magnitude below the smallest default retention window (30d), so
// a step big enough to shorten a window is always caught. The settle window is
// long enough to cover several hourly attempts, giving time sync room to land.
//
// The settle window is measured by the same uptime, so it counts the time the
// machine sleeps. On Windows it always did. On macOS and Linux it counted
// awake time alone until the uptime moved to the boot clock (sinceOpen), and
// counting sleep there too is on purpose: a step found before a night's sleep
// is believed at the first pass after the wake, not after six more awake hours.
// What that gives up is awake time in which time sync could take a bogus step
// back, and it still gets an hour of that after every step. A step is found BY
// a cleanup pass, and the next pass comes only after another hour awake,
// because the pruner's ticker runs on Go's clock, which stands still while the
// machine sleeps (runPruner in main.go). Time sync runs again as the network
// comes back at wake and normally lands well inside that hour. A correction it
// makes is a step like any other and starts the wait over; a step still in
// place at that pass is believed, as it would be on Windows.
//
// Neither can save a clock that boots fast and is NEVER corrected: nothing on
// the machine can tell that apart from a correct one, and refusing forever would
// just trade lost history for an unbounded database. It buys the time in which a
// networked host - which a monitoring daemon is by definition - gets fixed.
const (
	pruneClockStepSlack = 15 * time.Minute
	pruneClockSettle    = 6 * time.Hour
)

// clockStepped reports whether the wall clock has jumped relative to the
// store's uptime, and parks destructive pruning when it has. Reading now
// against the baseline captured at open (or at the last step) is what makes a
// jump visible: the uptime cannot be stepped and counts sleep (sinceOpen), so
// wall progress exceeding uptime progress is the clock moving under us rather
// than time passing, awake or asleep.
//
// Once it has found a step it keeps pruning parked until the uptime is
// pruneClockSettle past it. That wait is on the same clock, so it counts sleep
// too: a change from Go's clock, on which macOS and Linux counted awake hours
// alone. pruneClockSettle says why that is acceptable.
//
// A detected step RE-BASELINES rather than latching. Otherwise the honest case
// poisons itself: an RTC-less board boots near 1970, the floor guard above skips
// while it is implausible, then NTP steps it forward by decades - a legitimate
// correction that a latching guard would read as corruption and refuse to prune
// on forever.
func (s *Store) clockStepped(now time.Time, uptime time.Duration) bool {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	// Round(0) strips the monotonic reading so Sub compares WALL time; without it
	// Go silently answers with the monotonic delta and the drift is always zero.
	drift := now.Round(0).Sub(s.clockBase) - (uptime - s.clockBaseUp)
	if drift < 0 {
		drift = -drift // a backward step is no more trustworthy than a forward one
	}
	if drift > pruneClockStepSlack {
		s.clockBase, s.clockBaseUp = now.Round(0), uptime
		s.clockSettleUp = uptime + pruneClockSettle
		// A detected step is the bounded trigger for re-judging quarantined
		// pauses - the judgement Open ran under a plausible-but-stale clock (a
		// batteryless board a year behind) wrongly held genuine rows, and
		// nothing re-ran it until restart. But do NOT arm it yet: this same
		// branch just declared the post-step clock untrusted for destructive
		// pruning, and re-judging history is the same bargain - a temporary
		// bogus step (VM snapshot resume, hypervisor sync glitch) could
		// quarantine genuine pauses or restore held garbage. Note the step;
		// the arm happens below, once the clock survives the settle window.
		// A reverting step lands back in this branch and keeps deferring, so
		// the judgement runs once, under whatever clock the settle vouches for.
		s.pauseStepSeen = true
		return true
	}
	if uptime < s.clockSettleUp {
		return true
	}
	// Steady through the settle window after a step: the clock has earned the
	// re-judgement. Arming is once per correction, not once per write, so a
	// crafted far-future row costs one repair transaction per real clock
	// change - not the every-probe-round loop the old restart-only rule
	// guarded against.
	if s.pauseStepSeen {
		s.pauseStepSeen = false
		// The reading the settle window vouched for, and the uptime read with it.
		s.pauseVetted, s.pauseVettedUp = now.Round(0), uptime
		s.pauseRepairArm.Add(1)
	}
	return false
}

// repairReading picks the clock the deferred pause repair may judge with, or
// reports that no reading is currently trustworthy (a detected step is still
// settling - every generation parks, not only the one that step would arm).
// Once any step has settled, its vetted reading - advanced by the uptime since,
// which no later wall step can bend - is the judging frame, and the
// caller-supplied reading is ignored; before any step, the fallback (the
// caller's clock, plausibility-gated by the caller) is all there is.
//
// The frame used to advance by time.Since(pauseVetted): Go's monotonic clock,
// which stands still while macOS and Linux sleep, so every hour asleep left the
// frame an hour further behind the real time. Once it was more than
// pauseRepairFutureSkew (48 hours) behind, a repair - armed by a restore that
// brought held rows, or retried after one that failed - took the newest
// genuine pauses for rows from the future and held them aside, the pause that
// recorded the sleep among them, so that stretch counted as watched. A repair
// in the first hour after a long weekend with the lid shut could do it. And
// with sleep no longer mistaken for a step (sinceOpen), no settle re-vets the
// frame after a sleep any more, so the lag would have added up over every
// night since the last real step. The uptime counts the sleep, so the frame
// keeps pace with the wall clock without trusting it.
func (s *Store) repairReading(fallback func() int64) (int64, bool) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	if s.pauseStepSeen {
		return 0, false
	}
	if !s.pauseVetted.IsZero() {
		_, uptime := pruneClock(s)
		return s.pauseVetted.Add(uptime - s.pauseVettedUp).Unix(), true
	}
	return fallback(), true
}

// futureHorizon is the bound of a cleanup's future arms: pruneFutureSlack ahead
// of the clock a pass started on, moved on by the time the pass has run (see
// PruneLive, which says why it is the pass's uptime and not a later reading of
// the clock). One text for the pass and for the count of what the next pass
// removes as ahead of the clock (PruneDueAhead), which asks for a pass that
// has not run yet.
func futureHorizon(start time.Time, ran time.Duration) int64 {
	return start.Add(ran + pruneFutureSlack).Unix()
}

// pruneCut names which of Prune's three cutoffs a table is pruned on.
type pruneCut int

const (
	pruneOnSamples pruneCut = iota // the latency retention
	pruneOnSpeed                   // the speed retention
	pruneOnEvents                  // the outage (downtime) retention
)

// at picks the cutoff this names from the three a pass cuts at, or PruneDue
// counts at, in unix seconds.
func (c pruneCut) at(samplesBefore, speedBefore, eventsBefore time.Time) int64 {
	switch c {
	case pruneOnSpeed:
		return speedBefore.Unix()
	case pruneOnEvents:
		return eventsBefore.Unix()
	}
	return samplesBefore.Unix()
}

// pruneAge is each table's retention rule as Prune applies it: the cutoff the
// table rides, and which of its rows that cutoff removes, with ?1 standing for
// the cutoff. Prune builds its DELETEs from these texts and PruneDue its
// counts, so what a restore is told the next cleanup removes is what the next
// cleanup removes. The two used to be separate: the restore compared the
// oldest START it had seen with the cutoff, and so warned about a straddling
// outage, an open one and a pause running into the window, which the cleanup
// keeps. PruneLive's own comments say why each rule is what it is.
//
// The future arms are not here. They are about a clock that ran fast, not
// about retention, and PruneDueAhead counts them, by pruneAheadCol.
var pruneAge = map[string]struct {
	on   pruneCut
	rule string
}{
	"samples": {pruneOnSamples, `ts < ?1`},
	// DNS samples share the latency retention.
	"dns": {pruneOnSamples, `ts < ?1`},
	// A speedtest's span has length, so it goes once its END is past the cutoff.
	"speed_spans": {pruneOnSamples, `ts < ?1 AND ts + duration_s < ?1`},
	"speed":       {pruneOnSpeed, `ts < ?1`},
	// The selection reports ride the speed retention on their run's ts.
	"speed_servers": {pruneOnSpeed, `run_ts < ?1`},
	// Whole outages: a 'down' goes only with a recovery that is past the cutoff
	// too, so a straddling outage and an open one are kept.
	"events": {pruneOnEvents, `ts < ?1 AND NOT (type = 'down'
		AND (SELECT MIN(u.ts) FROM events u WHERE u.type = 'up' AND u.ts > events.ts) >= ?1)`},
	// Whole spans: a pause goes once its END is past the cutoff.
	"pauses": {pruneOnEvents, `ts + duration_s < ?1`},
}

// Prune is PruneLive at cutoffs no window change moves: it cuts at the three
// it is handed from its first chunk to its last.
func (s *Store) Prune(ctx context.Context, samplesBefore, speedBefore, eventsBefore time.Time) (int64, error) {
	return s.PruneLive(ctx, func(time.Time) (time.Time, time.Time, time.Time) {
		return samplesBefore, speedBefore, eventsBefore
	})
}

// PruneLive deletes old rows - and future-stamped ones beyond pruneFutureSlack.
// Each table has its own cutoff so latency samples, speed history, and outage
// events can be retained for different windows. Returns total rows removed.
//
// cutoffs gives the latency, speed and outage cutoffs that the retention
// windows in force give a pass that started at start: the start less each
// window, or the epoch for a window that keeps forever (settings.PruneCutoff).
// The pass asks it once as it starts and cuts at what it answers. It used to
// be handed those cutoffs and keep them to its end, and a pass over a big
// backlog runs for minutes. A window raised in that time - the fix for one
// lowered by mistake, and what a restore's warning advises for restored rows
// past a window - left the pass deleting what the raised window keeps until it
// ended, rows restored meanwhile among them, and a restore's reply cannot see
// what a pass deletes after it.
//
// So a pass asks again before every chunk it deletes by age, and cuts that
// chunk at the earlier of the cutoff it started at and the one it is given
// now (pruneSweep.cut). A window raised, or set to keep forever, stops it
// deleting what the new window keeps from its next chunk on. The answer is
// asked for the pass's start, not for the moment of asking, so the raise
// counts in full however long the pass has run. A lowered window waits for
// the next pass, as it always has: a pass never cuts later than it started
// at, and never deletes more than it set out to. That also keeps every chunk
// inside what the close of outages at its start (resolveDanglingDowns, below)
// was done for. The future horizon is the pass's own, and no window moves it:
// it is pruneFutureSlack ahead of the clock the pass started on, moved on by
// the pass's uptime, sleep included (see ahead, below).
//
// A window that moves while the pass is between two tables that share it
// leaves them cut at different cutoffs: a raise between the speed runs and
// their selection reports keeps the reports of runs already deleted, and one
// between the outages and the paused time keeps paused time beside outages
// already deleted, for as long as the new window keeps them. That is the
// state a pass stopped between the two tables leaves (see the speed_servers
// arm), and it lasts longer only because the operator asked to keep more.
//
// cutoffs is asked under outageMu for the chunks of old outages, so it must
// take no lock of this package and wait for nothing that could hold one: it
// reads the windows, and that is all.
//
// The deletes run in chunks of at most pruneChunkRows rows, with a wait of
// pruneChunkPause after every full one, so probe writes get the writer in
// between however big the backlog is. Every chunk commits on its own. A pass
// that fails or is stopped part way returns the rows it did remove, and the
// next pass removes the rest by the same rule.
//
// A pass that removed walTrimRows or more ends by emptying the write-ahead
// log (trimWALAfter). Whatever comes of that, the pass has succeeded.
//
// A pass that would start while a restore is bringing rows in does not start:
// it returns ErrCleanupHeld and deletes nothing (HoldCleanup).
func (s *Store) PruneLive(ctx context.Context, cutoffs func(start time.Time) (samplesBefore, speedBefore, eventsBefore time.Time)) (int64, error) {
	start, uptime := pruneClock(s)
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
	// The mirror of that guard, which used to be missing. A clock reading AFTER
	// plausibleEpoch was trusted unconditionally, so one reading merely FAST -
	// a garbage RTC, a restored VM snapshot, a hypervisor time-sync glitch -
	// pushed every cutoff below past the newest genuine row and the `ts < before`
	// arm deleted real latency, DNS, speed, event and pause history. Permanently,
	// while reporting success: a later NTP correction restores nothing, and
	// because the pauses go too, unobserved time becomes observed and uptime
	// reads INFLATED rather than obviously broken.
	//
	// Same conclusion repairFutureReachingPausesAt already reaches about the same
	// clock ("not good enough for a permanent verdict") - this is that principle
	// applied to DELETE, and the same bargain as the floor above: the pruner
	// retries hourly, so waiting costs a delayed cleanup and acting costs history.
	if s.clockStepped(start, uptime) {
		stats.Inc("db.prune_skipped_clock")
		return 0, nil
	}
	// A restore is bringing rows in (HoldCleanup), so this pass does not start.
	// Like the two guards above it costs a delayed cleanup: the next pass
	// removes what this one would have.
	if s.cleanupHolds.Load() > 0 {
		stats.Inc("db.prune_skipped_restore")
		return 0, ErrCleanupHeld
	}
	// The cutoffs the pass starts at: the windows in force, measured from the
	// clock reading the guards above have just vetted.
	samplesBefore, speedBefore, eventsBefore := cutoffs(start)
	// A settle-completing clockStepped (above) ARMS the deferred pause
	// re-judgement and returns false, letting this same Prune proceed. Run the
	// judgement HERE, before the destructive sweep below deletes future-reaching
	// pause rows outright: the repair MOVES them to pauses_quarantine (which the
	// sweep never touches), so a later correction can restore them. Without this
	// the settle-completing prune armed the repair and then, in the same call,
	// deleted the very rows it would have quarantined. A no-op fast path (two
	// atomic loads) when nothing is armed.
	s.maybeRepairFuturePauses()
	// A cleanup is a writer, and it dates recoveries from the samples. The
	// waiting rounds go in first. If they cannot, the cleanup goes on: what
	// it removes is old, and what waits is new.
	s.SaveBuffered(SaveOrder)
	// Each table's retention arm is its rule in pruneAge, on the cutoff that
	// rule names: the earlier of the one the pass started at and the one the
	// windows in force give it now (see above). pruneChunked asks before every
	// chunk.
	cut := func(table string) int64 {
		on := pruneAge[table].on
		return min(on.at(samplesBefore, speedBefore, eventsBefore), on.at(cutoffs(start)))
	}
	// The future arms' horizon: pruneFutureSlack ahead of the clock, asked for
	// before every chunk of theirs. It used to be worked out here, once, from
	// start. But a pass over a big backlog runs for minutes and hands the
	// writer to the monitor between chunks, and its waits are Go timers, which
	// stand still while macOS and Linux sleep (pruneWait; pruneClockSettle says
	// the same of the pruner's ticker). A pass caught by the lid closing went
	// on after the wake with the horizon of the day it started. After more
	// than pruneFutureSlack asleep, everything the monitor had written since
	// the wake was past it, and the future arm of every table the pass had not
	// reached yet deleted it: the rounds, the outage the reconnect made, a
	// speedtest.
	//
	// So the horizon moves on by the pass's own uptime, on the clock that
	// counts sleep and that nothing can set (sinceOpen): the same frame the
	// pause repair judges in (repairReading). Never by a fresh wall reading. A
	// clock set back while the pass runs would pull the horizon back with it
	// and delete rows that were inside it when the pass started, and the guard
	// above vetted the reading the pass started on, not any later one.
	//
	// The wall clock is read all the same, to see that it still agrees with
	// the uptime. When the two have come apart by more than the guard's slack
	// since the pass started, the clock was set under it: by time sync, or at
	// wake inside a virtual machine, whose clocks stop while the host sleeps
	// (sinceOpen). What the monitor writes from then on is stamped by a clock
	// this pass never vetted, so the future arms end there, quietly, and
	// delete nothing more as ahead of the clock. The next pass judges the
	// clock again as it starts: a step still past the guard's slack is counted
	// (db.prune_skipped_clock) and waited out, a smaller one is trusted. The
	// two measure from different points, this one from the pass's start and
	// the guard from its own baseline (clockStepped), so a step that ends a
	// pass's future arms can leave the clock inside the guard's slack. The
	// rows ahead of the clock go when a pass runs on a clock it trusts. An
	// uptime that reads less than it did at the start ends the future arms
	// too: a boot clock that could not be read this once (sinceOpen falls back
	// to Go's clock, which lacks the sleep since Open). And where the uptime
	// is Go's clock throughout, for want of a boot clock at Open, a sleep
	// under the pass reads as a step here, as it does to the guard. The
	// retention arms go on either way. They cut at the cutoffs of the vetted
	// start and never later, whatever the clock does next.
	ahead := func() (int64, bool) {
		now, up := pruneClock(s)
		ran := up - uptime
		// Round(0) for the reason clockStepped gives: wall time, not Go's
		// monotonic reading.
		drift := now.Round(0).Sub(start.Round(0)) - ran
		if drift < 0 {
			drift = -drift
		}
		if ran < 0 || drift > pruneClockStepSlack {
			return 0, false
		}
		return futureHorizon(start, ran), true
	}
	// Close any dangling 'down' whose recovery lives only in the samples about to
	// be deleted, so pruning can't turn it into phantom downtime. Not the running
	// monitor's own: the line Delete now keeps away from keeps this away too,
	// because a short window puts the cutoff within reach of an outage the
	// monitor still holds (see resolveDanglingDowns for what that gives up).
	//
	// At the cutoff the pass started at, which is the latest it can cut the
	// samples at: a window raised and then lowered again while it runs takes its
	// chunks back up to it, and a close at the raised window's cutoff would have
	// left the outages the samples between the two proved with no end once those
	// went. When the raise stands, the samples an outage was closed from stay,
	// and so does the end: it is the second those samples prove, with the length
	// they give it (paused time taken out), what the readers work out from the
	// samples themselves (firstQuorumRecovery), so every figure reads as it did.
	// A pass stopped after this close has always left the same.
	if err := s.resolveDanglingDowns(ctx, samplesBefore.Unix(), start.Unix(), s.live.line); err != nil {
		// A failed write the close has counted already (recordedErr).
		if !errors.As(err, new(recordedErr)) {
			recordDBErr(err)
		}
		return 0, err
	}
	sw := &pruneSweep{budget: pruneChunkRows, cut: cut, ahead: ahead}
	// From the first DELETE on, rows may be gone whatever this call goes on to
	// return: every chunk commits on its own. So a failure or a shutdown after
	// that point drops the read caches too, the way a chunked import does. A
	// reader in the middle of a pass can memoize an answer from a half-pruned
	// table, and nothing else would ever drop it.
	fail := func(err error) (int64, error) {
		s.invalidateReadCaches()
		return sw.total, err
	}
	// Two arms per table, the cutoff and the future horizon, not one statement
	// with an OR: with the OR the planner walks the whole ts index to honour
	// the ORDER BY, and the row limit needs that order. pruneChunked hands each
	// statement its arm's bound and then the limit: in the retention arm ?1 is
	// the cutoff and the limit is ?2.
	for _, table := range [...]string{"samples", "dns", "speed"} {
		for _, arm := range [...]struct {
			kind  pruneArm
			where string
		}{{pruneByAge, pruneAge[table].rule}, {pruneAhead, `ts > ?`}} {
			if err := s.pruneChunked(ctx, sw, table, arm.kind, nil, `DELETE FROM `+table+` WHERE rowid IN (
				SELECT rowid FROM `+table+` WHERE `+arm.where+` ORDER BY ts, rowid LIMIT ?)`); err != nil {
				return fail(err)
			}
		}
	}
	// The selection reports ride the speed retention (their run_ts IS speed.ts;
	// no FK anywhere, so the cascade is manual). Keyed on its own column with
	// both arms of the same cut - cutoff and future horizon - rather than a
	// join to speed: a crash between the two DELETEs leaves only bounded
	// orphans that the next hourly pass removes by the same rule. A pass
	// stopped between two chunks leaves the same bounded orphans.
	for _, arm := range [...]struct {
		kind  pruneArm
		where string
	}{{pruneByAge, pruneAge["speed_servers"].rule}, {pruneAhead, `run_ts > ?`}} {
		if err := s.pruneChunked(ctx, sw, "speed_servers", arm.kind, nil, `DELETE FROM speed_servers WHERE rowid IN (
			SELECT rowid FROM speed_servers WHERE `+arm.where+` ORDER BY run_ts, rowid LIMIT ?)`); err != nil {
			return fail(err)
		}
	}
	// The speedtest times ride the LATENCY retention: the latency chart's hover
	// note is their only reader, and it has no point to hover once the samples
	// they cover are gone. Like a pause they have length, so a span goes only
	// once its END is past the cutoff; one that runs into the kept window stays
	// until it does. The future arm is the same horizon every table gets.
	if err := s.pruneChunked(ctx, sw, "speed_spans", pruneByAge, nil, `DELETE FROM speed_spans WHERE rowid IN (
		SELECT rowid FROM speed_spans WHERE `+pruneAge["speed_spans"].rule+` ORDER BY ts, rowid LIMIT ?)`); err != nil {
		return fail(err)
	}
	if err := s.pruneChunked(ctx, sw, "speed_spans", pruneAhead, nil, `DELETE FROM speed_spans WHERE rowid IN (
		SELECT rowid FROM speed_spans WHERE ts > ? ORDER BY ts, rowid LIMIT ?)`); err != nil {
		return fail(err)
	}
	// Events (outage transitions) prune as WHOLE outages: a 'down' older than the
	// cutoff whose paired 'up' is at/after it straddles the boundary, so keep it -
	// deleting only the 'down' would orphan the 'up' and split one outage into
	// phantom downtime the uptime math then miscounts (audit: whole-outage pruning).
	// Everything else < cutoff (complete past outages) and any future-stamped event
	// still goes. A dangling 'down' (no 'up' yet) is kept, like an in-progress outage.
	//
	// In chunks the same rule has to hold after EVERY chunk, not only after the
	// last: a pass can be stopped between two, and the wait between two is long
	// enough for a reader. So a chunk is not cut at a row count. It ends on a
	// recovery: b is the ts of the Nth 'up' under the cutoff, oldest first, and
	// the chunk takes every row the rule lets go up to and including it. A
	// 'down' that may go has its 'up' after it and under the cutoff, so it goes
	// no later than the chunk that takes that 'up'. Cut by rows instead, the
	// 'down' could go alone, and the 'up' left behind reads as an outage that
	// started at ts - duration_s: later and shorter than the real one whenever a
	// pause sat inside it.
	//
	// The limit counts recoveries, so a chunk can remove about twice as many
	// rows as it names, a 'down' for every 'up'. These are small rows, two for
	// an outage. With fewer than N recoveries left b is the cutoff itself and
	// the statement is the unchunked one. It reads every row under the cutoff
	// each time, which is fine for a table this size and would not be for
	// samples. The rule itself is pruneAge's, with the chunk's bound added.
	//
	// ?1 is the cutoff in the rule and in b alike, one value for the chunk, so
	// every chunk is the rule at one cutoff, ending on a recovery under it. The
	// cutoff can differ from one chunk to the next, when a window is raised or
	// the raise undone (see PruneLive), and that is safe, because what keeps a
	// chunk whole does not lean on the chunks before it. A 'down' the rule lets
	// go at a chunk's cutoff has its 'up' after it and under that cutoff, so it
	// goes no later than the chunk that takes that 'up', as above, whatever the
	// chunks before were cut at. And one kept at a later cutoff as a straddler,
	// its recovery at or past that cutoff, straddles every earlier one too.
	//
	// Both arms run under outageMu, like every change to the outage record but
	// the monitor's. A Delete now's close that read an outage before a chunk
	// here took it would write that outage's end afterwards, with no outage
	// left for it, and one about to move an 'up' dated past the horizon back
	// would find it gone and write no end at all (see outageMu). The close
	// above has let go of the lock by now. A close that comes during the sweep
	// waits for it, and the sweep is short: two rows an outage, and a chunk is
	// rarely full.
	sweepEvents := func() error {
		s.outageMu.Lock()
		defer s.outageMu.Unlock()
		if err := s.pruneChunked(ctx, sw, "events", pruneByAge, nil, `
			WITH b(ts) AS (SELECT COALESCE(
				(SELECT ts FROM events WHERE type = 'up' AND ts < ?1 ORDER BY ts, rowid LIMIT 1 OFFSET ?2 - 1), ?1))
			DELETE FROM events
			WHERE (`+pruneAge["events"].rule+`)
			  AND ts <= (SELECT ts FROM b)`); err != nil {
			return err
		}
		// The future arm is cut by rows like every other table's. Nothing that
		// reads the present looks past currentHorizon, and these rows are further
		// out than that, so no reader can tell where a chunk of them ended.
		return s.pruneChunked(ctx, sw, "events", pruneAhead, nil, `DELETE FROM events WHERE rowid IN (
			SELECT rowid FROM events WHERE ts > ? ORDER BY ts, rowid LIMIT ?)`)
	}
	if err := sweepEvents(); err != nil {
		return fail(err)
	}
	// Pause spans share the outage retention (they are the uptime DENOMINATOR), and
	// like an outage they have LENGTH - so they prune whole, on the same rule the
	// events sweep above uses. A row is dropped only once its whole span
	// (ts + duration_s) has fallen past the cutoff; one that starts before it but
	// runs INTO the retained window is kept. `ts < cutoff` deleted such a row whole,
	// and every unobserved second it covered inside the window silently became
	// observed-and-up: coverage jumps toward 1.0 and the uptime% moves, with no data
	// change at all. This is not a rare alignment - runPruner ticks hourly and the
	// cutoff sweeps forward continuously, so EVERY pause row met that DELETE while
	// most of its span was still in-window. Live pauses checkpoint every ~5 minutes
	// (monitor.pauseCheckpoint) so each lost at most that, but the startup-gap row -
	// one span for a whole process-down stretch - could lose months.
	//
	// Keep the straddling row rather than splitting it at the cutoff. Keeping is
	// bounded: pausedOverlap already clamps every span to the window it is asked
	// about, so a retained pre-cutoff prefix is never counted anywhere, and the row
	// still goes at the first tick after its END passes the cutoff - at most a
	// handful of rows outlive retention at a time, exactly as the straddling 'down'
	// above does. Splitting would rewrite ts, which is the merge key export/import
	// dedups pause rows on (see exportTables): a split row re-imported next to the
	// unsplit original in an older backup would land as a SECOND overlapping span
	// and double-subtract that time from the denominator.
	// The future-reaching half (ts > horizon) is deleted only when the deferred
	// pause repair is HEALED. If the repair above failed or parked (still armed),
	// deleting those rows now would destroy exactly what the repair would have
	// quarantined for a later clock correction - so leave them and let the next
	// prune (hourly) retry the repair first. The retention-floor half (rows whose
	// END predates the window) is always safe to sweep.
	if err := s.pruneChunked(ctx, sw, "pauses", pruneByAge, nil, `DELETE FROM pauses WHERE rowid IN (
		SELECT rowid FROM pauses WHERE `+pruneAge["pauses"].rule+` ORDER BY rowid LIMIT ?)`); err != nil {
		return fail(err)
	}
	// Asked before every chunk, not once: a restore landing between two chunks
	// arms a new judgement, and the rows it would hold aside are the ones this
	// arm deletes.
	deferred := false
	healed := func() bool {
		if !s.pauseRepairArmed() {
			return true
		}
		if !deferred {
			deferred = true
			stats.Inc("db.prune_pauses_future_deferred")
		}
		return false
	}
	if err := s.pruneChunked(ctx, sw, "pauses", pruneAhead, healed, `DELETE FROM pauses WHERE rowid IN (
		SELECT rowid FROM pauses WHERE ts > ? ORDER BY ts, rowid LIMIT ?)`); err != nil {
		return fail(err)
	}
	// Pruning deleted rows (and resolveDanglingDowns may have written a synthetic
	// 'up'), so drop the memoized quorum scans and cached chart aggregates. A
	// fixed-window Series cache would otherwise keep serving pruned rows for up to
	// bucketSec/4 (~88 min on the 1-year bucket).
	s.invalidateReadCaches()
	stats.Inc("db.prune_count")
	// The whole pass, the waits between chunks included: a big cleanup reads
	// as the minutes it took.
	stats.AddF("db.prune_ms_sum", util.DurMS(time.Since(start)))
	// Last, and outside the cleanup's own time: the rows are gone whatever
	// happens to the log. The count is the whole pass's, every chunk of every
	// table. A pass that failed or was stopped returned above and leaves the
	// log to the size limit. The wait first is the one every full chunk gets:
	// a probe write that found the writer taken by the last chunk gets its
	// look at it before the trim can take it.
	s.trimWALAfter(ctx, sw.total, pruneWait)
	return sw.total, nil
}

// RowMark is where table stands now, so PruneDue can tell the rows stored after
// it from the rows that were already there: the table's highest rowid, or 0
// when it is empty. SQLite gives a new row the rowid after the highest one in
// its table, and nothing renumbers these tables (none has an INTEGER PRIMARY
// KEY or AUTOINCREMENT, and nothing here runs VACUUM), so every row stored
// later has a higher one. MAX(rowid) is one seek to the end of the table. The
// rounds waiting in memory are this install's own, so they are written first
// and land under the mark - the restore's first batch would write them first
// anyway, before it merges a row.
//
// The way round it is a rowid handed out again, which needs every row above it
// deleted first: the table's newest rows, all of them, after the mark.
//
// Delete now does exactly that (Clear). It empties the table, and the next row
// stored gets rowid 1. Pressed while a restore was still bringing rows of that
// kind in, it left every restored row that landed afterwards on a rowid at or
// below the mark, out of PruneDue's count, and with a mark in the millions
// that was all of them: the reply said nothing and the next cleanup deleted
// them. So a Delete now is noted in every watch open on the cleanup
// (PruneTally.Cleared), and a restore counts a table that was emptied after
// its watch opened from mark 0: every row then in it was stored after the
// Delete now, the restore's and the daemon's own new ones, and the new ones
// are under no window.
//
// The other is a cleanup that was running when the restore began and removes
// the newest rows of a small table that had nothing but old rows at its end,
// which only an earlier restore of old rows leaves behind (the daemon's own
// writes keep the newest latency rows current, and no pass starts during a
// restore: HoldCleanup). The rows that come in on the old rowids are then left
// out of PruneDue's count; nothing is ever counted that was not stored after
// the mark.
//
// The last is the delete of one outage from the list (DeleteOutage), when it
// is the newest outage on record and no restored outage row has landed yet:
// its rowids are the table's highest, and the first restored rows take them
// again, at or below the mark. That one is not counted either. It is two rows
// at most, and it takes the operator deleting the newest outage in the moment
// between the mark and the restore's first batch of outages.
func (s *Store) RowMark(ctx context.Context, table string) (int64, error) {
	if _, ok := exportTables[table]; !ok {
		return 0, fmt.Errorf("row mark: unknown table %q", table)
	}
	s.SaveBuffered(SaveOrder)
	var mark sql.NullInt64 // NULL when the table is empty
	err := s.db.QueryRowContext(ctx, `SELECT MAX(rowid) FROM `+table).Scan(&mark)
	return mark.Int64, err
}

// PruneDue counts, table by table, the rows the next Prune at these cutoffs
// removes by retention, among the rows each table gained after its RowMark in
// since: what a restore is about to lose. It counts by pruneAge, the rules
// Prune deletes by, so a straddling outage, an open one, and a pause that runs
// into the window are counted exactly when the cleanup would delete them. The
// future arms are not counted here: PruneDueAhead counts those.
//
// Like Prune it first runs any re-judgement of held pauses that is owed. A
// restore that brought held rows (pauses_quarantine) arms one, the next probe
// round runs it anyway, and it is what hands a held row whose span is plainly
// in the past back to pauses, where the pause rule then applies to it. Run
// here, it happens before the count rather than beside it, and the held rows
// it leaves are the ones no cleanup removes.
//
// The other repair Prune makes first is counted too: closing an outage that
// never got its recovery, from the samples about to be deleted
// (resolveDanglingDowns). PruneDue used to leave it out and count such an
// outage as the open one it still is, which the cleanup keeps. But the cleanup
// does not keep it. It gives the outage an end at the second the old samples
// show, and where that second is past the outage cutoff as well, its sweep
// then deletes the outage whole, in the same pass. A backup from an install
// that kept its latency forever and restarted in the middle of an outage,
// restored onto the default windows, is that case: the reply named the old
// readings and said nothing of the outage the next cleanup deleted. So
// PruneDue asks the close for a dry run (closeDanglingDowns), at the latency
// cutoff it is counting at, and counts the outage rules as they stand once
// those ends are written (pruneDueAfterCloses). The end the close writes is a
// row of the daemon's, not the restore's, and is not counted; an 'up' it moves
// back from ahead of the clock is a row that was there, and is.
//
// The rows after a mark can also put rows from before it on the cleanup's
// list: a restored recovery can end one of this install's own old outages past
// the cutoff, and Prune deletes outages whole. Those are this install's rows,
// not the restore's, so PruneDue leaves them out, and PruneDueClosedOutages
// counts them.
//
// A table missing from since is not counted, and neither is one Prune never
// removes rows from by age (pauses_quarantine). The rows the daemon writes
// itself after a mark are new, so they are only past a window shorter than the
// time since the mark. The one exception is an end a close wrote after the
// mark - a latency Delete now in the middle of a restore writes them - which
// is dated at the second its samples show and counted with the restored rows
// when that is past the window: the cleanup deletes it like them. Cheap on a
// big install, because no count reads a whole
// table (TestPruneDueCountsSeek): the latency and speed rules are ranges on
// their table's ts index, which holds only the rows past the cutoff - two
// million of them count in about 150 ms on a laptop - and the outage and span
// rules read only the rows after the mark, the outage one with a seek for each
// row's recovery, which is fine for a table that size.
func (s *Store) PruneDue(ctx context.Context, samplesBefore, speedBefore, eventsBefore time.Time, since map[string]int64) (map[string]int64, error) {
	s.maybeRepairFuturePauses()
	// A reader saves first, though nothing waiting can be counted: the rounds
	// in memory are new, so under no window a restore can take.
	s.SaveBuffered(SaveRead)
	due := make(map[string]int64, len(since))
	for table, mark := range since {
		age, ok := pruneAge[table]
		if !ok {
			continue
		}
		var n int64
		if err := s.db.QueryRowContext(ctx, pruneDueCount(table),
			age.on.at(samplesBefore, speedBefore, eventsBefore), mark).Scan(&n); err != nil {
			return nil, err
		}
		due[table] = n
	}
	if mark, ok := since["events"]; ok {
		closes, err := s.pruneDueCloses(ctx, samplesBefore, eventsBefore, since)
		if err != nil {
			return nil, err
		}
		n, err := s.pruneDueAfterCloses(ctx, closes, eventsBefore.Unix(), mark, false)
		if err != nil {
			return nil, err
		}
		due["events"] += n
	}
	return due, nil
}

// dueClose is an end the close a cleanup begins with would record
// (plannedClose) at a second that is past the outage cutoff too, so that the
// same cleanup's sweep then deletes the outage it ends.
type dueClose struct {
	plannedClose
	// restored: this end is the restore's doing. The outage is a restored row,
	// or a restored sample is among the readings of the second that proves its
	// end, or the outage had an end of its own until a restored 'down' landed
	// inside it (pruneDueOwnNext). The rest are ends the cleanup would have
	// written anyway.
	restored bool
}

// pruneDueOwnNext finds the event that followed a 'down' (?1) before a restore:
// the next one among the rows at or below the events mark (?2), a 'down' first
// where two share a second, as the close pairs them. An 'up' there that the
// clock has reached was that outage's end, and the close had nothing to do
// with it. A restored 'down' between the two takes the 'up' for its own
// outage, the first one then reads as having no end, and the close gives it
// one from this install's own old samples: an end that is the restore's doing
// though no restored reading shows it. The rows between the two are restored
// ones, walked on the ts index: +rowid keeps the planner from reading every
// row up to the mark instead and sorting them (TestPruneDueCountsSeek).
const pruneDueOwnNext = `SELECT ts, type FROM events WHERE type IN ('down','up') AND +rowid <= ?2
	AND ts >= ?1 AND NOT (ts = ?1 AND type = 'down')
	ORDER BY ts, CASE type WHEN 'down' THEN 0 ELSE 1 END LIMIT 1`

// pruneDueCloses is the dry run of the close the next cleanup at these
// cutoffs begins with, kept to the ends that land past the outage cutoff:
// the ones that put rows on that cleanup's list. It passes the close what
// PruneLive passes it: the latency cutoff, and the line that keeps the close
// away from the running monitor's outages (monitorOutages), as it stands now.
// An outage behind that line gets no end from the cleanup, so none is counted
// for it. The monitor can move the line before the cleanup runs, by opening
// an outage or by ending one, and the count is of a cleanup that finds it
// where it is.
func (s *Store) pruneDueCloses(ctx context.Context, samplesBefore, eventsBefore time.Time, since map[string]int64) ([]dueClose, error) {
	// A reader of the samples saves first. The rounds that wait are new, and
	// prove no end past a cutoff.
	s.SaveBuffered(SaveRead)
	// At the clock a pass reads as it starts (pruneClock), which is what bounds
	// the search for a recovery and says which events are ahead of it.
	now, _ := pruneClock(s)
	var plan []plannedClose
	if err := s.closeDanglingDowns(ctx, samplesBefore.Unix(), now.Unix(), s.live.line, &plan); err != nil {
		return nil, err
	}
	var out []dueClose
	for _, c := range plan {
		if c.at >= eventsBefore.Unix() {
			continue // the outage it ends straddles the cutoff, and the sweep keeps it
		}
		d := dueClose{plannedClose: c}
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE type = 'down' AND ts = ?1 AND rowid > ?2)`,
			c.down, since["events"]).Scan(&d.restored); err != nil {
			return nil, err
		}
		if mark, ok := since["samples"]; ok && !d.restored {
			if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM samples WHERE ts = ?1 AND rowid > ?2)`,
				c.at, mark).Scan(&d.restored); err != nil {
				return nil, err
			}
		}
		if !d.restored {
			// An outage of the install's own that had its end before the restore:
			// only a restored 'down' between the two leaves it for the close.
			var at int64
			var kind string
			switch err := s.db.QueryRowContext(ctx, pruneDueOwnNext, c.down, since["events"]).Scan(&at, &kind); {
			case err == nil:
				d.restored = kind == "up" && at <= currentHorizon(now.Unix())
			case errors.Is(err, sql.ErrNoRows):
			default:
				return nil, err
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// pruneDueAfterCloses counts the outage rows the next cleanup deletes only
// because its close first records the ends in closes: the rows after the mark
// for PruneDue, or, with own set, the rows at or below it that go only because
// of the ends that are the restore's doing, for PruneDueClosedOutages.
//
// An end at a second past the cutoff does to the sweep what a restored
// recovery there does: every 'down' before it that the rule keeps today, for
// want of a recovery under the cutoff, has one now. The latest such end
// reaches them all, so one range is counted, the rows the rule does not
// delete as the table stands (pruneDueKeptDowns). For the install's own rows
// the range starts at the latest end the cleanup would have written without
// the restore, since the rows before that one go either way.
//
// And an 'up' the close moves back from ahead of the clock, instead of
// writing a new one, is a row that was there and is past the cutoff once
// moved.
func (s *Store) pruneDueAfterCloses(ctx context.Context, closes []dueClose, cutoff, mark int64, own bool) (int64, error) {
	var from, to int64 // downs in [from, to) get a recovery under the cutoff
	for _, c := range closes {
		switch {
		case own && !c.restored:
			from = max(from, c.at)
		default:
			to = max(to, c.at)
		}
	}
	var total int64
	if to > from {
		if err := s.db.QueryRowContext(ctx, pruneDueKeptDowns(own), cutoff, mark, from, to).Scan(&total); err != nil {
			return 0, err
		}
	}
	for _, c := range closes {
		if c.moved == 0 || (own && !c.restored) {
			continue
		}
		n, err := s.countUpAt(ctx, c.moved, mark, own)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// countUpAt counts the 'up' rows at ts on one side of a mark: the rows a
// close moves when it moves the 'up' at ts back (closeDanglingDowns moves
// every 'up' at that second, by its type and ts).
func (s *Store) countUpAt(ctx context.Context, ts, mark int64, own bool) (int64, error) {
	side := `rowid > ?2`
	if own {
		side = `rowid <= ?2`
	}
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type = 'up' AND ts = ?1 AND `+side, ts, mark).Scan(&n)
	return n, err
}

// pruneAheadCol is, for each table the cleanup deletes from, the column its
// future arm reads: a row goes when that is past the pass's horizon
// (pruneFutureSlack ahead of its start), whatever the retention windows say.
// PruneDueAhead counts by it, and TestPruneDueCountsWhatPruneRemovesByAge
// holds it to what a pass removes.
var pruneAheadCol = map[string]string{
	"samples": "ts", "dns": "ts", "speed_spans": "ts",
	"speed": "ts", "speed_servers": "run_ts",
	"events": "ts", "pauses": "ts",
}

// PruneDueAhead counts, table by table, the rows the next Prune removes for
// being stamped too far ahead of the clock (pruneFutureSlack), among the rows
// each table gained after its RowMark in since. PruneDue leaves these out: it
// counts by retention, and these go whatever the windows are. A restore was
// told nothing of them, and a backup from a machine whose clock ran fast, or
// one restored onto a machine whose clock is behind, lost those rows at the
// next cleanup with a clean reply.
//
// Two kinds of row past the horizon are not deleted for it, and are not
// counted. A pause that far ahead is kept while the re-judgement of held
// pauses is still owed (see PruneLive), and PruneDue, which a restore calls
// first, runs that re-judgement when it can. And the last outage's own 'up'
// is moved back by the close a cleanup begins with, where the samples about to
// go prove an earlier end (closeDanglingDowns): it then goes by age or stays,
// and PruneDue has counted it if it goes. samplesBefore is the latency cutoff
// that close works at.
//
// The horizon is the one a pass starting now begins with, by the clock it
// reads (pruneClock, futureHorizon). A pass moves its own on by the time it
// runs, and ends its future arms when the clock is set under it (PruneLive):
// neither can be known before the pass, and the count is of a pass that
// deletes as it starts. The close is asked with the line that keeps it away
// from the running monitor's outages, as the cleanup asks it (pruneDueCloses).
// Each count is a range on the column's index after the mark
// (TestPruneDueCountsSeek).
func (s *Store) PruneDueAhead(ctx context.Context, samplesBefore time.Time, since map[string]int64) (map[string]int64, error) {
	s.SaveBuffered(SaveRead)
	now, _ := pruneClock(s)
	horizon := futureHorizon(now, 0)
	ahead := make(map[string]int64, len(since))
	for table, mark := range since {
		if _, ok := pruneAheadCol[table]; !ok {
			continue
		}
		if table == "pauses" && s.pauseRepairArmed() {
			continue
		}
		var n int64
		if err := s.db.QueryRowContext(ctx, pruneDueAheadCount(table), horizon, mark).Scan(&n); err != nil {
			return nil, err
		}
		ahead[table] = n
	}
	if mark, ok := since["events"]; ok && ahead["events"] > 0 {
		var plan []plannedClose
		if err := s.closeDanglingDowns(ctx, samplesBefore.Unix(), now.Unix(), s.live.line, &plan); err != nil {
			return nil, err
		}
		for _, c := range plan {
			if c.moved <= horizon {
				continue
			}
			n, err := s.countUpAt(ctx, c.moved, mark, false)
			if err != nil {
				return nil, err
			}
			ahead["events"] -= n
		}
	}
	return ahead, nil
}

// pruneDueAheadCount is PruneDueAhead's statement for one table of
// pruneAheadCol: the rows after the mark past the horizon. ?1 is the horizon
// and ?2 the mark.
func pruneDueAheadCount(table string) string {
	return `SELECT COUNT(*) FROM ` + table + ` WHERE rowid > ?2 AND ` + pruneAheadCol[table] + ` > ?1`
}

// pruneDueKeptDowns is pruneDueAfterCloses' statement: the 'down' rows on one
// side of the mark, from ?3 up to ?4, that the events rule of pruneAge does
// not delete as the table stands. ?1 is the cutoff and ?2 the mark. The rule
// is pruneAge's own text, so what "kept today" means follows it.
func pruneDueKeptDowns(own bool) string {
	side := `rowid > ?2`
	if own {
		side = `rowid <= ?2`
	}
	return `SELECT COUNT(*) FROM events WHERE ` + side + ` AND type = 'down' AND ts >= ?3 AND ts < ?4
		AND (` + pruneAge["events"].rule + `) IS NOT TRUE`
}

// PruneDueClosedOutages counts the outage records this install already had -
// the events rows at or below their mark in since (RowMark) - that the next
// Prune at these cutoffs deletes only because of the rows stored after the
// marks. Prune
// deletes outages whole (pruneAge): a 'down' past the cutoff stays while its
// recovery is inside the window or has not come. A restored 'up' that lands
// inside one of this install's own old outages - one still open, or one whose
// recovery is inside the window - becomes that outage's first recovery, past
// the cutoff, and the cleanup then deletes this install's 'down' with the
// restored rows. PruneDue counts only the rows after the mark, so a restore
// that brought such a recovery and its 'down' was told about two rows, and the
// cleanup deleted three.
//
// The count is the rule's own, asked twice in one statement: the rows at or
// below the mark that it deletes now, less the ones it deletes when the table
// holds those rows alone - the rule run again over a common table expression
// named events that holds only them, which the rule's own references to events
// then read. So the count follows pruneAge's text, whatever that becomes, and
// it takes away rows, not totals, so it cannot come out below zero. Today only
// a 'down' can be among them: every other row goes by its own ts, whatever else
// the table holds. The recovery that ends such an outage is past the cutoff
// itself, so it is one of the restored rows PruneDue counts, and these only
// ever come with a count of those.
//
// Restored latency readings can end one of this install's own outages too.
// The close a cleanup begins with gives an outage with no end the second the
// old samples show (resolveDanglingDowns), and where a restored reading is
// among the ones that prove it, or the outage that gets the end is itself
// restored, the install's own 'down' rows before that second go with it. Those
// are counted here as well, from the same dry run PruneDue counts from
// (pruneDueCloses, pruneDueAfterCloses): samplesBefore is the latency cutoff
// the close works at, and since holds the marks, the samples table's for
// telling a restored reading. The ends the cleanup would have written without
// the restore are left out, with the rows they take.
//
// And a restored 'down' can do it with no restored reading at all. Landing
// inside one of this install's own outages, one whose recovery is inside the
// window, it takes that recovery for its own outage. The install's outage then
// reads as having no end, the close gives it one from the install's own old
// samples, where they show a good second before the restored 'down', and that
// end is past the cutoff where the recovery it had was not. The cleanup would
// not have written that end without the restore either, so it is counted with
// the others (pruneDueOwnNext).
//
// NOT MATERIALIZED keeps SQLite from copying the rows at or below the mark out
// first, which would take away the ts index the rule finds each recovery by
// (TestPruneDueCountsSeek). The statement reads this install's own outage
// records, with a seek for each one's recovery, which is fine for a table that
// size.
func (s *Store) PruneDueClosedOutages(ctx context.Context, samplesBefore, eventsBefore time.Time, since map[string]int64) (int64, error) {
	mark, ok := since["events"]
	if !ok {
		return 0, nil
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, pruneDueClosedCount(), eventsBefore.Unix(), mark).Scan(&n); err != nil {
		return 0, err
	}
	closes, err := s.pruneDueCloses(ctx, samplesBefore, eventsBefore, since)
	if err != nil {
		return 0, err
	}
	more, err := s.pruneDueAfterCloses(ctx, closes, eventsBefore.Unix(), mark, true)
	return n + more, err
}

// ErrCleanupHeld is what PruneLive returns for a pass it did not start because
// a restore was bringing rows in (HoldCleanup). Nothing failed and nothing was
// deleted: the pass is counted as skipped (db.prune_skipped_restore) and the
// next one removes what it would have.
var ErrCleanupHeld = errors.New("cleanup held off while a backup is being restored")

// HoldCleanup keeps the cleanup from starting a pass until the function it
// returns is called. A restore takes it before its first row lands and lets go
// once it has replied.
//
// A pass that landed in the middle of a restore read the windows in force at
// that moment, and a backup's own windows only come into force at the reload
// that ends the restore: our exports write the config last, so that a restore
// that fails among the rows leaves the settings alone. A backup that keeps
// history for longer than this install did - a long history moved onto a new
// install, which is what a restore is mostly for - had the rows it had
// brought so far deleted at the install's shorter window, a moment before the
// window that keeps them arrived. The reply could report the loss (WatchPrunes)
// and no more, and the only way back was to send the whole file again. On a
// new install the first pass comes ten minutes after the start, which is about
// when a backup is being restored.
//
// So a pass that would start while a restore is in flight does not: PruneLive
// returns ErrCleanupHeld before it reads a window or deletes a row. The pruner
// runs every hour and a cleanup removes whatever is past the windows when it
// runs, so the pass after the restore removes what the skipped one would
// have, at the windows the restore left in force.
//
// A pass already running when the hold is taken goes on. It reads the windows
// again before every chunk, so it follows a window the restore raises from its
// next chunk on. Until the restore's config lands it can still delete rows the
// restore has brought, and the restore's watch on the cleanup says what it
// deleted (WatchPrunes).
//
// Delete now is not held. It is the operator's own, and asked for.
//
// The hold is a count, not a lock, so nothing ever waits on it, a pass at
// shutdown least of all. The function returned lets go once, however often it
// is called, and the restore calls it on every way out of its handler, a
// panic included. Safe on a nil store, like WatchPrunes.
func (s *Store) HoldCleanup() (release func()) {
	if s == nil {
		return func() {}
	}
	s.cleanupHolds.Add(1)
	var once sync.Once
	return func() { once.Do(func() { s.cleanupHolds.Add(-1) }) }
}

// PruneTally is what the cleanup deleted while a watch on it was open
// (WatchPrunes), table by table.
type PruneTally struct {
	// Rows is how many rows the cleanup deleted from each table, every arm of
	// every pass. A table missing from it lost none.
	Rows map[string]int64
	// Cut is, for each table, the latest cutoff in unix seconds at which a
	// chunk deleted rows of it by age: the cutoff that chunk was cut at, which
	// is not always the one its pass started at (PruneLive). A chunk deletes by
	// age only rows older than its cutoff, so no row younger than this was
	// among them. The future arm's chunks note no cutoff: what they delete is
	// stamped past the horizon, not old. A table missing from it lost no rows
	// by age, or lost them only at the epoch, where a window that keeps
	// forever cuts, and no row is older than that.
	Cut map[string]int64
	// Cleared is, for each table, how many times a Delete now emptied it while
	// the watch was open (Clear). Not a cleanup's doing, but a watch is how a
	// restore learns what was deleted beside it, and a table emptied under it
	// has handed its rowids out again (RowMark). A table missing from it was
	// not emptied.
	Cleared map[string]int64
	// Clearing names the tables a Delete now had begun on and not yet ended
	// when the tally was taken: waiting for its turn at the outage record, or
	// in the middle of its delete. It has not emptied them yet, and is about
	// to.
	Clearing map[string]bool
}

// PruneWatch is a watch on the cleanup, from WatchPrunes to Close.
type PruneWatch struct {
	s      *Store
	tally  PruneTally  // under s.pruneWatchMu; Clearing is filled in as it is read
	closed *PruneTally // what Close returned, once it has
}

// WatchPrunes opens a watch on the cleanup. Until Close, every chunk of every
// pass that deletes rows adds them to it, table by table, with the cutoff a
// chunk that deleted them by age was cut at (PruneTally). Noted chunk by
// chunk, a pass that followed a window raised while it ran is noted at the
// cutoffs it cut at from then on, not at the later one it started at, which
// would say it could reach rows it no longer could. A restore opens one as it
// starts and closes it once it has counted what the next cleanup deletes
// (PruneDue): a pass that was running as the restore began may already have
// deleted rows the restore brought, which no count taken afterwards can see,
// and the restore says so rather than let them go unmentioned. No pass starts
// while a restore is in flight (HoldCleanup), so that is the one pass there
// can be. It is kept by table because a
// pass that deleted rows of one kind says nothing about the rows of another:
// an install older than its latency window deletes aged samples at every
// hourly pass, whatever a restore brings.
//
// A watch also notes each Delete now that empties a table while it is open
// (PruneTally.Cleared), and names the tables one is on as it is read
// (PruneTally.Clearing): the restore tells its rows from the install's by
// rowid, and a table emptied under it hands its rowids out again (RowMark).
//
// A watch holds what was deleted while it was open, and nothing from before.
// The restore used to read a tally kept since the store opened, as it began
// and again at the end, and that tally kept the latest cutoff any pass had
// ever been handed for each table, recorded as the pass started. A window
// lowered for one pass and raised again left that cutoff standing, so every
// later restore of rows between the two windows was told a cleanup may have
// deleted them, whenever an ordinary pass beside it deleted this install's own
// rows past the raised window. Safe on a nil store, like SaveBuffered: the
// watch then holds nothing.
func (s *Store) WatchPrunes() *PruneWatch {
	w := &PruneWatch{s: s, tally: PruneTally{Rows: map[string]int64{}, Cut: map[string]int64{}, Cleared: map[string]int64{}}}
	if s == nil {
		return w
	}
	s.pruneWatchMu.Lock()
	defer s.pruneWatchMu.Unlock()
	if s.pruneWatches == nil {
		s.pruneWatches = map[*PruneWatch]struct{}{}
	}
	s.pruneWatches[w] = struct{}{}
	return w
}

// Close ends the watch and returns what the cleanup deleted while it was
// open. A second call returns the same.
func (w *PruneWatch) Close() PruneTally {
	if w.s != nil {
		w.s.pruneWatchMu.Lock()
		defer w.s.pruneWatchMu.Unlock()
		delete(w.s.pruneWatches, w)
	}
	if w.closed == nil {
		t := w.read()
		w.closed = &t
	}
	return *w.closed
}

// Tally returns what the watch holds now and leaves it open. A restore reads
// it before it counts, to know which tables a Delete now has emptied under it
// (PruneTally.Cleared), and closes the watch after.
func (w *PruneWatch) Tally() PruneTally {
	if w.s != nil {
		w.s.pruneWatchMu.Lock()
		defer w.s.pruneWatchMu.Unlock()
	}
	if w.closed != nil {
		return *w.closed
	}
	return w.read()
}

// read copies what the watch holds, with the tables a Delete now is on at this
// moment. The caller holds pruneWatchMu, where there is a store.
func (w *PruneWatch) read() PruneTally {
	t := PruneTally{Rows: maps.Clone(w.tally.Rows), Cut: maps.Clone(w.tally.Cut),
		Cleared: maps.Clone(w.tally.Cleared), Clearing: map[string]bool{}}
	if w.s != nil {
		for table, n := range w.s.clearing {
			if n > 0 {
				t.Clearing[table] = true
			}
		}
	}
	return t
}

// noteClearing records that a Delete now has begun on tables, and returns the
// function that records its end: once, with whether it emptied them. Every
// watch open at that end notes the tables as emptied (PruneTally.Cleared), and
// from the beginning to the end a watch that is read names them as being
// emptied (PruneTally.Clearing), which covers a Delete now that waits - a
// downtime one waits for a restore's outage history - and the moment between
// its commit and its note.
func (s *Store) noteClearing(tables []string) (ended func(emptied bool)) {
	s.pruneWatchMu.Lock()
	if s.clearing == nil {
		s.clearing = map[string]int{}
	}
	for _, table := range tables {
		s.clearing[table]++
	}
	s.pruneWatchMu.Unlock()
	var once sync.Once
	return func(emptied bool) {
		once.Do(func() {
			s.pruneWatchMu.Lock()
			defer s.pruneWatchMu.Unlock()
			for _, table := range tables {
				s.clearing[table]--
				if !emptied {
					continue
				}
				for w := range s.pruneWatches {
					w.tally.Cleared[table]++
				}
			}
		})
	}
}

// notePruned adds a chunk that deleted n rows of table to every open watch
// (WatchPrunes), and when it deleted them by age, the cutoff it was cut at.
func (s *Store) notePruned(table string, n int64, byAge bool, cut int64) {
	s.pruneWatchMu.Lock()
	defer s.pruneWatchMu.Unlock()
	for w := range s.pruneWatches {
		w.tally.Rows[table] += n
		if byAge && cut > w.tally.Cut[table] {
			w.tally.Cut[table] = cut
		}
	}
}

// pruneDueCount is PruneDue's statement for one table of pruneAge: its rule,
// among the rows after the mark. ?1 is the cutoff and ?2 the mark.
func pruneDueCount(table string) string {
	return `SELECT COUNT(*) FROM ` + table + ` WHERE rowid > ?2 AND (` + pruneAge[table].rule + `)`
}

// pruneDueClosedCount is PruneDueClosedOutages' statement: the events rule of
// pruneAge over the rows at or below the mark, less the rows it takes when the
// table holds those alone. ?1 is the cutoff and ?2 the mark. It names no event
// types, unlike the reads that answer for uptime: it is Prune's rule, and Prune
// reaches every row, whatever its type (TestEventTypeFilterCoversEveryEventRead).
func pruneDueClosedCount() string {
	rule := pruneAge["events"].rule
	return `SELECT COUNT(*) FROM events WHERE rowid <= ?2 AND (` + rule + `)
		AND rowid NOT IN (
			WITH events AS NOT MATERIALIZED (SELECT rowid AS own, * FROM main.events WHERE rowid <= ?2)
			SELECT own FROM events WHERE (` + rule + `))`
}

// pruneChunkRows bounds how many rows Prune deletes between two waits. SQLite
// has a single writer, and one DELETE over a big backlog (retention lowered, a
// box that was off for weeks, a restore of old rows) held it for the whole
// backlog: 2.1 s for 1.8M rows on a fast laptop, and a save of probe readings
// beside it waited those 2.1 s, against the 5 s busy timeout after which the
// save fails and its readings wait in memory for the next try. A full chunk of
// 20000 sample rows holds the writer for about 50 ms on that laptop, a
// hundredth of those 5 s. The same idea as importTxRows,
// sized for deletes. An hour of probe rounds at the default cadence is about a
// quarter of this, so the hourly pass of a settled install never fills a chunk
// and never waits. A variable so a test can make a chunk small enough to cut
// through an outage.
var pruneChunkRows int64 = 20000

// pruneChunkPause is how long Prune leaves the writer alone after a full
// chunk. Letting go of the lock is not enough: a writer that found it taken
// sleeps before it looks again, in steps that grow to 100 ms (the busy handler
// of the bundled SQLite, modernc.org/sqlite v1.52.0), so chunks run back to
// back keep winning the lock from a writer that is asleep each time it comes
// free. One and a half of the longest step means every waiting writer looks at
// least once while the lock is free.
var pruneChunkPause = 150 * time.Millisecond

// pruneChunkHook runs after a chunk has committed and before the wait that
// follows a full one. Test seam only (nil in production), like importChunkHook.
// held is how long the statement's call took. That is more than the time it
// held the writer: it includes any time the statement waited for the writer
// itself, and the checkpoint SQLite can run once the commit is done.
var pruneChunkHook func(q string, rows int64, held time.Duration, full bool)

// pruneSweep is one pass's running state.
type pruneSweep struct {
	total  int64                    // rows removed so far, every table
	budget int64                    // rows the pass may still delete before it must wait
	cut    func(table string) int64 // a retention arm's cutoff for a table, asked before every chunk
	// ahead is the bound of a future arm, asked before every chunk: the
	// pass's horizon as its uptime has moved it on, or false once the wall
	// clock was set while the pass ran, which ends the arm (PruneLive).
	ahead func() (horizon int64, ok bool)
}

// pruneArm says which of a table's two deletes a statement is, and so what
// pruneChunked hands it as its first parameter.
type pruneArm int

const (
	// pruneByAge is the retention arm, which deletes rows past the table's
	// cutoff. Its bound is the cutoff the pass cuts the table at, asked again
	// before every chunk (pruneSweep.cut), since a window raised while the
	// pass runs moves it (PruneLive).
	pruneByAge pruneArm = iota
	// pruneAhead is the future arm, which deletes rows stamped past the pass's
	// horizon. Its bound is the horizon, which no window moves and the pass's
	// own uptime does (pruneSweep.ahead).
	pruneAhead
)

// pruneChunked runs one DELETE until it has nothing left to delete. q takes
// two parameters: the arm's bound (pruneArm), which is asked for before every
// chunk, and then the most rows the chunk may remove (the events rule counts
// recoveries there, see PruneLive). Each chunk is one autocommit statement, so
// it is atomic, it never upgrades a read snapshot, and no cursor is open when
// the next one starts. allowed, when set, is asked before every chunk and ends
// the sweep quietly when it says no, as a future arm's bound does once the
// clock was set under the pass (pruneSweep.ahead). table is the table q
// deletes from. What each chunk removes goes to every open watch on the
// cleanup, and for the retention arm the cutoff that chunk was cut at with it
// (WatchPrunes).
//
// The budget is the pass's, not the statement's: a chunk gets what the tables
// before it left, so a run of nearly full tables cannot add up to one long
// hold with no wait in it.
//
// A chunk that loses the writer for the whole busy timeout ends the pass with
// that error, like any other failure. A restore commits its batches back to
// back, so it can happen beside one. The rows wait for the next pass.
//
// A cancelled context stops the pass inside a wait at once, and interrupts a
// statement that is running. A statement still waiting for the writer is not
// interruptible: it sits out the rest of its wait first, as it always has.
func (s *Store) pruneChunked(ctx context.Context, sw *pruneSweep, table string, arm pruneArm, allowed func() bool, q string) error {
	for {
		if allowed != nil && !allowed() {
			return nil
		}
		var bound int64
		if arm == pruneByAge {
			bound = sw.cut(table)
		} else if at, ok := sw.ahead(); ok {
			bound = at
		} else {
			return nil // the clock was set under the pass: nothing more goes as ahead of it
		}
		began := time.Now()
		res, err := s.db.ExecContext(ctx, q, bound, sw.budget)
		if err != nil {
			recordDBErr(err)
			return err
		}
		n, _ := res.RowsAffected() // the driver never fails this
		sw.total += n
		if n > 0 {
			s.notePruned(table, n, arm == pruneByAge, bound)
		}
		full := n >= sw.budget
		sw.budget -= n
		if pruneChunkHook != nil {
			pruneChunkHook(q, n, time.Since(began), full)
		}
		if !full {
			return nil
		}
		sw.budget = pruneChunkRows
		if err := pruneWait(ctx); err != nil {
			return err
		}
	}
}

// pruneWait leaves the writer free for pruneChunkPause, or returns at once
// with the context's error when the daemon is shutting down.
func pruneWait(ctx context.Context) error {
	t := time.NewTimer(pruneChunkPause)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// trimWALAfter empties the write-ahead log after a delete of walTrimRows or
// more. first, when set, runs before the trim and calls it off by returning an
// error. It reports nothing to its caller on purpose: the delete has
// committed, and a log that could not be emptied is still cut back to
// walSizeLimit once it restarts.
func (s *Store) trimWALAfter(ctx context.Context, rows int64, first func(context.Context) error) {
	if rows < walTrimRows {
		return
	}
	if first != nil && first(ctx) != nil {
		return // the caller is stopping
	}
	switch got, err := s.trimWAL(ctx); got {
	case walTrimDone:
		stats.Inc("db.wal_trim")
	case walTrimNotTried, walTrimGaveUp:
		stats.Inc("db.wal_trim_blocked")
	case walTrimFailed:
		stats.Inc("db.wal_trim_failed")
		recordDBErr(err)
		log.Printf("pingularity: could not empty the write-ahead log after a large delete (it is cut back to %d MiB once it restarts): %v", walSizeLimit>>20, err)
	}
}

// trimWAL copies the log back into the database and cuts the -wal file to
// nothing (wal_checkpoint(TRUNCATE)). It runs on the caller's goroutine, which
// is the pruner's or a request's, never the monitor's.
//
// Two passes, because TRUNCATE copies while it holds the writer. The PASSIVE
// pass takes no writer lock and never waits, so whatever is left to copy is
// copied there. TRUNCATE runs only if that pass finished the log, and then has
// a few frames at most to copy. If a reader still needs the log, the PASSIVE
// pass cannot finish and the trim stops there without ever taking the writer.
//
// TRUNCATE can still meet a reader that is on the end of the log, or a writer
// in the middle of a transaction. It waits for them, and once it has the
// writer it holds it while it waits for the reader, so the connection's busy
// timeout is lowered to walTrimWait for that one statement and put back
// afterwards.
//
// A blocked trim is the usual outcome while a dashboard is open on a wide
// chart or an export is running. Nothing retries it: the size limit cuts the
// log back once the reader has left and the log restarts.
func (s *Store) trimWAL(ctx context.Context) (walTrim, error) {
	if ctx.Err() != nil {
		return walTrimSkipped, nil
	}
	fail := func(err error) (walTrim, error) {
		if ctx.Err() != nil {
			return walTrimSkipped, nil // stopping, not failing
		}
		return walTrimFailed, err
	}
	wait, cancel := context.WithTimeout(ctx, walTrimWait)
	conn, err := s.db.Conn(wait)
	cancel()
	if err != nil {
		// Only running out of time is "every pooled connection is busy".
		// Anything else (a closed store, a file that cannot be opened) is a
		// fault and must not read as a reader being in the way.
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return walTrimNotTried, nil
		}
		return fail(err)
	}
	defer conn.Close()
	var busy, logFrames, copied int64
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &copied); err != nil {
		return fail(err)
	}
	// The order matters. When another connection holds the checkpoint lock
	// the reply is busy 1, log -1, copied -1. Asking for a missing log first
	// would read that reply as "no log".
	switch {
	case busy != 0:
		return walTrimNotTried, nil // another connection is checkpointing
	case logFrames < 0:
		return walTrimSkipped, nil // no log: an in-memory database
	case copied < logFrames:
		return walTrimNotTried, nil // a reader still needs part of the log
	}
	restore, err := lowerBusyTimeout(ctx, conn, walTrimWait)
	if err != nil {
		return fail(err)
	}
	defer restore()
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &copied); err != nil {
		return fail(err)
	}
	if busy != 0 {
		return walTrimGaveUp, nil
	}
	return walTrimDone, nil
}

// lowerBusyTimeout makes statements on conn wait d for the writer, in place
// of the timeout the connection came with, and returns the function that puts
// that one back.
//
// The connection goes back to the pool, so it must leave with the timeout
// it came with: a probe write on a connection left at a quarter second would
// give up twenty times sooner than it should. The restore does not run
// under the caller's context, which may be over by then. A connection
// that cannot be restored is dropped instead, and the pool opens a new
// one with the DSN's pragmas.
func lowerBusyTimeout(ctx context.Context, conn *sql.Conn, d time.Duration) (restore func(), err error) {
	var prev int64
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&prev); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, d.Milliseconds())); err != nil {
		return nil, err
	}
	return func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout = %d`, prev)); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}, nil
}

// ReusableBytes is the space inside the database file that holds no data:
// whole free pages, in bytes. Deleting rows never shrinks the file. The pages
// they filled go on a free list, and new rows use them before the file grows.
// A page that is only partly empty is not counted, so the figure is a floor.
// One statement, so both numbers come from one snapshot.
func (s *Store) ReusableBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT freelist_count * page_size FROM pragma_freelist_count(), pragma_page_size()`).Scan(&n)
	return n, err
}

// IsBusy reports whether err is SQLite giving up on the writer: something else
// held it for longer than the busy timeout. It is the test db.busy counts by,
// so a caller that reports the case in its own words agrees with /metrics
// about which errors those are.
func IsBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "busy") || strings.Contains(msg, "locked")
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
	case IsBusy(err):
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

// recordedErr is an error recordDBErr has already counted. A function that
// counts some of the errors it returns and not others returns the counted ones
// wrapped in it, so a caller that counts the rest can tell them apart
// (errors.As) and /metrics sees each failure once. It reads and unwraps as the
// error it holds.
type recordedErr struct{ error }

func (e recordedErr) Unwrap() error { return e.error }

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
// too - otherwise orphaned DNS rows keep feeding the chart after a clear. The
// speedtest times go with it for the same reason: that chart's hover note is
// their only reader, and a chart with no data has no point to hover.
//
// A latency clear also takes the only record of when some outages ended. A
// restart in the middle of an outage leaves its 'down' with no 'up': the
// process that wrote it stopped, and the next one starts out believing the link
// is up, so it never writes one. The readers bound such an outage at the first
// good second in the samples (firstQuorumRecovery). With every sample deleted
// the next one they find is whatever the monitor writes after the clear, so a
// ten-minute outage three hours back read as three hours long - and once those
// later rows passed the retention, a cleanup would have written that length
// down for good. So the clear first closes each such outage at the second the
// samples prove, as a cleanup does before it prunes them, and leaves alone every
// outage the running monitor may still close itself (resolveDanglingDowns, with
// the line of monitorOutages).
//
// The database file keeps its size. The rows' pages go on the free list
// (ReusableBytes) and new rows fill them. A clear of walTrimRows or more ends
// by emptying the write-ahead log, which cannot fail the clear.
func (s *Store) Clear(ctx context.Context, kind string) (int64, error) {
	var tables []string
	switch kind {
	case "latency":
		tables = []string{"samples", "dns", "speed_spans"}
	case "speed":
		// The selection reports go with their runs (manual cascade, no FKs):
		// rows kept here would describe runs the operator just deleted.
		tables = []string{"speed", "speed_servers"}
	case "downtime":
		// Both tables, for the same reason the export ships them as one category
		// (see dataCategories): `events` holds the outages, `pauses` holds the spans
		// that say which seconds were watched at all. Clearing only the events left
		// the pause rows suppressing observation coverage - the outages disappear
		// from the log and the heatmap while uptime stays exactly as thin as before,
		// with nothing on any screen left to explain it. The quarantine goes too: a
		// row left there is handed back to `pauses` by the next open under a good
		// clock (see repairFutureReachingPausesAt), which would return unobserved time
		// the operator deliberately deleted.
		tables = []string{"events", "pauses", "pauses_quarantine"}
	default:
		return 0, fmt.Errorf("unknown data kind %q", kind)
	}
	// A restore that is running has to know (noteClearing): a table emptied
	// under it hands its rowids out again, and the restore tells the rows it
	// brought from the rows already here by rowid (RowMark). Noted from here,
	// before any wait, to the commit, or to wherever this gives up.
	ended := s.noteClearing(tables)
	defer ended(false)
	// The readings still waiting belong to what is being cleared. Saved first
	// they are deleted and counted with the rest. Left waiting they would come
	// back after the clear. So a clear that cannot save them deletes nothing.
	if kind == "latency" {
		if err := s.SaveBuffered(SaveOrder); err != nil {
			return 0, err
		}
		// The outages whose end only the samples show are closed next: after the
		// save, because the waiting rounds are evidence too, and before the
		// delete. Not inside its transaction. The search saves and reads and the
		// close writes through InsertEvent, and none of that may run while this
		// call holds a transaction open (the in-memory pool has one connection);
		// a transaction that read first and wrote after would also fail with
		// SQLITE_BUSY_SNAPSHOT whenever a probe round committed in between (see
		// SetSettingsDiff).
		//
		// A close that fails deletes nothing, like a save that fails: deleting
		// the samples with the recovery unwritten is the damage this step exists
		// to prevent, and the operator can press the button again. A failure or a
		// crash between the close and the delete leaves closing events and no
		// delete. Each sits at the second the samples prove, with the observed
		// length, so every reader books the outage as it did from the samples,
		// and the next clear or cleanup finds it closed and passes it by. The
		// read caches drop as a cleanup drops them: in the close when it wrote
		// anything, and again once the delete commits.
		//
		// The cutoff is the present. Every sample goes, so every recovery they
		// prove is written - except one dated ahead of the clock, which a cleanup
		// never writes either. An 'up' dated ahead of the clock that closes an
		// outage the monitor does not hold is moved back to the proven second
		// rather than joined by a second closing event, as a cleanup does: no
		// reader counts it until the clock gets there, and a backward step would
		// let the next cleanup delete it as too far ahead. One the monitor wrote
		// for its own outage stays where it is; the widen in Monitor.transition
		// puts it there on purpose. A clock that is not set yet (before
		// plausibleEpoch: a board with no battery, before time sync) reaches no
		// outage recorded under a correct one, so none of those is closed and the
		// delete goes ahead, as it always did.
		//
		// A round saved between the close and the delete goes with the rest. If
		// it was the first good round after an outage the monitor does not hold
		// (one that ran on into this process and ended just now), that outage
		// ends at the next good round instead, one probe interval late.
		//
		// The close holds outageMu for the whole of its run, so it never meets
		// another close or a delete of outages half way (see there). The delete
		// below does without it: it removes no outage.
		nowU := time.Now().Unix()
		if err := s.resolveDanglingDowns(ctx, nowU+1, nowU, s.live.line); err != nil {
			// A failed write the close has counted already (recordedErr).
			if !errors.As(err, new(recordedErr)) {
				recordDBErr(err)
			}
			return 0, err
		}
	}
	// The downtime delete takes the whole outage record, under outageMu. A
	// close that read an outage before this delete would write that outage's
	// end after it: an end with no outage (see outageMu). Held to the end, the
	// trim included, which adds half a second at the most, and only after a
	// delete of walTrimRows rows or more.
	if kind == "downtime" {
		s.outageMu.Lock()
		defer s.outageMu.Unlock()
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
	ended(true)
	s.invalidateReadCaches()
	// Here by decision more than by need. Emptying a whole table frees its
	// pages without logging its rows (0.55 bytes of log a row measured,
	// against 43 for a cleanup), so what this empties is the log as it stood
	// before the clear. No wait first: it would be most of what a Delete now
	// takes.
	s.trimWALAfter(ctx, total, nil)
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
// realCols is the same allowlist for REAL-affinity columns - the ones the graphs
// actually plot. Text in one of these is the mirror landmine: a Go scan into
// float64 errors, so a single poisoned row takes down the whole window's read,
// and where SQLite coerces instead (inside AVG) it counts as 0 and reports a
// fake perfect measurement rather than failing visibly.
//
// rowSane, when set, vets a row that already has the right column TYPES for
// whether its values mean anything. Only tables whose rows are intervals need it:
// a duration is not merely a number, it is the length of a span that other code
// adds to a timestamp, and both of those have values a type check cannot catch.
//
// Two tables are left out on purpose. server_health is a cache of which servers
// refused uploads, re-earned on the machine that runs the tests. speed_spans only
// feeds the latency chart's hover note: carrying it would stamp nearly every
// backup with a newer schema (reconnect tests are on by default, so almost every
// install has rows) and make older releases refuse the whole file, for no number
// anywhere. A restore simply has no hover notes for the history it brings.
var exportTables = map[string]struct {
	cols     []string
	keyCols  []string
	notNull  []string
	intCols  []string
	realCols []string
	rowSane  func(map[string]any) bool
}{
	"samples": {cols: []string{"ts", "target", "latency_ms", "success", "family"}, keyCols: []string{"ts", "target"}, notNull: []string{"success"}, intCols: []string{"ts", "success"}, realCols: []string{"latency_ms"}},
	"dns":     {cols: []string{"ts", "latency_ms", "success"}, keyCols: []string{"ts"}, notNull: []string{"success"}, intCols: []string{"ts", "success"}, realCols: []string{"latency_ms"}},
	"events":  {rowSane: eventRowSane, cols: []string{"ts", "type", "duration_s", "detail"}, keyCols: []string{"ts", "type"}, intCols: []string{"ts", "duration_s"}},
	// pauses is the uptime DENOMINATOR and rides the same "downtime" category as
	// events (see dataCategories), because the two are one record: events carry the
	// OBSERVED downtime, pauses carry which wall seconds were observed at all
	// (pausedOverlap feeds UptimeSince's denominator, DowntimeByDay's prorate and
	// orphanGapDowntime). Exporting one without the other restores a history where
	// every unobserved second silently becomes observed-and-up, so the restored box
	// reports a DIFFERENT uptime from the machine the backup came from - and worse,
	// a window that observed nothing comes back with coverage 1.0, defeating the
	// Observation.Defined guard that exists so such a window is not published as a
	// misleading 100%. Merged by ts like the other time-series tables, which keeps a
	// re-import idempotent - and is why Prune must never rewrite a pause row's ts
	// (see the straddle rule there).
	"pauses": {cols: []string{"ts", "duration_s"}, keyCols: []string{"ts"}, notNull: []string{"duration_s"},
		intCols: []string{"ts", "duration_s"}, rowSane: pauseRowSane},
	// The held-aside half of the pause record. A backup taken between a wrong
	// stale-clock judgement and the settled-step re-judgement used to omit
	// these rows entirely - if the source box then died, the genuine history
	// in quarantine was gone and the restored box reported different coverage
	// than the source. Held rows export under their own key and land back in
	// the DESTINATION's quarantine, never directly in coverage: the
	// destination's own re-judgement (armed by the import) exonerates what its
	// clock can vouch for and keeps holding what it cannot - the same
	// judgement local rows get. A file CARRYING this key stamps envelope
	// schema 4 (see the web layer's exportSchema contract), so older builds
	// refuse it loudly instead of skipping the key and silently shedding the
	// held history; an empty quarantine omits the key and stamps as before.
	"pauses_quarantine": {cols: []string{"ts", "duration_s"}, keyCols: []string{"ts"}, notNull: []string{"duration_s"},
		intCols: []string{"ts", "duration_s"}, rowSane: quarantineRowSane},
	"speed": {cols: []string{"ts", "down_mbps", "up_mbps", "ping_ms", "server", "server_id",
		"public_ipv4", "public_ipv6", "isp", "isp_location", "dns_ip", "dns_provider", "dns_location",
		"packet_loss", "healthy", "jitter_ms", "download_bytes", "upload_bytes", "cf_colo", "exit_summary", "run_trigger",
		"idle_ms", "loaded_down_ms", "loaded_up_ms", "loaded_down_p95_ms", "loaded_up_p95_ms", "engine",
		"ping_best_ms", "ip_family", "udp_direction", "failed", "usage_run_ts",
		"race_outcome", "race_origins", "race_winner_kind", "race_winner_label", "race_winner_ms", "race_racers",
		"race_winner_lat", "race_winner_lon", "round_ts", "fail_stage"},
		keyCols: []string{"ts"},
		// server is read as a plain string everywhere (and COALESCEd on read as a
		// second belt) - a crafted row without one must be skipped, not inserted
		// as NULL, which once wedged every speed read with a Scan error.
		notNull: []string{"server"},
		// failed is on the int allowlist so the marker cannot arrive as text: an
		// unreadable marker is a row no read can classify, and speedNotFailed
		// then hides it for good rather than showing a run that measured nothing.
		// usage_run_ts is there for the same reason one step on: a reference that
		// arrives as text no `usage_run_ts = ?` can match is an accounting row
		// deleting its run will never find, billing bytes for a speedtest that no
		// longer exists with nothing in the UI to remove them.
		// round_ts likewise: a reference that arrives as text is a round member
		// no `round_ts = ?` cascade or `round_ts IS NULL` read can classify.
		intCols: []string{"ts", "healthy", "download_bytes", "upload_bytes", "failed", "usage_run_ts", "race_racers", "round_ts"},
		realCols: []string{"down_mbps", "up_mbps", "ping_ms", "ping_best_ms", "jitter_ms", "packet_loss",
			"idle_ms", "loaded_down_ms", "loaded_up_ms", "loaded_down_p95_ms", "loaded_up_p95_ms", "race_winner_ms",
			"race_winner_lat", "race_winner_lon"}},
	// The per-run selection reports ride the speed category (they are the
	// speed rows' explanations - see SpeedServerRow). Merged by (run_ts,
	// server_id): one row per candidate per run, so a re-import is idempotent
	// like the other time-series tables. Deliberately INCLUDED in export,
	// unlike pauses_quarantine: this table explains history, and a backup that
	// restores runs without their explanations silently loses the very data
	// the table exists to preserve.
	"speed_servers": {cols: []string{"run_ts", "server_id", "server", "distance_km", "rank_order", "rank_ping_ms",
		"selected", "measured", "error", "down_mbps", "up_mbps", "ping_ms", "ping_best_ms", "jitter_ms",
		"download_bytes", "upload_bytes", "capacity_mbps", "believed_capacity_mbps", "capped_direction",
		"score", "winner", "win_reason"},
		keyCols: []string{"run_ts", "server_id"},
		// server_id/server join and label the row everywhere; a crafted row
		// without them must be skipped, not inserted as NULL (the speed table's
		// own lesson - see its notNull comment; the NullString scan is the
		// second belt).
		notNull: []string{"server_id", "server", "selected", "measured", "winner"},
		intCols: []string{"run_ts", "rank_order", "selected", "measured", "download_bytes", "upload_bytes", "winner"},
		realCols: []string{"distance_km", "rank_ping_ms", "down_mbps", "up_mbps", "ping_ms", "ping_best_ms",
			"jitter_ms", "capacity_mbps", "believed_capacity_mbps", "score"},
		rowSane: speedServerRowSane},
	"settings": {cols: []string{"key", "value"}, keyCols: nil, notNull: []string{"key", "value"}},
}

// speedServerRowSane rejects selection-report rows that cannot join a run: a
// row without a plausible run_ts would be unreachable garbage (every read is
// keyed by run ts) that only Prune's future-horizon arm could ever remove.
func speedServerRowSane(row map[string]any) bool {
	ts, ok := row["run_ts"].(int64)
	return ok && ts > 0
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
	// First-run Quick Setup machinery: the answer and the offer clock belong to
	// the INSTALL, not the data. Restoring a mid-offer backup must not reopen
	// the dialog on (or hold monitoring of) an established destination; the
	// destination's own boot decision, taken from its restored history, stands.
	"quick_setup_done": true, "quick_setup_offer_since": true,
	"telemetry_last_speed_ts": true, "telemetry_last_event_ts": true,
	"telemetry_last_send_ts": true, "telemetry_clean_shutdown": true,
	// Digest delivery state (when the last summary went out): local-only, and a
	// stale/crafted value could suppress or force a digest, so it's neither
	// exported nor accepted. The cadence preference (digest_freq) stays exportable.
	"digest_last_sent": true,
	// The install's birth provenance: "the daemon that wrote this created this
	// database". Every rule around it exists to keep that claim honest - only
	// the process that read the empty store may write it, an established store
	// is never stamped retroactively, a restart drops the pending witness, a
	// restore voids it. A backup that carried the marker would route around all
	// of that: any destination could be handed a birth it never had.
	//
	// And a FORGED marker is worse than a missing one. Absent, it reads as
	// "unknown" and the install fails closed - the ambiguous-container access
	// warning fires and access stays local-only. Present, it is believed: the
	// warning goes quiet and anything later keyed on the marker (a 0.63 upgrade
	// decision, say) trusts a birth nobody witnessed. Denied in BOTH directions
	// by this one list, so neither an export nor a crafted file can move it.
	"install_born_version": true,
	// The settings layer's note that it has applied its per-engine direction and
	// retries split to THIS table (settings.recordMigrations). It describes the
	// destination's rows, not the backup's: the destination writes its own at
	// boot, and one riding in from another install could only suppress a seed the
	// destination still owed itself.
	"engine_split_done": true,
	// Who may reach the dashboard at all. Install-scoped in the strongest sense:
	// it describes ONE host's network posture - whether that machine's port is
	// safe to answer on - and says nothing about the history in the file.
	//
	// The daemon refuses to write this key on evidence far better than a backup.
	// warnAmbiguousContainerAccess will not persist an open posture even when
	// every container tell agrees, because a wrong answer either publishes a
	// dashboard or locks the operator out of their own container; it warns and
	// leaves the decision to a human. A file an attacker may craft, or simply
	// last month's backup from another machine, must not be the one path that
	// writes it - in either direction. Opening one is a silent exposure with no
	// warning in the response; closing one makes a container 403 its own
	// published port with nothing saying why.
	//
	// Restoring a backup moves the DATA. The destination decides who can see it.
	"access_local_only": true,
	// The hold a store rebuilt after corruption keeps on network access until a
	// login is set on it (AccessHoldAfterRebuild). Like the access scope above,
	// it describes one host's store, not the history: exported, a restore would
	// hold some healthy install off its own network; imported, a crafted file
	// could close a published port with nothing saying why.
	accessHoldKey: true,
	// When THIS box started watching. It lives in the settings table but it is
	// evidence, not configuration: monitoringSince takes the MIN of it and the
	// earliest row on disk, making it the denominator behind every uptime figure.
	//
	// Restoring it from another machine therefore imports that machine's lifetime
	// as this one's. Being older it always wins the MIN, so a box up for an hour
	// reported 720h of monitoring off a 30-day-old backup - with coverage still
	// 1.0, so nothing marked the answer as thin. Uptime then reads high, because
	// the observed-downtime numerator is this box's and the denominator is
	// somebody else's.
	//
	// Denying it loses nothing: monitoringSince re-derives the anchor from the
	// earliest sample or event actually present, so a restore that brings HISTORY
	// still moves it - correctly, because those rows are here to back the claim.
	firstSeenKey: true,
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
// withBestOfCount migrates a backup from before Best-of was a count: its
// "speed_best_of" on/off becomes the "speed_best_of_count" it meant (on =
// three servers, off = one) when the backup carries no count of its own.
// Without this the import's per-row upsert would leave a count this host had
// already stored, which then outranks the restored on/off (the count wins
// whenever present) and the backup's Best-of would silently not come back.
func withBestOfCount(rows []map[string]any) []map[string]any {
	var legacy string
	hasLegacy, hasCount := false, false
	for _, r := range rows {
		switch k, _ := r["key"].(string); k {
		case "speed_best_of":
			legacy, _ = r["value"].(string)
			hasLegacy = true
		case "speed_best_of_count":
			hasCount = true
		}
	}
	if !hasLegacy || hasCount {
		return rows
	}
	n := "1"
	switch strings.ToLower(strings.TrimSpace(legacy)) {
	case "1", "true":
		n = "3"
	}
	return append(rows, map[string]any{"key": "speed_best_of_count", "value": n})
}

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
//
// The waiting probe readings are saved first. If they cannot be, the export
// carries what is saved.
func (s *Store) ExportTableRows(ctx context.Context, table string, fn func(map[string]any) error) error {
	s.SaveBuffered(SaveRead)
	return exportRows(ctx, s.db, table, fn)
}

// BeginReadSnapshot opens a read-only transaction. In WAL mode this pins a single
// consistent database snapshot from its first read, so exporting several tables
// within it (via ExportTableRowsTx) can't interleave rows from different commits or
// pick up rows newer than the export started (audit: export is not one snapshot).
// The caller MUST Rollback it when done; it only prevents WAL CHECKPOINT past its
// mark (a bounded WAL growth for the export's duration), never live writes.
//
// The waiting probe readings are saved before the snapshot opens, because
// nothing read inside it can save them any more. If they cannot be saved the
// snapshot opens on what is: a disk too full to take them must not stand in
// the way of the export that rescues the rest.
func (s *Store) BeginReadSnapshot(ctx context.Context) (*sql.Tx, error) {
	s.SaveBuffered(SaveRead)
	return s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

// HasQuarantinedPausesTx reports whether any pause rows sit in quarantine,
// against the same read snapshot an export streams from. The answer decides
// whether the file carries the pauses_quarantine key at all - and therefore
// which schema version it needs - so it must describe the exact instant the
// rows do, not the live table.
func (s *Store) HasQuarantinedPausesTx(ctx context.Context, tx *sql.Tx) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pauses_quarantine)`).Scan(&n)
	return n != 0, err
}

// speedColumnsPastSchema4 are the `speed` columns added after envelope schema 4
// - the ones no shipped build that accepts a stamp of 4 or lower has ever heard
// of. They are the reason the speed category needs a content check at all:
// unlike pauses_quarantine, a COLUMN lives inside a category every file already
// carries, so the key-driven stamp cannot see it.
//
// The stakes are higher than "the column is lost". An older build does NOT skip
// what it does not recognise - ImportTableBatch aborts the category with
// `unknown column`, and it does so partway through a restore that has already
// committed latency. So a file carrying any of these must stamp above what
// those builds accept, and a file stamping low must carry none of them.
//
// usage_run_ts belongs on this list and not on a list of its own: the newest tag
// is v0.61.3, which accepts 4, and the stamp of 5 was added in the same
// unreleased campaign as this column - so "the builds that have never heard of
// it" is the same set for all four, and one more version would separate files
// that no build in the wild tells apart. It must ride in the export at all
// (rather than being left a local-only column) because it is what a delete
// cascades on: a restored backup that dropped it would put every accounting row
// back beyond the reach of deleting its run, billing bytes for speedtests the
// operator has removed.
//
// The race verdict columns (race_*) came later still, after the stamp of 5 had
// shipped, so a file carrying any of them needs 6: the builds that accept 5
// abort on them the same way. round_ts (a kept Best-of round) needs 7, and
// fail_stage (a failed test kept by Record failed tests) needs 8, each for the
// same reason one rung up. speedColumnSchema is the one place each column's
// version lives; the list below and the export's stamp both read it.
func speedColumnsPastSchema4() []string {
	out := make([]string, 0, len(speedColumnSchema))
	for c := range speedColumnSchema {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// speedColumnSchema is the envelope version a file needs to carry each speed
// column added after schema 4 (see speedColumnsPastSchema4).
var speedColumnSchema = map[string]int{
	"ip_family": 5, "udp_direction": 5, "failed": 5, "usage_run_ts": 5,
	"race_outcome": 6, "race_origins": 6, "race_winner_kind": 6, "race_winner_label": 6,
	"race_winner_ms": 6, "race_racers": 6, "race_winner_lat": 6, "race_winner_lon": 6,
	"round_ts":   7,
	"fail_stage": 8,
}

// SpeedColumnSchema is the envelope version a file carrying col needs, or 0
// for a column every reader has always known.
func SpeedColumnSchema(col string) int { return speedColumnSchema[col] }

// SpeedColumnsPastSchema4InUse reports which of those columns actually carry a
// value in the snapshot an export streams from - not which exist in the schema,
// which is always all of them. An install that has never recorded one keeps its
// backup restorable on older builds; one that has stamps 5, and those builds
// refuse it at the envelope check instead of failing mid-restore.
//
// NULL is "unrecorded" for every one of them, and `failed = 0` says the same thing as
// NULL everywhere else (see speedNotFailed), so neither counts as carrying
// anything. The answer must describe the exact instant the rows do, hence the
// caller's snapshot rather than the live table.
//
// The `failed` test is the exact complement of speedNotFailed - not NULL and not
// 0 - and has to be, because the two answer one question from opposite sides:
// "is this row hidden as accounting?" and "must the file carry the column that
// hides it?". Written as `failed = 1` they disagreed about every other non-zero
// marker (reachable only through a crafted or hand-edited backup - the daemon
// writes 1 or NULL - since `failed` sits on the import allowlist as a whole
// number). Such a row was hidden from every measurement read AND reported as
// "nothing here uses this column", so the export dropped the column from it and
// stamped low; restoring that backup brought the row back with no marker at all,
// which is a 0.00 Mbps speedtest in the chart, the history table, the CSV, the
// run count and the averages, on a box that measured nothing.
// usage_run_ts has only ONE spelling of "unset" - NULL - so unlike `failed` it
// needs no second exclusion: any value at all is a reference some delete could
// cascade on, and a backup that shed it would restore the row beyond the reach
// of deleting its run. Erring toward "in use" is the safe direction anyway; the
// cost is a file older builds refuse up front.
// fail_stage reads speedHasStage, the listing's own spelling of "set", and asks
// about ANY row rather than only failure records: a stray stage on another row
// costs a higher stamp, while a stage the file dropped would restore a listed
// failed test as a hidden accounting row.
func (s *Store) SpeedColumnsPastSchema4InUse(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	var ipFamily, udpDirection, failed, usageRunTS bool
	var raceOutcome, raceOrigins, raceKind, raceLabel, raceMS, raceRacers, raceLat, raceLon, roundTS, failStage bool
	err := tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM speed WHERE ip_family IS NOT NULL AND ip_family <> ''),
		EXISTS(SELECT 1 FROM speed WHERE udp_direction IS NOT NULL AND udp_direction <> ''),
		EXISTS(SELECT 1 FROM speed WHERE failed IS NOT NULL AND failed <> 0),
		EXISTS(SELECT 1 FROM speed WHERE usage_run_ts IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE race_outcome IS NOT NULL AND race_outcome <> ''),
		EXISTS(SELECT 1 FROM speed WHERE race_origins IS NOT NULL AND race_origins <> ''),
		EXISTS(SELECT 1 FROM speed WHERE race_winner_kind IS NOT NULL AND race_winner_kind <> ''),
		EXISTS(SELECT 1 FROM speed WHERE race_winner_label IS NOT NULL AND race_winner_label <> ''),
		EXISTS(SELECT 1 FROM speed WHERE race_winner_ms IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE race_racers IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE race_winner_lat IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE race_winner_lon IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE round_ts IS NOT NULL),
		EXISTS(SELECT 1 FROM speed WHERE `+speedHasStage+`)`).Scan(
		&ipFamily, &udpDirection, &failed, &usageRunTS,
		&raceOutcome, &raceOrigins, &raceKind, &raceLabel, &raceMS, &raceRacers, &raceLat, &raceLon, &roundTS, &failStage)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ip_family": ipFamily, "udp_direction": udpDirection,
		"failed": failed, "usage_run_ts": usageRunTS,
		"race_outcome": raceOutcome, "race_origins": raceOrigins, "race_winner_kind": raceKind,
		"race_winner_label": raceLabel, "race_winner_ms": raceMS, "race_racers": raceRacers,
		"race_winner_lat": raceLat, "race_winner_lon": raceLon, "round_ts": roundTS,
		"fail_stage": failStage}, nil
}

// LastDecidedRace is the newest run whose city race was decided and whose
// winner carries a USABLE coordinate: the city the next run enters as its
// "recent" origin (see main autoOrigins). ok=false with no such run. An
// unusable coordinate (NULL, 0,0, off the globe - reachable through an
// imported backup, whose column check is a type check only) is skipped in
// the query itself, so the walk goes on to the previous decided race rather
// than stopping at a row it cannot use. Walks the speed table newest-first
// and stops at the first hit: a row or two on an install that races, but a
// pinned install (whose runs still read OriginsFn, see RunReason) walks every
// bypassed_pin row back to its last decided race - or the whole table if it
// never raced - once per run. The table is a few rows an hour, so that is
// milliseconds at a decade of history; no ts floor is worth the "recent"
// candidate going missing.
func (s *Store) LastDecidedRace(ctx context.Context) (label string, lat, lon float64, ok bool, err error) {
	var la, lo sql.NullFloat64
	var lbl sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(race_winner_label,''), race_winner_lat, race_winner_lon FROM speed
		 WHERE race_outcome = 'decided' AND race_winner_lat IS NOT NULL AND race_winner_lon IS NOT NULL
		   AND (race_winner_lat <> 0 OR race_winner_lon <> 0)
		   AND race_winner_lat BETWEEN -90 AND 90 AND race_winner_lon BETWEEN -180 AND 180
		   AND `+speedIsResult+`
		 ORDER BY ts DESC LIMIT 1`).Scan(&lbl, &la, &lo)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, 0, false, nil
	}
	if err != nil {
		recordDBErr(err)
		return "", 0, 0, false, err
	}
	if !la.Valid || !lo.Valid || (la.Float64 == 0 && lo.Float64 == 0) ||
		math.IsNaN(la.Float64) || math.IsNaN(lo.Float64) || math.IsInf(la.Float64, 0) || math.IsInf(lo.Float64, 0) {
		return "", 0, 0, false, nil
	}
	return lbl.String, la.Float64, lo.Float64, true, nil
}

// AllSpeedColumnsPastSchema4InUse is what a caller must assume when the check
// above fails: every new column is in use, so the file stamps high and an older
// build refuses it. "An older build refuses the backup" is a safe failure;
// "a column is silently shed" is the one this machinery exists to prevent.
func AllSpeedColumnsPastSchema4InUse() map[string]bool {
	m := map[string]bool{}
	for _, c := range speedColumnsPastSchema4() {
		m[c] = true
	}
	return m
}

// SpeedColumnsPastSchema4 exposes the column list to the web layer, which drops
// the unused ones from the rows it streams. The list and the stamp decision must
// come from the same place, or a file could omit a column while still claiming
// to need it - or worse, carry one while claiming not to.
func SpeedColumnsPastSchema4() []string { return speedColumnsPastSchema4() }

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

// importChunkHook runs after a mid-batch chunk commit succeeds, before the
// next transaction opens. Test seam only (nil in production): the interesting
// window is a cancellation landing between a durable commit and the BeginTx
// that follows it, which makes ImportTableBatch return rows-committed WITH an
// error - and a retry of those same rows is all NOT EXISTS no-ops.
var importChunkHook func()

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
// ImportTableBatch with a shared counter so the cap stays global, and for the
// downtime category's tables the ImportTableBatch of a hold on the outage
// record. ImportTable brings those tables in under a hold of its own, taken
// for the call and let go when it returns.
func (s *Store) ImportTable(ctx context.Context, table string, rows []map[string]any) (int, error) {
	if outageTables[table] {
		hold := s.HoldOutageRecord()
		defer hold.Release()
		return hold.ImportTableBatch(ctx, table, rows, map[int64]int{})
	}
	return s.ImportTableBatch(ctx, table, rows, map[int64]int{})
}

// ImportTableBatch is ImportTable with a caller-owned per-timestamp counter
// (perTS). A streaming restore decodes the file in fixed-size batches and calls
// this once per batch with the SAME map, so the maxRowsPerTS flood cap counts
// across all batches. A per-call counter would reset each batch, letting a
// crafted file pack maxRowsPerTS rows per batch at one ts and restore the O(N^2)
// dedup cost the cap exists to bound.
//
// It refuses the downtime category's tables (outageTables): the outages, the
// paused time and the held-aside pauses. Those go in under outageMu, like
// every change to the outage record but the monitor's (see there), and the
// lock has to be held across every call that brings a part of them, which is
// what the hold on the outage record does (HoldOutageRecord). They go in
// through its ImportTableBatch, or through ImportTable, which takes a hold for
// its one call.
//
// This used to take the lock itself, for the outages alone and for one call
// at a time. That kept a close away from the rows of that call: one that read
// an outage as having no end, before the call brought the backup's end for it
// at the same second, wrote that end again after it, because the merge skips
// a row only when one with its key is stored already. It left every gap
// between two calls open, and a caller that streamed an outage history
// through here in batches would have had the second end back, and the length
// that counts unwatched time, both of which the hold exists to prevent. A
// caller that brings them in batches now has to take the hold.
func (s *Store) ImportTableBatch(ctx context.Context, table string, rows []map[string]any, perTS map[int64]int) (int, error) {
	if outageTables[table] {
		return 0, fmt.Errorf("import %s: the outage history goes in only under the hold on the outage record (HoldOutageRecord)", table)
	}
	return s.importTableBatch(ctx, table, rows, perTS)
}

// outageTables are the downtime category's tables, the outages and the time
// that was not watched, which go in only under the hold on the outage record
// (HoldOutageRecord): Store.ImportTableBatch refuses them.
var outageTables = map[string]bool{"events": true, "pauses": true, "pauses_quarantine": true}

// OutageHold is a restore's hold on the outage record: outageMu, from
// HoldOutageRecord to Release. It belongs to the goroutine that took it, like
// the lock itself.
type OutageHold struct {
	s        *Store
	released bool
}

// HoldOutageRecord takes outageMu for the whole of a restore's downtime
// category, and keeps it until Release: the outages (events), the paused time
// (pauses) and the held-aside pauses (pauses_quarantine), which our exports
// write one after the other. The rows go in through the hold's
// ImportTableBatch.
//
// Taken per call, as ImportTableBatch once took it for the outages, the lock
// left every gap between two batches open, and a restore brings a big history
// in batches (5,000 rows in the web layer). An outage whose 'down' ended one
// batch had its 'up' in the next. The latency category comes first in a backup,
// so its samples already proved the recovery, and a close run in the gap -
// Delete now, or a cleanup that had reached those samples - read the 'down' as
// having no end and wrote one at the first good second. The next batch then
// stored the backup's own 'up' at the second the monitor had dated it, a
// different second, so the merge kept both: one outage, two ends, and the
// readers counted the second as an outage of its own.
//
// The paused time is held with the outages because the close reads it too: it
// takes the paused seconds out of the length it writes (pausedOverlap). An
// outage that a restart left open is usually followed by a pause, the one the
// next process books for the time it was not running (anything over two
// minutes), and our exports write the paused time after the outages. A close
// run between the two wrote that outage an end whose length counted the
// unwatched stretch as downtime, which the paused time, landing next, then
// also took out of the watched time: booked twice, for good, as the close's
// own comments describe. Held across both, a close sees the restore's downtime
// category whole or not at all. The held-aside pauses ride along, though the
// close does not read them: they are part of the same category, a hand-built
// file can put them between the other two, and our exports write them last,
// and only when there are any.
//
// What the hold costs. It is held while the restore reads its downtime rows
// from the request, so for as long as the client takes to send them, not only
// while they are stored. Measured on a laptop, with the file already there, a
// restore took about a quarter of a second over 20,000 outage rows and 1,000
// pauses, and about two and a half over 200,000 and 10,000. A stalled upload
// is cut off by the web layer's read deadline, which a batch rearms, and a
// failed restore lets go at once. What waits meanwhile is what takes
// outageMu: a Delete now (the latency close and the downtime delete), the
// delete of one outage, and the hourly cleanup, at its close before its first
// delete and at its sweep of old outages. None of them holds anything another
// goroutine needs while it waits (see outageMu), so a cleanup starts late,
// and a Delete now answers once the restore has moved past its outages. Those
// waits do not follow the waiter's context: a request whose client has gone,
// or a cleanup at shutdown, still waits for its turn and gives up only then.
// The monitor never waits: its writes do not take the lock.
//
// Taken with nothing of this package held and no transaction open, as
// outageMu always is. The restore may hold the web layer's importMu already (a
// file that carries its config ahead of its outages); it lets go of this hold
// before it moves on to any other category, so it never takes importMu with
// outageMu held.
func (s *Store) HoldOutageRecord() *OutageHold {
	s.outageMu.Lock()
	return &OutageHold{s: s}
}

// ImportTableBatch is Store.ImportTableBatch for the holder of the outage
// record, and the way the downtime category's tables go in: the store's own
// refuses them. It does not take outageMu again: the hold has it, and a
// sync.Mutex taken twice by one goroutine waits for itself forever. After
// Release it refuses, rather than write outage rows with nothing held.
func (h *OutageHold) ImportTableBatch(ctx context.Context, table string, rows []map[string]any, perTS map[int64]int) (int, error) {
	if h.released {
		return 0, fmt.Errorf("import %s: the hold on the outage record has been released", table)
	}
	return h.s.importTableBatch(ctx, table, rows, perTS)
}

// Release lets go of the outage record. A second call does nothing, and so
// does a call on a nil hold, so a restore can release it wherever it stops.
func (h *OutageHold) Release() {
	if h == nil || h.released {
		return
	}
	h.released = true
	h.s.outageMu.Unlock()
}

// importTableBatch is ImportTableBatch's work, with outageMu held by the
// caller wherever the rows are outage rows.
func (s *Store) importTableBatch(ctx context.Context, table string, rows []map[string]any, perTS map[int64]int) (int, error) {
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
	realCol := map[string]bool{}
	for _, c := range t.realCols {
		realCol[c] = true
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
	// An imported row is stored only where no row with its key is. A waiting
	// reading with that key is not stored yet and would be stored twice, so
	// what waits goes in first, here and before every later chunk. A restore
	// that stops part way is what this function works hardest to avoid, so it
	// goes on when the save fails.
	s.SaveBuffered(SaveOrder)
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
	if table == "settings" {
		rows = withBestOfCount(rows)
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
			// Normalise the accounting marker to the 1 the daemon writes. `failed` is
			// on the int allowlist below, which keeps it from arriving as text, but
			// the allowlist is a TYPE check with no range: a crafted or hand-edited
			// file can carry any whole number, and every value but 1 is a marker
			// nothing in this codebase has a meaning for.
			//
			// Clamped, not dropped, and the row kept - the other two options are both
			// losses. Dropping the field leaves NULL, which every read then serves as
			// a real 0.00 Mbps run, the exact fabrication the column exists to
			// prevent; dropping the row throws away the bytes it exists to record,
			// which are on the user's bill either way. Clamping is also what the byte
			// guard above does with an out-of-range number, and 1 is already how every
			// measurement read treats a non-zero marker (see speedNotFailed), so this
			// changes no row's meaning - only whether the rest of the code can read it.
			//
			// normVal decides what "a whole number" is here so this agrees with the
			// intCol guard below rather than second-guessing it: a value that is not
			// one (a fraction, NaN, out of int64 range) is left exactly as it came and
			// that guard skips the row, as it does today.
			//
			// This does NOT stand in for the predicate agreement in
			// SpeedColumnsPastSchema4InUse - a row stored before this clamp existed
			// keeps its value forever, since nothing rewrites rows at rest. What it
			// buys is that `failed` means one thing at rest: the reads that hide these
			// rows, the export check that decides whether a backup must carry the
			// column, and the delete that clears a run's usage all judge the same
			// values. They agree today whatever the marker is - each one asks "is it
			// non-zero" rather than "is it 1" - and the clamp is what keeps a future
			// `failed = 1` written somewhere new from quietly disagreeing with them.
			if v, ok := r["failed"]; ok && v != nil {
				if nv, ok := normVal(v); ok {
					if n, isInt := nv.(int64); isInt && n != 0 {
						r["failed"] = int64(1)
					}
				}
			}
		}
		// Reject a row whose ts isn't a finite number >= 0: imported JSON is
		// untrusted, and a NaN/Inf/negative ts silently corrupts retention pruning
		// (DELETE ... WHERE ts < cutoff) and the event-derived uptime math.
		var tsKey int64
		hasTS := false
		tsv, ok := r["ts"]
		if !ok {
			// speed_servers keys its time by run_ts - the one keyed time-series
			// table whose column is not literally "ts". Same sanity and flood
			// rules, or a crafted backup packs unbounded rows at one run_ts and
			// escapes the cap this block exists for.
			tsv, ok = r["run_ts"]
		}
		if ok {
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
		norm := make(map[string]any, len(t.cols))
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
				// This is an ALLOWLIST on purpose. normVal turns a whole JSON number
				// into int64 and leaves every type it does not recognise alone, so a
				// denylist of float64 (the fractional case) let all the others straight
				// through - a string, a bool, anything. SQLite does not enforce column
				// types, so `"duration_s": "oops"` landed as TEXT and then broke every
				// read that scans the column into an int64: one crafted or hand-edited
				// backup row 500s the heatmap and the uptime query for the whole
				// retention window. nil is the single legitimate non-integer (JSON null
				// for a nullable column - an 'up' event carries no duration_s); the
				// notNull check above already rejects it where a value is required.
				if intCol[c] && nv != nil {
					if _, isInt := nv.(int64); !isInt {
						bad = true
						break
					}
				}
				// The same allowlist for REAL-affinity columns, and for the same
				// reason: SQLite does not enforce column types, so `"latency_ms":
				// "oops"` landed as TEXT and every read that scans it into a Go
				// float64 fails - one crafted row 500s the latency chart or the
				// whole speed history for its entire window. Worse where SQLite
				// coerces instead of erroring: inside AVG a non-numeric string
				// counts as 0, so a poisoned DNS row reports a perfect 0 ms rather
				// than failing visibly.
				//
				// normVal has already turned every JSON number into int64 or
				// float64, so those two ARE the valid set; nil is the legitimate
				// third (a nullable column for something genuinely unmeasured,
				// which is most of them).
				if realCol[c] && nv != nil {
					if _, isF := nv.(float64); !isF {
						if _, isInt := nv.(int64); !isInt {
							bad = true
							break
						}
					}
				}
				norm[c] = nv
			}
		}
		// Types are right; now the SEMANTICS. Type validity is not enough for a row
		// whose meaning is an interval - see rowSane. It runs BEFORE the insert list
		// is built, and takes `norm` by reference, so it can also REPAIR a row rather
		// than only reject it: dropping one impossible field is often the honest
		// outcome where dropping the whole row is not. An imported 'up' carrying a
		// negative duration is the case that forced this - discarding the row would
		// discard the RECOVERY too, leaving a dangling 'down' that reads as an outage
		// still in progress, which is worse than the number it was trying to fix.
		if !bad && t.rowSane != nil && !t.rowSane(norm) {
			bad = true
		}
		for _, c := range t.cols {
			if nv, ok := norm[c]; ok {
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
			// Held rows are DURABLE from this commit on, so the re-judgement
			// arms here, not only at the happy-path return: if the next
			// BeginTx fails (a cancellation landing just after the commit),
			// the caller sees an error, retries - and the retried rows are
			// all NOT EXISTS no-ops that would never arm. The rows would then
			// sit out of coverage until a restart or the next clock step.
			if table == "pauses_quarantine" && pending > 0 {
				s.pauseRepairArm.Add(1)
			}
			committed, pending, inTx = committed+pending, 0, 0
			if importChunkHook != nil {
				importChunkHook()
			}
			// Between two chunks no transaction is open, and rounds have
			// arrived while the last one ran.
			s.SaveBuffered(SaveOrder)
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
	// Freshly imported held rows must not wait for a restart (or the next
	// clock step) to be re-judged: arm a generation so the next write runs
	// the same exoneration a local quarantine gets. Eligibility still holds -
	// if a step happens to be settling, the repair parks until it clears.
	if table == "pauses_quarantine" && committed+pending > 0 {
		s.pauseRepairArm.Add(1)
	}
	return committed + pending, nil
}

// normVal turns JSON-decoded whole-number float64s into int64 so INTEGER columns
// and key comparisons stay exact (e.g. ts, success). Reports ok=false for a
// non-finite float (NaN/Inf): SQLite has no NaN/Inf literal and storing one
// would poison aggregates - the caller drops the row.
// pauseRowSane rejects pause rows whose interval is not one. A pause covers
// [ts, ts+duration_s) and is the uptime DENOMINATOR, so a bad row does not look
// wrong, it silently rewrites how much of a window the monitor claims to have
// watched.
//
// Two ways a type-valid row is still nonsense:
//
//   - duration <= 0 is not a span. A negative one runs the interval BACKWARDS, so
//     pausedOverlap's MIN(end)-MAX(start) subtracts a quantity with no meaning.
//   - ts+duration_s must not overflow int64. Go computes that endpoint (pauseSpans,
//     spanOverlap) while SQLite promotes the same expression to REAL, so a row near
//     MaxInt64 makes the two disagree about where the pause ends - and the Go side
//     wraps to a huge NEGATIVE endpoint that sorts before its own start.
//
// A negative ts is left alone deliberately: it predates the epoch, which the
// window clamps already handle, and is not the same class of hazard.
func pauseRowSane(row map[string]any) bool {
	ts, okTS := row["ts"].(int64)
	dur, okDur := row["duration_s"].(int64)
	if !okTS || !okDur {
		return false // both are NOT NULL; a missing one is already rejected upstream
	}
	if !pauseSpanImportable(ts, dur, time.Now().Unix()) {
		// Never silently. A dropped pause row is the uptime denominator going
		// missing while the events and samples from the same file land, so the
		// restored box publishes a DIFFERENT uptime and coverage from the machine
		// the backup came from - and the merge-by-ts counts make a shortfall
		// indistinguishable from rows already present. The file still holds the
		// rows; if the refusal was clock skew, a re-run after sync lands them.
		stats.Inc("import.pause_dropped")
		log.Printf("pingularity: import dropped pause row (ts=%d duration_s=%d): not a believable span; if this is a genuine backup and this machine's clock was behind when it was restored, re-run the import once the clock has synced", ts, dur)
		return false
	}
	return true
}

// quarantineRowSane vets an imported HELD pause row: structurally a span
// (positive, epoch-plausible, bounded by the retention ceiling) but with NO
// future-end cap - reaching past the clock is why the row is in quarantine,
// and pauseRowSane's importable check would reject every genuine held row.
// Restoration into coverage is not decided here: the row lands back in
// quarantine and only the destination's re-judgement (pauseRepairFutureSkew)
// can move it, so a crafted file gets no wider claim on coverage than the
// local quarantine machinery already grants rows at rest.
func quarantineRowSane(row map[string]any) bool {
	ts, okTS := row["ts"].(int64)
	dur, okDur := row["duration_s"].(int64)
	if !okTS || !okDur {
		return false
	}
	if !pauseSpanBounded(ts, dur) {
		stats.Inc("import.pause_quarantine_dropped")
		log.Printf("pingularity: import dropped quarantined pause row (ts=%d duration_s=%d): not a structurally believable span", ts, dur)
		return false
	}
	return true
}

// PauseSpanSane reports whether a pause span means anything. Exported and shared
// so EVERY writer is held to one rule: this began as an import-only check, and the
// monitor - writing through InsertPause, which validated nothing but a positive
// duration - could therefore persist a row the importer would have rejected. It
// did, on any board without an RTC: booting near the epoch with monitoring off and
// then syncing produced a single span of half a million hours, which is subtracted
// from every uptime window and drives observation coverage to nothing.
func PauseSpanSane(ts, dur int64) bool {
	if !pauseSpanBounded(ts, dur) {
		return false
	}
	// And it has to END by about now. Bounding only the start and the length still
	// admitted a span beginning two hours ago and running to the ceiling: every
	// individual check passed, and the row then clamped to every window anyone asked
	// about, so coverage read zero for a decade. Prune deliberately will not rewrite
	// a row that straddles its cutoff, so retention never repairs it either.
	//
	// A pause describes time that has ALREADY gone unobserved, so an end in the
	// future is not a measurement. The skew allowance is for an honest disagreement
	// between the clock that wrote the row and the one checking it - a backup moved
	// between machines, or a resume mid-flush - not for a span that means to reach
	// forward.
	return ts+dur <= time.Now().Unix()+pauseFutureSkew
}

// pauseSpanBounded is the clock-free core of the pause rule - what can be said
// about a span from the row alone, with no appeal to the present: a positive
// length, a start after the project existed, a believable duration, a start
// below the shared ts ceiling (which also keeps the endpoint from
// overflowing). repairInsanePausesAt applies exactly these bounds too.
func pauseSpanBounded(ts, dur int64) bool {
	if dur <= 0 {
		return false
	}
	// A pause before the project existed describes nothing, and is the same class
	// of hazard as an implausible length: it anchors nothing and only subtracts.
	if ts < plausibleEpoch {
		return false
	}
	// And it cannot be longer than the longest history this product will ever hold.
	// A pause row is the uptime DENOMINATOR and pauseSpans clamps it to whatever
	// window is asked about, so ONE row claiming a century subtracts every window
	// entirely: coverage reads 0 on the pill, /metrics drops the ratio series, the
	// digest declines to state a percentage, and every heatmap day mints as "not
	// monitored" - permanently, because Prune's straddle rule deliberately will not
	// rewrite a pause row's span. Nothing legitimate produces one: the monitor
	// writes a pause per episode, bounded by the checkpoint cadence.
	if dur > maxPauseDuration {
		return false
	}
	// The ts ceiling is the REPAIR's ceiling, not the tightest per-row overflow
	// bound. ts <= MaxInt64-dur was enough to keep ts+duration_s computable, but
	// the at-Open repair's clock-free DELETE (repairInsanePausesAt) removes
	// everything above MaxInt64-maxPauseDuration outright - constant arguments,
	// so no per-row arithmetic can itself overflow in SQL - and a row in the
	// band between the two bounds (ts=MaxInt64-100, dur=50) was accepted at the
	// door only to be deleted at the next Open and mislogged as an older build's
	// residue. A start within ten years of int64's end is no wall time anyway
	// (year 292 billion), so refuse the whole band and keep door and repair in
	// exact agreement. This also implies ts+dur cannot overflow, since dur is
	// already capped at maxPauseDuration above.
	return ts <= math.MaxInt64-maxPauseDuration
}

// pauseSpanImportable is the importer's acceptance rule, split from the live one
// because the future-end check anchors on the clock CHECKING the row - for a
// restore, the DESTINATION's - while the rows were written by the source's. On a
// machine whose clock is implausibly early (an RTC-less board restored before NTP
// syncs: the very hardware plausibleEpoch exists for) now+skew predates every
// believable ts, so the live rule would fail EVERY genuine span in the file: the
// outage events and samples land, the whole denominator vanishes, and the box
// publishes higher uptime and full coverage for windows the source knew were
// unobserved. A clock that early cannot anchor a judgement - Prune already
// declines to prune under it - so fall back to the clock-free bounds instead of
// vetoing the table. Under a plausible clock the future-end tradeoff stands
// exactly as the live rule has it: an accepted future-reaching span clamps into
// every queried window and Prune never repairs it, which is the worse failure.
func pauseSpanImportable(ts, dur, nowU int64) bool {
	if !pauseSpanBounded(ts, dur) {
		return false
	}
	if nowU < plausibleEpoch {
		return true
	}
	return ts+dur <= nowU+pauseFutureSkew
}

// pauseFutureSkew is how far past the present a pause may claim to end before it
// stops being a measurement and starts being a claim about the future.
const pauseFutureSkew = 5 * 60

// maxPauseDuration is the longest single unobserved span that can be believed,
// mirroring config.MaxRetention (10 years) - the ceiling the settings layer puts
// on how long any history is kept. Duplicated as a literal rather than imported
// so `store` keeps no dependency on `config`; the two are pinned together by
// TestMaxPauseDurationTracksRetentionCeiling.
const maxPauseDuration = 10 * 365 * 24 * 3600

// eventRowSane vets an imported outage row for meaning, not just for type. The
// events table drives every uptime figure, and two shapes in it are not data:
//
//   - a type that is neither 'down' nor 'up'. The readers switch on exactly those
//     two and silently ignore anything else, so a third value is a row that will
//     never be counted but still occupies its (ts, type) key.
//   - an impossible duration_s. InsertEvent stores NULL when the caller passes a
//     negative, so the product cannot emit one; a negative in a file is corrupt by
//     construction, and it used to reach observedOutageSpans' "no limit" branch and
//     book the whole down-to-up gap as downtime. A positive one is bounded too,
//     at maxPauseDuration - the same retention ceiling the pause guard uses -
//     because completedOutagesSince anchors an unpaired 'up' at ts-duration_s,
//     so one row claiming 1e15 seconds is an outage reaching back thirty million
//     years that every queried window lands inside: the exact single-row rewrite
//     of every uptime figure this guard exists to stop, through its other door.
//
// The two are handled differently ON PURPOSE. An unreadable type makes the whole
// row meaningless, so it goes. A bad duration does not: the row's PRIMARY content
// is the transition - the link came back at this second - and that is worth
// keeping. Dropping the row instead would delete the recovery and leave the
// preceding 'down' dangling, which every reader treats as an outage still running,
// so a file claiming one impossible number would have been "corrected" into an
// unbounded ongoing outage. The duration alone is dropped; a completed outage with
// no recorded length is a shape the store already handles.
//
// A MISSING duration is left alone: that is the real shape of a 'down' row and of
// an 'up' whose outage was never closed.
func eventRowSane(row map[string]any) bool {
	// Require a STRING, then require it to be one of the two. Checking the enum only
	// when the value happened to already be a string left the guard open to every
	// other JSON type: normVal turns a whole number into int64 and leaves a bool
	// alone, neither is a string, so both skipped the check and SQLite's TEXT
	// affinity stored them as "7" and "1". Such a row is invisible to every reader -
	// they all switch on down/up - while still occupying its (ts, type) key, and it
	// is not inert: UptimeSince decides whether an outage is still running from the
	// NEWEST event, so an unreadable one arriving after a dangling 'down' reads as
	// "not down" and erased a real ongoing outage from the uptime figure while the
	// heatmap went on counting it.
	typ, ok := row["type"].(string)
	if !ok || (typ != "down" && typ != "up") {
		return false
	}
	if dur, ok := row["duration_s"].(int64); ok && (dur < 0 || dur > maxPauseDuration) {
		delete(row, "duration_s") // keep the transition, discard the impossible length
		// Counted so a restore that repaired rows is visible on /metrics rather
		// than silent - the last time an import quietly rewrote a figure, nothing
		// said so and the divergence went unnoticed until audit.
		stats.Inc("import.event_duration_dropped")
	}
	return true
}

func normVal(v any) (any, bool) {
	switch f := v.(type) {
	// Go integer kinds normalize to int64 so downstream sees ONE integer type.
	// A JSON decode only ever yields float64, but ImportTable is also called
	// directly with hand-built maps (tests, and any in-process caller), and those
	// carry plain ints - which the intCols allowlist would otherwise reject as
	// "not an integer" for being the wrong Go type.
	case int:
		return int64(f), true
	case int8:
		return int64(f), true
	case int16:
		return int64(f), true
	case int32:
		return int64(f), true
	case int64:
		return f, true
	case uint:
		if uint64(f) > math.MaxInt64 {
			return nil, false
		}
		return int64(f), true
	case uint8:
		return int64(f), true
	case uint16:
		return int64(f), true
	case uint32:
		return int64(f), true
	case uint64:
		if f > math.MaxInt64 {
			return nil, false
		}
		return int64(f), true
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
