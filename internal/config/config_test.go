package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
)

// defaultDBPath must resolve the same machine-wide path for a service and an
// elevated CLI on each OS, and never a relative name. On Windows that means
// %ProgramData% regardless of euid (which is always -1 there).
func TestDefaultDBPath(t *testing.T) {
	tests := []struct {
		name          string
		goos          string
		euid          int
		programData   string
		userConfigDir string
		want          string
	}{
		{"windows service (euid -1)", "windows", -1, `C:\ProgramData`, `C:\Users\x\AppData\Roaming`, filepath.Join(`C:\ProgramData`, "pingularity", "pingularity.db")},
		{"windows admin cli (euid -1)", "windows", -1, `C:\ProgramData`, "", filepath.Join(`C:\ProgramData`, "pingularity", "pingularity.db")},
		{"windows empty ProgramData falls back", "windows", -1, "", "", filepath.Join(`C:\ProgramData`, "pingularity", "pingularity.db")},
		{"darwin root daemon", "darwin", 0, "", "/Users/x/Library/Application Support", filepath.Join("/Library/Application Support", "pingularity", "pingularity.db")},
		{"darwin non-root", "darwin", 501, "", "/Users/x/Library/Application Support", filepath.Join("/Users/x/Library/Application Support", "pingularity", "pingularity.db")},
		{"linux root service", "linux", 0, "", "/home/x/.config", filepath.Join("/var/lib/pingularity", "pingularity.db")},
		{"linux non-root", "linux", 1000, "", "/home/x/.config", filepath.Join("/home/x/.config", "pingularity", "pingularity.db")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := defaultDBPath(tt.goos, tt.euid, tt.programData, tt.userConfigDir)
			if got != tt.want {
				t.Fatalf("defaultDBPath(%q, %d, %q, %q) = %q, want %q", tt.goos, tt.euid, tt.programData, tt.userConfigDir, got, tt.want)
			}
			// The unix branches must be rooted (never a cwd-relative name).
			// filepath.IsAbs uses the HOST's rules - a unix "/var/lib" path built
			// with filepath.Join reads as non-absolute on a Windows runner - so
			// normalise separators and check the root directly, host-independently.
			if tt.goos != "windows" && !strings.HasPrefix(filepath.ToSlash(got), "/") {
				t.Fatalf("path %q must be absolute", got)
			}
		})
	}
}

// A Windows service and an elevated CLI both go through defaultDBPath with the
// same euid (-1), so they must agree byte-for-byte.
func TestDefaultDBPathWindowsServiceAndCLIAgree(t *testing.T) {
	service := defaultDBPath("windows", -1, `C:\ProgramData`, "")
	cli := defaultDBPath("windows", -1, `C:\ProgramData`, `C:\Users\admin\AppData\Roaming`)
	if service != cli {
		t.Fatalf("service path %q != CLI path %q", service, cli)
	}
}

// A relative -db must be resolved to an absolute path at parse time so it can't
// silently retarget the database via the process working directory.
func TestParseFlagsResolvesDBToAbsolute(t *testing.T) {
	c, err := ParseFlags([]string{"-db", "pingularity.db"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !filepath.IsAbs(c.DBPath) {
		t.Fatalf("DBPath = %q, want absolute", c.DBPath)
	}
	// The default DB path must likewise be absolute.
	d, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("default parse: %v", err)
	}
	if !filepath.IsAbs(d.DBPath) {
		t.Fatalf("default DBPath = %q, want absolute", d.DBPath)
	}
}

// ParseFlags must reject stray positional args. Go's flag package stops at the
// first non-flag token and silently drops everything after it, so a typo'd
// subcommand (e.g. "install sttart -listen 127.0.0.1:9000") would drop the real
// flags and fall back to the bind-all default - a real security footgun.
func TestParseFlagsRejectsStrayPositional(t *testing.T) {
	if _, err := ParseFlags([]string{"sttart", "-listen", "127.0.0.1:9000"}); err == nil {
		t.Fatal("stray leading positional must be rejected, got nil error")
	} else if !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("want 'unexpected argument' error, got %v", err)
	}
	if _, err := ParseFlags([]string{"-listen", "127.0.0.1:9000", "extra"}); err == nil {
		t.Fatal("trailing positional must be rejected")
	}
}

func TestParseFlagsAppliesFlagsAndDefaults(t *testing.T) {
	c, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("default parse: %v", err)
	}
	if c.ListenAddr != ":9000" {
		t.Fatalf("default ListenAddr = %q, want :9000 (bind-all)", c.ListenAddr)
	}
	c, err = ParseFlags([]string{"-listen", "127.0.0.1:8080", "-latency=false", "-up-after", "5"})
	if err != nil {
		t.Fatalf("override parse: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:8080" {
		t.Fatalf("ListenAddr override = %q, want 127.0.0.1:8080", c.ListenAddr)
	}
	if c.LatencyEnabled {
		t.Error("-latency=false should disable latency probing")
	}
	if c.UpAfter != 5 {
		t.Fatalf("UpAfter = %d, want 5", c.UpAfter)
	}
	if _, err := ParseFlags([]string{"-nope"}); err == nil {
		t.Fatal("unknown flag must error")
	}
}

// Scheduled speedtests are opt-in (Automatic off by default), but an on-reconnect
// test is on by default so the link is measured right after it recovers. Flags override.
func TestDefaultSpeedtestSchedule(t *testing.T) {
	d := Default()
	if d.SpeedtestEnabled {
		t.Error("SpeedtestEnabled default = true, want false (scheduled tests opt-in)")
	}
	if !d.SpeedtestOnReconnect {
		t.Error("SpeedtestOnReconnect default = false, want true (on by default)")
	}
	c, err := ParseFlags([]string{"-speedtest", "-speedtest-on-reconnect=false"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.SpeedtestEnabled {
		t.Error("-speedtest should enable scheduled speedtests")
	}
	if c.SpeedtestOnReconnect {
		t.Error("-speedtest-on-reconnect=false should disable the reconnect test")
	}
}

// A -listen value the web server could never bind must fail at parse time, so
// `pingularity install` catches it instead of installing a service that runs
// forever with a dead dashboard.
func TestParseFlagsValidatesListen(t *testing.T) {
	for _, bad := range []string{"9000", "127.0.0.1", ":notaport", ":99999", "localhost:"} {
		if _, err := ParseFlags([]string{"-listen", bad}); err == nil {
			t.Errorf("-listen %q must be rejected", bad)
		}
	}
	for _, good := range []string{":9000", "127.0.0.1:8080", "[::1]:9000", "localhost:9000"} {
		if _, err := ParseFlags([]string{"-listen", good}); err != nil {
			t.Errorf("-listen %q should parse, got %v", good, err)
		}
	}
}

// -allow-host must carry bare Host header values only. An entry with a scheme,
// port, or path can never match the port-stripped Host the rebinding guard
// compares against, so it must fail at parse time instead of silently 403ing
// every proxied request.
func TestParseFlagsValidatesAllowedHosts(t *testing.T) {
	for _, bad := range []string{
		"https://fleet.example.com",
		"http://fleet.example.com",
		"//fleet.example.com",
		"fleet.example.com:8443",
		"fleet.example.com/path",
		"fleet.example.com?x=1",
		"fleet.example.com#frag",
		"ok.example.com,bad.example.com:9000",
	} {
		if _, err := ParseFlags([]string{"-allow-host", bad}); err == nil {
			t.Errorf("-allow-host %q must be rejected", bad)
		}
	}
	for _, good := range []string{
		"",
		"fleet.example.com",
		"fleet.example.com,dash.example.org",
		"  spaced.example.com  ",
	} {
		if _, err := ParseFlags([]string{"-allow-host", good}); err != nil {
			t.Errorf("-allow-host %q should parse, got %v", good, err)
		}
	}
}

func TestParseFlagsIPv4Mode(t *testing.T) {
	c, err := ParseFlags(nil)
	if err != nil {
		t.Fatalf("default parse: %v", err)
	}
	if c.IPv4Mode != "auto" {
		t.Fatalf("default IPv4Mode = %q, want auto", c.IPv4Mode)
	}
	c, err = ParseFlags([]string{"-ipv4", "off"})
	if err != nil {
		t.Fatalf("-ipv4 off: %v", err)
	}
	if c.IPv4Mode != "off" {
		t.Fatalf("IPv4Mode = %q, want off", c.IPv4Mode)
	}
}

// An unrecognized IPv4/IPv6 mode must be REJECTED at parse time, not silently
// coerced to "auto" by the settings layer (which would probe the opposite of
// what the operator asked, with nothing logged). Every accepted value must also
// round-trip unchanged.
func TestParseFlagsRejectsInvalidIPMode(t *testing.T) {
	for _, bad := range []string{"disabled", "yes", "true", "on-reconnect", "AUTO", ""} {
		if _, err := ParseFlags([]string{"-ipv6", bad}); err == nil {
			t.Errorf("-ipv6 %q: expected an error, got nil", bad)
		}
		if _, err := ParseFlags([]string{"-ipv4", bad}); err == nil {
			t.Errorf("-ipv4 %q: expected an error, got nil", bad)
		}
	}
	for _, ok := range []string{"auto", "on", "off"} {
		if _, err := ParseFlags([]string{"-ipv6", ok}); err != nil {
			t.Errorf("-ipv6 %q: unexpected error %v", ok, err)
		}
	}
}

// The parse-time flag bounds must equal the settings layer's clamps: config
// rejects what settings would silently rewrite, so the two must never drift.
func TestFlagBoundsMatchSettings(t *testing.T) {
	pairs := []struct {
		name       string
		cfg, wants any
	}{
		{"MinInterval", MinInterval, settings.MinLatency},
		{"MaxInterval", MaxInterval, settings.MaxLatency},
		{"MinTimeout", MinTimeout, settings.MinTimeout},
		{"MaxTimeout", MaxTimeout, settings.MaxTimeout},
		{"MinSpeedInterval", MinSpeedInterval, settings.MinSpeed},
		{"MaxSpeedInterval", MaxSpeedInterval, settings.MaxSpeed},
		{"MinStreak", MinStreak, settings.MinStreak},
		{"MaxStreak", MaxStreak, settings.MaxStreak},
	}
	for _, p := range pairs {
		if p.cfg != p.wants {
			t.Errorf("%s: config %v != settings %v", p.name, p.cfg, p.wants)
		}
	}
}

// Out-of-range flags must fail at parse time (so `install` catches them), not
// silently run clamped.
func TestParseFlagsValidatesRanges(t *testing.T) {
	bad := [][]string{
		{"-interval", "500ms"},
		{"-interval", "2h"},
		{"-timeout", "45s"},
		{"-down-after", "20"},
		{"-up-after", "0"},
		{"-speedtest-interval", "10s"},
		{"-speedtest-interval", "30s"}, // below the 1-minute floor
		{"-retain", "-1h"},
	}
	for _, args := range bad {
		if _, err := ParseFlags(args); err == nil {
			t.Errorf("ParseFlags(%v): want error, got nil", args)
		}
	}
	// The documented defaults and edge values must still parse.
	ok := [][]string{
		{},
		{"-interval", "1s", "-timeout", "30s", "-down-after", "10", "-up-after", "1"},
		{"-speedtest-interval", "1m"},
		{"-retain", "0"},
	}
	for _, args := range ok {
		if _, err := ParseFlags(args); err != nil {
			t.Errorf("ParseFlags(%v): unexpected error: %v", args, err)
		}
	}
}

// New parse-time rejections: empty -db (would Abs() to the cwd and crash-loop
// the installed service), fractional-second durations (the settings store
// truncates to whole seconds on the first save), and retention past the store's
// 10y cap (would silently shrink).
func TestParseFlagsRejectsSilentRewrites(t *testing.T) {
	bad := [][]string{
		{"-db", ""},
		{"-db="},
		{"-interval", "2500ms"},
		{"-timeout", "1500ms"},
		{"-speedtest-interval", "90500ms"},
		{"-retain", "100000h"},
		{"-retain", "1h30m30s500ms"},
	}
	for _, args := range bad {
		if _, err := ParseFlags(args); err == nil {
			t.Errorf("ParseFlags(%v): want error, got nil", args)
		}
	}
	ok := [][]string{
		{"-interval", "2s"},
		{"-retain", "87600h"},
		{"-retain", "0"},
	}
	for _, args := range ok {
		if _, err := ParseFlags(args); err != nil {
			t.Errorf("ParseFlags(%v): unexpected error: %v", args, err)
		}
	}
}

// The retention cap must equal the settings store's duration clamp.
func TestMaxRetentionMatchesSettings(t *testing.T) {
	if MaxRetention != settings.MaxDuration {
		t.Errorf("config.MaxRetention %v != settings.MaxDuration %v", MaxRetention, settings.MaxDuration)
	}
}

// Explicit monitoring flags are headless consent (MonitoringConsent), so main
// can skip the browser-only Quick Setup hold. Nothing implicit counts.
func TestMonitoringConsentFromFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"no flags", nil, false},
		{"unrelated flag only", []string{"-listen", ":9001"}, false},
		{"-speedtest", []string{"-speedtest"}, true},
		{"-speedtest-interval", []string{"-speedtest-interval", "30m"}, true},
		{"-latency", []string{"-latency=false"}, true},
		{"-interval", []string{"-interval", "10s"}, true},
	}
	for _, c := range cases {
		got, err := ParseFlags(c.args)
		if err != nil {
			t.Fatalf("%s: ParseFlags: %v", c.name, err)
		}
		if got.MonitoringConsent != c.want {
			t.Errorf("%s: MonitoringConsent = %v, want %v", c.name, got.MonitoringConsent, c.want)
		}
	}
}

// -quick-setup is the explicit headless-consent knob for operators who never
// touch a monitoring flag: it must set QuickSetupSkip only for "skip", leave it
// off for the default/"prompt", and reject any other value.
func TestQuickSetupFlag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    bool
		wantErr bool
	}{
		{"absent", nil, false, false},
		{"prompt", []string{"-quick-setup", "prompt"}, false, false},
		{"empty string", []string{"-quick-setup", ""}, false, false},
		{"skip", []string{"-quick-setup", "skip"}, true, false},
		{"skip via equals", []string{"-quick-setup=skip"}, true, false},
		{"non-monitoring flags plus skip", []string{"-timeout", "4s", "-down-after", "4", "-quick-setup=skip"}, true, false},
		{"garbage", []string{"-quick-setup", "yolo"}, false, true},
	}
	for _, c := range cases {
		got, err := ParseFlags(c.args)
		if (err != nil) != c.wantErr {
			t.Fatalf("%s: err=%v, wantErr=%v", c.name, err, c.wantErr)
		}
		if c.wantErr {
			continue
		}
		if got.QuickSetupSkip != c.want {
			t.Errorf("%s: QuickSetupSkip = %v, want %v", c.name, got.QuickSetupSkip, c.want)
		}
	}
}

// AccessExplicit must be true exactly when the operator SAID something: a
// -access flag actually passed on the command line (either value - "-access
// local" is a choice, distinct from silence) or a non-blank PINGULARITY_ACCESS.
// The silent "local" default is NOT explicit: at boot only explicit input may
// override the stored access_local_only, in either direction (we never guess).
func TestAccessExplicit(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"neither flag nor env", nil, "", false},
		{"unrelated flag only", []string{"-listen", ":9001"}, "", false},
		{"blank env is not a choice", nil, "   ", false},
		{"env network", nil, "network", true},
		{"env local", nil, "local", true},
		{"env with whitespace", nil, " network ", true},
		{"flag network", []string{"-access", "network"}, "", true},
		{"flag naming the default is still explicit", []string{"-access", "local"}, "", true},
		{"flag via equals", []string{"-access=network"}, "", true},
		{"flag and env together", []string{"-access", "local"}, "network", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PINGULARITY_ACCESS", c.env)
			got, err := ParseFlags(c.args)
			if err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			if got.AccessExplicit != c.want {
				t.Errorf("AccessExplicit = %v, want %v", got.AccessExplicit, c.want)
			}
		})
	}
}

// -access / PINGULARITY_ACCESS is the explicit access mode. Precedence:
// flag > env > default(local); an invalid value from either source fails loudly.
func TestAccessFlag(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		env     string
		want    string
		wantErr bool
	}{
		{"default is local", nil, "", "local", false},
		{"flag network", []string{"-access", "network"}, "", "network", false},
		{"flag local", []string{"-access", "local"}, "", "local", false},
		{"env network", nil, "network", "network", false},
		{"flag overrides env", []string{"-access", "local"}, "network", "local", false},
		{"invalid flag", []string{"-access", "sideways"}, "", "", true},
		{"invalid env", nil, "yolo", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("PINGULARITY_ACCESS", c.env)
			got, err := ParseFlags(c.args)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if c.wantErr {
				return
			}
			if got.Access != c.want {
				t.Errorf("Access=%q, want %q", got.Access, c.want)
			}
		})
	}
}
