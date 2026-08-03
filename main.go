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
	"github.com/pingular/pingularity/internal/stats"
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

// maxWorkerRestarts bounds how many times a panicking long-lived worker loop is
// restarted (with exponential backoff) before spawnLoop gives up on it, so a
// persistently-crashing loop can't spin forever.
const maxWorkerRestarts = 5

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
	// spawnLoop runs a LONG-LIVED worker (a loop meant to live for the process
	// lifetime). Unlike spawn's one-shot recover, a panic here must not silently
	// remove the worker until the next restart: recover, log, and restart it with
	// bounded exponential backoff up to maxWorkerRestarts, then give up (still
	// without crashing the daemon - the core monitor runs off the main goroutine).
	// A clean return means the worker honored ctx and exited, so do not restart.
	spawnLoop := func(name string, fn func()) {
		spawn(func() {
			// Worker health on /metrics: up=1 while this loop is running, cleared to 0
			// when it returns or gives up; restarts counts panic-recoveries. A worker
			// that dies (up=0) or thrashes (restarts climbing) is then alertable.
			stats.Set("worker."+name+".up", 1)
			stats.Seed("worker." + name + ".restarts")
			defer stats.Set("worker."+name+".up", 0)
			backoff := time.Second
			// maxWorkerRestarts is meant to catch a THRASHING worker (rapid crash loop),
			// not a worker that panics rarely over a long uptime. A run that lasted this
			// long before panicking counts as healthy and resets the restart budget, so
			// the give-up limit only tallies CONSECUTIVE quick failures.
			const healthyRun = 60 * time.Second
			for attempt := 0; ctx.Err() == nil; attempt++ {
				if attempt > 0 {
					stats.Inc("worker." + name + ".restarts")
				}
				start := time.Now()
				panicked := func() (didPanic bool) {
					defer func() {
						if r := recover(); r != nil {
							didPanic = true
							p.log.Error("background worker panicked", "worker", name, "panic", r, "stack", string(debug.Stack()))
						}
					}()
					fn()
					return false
				}()
				if !panicked || ctx.Err() != nil {
					return
				}
				if time.Since(start) >= healthyRun {
					attempt, backoff = 0, time.Second // fresh budget after a healthy run
				}
				if attempt+1 >= maxWorkerRestarts {
					p.log.Error("background worker gave up after repeated panics", "worker", name, "restarts", attempt+1)
					return
				}
				p.log.Warn("restarting panicked background worker", "worker", name, "next_attempt", attempt+2, "backoff", backoff.String())
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				if backoff < 30*time.Second {
					backoff *= 2
				}
			}
		})
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
	var sessionKey []byte // independent session-token secret bound to the key file (see web.Server.SessionKey)
	box, boxErr := secret.New(p.cfg.DBPath)
	switch {
	case box == nil: // real failure: no crypter at all -> plaintext (unchanged behavior)
		// Straight to stderr, bypassing the log level: this runs before settings
		// load, so the level is still "off" and a plain Error would be swallowed
		// on every install - and this is a silent security degradation.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: secret key unavailable; iperf3 passwords will be stored in the clear: %v\n", boxErr)
		err := boxErr
		earlyLog = append(earlyLog, func() {
			p.log.Error("secret key unavailable; iperf3 passwords will be stored in the clear", "err", err)
		})
	default: // box usable; boxErr may be secret.ErrKeyPermsInsecure
		if boxErr != nil {
			// A valid key was loaded but its file could not be tightened and may be
			// readable by others. Encryption stays ON (strictly better than the old
			// discard-to-plaintext), but say so loudly.
			fmt.Fprintf(os.Stderr, "pingularity: WARNING: key file permissions could not be secured; encryption is ON but the key file may be readable by others: %v\n", boxErr)
			err := boxErr
			earlyLog = append(earlyLog, func() {
				p.log.Warn("key file permissions could not be secured; encryption remains on", "err", err)
			})
		}
		setOpts = append(setOpts, settings.WithCrypter(box))
		// Derive the session-token signing secret from the same key file, so a
		// DB-only copy (which has the bcrypt hash but not the key) can't forge a
		// session. Absent a key, sessionKey stays nil and tokens key on the hash
		// alone (the prior behavior).
		sessionKey = box.DeriveSubkey("session-token-v1")
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
		// The web guard now REFUSES to serve on this controller rather than
		// answering from defaults (see settings.Controller.Loaded): "no login
		// configured" and "the login could not be read" are indistinguishable
		// from here, and serving on the wrong one hands out an unauthenticated
		// dashboard while the operator's password sits on disk.
		//
		// So the wording is no longer "using defaults" - nothing is served on
		// them - and a retry is armed, because failing closed on a transient
		// read error would otherwise cost the dashboard until someone noticed
		// and sent SIGHUP. Monitoring is unaffected either way; it does not go
		// through the guard.
		fmt.Fprintf(os.Stderr, "pingularity: WARNING: could not load settings; refusing to serve the UI/API until they load: %v\n", err)
		err := err
		earlyLog = append(earlyLog, func() {
			p.log.Error("load settings; refusing to serve until a reload succeeds", "err", err)
		})
		spawnLoop("settings-retry", func() { p.retrySettingsLoad(ctx, set) })
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
	seedKnownCounters() // create fixed counters at 0 so a first event after restart is a visible 0->1 step (rate/increase can miss a series that appears at 1)

	// Speedtests/reconnects refresh connection info; this loop is just a backstop
	// to keep it no staler than an hour when those don't fire.
	spawnLoop("netinfo", func() { ni.Loop(ctx, time.Hour) })

	// Background poll for a newer release (see internal/update). Best-effort: honors
	// the toggle, skips dev builds, and a dead endpoint is a no-op. Status surfaces
	// on /api/status; the About tab lights a cue.
	upd := update.New(version, set.UpdateCheckEnabled, p.log)
	spawnLoop("update-check", func() { upd.Loop(ctx) })

	// Alert delivery (webhook) + dead-man's-switch heartbeat.
	notifier := notify.New(set.WebhookURL, p.log)
	spawnLoop("heartbeat", func() { p.runHeartbeat(ctx, set) })

	// Periodic health digest (off/daily/weekly) to the same webhook. Deliberately
	// NOT gated on set.Monitoring: the digest now states the span it observed, so a
	// paused or scheduled-off period is reported honestly instead of either
	// silently skipped (power button) or reported as a confident 100% (schedule).
	dig := &digest.Manager{Store: p.store, Notify: notifier, Log: p.log, FreqFn: set.DigestFreq, RetentionFn: set.DowntimeRetention}
	spawnLoop("digest", func() { dig.Loop(ctx) })

	// Speedtest scheduler - always constructed so it can be toggled live.
	tester := speedtest.NewOokla()
	tester.ServerIDFn = set.SpeedServerID // honor the chosen server, live
	// Auto server selection: a city the user searched overrides everything
	// (AutoLocFn - a statement of intent, not a hypothesis to measure).
	// Otherwise the candidate cities the connection itself names - the exit
	// router's, the ISP geolocation's, and the one the Ookla API places our
	// address in - each contribute their closest servers to one deduplicated
	// ping race, and the city whose server answers fastest becomes the centre
	// (see speedtest.raceCities and autoOrigins).
	tester.AutoLocFn = set.AutoLocation
	tester.OriginsFn = func() []speedtest.Origin { return autoOrigins(ni.Get()) }
	// The user's ISP name grants its own server a guaranteed lane in the
	// auto-select ping race (an on-net server is the most likely winner). A
	// stale or empty name is harmless - it only adds or skips one racer.
	tester.ISPFn = func() string { return ni.Get().ISP }
	// Whether there is any speed history yet. The first best-of round on a fresh
	// install is the one that seeds the baseline later checks read, so it is
	// decided on ping alone rather than on throughput nothing can vet yet (see
	// firstRunByPing). Counted, not cached: it flips false->true after one run
	// and the query is one indexed count per run, not per sample.
	tester.PriorDataFn = func() bool {
		c, err := p.store.TableCounts(context.Background())
		if err != nil {
			return true // cannot tell: assume history exists rather than re-seeding
		}
		return c["speed"] > 0
	}
	sched := speedtest.NewScheduler(tester, p.store, p.cfg.SpeedtestInterval, p.log)
	tester.OnServer = sched.SetCurrentServer    // surface the live server during a run
	tester.DirectionFn = set.SpeedDirection     // Ookla direction (per-engine; iperf3 has its own)
	tester.RetriesFn = set.SpeedRetries         // Ookla retries (per-engine; iperf3 has its own)
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
		DirectionFn:  set.IperfDirection,
		UDPFn:        set.IperfUDP,
		UDPRateFn:    set.IperfUDPRate,
		BindFn:       set.IperfBind,
		WindowFn:     set.IperfWindow,
		IPVersionFn:  set.IperfIPVer,
		RetriesFn:    set.IperfRetries,
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
		if set.SpeedEngine() == "iperf3" {
			if speedtest.IperfAvailable() {
				return iperfTester
			}
			// The operator selected iperf3 but the binary isn't on PATH (the official
			// container ships none - the -iperf image variant does). Falling back to
			// Ookla keeps some data flowing, but it measures the public internet, not
			// the user's own server - so make the substitution OBSERVABLE via a metric
			// rather than only a UI note. The recorded row still carries engine="ookla".
			stats.Inc("speed.iperf_unavailable")
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
		// Own goroutine (like Outage's dispatch): RunOnce fires this inline on
		// the scheduler Loop goroutine, and SpeedThreshold now retries transient
		// webhook failures, so delivering in place would stall scheduling for
		// the whole backoff. At most one alert per breaching run and runs are
		// minutes apart, so these cannot pile up; spawn makes shutdown wait.
		spawn(func() { notifier.SpeedThreshold(ctx, sp, failures) })
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
	spawnLoop("scheduler", func() { sched.Loop(ctx) })

	// On reconnect: re-check public IP/DNS (it may have changed) and, if enabled,
	// run a speedtest. These monitor callbacks fire synchronously from the monitor
	// loop, and the work they spawn touches the store - so route it through spawn()
	// (not a bare `go`) so the shutdown drain waits for it before store.Close(). All
	// of it honors ctx, so a cancelled refresh/run/alert returns inside the bound.
	//
	// Both jobs are rate-limited: a flapping link confirms a reconnect every few
	// seconds and neither may be dispatched per flap (see reconnectGate). The two
	// gates are separate so the cheap lookup and the expensive speedtest keep
	// their own last-fire time and their own floor.
	var netinfoGate, speedGate reconnectGate
	m.OnReconnect = func() {
		// One clock reading for the whole callback, so the two gates and the speed
		// schedule all judge the same instant.
		now := time.Now()
		if netinfoGate.allow(now, reconnectNetinfoGap) {
			spawn(func() { ni.Refresh(ctx) })
		} else {
			// Flap storm: skip the lookups. The hourly netinfo loop is still the
			// backstop, so the panel is never worse than an hour stale. Note a
			// Refresh that no-ops (connection info off) also consumes the window -
			// re-checking NetinfoEnabled here would duplicate the enable check
			// netinfo deliberately centralizes in Refresh, for at most an extra
			// few minutes of staleness after the toggle goes back on.
			stats.Inc("netinfo.reconnect_suppressed")
		}
		// Independent of the Automatic (scheduled) toggle: run as long as monitoring
		// (power) and on-reconnect are both on. The speed schedule (quiet hours) is
		// still honored via SpeedAllowed. The gate is consulted LAST, after every
		// setting that could veto the run: a trigger the settings already suppressed
		// must not consume the window and space out the next allowed one.
		if set.Monitoring() && set.SpeedtestOnReconnect() && set.SpeedAllowed(now) {
			if speedGate.allow(now, reconnectSpeedGap(set.SpeedInterval())) {
				spawn(func() {
					// A dispatch that bounces off RunOnce's single-flight has measured
					// nothing, so it must not consume the window: the run it collided
					// with may have started BEFORE this reconnect, in which case it is
					// measuring the link that was still down, and keeping the window
					// would suppress the real recovery test for up to a day. Bounced
					// dispatches are free (the busy check returns before any network
					// work), so handing the window back cannot cost a flap storm
					// anything.
					if _, err := sched.RunOnce(ctx, "reconnect"); errors.Is(err, speedtest.ErrBusy) {
						speedGate.release(now)
					}
				})
			} else {
				stats.Inc("speed.reconnect_suppressed") // too soon after the last reconnect test
			}
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
			stats.Inc("notify.outage_dropped") // alert lost: webhook backpressured past the cap
			p.log.Warn("outage alert dropped: notifier backlog full (webhook unreachable?)", "online", online)
			return
		}
		depth := outageBacklog.Add(1)
		stats.SetMax("notify.backlog_max", int64(depth)) // high-water mark of the dispatch backlog
		spawn(func() {
			defer outageBacklog.Add(-1)
			notifier.Outage(ctx, online, durationS)
		})
	}

	spawnLoop("pruner", func() { p.runPruner(ctx, set) })

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
	srv.DBPath = p.cfg.DBPath             // /metrics reports the on-disk DB size
	srv.InContainer = containerized       // relax the loopback-only filter (unenforceable in a container)
	srv.Bridged = util.BridgedContainer() // only a bridged container defeats the loopback test; host-net still enforces
	srv.SessionKey = sessionKey           // key-file-bound secret folded into session-token MACs (see tokenKey)
	srv.MetricsToken = p.cfg.MetricsToken // optional read-only scrape credential for /metrics
	srv.Update = upd                      // update status on /api/status + powers the toggle
	// The settings server-browsing list centres on the same candidates auto
	// races, so the picker can never point at a city the race would not choose.
	srv.AutoOriginsFn = tester.OriginsFn
	srv.Logs = p.ring // backs the About-tab log viewer (/api/logs)
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
// seedKnownCounters initializes the fixed, known operational counters (and the
// enumerable failure/trigger families) to 0 at startup, so a first event after a
// restart reads as a 0->1 step that rate()/increase() can see - a series that only
// springs into existence at 1 hides its first increment from Prometheus (F7). Only
// counters with a bounded, compile-time key set are seeded; dynamic ones aren't.
func seedKnownCounters() {
	names := []string{
		"monitor.rounds", "monitor.downs", "monitor.blips", "monitor.bad_rounds",
		"monitor.pauses", "monitor.degraded_episodes", "monitor.flap.ipv4", "monitor.flap.ipv6",
		"monitor.event_dropped",
		"dns.attempts",
		"speed.fail", "speed.errbusy", "speed.iperf_unavailable", "speed.iperf_partial",
		// Reconnect triggers the spacing gates refused (see reconnectGate): a
		// climbing rate is a flapping link, not a broken feature.
		"speed.reconnect_suppressed", "netinfo.reconnect_suppressed",
		// A run that had to start while a previous run's abandoned transfer was still
		// moving bytes: those numbers read low. Climbing = someone is abort-storming.
		"speed.overlapped_orphan", "speed.abandoned_resumed",
		// A panic inside the measurement library, contained to its own transfer.
		"speed.transfer_panic",
		// A best-of round where one server reported a direction the rest of the
		// round contradicts (a buffer-absorbing server counting bytes it never
		// delivered). Climbing means a server near you is inflating results -
		// worth knowing, because before the guard those rounds became history.
		"speed.implausible_direction",
		// The first best-of round on an install with no history, decided on ping
		// alone so an unverifiable throughput reading cannot seed the baseline.
		// Fires at most once per install; a nonzero value long after setup means
		// history is being lost.
		"speed.first_run_by_ping",
		// One best-of candidate failed mid-round while the run carried on with
		// the rest. Whole-run speed.fail.* never sees these, so before this
		// counter a persistently dead nearby server was invisible unless it
		// killed the entire round.
		"speed.bestof_candidate_failed",
		// A scheduled run dropped because another trigger (manual, reconnect,
		// degraded) already held the single-flight. The slot is not retried, so a
		// climbing rate explains gaps in an otherwise regular history.
		"speed.scheduled_skipped",
		// The auto city race: how many runs chose a centre by measurement, how
		// many found nobody answering (a ping-hostile network - the condition
		// worth alerting on, so its first occurrence after a restart must be
		// visible), and how many skipped the race because no candidate city had
		// a coordinate at all (a climbing rate means auto is running blind, e.g.
		// connection-info lookups switched off).
		"speed.cityrace_decided", "speed.cityrace_silent", "speed.cityrace_unanchored",
		// A pause span that crossed a clock correction and was refused rather than
		// recorded: expected once on an RTC-less boot, never in steady state.
		"monitor.pause_clock_corrections",
		// An unobserved stretch the store refused: retried until it lands, because
		// dropping it makes coverage read HIGHER than the truth.
		"monitor.unobserved_gap_retries", "monitor.unobserved_gap_refused",
		"notify.outage_dropped",
		"db.err", "db.busy", "db.io_err", "db.disk_full", "db.corrupt", "db.prune_count",
		"db.prune_skipped_clock",
		"web.login_fail", "web.limiter_trips", "web.metrics_targets_capped",
	}
	for _, cls := range []string{"timeout", "refused", "net_unreachable", "host_unreachable", "dns", "other"} {
		names = append(names, "probe.fail."+cls, "dns.fail."+cls)
	}
	for _, trig := range []string{"startup", "scheduled", "reconnect", "degraded", "manual"} {
		names = append(names, "speed.run."+trig)
	}
	for _, stage := range []string{"server_list", "server_fetch", "no_servers", "ping", "na", "download", "upload", "bidir", "other"} {
		names = append(names, "speed.fail."+stage)
	}
	for _, dest := range []string{"discord", "slack", "healthchecks", "generic", "heartbeat"} {
		names = append(names, "notify."+dest+".ok", "notify."+dest+".fail", "notify."+dest+".blocked")
	}
	stats.Seed(names...)
}

// retrySettingsLoad re-attempts the initial settings read until it succeeds.
//
// It exists because the web guard fails CLOSED when settings never loaded: that
// is the right call for a stored password, but on a transient fault - a lock the
// busy timeout did not absorb, a passing I/O error - it would otherwise cost the
// operator the whole UI and API until they noticed and sent SIGHUP. A read that
// failed once is very likely to succeed shortly after, so retrying turns a
// permanent outage back into a blip.
//
// Reload is the same call SIGHUP makes, and clears the flag on success. It stops
// as soon as it wins: a controller that has loaded has nothing to retry, and a
// later failure is a different problem with its own handling (settings writes
// refuse, and Reload is not on any hot path).
func (p *program) retrySettingsLoad(ctx context.Context, set *settings.Controller) {
	const (
		first = 5 * time.Second
		max   = 5 * time.Minute
	)
	for wait := first; ; {
		if !sleepCtx(ctx, wait) {
			return
		}
		if set.Loaded() {
			return // SIGHUP or an import beat us to it
		}
		if err := set.Reload(ctx); err == nil || errors.Is(err, settings.ErrLegacyReseal) {
			p.log.Warn("settings loaded on retry; serving normally again")
			return
		} else {
			p.log.Error("settings retry failed; still refusing to serve", "err", err, "next_try_in", wait)
		}
		if wait *= 2; wait > max {
			wait = max
		}
	}
}

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
	// Deliberately NOT pruned here. Prune's own guard catches a clock that STEPS
	// while we are running, by checking it against monotonic time - but a machine
	// whose clock is already wrong at boot never steps, so there is nothing for
	// that guard to see. Pruning is the one irreversible thing this process does
	// on a schedule, and at t=0 it would run on the least trustworthy reading the
	// process will ever hold: before NTP, on hardware that may have no RTC at all.
	//
	// So the first pass waits. Nothing is lost by it - the cleanup is idempotent
	// and the ticker below repeats forever - and the wait is what turns "boots
	// with a garbage RTC" from data loss into a delayed tidy-up, because time
	// sync on a networked host (which a monitoring daemon is) lands inside it.
	if !sleepCtx(ctx, pruneStartupGrace) {
		return
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

// pruneStartupGrace is how long the pruner waits before its first destructive
// pass, giving time sync a chance to correct a boot clock. Comfortably longer
// than NTP needs on a working network, and irrelevant on a healthy clock since
// the work is a no-op cleanup either way.
var pruneStartupGrace = 10 * time.Minute

// sleepCtx waits d, reporting false if the context was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// autoOrigins enumerates the candidate cities auto server selection races. Kept
// as a plain function so the enumeration itself is tested rather than a copy of
// it - the same reason listCentre is one.
//
// Each origin yields its OWN Ookla server list, and those lists are disjoint
// rather than reordered: measured on a live connection, three candidate cities
// returned 29, 20 and 20 servers with an empty pairwise intersection.
// So choosing a centre decides which servers EXIST at all - which is why the
// centre is raced (see speedtest.raceCities) rather than taken from a priority
// cascade that measures nothing. Order here decides only what an unanswered
// race falls back to (the exit router, the old cascade's answer); who wins is
// decided by ping.
//
// EVERY ORIGIN HERE IS DERIVED FROM THE CONNECTION. A searched city is not one
// of them: it is a statement of intent, wired through AutoLocFn, and it
// bypasses the race. A pinned server (speed_server_id) overrides both.
func autoOrigins(i netinfo.Info) []speedtest.Origin {
	var out []speedtest.Origin
	// The exit-node city: the last hop still inside the ISP, and the truest
	// network origin we can name - when RIPE has a coordinate for it, which for
	// a residential ISP's last hop it frequently does not (a hostname, no
	// lat/lon).
	if ex := i.Exit; ex != nil && (ex.Lat != 0 || ex.Lon != 0) {
		out = append(out, speedtest.Origin{Kind: "exit", Label: ex.Loc, Lat: ex.Lat, Lon: ex.Lon, Anchored: true})
	}
	// The ISP city: the eyeball geolocation of our own public IP, the only
	// origin that claims to describe where the SUBSCRIBER is. netinfo has
	// always fetched it for the Connection panel and used to throw the
	// coordinate away (see publicIPGeo), which is why the old cascade fell
	// through to the Cloudflare PoP on exactly the links where the two
	// disagree.
	if i.Lat != 0 || i.Lon != 0 {
		label := i.City
		if label != "" && i.Country != "" {
			label += ", " + i.Country
		}
		out = append(out, speedtest.Origin{Kind: "isp", Label: label, Lat: i.Lat, Lon: i.Lon, Anchored: true})
	}
	// The geo city: no centre of ours at all - the Ookla API geolocates our
	// source address and returns the pool IT believes we belong to. Usually a
	// duplicate of the ISP city's pool, which the union dedupe collapses for
	// free; on CGNAT or a tunnelled link it is not, and it is the only origin
	// that reflects the server operator's own opinion of where we are rather
	// than ours.
	out = append(out, speedtest.Origin{Kind: "geo", Label: "your connection"})
	return out
}

// Minimum spacing for the work a reconnect triggers (see m.OnReconnect in run).
// The callback fires on every confirmed down->up transition, and a marginal
// DSL/Wi-Fi/PPPoE link with the shipped defaults (5s interval, down_after 2,
// up_after 1) confirms one roughly every 15s for as long as it flaps - so
// without a floor the trigger dispatches a full speedtest and a full round of
// third-party lookups per flap. Neither is survivable: consecutive speedtests
// burn the data allowance this product itself meters (see
// pingularity_speed_data_used_bytes), compete with the link that is trying to
// recover, and measure a link that is still flapping - garbage numbers at the
// highest possible cost; the lookups are cheap but there are ~4 per refresh and
// a third party is entitled to answer 429. RunOnce's single-flight only prevents
// OVERLAP, which is why it never bounded any of this: the run after an ErrBusy
// starts the instant the previous one ends.
const (
	// reconnectNetinfoGap floors the reconnect connection-info refresh. Far
	// cheaper than a speedtest (a handful of small HTTPS requests, no bulk
	// transfer) and far more time-sensitive - a new PPPoE session usually means a
	// new public IP, and showing the old one is a visible lie - so it gets its own
	// short floor rather than the speedtest's. At 5 minutes a flap storm costs
	// ~12 refreshes an hour instead of ~240, which keeps the per-provider request
	// rate inside the free tiers these endpoints hand out.
	reconnectNetinfoGap = 5 * time.Minute

	// minReconnectSpeedGap is the hard floor between reconnect-triggered
	// speedtests: 15 minutes, i.e. the larger of 15x the smallest schedule the
	// settings layer will accept (settings.MinSpeed / config.MinSpeedInterval =
	// 1m) and a quarter of the shipped hourly schedule. A run moves hundreds of
	// megabytes and takes 30-60s, so this is the shortest spacing that is still
	// defensible on a metered LTE/5G backup link.
	minReconnectSpeedGap = 15 * time.Minute
)

// reconnectSpeedGap returns the minimum spacing between reconnect-triggered
// speedtests for a configured scheduled interval.
//
// The constant above is a floor, not the answer: the interval the user
// configured is their own statement of how much data and how much link
// contention a speedtest is worth, and someone who asked for one test every 6
// hours plainly does not want this trigger running one every 15 minutes behind
// their back. So take the larger of the two - the reconnect trigger never fires
// more often than either the hard floor or the user's own cadence. Reading the
// interval as a BUDGET rather than as a schedule is what keeps this consistent
// with the trigger being independent of SpeedtestEnabled (a stock install ships
// scheduled tests off and on-reconnect on, config.Default): the number still
// says what a test is worth to them, whether or not it is also a schedule.
// settings clamps Speed to MinSpeed..MaxSpeed (1m..24h), so the result is
// bounded at one reconnect test per day in the worst case.
func reconnectSpeedGap(interval time.Duration) time.Duration {
	if interval > minReconnectSpeedGap {
		return interval
	}
	return minReconnectSpeedGap
}

// reconnectGate spaces out one kind of reconnect-triggered work. Extracted from
// run() for the same reason as defaultSettings below: policy buried in a 600-line
// closure is policy no test can reach, and this one only misbehaves during a flap
// storm that no unit test would otherwise reproduce. See main_reconnect_test.go.
//
// The zero value is an OPEN gate. The first reconnect of a process must never be
// suppressed - measuring the link right after it recovers is the entire point of
// the trigger, and it is on by default - so only a recorded previous fire can
// close it.
//
// allow takes now rather than reading the clock itself so the policy is testable
// with a fake one; the caller passes time.Now(), whose monotonic reading makes
// the comparison immune to a wall-clock step (an NTP correction must not open or
// wedge the gate).
type reconnectGate struct {
	// OnReconnect runs on the monitor goroutine today, so the lock is uncontended -
	// but a gate whose safety depends on which goroutine its caller happens to be
	// is a trap for the next call site, and this type is meant to be reused.
	mu   sync.Mutex
	last time.Time // zero = never fired, i.e. open
}

// allow reports whether the gated work may run at now, given a minimum spacing
// of min, and records the fire when it says yes. A SUPPRESSED call records
// nothing: the window is measured from the last thing that actually ran, so a
// link flapping faster than the window cannot slide it forward forever and
// starve the trigger completely.
func (g *reconnectGate) allow(now time.Time, min time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && now.Sub(g.last) < min {
		return false
	}
	g.last = now
	return true
}

// release gives back a window that allow() reserved for work which then turned
// out not to happen. It only rewinds if `now` is still the recorded fire, so a
// later allow() that has already claimed the gate is never undone.
//
// This exists for the ErrBusy case. Reserving the window at dispatch and keeping
// it regardless assumed the run we collided with is measuring the same recovery -
// but a scheduled run that STARTED BEFORE the link came back is measuring the
// link that was still broken. Keeping the window then suppressed the reconnect
// test for up to a day on the strength of a measurement of the wrong thing.
// Giving it back costs nothing: a bounced dispatch never reached the network.
func (g *reconnectGate) release(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last.Equal(now) {
		g.last = time.Time{}
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
		SpeedEngine:        "ookla",        // default speedtest backend; iperf3 is opt-in and PATH-gated (see IperfAvailable/IperfVersion - presence on PATH is not a capability guarantee)
		SpeedDirection:     "both",         // Ookla directions (both|down|up); iperf3 has its own below
		SpeedRetries:       1,              // Ookla: retry a failed direction once (transient busy/reset)
		IperfDirection:     "both",         // iperf3 directions (both|down|up|bidir), independent of Ookla
		IperfRetries:       1,              // iperf3: retry a failed direction once, independent of Ookla
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

// effectiveControlAction downgrades `restart` to `start` when the service is
// known to be stopped. Only a POSITIVE stopped status does this: on any status
// error (not installed, no permission, platform quirk) the original action
// proceeds so the user sees the real error instead of a misleading start one.
func effectiveControlAction(action string, s service.Service) string {
	if action != "restart" {
		return action
	}
	if st, err := s.Status(); err == nil && st == service.StatusStopped {
		return "start"
	}
	return action
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

	// start/stop/restart/status take no arguments; a stray one is almost always a
	// mistake (e.g. `pingularity start -listen :9000`, which silently does nothing
	// because flags are fixed at install time) - say so rather than ignoring it.
	switch action {
	case "start", "stop", "restart", "status":
		if len(args) > 0 {
			return fmt.Errorf("`pingularity %s` takes no arguments (got %q); service flags are set at install time", action, strings.Join(args, " "))
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
		// Best-effort: stop before removing. Removal proceeds either way (the
		// unit should go), but a CONFIRMED still-running service deserves a
		// warning - the unit vanishes and the live process is orphaned with
		// nothing left to manage it.
		if serr := service.Control(s, "stop"); serr != nil {
			st, sterr := s.Status()
			if w := stopWarning(serr, st, sterr); w != "" {
				fmt.Println(w)
			}
		}
	}

	// `restart` of a STOPPED service: the platform restart fails on its stop
	// half (Windows: "The service has not been started"), which strands the
	// one flow that needs it most - the documented Windows upgrade sequence
	// (pingularity stop, winget upgrade, start the new binary), where a user
	// typing restart instead of start hits a dead end with the service down.
	// The intent of restart-when-stopped is unambiguous: make it run.
	action = effectiveControlAction(action, s)

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
			fmt.Printf("pingularity installed, but could not start it: %v\nStart it with:  %s\n", err, elevate("pingularity start"))
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
// stopWarning returns the operator warning to print when the best-effort stop
// before uninstall failed, or "" when silence is right. It warns ONLY when the
// service is confirmed still running, so an already-stopped unit (the common,
// benign stop "failure" on Windows/launchd) stays quiet.
func stopWarning(stopErr error, status service.Status, statusErr error) string {
	if stopErr == nil || statusErr != nil || status != service.StatusRunning {
		return ""
	}
	return fmt.Sprintf("Warning: could not stop the running service (%v); it may keep running after the unit is removed - stop it manually before or after uninstalling.", stopErr)
}

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
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host, port = addr, "9000" // no port present; treat the whole value as the host
	}
	if port == "" {
		port = "9000"
	}
	// Only the wildcard/empty/loopback binds are shown as localhost (where a
	// machine-local browser reaches them); an explicit reachable host is the name
	// the operator should actually point at, so preserve it.
	switch host {
	case "", "0.0.0.0", "::", "127.0.0.1", "::1":
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
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
  -speedtest-on-reconnect=false  Don't run a speedtest after a reconnect
                            (on by default, independent of -speedtest; rate-limited
                            to one per -speedtest-interval, or per 15m if shorter)
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
