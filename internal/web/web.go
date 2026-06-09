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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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

// SpeedTrigger runs a speedtest on demand and reports whether one is in
// progress. It is satisfied by speedtest.Scheduler and may be nil when
// speedtests are disabled.
type SpeedTrigger interface {
	RunOnce(ctx context.Context, reason string) (store.SpeedSample, error)
	Running() bool
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
	store      *store.Store
	status     StatusFunc
	speed      SpeedTrigger
	settings   *settings.Controller
	netinfo    NetInfo
	version    string
	started    time.Time // process start (for the runtime pill)
	listenAddr string    // bound address (for showing reachable URLs)
	log        *slog.Logger
	logins     *failLimiter // per-IP throttle on password failures

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

	// InContainer relaxes the loopback-only access filter: a bridged container
	// NATs every request to the gateway, so the filter can't be enforced by peer
	// IP and must not lock the dashboard out. Set by main before Serve.
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

	// OnLogClear, when set, runs after the /api/logs clear branch empties the
	// in-memory ring, so main can also drop the on-disk logs.txt snapshot that
	// would otherwise resurrect the cleared lines after an unclean restart. Nil is
	// a no-op (tests, headless). Set by main before Serve.
	OnLogClear func()

	// importSem caps concurrent /api/import runs so a flood of slow-body uploads
	// can't tie up unbounded goroutines/FDs. Lazily built (importGate) so a
	// struct-literal Server still gates.
	importSemOnce sync.Once
	importSem     chan struct{}

	// Cached status aggregates: uptime windows scan the outage events table, the
	// data totals/averages scan the speed table. Recomputed at most once per
	// aggTTL instead of on every status poll.
	aggMu     sync.Mutex
	aggAt     time.Time
	aggBusy   bool   // a recompute is in flight; others serve the stale cache
	aggGen    uint64 // bumped by invalidators; an in-flight recompute that started before the bump can't re-stamp aggAt
	uptime    store.Uptime
	dataBytes int64
	avgDownB  int64
	avgUpB    int64
	usage     store.DataUsage
}

// aggTTL bounds how stale the cached status aggregates may be.
const aggTTL = 30 * time.Second

// aggregates returns the cached status figures (uptime ratios + speedtest data
// totals/averages), recomputing only when the cache has expired.
func (s *Server) aggregates() (uptime store.Uptime, dataBytes, avgDown, avgUp int64, usage store.DataUsage) {
	s.aggMu.Lock()
	fresh := !s.aggAt.IsZero() && time.Since(s.aggAt) < aggTTL
	// Serve the cache when fresh, or when stale but another caller is already
	// recomputing - a slightly stale value beats serializing concurrent
	// /api/status and /metrics behind the slow DB scans below. But never serve
	// the never-filled cold cache (aggAt still zero): its zero Uptime{} would
	// publish a spurious 0% uptime to a /metrics scrape racing the first
	// recompute at startup. In that case fall through and scan ourselves (a rare
	// redundant scan at cold start; the aggGen guard keeps the re-stamp honest).
	if (fresh || s.aggBusy) && !s.aggAt.IsZero() {
		defer s.aggMu.Unlock()
		return s.uptime, s.dataBytes, s.avgDownB, s.avgUpB, s.usage
	}
	// This goroutine owns the recompute; run the scans without the lock so other
	// callers keep serving the cache. Always release ownership, even on a scan
	// panic - else the guard's recover() leaves aggBusy stuck true and freezes
	// the cache for the rest of the process.
	s.aggBusy = true
	gen := s.aggGen // if an invalidation lands during the scan, don't re-stamp aggAt with pre-invalidation data
	s.aggMu.Unlock()
	defer func() {
		s.aggMu.Lock()
		s.aggBusy = false
		s.aggMu.Unlock()
	}()
	// Detached, bounded context: the cache is shared, so one caller disconnecting
	// mid-scan must not cancel the fill and stamp zeros in for everyone.
	scanCtx, cancel := context.WithTimeout(context.Background(), aggTTL)
	defer cancel()
	now := time.Now()
	nUptime, e1 := s.store.UptimeWindows(scanCtx, now)
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
	mux.HandleFunc("/api/speed/runs.csv", s.handleSpeedRunsCSV)
	mux.HandleFunc("/api/speed/usage", s.handleSpeedUsage)
	mux.HandleFunc("/api/notify/test", s.handleNotifyTest)
	mux.HandleFunc("/api/speedtest", s.handleSpeedtest)
	mux.HandleFunc("/api/speedtest/servers", s.handleSpeedtestServers)
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
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)
	mux.HandleFunc("/metrics", s.handleMetrics)
	// guard applies the loopback-only filter and authentication to every route;
	// securityHeaders stamps its headers on every response, including guard
	// rejections; logRequests wraps it all (outermost) so even rejected
	// requests are logged at debug.
	return s.logRequests(securityHeaders(s.guard(mux)))
}

// securityHeaders adds defense-in-depth headers to every response: nosniff
// stops MIME-sniffing of the JSON/download endpoints, and the CSP gives the
// single-file UI's ~50 innerHTML sinks a second layer against a future missed
// esc() - nothing may load from an external origin, plugins are blocked, and
// <base> tags and foreign form targets are refused. frame-ancestors 'none'
// refuses framing entirely (the UI never runs embedded), closing the
// clickjacking vector against an auth-off dashboard that no session cookie
// guards. All script/style is inline, so those two directives need 'unsafe-inline'.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
		"img-src 'self' data:; font-src data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", csp)
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
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = uiAsset.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		w.Write(body)
	}
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
		s.log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sr.status,
			"host", r.Host, "ip", clientIP(r), "dur_ms", util.Round1(util.DurMS(time.Since(start))))
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
		// own deadline for large restores). No WriteTimeout: /api/netinfo POST and
		// /api/speedtest/servers have per-handler deadlines, and /api/speedtest
		// runs until the test completes, bounded only by BaseContext.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
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

	resp := map[string]any{
		"version":              s.version,
		"online":               online,
		"state_seconds":        int(now.Sub(since).Seconds()),
		"runtime_seconds":      int(now.Sub(s.started).Seconds()),
		"latency_ms":           latency,
		"dns_ms":               dnsMS,
		"uptime":               uptime,     // up-fraction per window (6h/24h/7d/30d/1y/all) for the selectable pill
		"uptime_24h":           uptime.H24, // kept for back-compat / metrics parity
		"uptime_7d":            uptime.D7,
		"targets":              targets,
		"speed":                speed,          // nil until the first test completes
		"speedtest":            s.speed != nil, // whether manual triggering is available
		"speedtest_running":    s.speed != nil && s.speed.Running(),
		"speedtest_server":     speedRunningServer(s.speed),
		"speedtest_auto":       s.settings.SpeedServerID() == "", // auto-select fastest server
		"speedtest_auto_label": s.settings.SpeedAutoLabel(),      // city the auto-picker centres on
		"speedtest_enabled":    s.settings.SpeedtestEnabled(),
		"speed_interval_s":     int(s.settings.SpeedInterval().Seconds()),
		"data_used_bytes":      dataBytes,                  // cumulative speedtest data (down+up)
		"data_usage":           usage,                      // per-window breakdown (6h/24h/7d/30d/1y/all)
		"speed_avg_down_bytes": avgDownB,                   // avg per-run download bytes (recent runs; 0 if none)
		"speed_avg_up_bytes":   avgUpB,                     // avg per-run upload bytes
		"families":             st.Families,                // per-family (IPv4/IPv6) live state
		"paused":               st.Paused,                  // monitoring stopped via the power button
		"first_seen":           s.store.InstallBornAt(ctx), // stable per-install id; scopes the first-run coachmark
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
	// and cap as the data window.
	if d, ok := customDataMins(r.URL.Query().Get("upMins")); ok {
		if v, err := s.store.UptimeSince(ctx, now.Add(d)); err == nil {
			resp["uptime_custom"] = v
		}
	}
	// Cached update status (non-blocking read) for the About tab's cue and
	// current/latest display. Absent in tests/headless (no checker).
	if s.Update != nil {
		resp["update"] = s.Update.Status()
	}
	// A bridged container measures the CONTAINER's path, not this host's: an
	// extra hop of latency and a traceroute that stops at the container gateway.
	// The dashboard has to say so, because the numbers look perfectly healthy
	// either way. Only sent when true, so the common case costs nothing.
	if util.BridgedContainer() {
		resp["bridged_container"] = true
	}
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
	floor, ceil := now.Add(-maxWinMins*time.Minute), now.Add(maxWinMins*time.Minute)
	since = time.Unix(f, 0)
	// A window beyond the far future is clamped, not refused: refusing would
	// fall back to the default window and draw the last 7 days under a label
	// saying 2030. Clamped, it selects nothing and the empty state says so.
	if since.After(ceil) {
		since = ceil
	}
	if tv := r.URL.Query().Get("to"); tv != "" {
		t, err := strconv.ParseInt(tv, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		if t != 0 {
			// Reversedness is judged on what was ASKED for, before any clamping.
			// A caller-reversed pair is bad input and falls back to ?mins=, but a
			// window that only becomes empty because the floor below raised its
			// start is a legitimate request for pruned history: the honest answer
			// there is no rows, not a silently different window.
			if !time.Unix(t, 0).After(since) {
				return time.Time{}, time.Time{}, false
			}
			until = time.Unix(t, 0)
			if until.After(ceil) {
				until = ceil
			}
		}
	}
	if since.Before(floor) {
		since = floor
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
	hist, err := s.store.SpeedHistoryRange(r.Context(), since, until)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if hist == nil {
		hist = []store.SpeedSample{}
	}
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total, err := s.store.SpeedCount(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"offset": off, "total": total})
		return
	}
	limit, offset := parsePage(r, 50)
	total, err := s.store.SpeedCount(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	runs, err := s.store.SpeedRuns(r.Context(), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if runs == nil {
		runs = []store.SpeedSample{}
	}
	writeJSON(w, map[string]any{"runs": runs, "total": total})
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	// SpeedHistory returns all rows since the epoch, oldest first.
	hist, err := s.store.SpeedHistory(r.Context(), time.Unix(0, 0))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="pingularity-speed-runs.csv"`)
	cw := csv.NewWriter(w)
	defer cw.Flush()
	cw.Write([]string{"timestamp", "download_mbps", "upload_mbps", "ping_ms", "jitter_ms",
		"packet_loss_pct", "idle_ms", "loaded_down_ms", "loaded_up_ms", "loaded_down_max_ms", "loaded_up_max_ms", "healthy", "download_bytes", "upload_bytes",
		"trigger", "engine", "server", "server_id", "isp", "isp_location",
		"public_ipv4", "public_ipv6", "dns_server", "dns_provider", "dns_location",
		"cf_colo", "exit_path"})
	// Emit newest first to match the on-screen table.
	for i := len(hist) - 1; i >= 0; i-- {
		sp := hist[i]
		cw.Write([]string{
			time.Unix(sp.TS, 0).UTC().Format(time.RFC3339),
			strconv.FormatFloat(sp.DownMbps, 'f', 2, 64),
			strconv.FormatFloat(sp.UpMbps, 'f', 2, 64),
			strconv.FormatFloat(sp.PingMS, 'f', 1, 64),
			fptr1(sp.JitterMS), fptr(sp.PacketLoss),
			fptr1(sp.IdleMS), fptr1(sp.LoadedDownMS), fptr1(sp.LoadedUpMS), fptr1(sp.LoadedDownMaxMS), fptr1(sp.LoadedUpMaxMS), healthStr(sp.Healthy),
			iptr(sp.DownBytes), iptr(sp.UpBytes),
			csvSafe(sp.Trigger), csvSafe(engineCSV(sp.Engine)),
			csvSafe(sp.Server), csvSafe(sp.ServerID), csvSafe(sp.ISP), csvSafe(sp.ISPLocation),
			sp.PublicIPv4, sp.PublicIPv6, sp.DNSIP, csvSafe(sp.DNSProvider), csvSafe(sp.DNSLocation),
			csvSafe(sp.CFColo), csvSafe(sp.ExitSummary),
		})
	}
}

// handleSpeedUsage returns cumulative speedtest data volume per time window.
// Optional ?dataMins=N adds that custom window's volume under "custom" (so the
// popover's custom row is right even when it isn't the active window).
func (s *Server) handleSpeedUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	u, err := s.store.SpeedDataUsage(r.Context(), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	if strings.TrimSpace(in.URL) == "" {
		http.Error(w, "no webhook URL set", http.StatusBadRequest)
		return
	}
	n := notify.New(func() string { return in.URL }, s.log)
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

// handleEvents returns a page of up/down transition events (newest first) for
// the paginated outages table.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, offset := parsePage(r, 10)
	total, err := s.store.EventCount(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ev, err := s.store.EventsPage(r.Context(), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
	// Detach from the request's cancellation, keeping its values. A manual run can
	// take minutes with "best of 3 servers" on, and tying the measurement to the
	// HTTP request meant a reload, a closed tab or any client-side give-up killed
	// it mid-transfer - surfacing as "speedtest failed: context canceled" and
	// storing nothing. The run is still bounded by the engine's own deadline, so
	// this cannot leak: it just lets a test the user asked for finish.
	res, err := s.speed.RunOnce(context.WithoutCancel(r.Context()), "manual")
	if errors.Is(err, speedtest.ErrBusy) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "speedtest failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, res)
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
	// Downsample to ~1500 buckets so wide windows stay small/fast. The width
	// comes from the part of the window that can actually HOLD data, not from
	// ?mins=: using mins would bucket a two-hour window from last year as
	// coarsely as a whole year, and counting an end that has not arrived yet
	// would coarsen every window whose end is in the future - which is most of
	// them, since typing a bare year or "jul 1 to dec 31" ends next January.
	end := until
	if end.IsZero() || end.After(now) {
		end = now
	}
	span := end.Sub(since)
	bucket := int(span/time.Second) / 1500
	if bucket < 1 {
		bucket = 1
	}
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
func (s *Server) handleIperfCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	addr := strings.TrimSpace(r.URL.Query().Get("addr"))
	// Report only reachable/not, never the raw dial error, so this can't probe
	// refused-vs-timeout-vs-no-route for arbitrary internal hosts.
	if rtt, err := speedtest.CheckIperfServer(ctx, addr); err == nil {
		writeJSON(w, map[string]any{"reachable": true, "rtt_ms": rtt})
	} else {
		writeJSON(w, map[string]any{"reachable": false})
	}
}

// handleSpeedtestServers returns Ookla servers for the picker. With ?city=<name>
// it geocodes the city and returns servers near it; otherwise servers near the
// caller's own location.
func (s *Server) handleSpeedtestServers(w http.ResponseWriter, r *http.Request) {
	if s.netinfo == nil { // permitted nil: 503, not a panic (see handleNetinfo)
		http.Error(w, "network info unavailable", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 18*time.Second)
	defer cancel()

	// Pin an exact server by Ookla ID: the "City or server ID" box resolves a
	// numeric entry here so the UI can confirm its name before saving.
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		srv, err := speedtest.GetOoklaServer(ctx, id)
		if err != nil {
			http.Error(w, "server not found", http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"servers": []speedtest.ServerInfo{srv}})
		return
	}

	var lat, lon float64
	var locName string
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
		// With no explicit coordinate, centre the default list on the ISP's
		// exit-router location (matching the live auto-select), then the Cloudflare
		// PoP where a traceroute exit can't run (containers, non-Linux), falling
		// through to the caller's IP when neither is known yet.
		if lat == 0 && lon == 0 {
			if ex := s.netinfo.Get().Exit; ex != nil && (ex.Lat != 0 || ex.Lon != 0) {
				lat, lon, locName = ex.Lat, ex.Lon, ex.Loc
			} else if city, clat, clon, ok := netinfo.ColoCoord(s.netinfo.Get().CFColo); ok {
				lat, lon, locName = clat, clon, city
			}
		}
	}

	list, err := speedtest.ListOoklaServers(ctx, lat, lon)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if list == nil {
		list = []speedtest.ServerInfo{}
	}
	writeJSON(w, map[string]any{"servers": list, "location": locName, "lat": lat, "lon": lon})
}

// geocodeClient caps the Nominatim fetch like netinfo's lookup clients: an
// explicit timeout and no redirects, so a hijacked upstream can't bounce the
// daemon's GET to an arbitrary host and stream an unbounded body back.
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
	SpeedAutoLoc             *string `json:"speed_auto_loc"`
	SpeedAutoLabel           *string `json:"speed_auto_label"`
	SpeedtestEnabled         *bool   `json:"speedtest_enabled"`
	SpeedtestOnReconnect     *bool   `json:"speedtest_on_reconnect"`
	IPv6Mode                 *string `json:"ipv6_mode"`
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
	OoklaLoss        *bool            `json:"ookla_loss"`
	SpeedBestOf      *bool            `json:"speed_best_of"`
	IperfOmit        *int             `json:"iperf_omit"`
	SpeedDirection   *string          `json:"speed_direction"`
	IperfUDP         *bool            `json:"iperf_udp"`
	IperfUDPRate     *int             `json:"iperf_udp_rate"`
	IperfWindow      *int             `json:"iperf_window"`
	SpeedRetries     *int             `json:"speed_retries"`
	IperfCongestion  *string          `json:"iperf_congestion"`
	IperfNoDelay     *bool            `json:"iperf_nodelay"`
	IperfDSCP        *string          `json:"iperf_dscp"`
	IperfMSS         *int             `json:"iperf_mss"`
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
	Iperf3Available bool         `json:"iperf3_available"`
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
		SpeedAutoLoc:             ptr(v.SpeedAutoLoc),
		SpeedAutoLabel:           ptr(v.SpeedAutoLabel),
		SpeedtestEnabled:         ptr(v.SpeedtestEnabled),
		SpeedtestOnReconnect:     ptr(v.SpeedtestOnReconnect),
		IPv6Mode:                 ptr(v.IPv6Mode),
		ExitTarget:               ptr(v.ExitTarget),
		SpeedtestAdaptive:        ptr(v.SpeedtestAdaptive),
		SpeedtestOnDegraded:      ptr(v.SpeedtestOnDegraded),
		DegradedPingMS:           ptr(v.DegradedPingMS),
		SpeedtestSkipBusy:        ptr(v.SpeedtestSkipBusy),
		SpeedBusyMbps:            ptr(v.SpeedBusyMbps),
		SpeedEngine:              ptr(v.SpeedEngine),
		IperfServer:              ptr(v.IperfServer),
		IperfServers:             iperfServersToDTO(v.IperfServers),
		IperfDur:                 ptr(v.IperfDur),
		IperfStreams:             ptr(v.IperfStreams),
		OoklaConnections:         ptr(v.OoklaConnections),
		OoklaLoss:                ptr(v.OoklaLoss),
		SpeedBestOf:              ptr(v.SpeedBestOf),
		IperfOmit:                ptr(v.IperfOmit),
		SpeedDirection:           ptr(v.SpeedDirection),
		IperfUDP:                 ptr(v.IperfUDP),
		IperfUDPRate:             ptr(v.IperfUDPRate),
		IperfWindow:              ptr(v.IperfWindow),
		SpeedRetries:             ptr(v.SpeedRetries),
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
			SpeedAutoLoc:         in.SpeedAutoLoc,
			SpeedAutoLabel:       in.SpeedAutoLabel,
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
			ExitTarget:           in.ExitTarget,
			SpeedtestAdaptive:    in.SpeedtestAdaptive,
			SpeedtestOnDegraded:  in.SpeedtestOnDegraded,
			DegradedPingMS:       in.DegradedPingMS,
			SpeedtestSkipBusy:    in.SpeedtestSkipBusy,
			SpeedBusyMbps:        in.SpeedBusyMbps,
			SpeedEngine:          in.SpeedEngine,
			IperfServer:          in.IperfServer,
			IperfDur:             in.IperfDur,
			IperfStreams:         in.IperfStreams,
			OoklaConnections:     in.OoklaConnections,
			OoklaLoss:            in.OoklaLoss,
			SpeedBestOf:          in.SpeedBestOf,
			IperfOmit:            in.IperfOmit,
			SpeedDirection:       in.SpeedDirection,
			IperfUDP:             in.IperfUDP,
			IperfUDPRate:         in.IperfUDPRate,
			IperfWindow:          in.IperfWindow,
			SpeedRetries:         in.SpeedRetries,
			IperfCongestion:      in.IperfCongestion,
			IperfNoDelay:         in.IperfNoDelay,
			IperfDSCP:            in.IperfDSCP,
			IperfMSS:             in.IperfMSS,
			ThreshDownMbps:       in.ThreshDownMbps,
			ThreshUpMbps:         in.ThreshUpMbps,
			ThreshPingMS:         in.ThreshPingMS,
			ThreshJitterMS:       in.ThreshJitterMS,
			ThreshLossPct:        in.ThreshLossPct,
			ThreshConsec:         in.ThreshConsec,
			ThreshBloatDownMS:    in.ThreshBloatDownMS,
			ThreshBloatUpMS:      in.ThreshBloatUpMS,
			AlertOnOutage:        in.AlertOnOutage,
			WebhookURL:           in.WebhookURL,
			HeartbeatURL:         in.HeartbeatURL,
			DigestFreq:           in.DigestFreq,
			SchedLatEnabled:      in.SchedLatEnabled,
			SchedLatWindows:      in.SchedLatWindows,
			SchedSpeedEnabled:    in.SchedSpeedEnabled,
			SchedSpeedWindows:    in.SchedSpeedWindows,
		}
		if in.IperfServers != nil { // nil = keep the saved list (and its passwords)
			pat.IperfServers = iperfServersFromDTO(in.IperfServers) // blank pw kept by Update
		}
		v, err := s.settings.Update(r.Context(), pat)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		if err := s.settings.SetMonitoring(r.Context(), in.Enabled); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.log.Info("monitoring toggled", "enabled", in.Enabled)
	}
	writeJSON(w, map[string]bool{"enabled": s.settings.Monitoring()})
}

// dataCategories maps each export/import category to its envelope key and
// table, in a fixed order. config = settings (clean override on import); the
// rest are time-series tables that merge by key on import. The latency
// category spans two tables - the ping samples and the DNS-resolve series -
// bundled the same way Clear and Prune treat them.
var dataCategories = []struct{ cat, key, table string }{
	{"config", "config", "settings"},
	{"latency", "latency", "samples"},
	{"latency", "dns", "dns"},
	{"speed", "speed", "speed"},
	{"downtime", "downtime", "events"},
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
	fmt.Fprintf(bw, `{"pingularity_export":1,"exported_at":%d`, time.Now().Unix())
	for _, dc := range sel {
		fmt.Fprintf(bw, ",%q:[", dc.key)
		first := true
		err := s.store.ExportTableRows(r.Context(), dc.table, func(m map[string]any) error {
			if !first {
				bw.WriteByte(',')
			}
			first = false
			return enc.Encode(m) // row object + newline (valid JSON whitespace inside the array)
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

// maxConcurrentImports caps how many /api/import runs execute at once; a slow or
// stalled body ties up a goroutine and connection, so an unbounded fan-out is a
// cheap resource-exhaustion lever. A handful is plenty (imports are rare, heavy,
// and mutate shared settings).
const maxConcurrentImports = 4

// importGate returns the (lazily built) import concurrency semaphore.
func (s *Server) importGate() chan struct{} {
	s.importSemOnce.Do(func() { s.importSem = make(chan struct{}, maxConcurrentImports) })
	return s.importSem
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
	// Bound concurrency: refuse rather than queue when the gate is full, so a
	// burst of slow uploads can't pile up goroutines/FDs waiting behind each other.
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
	// DoS ceiling only, not a working limit: the stream decode holds one bounded
	// batch at a time, so this just bounds how long a hostile body can hold the
	// endpoint.
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)

	// Envelope check. Our exporter always writes {"pingularity_export":1,...}
	// first, so the version is known before any data row is applied - streaming
	// means rows land as they parse, so this can't wait for the whole file.
	dec := json.NewDecoder(r.Body)
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		http.Error(w, "not a Pingularity export file", http.StatusBadRequest)
		return
	}
	if key, err := dec.Token(); err != nil || key != "pingularity_export" {
		http.Error(w, "not a Pingularity export file", http.StatusBadRequest)
		return
	}
	var ver int
	if err := dec.Decode(&ver); err != nil || ver < 1 {
		http.Error(w, "unrecognized export file version", http.StatusBadRequest)
		return
	}
	if ver > 1 {
		http.Error(w, "this backup is from a newer Pingularity version; update before restoring it", http.StatusBadRequest)
		return
	}

	keyToCat := map[string]struct{ cat, table string }{}
	for _, dc := range dataCategories {
		keyToCat[dc.key] = struct{ cat, table string }{dc.cat, dc.table}
	}
	// Retention window per table, for the about-to-be-pruned warning below
	// (0 = keep forever = nothing to warn about).
	retention := map[string]time.Duration{
		"samples": s.settings.Retention(), "dns": s.settings.Retention(),
		"speed": s.settings.SpeedRetention(), "events": s.settings.DowntimeRetention(),
	}
	result := map[string]int{}
	prunable := map[string]bool{} // category -> imported rows predate its retention window
	importedConfig := false
	// Each category is committed as it is reached (config first in our own
	// exports). A later failure can't roll the earlier ones back, so record it
	// and fall through to the reload/invalidate below instead of returning early
	// - otherwise applied config would sit latent in the DB and silently
	// activate only on the next restart.
	var importErr error
	importStatus := http.StatusInternalServerError
	for importErr == nil && dec.More() {
		resetDeadline() // progress made: rearm before the next (possibly large) value
		keyTok, err := dec.Token()
		if err != nil {
			importErr, importStatus = fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest
			break
		}
		key, _ := keyTok.(string)
		dc, known := keyToCat[key]
		if !known || r.URL.Query().Get(dc.cat) == "" {
			// exported_at, an unselected category, or an unknown key: walk past
			// its value without materializing it.
			if err := skipJSONValue(dec); err != nil {
				importErr, importStatus = fmt.Errorf("invalid JSON: %w", err), http.StatusBadRequest
			}
			continue
		}
		n, old, err := s.importArray(r.Context(), dec, key, dc.table, retention[dc.table], resetDeadline)
		result[dc.cat] += n // latency spans two tables (samples + dns); sum them
		if old {
			prunable[dc.cat] = true
		}
		if dc.cat == "config" {
			importedConfig = true
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			switch {
			case errors.As(err, &maxErr):
				importErr, importStatus = fmt.Errorf("backup exceeds the import size limit (%d GiB)", maxErr.Limit>>30), http.StatusRequestEntityTooLarge
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
	var warnings []string
	// Imported config went straight to the DB; reload so the monitor/scheduler
	// pick it up without a restart. Run this even when a later category failed,
	// so the applied config takes effect immediately (and is logged) rather than
	// lurking until the next restart.
	if importedConfig {
		if err := s.settings.Reload(r.Context()); err != nil {
			s.log.Warn("settings reload after import", "err", err)
		}
		// Backups never carry the password hash (settingsExportDeny), so restoring
		// onto a box without its own password can import auth_enabled=true that
		// nothing can enforce - the login toggle would show ON while every request
		// sails through. Make intent match enforcement, and say so.
		if s.settings.AuthEnabled() && !s.settings.AuthActive() {
			if err := s.settings.SetAuthEnabled(r.Context(), false); err != nil {
				s.log.Warn("disable unenforceable imported auth", "err", err)
			} else {
				warnings = append(warnings, "This backup had login protection on, but backups never include the password - login stays off until you set a new password in the Access tab.")
				s.log.Warn("imported config had login enabled but no password can come with a backup; login left off")
			}
		}
	}
	if result["downtime"] > 0 || result["speed"] > 0 {
		s.invalidateAggregates() // the uptime/data pills must not serve pre-import numbers for another aggTTL
	}
	if importErr != nil {
		s.log.Warn("data import failed after partial apply", "result", result, "err", importErr)
		http.Error(w, importErr.Error(), importStatus)
		return
	}
	for _, cat := range []string{"latency", "speed", "downtime"} { // stable order
		if prunable[cat] {
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
// applied and whether any parsed row predates the retention window - a restored
// row older than the cutoff comes back only to be pruned within the hour, which
// reads as a broken restore unless the response says so.
func (s *Server) importArray(ctx context.Context, dec *json.Decoder, key, table string, window time.Duration, onProgress func()) (n int, prunable bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, false, fmt.Errorf("bad %s data: %w", key, err)
	}
	if tok != json.Delim('[') {
		return 0, false, fmt.Errorf("bad %s data: expected an array", key)
	}
	var cutoff int64
	if window > 0 {
		cutoff = time.Now().Add(-window).Unix()
	}
	const batchRows = 5000 // = store.importTxRows: one handler batch per store transaction
	batch := make([]map[string]any, 0, batchRows)
	var batchBytes int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		applied, ierr := s.store.ImportTable(ctx, table, batch)
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
			return n, prunable, fmt.Errorf("bad %s data: %w", key, derr)
		}
		var row map[string]any
		if derr := json.Unmarshal(raw, &row); derr != nil {
			return n, prunable, fmt.Errorf("bad %s data: %w", key, derr)
		}
		if cutoff > 0 {
			if ts, ok := row["ts"].(float64); ok && int64(ts) < cutoff {
				prunable = true
			}
		}
		batch = append(batch, row)
		batchBytes += len(raw)
		if len(batch) >= batchRows || batchBytes >= importBatchBytes {
			if ferr := flush(); ferr != nil {
				return n, prunable, ferr
			}
		}
	}
	if _, terr := dec.Token(); terr != nil { // closing ']'
		return n, prunable, fmt.Errorf("bad %s data: %w", key, terr)
	}
	return n, prunable, flush()
}

// importBatchBytes flushes an import batch once its decoded rows exceed this many
// bytes, even below batchRows, so peak memory tracks bytes not just row count.
const importBatchBytes = 8 << 20

// importReadWindow is how long an import may stall between decoded batches before
// its connection is reaped (see handleImport's per-progress deadline).
const importReadWindow = 2 * time.Minute

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
func skipJSONValue(dec *json.Decoder) error { return skipJSONValueDepth(dec, 0) }

func skipJSONValueDepth(dec *json.Decoder, depth int) error {
	if depth > maxImportDepth {
		return fmt.Errorf("bad import data: JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); ok && (d == '{' || d == '[') {
		for dec.More() {
			if err := skipJSONValueDepth(dec, depth+1); err != nil {
				return err
			}
		}
		_, err = dec.Token() // the matching closing delimiter
		return err
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
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

// handleLogs backs the About-tab log viewer. GET returns {level, redact, lines}
// (the current on/off level, PII-redaction flag, and the in-memory tail); POST
// {level, redact} sets logging and PII redaction (persisted + applied live via
// the settings broadcast) and {clear:true} empties the buffer. Either way it
// returns the fresh {level, redact, lines}.
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
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			s.log.Info("log level changed", "level", s.settings.LogLevel())
		}
		if in.Redact != nil {
			if err := s.settings.SetLogRedactPII(r.Context(), *in.Redact); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
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
	lines := []logbuf.Entry{}
	if s.Logs != nil {
		lines = s.Logs.Entries()
	}
	writeJSON(w, map[string]any{"level": s.settings.LogLevel(), "redact": s.settings.LogRedactPII(), "lines": lines})
}

// handleMetrics emits a minimal Prometheus exposition so an existing
// Prometheus/Grafana stack can scrape Pingularity directly.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.status == nil { // not wired (only happens in misconfiguration/tests) - degrade, don't panic
		http.Error(w, "status unavailable", http.StatusServiceUnavailable)
		return
	}
	st := s.status()
	targets, terr := s.store.LatestPerTarget(ctx, s.targetGrace())
	if terr != nil {
		s.log.Debug("metrics read failed", "op", "latest_per_target", "err", terr)
	}
	uptime, dataBytes, avgDownB, avgUpB, usage := s.aggregates()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# HELP pingularity_build_info Build metadata; constant 1, version and Go toolchain in the labels.")
	fmt.Fprintln(w, "# TYPE pingularity_build_info gauge")
	fmt.Fprintf(w, "pingularity_build_info{version=%q,goversion=%q} 1\n", s.version, runtime.Version())

	fmt.Fprintln(w, "# HELP pingularity_runtime_seconds Process uptime in seconds.")
	fmt.Fprintln(w, "# TYPE pingularity_runtime_seconds gauge")
	fmt.Fprintf(w, "pingularity_runtime_seconds %d\n", int(time.Since(s.started).Seconds()))

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
		if !st.Online { // length of the outage in progress; absent (not 0) while up
			fmt.Fprintln(w, "# HELP pingularity_current_outage_seconds Length of the outage in progress (seconds); absent while online.")
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
		fmt.Fprintf(w, "pingularity_family_up{family=%q} %d\n", f.Family, util.B2I(f.Online))
	}
	fmt.Fprintln(w, "# HELP pingularity_family_latency_seconds Per-family latency, lowest across that family's anchors (only while the family is online).")
	fmt.Fprintln(w, "# TYPE pingularity_family_latency_seconds gauge")
	for _, f := range st.Families {
		if f.Online { // an offline family has no latency reading; skip rather than emit 0
			fmt.Fprintf(w, "pingularity_family_latency_seconds{family=%q} %g\n", f.Family, f.LatencyMS/1000.0)
		}
	}
	// When this family's current up/down state began - the per-family sibling of
	// state_since, so an operator can measure how long an IPv6-only outage has
	// run (the v4_only/v6_only_down_s counters give totals, not this episode).
	fmt.Fprintln(w, "# HELP pingularity_family_state_since_timestamp_seconds When this family's current up/down state began (unix seconds).")
	fmt.Fprintln(w, "# TYPE pingularity_family_state_since_timestamp_seconds gauge")
	for _, f := range st.Families {
		if !f.Since.IsZero() {
			fmt.Fprintf(w, "pingularity_family_state_since_timestamp_seconds{family=%q} %d\n", f.Family, f.Since.Unix())
		}
	}

	fmt.Fprintln(w, "# HELP pingularity_target_up Last probe success per target (1/0).")
	fmt.Fprintln(w, "# TYPE pingularity_target_up gauge")
	for _, t := range targets {
		fmt.Fprintf(w, "pingularity_target_up{target=%q} %d\n", t.Target, util.B2I(t.Success))
	}
	fmt.Fprintln(w, "# HELP pingularity_target_latency_seconds Last probe latency per target (only for a successful probe; a down target has no reading, not 0).")
	fmt.Fprintln(w, "# TYPE pingularity_target_latency_seconds gauge")
	best, haveBest := 0.0, false
	for _, t := range targets {
		if t.Success { // a failed probe has no latency; emitting 0 would read as a fast link
			fmt.Fprintf(w, "pingularity_target_latency_seconds{target=%q} %g\n", t.Target, t.LatencyMS/1000.0)
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

	fmt.Fprintln(w, "# HELP pingularity_uptime_ratio Fraction of time the link was up (from debounced outage events).")
	fmt.Fprintln(w, "# TYPE pingularity_uptime_ratio gauge")
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"6h\"} %g\n", uptime.H6)
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"24h\"} %g\n", uptime.H24)
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"7d\"} %g\n", uptime.D7)
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"30d\"} %g\n", uptime.D30)
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"1y\"} %g\n", uptime.Y1)
	fmt.Fprintf(w, "pingularity_uptime_ratio{window=\"all\"} %g\n", uptime.All)

	fmt.Fprintln(w, "# HELP pingularity_speed_data_used_bytes Cumulative speedtest data transferred (down+up) within retention.")
	fmt.Fprintln(w, "# TYPE pingularity_speed_data_used_bytes gauge")
	fmt.Fprintf(w, "pingularity_speed_data_used_bytes %d\n", dataBytes)
	// Per-window usage (same windows as uptime_ratio). Pruning makes the total
	// above non-monotonic, so a metered-link operator can't derive "this month's
	// speedtest data" from it - serve the windowed numbers the dashboard already
	// computes.
	fmt.Fprintln(w, "# HELP pingularity_speed_data_used_window_bytes Speedtest data transferred (down+up) within the window.")
	fmt.Fprintln(w, "# TYPE pingularity_speed_data_used_window_bytes gauge")
	for _, wv := range []struct {
		w string
		v int64
	}{{"6h", usage.H6}, {"24h", usage.H24}, {"7d", usage.D7}, {"30d", usage.D30}, {"1y", usage.Y1}} {
		fmt.Fprintf(w, "pingularity_speed_data_used_window_bytes{window=%q} %d\n", wv.w, wv.v)
	}
	if avgDownB > 0 || avgUpB > 0 { // absent before any run, like the other speed readings - never a fake 0
		fmt.Fprintln(w, "# HELP pingularity_speed_avg_run_bytes Average bytes per speedtest run (recent runs), by direction; absent before any run.")
		fmt.Fprintln(w, "# TYPE pingularity_speed_avg_run_bytes gauge")
		fmt.Fprintf(w, "pingularity_speed_avg_run_bytes{direction=\"down\"} %d\n", avgDownB)
		fmt.Fprintf(w, "pingularity_speed_avg_run_bytes{direction=\"up\"} %d\n", avgUpB)
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

	if sp, serr := s.store.LatestSpeed(ctx); serr != nil {
		s.log.Debug("metrics read failed", "op", "latest_speed", "err", serr)
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
		fmt.Fprintf(w, "pingularity_speed_info{engine=%q} 1\n", engine)
		if sp.Healthy != nil { // the in-app threshold verdict, so alerts reuse it instead of re-encoding thresholds
			fmt.Fprintln(w, "# HELP pingularity_speed_healthy Last speedtest passed its configured thresholds (1/0); absent when no thresholds are configured or the run measured nothing they cover.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_healthy gauge")
			fmt.Fprintln(w, "pingularity_speed_healthy", util.B2I(*sp.Healthy))
		}
		if sp.DownBytes != nil || sp.UpBytes != nil {
			// What THIS run consumed (the retention-wide totals above can't answer
			// that) - the number a metered-link operator alerts on per run.
			fmt.Fprintln(w, "# HELP pingularity_speed_last_run_bytes Data the last speedtest transferred, by direction; absent when the engine did not measure it.")
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
			fmt.Fprintln(w, "# HELP pingularity_speed_ping_ms Last measured speedtest latency (ms); absent when the run did not probe latency.")
			fmt.Fprintln(w, "# TYPE pingularity_speed_ping_ms gauge")
			fmt.Fprintf(w, "pingularity_speed_ping_ms %g\n", sp.PingMS)
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
		if sp.LoadedDownMaxMS != nil || sp.LoadedUpMaxMS != nil {
			fmt.Fprintln(w, "# HELP pingularity_speed_loaded_latency_max_ms Worst-case latency during the speedtest load phase, by direction (ms).")
			fmt.Fprintln(w, "# TYPE pingularity_speed_loaded_latency_max_ms gauge")
			if sp.LoadedDownMaxMS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_max_ms{direction=\"down\"} %g\n", *sp.LoadedDownMaxMS)
			}
			if sp.LoadedUpMaxMS != nil {
				fmt.Fprintf(w, "pingularity_speed_loaded_latency_max_ms{direction=\"up\"} %g\n", *sp.LoadedUpMaxMS)
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

	writeStatMetrics(w)
}

// writeStatMetrics dumps the internal stats registry into the exposition. It
// holds a dynamic set of dotted names (monitor.blips, speed.run.scheduled,
// notify.discord.ok, …), so rather than minting a metric per key they share two
// stable families keyed by a `stat` label: monotonic accumulators (counters +
// float sums) under pingularity_stat_total, and gauges under pingularity_stat.
// The registry is always-on and never drained, so these are honest Prometheus
// counters.
func writeStatMetrics(w io.Writer) {
	snap := stats.Lifetime()
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
			fmt.Fprintf(w, "pingularity_stat_total{stat=%q} %g\n", k, totals[k])
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
			fmt.Fprintf(w, "pingularity_stat{stat=%q} %g\n", k, g[k])
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
		strings.HasPrefix(name, "db."):
		return true
	case name == "web.login_fail" || name == "web.limiter_trips":
		return true // security signals an operator wants (failed logins, throttle trips)
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

// speedRunningServer returns the in-progress run's server, or "" when idle.
func speedRunningServer(t SpeedTrigger) string {
	if t == nil || !t.Running() {
		return ""
	}
	return t.CurrentServer()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
	const maxLimit = 1000
	limit = defLimit
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		if n > maxLimit {
			n = maxLimit
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
