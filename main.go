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
	"net/http"
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
	case "healthz":
		fail(healthzCmd(args))
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

	// optsIgnored records that runCmd found a set-but-unexpanded PINGULARITY_OPTS
	// (container installs; see ignoredOptsWarning). The stderr line has already
	// been printed by then - this replays the event into the structured log and
	// ring once they exist, so the About tab shows it too.
	optsIgnored bool

	// dbCreated records that this boot found nothing at cfg.DBPath, so the
	// store.Open that followed is what created the database. Only Start can see
	// that, which is why it is carried across to run and handed to settings as
	// half the evidence behind settings.KeyInstallBornVersion.
	dbCreated bool
}

// dbCreatedNow reports whether the store.Open that follows will be what creates
// the database at path. Split out of Start so tests can reach it.
//
// Only a definite "it is not there" answers yes; every other stat outcome - the
// file exists, a directory sits at the path, the stat failed - answers no,
// because a birth recorded that never happened silences a warning that matters
// while an unproven one only costs a warning. A ":memory:" path stats as
// not-there, which is right for it: opening one always creates a fresh database.
func dbCreatedNow(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (p *program) Start(s service.Service) error {
	// Must stay above the open: the open is what creates the file.
	p.dbCreated = dbCreatedNow(p.cfg.DBPath)

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
			// Worker health on /metrics: up=1 while this loop is running; on exit the
			// terminal write below distinguishes completion (series removed) from
			// death (the alertable 0); restarts counts panic-recoveries. A worker
			// that dies (up=0) or thrashes (restarts climbing) is then alertable.
			stats.Set("worker."+name+".up", 1)
			stats.Seed("worker." + name + ".restarts")
			// Terminal state: a worker that COMPLETED its job (clean return
			// with the process still live - settings-retry succeeding) removes
			// its up gauge entirely, so the documented worker_up==0 alert
			// matches only real deaths: the give-up path below and shutdown.
			completed := false
			defer func() {
				if completed {
					stats.Delete("worker." + name + ".up")
				} else {
					stats.Set("worker."+name+".up", 0)
				}
			}()
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
					completed = !panicked && ctx.Err() == nil
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

	// Runtime-adjustable settings (UI + persisted), seeded from config. Access
	// defaults to loopback-only EVERYWHERE - native and containers, host-net and
	// bridged alike (see defaultSettings): network reach is an explicit operator
	// choice (-access network / PINGULARITY_ACCESS=network), never inferred from
	// the environment. containerized feeds the status payload, the ignored-OPTS
	// warning, and the ambiguous-provenance WARNING below - nothing about the
	// environment, the store's age, or its shape ever decides access.
	containerized := util.InContainer()
	def := defaultSettings(p.cfg)
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
	// Name the release that initializes a brand-new store in its birth marker
	// (settings.KeyInstallBornVersion); the marker's presence - not this value -
	// is forward provenance: a store stamped by this build was born under the
	// fail-closed default, so its missing access key means "never chose", and the
	// ambiguity warning below stays quiet for it. dbCreated goes with it because
	// settings cannot see the filesystem: an install stopped before it ever
	// measured leaves a database whose contents read brand-new, so a version-only
	// stamp would mark an upgraded 0.61 store as born under this release and
	// silence the warning explaining its published port's new 403.
	setOpts = append(setOpts, settings.WithBornVersion(version), settings.WithDatabaseCreated(p.dbCreated))
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
	// A birth marker that is not on disk survives this boot only as long as the
	// process does (see settings.Controller.BornMarkerErr): the controller that
	// saw the store brand-new can still complete the stamp on a later reload or
	// settings write, but a restart is a controller that did not see it, and it
	// must refuse forever. Settings already said so on stderr - before this
	// logger existed - but repeat it here, or the ambiguity warning below recurs
	// forever on this install with no recorded cause. Nothing is DECIDED by the
	// missing marker (an unmarked store never opens access), so the daemon
	// carries on monitoring.
	if berr := set.BornMarkerErr(); berr != nil {
		earlyLog = append(earlyLog, func() {
			p.log.Warn("could not record this install's birth version at boot",
				"err", berr, "key", settings.KeyInstallBornVersion,
				"effect", "monitoring and access are unaffected. This daemon retries the stamp while it runs, but if it restarts unmarked, the install's provenance is unprovable from then on: in a container it warns about ambiguous access provenance on every boot until -access/PINGULARITY_ACCESS is passed explicitly")
		})
	}
	// Materialize the first-run Quick Setup decision - but only on a LOADED
	// controller (ErrLegacyReseal means loaded; a failed load must not take a
	// sticky decision from defaults: the offer-clock write has no loaded-guard,
	// and once seeded it would hold an established install's monitoring for
	// 48h). Skipping costs nothing - the same decision runs next boot, or after
	// the retry loop's successful Reload via the next restart.
	if settings.LoadedOK(err) {
		// Record the first-run decision on the loaded controller (consent-by-flags
		// then offer seeding). Deferred logging - the real logger isn't up yet. The
		// settings-retry loop runs the SAME step after a successful reload, so a
		// first boot whose settings load FAILED still gets the decision (and a fresh
		// install's consent hold) once settings come back, instead of quietly
		// starting to probe.
		p.materializeQuickSetup(ctx, set, func(msg string, e error) {
			earlyLog = append(earlyLog, func() { p.log.Warn(msg, "err", e) })
		})
		// A container install from 0.61 or earlier never STORED an access choice
		// (the filter was defaulted off by an unpersisted seed), so the
		// fail-closed default 403s its published port on upgrade. That install
		// is INDISTINGUISHABLE on disk from a container born private under a
		// build that predates the birth marker, so the daemon only SAYS so - it
		// never opens access on the guess (see warnAmbiguousContainerAccess).
		// The WARN rides earlyLog like the rest, so it survives the default
		// "off" level.
		// An EXPLICIT -access / PINGULARITY_ACCESS is authoritative over a
		// disagreeing stored access_local_only (see applyExplicitAccess) - the
		// recovery path for a container whose stored local-only would 403 its
		// own published port with no shell to fix it from. Deferred sinks: the
		// real logger is not up yet.
		p.applyExplicitAccess(ctx, set, true,
			func(msg string, args ...any) { earlyLog = append(earlyLog, func() { p.log.Warn(msg, args...) }) },
			func(msg string, args ...any) { earlyLog = append(earlyLog, func() { p.log.Info(msg, args...) }) })
	}
	// Every LATER load - the retry loop, a reload signal, a web import - runs
	// the same sequence through the controller's post-load hook, so no load
	// path can miss it. Registered before any of those can fire.
	p.registerSettingsLoadedHook(ctx, set)

	// The one always-on startup line, straight to stdout past the level gate.
	// After the access reconcile above so the mode it states is the one actually
	// in force this boot.
	fmt.Fprintln(os.Stdout, startupLine(version, p.cfg.ListenAddr, set.AccessLocalOnly()))

	// A fresh install measures NOTHING while the Quick Setup offer is open (see
	// newMonitoringLiveFn), and the startup line above is what a healthy install
	// prints too - so a headless or package install used to look fine, answer its
	// health endpoint, and collect nothing for up to two days with not one word
	// about why. Say it once, on the same stdout past the level gate that the
	// startup line uses, because the install this is for runs with logging "off".
	// It reads the SAME hold the measurement loops obey, so it cannot announce a
	// hold that isn't there; the one rough edge is a fresh install whose settings
	// failed to load, which is also held and gets this line while its dashboard
	// is still refusing to serve - the WARNING above is what explains that.
	if quickSetupHoldState(ctx, set) == qsHeld {
		fmt.Fprintln(os.Stdout, firstRunHoldLine(p.cfg.ListenAddr))
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
	// auth: a non-loopback listen AND the loopback filter off. Every install
	// defaults the filter ON (fail closed) and nothing but explicit operator
	// input ever turns it off, so this fires only where it was opened on purpose
	// (-access network / PINGULARITY_ACCESS, the Access tab) - exactly the
	// installs this warning is for.
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

	p.replayIgnoredOpts()

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
					p.handleReload(ctx, set)
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
	// qsOnHoldRelease fires ONCE at the hold-release edge (the CAS inside
	// newMonitoringLiveFn). Wired after netinfo exists (assignment happens
	// during single-threaded setup, before any loop that calls monitoringLive
	// spawns; the predicate dereferences it at fire time): the 48h expiry is a
	// pure clock event with NO settings broadcast, so without this poke
	// netinfo would sit on its minute-long disabled poll while the consent
	// speedtest's bounded readiness wait (sched.ReadyFn) runs out - and the
	// first-ever run would auto-select unanchored after all. The answer-path
	// release fires it too, where the loop's resume one-shot makes it a no-op.
	var qsOnHoldRelease func()
	monitoringLive := newMonitoringLiveFn(ctx, set, &qsOnHoldRelease)

	m := monitor.New(p.cfg, pr, p.store, p.log)
	m.IntervalFn = set.LatencyInterval
	m.WakeFn = set.Changed
	m.DownAfterFn = set.DownAfter
	m.UpAfterFn = set.UpAfter
	m.EnabledFn = monitoringLive
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
	ni.EnabledFn = func() bool { return monitoringLive() && set.NetinfoEnabled() }
	// Settings broadcasts wake the netinfo loop, so power-on refreshes NOW
	// instead of at the next minute tick - the first speedtest's server
	// selection (sched.ReadyFn below) is what rides on that promptness.
	ni.WakeFn = set.Changed
	qsOnHoldRelease = ni.Nudge // the broadcast-less 48h edge (see monitoringLive)
	seedKnownCounters()        // create fixed counters at 0 so a first event after restart is a visible 0->1 step (rate/increase can miss a series that appears at 1)

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
	notifier.FormatFn = set.WebhookFormat // the Alerts tab's payload-shape override
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
	// Auto server selection: the candidate cities the connection itself names
	// - the exit router's, the ISP geolocation's, the starred servers', the
	// last race's winner's, and the one the Ookla API places our address in -
	// each contribute their
	// closest servers to one deduplicated ping race, and the city whose server
	// answers fastest becomes the centre (see speedtest.raceCities and
	// autoOrigins).
	// A starred server's catalogue coordinate, for a pinned best-of run whose
	// pin the live catalogue could not place (see speedtest.recentrePin).
	tester.SavedCoordFn = newSavedCoordFn(set)
	recentWinner := newRecentWinnerFn(p.store)
	tester.OriginsFn = func() []speedtest.Origin { return autoOrigins(ni.Get(), set.SpeedServers(), recentWinner()) }
	// The user's ISP name grants its own server a guaranteed lane in the
	// auto-select ping race (an on-net server is the most likely winner). A
	// stale or empty name is harmless - it only adds or skips one racer.
	tester.ISPFn = func() string { return ni.Get().ISP }
	tester.PriorDataFn = newPriorDataFn(p.store)
	tester.IncumbentFn = newIncumbentFn(p.store)
	tester.ChallengeFn = newChallengeFn(p.store, set)
	tester.IncumbentScoresFn = newIncumbentScoresFn(p.store)
	// A server convicted of refusing every upload is excluded for hours, and
	// that verdict costs a whole measurement turn to earn (the cheap GET health
	// check cannot see the problem - see noteUploadRejection). Carry it across
	// restarts, or every restart pays for it again: measured on 2026-08-28, a
	// box that had restarted six minutes earlier spent 45 s of a Best-of round
	// on a server it had already convicted.
	if rows, err := p.store.ServerHealth(ctx); err != nil {
		p.log.Warn("could not read the saved server exclusions; starting with none", "err", err)
	} else if len(rows) > 0 {
		out := make([]speedtest.ServerHealthRow, 0, len(rows))
		for _, r := range rows {
			out = append(out, speedtest.ServerHealthRow{ServerID: r.ServerID, Expires: r.Expires, Fails: r.Fails})
		}
		speedtest.LoadServerHealth(out)
		p.log.Debug("reloaded server exclusions", "servers", len(out))
	}
	// Called from a run when it convicts a server. Its own short deadline: the
	// run is mid-round, and a slow write must not hold it up.
	speedtest.PersistServerHealth = func(r speedtest.ServerHealthRow) {
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.store.SaveServerHealth(wctx, store.ServerHealth{ServerID: r.ServerID, Expires: r.Expires, Fails: r.Fails}); err != nil {
			p.log.Warn("could not save a server exclusion; it will be forgotten on restart", "server_id", r.ServerID, "err", err)
		}
	}

	sched := speedtest.NewScheduler(tester, p.store, p.cfg.SpeedtestInterval, p.log)
	tester.OnServer = sched.SetCurrentServer        // surface the live server during a run
	tester.DirectionFn = set.SpeedDirection         // Ookla direction (per-engine; iperf3 has its own)
	tester.RetriesFn = set.SpeedRetries             // Ookla retries (per-engine; iperf3 has its own)
	tester.ConnectionsFn = set.OoklaConnections     // Ookla parallel connections (0 = auto)
	tester.LossFn = set.OoklaLoss                   // Ookla packet-loss probe
	tester.DiscardLosersFn = set.SpeedDiscardLosers // a Best-of round keeps only its winner, or every server it measured
	tester.BestOfCountFn = set.SpeedBestOfCount     // measure N servers, keep the best (scheduled/manual only)
	tester.FavouritesFn = func() []string {         // the starred servers: seats in every Best-of round
		saved := set.SpeedServers()
		ids := make([]string, 0, len(saved))
		for _, s := range saved {
			ids = append(ids, s.ID)
		}
		return ids
	}
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
	// Container networking failures deserve an explanation the errno never gives
	// (localhost is the container itself, host NICs/IPs don't exist in its network
	// namespace, the default bridge has no IPv6). The engine stays container-blind:
	// the matcher is injected only here, and only in a BRIDGED container (see
	// iperfEnvHintFn) - natively, and in a host-network container where localhost
	// IS the host, the same errors mean exactly what they say and get no hint.
	iperfTester.EnvHint = iperfEnvHintFn(util.BridgedContainer(), set)
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
		return monitoringLive() && set.SpeedtestEnabled() && set.SpeedAllowed(time.Now())
	}
	sched.ReadyFn = newFirstRunReadyFn(set, ni)
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
		if !monitoringLive() || !set.NetinfoEnabled() || i.UpdatedAt == 0 {
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
		if monitoringLive() && set.SpeedtestOnReconnect() && set.SpeedAllowed(now) {
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
		if !set.SpeedtestOnDegraded() || !set.SpeedtestEnabled() || !monitoringLive() {
			return 0
		}
		return set.DegradedPingMS()
	}
	m.OnDegraded = func() {
		if set.SpeedAllowed(time.Now()) {
			// Read on the monitor goroutine, inside the callback: this names THIS
			// dispatch, so a bounce reported late - after the episode ended, or after a
			// later dispatch replaced it - is ignored instead of re-firing an episode
			// that has already been served.
			id := m.DegradedDispatch()
			spawn(func() { degradedDispatch(ctx, sched.RunOnce, func() { m.RetryDegraded(id) }) })
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
	srv.InContainer = containerized       // informational only (status "containerized"); no longer gates access
	srv.SessionKey = sessionKey           // key-file-bound secret folded into session-token MACs (see tokenKey)
	srv.MetricsToken = p.cfg.MetricsToken // optional read-only scrape credential for /metrics
	srv.Update = upd                      // update status on /api/status + powers the toggle
	// The settings server-browsing list centres where auto last landed, and
	// before any auto run exists it falls back to these same candidates - so
	// the picker starts from cities the race would consider and then follows
	// what it actually chose (web.lastAutoRunServerID).
	srv.AutoOriginsFn = tester.OriginsFn
	srv.RaceListingFn = tester.RaceListing        // the picker's Auto button: the field a run would race
	srv.PingServersFn = speedtest.PingServersByID // the saved pane's refresh: re-measure the kept servers
	srv.Logs = p.ring                             // backs the About-tab log viewer (/api/logs)
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
// seedKnownCounters initializes the fixed, known operational counters, the
// float sums behind exported duration families (via stats.SeedF), and the
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
		// A Team Cymru lookup the host resolver failed and a public resolver
		// answered: climbing = the host's DNS is timing out on the origin zone.
		"netinfo.cymru_fallback",
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
		// A candidate was excluded from ranking because its optional HTTP Legacy
		// Fallback is absent - every transfer against it would fail. Climbing means
		// the reachable pool is shrinking; all_servers_no_fallback means it is empty
		// and runs are proceeding against servers that will fail.
		"speed.server_no_fallback", "speed.all_servers_no_fallback",
		// An upload retried single-stream because no chunk completed in the capture
		// window - the uplink is slower than the parallel chunk set needs.
		"speed.upload_starvation_retry", "speed.upload_starvation_rescued",
		// A by-ID lookup (the picker's Find, its pin probe, or the browse list's
		// last-run centring) found a server with no HTTP legacy fallback - one
		// that fails every run. One user action can count more than once.
		"speed.pinned_server_no_fallback",
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
		// ... and how many runs then ranked the race's own list and pings
		// instead of fetching and pinging the same servers again (every
		// decided race should; a gap means the run refetched).
		"speed.cityrace_field_reused",
		// The auto-select challenger: runs that measured the incumbent's rival
		// instead of the incumbent, and how many of those took the seat.
		"speed.challenge", "speed.challenge_won", "speed.challenge_failed",
		"speed.pin_coord_unrecovered",
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
		// The /metrics label-collision disclosure and the step-up security
		// counter (sibling of login_fail); both alert-worthy first events.
		"web.metrics_label_collisions", "web.stepup_fail",
		// An unobserved stretch dropped for good (the retried sibling above
		// eventually lands; this one is the loss) and the OTHER speedtest
		// panic counter - transfer_panic and transport_panic are distinct.
		"monitor.unobserved_gap_dropped", "speed.transport_panic",
		// Import/restore repairs: rows the importer refused, disclosed on
		// /metrics rather than silently absent from history.
		"import.event_duration_dropped", "import.pause_dropped",
		// The summary count for run durations (its float sum is seeded below).
		"speed.duration_n",
		// Chart-aggregate cache accounting for /api/series. Every Store.Series
		// call books exactly one of the outcomes below - a cache hit, one of the
		// cache misses, or a sub-minute bypass that never consults the cache -
		// and every outcome but the hit runs a scan, which books series.query
		// (recorded in Store.Series and seriesQuery, internal/store/store.go;
		// exported as the pingularity_series_* families by writeNamedStats,
		// internal/web/web.go). So a scrape reads queries = outcomes - hits: the
		// families are read against each other, hits against queries, and one
		// member appearing at 1 while the rest sit at 0 skews a ratio rather
		// than only delaying its own first step. Every state the store
		// records belongs here for that reason; add the next one to this list
		// and to the named-family table in writeNamedStats together.
		"series.cache.hit", "series.cache.expired", "series.cache.new",
		"series.cache.empty", "series.bypass", "series.query",
	}
	// Float sums behind exported duration families: absent-until-first-event
	// means the family itself is missing from scrapes, hiding the first
	// outage/run/prune from rate() exactly like an unseeded counter would.
	stats.SeedF("monitor.outage_s_sum", "speed.duration_s_sum", "db.prune_ms_sum")
	for _, cls := range []string{"timeout", "refused", "net_unreachable", "host_unreachable", "dns", "other"} {
		names = append(names, "probe.fail."+cls, "dns.fail."+cls)
	}
	for _, trig := range []string{"startup", "scheduled", "reconnect", "degraded", "manual"} {
		names = append(names, "speed.run."+trig)
	}
	for _, stage := range []string{"server_list", "server_fetch", "no_servers", "ping", "na", "download", "upload", "bidir", "other"} {
		names = append(names, "speed.fail."+stage)
	}
	for _, dest := range []string{"discord", "slack", "healthchecks", "ntfy", "generic", "heartbeat"} {
		names = append(names, "notify."+dest+".ok", "notify."+dest+".fail", "notify."+dest+".blocked", "notify."+dest+".lat_n")
		stats.SeedF("notify." + dest + ".lat_ms_sum")
	}
	stats.Seed(names...)
}

// newMonitoringLiveFn builds monitoringLive, the master switch the measurement
// loops actually obey: the power button AND the first-run hold. While the Quick
// Setup offer is open the daemon measures nothing - the dialog's button is
// called Start monitoring and it must be telling the truth. The hold can only
// release (answer or 48h expiry), so once seen released it is never re-checked:
// the settings read is cheap but the offer clock is a DB read, and this runs
// every round. onHoldRelease is a pointer so run() can wire the netinfo nudge
// after the manager exists; it is dereferenced only at the release edge.
func newMonitoringLiveFn(ctx context.Context, set *settings.Controller, onHoldRelease *func()) func() bool {
	var qsHoldReleased atomic.Bool
	return func() bool {
		if !qsHoldReleased.Load() {
			switch quickSetupHoldState(ctx, set) {
			case qsHeld:
				return false
			case qsProvisional:
				// Latch nothing here (see quickSetupHoldState): the real hold has
				// to take over once the offer clock is seeded.
				return set.Monitoring()
			case qsReleased:
				// Permanent, so latch it and stop paying for the offer-clock read
				// every round; the one-shot nudge is the 48h edge (see above).
				if qsHoldReleased.CompareAndSwap(false, true) && onHoldRelease != nil && *onHoldRelease != nil {
					(*onHoldRelease)()
				}
			}
		}
		return set.Monitoring()
	}
}

// qsHoldState is what the first-run consent hold says about monitoring right
// now: held, released for good, or a provisional pass that nothing may latch.
type qsHoldState int

const (
	// qsHeld: the first-run offer is open (or the state cannot be read and the
	// install looks genuinely fresh) - measure nothing.
	qsHeld qsHoldState = iota
	// qsProvisional: the hold does not apply to THIS install, but the decision
	// rests on state that is still settling (settings unloaded, or loaded with
	// the offer clock not yet seeded), so a caller must not record it as final.
	qsProvisional
	// qsReleased: answered, or the grace expired. Permanent - the hold can only
	// release, never come back - so a caller may latch this and stop asking.
	qsReleased
)

// quickSetupHoldState is the ONE reading of the first-run hold. The monitoring
// predicate obeys it and the boot notice reports it, and those two must never
// disagree: a boot line announcing a hold the loops don't have (or, worse, a
// silent boot that measures nothing) is exactly the confusion this line exists
// to end.
func quickSetupHoldState(ctx context.Context, set *settings.Controller) qsHoldState {
	// Until settings load, the first-run decision can't be made and the
	// offer clock isn't seeded - QuickSetupHold below would then read a bare
	// offer_since==0 and fail OPEN. Hold a GENUINELY FRESH install (nothing
	// in the store: no history, anchor, or config) instead, so it can't
	// start probing unasked - but do NOT latch the release, so the normal
	// hold takes over once the retry loads settings and seeds the clock. An
	// established install already consented and keeps running; a store-read
	// error holds too (the fresh direction).
	if !set.Loaded() {
		if est, err := set.EstablishedInStore(ctx); err != nil || !est {
			return qsHeld
		}
		return qsProvisional
	}
	// Answered is decided FIRST: it is permanent, it lives in the loaded
	// settings (no store read), and QuickSetupHold(true, ...) is false for
	// every clock value - so nothing the offer clock could say changes the
	// answer. Deciding it after the store read put an ANSWERED install on the
	// first-run hold whenever that read failed transiently (a busy disk at
	// boot): monitoring paused and the boot notice claimed a first run, on an
	// install that consented long ago. Held-on-error is the right direction
	// only for the unanswered cases below - and this order also spares
	// monitoringLive a store-wide read per probe round on every answered
	// install.
	if set.QuickSetupDone() {
		return qsReleased
	}
	// Read the offer clock with its error surfaced: a transient store read
	// error must HOLD, not read as "no clock -> release" and then latch that
	// release forever off a single failed read. (Only unanswered installs
	// reach here, so held is always the safe direction now.)
	since, err := set.QuickSetupOfferSinceErr(ctx)
	if err != nil {
		return qsHeld
	}
	// LOADED with Quick Setup unanswered and NO offer clock is the same
	// bare-zero hazard one step later: the first-run decision has not been
	// materialized yet (a late/SIGHUP Reload flips Loaded and broadcasts
	// before the offer clock lands - the recovery paths pre-seed to close
	// that window, but this must not depend on it - or the offer write
	// failed at boot). QuickSetupHold reads the bare zero as "never
	// offered", and a caller latching that release would make it PERMANENT -
	// a fresh install probing without first-run consent. Same treatment
	// as the unloaded case: hold a genuinely fresh install, keep an
	// established one running, and latch nothing so the real hold takes
	// over once the clock is seeded.
	if !set.QuickSetupDone() && since == 0 {
		if est, err := set.EstablishedInStore(ctx); err != nil || !est {
			return qsHeld
		}
		return qsProvisional
	}
	if settings.QuickSetupHold(set.QuickSetupDone(), since, time.Now().Unix()) {
		return qsHeld
	}
	return qsReleased
}

// seedQuickSetupOfferEarly seeds the first-run offer clock on a controller
// that has NOT loaded yet, BEFORE a recovery Reload can flip Loaded()==true
// and broadcast a wake (Reload does both internally). Seeding only after
// Reload returns would leave a window where a woken monitoringLive sees a
// loaded controller with a bare offer_since==0 - the fail-closed branch there
// holds it, but the window must not exist in the first place, and the status
// endpoint's quick_setup_pending has no such guard. EnsureQuickSetupOffer
// reads the store directly (valid while unloaded) and the clock write has no
// loaded-guard, so the fresh-install half works here; the established
// install's answered marker is a settings write that refuses while unloaded
// (ErrSettingsUnavailable) - expected and harmless, materializeQuickSetup
// re-runs after the successful Reload and takes that half then. No-op once
// loaded, so the SIGHUP path can call it on every signal.
func (p *program) seedQuickSetupOfferEarly(ctx context.Context, set *settings.Controller) {
	if set.Loaded() {
		return
	}
	if err := set.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil && !errors.Is(err, settings.ErrSettingsUnavailable) {
		p.log.Warn("quick setup offer pre-seed failed; retaken after reload", "err", err)
	}
}

// materializeQuickSetup records the first-run decision on a LOADED controller:
// consent-by-flags (or -quick-setup=skip) marks Quick Setup answered, then
// EnsureQuickSetupOffer seeds the offer clock for a genuinely fresh install or
// marks an established one answered. Idempotent. Both boot and the settings-retry
// loop call it - the retry is the ONLY path that runs it when a first boot's
// settings load failed, and without it a fresh install would never get its offer
// clock or its consent hold and would start probing unasked. warn(message, err)
// lets boot defer logging (logger not up yet) while the retry logs immediately.
func (p *program) materializeQuickSetup(ctx context.Context, set *settings.Controller, warn func(string, error)) {
	if (p.cfg.MonitoringConsent || p.cfg.QuickSetupSkip) && !set.QuickSetupDone() {
		if err := set.SetQuickSetupDone(ctx, true); err != nil {
			warn("quick setup consent-by-flags mark failed; retaken next boot", err)
		}
	}
	if err := set.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		warn("quick setup offer decision failed; retaken next boot", err)
	}
}

// handleReload is a reload signal's whole effect (SIGHUP; `systemctl reload`).
// A SIGHUP is a THIRD path (besides boot and the retry loop) that takes an
// unloaded controller to loaded - e.g. a first boot whose settings load
// failed, recovered by SIGHUP before the retry ticked. Seed the offer clock
// BEFORE Reload flips Loaded and broadcasts (a no-op on a loaded controller),
// for the same latch window the retry loop closes.
func (p *program) handleReload(ctx context.Context, set *settings.Controller) {
	p.seedQuickSetupOfferEarly(ctx, set)
	// ErrLegacyReseal means LOADED - settings are fully live, only the legacy
	// iperf3 password re-encryption failed - and the boot path and retry loop
	// both continue on it. Bailing here skipped the whole post-load sequence
	// on exactly the installs where no other path would ever run it.
	if err := set.Reload(ctx); !settings.LoadedOK(err) {
		p.log.Warn("settings reload on signal", "err", err)
		return
	} else if err != nil {
		p.log.Warn("settings reloaded on signal; legacy iperf3 password re-encryption still failing", "err", err)
	} else {
		p.log.Info("settings reloaded on signal")
	}
	// Materialize the rest of the first-run decision here too (answer markers
	// refuse while unloaded), or a fresh install would never get its consent
	// hold and would start probing unasked. The access sequence already ran:
	// the controller's post-load hook fires inside Reload - on this path, the
	// retry loop, and the web import path alike - so a reload that pulls in a
	// stored value disagreeing with a still-set explicit override re-applies
	// the override every time.
	p.materializeQuickSetup(ctx, set, func(msg string, e error) { p.log.Warn(msg, "err", e) })
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
		// Offer clock FIRST, Reload second: Reload itself flips Loaded()==true
		// and broadcasts the wake, so seeding only afterwards (in the
		// materialize below) leaves a window where a woken monitoringLive sees
		// a loaded controller, an open Quick Setup, and a bare offer clock -
		// and would have latched its hold released for good.
		p.seedQuickSetupOfferEarly(ctx, set)
		if err := set.Reload(ctx); settings.LoadedOK(err) {
			p.log.Warn("settings loaded on retry; serving normally again")
			// The boot path skipped the first-run decision on the failed load. Take
			// the rest of it now (the offer clock was pre-seeded above; the answer
			// markers are settings writes that refuse while unloaded): a fresh
			// install must get its consent hold rather than having started probing
			// unasked while unloaded.
			p.materializeQuickSetup(ctx, set, func(msg string, e error) { p.log.Warn(msg, "err", e) })
			// The access sequence already ran: the controller's post-load hook
			// fires inside Reload, on this path and every other one.
			// This Reload is where a fresh install whose first load failed gets
			// its birth marker - the boot path already passed, and this loop stops
			// as soon as settings load, so it is this process's last SCHEDULED
			// attempt (a settings write can still complete a pending stamp). A
			// stamp still missing HERE is reported for the same reason (the
			// boot-path report above could not have run: it only reaches here when
			// New itself failed).
			if berr := set.BornMarkerErr(); berr != nil {
				p.log.Warn("could not record this install's birth version on the settings retry",
					"err", berr, "key", settings.KeyInstallBornVersion)
			}
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

// newPriorDataFn reports whether this install has any speed history yet. The
// first best-of round on a fresh install is the one that seeds the baseline
// later checks read, so it is decided on ping alone rather than on throughput
// nothing can vet yet (see firstRunByPing). Counted, not cached: it flips
// false->true after one run, and it is read at most once per speedtest run
// (RunReason asks firstRunByPing once, and only on a best-of round) - never per
// latency sample.
//
// It is a SCAN, not the indexed count the TableCounts version was: nothing
// indexes `failed`, so SQLite reads the speed table (EXPLAIN QUERY PLAN on
// store.SpeedCount's query answers "SCAN speed", where a bare COUNT(*) answered
// "SCAN speed USING COVERING INDEX idx_speed_ts"). Affordable because of WHAT it
// scans, not how it scans it: speed holds one row per speedtest run and is
// pruned by -retain-speed, so on the defaults (hourly, kept a year) the scan is
// over ~8760 rows, once an hour.
//
// SpeedCount, not TableCounts["speed"]: the raw table count also includes the
// usage-accounting rows a failed run leaves behind (store.speedNotFailed is
// what SpeedCount filters on, and it is the same predicate that hides those
// rows from every chart, table and threshold verdict). Counting them meant a
// first speedtest that failed after moving bytes looked like history, and the
// next run - the install's first actual measurement - skipped the ping-only
// bootstrap and was judged against a baseline nothing had measured.
func newPriorDataFn(st *store.Store) func() bool {
	return func() bool {
		n, err := st.SpeedCount(context.Background())
		if err != nil {
			return true // cannot tell: assume history exists rather than re-seeding
		}
		return n > 0
	}
}

// newIncumbentFn names the server the last AUTOMATIC Ookla run measured, for
// the run to keep while it stays within noise of the fastest (see
// speedtest.Ookla.IncumbentFn). Read once per auto run, at selection time. The
// walk skips pinned runs rather than stopping at one: a week pinned to a
// server and then un-pinned should resume the auto history, not start a new
// one on whichever box happens to ping fastest that hour. Bounded, so a
// longtime-pinned install reads a dozen rows and gives up, the same bound the
// browse centring uses (see internal/web lastAutoRunServerID). "" on any
// error: no incumbent is the pre-existing behaviour, and a wrong incumbent
// would still have to win its seat on this run's ping.
func newIncumbentFn(st *store.Store) func() string {
	return func() string {
		rows, err := st.LastSpeedWinners(context.Background(), 12)
		if err != nil {
			return ""
		}
		for _, r := range rows {
			// A challenger that LOST measured a server the seat did not go
			// to; skipping it is what makes a challenge an experiment rather
			// than a coin toss (see speedtest.ChallengeLost).
			if speedtest.PinnedRun(r.WinReason) || speedtest.ChallengeLost(r.WinReason) || r.ServerID == "" {
				continue
			}
			return r.ServerID
		}
		return ""
	}
}

// newChallengeFn says whether the next scheduled auto run is a CHALLENGE run:
// one that measures the incumbent's strongest rival instead of the incumbent
// (see speedtest.Ookla.ChallengeFn). Due when the last N AUTOMATIC winners -
// every unpinned Ookla run, whatever triggered it - hold no challenge at all,
// N being the Every setting; the challenge itself lands on the next scheduled
// run. Pinned runs are excluded in the query, so they neither count toward N
// nor, however long the pinned stretch, push the auto rows out of the window.
// Derived from the selection history rather than an in-memory counter, so a
// daemon restarted every day still challenges on schedule.
func newChallengeFn(st *store.Store, set *settings.Controller) func() bool {
	return func() bool {
		every := set.SpeedChallengeEvery()
		if every <= 0 {
			return false
		}
		rows, err := st.LastSpeedWinnersExcluding(context.Background(), every,
			speedtest.WinReasonPinned, speedtest.WinReasonPinnedBestOf, speedtest.WinReasonPinnedCompanion)
		if err != nil {
			return false
		}
		auto := 0
		for _, r := range rows {
			if speedtest.ChallengeRun(r.WinReason) {
				return false // challenged within the last N auto runs
			}
			auto++
			if auto >= every {
				return true
			}
		}
		return false
	}
}

// newRecentWinnerFn names the city that won the newest decided race, as an
// origin for the next one (see autoOrigins). Read live each run, like the
// other origins; nil with no decided race in the history or on any error -
// the pre-existing field, which is the safe reading.
func newRecentWinnerFn(st *store.Store) func() *speedtest.Origin {
	return func() *speedtest.Origin {
		label, lat, lon, ok, err := st.LastDecidedRace(context.Background())
		if err != nil || !ok {
			return nil
		}
		return &speedtest.Origin{Kind: "recent", Label: label, Lat: lat, Lon: lon, Anchored: true}
	}
}

// newIncumbentScoresFn hands the engine a server's recent measured scores
// under one test direction, newest first, for a challenge verdict (see
// speedtest.Ookla.IncumbentScoresFn).
func newIncumbentScoresFn(st *store.Store) func(id, dir string) []float64 {
	return func(id, dir string) []float64 {
		scores, err := st.RecentServerScores(context.Background(), id, dir, speedtest.ChallengeHistory)
		if err != nil {
			return nil
		}
		return scores
	}
}

// newSavedCoordFn looks a pinned server up in the saved picker list for the
// catalogue coordinate stored when it was starred. ok=false for a server that
// is not on the list or was starred without a coordinate (a by-ID row stores
// 0,0) - the run then falls back exactly as it did before the list existed.
//
// Named, like newFirstRunReadyFn, so a root test can pin it against the real
// controller: this lookup is the only reader of the saved picker list (the
// speed_servers SETTING - the speed_servers table of per-run reports has its
// own readers) outside the picker itself.
func newSavedCoordFn(set *settings.Controller) func(id string) (lat, lon float64, ok bool) {
	return func(id string) (float64, float64, bool) {
		for _, s := range set.SpeedServers() {
			if s.ID == id {
				return s.Lat, s.Lon, s.Lat != 0 || s.Lon != 0
			}
		}
		return 0, 0, false
	}
}

// newFirstRunReadyFn builds the consent run's selection-readiness predicate
// (speedtest.Scheduler.ReadyFn): with a server pinned the
// race doesn't need netinfo; a working iperf3 engine measures its own
// configured server (though iperf3-but-unavailable falls back to Ookla, so
// THAT case still waits); with connection info off nothing is ever coming;
// otherwise ready = netinfo has published at least once (even a failed fetch
// stamps UpdatedAt, so a dead lookup can't hold the first test past the
// scheduler's bounded wait).
//
// Known, accepted residual: the first publish carries the FAST fields; the
// exit-router hop is patched in only after its traceroute finishes, so the
// consent run can race {isp, saved..., recent, geo} while later runs race
// {exit, isp, saved..., recent, geo} (saved = the starred servers' cities,
// recent = the last race's winner, see autoOrigins).
// Waiting for the exit would hold the first test up to the trace's own 12s
// budget on every fresh install - a worse trade than a first selection
// centred on the ISP city (and the ISP-name lane usually seats the on-net
// server regardless, which is the exit-city winner in practice).
//
// A named constructor so the root tests can pin the composition against the
// real settings controller and netinfo manager - the wiring is the feature.
func newFirstRunReadyFn(set *settings.Controller, ni *netinfo.Manager) func() bool {
	return func() bool {
		if set.SpeedServerID() != "" {
			return true
		}
		if set.SpeedEngine() == "iperf3" && speedtest.IperfAvailable() {
			return true
		}
		if !set.NetinfoEnabled() {
			return true
		}
		return ni.Get().UpdatedAt != 0
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
// The origins are the connection's own cities plus the cities the user has
// starred a server in - a star is a statement about where good servers are,
// and it races like any other origin rather than overriding anything. A
// pinned server (speed_server_id) overrides the race.
func autoOrigins(i netinfo.Info, saved []settings.SavedServer, recent *speedtest.Origin) []speedtest.Origin {
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
	// The starred servers' cities. The picker stored each one's catalogue
	// coordinate when it was starred, and a star is the user saying "this is
	// where my good servers are" - so each city races every run, whatever the
	// connection-derived origins say. It is also the one origin that survives
	// exit discovery going dark (a resolver timing out on the ASN zone took the
	// exit out for an evening, measured, and the race then had only the ISP's
	// geolocation - a city 500 km from the servers that were faster).
	// pickOrigins folds stars in one city into one fetch and caps the fan-out;
	// the ISP lane inside the pool still seats the on-net server. Before geo,
	// because geo is usually the ISP city's pool again; after exit and isp,
	// because order names the silent-race fallback and those are measured.
	for _, s := range saved {
		if s.Lat == 0 && s.Lon == 0 {
			continue // starred from a by-ID reply: no coordinate (see speedtest.recentrePin)
		}
		out = append(out, speedtest.Origin{Kind: "saved", Label: s.Name, Lat: s.Lat, Lon: s.Lon, Anchored: true})
	}
	// The city that won the LAST race (see newRecentWinnerFn): the one
	// candidate that survives every lookup going dark with nothing starred -
	// the 2026-08-26 evening, where a resolver timeout took the exit out and
	// Montréal was never entered. A candidate, not a verdict: it still has to
	// win on ping, so a city the user has since moved away from costs one
	// fetch and a few pings and loses. On an ordinary day it is the exit or a
	// star's city again and pickOrigins folds it into that one. After the
	// stars, so a star is never displaced by it at the anchored cap; before
	// geo for the reason the stars are.
	if recent != nil && recent.Anchored && (recent.Lat != 0 || recent.Lon != 0) {
		out = append(out, speedtest.Origin{Kind: "recent", Label: recent.Label, Lat: recent.Lat, Lon: recent.Lon, Anchored: true})
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

// degradedDispatch runs the degradation-triggered speedtest and hands the episode
// back when the run never started. Extracted from run() for the same reason as
// reconnectGate: the ErrBusy branch is the whole point and no test can reach it
// inside the closure.
//
// RunOnce answers ErrBusy the moment another run (scheduled, manual, reconnect)
// owns the runner. That dispatch measured nothing, and the monitor's
// once-per-episode latch otherwise re-arms only when latency RECOVERS - so one
// collision costs the entire brownout its measurement. retry re-arms the episode
// instead, and the next confirmed degraded round dispatches again.
//
// Any other error is a real attempt (it reached the network, and may even have
// moved billable bytes), so it keeps the episode consumed: a link that is broken
// rather than busy must not be re-tested every round for the length of a brownout.
func degradedDispatch(ctx context.Context, run func(context.Context, string) (store.SpeedSample, error), retry func()) {
	if _, err := run(ctx, "degraded"); errors.Is(err, speedtest.ErrBusy) {
		retry()
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
				// Dropped on purpose: the watchdog on the other end already treats a
				// missed ping as the alert, so there is nothing for this loop to do
				// about a failure. Heartbeat has logged it.
				_ = notify.Heartbeat(ctx, client, set.HeartbeatURL(), p.log)
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
	// The stat above refuses to create a database, so this command never witnesses
	// a birth. Without saying so, an empty-but-existing store would come out of
	// here wearing a birth marker naming the release that cleared its password.
	set, _ := settings.New(context.Background(), st, settings.Values{}, settings.WithDatabaseCreated(false))
	if err := set.ClearAuth(context.Background()); err != nil {
		return err
	}
	fmt.Printf("Authentication disabled and password cleared (%s).\n", cfg.DBPath)
	fmt.Printf("If the service is currently running, restart it (`%s`)\n", elevate("pingularity restart"))
	fmt.Println(" - it caches settings in memory and would keep enforcing the old password.")
	return nil
}

// healthzDefaultAddr is where `pingularity healthz` probes when no -addr is
// given: the daemon's default listen port, reached over loopback. CONTRACT: the
// official images wire HEALTHCHECK ["/pingularity","healthz"] against exactly
// this default - change it only together with the Dockerfiles.
const healthzDefaultAddr = "127.0.0.1:9000"

// healthzCmd implements `pingularity healthz`: probe a running instance's
// /healthz and report by exit code - 0 when it answers 200, nonzero (with a
// one-line stderr reason via fail) otherwise. Built for the container
// HEALTHCHECK, where the image ships no curl/wget: plain HTTP, no auth
// (/healthz is exempt from the guard, filters included), and a short timeout so
// a hung daemon fails the probe instead of hanging it.
func healthzCmd(args []string) error {
	fs := flag.NewFlagSet("healthz", flag.ContinueOnError)
	addr := fs.String("addr", healthzDefaultAddr, "host:port the running instance listens on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// The same footgun config.ParseFlags rejects for `run`/`install`, and it is
	// worse here: Go's flag package stops at the first non-flag token, so
	// `healthz typo -addr 10.0.0.5:9001` never sees -addr, probes
	// healthzDefaultAddr instead, and exits 0 if anything healthy answers there
	// - a liveness probe reporting green for a machine nobody asked about,
	// silently. Refuse before the probe rather than reporting on the wrong one.
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", rest[0])
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + *addr + "/healthz")
	if err != nil {
		return fmt.Errorf("healthz: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s answered %s", *addr, resp.Status)
	}
	return nil
}

// reconcileAccess makes an EXPLICITLY-passed -access flag / PINGULARITY_ACCESS
// env authoritative over the stored access_local_only at boot, persisting the
// operator's choice through the settings controller. cfg.Access normally only
// seeds the FRESH-INSTALL default (defaultSettings; stored keys win via the
// overlay), so a bridged container that persisted local_only=true under the old
// warn-only regime was 403'd off its own published port on upgrade - including
// the Access tab - and the distroless image has no shell to repair it. The
// we-never-guess rule holds: only explicit input overrides (the silent "local"
// default reconciles nothing), and it overrides in EITHER direction. Call only
// on a LOADED controller - reconciling against compiled-in defaults would take
// a persistent decision from values nobody chose.
func reconcileAccess(ctx context.Context, cfg config.Config, set *settings.Controller) (changed bool, err error) {
	if !cfg.AccessExplicit {
		return false, nil
	}
	wantLocalOnly := cfg.Access != "network"
	if set.AccessLocalOnly() == wantLocalOnly {
		return false, nil
	}
	return true, set.SetAccessLocalOnly(ctx, wantLocalOnly)
}

// registerSettingsLoadedHook makes every FUTURE settings load run the access
// sequence: the retry loop, the reload signal, and the web import path - which
// calls Controller.Reload directly and is invisible to main's handlers, so a
// restored backup carrying access_local_only could adopt a lockout over a
// still-set explicit override. The boot load has already run its own sequence
// (through earlyLog, before the log level is known) by the time this
// registers. The ambiguity warning rides only firstLoad: it is a boot-shaped
// explanation ('re-judged next boot'), and re-emitting it on every reload of
// an ambiguous container - which reconcileAccess can never resolve without
// the explicit flag - would spam a WARN and a store-wide read per HUP.
func (p *program) registerSettingsLoadedHook(ctx context.Context, set *settings.Controller) {
	set.OnLoaded(func(firstLoad bool) {
		p.applyExplicitAccess(ctx, set, firstLoad,
			func(msg string, args ...any) { p.log.Warn(msg, args...) },
			func(msg string, args ...any) { p.log.Info(msg, args...) })
	})
}

// applyExplicitAccess runs the post-load access sequence - the container
// ambiguity warning, then the explicit -access/PINGULARITY_ACCESS override -
// and must run on EVERY path that takes settings from unloaded to loaded, and
// on every later reload. The docs promise the explicit override is
// authoritative at every start; it used to be applied only on the boot whose
// FIRST settings read succeeded, so the one boot most likely to be a
// container-lockout recovery attempt (a restart mid-fault, recovered by the
// retry loop) silently ignored the override and kept answering 403 - with no
// shell in the image to see why. A reload needs it for the same reason from
// the other direction: a reload can pull in a disagreeing stored value (an
// out-of-band edit, a restored backup), and the still-set override must beat
// it. reconcileAccess itself keeps the we-never-guess rule: without the
// explicit flag this warns at most and writes nothing.
// warn/info abstract the sink: the boot path replays through earlyLog before
// the log level is known; every other caller logs directly.
func (p *program) applyExplicitAccess(ctx context.Context, set *settings.Controller, warnAmbiguity bool,
	warn func(msg string, args ...any), info func(msg string, args ...any)) {
	if warnAmbiguity {
		if werr := warnAmbiguousContainerAccess(ctx, p.cfg, p.store, set, util.InContainer(), warn); werr != nil {
			warn("container access provenance could not be judged; nothing changed, re-judged next boot", "err", werr)
		}
	}
	if changed, aerr := reconcileAccess(ctx, p.cfg, set); aerr != nil {
		warn("explicit -access/PINGULARITY_ACCESS could not override the stored access setting", "err", aerr)
	} else if changed {
		localOnly := p.cfg.Access != "network"
		info("stored access setting overridden by explicit operator input",
			"access", p.cfg.Access, "access_local_only", localOnly,
			"why", "-access/PINGULARITY_ACCESS was passed explicitly, and explicit operator intent beats the stored value")
	}
}

// accessAmbiguousWarnMsg is the one WARN warnAmbiguousContainerAccess emits.
// Kept as a constant so the wiring test can pin the exact line an operator has
// to find in the log when their published port starts answering 403.
const accessAmbiguousWarnMsg = "container install with no recorded access choice: access stays LOCAL-ONLY, so a published port answers 403 until you opt in"

// warnAmbiguousContainerAccess EXPLAINS a container boot whose access
// provenance cannot be established. It decides nothing and writes NOTHING - it
// only calls warn (at most once) with the situation and the way out.
//
// Containers from 0.61 or earlier defaulted the loopback-only filter OFF
// through an unpersisted seed, so an install that never touched the Access tab
// stored no access_local_only and its dashboard answered the network. The
// default fails closed now, which 403s such an install's published port on
// upgrade - and the distroless image has no shell to repair it from. An earlier
// attempt tried to spare those installs by PERSISTING access_local_only=false
// whenever the store looked that old (established, no birth marker, no stored
// access key). That inference is unsound and has been removed:
//
//   - the birth marker (settings.KeyInstallBornVersion) landed AFTER the
//     fail-closed default did. A container first installed by a build from that
//     window was therefore born PRIVATE yet carries no marker - byte-identical
//     on disk to a genuine 0.61-or-earlier install, as is any pre-marker
//     database copied into a container;
//   - so the migration's evidence proved only age, not that anything was ever
//     reachable, and on that whole population it silently persisted
//     network-reachable access. The default listen is every interface and auth
//     is usually off, so the cost of guessing wrong is an unannounced,
//     unauthenticated dashboard on the LAN;
//   - no other on-disk signal separates the two: date heuristics (install
//     anchor, oldest sample) re-open the same hole through a restored backup.
//
// Ambiguity therefore fails CLOSED. The cost is bounded and self-announcing:
// the 403 body names the three ways to open access, an explicit
// -access/PINGULARITY_ACCESS is authoritative at EVERY boot (reconcileAccess)
// so `-e PINGULARITY_ACCESS=network` restores a locked-out container without a
// shell, and the recovery is documented. A recoverable, self-diagnosing lockout
// beats a silent open port; a heuristic may advise, never persist.
//
// The detection below is the removed migration's, minus the write - it fires
// only when ALL of these hold:
//
//   - the process runs in a container (natively the filter always defaulted ON,
//     so nothing about the upgrade changed);
//   - no access_local_only key is STORED. Key PRESENCE, not the overlaid value:
//     the overlay answers the flag-seeded default when nothing is stored, which
//     is indistinguishable from a stored agreement. A stored key - the
//     operator's or quick setup's - means the choice was made, so there is
//     nothing to explain;
//   - no birth marker is STORED. Presence is READ here as "initialized by a
//     build that already defaulted closed, so its missing access key means never
//     chose and the install was never reachable to begin with". Sound for
//     markers written under the creation verdict (settings.WithDatabaseCreated);
//     not for the ones v0.70.0-rc.1 through v0.80.0-rc.2 wrote on emptiness
//     alone, so a 0.61 container still un-established when one of those releases
//     opened it came out marked, and this gate stays silent about its 403.
//     Accepted: nothing can un-stamp those markers, and ignoring presence would
//     instead warn every genuinely fresh container born in that window;
//   - the STORE is established (settings.EstablishedInStore - the same signal
//     the quick-setup upgrade gate keys on), so a genuinely fresh container,
//     which was never reachable either, is not lectured;
//   - no EXPLICIT -access/PINGULARITY_ACCESS was passed. Explicit operator
//     input settles access in either direction (reconcileAccess persists it),
//     so there is no ambiguity left to report.
//
// A store read error reports nothing and is returned, so the caller can log it
// and the same judgement runs again next boot (the EnsureQuickSetupOffer
// failure direction).
func warnAmbiguousContainerAccess(ctx context.Context, cfg config.Config, st *store.Store, set *settings.Controller, inContainer bool, warn func(msg string, args ...any)) error {
	if !inContainer || cfg.AccessExplicit {
		return nil
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		return err
	}
	if _, stored := all["access_local_only"]; stored { // settings' keyAccessLocalOnly
		return nil
	}
	if _, born := all[settings.KeyInstallBornVersion]; born {
		return nil
	}
	est, err := set.EstablishedInStore(ctx)
	if err != nil || !est {
		return err
	}
	warn(accessAmbiguousWarnMsg,
		"why", "this store carries no birth marker and no stored access choice - the shape left behind BOTH by a container upgraded from 0.61 or earlier, whose dashboard answered the network by default, and by one born private under a build too old to record its birth; nothing on disk tells them apart, and guessing open would put an unauthenticated dashboard on the LAN",
		"fix", "if this dashboard was reachable from the network before, restart with -access network (-e PINGULARITY_ACCESS=network) - it is authoritative at every boot - and set a password at the same time; you can also turn network access on from the machine itself in the Access tab",
		"access_local_only", true)
	return nil
}

// defaultSettings is the fresh-install configuration: the seed values a brand-new
// database is created with. Extracted from run() so a test can execute it - the
// literal has three same-typed retention durations and two same-typed intervals
// side by side, so a field swap compiles and every package-level test still
// passes; only asserting the mapping catches it. See TestDefaultSettings.
func defaultSettings(cfg config.Config) settings.Values {
	// Access is loopback-only by default EVERYWHERE (native, host-net, bridged) -
	// fail closed. Opening the dashboard to the LAN is an explicit operator choice
	// (-access network / PINGULARITY_ACCESS=network), never inferred from the
	// container's network mode. A container that publishes a port sets it; the
	// tradeoff (a published port 403s until then) is deliberate - we never guess.
	networkAccess := cfg.Access == "network"
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
		AccessLocalOnly:    !networkAccess, // loopback-only unless the operator explicitly opted into network access
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
		IperfStreams:       8,              // iperf3 parallel TCP streams: one stream measures its own congestion window, not the link (measured 116 vs 268 Mbps, 1 vs 8, on a ~350 Mbps path)
		IperfOmit:          1,              // iperf3 warm-up seconds discarded (skip TCP slow-start)
		IperfUDP:           true,           // iperf3 packet-loss/jitter UDP pass on by default
		IperfWindow:        0,              // iperf3 TCP window/socket-buffer KB (0 = OS auto-tune)
		OoklaLoss:          true,           // Ookla packet-loss UDP probe on by default
		SpeedDiscardLosers: true,           // a Best-of round records only its winner; every result is opt-in
		SpeedBestOfCount:   1,              // one server per test; a Best-of round costs N times the data - opt in
		// Auto-select challenger: twice a day at the hourly default, no extra
		// data; the bar the rival must clear is derived from the incumbent's own
		// record (see speedtest.challengeWon), so there is no margin to set.
		SpeedChallengeEvery: 12,
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

// ignoredOptsMsg is worded for `docker logs`: it names the variable (never its
// value - /etc/default/pingularity commonly carries secrets) and says where
// flags actually go in a container. "Expanded", not "read": the binary itself
// never parses the variable anywhere - on native installs it is systemd that
// expands it into argv before exec.
const ignoredOptsMsg = "PINGULARITY_OPTS is set, but Pingularity's official container images do not expand it - " +
	"only Pingularity's Linux systemd units do (/etc/default/pingularity). " +
	"Pass flags as image arguments instead (compose `command:` / `docker run <image> <flags>`)."

// ignoredOptsWarning reports whether a set PINGULARITY_OPTS should draw the
// stale-configuration warning. The contract this speaks for is the OFFICIAL
// images: both use an exec-form ENTRYPOINT, nothing in them expands the
// variable, and this binary never parses it, so a non-blank value there is dead
// configuration - including when the operator ALSO passed equivalent flags via
// the image arguments (the variable itself is still stale). On a native install
// the systemd unit expands the same variable into this process's argv, so the
// set-and-working case stays silent via inContainer=false.
//
// InContainer is a heuristic, and the warning's claims are scoped accordingly:
// a custom wrapper image that expands the variable itself, or a systemd-managed
// container, would still warn (the message names the official images); a
// container runtime the marker files miss, or a manual native launch where the
// variable is equally ignored, stays silent. Membership of the variable's
// tokens in os.Args is deliberately NOT the rule - `-db` is in every
// container's argv from the ENTRYPOINT, so a value like "-db /custom/path"
// (the most dangerous ignored flag) would never warn under token matching.
func ignoredOptsWarning(opts string, inContainer bool) bool {
	return strings.TrimSpace(opts) != "" && inContainer
}

// iperfEnvHintFn returns the matcher run() wires into Iperf.EnvHint, or nil
// when no hint may ever fire. bridged is util.BridgedContainer() in production:
// every hint below explains a BRIDGED-namespace failure (a private localhost,
// missing host NICs/IPs, the v6-less default bridge), and every one of them is
// WRONG in a host-network container, where localhost IS the host and its NICs
// are visible - keying on merely "containerized" sent host-net operators
// chasing host.docker.internal for a server plain 127.0.0.1 does reach. A named
// constructor so the gate's key (bridged, not containerized) is pinned by test.
func iperfEnvHintFn(bridged bool, set *settings.Controller) func(errText string) string {
	if !bridged {
		return nil
	}
	return func(errText string) string {
		return iperfContainerHint(errText, set.IperfServer(), set.IperfBind(), set.IperfIPVer())
	}
}

// iperfContainerHint maps an iperf3 transfer/UDP-probe failure onto the
// container-networking mistake that produces it, so the surfaced error names the
// fix instead of a bare errno. Wired into the engine (Iperf.EnvHint) ONLY in a
// bridged container (see iperfEnvHintFn) - natively, and in a host-network
// container, these errors mean what they say and get no hint.
// server/bind/ipver are the settings the failed run used; the mapping is a pure
// function over the error text plus those (never the environment), so it is
// testable anywhere and the speedtest engine stays container-blind. Returns ""
// for anything it doesn't recognize - hints are appended text, never behavior.
func iperfContainerHint(errText, server, bind, ipver string) string {
	s := strings.ToLower(errText)
	has := func(sub string) bool { return strings.Contains(s, sub) }
	switch {
	// --bind-dev with a host NIC name: interface names don't cross network
	// namespaces, so SO_BINDTODEVICE fails ENODEV inside the container.
	case bind != "" && (has("no such device") || has("enodev") || has("bad interface")):
		return fmt.Sprintf("host interface %q does not exist inside the container's network namespace; bind a container address instead, or run the container with host networking", bind)
	// --bind with a host IP: the address isn't assigned in the container's
	// namespace, so bind() fails EADDRNOTAVAIL. Checked before the -6 class:
	// with an explicit bind set, the bind is the likelier direct cause.
	case bind != "" && (has("cannot assign requested address") || has("eaddrnotavail")):
		return fmt.Sprintf("bind address %q does not exist inside the container's network namespace; use the container's own address, or host networking to bind host IPs", bind)
	// A forced -6 on the default Docker bridge, which carries no IPv6: connect
	// fails unreachable (no route) or cannot-assign (no v6 source address).
	case ipver == "6" && (has("unreachable") || has("cannot assign requested address") || has("eaddrnotavail")):
		return "the default Docker bridge has no IPv6; enable IPv6 on the container network or use host networking"
	// A loopback server target: inside a bridged container 127.0.0.1/localhost
	// is the container itself, so a server on the host refuses the connection.
	case has("connection refused") && loopbackIperfServer(server):
		return "the container has its own localhost, so an iperf3 server on the host is not reachable at " + server + "; use host.docker.internal or the host's LAN IP"
	}
	return ""
}

// loopbackIperfServer reports whether the configured iperf3 server points at
// loopback ("localhost", 127/8, ::1) - which inside a bridged container is the
// container itself, not the machine the operator meant.
func loopbackIperfServer(server string) bool {
	host := strings.TrimSpace(server)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else {
		host = strings.Trim(host, "[]") // bracketed IPv6 literal without a port
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// replayIgnoredOpts re-emits runCmd's ignored-PINGULARITY_OPTS finding into the
// structured log and ring once they exist (stderr already carried it before
// logging was up), so the About tab shows it too. Coverage split: automatic
// unit coverage (TestReplayIgnoredOpts) pins this helper's armed/unarmed
// behavior; the manually dispatched deep-test pins the production invocation
// in program.run and delivery through the real logger into the About ring via
// its /api/logs assertion. Static strings only - the variable's VALUE must
// never appear (see ignoredOptsMsg).
func (p *program) replayIgnoredOpts() {
	if !p.optsIgnored {
		return
	}
	p.log.Warn("PINGULARITY_OPTS is set, but the official container images do not expand it",
		"hint", "pass flags as image arguments (compose `command:` / `docker run <image> <flags>`)")
}

func runCmd(args []string) error {
	cfg, err := config.ParseFlags(args)
	if err != nil {
		return err
	}
	// Before anything that can exit early (a bad -db aborts startup well before
	// program.run): a container operator who set PINGULARITY_OPTS expecting it to
	// become flags gets told immediately, in `docker logs`, rather than debugging
	// a 403 from a flag that never existed. See ignoredOptsWarning for the rule.
	optsIgnored := ignoredOptsWarning(os.Getenv("PINGULARITY_OPTS"), util.InContainer())
	if optsIgnored {
		fmt.Fprintln(os.Stderr, "pingularity: WARNING: "+ignoredOptsMsg)
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
	// Logging is binary: when on, debug streams to both stdout/journald and the ring.
	// "Off" is not silence - it drops routine chatter but keeps WARN and ERROR (see
	// applyLogLevel) - so on a healthy install the ring sits idle rather than empty,
	// and fills exactly when something has gone wrong.
	// The ring keeps each line raw and PII-masked; the dashboard chooses which to show.
	log := buildLogger(os.Stdout, lvl, ring)
	prg := &program{cfg: cfg, log: log, logLevel: lvl, ring: ring, optsIgnored: optsIgnored}
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

// startupLine renders the single always-on boot line. The default log level is
// "off", so a healthy daemon - a container especially - would otherwise log
// NOTHING and `docker logs` would show an empty stream indistinguishable from a
// hung process. run() writes it straight to stdout, bypassing the level gate
// (the same precedent as the stderr security warnings). Exactly one line,
// operational shape only - version, listen address, access mode, dashboard URL -
// never a secret.
func startupLine(version, listenAddr string, localOnly bool) string {
	access := "network"
	if localOnly {
		access = "local-only"
	}
	return fmt.Sprintf("pingularity %s: listening on %s, access %s, dashboard at %s",
		version, listenAddr, access, dashboardURL(listenAddr))
}

// quickSetupHoldGrace is how long the first-run hold lasts before it expires on
// its own - the same window settings.QuickSetupHold enforces, restated here
// because the settings constant is unexported. NOTHING decides on this value:
// it is wording for the boot notice only, and TestFirstRunHoldNoticeStatesTheRealGrace
// pins it against the real predicate so the two cannot drift apart.
const quickSetupHoldGrace = 48 * time.Hour

// quickSetupHoldGraceText renders that window the way an operator reads it
// ("48h", not Go's "48h0m0s"). Both places that quote the deadline to an
// OPERATOR - the boot notice and the -quick-setup entry in usage() - render it
// from the constant through here, so neither can end up stating a deadline the
// daemon does not enforce (the constant itself is pinned to the real predicate
// by TestFirstRunHoldNoticeStatesTheRealGrace, and the usage entry to the
// constant by TestUsageDocumentsEveryFlagTheHoldNoticeAdvertises).
func quickSetupHoldGraceText() string {
	return strings.TrimSuffix(quickSetupHoldGrace.String(), "0m0s")
}

// firstRunHoldLine renders the boot notice for the one state that otherwise
// looks exactly like a healthy install while collecting nothing: the first-run
// Quick Setup hold. Same shape as startupLine - one line, operational facts
// only - and it names both exits (answer the dialog, or -quick-setup=skip) plus
// when the hold gives up on its own, so nobody has to find that in the docs.
func firstRunHoldLine(listenAddr string) string {
	return fmt.Sprintf("pingularity: first run: monitoring is on hold - nothing is being measured yet. "+
		"Open %s and answer Quick Setup to start it, or restart with -quick-setup=skip; "+
		"left unanswered it starts on its own %s after first launch.",
		dashboardURL(listenAddr), quickSetupHoldGraceText())
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
	}
	// Names ignore case and a trailing dot only roots one, so "LOCALHOST:9000" and
	// "localhost.:9000" bind 127.0.0.1 like "localhost:9000" does - an exact compare
	// warned about a dashboard nobody else could open. lanEntriesFor in internal/web
	// answers the same question this way.
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
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
// It is the PRE-SETTINGS-LOAD pin only, where nothing should be emitted yet. The
// "off" SETTING is not this silent: applyLogLevel maps it to WARN so a failure
// still leaves a record. Logging is otherwise binary: on = the single maximal
// level (debug).
const logLevelOff = slog.Level(1 << 20)

// applyLogLevel sets the live logger threshold. "off" is not silence: it drops
// routine INFO/DEBUG but keeps WARN and ERROR, for the reason the branch below
// gives. Any other value is the one "on" level - full debug to stdout/journald
// and the ring.
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

// usage prints the curated help behind `pingularity help`, `-h` and `--help`.
// It is hand-written rather than flag's PrintDefaults so the flags arrive in a
// useful order with room to explain themselves - which means nothing but a test
// keeps it honest about what internal/config actually defines. It drifted twice
// for that reason (-quick-setup, then -access, the flag that decides whether
// anyone but loopback may open the dashboard at all), so
// TestCuratedHelpDocumentsEveryRunFlag now reads the flag list out of the real
// FlagSet and requires an entry here for each name, in both directions.
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
  pingularity healthz          Probe a running instance's /healthz (exit 0 = healthy;
                               -addr host:port, default 127.0.0.1:9000)
  pingularity version          Print version

Run flags (all optional - defaults work with no flags):
  -access string   Who may open the dashboard: 'local' (loopback only, the
                   default) or 'network' (reachable from the LAN - set a login
                   too). A container that publishes a port needs 'network', or
                   -e PINGULARITY_ACCESS=network; without it every request from
                   off the machine is refused with a 403 explaining this - bar
                   /healthz and /readyz, which answer any peer (verdict only).
  -listen string   Web UI + metrics address (default ":9000" = all interfaces,
                   IPv4 + IPv6). Binding is not access: while -access is 'local'
                   only loopback gets in, whatever this binds. Narrow it to
                   127.0.0.1:9000 to refuse the connection outright instead.
  -allow-host s    Extra Host header values to accept, comma-separated. Only
                   needed behind a reverse proxy on a public domain - IPs,
                   localhost, dotless names, and .local/.lan/.home/.internal
                   always work (DNS-rebinding protection).
  -trusted-proxy s Proxy IPs/CIDRs whose X-Forwarded-For identifies the real
                   client, comma-separated. Behind a same-host proxy this keeps
                   one visitor's failed logins from rate-limiting everyone.
  -metrics-token s Read-only token for /metrics (sent as a Bearer token or as
                   the Basic password), so Prometheus needn't hold the admin
                   login. Only consulted while Require login is on.
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
                            (on by default, independent of -speedtest; at most one
                            per -speedtest-interval, or per 15m when that interval
                            is shorter)
  -latency=false            Disable latency probing (speedtest-only mode)
  -retain dur               Prune latency samples older than this
                            (default 720h = 30 days; 0 = forever)
  -retain-speed dur         Prune speed history older than this (default 8760h = 1 year)
  -retain-downtime dur      Prune outage history older than this (default 8760h = 1 year)
  -quick-setup s            First-run Quick Setup, for headless installs:
                            'skip' marks it answered, so monitoring starts at
                            boot; 'prompt' (default) leaves it to the browser
                            dialog, and monitoring stays held until that is
                            answered or %s passes

These flags only seed the initial values - almost everything (intervals,
thresholds, retention, alert webhooks, …) is adjustable live in the settings
drawer and persists across restarts.

Service commands (install/start/stop/uninstall) must be run %s.

Examples:
  pingularity                  # run in foreground; UI on http://localhost:9000
  %s     # install as a service and start it; DB -> %s, UI on :9000
`, db, quickSetupHoldGraceText(), elevationHint(), elevate("pingularity install"), filepath.Dir(db))
}
