// Package settings holds Pingularity's runtime-adjustable values, persisted in
// the database (so web-UI changes survive restarts) and read live by the
// monitor, scheduler, and pruner.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/pingular/pingularity/internal/secret"
	"github.com/pingular/pingularity/internal/store"
)

// Persisted setting keys.
const (
	keyLatency           = "latency_interval_s"
	keyLatencyEnabled    = "latency_enabled"
	keyDNSProbe          = "dns_probe_enabled"
	keyNetinfo           = "netinfo_enabled" // connection-info lookups (public IP, ISP, geo, exit)
	keySpeed             = "speed_interval_s"
	keyRetention         = "retention_s"          // latency; 0 = keep forever
	keySpeedRet          = "speed_retention_s"    // speed history; 0 = keep forever
	keyDowntimeRet       = "downtime_retention_s" // outages; 0 = keep forever
	keyTimeout           = "timeout_s"
	keyDownAfter         = "down_after"
	keyUpAfter           = "up_after"
	keySpeedServer       = "speed_server_id"
	keySpeedAutoLoc      = "speed_auto_loc"   // "lat,lon" the auto picker centres on ("" = your IP)
	keySpeedAutoLabel    = "speed_auto_label" // human label for the auto location
	keySpeedEnabled      = "speedtest_enabled"
	keySpeedOnRecon      = "speedtest_on_reconnect"
	keyIPv6Mode          = "ipv6_mode"
	keyExitTarget        = "exit_target" // host/IP the exit-router traceroute heads toward ("" = 1.1.1.1)
	keyMonitoring        = "monitoring"
	keyThreshDown        = "thresh_down_mbps"     // min acceptable download; 0 = off
	keyThreshUp          = "thresh_up_mbps"       // min acceptable upload; 0 = off
	keyThreshPing        = "thresh_ping_ms"       // max acceptable ping; 0 = off
	keyThreshJitter      = "thresh_jitter_ms"     // max acceptable jitter; 0 = off
	keyThreshLoss        = "thresh_loss_pct"      // max acceptable packet loss %; 0 = off
	keyThreshConsec      = "thresh_consecutive"   // consecutive breaches before alerting; <2 = every run
	keyThreshBloatDown   = "thresh_bloat_down_ms" // max download bufferbloat (added ms under load); 0 = off
	keyThreshBloatUp     = "thresh_bloat_up_ms"   // max upload bufferbloat (added ms under load); 0 = off
	keyAlertOnOutage     = "alert_on_outage"      // notify on link down/up
	keyWebhookURL        = "webhook_url"          // generic alert webhook ("" = off)
	keyWebhookFormat     = "webhook_format"       // webhook payload shape: ""/"auto" = detect by host, "ntfy", "generic"
	keyHeartbeatURL      = "heartbeat_url"        // dead-man's-switch ping URL ("" = off)
	keyDigestFreq        = "digest_freq"          // periodic summary cadence: off|daily|weekly
	keySchedLatEnabled   = "sched_lat_enabled"    // gate latency probing to its windows
	keySchedLatWindows   = "sched_lat_windows"    // JSON array of Window
	keySchedSpeedEnabled = "sched_speed_enabled"  // gate scheduled/reconnect speedtests to its windows
	keySchedSpeedWindows = "sched_speed_windows"  // JSON array of Window
	// Legacy single-window schedule keys (pre-multi-window), read once to migrate.
	keyScheduleEnabled = "schedule_enabled"  // legacy master toggle (gated both features)
	keySchedSpeedDays  = "sched_speed_days"  // 7-char 0/1 mask, index = time.Weekday (Sun=0)
	keySchedSpeedStart = "sched_speed_start" // window start, minutes from midnight
	keySchedSpeedEnd   = "sched_speed_end"   // window end, minutes from midnight (==start ⇒ all day)
	keySchedLatDays    = "sched_lat_days"
	keySchedLatStart   = "sched_lat_start"
	keySchedLatEnd     = "sched_lat_end"
	keyAccessLocalOnly = "access_local_only"    // serve only loopback clients
	keyAuthEnabled     = "auth_enabled"         // require a login
	keyAuthUser        = "auth_user"            // login username (default "admin")
	keyAuthHash        = "auth_hash"            // bcrypt hash of the password ("" = unset)
	keyAuthSessEpoch   = "auth_session_epoch"   // logout revocation epoch (folded into token MACs; persisted so logout survives a restart)
	keyUpdateCheck     = "update_check_enabled" // daily background poll for a newer release
	keyQuickSetup      = "quick_setup_done"     // the first-run Quick Setup dialog was answered or dismissed; never show it again
	keyLogLevel        = "log_level"            // logging: "off" (nothing) or any other value = on (full debug). UI sends debug|off
	keyLogRedactPII    = "log_redact_pii"       // censor PII values (IPs/ISP/hostnames/user) in logs
	// Adaptive / event-driven speedtesting (see internal/speedtest, internal/monitor).
	keySpeedAdaptive   = "speedtest_adaptive"    // shorten the interval while the last run was unhealthy
	keySpeedOnDegraded = "speedtest_on_degraded" // run a test when latency degrades (not just on reconnect)
	keyDegradedPingMS  = "degraded_ping_ms"      // latency (ms) that counts as degraded; 0 = off
	keySpeedSkipBusy   = "speedtest_skip_busy"   // defer a scheduled test while the link is already busy
	keySpeedBusyMbps   = "speedtest_busy_mbps"   // throughput (Mbps) above which the link counts as busy
	// Speedtest engine selection (see internal/speedtest).
	keySpeedEngine  = "speed_engine"  // ookla|iperf3 - which backend runs speedtests
	keyIperfServer  = "iperf_server"  // host[:port] of the ACTIVE iperf3 server (engine=iperf3)
	keyIperfServers = "iperf_servers" // JSON array of saved IperfTarget (the picker list)
	// Test parameters. The speed_* keys apply to whichever engine is selected; the
	// iperf_* / ookla_* ones are that engine's own knobs and are ignored by the other.
	keySpeedDirection   = "speed_direction"   // Ookla directions: both|down|up (sequential; no bidir)
	keySpeedRetries     = "speed_retries"     // Ookla extra attempts per direction on a transient failure
	keyIperfDirection   = "iperf_direction"   // iperf3 directions: both|down|up|bidir (migrated from speed_direction)
	keyIperfRetries     = "iperf_retries"     // iperf3 extra attempts per direction (migrated from speed_retries)
	keyIperfDur         = "iperf_duration"    // seconds per direction (-t)
	keyIperfStreams     = "iperf_streams"     // parallel TCP streams (-P)
	keyIperfOmit        = "iperf_omit"        // warm-up seconds discarded (-O)
	keyIperfUDP         = "iperf_udp"         // run the UDP loss/jitter pass
	keyIperfUDPRate     = "iperf_udp_rate"    // UDP probe rate Mbps; 0 = auto
	keyIperfWindow      = "iperf_window"      // TCP window / socket-buffer KB (-w); 0 = auto
	keyIperfCongest     = "iperf_congestion"  // TCP congestion algorithm (-C); "" = system default
	keyIperfNoDelay     = "iperf_nodelay"     // disable Nagle (-N)
	keyIperfDSCP        = "iperf_dscp"        // IP DSCP marking (--dscp); "" = none
	keyIperfMSS         = "iperf_mss"         // TCP max segment size bytes (-M); 0 = auto
	keyOoklaConnections = "ookla_connections" // Ookla parallel connections (0 = library default)
	keyOoklaLoss        = "ookla_loss"        // run Ookla's UDP packet-loss probe
	keySpeedBestOf      = "speed_best_of"     // Ookla: race 3 servers, keep the best result
	// Per-server network path + RSA auth (bind, ipver, auth, username, password, rsa
	// key) live inside each IperfTarget, serialized in keyIperfServers - not as their
	// own keys - so they're scoped to the server they belong to.
)

// Test-parameter bounds (mirrored by the UI inputs' min/max).
const (
	MinIperfDur     = 1
	MaxIperfDur     = 30
	MinIperfStreams = 1
	MaxIperfStreams = 32

	MaxOoklaConnections = 16 // ceiling for Ookla parallel connections; 0 = library default
	MaxIperfOmit        = 5
	MaxIperfWindow      = 65536 // KB (64 MB) ceiling for -w; 0 = auto
	MaxSpeedRetries     = 3     // ceiling for per-direction retries; 0 = no retry
	MaxIperfMSS         = 9000  // bytes ceiling for -M (jumbo-frame headroom); 0 = auto
)

// AllDays selects every weekday (the schedule default).
const AllDays = "1111111"

// maxWindows bounds how many windows a single feature's schedule may hold
// (storage + UI sanity).
const maxWindows = 12

// Window is one active range in a schedule: the weekdays it covers (7-char
// "0/1" mask indexed by time.Weekday, Sun=0) and a start/end minute-of-day.
// Start==End means all day; Start>End wraps past midnight.
type Window struct {
	Days  string `json:"days"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// maxIperfServers bounds the saved iperf3 server list (storage + UI sanity).
const maxIperfServers = 12

// IperfTarget is one saved iperf3 server. Beyond the label and "host"/"host:port"
// address, each carries its own network path (Bind, IPVer) and RSA auth (Auth,
// Username, Password, RSAKey) - these belong to the server, not to global
// preferences, so the VPS gets its credentials and the LAN box does not. The
// tester reads whichever target matches the active IperfServer (see
// Values.activeIperf); the test-shape knobs (duration, streams, ...) stay global
// on Values.
//
// Password is the secret (iperf3's IPERF3_PASSWORD): stored in the JSON but
// write-only over the API - the web layer sends a has_password flag instead.
type IperfTarget struct {
	Label    string `json:"label"`
	Addr     string `json:"addr"`
	Bind     string `json:"bind,omitempty"`     // --bind source address ("" = default route)
	IPVer    string `json:"ipver,omitempty"`    // "auto"|"4"|"6" address-family pin
	Auth     bool   `json:"auth,omitempty"`     // require RSA auth for this server
	Username string `json:"username,omitempty"` // --username
	Password string `json:"password,omitempty"` // IPERF3_PASSWORD (secret; write-only over the API)
	RSAKey   string `json:"rsa_key,omitempty"`  // server's RSA PUBLIC key PEM
	// PKCS1 forces the legacy --use-pkcs1-padding for servers on an iperf3 build
	// predating the OAEP padding change (e.g. unpatched 3.16); without it a
	// patched client + unpatched server fail auth ("test authorization failed").
	PKCS1 bool `json:"pkcs1,omitempty"`
}

// activeIperf returns the saved target whose address matches the active
// IperfServer, or a zero target when none matches (no bind, auto IP family,
// auth off).
func (v Values) activeIperf() IperfTarget {
	for _, t := range v.IperfServers {
		if t.Addr == v.IperfServer {
			return t
		}
	}
	return IperfTarget{}
}

// Bounds keep UI input sane.
const (
	MinLatency = 1 * time.Second
	MaxLatency = time.Hour
	MinSpeed   = time.Minute
	MaxSpeed   = 24 * time.Hour
	MinTimeout = 1 * time.Second
	MaxTimeout = 30 * time.Second
	MinStreak  = 1
	MaxStreak  = 10
	// MaxDuration caps every persisted duration (secs() clamps reads to it) -
	// exported so config's flag validation can reject what storage would rewrite.
	MaxDuration = 10 * 365 * 24 * time.Hour
)

// Values is a snapshot of all runtime settings.
type Values struct {
	Latency              time.Duration
	LatencyEnabled       bool
	DNSProbe             bool // measure DNS-resolution latency each probe round
	NetinfoEnabled       bool // look up public IP / ISP / geo / exit path (third-party services)
	Speed                time.Duration
	Retention            time.Duration // latency samples
	SpeedRetention       time.Duration // speed history
	DowntimeRetention    time.Duration // outage history (heatmap)
	Timeout              time.Duration
	DownAfter            int
	UpAfter              int
	SpeedServerID        string // Ookla server ID ("" = auto)
	SpeedAutoLoc         string // "lat,lon" the auto picker centres on ("" = your IP)
	SpeedAutoLabel       string // human label for SpeedAutoLoc
	SpeedtestEnabled     bool
	SpeedtestOnReconnect bool
	IPv6Mode             string // "auto" | "on" | "off"
	// ExitTarget is where the exit-router traceroute heads; "" -> 1.1.1.1. Since
	// different destinations leave the ISP in a different number of hops, this lets
	// the exit/handoff readout follow the path the user cares about.
	ExitTarget string
	Monitoring bool // master probe on/off (power button)

	// Alerting.
	ThreshDownMbps float64 // min acceptable download; 0 = no threshold
	ThreshUpMbps   float64 // min acceptable upload;   0 = no threshold
	ThreshPingMS   float64 // max acceptable ping;     0 = no threshold
	ThreshJitterMS float64 // max acceptable jitter;   0 = no threshold
	ThreshLossPct  float64 // max acceptable packet loss %; 0 = no threshold
	ThreshConsec   int     // consecutive threshold breaches before alerting (>=1; 1 = every run)
	// Bufferbloat: the latency ADDED under load (loaded - idle), per direction;
	// 0 = no threshold. Mirrors how the dashboard defines bufferbloat.
	ThreshBloatDownMS float64
	ThreshBloatUpMS   float64
	AlertOnOutage     bool   // notify the webhook when the link goes down/up
	WebhookURL        string // generic alert webhook ("" = disabled)
	WebhookFormat     string // payload shape override: ""/"auto" = detect by host, "ntfy", "generic"
	HeartbeatURL      string // dead-man's-switch ping URL ("" = disabled)
	DigestFreq        string // periodic summary cadence: "off" | "daily" | "weekly"

	// Schedule - latency probing and scheduled/reconnect speedtests each gate to
	// their own window list when enabled (disabled ⇒ 24/7); a feature runs when
	// ANY of its windows is active.
	SchedLatEnabled   bool
	SchedLatWindows   []Window
	SchedSpeedEnabled bool
	SchedSpeedWindows []Window

	// Access control. AccessLocalOnly serves only loopback clients (a live
	// filter, not a rebind). AuthEnabled requires a login; AuthHash is the
	// password's bcrypt ("" = unset, which keeps auth inert even if AuthEnabled
	// is true, avoiding a lockout).
	AccessLocalOnly bool
	AuthEnabled     bool
	AuthUser        string
	AuthHash        string

	// UpdateCheckEnabled gates the daily background poll for a newer release (see
	// internal/update): default on, a plain GET to the public version endpoint
	// with no identity sent. Owned by its own toggle, not the settings form.
	UpdateCheckEnabled bool

	// QuickSetupDone records that the first-run Quick Setup dialog was answered
	// (or dismissed - dismissal is an answer: keep the defaults). The dashboard
	// offers the dialog only while this is false AND the install is young (see
	// the status handler); either exit persists true so it never returns.
	QuickSetupDone bool

	// LogLevel is the logging switch: "off" logs nothing; any other value turns
	// full (debug) logging on, for both the console output and the in-memory buffer
	// the dashboard shows (the UI only sends "debug" or "off"). Owned by the
	// About-tab logging control, not the settings form.
	LogLevel string

	// LogRedactPII, when true (the default), censors PII values - client/public
	// IPs, ISP, hostnames, DNS resolver, username - in log output, replacing the
	// value with a marker while keeping the field so the redaction is visible.
	// Turn off for full detail when diagnosing on your own box.
	LogRedactPII bool

	// Adaptive / event-driven speedtesting.
	SpeedtestAdaptive   bool    // shorten the interval while the last run breached a threshold
	SpeedtestOnDegraded bool    // fire a test when continuous latency degrades (beyond reconnects)
	DegradedPingMS      float64 // latency (ms) that counts as degraded; 0 = off
	SpeedtestSkipBusy   bool    // defer a scheduled test while the link is already moving data
	SpeedBusyMbps       float64 // throughput (Mbps) above which the link counts as busy

	// Speedtest engine. "ookla" uses the Ookla server network; "iperf3" runs
	// against the user's own server (IperfServer), and is honored only when the
	// iperf3 binary is present (capability-gated in the UI and at selection).
	SpeedEngine string // "ookla" | "iperf3"
	IperfServer string // host[:port] of the ACTIVE iperf3 server (engine=iperf3)
	// IperfServers is the saved picker list; IperfServer is the selected one (kept
	// separate so the tester reads a single address).
	IperfServers []IperfTarget
	// Direction and Retries are PER-ENGINE: the Ookla and iperf3 tabs each carry their
	// own, so tuning one engine never disturbs the other. The Speed* pair is Ookla's,
	// the Iperf* pair is iperf3's; a pre-split install seeds the iperf3 pair from the
	// old shared speed_* values on first load (see overlay).
	//
	// Direction is which directions to test: "both" | "down" | "up" | "bidir".
	// "both" runs download then upload; "bidir" runs both at once (--bidir) to expose
	// full-duplex contention - iperf3 only, since Ookla is always sequential (SpeedDirection
	// rejects "bidir" and falls back to "both"); a single direction halves data and time.
	// Retries is the extra attempts per direction on a transient failure - chiefly the
	// server still busy from the previous test; 0 = off. On iperf3 it also covers the UDP
	// pass, but not Ookla's loss probe (that one is best-effort: it fails silently).
	SpeedDirection string // Ookla: both|down|up
	SpeedRetries   int    // Ookla
	IperfDirection string // iperf3: both|down|up|bidir
	IperfRetries   int    // iperf3

	// iperf3 test parameters. Duration is seconds per direction; Streams is the
	// parallel TCP stream count (>1 better saturates fast/high-RTT links); Omit is
	// the warm-up seconds discarded before measuring (skips TCP slow-start).
	IperfDur     int
	IperfStreams int
	IperfOmit    int
	// IperfUDP gates the extra UDP pass that measures packet loss + jitter (TCP
	// can't); IperfUDPRate caps it (Mbps); 0 = auto (~half the TCP rate).
	IperfUDP     bool
	IperfUDPRate int
	// IperfWindow sets the TCP window / socket-buffer KB (-w); 0 = auto, raise it to
	// unlock throughput on high-BDP (long-distance) links.
	IperfWindow int

	// OoklaConnections is the number of parallel Ookla connections (0 = the
	// library's default). The Ookla analogue of iperf's parallel streams.
	OoklaConnections int
	// OoklaLoss gates Ookla's packet-loss probe - a few extra seconds of UDP after
	// the transfers. The Ookla analogue of IperfUDP (which also measures jitter;
	// Ookla gets jitter from its ping phase, so this only adds loss).
	OoklaLoss bool
	// SpeedBestOf runs a scheduled or manual Ookla test against the chosen server
	// AND the next two by ping, keeping only the best result. Costs ~3x the time and
	// data of a normal test. Ookla-only: iperf3 dials one configured host and has no
	// candidate list to rank.
	SpeedBestOf bool
	// Advanced iperf3 transfer knobs. Congestion is the TCP congestion algorithm (-C,
	// e.g. cubic/bbr); NoDelay disables Nagle (-N); DSCP marks the IP DiffServ value
	// (--dscp); MSS pins the TCP max segment size in bytes (-M). All optional - blank/0
	// keeps iperf3's default. Congestion/NoDelay/MSS are TCP-only; DSCP marks UDP too.
	IperfCongestion string
	IperfNoDelay    bool
	IperfDSCP       string
	IperfMSS        int
	// Per-server network path (bind, ipver) and RSA auth (username/password/key)
	// are not global - they live on each IperfTarget in IperfServers; the tester
	// reads whichever matches the active IperfServer (see activeIperf).
}

// Controller is the thread-safe source of truth for runtime settings.
type Controller struct {
	store    *store.Store
	mu       sync.RWMutex
	wmu      sync.Mutex // serializes writers (read-modify-write + persist); see mutate
	vals     Values
	defaults Values        // the seeded config defaults (immutable after New)
	changed  chan struct{} // closed+replaced on change to wake waiters
	crypter  Crypter       // seals iperf3 passwords at rest; nil = store them in the clear
	// sealedByAddr holds the original at-rest ciphertext for any iperf3 server whose
	// password could not be recovered at load (no crypter, or a key that no longer
	// matches the stored secrets), keyed by server address. sanitizeIperfServers
	// blanks such a password in memory so the tester never sends ciphertext; keeping
	// the ciphertext here lets a later save re-attach it (restoreSealed) instead of
	// persisting the blank, so an unrelated form save can't erase a still-recoverable
	// password. Rebuilt each load; set under wmu in New/Reload, read under wmu in mutate.
	sealedByAddr map[string]string
	// initErr records that the initial settings read failed, so the controller is
	// running on defaults rather than stored config. Writes are refused (see
	// mutate) to avoid clobbering the stored config with defaults, and the web
	// guard refuses to SERVE (see Loaded) because "no login configured" and "the
	// login could not be read" are the same answer from here and only one of them
	// is safe to act on. A successful Reload clears it.
	//
	// Atomic rather than wmu-guarded: Loaded is read on every request, and taking
	// the writer lock on the hot path to answer it would serialise the server.
	initErr atomic.Bool
	// sessionEpoch is the persisted session-revocation epoch, folded into every
	// session-token MAC. Kept in an atomic (not Values) so it's cheap to read on
	// every authenticated request and never flows through the export/normalize path.
	// Seeded from the store at load; BumpSessionEpoch advances it AND persists it, so
	// a logout stays revoked across a restart (an in-memory-only epoch reset to 0 on
	// restart, letting a logged-out token revalidate).
	sessionEpoch atomic.Int64
}

// ErrSettingsUnavailable is returned by settings writes when the initial load
// from the store failed: the controller is running on seeded defaults, so
// persisting the form would overwrite the operator's stored config with
// defaults. A successful Reload (SIGHUP / post-import) clears the condition.
var ErrSettingsUnavailable = errors.New("settings: initial load failed; refusing to persist over stored config")

// Crypter seals the secrets that have to be kept recoverable (see internal/secret).
// Only the iperf3 server passwords go through it: iperf3 needs them in the clear at
// test time, so unlike the dashboard login they can't be hashed.
type Crypter interface {
	Seal(plain string) (string, error)
	Unseal(sealed string) (string, error)
}

// Option configures a Controller at construction.
type Option func(*Controller)

// WithCrypter encrypts the stored iperf3 passwords at rest. Without it they are
// stored as they always were (in the clear), which is what the tests use.
func WithCrypter(cr Crypter) Option { return func(c *Controller) { c.crypter = cr } }

// sealed/unsealed convert the iperf_servers VALUE (a JSON blob) on its way to and
// from the store, so overlay/formKeys stay pure functions over plaintext Values and
// the crypto lives in exactly one place: the store boundary.
//
// unsealServers also reports whether anything was still legacy plaintext, so New can
// migrate it on first run.
func (c *Controller) unsealServers(raw string) (string, bool) {
	c.sealedByAddr = nil // rebuilt from this load
	if raw == "" {
		return raw, false
	}
	if c.crypter == nil {
		// No crypter: the stored passwords are still sealed ciphertext this process
		// can't read. Remember each so a later save re-attaches the ciphertext
		// (restoreSealed) instead of the blank sanitizeIperfServers substitutes, so an
		// unrelated form save can't erase a password that is still recoverable on disk.
		c.rememberSealed(raw)
		return raw, false
	}
	var ts []IperfTarget
	if json.Unmarshal([]byte(raw), &ts) != nil {
		return raw, false
	}
	legacy, failed := false, 0
	for i := range ts {
		if ts[i].Password == "" {
			continue
		}
		if !secret.Sealed(ts[i].Password) {
			legacy = true // stored before encryption existed - re-seal it
			continue
		}
		pw, err := c.crypter.Unseal(ts[i].Password)
		if err != nil {
			// Wrong or lost key file: blank the in-memory value (a failed auth that
			// re-prompts beats a garbage password sent to the server), but remember the
			// on-disk ciphertext so a later save re-attaches it rather than erasing it -
			// the original key file may yet be restored (see restoreSealed).
			c.rememberSealedOne(ts[i].Addr, ts[i].Password)
			ts[i].Password = ""
			failed++
			continue
		}
		ts[i].Password = pw
	}
	if failed > 0 {
		// Straight to stderr, bypassing the log level: this runs during settings load,
		// so the level may still be "off". A regenerated or copied-in key mints fine but
		// can't decrypt the existing secrets; staying silent (the create branch in
		// secret.loadOrCreateKey returns no error) would let the next unrelated save
		// overwrite the recoverable ciphertext before the operator can intervene.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: %d stored iperf3 password(s) could not be decrypted; the secret key does not match the stored secrets - restore the original pingularity.key or re-enter the passwords\n", failed)
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return raw, false
	}
	return string(b), legacy
}

// rememberSealed records the at-rest ciphertext of every still-sealed password in
// raw, keyed by server address, so restoreSealed can put it back on a later write.
// Used when there is no crypter: the passwords can't be read but must not be lost.
func (c *Controller) rememberSealed(raw string) {
	var ts []IperfTarget
	if json.Unmarshal([]byte(raw), &ts) != nil {
		return
	}
	for _, t := range ts {
		if secret.Sealed(t.Password) {
			c.rememberSealedOne(t.Addr, t.Password)
		}
	}
}

// rememberSealedOne records one server's at-rest ciphertext, keyed by its address.
func (c *Controller) rememberSealedOne(addr, ciphertext string) {
	if c.sealedByAddr == nil {
		c.sealedByAddr = make(map[string]string)
	}
	c.sealedByAddr[strings.TrimSpace(addr)] = ciphertext
}

// restoreSealed re-attaches the original at-rest ciphertext to any server whose
// password could not be recovered at load and is still blank on the way to disk.
// Without it, persisting that blank would overwrite the recoverable ciphertext, so
// an unrelated settings save would permanently destroy a password the operator
// could otherwise get back by restoring pingularity.key. A re-entered (non-blank)
// password wins, and a server dropped from the list simply isn't restored. The
// re-attached value is already sealed, so sealServers passes it through unchanged.
func (c *Controller) restoreSealed(raw string) string {
	if len(c.sealedByAddr) == 0 || raw == "" {
		return raw
	}
	var ts []IperfTarget
	if json.Unmarshal([]byte(raw), &ts) != nil {
		return raw
	}
	changed := false
	for i := range ts {
		if ts[i].Password != "" {
			continue
		}
		if ct, ok := c.sealedByAddr[strings.TrimSpace(ts[i].Addr)]; ok {
			ts[i].Password = ct
			changed = true
		}
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return raw
	}
	return string(b)
}

func (c *Controller) sealServers(raw string) (string, error) {
	if c.crypter == nil || raw == "" {
		return raw, nil
	}
	var ts []IperfTarget
	if json.Unmarshal([]byte(raw), &ts) != nil {
		return raw, nil
	}
	for i := range ts {
		if ts[i].Password == "" {
			continue
		}
		pw, err := c.crypter.Seal(ts[i].Password)
		if err != nil {
			// Abort the whole save rather than fall through and persist this password
			// in the clear. Silently keeping the plaintext is worse than failing loudly:
			// encryption is on, so the operator would never learn a credential is sitting
			// unsealed on disk. The caller surfaces the error and leaves the prior on-disk
			// value untouched (mutate writes nothing; New/Reload return ErrLegacyReseal).
			return "", fmt.Errorf("sealing iperf3 password for %q: %w", ts[i].Addr, err)
		}
		ts[i].Password = pw
	}
	b, err := json.Marshal(ts)
	if err != nil {
		return raw, nil
	}
	return string(b), nil
}

// New builds a Controller seeded with defaults, then overlays any persisted
// values. On a read error it still returns a usable (defaults-only) controller
// alongside the error, so callers can log and continue rather than crash.
func New(ctx context.Context, st *store.Store, def Values, opts ...Option) (*Controller, error) {
	c := &Controller{store: st, vals: normalize(def), defaults: normalize(def), changed: make(chan struct{})}
	for _, o := range opts {
		o(c)
	}
	m, err := st.AllSettings(ctx)
	if err != nil {
		c.initErr.Store(true)
		return c, err
	}
	raw, legacy := c.unsealServers(m[keyIperfServers])
	m[keyIperfServers] = raw
	c.seedSessionEpoch(m)
	c.vals = normalize(overlay(def, m))
	// Passwords saved before encryption existed are still in the clear on disk: seal
	// them now, so enabling this doesn't quietly leave the old ones exposed forever.
	if legacy {
		sealed, err := c.sealServers(c.restoreSealed(iperfServersJSON(c.vals.IperfServers)))
		if err != nil {
			// A seal failure here means we could NOT re-encrypt the legacy plaintext:
			// abort the migration write rather than persist it in the clear again. Same
			// distinct signal as a failed write below - settings are in effect, only the
			// at-rest re-encryption is pending.
			return c, fmt.Errorf("%w: %v", ErrLegacyReseal, err)
		}
		if _, err := c.store.SetSettingsDiff(ctx, map[string]string{
			keyIperfServers: sealed,
		}); err != nil {
			// The settings themselves loaded fine and are fully in effect - only
			// the at-rest re-encryption write failed. Say so distinctly, or the
			// caller warns "using defaults" about a config that is not defaulted.
			return c, fmt.Errorf("%w: %v", ErrLegacyReseal, err)
		}
	}
	return c, nil
}

// ErrLegacyReseal marks a settings load that SUCCEEDED but could not rewrite
// legacy plaintext iperf3 passwords in their sealed form. Settings are in
// effect; only the at-rest re-encryption is pending.
var ErrLegacyReseal = errors.New("settings loaded; legacy iperf3 password re-encryption failed")

// overlay applies persisted key/value settings onto v. Shared by New and Reload.
func overlay(v Values, m map[string]string) Values {
	if d, ok := secs(m[keyLatency]); ok {
		v.Latency = d
	}
	if b, ok := pbool(m[keyLatencyEnabled]); ok {
		v.LatencyEnabled = b
	}
	if b, ok := pbool(m[keyDNSProbe]); ok {
		v.DNSProbe = b
	}
	if b, ok := pbool(m[keyNetinfo]); ok {
		v.NetinfoEnabled = b
	}
	if d, ok := secs(m[keySpeed]); ok {
		v.Speed = d
	}
	if d, ok := secs(m[keyRetention]); ok {
		v.Retention = d
	}
	if d, ok := secs(m[keySpeedRet]); ok {
		v.SpeedRetention = d
	}
	if d, ok := secs(m[keyDowntimeRet]); ok {
		v.DowntimeRetention = d
	}
	if d, ok := secs(m[keyTimeout]); ok {
		v.Timeout = d
	}
	if n, ok := atoi(m[keyDownAfter]); ok {
		v.DownAfter = n
	}
	if n, ok := atoi(m[keyUpAfter]); ok {
		v.UpAfter = n
	}
	if val, ok := m[keySpeedServer]; ok {
		v.SpeedServerID = val
	}
	if val, ok := m[keySpeedAutoLoc]; ok {
		v.SpeedAutoLoc = val
	}
	if val, ok := m[keySpeedAutoLabel]; ok {
		v.SpeedAutoLabel = val
	}
	if b, ok := pbool(m[keySpeedEnabled]); ok {
		v.SpeedtestEnabled = b
	}
	if b, ok := pbool(m[keySpeedOnRecon]); ok {
		v.SpeedtestOnReconnect = b
	}
	if val := m[keyIPv6Mode]; val != "" {
		v.IPv6Mode = val
	}
	if val, ok := m[keyExitTarget]; ok {
		v.ExitTarget = val
	}
	if b, ok := pbool(m[keyMonitoring]); ok {
		v.Monitoring = b
	}
	if b, ok := pbool(m[keyUpdateCheck]); ok {
		v.UpdateCheckEnabled = b
	}
	if b, ok := pbool(m[keyQuickSetup]); ok {
		v.QuickSetupDone = b
	}
	if val, ok := m[keyLogLevel]; ok {
		v.LogLevel = val
	}
	if b, ok := pbool(m[keyLogRedactPII]); ok {
		v.LogRedactPII = b
	}
	if b, ok := pbool(m[keySpeedAdaptive]); ok {
		v.SpeedtestAdaptive = b
	}
	if b, ok := pbool(m[keySpeedOnDegraded]); ok {
		v.SpeedtestOnDegraded = b
	}
	if f, ok := pfloat(m[keyDegradedPingMS]); ok {
		v.DegradedPingMS = f
	}
	if b, ok := pbool(m[keySpeedSkipBusy]); ok {
		v.SpeedtestSkipBusy = b
	}
	if f, ok := pfloat(m[keySpeedBusyMbps]); ok {
		v.SpeedBusyMbps = f
	}
	if val := m[keySpeedEngine]; val != "" {
		v.SpeedEngine = val
	}
	if val, ok := m[keyIperfServer]; ok {
		v.IperfServer = val
	}
	if raw, ok := m[keyIperfServers]; ok {
		var ts []IperfTarget
		if json.Unmarshal([]byte(raw), &ts) == nil {
			v.IperfServers = ts
		}
	}
	if n, ok := atoi(m[keyIperfDur]); ok {
		v.IperfDur = n
	}
	if n, ok := atoi(m[keyIperfStreams]); ok {
		v.IperfStreams = n
	}
	if n, ok := atoi(m[keyOoklaConnections]); ok {
		v.OoklaConnections = n
	}
	if b, ok := pbool(m[keyOoklaLoss]); ok {
		v.OoklaLoss = b
	}
	if b, ok := pbool(m[keySpeedBestOf]); ok {
		v.SpeedBestOf = b
	}
	if n, ok := atoi(m[keyIperfOmit]); ok {
		v.IperfOmit = n
	}
	if val := m[keySpeedDirection]; val != "" {
		v.SpeedDirection = val
	}
	// iperf3 direction is per-engine now. Migrate only when the key is truly ABSENT
	// (a pre-split install), seeding from the old shared speed_direction; a present
	// key - even empty - is taken as-is so a round-trip is exact.
	if val, ok := m[keyIperfDirection]; ok {
		v.IperfDirection = val
	} else if val := m[keySpeedDirection]; val != "" {
		v.IperfDirection = val
	}
	if b, ok := pbool(m[keyIperfUDP]); ok {
		v.IperfUDP = b
	}
	if n, ok := atoi(m[keyIperfUDPRate]); ok {
		v.IperfUDPRate = n
	}
	if n, ok := atoi(m[keyIperfWindow]); ok {
		v.IperfWindow = n
	}
	if n, ok := atoi(m[keySpeedRetries]); ok {
		v.SpeedRetries = n
	}
	// iperf3 retries is per-engine now; seed from the old shared speed_retries when a
	// pre-split install has no iperf_retries key.
	if n, ok := atoi(m[keyIperfRetries]); ok {
		v.IperfRetries = n
	} else if n, ok := atoi(m[keySpeedRetries]); ok {
		v.IperfRetries = n
	}
	if val, ok := m[keyIperfCongest]; ok {
		v.IperfCongestion = val
	}
	if b, ok := pbool(m[keyIperfNoDelay]); ok {
		v.IperfNoDelay = b
	}
	if val, ok := m[keyIperfDSCP]; ok {
		v.IperfDSCP = val
	}
	if n, ok := atoi(m[keyIperfMSS]); ok {
		v.IperfMSS = n
	}
	if f, ok := pfloat(m[keyThreshDown]); ok {
		v.ThreshDownMbps = f
	}
	if f, ok := pfloat(m[keyThreshUp]); ok {
		v.ThreshUpMbps = f
	}
	if f, ok := pfloat(m[keyThreshPing]); ok {
		v.ThreshPingMS = f
	}
	if f, ok := pfloat(m[keyThreshJitter]); ok {
		v.ThreshJitterMS = f
	}
	if f, ok := pfloat(m[keyThreshLoss]); ok {
		v.ThreshLossPct = f
	}
	if n, ok := atoi(m[keyThreshConsec]); ok {
		v.ThreshConsec = n
	}
	if f, ok := pfloat(m[keyThreshBloatDown]); ok {
		v.ThreshBloatDownMS = f
	}
	if f, ok := pfloat(m[keyThreshBloatUp]); ok {
		v.ThreshBloatUpMS = f
	}
	if b, ok := pbool(m[keyAlertOnOutage]); ok {
		v.AlertOnOutage = b
	}
	if val, ok := m[keyWebhookURL]; ok {
		v.WebhookURL = val
	}
	if val, ok := m[keyWebhookFormat]; ok {
		v.WebhookFormat = val
	}
	if val, ok := m[keyHeartbeatURL]; ok {
		v.HeartbeatURL = val
	}
	if val, ok := m[keyDigestFreq]; ok {
		v.DigestFreq = val
	}
	v.SchedLatEnabled, v.SchedLatWindows = loadSchedule(m,
		keySchedLatEnabled, keySchedLatWindows, keySchedLatDays, keySchedLatStart, keySchedLatEnd)
	v.SchedSpeedEnabled, v.SchedSpeedWindows = loadSchedule(m,
		keySchedSpeedEnabled, keySchedSpeedWindows, keySchedSpeedDays, keySchedSpeedStart, keySchedSpeedEnd)
	if b, ok := pbool(m[keyAccessLocalOnly]); ok {
		v.AccessLocalOnly = b
	}
	if b, ok := pbool(m[keyAuthEnabled]); ok {
		v.AuthEnabled = b
	}
	if val, ok := m[keyAuthUser]; ok {
		v.AuthUser = val
	}
	if val, ok := m[keyAuthHash]; ok {
		v.AuthHash = val
	}
	return v
}

// Reload re-reads all settings from the store (e.g. after a data import
// overwrote the settings table) and applies + broadcasts them. It holds the
// writer lock so a concurrent setter can't slip between the DB read and the
// broadcast and have its change stomped by stale data.
func (c *Controller) Reload(ctx context.Context) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	m, err := c.store.AllSettings(ctx)
	if err != nil {
		return err
	}
	raw, legacy := c.unsealServers(m[keyIperfServers])
	m[keyIperfServers] = raw
	c.seedSessionEpoch(m)
	v := normalize(overlay(c.defaults, m))
	// Apply the loaded config to the live process FIRST, before the legacy
	// re-seal write. Otherwise a transient write failure would discard a
	// perfectly valid imported config (the monitor/scheduler would keep running
	// the old settings). This mirrors New, which sets vals then returns the
	// distinguishable ErrLegacyReseal on a reseal failure.
	c.broadcast(v)
	c.initErr.Store(false)
	// A config import can restore passwords in the clear (an older export still
	// carries them, and mergeImportedIperfPasswords keeps them). Re-seal them
	// here, same as New does on first load, so an import doesn't leave passwords
	// unsealed on disk until the next restart or settings save.
	if legacy {
		sealed, err := c.sealServers(c.restoreSealed(iperfServersJSON(v.IperfServers)))
		if err != nil {
			// Sealing failed: don't rewrite the imported passwords in the clear. The
			// config is already live in memory (broadcast above); report the same
			// distinguishable ErrLegacyReseal a failed write would.
			return fmt.Errorf("%w: %v", ErrLegacyReseal, err)
		}
		if _, err := c.store.SetSettingsDiff(ctx, map[string]string{
			keyIperfServers: sealed,
		}); err != nil {
			return fmt.Errorf("%w: %v", ErrLegacyReseal, err)
		}
	}
	return nil
}

// Getters (each safe for concurrent use).
func (c *Controller) LatencyInterval() time.Duration   { return c.get().Latency }
func (c *Controller) LatencyEnabled() bool             { return c.get().LatencyEnabled }
func (c *Controller) DNSProbe() bool                   { return c.get().DNSProbe }
func (c *Controller) NetinfoEnabled() bool             { return c.get().NetinfoEnabled }
func (c *Controller) SpeedInterval() time.Duration     { return c.get().Speed }
func (c *Controller) Retention() time.Duration         { return c.get().Retention }
func (c *Controller) SpeedRetention() time.Duration    { return c.get().SpeedRetention }
func (c *Controller) DowntimeRetention() time.Duration { return c.get().DowntimeRetention }
func (c *Controller) Timeout() time.Duration           { return c.get().Timeout }
func (c *Controller) DownAfter() int                   { return c.get().DownAfter }
func (c *Controller) UpAfter() int                     { return c.get().UpAfter }
func (c *Controller) SpeedServerID() string            { return c.get().SpeedServerID }
func (c *Controller) SpeedAutoLabel() string           { return c.get().SpeedAutoLabel }

// AutoLocation parses the stored "lat,lon" the auto picker centres on. ok is
// false when unset, meaning use the caller's own IP location.
func (c *Controller) AutoLocation() (lat, lon float64, ok bool) {
	s := c.get().SpeedAutoLoc
	if s == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	lat, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lon, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if e1 != nil || e2 != nil {
		return 0, 0, false
	}
	return lat, lon, true
}
func (c *Controller) SpeedtestEnabled() bool     { return c.get().SpeedtestEnabled }
func (c *Controller) SpeedtestOnReconnect() bool { return c.get().SpeedtestOnReconnect }
func (c *Controller) SpeedtestAdaptive() bool    { return c.get().SpeedtestAdaptive }
func (c *Controller) SpeedtestOnDegraded() bool  { return c.get().SpeedtestOnDegraded }
func (c *Controller) DegradedPingMS() float64    { return c.get().DegradedPingMS }
func (c *Controller) SpeedtestSkipBusy() bool    { return c.get().SpeedtestSkipBusy }
func (c *Controller) SpeedBusyMbps() float64     { return c.get().SpeedBusyMbps }
func (c *Controller) SpeedEngine() string        { return c.get().SpeedEngine }
func (c *Controller) IperfServer() string        { return c.get().IperfServer }
func (c *Controller) IperfDur() int              { return c.get().IperfDur }
func (c *Controller) IperfStreams() int          { return c.get().IperfStreams }
func (c *Controller) OoklaConnections() int      { return c.get().OoklaConnections }
func (c *Controller) OoklaLoss() bool            { return c.get().OoklaLoss }
func (c *Controller) SpeedBestOf() bool          { return c.get().SpeedBestOf }

// Direction and Retries are per-engine: Speed* is Ookla's, Iperf* is iperf3's.
// honor them.
func (c *Controller) SpeedDirection() string  { return c.get().SpeedDirection }
func (c *Controller) SpeedRetries() int       { return c.get().SpeedRetries }
func (c *Controller) IperfDirection() string  { return c.get().IperfDirection }
func (c *Controller) IperfRetries() int       { return c.get().IperfRetries }
func (c *Controller) IperfOmit() int          { return c.get().IperfOmit }
func (c *Controller) IperfUDP() bool          { return c.get().IperfUDP }
func (c *Controller) IperfUDPRate() int       { return c.get().IperfUDPRate }
func (c *Controller) IperfWindow() int        { return c.get().IperfWindow }
func (c *Controller) IperfCongestion() string { return c.get().IperfCongestion }
func (c *Controller) IperfNoDelay() bool      { return c.get().IperfNoDelay }
func (c *Controller) IperfDSCP() string       { return c.get().IperfDSCP }
func (c *Controller) IperfMSS() int           { return c.get().IperfMSS }
func (c *Controller) IperfLabel() string      { return c.get().activeIperf().Label }
func (c *Controller) IperfBind() string       { return c.get().activeIperf().Bind }
func (c *Controller) IperfIPVer() string      { return c.get().activeIperf().IPVer }
func (c *Controller) IperfAuth() bool         { return c.get().activeIperf().Auth }
func (c *Controller) IperfUsername() string   { return c.get().activeIperf().Username }
func (c *Controller) IperfPassword() string   { return c.get().activeIperf().Password }
func (c *Controller) IperfRSAKey() string     { return c.get().activeIperf().RSAKey }
func (c *Controller) IperfPKCS1() bool        { return c.get().activeIperf().PKCS1 }
func (c *Controller) IPv6Mode() string        { return c.get().IPv6Mode }
func (c *Controller) ExitTarget() string      { return c.get().ExitTarget }
func (c *Controller) Monitoring() bool        { return c.get().Monitoring }
func (c *Controller) AlertOnOutage() bool     { return c.get().AlertOnOutage }
func (c *Controller) WebhookURL() string      { return c.get().WebhookURL }
func (c *Controller) WebhookFormat() string   { return c.get().WebhookFormat }
func (c *Controller) HeartbeatURL() string    { return c.get().HeartbeatURL }
func (c *Controller) AccessLocalOnly() bool   { return c.get().AccessLocalOnly }
func (c *Controller) AuthEnabled() bool       { return c.get().AuthEnabled }
func (c *Controller) AuthUser() string        { return c.get().AuthUser }
func (c *Controller) AuthHash() string        { return c.get().AuthHash }

// AuthActive reports whether requests must be authenticated: auth is enabled
// AND a password hash is set. The hash guard means enabling auth without a
// password is inert rather than a lockout.
// Loaded reports whether the controller is running on STORED configuration. It
// is false when the initial read failed, in which case every value it returns is
// a compiled-in default rather than anything the operator chose.
//
// This exists because the difference is invisible in the values themselves and
// dangerous in exactly one of them: with no stored config, AuthActive() answers
// "no login here" whether the operator never set one or the daemon merely could
// not read the one they did set. The web guard consults this so it can refuse
// rather than guess - a stored password must never be dropped because a page of
// the database was unreadable at boot.
func (c *Controller) Loaded() bool { return !c.initErr.Load() }

func (c *Controller) AuthActive() bool {
	v := c.get()
	return v.AuthEnabled && v.AuthHash != ""
}

// HasPassword reports whether a password has been set.
func (c *Controller) HasPassword() bool { return c.get().AuthHash != "" }

// SpeedAllowed reports whether a scheduled/startup/reconnect/degraded speedtest
// may run at t - manual Run-now bypasses it (always true when the schedule is disabled).
func (c *Controller) SpeedAllowed(t time.Time) bool {
	v := c.get()
	if !v.SchedSpeedEnabled {
		return true
	}
	return windowsActive(v.SchedSpeedWindows, t)
}

// LatencyAllowed reports whether latency probing may run at t (always true
// when its schedule is disabled).
func (c *Controller) LatencyAllowed(t time.Time) bool {
	v := c.get()
	if !v.SchedLatEnabled {
		return true
	}
	return windowsActive(v.SchedLatWindows, t)
}

// Snapshot and Defaults hand the whole Values to external callers, so they
// deep-copy the slice fields. get() returns Values by value, but its slices
// still alias c.vals' live backing arrays (shared with the monitor/scheduler
// readers); cloning here keeps a caller that mutates a returned slice from
// racing those readers. The scalar getters skip the copy - they extract a value
// and never retain a slice.
func (c *Controller) Snapshot() Values { return cloneValues(c.get()) }
func (c *Controller) Defaults() Values { return cloneValues(c.defaults) }

// cloneValues deep-copies v's slice fields. Window and IperfTarget hold only
// value types, so a per-element copy is a full clone.
func cloneValues(v Values) Values {
	v.SchedLatWindows = slices.Clone(v.SchedLatWindows)
	v.SchedSpeedWindows = slices.Clone(v.SchedSpeedWindows)
	v.IperfServers = slices.Clone(v.IperfServers)
	return v
}

// Thresholds is the set of speedtest alert thresholds; 0 means a metric is not
// checked. Down/Up are minimums (alert when below); Ping, Jitter, Loss, and the
// two Bloat values are maximums (alert when above). Bloat is the latency added
// under load (loaded - idle), per direction.
type Thresholds struct {
	DownMbps    float64
	UpMbps      float64
	PingMS      float64
	JitterMS    float64
	LossPct     float64
	BloatDownMS float64
	BloatUpMS   float64
}

// Any reports whether at least one threshold is enabled (non-zero), so callers
// can skip evaluation entirely when none are set.
func (t Thresholds) Any() bool {
	return t.DownMbps > 0 || t.UpMbps > 0 || t.PingMS > 0 || t.JitterMS > 0 ||
		t.LossPct > 0 || t.BloatDownMS > 0 || t.BloatUpMS > 0
}

// Thresholds returns the configured speedtest alert thresholds.
func (c *Controller) Thresholds() Thresholds {
	v := c.get()
	return Thresholds{
		DownMbps: v.ThreshDownMbps, UpMbps: v.ThreshUpMbps, PingMS: v.ThreshPingMS,
		JitterMS: v.ThreshJitterMS, LossPct: v.ThreshLossPct,
		BloatDownMS: v.ThreshBloatDownMS, BloatUpMS: v.ThreshBloatUpMS,
	}
}

// BreachStreak returns how many consecutive threshold-breaching runs must occur
// before an alert fires (>=1; 1 alerts on every breach). Debounces single blips.
func (c *Controller) BreachStreak() int { return c.get().ThreshConsec }

// DigestFreq returns the periodic-summary cadence ("off"|"daily"|"weekly").
func (c *Controller) DigestFreq() string { return c.get().DigestFreq }

func (c *Controller) get() Values {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.vals
}

// Changed returns a channel closed when settings next change (re-fetch after
// each wake; the channel is replaced on every change).
func (c *Controller) Changed() <-chan struct{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.changed
}

// mutate performs one atomic settings write: under the writer lock it copies the
// current values, lets fn change that copy and name the keys to persist, writes
// those keys, then applies + broadcasts the result. Holding wmu across the whole
// read-modify-write lets concurrent setters compose instead of clobbering each
// other's fields, and persisting only the returned keys keeps a setter from
// round-tripping (and thus reverting) values it doesn't own. The DB write
// precedes the broadcast, so on a store error the in-memory values are untouched.
func (c *Controller) mutate(ctx context.Context, fn func(v *Values) map[string]string) error {
	return c.mutateErr(ctx, func(v *Values) (map[string]string, error) { return fn(v), nil })
}

// mutateErr is mutate for mutators that can themselves fail (Update's
// stored-baseline read): a non-nil error from fn aborts the write with nothing
// persisted, applied, or broadcast.
func (c *Controller) mutateErr(ctx context.Context, fn func(v *Values) (map[string]string, error)) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// The initial load failed, so the in-memory values are defaults, not the
	// stored config. Persisting now would overwrite the stored config with those
	// defaults, so refuse until a successful Reload recovers the real values.
	if c.initErr.Load() {
		return ErrSettingsUnavailable
	}
	v := c.get()
	kv, err := fn(&v)
	if err != nil {
		return err
	}
	// Seal the iperf3 passwords on the way to disk. Every write goes through mutate,
	// so this is the only place that needs to know. restoreSealed first re-attaches
	// any ciphertext this process couldn't decrypt at load, so an unrelated save can't
	// erase a still-recoverable password (see restoreSealed).
	if raw, ok := kv[keyIperfServers]; ok {
		sealed, err := c.sealServers(c.restoreSealed(raw))
		if err != nil {
			// Sealing failed - abort before the store write and before broadcast, so a
			// credential that couldn't be encrypted is never persisted in the clear and
			// the in-memory values stay exactly as they were (the save simply fails).
			return err
		}
		kv[keyIperfServers] = sealed
	}
	// SetSettingsDiff also reports which keys changed; that fed the removed
	// settings.changed.* counters, so the diff is now ignored.
	if _, err := c.store.SetSettingsDiff(ctx, kv); err != nil {
		return err
	}
	c.broadcast(v)
	return nil
}

// Patch is a partial settings-form update for Update. A nil field means "keep
// the current value", so an API caller that sends only the fields it wants to
// change can't silently reset everything else. Slice fields follow the same
// rule with nil = keep; an explicit empty slice clears the list.
//
// Fields the form doesn't own - the master Monitoring flag (power button), the
// access/auth settings (access tab), the update-check toggle, and the logging
// controls - have no Patch field at all, so a form submit can never touch them.
type Patch struct {
	Latency              *time.Duration
	LatencyEnabled       *bool
	DNSProbe             *bool
	NetinfoEnabled       *bool
	Speed                *time.Duration
	Retention            *time.Duration
	SpeedRetention       *time.Duration
	DowntimeRetention    *time.Duration
	Timeout              *time.Duration
	DownAfter            *int
	UpAfter              *int
	SpeedServerID        *string
	SpeedAutoLoc         *string
	SpeedAutoLabel       *string
	SpeedtestEnabled     *bool
	SpeedtestOnReconnect *bool
	IPv6Mode             *string
	ExitTarget           *string
	SpeedtestAdaptive    *bool
	SpeedtestOnDegraded  *bool
	DegradedPingMS       *float64
	SpeedtestSkipBusy    *bool
	SpeedBusyMbps        *float64
	SpeedEngine          *string
	IperfServer          *string
	IperfServers         []IperfTarget
	IperfDur             *int
	IperfStreams         *int
	OoklaConnections     *int
	OoklaLoss            *bool
	SpeedBestOf          *bool
	IperfOmit            *int
	SpeedDirection       *string
	IperfDirection       *string
	IperfUDP             *bool
	IperfUDPRate         *int
	IperfWindow          *int
	SpeedRetries         *int
	IperfRetries         *int
	IperfCongestion      *string
	IperfNoDelay         *bool
	IperfDSCP            *string
	IperfMSS             *int
	ThreshDownMbps       *float64
	ThreshUpMbps         *float64
	ThreshPingMS         *float64
	ThreshJitterMS       *float64
	ThreshLossPct        *float64
	ThreshConsec         *int
	ThreshBloatDownMS    *float64
	ThreshBloatUpMS      *float64
	AlertOnOutage        *bool
	WebhookURL           *string
	WebhookFormat        *string
	HeartbeatURL         *string
	DigestFreq           *string
	SchedLatEnabled      *bool
	SchedLatWindows      []Window
	SchedSpeedEnabled    *bool
	SchedSpeedWindows    []Window
}

// apply overlays the patch's non-nil fields onto v.
func (p Patch) apply(v *Values) {
	setIf(&v.Latency, p.Latency)
	setIf(&v.LatencyEnabled, p.LatencyEnabled)
	setIf(&v.DNSProbe, p.DNSProbe)
	setIf(&v.NetinfoEnabled, p.NetinfoEnabled)
	setIf(&v.Speed, p.Speed)
	setIf(&v.Retention, p.Retention)
	setIf(&v.SpeedRetention, p.SpeedRetention)
	setIf(&v.DowntimeRetention, p.DowntimeRetention)
	setIf(&v.Timeout, p.Timeout)
	setIf(&v.DownAfter, p.DownAfter)
	setIf(&v.UpAfter, p.UpAfter)
	setIf(&v.SpeedServerID, p.SpeedServerID)
	setIf(&v.SpeedAutoLoc, p.SpeedAutoLoc)
	setIf(&v.SpeedAutoLabel, p.SpeedAutoLabel)
	setIf(&v.SpeedtestEnabled, p.SpeedtestEnabled)
	setIf(&v.SpeedtestOnReconnect, p.SpeedtestOnReconnect)
	setIf(&v.IPv6Mode, p.IPv6Mode)
	setIf(&v.ExitTarget, p.ExitTarget)
	setIf(&v.SpeedtestAdaptive, p.SpeedtestAdaptive)
	setIf(&v.SpeedtestOnDegraded, p.SpeedtestOnDegraded)
	setIf(&v.DegradedPingMS, p.DegradedPingMS)
	setIf(&v.SpeedtestSkipBusy, p.SpeedtestSkipBusy)
	setIf(&v.SpeedBusyMbps, p.SpeedBusyMbps)
	setIf(&v.SpeedEngine, p.SpeedEngine)
	setIf(&v.IperfServer, p.IperfServer)
	if p.IperfServers != nil {
		v.IperfServers = p.IperfServers
	}
	setIf(&v.IperfDur, p.IperfDur)
	setIf(&v.IperfStreams, p.IperfStreams)
	setIf(&v.OoklaConnections, p.OoklaConnections)
	setIf(&v.OoklaLoss, p.OoklaLoss)
	setIf(&v.SpeedBestOf, p.SpeedBestOf)
	setIf(&v.IperfOmit, p.IperfOmit)
	setIf(&v.SpeedDirection, p.SpeedDirection)
	setIf(&v.IperfDirection, p.IperfDirection)
	setIf(&v.IperfUDP, p.IperfUDP)
	setIf(&v.IperfUDPRate, p.IperfUDPRate)
	setIf(&v.IperfWindow, p.IperfWindow)
	setIf(&v.SpeedRetries, p.SpeedRetries)
	setIf(&v.IperfRetries, p.IperfRetries)
	setIf(&v.IperfCongestion, p.IperfCongestion)
	setIf(&v.IperfNoDelay, p.IperfNoDelay)
	setIf(&v.IperfDSCP, p.IperfDSCP)
	setIf(&v.IperfMSS, p.IperfMSS)
	setIf(&v.ThreshDownMbps, p.ThreshDownMbps)
	setIf(&v.ThreshUpMbps, p.ThreshUpMbps)
	setIf(&v.ThreshPingMS, p.ThreshPingMS)
	setIf(&v.ThreshJitterMS, p.ThreshJitterMS)
	setIf(&v.ThreshLossPct, p.ThreshLossPct)
	setIf(&v.ThreshConsec, p.ThreshConsec)
	setIf(&v.ThreshBloatDownMS, p.ThreshBloatDownMS)
	setIf(&v.ThreshBloatUpMS, p.ThreshBloatUpMS)
	setIf(&v.AlertOnOutage, p.AlertOnOutage)
	setIf(&v.WebhookURL, p.WebhookURL)
	setIf(&v.WebhookFormat, p.WebhookFormat)
	setIf(&v.HeartbeatURL, p.HeartbeatURL)
	setIf(&v.DigestFreq, p.DigestFreq)
	setIf(&v.SchedLatEnabled, p.SchedLatEnabled)
	if p.SchedLatWindows != nil {
		v.SchedLatWindows = p.SchedLatWindows
	}
	setIf(&v.SchedSpeedEnabled, p.SchedSpeedEnabled)
	if p.SchedSpeedWindows != nil {
		v.SchedSpeedWindows = p.SchedSpeedWindows
	}
}

// setIf copies *src into *dst when src is set (non-nil).
func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// keys reports which persisted keys this patch explicitly submits (non-nil
// fields), mirroring apply. Update needs the distinction: only a SUBMITTED
// field may newly pin a value the store doesn't hold yet (a flag-backed or
// shipped-default value the operator saved); fields merely carried along keep
// the store a sparse overlay. TestPatchKeysCoversEveryField pins the mapping.
func (p Patch) keys() map[string]bool {
	ks := make(map[string]bool)
	mark := func(set bool, key string) {
		if set {
			ks[key] = true
		}
	}
	mark(p.Latency != nil, keyLatency)
	mark(p.LatencyEnabled != nil, keyLatencyEnabled)
	mark(p.DNSProbe != nil, keyDNSProbe)
	mark(p.NetinfoEnabled != nil, keyNetinfo)
	mark(p.Speed != nil, keySpeed)
	mark(p.Retention != nil, keyRetention)
	mark(p.SpeedRetention != nil, keySpeedRet)
	mark(p.DowntimeRetention != nil, keyDowntimeRet)
	mark(p.Timeout != nil, keyTimeout)
	mark(p.DownAfter != nil, keyDownAfter)
	mark(p.UpAfter != nil, keyUpAfter)
	mark(p.SpeedServerID != nil, keySpeedServer)
	mark(p.SpeedAutoLoc != nil, keySpeedAutoLoc)
	mark(p.SpeedAutoLabel != nil, keySpeedAutoLabel)
	mark(p.SpeedtestEnabled != nil, keySpeedEnabled)
	mark(p.SpeedtestOnReconnect != nil, keySpeedOnRecon)
	mark(p.IPv6Mode != nil, keyIPv6Mode)
	mark(p.ExitTarget != nil, keyExitTarget)
	mark(p.SpeedtestAdaptive != nil, keySpeedAdaptive)
	mark(p.SpeedtestOnDegraded != nil, keySpeedOnDegraded)
	mark(p.DegradedPingMS != nil, keyDegradedPingMS)
	mark(p.SpeedtestSkipBusy != nil, keySpeedSkipBusy)
	mark(p.SpeedBusyMbps != nil, keySpeedBusyMbps)
	mark(p.SpeedEngine != nil, keySpeedEngine)
	mark(p.IperfServer != nil, keyIperfServer)
	mark(p.IperfServers != nil, keyIperfServers)
	mark(p.IperfDur != nil, keyIperfDur)
	mark(p.IperfStreams != nil, keyIperfStreams)
	mark(p.OoklaConnections != nil, keyOoklaConnections)
	mark(p.OoklaLoss != nil, keyOoklaLoss)
	mark(p.SpeedBestOf != nil, keySpeedBestOf)
	mark(p.IperfOmit != nil, keyIperfOmit)
	mark(p.SpeedDirection != nil, keySpeedDirection)
	mark(p.IperfDirection != nil, keyIperfDirection)
	mark(p.IperfUDP != nil, keyIperfUDP)
	mark(p.IperfUDPRate != nil, keyIperfUDPRate)
	mark(p.IperfWindow != nil, keyIperfWindow)
	mark(p.SpeedRetries != nil, keySpeedRetries)
	mark(p.IperfRetries != nil, keyIperfRetries)
	mark(p.IperfCongestion != nil, keyIperfCongest)
	mark(p.IperfNoDelay != nil, keyIperfNoDelay)
	mark(p.IperfDSCP != nil, keyIperfDSCP)
	mark(p.IperfMSS != nil, keyIperfMSS)
	mark(p.ThreshDownMbps != nil, keyThreshDown)
	mark(p.ThreshUpMbps != nil, keyThreshUp)
	mark(p.ThreshPingMS != nil, keyThreshPing)
	mark(p.ThreshJitterMS != nil, keyThreshJitter)
	mark(p.ThreshLossPct != nil, keyThreshLoss)
	mark(p.ThreshConsec != nil, keyThreshConsec)
	mark(p.ThreshBloatDownMS != nil, keyThreshBloatDown)
	mark(p.ThreshBloatUpMS != nil, keyThreshBloatUp)
	mark(p.AlertOnOutage != nil, keyAlertOnOutage)
	mark(p.WebhookURL != nil, keyWebhookURL)
	mark(p.WebhookFormat != nil, keyWebhookFormat)
	mark(p.HeartbeatURL != nil, keyHeartbeatURL)
	mark(p.DigestFreq != nil, keyDigestFreq)
	mark(p.SchedLatEnabled != nil, keySchedLatEnabled)
	mark(p.SchedLatWindows != nil, keySchedLatWindows)
	mark(p.SchedSpeedEnabled != nil, keySchedSpeedEnabled)
	mark(p.SchedSpeedWindows != nil, keySchedSpeedWindows)
	return ks
}

// Update validates, clamps, persists, and applies a dashboard-form patch. Nil
// (omitted) fields keep their current value - PATCH semantics - so a partial
// API body can't silently reset the ~50 form settings it didn't mention.
// Fields the form doesn't own (Monitoring, access/auth, update check, logging)
// have no Patch field, so a stale form submit can't silently revert a
// concurrent password change or power toggle.
func (c *Controller) Update(ctx context.Context, p Patch) (Values, error) {
	// Incoming iperf3 passwords are always plaintext (the API is write-only and
	// never echoes the secret back), so one beginning with the reserved seal prefix
	// is a real password that Seal would mistake for ciphertext and pass through in
	// the clear - then fail to Unseal at test time, silently breaking auth. Reject
	// it up front so the operator learns to pick another password, rather than
	// having it silently blanked deeper in sanitizeIperfServers.
	if p.IperfServers != nil {
		for _, t := range p.IperfServers {
			if secret.Sealed(strings.TrimSpace(t.Password)) {
				return Values{}, fmt.Errorf("iperf3 password may not begin with the reserved %q prefix; choose a different password", secret.Prefix)
			}
		}
	}
	var out Values
	err := c.mutateErr(ctx, func(cur *Values) (map[string]string, error) {
		// The diff baseline below is the STORE, not the in-memory values; read it
		// under the writer lock so no concurrent write can slip between this read
		// and the decision. On a read error the save fails with nothing applied.
		stored, err := c.store.AllSettings(ctx)
		if err != nil {
			return nil, err
		}
		old := formKeys(normalize(*cur)) // the running values (for the unsubmitted-key rule)
		v := *cur
		p.apply(&v)
		// Each server's iperf3 password is write-only: the form never echoes it, so a
		// blank incoming password means "keep the stored one" (matched by address).
		// Normalize FIRST so the merge keys incoming and stored addresses the same way
		// (both trimmed + length-capped); otherwise an odd address could miss its stored
		// password and silently clear it.
		nv := normalize(v)
		nv.IperfServers = mergeIperfPasswords(nv.IperfServers, cur.IperfServers)
		*cur = nv
		out = *cur
		// Persist ONLY what this save establishes - writing the full ~50-key
		// snapshot froze every default a partial POST didn't mention, which
		// shadowed CLI flags AND (since a persisted config key reads as
		// establishment) could make a fresh install look upgraded and skip
		// first-run consent. A SUBMITTED key's baseline is what a restart would
		// otherwise read back: the stored value when the key is persisted, else
		// the SHIPPED default - never the flag-overlaid in-memory value, which
		// made an explicit save of a flag-backed value a no-op (old == new) that
		// silently reverted once the flag was removed. It also persists when it
		// differs from the running value, so setting a flag-backed field to the
		// shipped default sticks too. An UNSUBMITTED key still persists only when
		// its value actually moved (a normalize side effect), keeping the store a
		// sparse overlay: untouched defaults and unsaved flag values stay
		// unpersisted, so future shipped-default and flag changes flow through,
		// and a no-op POST persists nothing.
		newKeys := formKeys(out)
		shipped := formKeys(normalize(shippedDefaults(c.defaults)))
		submitted := p.keys()
		diff := make(map[string]string, len(newKeys))
		for k, val := range newKeys {
			if !submitted[k] {
				if val != old[k] {
					diff[k] = val
				}
				continue
			}
			base, isStored := stored[k]
			if !isStored {
				base = shipped[k]
			} else if k == keyIperfServers {
				// Stored sealed (ciphertext); old[k] is that same list's in-memory
				// plaintext, so compare against it or every save would re-seal (and
				// rewrite) an unchanged list.
				base = old[k]
			}
			if val != base || val != old[k] {
				diff[k] = val
			}
		}
		return diff, nil
	})
	if err != nil {
		return Values{}, err
	}
	return out, nil
}

// formKeys maps the settings-form fields (and only those) to their persisted
// key/value pairs.
func formKeys(v Values) map[string]string {
	return map[string]string{
		keyLatency:           sec(v.Latency),
		keyLatencyEnabled:    b2s(v.LatencyEnabled),
		keyDNSProbe:          b2s(v.DNSProbe),
		keyNetinfo:           b2s(v.NetinfoEnabled),
		keySpeed:             sec(v.Speed),
		keyRetention:         sec(v.Retention),
		keySpeedRet:          sec(v.SpeedRetention),
		keyDowntimeRet:       sec(v.DowntimeRetention),
		keyTimeout:           sec(v.Timeout),
		keyDownAfter:         strconv.Itoa(v.DownAfter),
		keyUpAfter:           strconv.Itoa(v.UpAfter),
		keySpeedServer:       v.SpeedServerID,
		keySpeedAutoLoc:      v.SpeedAutoLoc,
		keySpeedAutoLabel:    v.SpeedAutoLabel,
		keySpeedEnabled:      b2s(v.SpeedtestEnabled),
		keySpeedOnRecon:      b2s(v.SpeedtestOnReconnect),
		keyIPv6Mode:          v.IPv6Mode,
		keyExitTarget:        v.ExitTarget,
		keyThreshDown:        f2s(v.ThreshDownMbps),
		keyThreshUp:          f2s(v.ThreshUpMbps),
		keyThreshPing:        f2s(v.ThreshPingMS),
		keyThreshJitter:      f2s(v.ThreshJitterMS),
		keyThreshLoss:        f2s(v.ThreshLossPct),
		keyThreshConsec:      strconv.Itoa(v.ThreshConsec),
		keyThreshBloatDown:   f2s(v.ThreshBloatDownMS),
		keyThreshBloatUp:     f2s(v.ThreshBloatUpMS),
		keySpeedAdaptive:     b2s(v.SpeedtestAdaptive),
		keySpeedOnDegraded:   b2s(v.SpeedtestOnDegraded),
		keyDegradedPingMS:    f2s(v.DegradedPingMS),
		keySpeedSkipBusy:     b2s(v.SpeedtestSkipBusy),
		keySpeedBusyMbps:     f2s(v.SpeedBusyMbps),
		keySpeedEngine:       v.SpeedEngine,
		keyIperfServer:       v.IperfServer,
		keyIperfServers:      iperfServersJSON(v.IperfServers),
		keyIperfDur:          strconv.Itoa(v.IperfDur),
		keyIperfStreams:      strconv.Itoa(v.IperfStreams),
		keyOoklaConnections:  strconv.Itoa(v.OoklaConnections),
		keyOoklaLoss:         b2s(v.OoklaLoss),
		keySpeedBestOf:       b2s(v.SpeedBestOf),
		keyIperfOmit:         strconv.Itoa(v.IperfOmit),
		keySpeedDirection:    v.SpeedDirection,
		keyIperfDirection:    v.IperfDirection,
		keyIperfUDP:          b2s(v.IperfUDP),
		keyIperfUDPRate:      strconv.Itoa(v.IperfUDPRate),
		keyIperfWindow:       strconv.Itoa(v.IperfWindow),
		keySpeedRetries:      strconv.Itoa(v.SpeedRetries),
		keyIperfRetries:      strconv.Itoa(v.IperfRetries),
		keyIperfCongest:      v.IperfCongestion,
		keyIperfNoDelay:      b2s(v.IperfNoDelay),
		keyIperfDSCP:         v.IperfDSCP,
		keyIperfMSS:          strconv.Itoa(v.IperfMSS),
		keyAlertOnOutage:     b2s(v.AlertOnOutage),
		keyWebhookURL:        v.WebhookURL,
		keyWebhookFormat:     v.WebhookFormat,
		keyHeartbeatURL:      v.HeartbeatURL,
		keyDigestFreq:        v.DigestFreq,
		keySchedLatEnabled:   b2s(v.SchedLatEnabled),
		keySchedLatWindows:   windowsJSON(v.SchedLatWindows),
		keySchedSpeedEnabled: b2s(v.SchedSpeedEnabled),
		keySchedSpeedWindows: windowsJSON(v.SchedSpeedWindows),
		keyQuickSetup:        b2s(v.QuickSetupDone),
	}
}

// shippedDefaults returns the fresh-install (shipped) values of the form
// fields, given the seeded defaults. The controller is seeded with the shipped
// defaults plus any CLI flag overlays already applied (it cannot tell the two
// apart from def alone), so the flag-seedable fields are pinned back to their
// shipped constants here and every other field passes through unchanged - no
// flag can alter those. Update diffs an unpersisted submitted key against THIS
// baseline: a saved flag-backed value must persist (or it silently reverts when
// the flag is removed), while a value matching the shipped default stays
// unpersisted so the store remains a sparse overlay. The constants mirror
// config.Default(); TestShippedDefaultsMatchConfig pins them so they can't
// drift.
func shippedDefaults(def Values) Values {
	def.Latency = 5 * time.Second                // -interval
	def.LatencyEnabled = true                    // -latency
	def.Timeout = 3 * time.Second                // -timeout
	def.DownAfter = 2                            // -down-after
	def.UpAfter = 1                              // -up-after
	def.SpeedtestEnabled = false                 // -speedtest
	def.Speed = time.Hour                        // -speedtest-interval
	def.SpeedtestOnReconnect = true              // -speedtest-on-reconnect
	def.IPv6Mode = "auto"                        // -ipv6
	def.Retention = 30 * 24 * time.Hour          // -retain
	def.SpeedRetention = 365 * 24 * time.Hour    // -retain-speed
	def.DowntimeRetention = 365 * 24 * time.Hour // -retain-downtime
	return def
}

// SetMonitoring flips just the master on/off flag (power button).
func (c *Controller) SetMonitoring(ctx context.Context, on bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.Monitoring = on
		return map[string]string{keyMonitoring: b2s(on)}
	})
}

// SetMonitoringAnsweringSetup sets the power state AND, when powering ON a
// still-unanswered install, marks Quick Setup done - both in ONE transaction and
// one broadcast. An explicit power-on IS an answer to the first-run offer, and
// splitting it into two writes (as an earlier version did, suppressing the
// marker-write error) could report monitoring enabled while the boot hold still
// blocked probes, because the marker never landed. Atomic here: either the UI
// shows on AND the hold is released, or neither moved. Returns whether the marker
// was written, so the caller need not re-read.
func (c *Controller) SetMonitoringAnsweringSetup(ctx context.Context, on bool) (markedDone bool, err error) {
	err = c.mutate(ctx, func(v *Values) map[string]string {
		kv := map[string]string{keyMonitoring: b2s(on)}
		v.Monitoring = on
		if on && !v.QuickSetupDone {
			v.QuickSetupDone = true
			kv[keyQuickSetup] = b2s(true)
			markedDone = true
		}
		return kv
	})
	if err != nil {
		markedDone = false // the transaction rolled back; nothing was written
	}
	return markedDone, err
}

// SetUpdateCheckEnabled flips the daily background poll for a newer release.
func (c *Controller) SetUpdateCheckEnabled(ctx context.Context, on bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.UpdateCheckEnabled = on
		return map[string]string{keyUpdateCheck: b2s(on)}
	})
}

// UpdateCheckEnabled reports whether the daily update check is on.
func (c *Controller) UpdateCheckEnabled() bool { return c.get().UpdateCheckEnabled }

// QuickSetupDone reports whether the first-run Quick Setup dialog was answered.
func (c *Controller) QuickSetupDone() bool { return c.get().QuickSetupDone }

// SetQuickSetupDone records the first-run dialog's answer (Start and dismiss
// both count). Monotonic in practice: nothing ever sets it back.
// QuickSetupAnswer is a complete first-run answer. The web handler validates the
// raw input and pre-hashes the password (bcrypt), then hands the whole thing here
// to be applied ATOMICALLY - one transaction, one broadcast - so a partial apply
// can never leave the answered marker set with the rest half-written, and so only
// these keys are persisted (the settings-form path freezes every default; this
// does not, leaving CLI-seeded and untouched settings alone).
type QuickSetupAnswer struct {
	// Dismiss means "keep every default, just mark answered" (the X / Esc
	// decline). It writes ONLY the marker - no cadence, access, or auth - so it
	// neither freezes a setting nor can change access. Overrides the rest.
	Dismiss          bool
	SpeedtestEnabled bool
	SpeedSeconds     int // clamped to [MinSpeed, MaxSpeed]; applied only when SpeedtestEnabled
	UpdateCheck      bool
	LocalOnly        bool
	AuthEnabled      bool   // login required
	AuthUser         string // trimmed; used only when AuthHash != ""
	AuthHash         string // bcrypt hash, "" = no password set (login stays inert)
}

// ApplyQuickSetup writes the whole first-run answer in ONE mutate (atomic + a
// single broadcast) and marks Quick Setup done in the same transaction. Only the
// keys this answer covers are persisted - cadence, update check, access/auth, and
// the marker - so nothing freezes the ~50 form defaults the settings-form path
// would. Idempotent: a re-apply after a lost response re-writes the same keys.
func (c *Controller) ApplyQuickSetup(ctx context.Context, a QuickSetupAnswer) error {
	if a.Dismiss {
		// Marker only: SetQuickSetupDone writes a single key (no freeze), and
		// touches no access/auth - safe to call even when a login is active.
		return c.SetQuickSetupDone(ctx, true)
	}
	return c.mutate(ctx, func(v *Values) map[string]string {
		kv := map[string]string{
			keySpeedEnabled:    b2s(a.SpeedtestEnabled),
			keyUpdateCheck:     b2s(a.UpdateCheck),
			keyAccessLocalOnly: b2s(a.LocalOnly),
			keyMonitoring:      b2s(true), // "Start monitoring" must actually start it
			keyQuickSetup:      b2s(true), // the marker, in the SAME transaction
		}
		v.SpeedtestEnabled = a.SpeedtestEnabled
		v.UpdateCheckEnabled = a.UpdateCheck
		v.AccessLocalOnly = a.LocalOnly
		v.Monitoring = true // "Start monitoring" turns the power on, in the same tx
		v.QuickSetupDone = true
		if a.SpeedtestEnabled {
			d := time.Duration(a.SpeedSeconds) * time.Second
			if d < MinSpeed {
				d = MinSpeed
			} else if d > MaxSpeed {
				d = MaxSpeed
			}
			v.Speed = d
			kv[keySpeed] = sec(d)
		}
		// Auth: a hash means "set a password and turn login on"; its absence
		// leaves auth per the AuthEnabled flag (inert without a hash - matches
		// SetAuthPassword's own semantics).
		if a.AuthHash != "" {
			user := capLen(strings.TrimSpace(a.AuthUser), maxUser)
			if user == "" {
				user = "admin"
			}
			v.AuthUser, v.AuthHash, v.AuthEnabled = user, a.AuthHash, a.AuthEnabled
			kv[keyAuthUser] = user
			kv[keyAuthHash] = a.AuthHash
			kv[keyAuthEnabled] = b2s(a.AuthEnabled)
		} else {
			v.AuthEnabled = a.AuthEnabled
			kv[keyAuthEnabled] = b2s(a.AuthEnabled)
		}
		return kv
	})
}

func (c *Controller) SetQuickSetupDone(ctx context.Context, done bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.QuickSetupDone = done
		return map[string]string{keyQuickSetup: b2s(done)}
	})
}

// keyQuickSetupOffer is when the first-run offer opened - machinery state, not
// config, seeded exactly once by EnsureQuickSetupOffer. The offer's clock must
// be its own: the install anchor (first_seen_ts) only persists once samples
// exist, and while the offer HOLDS monitoring there are no samples - anchoring
// the hold on it would deadlock a headless install into never monitoring.
const keyQuickSetupOffer = "quick_setup_offer_since"

// quickSetupGrace is how long the first-run offer (and its monitoring hold)
// stays open before it self-expires: long enough that a fresh install whose
// dashboard is first opened after a restart still gets its one offer, short
// enough that an install nobody browses to starts monitoring on its own.
const quickSetupGrace = 48 * 60 * 60

// QuickSetupHold is the one definition of "the first-run offer is open": not
// yet answered, an offer clock exists, and the grace has not run out. The
// status payload's quick_setup_pending and the monitoring hold both use it -
// they must never disagree, or the dialog would promise a state the daemon
// does not have.
func QuickSetupHold(done bool, offerSinceUnix, nowUnix int64) bool {
	if done || offerSinceUnix == 0 {
		return false
	}
	elapsed := nowUnix - offerSinceUnix
	// A NEGATIVE elapsed (offer clock in the future) still HOLDS: releasing it
	// would start monitoring with defaults on a fresh install without the user
	// ever answering Quick Setup - a consent bypass. It just must not extend the
	// 48h fallback for years; EnsureQuickSetupOffer re-anchors a future offer
	// clock at boot (through the fresh-vs-established gate) so elapsed lands back
	// in [0, grace) and an established install is marked answered rather than
	// re-held. The one residual is a MID-session backward step on an
	// already-released fresh install (done=false, monitoring running): the
	// dialog can re-pop until the next restart re-runs the boot gate and marks
	// it answered. Rare, recoverable, and self-healing - accepted over a consent
	// bypass.
	return elapsed < quickSetupGrace
}

// EnsureQuickSetupOffer runs once at boot and materializes the offer decision.
// "Fresh" means the database holds NO measurement history at all - not that
// the install anchor is young. Any history (a day of samples, one speed run,
// an anchor from a past dashboard visit) is an upgrade: the marker is written
// outright, so it can never see a first-run prompt and - critically - its
// already-running monitoring is never held. Anchor age alone would get both
// wrong: a headless or speedtest-only install has history but no anchor, and a
// day-old install upgrading has a young anchor but is running. A history read
// error takes NO decision (no writes): the same boot logic runs again next
// start, which is the self-correcting failure direction.
// EstablishedInStore reports whether the STORE shows an established install:
// measurement history, an install anchor, or persisted operator configuration.
// It reads the store DIRECTLY (not the in-memory Values), so the answer is valid
// even when the settings controller failed to load - which is exactly when the
// boot-time offer decision was skipped. This is the single definition of
// "established": EnsureQuickSetupOffer uses it to mark an upgrade answered, and
// the boot monitoring hold uses it to hold ONLY a genuinely fresh install while
// settings are still unloaded. A read error is returned, not swallowed, so each
// caller can pick its own failure direction.
func (c *Controller) EstablishedInStore(ctx context.Context) (bool, error) {
	history, err := c.store.HasHistory(ctx)
	if err != nil {
		return false, err
	}
	// Any measurement history or an install anchor is an upgrade outright.
	if history || c.store.InstallBornAt(ctx) != 0 {
		return true, nil
	}
	// Also established: a database that already carries operator configuration but
	// no measurements yet (an upgrade from before the install anchor existed, or a
	// config written/restored ahead of the first probe). A genuinely new install
	// has no such keys - defaults live in memory; only explicit changes persist -
	// so this never misreads a real first-run install as established.
	all, err := c.store.AllSettings(ctx)
	if err != nil {
		return false, err
	}
	return hasPriorConfiguration(all), nil
}

func (c *Controller) EnsureQuickSetupOffer(ctx context.Context, nowUnix int64) error {
	if c.QuickSetupDone() {
		return nil
	}
	// History gate FIRST, unconditionally: any measurement history or an install
	// anchor means the install is established, so materialize it as answered and
	// never hold it - whatever the offer clock reads. This closes both clock
	// hazards at once: a FUTURE offer clock (booted skewed ahead, corrected
	// backward) that would otherwise stall the 48h fallback for years, AND a
	// past-but-young offer clock (a small backward step after the 48h release)
	// that would otherwise RE-hold and re-pause a running install. An earlier
	// version checked the offer clock before history and missed the past-young
	// case.
	est, err := c.EstablishedInStore(ctx)
	if err != nil {
		return err // a read error takes NO decision; retried next boot
	}
	if est {
		return c.SetQuickSetupDone(ctx, true)
	}
	// Genuinely fresh (no history). The offer-clock read must NOT mask a store
	// error as 0 (as QuickSetupOfferSince does): "never seeded" and "could not
	// read" take opposite actions here, and acting on the masked 0 would rewrite
	// the clock to now, restarting the 48h consent countdown off one transient
	// failure.
	since, rerr := c.QuickSetupOfferSinceErr(ctx)
	return c.seedOfferClock(ctx, since, rerr, nowUnix)
}

// seedOfferClock is EnsureQuickSetupOffer's fresh-install leg, taking the
// offer-clock read RESULT so its failure contract is enforced (and testable) in
// one place: a read error takes NO decision and makes NO write - the same
// failure direction as the history gate, retried next boot. Otherwise a normal
// past offer clock is left exactly as it was (a fresh install mid-hold, or one
// released after 48h); only two states act: never-offered (since==0) seeds the
// clock, and a future clock (since>now) is pulled back to now so the countdown
// is sane.
func (c *Controller) seedOfferClock(ctx context.Context, since int64, rerr error, nowUnix int64) error {
	if rerr != nil {
		return rerr
	}
	if since != 0 && since <= nowUnix {
		return nil
	}
	return c.store.SetSetting(ctx, keyQuickSetupOffer, strconv.FormatInt(nowUnix, 10))
}

// installStateKeys are settings-table keys that hold INSTALL or session
// bookkeeping, not operator configuration. A genuinely fresh install can already
// carry some of these at first boot (the offer clock EnsureQuickSetupOffer seeds,
// a session epoch, legacy telemetry rows from an older build), so they must NOT
// count as evidence that the install was configured - see hasPriorConfiguration.
var installStateKeys = map[string]bool{
	keyQuickSetup:      true, // quick_setup_done      - the answer marker itself
	keyQuickSetupOffer: true, // quick_setup_offer_since - the first-run offer clock
	keyAuthSessEpoch:   true, // auth_session_epoch    - logout revocation, session state
	"first_seen_ts":    true, // the monitoring anchor - evidence, not config
	// Legacy telemetry identity/state (feature removed; only on older DBs).
	"telemetry_id": true, "telemetry_install_id": true, "telemetry_salt": true,
	"telemetry_id_born_at": true, "telemetry_consent_version": true,
	"telemetry_last_speed_ts": true, "telemetry_last_event_ts": true,
	"telemetry_last_send_ts": true, "telemetry_clean_shutdown": true,
	"digest_last_sent": true, // when the last summary went out - delivery state
}

// hasPriorConfiguration reports whether a raw settings map holds any OPERATOR
// configuration: any key that is not pure install/session bookkeeping
// (installStateKeys). A fresh install persists none - its defaults live in
// memory and only explicit UI/CLI/import changes reach the table - so a single
// non-bookkeeping key means the install was deliberately set up. Used to treat a
// configured-but-historyless database as established rather than fresh.
func hasPriorConfiguration(m map[string]string) bool {
	for k := range m {
		if !installStateKeys[k] {
			return true
		}
	}
	return false
}

// QuickSetupOfferSince reads the offer clock (0 = never seeded). Masks a read
// error as 0 - fine for the status endpoint (a transient error just shows
// not-pending for one poll and self-heals), but NOT for the latching monitoring
// hold: see QuickSetupOfferSinceErr.
func (c *Controller) QuickSetupOfferSince(ctx context.Context) int64 {
	n, _ := c.QuickSetupOfferSinceErr(ctx)
	return n
}

// QuickSetupOfferSinceErr reads the offer clock and SURFACES a store read error
// instead of masking it as 0. The monitoring hold must use this: a transient
// read error read as "no clock" would release the hold - and monitoringLive
// LATCHES that release permanently, starting a fresh install probing unasked off
// one failed read. On error the hold instead holds (fail-safe).
func (c *Controller) QuickSetupOfferSinceErr(ctx context.Context) (int64, error) {
	m, err := c.store.AllSettings(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := strconv.ParseInt(m[keyQuickSetupOffer], 10, 64)
	return n, nil
}

// SetLogLevel sets the logging switch: "off" logs nothing, any other value turns
// full (debug) logging on; an unrecognized value coerces to "info" (still on). The
// UI only sends "debug" or "off".
func (c *Controller) SetLogLevel(ctx context.Context, level string) error {
	switch level {
	case "debug", "info", "warn", "error", "off":
	default:
		level = "info"
	}
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.LogLevel = level
		return map[string]string{keyLogLevel: level}
	})
}

// LogLevel reports the configured log verbosity (debug|info|warn|error|off).
func (c *Controller) LogLevel() string { return c.get().LogLevel }

// SetLogRedactPII toggles PII censoring in log output (owned by the About-tab
// logging control, like the level).
func (c *Controller) SetLogRedactPII(ctx context.Context, on bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.LogRedactPII = on
		return map[string]string{keyLogRedactPII: b2s(on)}
	})
}

// LogRedactPII reports whether PII values are censored in logs (default true).
func (c *Controller) LogRedactPII() bool { return c.get().LogRedactPII }

// SetAccessLocalOnly flips the loopback-only access filter.
func (c *Controller) SetAccessLocalOnly(ctx context.Context, on bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.AccessLocalOnly = on
		return map[string]string{keyAccessLocalOnly: b2s(on)}
	})
}

// SetAuthEnabled toggles whether a login is required. Enabling without a
// password set is allowed but stays inert (see AuthActive) until one is.
func (c *Controller) SetAuthEnabled(ctx context.Context, on bool) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.AuthEnabled = on
		return map[string]string{keyAuthEnabled: b2s(on)}
	})
}

// SetAuthPassword stores the username and bcrypt hash and enables auth in one
// step (setting a password is the act of turning auth on).
func (c *Controller) SetAuthPassword(ctx context.Context, user, hash string) error {
	// Cap the username rune-safely at the persistence boundary (this setter bypasses
	// normalize), so an oversized login name can't be stored mid-codepoint.
	user = capLen(strings.TrimSpace(user), maxUser)
	if user == "" {
		user = "admin"
	}
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.AuthUser, v.AuthHash, v.AuthEnabled = user, hash, true
		return map[string]string{keyAuthUser: user, keyAuthHash: hash, keyAuthEnabled: b2s(true)}
	})
}

// SetAuthUser updates just the login username; the password hash is independent
// (bcrypt covers only the password), so it's unaffected.
func (c *Controller) SetAuthUser(ctx context.Context, user string) error {
	// Cap rune-safely at the persistence boundary (bypasses normalize; see
	// SetAuthPassword).
	user = capLen(strings.TrimSpace(user), maxUser)
	if user == "" {
		user = "admin"
	}
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.AuthUser = user
		return map[string]string{keyAuthUser: user}
	})
}

// seedSessionEpoch folds the persisted session-revocation epoch into the atomic
// (0 when absent or unparseable), but NEVER lowers the live value: it stores
// max(current, persisted). Called under the writer lock at load/reload.
//
// The max matters because BumpSessionEpoch advances the in-memory epoch first and
// only best-effort persists it, so the on-disk value can legitimately trail the
// live one - a persist that failed, or a Reload that raced a logout and read the
// table before the bump's write landed. Storing the read value verbatim there would
// move the epoch BACKWARD and revalidate every token the logout just revoked. The
// Load-then-Store is a safe read-modify-write because the only other writer of the
// epoch, BumpSessionEpoch, also holds wmu, and both callers of this hold wmu too.
func (c *Controller) seedSessionEpoch(m map[string]string) {
	var n int64
	if v, ok := m[keyAuthSessEpoch]; ok {
		n, _ = strconv.ParseInt(v, 10, 64)
	}
	if cur := c.sessionEpoch.Load(); cur > n {
		n = cur // never revoke less than we already have (monotonic across reloads)
	}
	c.sessionEpoch.Store(n)
}

// SessionEpoch returns the current session-revocation epoch, folded into every
// session-token MAC (see BumpSessionEpoch).
func (c *Controller) SessionEpoch() int64 { return c.sessionEpoch.Load() }

// BumpSessionEpoch advances the session-revocation epoch and persists it, so a
// logout invalidates every outstanding token AND the revocation survives a
// restart (an in-memory-only epoch reset to 0, letting a logged-out token
// revalidate). The in-memory value advances first so the live process revokes
// immediately even if the persist write fails; a failed write is returned so the
// caller can log it (the durability guarantee is then best-effort until the next
// successful write).
//
// The whole advance+persist runs under the writer lock - the same lock mutate and
// Reload hold - so it serializes against them. Without it a bump could interleave
// with a Reload (which reads the epoch off disk and applies it, seedSessionEpoch)
// and revive revoked tokens, and two concurrent logouts could persist out of order,
// leaving a stale epoch on disk that a restart would load. Serializing under wmu
// makes both bump-vs-reload and bump-vs-bump total-order. This never takes c.mu
// (only the atomic and the store), so it can't invert the wmu-then-mu order that
// mutate/Reload use (they take wmu, then mu via broadcast); and callers are
// external handlers that never already hold wmu, so there is no re-entrant deadlock.
func (c *Controller) BumpSessionEpoch(ctx context.Context) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := c.sessionEpoch.Add(1)
	return c.store.SetSetting(ctx, keyAuthSessEpoch, strconv.FormatInt(n, 10))
}

// ClearAuth disables auth and wipes the stored password (the lockout-recovery
// path, used by the reset-auth CLI command).
func (c *Controller) ClearAuth(ctx context.Context) error {
	return c.mutate(ctx, func(v *Values) map[string]string {
		v.AuthEnabled, v.AuthHash = false, ""
		return map[string]string{keyAuthEnabled: b2s(false), keyAuthHash: ""}
	})
}

// broadcast atomically applies new values and wakes every current waiter.
// Closing the channel releases all blocked goroutines at once (no fan-out
// bookkeeping); a fresh channel is then installed for the next change. Callers
// re-read Changed() after each wake, so they always wait on the live channel.
func (c *Controller) broadcast(v Values) {
	c.mu.Lock()
	c.vals = v
	close(c.changed)
	c.changed = make(chan struct{})
	c.mu.Unlock()
}

func normalize(v Values) Values {
	v.Latency = clampD(v.Latency, MinLatency, MaxLatency)
	v.Speed = clampD(v.Speed, MinSpeed, MaxSpeed)
	v.Timeout = clampD(v.Timeout, MinTimeout, MaxTimeout)
	// Retention windows: 0 means "keep forever". Clamp to [0, MaxDuration]. The
	// stored-string load path clamps via secs(), but the API Patch pointer path does
	// not, so without this a caller could set a multi-century window that normalize
	// keeps in memory yet the very next reload silently rewrites - in-memory and
	// stored disagreeing until a restart. clampD maps negatives to 0 (forever).
	v.Retention = clampD(v.Retention, 0, MaxDuration)
	v.SpeedRetention = clampD(v.SpeedRetention, 0, MaxDuration)
	v.DowntimeRetention = clampD(v.DowntimeRetention, 0, MaxDuration)
	v.DownAfter = clampI(v.DownAfter, MinStreak, MaxStreak)
	v.UpAfter = clampI(v.UpAfter, MinStreak, MaxStreak)
	switch v.IPv6Mode {
	case "on", "off", "auto":
	default:
		v.IPv6Mode = "auto"
	}
	// Webhook format override: an unknown value falls back to hostname
	// detection rather than silently mis-shaping deliveries. "" normalizes to
	// "auto" so the settings select always has a matching option to display -
	// a value no option carries renders as a blank dropdown.
	switch v.WebhookFormat {
	case "auto", "ntfy", "generic":
	default:
		v.WebhookFormat = "auto"
	}
	// Auto-picker location: keep only a usable "lat,lon" pair (finite, in range).
	// ParseFloat accepts "NaN"/"Inf", which would centre the Ookla server picker
	// on garbage coordinates; "" means centre on your own IP, same as unset.
	v.SpeedAutoLoc = strings.TrimSpace(v.SpeedAutoLoc)
	if v.SpeedAutoLoc != "" && !validAutoLoc(v.SpeedAutoLoc) {
		v.SpeedAutoLoc = ""
	}
	v.SpeedAutoLabel = capLen(v.SpeedAutoLabel, maxLabelLen)
	v.SpeedServerID = capLen(strings.TrimSpace(v.SpeedServerID), maxServerID)
	// Exit-path target: trim and drop anything that can't be a host/IP (empty,
	// flag-shaped, whitespaced); "" means the runtime default (1.1.1.1). netinfo
	// resolves it and falls back if it can't, so this is just storage hygiene.
	v.ExitTarget = strings.TrimSpace(v.ExitTarget)
	if strings.HasPrefix(v.ExitTarget, "-") || strings.ContainsAny(v.ExitTarget, " \t\r\n") || len(v.ExitTarget) > 255 {
		v.ExitTarget = ""
	}
	// Log level: "off" (nothing logged) or any other value = on (full debug);
	// anything else (unset/corrupt) -> info, which applyLogLevel treats as on.
	switch v.LogLevel {
	case "debug", "info", "warn", "error", "off":
	default:
		v.LogLevel = "info"
	}
	// A negative/NaN/Inf threshold (corrupt DB row or crafted import) would slip
	// past a `< 0` guard - NaN compares false to everything - and silently disable
	// that alert in evalThresholds. Force any non-finite/negative value to 0 ("off").
	v.ThreshDownMbps = sanitizeThresh(v.ThreshDownMbps)
	v.ThreshUpMbps = sanitizeThresh(v.ThreshUpMbps)
	v.ThreshPingMS = sanitizeThresh(v.ThreshPingMS)
	v.ThreshJitterMS = sanitizeThresh(v.ThreshJitterMS)
	// Packet loss is a percentage; cap at 100 (a >100% threshold can never fire).
	v.ThreshLossPct = sanitizeThresh(v.ThreshLossPct)
	if v.ThreshLossPct > 100 {
		v.ThreshLossPct = 100
	}
	v.ThreshBloatDownMS = sanitizeThresh(v.ThreshBloatDownMS)
	v.ThreshBloatUpMS = sanitizeThresh(v.ThreshBloatUpMS)
	// Same NaN/Inf/negative defense as the alert thresholds (collapse to 0 = "off"),
	// then cap each at a generous ceiling (mirrored by the UI inputs' max=) so a
	// crafted import or direct DB edit can't set a value that silently never fires.
	v.DegradedPingMS = clampF(sanitizeThresh(v.DegradedPingMS), 0, 5000) // ms
	v.SpeedBusyMbps = clampF(sanitizeThresh(v.SpeedBusyMbps), 0, 10000)  // Mbps
	// At least 1 (alert every breach); cap so a crafted value can't suppress
	// alerts indefinitely. MaxStreak mirrors the outage debounce bound.
	v.ThreshConsec = clampI(v.ThreshConsec, MinStreak, MaxStreak)
	// Length-cap the free-form string settings (storage hygiene). The normal API
	// is bounded by the 64 KiB body cap, but the config-import path accepts a 64
	// MiB body and writes values verbatim, after which an oversized value is
	// re-persisted on every save, shipped on every settings GET, and (for Bind)
	// handed to iperf3 as an argv element where a multi-MB arg fails execve.
	v.WebhookURL = capLen(strings.TrimSpace(v.WebhookURL), maxURL)
	v.HeartbeatURL = capLen(strings.TrimSpace(v.HeartbeatURL), maxURL)
	switch v.DigestFreq {
	case "daily", "weekly":
	default:
		v.DigestFreq = "off"
	}
	switch v.SpeedEngine {
	case "ookla", "iperf3":
	default:
		v.SpeedEngine = "ookla"
	}
	v.IperfServer = capLen(strings.TrimSpace(v.IperfServer), maxServerAddr)
	v.IperfServers = sanitizeIperfServers(v.IperfServers)
	v.IperfDur = clampI(v.IperfDur, MinIperfDur, MaxIperfDur)
	v.IperfStreams = clampI(v.IperfStreams, MinIperfStreams, MaxIperfStreams)
	v.OoklaConnections = clampI(v.OoklaConnections, 0, MaxOoklaConnections)
	v.IperfOmit = clampI(v.IperfOmit, 0, MaxIperfOmit)
	switch v.SpeedDirection { // Ookla is always sequential - no bidir
	case "both", "down", "up":
	default:
		v.SpeedDirection = "both"
	}
	switch v.IperfDirection { // iperf3 adds --bidir (both directions at once)
	case "both", "down", "up", "bidir":
	default:
		v.IperfDirection = "both"
	}
	v.IperfUDPRate = clampI(v.IperfUDPRate, 0, 10000)
	v.IperfWindow = clampI(v.IperfWindow, 0, MaxIperfWindow)
	v.SpeedRetries = clampI(v.SpeedRetries, 0, MaxSpeedRetries)
	v.IperfRetries = clampI(v.IperfRetries, 0, MaxSpeedRetries)
	v.IperfCongestion = sanitizeIperfToken(v.IperfCongestion)
	v.IperfDSCP = sanitizeIperfToken(v.IperfDSCP)
	v.IperfMSS = clampI(v.IperfMSS, 0, MaxIperfMSS)
	v.SchedLatWindows = sanitizeWindows(v.SchedLatWindows)
	v.SchedSpeedWindows = sanitizeWindows(v.SchedSpeedWindows)
	// An enabled schedule with no valid windows is a footgun: windowsActive returns
	// false for an empty set, so LatencyAllowed/SpeedAllowed would answer "never" and
	// silently switch the whole feature off (latency probing stops; scheduled/
	// reconnect/degraded speedtests never fire) with no window to explain why. Treat
	// "enabled but empty" as "no restriction" - force the flag off so the gate reads
	// as unscheduled (always allowed), which is what an empty allow-list means here.
	if v.SchedLatEnabled && len(v.SchedLatWindows) == 0 {
		v.SchedLatEnabled = false
	}
	if v.SchedSpeedEnabled && len(v.SchedSpeedWindows) == 0 {
		v.SchedSpeedEnabled = false
	}
	// Cap the username like every other free-form string, so an oversized
	// auth_user from a config import can't bloat the DB and every access-status
	// GET (import doesn't deny auth_user; only auth_hash is denied).
	v.AuthUser = capLen(strings.TrimSpace(v.AuthUser), maxUser)
	if v.AuthUser == "" {
		v.AuthUser = "admin"
	}
	return v
}

// validAutoLoc reports whether s is a usable "lat,lon" pair: two parseable
// numbers within [-90,90] / [-180,180] (NaN/Inf fail the range checks).
func validAutoLoc(s string) bool {
	if len(s) > 64 {
		return false
	}
	parts := strings.SplitN(s, ",", 2)
	if len(parts) != 2 {
		return false
	}
	lat, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	lon, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	return e1 == nil && e2 == nil && lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// normDays returns a valid 7-char weekday mask, defaulting to all days when
// the input is missing or malformed.
func normDays(s string) string {
	if len(s) != 7 {
		return AllDays
	}
	for _, r := range s {
		if r != '0' && r != '1' {
			return AllDays
		}
	}
	return s
}

func clampMin(m int) int {
	if m < 0 {
		return 0
	}
	if m > 1439 {
		return 1439
	}
	return m
}

// loadSchedule reads a feature's schedule (enabled flag + window list) from the
// settings map. When the windows key is absent it migrates the legacy
// single-window keys, with the legacy master toggle (schedule_enabled) seeding
// the enable flag - so an existing database keeps its schedule across the upgrade.
func loadSchedule(m map[string]string, enKey, winKey, legacyDays, legacyStart, legacyEnd string) (bool, []Window) {
	enabled := false
	if b, ok := pbool(m[enKey]); ok {
		enabled = b
	} else if b, ok := pbool(m[keyScheduleEnabled]); ok {
		enabled = b // legacy master toggle gated both features
	}
	if raw, ok := m[winKey]; ok {
		var ws []Window
		if json.Unmarshal([]byte(raw), &ws) == nil {
			return enabled, ws
		}
	}
	if d, ok := m[legacyDays]; ok { // migrate the single legacy window
		w := Window{Days: d}
		if n, ok := atoi(m[legacyStart]); ok {
			w.Start = n
		}
		if n, ok := atoi(m[legacyEnd]); ok {
			w.End = n
		}
		return enabled, []Window{w}
	}
	return enabled, nil
}

// windowsJSON serializes a window list for storage ("[]" when empty).
func windowsJSON(ws []Window) string {
	if len(ws) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ws)
	return string(b)
}

// sanitizeIperfToken keeps a short free-form iperf3 value (congestion algorithm,
// DSCP) safe to store and pass as an exec arg: trim and drop anything that isn't a
// bare word - empty, leading '-' (flag injection), embedded whitespace, or absurd
// length. "" means "omit the flag"; the tester validates the live value via iperf3.
func sanitizeIperfToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n") || len(s) > 32 {
		return ""
	}
	return s
}

// iperfServersJSON serializes the saved iperf3 server list ("[]" when empty).
func iperfServersJSON(ts []IperfTarget) string {
	if len(ts) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ts)
	return string(b)
}

// sanitizeIperfServers trims and bounds the saved list: trim and length-cap each
// label and address, drop entries with an empty or flag-shaped (leading '-')
// address, dedupe by address (first wins), and cap the list. The address isn't
// strictly validated here - the tester's parseIperfServer does that at run time -
// this just keeps the store sane. Each entry's path + auth fields are normalized
// too: a flag-shaped/whitespaced bind is dropped, IP version is enum-guarded, and
// credentials are trimmed and length-capped.
func sanitizeIperfServers(ts []IperfTarget) []IperfTarget {
	const maxLabel, maxAddr, maxUser, maxKey = 60, 255, 120, 8192
	out := make([]IperfTarget, 0, len(ts))
	seen := make(map[string]bool)
	for _, t := range ts {
		addr := strings.TrimSpace(t.Addr)
		if addr == "" || strings.HasPrefix(addr, "-") || seen[addr] {
			continue
		}
		addr = capLen(addr, maxAddr)                          // rune-safe: never cut a multi-byte host mid-codepoint
		label := capLen(strings.TrimSpace(t.Label), maxLabel) // rune-safe: labels are free-form text
		bind := strings.TrimSpace(t.Bind)
		if strings.HasPrefix(bind, "-") || strings.ContainsAny(bind, " \t\r\n") {
			bind = "" // can't be a real source address
		}
		bind = capLen(bind, maxAddr) // rune-safe (see capLen)
		ipver := t.IPVer
		if ipver != "4" && ipver != "6" {
			ipver = "auto"
		}
		user := capLen(strings.TrimSpace(t.Username), maxUser) // rune-safe (see capLen)
		key := capLen(strings.TrimSpace(t.RSAKey), maxKey)     // rune-safe; outer whitespace trimmed, PEM newlines kept
		pw := t.Password
		// A sealed value here is either a password the user typed that happens to begin
		// with the seal prefix (Seal would pass it through as "already sealed", storing
		// it in the clear, and the next Unseal would fail), or stored ciphertext this
		// process could not decrypt (no crypter, or a mismatched key). Either way it
		// must not reach iperf3, so blank it. When it is unrecoverable ciphertext,
		// restoreSealed re-attaches the original on the way back to disk, so blanking
		// here never erases a password the operator could still get back.
		if secret.Sealed(pw) {
			pw = ""
		}
		// Rune-safe cap at the same 255-byte limit the UI enforces. The UI's maxlength
		// counts UTF-16 units, so a multi-byte/emoji password can arrive over 255 bytes;
		// a raw pw[:maxAddr] would split a codepoint and store invalid UTF-8 that JSON/
		// SQLite then mangle. capLen backs the cut to a valid rune boundary (see capLen).
		pw = capLen(pw, maxAddr)
		seen[addr] = true
		out = append(out, IperfTarget{
			Label: label, Addr: addr, Bind: bind, IPVer: ipver,
			Auth: t.Auth, Username: user, Password: pw, RSAKey: key, PKCS1: t.PKCS1,
		})
		if len(out) >= maxIperfServers {
			break
		}
	}
	return out
}

// mergeIperfPasswords fills a blank incoming password from the stored server with
// the same address: the API never echoes passwords, so blank means "keep what's
// saved" (a deleted server drops its credentials with it).
func mergeIperfPasswords(incoming, stored []IperfTarget) []IperfTarget {
	saved := make(map[string]string, len(stored))
	for _, t := range stored {
		if t.Password != "" {
			saved[strings.TrimSpace(t.Addr)] = t.Password
		}
	}
	for i := range incoming {
		if incoming[i].Password == "" {
			if pw, ok := saved[strings.TrimSpace(incoming[i].Addr)]; ok {
				incoming[i].Password = pw
			}
		}
	}
	return incoming
}

// sanitizeWindows normalizes each window's day mask and clamps its minutes,
// dropping any past the per-feature cap.
func sanitizeWindows(ws []Window) []Window {
	if len(ws) > maxWindows {
		ws = ws[:maxWindows]
	}
	out := make([]Window, len(ws))
	for i, w := range ws {
		out[i] = Window{Days: normDays(w.Days), Start: clampMin(w.Start), End: clampMin(w.End)}
	}
	return out
}

// windowsActive reports whether t falls inside ANY of the schedule's windows.
// An empty list means no active time (an enabled schedule with no windows never
// runs - the UI seeds a window when a schedule is turned on).
func windowsActive(ws []Window, t time.Time) bool {
	for _, w := range ws {
		if windowActive(w.Days, w.Start, w.End, t) {
			return true
		}
	}
	return false
}

// windowActive reports whether t falls inside a schedule window: a weekday
// mask plus a minutes-from-midnight [start,end) range. start==end means the
// whole selected day; start>end wraps past midnight, attributing the
// post-midnight portion to the window that began the previous day.
func windowActive(days string, start, end int, t time.Time) bool {
	if len(days) != 7 {
		return true // fail open on malformed data
	}
	wd := int(t.Weekday())
	m := t.Hour()*60 + t.Minute()
	switch {
	case start == end:
		return dayOn(days, wd)
	case start < end:
		return dayOn(days, wd) && m >= start && m < end
	default: // wraps midnight
		if m >= start {
			return dayOn(days, wd)
		}
		if m < end {
			return dayOn(days, wd-1)
		}
		return false
	}
}

func dayOn(days string, wd int) bool {
	wd = ((wd % 7) + 7) % 7
	return days[wd] == '1'
}

func clampD(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
func clampI(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

// Length caps for the free-form string settings normalize bounds (mirrored by
// the UI inputs' maxlength). Storage hygiene against a crafted config import.
const (
	maxURL        = 2048 // webhook / heartbeat URL
	maxServerAddr = 255  // active iperf3 server host[:port]
	maxLabelLen   = 120  // human label (speed auto-location)
	maxServerID   = 64   // Ookla numeric server id
	maxUser       = 128  // login username
)

// capLen truncates s to at most n bytes, backing the cut off to a UTF-8 boundary
// so a multi-byte rune is never split (a mid-codepoint slice would corrupt the
// stored value and could even truncate to invalid UTF-8 that JSON/SQLite then
// mangle). The result is therefore <= n bytes and always valid UTF-8 if s was.
func capLen(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// clampF bounds a float to [lo, hi]. Callers pass sanitizeThresh'd values, so the
// input is already finite and non-negative; this just applies the ceiling.
func clampF(f, lo, hi float64) float64 {
	if f < lo {
		return lo
	}
	if f > hi {
		return hi
	}
	return f
}
func sec(d time.Duration) string { return strconv.FormatInt(int64(d.Seconds()), 10) }
func f2s(f float64) string       { return strconv.FormatFloat(f, 'f', -1, 64) }
func pfloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// sanitizeThresh forces an alert threshold to a usable value: negative, NaN, and
// Inf all become 0 ("unchecked"), so a corrupt value can't silently disable the
// alert via NaN comparisons in evalThresholds.
func sanitizeThresh(f float64) float64 {
	if f < 0 || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}
func b2s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
func secs(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	if max := int64(MaxDuration / time.Second); n > max {
		n = max
	}
	return time.Duration(n) * time.Second, true
}
func atoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
func pbool(s string) (bool, bool) {
	switch s {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	}
	return false, false
}
