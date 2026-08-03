package main

import (
	"bytes"
	"errors"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/kardianos/service"

	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/logfilter"
	"github.com/pingular/pingularity/internal/stats"
)

// nonLoopbackListen decides whether the "reachable on the network with auth
// OFF" warning fires, so its classification of bind addresses must be exact:
// only a loopback-pinned listen is safe-silent.
func TestNonLoopbackListen(t *testing.T) {
	cases := []struct {
		addr string
		want bool // true = reachable off-box (warn)
	}{
		{":9000", true},
		{"0.0.0.0:9000", true},
		{"[::]:9000", true},
		{"192.168.1.5:9000", true},
		{"127.0.0.1:9000", false},
		{"localhost:9000", false},
		{"[::1]:9000", false},
	}
	for _, c := range cases {
		if got := nonLoopbackListen(c.addr); got != c.want {
			t.Errorf("nonLoopbackListen(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

// absDBArgs must resolve a relative -db to an absolute path (so the service
// can't have its DB retargeted by its working directory) for every flag form,
// and leave an already-absolute path and unrelated flags untouched.
func TestAbsDBArgs(t *testing.T) {
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }

	// An already-absolute path must be OS-appropriate: "/var/..." is absolute on
	// Unix but relative on Windows (no drive letter), so pick a genuinely-absolute
	// path for the current OS to exercise the "left untouched" branch.
	absDB := "/var/lib/pingularity/x.db"
	if runtime.GOOS == "windows" {
		absDB = `C:\pingularity\x.db`
	}

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"space form", []string{"-db", "data.db"}, []string{"-db", abs("data.db")}},
		{"equals form", []string{"-db=data.db"}, []string{"-db=" + abs("data.db")}},
		{"double-dash space", []string{"--db", "data.db"}, []string{"--db", abs("data.db")}},
		{"double-dash equals", []string{"--db=data.db"}, []string{"--db=" + abs("data.db")}},
		{"already absolute untouched", []string{"-db", absDB}, []string{"-db", absDB}},
		{"unrelated flags untouched", []string{"-listen", ":9000", "-interval", "5s"}, []string{"-listen", ":9000", "-interval", "5s"}},
	}
	for _, c := range cases {
		if got := absDBArgs(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%s: absDBArgs(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}

// buildLogger wires the run-path logger to capture each line in both raw and
// masked form into the ring, while writing the full line to stdout. The masking
// is unit-tested in logfilter, but this install site had no coverage, so a
// regression that dropped the capture (or the masking) would pass every test.
func TestBuildLoggerCapturesRawAndMasked(t *testing.T) {
	var stdout bytes.Buffer
	lvl := new(slog.LevelVar)
	lvl.Set(slog.LevelDebug)
	ring := logbuf.New(10)
	log := buildLogger(&stdout, lvl, ring)

	log.Info("login", "ip", "203.0.113.7", "user", "alice")
	es := ring.Entries()
	if len(es) != 1 {
		t.Fatalf("ring captured %d entries, want 1", len(es))
	}
	e := es[0]
	// Raw form keeps full detail; masked form hides the PII values but keeps keys.
	if !strings.Contains(e.Raw, "203.0.113.7") || !strings.Contains(e.Raw, "alice") {
		t.Errorf("raw form dropped detail: %s", e.Raw)
	}
	if strings.Contains(e.Masked, "203.0.113.7") || strings.Contains(e.Masked, "alice") {
		t.Errorf("PII leaked into masked form: %s", e.Masked)
	}
	if !strings.Contains(e.Masked, logfilter.Redacted) {
		t.Errorf("masked form missing the %q marker: %s", logfilter.Redacted, e.Masked)
	}
	// stdout/journald always gets the full line.
	if !strings.Contains(stdout.String(), "203.0.113.7") {
		t.Errorf("stdout should carry full detail: %s", stdout.String())
	}
}

// Pins that uninstall warns exactly when a running process would be orphaned
// by removing the unit, and never cries wolf: an already-stopped unit (the
// common, benign stop "failure" on Windows/launchd) and an unknowable status
// both stay quiet.
func TestStopWarning(t *testing.T) {
	stopErr := errors.New("The service has not been started")
	cases := []struct {
		name      string
		stopErr   error
		status    service.Status
		statusErr error
		warn      bool
	}{
		{"clean stop", nil, service.StatusStopped, nil, false},
		{"still running", stopErr, service.StatusRunning, nil, true},
		{"already stopped", stopErr, service.StatusStopped, nil, false},
		{"status unknown", stopErr, service.StatusRunning, errors.New("scm unavailable"), false},
	}
	for _, c := range cases {
		got := stopWarning(c.stopErr, c.status, c.statusErr)
		if (got != "") != c.warn {
			t.Errorf("%s: stopWarning = %q, want warning=%v", c.name, got, c.warn)
		}
	}
}

// Pins that every blocked-destination series exists at 0 before its first
// event, so Prometheus rate()/increase() can see the first block after a
// restart - the F7 invariant seedKnownCounters exists to hold. A blocked
// destination is an SSRF-relevant misconfiguration; its first occurrence is
// exactly the step that must not be invisible.
func TestSeedsBlockedCounters(t *testing.T) {
	stats.ResetForTest()
	seedKnownCounters()
	got := stats.Lifetime().Counters
	for _, dest := range []string{"discord", "slack", "healthchecks", "generic", "heartbeat"} {
		name := "notify." + dest + ".blocked"
		v, ok := got[name]
		if !ok {
			t.Errorf("%s not seeded; its first increment is invisible to rate()", name)
		} else if v != 0 {
			t.Errorf("%s seeded at %d, want 0", name, v)
		}
	}
}
