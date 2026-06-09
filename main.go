// Command pingularity monitors internet connectivity, serves a built-in dashboard
// plus Prometheus metrics, and can install itself as a background service.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata" // embedded IANA zone db (~400 KiB): the heatmap buckets days in the viewer's timezone, and Windows/scratch containers ship no zoneinfo files

	"github.com/kardianos/service"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/digest"
	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/logfilter"
	"github.com/pingular/pingularity/internal/monitor"
	"github.com/pingular/pingularity/internal/netinfo"
	"github.com/pingular/pingularity/internal/netstat"
	"github.com/pingular/pingularity/internal/notify"
	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/secret"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/update"
	"github.com/pingular/pingularity/internal/util"
	"github.com/pingular/pingularity/internal/web"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// busySampleWindow is how long the busy-defer check samples interface counters.
// Short enough not to delay a due test, long enough to smooth bursty traffic.
const busySampleWindow = 700 * time.Millisecond

// logBufferLines is how many recent log lines the in-memory ring keeps for the
// dashboard's log viewer; older lines drop off. Sized to hold a useful window
// even at debug (one probe round is a couple of summary lines, not ~11). The
// ring is also snapshotted to disk so it survives a restart (see ringPath).
const logBufferLines = 4000

// ringPath returns where the log ring is snapshotted (a "logs.txt" sibling of the
// DB file), or "" for an in-memory DB - nothing durable to pair it with. The
// memory test is the store's exact ":memory:" (store.Open at internal/store), not
// a substring: a path like "<dir>/:memory:" is a real on-disk file there, so it
// gets a real snapshot here too rather than silently losing its history.
func ringPath(dbPath string) string {
	if dbPath == "" || dbPath == ":memory:" {
		return ""
	}
	return filepath.Join(filepath.Dir(dbPath), "logs.txt")
}

func main() {
	args := os.Args[1:]
	cmd := "run"
	switch {
	case len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help"):
		// Would otherwise be treated as a run flag (isFlag), never reaching the
		// curated help below.
		cmd = "help"
	case len(args) > 0 && !isFlag(args[0]):
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "run":
		fail(runCmd(args))
	case "install", "uninstall", "start", "stop", "restart", "status":
		fail(controlCmd(cmd, args))
	case "reset-auth":
		fail(resetAuthCmd(args))
	case "version":
		fmt.Println("pingularity", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "pingularity: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

// program implements service.Interface so the same code runs in the foreground
// and under systemd/launchd/Windows service managers.
type program struct {
	cfg      config.Config
	log      *slog.Logger
	logLevel *slog.LevelVar // runtime-adjustable verbosity (set live from the UI)
	ring     *logbuf.Ring   // in-memory tail of recent log lines for the dashboard
	store    *store.Store
	cancel   context.CancelFunc
	done     chan struct{}
}

func (p *program) Start(s service.Service) error {
	st, err := store.Open(p.cfg.DBPath)
	if err != nil {
		return err
	}
	p.store = st

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go p.run(ctx)
	return nil
}

func (p *program) run(ctx context.Context) {
	// A daemon whose web server died has no dashboard, API, or /metrics - it must
	// exit non-zero so the service manager restarts/flags it instead of it running
	// headless forever. Registered first so by LIFO it runs last, after the store
	// is closed and the workers are drained.
	var webFailed atomic.Bool
	defer func() {
		if webFailed.Load() {
			os.Exit(1)
		}
	}()
	defer close(p.done)
	defer p.store.Close()
	// All background workers run on ctx and touch the store, so on shutdown we
	// must wait for them to stop before closing the DB - else a final InsertSpeed
	// / Prune / status read hits a closed handle. Registered AFTER store.Close,
	// so by LIFO this runs FIRST: wait, then close, then signal done. Bounded so a
	// wedged worker can't hang shutdown forever.
	var bg sync.WaitGroup
	spawn := func(fn func()) {
		bg.Add(1)
		go func() {
			defer bg.Done()
			// A panic in one loop must not crash the daemon (its job is to not miss
			// outages). Recover, log loudly, and let the other loops keep running.
			defer func() {
				if r := recover(); r != nil {
					p.log.Error("background worker panicked", "panic", r, "stack", string(debug.Stack()))
				}
			}()
			fn()
		}()
	}
	defer func() {
		done := make(chan struct{})
		go func() { bg.Wait(); close(done) }()
		// Bounded below Stop's 5s wait on p.done so the close normally finishes
		// first. Loops return almost instantly on cancel; only an in-flight
		// speedtest can approach this, and it honors ctx. Even if a worker wedges,
		// skipping the graceful close is safe - the WAL is crash-consistent.
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			p.log.Warn("shutdown: background workers did not stop in time; closing store anyway")
		}
	}()

	// Records raised before the log level is known cannot be emitted yet: the
	// handler starts pinned to logLevelOff (see run) and would drop them, which
	// is why the critical ones below also go straight to stderr. Hold the
	// structured copies and replay them once applyLogLevel has run, so they also
	// reach the RING - the ring is what the dashboard log viewer shows and what
	// /api/logs?download puts in a bug report, and stderr never gets there.
	var earlyLog []func()
	earlyLog = append(earlyLog, func() {
		p.log.Info("pingularity started",
			"version", version, "db", p.cfg.DBPath,
			"interval", p.cfg.Interval.String(), "listen", p.cfg.ListenAddr)
	})

	// Runtime-adjustable settings (UI + persisted), seeded from config.
	// Local-only is a sensible default for a native install (LAN access is opt-in),
	// but inside a container the dashboard is only reachable over the network, and
	// the filter can't be enforced there anyway - so default it off in containers.
	containerized := util.InContainer()
	def := defaultSettings(p.cfg, containerized)
	// Seal the stored iperf3 passwords at rest (key file beside the DB, 0600). If the key
	// can't be opened we carry on WITHOUT encryption rather than refuse to run: monitoring
	// is the job, and a legible warning beats a dead daemon.
	var setOpts []settings.Option
	if box, err := secret.New(p.cfg.DBPath); err != nil {
		// Straight to stderr, bypassing the log level: this runs before settings
		// load, so the level is still "off" and a plain Error would be swallowed
		// on every install - and this is a silent security degradation.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: secret key unavailable; iperf3 passwords will be stored in the clear: %v\n", err)
		err := err
		earlyLog = append(earlyLog, func() {
			p.log.Error("secret key unavailable; iperf3 passwords will be stored in the clear", "err", err)
		})
	} else {
		setOpts = append(setOpts, settings.WithCrypter(box))
	}
	set, err := settings.New(ctx, p.store, def, setOpts...)
	if errors.Is(err, settings.ErrLegacyReseal) {
		// Settings loaded and are in effect; only re-encrypting old plaintext
		// iperf3 passwords failed. Warning "using defaults" here would tell the
		// operator their config was ignored when it wasn't.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: %v\n", err)
		err := err
		earlyLog = append(earlyLog, func() { p.log.Warn("legacy iperf3 password re-encryption failed; settings loaded", "err", err) })
	} else if err != nil {
		// New still returns a controller seeded with defaults, so log and proceed
		// rather than crash on a transient read error. Same pre-level stderr
		// treatment as above.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: could not load settings; using defaults: %v\n", err)
		err := err
		earlyLog = append(earlyLog, func() { p.log.Error("load settings; using defaults", "err", err) })
	}

	// Apply the saved log level now that settings are loaded (the logger starts
	// pinned to logLevelOff so "off" is truly silent), and keep it synced: the
	// level rides the settings broadcast, so a change from the dashboard or a
	// SIGHUP reload re-applies here without a restart.
	applyLogLevel(p.logLevel, set.LogLevel())
	// Now replay what happened before the level was known. With logging off these
	// are dropped exactly as any other record would be; with it on, the log finally
	// carries the version that produced it.
	for _, emit := range earlyLog {
		emit()
	}
	earlyLog = nil
	spawn(func() {
		for {
			// Fetch the channel BEFORE reading values (like the monitor and the
			// speedtest scheduler): a change landing after the read closes the
			// channel we already hold, so no update can slip between read and wait.
			ch := set.Changed()
			applyLogLevel(p.logLevel, set.LogLevel())
			select {
			case <-ctx.Done():
				return
			case <-ch:
			}
		}
	})

	// Snapshot the log ring to disk every minute and once on shutdown, so the
	// dashboard's log viewer keeps its recent history across a restart (journald
	// still has the full stream; this just restores the in-app tail).
	if rp := ringPath(p.cfg.DBPath); rp != "" {
		spawn(func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			save := func() {
				if err := p.ring.SaveFile(rp); err != nil {
					p.log.Debug("log ring snapshot failed", "err", err)
				}
			}
			for {
				select {
				case <-ctx.Done():
					save() // final snapshot before exit
					return
				case <-t.C:
					save()
				}
			}
		})
	}

	// One-line effective-config dump so the daemon is self-describing across restarts
	// ("what's actually in effect"). Secrets (webhook/heartbeat URLs, auth hash, iperf
	// passwords) are deliberately omitted - only operational shape, never credentials.
	p.log.Info("effective config",
		"targets", len(p.cfg.Targets), "interval", set.LatencyInterval(), "timeout", set.Timeout(),
		"down_after", set.DownAfter(), "up_after", set.UpAfter(),
		"latency", set.LatencyEnabled(), "dns_probe", set.DNSProbe(), "netinfo", set.NetinfoEnabled(),
		"ipv4_mode", p.cfg.IPv4Mode, "ipv6_mode", set.IPv6Mode(),
		"monitoring", set.Monitoring(), "speedtest", set.SpeedtestEnabled(),
		"speed_engine", set.SpeedEngine(), "speed_interval", set.SpeedInterval(),
		"thresholds", set.Thresholds().Any(), "auth", set.AuthEnabled(),
		"access_local_only", set.AccessLocalOnly())

	// Warn when the control plane is actually reachable from the network without
	// auth: a non-loopback listen AND the loopback filter off (native installs
	// default the filter ON, so a fresh install is quiet; containers default it
	// off - the filter is unenforceable there - and do warn).
	if nonLoopbackListen(p.cfg.ListenAddr) && !set.AccessLocalOnly() && !set.AuthActive() {
		// Also straight to stderr, bypassing the log level: the default install
		// runs with logging "off", and this warning targets exactly that install.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: dashboard reachable on the network with authentication OFF (listen %s)\n"+
			"  set a password in the UI, or use -listen 127.0.0.1:9000 to bind loopback only\n", p.cfg.ListenAddr)
		p.log.Warn("dashboard reachable on the network with authentication OFF",
			"listen", p.cfg.ListenAddr,
			"hint", "set a password in the UI, or use -listen 127.0.0.1:9000 to bind loopback only")
	}

	// "Local only" is enforced on the TCP peer, and visitors arriving through a
	// same-host reverse proxy (what -allow-host declares) all look local, so the
	// two settings contradict each other. Same stderr treatment as above.
	if set.AccessLocalOnly() && p.cfg.AllowedHosts != "" {
		fmt.Fprintln(os.Stderr, "pingularity: WARNING: 'local only' access is on, but -allow-host declares a reverse proxy;"+
			"\n  visitors through the proxy arrive as local connections and are NOT blocked - enable authentication instead")
		p.log.Warn("'local only' access cannot block visitors arriving through the -allow-host reverse proxy",
			"hint", "enable authentication instead")
	}

	// A reload signal (SIGHUP on Unix; `systemctl reload` works too - the unit
	// gets ExecReload via the ReloadSignal option, see svcopts_other.go) reloads
	// persisted settings live, picking up an out-of-band change like
	// `pingularity reset-auth` without a restart. Windows has no such signal
	// (reloadSignals returns nil there), so the goroutine is skipped entirely.
	if sigs := reloadSignals(); len(sigs) > 0 {
		spawn(func() {
			hup := make(chan os.Signal, 1)
			signal.Notify(hup, sigs...)
			defer signal.Stop(hup)
			for {
				select {
				case <-ctx.Done():
					return
				case <-hup:
					if err := set.Reload(ctx); err != nil {
						p.log.Warn("settings reload on signal", "err", err)
					} else {
						p.log.Info("settings reloaded on signal")
					}
				}
			}
		})
	}

	pr := prober.New(p.cfg.Targets, p.cfg.Timeout)
	pr.TimeoutFn = set.Timeout
	// Family probing: "on"/"off" force a family; "auto" probes it only while the
	// host actually has that family (re-checked periodically, so an address coming
	// or going mid-run is picked up without a restart). IPv6 mode is a live
	// setting; IPv4 mode comes from the -ipv4 flag.
	famMode := func(fam string) string {
		if fam == config.IPv6 {
			return set.IPv6Mode()
		}
		return p.cfg.IPv4Mode
	}
	pr.FamilyEnabledFn = func(fam string) bool {
		switch famMode(fam) {
		case "on":
			return true
		case "off":
			return false
		}
		// auto
		if fam == config.IPv6 {
			return prober.HasGlobalIPv6Cached()
		}
		return prober.HasGlobalIPv4Cached()
	}
	// Explicitly-off families are never dialed, and never pulled back in by the
	// prober's fail-open (which exists for "auto" transients, not for overriding
	// the operator).
	pr.FamilyOffFn = func(fam string) bool { return famMode(fam) == "off" }
	// _mode keys, not "ipv6": that key is reserved for a real address
	// (netinfo logs one under it) and the redactor censors it by key, so a mode
	// string logged there reads as [redacted] and tells support nothing.
	p.log.Info("family probing", "ipv4_mode", p.cfg.IPv4Mode, "ipv6_mode", set.IPv6Mode())
	m := monitor.New(p.cfg, pr, p.store, p.log)
	m.IntervalFn = set.LatencyInterval
	m.WakeFn = set.Changed
	m.DownAfterFn = set.DownAfter
	m.UpAfterFn = set.UpAfter
	m.EnabledFn = set.Monitoring
	m.LatencyFn = func() bool {
		// Both families explicitly off = nothing the operator allows us to dial;
		// idle (like the latency toggle being off) rather than probe disabled
		// anchors and manufacture a false outage. "auto" absence does NOT idle:
		// a vanished interface address may BE the outage, so those rounds still
		// run (and fail) against the eligible set.
		if famMode(config.IPv4) == "off" && famMode(config.IPv6) == "off" {
			return false
		}
		return set.LatencyEnabled() && set.LatencyAllowed(time.Now())
	}
	m.DNSFn = set.DNSProbe

	// Connection info (public IP / ISP / geo / DNS), refreshed periodically.
	ni := netinfo.NewManager(p.log)
	// Last-known fallback: if a live lookup fails and the in-memory cache is empty,
	// reuse the most recent speedtest run's recorded IP/ISP/DNS so the panel still
	// shows something.
	ni.LastKnownFn = func() *netinfo.Info {
		sp, err := p.store.LatestConnInfo(context.Background())
		if err != nil || sp == nil {
			return nil
		}
		i := &netinfo.Info{PublicIP: sp.PublicIPv4, PublicIPv6: sp.PublicIPv6, ISP: sp.ISP}
		// ISPLocation was joined with empty parts skipped, so it can't be split back
		// positionally. It's only ever re-joined for display, so stash the whole
		// string in City.
		i.City = sp.ISPLocation
		if sp.DNSIP != "" {
			i.DNSUpstream = &netinfo.DNSEntry{IP: sp.DNSIP, Provider: sp.DNSProvider, Location: sp.DNSLocation}
		}
		return i
	}
	ni.ExitTargetFn = set.ExitTarget // user-chosen destination for exit-router discovery
	// Connection info is a measurement like any other, so a paused monitor stops
	// making these lookups too - and the setting turns them off for good. Without
	// this the hourly loop kept sending the public IP to third parties while the
	// dashboard said monitoring was paused.
	ni.EnabledFn = func() bool { return set.Monitoring() && set.NetinfoEnabled() }
	// Speedtests/reconnects refresh connection info; this loop is just a backstop
	// to keep it no staler than an hour when those don't fire.
	spawn(func() { ni.Loop(ctx, time.Hour) })

	// Background poll for a newer release (see internal/update). Best-effort: honors
	// the toggle, skips dev builds, and a dead endpoint is a no-op. Status surfaces
	// on /api/status; the About tab lights a cue.
	upd := update.New(version, set.UpdateCheckEnabled, p.log)
	spawn(func() { upd.Loop(ctx) })

	// Alert delivery (webhook) + dead-man's-switch heartbeat.
	notifier := notify.New(set.WebhookURL, p.log)
	spawn(func() { p.runHeartbeat(ctx, set) })

	// Periodic health digest (off/daily/weekly) to the same webhook.
	dig := &digest.Manager{Store: p.store, Notify: notifier, Log: p.log, FreqFn: set.DigestFreq, EnabledFn: set.Monitoring}
	spawn(func() { dig.Loop(ctx) })

	// Speedtest scheduler - always constructed so it can be toggled live.
	tester := speedtest.NewOokla()
	tester.ServerIDFn = set.SpeedServerID // honor the chosen server, live
	// Auto server selection centres on, in order: a city the user searched, then
	// the ISP's exit-router location (the last hop inside the ISP - a truer network
	// origin than the public IP's geolocation), then the Cloudflare PoP (the same
	// idea where a traceroute exit can't run - containers, non-Linux), then the
	// public IP itself.
	tester.AutoLocFn = func() (lat, lon float64, ok bool) {
		if lat, lon, ok := set.AutoLocation(); ok {
			return lat, lon, true
		}
		if ex := ni.Get().Exit; ex != nil && (ex.Lat != 0 || ex.Lon != 0) {
			return ex.Lat, ex.Lon, true
		}
		if _, clat, clon, ok := netinfo.ColoCoord(ni.Get().CFColo); ok {
			return clat, clon, true
		}
		return 0, 0, false
	}
	// The user's ISP name grants its own server a guaranteed lane in the
	// auto-select ping race (an on-net server is the most likely winner). A
	// stale or empty name is harmless - it only adds or skips one racer.
	tester.ISPFn = func() string { return ni.Get().ISP }
	sched := speedtest.NewScheduler(tester, p.store, p.cfg.SpeedtestInterval, p.log)
	tester.OnServer = sched.SetCurrentServer    // surface the live server during a run
	tester.DirectionFn = set.SpeedDirection     // engine-agnostic: which directions to run
	tester.RetriesFn = set.SpeedRetries         // engine-agnostic: retry count
	tester.ConnectionsFn = set.OoklaConnections // Ookla parallel connections (0 = auto)
	tester.LossFn = set.OoklaLoss               // Ookla packet-loss probe
	tester.BestOfFn = set.SpeedBestOf           // race 3 servers, keep the best (scheduled/manual only)
	tester.Log = p.log
	// iperf3 engine: tests against the user's own iperf3 server. Only selected when
	// the binary is on PATH (otherwise we fall back to Ookla), so a missing
	// dependency degrades gracefully.
	iperfTester := &speedtest.Iperf{
		Log:          p.log,
		ServerFn:     set.IperfServer,
		LabelFn:      set.IperfLabel,
		DurationFn:   set.IperfDur,
		StreamsFn:    set.IperfStreams,
		OmitFn:       set.IperfOmit,
		DirectionFn:  set.SpeedDirection,
		UDPFn:        set.IperfUDP,
		UDPRateFn:    set.IperfUDPRate,
		BindFn:       set.IperfBind,
		WindowFn:     set.IperfWindow,
		IPVersionFn:  set.IperfIPVer,
		RetriesFn:    set.SpeedRetries,
		CongestionFn: set.IperfCongestion,
		NoDelayFn:    set.IperfNoDelay,
		DSCPFn:       set.IperfDSCP,
		MSSFn:        set.IperfMSS,
		AuthFn:       set.IperfAuth,
		UsernameFn:   set.IperfUsername,
		PasswordFn:   set.IperfPassword,
		RSAKeyFn:     set.IperfRSAKey,
		PKCS1Fn:      set.IperfPKCS1,
	}
	iperfTester.OnServer = sched.SetCurrentServer // surface the live server during an iperf3 run
	sched.TesterFn = func() speedtest.Tester {
		if set.SpeedEngine() == "iperf3" && speedtest.IperfAvailable() {
			return iperfTester
		}
		return tester
	}
	sched.IntervalFn = set.SpeedInterval
	sched.WakeFn = set.Changed
	sched.EnabledFn = func() bool {
		return set.Monitoring() && set.SpeedtestEnabled() && set.SpeedAllowed(time.Now())
	}
	sched.ThresholdsFn = set.Thresholds      // mark each run healthy/unhealthy
	sched.BreachStreakFn = set.BreachStreak  // debounce: alert only after N consecutive breaches
	sched.AdaptiveFn = set.SpeedtestAdaptive // shorten the interval while the last run breached
	sched.OnUnhealthy = func(sp store.SpeedSample, failures []string) {
		notifier.SpeedThreshold(ctx, sp, failures)
	}
	// Busy-defer: skip a scheduled run while the link is already moving data. Off ⇒
	// don't sample at all; on ⇒ sample the busiest interface briefly and compare to
	// the configured Mbps. Only scheduled runs consult this; reconnect/degraded/
	// manual runs go regardless.
	sched.BusyFn = func() bool {
		if !set.SpeedtestSkipBusy() {
			return false
		}
		mbps, ok := netstat.Throughput(ctx, busySampleWindow)
		return ok && mbps > set.SpeedBusyMbps()
	}
	// After each speedtest, re-check IP/ISP/DNS and record it with the result.
	sched.ConnInfoFn = func(c context.Context) speedtest.ConnInfo {
		ni.Refresh(c)
		i := ni.Get()
		// Refresh is the AUTOMATIC path: when connection info is off or monitoring
		// is paused it makes no request and leaves the cache in place. A manual
		// speedtest can still run in that state (RUN is ungated), so stamping the
		// cached identity would record a possibly-stale IP/ISP/exit as this run's
		// context. Only stamp an identity we could actually have just refreshed.
		if !set.Monitoring() || !set.NetinfoEnabled() || i.UpdatedAt == 0 {
			return speedtest.ConnInfo{}
		}
		if i.CarriedIdentity() {
			// The snapshot's identity is a carried-forward last-known (the live IP
			// echo failed): don't stamp it into the run as if it were current. A
			// snapshot whose only failure is the ISP lookup keeps its fresh
			// IP/DNS/colo and IS recorded.
			return speedtest.ConnInfo{}
		}
		ci := speedtest.ConnInfo{
			PublicIPv4: i.PublicIP, PublicIPv6: i.PublicIPv6,
			ISP: i.ISP, ISPLocation: joinNonEmpty(i.City, i.Country),
		}
		if i.DNSUpstream != nil {
			ci.DNSIP, ci.DNSProvider, ci.DNSLocation = i.DNSUpstream.IP, i.DNSUpstream.Provider, i.DNSUpstream.Location
		}
		ci.CFColo, ci.ExitSummary = i.CFColo, exitSummary(i.Exit)
		return ci
	}
	spawn(func() { sched.Loop(ctx) })

	// On reconnect: re-check public IP/DNS (it may have changed) and, if enabled,
	// run a speedtest. These monitor callbacks fire synchronously from the monitor
	// loop, and the work they spawn touches the store - so route it through spawn()
	// (not a bare `go`) so the shutdown drain waits for it before store.Close(). All
	// of it honors ctx, so a cancelled refresh/run/alert returns inside the bound.
	m.OnReconnect = func() {
		spawn(func() { ni.Refresh(ctx) })
		// Independent of the Automatic (scheduled) toggle: run as long as monitoring
		// (power) and on-reconnect are both on. The speed schedule (quiet hours) is
		// still honored via SpeedAllowed.
		if set.Monitoring() && set.SpeedtestOnReconnect() && set.SpeedAllowed(time.Now()) {
			spawn(func() { sched.RunOnce(ctx, "reconnect") })
		}
	}
	// Degradation-triggered speedtest: when base latency stays high without fully
	// dropping, capture it with a test. DegradedPingFn returns 0 (detection off)
	// unless the feature, speedtests, and monitoring are all on; OnDegraded still
	// honors the speed schedule window.
	m.DegradedPingFn = func() float64 {
		if !set.SpeedtestOnDegraded() || !set.SpeedtestEnabled() || !set.Monitoring() {
			return 0
		}
		return set.DegradedPingMS()
	}
	m.OnDegraded = func() {
		if set.SpeedAllowed(time.Now()) {
			spawn(func() { sched.RunOnce(ctx, "degraded") })
		}
	}
	// Alert on every confirmed up/down transition, when enabled. Delivery is
	// serialized (notifier.Outage holds a lock across retries so a flap's up
	// can't overtake its down), so a dead webhook makes each alert live for the
	// full retry schedule. Bound the backlog: past a small cap the webhook is
	// clearly wedged and these alerts are undeliverable anyway, so drop rather
	// than pile up goroutines through a sustained flapping outage.
	var outageBacklog atomic.Int32
	m.OnTransition = func(online bool, durationS int) {
		if !set.AlertOnOutage() {
			return
		}
		if outageBacklog.Load() >= 8 {
			p.log.Warn("outage alert dropped: notifier backlog full (webhook unreachable?)", "online", online)
			return
		}
		outageBacklog.Add(1)
		spawn(func() {
			defer outageBacklog.Add(-1)
			notifier.Outage(ctx, online, durationS)
		})
	}

	spawn(func() { p.runPruner(ctx, set) })

	srv := web.New(p.store, func() web.LiveStatus {
		s := m.Snapshot()
		ls := web.LiveStatus{Online: s.Online, Since: s.Since, Paused: s.Paused, Probing: s.Probing, DNSms: s.DNSms, DNSok: s.DNSok, DNSactive: s.DNSactive}
		for _, f := range s.Families {
			ls.Families = append(ls.Families, web.FamilyStatus{
				Family: f.Family, Online: f.Online, LatencyMS: f.LatencyMS, Since: f.Since,
			})
		}
		return ls
	}, sched, set, ni, version, p.log)
	// Extra Host values for the DNS-rebinding guard (reverse-proxy domains).
	for _, h := range strings.Split(p.cfg.AllowedHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			srv.AllowedHosts = append(srv.AllowedHosts, h)
		}
	}
	// Trusted proxies: behind these peers the login limiter keys on the
	// forwarded client, not the proxy, so one bad actor can't lock everyone out.
	// ParseFlags already validated the list; a failure here just means the
	// limiter falls back to keying on the proxy address.
	if p.cfg.TrustedProxies != "" {
		if err := srv.SetTrustedProxies(strings.Split(p.cfg.TrustedProxies, ",")); err != nil {
			fmt.Fprintln(os.Stderr, "pingularity: -trusted-proxy:", err)
			p.log.Error("-trusted-proxy", "err", err)
		}
	}
	srv.DBPath = p.cfg.DBPath       // /metrics reports the on-disk DB size
	srv.InContainer = containerized // relax the loopback-only filter (unenforceable in a container)
	srv.Update = upd                // update status on /api/status + powers the toggle
	srv.Logs = p.ring               // backs the About-tab log viewer (/api/logs)
	srv.OnLogClear = func() {
		// The /api/logs clear already emptied the ring; rewrite the on-disk
		// snapshot to match so a restart within the snapshot interval can't
		// resurrect the cleared lines (the ring is empty, so this truncates it).
		if rp := ringPath(p.cfg.DBPath); rp != "" {
			if err := p.ring.SaveFile(rp); err != nil {
				p.log.Debug("log snapshot clear failed", "err", err)
			}
		}
	}
	spawn(func() {
		// Serve returns nil on a graceful shutdown, so any error here is real
		// (port already in use, bad -listen, ...). Fatal: print straight to stderr
		// - the default log level is "off", and a dead web server must be visible
		// on a default install - then wind the whole daemon down.
		if err := srv.Serve(ctx, p.cfg.ListenAddr); err != nil {
			fmt.Fprintln(os.Stderr, "pingularity: web server:", err)
			p.log.Error("web server", "err", err)
			webFailed.Store(true)
			p.cancel()
		}
	})

	if err := m.Run(ctx); err != nil && err != context.Canceled {
		p.log.Error("monitor", "err", err)
	}
	p.log.Info("pingularity stopped")
}

// runPruner deletes old data once at startup and hourly thereafter. Latency
// samples, speed history, and outages each use their own retention window; a
// window of 0 keeps that data forever.
func (p *program) runPruner(ctx context.Context, set *settings.Controller) {
	// cutoff returns the prune-before time for a window (epoch = keep forever).
	cutoff := func(d time.Duration) time.Time {
		if d <= 0 {
			return time.Unix(0, 0)
		}
		return time.Now().Add(-d)
	}
	prune := func() {
		n, err := p.store.Prune(ctx, cutoff(set.Retention()), cutoff(set.SpeedRetention()), cutoff(set.DowntimeRetention()))
		if err != nil {
			p.log.Error("prune", "err", err)
			return
		}
		if n > 0 {
			p.log.Info("pruned old data", "rows", n)
		}
	}
	prune()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			prune()
		}
	}
}

// heartbeatInterval is how often the dead-man's-switch URL is pinged. A steady
// cadence lets an external watchdog (Healthchecks.io, Uptime Kuma push, etc.)
// alert when Pingularity or the whole host goes silent.
const heartbeatInterval = time.Minute

// runHeartbeat pings the dead-man's-switch URL on a fixed cadence. A no-op while
// no URL is set or monitoring is off - a paused monitor goes silent on purpose,
// so its watchdog should too.
func (p *program) runHeartbeat(ctx context.Context, set *settings.Controller) {
	client := notify.NewHeartbeatClient()
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if set.Monitoring() {
				notify.Heartbeat(ctx, client, set.HeartbeatURL(), p.log)
			}
		}
	}
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
	}
	return nil
}

// resetAuthCmd clears the stored password and disables auth - the forgot-password
// recovery path. Run locally with the privileges the service database requires
// (sudo on Unix, an elevated prompt on Windows) so it can open that file.
func resetAuthCmd(args []string) error {
	cfg, err := config.ParseFlags(args) // honors -db
	if err != nil {
		return err
	}
	// Never CREATE a database here. A missing path usually means the command ran
	// without the service's privileges (its DB is owned by the service account);
	// clearing auth on a fresh per-user DB would do nothing while the live service
	// keeps enforcing the old password. Refuse with a hint.
	if _, err := os.Stat(cfg.DBPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no database found at %s; re-run %s (the service database is owned by the service account) or pass -db <path>", cfg.DBPath, elevationHint())
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	set, _ := settings.New(context.Background(), st, settings.Values{})
	if err := set.ClearAuth(context.Background()); err != nil {
		return err
	}
	fmt.Printf("Authentication disabled and password cleared (%s).\n", cfg.DBPath)
	fmt.Printf("If the service is currently running, restart it (`%s`)\n", elevate("pingularity restart"))
	fmt.Println(" - it caches settings in memory and would keep enforcing the old password.")
	return nil
}

// defaultSettings is the fresh-install configuration: the seed values a brand-new
// database is created with. Extracted from run() so a test can execute it - the
// literal has three same-typed retention durations and two same-typed intervals
// side by side, so a field swap compiles and every package-level test still
// passes; only asserting the mapping catches it. See TestDefaultSettings.
func defaultSettings(cfg config.Config, containerized bool) settings.Values {
	// Local-only is a sensible default for a native install (LAN access is opt-in),
	// but inside a container the dashboard is only reachable over the network, and
	// the filter can't be enforced there anyway - so default it off in containers.
	return settings.Values{
		Latency: cfg.Interval, LatencyEnabled: cfg.LatencyEnabled,
		DNSProbe: true, // measure DNS-resolution latency by default (toggle in the Latency tab)
		// Connection info on by default - it is what the Connection panel shows.
		// Turning it off stops every third-party lookup (public IP, ISP, geo, exit).
		NetinfoEnabled: true,
		Speed:          cfg.SpeedtestInterval, Retention: cfg.Retention,
		SpeedRetention: cfg.SpeedRetention, DowntimeRetention: cfg.DowntimeRetention,
		Timeout: cfg.Timeout, DownAfter: cfg.DownAfter, UpAfter: cfg.UpAfter,
		SpeedtestEnabled:     cfg.SpeedtestEnabled,
		SpeedtestOnReconnect: cfg.SpeedtestOnReconnect, IPv6Mode: cfg.IPv6Mode, Monitoring: true,
		AccessLocalOnly:    !containerized, // loopback-only on native (LAN opt-in); off in containers (see above)
		ExitTarget:         "1.1.1.1",      // default exit-discovery destination (overridable in settings)
		UpdateCheckEnabled: true,           // default on; daily release poll, opt-out in the About tab
		LogRedactPII:       true,           // default on; censor PII in logs, opt-out in the About tab
		LogLevel:           "off",          // logging off by default; turn on (debug) from the About tab
		SpeedBusyMbps:      5,              // default "busy" threshold; only consulted when SpeedtestSkipBusy is on
		DegradedPingMS:     150,            // default degraded latency; only consulted when SpeedtestOnDegraded is on
		SpeedEngine:        "ookla",        // default speedtest backend; iperf3 is opt-in and capability-gated
		SpeedDirection:     "both",         // directions to test, whichever engine is selected
		SpeedRetries:       1,              // retry a failed direction once (transient busy/reset)
		IperfDur:           5,              // iperf3 seconds per direction
		IperfStreams:       1,              // iperf3 parallel TCP streams (1 = single stream)
		IperfOmit:          1,              // iperf3 warm-up seconds discarded (skip TCP slow-start)
		IperfUDP:           true,           // iperf3 packet-loss/jitter UDP pass on by default
		IperfWindow:        0,              // iperf3 TCP window/socket-buffer KB (0 = OS auto-tune)
		OoklaLoss:          true,           // Ookla packet-loss UDP probe on by default
		SpeedBestOf:        false,          // best-of-3 costs 3x the data - opt in
	}
}

// buildLogger builds the run-path logger: it writes the full line to stdout at
// level lvl and captures each record in both raw and PII-masked form into ring,
// so the dashboard can toggle masking at display time. Extracted so a test can
// pin the capture wiring (the masking is unit-tested; this install site was not).
func buildLogger(stdout io.Writer, lvl slog.Leveler, ring *logbuf.Ring) *slog.Logger {
	sink := func(raw, masked string) {
		if ring != nil {
			ring.Append(raw, masked)
		}
	}
	return slog.New(logfilter.NewCapture(stdout, &slog.HandlerOptions{Level: lvl}, sink))
}

func runCmd(args []string) error {
	cfg, err := config.ParseFlags(args)
	if err != nil {
		return err
	}
	// Runtime-adjustable verbosity (changed live from the dashboard) plus a small
	// in-memory ring that captures the same lines stdout/journald gets, so the
	// About tab can show recent logs. Both are seeded from settings once loaded.
	lvl := new(slog.LevelVar)
	lvl.Set(logLevelOff) // suppressed until settings load applies on/off, so "off" is truly silent (incl. the pre-load startup line)
	ring := logbuf.New(logBufferLines)
	if rp := ringPath(cfg.DBPath); rp != "" {
		// Restore the viewer's recent history before the logger is wired, so old
		// lines sit above this session's. Best-effort (logger not built yet).
		if err := ring.LoadFile(rp); err != nil {
			fmt.Fprintf(os.Stderr, "pingularity: restore log history: %v\n", err)
		}
	}
	// Logging is binary: when on, debug streams to both stdout/journald and the ring;
	// when off, the level filters everything out, so the ring stays empty on its own.
	// The ring keeps each line raw and PII-masked; the dashboard chooses which to show.
	log := buildLogger(os.Stdout, lvl, ring)
	prg := &program{cfg: cfg, log: log, logLevel: lvl, ring: ring}
	s, err := service.New(prg, svcConfig(nil))
	if err != nil {
		return err
	}
	// Run blocks until terminated; works both interactively and as a service.
	return s.Run()
}

func controlCmd(action string, args []string) error {
	// A deb/rpm install owns its own systemd unit that this CLI can't see:
	// kardianos/service manages /etc/systemd/system/<name>.service, but packages
	// install to /usr/lib/systemd/system. Acting here would half-uninstall (stop +
	// disable, then fail removing a path that isn't there) or plant a duplicate
	// unit on install. Refuse and point at the package manager instead.
	if action == "install" || action == "uninstall" {
		if hint, managed := packageManagerHint(); managed {
			if action == "uninstall" {
				return fmt.Errorf("pingularity was installed by your system package manager; remove it there (%s), not with `pingularity uninstall`", hint)
			}
			return fmt.Errorf("pingularity is already installed and managed by your system package manager; update or remove it there (%s), not with `pingularity install`", hint)
		}
	}

	var svcArgs []string
	var listenAddr string
	if action == "install" {
		// Validate the flags now with the daemon's own parser, so a typo like
		// `-listne :9000` fails here with a clear message instead of installing a
		// unit that crash-loops at startup under Restart=always.
		cfg, err := config.ParseFlags(args)
		if err != nil {
			return err
		}
		listenAddr = cfg.ListenAddr
		// The installed service re-invokes `pingularity run <flags>`; pass them
		// straight through - except a relative -db, which is pinned to an
		// absolute path NOW (relative to where the user ran install). At service
		// start the daemon would otherwise resolve it against ITS working
		// directory, which on Windows is always System32 (the SCM ignores
		// WorkingDirectory), silently planting the database there.
		svcArgs = append([]string{"run"}, absDBArgs(args)...)
	}
	prg := &program{cfg: config.Default(), log: slog.Default()}
	s, err := service.New(prg, svcConfig(svcArgs))
	if err != nil {
		return err
	}

	// "status" is reported via Status(), not Control(). A missing unit surfaces
	// as ErrNotInstalled, not a status - report it as the documented third state
	// (exit 0) rather than failing.
	if action == "status" {
		st, err := s.Status()
		if errors.Is(err, service.ErrNotInstalled) {
			fmt.Println("pingularity: not installed")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println("pingularity:", statusString(st))
		return nil
	}

	if action == "uninstall" {
		if !confirmUninstall(args) {
			fmt.Println("Aborted - service left in place.")
			return nil
		}
		_ = service.Control(s, "stop") // best-effort: stop before removing
	}

	if err := service.Control(s, action); err != nil {
		return err
	}
	switch action {
	case "install":
		// Start it now so a fresh install is running immediately, matching the
		// deb/rpm packages (whose postinstall enables + starts). A start failure
		// (e.g. the port is in use) does NOT fail the install - the unit is
		// registered and enabled; report it and point at `pingularity start`.
		if err := service.Control(s, "start"); err != nil {
			fmt.Printf("pingularity installed, but could not start it: %v\nStart it with:  pingularity start\n", err)
			return nil
		}
		fmt.Printf("pingularity installed and started. Dashboard on %s\n", dashboardURL(listenAddr))
	case "uninstall":
		// The service may have been installed with -db elsewhere, and uninstall
		// doesn't parse that flag, so naming a concrete path here can point at the
		// wrong directory. Say the data is untouched without guessing where it is.
		fmt.Println("pingularity service removed. Your database and key are untouched, wherever you configured them.")
	}
	return nil
}

// absDBArgs returns args with any relative -db value replaced by its absolute
// form (resolved against the current directory - where the user ran install).
// Handles "-db v", "-db=v", and the double-dash forms; everything else passes
// through untouched.
func absDBArgs(args []string) []string {
	out := slices.Clone(args)
	for i := 0; i < len(out); i++ {
		flagName, val, hasEq := strings.Cut(out[i], "=")
		if flagName != "-db" && flagName != "--db" {
			continue
		}
		abs := func(v string) string {
			if a, err := filepath.Abs(v); err == nil {
				return a
			}
			return v
		}
		if hasEq {
			out[i] = flagName + "=" + abs(val)
		} else if i+1 < len(out) {
			out[i+1] = abs(out[i+1])
			i++
		}
	}
	return out
}

// confirmUninstall warns the user and asks for confirmation, unless a
// -y/--yes/-f/--force flag is present (for scripted use).
func confirmUninstall(args []string) bool {
	for _, a := range args {
		switch a {
		case "-y", "--yes", "-f", "--force":
			return true
		}
	}
	fmt.Println("WARNING: this will STOP and REMOVE the pingularity background service.")
	fmt.Println("Your data is NOT deleted (the database and key are left in place wherever they were configured).")
	fmt.Print("Proceed? [y/N]: ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

// packageManagerHint reports whether pingularity was installed by the OS package
// manager (deb/rpm) rather than by this CLI, and returns a phrasing of the
// remove command to point the user at. kardianos/service installs to
// /etc/systemd/system; the packages install to /usr/lib/systemd/system, so an
// /etc unit means a CLI install (we own it) while only the package unit means the
// package manager owns it. Linux-only; false everywhere else.
func packageManagerHint() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	if _, err := os.Stat("/etc/systemd/system/pingularity.service"); err == nil {
		return "", false // a CLI-managed unit exists; this CLI owns it
	}
	if _, err := os.Stat("/usr/lib/systemd/system/pingularity.service"); err != nil {
		return "", false // no package unit either; nothing to defer to
	}
	// Name the actual manager where we can tell, so the hint is a runnable command.
	if _, err := os.Stat("/usr/bin/dpkg"); err == nil {
		return "e.g. sudo apt remove pingularity", true
	}
	if _, err := os.Stat("/usr/bin/rpm"); err == nil {
		return "e.g. sudo dnf remove pingularity", true
	}
	return "use your system package manager", true
}

// svcConfig describes the OS service. On install, arguments are the flags the
// service runs with.
func svcConfig(arguments []string) *service.Config {
	wd := ""
	if exe, err := os.Executable(); err == nil {
		wd = filepath.Dir(exe)
	}
	sc := &service.Config{
		Name:        "pingularity",
		DisplayName: "Pingularity",
		Description: "Internet connectivity monitor with a built-in web dashboard.",
		Arguments:   arguments,
		// Best-effort niceness for relative paths on the platforms that honor it
		// (systemd/launchd; the Windows SCM ignores it - which is why install
		// absolutizes -db up front, see absDBArgs).
		WorkingDirectory: wd,
	}
	// Per-OS recovery + network-readiness ordering (systemd deps here, Windows SCM
	// recovery/dependencies there). See svcopts_{windows,other}.go.
	applyServiceOpts(sc)
	return sc
}

func statusString(s service.Status) string {
	switch s {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "not installed"
	}
}

// dashboardURL renders a friendly local URL for a listen address (a bare port,
// empty host, or wildcard all resolve to localhost), for the post-install pointer.
func dashboardURL(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "9000"
	}
	return "http://localhost:" + port
}

// nonLoopbackListen reports whether addr binds a non-loopback interface (so the
// dashboard is reachable from the network). An empty host (e.g. ":9000") binds
// all interfaces.
func nonLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr // no port present; treat the whole value as the host
	}
	switch host {
	case "", "0.0.0.0", "::":
		return true // all interfaces
	case "localhost":
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback()
	}
	return true // an explicit non-localhost hostname is network-reachable
}

func fail(err error) {
	if err == nil {
		return
	}
	// `run -h` / `install -h`: flag.Parse already printed the flag usage, so
	// this is help delivered, not a failure.
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "pingularity:", err)
	os.Exit(1)
}

// logLevelOff is far above slog.LevelError (8) so every record is filtered out.
// Logging is binary: off = nothing anywhere; on = the single maximal level (debug).
const logLevelOff = slog.Level(1 << 20)

// applyLogLevel sets the live logger threshold: "off" suppresses everything, any
// other value is the one "on" level - full debug to stdout/journald and the ring.
func applyLogLevel(lv *slog.LevelVar, name string) {
	if name == "off" {
		// "off" silences routine INFO/DEBUG chatter but still surfaces WARN and
		// ERROR: a failed sample write (full disk), a wedged webhook, or a settings
		// persist failure must not vanish just because logging sits at its default.
		// Real WARN/ERROR volume in normal operation is ~0, so stdout stays quiet
		// in practice, while the About tab's log ring - which shares this level -
		// captures whatever went wrong for a user to find later. (The pre-load pin
		// stays at logLevelOff so startup before settings load is truly silent.)
		lv.Set(slog.LevelWarn)
		return
	}
	lv.Set(slog.LevelDebug)
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

// elevate prefixes a shell command with the OS privilege-elevation. On Unix that
// is `sudo`; on Windows there is no prefix - the command must be run from an
// elevated (Administrator) prompt (see elevationHint).
func elevate(cmd string) string {
	if runtime.GOOS == "windows" {
		return cmd
	}
	return "sudo " + cmd
}

// elevationHint is the OS-appropriate phrasing for "run with admin privileges".
func elevationHint() string {
	if runtime.GOOS == "windows" {
		return "from an elevated (Administrator) prompt"
	}
	return "with sudo"
}

// exitSummary renders an exit path ("router (loc) → handoff (loc, ASn)") for
// recording with a speedtest run. "" when no exit was discovered.
func exitSummary(e *netinfo.ExitInfo) string {
	if e == nil {
		return ""
	}
	hop := func(name, ip, loc, asn string) string {
		s := name
		if s == "" {
			s = ip
		}
		var extra []string
		if loc != "" {
			extra = append(extra, loc)
		}
		if asn != "" {
			extra = append(extra, "AS"+asn)
		}
		if len(extra) > 0 {
			s += " (" + strings.Join(extra, ", ") + ")"
		}
		return s
	}
	var parts []string
	if e.IP != "" || e.Name != "" {
		parts = append(parts, hop(e.Name, e.IP, e.Loc, ""))
	}
	if e.NextIP != "" || e.NextName != "" {
		parts = append(parts, hop(e.NextName, e.NextIP, e.NextLoc, e.NextASN))
	}
	return strings.Join(parts, " → ")
}

// joinNonEmpty joins the non-empty parts with ", ".
func joinNonEmpty(parts ...string) string {
	var nz []string
	for _, p := range parts {
		if p != "" {
			nz = append(nz, p)
		}
	}
	return strings.Join(nz, ", ")
}

func usage() {
	db := config.DefaultDBPath()
	fmt.Printf(`pingularity - internet connectivity monitor with a built-in web dashboard

Usage:
  pingularity [run] [flags]    Monitor connectivity + serve the web UI (default)
  pingularity install [flags]  Install as a background service (flags are persisted)
  pingularity start|stop       Start/stop the installed service
  pingularity restart|status   Restart / show service status
  pingularity uninstall [-y]   Remove the service (prompts first; -y to skip prompt)
  pingularity reset-auth       Clear the password + disable auth (forgot-password recovery)
  pingularity version          Print version

Run flags (all optional - defaults work with no flags):
  -listen string   Web UI + metrics address (default ":9000" = all interfaces,
                   IPv4 + IPv6; use 127.0.0.1:9000 for local-only)
  -allow-host s    Extra Host header values to accept, comma-separated. Only
                   needed behind a reverse proxy on a public domain - IPs,
                   localhost, dotless names, and .local/.lan/.home/.internal
                   always work (DNS-rebinding protection).
  -trusted-proxy s Proxy IPs/CIDRs whose X-Forwarded-For identifies the real
                   client, comma-separated. Behind a same-host proxy this keeps
                   one visitor's failed logins from rate-limiting everyone.
  -db string       SQLite path (default: %s;
                   a system path when run as a service, else a per-user data dir;
                   the directory is auto-created)
  -interval dur    Time between probe rounds (default 5s; 1s-1h)
  -timeout dur     Per-target dial timeout (default 3s; 1s-30s)
  -down-after int  Consecutive failures before DOWN (default 2; 1-10)
  -up-after int    Consecutive successes before UP (default 1; 1-10)
  -ipv4 string     IPv4 probing: auto | on | off (default auto = only while the
                   host has an IPv4 address)
  -ipv6 string     IPv6 probing: auto | on | off (default auto; changeable live)
  -speedtest                Enable scheduled speedtests (startup + interval;
                            off by default. Reconnect-triggered tests are
                            separate: see -speedtest-on-reconnect)
  -speedtest-interval dur   Time between scheduled speedtests (default 1h; 1m-24h)
  -speedtest-on-reconnect=false  Don't run a speedtest after each reconnect
                            (they are on by default, independent of -speedtest)
  -latency=false            Disable latency probing (speedtest-only mode)
  -retain dur               Prune latency samples older than this
                            (default 720h = 30 days; 0 = forever)
  -retain-speed dur         Prune speed history older than this (default 8760h = 1 year)
  -retain-downtime dur      Prune outage history older than this (default 8760h = 1 year)

These flags only seed the initial values - almost everything (intervals,
thresholds, retention, alert webhooks, …) is adjustable live in the settings
drawer and persists across restarts.

Service commands (install/start/stop/uninstall) must be run %s.

Examples:
  pingularity                  # run in foreground; UI on http://localhost:9000
  %s     # install as a service and start it; DB -> %s, UI on :9000
`, db, elevationHint(), elevate("pingularity install"), filepath.Dir(db))
}
