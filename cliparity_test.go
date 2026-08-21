package main

import (
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pingular/pingularity/internal/config"
)

// The two places main.go speaks for a parser it does not own, and the drift
// each one hid.
//
// internal/config owns the run flags and the rule about stray positionals;
// main.go owns what an operator SEES (`pingularity help`, hand-written so the
// flags come in a readable order with real explanations) and the one command
// that parses its own arguments (`healthz`). Both fell out of step silently:
//
//   - The curated help documented 18 of the 20 flags ParseFlags defines,
//     leaving out -access and -metrics-token. -access is the one that stings:
//     it decides who may open the dashboard at all, so the help's flag list
//     denied the existence of the flag that governs the listen address it does
//     describe. The project already knew this drift class - the -quick-setup
//     guard in firstrun_holdnotice_test.go exists because the same thing
//     happened once before, and says so - but that guard only covers flags the
//     first-run notice names, which is why -access walked straight past it.
//     These read the flag list out of the REAL FlagSet, so the whole set is
//     covered rather than one notice's worth.
//
//   - `pingularity healthz typo -addr 10.0.0.5:9001` probed 127.0.0.1:9000 and
//     exited 0. Go's flag package stops at the first non-flag token, which is
//     exactly why ParseFlags refuses stray positionals; healthz was the only
//     subcommand that never adopted the rule, so one typo turned a liveness
//     probe into a green light for a machine nobody asked about.

// flagRowPattern matches a flag row in either text: `flag`'s PrintDefaults
// writes "  -name <type>" and pushes continuations onto deeper-indented lines,
// and usage() lays its rows out the same way ("  -latency=false" included), so
// one pattern enumerates both sides of the comparison.
var flagRowPattern = regexp.MustCompile(`(?m)^  -([A-Za-z0-9][-A-Za-z0-9]*)`)

// flagRowNames returns the flag names rows in text introduce, in order.
func flagRowNames(text string) []string {
	var names []string
	for _, m := range flagRowPattern.FindAllStringSubmatch(text, -1) {
		names = append(names, m[1])
	}
	return names
}

// definedRunFlags asks the real parser which flags it defines, rather than
// keeping a list here that would drift the same way the usage text did: -h is
// undefined, so flag prints the FlagSet's defaults - one row per flag - and
// returns ErrHelp. ParseFlags builds the set with ContinueOnError and never
// sets an output, so that goes to os.Stderr, which is why this swaps it (the
// same capture failopen_lost_bornmarker_test.go's quietStderr does).
func definedRunFlags(t *testing.T) []string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "flagdefaults")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	defer f.Close()
	orig := os.Stderr
	defer func() { os.Stderr = orig }()
	os.Stderr = f
	_, perr := config.ParseFlags([]string{"-h"})
	os.Stderr = orig

	if !errors.Is(perr, flag.ErrHelp) {
		t.Fatalf("ParseFlags(-h) = %v, want flag.ErrHelp; this test reads the flag list out of the FlagSet's own help output and has just lost its source", perr)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured flag defaults: %v", err)
	}
	names := flagRowNames(string(b))
	if len(names) == 0 {
		t.Fatalf("captured no flag rows from ParseFlags(-h), so this test would prove nothing; flag's PrintDefaults layout changed:\n%s", b)
	}
	return names
}

// `pingularity help` is the flag list an operator reads - it is what -h,
// --help and `help` all print - and it is hand-written, so nothing but this
// makes it agree with internal/config. It did not: -access and -metrics-token
// were defined by the parser and absent from the help, and -access is the flag
// that decides whether the dashboard answers anyone but loopback. Both
// directions matter. A flag the help omits does not exist as far as the person
// writing an unattended install script is concerned; a flag the help invents
// hands them a command line the daemon rejects with "flag provided but not
// defined".
func TestCuratedHelpDocumentsEveryRunFlag(t *testing.T) {
	defined := definedRunFlags(t)
	usageText := captureUsage(t)

	for _, name := range defined {
		if _, ok := usageEntry(usageText, "-"+name); !ok {
			t.Errorf("internal/config defines -%s and the curated help has no entry for it - `pingularity help` is where an operator looks a flag up, and for them it does not exist.\nusage:\n%s", name, usageText)
		}
	}
	for _, name := range flagRowNames(usageText) {
		if !slices.Contains(defined, name) {
			t.Errorf("the curated help documents -%s, which the parser does not define: anyone following the help gets `flag provided but not defined`.\nparser defines: %v", name, defined)
		}
	}
}

// The -access entry, held to what the parser really accepts - the same
// treatment TestUsageDocumentsEveryFlagTheHoldNoticeAdvertises gives
// -quick-setup, because an entry that names a flag without naming its values
// documents nothing anyone can act on. The -listen check is the other half of
// the original defect: -listen's entry described ":9000" as "all interfaces"
// with no hint that binding is not access, so the one surface that never
// mentioned -access at all also implied the dashboard was LAN-open by default.
// It is not - internal/web/auth.go 403s every non-loopback peer while access is
// local, bar /healthz and /readyz - so the row that describes the bind address
// has to point at the flag that decides reachability.
func TestUsageAccessEntryMatchesTheParser(t *testing.T) {
	t.Setenv("PINGULARITY_ACCESS", "") // a set env would seed a different default than an operator's fresh install sees
	usageText := captureUsage(t)

	entry, ok := usageEntry(usageText, "-access")
	if !ok {
		// Not "return // reported above": that idiom is safe in
		// firstrun_holdnotice_test.go only because the t.Errorf it defers to
		// runs in the SAME function, so the test is already red when it
		// returns. Here the absence is reported by a DIFFERENT test, so a
		// return would report PASS for a help text that documents nothing -
		// and would silently skip every assertion below, including the
		// -listen check, which is the only guard in the repo on that row's
		// wording.
		t.Fatalf("the curated help has no -access entry at all, so this test cannot check what it says - and the -access row is exactly what went missing before:\nusage:\n%s", usageText)
	}
	for _, v := range []string{"local", "network"} {
		if _, err := config.ParseFlags([]string{"-access=" + v}); err != nil {
			t.Fatalf("-access=%s is rejected by the parser, so the usage entry must not offer it: %v", v, err)
		}
		if !strings.Contains(entry, "'"+v+"'") {
			t.Errorf("the -access usage entry never says what '%s' does:\n%s", v, entry)
		}
	}
	if _, err := config.ParseFlags([]string{"-access=bogus"}); err == nil {
		t.Error("the parser accepts an -access value the usage entry does not describe; the entry no longer covers what the flag takes")
	}
	// Which value you get by saying nothing is the whole point of the entry: a
	// container that publishes a port and never passes -access is the case that
	// ends up 403ing its own operator.
	cfg, err := config.ParseFlags(nil)
	if err != nil {
		t.Fatalf("ParseFlags(no flags): %v", err)
	}
	if cfg.Access != "local" {
		t.Fatalf("the silent default is now %q; this test and the -access entry both describe 'local' as the default", cfg.Access)
	}
	if !strings.Contains(entry, "default") {
		t.Errorf("the -access usage entry never says which value applies when nobody passes the flag:\n%s", entry)
	}

	listen, ok := usageEntry(usageText, "-listen")
	if !ok {
		t.Fatalf("the curated help has no -listen entry, so the row that has to point at the access mode is gone entirely:\nusage:\n%s", usageText)
	}
	if !strings.Contains(listen, "access") {
		t.Errorf("the -listen entry describes the bind address without naming the access mode that decides who may connect. The row this replaced offered 127.0.0.1:9000 as the way to get local-only, when local-only is already the default and internal/web/auth.go 403s every non-loopback peer (bar /healthz and /readyz) whatever this binds - so an operator reading only this row ends up with the opposite of the real default posture:\n%s", listen)
	}
}

// `healthz` parses its own arguments (the rest of it is pinned in
// container_healthz_test.go), and it was the one subcommand that skipped the
// rule internal/config calls a real security footgun: Go's flag package stops
// at the first non-flag token, so `healthz typo -addr 10.0.0.5:9001` never saw
// -addr, probed healthzDefaultAddr instead, and exited 0 if anything healthy
// happened to answer there. A liveness probe reporting green about a machine
// nobody asked about is the worst way for it to be wrong, and it is silent.
func TestHealthzRejectsStrayPositional(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Leading is the damaging position; trailing is harmless in itself but has
	// to be refused too, because a command that swallows arbitrary junk is how
	// the leading case stayed unnoticed.
	for _, c := range []struct {
		args []string
		cost string
	}{
		{[]string{"typo", "-addr", addr}, "parsing stopped at the positional, so the -addr behind it never applied and this answered for whatever listens on " + healthzDefaultAddr},
		{[]string{"-addr", addr, "extra"}, "a health probe that accepts an argument it does not understand tells the operator their command line was fine when it was not"},
	} {
		err := healthzCmd(c.args)
		if err == nil {
			t.Errorf("healthzCmd(%v) returned nil (exit 0): %s", c.args, c.cost)
			continue
		}
		if !strings.Contains(err.Error(), "unexpected argument") {
			t.Errorf("healthzCmd(%v) = %v; want the same `unexpected argument` refusal every other subcommand gives (internal/config.ParseFlags)", c.args, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a refused healthz still sent %d request(s); the refusal has to come before the probe, or the exit code is still reporting on something", n)
	}
}
