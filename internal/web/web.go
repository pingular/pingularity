// Package web serves Pingularity's built-in dashboard, a JSON API, and a
// Prometheus /metrics endpoint, all from the single binary.
package web

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
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
	"unicode/utf8"

	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/netinfo"
	"github.com/pingular/pingularity/internal/netstat"
	"github.com/pingular/pingularity/internal/notify"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/update"
	"github.com/pingular/pingularity/internal/util"
)

//go:embed ui/index.html
var uiFS embed.FS

// FamilyStatus is one address family's live state.
type FamilyStatus struct {
	Family    string    `json:"family"`
	Online    bool      `json:"online"`
	LatencyMS float64   `json:"latency_ms"`
	Since     time.Time `json:"-"` // when this family's current up/down state began (metrics only)
}

// LiveStatus is the monitor's current debounced state, overall and per family.
type LiveStatus struct {
	Online    bool
	Since     time.Time
	Paused    bool
	Probing   bool // probe rounds are actually running right now
	Families  []FamilyStatus
	DNSms     float64 // last round's DNS-resolve time in ms
	DNSok     bool    // last round's DNS resolution succeeded
	DNSactive bool    // a DNS reading is live right now (probing running AND the DNS sub-toggle on)
}

// StatusFunc reports the monitor's current debounced state.
type StatusFunc func() LiveStatus

// SpeedTrigger runs a speedtest on demand and reports the run in progress, if
// there is one. "Is one running" needs no method of its own: RunID answers 0
// when nothing is running, and speedRunStatus derives both facts from that
// single read precisely so they cannot disagree. It is satisfied by
// speedtest.Scheduler and may be nil when speedtests are disabled.
type SpeedTrigger interface {
	RunOnce(ctx context.Context, reason string) (store.SpeedSample, error)
	RunID() uint64
	Abort(id uint64) bool
	CurrentServer() string
	NextRun() time.Time
}

// NetInfo supplies the latest connection info (IP/ISP/geo/DNS) and can force a
// full re-fetch (incl. the exit traceroute) on demand.
type NetInfo interface {
	Get() netinfo.Info
	RefreshNow(ctx context.Context) netinfo.Info
}

// Server wires the store and live status into HTTP handlers.
type Server struct {
	store  *store.Store
	status StatusFunc
	speed  SpeedTrigger
	// RaceListingFn answers the picker's Auto button: the field an automatic
	// Ookla run would race right now (see speedtest.Ookla.RaceListing). Nil
	// when no Ookla tester is wired (tests), in which case the endpoint says so.
	RaceListingFn func(ctx context.Context) (speedtest.RaceListing, error)
	// PingServersFn re-measures the picker's kept servers on demand (the
	// saved pane's refresh): each ID resolved, its endpoint probed for the
	// health verdict, and pinged the way the race pings, no transfer (see
	// speedtest.PingServersByID). Nil when no Ookla tester is wired, in which
	// case the endpoint says so.
	PingServersFn func(ctx context.Context, ids []string) map[string]speedtest.ServerPing
	settings      *settings.Controller
	netinfo       NetInfo
	version       string
	started       time.Time // process start (for the runtime pill)
	listenAddr    string    // bound address (for showing reachable URLs)
	log           *slog.Logger
	logins        *failLimiter // per-IP throttle on password failures

	// serveCtx is the run context, captured when Serve starts; background work
	// spawned from a handler (the exit-target re-trace) derives from it so a
	// shutdown cancels it, and serveWG lets Serve drain that work before
	// returning - which (main blocks on the Serve goroutine before closing the
	// store) keeps it from touching a closed store. Nil outside Serve (tests).
	serveCtx context.Context
	serveWG  sync.WaitGroup

	// AllowedHosts lists extra Host values the DNS-rebinding check admits
	// (public reverse-proxy domains; see hostAllowed). Set by main from
	// -allow-host before Serve; nil is fine.
	AllowedHosts []string

	// AutoOriginsFn enumerates the candidate cities auto server-selection races
	// - main's autoOrigins, the same closure the tester gets, so the two cannot
	// drift. The settings server-BROWSING list centres on the last auto run's
	// server and FALLS BACK to the first of these carrying a coordinate (see
	// lastAutoRunServerID). Asking main rather than re-deriving it here IS the
	// repair: this handler used to run its own cascade and the two agreed on
	// nothing. Nil leaves the list uncentred, which is the unanchored candidate
	// and an honest answer.
	AutoOriginsFn func() []speedtest.Origin

	// SessionKey is an independent secret (derived from the key file beside the DB
	// via secret.Box.DeriveSubkey) folded into every session-token MAC, so a
	// DB-only copy - which holds the bcrypt hash but not the key file - can't forge
	// a session (see tokenKey). Set by main before Serve; nil falls back to keying
	// on the hash alone (an ephemeral :memory: server, e.g. in tests).
	SessionKey []byte

	// MetricsToken, when set (-metrics-token), is an optional read-only credential a
	// scraper may present to /metrics (Bearer or Basic password) instead of the admin
	// login, so Prometheus never holds an account that can change settings. Empty =
	// /metrics uses the normal admin auth. Only consulted when Require login is on.
	MetricsToken string

	// InContainer is informational only now (surfaced as "containerized" in the
	// status/settings payloads for UI hints). It no longer gates the access
	// filter: local-only is enforced for everyone, and a container opts into
	// network reach with -access network. Set by main before Serve.
	InContainer bool

	// DBPath is the on-disk database path, used only for its size on /metrics.
	// Set by main before Serve; empty (or ":memory:") skips the gauge.
	DBPath string

	// Update polls for a newer release; its Status() is folded into /api/status
	// and the toggle endpoint flips it. Set by main before Serve; nil-checked
	// (tests/headless run without it).
	Update *update.Checker

	// Logs is the in-memory tail of recent log lines, shown by the About-tab log
	// viewer (/api/logs). Set by main before Serve; nil-checked.
	Logs *logbuf.Ring

	// importMu serializes a restore's settings reconcile against credential changes
	// (POST /api/access). Without it the reconcile has to GUESS whether a username
	// it did not expect came from the backup or from the operator, and it guessed
	// wrong for a password-only rotation - which keeps the username by design, so
	// the password hash moved while the name did not.
	importMu sync.Mutex
	// reconciling is true while imported settings are live but the safety repair has
	// not finished. The guard treats it as local-only, because during that window
	// the box is running on whatever the backup said - and a backup taken from an
	// open machine says "no login, reachable from the network".
	reconciling atomic.Bool

	// OnLogClear, when set, runs after the /api/logs clear branch empties the
	// in-memory ring, so main can also drop the on-disk logs.txt snapshot that
	// would otherwise resurrect the cleared lines after an unclean restart. Nil is
	// a no-op (tests, headless). Set by main before Serve.
	OnLogClear func()

	// importSem admits one /api/import run at a time (maxConcurrentImports) and
	// refuses the rest, so a flood of slow-body uploads can't tie up unbounded
	// goroutines/FDs and, more importantly, so two restores can't fight over
	// SQLite's single writer, where whichever one loses the wait lands
	// half-finished (see maxConcurrentImports). Note this is NOT what
	// importMu above covers: an import takes importMu only after its rows are
	// already committed, for the settings reconcile, and only when the backup
	// carried the config category (the importedConfig branch in handleImport).
	// The two other holders, handleAccess and handleQuickSetup, take it to keep
	// the access and first-run answers from interleaving with that reconcile;
	// neither one restores rows. So importMu was never held across the row writes
	// where the contention happens. Lazily built (importGate) so a struct-literal
	// Server still gates.
	importSemOnce sync.Once
	importSem     chan struct{}

	// exportSem caps concurrent /api/export + /api/speed/runs.csv runs. Each holds a
	// SQLite read cursor for the whole stream; with a small pool, a few slow clients
	// could otherwise pin every connection and starve the probe writer. Lazily built
	// (exportGate) so a struct-literal Server still gates.
	exportSemOnce sync.Once
	exportSem     chan struct{}

	// Cached status aggregates: uptime windows scan the outage events table, the
	// data totals/averages scan the speed table. Recomputed at most once per
	// aggTTL instead of on every status poll.
	aggMu   sync.Mutex
	aggAt   time.Time
	aggOK   bool          // last refresh attempt's outcome; collector health tracks attempts, not cache warmth
	aggBusy bool          // a recompute is in flight; others serve the stale cache
	aggWait chan struct{} // non-nil while a recompute owns the cold fill; closed on completion so cold callers wait rather than launch a duplicate scan
	aggGen  uint64        // bumped by invalidators; an in-flight recompute that started before the bump can't re-stamp aggAt
	// One Observation per window, ratio and coverage together: the cache cannot
	// hold a ratio whose coverage was dropped on the way in (it used to hold two
	// parallel store.Uptime values, and only /metrics ever read the second).
	uptime    store.Uptime
	dataBytes int64
	avgDownB  int64
	avgUpB    int64
	usage     store.DataUsage

	// Per-collector health for the /metrics scrape itself (F4): cumulative read
	// errors and the last-success timestamp per collector, so "the DB is failing but
	// the scrape still returns 200 with stale/absent data" is visible, not silent.
	collMu   sync.Mutex
	collErrs map[string]int64 // cumulative failed reads per collector
	collOK   map[string]int64 // last-success unix ts per collector
}

// collResult records one collector read's outcome and updates the cumulative health
// tallies, returning the same ok for the caller to fold into metrics_data_valid.
func (s *Server) collResult(name string, ok bool) bool {
	s.collMu.Lock()
	if s.collErrs == nil {
		s.collErrs, s.collOK = map[string]int64{}, map[string]int64{}
	}
	if ok {
		s.collOK[name] = time.Now().Unix()
	} else {
		s.collErrs[name]++
	}
	s.collMu.Unlock()
	return ok
}

// aggTTL bounds how stale the cached status aggregates may be.
const aggTTL = 30 * time.Second

// aggregates returns the cached status figures (uptime ratios + speedtest data
// totals/averages), recomputing only when the cache has expired.
func (s *Server) aggregates() (uptime store.Uptime, dataBytes, avgDown, avgUp int64, usage store.DataUsage) {
	s.aggMu.Lock()
	for {
		fresh := !s.aggAt.IsZero() && time.Since(s.aggAt) < aggTTL
		// Serve the cache when fresh, or when stale but another caller is already
		// recomputing - a slightly stale value beats serializing concurrent
		// /api/status and /metrics behind the slow DB scans below. But never serve
		// the never-filled cold cache (aggAt still zero): its zero Uptime{} would
		// publish a spurious 0% uptime to a /metrics scrape racing the first
		// recompute at startup. Instead, if a recompute already owns the cold fill,
		// WAIT for it rather than launch a duplicate scan - a /readyz stampede at
		// cold start otherwise piles goroutines onto the small SQLite pool, each
		// running the same full-table scans and delaying the first fill.
		if (fresh || s.aggBusy) && !s.aggAt.IsZero() {
			defer s.aggMu.Unlock()
			return s.uptime, s.dataBytes, s.avgDownB, s.avgUpB, s.usage
		}
		if s.aggBusy { // cold AND a recompute owns it: wait for it, then re-check
			w := s.aggWait
			s.aggMu.Unlock()
			<-w
			s.aggMu.Lock()
			continue
		}
		break // cold and unowned: this goroutine owns the recompute
	}
	// This goroutine owns the recompute; run the scans without the lock so other
	// callers keep serving the cache. Always release ownership, even on a scan
	// panic - else the guard's recover() leaves aggBusy stuck true and freezes
	// the cache for the rest of the process (and cold waiters would block on the
	// never-closed channel).
	s.aggBusy = true
	s.aggWait = make(chan struct{})
	gen := s.aggGen // if an invalidation lands during the scan, don't re-stamp aggAt with pre-invalidation data
	s.aggMu.Unlock()
	defer func() {
		s.aggMu.Lock()
		s.aggBusy = false
		if s.aggWait != nil {
			close(s.aggWait)
			s.aggWait = nil
		}
		s.aggMu.Unlock()
	}()
	// Detached, bounded context: the cache is shared, so one caller disconnecting
	// mid-scan must not cancel the fill and stamp zeros in for everyone.
	scanCtx, cancel := context.WithTimeout(context.Background(), aggTTL)
	defer cancel()
	now := time.Now()
	nUptime, e1 := s.store.UptimeWindows(scanCtx, now, s.settings.DowntimeRetention())
	nUsage, e3 := s.store.SpeedDataUsage(scanCtx, now)
	nAvgDown, nAvgUp, e2 := s.store.SpeedAvgBytes(scanCtx)
	// The store has no logger, so time its aggregate scans here - a slow refresh is
	// the closest thing to a slow-query signal an operator can watch at debug.
	s.log.Debug("aggregate refresh", "dur_ms", util.Round1(util.DurMS(time.Since(now))))
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	// Publish only when all three reads succeed; on any failure keep the previous
	// (stale but valid) values for the next caller to retry. Gating on all three
	// stops a transient SpeedAvgBytes failure from stamping zero averages in for
	// the whole TTL. Log it - a failing local DB would otherwise look like empty
	// data.
	s.aggOK = e1 == nil && e2 == nil && e3 == nil
	if err := errors.Join(e1, e2, e3); err != nil {
		s.log.Warn("aggregate refresh failed; serving previous values", "err", err)
	} else {
		s.uptime = nUptime
		s.usage = nUsage
		s.dataBytes = nUsage.All
		s.avgDownB, s.avgUpB = nAvgDown, nAvgUp
		// Publish the values either way (no staler than the cache they replace),
		// but only mark the cache fresh if nothing invalidated it mid-scan - the
		// scan may have read the events table before, say, an outage deletion.
		if s.aggGen == gen {
			s.aggAt = now
		}
	}
	return s.uptime, s.dataBytes, s.avgDownB, s.avgUpB, s.usage
}

// targetGrace is how far a target's newest sample may lag the newest round
// before the target counts as no longer probed and is dropped from the status
// pills and /metrics (e.g. after IPv6 is toggled off live).
func (s *Server) targetGrace() time.Duration {
	return 3 * s.settings.LatencyInterval()
}

// New builds a Server. speed may be nil if speedtests are disabled.
func New(st *store.Store, status StatusFunc, speed SpeedTrigger, set *settings.Controller, ni NetInfo, version string, log *slog.Logger) *Server {
	return &Server{store: st, status: status, speed: speed, settings: set, netinfo: ni, version: version, started: time.Now(), log: log, logins: newFailLimiter()}
}

// Handler returns the HTTP mux serving the UI, API, and metrics.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// The dashboard is one embedded HTML file; serveUI adds the cache
	// validator + compression a plain FileServer over an embed.FS can't.
	mux.HandleFunc("/", serveUI)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/series", s.handleSeries)
	mux.HandleFunc("/api/speed", s.handleSpeedHistory)
	mux.HandleFunc("/api/speed/runs", s.handleSpeedRuns)
	mux.HandleFunc("/api/speed/runs/delete", s.handleSpeedRunDelete)
	mux.HandleFunc("/api/speed/runs/servers", s.handleSpeedRunServers)
	mux.HandleFunc("/api/speed/runs.csv", s.handleSpeedRunsCSV)
	mux.HandleFunc("/api/speed/usage", s.handleSpeedUsage)
	mux.HandleFunc("/api/notify/test", s.handleNotifyTest)
	mux.HandleFunc("/api/notify/heartbeat/test", s.handleNotifyHeartbeatTest)
	mux.HandleFunc("/api/speedtest", s.handleSpeedtest)
	mux.HandleFunc("/api/speedtest/abort", s.handleSpeedtestAbort)
	mux.HandleFunc("/api/speedtest/servers", s.handleSpeedtestServers)
	mux.HandleFunc("/api/speedtest/candidates", s.handleSpeedtestCandidates)
	mux.HandleFunc("/api/speedtest/ping", s.handleSpeedtestPing)
	mux.HandleFunc("/api/speed/server-pings", s.handleSpeedServerPings)
	mux.HandleFunc("/api/iperf/check", s.handleIperfCheck)
	mux.HandleFunc("/api/heatmap", s.handleHeatmap)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/outages/delete", s.handleOutageDelete)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/data/delete", s.handleDataDelete)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/import", s.handleImport)
	mux.HandleFunc("/api/monitoring", s.handleMonitoring)
	mux.HandleFunc("/api/update", s.handleUpdate)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/netinfo", s.handleNetinfo)
	mux.HandleFunc("/api/access", s.handleAccess)
	mux.HandleFunc("/api/quick-setup", s.handleQuickSetup)
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	// guard applies the loopback-only filter and authentication to every route;
	// securityHeaders stamps its headers on every response, including guard
	// rejections; logRequests wraps it all (outermost) so even rejected
	// requests are logged at debug. compressResponses sits near the routes: it
	// needs the handler's own Content-Type/Content-Encoding to decide, and guard's
	// rejections are a few bytes each anyway.
	//
	// recoverPanics is INSIDE compressResponses on purpose - a defer that
	// finalizes the response must not run before the recover that sets its status.
	// See recoverPanics.
	return s.middleware(mux)
}

// middleware wraps a route handler in the full production chain. It is a named
// function rather than an expression inside Handler so a test can put its own
// handler through the REAL ordering: the panic/compression interaction this
// ordering exists to fix is invisible to a test that composes the chain by hand
// and drifts from it.
func (s *Server) middleware(routes http.Handler) http.Handler {
	return s.logRequests(securityHeaders(writeDeadline(s.guard(compressResponses(s.recoverPanics(routes))))))
}

// securityHeaders adds defense-in-depth headers to every response: nosniff
// stops MIME-sniffing of the JSON/download endpoints, and the CSP gives the
// single-file UI's ~50 innerHTML sinks a second layer against a future missed
// esc() - nothing may load from an external origin, plugins are blocked, and
// <base> tags and foreign form targets are refused. frame-ancestors 'none'
// refuses framing entirely (the UI never runs embedded), closing the
// clickjacking vector against an auth-off dashboard that no session cookie
// guards. script-src pins the single inline <script> by its sha256 hash instead
// of 'unsafe-inline', so an injected <script> is refused while the app's own
// runs; style-src keeps 'unsafe-inline' because the UI sets ~160 inline style
// attributes a hash cannot cover. Referrer-Policy stops a dashboard URL (which
// can carry an auth token in a link the operator pastes) leaking cross-origin,
// and /api responses are marked no-store so a shared cache never retains config,
// logs, or history.
func securityHeaders(next http.Handler) http.Handler {
	csp := contentSecurityPolicy()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", csp)
		h.Set("Referrer-Policy", "no-referrer")
		// A dashboard reachable from the internet must not end up in a search
		// index. It is one person's monitoring of one connection, not public
		// content: indexing it would publish the operator's ISP, exit city,
		// speed history and outage log to anyone searching, and (because every
		// install renders the same titles and headings) would also scatter
		// duplicate branded pages across the web. Header rather than a meta
		// tag so it covers /api and /metrics too, which no crawler should list
		// either. The public demo is a separate build on its own host and is
		// unaffected.
		h.Set("X-Robots-Tag", "noindex, nofollow")
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// cspValue memoizes the computed Content-Security-Policy (the embedded UI is
// immutable per build, so its inline-script hash never changes at runtime).
var cspValue struct {
	once  sync.Once
	value string
}

func contentSecurityPolicy() string {
	cspValue.once.Do(func() {
		script := "'unsafe-inline'" // safe fallback if the script block can't be located
		if raw, err := uiFS.ReadFile("ui/index.html"); err == nil {
			if h, ok := inlineScriptHash(raw); ok {
				script = "'sha256-" + h + "'"
			}
		}
		cspValue.value = "default-src 'none'; script-src " + script + "; style-src 'unsafe-inline'; " +
			"img-src 'self' data:; font-src data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	})
	return cspValue.value
}

// inlineScriptHash returns the base64 sha256 of the bytes between the first
// <script> and </script> in raw - exactly what a browser hashes for a CSP
// script-src allowance. A literal "</script>" cannot appear earlier in a working
// page (the browser would end the script there too), so the first match is the
// real closing tag.
func inlineScriptHash(raw []byte) (string, bool) {
	const open = "<script>"
	i := bytes.Index(raw, []byte(open))
	if i < 0 {
		return "", false
	}
	rest := raw[i+len(open):]
	j := bytes.Index(rest, []byte("</script>"))
	if j < 0 {
		return "", false
	}
	sum := sha256.Sum256(rest[:j])
	return base64.StdEncoding.EncodeToString(sum[:]), true
}

// baselineWriteWindow bounds the response-WRITE phase of an ordinary request.
// There is no server-wide WriteTimeout because the self-paced endpoints below
// must run until they finish, but without any write bound a client that requests
// the ~490 KB UI or the ~8 MiB /api/logs dump and then stops reading pins a
// goroutine and socket forever. 3 minutes is generous even for /api/logs over a
// slow link (~45 KB/s) while turning "indefinitely" into "bounded".
const baselineWriteWindow = 3 * time.Minute

// selfPacedPath is a long-running or streaming endpoint that manages its own
// progress deadline (or runs under BaseContext until completion) and so must be
// exempt from baselineWriteWindow.
func selfPacedPath(p string) bool {
	switch p {
	case "/api/speedtest", "/api/speedtest/abort", "/api/speedtest/servers", "/api/speedtest/candidates", "/api/speedtest/ping", "/api/netinfo",
		"/api/iperf/check", "/api/export", "/api/import",
		"/api/speed/runs.csv", "/api/update", "/api/notify/test", "/api/notify/heartbeat/test":
		return true
	}
	return false
}

// writeDeadline applies baselineWriteWindow to every ordinary response. The
// deadline is absolute and reset per request, so a keep-alive connection's next
// request gets a fresh window.
func writeDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !selfPacedPath(r.URL.Path) {
			// ResponseController reaches the real conn through statusRecorder's
			// Unwrap; a set failure (unsupported writer) is non-fatal, so ignore it
			// and serve without the bound rather than failing the request.
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(baselineWriteWindow))
		}
		next.ServeHTTP(w, r)
	})
}

// uiAsset is the embedded dashboard page plus its gzip body and content-hash
// ETag, prepared once on first request (the embed is immutable per build).
var uiAsset struct {
	once sync.Once
	raw  []byte
	gz   []byte
	etag string
}

// serveUI serves the single-file dashboard with a cache validator and
// compression: the content-hash ETag turns a reload into a 304 instead of a
// full ~330 KB transfer, the precompressed gzip body cuts a cold load to
// roughly a fifth, and Cache-Control: no-cache means "revalidate every time"
// so an upgraded binary's new page is picked up immediately.
func serveUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r) // the UI is one file; anything else under / stays a 404
		return
	}
	uiAsset.once.Do(func() {
		uiAsset.raw, _ = uiFS.ReadFile("ui/index.html")
		sum := sha256.Sum256(uiAsset.raw)
		uiAsset.etag = `"` + hex.EncodeToString(sum[:16]) + `"`
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		zw.Write(uiAsset.raw)
		zw.Close()
		uiAsset.gz = buf.Bytes()
	})
	h := w.Header()
	h.Set("ETag", uiAsset.etag)
	h.Set("Cache-Control", "no-cache")
	h.Set("Vary", "Accept-Encoding")
	if strings.Contains(r.Header.Get("If-None-Match"), uiAsset.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	body := uiAsset.raw
	if acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		body = uiAsset.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

// logCap bounds how much of a request-derived string (Host, username, path) may
// reach a log line - and thence the in-memory ring and its on-disk snapshot. A
// header value can be tens of KB; without a cap a request flood could pin large
// strings in the ring. 256 bytes is plenty to identify a value while logging.
const logCap = 256

// capForLog truncates s to logCap bytes on a UTF-8 boundary (never mid-rune),
// appending an ellipsis when it had to cut, so an oversized request-derived value
// can't bloat the log ring.
func capForLog(s string) string {
	if len(s) <= logCap {
		return s
	}
	n := logCap
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// acceptsGzip reports whether Accept-Encoding positively permits gzip: it must
// list "gzip" (or a "*" wildcard) with a q-value above zero. A plain substring
// check is wrong twice over - it matches "gzip;q=0", which explicitly REFUSES
// gzip, and it can't honour a "*;q=0" that disables every unlisted coding.
func acceptsGzip(r *http.Request) bool {
	gzipOK, wildcardOK := false, false
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue
		}
		name, q := tok, 1.0
		if i := strings.IndexByte(tok, ';'); i >= 0 {
			name = strings.TrimSpace(tok[:i])
			// Scan the parameters for q=; an unparseable value leaves q at 1 (lenient).
			for _, param := range strings.Split(tok[i+1:], ";") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(param), "q="); ok {
					if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = f
					}
				}
			}
		}
		switch {
		case strings.EqualFold(name, "gzip"):
			gzipOK = q > 0 // an explicit gzip entry is authoritative, even if q=0
			return gzipOK
		case name == "*":
			wildcardOK = q > 0
		}
	}
	return wildcardOK
}

// logRequests logs one line per request (method, path, status, duration) at debug.
// When debug is off it passes through untouched - no wrapper, no timing - so it's
// free in the default Info posture.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)
		s.log.Debug("http", "method", r.Method, "path", capForLog(r.URL.Path), "status", sr.status,
			"host", capForLog(r.Host), "ip", clientIP(r), "dur_ms", util.Round1(util.DurMS(time.Since(start))))
	})
}

// statusRecorder captures the response status for request logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.NewResponseController reach the real writer through this
// wrapper (handleImport extends its read deadline that way; without Unwrap the
// extension silently fails whenever debug logging is on).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// compressMinBytes is the response size below which gzip is skipped. Three
// reasons for 1 KiB rather than something smaller:
//   - gzip's header+trailer is 18 bytes before any deflate framing, so a short
//     body can come back BIGGER than it went in. /api/heatmap's empty answer is
//     87 bytes and gzips to 95.
//   - a sub-1 KiB body already fits in one TCP segment (a 1500-byte MTU leaves
//     ~1460 bytes of payload), so compressing it removes no round trip - it just
//     spends CPU on both ends.
//   - it is the same order as the tuned gzip_min_length operators put on nginx
//     (whose own default of 20 is widely treated as a footgun).
//
// The endpoints this exists for are all far above it: /api/speed is 82 KB,
// /api/series 101 KB, /api/speed/runs 35 KB, /api/status 2 KB.
const compressMinBytes = 1024

// gzipPool recycles gzip writers: a level-6 deflate state is a ~260 KB
// allocation, and the pollers hit these endpoints every few seconds per open
// tab. gzip.Writer.Reset makes reuse safe.
var gzipPool = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
	return zw
}}

// streamsBody reports whether a path writes its response incrementally under a
// progress-refreshed write deadline (exportDeadlineBumper). These are EXCLUDED
// from compression rather than wrapped: their whole design is to hold O(1) rows
// in memory while a slow client drains them, and the deadline is rearmed per
// flushed chunk. Putting a compressor in that path would (a) interpose a deflate
// window between the row iterator and the socket, so "bytes moved" - which is
// what the bumper is really measuring - stops meaning what it means today, and
// (b) buy little: both are one-shot manual downloads, not the polled endpoints
// that re-transfer the same payload every few seconds. The rest of the API,
// including /metrics, goes through the compressor.
func streamsBody(p string) bool {
	switch p {
	case "/api/export", "/api/speed/runs.csv":
		return true
	}
	return false
}

// compressible reports whether a status code may carry a body worth encoding.
// 204 and 304 must not have one at all, and a 1xx is informational - stamping
// Content-Encoding on any of them is a protocol error, not an optimization.
func compressible(status int) bool {
	return status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified
}

// compressResponses gzips eligible responses. It sits between the mux and the
// rest of the chain and is deliberately conservative: it holds the first
// compressMinBytes of body back to learn how big the response actually is, and
// only then decides. A response that never reaches the threshold is passed
// through byte-for-byte with no Content-Encoding, so small answers cost one
// buffer copy and nothing else.
func compressResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HEAD has no body to compress; the only effect would be on advisory
		// headers, and serveUI already computes its own for HEAD.
		if r.Method == http.MethodHead || !acceptsGzip(r) || streamsBody(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		// The chosen representation depends on Accept-Encoding whichever way the
		// size decision falls, so a shared cache must key on it. Add (not Set) to
		// avoid clobbering a Vary a handler may already have set.
		addVary(w.Header(), "Accept-Encoding")
		gw := &gzipWriter{ResponseWriter: w, status: http.StatusOK}
		defer gw.close()
		next.ServeHTTP(gw, r)
	})
}

// addVary appends a field name to Vary unless it is already listed.
func addVary(h http.Header, field string) {
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), field) {
				return
			}
		}
	}
	h.Add("Vary", field)
}

// gzipWriter is the deferred-decision compressing ResponseWriter behind
// compressResponses.
//
// It buffers plaintext into sniff until either compressMinBytes is reached (so
// compression is worth it) or the handler finishes (so the exact size is known).
// Compressed output then accumulates in buf so the response can carry a real
// Content-Length instead of falling back to chunked - the body was already fully
// materialized in memory by the handler, and buf holds the COMPRESSED bytes,
// which are several times smaller than the slice the handler built.
//
// A handler that calls Flush is telling us it wants bytes on the wire now, so
// that abandons the Content-Length path and streams from there on (see flush).
type gzipWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool // the handler called WriteHeader

	decided bool // the compress/passthrough choice has been made
	plain   bool // decided: pass through uncompressed
	sniff   []byte
	gz      *gzip.Writer
	buf     bytes.Buffer // compressed bytes held back for Content-Length
	stream  bool         // Flush forced the body out; no Content-Length
	sentHdr bool         // inner WriteHeader has been called
}

// Unwrap keeps http.NewResponseController's search reaching the real conn
// through this wrapper - the same contract statusRecorder documents. Without it
// the write deadlines writeDeadline installs would silently stop being settable
// for every compressed route.
func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader, g.status = true, code
	// A bodyless or informational status is passed straight through: there is
	// nothing to encode and the header must not claim otherwise.
	if !compressible(code) {
		g.decide(true)
	}
	// Otherwise the inner WriteHeader is deferred until the decision, because
	// compressing has to Del Content-Length and Set Content-Encoding first.
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if !g.wroteHeader {
		g.wroteHeader = true
	}
	if !g.decided {
		// Buffer only as far as the DECISION needs, never the whole write. The
		// threshold is 1 KiB, so appending all of p first meant a handler that wrote
		// its body in one call - the JSON endpoints marshal into a single buffer and
		// hand it over whole - held a second full copy of a multi-megabyte response
		// purely to answer "is this at least 1 KiB?". The answer is known after the
		// first KiB; everything past it goes straight to the writer decide picks.
		need := compressMinBytes - len(g.sniff)
		if len(p) < need {
			g.sniff = append(g.sniff, p...)
			return len(p), nil
		}
		g.sniff = append(g.sniff, p[:need]...)
		g.decide(false) // drains sniff into the chosen writer
		if rest := p[need:]; len(rest) > 0 {
			if _, err := g.writeDecided(rest); err != nil {
				return 0, err
			}
		}
		return len(p), nil
	}
	return g.writeDecided(p)
}

// writeDecided sends bytes to whichever writer decide settled on.
func (g *gzipWriter) writeDecided(p []byte) (int, error) {
	if g.plain {
		return g.ResponseWriter.Write(p)
	}
	return g.gz.Write(p)
}

// decide settles compress-vs-passthrough. plain forces passthrough (a bodyless
// status, an already-encoded body, or a body that stayed under the threshold).
func (g *gzipWriter) decide(plain bool) {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	// Never double-encode: serveUI serves the precompressed dashboard and sets
	// Content-Encoding itself, and any future handler that does the same is
	// likewise left alone.
	if plain || h.Get("Content-Encoding") != "" {
		g.plain = true
		g.sendHeader()
		g.flushSniff()
		return
	}
	// Content-Type must be pinned from the PLAINTEXT before any deflate byte
	// reaches the inner writer: net/http sniffs the first 512 bytes it is given,
	// and handed gzip it would label a JSON response application/x-gzip.
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", http.DetectContentType(g.sniff))
	}
	h.Del("Content-Length") // whatever the handler declared describes the plaintext
	h.Set("Content-Encoding", "gzip")
	g.gz = gzipPool.Get().(*gzip.Writer)
	g.gz.Reset(&g.buf)
	if len(g.sniff) > 0 {
		_, _ = g.gz.Write(g.sniff)
	}
	g.sniff = nil
}

// sendHeader calls the inner WriteHeader exactly once.
func (g *gzipWriter) sendHeader() {
	if g.sentHdr {
		return
	}
	g.sentHdr = true
	g.ResponseWriter.WriteHeader(g.status)
}

// flushSniff drains the held-back plaintext to the inner writer.
func (g *gzipWriter) flushSniff() {
	if len(g.sniff) > 0 {
		_, _ = g.ResponseWriter.Write(g.sniff)
		g.sniff = nil
	}
}

// Flush implements http.Flusher, which is also what http.NewResponseController
// finds first when it walks this wrapper. A flush means the handler wants bytes
// delivered now, so the Content-Length optimization is dropped and everything
// buffered so far goes out; from here the response streams.
func (g *gzipWriter) Flush() {
	if !g.decided {
		// Under the threshold at flush time, so the size test has failed on the
		// evidence available: send it uncompressed rather than betting on more.
		g.decide(len(g.sniff) < compressMinBytes)
	}
	if g.plain {
		g.sendHeader()
		g.flushSniff()
	} else {
		g.stream = true
		_ = g.gz.Flush()
		g.sendHeader()
		if g.buf.Len() > 0 {
			_, _ = g.ResponseWriter.Write(g.buf.Bytes())
			g.buf.Reset()
		}
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// close finalizes the response. Called from compressResponses' defer, so it runs
// even when a handler panics after writing.
func (g *gzipWriter) close() {
	if !g.decided {
		// Nothing more is coming, so sniff is the WHOLE body and the threshold
		// test is exact rather than a prefix guess.
		g.decide(len(g.sniff) < compressMinBytes)
	}
	if g.plain {
		// A handler that wrote nothing and never set a status leaves the inner
		// writer untouched, so net/http still emits its own default.
		if g.wroteHeader {
			g.sendHeader()
		}
		g.flushSniff()
		return
	}
	_ = g.gz.Close()
	g.gz.Reset(io.Discard) // drop the reference to buf before pooling
	gzipPool.Put(g.gz)
	g.gz = nil
	if !g.stream {
		g.Header().Set("Content-Length", strconv.Itoa(g.buf.Len()))
	}
	g.sendHeader()
	if g.buf.Len() > 0 {
		_, _ = g.ResponseWriter.Write(g.buf.Bytes())
	}
}

// Serve runs the HTTP server until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, addr string) error {
	s.listenAddr = addr // set before serving; handlers read it for the reachable-URL display
	s.serveCtx = ctx    // handlers derive detached-but-shutdown-aware background work from this
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
		// Tie every request's context to the run context so a graceful shutdown
		// cancels in-flight handlers (e.g. a long manual speedtest) instead of
		// letting them touch the store after it's closed.
		BaseContext: func(net.Listener) context.Context { return ctx },
		// Slowloris / idle-connection hardening: ReadHeaderTimeout bounds slow
		// headers, IdleTimeout reaps idle keep-alives, MaxHeaderBytes caps a
		// header flood, ReadTimeout bounds a slow body (handleImport raises its
		// own deadline for large restores). No server-wide WriteTimeout (it would
		// cut /api/speedtest, streaming export/import, etc.); instead the
		// writeDeadline middleware applies baselineWriteWindow to ordinary
		// responses and exempts the self-paced endpoints, so a non-reading client
		// can't pin a goroutine on the UI or a log dump indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 32 KiB is ample for this API's headers (no big cookies/tokens ride here).
		// The stdlib default (1 MiB) let a single request buffer ~1 MiB of header
		// bytes, and a request-derived value (the Host) then flowed into a warning
		// and the log ring - a cheap way to pin large strings in memory. Lower the
		// cap and truncate the logged value (capForLog) as defence in depth.
		MaxHeaderBytes: 32 * 1024,
	}
	// Cancel the shutdown watcher when Serve returns so it can't leak if
	// ListenAndServe fails immediately (before the parent ctx is cancelled).
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-watchCtx.Done()
		if ctx.Err() == nil {
			return // Serve is returning after a listen failure, not a shutdown
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx) // waits for in-flight handlers, up to the grace
	}()
	s.log.Info("web ui listening", "addr", "http://"+addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	// ListenAndServe returns the instant Shutdown is *called*; wait for it to
	// finish draining so the caller (which then closes the store) doesn't race
	// in-flight handlers.
	<-shutdownDone
	// Also drain background work a handler detached (the exit-target re-trace),
	// which srv.Shutdown does not track, so it can't touch the store after Close.
	s.serveWG.Wait()
	return nil
}

// uptimePayload renders the window set for /api/status: the up-fractions the
// uptime pill shows, and beside them the fraction of each window that was
// actually observed.
//
// Both maps are filled in ONE loop over store.Uptime.Each() on purpose. /api/status
// used to take the ratios and throw the coverage away with a `_`, so on a
// speedtest-only install (`-latency=false`, documented) or with both families off
// the dashboard rendered "100.000%" for a window that observed nothing while
// /metrics, in the same second, correctly published no ratio at all. Filling the
// two maps from the same loop variable makes publishing a ratio without its
// coverage impossible to do by omission - you would have to delete a line that is
// visibly paired with the one above it.
func uptimePayload(u store.Uptime) (ratios, coverage map[string]float64) {
	ratios = make(map[string]float64, 6)
	coverage = make(map[string]float64, 6)
	for _, n := range u.Each() {
		ratios[n.Window] = n.Obs.Ratio()
		coverage[n.Window] = n.Obs.Coverage()
	}
	return ratios, coverage
}

// bridgedContainerFn answers "does this daemon run in a container WITHOUT the
// host's network namespace?" - the state where loopback addresses reach the
// container itself, not the machine. Both the status banner and the settings
// payload's bridged flag read it. A var so tests can stub the answer; production
// always reads util.BridgedContainer.
var bridgedContainerFn = util.BridgedContainer

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.status == nil { // not wired (only happens in misconfiguration/tests) - degrade, don't panic
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
		return
	}
	st := s.status()
	online, since := st.Online, st.Since
	now := time.Now()

	uptime, dataBytes, avgDownB, avgUpB, usage := s.aggregates()
	uptimeRatios, uptimeCoverage := uptimePayload(uptime)
	targets, terr := s.store.LatestPerTarget(ctx, s.targetGrace())
	if terr != nil {
		s.log.Debug("status read failed", "op", "latest_per_target", "err", terr)
	}
	speed, serr := s.store.LatestSpeed(ctx)
	if serr != nil {
		s.log.Debug("status read failed", "op", "latest_speed", "err", serr)
	}

	// Current latency = best among the latest successful target readings.
	var latency *float64
	for _, t := range targets {
		if t.Success && (latency == nil || t.LatencyMS < *latency) {
			v := t.LatencyMS
			latency = &v
		}
	}

	// DNS resolve time from the last probe round (nil unless the last lookup
	// succeeded; failures show as gaps in the chart, so the panel just shows the
	// live figure).
	var dnsMS *float64
	if st.DNSok {
		v := st.DNSms
		dnsMS = &v
	}

	// One snapshot for every speedtest field (see speedRunStatus): separate
	// calls re-split the scheduler's one-word running/id pair, and a run ending
	// between them said "running with no id" - arming an id-0 abort.
	speedRunning, speedRun, speedServer := speedRunStatus(s.speed)

	resp := map[string]any{
		"version":           s.version,
		"online":            online,
		"state_seconds":     int(now.Sub(since).Seconds()),
		"runtime_seconds":   int(now.Sub(s.started).Seconds()),
		"latency_ms":        latency,
		"dns_ms":            dnsMS,
		"uptime":            uptimeRatios,       // up-fraction per window (6h/24h/7d/30d/1y/all) for the selectable pill
		"uptime_coverage":   uptimeCoverage,     // the same windows' observation coverage - the pill must not show a % for a 0 here
		"uptime_24h":        uptime.H24.Ratio(), // kept for back-compat / metrics parity
		"uptime_7d":         uptime.D7.Ratio(),
		"targets":           targets,
		"speed":             speed,          // nil until the first test completes
		"speedtest":         s.speed != nil, // whether manual triggering is available
		"speedtest_running": speedRunning,
		"speedtest_run_id":  speedRun, // 0 when idle; pass back to abort THIS run
		"speedtest_server":  speedServer,
		// True only when a run will ACTUALLY race candidates: no pinned Ookla
		// server AND the Ookla engine is the one that will run. iperf3 connects to
		// the server the operator configured - it has no candidate pool and does
		// no selection - so reporting "auto" there made the UI announce
		// server-finding that never happens. The IperfAvailable() check mirrors
		// the scheduler's own fallback: a selected-but-missing iperf3 binary means
		// Ookla runs after all, and then selection IS happening.
		"speedtest_auto": s.settings.SpeedServerID() == "" &&
			!(s.settings.SpeedEngine() == "iperf3" && speedtest.IperfAvailable()),
		"speedtest_enabled":    s.settings.SpeedtestEnabled(),
		"speed_interval_s":     int(s.settings.SpeedInterval().Seconds()),
		"data_used_bytes":      dataBytes,                  // cumulative speedtest data (down+up)
		"data_usage":           usage,                      // per-window breakdown (6h/24h/7d/30d/1y/all)
		"speed_avg_down_bytes": avgDownB,                   // avg per-run download bytes (recent runs; 0 if none)
		"speed_avg_up_bytes":   avgUpB,                     // avg per-run upload bytes
		"families":             st.Families,                // per-family (IPv4/IPv6) live state
		"paused":               st.Paused,                  // monitoring stopped via the power button
		"first_seen":           s.store.InstallBornAt(ctx), // stable per-install id; scopes the first-run coachmark
		"quick_setup_pending":  s.quickSetupPending(ctx),   // offer the first-run Quick Setup dialog (fresh installs only)
	}
	// The scheduler's own next-due time (anchor + interval + jitter, adaptive
	// cadence included), so the header's "next ..." matches reality instead of
	// guessing from the newest run of any trigger. Omitted before the loop
	// starts or when scheduling is off; the UI hides past values (deferred run).
	if s.speed != nil && s.settings.SpeedtestEnabled() {
		if nr := s.speed.NextRun(); !nr.IsZero() {
			resp["speedtest_next_ts"] = nr.Unix()
		}
	}

	// Optional custom data-usage window for the data bubble (?dataMins=N,
	// mirroring the chart-window picker).
	if d, ok := customDataMins(r.URL.Query().Get("dataMins")); ok {
		if b, err := s.store.SpeedDataUsageSince(ctx, now.Add(d)); err == nil {
			resp["data_used_custom"] = b
		}
	}
	// Optional custom uptime window for the uptime pill (?upMins=N), same parsing
	// and cap as the data window. Its coverage ships beside it for the same reason
	// the preset windows' does: this pill is a fourth uptime figure on the dashboard
	// and was the one the "two of four consumers" framing missed.
	if d, ok := customDataMins(r.URL.Query().Get("upMins")); ok {
		if o, err := s.store.UptimeSince(ctx, now.Add(d), s.settings.DowntimeRetention()); err == nil {
			resp["uptime_custom"] = o.Ratio()
			resp["uptime_custom_coverage"] = o.Coverage()
		}
	}
	// Cached update status (non-blocking read) for the About tab's cue and
	// current/latest display. Absent in tests/headless (no checker).
	if s.Update != nil {
		resp["update"] = s.Update.Status()
	}
	// A bridged container measures the CONTAINER's path, not this host's: an
	// extra hop of latency, a traceroute that stops at the container gateway, and
	// a DNS resolver the runtime substituted - the host's own is typically a
	// loopback stub (systemd-resolved's 127.0.0.53) that means "me", and inside a
	// bridged namespace "me" is the container, so Docker swaps in its public
	// default. resolverEgressIP then correctly reports THAT resolver's operator,
	// which is a different network entirely, not merely a hop further away.
	// The dashboard has to say so, because the numbers look perfectly healthy
	// either way. Only sent when true, so the common case costs nothing.
	if bridgedContainerFn() {
		resp["bridged_container"] = true
	}
	// The current access mode, so Quick Setup can default its access choice to
	// match how the install actually booted. Without this a container started with
	// -access network would show "This machine only" selected, and accepting the
	// default would lock the operator out of the port they just opened.
	resp["access_local_only"] = s.settings.AccessLocalOnly()
	// Connection info off means the panel below is frozen: no automatic lookup
	// will refresh it, so the dashboard has to say so rather than present stale
	// figures as current. Only sent when off, so the common case costs nothing.
	if !s.settings.NetinfoEnabled() {
		resp["netinfo_off"] = true
	}
	writeJSON(w, resp)
}

// maxWinMins caps how far back a chart window may reach, on both the relative
// ?mins= form and the absolute ?from/?to one - an absolute window must never be
// able to ask for a wider scan than the parameter beside it already allows.
const maxWinMins = 366 * 24 * 60

// The read API's sizing policy, in one place so no endpoint invents its own.
// Every handler that returns a list bounds its response BY CONSTRUCTION - not by
// how long the daemon has been running - and takes that bound from one of
// exactly two shapes:
//
//   - A list a human scrolls (outages, speed runs) pages with ?limit/?offset,
//     capped at maxPageLimit. See parsePage.
//   - A series a canvas draws (latency, speedtests) is downsampled to about
//     maxSeriesPoints, the bucket width from seriesBucket. See handleSeries and
//     handleSpeedHistory.
//
// The log viewer is the one reader whose backing store shifts underneath it, so
// it pages by a monotonic cursor rather than an offset - an offset into a ring
// silently skips or repeats lines as it evicts - but it takes its window size
// from the same maxPageLimit ceiling. See handleLogs.
//
// /api/heatmap is the shape that has no list to bound: its output is already
// fixed at ~366 day rows. What it has to bound instead is the WORK, and the rule
// there is the one pauseSpans' comment already states - one query per table per
// request, intersect in Go - never one query per row of the answer.
const (
	maxPageLimit    = 1000
	maxSeriesPoints = 1500
)

// seriesBucket returns the downsampling bucket width, in seconds, for the window
// [since, until) evaluated at now: wide enough that the window holds about
// maxSeriesPoints buckets, and 1 (off) for any window narrow enough not to need
// it. The width is floored, so a window can hold a handful more buckets than
// maxSeriesPoints - unchanged from the arithmetic /api/series has always used,
// and the difference is a rounding error against the bound that matters.
//
// The width comes from the part of the window that can actually HOLD data, not
// from the requested span: measuring to a requested end would bucket a two-hour
// window from last year as coarsely as a whole year, and counting an end that
// has not arrived yet would coarsen every window whose end is in the future -
// which is most of them, since typing a bare year or "jul 1 to dec 31" ends next
// January.
func seriesBucket(since, until, now time.Time) int {
	end := until
	if end.IsZero() || end.After(now) {
		end = now
	}
	bucket := int(end.Sub(since)/time.Second) / maxSeriesPoints
	if bucket < 1 {
		bucket = 1
	}
	return bucket
}

// parseRangeParams reads an absolute window from ?from/?to (unix seconds,
// half-open [from, to); to may be omitted or 0 for an open end). ok=false means
// the caller should fall back to its ?mins= window: bad parameters on the read
// endpoints default gracefully rather than erroring (see breadth_test.go), and
// the UI validates before it ever sends a range, so this is a backstop and not
// the user-facing error path.
func parseRangeParams(r *http.Request, now time.Time) (since, until time.Time, ok bool) {
	fv := r.URL.Query().Get("from")
	if fv == "" {
		return time.Time{}, time.Time{}, false
	}
	f, err := strconv.ParseInt(fv, 10, 64)
	if err != nil || f <= 0 {
		return time.Time{}, time.Time{}, false
	}
	since = time.Unix(f, 0)
	if tv := r.URL.Query().Get("to"); tv != "" {
		t, err := strconv.ParseInt(tv, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		if t != 0 {
			// Reversedness is judged on what was ASKED for, before any clamping.
			// A caller-reversed pair is bad input and falls back to ?mins=, but a
			// window that only becomes empty because the clamps below moved an end
			// is a legitimate request for history outside the band: the honest
			// answer there is no rows, not a silently different window.
			if !time.Unix(t, 0).After(since) {
				return time.Time{}, time.Time{}, false
			}
			until = time.Unix(t, 0)
		}
	}
	// Each end is clamped into the band, not refused: refusing would fall back to
	// the default window and draw the last 7 days under a label saying 2030.
	// Clamped, the window selects nothing and the empty state says so.
	floor, ceil := now.Add(-maxWinMins*time.Minute), now.Add(maxWinMins*time.Minute)
	if since.After(ceil) {
		since = ceil
	}
	if since.Before(floor) {
		since = floor
	}
	if until.After(ceil) {
		until = ceil
	}
	return since, until, true
}

func (s *Server) handleSpeedHistory(w http.ResponseWriter, r *http.Request) {
	mins := 7 * 24 * 60 // default: last 7 days
	if v := r.URL.Query().Get("mins"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxWinMins {
			mins = n
		}
	}
	now := time.Now()
	since, until := now.Add(-time.Duration(mins)*time.Minute), time.Time{}
	if f, u, ok := parseRangeParams(r, now); ok {
		since, until = f, u
	}
	// Downsample on the same rule as /api/series. Without it this endpoint's size
	// was set by retention x cadence rather than by anything the chart can draw: a
	// year of the DEFAULT hourly schedule is 8,759 runs and 6.4 MB, re-fetched
	// every 60s by every visible tab, and the same year of 5-minute LAN tests is
	// 105k runs and 74 MB with the whole slice held in memory per in-flight
	// request. Bucketed, both are the ~1500 points the canvas has pixels for. A
	// window with fewer runs than that is returned unchanged, so 1d/7d/30d are
	// byte-for-byte what they were.
	hist, total, err := s.store.SpeedHistoryBudget(r.Context(), since, until, maxSeriesPoints)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if hist == nil {
		hist = []store.SpeedSample{}
	}
	// Say so when the response is a thinned view rather than the whole window.
	// The body is a bare array and has been since this endpoint existed, so the
	// disclosure goes in headers rather than wrapping it in an object and breaking
	// every existing consumer. Without it a thinned response is indistinguishable
	// from a complete one: a client totalling bytes or counting runs off a 1500-
	// point array covering 40,000 runs is wrong and has no way to know.
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Returned-Count", strconv.Itoa(len(hist)))
	w.Header().Set("X-Sampled", strconv.FormatBool(len(hist) < total))
	writeJSON(w, hist)
}

// handleSpeedRuns returns recent speedtest runs (newest first) with full
// connection context, for the expandable history table.
func (s *Server) handleSpeedRuns(w http.ResponseWriter, r *http.Request) {
	// locate=<ts>: report a run's position (how many runs are newer) so the
	// dashboard can open the table on the row for a clicked chart point.
	if v := r.URL.Query().Get("locate"); v != "" {
		ts, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "bad locate timestamp", http.StatusBadRequest)
			return
		}
		off, err := s.store.SpeedRunOffset(r.Context(), ts)
		if err != nil {
			s.internalError(w, err)
			return
		}
		total, err := s.store.SpeedCount(r.Context())
		if err != nil {
			s.internalError(w, err)
			return
		}
		writeJSON(w, map[string]any{"offset": off, "total": total})
		return
	}
	limit, offset := parsePage(r, 50)
	total, err := s.store.SpeedCount(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	runs, err := s.store.SpeedRuns(r.Context(), limit, offset)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if runs == nil {
		runs = []store.SpeedSample{}
	}
	// Why each run's server was the one measured, from its selection report,
	// so the table can say "incumbent" or "challenger" beside a server name
	// that differs from the rows around it. A lookup failure loses the tags,
	// not the listing.
	tss := make([]int64, 0, len(runs))
	for _, run := range runs {
		tss = append(tss, run.TS)
	}
	if reasons, err := s.store.WinReasonsFor(r.Context(), tss); err == nil {
		for i := range runs {
			runs[i].WinReason = reasons[runs[i].TS]
		}
	} else {
		s.log.Warn("runs listing: could not read win reasons", "err", err)
	}
	writeJSON(w, map[string]any{"runs": runs, "total": total})
}

// handleSpeedRunServers serves one run's server-selection report (table
// speed_servers): who was considered, what each measured, how each scored, and
// why the winner won. GET /api/speed/runs/servers?ts=<unix>. In production the
// DB often sits inside a Docker volume the operator cannot open, so this is
// the report's only reachable surface. 404 means no such RUN; a run that
// exists but has no rows (pre-feature history, an old backup, an iperf3 run)
// answers 200 with an empty list - a missing explanation is normal, a missing
// run is the error. Distinct from /api/speedtest/servers, the server-BROWSE
// endpoint: that one lists pinnable servers, this one explains a past run.
func (s *Server) handleSpeedRunServers(w http.ResponseWriter, r *http.Request) {
	ts, err := strconv.ParseInt(r.URL.Query().Get("ts"), 10, 64)
	if err != nil || ts <= 0 {
		http.Error(w, "bad ts", http.StatusBadRequest)
		return
	}
	rows, err := s.store.SpeedServers(r.Context(), ts)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(rows) == 0 {
		ok, err := s.store.SpeedRunExists(r.Context(), ts)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if !ok {
			http.Error(w, "no such run", http.StatusNotFound)
			return
		}
		rows = []store.SpeedServerRow{}
	}
	writeJSON(w, map[string]any{"ts": ts, "servers": rows})
}

// engineCSV reports the backend that produced a run, mapping a legacy empty
// value (rows recorded before the engine column existed) to "ookla".
func engineCSV(e string) string {
	if e == "" {
		return "ookla"
	}
	return e
}

// csvSafe defuses spreadsheet formula injection: Excel/Sheets treat a cell
// starting with =, +, -, @, tab, or CR as a formula, so prefix such free-text
// cells with a single quote to force literal text.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// handleSpeedRunDelete removes one speedtest run by timestamp.
// POST /api/speed/runs/delete  body {"ts": <unix seconds>}. ts is the run's
// identity across the UI/API (same key as the chart<->table link and ?locate).
// Idempotent: deleting an already-gone run reports deleted:0, not an error.
// On a real deletion the cached aggregates are invalidated, like the outage,
// bulk and import paths: the run's bytes feed the data pills and the /metrics
// byte series, and the UI reloads status the instant the row disappears.
func (s *Server) handleSpeedRunDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		TS int64 `json:"ts"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	if in.TS <= 0 {
		http.Error(w, "missing or invalid ts", http.StatusBadRequest)
		return
	}
	n, err := s.store.DeleteSpeed(r.Context(), in.TS)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n > 0 {
		s.invalidateAggregates()
	}
	s.log.Info("speedtest run deleted", "ts", in.TS, "rows", n)
	writeJSON(w, map[string]any{"deleted": n})
}

// POST /api/outages/delete  body {"ts": <unix seconds of the outage's closing
// 'up' event>} - the row's identity in the outages table. Deletes the up+down
// pair (see store.DeleteOutage); idempotent like the speed-run delete. On a
// real deletion the cached uptime aggregates are invalidated so the uptime
// pill doesn't keep serving the deleted outage for another aggTTL.
func (s *Server) handleOutageDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		TS int64 `json:"ts"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	if in.TS <= 0 {
		http.Error(w, "missing or invalid ts", http.StatusBadRequest)
		return
	}
	n, err := s.store.DeleteOutage(r.Context(), in.TS)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n > 0 {
		s.invalidateAggregates()
	}
	s.log.Info("outage deleted", "ts", in.TS, "rows", n)
	writeJSON(w, map[string]any{"deleted": n})
}

// invalidateAggregates marks the cached status aggregates stale after outage
// or speed data changes, so the uptime/data pills don't keep serving deleted
// (or freshly imported) rows for another aggTTL. The generation bump stops an
// in-flight recompute - which may have read the table pre-change - from
// re-stamping the cache as fresh.
func (s *Server) invalidateAggregates() {
	s.aggMu.Lock()
	s.aggAt = time.Time{}
	s.aggGen++
	s.aggMu.Unlock()
}

// handleSpeedRunsCSV streams every recorded run as CSV (newest first) for export.
func (s *Server) handleSpeedRunsCSV(w http.ResponseWriter, r *http.Request) {
	// Cap concurrent exports so a stalled client can't pin a read cursor (see
	// exportGate); refuse rather than queue when the gate is full.
	gate := s.exportGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		http.Error(w, "another export is already in progress; try again shortly", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pingularity-speed-runs.csv"`)
	cw := csv.NewWriter(w)
	// Progress-refreshed write deadline: a client that stops reading must not hold
	// the DB cursor open for minutes. Rearm on the header and every flushed row.
	bump := exportDeadlineBumper(w, s.log)
	bump()
	// ip_family/udp_direction append at the END so consumers indexing existing
	// columns by position keep working; blank = unrecorded (older rows, engines
	// that don't report them), same absent-not-guessed rule as the JSON.
	cw.Write([]string{"timestamp", "download_mbps", "upload_mbps", "ping_ms", "ping_best_ms", "jitter_ms",
		"packet_loss_pct", "idle_ms", "loaded_down_ms", "loaded_up_ms", "loaded_down_p95_ms", "loaded_up_p95_ms", "healthy", "download_bytes", "upload_bytes",
		"trigger", "engine", "server", "server_id", "isp", "isp_location",
		"public_ipv4", "public_ipv6", "dns_server", "dns_provider", "dns_location",
		"cf_colo", "exit_path", "ip_family", "udp_direction",
		// round_of: the winner's timestamp on a row that was measured in a
		// Best-of round and did not win it (Discard losers off); blank on a
		// test's own result. Appended at the end like the two before it.
		"round_of"})
	// Stream newest-first straight from a descending row iterator (no whole-history
	// slice), flushing per row so back-pressure from a slow client bounds memory and
	// the write deadline keeps advancing only while bytes actually move.
	err := s.store.SpeedHistoryDescFunc(r.Context(), func(sp store.SpeedSample) error {
		cw.Write([]string{
			time.Unix(sp.TS, 0).UTC().Format(time.RFC3339),
			// A direction that wasn't measured (down-only/up-only/partial run) has a
			// nil byte pointer; its scalar Mbps is 0, which as "0.00" reads as a
			// catastrophic measured result rather than "not tested". Byte-presence is
			// the measured signal (as the tiles/Prometheus already use), so blank the
			// cell when it's absent.
			csvMbps(sp.DownMbps, sp.DownBytes),
			csvMbps(sp.UpMbps, sp.UpBytes),
			csvPing(sp.PingMS),
			fptr1(sp.PingBestMS),
			fptr1(sp.JitterMS), fptr(sp.PacketLoss),
			fptr1(sp.IdleMS), fptr1(sp.LoadedDownMS), fptr1(sp.LoadedUpMS), fptr1(sp.LoadedDownP95MS), fptr1(sp.LoadedUpP95MS), healthStr(sp.Healthy),
			iptr(sp.DownBytes), iptr(sp.UpBytes),
			csvSafe(sp.Trigger), csvSafe(engineCSV(sp.Engine)),
			csvSafe(sp.Server), csvSafe(sp.ServerID), csvSafe(sp.ISP), csvSafe(sp.ISPLocation),
			// csvSafe on the IP columns too: organic rows are IP-validated, but a hostile
			// backup can implant a formula in these fields (ImportTable now blanks
			// non-IP values, so this is belt-and-suspenders and harmless for real IPs).
			csvSafe(sp.PublicIPv4), csvSafe(sp.PublicIPv6), csvSafe(sp.DNSIP), csvSafe(sp.DNSProvider), csvSafe(sp.DNSLocation),
			csvSafe(sp.CFColo), csvSafe(sp.ExitSummary),
			// csvSafe despite the closed "4"/"6" and "down"/"up" enums: a crafted
			// backup can implant arbitrary text in these TEXT columns.
			csvSafe(sp.IPFamily), csvSafe(sp.UDPDirection),
			roundOf(sp.RoundTS),
		})
		cw.Flush()
		bump()
		return cw.Error()
	})
	cw.Flush()
	if err != nil {
		// Headers are long since sent, so abort the connection rather than append an
		// error line into a complete-looking CSV (guard() re-panics this sentinel).
		s.log.Error("speed-runs CSV export failed", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// handleSpeedUsage returns cumulative speedtest data volume per time window.
// Optional ?dataMins=N adds that custom window's volume under "custom" (so the
// popover's custom row is right even when it isn't the active window).
func (s *Server) handleSpeedUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	u, err := s.store.SpeedDataUsage(r.Context(), now)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if d, ok := customDataMins(r.URL.Query().Get("dataMins")); ok {
		if b, err := s.store.SpeedDataUsageSince(r.Context(), now.Add(d)); err == nil {
			writeJSON(w, struct {
				store.DataUsage
				Custom int64 `json:"custom"`
			}{u, b})
			return
		}
	}
	writeJSON(w, u)
}

// handleNotifyTest sends a test alert to the webhook URL in the request body so
// users can verify their configuration (POST {url}).
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		URL string `json:"url"`
		// The Alerts tab's currently SELECTED Webhook format (possibly
		// unsaved) - the test must exercise the same payload shape real
		// alerts will use, or a self-hosted ntfy passes Test on a JSON blob
		// it would never receive again.
		Format string `json:"format"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	if strings.TrimSpace(in.URL) == "" {
		http.Error(w, "no webhook URL set", http.StatusBadRequest)
		return
	}
	// Same tolerance as settings sanitize: an unknown value means hostname
	// detection, exactly what FormatFn's absence meant before.
	switch in.Format {
	case "auto", "ntfy", "generic":
	default:
		in.Format = ""
	}
	n := notify.New(func() string { return in.URL }, s.log)
	n.FormatFn = func() string { return in.Format }
	// Surface the delivery result - the point of the Test button is to confirm
	// the webhook works. Send scrubs the URL/token from the error (the host may
	// remain, but it's the user's own webhook), so it's safe to return. 502 so
	// the UI's r.ok check catches it.
	if err := n.Send(r.Context(), "🔔 Pingularity test alert - your webhook is working.",
		map[string]any{"event": "test"}); err != nil {
		http.Error(w, "delivery failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]bool{"sent": true})
}

// handleNotifyHeartbeatTest pings the heartbeat URL in the request body (POST
// {url}) so users can check a dead-man's-switch before saving it. Push watchdogs
// count any request as "I am alive", so this is a real check-in and resets the
// countdown - there is no way to test one without doing so.
func (s *Server) handleNotifyHeartbeatTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		URL string `json:"url"` // the field's current value, saved or not
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	if strings.TrimSpace(in.URL) == "" {
		http.Error(w, "no heartbeat URL set", http.StatusBadRequest)
		return
	}
	// The heartbeat client, not the webhook one: it follows redirects, and
	// hc-ping.com redirects - the webhook client would report a working URL as
	// broken. Heartbeat scrubs the URL out of its error (it is a credential), so
	// the reason is safe to return; 502 so the UI's r.ok check catches it.
	if err := notify.Heartbeat(r.Context(), notify.NewHeartbeatClient(), in.URL, s.log); err != nil {
		http.Error(w, "ping failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]bool{"sent": true})
}

// handleEvents returns a page of up/down transition events (newest first) for
// the paginated outages table.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePage(r, 10)
	total, err := s.store.EventCount(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	ev, err := s.store.EventsPage(r.Context(), limit, offset)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if ev == nil {
		ev = []store.Event{}
	}
	writeJSON(w, map[string]any{"events": ev, "total": total})
}

func (s *Server) handleHeatmap(w http.ResponseWriter, r *http.Request) {
	days := 365
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 366 {
			days = n
		}
	}
	// Day boundaries follow the viewer's timezone when given (?tz=<IANA name>),
	// falling back to the server's local zone as before.
	loc := time.Local
	if tz := r.URL.Query().Get("tz"); tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	since := time.Now().AddDate(0, 0, -days)
	rows, err := s.store.DowntimeByDay(r.Context(), since, loc)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if rows == nil {
		rows = []store.DowntimeDay{}
	}
	writeJSON(w, rows)
}

// handleSpeedtest triggers an on-demand measurement and returns the result.
func (s *Server) handleSpeedtest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
		return
	}
	if s.speed == nil {
		http.Error(w, "speedtests are disabled", http.StatusServiceUnavailable)
		return
	}
	// A manual run can take minutes with "best of 3 servers" on. It must outlive the
	// HTTP request - a reload, a closed tab, or any client-side give-up must not kill
	// it mid-transfer (that surfaced as "speedtest failed: context canceled" and
	// stored nothing) - so it does NOT ride r.Context(). But it must NOT outlive the
	// daemon either: the old context.WithoutCancel fully detached it, so a run in
	// flight at shutdown kept going and could InsertSpeed into a store main had
	// already closed. Tie it to the server run context so a shutdown cancels it, and
	// track it on serveWG so Serve drains it before the store closes.
	base := s.serveCtx
	if base == nil {
		base = context.Background() // not serving (tests): a plain detached ctx
	}
	// Refuse once shutdown has started: serveCtx is cancelled, so a new run would
	// abort immediately and could still race the closing store.
	if base.Err() != nil {
		http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
		return
	}
	s.serveWG.Add(1)
	defer s.serveWG.Done()
	res, err := s.speed.RunOnce(base, "manual")
	if errors.Is(err, speedtest.ErrBusy) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if errors.Is(err, speedtest.ErrAborted) {
		// Aborted before any server produced a result: a clean stop, not a failure.
		// (An abort after a best-of-N server succeeded returns that result below.)
		writeJSON(w, map[string]any{"aborted": true})
		return
	}
	if err != nil {
		http.Error(w, "speedtest failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, res)
}

// handleSpeedtestAbort cancels a run in flight. A best-of-N run that has already
// measured a server keeps that result (the in-flight POST to /api/speedtest
// returns it); an abort before the first result stores nothing and that POST
// returns {"aborted":true}. A stop the scheduler refused - nothing running, or
// the named run already over - answers 409, not the kill's 204, so the two
// outcomes are distinguishable to a caller. The dashboard never reads this
// response (its abort fetch ignores it and re-syncs from the status poll), so
// the honest status costs the UI nothing.
func (s *Server) handleSpeedtestAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
		return
	}
	if s.speed == nil {
		http.Error(w, "speedtests are disabled", http.StatusServiceUnavailable)
		return
	}
	// `run` names the run the caller decided to stop, as reported by
	// speedtest_run_id when they saw it. A stop is acted on some time after it is
	// decided - the dashboard polls every few seconds and then waits on a confirm()
	// dialog - and without the id the daemon could only cancel whoever held the flag
	// on arrival, which is a different run if one started in between. Omitting it
	// still means "whatever is running now", for a caller who never saw a specific
	// run.
	var id uint64
	if v := r.URL.Query().Get("run"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			http.Error(w, "run must be a run id", http.StatusBadRequest)
			return
		}
		id = n
	}
	if !s.speed.Abort(id) {
		// Refused: nothing is running, or the run this stop was decided against
		// has already ended (a stale id from a previous boot looks the same).
		// Answering 204 here made a refusal byte-identical to a kill.
		http.Error(w, "no matching speedtest run to stop: nothing is running, or that run already ended", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// speedRunStatus derives every speedtest status field from ONE load of the run
// id. The scheduler keeps "is one running" and "which one" in a single word
// precisely so they cannot disagree (see Scheduler.Running) - but reading them
// back through separate calls re-split them: each call is one load of that
// word, so a run ending between the loads answered running=true with run_id=0,
// the exact "running with no id" state whose stop click sends an id-0 abort.
// Deriving running from the id keeps the pair whole in either evaluation
// order, and the server label rides the same snapshot so it can never name a
// run this answer says is not there.
func speedRunStatus(sp SpeedTrigger) (running bool, id uint64, server string) {
	if sp == nil {
		return false, 0, ""
	}
	if id = sp.RunID(); id == 0 {
		return false, 0, ""
	}
	return true, id, sp.CurrentServer()
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	mins := 120
	if v := r.URL.Query().Get("mins"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= maxWinMins {
			mins = n
		}
	}
	now := time.Now()
	since, until := now.Add(-time.Duration(mins)*time.Minute), time.Time{}
	if f, u, ok := parseRangeParams(r, now); ok {
		since, until = f, u
	}
	// Downsample so wide windows stay small/fast; see seriesBucket for the rule.
	bucket := seriesBucket(since, until, now)
	// ?exclude=name,name drops those targets from the lowest-latency line (the
	// UI toggle). Capped so a hostile client can't grow the IN clause unbounded.
	var exclude []string
	if v := r.URL.Query().Get("exclude"); v != "" {
		exclude = strings.Split(v, ",")
		if len(exclude) > 12 {
			exclude = exclude[:12]
		}
	}
	pts, err := s.store.Series(r.Context(), since, until, bucket, exclude)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if pts == nil {
		pts = []store.SeriesPoint{}
	}
	writeJSON(w, pts)
}

func (s *Server) handleNetinfo(w http.ResponseWriter, r *http.Request) {
	if s.netinfo == nil { // permitted nil (e.g. New(...) with no collector): 503, not a panic
		http.Error(w, "network info unavailable", http.StatusServiceUnavailable)
		return
	}
	// POST forces a full re-fetch (incl. the exit traceroute, a few seconds) -
	// the manual refresh button. GET returns the cached snapshot.
	if r.Method == http.MethodPost {
		if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		writeJSON(w, s.netinfo.RefreshNow(ctx))
		return
	}
	writeJSON(w, s.netinfo.Get())
}

// handleIperfCheck is the reachability probe behind the per-server status light:
// it TCP-dials the requested iperf3 address and reports whether it connected
// (plus handshake RTT). It only opens and closes a socket - the same thing a
// speedtest against that address does - so it's no broader than the engine.
// POST-only with the JSON content-type CSRF guard, like the other endpoints
// with network side effects: a cross-site page must not be able to use this
// host as a LAN port-scanning pivot via no-cors GETs.
// iperfCheckBudget is both the dial timeout AND the uniform response floor for
// handleIperfCheck: every outcome takes this long, so the response time leaks
// nothing. A var (not const) so tests can shrink it.
var iperfCheckBudget = 4 * time.Second

func (s *Server) handleIperfCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), iperfCheckBudget)
	defer cancel()
	addr := strings.TrimSpace(r.URL.Query().Get("addr"))
	start := time.Now()
	rtt, err := speedtest.CheckIperfServer(ctx, addr)
	// Uniform-time response. This endpoint is deliberately NOT SSRF-blocked (a LAN
	// iperf3 server is a legitimate target), so answering the moment the dial
	// resolves would turn its latency into a probe oracle: a refused or reachable
	// host answers in ~1 RTT while a filtered one stalls to the timeout, letting a
	// caller map internal hosts/ports by timing alone. The reported/not-reachable
	// verdict already hides the raw error; pad the wall-clock to the full budget so
	// it hides the timing too. The rtt_ms field still reports the real handshake
	// time - only the response's arrival is flattened.
	if d := iperfCheckBudget - time.Since(start); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done(): // client hung up; no point holding the goroutine
		}
	}
	if err == nil {
		writeJSON(w, map[string]any{"reachable": true, "rtt_ms": rtt})
	} else {
		writeJSON(w, map[string]any{"reachable": false})
	}
}

// handleSpeedtestServers returns Ookla servers for the picker. With ?city=<name>
// it geocodes the city and returns servers near it; otherwise the city of the
// server the last auto run used (see the default branch below), falling back to
// the first coordinate-carrying candidate city, else the caller's own location.
func (s *Server) handleSpeedtestServers(w http.ResponseWriter, r *http.Request) {
	// POST + application/json, for the same reason handleIperfCheck demands them
	// ("body-less POST: CSRF guard"): this handler REACHES OUT - to Ookla, and to
	// a geocoder for a city query. Any page open in the operator's browser can aim
	// a form-style GET or POST at http://127.0.0.1:9000 without reading the reply
	// and without tripping CORS, and the loopback filter cannot object because the
	// request really is from loopback. Requiring a JSON content type puts it
	// outside what a cross-site request can send without a preflight, which this
	// daemon never approves.
	//
	// Nothing here is SSRF - the destinations are fixed and redirects are refused
	// (see geocodeClient) - so what is being closed is a page making someone
	// else's daemon generate traffic, not an attacker choosing its target.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) {
		return
	}
	if s.netinfo == nil { // permitted nil: 503, not a panic (see handleNetinfo)
		http.Error(w, "network info unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()

	// Pin an exact server by Ookla ID: the "City or server ID" box resolves a
	// numeric entry here so the UI can confirm its name before saving.
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		srv, err := getOoklaServer(ctx, id)
		if err != nil {
			// Only "no such server" is the user's to fix. A transport failure or
			// this handler's own deadline says nothing about the ID, and the page
			// must not paint "not found" over a real server - or refuse to save
			// it - because Ookla was unreachable for a moment. Known residual: the
			// library never inspects the HTTP status, so an Ookla error page
			// arrives as a decode error (malformed HTML, the usual case) and
			// reads as unreachable - but a well-formed one decodes to an empty
			// list and still reads as not found.
			switch {
			case errors.Is(err, speedtest.ErrServerNotFound):
				http.Error(w, "server not found", http.StatusNotFound)
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				http.Error(w, "server lookup timed out", http.StatusGatewayTimeout)
			default:
				http.Error(w, "could not reach the server catalogue", http.StatusBadGateway)
			}
			return
		}
		// NOT through browseServers: this reply must never carry a coordinate. The
		// by-ID endpoint backfills the CALLER'S own position on a sparse record
		// (see recentrePin), so the plain type - which withholds Lat/Lon - is what
		// keeps a starred by-ID server from being saved at our address.
		writeJSON(w, map[string]any{"servers": []speedtest.ServerInfo{srv}})
		return
	}

	var lat, lon float64
	var locName string
	var centre string                   // "last_run" when centred on the last auto run's server
	var centreSrv *speedtest.ServerInfo // that server; guaranteed into the list below
	if city := strings.TrimSpace(r.URL.Query().Get("city")); city != "" {
		var err error
		lat, lon, locName, err = geocode(ctx, city)
		if err != nil {
			http.Error(w, "could not find that city", http.StatusBadGateway)
			return
		}
	} else {
		// Allow refetching a saved auto location directly by coordinate.
		lat, _ = strconv.ParseFloat(r.URL.Query().Get("lat"), 64)
		lon, _ = strconv.ParseFloat(r.URL.Query().Get("lon"), 64)
		// ParseFloat accepts "NaN"/"Inf", which would flow into the Ookla query
		// and then abort the JSON encode of the response (an empty 200); reject.
		if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
			http.Error(w, "invalid lat/lon", http.StatusBadRequest)
			return
		}
		// With no explicit coordinate, centre the default list where auto last
		// LANDED: the server the most recent auto run measured, its own
		// selection report proving it was chosen by the race and not a pin.
		// This remembers for DISPLAY only - the race stays uncached and
		// re-decides every run (speedtest.raceCities) and nothing here feeds
		// back into selection - but it upgrades the browse guarantee from "a
		// city the race would consider" to "the city it last chose", and the
		// last-used server is always findable in the list (prepended below in
		// the rare case the metro fetch around it omits it). Stale only until the
		// next auto run - a reconnect triggers one - and the panel words it in
		// the past tense rather than pretending a present centre.
		//
		// The centring must never trust the by-ID record's coordinates: that
		// endpoint backfills the CALLER'S own geolocation into a server's
		// lat/lon when its record is sparse (measured - a "Montréal, QC"
		// server came back carrying this machine's position), which once
		// centred the list on the operator while the caption named the
		// server's city. Instead the server's name is searched and its OWN
		// row - matched by ID, which disambiguates same-named cities
		// worldwide for free - supplies real catalog coordinates, and the
		// ordinary metro fetch runs around those. The search result is only
		// that coordinate oracle, never the list: a name cohort collapses to
		// one row when the measured server wears a suburb's name tag
		// (measured: an Ottawa-scoped run landing on "Nepean, ON").
		//
		// With no qualifying run (fresh install, iperf3/pre-feature history, a
		// longtime pin, or the ID lookup failing), fall back to the first
		// candidate city auto-select would race that carries a coordinate: the
		// exit router when RIPE placed it, else the ISP geolocation of our
		// public IP, else a starred server's city, else the last race's winning
		// city (main.autoOrigins, via speedtest.FirstAnchoredOrigin).
		// Nothing anchored leaves the fetch uncentred, which is not a gap but
		// the remaining candidate itself - the Ookla API then places our source
		// address and returns the pool IT puts us in.
		//
		// This handler used to run its OWN cascade - exit coordinate, else the
		// Cloudflare PoP - which agreed with auto-select on nothing. The PoP
		// rung fired on any missing exit COORDINATE, not only where a traceroute
		// could not run, so on a residential link whose last hop RIPE cannot
		// place (measured: the traceroute RAN and found the hop) it centred on
		// a distant PoP while every city auto considers was domestic - and
		// those pools are disjoint, so the server auto actually tests from could
		// not be found in the picker at all. The PoP is gone from this list
		// because it is the one centre the race can never choose.
		//
		// Still a BROWSING list either way: it deliberately does NOT race -
		// that would spend a list fetch per city and a round of pings at other
		// people's servers every time the settings pane opens, to reorder a
		// list the user is about to pick from by hand. Two accepted edges keep
		// the last-run path honest rather than perfect, both self-healing on
		// the next auto run: a run whose report insert failed (or imported
		// history without reports) is skipped, so "your last auto test" can
		// name the newest EXPLAINABLE run rather than the newest run; and a
		// run auto-selected inside a since-cleared searched-city scope still
		// counts as auto, because the report records how the winner won, not
		// the scope it won under. On the FALLBACK the guarantee degrades to
		// the old, weaker one, and the panel words it so: the centre is a city
		// the race would consider, never one it could not choose, and the
		// server auto tests from need not appear here - search its city to
		// see it.
		if lat == 0 && lon == 0 {
			if id, ok := s.lastAutoRunServerID(ctx); ok {
				// Sub-budget: the by-ID lookup hits a different Ookla backend
				// than the list fetch, and a differential stall there must not
				// starve the fetch this handler exists for - the centring is
				// worth less than the list it centres.
				idCtx, cancelID := context.WithTimeout(ctx, 6*time.Second)
				srv, err := getOoklaServer(idCtx, id)
				cancelID()
				if err == nil && srv.Name != "" {
					// Its own sub-budget too: a stalled search must leave the
					// coordinate fetch time to run, or a hang here turns the
					// whole browse into a 502.
					searchCtx, cancelSearch := context.WithTimeout(ctx, 8*time.Second)
					found, err := searchOoklaServers(searchCtx, srv.Name)
					cancelSearch()
					if err == nil {
						for i := range found {
							if found[i].ID == srv.ID && (found[i].Lat != 0 || found[i].Lon != 0) {
								lat, lon, locName = found[i].Lat, found[i].Lon, srv.Name
								centre, centreSrv = "last_run", &srv
								break
							}
						}
					}
				}
			}
			// The first anchored origin: the exit router, else the ISP city, else
			// the first city the user has starred a server in, else the city that
			// won the last race (main.autoOrigins).
			if centre == "" && s.AutoOriginsFn != nil {
				if org, ok := speedtest.FirstAnchoredOrigin(s.AutoOriginsFn()); ok {
					lat, lon, locName = org.Lat, org.Lon, org.Label
				}
			}
		}
	}

	list, err := listOoklaServers(ctx, lat, lon)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if list == nil {
		list = []speedtest.ServerInfo{}
	}
	// The point of last-run centring is that the last-used server is findable
	// here; a metro fetch around its own registered coordinates should always
	// include it, but the guarantee is cheap to make unconditional, so it is.
	if centreSrv != nil && !slices.ContainsFunc(list, func(si speedtest.ServerInfo) bool { return si.ID == centreSrv.ID }) {
		pre := *centreSrv
		pre.DistanceKM = 0 // distance to the centre, which is this server
		// The by-ID record it was resolved from carries no coordinate (that
		// endpoint reports the caller's), but lat/lon here ARE this server's
		// catalogue position - the metro fetch was centred on them - so the row
		// goes out with them like every other listed server. Without this a star
		// on the one row the caption names saved it at 0,0.
		pre.Lat, pre.Lon = lat, lon
		list = append([]speedtest.ServerInfo{pre}, list...)
	}
	writeJSON(w, map[string]any{"servers": browseServers(list), "location": locName, "lat": lat, "lon": lon, "centre": centre})
}

// browseServer is one row of the picker's browse listing: a ServerInfo plus the
// catalogue coordinate ServerInfo itself withholds from JSON. The picker saves
// that coordinate when a server is starred: the listing is the only reply whose
// position is the server's own - the by-ID endpoint reports the CALLER'S
// position for a server on the caller's ISP (see recentrePin in
// internal/speedtest) - and a pinned best-of run reads the saved pair back when
// the live catalogue cannot place the pin. The by-ID reply itself never comes
// through here (ServerInfo's json:"-" tags keep it bare); omitempty is for a
// listed row whose catalogue pair failed to parse, and the tested guard should
// a by-ID-shaped record ever be routed through this wrapper without the
// override the prepended last-run row gets.
type browseServer struct {
	speedtest.ServerInfo
	Lat float64 `json:"lat,omitempty"`
	Lon float64 `json:"lon,omitempty"`
}

func browseServers(list []speedtest.ServerInfo) []browseServer {
	out := make([]browseServer, len(list))
	for i, s := range list {
		out[i] = browseServer{ServerInfo: s, Lat: s.Lat, Lon: s.Lon}
	}
	return out
}

// handleSpeedtestCandidates answers the picker's Auto button with the field an
// automatic run would race right now: every origin's pool (exit router, ISP
// city, the starred servers' cities, the last race's winning city, Ookla's
// own placement), deduplicated,
// pinged, fastest first - the candidates the RUN button would choose between,
// with the pings the race ranks on. Pings only, no transfer. POST + JSON for
// the same reason handleSpeedtestServers demands them: this reaches out.
func (s *Server) handleSpeedtestCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) {
		return
	}
	if s.RaceListingFn == nil {
		http.Error(w, "auto selection is not available", http.StatusServiceUnavailable)
		return
	}
	// A race pinged through a running transfer measures the transfer, not the
	// servers; and the run's own selection is what the list claims to preview.
	if s.speed != nil && s.speed.RunID() != 0 {
		http.Error(w, "a speedtest is running", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	l, err := s.RaceListingFn(ctx)
	if err != nil {
		s.log.Debug("candidate race failed", "err", err)
		http.Error(w, "could not race the candidate cities", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"origins": originsDTO(l.Origins),
		"winner":  originDTO(l.Winner),
		"servers": raceCandidatesDTO(l.Servers),
	})
}

// raceCandidate is one row of the Auto listing on the wire: a browse row (the
// catalogue coordinate rides along, so a star saves it) plus the origin whose
// pool surfaced the server.
type raceCandidate struct {
	browseServer
	Origin      string `json:"origin"`
	OriginLabel string `json:"origin_label,omitempty"`
	InField     bool   `json:"in_field"` // a run centred where this race centres would choose from this row
}

func raceCandidatesDTO(in []speedtest.RaceCandidate) []raceCandidate {
	out := make([]raceCandidate, len(in))
	for i, c := range in {
		out[i] = raceCandidate{browseServer: browseServer{ServerInfo: c.ServerInfo, Lat: c.Lat, Lon: c.Lon},
			Origin: c.Origin, OriginLabel: c.OriginLabel, InField: c.InField}
	}
	return out
}

// roundOf renders a round member's winner timestamp for the CSV; blank when
// the row is a test's own result.
func roundOf(ts *int64) string {
	if ts == nil {
		return ""
	}
	return time.Unix(*ts, 0).UTC().Format(time.RFC3339)
}

// bestOfCountFrom is the Best-of count a settings POST asks for: the count
// when sent, else the retired on/off from an older client (on meant three
// servers, off one), else nothing. current is the count in force: an old
// client that read "on" and posts it back (the previous build's page, still
// open after the daemon was upgraded, posts the whole form) means "keep the
// round", not "make it three", so "on" over a round already larger than one
// changes nothing.
func bestOfCountFrom(count *int, legacy *bool, current int) *int {
	if count != nil || legacy == nil {
		return count
	}
	n := 1
	if *legacy {
		if current > 1 {
			return nil
		}
		n = settings.LegacyBestOfCount
	}
	return &n
}

// maxPingIDs bounds one refresh: the kept list is capped at twelve (see
// settings.maxSpeedServers), and each ID costs a by-ID fetch plus a probe set
// at a third party.
const maxPingIDs = 12

// handleSpeedtestPing re-measures a handful of servers by ID on demand -
// POST {"ids":[...]} -> {"pings":{id: ms|null}, "health":{id: bool}} - for the
// picker's saved pane, whose servers are mostly outside whatever list was
// last fetched. health carries only determined verdicts (the three-state rule
// browse rows follow), so a kept server's Unsupported mark can come from the
// refresh. POST + JSON for the reason handleSpeedtestServers gives: it reaches
// out. 409 while a run is in flight, as the candidate race is: a ping through
// a transfer measures the transfer.
func (s *Server) handleSpeedtestPing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) {
		return
	}
	if s.PingServersFn == nil {
		http.Error(w, "server pings are not available", http.StatusServiceUnavailable)
		return
	}
	if s.speed != nil && s.speed.RunID() != 0 {
		http.Error(w, "a speedtest is running", http.StatusConflict)
		return
	}
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "bad JSON body", http.StatusBadRequest)
		return
	}
	ids := cleanServerIDs(body.IDs)
	if len(ids) == 0 {
		http.Error(w, "no server ids", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res := s.PingServersFn(ctx, ids)
	pings := make(map[string]*float64, len(res))
	health := make(map[string]bool)
	for id, p := range res {
		pings[id] = p.PingMS
		if p.FallbackOK != nil {
			health[id] = *p.FallbackOK
		}
	}
	writeJSON(w, map[string]any{"pings": pings, "health": health})
}

// handleSpeedServerPings answers with what the daemon's own runs measured:
// GET ?ids=a,b,c -> {"pings":{id: median ms}} of each server's recent ranking
// pings (see store.RecentRankPings). No network - it is what the saved pane
// shows before anyone asks for a fresh measurement, and a server the runs
// never ranked is simply absent.
func (s *Server) handleSpeedServerPings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	ids := cleanServerIDs(strings.Split(r.URL.Query().Get("ids"), ","))
	if len(ids) == 0 {
		writeJSON(w, map[string]any{"pings": map[string]float64{}})
		return
	}
	pings, err := s.store.RecentRankPings(r.Context(), ids, recentRankPingRuns)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, map[string]any{"pings": pings})
}

// recentRankPingRuns is how many of a server's ranking pings the saved pane's
// median is taken over: enough to sit on the typical value rather than the last
// hour's, few enough to follow a server that has genuinely changed.
const recentRankPingRuns = 20

// cleanServerIDs keeps the plausible Ookla IDs (digits, as the catalogue
// issues them), deduplicated, in order, at most maxPingIDs of them.
func cleanServerIDs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 12 || seen[id] || strings.Trim(id, "0123456789") != "" {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == maxPingIDs {
			break
		}
	}
	return out
}

func originDTO(o *speedtest.Origin) map[string]any {
	if o == nil {
		return nil
	}
	m := map[string]any{"kind": o.Kind, "label": o.Label}
	// The coordinate rides along for an anchored origin so the picker can
	// measure a held row's distance from the centre now in effect (the
	// candidates' own distances are from this point); an unanchored origin has
	// none and sends none.
	if o.Anchored {
		m["lat"], m["lon"] = o.Lat, o.Lon
	}
	return m
}

func originsDTO(in []speedtest.Origin) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for i := range in {
		out = append(out, originDTO(&in[i]))
	}
	return out
}

// lastAutoRunServerID finds the server the most recent AUTO-selected Ookla run
// measured, for centring the default browse list. A run qualifies only when
// its own selection report (speed_servers) shows a winner that was not pinned
// - a pinned one-off centring the browse list on the pin's city is the exact
// confusion this path exists to remove, so runs without rows (pre-feature
// history, imports, iperf3) are skipped rather than trusted. The scan is
// bounded: a longtime-pinned instance takes the anchored-origin fallback
// instead of walking its whole history.
func (s *Server) lastAutoRunServerID(ctx context.Context) (string, bool) {
	const scanRuns = 12
	if s.store == nil {
		return "", false
	}
	runs, err := s.store.SpeedResults(ctx, scanRuns) // results only: a kept round's members have no report and would spend the dozen
	if err != nil {
		return "", false
	}
	for _, run := range runs {
		if run.ServerID == "" || (run.Engine != "" && run.Engine != "ookla") {
			continue
		}
		rows, err := s.store.SpeedServers(ctx, run.TS)
		if err != nil {
			return "", false
		}
		for _, row := range rows {
			if !row.Winner {
				continue
			}
			if speedtest.PinnedRun(row.WinReason) {
				break // a pinned run, single-target or best-of: keep looking for the last AUTO one
			}
			if row.ServerID == "" {
				break // an imported report missing the ID can centre nothing
			}
			return row.ServerID, true
		}
	}
	return "", false
}

// geocodeClient caps the Nominatim fetch like netinfo's lookup clients: an
// explicit timeout and no redirects, so a hijacked upstream can't bounce the
// daemon's GET to an arbitrary host and stream an unbounded body back.
// listOoklaServers is the settings picker's list fetch. A package var for the
// same reason speedtest's own fetch seams are: it is the only way a test can
// assert WHAT COORDINATE this handler centres on without opening a socket at
// Ookla, and that coordinate is the whole defect this handler was fixed for.
var listOoklaServers = speedtest.ListOoklaServers

// getOoklaServer resolves a server by ID - both the picker's by-ID search and
// the last auto run's server for the browse centring above; a seam for the same
// reason listOoklaServers is.
var getOoklaServer = speedtest.GetOoklaServer

// searchOoklaServers fetches the browse list by city-name keyword for the
// last-run path (real per-server distances, untrusted coordinates avoided);
// a seam for the same reason listOoklaServers is.
var searchOoklaServers = speedtest.SearchOoklaServers

var geocodeClient = &http.Client{
	Timeout:       10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// geocode resolves a free-text place to a coordinate via OpenStreetMap Nominatim.
func geocode(ctx context.Context, query string) (lat, lon float64, name string, err error) {
	u := "https://nominatim.openstreetmap.org/search?format=json&limit=1&q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, 0, "", err
	}
	req.Header.Set("User-Agent", "pingularity (speedtest server picker)")
	resp, err := geocodeClient.Do(req)
	if err != nil {
		return 0, 0, "", err
	}
	defer resp.Body.Close()
	var arr []struct {
		Lat         string `json:"lat"`
		Lon         string `json:"lon"`
		DisplayName string `json:"display_name"`
	}
	// A limit=1 Nominatim reply is well under 64 KiB; cap the body so a
	// compromised peer can't stream gigabytes into the decoder.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&arr); err != nil {
		return 0, 0, "", err
	}
	if len(arr) == 0 {
		return 0, 0, "", fmt.Errorf("no match for %q", query)
	}
	lat, err = strconv.ParseFloat(arr[0].Lat, 64)
	if err != nil {
		return 0, 0, "", fmt.Errorf("bad latitude %q: %w", arr[0].Lat, err)
	}
	lon, err = strconv.ParseFloat(arr[0].Lon, 64)
	if err != nil {
		return 0, 0, "", fmt.Errorf("bad longitude %q: %w", arr[0].Lon, err)
	}
	// ParseFloat accepts "NaN"/"Inf"; a non-finite coordinate would flow into the
	// Ookla query and later abort the JSON encode as an empty 200. Reject it here
	// so both the city and lat/lon branches are guarded (see the sibling check).
	if math.IsNaN(lat) || math.IsInf(lat, 0) || math.IsNaN(lon) || math.IsInf(lon, 0) {
		return 0, 0, "", fmt.Errorf("non-finite coordinate for %q", query)
	}
	return lat, lon, arr[0].DisplayName, nil
}

// iperfServerDTO is one saved iperf3 server in the settings JSON. Its password is
// write-only: GET sends HasPassword instead of the secret, and a blank Password on
// POST means "keep the stored one" (settings.Update merges by address).
type iperfServerDTO struct {
	Label       string `json:"label"`
	Addr        string `json:"addr"`
	Bind        string `json:"bind"`
	IPVer       string `json:"ipver"`
	Auth        bool   `json:"auth"`
	Username    string `json:"username"`
	RSAKey      string `json:"rsa_key"`
	Password    string `json:"password,omitempty"`     // POST in, never set on GET
	HasPassword bool   `json:"has_password,omitempty"` // GET out: whether a password is stored
	PKCS1       bool   `json:"pkcs1,omitempty"`        // force --use-pkcs1-padding (legacy iperf3 servers)
}

// iperfServersToDTO maps stored targets to the API shape, withholding each password
// (sending only has_password) so the secret never leaves the host.
func iperfServersToDTO(ts []settings.IperfTarget) []iperfServerDTO {
	out := make([]iperfServerDTO, len(ts))
	for i, t := range ts {
		out[i] = iperfServerDTO{
			Label: t.Label, Addr: t.Addr, Bind: t.Bind, IPVer: t.IPVer,
			Auth: t.Auth, Username: t.Username, RSAKey: t.RSAKey, PKCS1: t.PKCS1,
			HasPassword: t.Password != "",
		}
	}
	return out
}

// iperfServersFromDTO maps incoming servers back to settings targets, carrying the
// password through (blank stays blank - settings.Update fills it from the stored one).
func iperfServersFromDTO(ds []iperfServerDTO) []settings.IperfTarget {
	out := make([]settings.IperfTarget, len(ds))
	for i, d := range ds {
		out[i] = settings.IperfTarget{
			Label: d.Label, Addr: d.Addr, Bind: d.Bind, IPVer: d.IPVer,
			Auth: d.Auth, Username: d.Username, Password: d.Password, RSAKey: d.RSAKey, PKCS1: d.PKCS1,
		}
	}
	return out
}

// settingsDTO is the JSON shape for GET/POST /api/settings. The form fields
// are pointers so a POST is a PATCH: a field absent from the body decodes to
// nil and keeps its current value, instead of silently resetting ~50 settings
// to Go zero values. GET (and the POST echo) fills every pointer, so the
// response shape is unchanged.
type settingsDTO struct {
	LatencySeconds           *int64  `json:"latency_seconds"`
	LatencyEnabled           *bool   `json:"latency_enabled"`
	DNSProbe                 *bool   `json:"dns_probe_enabled"`
	NetinfoEnabled           *bool   `json:"netinfo_enabled"`
	SpeedSeconds             *int64  `json:"speed_seconds"`
	RetentionSeconds         *int64  `json:"retention_seconds"`
	SpeedRetentionSeconds    *int64  `json:"speed_retention_seconds"`
	DowntimeRetentionSeconds *int64  `json:"downtime_retention_seconds"`
	TimeoutSeconds           *int64  `json:"timeout_seconds"`
	DownAfter                *int    `json:"down_after"`
	UpAfter                  *int    `json:"up_after"`
	SpeedServerID            *string `json:"speed_server_id"`
	SpeedtestEnabled         *bool   `json:"speedtest_enabled"`
	SpeedtestOnReconnect     *bool   `json:"speedtest_on_reconnect"`
	IPv6Mode                 *string `json:"ipv6_mode"`
	WebhookFormat            *string `json:"webhook_format"`
	QuickSetupDone           *bool   `json:"quick_setup_done"`
	ExitTarget               *string `json:"exit_target"`
	// Adaptive / event-driven speedtesting.
	SpeedtestAdaptive   *bool    `json:"speedtest_adaptive"`
	SpeedtestOnDegraded *bool    `json:"speedtest_on_degraded"`
	DegradedPingMS      *float64 `json:"degraded_ping_ms"`
	SpeedtestSkipBusy   *bool    `json:"speedtest_skip_busy"`
	SpeedBusyMbps       *float64 `json:"speedtest_busy_mbps"`
	// Speedtest engine selection. The slice fields use nil = keep current,
	// explicit [] = clear.
	SpeedEngine      *string          `json:"speed_engine"`
	IperfServer      *string          `json:"iperf_server"`
	IperfServers     []iperfServerDTO `json:"iperf_servers"`
	IperfDur         *int             `json:"iperf_duration"`
	IperfStreams     *int             `json:"iperf_streams"`
	OoklaConnections *int             `json:"ookla_connections"`
	// Ookla auto-select challenger cadence: after every N automatic tests the
	// next scheduled test measures the strongest rival instead of the
	// incumbent (0 = never). API-only - no row in the settings drawer; the
	// bar the rival must clear is derived from the incumbent's own record.
	SpeedChallengeEvery *int  `json:"speed_challenge_every"`
	OoklaLoss           *bool `json:"ookla_loss"`
	// SpeedDiscardLosers: a Best-of round keeps only its winner (true) or
	// records every server it measured as a member of the round (false).
	SpeedDiscardLosers *bool `json:"speed_discard_losers"`
	// SpeedBestOfCount is how many servers a Best-of round measures (1 = one).
	// SpeedBestOf is the retired on/off: still accepted on a POST from an older
	// client (true -> 3, false -> 1) when no count is sent, and still emitted
	// as the derived on/off for one release.
	SpeedBestOfCount *int    `json:"speed_best_of_count"`
	SpeedBestOf      *bool   `json:"speed_best_of"`
	IperfOmit        *int    `json:"iperf_omit"`
	SpeedDirection   *string `json:"speed_direction"`
	IperfDirection   *string `json:"iperf_direction"`
	IperfUDP         *bool   `json:"iperf_udp"`
	IperfUDPRate     *int    `json:"iperf_udp_rate"`
	IperfWindow      *int    `json:"iperf_window"`
	SpeedRetries     *int    `json:"speed_retries"`
	IperfRetries     *int    `json:"iperf_retries"`
	IperfCongestion  *string `json:"iperf_congestion"`
	IperfNoDelay     *bool   `json:"iperf_nodelay"`
	IperfDSCP        *string `json:"iperf_dscp"`
	IperfMSS         *int    `json:"iperf_mss"`

	// The saved Ookla picker list, following the same nil/[] rule as the iperf3
	// one above. settings.SavedServer holds nothing secret, so it IS the wire
	// shape - a parallel DTO would only be a place to drift.
	SpeedServers []settings.SavedServer `json:"speed_servers"`

	// Alerting.
	ThreshDownMbps    *float64 `json:"thresh_down_mbps"`
	ThreshUpMbps      *float64 `json:"thresh_up_mbps"`
	ThreshPingMS      *float64 `json:"thresh_ping_ms"`
	ThreshJitterMS    *float64 `json:"thresh_jitter_ms"`
	ThreshLossPct     *float64 `json:"thresh_loss_pct"`
	ThreshConsec      *int     `json:"thresh_consecutive"`
	ThreshBloatDownMS *float64 `json:"thresh_bloat_down_ms"`
	ThreshBloatUpMS   *float64 `json:"thresh_bloat_up_ms"`
	AlertOnOutage     *bool    `json:"alert_on_outage"`
	WebhookURL        *string  `json:"webhook_url"`
	HeartbeatURL      *string  `json:"heartbeat_url"`
	DigestFreq        *string  `json:"digest_freq"`
	// Schedule (latency probing and speedtests each gated to their own windows).
	SchedLatEnabled   *bool             `json:"sched_lat_enabled"`
	SchedLatWindows   []settings.Window `json:"sched_lat_windows"`
	SchedSpeedEnabled *bool             `json:"sched_speed_enabled"`
	SchedSpeedWindows []settings.Window `json:"sched_speed_windows"`
	// Bounds (GET only) so the UI can constrain inputs.
	MinLatencySeconds int64 `json:"min_latency_seconds,omitempty"`
	MaxLatencySeconds int64 `json:"max_latency_seconds,omitempty"`
	MinSpeedSeconds   int64 `json:"min_speed_seconds,omitempty"`
	MaxSpeedSeconds   int64 `json:"max_speed_seconds,omitempty"`
	MinTimeoutSeconds int64 `json:"min_timeout_seconds,omitempty"`
	MaxTimeoutSeconds int64 `json:"max_timeout_seconds,omitempty"`
	MaxStreak         int   `json:"max_streak,omitempty"`
	// ServerNowUnix/ServerTZOffsetMin (GET only) expose the server's clock and
	// current UTC offset so the schedule tab can place its "now" marker in the
	// server's local time - the time the windows are actually evaluated in.
	ServerNowUnix     int64 `json:"server_now_unix,omitempty"`
	ServerTZOffsetMin int   `json:"server_tz_offset_min"`
	// BusyDeferSupported (GET only) reports whether this host can read the
	// interface counters busy-defer needs (Linux); the toggle is greyed otherwise.
	BusyDeferSupported bool `json:"busy_defer_supported"`
	// Iperf3Available (GET+POST) reports whether the iperf3 binary is on PATH;
	// the iperf3 engine option is greyed otherwise. Sent on save too so
	// applySettings doesn't wrongly grey it after a POST.
	Iperf3Available bool   `json:"iperf3_available"`
	Iperf3Version   string `json:"iperf3_version,omitempty"` // the local iperf3's parsed version ("" if absent/unparseable); PATH presence is not capability
	// Containerized reports whether the daemon runs inside a container. When iperf3 is
	// unavailable it picks the right "how to enable" note: install the package (native)
	// vs switch to the -iperf image variant (container, which has no package manager).
	Containerized bool `json:"containerized"`
	// Bridged (GET+POST) narrows Containerized: the container does NOT share the
	// host's network namespace, so loopback addresses dial the container itself,
	// not the machine. The UI's iperf3 localhost warning keys on THIS, not on
	// Containerized - in a host-network container localhost IS the host and the
	// warning would be wrong there.
	Bridged         bool         `json:"bridged"`
	CongestionAlgos []string     `json:"congestion_algos,omitempty"` // host's allowed TCP congestion algos (GET only, suggestions)
	Defaults        *settingsDTO `json:"defaults,omitempty"`         // factory defaults (GET only)
}

func dtoFrom(v settings.Values) settingsDTO {
	return settingsDTO{
		LatencySeconds:           ptr(int64(v.Latency.Seconds())),
		LatencyEnabled:           ptr(v.LatencyEnabled),
		DNSProbe:                 ptr(v.DNSProbe),
		NetinfoEnabled:           ptr(v.NetinfoEnabled),
		SpeedSeconds:             ptr(int64(v.Speed.Seconds())),
		RetentionSeconds:         ptr(int64(v.Retention.Seconds())),
		SpeedRetentionSeconds:    ptr(int64(v.SpeedRetention.Seconds())),
		DowntimeRetentionSeconds: ptr(int64(v.DowntimeRetention.Seconds())),
		TimeoutSeconds:           ptr(int64(v.Timeout.Seconds())),
		DownAfter:                ptr(v.DownAfter),
		UpAfter:                  ptr(v.UpAfter),
		SpeedServerID:            ptr(v.SpeedServerID),
		SpeedtestEnabled:         ptr(v.SpeedtestEnabled),
		SpeedtestOnReconnect:     ptr(v.SpeedtestOnReconnect),
		IPv6Mode:                 ptr(v.IPv6Mode),
		WebhookFormat:            ptr(v.WebhookFormat),
		QuickSetupDone:           ptr(v.QuickSetupDone),
		ExitTarget:               ptr(v.ExitTarget),
		SpeedtestAdaptive:        ptr(v.SpeedtestAdaptive),
		SpeedtestOnDegraded:      ptr(v.SpeedtestOnDegraded),
		DegradedPingMS:           ptr(v.DegradedPingMS),
		SpeedtestSkipBusy:        ptr(v.SpeedtestSkipBusy),
		SpeedBusyMbps:            ptr(v.SpeedBusyMbps),
		SpeedEngine:              ptr(v.SpeedEngine),
		IperfServer:              ptr(v.IperfServer),
		IperfServers:             iperfServersToDTO(v.IperfServers),
		SpeedServers:             v.SpeedServers,
		IperfDur:                 ptr(v.IperfDur),
		IperfStreams:             ptr(v.IperfStreams),
		OoklaConnections:         ptr(v.OoklaConnections),
		SpeedChallengeEvery:      ptr(v.SpeedChallengeEvery),
		OoklaLoss:                ptr(v.OoklaLoss),
		SpeedDiscardLosers:       ptr(v.SpeedDiscardLosers),
		SpeedBestOfCount:         ptr(v.SpeedBestOfCount),
		SpeedBestOf:              ptr(v.SpeedBestOfCount > 1),
		IperfOmit:                ptr(v.IperfOmit),
		SpeedDirection:           ptr(v.SpeedDirection),
		IperfDirection:           ptr(v.IperfDirection),
		IperfUDP:                 ptr(v.IperfUDP),
		IperfUDPRate:             ptr(v.IperfUDPRate),
		IperfWindow:              ptr(v.IperfWindow),
		SpeedRetries:             ptr(v.SpeedRetries),
		IperfRetries:             ptr(v.IperfRetries),
		IperfCongestion:          ptr(v.IperfCongestion),
		IperfNoDelay:             ptr(v.IperfNoDelay),
		IperfDSCP:                ptr(v.IperfDSCP),
		IperfMSS:                 ptr(v.IperfMSS),

		ThreshDownMbps:    ptr(v.ThreshDownMbps),
		ThreshUpMbps:      ptr(v.ThreshUpMbps),
		ThreshPingMS:      ptr(v.ThreshPingMS),
		ThreshJitterMS:    ptr(v.ThreshJitterMS),
		ThreshLossPct:     ptr(v.ThreshLossPct),
		ThreshConsec:      ptr(v.ThreshConsec),
		ThreshBloatDownMS: ptr(v.ThreshBloatDownMS),
		ThreshBloatUpMS:   ptr(v.ThreshBloatUpMS),
		AlertOnOutage:     ptr(v.AlertOnOutage),
		WebhookURL:        ptr(v.WebhookURL),
		HeartbeatURL:      ptr(v.HeartbeatURL),
		DigestFreq:        ptr(v.DigestFreq),
		SchedLatEnabled:   ptr(v.SchedLatEnabled),
		SchedLatWindows:   v.SchedLatWindows,
		SchedSpeedEnabled: ptr(v.SchedSpeedEnabled),
		SchedSpeedWindows: v.SchedSpeedWindows,
	}
}

// ptr boxes a value for the DTO's pointer fields.
func ptr[T any](v T) *T { return &v }

// durp maps an optional seconds count to an optional Duration (nil stays nil).
func durp(n *int64) *time.Duration {
	if n == nil {
		return nil
	}
	d := secsToDur(*n)
	return &d
}

// secsToDur converts a seconds count to a Duration, clamping to 10 years (and
// flooring negatives at 0) so a hostile value can't overflow the int64
// nanoseconds in the multiply.
func secsToDur(n int64) time.Duration {
	if n < 0 {
		return 0
	}
	const max = int64(10 * 365 * 24 * 60 * 60)
	if n > max {
		n = max
	}
	return time.Duration(n) * time.Second
}

// quickSetupPending reports whether the dashboard should offer the first-run
// Quick Setup dialog. The rule lives in settings.QuickSetupHold - the SAME
// predicate that holds monitoring at boot (main.go), so the dialog and the
// paused power button always tell one story. The offer clock is seeded once by
// EnsureQuickSetupOffer; upgrades were materialized as already-answered there,
// so an unseeded clock here simply reads as "no offer".
func (s *Server) quickSetupPending(ctx context.Context) bool {
	return settings.QuickSetupHold(s.settings.QuickSetupDone(),
		s.settings.QuickSetupOfferSince(ctx), time.Now().Unix())
}

// handleQuickSetup applies the whole first-run answer in ONE atomic operation:
// cadence, update check, access scope, optional login, and the answered marker,
// persisting ONLY those keys (the settings-form path would freeze every default,
// which then shadows CLI-seeded and later config). Replaces the client's old
// sequence of partial /api/settings + /api/access + marker POSTs, which froze
// settings, committed opposite choices on a mid-sequence retry, and could leave a
// half-applied setup marked done.
func (s *Server) handleQuickSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Serialize against a restore reconcile, exactly as the settings/access
	// mutations do - a Quick Setup landing mid-import must not race it.
	s.importMu.Lock()
	defer s.importMu.Unlock()

	// Idempotent: once answered, a retry (a lost response after the server
	// committed) is a no-op success, never a second apply over now-real settings.
	if s.settings.QuickSetupDone() {
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	var in struct {
		Dismiss          bool   `json:"dismiss"`
		SpeedtestEnabled *bool  `json:"speedtest_enabled"`
		SpeedSeconds     int    `json:"speed_seconds"`
		UpdateCheck      *bool  `json:"update_check"`
		LocalOnly        *bool  `json:"local_only"`
		AuthEnabled      *bool  `json:"auth_enabled"` // pointer: distinguish omitted from explicit false
		Username         string `json:"username"`
		Password         string `json:"password"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // 415/400 already written
	}

	// Decline: mark answered, change nothing else. Reachable without a session
	// only while NO login is active - an install secured out-of-band (with no
	// daemon auth configured) must still be able to close the dialog. Once a
	// login IS active, the marker is a persistent server-owned write like any
	// other, and this endpoint is authExempt (the guard never checked), so it
	// must carry the same credentials every gated endpoint needs - otherwise
	// any unauthenticated LAN peer could permanently mark setup done and
	// release the boot monitoring hold.
	if in.Dismiss {
		if s.settings.AuthActive() && !s.authed(r) {
			// Same challenge discipline as the guard: offer Basic to CLI clients,
			// never to the SPA (X-Pingularity-UI), which has its own login overlay.
			if r.Header.Get("X-Pingularity-UI") == "" {
				w.Header().Set("WWW-Authenticate", `Basic realm="pingularity"`)
			}
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		if err := s.settings.ApplyQuickSetup(r.Context(), settings.QuickSetupAnswer{Dismiss: true}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
		return
	}

	// The offer window gates the WRITE, not just the dialog. /api/status stops
	// advertising Quick Setup once the 48-hour window closes (quick_setup_pending),
	// and the same expiry releases the boot monitoring hold, so an install left on
	// the default power setting is already probing by then - but this handler used
	// to check only the answered marker, so a browser tab left open past the
	// deadline (or anything else that kept this URL) could still post a whole
	// answer and overwrite the cadence, the access scope, and the login of a
	// running install. Going through
	// quickSetupPending - the SAME helper /api/status reads - is what stops the
	// advertised offer and the endpoint from ever disagreeing again, including on
	// a store read failure, which both sides see as "no offer" and close together.
	// That direction refuses a legitimate first-run answer during a transient
	// failure, which is the right way to fail here: the apply below would not have
	// survived that store anyway.
	//
	// This is a broken contract rather than a privilege escalation. Once a login is
	// active the check just below already refuses these payloads outright (403,
	// with or without a session), and the local-only and DNS-rebind filters run in
	// guard ahead of this endpoint's auth exemption, so they still bound who can
	// reach it. The `dismiss` path above stays DELIBERATELY ungated, and should not
	// be "fixed" to match: it writes only the server-owned answered marker and
	// nothing else (TestQuickSetupDismissTouchesNothingElse pins that), and that
	// marker is the very state an expired window already implies, so refusing it
	// would protect nothing while breaking the one action a stale dialog should
	// still be able to take - closing itself. It carries its own auth gate above.
	//
	// 403 matches the "already configured" refusal below - the request is
	// well-formed and understood, this first-run endpoint just will not serve it
	// anymore. A 409 (this file's code for "no matching speedtest run to stop")
	// would wrongly suggest retrying once some state changes; nothing reopens the
	// window.
	if !s.quickSetupPending(r.Context()) {
		http.Error(w, "Quick Setup is no longer offered; the first-run window has closed - change these under Settings", http.StatusForbidden)
		return
	}

	// A full answer changes access/auth. Once a login is ACTIVE, that is a
	// step-up-protected operation (see handleAccess) - this endpoint has no
	// current-password proof, so refuse rather than let a cookie-only session
	// rotate the password or disable login. Quick Setup is a first-run flow;
	// auth active here means access was configured out-of-band, so send the
	// operator to Settings > Access for any further change. The normal fresh
	// install (auth inactive) is unaffected, and once this endpoint enables
	// auth it also marks done in the same transaction, so a later call no-ops.
	// Checked BEFORE completeness so a re-run on a secured install gets the clear
	// "already configured" message, not a confusing missing-field error.
	if s.settings.AuthActive() {
		http.Error(w, "a login is already configured; change access under Settings > Access", http.StatusForbidden)
		return
	}

	// A full (non-dismiss) answer must be COMPLETE: booleans/ints can't tell an
	// omitted field from an explicit false/zero, so decode the required choices
	// as pointers and reject any that are missing. Otherwise a stray POST {} would
	// mark setup done and silently flip access/update to zero values.
	if in.SpeedtestEnabled == nil || in.UpdateCheck == nil || in.LocalOnly == nil {
		http.Error(w, "incomplete Quick Setup answer: speedtest_enabled, update_check, and local_only are required", http.StatusBadRequest)
		return
	}
	if *in.SpeedtestEnabled && in.SpeedSeconds <= 0 {
		http.Error(w, "speed_seconds is required (and > 0) when speedtests are enabled", http.StatusBadRequest)
		return
	}
	// A login is active iff a password is set. When the client sends auth_enabled
	// explicitly, it MUST agree with that - reject a mismatch in EITHER direction:
	// "enable a login, no password" (would complete setup with an inert login,
	// network-open despite asking for one) and "password with auth explicitly off"
	// (would silently activate a login the operator turned off). An omitted flag
	// just derives from the password. AuthEnabled below is the password truth, so
	// no inconsistent "enabled but no hash" / "hash but disabled" state persists.
	if in.AuthEnabled != nil && *in.AuthEnabled != (in.Password != "") {
		http.Error(w, "auth_enabled must match whether a password is provided", http.StatusBadRequest)
		return
	}

	ans := settings.QuickSetupAnswer{
		SpeedtestEnabled: *in.SpeedtestEnabled,
		SpeedSeconds:     in.SpeedSeconds,
		UpdateCheck:      *in.UpdateCheck,
		LocalOnly:        *in.LocalOnly,
		AuthEnabled:      in.Password != "", // auth is on iff a password was supplied
		AuthUser:         strings.TrimSpace(in.Username),
	}

	// Validate + hash BEFORE any write, so a rejected input never leaves the
	// answer half-applied (the whole point of the atomic endpoint). Only when a
	// password was actually supplied.
	if in.Password != "" {
		if u := ans.AuthUser; u != "" {
			if !utf8.ValidString(u) {
				http.Error(w, "username must be valid UTF-8", http.StatusBadRequest)
				return
			}
			if len(u) > maxUsernameBytes {
				http.Error(w, fmt.Sprintf("username too long: at most %d bytes", maxUsernameBytes), http.StatusBadRequest)
				return
			}
		}
		if len(in.Password) > 72 {
			http.Error(w, "password too long: at most 72 bytes", http.StatusBadRequest)
			return
		}
		hash, err := hashPassword(in.Password)
		if err != nil {
			http.Error(w, "could not hash password", http.StatusInternalServerError)
			return
		}
		ans.AuthHash = hash
	}

	if err := s.settings.ApplyQuickSetup(r.Context(), ans); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// A new hash re-keys the session token, and enabling login would otherwise
	// lock out the very browser that just set it - re-issue the cookie so the
	// operator stays signed in.
	if ans.AuthHash != "" {
		s.setSessionCookie(w, s.secureCookie(r))
		s.rememberGood(s.settings.AuthUser(), in.Password)
	}
	s.log.Info("quick setup applied", "speedtest", ans.SpeedtestEnabled, "local_only", ans.LocalOnly, "auth", s.settings.AuthActive())
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		out := dtoFrom(s.settings.Snapshot())
		out.MinLatencySeconds = int64(settings.MinLatency.Seconds())
		out.MaxLatencySeconds = int64(settings.MaxLatency.Seconds())
		out.MinSpeedSeconds = int64(settings.MinSpeed.Seconds())
		out.MaxSpeedSeconds = int64(settings.MaxSpeed.Seconds())
		out.MinTimeoutSeconds = int64(settings.MinTimeout.Seconds())
		out.MaxTimeoutSeconds = int64(settings.MaxTimeout.Seconds())
		out.MaxStreak = settings.MaxStreak
		out.BusyDeferSupported = netstat.Supported()
		now := time.Now()
		_, off := now.Zone()
		out.ServerNowUnix, out.ServerTZOffsetMin = now.Unix(), off/60
		out.Iperf3Available = speedtest.IperfAvailable()
		out.Iperf3Version = speedtest.IperfVersion()
		out.Containerized = s.InContainer
		out.Bridged = bridgedContainerFn()
		out.CongestionAlgos = speedtest.AvailableCongestionControl()
		def := dtoFrom(s.settings.Defaults())
		out.Defaults = &def
		writeJSON(w, out)
	case http.MethodPost:
		var in settingsDTO
		if err := decodeJSONBody(w, r, &in); err != nil {
			return // response already written (415/400)
		}
		prevExit := s.settings.ExitTarget() // detect an exit-path change to re-trace below
		// Update() is a PATCH: nil (absent) fields keep their current value, so a
		// partial body can't silently reset settings it didn't mention. Fields the
		// form doesn't own - the Monitoring flag (/api/monitoring) and access/auth
		// settings (/api/access) - have no Patch field at all, so a stale form
		// submit can't revert a concurrent password change.
		pat := settings.Patch{
			Latency:              durp(in.LatencySeconds),
			LatencyEnabled:       in.LatencyEnabled,
			DNSProbe:             in.DNSProbe,
			NetinfoEnabled:       in.NetinfoEnabled,
			SpeedServerID:        in.SpeedServerID,
			Speed:                durp(in.SpeedSeconds),
			Retention:            durp(in.RetentionSeconds),
			SpeedRetention:       durp(in.SpeedRetentionSeconds),
			DowntimeRetention:    durp(in.DowntimeRetentionSeconds),
			Timeout:              durp(in.TimeoutSeconds),
			DownAfter:            in.DownAfter,
			UpAfter:              in.UpAfter,
			SpeedtestEnabled:     in.SpeedtestEnabled,
			SpeedtestOnReconnect: in.SpeedtestOnReconnect,
			IPv6Mode:             in.IPv6Mode,
			WebhookFormat:        in.WebhookFormat,
			// NOTE: quick_setup_done is intentionally NOT routed from the settings
			// form into the Patch. It is a server-owned, monotonic first-run marker
			// (written only by boot, /api/quick-setup, and the power-on answer); a
			// generic settings POST must not be able to flip it back to false and
			// reopen Quick Setup (or re-freeze defaults through it). The GET
			// response still reports it for the client's read.
			ExitTarget:          in.ExitTarget,
			SpeedtestAdaptive:   in.SpeedtestAdaptive,
			SpeedtestOnDegraded: in.SpeedtestOnDegraded,
			DegradedPingMS:      in.DegradedPingMS,
			SpeedtestSkipBusy:   in.SpeedtestSkipBusy,
			SpeedBusyMbps:       in.SpeedBusyMbps,
			SpeedEngine:         in.SpeedEngine,
			IperfServer:         in.IperfServer,
			IperfDur:            in.IperfDur,
			IperfStreams:        in.IperfStreams,
			OoklaConnections:    in.OoklaConnections,
			SpeedChallengeEvery: in.SpeedChallengeEvery,
			OoklaLoss:           in.OoklaLoss,
			SpeedDiscardLosers:  in.SpeedDiscardLosers,
			SpeedBestOfCount:    bestOfCountFrom(in.SpeedBestOfCount, in.SpeedBestOf, s.settings.SpeedBestOfCount()),
			IperfOmit:           in.IperfOmit,
			SpeedDirection:      in.SpeedDirection,
			IperfDirection:      in.IperfDirection,
			IperfUDP:            in.IperfUDP,
			IperfUDPRate:        in.IperfUDPRate,
			IperfWindow:         in.IperfWindow,
			SpeedRetries:        in.SpeedRetries,
			IperfRetries:        in.IperfRetries,
			IperfCongestion:     in.IperfCongestion,
			IperfNoDelay:        in.IperfNoDelay,
			IperfDSCP:           in.IperfDSCP,
			IperfMSS:            in.IperfMSS,
			ThreshDownMbps:      in.ThreshDownMbps,
			ThreshUpMbps:        in.ThreshUpMbps,
			ThreshPingMS:        in.ThreshPingMS,
			ThreshJitterMS:      in.ThreshJitterMS,
			ThreshLossPct:       in.ThreshLossPct,
			ThreshConsec:        in.ThreshConsec,
			ThreshBloatDownMS:   in.ThreshBloatDownMS,
			ThreshBloatUpMS:     in.ThreshBloatUpMS,
			AlertOnOutage:       in.AlertOnOutage,
			WebhookURL:          in.WebhookURL,
			HeartbeatURL:        in.HeartbeatURL,
			DigestFreq:          in.DigestFreq,
			SchedLatEnabled:     in.SchedLatEnabled,
			SchedLatWindows:     in.SchedLatWindows,
			SchedSpeedEnabled:   in.SchedSpeedEnabled,
			SchedSpeedWindows:   in.SchedSpeedWindows,
		}
		if in.IperfServers != nil { // nil = keep the saved list (and its passwords)
			pat.IperfServers = iperfServersFromDTO(in.IperfServers) // blank pw kept by Update
		}
		if in.SpeedServers != nil { // nil = keep the saved list; [] = the user unstarred every one
			pat.SpeedServers = in.SpeedServers
		}
		v, err := s.settings.Update(r.Context(), pat)
		if err != nil {
			s.internalError(w, err)
			return
		}
		// Post-change snapshot of the operational settings (mirrors the startup
		// "effective config" line), so "I changed something and it broke" is a
		// greppable before/after - secrets stay out, the form never carries them.
		s.log.Info("settings updated",
			"interval", v.Latency.String(), "down_after", v.DownAfter, "up_after", v.UpAfter,
			"latency", v.LatencyEnabled, "dns_probe", v.DNSProbe, "ipv6_mode", v.IPv6Mode,
			"speedtest", v.SpeedtestEnabled, "speed_engine", v.SpeedEngine, "speed_interval", v.Speed.String(),
			"retention", v.Retention.String())
		// Re-run exit discovery in the background when the exit target changed, so
		// the connection panel reflects the new path on its next poll instead of
		// serving the now-stale cached trace for up to the 10-minute cache window.
		if s.netinfo != nil && v.ExitTarget != prevExit {
			// Detach from the request (a client disconnect must not cancel the
			// re-trace) but derive from the run context so a shutdown does, and
			// track it on serveWG so Serve drains it before the store closes.
			base := s.serveCtx
			if base == nil {
				base = context.Background() // not serving (tests): plain detached ctx
			}
			s.serveWG.Add(1)
			go func() {
				defer s.serveWG.Done()
				ctx, cancel := context.WithTimeout(base, 25*time.Second)
				defer cancel()
				s.netinfo.RefreshNow(ctx)
			}()
		}
		out := dtoFrom(v)
		out.BusyDeferSupported = netstat.Supported() // report on save too, so applySettings doesn't wrongly grey the toggle
		out.Iperf3Available = speedtest.IperfAvailable()
		out.Iperf3Version = speedtest.IperfVersion()
		out.Containerized = s.InContainer
		out.Bridged = bridgedContainerFn()                           // on save too, or applySettings would clear the loopback trap after every save
		out.CongestionAlgos = speedtest.AvailableCongestionControl() // on save too, or the dropdown loses the host's algos
		// Echo the server clock too (as GET does), or applySettings nulls the
		// schedule "now" skew and the coverage marker falls back to browser time.
		now := time.Now()
		_, off := now.Zone()
		out.ServerNowUnix, out.ServerTZOffsetMin = now.Unix(), off/60
		writeJSON(w, out)
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
	}
}

// handleDataDelete clears all rows of a data kind (POST {type: latency|speed|downtime}).
func (s *Server) handleDataDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Type string `json:"type"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	// Validate the kind here so only the client's bad input gets a 400; a
	// failed DELETE below is a server/DB fault and reports 500 like every
	// other store error in this file.
	switch in.Type {
	case "latency", "speed", "downtime":
	default:
		http.Error(w, fmt.Sprintf("unknown data kind %q", in.Type), http.StatusBadRequest)
		return
	}
	n, err := s.store.Clear(r.Context(), in.Type)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n > 0 && in.Type != "latency" { // downtime feeds the uptime pill, speed the data pill
		s.invalidateAggregates()
	}
	s.log.Info("data deleted", "type", in.Type, "rows", n)
	writeJSON(w, map[string]any{"deleted": n})
}

// handleMonitoring is the power button: GET reports state, POST {enabled:bool}
// starts/stops probing.
func (s *Server) handleMonitoring(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSONBody(w, r, &in); err != nil {
			return // response already written (415/400)
		}
		// An explicit power-ON is an answer to the first-run offer: without it the
		// boot hold would keep monitoring off while the button optimistically flips
		// on, then snaps back. Set the power state AND answer the offer in ONE
		// transaction - splitting them (and suppressing the marker error, as an
		// earlier version did) could report monitoring enabled while the hold still
		// blocked probes because the marker never landed.
		markedDone, err := s.settings.SetMonitoringAnsweringSetup(r.Context(), in.Enabled)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.log.Info("monitoring toggled", "enabled", in.Enabled, "answered_setup", markedDone)
	}
	writeJSON(w, map[string]bool{"enabled": s.settings.Monitoring()})
}

// dataCategories maps each export/import category to its envelope key and
// table, in a fixed order. config = settings (clean override on import); the
// rest are time-series tables that merge by key on import. The latency
// category spans two tables - the ping samples and the DNS-resolve series -
// bundled the same way Clear and Prune treat them.
// Order matters for EXPORT (import applies top-level keys in file order): config is
// written LAST so a restore applies settings only after every data category has
// landed. A data failure mid-restore then never reaches config, leaving the
// destination's settings untouched rather than half-changed (audit: partial import).
var dataCategories = []struct{ cat, key, table string }{
	{"latency", "latency", "samples"},
	{"latency", "dns", "dns"},
	{"speed", "speed", "speed"},
	// The speed category spans two tables the way latency and downtime do:
	// speed holds the runs, speed_servers holds each run's server-selection
	// report (who was considered, measured, scored - the run's explanation).
	// Written after "speed" so a restore lands the runs before their
	// explanations; both merge by key, so order only matters for config
	// staying last.
	{"speed", "speed_servers", "speed_servers"},
	{"downtime", "downtime", "events"},
	// The downtime category spans two tables, the way latency spans samples + dns:
	// the outage transitions AND the pause spans that say which wall seconds were
	// observed at all. pauses is the uptime DENOMINATOR (store.pausedOverlap feeds
	// UptimeSince, DowntimeByDay's prorate and orphanGapDowntime), so a backup
	// carrying events without it restores a history where every unobserved second
	// becomes observed-and-up: the restored box reads a different uptime% than the
	// one it was copied from, and publishes uptime for windows the source correctly
	// omitted. Written after "downtime" so a restore lands the events first; both
	// merge by ts, so order only matters for config staying last.
	{"downtime", "pauses", "pauses"},
	// The held-aside half of the pause record (see the store's exportTables
	// comment): exported so a backup taken while genuine history sits in
	// quarantine does not silently lose it. Written after pauses; restored
	// rows land back in the destination's quarantine for ITS re-judgement.
	{"downtime", "pauses_quarantine", "pauses_quarantine"},
	{"config", "config", "settings"},
}

// importMidHook is called by handleImport just after it snapshots the pre-import
// login state. Test seam only (nil in production): the interesting window for a
// concurrent credential change is between that snapshot and the post-reload
// repair, and it is otherwise reachable only by racing.
var importMidHook func()

// importReconcileBudget bounds the post-commit reconcile. It runs detached from
// the client, so it needs its own ceiling: long enough for a handful of settings
// writes, short enough that a wedged write cannot hold importMu indefinitely.
// A var only so tests can exhaust the budget deliberately.
var importReconcileBudget = 30 * time.Second

// importRestoreBudget bounds the last-resort restore of the pre-import
// auth/access keys after the reload has failed for good. Deliberately NOT the
// shared reconcile budget: exhausting that budget is one of the ways the
// reload fails in the first place, and the restore is what stands between that
// failure and a restart silently adopting the backup's login settings.
const importRestoreBudget = 10 * time.Second

// importReconcileHook is called once the imported settings are live but before the
// safety repair has run - the window in which the destination is running on
// whatever the backup said. Test seam only; nil in production.
var importReconcileHook func()

// exportSchema is the newest envelope version this build writes or accepts. It
// is a COMPATIBILITY CONTRACT, not a build stamp: bump it whenever an export
// starts carrying data an older build would silently drop. Each file is stamped
// with the version its own content needs (exportSchemaFor), not this constant:
// a file without any v2-only key is v1-shaped in every byte, and stamping it
// higher would only make older builds refuse a backup they fully understand.
//
// It went to 2 when the downtime category gained `pauses`. Pause rows are the
// uptime denominator, so a build that does not know the key skips it, restores
// the outage events alone, and reports success - leaving a history where every
// unobserved second reads as observed-and-up. The restored box then publishes a
// different uptime% than the one it was copied from, and publishes one for
// windows the source correctly refused to. v0.54.0 accepts 1 and rejects anything
// higher, so stamping 2 is exactly what converts that silent downgrade into its
// "update before restoring it" error.
//
// It went to 3 when the speed category gained `speed_servers` - each run's
// server-selection report. Not load-bearing the way pauses is (dropping it
// skews no number), but the table exists to keep runs explainable after the
// fact, and a restore that silently sheds the explanations while keeping the
// runs defeats that on the quiet. Per the contract above, new data an older
// build would silently drop bumps the version; files without the key still
// stamp 2 or 1 and restore everywhere.
//
// It went to 4 when the downtime category gained `pauses_quarantine` - the
// held-aside half of the pause record. Genuine history sits there between a
// wrong stale-clock judgement and its re-judgement, so a build that does not
// know the key restores the observation spans without their held remainder and
// reports success - the silent downgrade this contract exists to prevent. The
// key is content-dependent (handleExport omits it when the table is empty, the
// overwhelmingly common case), so ordinary backups still stamp 2 or 3 and keep
// restoring on the builds that predate it; only a file actually CARRYING held
// rows demands 4.
//
// It went to 5 when the speed table gained columns: `failed` (the marker that
// separates a usage-accounting row - a run that failed partway, whose bytes
// count toward data usage though it measured nothing - from a real measurement)
// and the descriptive pair `ip_family` / `udp_direction`. These are COLUMNS
// inside a category older builds already carry, not new keys, so the category
// mapping below cannot see them.
//
// AN OLDER BUILD DOES NOT DROP A COLUMN IT DOES NOT KNOW. It is tempting to
// reason that it would, and to bump only for the columns whose absence would
// change what the restored rows mean - `failed` would, the descriptive pair
// would not. But ImportTableBatch, unchanged since the first commit and so
// present in every shipped build, aborts the category with `unknown column
// (backup from a newer version?)`. And the abort lands MID-RESTORE: categories
// apply in file order and config is last, so latency has already been committed
// when speed is refused. The user is left with a half-restored database on the
// documented roll-back path - which is worse than either a clean refusal or a
// silently shed column.
//
// So the rule is not "which columns change the meaning" but the blunter "does
// the file carry any column those builds have never heard of". Content-dependent
// like `pauses_quarantine`, and enforced on the bytes rather than asserted:
// handleExport asks which of them any row actually uses, DROPS the unused ones
// from the rows it streams, and stamps 5 only if something is left. An install
// that has never recorded one keeps stamping 1-4 and keeps restoring on older
// builds; one that has stamps 5, and those builds refuse it at the envelope
// check, before a single category commits.
//
// minExportSchema is the oldest we still read: bumping the writer must not orphan
// the backups already on disk.
//
// It went to 6 when the speed table gained the race verdict columns (race_*,
// see store.SpeedSample.RaceOutcome): the same shape as 5, columns rather
// than keys, decided the same content-dependent way - a file carries them
// only when some row uses them, and stamps 6 only then. The builds that read
// 5 abort on them exactly as the builds that read 4 abort on `failed`.
// store.SpeedColumnSchema is where each column's version lives, so a column
// added later is one map entry rather than one more paragraph here.
//
// It went to 7 when the speed table gained round_ts (a Best-of round's kept
// losers, see store.SpeedSample.RoundTS): one more column on the same
// content-dependent rule, its version one entry in store.SpeedColumnSchema.
const (
	exportSchema    = 7
	minExportSchema = 1
)

// exportSchemaFor is the version an export carrying exactly these category keys
// needs: the OLDEST schema whose readers keep every one of them. v0.54.0
// rejects anything above 1 before reading a byte, so stamping the file's own
// requirement rather than exportSchema is what keeps a pauses-less backup
// restorable on the builds that predate pauses (the roll-back path).
// speedCols says which speed columns added after schema 4 the rows being
// exported still carry (true = in use), which no category key can express (see
// exportSchema's v5 and v6 paragraphs). The caller has already dropped the ones
// no row uses, so this is about the bytes actually being written, not the
// table shape; each column's own version comes from store.SpeedColumnSchema.
func exportSchemaFor(keys []string, speedCols map[string]bool) int {
	v := 1
	for _, k := range keys {
		switch k {
		case "pauses_quarantine": // the key the schema went to 4 for (see exportSchema)
			v = max(v, 4)
		case "speed_servers": // ... to 3 for
			v = max(v, 3)
		case "pauses": // ... and to 2 for
			v = max(v, 2)
		}
	}
	for c, inUse := range speedCols {
		if inUse {
			v = max(v, store.SpeedColumnSchema(c)) // ... 5 and 6 for the speed columns, which are columns rather than keys
		}
	}
	return v
}

// handleExport streams selected categories as a downloadable JSON file.
// GET /api/export?config=1&latency=1&speed=1&downtime=1
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	var sel []struct{ cat, key, table string }
	for _, dc := range dataCategories {
		if r.URL.Query().Get(dc.cat) != "" {
			sel = append(sel, dc)
		}
	}
	if len(sel) == 0 {
		http.Error(w, "select at least one category", http.StatusBadRequest)
		return
	}
	// Cap concurrent exports so a stalled client can't pin a read cursor across the
	// whole stream (see exportGate); refuse rather than queue when the gate is full.
	gate := s.exportGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		http.Error(w, "another export is already in progress; try again shortly", http.StatusTooManyRequests)
		return
	}
	// One read snapshot for the whole export, so every category is a consistent view
	// of the same instant (no cross-category skew, no rows newer than exported_at).
	// Opened BEFORE any header/byte so a failure is a clean 500, not a mid-stream abort.
	snap, terr := s.store.BeginReadSnapshot(r.Context())
	if terr != nil {
		s.log.Error("export snapshot begin", "err", terr)
		http.Error(w, "could not open a consistent snapshot", http.StatusInternalServerError)
		return
	}
	defer snap.Rollback()
	// The held-pause key is CONTENT-dependent: an empty pauses_quarantine
	// carries nothing an older build could drop, and including it would stamp
	// the file schema 4 (exportSchemaFor) and lock pre-quarantine builds out of
	// a backup they fully understand - the inverse of the mistake the schema
	// contract guards. Checked against the same snapshot the rows stream from.
	// If the check itself fails, KEEP the key: the failure mode must be "an
	// older build refuses the file", never "held history silently shed".
	for i, dc := range sel {
		if dc.key != "pauses_quarantine" {
			continue
		}
		if has, herr := s.store.HasQuarantinedPausesTx(r.Context(), snap); herr == nil && !has {
			sel = append(sel[:i], sel[i+1:]...)
		}
		break
	}
	// The speed category needs the same content check, for columns rather than a
	// key (see store.SpeedColumnsPastSchema4InUse). A column older builds do not
	// know is not skipped by them: their import aborts the category, partway
	// through a restore that has already committed latency. So the file must
	// either carry none of those columns - and keep stamping the version its
	// content actually needs - or stamp 5, where the envelope check refuses it
	// before a single category commits.
	//
	// On a check error assume every one of them is in use: "an older build
	// refuses the backup" is the safe failure, "a column is silently shed" is not.
	newSpeedCols := map[string]bool{}
	for _, dc := range sel {
		if dc.table != "speed" {
			continue
		}
		inUse, cerr := s.store.SpeedColumnsPastSchema4InUse(r.Context(), snap)
		if cerr != nil {
			s.log.Warn("export: could not tell which new speed columns are in use; stamping for all of them", "err", cerr)
			inUse = store.AllSpeedColumnsPastSchema4InUse()
		}
		newSpeedCols = inUse
		break
	}
	// Progress-refreshed write deadline: rearm as rows flush, so a client that stops
	// reading is reaped within one window instead of holding the cursor for minutes.
	bump := exportDeadlineBumper(w, s.log)
	bump()
	// Stream the envelope row-by-row so a huge table (millions of samples) exports
	// at O(1) memory instead of buffering every row into a map first. Once bytes
	// are flushed the status can't change, so a mid-stream error ABORTS the
	// connection instead (see below): the browser/curl reports a failed download,
	// rather than saving a clean-looking truncated backup whose corruption would
	// only surface at restore time.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="pingularity-export-`+time.Now().Format("20060102-150405")+`.json"`)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	enc := json.NewEncoder(bw)
	// Manifest fields in the envelope: schema version (pingularity_export), the
	// producing build, the snapshot time, and the categories included - so a restore
	// (or a human) has provenance for the file.
	cats := make([]string, 0, len(sel))
	for _, dc := range sel {
		cats = append(cats, dc.key)
	}
	catsJSON, _ := json.Marshal(cats)
	fmt.Fprintf(bw, `{"pingularity_export":%d,"producer_version":%q,"exported_at":%d,"categories":%s`,
		exportSchemaFor(cats, newSpeedCols), s.version, time.Now().Unix(), catsJSON)
	for _, dc := range sel {
		fmt.Fprintf(bw, ",%q:[", dc.key)
		first := true
		err := s.store.ExportTableRowsTx(r.Context(), snap, dc.table, func(m map[string]any) error {
			// Drop the post-schema-4 columns no row uses, so the low stamp this
			// file is about to carry is TRUE: an older build would abort the
			// whole category on any one of them, not skip it. A column that IS
			// in use stays, and exportSchemaFor has already pushed the stamp
			// out of that build's range - the two decisions read the same map.
			if dc.table == "speed" {
				for _, c := range store.SpeedColumnsPastSchema4() {
					if !newSpeedCols[c] {
						delete(m, c)
					}
				}
			}
			if !first {
				bw.WriteByte(',')
			}
			first = false
			if err := enc.Encode(m); err != nil { // row object + newline (valid JSON whitespace inside the array)
				return err
			}
			// Flush and rearm the deadline periodically so back-pressure from a slow
			// client bounds the buffer and the deadline only advances while bytes move.
			if bw.Buffered() >= 32<<10 {
				if err := bw.Flush(); err != nil {
					return err
				}
				bump()
			}
			return nil
		})
		bw.WriteByte(']')
		if err != nil {
			s.log.Error("export stream failed", "table", dc.table, "err", err)
			// Kill the connection so the client sees a failed transfer, not a
			// complete-looking 200 of invalid JSON. guard() re-panics this
			// sentinel, and net/http aborts without a terminating chunk.
			panic(http.ErrAbortHandler)
		}
	}
	bw.WriteByte('}')
}

// maxConcurrentImports caps how many /api/import runs execute at once. It is 1:
// a restore must not run beside another restore.
//
// It was 4, and the extra slots could not make two restores write at once:
// SQLite has a single writer (store.Open's pool comment), so importers serialize
// on it whatever this constant allows. So there is no throughput for a second
// slot to unlock: it only chooses whether the second restore waits outside the
// gate or races inside it.
//
// Racing has a losing side. An import applies its rows in bounded batches, one
// store transaction apiece (batchRows in handleImport = store.importTxRows), and
// importTxRows exists precisely because a restore that held the writer for a whole
// file would push other writers past their 5s busy_timeout (store's pragmaConn
// sets it) - importTxRows' own comment says exactly that of the live monitor's
// inserts, and a second restore's batches queue for the same writer on the same
// timeout. A batch that waits it out fails with SQLITE_BUSY, and because a restore
// is deliberately incremental rather than atomic (see the partial branch in
// handleImport), that failure does not leave the destination untouched: the caller
// gets HTTP 500 {"partial":true} naming the rows that did land - a half-finished
// restore of an intact backup.
//
// No failure rate is quoted here on purpose, and no distribution either. Which
// batch has to wait, and whether it waits out the full five seconds, is decided
// by SQLite's busy handler against timing this code does not control, so a figure
// copied from one sitting would only tell the next reader their build is fine
// when it is not.
//
// The slots also cost outright: each admitted importer decodes into a batch of
// its own (bounded by importBatchBytes), so peak memory tracks the slot count
// instead of staying at one batch, and four importers can hold all four of the
// store's pooled connections (store.Open caps a file-backed pool at 4).
//
// One at a time is also what the UI has always assumed - it sends ONE request
// covering every selected category and disables the button until it answers - so
// the callers this now refuses are a second tab, a second operator, or a retry
// after a proxy hangup.
const maxConcurrentImports = 1

// importGate returns the (lazily built) import concurrency semaphore.
func (s *Server) importGate() chan struct{} {
	s.importSemOnce.Do(func() { s.importSem = make(chan struct{}, maxConcurrentImports) })
	return s.importSem
}

// maxConcurrentExports caps how many streaming exports (/api/export +
// /api/speed/runs.csv) run at once. Each holds a SQLite read cursor for the whole
// stream; the pool is small (4 connections), so a few slow clients could otherwise
// pin every connection and starve the probe writer. Two leaves headroom.
const maxConcurrentExports = 2

// exportWriteWindow is how long a streaming export may stall between flushed
// chunks before its connection is reaped (the progress-refreshed write deadline).
const exportWriteWindow = 60 * time.Second

// exportGate returns the (lazily built) export concurrency semaphore.
func (s *Server) exportGate() chan struct{} {
	s.exportSemOnce.Do(func() { s.exportSem = make(chan struct{}, maxConcurrentExports) })
	return s.exportSem
}

// exportDeadlineBumper returns a func that rearms the response write deadline to
// exportWriteWindow from now, called as each chunk flushes so a client that stops
// reading is reaped within one window rather than pinning the DB cursor. A writer
// that doesn't support deadlines (some test recorders) makes it a logged no-op.
func exportDeadlineBumper(w http.ResponseWriter, log *slog.Logger) func() {
	rc := http.NewResponseController(w)
	return func() {
		if err := rc.SetWriteDeadline(time.Now().Add(exportWriteWindow)); err != nil {
			log.Debug("export write-deadline extension failed", "err", err)
		}
	}
}

// handleImport merges an uploaded export into the selected categories.
// POST /api/import?config=1&...   body = the export JSON. Time-series data is
// merged (existing rows kept); config overrides the settings and reloads live.
// The body is decoded as a STREAM in bounded batches, mirroring the export
// side: a default-retention backup runs to hundreds of MB of latency samples,
// so buffering the whole file (the old 64 MiB-capped approach) meant the import
// rejected the product's own exports. Peak memory stays bounded to one batch
// (capped by both row count and accumulated bytes), not by file size.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	// One restore at a time (maxConcurrentImports), and REFUSE rather than queue
	// when the slot is taken. Queueing would fix the partial restores too, but it
	// parks this request's goroutine, connection and FD for the whole of the
	// running restore with nothing to bound the wait: a blocking send on the gate
	// returns when the running restore returns it, whenever that is, and no server
	// timeout cuts a handler blocked before it starts reading the body.
	// That is the pile-up this gate exists to prevent. Refusing also answers the
	// likelier question honestly - a second concurrent restore is nearly always a
	// double-click, a second tab or a retry, and silently running a second
	// whole-database restore after the first is worse than declining it.
	gate := s.importGate()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	default:
		http.Error(w, "another import is already in progress; try again shortly", http.StatusTooManyRequests)
		return
	}
	// The most destructive endpoint (INSERT OR REPLACE settings + live Reload),
	// so it needs the same content-type CSRF guard the other mutating routes get
	// via decodeJSONBody - which this handler bypasses to stream a large body.
	if !requireJSONBody(w, r) {
		return
	}
	// A restore can be large and arrive over a slow link, so the server-wide
	// ReadTimeout is too tight. Grant a per-progress window instead of a flat
	// budget: rearm the deadline each time a batch decodes, so a legitimate import
	// runs as long as data keeps flowing, but a stalled/dribbling body (slowloris)
	// is reaped within one window rather than pinning the connection for minutes.
	resetDeadline := func() {
		if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(importReadWindow)); err != nil {
			s.log.Debug("import read-deadline extension failed", "err", err)
		}
	}
	resetDeadline()
	// No whole-body ceiling - the product's own exports outgrow any workable one
	// (see importReadBurst). What IS bounded is consumption per decode step: body
	// reads go through a renewable allowance, so any SINGLE element (which the
	// decoder materializes whole before a size check can see it) stays well below
	// process memory while total file size stays unbounded.
	body := &burstReader{r: r.Body, remaining: importReadBurst}
	// progress marks real decode progress: it rearms the read deadline AND renews
	// the byte allowance in one place, so the two bounds cannot drift apart.
	progress := func() { resetDeadline(); body.renew() }

	// A leading UTF-8 BOM (0xEF 0xBB 0xBF) is legal at the start of a UTF-8 file and
	// some exporters/editors prepend one, but json.Decoder.Token rejects it, turning
	// a valid export into a misleading "not a Pingularity export file" 400. Strip
	// exactly one BOM through the existing metered reader WITHOUT buffering the whole
	// body: a small bufio.Reader peeks the first three bytes and discards them only
	// when they are the BOM.
	br := bufio.NewReader(body)
	if pre, _ := br.Peek(3); len(pre) == 3 && pre[0] == 0xEF && pre[1] == 0xBB && pre[2] == 0xBF {
		_, _ = br.Discard(3)
	}

	// Envelope check. Our exporter always writes {"pingularity_export":N,...}
	// first, so the version is known before any data row is applied - streaming
	// means rows land as they parse, so this can't wait for the whole file.
	dec := json.NewDecoder(br)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		http.Error(w, "not a Pingularity export file", http.StatusBadRequest)
		return
	}
	if key, err := dec.Token(); err != nil || key != "pingularity_export" {
		http.Error(w, "not a Pingularity export file", http.StatusBadRequest)
		return
	}
	var ver int
	if err := dec.Decode(&ver); err != nil || ver < minExportSchema {
		http.Error(w, "unrecognized export file version", http.StatusBadRequest)
		return
	}
	// Older files still restore: an earlier schema is a SUBSET of this one (v1
	// simply had no pauses key), so nothing is misread - the restore just carries
	// less. Newer ones cannot, for the reason exportSchema exists.
	if ver > exportSchema {
		http.Error(w, "this backup is from a newer Pingularity version; update before restoring it", http.StatusBadRequest)
		return
	}

	keyToCat := map[string]struct{ cat, table string }{}
	for _, dc := range dataCategories {
		keyToCat[dc.key] = struct{ cat, table string }{dc.cat, dc.table}
	}
	result := map[string]int{}
	// Oldest row timestamp seen per category (unix seconds), for the
	// about-to-be-pruned warning below. The import RECORDS timestamps and decides
	// nothing: the window that will actually prune these rows is whatever
	// retention is live AFTER this restore, and a backup can carry its own
	// retention settings, which only go live at the reload further down. Comparing
	// here - against the destination's pre-import windows - got both directions
	// wrong, the silent one badly: a keep-forever destination importing a backup
	// with a short retention plus older rows had nothing to compare against (window
	// 0 = no warning ever), reported a clean restore, activated the backup's short
	// window, and lost the restored history at the next hourly prune.
	// Every table of a category feeds one entry (latency = samples + dns, speed =
	// speed + speed_servers, downtime = events + pauses + pauses_quarantine), so
	// the value is the MINIMUM across the category - that is the row the window
	// prunes first, and it is what keeps pauses/pauses_quarantine covered by the
	// downtime horizon they are pruned on.
	// Key ABSENT = no timestamped row was seen at all, which is not the same as a
	// row stamped 0 (an epoch ts is a real, very prunable timestamp) and must
	// produce no warning: speed_servers keys its time by run_ts and contributes
	// none, so a speed category carrying only selection reports has nothing to say.
	// A restore replaces the rows this daemon may have watched being created, so
	// any pending birth stamp stops describing what is in the store. Dropped
	// BEFORE the first row lands, not after: the stamp can be completed by a
	// settings write, and an import performs several.
	s.settings.ForgetBirthWitness()
	oldestTS := map[string]int64{}
	importedConfig := false
	// The login state BEFORE any config lands. A backup carries the login settings
	// but never the password hash (settingsExportDeny), so applying it verbatim can
	// leave this box in a state its own credentials do not fit - and the endpoint
	// that would fix that is itself behind the login. Captured here, repaired after
	// the reload below.
	var preAuthActive, preAuthEnabled, preHasPassword, preLocalOnly bool
	var preAuthUser string
	// Test seam: lets a test place a concurrent credential write exactly inside the
	// window between this snapshot and the repair below. Nil in production.
	if importMidHook != nil {
		importMidHook()
	}
	// Each category is committed as it is reached. Our own exports write config
	// LAST (see dataCategories) precisely so a data failure mid-restore never
	// touches the destination's settings. A later failure can't roll the earlier
	// ones back, so record it and fall through to the reload/invalidate below
	// instead of returning early - otherwise applied config would sit latent in
	// the DB and silently activate only on the next restart.
	var importErr error
	importStatus := http.StatusInternalServerError
	for importErr == nil && dec.More() {
		progress() // progress made: rearm before the next (possibly large) value
		keyTok, err := dec.Token()
		if err != nil {
			importErr, importStatus = fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest
			break
		}
		key, _ := keyTok.(string)
		dc, known := keyToCat[key]
		if !known || r.URL.Query().Get(dc.cat) == "" {
			// exported_at, an unselected category, or an unknown key: walk past its
			// value without materializing it. Every token renews the byte allowance
			// (the category as a whole may be arbitrarily large; only a single token
			// must stay bounded) and every few thousand tokens rearms the read
			// deadline, so a big skipped category on a slow link is not reaped
			// mid-walk while a stalled body still is.
			skippedToks := 0
			if err := skipJSONValue(dec, func() {
				body.renew()
				if skippedToks++; skippedToks%4096 == 0 {
					resetDeadline()
				}
			}); err != nil {
				// An element outgrowing the allowance is a size problem, and saying
				// "invalid JSON" about it sends the operator debugging the wrong thing.
				var tooBig *importElementTooLargeError
				if errors.As(err, &tooBig) {
					importErr, importStatus = err, http.StatusRequestEntityTooLarge
				} else {
					importErr, importStatus = fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest
				}
			}
			continue
		}
		n, minTS, sawTS, err := s.importArray(r.Context(), dec, key, dc.table, progress)
		result[dc.cat] += n // latency spans two tables (samples + dns); sum them
		if sawTS {
			if cur, ok := oldestTS[dc.cat]; !ok || minTS < cur {
				oldestTS[dc.cat] = minTS // oldest across every table of the category
			}
		}
		if dc.cat == "config" {
			importedConfig = true
		}
		if err != nil {
			var tooBig *importElementTooLargeError
			switch {
			case errors.As(err, &tooBig):
				// Before the "bad " prefix case: importArray wraps the reader's error
				// as "bad <key> data: ...", and a size trip must stay a 413.
				importErr, importStatus = err, http.StatusRequestEntityTooLarge
			case strings.Contains(err.Error(), "unknown column"):
				// Rows carrying columns this binary doesn't know come from a newer
				// version's backup - the caller's file, not a server fault.
				importErr, importStatus = err, http.StatusBadRequest
			case strings.HasPrefix(err.Error(), "bad "):
				importErr, importStatus = err, http.StatusBadRequest
			default:
				importErr, importStatus = err, http.StatusInternalServerError
			}
		}
	}
	// The category loop stops as soon as dec.More() reports no next element - which
	// is ALSO what a truncated body looks like (EOF before the closing '}'). Require
	// the exact closing token and then EOF, so a backup cut off partway (config
	// applied, later data silently missing) or one with trailing junk is a 400, not
	// a false "Imported". importArray already consumes each inner ']'.
	if importErr == nil {
		if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
			importErr, importStatus = errors.New("truncated or malformed export file"), http.StatusBadRequest
		} else if _, err := dec.Token(); !errors.Is(err, io.EOF) {
			importErr, importStatus = errors.New("unexpected trailing data after the export object"), http.StatusBadRequest
		}
	}

	var warnings []string
	// Imported config went straight to the DB; reload so the monitor/scheduler
	// pick it up without a restart. Run this even when a later category failed,
	// so the applied config takes effect immediately (and is logged) rather than
	// lurking until the next restart.
	if importedConfig {
		// Everything from here is RECONCILE, and it must finish. It runs on a
		// context detached from the client's: once a category has committed, hanging
		// up mid-restore used to leave the live controller and the stored settings
		// disagreeing, with a restart quietly adopting the backup's values. Bounded,
		// so a wedged write cannot pin the lock.
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(r.Context()), importReconcileBudget)
		defer rcancel()

		// One writer at a time. Holding this across the whole reconcile is what lets
		// the repair below KNOW that a username it did not expect came from the
		// backup: nothing else can change credentials in between.
		s.importMu.Lock()
		defer s.importMu.Unlock()

		// Snapshot here, not at parse time. The imported rows are already in the
		// database but not yet in the live controller, so these still describe the
		// destination's own settings - and taking them under the lock means they
		// cannot go stale before the repair reads them.
		preAuthActive = s.settings.AuthActive()
		preAuthEnabled = s.settings.AuthEnabled()
		preHasPassword = s.settings.HasPassword()
		preAuthUser = s.settings.AuthUser()
		preLocalOnly = s.settings.AccessLocalOnly()

		// From the reload until the repair finishes, the box is running on whatever
		// the backup said. Refuse remote requests for that window rather than
		// publishing an unprotected configuration and repairing it afterwards.
		s.reconciling.Store(true)
		defer s.reconciling.Store(false)

		rerr := s.settings.Reload(rctx)
		if rerr != nil {
			s.log.Warn("settings reload after import", "err", rerr)
		}
		// Test seam: fires INSIDE the reconcile window - after the imported settings
		// are live (or the reload has just failed), before the retry and the safety
		// repair below have run. Nil in production.
		if importReconcileHook != nil {
			importReconcileHook()
		}
		// The repairs below compare live values against the snapshot above, so they
		// only work once the reload has made the imported settings live. A reload
		// that failed BEFORE the broadcast (ErrLegacyReseal fails after it, so the
		// config is live regardless) leaves the live cache at the snapshot: every
		// repair condition reads "nothing changed", nothing is appended, and the
		// response is a clean success - while the backup's auth/access keys sit
		// committed in the DB, waiting for the next restart to pair the backup's
		// username with this machine's password hash (which never rides in a
		// backup). That silent deferred lockout is the exact state the repairs
		// exist to prevent, so a failed reload must end either with the imported
		// settings live or with the pre-import auth/access keys back in the store -
		// never in between, and never quietly. backupLive gates the repairs: when
		// the backup never went live there is nothing to repair, and the blanket
		// restore below has already put the pre-import keys back.
		backupLive := true
		if !settings.LoadedOK(rerr) {
			if rerr = s.settings.Reload(rctx); !settings.LoadedOK(rerr) {
				backupLive = false
				// rctx may be the very thing that failed (the shared budget can be
				// exhausted before the reload even runs), so the last-resort restore
				// gets its own small detached budget: skipping it BECAUSE the budget
				// ran out would be the one outcome this block exists to prevent.
				sctx, scancel := context.WithTimeout(context.WithoutCancel(rctx), importRestoreBudget)
				defer scancel()
				var restoreErrs []error
				if preAuthUser != "" { // "" = never configured; SetAuthUser would invent "admin"
					restoreErrs = append(restoreErrs, s.settings.SetAuthUser(sctx, preAuthUser))
				}
				restoreErrs = append(restoreErrs,
					s.settings.SetAuthEnabled(sctx, preAuthEnabled),
					s.settings.SetAccessLocalOnly(sctx, preLocalOnly))
				if restoreErr := errors.Join(restoreErrs...); restoreErr != nil {
					// Neither safe state was reachable: the backup's login settings are
					// in the DB and could not be replaced. Fail LOUDLY - a clean success
					// here is a lockout deferred to whenever the box next restarts.
					s.log.Error("post-import reload failed and restoring the pre-import login settings failed too",
						"reload_err", rerr, "restore_err", restoreErr)
					warnings = append(warnings, "The imported settings were saved but could not be loaded into "+
						"the running app ("+rerr.Error()+"), and putting your own login and network-access "+
						"settings back failed too ("+restoreErr.Error()+"). A restart would adopt the backup's "+
						"login settings - re-save yours in the Access tab before restarting.")
					if importErr == nil {
						importErr = errors.New("imported settings could not be reloaded, and the pre-import login settings could not be restored")
						importStatus = http.StatusInternalServerError
					}
				} else {
					s.log.Error("post-import reload failed; kept the pre-import login and access settings", "err", rerr)
					if err := s.settings.Reload(sctx); !settings.LoadedOK(err) {
						// Still not live, but now safe: the stored auth/access keys are
						// the pre-import ones, so a restart changes nothing the operator
						// owns.
						warnings = append(warnings, "The imported settings were saved but could not be loaded "+
							"into the running app ("+rerr.Error()+"). Your login and network-access settings were "+
							"kept; the rest of the backup's configuration takes effect at the next restart.")
					} else {
						warnings = append(warnings, "The imported settings are active, except the backup's login "+
							"and network-access settings: loading them cleanly failed ("+rerr.Error()+"), so yours "+
							"were kept. Review the Access tab if you meant to change them.")
						if err != nil {
							// ErrLegacyReseal, same as the ladder's earlier reloads treat it:
							// the reload succeeded past the broadcast, so the config above IS
							// live - only re-encrypting the backup's clear-text iperf3
							// passwords failed, which leaves them on disk unencrypted. That
							// consequence is the warning, not a bogus "takes effect at the
							// next restart".
							s.log.Warn("legacy iperf3 password re-encryption failed during the post-rollback reload; settings loaded", "err", err)
							warnings = append(warnings, "The backup's iperf3 password(s) could not be re-encrypted "+
								"("+err.Error()+"), so they are stored unencrypted for now. They will be re-encrypted "+
								"by the next successful settings save or restart.")
						}
					}
				}
			}
		}
		// A backup's username must not rename an account whose PASSWORD this box
		// still owns. The hash never rides in a backup, so applying a foreign
		// auth_user leaves a username and a hash that were never a pair: the
		// operator's credentials stop working, their session dies with them (tokens
		// are bound to the username), and POST /api/access - the only way to set new
		// ones - is behind the very login that no longer accepts them. Restoring onto
		// a box with no password set is untouched: there is nothing to lock out of.
		// preHasPassword, NOT preAuthActive. What makes a rename dangerous is owning a
		// PASSWORD the backup does not carry; whether login happened to be switched on
		// at that moment is beside the point. Keying on AuthActive (enabled AND a hash)
		// left a real state unprotected: a box with a password stored but login turned
		// off got renamed and re-enabled in one restore, pairing this machine's
		// password with a name nobody here chose.
		if backupLive && preHasPassword && s.settings.AuthUser() != preAuthUser {
			imported := s.settings.AuthUser()
			switch {
			default:
				if err := s.settings.SetAuthUser(rctx, preAuthUser); err != nil {
					// Only claim preservation once it is actually persisted.
					s.log.Error("restore login username after import", "err", err)
					warnings = append(warnings, "This backup's login name could not be replaced with "+
						"yours ("+err.Error()+"). The login name is now "+strconv.Quote(imported)+
						" while the password is still yours - set both in the Access tab.")
					break
				}
				warnings = append(warnings, "This backup uses the login name "+strconv.Quote(imported)+
					", but backups never include the password. Your existing login name and password were kept, "+
					"so you are not locked out. Change them in the Access tab if you meant to switch.")
				s.log.Warn("imported config would have renamed the login account; kept the existing one",
					"imported", imported, "kept", preAuthUser)
			}
		}
		// A backup that turns login OFF on a box that HAD a working login would leave
		// the dashboard readable by any LAN peer - or, behind a declared same-host
		// proxy, any visitor - with no credentials. The old repair forced local-only,
		// which does NOT shield a box reached through such a proxy. But the password
		// hash never rides in a backup (settingsExportDeny), so the destination's own
		// hash is still in the store: re-enabling auth_enabled restores full protection
		// with the operator's own credentials. Keep the login the operator already had
		// rather than let the backup disable it.
		if backupLive && preAuthActive && !s.settings.AuthActive() {
			if err := s.settings.SetAuthEnabled(rctx, true); err != nil {
				// Persist-before-claim, as the username repair above: only say the login
				// was kept once the write lands. If it did not, the login switch is off
				// while the password is still stored, so say plainly that the box may be
				// exposed - do not force local-only in its place, which cannot protect a
				// proxied box anyway.
				s.log.Error("preserve destination login after import", "err", err)
				warnings = append(warnings, "This backup turns login protection off, and re-enabling your "+
					"existing login FAILED ("+err.Error()+"): the login switch is now off although your "+
					"password is still stored, so the dashboard may be reachable without a login. Re-enable "+
					"login in the Access tab now.")
			} else {
				warnings = append(warnings, "This backup turns login protection off, but backups never include "+
					"your password, so your existing login was kept and the dashboard stays protected. Turn "+
					"login off in the Access tab if that is really what you want.")
			}
			s.log.Warn("imported config would have removed login protection; kept the destination login",
				"user", preAuthUser, "auth_active", s.settings.AuthActive())
		}
		// Backups never carry the password hash (settingsExportDeny), so restoring
		// onto a box without its own password can import auth_enabled=true that
		// nothing can enforce - the login toggle would show ON while every request
		// sails through. Make intent match enforcement, and say so.
		if backupLive && s.settings.AuthEnabled() && !s.settings.AuthActive() {
			if err := s.settings.SetAuthEnabled(rctx, false); err != nil {
				s.log.Warn("disable unenforceable imported auth", "err", err)
			}
			// Fail CLOSED: the backup wanted login on but no password rode with it, so
			// login can't be enforced. If the same backup also opened Network access,
			// blindly applying it would expose the dashboard to the LAN with no login.
			// Force local-only until a password is set, so a "LAN + login" source can't
			// silently restore as "LAN, no login" (audit: access restore fails open).
			// When a same-host proxy is declared (-allow-host), local-only CANNOT block
			// visitors arriving through it, so the "restricted to this machine" wording
			// would be a false promise: state the exposure and urge a password instead.
			// With no proxy declared the local-only restriction is real, so keep the
			// existing truthful wording.
			caveat := proxyLocalOnlyCaveat(s.AllowedHosts)
			if !s.settings.AccessLocalOnly() {
				// Persist-before-claim, as above: "restricted to this machine" may only
				// be said of a write that landed.
				if err := s.settings.SetAccessLocalOnly(rctx, true); err != nil {
					s.log.Error("force local-only after unenforceable imported auth", "err", err)
					if caveat != "" {
						warnings = append(warnings, "This backup had login protection on, but backups never include "+
							"the password, so login cannot be enforced, and restricting access to this machine "+
							"FAILED ("+err.Error()+"). "+caveat)
					} else {
						warnings = append(warnings, "This backup had login protection on, but backups never include "+
							"the password, so login cannot be enforced - and restricting access to this machine "+
							"FAILED ("+err.Error()+"): the dashboard is now reachable over the network without a "+
							"password. Set a new password in the Access tab.")
					}
				} else if caveat != "" {
					warnings = append(warnings, "This backup had login protection on, but backups never include "+
						"the password, so login cannot be enforced. Access was set to this machine only, but "+caveat)
				} else {
					msg := "This backup had login protection on, but backups never include the password. Login stays off AND access was restricted to this machine only - set a new password in the Access tab, then re-enable Network access."
					// In a container "this machine" is the container: a bridged
					// container's published port now answers 403 - including to the
					// browser that ran this restore - so "the Access tab" may be
					// unreachable advice, and the distroless image has no shell to
					// repair the stored setting from. Say the way back in: an
					// explicit access choice at start overrides the stored one
					// (reconcileAccess in main, authoritative at every boot).
					if s.InContainer {
						msg += " This daemon runs in a container: if that just locked you out (pages now answer 403), " +
							"restart the container with -e PINGULARITY_ACCESS=network - an explicit access choice at " +
							"start overrides the stored setting - then set things right in the Access tab."
					}
					warnings = append(warnings, msg)
				}
			} else if caveat != "" {
				warnings = append(warnings, "This backup had login protection on, but backups never include the "+
					"password, so login stays off. Access is limited to this machine, but "+caveat)
			} else {
				warnings = append(warnings, "This backup had login protection on, but backups never include the password - login stays off until you set a new password in the Access tab.")
			}
			s.log.Warn("imported config had login enabled but no password; login left off",
				"local_only", s.settings.AccessLocalOnly())
		}
	}
	if result["downtime"] > 0 || result["speed"] > 0 {
		s.invalidateAggregates() // the uptime/data pills must not serve pre-import numbers for another aggTTL
	}
	if importErr != nil {
		// A category failed mid-stream, but earlier categories (and any config) were
		// already committed - the import is intentionally incremental, not atomic. Say
		// so explicitly: return which rows DID land (committed), a partial flag, and any
		// warnings (incl. the access-restore security notice), so a caller knows the
		// destination is in a mixed state rather than untouched (audit: partial import).
		s.log.Warn("data import failed after partial apply", "committed", result, "err", importErr)
		committed := map[string]any{}
		for k, v := range result {
			committed[k] = v
		}
		body := map[string]any{"error": importErr.Error(), "partial": true, "committed": committed}
		if len(warnings) > 0 {
			body["warnings"] = warnings
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(importStatus)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	// Classify the recorded timestamps HERE, from the LIVE settings - deliberately
	// after the reload above, because a backup carries its own retention policy and
	// the reload is what makes it the policy that prunes.
	//
	// This reads the live values whether or not that reload succeeded, which looks
	// like an oversight and is not. Reload succeeded: the live windows ARE the
	// imported ones, which is exactly what the next prune will apply. Reload
	// failed: Reload leaves the previous values in the live cache, so the
	// destination's own windows really do govern the hour ahead - and the
	// reload-failure warnings above already say the stored config differs. Either
	// way the live values are the ones that prune, so there is nothing to
	// special-case.
	liveWindow := map[string]time.Duration{
		"latency":  s.settings.Retention(),
		"speed":    s.settings.SpeedRetention(),
		"downtime": s.settings.DowntimeRetention(),
	}
	for _, cat := range []string{"latency", "speed", "downtime"} { // stable order
		window := liveWindow[cat]
		ts, seen := oldestTS[cat]
		if window <= 0 || !seen { // keep forever, or no timestamped rows landed
			continue
		}
		if ts < time.Now().Add(-window).Unix() {
			warnings = append(warnings, "Some imported "+cat+" rows are older than the current "+cat+
				" retention window and will be pruned within the hour - raise retention in the Data tab if you want to keep them.")
		}
	}
	s.log.Info("data imported", "result", result)
	resp := map[string]any{}
	for k, v := range result {
		resp[k] = v
	}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, resp)
}

// importArray streams one export category's JSON array into table in bounded
// batches (matching the store's own per-transaction bound), returning the rows
// applied and the OLDEST row timestamp it parsed - a restored row older than the
// retention window comes back only to be pruned within the hour, which reads as a
// broken restore unless the response says so. The comparison itself belongs to
// the caller and happens after the settings reload, since the backup may carry
// the very retention policy that decides it; this only reports what it saw.
// sawTS distinguishes "no timestamped rows at all" (speed_servers is keyed by
// run_ts and has no ts column; an empty array has no rows) from a genuine row
// stamped 0, which is an ancient, entirely prunable timestamp.
func (s *Server) importArray(ctx context.Context, dec *json.Decoder, key, table string, onProgress func()) (n int, minTS int64, sawTS bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, false, fmt.Errorf("bad %s data: %w", key, err)
	}
	if tok != json.Delim('[') {
		return 0, 0, false, fmt.Errorf("bad %s data: expected an array", key)
	}
	const batchRows = 5000 // = store.importTxRows: one handler batch per store transaction
	batch := make([]map[string]any, 0, batchRows)
	var batchBytes int
	// One per-timestamp counter for the whole category, shared across every batch,
	// so the store's maxRowsPerTS flood cap stays global instead of resetting per
	// batch (see ImportTableBatch).
	perTS := map[int64]int{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		applied, ierr := s.store.ImportTableBatch(ctx, table, batch, perTS)
		n += applied
		batch = batch[:0]
		batchBytes = 0
		if onProgress != nil {
			onProgress() // a batch landed: rearm the read deadline for the next one
		}
		return ierr
	}
	for dec.More() {
		// Decode via RawMessage so peak memory is bounded by BYTES (importBatchBytes)
		// as well as row count: a row's string columns (e.g. target) are unbounded,
		// so 5000 x row-size alone could reach multiple GiB. len(raw) is the exact
		// wire size of this element.
		var raw json.RawMessage
		if derr := dec.Decode(&raw); derr != nil {
			return n, minTS, sawTS, fmt.Errorf("bad %s data: %w", key, derr)
		}
		// Reject a single oversized row before it is unmarshalled and batched: no
		// legitimate record approaches maxRowBytes, and this keeps one hostile row
		// from bloating a batch (importReadBurst bounds the transient decode).
		if len(raw) > maxRowBytes {
			return n, minTS, sawTS, fmt.Errorf("bad %s data: a single record exceeds the %d MiB per-record limit", key, maxRowBytes>>20)
		}
		var row map[string]any
		if derr := json.Unmarshal(raw, &row); derr != nil {
			return n, minTS, sawTS, fmt.Errorf("bad %s data: %w", key, derr)
		}
		// Only a timestamp the store would actually keep counts: it applies this
		// same sanity rule (finite, >= 0, in int64 range) and drops the row
		// otherwise, so a NaN/negative/absurd ts must not become "the oldest row we
		// imported" - and int64() of a non-finite float is not even defined.
		if ts, ok := row["ts"].(float64); ok && !math.IsInf(ts, 0) && !math.IsNaN(ts) && ts >= 0 && ts < float64(math.MaxInt64) {
			if v := int64(ts); !sawTS || v < minTS {
				minTS, sawTS = v, true
			}
		}
		batch = append(batch, row)
		batchBytes += len(raw)
		if len(batch) >= batchRows || batchBytes >= importBatchBytes {
			if ferr := flush(); ferr != nil {
				return n, minTS, sawTS, ferr
			}
		}
	}
	if _, terr := dec.Token(); terr != nil { // closing ']'
		return n, minTS, sawTS, fmt.Errorf("bad %s data: %w", key, terr)
	}
	return n, minTS, sawTS, flush()
}

// importBatchBytes flushes an import batch once its decoded rows exceed this many
// bytes, even below batchRows, so peak memory tracks bytes not just row count.
const importBatchBytes = 8 << 20

// importReadWindow is how long an import may stall between decoded batches before
// its connection is reaped (see handleImport's per-progress deadline).
const importReadWindow = 2 * time.Minute

// importReadBurst is how many body bytes the import may consume between two
// progress marks (a flushed batch, a skipped token). There is deliberately NO
// whole-body cap to pair it with: the product's own DEFAULT export outgrew the
// old 256 MiB one - 6 dual-stack targets probed every 5s for the 30-day latency
// retention is 6 x (30d / 5s) = 3,110,400 sample rows at ~90-100 encoded bytes
// each, ~280-300 MB for the samples array alone before the dns series rides
// along (a test pins this arithmetic to the config defaults) - and retention
// and interval are settings, so the honest maximum is unbounded. Any fixed
// number either rejects real backups mid-restore (the file streams whole even
// when only small categories are selected) or is too large to buffer. What the
// old cap actually protected - a single materialized element (a selected row's
// json.RawMessage, or one giant skipped scalar token) staying well below
// process memory - the allowance still bounds, and it deliberately EQUALS the
// old cap: any single element the old pipeline could buffer still restores
// (a test pins a 65 MiB skipped scalar); what changed is only that progress
// renews the allowance, so total size no longer accumulates toward it, while
// an element that outgrows it trips importElementTooLargeError before it can
// balloon. Legitimate consumption between marks tops out at a batch
// (importBatchBytes) plus one row (maxRowBytes) plus decoder lookahead, far
// under the allowance. maxRowBytes additionally rejects a single SELECTED row
// much smaller than that, so a batch never holds a pathological row and a
// hostile file is refused early.
const (
	importReadBurst = 256 << 20
	maxRowBytes     = 8 << 20
)

// importElementTooLargeError reports one JSON element consuming the whole read
// allowance. A distinct type (mirroring http.MaxBytesError) so handleImport can
// answer 413 naming the size instead of calling it invalid JSON.
type importElementTooLargeError struct{ limit int64 }

func (e *importElementTooLargeError) Error() string {
	return fmt.Sprintf("a single element in the backup exceeds the %d MiB import limit", e.limit>>20)
}

// burstReader meters body reads against a renewable allowance: Read draws it
// down, renew refills it at each progress mark. Total throughput is unbounded
// by design; what cannot happen is one element consuming without bound, since
// json.Decoder materializes each value whole before any size check can see it.
type burstReader struct {
	r         io.Reader
	remaining int64
}

func (b *burstReader) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, &importElementTooLargeError{limit: importReadBurst}
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.r.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *burstReader) renew() { b.remaining = importReadBurst }

// maxImportDepth bounds skipJSONValue's recursion. json.Decoder.Token does NOT
// enforce encoding/json's nesting limit, so without a cap the depth of a skipped
// value is bounded only by the import body size - and a deep-enough nesting
// overflows the goroutine stack, which is a runtime FATAL (not a panic), so the
// handler's recover() cannot catch it and the whole daemon dies mid-import. Our
// own exports nest 3 deep; 64 is generous headroom that no real backup reaches.
const maxImportDepth = 64

// skipJSONValue advances dec past exactly one JSON value (scalar, object, or
// array) without materializing it - how the import walks past categories it
// wasn't asked to restore, at O(1) memory even when that category is huge.
// onTok, if non-nil, runs after every consumed token, so the caller can mark
// progress (read allowance, deadline) through an arbitrarily long walk.
func skipJSONValue(dec *json.Decoder, onTok func()) error {
	return skipJSONValueDepth(dec, onTok, 0)
}

func skipJSONValueDepth(dec *json.Decoder, onTok func(), depth int) error {
	if depth > maxImportDepth {
		return fmt.Errorf("bad import data: JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if onTok != nil {
		onTok()
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		for dec.More() {
			if err := skipJSONValueDepth(dec, onTok, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // the matching closing delimiter
			return err
		}
		if onTok != nil {
			onTok()
		}
	}
	return nil
}

// handleUpdate reports (GET) or flips (POST {enabled:bool}) the update-check
// toggle. The check is a background loop; this only gates it and, on enable,
// kicks an immediate poll so the cue can appear before the daily tick. Both
// methods return the current update Status.
func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		var in struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeJSONBody(w, r, &in); err != nil {
			return // response already written (415/400)
		}
		if err := s.settings.SetUpdateCheckEnabled(r.Context(), in.Enabled); err != nil {
			s.internalError(w, err)
			return
		}
		if in.Enabled && s.Update != nil {
			s.Update.CheckNow()
		}
		s.log.Info("update check toggled", "enabled", in.Enabled)
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		return
	}
	if s.Update != nil {
		writeJSON(w, s.Update.Status())
		return
	}
	writeJSON(w, map[string]any{"enabled": s.settings.UpdateCheckEnabled()})
}

// defaultLogLines is the window a bare /api/logs returns: the newest lines, not
// the whole ring. The viewer polls every 2.5s and the ring holds 4000 entries in
// two forms, so "the whole ring" was a 1.1 MB uncompressed no-store response
// every 2.5s once the ring filled - which it does after ~1.8h of debug logging
// and stays filled, because logs.txt restores it across restarts. That is
// invisible on a LAN and fatal over the link being diagnosed: below ~1.8 Mbps
// the fetch cannot finish inside its own 5s deadline, and the viewer's
// catch-swallowed abort leaves the previously-rendered lines on screen, so it
// freezes while still looking live.
const defaultLogLines = 500

// logWindow reads the ?since/?limit/?epoch window for one /api/logs response and
// returns the entries plus the cursor fields the viewer needs.
//
// The cursor is a monotonic sequence, not an offset, because the ring evicts
// under the reader: an offset would silently skip or repeat lines every time it
// did. ?since is honoured only when ?epoch matches the ring that issued it - a
// restart reseeds the ring from logs.txt with sequences starting at 0 again, so
// a cursor an open tab has held across the restart names a DIFFERENT line, and
// answering it would render a wrong window with nothing to show it was wrong.
//
// dropped reports how many lines fell out of the ring while the reader was away,
// so the viewer can mark the gap rather than silently splicing over it.
func (s *Server) logWindow(r *http.Request) (lines []logbuf.Entry, first, next, dropped uint64, epoch string, limit, buffered int) {
	lines = []logbuf.Entry{}
	if s.Logs == nil {
		return lines, 0, 0, 0, "", 0, 0
	}
	epoch = s.Logs.Epoch()
	q := r.URL.Query()
	// ?limit=0 is the documented escape hatch for a scripted caller that really
	// does want the whole buffer; anything else is clamped to the same ceiling
	// parsePage puts on every other paged read. (parsePage itself can't be reused:
	// it treats 0 as "unset", which is the opposite of what it means here.)
	limit = defaultLogLines
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n >= 0 {
		if n > maxPageLimit {
			n = maxPageLimit
		}
		limit = n
	}
	sv := q.Get("since")
	if sv == "" || q.Get("epoch") != epoch {
		lines, first, next = s.Logs.Tail(limit)
		return lines, first, next, 0, epoch, limit, s.Logs.Len()
	}
	since, err := strconv.ParseUint(sv, 10, 64)
	if err != nil {
		lines, first, next = s.Logs.Tail(limit)
		return lines, first, next, 0, epoch, limit, s.Logs.Len()
	}
	lines, first, next = s.Logs.Since(since, limit)
	if since < first {
		dropped = first - since
	}
	return lines, first, next, dropped, epoch, limit, s.Logs.Len()
}

// handleLogs backs the About-tab log viewer. GET returns the log window
// (see logWindow) plus the current on/off level and PII-redaction flag; POST
// {level, redact} sets logging and PII redaction (persisted + applied live via
// the settings broadcast) and {clear:true} empties the buffer. Either way it
// returns the fresh window, read from the same query parameters - so the redact
// toggle, which is a purely client-side view flip, no longer costs a full-ring
// response just to acknowledge itself.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("download") != "" {
			// Plain-text log file for a bug report - works over plain HTTP where the
			// clipboard Copy button doesn't. masked=1 downloads the PII-masked form
			// (what the viewer shows with the mask on), so a shared report is clean.
			masked := r.URL.Query().Get("masked") == "1"
			var b strings.Builder
			if s.Logs != nil {
				for _, e := range s.Logs.Entries() {
					if masked {
						b.WriteString(e.Masked)
					} else {
						b.WriteString(e.Raw)
					}
					b.WriteByte('\n')
				}
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="pingularity-logs.txt"`)
			io.WriteString(w, b.String())
			return
		}
	case http.MethodPost:
		var in struct {
			Level  string `json:"level"`
			Redact *bool  `json:"redact"`
			Clear  bool   `json:"clear"`
		}
		if err := decodeJSONBody(w, r, &in); err != nil {
			return // response already written (415/400)
		}
		if in.Clear && s.Logs != nil {
			s.Logs.Clear()
			// Clearing the in-memory ring alone leaves the on-disk logs.txt snapshot
			// intact, which resurrects the cleared lines after an unclean restart.
			// OnLogClear (wired by main) drops that snapshot too; nil is a no-op.
			if s.OnLogClear != nil {
				s.OnLogClear()
			}
		}
		if in.Level != "" {
			if err := s.settings.SetLogLevel(r.Context(), in.Level); err != nil {
				s.internalError(w, err)
				return
			}
			s.log.Info("log level changed", "level", s.settings.LogLevel())
		}
		if in.Redact != nil {
			if err := s.settings.SetLogRedactPII(r.Context(), *in.Redact); err != nil {
				s.internalError(w, err)
				return
			}
			s.log.Info("log PII redaction changed", "redact", s.settings.LogRedactPII())
		}
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
		return
	}
	// Each entry carries both the raw and PII-masked line; the dashboard toggles
	// between them at display time. "redact" is the persisted default mask state.
	// first_seq/next_seq/dropped/epoch are the cursor; see logWindow.
	lines, first, next, dropped, epoch, limit, buffered := s.logWindow(r)
	writeJSON(w, map[string]any{
		"level": s.settings.LogLevel(), "redact": s.settings.LogRedactPII(),
		"epoch": epoch, "first_seq": first, "next_seq": next, "dropped": dropped,
		// limit is the cap that was applied and buffered is how many lines the ring
		// holds, so a caller can tell a truncated read from a complete one. 500 lines
		// back is otherwise ambiguous: it could be the entire buffer or its newest
		// tenth, and nothing in the body distinguished them.
		"limit": limit, "buffered": buffered,
		"lines": lines,
	})
}

// promLabel escapes a label value for the Prometheus text exposition format,
// which permits ONLY \\, \" and \n as escapes. Go's %q (strconv.Quote) also emits
// \t, \xNN and \uNNNN, which Prometheus rejects - and one such byte fails the WHOLE
// scrape, not just its line. Since target and engine label values pass through from
// arbitrary imported rows (/api/import does not sanitize them), an imported tab or
// control byte would otherwise break every consumer's scrape. Other control
// characters are dropped: they never appear in a legitimate target or engine name.
func promLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			if r < 0x20 || r == 0x7f {
				continue // control byte from a garbage/hostile import; drop it
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Bounds on the per-target metric series, which carry names straight from the
// samples table (an import can back-fill arbitrarily many, arbitrarily long).
const (
	metricsMaxTargets  = 64 // distinct target= series per scrape
	metricsMaxLabelLen = 96 // bytes of a target label before truncation
)

// promTargetLabel truncates an over-long target name (dropping any trailing
// partial rune so the value stays valid UTF-8) before promLabel-escaping it, so a
// crafted import can't emit multi-kilobyte label values into every scrape.
func promTargetLabel(s string) string {
	if len(s) > metricsMaxLabelLen {
		s = strings.ToValidUTF8(s[:metricsMaxLabelLen], "")
	}
	return promLabel(s)
}

// handleHealthz is liveness: the process is up and serving. It intentionally does
// no dependency checks (a DB hiccup must not cause a restart loop) - use /readyz for
// dependency readiness. Unauthenticated (see guard).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "ok")
}

// handleReadyz is readiness: 200 only when the store answers and the first status
// aggregate has been computed (so a scrape/dashboard won't hit a cold, zero-valued
// cache). 503 otherwise, so a load balancer holds traffic until the daemon is warm.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A daemon that could not read its access configuration is refusing every
	// other route (see the guard), so it is emphatically not ready - and saying so
	// here is the only way a supervisor or load balancer finds out. Checked before
	// the store ping, because this failure survives a perfectly healthy database.
	if s.settings != nil && !s.settings.Loaded() {
		http.Error(w, "not ready: settings could not be loaded", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.store == nil || s.store.DB().PingContext(ctx) != nil {
		http.Error(w, "not ready: store unavailable", http.StatusServiceUnavailable)
		return
	}
	s.aggregates() // warm the cache on demand so a readyz-only probe can flip ready
	s.aggMu.Lock()
	warm := !s.aggAt.IsZero()
	s.aggMu.Unlock()
	if !warm {
		http.Error(w, "not ready: warming up", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ready")
}

// handleMetrics emits a minimal Prometheus exposition so an existing
// Prometheus/Grafana stack can scrape Pingularity directly.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// A scrape is a read: only GET (and HEAD, for liveness pings) are valid. Anything
	// else is a client mistake, not a 200-with-a-body. net/http drops the body for HEAD.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	if s.status == nil { // not wired (only happens in misconfiguration/tests) - degrade, don't panic
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
		return
	}
	st := s.status()
	tStart := time.Now()
	targets, terr := s.store.LatestPerTarget(ctx, s.targetGrace())
	targetsDur := time.Since(tStart)
	if terr != nil {
		s.log.Warn("metrics read failed; target series omitted from this scrape", "op", "latest_per_target", "err", terr)
	}
	s.collResult("targets", terr == nil)
	// Bound metric cardinality: a target name is whatever the samples table holds,
	// and an import can back-fill arbitrarily many crafted names. Cap the distinct
	// per-target series so a hostile backup can't explode the operator's Prometheus
	// (a real deployment has a handful of anchors, far under the cap). Long labels
	// are truncated at emit time by promTargetLabel.
	if len(targets) > metricsMaxTargets {
		stats.Inc("web.metrics_targets_capped")
		targets = targets[:metricsMaxTargets]
	}
	// Label normalization (96-byte truncation, control-byte stripping) is
	// many-to-one over raw names, and duplicate label sets in one exposition
	// are dropped by Prometheus with a per-sample warning. One series per
	// LABEL: first target wins (LatestPerTarget's order is stable), later
	// colliders are skipped and disclosed on the collision counter.
	{
		seen := make(map[string]bool, len(targets))
		kept := make([]store.TargetLatency, 0, len(targets))
		for _, tr := range targets {
			lbl := promTargetLabel(tr.Target)
			if seen[lbl] {
				stats.Inc("web.metrics_label_collisions")
				continue
			}
			seen[lbl] = true
			kept = append(kept, tr)
		}
		targets = kept
	}
	aStart := time.Now()
	uptime, dataBytes, avgDownB, avgUpB, usage := s.aggregates()
	aggDur := time.Since(aStart)
	s.aggMu.Lock()
	// Warmth alone is not health: once the cache filled a single time, aggAt
	// stays non-zero forever, so it must be paired with the LAST refresh
	// attempt's outcome or the collector reads green while every refresh
	// fails and the served numbers age without bound.
	aggValid := !s.aggAt.IsZero() && s.aggOK
	s.aggMu.Unlock()
	s.collResult("aggregates", aggValid)

	// UptimeFloor is a store read like any other collector's: accounted, timed,
	// and folded into metrics_data_valid, so its failure can never be a silent
	// 200 with a missing series.
	fStart := time.Now()
	floor, ferr := s.store.UptimeFloor(ctx, s.settings.DowntimeRetention())
	floorDur := time.Since(fStart)
	if ferr != nil {
		s.log.Warn("metrics read failed; uptime floor omitted from this scrape", "op", "uptime_floor", "err", ferr)
	}
	s.collResult("uptime_floor", ferr == nil)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP pingularity_build_info Build metadata; constant 1, version and Go toolchain in the labels.")
	fmt.Fprintln(w, "# TYPE pingularity_build_info gauge")
	fmt.Fprintf(w, "pingularity_build_info{version=\"%s\",goversion=\"%s\"} 1\n", promLabel(s.version), promLabel(runtime.Version()))

	fmt.Fprintln(w, "# HELP pingularity_runtime_seconds Process uptime in seconds.")
	fmt.Fprintln(w, "# TYPE pingularity_runtime_seconds gauge")
	fmt.Fprintf(w, "pingularity_runtime_seconds %d\n", int(time.Since(s.started).Seconds()))
	fmt.Fprintln(w, "# HELP pingularity_process_start_time_seconds Start time of the process since the unix epoch (the Prometheus-conventional form; runtime_seconds kept for compatibility).")
	fmt.Fprintln(w, "# TYPE pingularity_process_start_time_seconds gauge")
	fmt.Fprintf(w, "pingularity_process_start_time_seconds %d\n", s.started.Unix())

	fmt.Fprintln(w, "# HELP pingularity_up Connectivity state (1 online, 0 offline).")
	fmt.Fprintln(w, "# TYPE pingularity_up gauge")
	fmt.Fprintln(w, "pingularity_up", util.B2I(st.Online))

	fmt.Fprintln(w, "# HELP pingularity_monitoring_paused Monitoring stopped via the power button (1 paused) - stored gauges freeze and live per-family/DNS series go absent while paused.")
	fmt.Fprintln(w, "# TYPE pingularity_monitoring_paused gauge")
	fmt.Fprintln(w, "pingularity_monitoring_paused", util.B2I(st.Paused))

	// The trust signal for pingularity_up/state_since: those keep their last
	// value whenever rounds stop (power button, latency toggle, a closed
	// schedule window, all families off) - this says whether they are live.
	fmt.Fprintln(w, "# HELP pingularity_probing_active Probe rounds are actually running right now (1/0); while 0, pingularity_up and state_since hold their last values.")
	fmt.Fprintln(w, "# TYPE pingularity_probing_active gauge")
	fmt.Fprintln(w, "pingularity_probing_active", util.B2I(st.Probing))

	if !st.Since.IsZero() {
		fmt.Fprintln(w, "# HELP pingularity_state_since_timestamp_seconds When the current up/down state began (unix seconds).")
		fmt.Fprintln(w, "# TYPE pingularity_state_since_timestamp_seconds gauge")
		fmt.Fprintf(w, "pingularity_state_since_timestamp_seconds %d\n", st.Since.Unix())
		// Length of the outage in progress; absent (not 0) while up. Only emitted
		// while probing is active: paused wall-time is excluded from the outage the
		// monitor finally records (see monitor pausedGap), so counting time.Since
		// here during a pause would let the live metric race past the value that
		// eventually lands in history - and make the README's "outage > 5m" alert
		// fire on an intentional pause. While paused, state_since still exposes when
		// the outage began, so a consumer can compute observed duration if it wants.
		if !st.Online && st.Probing {
			fmt.Fprintln(w, "# HELP pingularity_current_outage_seconds Length of the outage in progress (seconds); absent while online or while probing is paused.")
			fmt.Fprintln(w, "# TYPE pingularity_current_outage_seconds gauge")
			fmt.Fprintf(w, "pingularity_current_outage_seconds %d\n", int(time.Since(st.Since).Seconds()))
		}
	}

	// Each family's samples are emitted as one contiguous block (all _up lines,
	// then all _latency lines, ...): the exposition format requires a metric
	// family to be a single group, and strict parsers reject interleaving.
	fmt.Fprintln(w, "# HELP pingularity_family_up Per-address-family connectivity (1/0).")
	fmt.Fprintln(w, "# TYPE pingularity_family_up gauge")
	for _, f := range st.Families {
		fmt.Fprintf(w, "pingularity_family_up{family=\"%s\"} %d\n", promLabel(f.Family), util.B2I(f.Online))
	}
	fmt.Fprintln(w, "# HELP pingularity_family_latency_seconds Per-family latency, lowest across that family's anchors (only while the family is online).")
	fmt.Fprintln(w, "# TYPE pingularity_family_latency_seconds gauge")
	for _, f := range st.Families {
		if f.Online { // an offline family has no latency reading; skip rather than emit 0
			fmt.Fprintf(w, "pingularity_family_latency_seconds{family=\"%s\"} %g\n", promLabel(f.Family), f.LatencyMS/1000.0)
		}
	}
	// When this family's current up/down state began - the per-family sibling of
	// state_since, so an operator can measure how long an IPv6-only outage has
	// run (the v4_only/v6_only_down_s counters give totals, not this episode).
	fmt.Fprintln(w, "# HELP pingularity_family_state_since_timestamp_seconds When this family's current up/down state began (unix seconds).")
	fmt.Fprintln(w, "# TYPE pingularity_family_state_since_timestamp_seconds gauge")
	for _, f := range st.Families {
		if !f.Since.IsZero() {
			fmt.Fprintf(w, "pingularity_family_state_since_timestamp_seconds{family=\"%s\"} %d\n", promLabel(f.Family), f.Since.Unix())
		}
	}

	fmt.Fprintln(w, "# HELP pingularity_target_up Last probe success per target (1/0).")
	fmt.Fprintln(w, "# TYPE pingularity_target_up gauge")
	for _, t := range targets {
		fmt.Fprintf(w, "pingularity_target_up{target=\"%s\"} %d\n", promTargetLabel(t.Target), util.B2I(t.Success))
	}
	fmt.Fprintln(w, "# HELP pingularity_target_latency_seconds Last probe latency per target (only for a successful probe; a down target has no reading, not 0).")
	fmt.Fprintln(w, "# TYPE pingularity_target_latency_seconds gauge")
	best, haveBest := 0.0, false
	for _, t := range targets {
		if t.Success { // a failed probe has no latency; emitting 0 would read as a fast link
			fmt.Fprintf(w, "pingularity_target_latency_seconds{target=\"%s\"} %g\n", promTargetLabel(t.Target), t.LatencyMS/1000.0)
			if !haveBest || t.LatencyMS < best {
				best, haveBest = t.LatencyMS, true
			}
		}
	}
	if haveBest { // headline latency = lowest across the anchors that answered (the UI's number)
		fmt.Fprintln(w, "# HELP pingularity_latency_seconds Lowest latency across the anchors that answered last round - your base internet latency; absent when nothing answered.")
		fmt.Fprintln(w, "# TYPE pingularity_latency_seconds gauge")
		fmt.Fprintf(w, "pingularity_latency_seconds %g\n", best/1000.0)
	}
	// Per-target freshness: when each target was last probed. A frozen reading (e.g.
	// while paused) is detectable by a timestamp that stops advancing, since target_up
	// deliberately holds its last value. probe_last_round is the newest across all.
	if len(targets) > 0 {
		newest := int64(0)
		fmt.Fprintln(w, "# HELP pingularity_target_last_probe_timestamp_seconds When each target was last probed (unix seconds).")
		fmt.Fprintln(w, "# TYPE pingularity_target_last_probe_timestamp_seconds gauge")
		for _, t := range targets {
			fmt.Fprintf(w, "pingularity_target_last_probe_timestamp_seconds{target=\"%s\"} %d\n", promTargetLabel(t.Target), t.TS)
			if t.TS > newest {
				newest = t.TS
			}
		}
		fmt.Fprintln(w, "# HELP pingularity_probe_last_round_timestamp_seconds Newest probe round timestamp across all targets (unix seconds); a stalled loop stops advancing it.")
		fmt.Fprintln(w, "# TYPE pingularity_probe_last_round_timestamp_seconds gauge")
		fmt.Fprintf(w, "pingularity_probe_last_round_timestamp_seconds %d\n", newest)
	}

	// DNS-resolution probe (the chart's second line): a separate signal from anchor
	// latency. Only while the DNS probe is actually running (monitoring on, latency
	// probing allowed by its schedule, DNS sub-toggle on), so a disabled or paused
	// probe reads as absent rather than a fake "resolver down".
	if st.DNSactive {
		fmt.Fprintln(w, "# HELP pingularity_dns_up DNS resolution succeeded on the last probe round (1/0); present only while the DNS probe is running.")
		fmt.Fprintln(w, "# TYPE pingularity_dns_up gauge")
		fmt.Fprintln(w, "pingularity_dns_up", util.B2I(st.DNSok))
		if st.DNSok { // a failed lookup has no time; absent rather than a fake 0
			fmt.Fprintln(w, "# HELP pingularity_dns_resolve_seconds Time for one cache-busted DNS lookup last round, via the host's own resolver (seconds).")
			fmt.Fprintln(w, "# TYPE pingularity_dns_resolve_seconds gauge")
			fmt.Fprintf(w, "pingularity_dns_resolve_seconds %g\n", st.DNSms/1000.0)
		}
	}

	// uptime_ratio is now OBSERVED-time based: paused/unobserved wall time is
	// excluded from the denominator, and each window is clamped to the retention
	// horizon so a window can't outrun the outage data behind it. A window that
	// observed nothing (coverage 0) is omitted entirely rather than published as a
	// misleading 100%. pingularity_uptime_coverage_ratio reports how much of each
	// window was actually observed, and _since_timestamp the floor "all" reaches.
	fmt.Fprintln(w, "# HELP pingularity_uptime_ratio Fraction of OBSERVED time the link was up (observed downtime / observed time; paused and unobserved wall time excluded). A window that observed nothing is absent - see pingularity_uptime_coverage_ratio.")
	fmt.Fprintln(w, "# TYPE pingularity_uptime_ratio gauge")
	fmt.Fprintln(w, "# HELP pingularity_uptime_coverage_ratio Fraction of each uptime window that was actually observed (0..1); a low value means the window was mostly paused/unobserved and its uptime_ratio is thin evidence.")
	fmt.Fprintln(w, "# TYPE pingularity_uptime_coverage_ratio gauge")
	for _, n := range uptime.Each() {
		fmt.Fprintf(w, "pingularity_uptime_coverage_ratio{window=\"%s\"} %g\n", promLabel(n.Window), n.Obs.Coverage())
		// Defined() is the old `coverage > 0` guard, unchanged in behaviour and
		// deliberately unchanged in strictness: it asks whether there is a
		// measurement at all, not whether the measurement is good enough. Anything
		// stricter here would delete all six ratio series, permanently, for every
		// install that legitimately monitors part-time - and a consumer can always
		// impose their own floor with `and on(window)
		// pingularity_uptime_coverage_ratio > X`, while nothing lets them recover a
		// value this exporter refused to emit.
		if n.Obs.Defined() {
			fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"%s\"} %g\n", promLabel(n.Window), n.Obs.Ratio())
		}
	}
	if ferr == nil && floor > 0 {
		fmt.Fprintln(w, "# HELP pingularity_uptime_since_timestamp_seconds Earliest time the uptime figures can vouch for (unix seconds): the later of first observation and the outage-retention horizon. The 'all' window reaches back only to here.")
		fmt.Fprintln(w, "# TYPE pingularity_uptime_since_timestamp_seconds gauge")
		fmt.Fprintf(w, "pingularity_uptime_since_timestamp_seconds %d\n", floor)
	}

	fmt.Fprintln(w, "# HELP pingularity_speed_data_used_bytes Cumulative speedtest bytes (down+up) within retention, INCLUDING attempts that failed or were retried - they spent the data too; excludes iperf3 warm-up and the UDP loss probe, so it is a lower bound on wire traffic.")
	fmt.Fprintln(w, "# TYPE pingularity_speed_data_used_bytes gauge")
	fmt.Fprintf(w, "pingularity_speed_data_used_bytes %d\n", dataBytes)
	// Per-window usage (same windows as uptime_ratio). Pruning makes the total
	// above non-monotonic, so a metered-link operator can't derive "this month's
	// speedtest data" from it - serve the windowed numbers the dashboard already
	// computes.
	fmt.Fprintln(w, "# HELP pingularity_speed_data_used_window_bytes Speedtest bytes (down+up) within the window, including failed and retried attempts; a lower bound on wire traffic (see pingularity_speed_data_used_bytes).")
	fmt.Fprintln(w, "# TYPE pingularity_speed_data_used_window_bytes gauge")
	for _, wv := range []struct {
		w string
		v int64
	}{{"6h", usage.H6}, {"24h", usage.H24}, {"7d", usage.D7}, {"30d", usage.D30}, {"1y", usage.Y1}} {
		fmt.Fprintf(w, "pingularity_speed_data_used_window_bytes{window=\"%s\"} %d\n", promLabel(wv.w), wv.v)
	}
	if avgDownB > 0 || avgUpB > 0 { // absent before any run, like the other speed readings - never a fake 0
		fmt.Fprintln(w, "# HELP pingularity_speed_avg_run_bytes Average bytes per speedtest run (recent runs), by direction; a direction is absent (not 0) until it has been measured. Runs that failed partway are excluded on purpose - this is a projection of what the next run will cost, and an aborted run's partial bytes predict a bill no schedule produces (their bytes are still in pingularity_speed_data_used_bytes).")
		fmt.Fprintln(w, "# TYPE pingularity_speed_avg_run_bytes gauge")
		// Emit each direction only when it has data. A download-only history has a
		// zero upload average (no upload samples); publishing avg_run_bytes{up} 0
		// would read as a measured zero rather than "never measured".
		if avgDownB > 0 {
			fmt.Fprintf(w, "pingularity_speed_avg_run_bytes{direction=\"down\"} %d\n", avgDownB)
		}
		if avgUpB > 0 {
			fmt.Fprintf(w, "pingularity_speed_avg_run_bytes{direction=\"up\"} %d\n", avgUpB)
		}
	}

	// When the next scheduled speedtest is due (mirrors /api/status): lets an
	// operator alert on a wedged scheduler before the next run would even land.
	if s.speed != nil && s.settings.SpeedtestEnabled() {
		if nr := s.speed.NextRun(); !nr.IsZero() {
			fmt.Fprintln(w, "# HELP pingularity_speed_next_run_timestamp_seconds When the next scheduled speedtest is due (unix seconds); absent when scheduled tests are off.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_next_run_timestamp_seconds gauge")
			fmt.Fprintf(w, "pingularity_speed_next_run_timestamp_seconds %d\n", nr.Unix())
		}
	}

	sStart := time.Now()
	sp, serr := s.store.LatestSpeed(ctx)
	speedDur := time.Since(sStart)
	s.collResult("speed", serr == nil)
	if serr != nil {
		s.log.Warn("metrics read failed; speed series omitted from this scrape", "op", "latest_speed", "err", serr)
	} else if sp != nil {
		// Freshness anchor for the speed gauges below: a stale value looks
		// identical to a fresh one without it. Alert on
		// time() - this > 2 * your speedtest interval.
		fmt.Fprintln(w, "# HELP pingularity_speed_last_run_timestamp_seconds When the last speedtest ran (unix seconds).")
		fmt.Fprintln(w, "# TYPE pingularity_speed_last_run_timestamp_seconds gauge")
		fmt.Fprintf(w, "pingularity_speed_last_run_timestamp_seconds %d\n", sp.TS)
		engine := sp.Engine
		if engine == "" {
			engine = "ookla" // older rows / the default backend
		}
		fmt.Fprintln(w, "# HELP pingularity_speed_info Last speedtest's backend engine (in the label); value is constant 1.")
		fmt.Fprintln(w, "# TYPE pingularity_speed_info gauge")
		fmt.Fprintf(w, "pingularity_speed_info{engine=\"%s\"} 1\n", promTargetLabel(engine))
		if sp.Healthy != nil { // the in-app threshold verdict, so alerts reuse it instead of re-encoding thresholds
			fmt.Fprintln(w, "# HELP pingularity_speed_healthy Last speedtest passed its configured thresholds (1/0); absent when no thresholds are configured or the run measured nothing they cover.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_healthy gauge")
			fmt.Fprintln(w, "pingularity_speed_healthy", util.B2I(*sp.Healthy))
		}
		if sp.DownBytes != nil || sp.UpBytes != nil {
			// What THIS run consumed (the retention-wide totals above can't answer
			// that) - the number a metered-link operator alerts on per run.
			fmt.Fprintln(w, "# HELP pingularity_speed_last_run_bytes Data the last speedtest transferred, by direction; absent when the engine did not measure it. \"Last\" means the last run that measured something: a run that failed partway is never the last run, here or on the dashboard.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_last_run_bytes gauge")
			if sp.DownBytes != nil {
				fmt.Fprintf(w, "pingularity_speed_last_run_bytes{direction=\"down\"} %d\n", *sp.DownBytes)
			}
			if sp.UpBytes != nil {
				fmt.Fprintf(w, "pingularity_speed_last_run_bytes{direction=\"up\"} %d\n", *sp.UpBytes)
			}
		}
		// Emit each per-run figure ONLY when the run actually measured it, using the
		// same evidence the *_bytes gauges gate on: a direction the engine skipped
		// (e.g. speed_direction="down" skips upload) has no *_bytes and a zeroed
		// speed, so emitting 0.0 would read as a real reading and fire a permanent
		// false "below threshold" alert. Render absence instead.
		if sp.DownBytes != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_download_mbps Last measured download speed (Mbit/s); absent when the run did not measure download.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_download_mbps gauge")
			fmt.Fprintf(w, "pingularity_speed_download_mbps %g\n", sp.DownMbps)
		}
		if sp.UpBytes != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_upload_mbps Last measured upload speed (Mbit/s); absent when the run did not measure upload.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_upload_mbps gauge")
			fmt.Fprintf(w, "pingularity_speed_upload_mbps %g\n", sp.UpMbps)
		}
		if sp.PingMS != 0 {
			// Says WHERE it measures to, because the scrape also carries
			// pingularity_speed_idle_latency_ms and the two are routinely graphed
			// together: that one is the bufferbloat baseline against a fixed target of
			// its own, so the pair diverging is expected, not a fault. The iperf3 caveat
			// is not hypothetical - iperf.go falls back to the idle baseline when no
			// handshake probe lands, which is reachable whenever --bind names an
			// interface the probe's default route does not use, so an unhedged "to the
			// test server" would be false to a scraper in exactly that configuration.
			fmt.Fprintln(w, "# HELP pingularity_speed_ping_ms Last measured speedtest latency to the test server (ms); the iperf3 engine falls back to the bufferbloat idle baseline when no handshake probe reaches the server. Absent when the run did not probe latency.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_ping_ms gauge")
			fmt.Fprintf(w, "pingularity_speed_ping_ms %g\n", sp.PingMS)
		}
		if sp.PingBestMS != nil {
			// The floor under pingularity_speed_ping_ms, which is a MEAN over the
			// engine's ten samples and so moves several-fold on one stalled
			// handshake. A large gap between the two is a single slow sample, not a
			// slow link - alert on this one to mean "the link really is far", and
			// diff them to spot a lossy path.
			fmt.Fprintln(w, "# HELP pingularity_speed_ping_best_ms Fastest of the last speedtest's ping samples (ms); pingularity_speed_ping_ms is their mean. Absent on iperf3 runs, which report no per-sample values.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_ping_best_ms gauge")
			fmt.Fprintf(w, "pingularity_speed_ping_best_ms %g\n", *sp.PingBestMS)
		}
		if sp.JitterMS != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_jitter_ms Last measured speedtest jitter (ms).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_jitter_ms gauge")
			fmt.Fprintf(w, "pingularity_speed_jitter_ms %g\n", *sp.JitterMS)
		}
		if sp.PacketLoss != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_packet_loss_percent Last measured packet loss (%).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_packet_loss_percent gauge")
			fmt.Fprintf(w, "pingularity_speed_packet_loss_percent %g\n", *sp.PacketLoss)
		}
		// Prometheus base-unit siblings alongside the human-friendly _mbps/_ms/_percent
		// gauges above: bytes/second, seconds, and a 0..1 loss ratio, for dashboards
		// that follow the naming conventions. Same source values, converted.
		if sp.DownBytes != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_download_bytes_per_second Last measured download throughput (bytes/second).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_download_bytes_per_second gauge")
			fmt.Fprintf(w, "pingularity_speed_download_bytes_per_second %g\n", sp.DownMbps*1e6/8)
		}
		if sp.UpBytes != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_upload_bytes_per_second Last measured upload throughput (bytes/second).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_upload_bytes_per_second gauge")
			fmt.Fprintf(w, "pingularity_speed_upload_bytes_per_second %g\n", sp.UpMbps*1e6/8)
		}
		if sp.PingMS != 0 {
			fmt.Fprintln(w, "# HELP pingularity_speed_ping_seconds Last measured speedtest latency to the test server (seconds); same value and same iperf3 caveat as pingularity_speed_ping_ms.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_ping_seconds gauge")
			fmt.Fprintf(w, "pingularity_speed_ping_seconds %g\n", sp.PingMS/1000)
		}
		if sp.PacketLoss != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_packet_loss_ratio Last measured packet loss (0..1).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_packet_loss_ratio gauge")
			fmt.Fprintf(w, "pingularity_speed_packet_loss_ratio %g\n", *sp.PacketLoss/100)
		}
		// Latency under load: idle baseline + per-phase loaded medians. The
		// loaded-minus-idle delta is the bufferbloat; alert on it directly.
		if sp.IdleMS != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_idle_latency_ms Idle-baseline latency for the loaded-latency comparison (ms).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_idle_latency_ms gauge")
			fmt.Fprintf(w, "pingularity_speed_idle_latency_ms %g\n", *sp.IdleMS)
		}
		if sp.LoadedDownMS != nil || sp.LoadedUpMS != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_loaded_latency_ms Latency during the speedtest load phase, by direction (ms).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_loaded_latency_ms gauge")
			if sp.LoadedDownMS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_ms{direction=\"down\"} %g\n", *sp.LoadedDownMS)
			}
			if sp.LoadedUpMS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_ms{direction=\"up\"} %g\n", *sp.LoadedUpMS)
			}
		}
		if sp.LoadedDownP95MS != nil || sp.LoadedUpP95MS != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_loaded_latency_p95_ms 95th-percentile latency during the speedtest load phase, by direction (ms).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_loaded_latency_p95_ms gauge")
			if sp.LoadedDownP95MS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_p95_ms{direction=\"down\"} %g\n", *sp.LoadedDownP95MS)
			}
			if sp.LoadedUpP95MS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_p95_ms{direction=\"up\"} %g\n", *sp.LoadedUpP95MS)
			}
		}
	}

	// Process self-health: the hand-rolled exposition gives no free go_* metrics,
	// so surface the leak-detection basics ourselves - a months-running daemon
	// needs goroutine and heap trends visible.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintln(w, "# HELP pingularity_goroutines Number of live goroutines.")
	fmt.Fprintln(w, "# TYPE pingularity_goroutines gauge")
	fmt.Fprintf(w, "pingularity_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintln(w, "# HELP pingularity_memory_heap_bytes Bytes of allocated heap objects.")
	fmt.Fprintln(w, "# TYPE pingularity_memory_heap_bytes gauge")
	fmt.Fprintf(w, "pingularity_memory_heap_bytes %d\n", ms.HeapAlloc)
	// Standard-ish Go runtime signals, hand-emitted (no client_golang dependency):
	// resident/sys memory, GC count, and the scheduler shape - enough to spot a leak
	// or GC thrash on a long-running daemon.
	fmt.Fprintln(w, "# HELP pingularity_memory_sys_bytes Total bytes of memory obtained from the OS.")
	fmt.Fprintln(w, "# TYPE pingularity_memory_sys_bytes gauge")
	fmt.Fprintf(w, "pingularity_memory_sys_bytes %d\n", ms.Sys)
	fmt.Fprintln(w, "# HELP pingularity_gc_cycles_total Completed GC cycles since start.")
	fmt.Fprintln(w, "# TYPE pingularity_gc_cycles_total counter")
	fmt.Fprintf(w, "pingularity_gc_cycles_total %d\n", ms.NumGC)
	fmt.Fprintln(w, "# HELP pingularity_gomaxprocs Configured GOMAXPROCS.")
	fmt.Fprintln(w, "# TYPE pingularity_gomaxprocs gauge")
	fmt.Fprintf(w, "pingularity_gomaxprocs %d\n", runtime.GOMAXPROCS(0))
	if fds, ok := openFDs(); ok {
		fmt.Fprintln(w, "# HELP pingularity_open_fds Number of open file descriptors (Unix).")
		fmt.Fprintln(w, "# TYPE pingularity_open_fds gauge")
		fmt.Fprintf(w, "pingularity_open_fds %d\n", fds)
	}

	// On-disk database footprint (main file + WAL/SHM sidecars) - a retention
	// misconfiguration shows up here long before the disk fills.
	if s.DBPath != "" && s.DBPath != ":memory:" {
		var total int64
		seen := false
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if fi, err := os.Stat(s.DBPath + suffix); err == nil {
				total += fi.Size()
				seen = true
			}
		}
		if seen {
			fmt.Fprintln(w, "# HELP pingularity_db_bytes On-disk size of the database (incl. WAL/SHM).")
			fmt.Fprintln(w, "# TYPE pingularity_db_bytes gauge")
			fmt.Fprintf(w, "pingularity_db_bytes %d\n", total)
		}
		// Free space on the filesystem holding the data dir - an early warning long
		// before writes start failing. Only emitted where the platform supports it.
		if free, ok := diskFree(filepath.Dir(s.DBPath)); ok {
			fmt.Fprintln(w, "# HELP pingularity_disk_free_bytes Free space on the filesystem holding the database.")
			fmt.Fprintln(w, "# TYPE pingularity_disk_free_bytes gauge")
			fmt.Fprintf(w, "pingularity_disk_free_bytes %d\n", free)
		}
	}

	// Update-check state: the same facts the dashboard badge shows, so "an
	// update has been pending for a week" and "the feed has been unreachable
	// for days" (the firewalled-install signature) are alertable. Absent
	// entirely when the checker is off or unwired; the freshness timestamp is
	// absent until a poll has SUCCEEDED - a 0 would read as 1970-stale.
	if s.Update != nil {
		if ust := s.Update.Status(); ust.Enabled {
			fmt.Fprintln(w, "# HELP pingularity_update_available A newer release is available (1) or not (0).")
			fmt.Fprintln(w, "# TYPE pingularity_update_available gauge")
			fmt.Fprintf(w, "pingularity_update_available %d\n", util.B2I(ust.Available))
			if ust.CheckedUnix > 0 {
				fmt.Fprintln(w, "# HELP pingularity_update_check_timestamp_seconds When the release feed was last successfully polled (unix seconds).")
				fmt.Fprintln(w, "# TYPE pingularity_update_check_timestamp_seconds gauge")
				fmt.Fprintf(w, "pingularity_update_check_timestamp_seconds %d\n", ust.CheckedUnix)
			}
		}
	}

	// Collector health for this scrape (F4): whether each store read succeeded, how
	// long it took, cumulative errors, and last-success time. metrics_data_valid is 1
	// only when every read this scrape succeeded - so a DB failure that would
	// otherwise be a silent 200 with missing/stale series is alertable directly.
	type coll struct {
		name string
		ok   bool
		dur  time.Duration
	}
	colls := []coll{{"targets", terr == nil, targetsDur}, {"aggregates", aggValid, aggDur}, {"speed", serr == nil, speedDur}, {"uptime_floor", ferr == nil, floorDur}}
	s.collMu.Lock()
	errsCopy, okCopy := map[string]int64{}, map[string]int64{}
	for k, v := range s.collErrs {
		errsCopy[k] = v
	}
	for k, v := range s.collOK {
		okCopy[k] = v
	}
	s.collMu.Unlock()
	fmt.Fprintln(w, "# HELP pingularity_metrics_collector_success Whether each store collector read succeeded on this scrape (1/0).")
	fmt.Fprintln(w, "# TYPE pingularity_metrics_collector_success gauge")
	for _, c := range colls {
		fmt.Fprintf(w, "pingularity_metrics_collector_success{collector=\"%s\"} %d\n", c.name, util.B2I(c.ok))
	}
	fmt.Fprintln(w, "# HELP pingularity_metrics_collector_duration_seconds How long each collector's read took on this scrape.")
	fmt.Fprintln(w, "# TYPE pingularity_metrics_collector_duration_seconds gauge")
	for _, c := range colls {
		fmt.Fprintf(w, "pingularity_metrics_collector_duration_seconds{collector=\"%s\"} %g\n", c.name, c.dur.Seconds())
	}
	fmt.Fprintln(w, "# HELP pingularity_metrics_collector_errors_total Cumulative failed collector reads since startup.")
	fmt.Fprintln(w, "# TYPE pingularity_metrics_collector_errors_total counter")
	for _, c := range colls {
		fmt.Fprintf(w, "pingularity_metrics_collector_errors_total{collector=\"%s\"} %d\n", c.name, errsCopy[c.name])
	}
	fmt.Fprintln(w, "# HELP pingularity_metrics_collector_last_success_timestamp_seconds When each collector last read successfully (unix seconds).")
	fmt.Fprintln(w, "# TYPE pingularity_metrics_collector_last_success_timestamp_seconds gauge")
	for _, c := range colls {
		if ts := okCopy[c.name]; ts > 0 {
			fmt.Fprintf(w, "pingularity_metrics_collector_last_success_timestamp_seconds{collector=\"%s\"} %d\n", c.name, ts)
		}
	}
	dataValid := terr == nil && aggValid && serr == nil && ferr == nil
	fmt.Fprintln(w, "# HELP pingularity_metrics_data_valid 1 when every store read on this scrape succeeded; 0 when any failed (series may be missing or stale despite the 200).")
	fmt.Fprintln(w, "# TYPE pingularity_metrics_data_valid gauge")
	fmt.Fprintf(w, "pingularity_metrics_data_valid %d\n", util.B2I(dataValid))

	// One snapshot for the whole exposition: three writers reading three
	// separate snapshots let a counter disagree with its own back-compat
	// duplicate inside a single scrape.
	snap := stats.Lifetime()
	writeNamedStats(w, snap)
	writeHistograms(w, snap)
	writeStatMetrics(w, snap)
}

// writeHistograms emits the recorded distributions as Prometheus histograms
// (cumulative _bucket + _sum + _count), so a rule can compute p95/p99 and see
// spikes that fall between scrapes - which a last-value gauge loses.
//
// This table is the only reader of snap.Histos, and stats.Lifetime is read
// nowhere else in the process (web.go:4699 is its only non-test call): a
// histogram missing from the table is recorded forever and never exposed at
// all, with nothing to say so.
// Each entry's bucket bounds come from the recording side (internal/stats,
// LatencyBucketsSeconds or the histoBuckets override), not from here.
func writeHistograms(w io.Writer, snap stats.Snap) {
	for _, h := range []struct{ key, name, help string }{
		{"probe.latency", "pingularity_probe_latency_seconds", "Anchor round-trip latency distribution (seconds), per successful target probe."},
		{"dns.latency", "pingularity_dns_latency_seconds", "DNS resolve-time distribution (seconds), per successful DNS probe."},
		{"series.query.seconds", "pingularity_series_query_seconds", "Chart series aggregate duration (seconds), per executed query."},
	} {
		hist, ok := snap.Histos[h.key]
		if !ok || hist.Count == 0 {
			continue
		}
		fmt.Fprintf(w, "# HELP %s %s\n", h.name, h.help)
		fmt.Fprintf(w, "# TYPE %s histogram\n", h.name)
		for i, b := range hist.Bounds {
			fmt.Fprintf(w, "%s_bucket{le=\"%g\"} %d\n", h.name, b, hist.Counts[i])
		}
		fmt.Fprintf(w, "%s_bucket{le=\"+Inf\"} %d\n", h.name, hist.Count)
		fmt.Fprintf(w, "%s_sum %g\n", h.name, hist.Sum)
		fmt.Fprintf(w, "%s_count %d\n", h.name, hist.Count)
	}
}

// writeNamedStats emits well-named, single-quantity metric families derived from the
// internal registry (F8): pingularity_stat_total mixes counts, seconds and ms sums
// under one name, which the Prometheus naming conventions discourage. These give each
// real quantity its own family + labels; the generic stat_total stays for back-compat.
func writeNamedStats(w io.Writer, snap stats.Snap) {
	acc := make(map[string]float64, len(snap.Counters)+len(snap.Floats))
	for k, v := range snap.Counters {
		acc[k] = float64(v)
	}
	for k, v := range snap.Floats {
		acc[k] = v
	}
	// Single counters from a fixed key (scale converts ms sums to seconds).
	for _, m := range []struct {
		key, name, help string
		scale           float64
	}{
		{"monitor.rounds", "pingularity_probe_rounds_total", "Probe rounds completed.", 1},
		{"monitor.downs", "pingularity_outages_total", "Debounced outages started.", 1},
		{"monitor.outage_s_sum", "pingularity_outage_duration_seconds_total", "Cumulative observed outage duration (seconds).", 1},
		{"monitor.blips", "pingularity_probe_blips_total", "Sub-threshold connectivity blips that did not reach an outage.", 1},
		{"dns.attempts", "pingularity_dns_attempts_total", "DNS resolve probes attempted.", 1},
		{"db.prune_count", "pingularity_database_prunes_total", "Database prune passes completed.", 1},
		{"db.prune_ms_sum", "pingularity_database_prune_duration_seconds_total", "Cumulative database prune time (seconds).", 0.001},
		{"web.login_fail", "pingularity_login_failures_total", "Failed dashboard login attempts.", 1},
		{"web.limiter_trips", "pingularity_rate_limit_trips_total", "Login rate-limiter trips.", 1},
		// Chart-aggregate cache accounting. Fixed keys, no labels: the window,
		// bucket width and target list a request carries are unbounded, and a
		// label per bucket width alone would multiply these series by every
		// range the dashboards offer.
		{"series.cache.hit", "pingularity_series_cache_hits_total", "Chart series served from a live cache entry, running no query.", 1},
		{"series.cache.expired", "pingularity_series_cache_expired_total", "Chart series requests whose cache entry existed but had expired.", 1},
		{"series.cache.new", "pingularity_series_cache_new_total", "Chart series requests with no stored scan for the window (first sight, evicted, a scan that errored, or an entry another caller is still filling).", 1},
		{"series.cache.empty", "pingularity_series_cache_empty_total", "Chart series requests re-scanned because the window's last scan found no rows, which is deliberately not cached.", 1},
		{"series.bypass", "pingularity_series_bypass_total", "Chart series requests that never consulted the cache (sub-minute buckets).", 1},
		{"series.query", "pingularity_series_queries_total", "Chart series aggregates actually executed.", 1},
	} {
		if v, ok := acc[m.key]; ok {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %g\n", m.name, m.help, m.name, m.name, v*m.scale)
		}
	}
	// Speedtest run durations: a _sum/_count pair is a (quantile-less) summary,
	// and typing it as two standalone counters puts reserved suffixes on the
	// wrong TYPE (promtool lints it). Sample names are unchanged, so existing
	// queries keep working; the pair is seeded, so the family exists from boot.
	if sum, ok := acc["speed.duration_s_sum"]; ok {
		if n, ok2 := acc["speed.duration_n"]; ok2 {
			fmt.Fprintln(w, "# HELP pingularity_speed_run_duration_seconds Speedtest run durations: _sum is cumulative seconds, _count is timed runs.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_run_duration_seconds summary")
			fmt.Fprintf(w, "pingularity_speed_run_duration_seconds_sum %g\n", sum)
			fmt.Fprintf(w, "pingularity_speed_run_duration_seconds_count %g\n", n)
		}
	}

	// Notification delivery latency: per-destination summary in seconds. The
	// registry keys stay raw ms sums (notify.<dest>.lat_ms_sum, back-compat in
	// stat_total); this is the queryable form - "webhook got slow" is
	// rate(_sum)/rate(_count).
	{
		var dests []string
		for k := range acc {
			if d := strings.TrimSuffix(strings.TrimPrefix(k, "notify."), ".lat_ms_sum"); d != k && strings.HasPrefix(k, "notify.") {
				if _, ok := acc["notify."+d+".lat_n"]; ok {
					dests = append(dests, d)
				}
			}
		}
		if len(dests) > 0 {
			sort.Strings(dests)
			fmt.Fprintln(w, "# HELP pingularity_notification_delivery_duration_seconds Notification delivery time per destination: _sum is cumulative seconds, _count is timed deliveries.")
			fmt.Fprintln(w, "# TYPE pingularity_notification_delivery_duration_seconds summary")
			for _, d := range dests {
				fmt.Fprintf(w, "pingularity_notification_delivery_duration_seconds_sum{destination=\"%s\"} %g\n", promLabel(d), acc["notify."+d+".lat_ms_sum"]*0.001)
				fmt.Fprintf(w, "pingularity_notification_delivery_duration_seconds_count{destination=\"%s\"} %g\n", promLabel(d), acc["notify."+d+".lat_n"])
			}
		}
	}

	// Labeled families: keys "<prefix><labelvalue>" -> metric{label="labelvalue"}.
	emitFamily := func(prefix, name, label, help string) {
		var keys []string
		for k := range acc {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			return
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s=\"%s\"} %g\n", name, label, promLabel(k[len(prefix):]), acc[k])
		}
	}
	emitFamily("probe.fail.", "pingularity_probe_failures_total", "reason", "Probe dial failures by class.")
	emitFamily("dns.fail.", "pingularity_dns_failures_total", "reason", "DNS resolve failures by class.")
	emitFamily("speed.run.", "pingularity_speed_runs_total", "trigger", "Speedtest runs by trigger.")
	emitFamily("speed.fail.", "pingularity_speed_failures_total", "stage", "Speedtest failures by stage.")

	// Background-worker health. worker.<name>.restarts is a counter (acc); worker.<name>.up
	// is a gauge (snap.Gauges): 1 while the loop runs, 0 on death (give-up,
	// shutdown) - and REMOVED when a one-shot worker completes its job, so
	// worker_up==0 alerts match only real deaths (see spawnLoop in main.go).
	var upLines, restartLines []string
	for k, v := range snap.Gauges {
		if n := strings.TrimSuffix(strings.TrimPrefix(k, "worker."), ".up"); n != k && strings.HasPrefix(k, "worker.") {
			upLines = append(upLines, fmt.Sprintf("pingularity_worker_up{worker=\"%s\"} %d\n", promLabel(n), v))
		}
	}
	for k, v := range acc {
		if n := strings.TrimSuffix(strings.TrimPrefix(k, "worker."), ".restarts"); n != k && strings.HasPrefix(k, "worker.") {
			restartLines = append(restartLines, fmt.Sprintf("pingularity_worker_restarts_total{worker=\"%s\"} %g\n", promLabel(n), v))
		}
	}
	if len(upLines) > 0 {
		sort.Strings(upLines)
		fmt.Fprintln(w, "# HELP pingularity_worker_up Background worker running (1) or dead (0: gave up or shut down); a one-shot worker that completed its job is removed rather than 0, by worker.")
		fmt.Fprintln(w, "# TYPE pingularity_worker_up gauge")
		for _, l := range upLines {
			fmt.Fprint(w, l)
		}
	}
	if len(restartLines) > 0 {
		sort.Strings(restartLines)
		fmt.Fprintln(w, "# HELP pingularity_worker_restarts_total Background worker panic-restarts, by worker.")
		fmt.Fprintln(w, "# TYPE pingularity_worker_restarts_total counter")
		for _, l := range restartLines {
			fmt.Fprint(w, l)
		}
	}

	// Notifications: keys "notify.<destination>.<outcome>". Split ok/fail/blocked
	// into their own families keyed by destination (heartbeat included as a dest).
	type note struct{ dest, outcome string }
	notes := map[string]note{}
	for k := range acc {
		if p := strings.TrimPrefix(k, "notify."); p != k {
			if i := strings.LastIndexByte(p, '.'); i > 0 {
				notes[k] = note{dest: p[:i], outcome: p[i+1:]}
			}
		}
	}
	for _, oc := range []struct{ outcome, name, help string }{
		{"ok", "pingularity_notification_deliveries_total", "Notifications delivered, by destination."},
		{"fail", "pingularity_notification_failures_total", "Notification delivery failures, by destination."},
		{"blocked", "pingularity_notification_blocked_total", "Notifications blocked (SSRF/policy), by destination."},
	} {
		var keys []string
		for k, n := range notes {
			if n.outcome == oc.outcome {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", oc.name, oc.help, oc.name)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{destination=\"%s\"} %g\n", oc.name, promLabel(notes[k].dest), acc[k])
		}
	}
	// Database errors: a fixed set of db.* keys (prune_count/prune_ms_sum above are not errors).
	var dbKeys []string
	for _, r := range []string{"err", "busy", "io_err", "disk_full", "corrupt"} {
		if _, ok := acc["db."+r]; ok {
			dbKeys = append(dbKeys, r)
		}
	}
	if len(dbKeys) > 0 {
		sort.Strings(dbKeys)
		fmt.Fprintln(w, "# HELP pingularity_database_errors_total Database errors by class.")
		fmt.Fprintln(w, "# TYPE pingularity_database_errors_total counter")
		for _, r := range dbKeys {
			fmt.Fprintf(w, "pingularity_database_errors_total{reason=\"%s\"} %g\n", promLabel(r), acc["db."+r])
		}
	}
}

// writeStatMetrics dumps the internal stats registry into the exposition. It
// holds a dynamic set of dotted names (monitor.blips, speed.run.scheduled,
// notify.discord.ok, …), so rather than minting a metric per key they share two
// stable families keyed by a `stat` label: monotonic accumulators (counters +
// float sums) under pingularity_stat_total, and gauges under pingularity_stat.
// The registry is always-on and never drained, so these are honest Prometheus
// counters.
func writeStatMetrics(w io.Writer, snap stats.Snap) {
	totals := make(map[string]float64, len(snap.Counters)+len(snap.Floats))
	for k, v := range snap.Counters {
		if promStat(k) {
			totals[k] = float64(v)
		}
	}
	for k, v := range snap.Floats {
		if promStat(k) {
			totals[k] = v
		}
	}
	if len(totals) > 0 {
		fmt.Fprintln(w, "# HELP pingularity_stat_total Internal operational counters; the metric is in the stat label.")
		fmt.Fprintln(w, "# TYPE pingularity_stat_total counter")
		for _, k := range sortedKeys(totals) {
			fmt.Fprintf(w, "pingularity_stat_total{stat=\"%s\"} %g\n", promLabel(k), totals[k])
		}
	}
	g := make(map[string]float64, len(snap.Gauges))
	for k, v := range snap.Gauges {
		if promStat(k) {
			g[k] = float64(v)
		}
	}
	if len(g) > 0 { // gate on the FILTERED set: all-product gauges must not leave a dangling header
		fmt.Fprintln(w, "# HELP pingularity_stat Internal operational gauges; the metric is in the stat label.")
		fmt.Fprintln(w, "# TYPE pingularity_stat gauge")
		for _, k := range sortedKeys(g) {
			fmt.Fprintf(w, "pingularity_stat{stat=\"%s\"} %g\n", promLabel(k), g[k])
		}
	}
}

// promStat reports whether an internal stat is operational (belongs on
// /metrics) rather than product/usage analytics, which the endpoint omits.
// Operational counters have no rawer form - nothing persists each blip, flap,
// or delivery failure - so the counter is the ground truth an operator alerts
// on. Allowlisted by namespace so a future product counter can't silently leak
// in; the mixed web.* namespace is resolved per key.
func promStat(name string) bool {
	switch {
	case strings.HasPrefix(name, "monitor."),
		strings.HasPrefix(name, "probe."), // dial-failure taxonomy (probe.fail.<class>)
		strings.HasPrefix(name, "dns."),   // DNS-resolve failure taxonomy (dns.fail.<class>)
		strings.HasPrefix(name, "speed."),
		strings.HasPrefix(name, "netinfo."),
		strings.HasPrefix(name, "notify."),
		strings.HasPrefix(name, "worker."), // background-worker up/restarts
		// Chart-aggregate cache outcomes (series.cache.*, series.bypass,
		// series.query). promStat gates writeStatMetrics alone: every key the
		// table in writeNamedStats names ALSO has a hand-written row there, and
		// that writer builds its accumulator straight from the snapshot and
		// never calls promStat, so dropping this prefix would cost those keys
		// only their pingularity_stat_total{stat="series…"} samples - the
		// pingularity_series_*_total families would keep being emitted. It is a
		// NEW series.* key that hangs on this line: with no row in
		// writeNamedStats its only exporter is writeStatMetrics, and
		// stats.Lifetime is read nowhere else in the process (handleMetrics
		// makes the sole non-test call), so an unclassified name would be
		// recorded into a black hole - no log, no error, just absent.
		strings.HasPrefix(name, "series."),
		strings.HasPrefix(name, "db."):
		return true
	case strings.HasPrefix(name, "import."):
		// Import/restore repair counters: recorded expressly for /metrics
		// visibility (a repair that drops rows must not be silent).
		return true
	case name == "web.login_fail" || name == "web.limiter_trips",
		name == "web.stepup_fail", // security signal, sibling of login_fail
		// The /metrics self-disclosures: targets silently capped or dropped
		// on label collision - the operator's only sign their view shrank.
		name == "web.metrics_targets_capped", name == "web.metrics_label_collisions":
		return true
	default:
		// Product/usage analytics never belong on the operator's endpoint. Those
		// emitters are gone, but the guard stays: any unclassified namespace - a
		// future product counter, or names like settings.changed.* / web.ui_loads
		// that the filter test still injects - is excluded until deliberately
		// classified above.
		return false
	}
}

func sortedKeys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// internalError logs an internal store/settings failure server-side and returns a
// generic 500, so SQLite/driver internals (paths, schema, error text) don't leak
// to the client. User-actionable errors - a bad speedtest target, a webhook that
// won't deliver, a validation failure the operator must read to fix - keep their
// specific message via a direct http.Error instead.
func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// decodeJSONBody decodes a small control-plane JSON body into v under a 64 KiB
// cap, so an unauthenticated client can't stream an unbounded body at a
// fixed-struct handler. (Bulk import has its own larger cap.) It writes the
// full error response itself (415 or 400); on a non-nil error callers just
// return without writing again.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) error {
	// Require application/json on any request carrying a body. A cross-site HTML
	// form can only send text/plain or form encodings without a CORS preflight,
	// so this makes CSRF protection independent of the session cookie's
	// SameSite=Strict. The SPA always sends application/json; curl/Prometheus
	// clients send JSON or no body.
	if r.ContentLength != 0 {
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
			return fmt.Errorf("unsupported content-type %q", mt)
		}
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return err
	}
	return nil
}

// requireJSONBody enforces the same application/json content-type CSRF guard as
// decodeJSONBody, for the bulk import, which reads the body directly instead of
// through it. A cross-site HTML form can't set application/json without a CORS
// preflight, so this keeps CSRF protection from resting on the session cookie's
// SameSite=Strict alone. Returns false (415) on a non-JSON body; an empty body
// (ContentLength 0) is allowed.
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// requireJSONCT enforces the application/json content-type CSRF guard even for an
// EMPTY body - for mutating POSTs that carry no JSON payload (the speedtest trigger
// and netinfo refresh). decodeJSONBody/requireJSONBody exempt empty bodies, so
// without this a cross-site page could fire those with a body-less no-cors POST
// (no preflight). A cross-site HTML form can't set application/json without a CORS
// preflight, so this blocks it; the SPA sends the header on these calls and
// curl/Prometheus clients can add it.
func requireJSONCT(w http.ResponseWriter, r *http.Request) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "content-type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// maxDataMins (~100y) caps the ?dataMins window so the Duration multiply in
// customDataMins can't overflow.
const maxDataMins = 52560000

// customDataMins parses a ?dataMins=N custom-window value into a negative
// Duration (minutes ago), ready for now.Add(d). Returns ok=false for empty,
// non-numeric, or non-positive input; clamps at maxDataMins.
func customDataMins(v string) (time.Duration, bool) {
	m, err := strconv.Atoi(v)
	if err != nil || m <= 0 {
		return 0, false
	}
	if m > maxDataMins {
		m = maxDataMins
	}
	return -time.Duration(m) * time.Minute, true
}

// parsePage parses ?limit (default defLimit, must be >0) and ?offset (default 0,
// must be >=0) for the paginated list endpoints.
func parsePage(r *http.Request, defLimit int) (limit, offset int) {
	// Cap the page size: an unbounded ?limit flows straight into SELECT ... LIMIT
	// and, on the default auth-off LAN posture, lets any client force a huge
	// materialize+JSON-encode - a cheap memory-amplification vector.
	limit = defLimit
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		if n > maxPageLimit {
			n = maxPageLimit
		}
		limit = n
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n >= 0 {
		offset = n
	}
	return limit, offset
}

// fptr formats an optional float for CSV ("" when nil).
func fptr(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', 2, 64)
}

// fptr1 is fptr at one decimal place (for jitter ms).
func fptr1(f *float64) string {
	if f == nil {
		return ""
	}
	return strconv.FormatFloat(*f, 'f', 1, 64)
}

// iptr formats an optional int64 for CSV ("" when nil).
func iptr(i *int64) string {
	if i == nil {
		return ""
	}
	return strconv.FormatInt(*i, 10)
}

// csvMbps renders a directional throughput for CSV, blank when that direction
// wasn't measured (nil byte pointer) so an untested direction isn't exported as a
// real "0.00". Byte-presence is the measured signal (same as the tiles/metrics).
func csvMbps(mbps float64, bytes *int64) string {
	if bytes == nil {
		return ""
	}
	return strconv.FormatFloat(mbps, 'f', 2, 64)
}

// csvPing renders a latency figure, blank when the run never probed one. Zero is
// the "not probed" sentinel for ping (a successful iperf3 run that moved real
// bytes can legitimately report 0), and the tiles, the runs table, the charts
// and /metrics all treat it as absent - the CSV was the last surface still
// exporting it as a literal "0.0", which reads as a perfect round trip rather
// than as no measurement, and is worse in an export than on screen because a
// spreadsheet will happily average it.
func csvPing(ms float64) string {
	if ms <= 0 {
		return ""
	}
	return strconv.FormatFloat(ms, 'f', 1, 64)
}

// healthStr renders the threshold verdict for CSV ("" when not evaluated).
func healthStr(b *bool) string {
	if b == nil {
		return ""
	}
	if *b {
		return "healthy"
	}
	return "unhealthy"
}
