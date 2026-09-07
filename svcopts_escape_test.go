//go:build !windows

package main

import (
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/kardianos/service"
)

// fakeSystemd stands in for kardianos's systemd backend so applyServiceOpts
// takes its systemd branch on any host. Only String matters here; New is never
// reached because the test renders the unit itself.
type fakeSystemd struct{}

func (fakeSystemd) String() string    { return "linux-systemd" }
func (fakeSystemd) Detect() bool      { return true }
func (fakeSystemd) Interactive() bool { return true }
func (fakeSystemd) New(service.Interface, *service.Config) (service.Service, error) {
	return nil, errors.New("fakeSystemd cannot build a service")
}

// kardianosFuncs mirrors the func map kardianos feeds the systemd template
// (service_linux.go): cmd double-quotes an argument and escapes only the
// double quote; cmdEscape rewrites spaces in the executable path.
var kardianosFuncs = template.FuncMap{
	"cmd":       func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` },
	"cmdEscape": func(s string) string { return strings.ReplaceAll(s, " ", `\x20`) },
}

// renderUnit renders the self-install unit for the flags an operator passed to
// `pingularity install`, exactly as controlCmd + svcConfig hand them to
// kardianos, and returns its ExecStart line.
func renderUnit(t *testing.T, flags []string) string {
	t.Helper()
	sc := svcConfig(append([]string{"run"}, flags...))
	tmpl, err := template.New("").Funcs(kardianosFuncs).Parse(systemdScript)
	if err != nil {
		t.Fatalf("systemdScript failed to parse: %v", err)
	}
	data := struct {
		Description      string
		Path             string
		Name             string
		Dependencies     []string
		Arguments        []string
		WorkingDirectory string
		ReloadSignal     string
		Restart          string
	}{
		Description: sc.Description, Path: "/usr/local/bin/pingularity", Name: sc.Name,
		Dependencies: sc.Dependencies, Arguments: sc.Arguments,
		ReloadSignal: "HUP", Restart: "always",
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatalf("systemdScript failed to render: %v", err)
	}
	for _, line := range strings.Split(b.String(), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			return line
		}
	}
	t.Fatalf("no ExecStart line in rendered unit:\n%s", b.String())
	return ""
}

// systemd reads ExecStart with its own rules, double quotes or not: %x is a
// specifier (%t the runtime directory, %h the home), $VAR and ${VAR} are
// environment substitutions, and a backslash starts a C escape. kardianos's
// cmd escapes only the double quote, so a -metrics-token with a % in it reached
// the daemon as a different token and a trailing backslash left a unit systemd
// refuses to load. Each install argument must reach the unit escaped per
// systemd.unit(5)/systemd.service(5): %% for %, $$ for $, a doubled backslash -
// while the template's own $PINGULARITY_OPTS stays expandable.
func TestSystemdUnitKeepsFlagValuesVerbatim(t *testing.T) {
	orig := service.AvailableSystems()
	service.ChooseSystem(fakeSystemd{})
	t.Cleanup(func() { service.ChooseSystem(orig...) })
	if service.Platform() != "linux-systemd" {
		t.Fatalf("platform stand-in not in effect: %q", service.Platform())
	}

	line := renderUnit(t, []string{"-metrics-token", "tok%tX", "-db", `/var/lib/ping${HOME}y`, "-allow-host", `a\`, "-listen", ":9000"})

	for _, want := range []string{
		`"tok%%tX"`,                // %t is systemd's runtime directory; %% is a literal percent
		`"/var/lib/ping$${HOME}y"`, // ${HOME} substitutes even mid-word; $$ is a literal dollar
		`"a\\"`,                    // a lone trailing backslash escapes the closing quote
		`":9000"`,                  // an ordinary value is untouched
		" $PINGULARITY_OPTS",       // the template's own variable must still expand
	} {
		if !strings.Contains(line, want) {
			t.Errorf("ExecStart lacks %s:\n%s", want, line)
		}
	}
	for _, bad := range []string{`"tok%tX"`, `ping${HOME}y"`, `"a\"`} {
		if strings.Contains(line, bad) {
			t.Errorf("ExecStart still carries the raw value %s, which systemd rewrites:\n%s", bad, line)
		}
	}
}

// launchd's plist and the Windows SCM expand nothing, so on any other platform
// the arguments must reach the service definition exactly as typed.
func TestNonSystemdPlatformsKeepArgumentsAsTyped(t *testing.T) {
	if service.Platform() == "linux-systemd" {
		t.Skip("this host is systemd; the escaping is right here")
	}
	sc := svcConfig([]string{"run", "-metrics-token", "tok%tX", "-db", `C:\data\p.db`})
	if got, want := strings.Join(sc.Arguments, " "), `run -metrics-token tok%tX -db C:\data\p.db`; got != want {
		t.Errorf("arguments changed on %s: got %q, want %q", service.Platform(), got, want)
	}
}

// systemdArgs applies systemd's three escapes and nothing else: %% for a
// percent, $$ for a dollar, a doubled backslash - in that order, so the
// backslash doubling never touches what the other two produce - and leaves the
// double quote to kardianos's cmd.
func TestSystemdArgs(t *testing.T) {
	cases := []struct{ in, want string }{
		{"run", "run"},
		{":9000", ":9000"},
		{"tok%tX", "tok%%tX"},
		{"100%", "100%%"},
		{"$HOME", "$$HOME"},
		{"a${X}b", "a$${X}b"},
		{"$$", "$$$$"},
		{`a\`, `a\\`},
		{`C:\new\tab`, `C:\\new\\tab`}, // \n and \t would otherwise become a newline and a tab
		{`\%`, `\\%%`},                 // backslash first: the %% it precedes is not re-escaped
		{`say "hi"`, `say "hi"`},       // the quote is cmd's job
		{"", ""},
	}
	for _, c := range cases {
		if got := systemdArgs([]string{c.in})[0]; got != c.want {
			t.Errorf("systemdArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := systemdArgs(nil); got != nil {
		t.Errorf("systemdArgs(nil) = %v, want nil (start/stop/status carry no arguments)", got)
	}
	in := []string{"run", "-db", "%h"}
	out := systemdArgs(in)
	if strings.Join(in, " ") != "run -db %h" {
		t.Errorf("systemdArgs rewrote its input in place: %q", in)
	}
	if strings.Join(out, " ") != "run -db %%h" {
		t.Errorf("systemdArgs(%q) = %q", in, out)
	}
}
