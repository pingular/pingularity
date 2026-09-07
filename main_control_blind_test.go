package main

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/kardianos/service"
)

// The rule an operator needs before typing the command - status needs sudo on
// macOS, and unelevated it says unknown and points at healthz - must be said
// by the help text and both places the docs list the service commands, so
// running it unelevated is not what the docs tell people to do.
func TestStatusElevationIsDocumented(t *testing.T) {
	usage := captureUsage(t)
	if !strings.Contains(usage, "(install/start/stop/restart/uninstall)") {
		t.Errorf("usage() omits restart from the commands that need elevation")
	}
	if !strings.Contains(usage, "On macOS that goes for status too") || !strings.Contains(usage, "unknown") {
		t.Errorf("usage() does not say status needs sudo on macOS and reports unknown otherwise:\n%s", usage)
	}
	for _, doc := range []string{"docs/cli.md", "README.md"} {
		b, err := os.ReadFile(doc)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		// Prose wraps where the paragraph happens to end, so match on the words
		// rather than the line breaks - rewrapping a doc is not a regression.
		text := strings.Join(strings.Fields(string(b)), " ")
		if !strings.Contains(text, "launchd shows a system daemon only to root") || !strings.Contains(text, "`pingularity healthz`") {
			t.Errorf("%s does not say status needs sudo on macOS (launchd shows a system daemon only to root) and point at `pingularity healthz`", doc)
		}
	}
}

// The blind side of launchdStatusBlind: an unelevated caller on macOS gets a
// "stopped" reading that only means the plist exists, so restart must not be
// downgraded on it - the downgrade is for a service KNOWN to be stopped. On
// every other reading the existing rule stands.
func TestEffectiveControlActionOnABlindReading(t *testing.T) {
	orig := launchdStatusBlind
	t.Cleanup(func() { launchdStatusBlind = orig })

	launchdStatusBlind = func() bool { return true }
	if got := effectiveControlAction("restart", fakeStatusService{st: service.StatusStopped}); got != "restart" {
		t.Errorf("blind stopped reading: restart became %q", got)
	}
	if got := effectiveControlAction("restart", fakeStatusService{st: service.StatusRunning}); got != "restart" {
		t.Errorf("blind running reading: restart became %q", got)
	}
	if got := effectiveControlAction("start", fakeStatusService{st: service.StatusStopped}); got != "start" {
		t.Errorf("blind reading changed start to %q", got)
	}

	launchdStatusBlind = func() bool { return false }
	if got := effectiveControlAction("restart", fakeStatusService{st: service.StatusStopped}); got != "start" {
		t.Errorf("sighted stopped reading: restart stayed %q, want the start downgrade", got)
	}
}

// The real predicate: blind exactly on macOS when not root. Root's
// `launchctl list` sees the system domain, and systemd and the SCM answer any
// caller, so nowhere else may a stopped reading be doubted. Asked of both
// inputs rather than of this host, so neither half can be dropped unnoticed by
// whoever happens to run the suite.
func TestLaunchdStatusBlindPredicate(t *testing.T) {
	cases := []struct {
		goos string
		euid int
		want bool
	}{
		{"darwin", 501, true}, // the case that started this: a Mac, an ordinary user
		{"darwin", 0, false},  // sudo: launchd answers, so believe it
		{"linux", 501, false},
		{"linux", 0, false},
		{"windows", 501, false},
		{"freebsd", 501, false},
	}
	for _, c := range cases {
		if got := launchdStatusBlindFor(c.goos, c.euid); got != c.want {
			t.Errorf("launchdStatusBlindFor(%q,%d) = %v, want %v", c.goos, c.euid, got, c.want)
		}
	}
	// And the var asks it of this process.
	if got, want := launchdStatusBlind(), launchdStatusBlindFor(runtime.GOOS, os.Geteuid()); got != want {
		t.Errorf("launchdStatusBlind() = %v on %s as uid %d, want %v", got, runtime.GOOS, os.Geteuid(), want)
	}
}
