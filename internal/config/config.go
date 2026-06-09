// Package config holds Pingularity's runtime configuration and flag parsing.
package config

import (
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Target is a single endpoint the prober dials to gauge connectivity.
type Target struct {
	Name    string // human label, e.g. "cloudflare"
	Network string // "tcp4" or "tcp6" (forces the address family)
	Address string // host:port
	Family  string // "ipv4" | "ipv6"
}

// Config is the fully-resolved runtime configuration for a `pingularity run`.
type Config struct {
	DBPath         string        // path to the SQLite database file
	Interval       time.Duration // time between probe rounds
	Timeout        time.Duration // per-target dial timeout
	LatencyEnabled bool          // probe latency/connectivity at all
	DownAfter      int           // consecutive failed rounds before declaring DOWN
	UpAfter        int           // consecutive ok rounds before declaring UP
	Targets        []Target      // endpoints probed each round (quorum)
	ListenAddr     string        // HTTP listen address for UI + /metrics

	SpeedtestEnabled     bool          // run scheduled speedtests (startup, interval, degraded); on-reconnect is governed separately by SpeedtestOnReconnect
	SpeedtestInterval    time.Duration // time between scheduled speedtests
	SpeedtestOnReconnect bool          // run a speedtest when the link reconnects

	IPv4Mode          string        // "auto" | "on" | "off"
	IPv6Mode          string        // "auto" | "on" | "off"
	Retention         time.Duration // prune latency samples older than this (0 = forever)
	SpeedRetention    time.Duration // prune speed history older than this (0 = forever)
	DowntimeRetention time.Duration // prune outage history older than this (0 = forever)

	// AllowedHosts lists extra Host header values (comma-separated) the web layer's
	// DNS-rebinding guard accepts - needed only behind a reverse proxy on a public
	// domain. IP literals, localhost, dotless LAN names, and non-registrable suffixes
	// (.local, .lan, .home, .internal, .home.arpa) are always accepted.
	AllowedHosts string

	// TrustedProxies lists proxy IPs/CIDRs (comma-separated) whose
	// X-Forwarded-For the login rate limiter keys on, so everyone behind a
	// same-host proxy doesn't share one lockout bucket.
	TrustedProxies string
}

// Family names.
const (
	IPv4 = "ipv4"
	IPv6 = "ipv6"
)

// DefaultTargets are well-known, highly-available anycast endpoints in both
// address families. Several per family so a single endpoint flapping can't produce
// a false outage. In the default "auto" modes a family is probed only when the
// host actually has it (see prober.HasGlobalIPv4 / prober.HasGlobalIPv6).
func DefaultTargets() []Target {
	return []Target{
		{Name: "cloudflare", Network: "tcp4", Address: "1.1.1.1:443", Family: IPv4},
		{Name: "google", Network: "tcp4", Address: "8.8.8.8:443", Family: IPv4},
		{Name: "quad9", Network: "tcp4", Address: "9.9.9.9:443", Family: IPv4},
		{Name: "cloudflare-v6", Network: "tcp6", Address: "[2606:4700:4700::1111]:443", Family: IPv6},
		{Name: "google-v6", Network: "tcp6", Address: "[2001:4860:4860::8888]:443", Family: IPv6},
		{Name: "quad9-v6", Network: "tcp6", Address: "[2620:fe::fe]:443", Family: IPv6},
	}
}

// DefaultDBPath returns a sensible database location so -db is rarely needed: a
// machine-wide system path when running as a service (or root), else a per-user
// data directory. The parent directory is created on open.
//
// The key property is that on a given machine the installed service and an
// elevated/interactive CLI (reset-auth, status) resolve the identical default,
// so they operate on the same database. On Windows euid is meaningless (always
// -1), so we always pick the machine-wide %ProgramData% location.
func DefaultDBPath() string {
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		userConfigDir = ""
	}
	return defaultDBPath(runtime.GOOS, os.Geteuid(), os.Getenv("ProgramData"), userConfigDir)
}

// defaultDBPath is the OS decision split out so every branch is unit-testable on
// any host. userConfigDir is os.UserConfigDir()'s result, or "" if it errored.
func defaultDBPath(goos string, euid int, programData, userConfigDir string) string {
	switch goos {
	case "windows":
		// euid is always -1 here, so the service (LocalSystem) and an admin CLI
		// must agree on one machine-wide path regardless of it.
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "pingularity", "pingularity.db")
	case "darwin":
		if euid == 0 { // root daemon (launchd)
			return filepath.Join("/Library/Application Support", "pingularity", "pingularity.db")
		}
	default: // linux and other unix
		if euid == 0 { // root / service
			return filepath.Join("/var/lib/pingularity", "pingularity.db")
		}
	}
	// Non-root interactive user on macOS/Linux.
	if userConfigDir != "" {
		return filepath.Join(userConfigDir, "pingularity", "pingularity.db")
	}
	// Last resort when UserConfigDir has no HOME to work with: an absolute temp
	// path, never a relative name that would follow the process's cwd.
	return filepath.Join(os.TempDir(), "pingularity", "pingularity.db")
}

// Default returns a Config populated with sane zero-config defaults.
func Default() Config {
	return Config{
		DBPath:         DefaultDBPath(),
		Interval:       5 * time.Second,
		LatencyEnabled: true,
		Timeout:        3 * time.Second,
		DownAfter:      2,
		UpAfter:        1,
		Targets:        DefaultTargets(),
		ListenAddr:     ":9000", // all interfaces, IPv4 + IPv6

		SpeedtestEnabled:     false, // scheduled/interval tests are opt-in
		SpeedtestInterval:    time.Hour,
		SpeedtestOnReconnect: true, // on by default: measure the link right after it recovers
		IPv4Mode:             "auto",
		IPv6Mode:             "auto",
		Retention:            30 * 24 * time.Hour,  // latency samples: 30 days
		SpeedRetention:       365 * 24 * time.Hour, // speed history: 1 year
		DowntimeRetention:    365 * 24 * time.Hour, // outages: 1 year
	}
}

// Flag bounds, mirroring the settings layer's clamps (settings.Min*/Max*): the
// UI and settings.normalize enforce the same ranges, so rejecting an
// out-of-range flag here means `pingularity install` fails loudly instead of
// the daemon silently running a clamped value. A test pins these to the
// settings constants so the two layers can't drift.
const (
	MinInterval, MaxInterval           = time.Second, time.Hour
	MinTimeout, MaxTimeout             = time.Second, 30 * time.Second
	MinSpeedInterval, MaxSpeedInterval = time.Minute, 24 * time.Hour
	MinStreak, MaxStreak               = 1, 10
	// MaxRetention mirrors the settings store's duration cap: a bigger flag
	// value would silently shrink to this on the first settings save.
	MaxRetention = 10 * 365 * 24 * time.Hour
)

// ParseFlags overlays command-line flags onto the defaults. args excludes the
// program name and the subcommand.
func ParseFlags(args []string) (Config, error) {
	c := Default()
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.StringVar(&c.DBPath, "db", c.DBPath, "path to the SQLite database file")
	fs.DurationVar(&c.Interval, "interval", c.Interval, "time between probe rounds (1s-1h); a value saved in the UI takes precedence")
	fs.BoolVar(&c.LatencyEnabled, "latency", c.LatencyEnabled, "probe latency/connectivity")
	fs.DurationVar(&c.Timeout, "timeout", c.Timeout, "per-target dial timeout (1s-30s); a value saved in the UI takes precedence")
	fs.IntVar(&c.DownAfter, "down-after", c.DownAfter, "consecutive failures before DOWN (1-10)")
	fs.IntVar(&c.UpAfter, "up-after", c.UpAfter, "consecutive successes before UP (1-10)")
	fs.StringVar(&c.ListenAddr, "listen", c.ListenAddr, "HTTP listen address for UI + metrics")
	fs.StringVar(&c.AllowedHosts, "allow-host", c.AllowedHosts, "extra Host header values to accept, comma-separated (reverse-proxy domains)")
	fs.StringVar(&c.TrustedProxies, "trusted-proxy", c.TrustedProxies, "proxy IPs/CIDRs whose X-Forwarded-For identifies the client, comma-separated")
	fs.BoolVar(&c.SpeedtestEnabled, "speedtest", c.SpeedtestEnabled, "enable scheduled speedtests (startup + every -speedtest-interval); off by default. On-reconnect tests are governed by -speedtest-on-reconnect, the run-while-degraded trigger by its own UI toggle")
	fs.DurationVar(&c.SpeedtestInterval, "speedtest-interval", c.SpeedtestInterval, "time between scheduled speedtests (1m-24h)")
	fs.BoolVar(&c.SpeedtestOnReconnect, "speedtest-on-reconnect", c.SpeedtestOnReconnect, "run a speedtest on reconnect")
	fs.StringVar(&c.IPv4Mode, "ipv4", c.IPv4Mode, "IPv4 probing: auto | on | off")
	fs.StringVar(&c.IPv6Mode, "ipv6", c.IPv6Mode, "IPv6 probing: auto | on | off")
	fs.DurationVar(&c.Retention, "retain", c.Retention, "prune latency samples older than this (0 = forever); also settable in the UI")
	fs.DurationVar(&c.SpeedRetention, "retain-speed", c.SpeedRetention, "prune speed history older than this (0 = forever); also settable in the UI")
	fs.DurationVar(&c.DowntimeRetention, "retain-downtime", c.DowntimeRetention, "prune outage history older than this (0 = forever); also settable in the UI")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	// Reject stray positional args. Go's flag package stops at the first non-flag
	// token and silently ignores the rest, so a typo'd positional (e.g. `run foo
	// -listen ...` or `install sttart -listen ...`) would drop the real flags -
	// reverting to the bind-all default - and slip past controlCmd's install-time
	// validation.
	if rest := fs.Args(); len(rest) > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", rest[0])
	}
	if err := validateListen(c.ListenAddr); err != nil {
		return Config{}, err
	}
	if err := validateRanges(c); err != nil {
		return Config{}, err
	}
	if err := validateTrustedProxies(c.TrustedProxies); err != nil {
		return Config{}, err
	}
	if err := validateAllowedHosts(c.AllowedHosts); err != nil {
		return Config{}, err
	}
	// The IPv4/IPv6 mode is an enum; settings.normalize would silently coerce an
	// unrecognized value (e.g. `-ipv6 disabled`) to "auto", so the daemon would
	// probe the opposite of what was asked with nothing logged. Reject it here,
	// at parse time, like every other flag.
	for _, m := range []struct{ flag, val string }{{"-ipv4", c.IPv4Mode}, {"-ipv6", c.IPv6Mode}} {
		switch m.val {
		case "auto", "on", "off":
		default:
			return Config{}, fmt.Errorf("invalid %s %q: must be auto, on, or off", m.flag, m.val)
		}
	}
	// A blank -db (an unset shell variable in an env file, `-db=`) would Abs()
	// to the working directory below and crash-loop the installed service when
	// store.Open hits a directory. Fail loudly here, where install validates.
	if strings.TrimSpace(c.DBPath) == "" {
		return Config{}, fmt.Errorf("invalid -db: empty path")
	}
	// Resolve -db to an absolute path (relative to the current working dir) now,
	// so a relative value can never silently retarget the DB via the process's
	// cwd - which on Windows the service ignores, landing it in System32.
	abs, err := filepath.Abs(c.DBPath)
	if err != nil {
		return Config{}, fmt.Errorf("invalid -db %q: %w", c.DBPath, err)
	}
	c.DBPath = abs
	return c, nil
}

// validateRanges rejects out-of-range numeric flags. The settings layer would
// otherwise silently clamp them (settings.normalize), so the daemon would run a
// different cadence than the flag asked for with nothing telling the operator -
// the default log level is off. Fail at parse time instead, like -listen.
func validateRanges(c Config) error {
	type bound struct {
		flag     string
		val      time.Duration
		min, max time.Duration
	}
	for _, b := range []bound{
		{"-interval", c.Interval, MinInterval, MaxInterval},
		{"-timeout", c.Timeout, MinTimeout, MaxTimeout},
		{"-speedtest-interval", c.SpeedtestInterval, MinSpeedInterval, MaxSpeedInterval},
	} {
		if b.val < b.min || b.val > b.max {
			return fmt.Errorf("invalid %s %s: must be between %s and %s", b.flag, b.val, b.min, b.max)
		}
		// The settings store keeps whole seconds; a fractional flag would run
		// as given until the first settings save silently truncated it.
		if b.val != b.val.Truncate(time.Second) {
			return fmt.Errorf("invalid %s %s: whole seconds only", b.flag, b.val)
		}
	}
	for _, b := range []struct {
		flag string
		val  int
	}{{"-down-after", c.DownAfter}, {"-up-after", c.UpAfter}} {
		if b.val < MinStreak || b.val > MaxStreak {
			return fmt.Errorf("invalid %s %d: must be between %d and %d", b.flag, b.val, MinStreak, MaxStreak)
		}
	}
	// Retention windows: 0 = keep forever is meaningful, so only reject
	// negatives - plus the storage cap a bigger value would silently shrink to.
	for _, b := range []struct {
		flag string
		val  time.Duration
	}{{"-retain", c.Retention}, {"-retain-speed", c.SpeedRetention}, {"-retain-downtime", c.DowntimeRetention}} {
		if b.val < 0 {
			return fmt.Errorf("invalid %s %s: must be 0 (keep forever) or positive", b.flag, b.val)
		}
		if b.val > MaxRetention {
			return fmt.Errorf("invalid %s %s: at most %s (10 years; 0 = keep forever)", b.flag, b.val, MaxRetention)
		}
		if b.val != b.val.Truncate(time.Second) {
			return fmt.Errorf("invalid %s %s: whole seconds only", b.flag, b.val)
		}
	}
	return nil
}

// validateAllowedHosts rejects a -allow-host entry that carries a scheme, port,
// or path. The web layer's rebinding guard compares against a port-stripped,
// lowercased Host header, so anything but a bare hostname can never match and
// would silently 403 every proxied request. Fail at `pingularity install`
// instead, like -listen and -trusted-proxy.
func validateAllowedHosts(list string) error {
	for _, h := range strings.Split(list, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if strings.Contains(h, "://") || strings.HasPrefix(h, "//") {
			return fmt.Errorf("invalid -allow-host %q: drop the scheme, use a bare Host header value (e.g. fleet.example.com)", h)
		}
		if strings.ContainsAny(h, "/?#") {
			return fmt.Errorf("invalid -allow-host %q: drop the path, use a bare Host header value (e.g. fleet.example.com)", h)
		}
		if _, _, err := net.SplitHostPort(h); err == nil {
			return fmt.Errorf("invalid -allow-host %q: drop the port, use a bare Host header value (e.g. fleet.example.com)", h)
		}
	}
	return nil
}

// validateTrustedProxies rejects a -trusted-proxy entry that is neither a CIDR
// nor an IP, so `pingularity install` fails here instead of the service coming
// up with the limiter silently keyed on the proxy address.
func validateTrustedProxies(list string) error {
	for _, c := range strings.Split(list, ",") {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := netip.ParsePrefix(c); err != nil {
			if _, err := netip.ParseAddr(c); err != nil {
				return fmt.Errorf("invalid -trusted-proxy %q: not a CIDR or IP", c)
			}
		}
	}
	return nil
}

// validateListen rejects a -listen value the web server could never bind (e.g.
// "9000" with the colon missing, or a bad port), so `pingularity install` fails
// here with a clear message instead of installing a service whose dashboard is
// dead.
func validateListen(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid -listen %q (want host:port, e.g. \":9000\"): %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("invalid -listen %q: missing port", addr)
	}
	if _, err := net.LookupPort("tcp", port); err != nil {
		return fmt.Errorf("invalid -listen %q: bad port %q", addr, port)
	}
	return nil
}
