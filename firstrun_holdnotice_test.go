package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// A fresh install measures NOTHING until Quick Setup is answered or the 48h
// grace runs out, and the boot output used to say nothing about it: the data
// directory notice and the startup line are what a healthy install prints too,
// the health endpoint answers 200, and a headless or package install therefore
// looked fine while collecting zero samples for up to two days. Boot the REAL
// binary on an empty data directory - the only place run()'s wiring is
// observable - and require the boot output to state the hold, what ends it, and
// how long it lasts. The skip case is here too: a notice that fires for an
// install which is NOT held teaches operators to ignore it.
func TestFreshInstallStartupOutputSaysMonitoringIsHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-boot first-run notice test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping build-and-boot first-run notice test")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "pingularity")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// No monitoring flag and no -quick-setup: silence, so nothing consents on the
	// operator's behalf and the first-run hold is in force.
	//
	// The needle is the WHOLE notice, rendered by the same function run() prints
	// (firstRunHoldLine) against the address this boot was actually given.
	// Facet substrings were not enough: this test used to accept the dashboard
	// URL as its "how do I end the hold" evidence, and the PRE-EXISTING startup
	// line prints that URL too - so that sub-assertion passed with the entire
	// notice deleted (verified by deleting it: the other three facets failed and
	// it did not). What the line has to SAY is pinned by
	// TestFirstRunHoldLineIsOneActionableLine; what this pins is that the real
	// binary prints it, on a fresh install, in the output an operator sees.
	held, heldAddr := bootFreshInstall(t, bin)
	notice := firstRunHoldLine(heldAddr)
	if !strings.Contains(held, notice) {
		t.Errorf("fresh-install boot output never carries the first-run hold notice - the install looks healthy while collecting nothing.\nwant this line:\n%s\nfull output:\n%s", notice, held)
	}

	// -quick-setup=skip answers the offer at boot, so monitoring runs from the
	// first round and there is no hold to announce. The needle is that SAME
	// rendered line, not a lowercase phrase quoted out of it: "on hold" was one
	// reword of the notice away from matching nothing at all, and a guard that
	// cannot fail would let the notice fire on an install that is NOT held,
	// which teaches operators to ignore it (verified: reword the notice and
	// print it unconditionally, and the old guard stayed green). Sharing one
	// needle with the assertion above is what keeps that impossible - reword the
	// notice and this guard follows it, or the positive assertion fails.
	//
	// This also pins the flag SPELLING the notice tells operators to use: an
	// unknown flag is rejected at startup, the daemon never serves, and
	// bootFreshInstall fails outright.
	skipped, skippedAddr := bootFreshInstall(t, bin, "-quick-setup=skip")
	if unheld := firstRunHoldLine(skippedAddr); strings.Contains(skipped, unheld) {
		t.Errorf("-quick-setup=skip boots with monitoring RUNNING, but the boot output still announces the hold:\n%s\nfull output:\n%s", unheld, skipped)
	}
}

// bootFreshInstall builds nothing: it boots the already-built binary against an
// empty data directory on a free high port, waits until the API answers (which
// means boot is past every startup line, so whatever it was going to say it has
// said), then kills it and returns everything it printed on both streams.
//
// It returns the -listen address it used, not the port, so a caller can render
// what the daemon rendered (firstRunHoldLine takes that address). Handing back
// the port instead left each caller to rebuild the address, and an address
// rebuilt slightly differently produces a needle that matches nothing - which a
// "must not contain" assertion would report as a pass.
func bootFreshInstall(t *testing.T, bin string, extraArgs ...string) (output, listenAddr string) {
	t.Helper()
	dir := t.TempDir()
	port, releasePort := reserveHighPort(t)
	addr := "127.0.0.1:" + port
	args := append([]string{"run", "-listen", addr, "-db", filepath.Join(dir, "fresh.db")}, extraArgs...)
	cmd := exec.Command(bin, args...)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	releasePort()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	var outBuf, errBuf []byte
	drained := make(chan struct{}, 2)
	go func() { outBuf, _ = io.ReadAll(stdout); drained <- struct{}{} }()
	go func() { errBuf, _ = io.ReadAll(stderr); drained <- struct{}{} }()
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + addr + "/api/status")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		if cmd.ProcessState != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	killed = true
	_ = cmd.Wait()
	<-drained
	<-drained
	if !ready {
		t.Fatalf("daemon never served /api/status; stdout:\n%s\nstderr:\n%s", outBuf, errBuf)
	}
	return string(outBuf) + string(errBuf), addr
}

// The notice must be ONE line, in the startup-line voice, and it must name both
// ways out plus the URL of the dashboard the operator is being sent to - the
// point of the line is that nobody should have to go read the docs to find out
// why the install is measuring nothing.
func TestFirstRunHoldLineIsOneActionableLine(t *testing.T) {
	line := firstRunHoldLine(":9000")
	if strings.Contains(line, "\n") {
		t.Errorf("first-run hold notice is not a single line: %q", line)
	}
	if !strings.HasPrefix(line, "pingularity") {
		t.Errorf("first-run hold notice breaks the startup-line style (no \"pingularity\" prefix): %q", line)
	}
	for _, want := range []string{
		"Quick Setup",           // what has to be answered
		"http://localhost:9000", // where to answer it (dashboardURL of the listen address)
		"-quick-setup=skip",     // the headless way out, spelled as the flag really is
		"48h",                   // how long the hold lasts
	} {
		if !strings.Contains(line, want) {
			t.Errorf("first-run hold notice missing %q: %q", want, line)
		}
	}
	// A non-default listen address must be reflected, or the operator is sent to
	// a dashboard that isn't there.
	if other := firstRunHoldLine("127.0.0.1:19123"); !strings.Contains(other, "http://localhost:19123") {
		t.Errorf("first-run hold notice ignores the listen address: %q", other)
	}
}

// The notice promises a duration the daemon does not enforce - the real grace
// lives in settings.QuickSetupHold, whose constant is unexported. Pin the two
// together: if the grace ever moves, this fails instead of the daemon quietly
// telling every fresh install the wrong deadline.
func TestFirstRunHoldNoticeStatesTheRealGrace(t *testing.T) {
	const since = int64(1_700_000_000)
	grace := int64(quickSetupHoldGrace / time.Second)
	// One second short of the stated grace the offer is still open...
	if !settings.QuickSetupHold(false, since, since+grace-1) {
		t.Errorf("the notice says the hold lasts %s, but settings.QuickSetupHold had already released by then", quickSetupHoldGrace)
	}
	// ...and at it, it is over.
	if settings.QuickSetupHold(false, since, since+grace) {
		t.Errorf("the notice says the hold lasts %s, but settings.QuickSetupHold still holds past that - fresh installs are told the wrong deadline", quickSetupHoldGrace)
	}
}

// quickSetupHoldState is what both the notice and the measurement loops read,
// so its answers ARE the feature: a fresh install must read as held, and
// answering Quick Setup must release it (otherwise the install that was told
// "answer it and monitoring starts" still measures nothing).
func TestQuickSetupHoldStateHoldsFreshInstallUntilAnswered(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, settings.Values{Monitoring: true})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	// The boot path seeds the offer clock on a genuinely fresh install; that is
	// the state the notice speaks for.
	if err := set.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatalf("EnsureQuickSetupOffer: %v", err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsHeld {
		t.Fatalf("fresh install with the offer open: hold state = %v, want qsHeld (%v) - boot would stay silent while measuring nothing", got, qsHeld)
	}
	if err := set.SetQuickSetupDone(ctx, true); err != nil {
		t.Fatalf("SetQuickSetupDone: %v", err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsReleased {
		t.Fatalf("after Quick Setup was answered: hold state = %v, want qsReleased (%v)", got, qsReleased)
	}
}

// quickSetupHoldState's FIRST branch is the one nothing could see. Settings
// have not loaded, so the offer clock is not seeded, and settings.QuickSetupHold
// would read a bare offer_since==0 and fail OPEN. The branch fails CLOSED
// instead - it holds a GENUINELY FRESH install (nothing in the store), keeps an
// ESTABLISHED one running, and latches neither answer, so the real hold takes
// over the moment settings load. Replacing the whole branch with a bare
// `return qsProvisional` left this package green (verified), which means a
// fresh install could start probing before anyone consented and no test would
// have said a word.
func TestQuickSetupHoldStateHoldsFreshInstallWhileSettingsUnloaded(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// A failed initial load is the only route to a controller that is not
	// Loaded(), and hiding the settings table is the smallest REAL fault that
	// causes one: store.AllSettings errors, New surfaces it, and the controller
	// runs on defaults. The table comes straight back, so every later read here
	// - EstablishedInStore's included - is answered by a working store, which is
	// exactly the shape this branch exists for: settings unloaded now, loadable
	// a moment later (a locked or slow DB at boot, retried by the settings-retry
	// loop). New never reached ensureBornMarker either, so the store is left as
	// bare as a first boot's.
	renameSettingsTable(t, st, "settings", "settings_hidden")
	set, err := settings.New(ctx, st, settings.Values{Monitoring: true})
	if err == nil {
		t.Fatal("settings.New succeeded with the settings table hidden; the unloaded controller this whole test is about was never built")
	}
	renameSettingsTable(t, st, "settings_hidden", "settings")
	if set.Loaded() {
		t.Fatal("controller reports loaded settings after a failed load; the branch under test is unreachable")
	}

	// Precondition: this is the genuinely-FRESH case, not the store-read-error
	// one. Both hold, and a test that cannot tell them apart proves only half
	// the branch.
	if est, err := set.EstablishedInStore(ctx); err != nil || est {
		t.Fatalf("EstablishedInStore = %v, %v; want false, <nil> - this fixture must be a fresh install answered by a WORKING store, or the assertions below cannot say which leg held", est, err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsHeld {
		t.Errorf("fresh install, settings unloaded: hold state = %v, want qsHeld (%v) - nothing has consented, and the offer clock that would judge the hold does not exist yet", got, qsHeld)
	}
	// The consequence, through the predicate the measurement loops actually
	// obey: the defaults say Monitoring is on, so the hold is the ONLY thing
	// that can stop this install probing before its settings are even known.
	if newMonitoringLiveFn(ctx, set, nil)() {
		t.Error("a fresh install whose settings failed to load is already measuring: nobody was asked, and the first-run consent hold it is owed has not been materialized")
	}

	// The other half, which is what keeps "fail closed" from meaning "hold
	// everything": an established install consented long ago, and pausing it
	// over a transient settings-load failure would be an outage of the
	// monitoring itself. Its pass is PROVISIONAL - see the qsProvisional doc:
	// a caller may not latch it, or one unloaded read would release the hold
	// permanently on an install that never answered.
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: 1_700_000_000, DownMbps: 94.2, UpMbps: 11.7, PingMS: 12.5, Server: "Somewhere",
	}); err != nil {
		t.Fatalf("insert measurement: %v", err)
	}
	if est, err := st.HasHistory(ctx); err != nil || !est {
		t.Fatalf("HasHistory = %v, %v after a recorded measurement; the established half of this test needs a store that reads as established", est, err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsProvisional {
		t.Errorf("established install, settings unloaded: hold state = %v, want qsProvisional (%v) - qsHeld would pause a consenting install's monitoring over a load failure, and qsReleased is latchable, which would end the hold for good off one unloaded read", got, qsProvisional)
	}
	if !newMonitoringLiveFn(ctx, set, nil)() {
		t.Error("an install with measurement history stopped measuring because its settings failed to load; it consented long ago and the first-run hold does not apply to it")
	}
}

// renameSettingsTable renames a table on a REAL store, which is how this file
// makes store.AllSettings fail on demand (and only then). The :memory: pool is
// one connection, so the rename is visible to every later call on st; the
// schema carries no view, trigger or foreign key naming the table, so nothing
// else is rewritten by the rename.
func renameSettingsTable(t *testing.T, st *store.Store, from, to string) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "ALTER TABLE "+from+" RENAME TO "+to); err != nil {
		t.Fatalf("rename %s -> %s: %v", from, to, err)
	}
}

// The hold notice tells operators to "restart with -quick-setup=skip", and
// `pingularity -h` is where they go to look that up. The usage text listed
// every other run flag and not that one (verified: it did not contain the
// string "quick-setup" anywhere), so the boot line advertised a flag the
// program's own documentation denied. Take the flags back out of the NOTICE and
// require an entry for each, so the two cannot drift apart again: rename or
// re-spell the flag in either place and this fails, instead of an operator
// finding out at 3am that the way out of the hold is undocumented.
func TestUsageDocumentsEveryFlagTheHoldNoticeAdvertises(t *testing.T) {
	notice := firstRunHoldLine(":9000")
	var flags []string
	for _, tok := range strings.Fields(notice) {
		// Trim sentence punctuation, then drop the value: the notice spells the
		// flag as it is USED ("-quick-setup=skip"), usage documents the flag.
		name, _, _ := strings.Cut(strings.TrimRight(tok, ".,;:)"), "=")
		if len(name) > 1 && strings.HasPrefix(name, "-") {
			flags = append(flags, name)
		}
	}
	if len(flags) == 0 {
		t.Fatalf("the notice names no flag at all, so this test would prove nothing: the headless way out of the hold has gone missing from %q", notice)
	}

	usageText := captureUsage(t)
	for _, f := range flags {
		if _, ok := usageEntry(usageText, f); !ok {
			t.Errorf("the first-run notice tells operators to use %s, but the usage text has no entry for it - the one place they look it up says the flag does not exist.\nnotice:\n%s\nusage:\n%s", f, notice, usageText)
		}
	}

	// An entry that names the flag and not its values documents nothing an
	// operator can act on. Each value asserted here is proved against the real
	// parser first, so this can never end up pinning a value the daemon
	// rejects - and the closing check proves the two are the WHOLE set, so a
	// third value could not be added without documenting it.
	entry, ok := usageEntry(usageText, "-quick-setup")
	if !ok {
		return // already reported above
	}
	for _, v := range []string{"skip", "prompt"} {
		if _, err := config.ParseFlags([]string{"-quick-setup=" + v}); err != nil {
			t.Fatalf("-quick-setup=%s is rejected by the parser, so the usage entry must not offer it: %v", v, err)
		}
		if !strings.Contains(entry, "'"+v+"'") {
			t.Errorf("the -quick-setup usage entry never says what '%s' does:\n%s", v, entry)
		}
	}
	if _, err := config.ParseFlags([]string{"-quick-setup=bogus"}); err == nil {
		t.Error("the parser accepts a -quick-setup value the usage entry does not describe; the entry no longer covers what the flag takes")
	}
	// The window is the operator's whole reason to care which value to pass, and
	// it is rendered from quickSetupHoldGrace in both places for that reason
	// (see quickSetupHoldGraceText): a hardcoded "48h" here would outlive the
	// constant.
	if !strings.Contains(entry, quickSetupHoldGraceText()) {
		t.Errorf("the -quick-setup usage entry never states the %s the hold really lasts, so nothing tells an operator how long 'prompt' costs them:\n%s", quickSetupHoldGraceText(), entry)
	}
}

// captureUsage runs usage() with os.Stdout pointed at a temp file and returns
// what it printed. usage() writes through fmt.Printf, which resolves os.Stdout
// at call time, so the swap is enough - and it is the only way to read the text
// `pingularity -h` gives an operator without shelling out to a build.
func captureUsage(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	defer f.Close()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	os.Stdout = f
	usage()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured usage: %v", err)
	}
	return string(b)
}

// usageEntry returns one flag's block from the usage text: its row plus the
// indented continuation lines beneath it. Flag rows are indented two spaces and
// continuations four or more, so an assertion about what an entry SAYS cannot
// be satisfied by a neighbouring flag's description.
func usageEntry(usageText, flag string) (string, bool) {
	lines := strings.Split(usageText, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "  "+flag+" ") || strings.HasPrefix(l, "  "+flag+"=") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	entry := []string{lines[start]}
	for _, l := range lines[start+1:] {
		if !strings.HasPrefix(l, "    ") {
			break
		}
		entry = append(entry, l)
	}
	return strings.Join(entry, "\n"), true
}
