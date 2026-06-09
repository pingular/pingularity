package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/logfilter"
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

	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"space form", []string{"-db", "data.db"}, []string{"-db", abs("data.db")}},
		{"equals form", []string{"-db=data.db"}, []string{"-db=" + abs("data.db")}},
		{"double-dash space", []string{"--db", "data.db"}, []string{"--db", abs("data.db")}},
		{"double-dash equals", []string{"--db=data.db"}, []string{"--db=" + abs("data.db")}},
		{"already absolute untouched", []string{"-db", "/var/lib/pingularity/x.db"}, []string{"-db", "/var/lib/pingularity/x.db"}},
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
