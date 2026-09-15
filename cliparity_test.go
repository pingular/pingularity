package main

import (
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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

// -speedtest seeds the setting that gates the whole scheduled group, and main
// wires the while-degraded trigger behind it: DegradedPingFn returns 0 -
// detection off, not merely no dispatch - unless scheduled speedtests are on.
// docs/cli.md's -speedtest row and the flag's own usage string both filed the
// degraded trigger beside on-reconnect as governed by "its own UI toggle", and
// on-reconnect really is independent, so the sentence promised an independence
// only one of the two triggers has. A headless install that turned the toggle
// on over the API with -speedtest off got no degraded tests and no complaint.
// The README's paragraph carries the qualifier; this holds the other two
// surfaces to the same words, and the premise to main.go.
func TestSpeedtestFlagSaysTheDegradedTriggerNeedsIt(t *testing.T) {
	src := mustReadRepoFile(t, "main.go")
	gate := regexp.MustCompile(`m\.DegradedPingFn = func\(\) float64 \{\s*\n\s*if ([^\n]+)`).FindStringSubmatch(src)
	if gate == nil || !strings.Contains(gate[1], "!set.SpeedtestEnabled()") {
		t.Fatalf("main.go no longer gates DegradedPingFn on SpeedtestEnabled; the coupling this test documents has changed: %q", gate)
	}
	const rule = "needs scheduled tests on"

	entry, ok := flagDefaultsEntry(runFlagDefaultsText(t), "-speedtest")
	if !ok {
		t.Fatal("ParseFlags(-h) prints no -speedtest row; the flag was renamed or PrintDefaults changed shape")
	}
	var row string
	for _, l := range strings.Split(mustReadRepoFile(t, filepath.Join("docs", "cli.md")), "\n") {
		if strings.HasPrefix(l, "| `-speedtest` |") {
			row = l
			break
		}
	}
	if row == "" {
		t.Fatal("docs/cli.md has no `-speedtest` row in its flag table")
	}
	para := paragraphsMentioning(mustReadRepoFile(t, "README.md"), "**while degraded**")
	if para == "" {
		t.Fatal("README.md no longer has a paragraph about the **while degraded** toggle")
	}

	for _, s := range []struct{ name, text string }{
		{"the -speedtest usage string (`pingularity run -h`)", entry},
		{"docs/cli.md's -speedtest row", row},
		{"the README's while-degraded paragraph", para},
	} {
		if !strings.Contains(s.text, rule) {
			t.Errorf("%s never says the while-degraded trigger %s, but main.go's gate (%s) turns detection off without them:\n%s",
				s.name, rule, strings.TrimSpace(gate[1]), s.text)
		}
	}
}

// paragraphsMentioning returns every blank-line-delimited block of doc that
// mentions marker - a table, a paragraph, a list item - joined, so an assertion
// about one topic reads everything the document says about it and nothing else.
func paragraphsMentioning(doc, marker string) string {
	var out []string
	for _, para := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n\n") {
		if strings.Contains(para, marker) {
			out = append(out, para)
		}
	}
	return strings.Join(out, "\n\n")
}

// runFlagDefaultsText is what `pingularity run -h` prints: the FlagSet's own
// PrintDefaults, one row per flag with its usage string beneath - the second
// place an operator reads a flag's description, after the curated help. Same
// capture as definedRunFlags, kept whole here because the text is the point.
func runFlagDefaultsText(t *testing.T) string {
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
		t.Fatalf("ParseFlags(-h) = %v, want flag.ErrHelp", perr)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured flag defaults: %v", err)
	}
	return string(b)
}

// flagDefaultsEntry returns one flag's row and usage from PrintDefaults text. A
// bool flag's row is the bare name ("  -speedtest"), which is why usageEntry,
// written for the curated help's "  -flag type" rows, cannot read it.
func flagDefaultsEntry(text, flag string) (string, bool) {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l != "  "+flag && !strings.HasPrefix(l, "  "+flag+" ") {
			continue
		}
		entry := []string{l}
		for _, c := range lines[i+1:] {
			if !strings.HasPrefix(c, "    ") {
				break
			}
			entry = append(entry, c)
		}
		return strings.Join(entry, "\n"), true
	}
	return "", false
}

// The help's last example pairs the one root-only command, `sudo pingularity
// install`, with a concrete directory - and rendered that directory from the
// euid of whoever was reading the help. Help is read unelevated, so the line
// named the reader's per-user data dir for a service that runs as root and
// resolves the machine-wide path (internal/config's rule, pinned there for
// darwin and linux). An operator who tried the daemon in the foreground first
// and then installed was told the two share a directory: the empty dashboard
// that followed read as lost data, and a backup aimed where the help pointed
// missed the service's database. uninstall already refuses to guess a path for
// this reason; the install example CAN know, because no -db is passed in it.
func TestHelpInstallExampleNamesTheServiceDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("run unelevated: the defect is what an ordinary reader sees, and as root the two paths coincide")
	}
	var serviceDir string
	switch runtime.GOOS {
	case "darwin":
		serviceDir = filepath.Join("/Library/Application Support", "pingularity")
	case "windows":
		t.Skip("windows resolves one machine-wide path regardless of euid")
	default:
		serviceDir = "/var/lib/pingularity"
	}
	usageText := captureUsage(t)
	m := regexp.MustCompile(`(?m)^\s*sudo pingularity install\s+#.*DB -> (.+?), UI on :9000\s*$`).FindStringSubmatch(usageText)
	if m == nil {
		t.Fatalf("the curated help has no `sudo pingularity install ... DB -> <dir>` example; the layout this test reads changed:\n%s", usageText)
	}
	if m[1] != serviceDir {
		t.Errorf("`pingularity help` says `sudo pingularity install` puts the database in %q; the service it registers runs as root and uses %q (the reader's own default, which the -db row already names, is %q)",
			m[1], serviceDir, filepath.Dir(config.DefaultDBPath()))
	}
}

// -on-corrupt decides what a start does with a database it cannot open: keep it
// where it is, or set it aside and begin again empty. Which one an operator
// gets by saying nothing is the whole content of that flag for them, and it is
// written out in three places - the parser's own default, the usage string it
// prints, and the row in docs/cli.md people read before they ever run it. The
// flag table is documentation and nothing else compiles it, so a default edited
// there and nowhere else promises someone their year of history survives a
// power cut when the daemon has been told to replace it. This holds all three
// to config.Default(), in the shape TestSpeedtestFlagSaysTheDegradedTriggerNeedsIt
// holds the -speedtest row.
func TestOnCorruptSurfacesAgreeOnTheDefault(t *testing.T) {
	def := config.Default().OnCorrupt
	if def != config.OnCorruptRefuse && def != config.OnCorruptRebuild {
		t.Fatalf("config.Default().OnCorrupt is %q, which is neither mode the flag accepts", def)
	}
	// What a start with no -on-corrupt really does, so the documented default is
	// held to behaviour rather than to another sentence.
	cfg, err := config.ParseFlags(nil)
	if err != nil {
		t.Fatalf("ParseFlags(no flags): %v", err)
	}
	if cfg.OnCorrupt != def {
		t.Fatalf("a start with no -on-corrupt runs as %q, not the documented default %q", cfg.OnCorrupt, def)
	}

	var row string
	for _, l := range strings.Split(mustReadRepoFile(t, filepath.Join("docs", "cli.md")), "\n") {
		if strings.HasPrefix(l, "| `-on-corrupt` |") {
			row = l
			break
		}
	}
	if row == "" {
		t.Fatal("docs/cli.md has no `-on-corrupt` row in its flag table; the flag table is where an operator decides whether their database is safe across a restart")
	}
	if cells := strings.Split(row, "|"); len(cells) < 4 {
		t.Fatalf("docs/cli.md's -on-corrupt row is not a three-column flag row: %s", row)
	} else if got := strings.TrimSpace(cells[2]); got != "`"+def+"`" {
		t.Errorf("docs/cli.md's -on-corrupt row gives the default as %s; a start with no flag does %q. One of the two is lying to an operator about whether a damaged database is kept:\n%s", got, def, row)
	}
	for _, mode := range []string{config.OnCorruptRefuse, config.OnCorruptRebuild} {
		if !strings.Contains(row, "`"+mode+"`") {
			t.Errorf("docs/cli.md's -on-corrupt row never mentions `%s`, so the road it does not describe is one nobody can choose:\n%s", mode, row)
		}
	}

	// The README's corruption notes open with a headline, and for someone
	// skimming it is the whole answer to which way a damaged database falls when
	// they pass nothing.
	var headline string
	for _, l := range strings.Split(mustReadRepoFile(t, "README.md"), "\n") {
		if strings.HasPrefix(l, "- **A database that won't open is ") {
			headline = l
			break
		}
	}
	wantHeadline := map[string]string{config.OnCorruptRefuse: "left where it is", config.OnCorruptRebuild: "set aside"}[def]
	if headline == "" {
		t.Error("README.md's corruption notes no longer open with \"- **A database that won't open is ...\"; that headline is where a skimming operator learns whether their database survives a start")
	} else if !strings.Contains(headline, wantHeadline) {
		t.Errorf("README.md's corruption headline does not say the database is %s, which is what a start with no -on-corrupt (%q) does to it:\n%s", wantHeadline, def, headline)
	}

	// The two surfaces the daemon itself prints: the flag's usage string
	// (`pingularity run -h`) and the curated help (`pingularity help`).
	marker := "'" + def + "' (default)"
	entry, ok := flagDefaultsEntry(runFlagDefaultsText(t), "-on-corrupt")
	if !ok {
		t.Fatal("ParseFlags(-h) prints no -on-corrupt row; the flag was renamed or PrintDefaults changed shape")
	}
	help, ok := usageEntry(captureUsage(t), "-on-corrupt")
	if !ok {
		t.Fatal("the curated help has no -on-corrupt entry")
	}
	for _, s := range []struct{ name, text string }{
		{"the -on-corrupt usage string (`pingularity run -h`)", entry},
		{"the curated help's -on-corrupt entry (`pingularity help`)", help},
	} {
		if !strings.Contains(s.text, marker) {
			t.Errorf("%s never says %s, so nobody reading it knows which way their database falls when they pass nothing:\n%s", s.name, marker, s.text)
		}
	}

	// What the refusal leaves: the file where it is, nothing moved or replaced.
	// Not "untouched" - the migration is one transaction, so damage it meets
	// leaves an older release's file as it was, but damage only the repairs after
	// it read is met once the migration has committed: that file keeps the new
	// columns and indexes, grows, and its bytes change, and an operator promised
	// otherwise compares checksums and concludes the daemon wrote over the one copy
	// of their data.
	for _, s := range []struct{ name, text string }{
		{"the -on-corrupt usage string (`pingularity run -h`)", entry},
		{"the curated help's -on-corrupt entry (`pingularity help`)", help},
		{"docs/cli.md's -on-corrupt row", row},
	} {
		flat := strings.Join(strings.Fields(s.text), " ")
		if !strings.Contains(flat, "where it is") || strings.Contains(flat, "untouched") {
			t.Errorf("%s does not say the refused file stays where it is, or promises it untouched when the migration may have written to it:\n%s", s.name, s.text)
		}
	}
}
