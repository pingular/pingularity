package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const postinstallPath = "packaging/postinstall.sh"

// systemctlStub stands in for systemd. POSTINST_STATES is the sequence of unit
// states `is-active` answers, one per call, the last repeating - which is how a
// unit that will not stay up looks from a script: Restart=always/RestartSec=5
// parks it in "activating" (auto-restart) for five seconds out of every six.
// POSTINST_START_RC is what `start`/`try-restart` return.
//
// They return 0 by default ON PURPOSE. The unit declares no Type=, so it is
// Type=simple: systemd calls the start job done at fork/exec, not when the
// daemon is serving, and a binary that execs and then exits is a start that
// "succeeded". That is measurable here without systemd - `pingularity run`
// given a -db that names a directory execs cleanly and exits 1 well under a
// second - and it is why nothing downstream may trust the return code alone.
//
// POSTINST_UNBOOTED makes every call fail the way systemctl does where it is
// installed but systemd is not running - a chroot, a container that was never
// booted, WSL without systemd: two lines of complaint on stderr and a non-zero
// exit. POSTINST_ISACTIVE_ERR is a complaint `is-active` writes to stderr beside
// its answer.
const systemctlStub = `#!/bin/sh
echo "systemctl $*" >> "$POSTINST_LOG"
if [ -n "$POSTINST_UNBOOTED" ]; then
  echo "System has not been booted with systemd as init system (PID 1). Can't operate." >&2
  echo "Failed to connect to bus: Host is down" >&2
  exit 1
fi
case "$1" in
  is-active)
    if [ -n "$POSTINST_ISACTIVE_ERR" ]; then echo "$POSTINST_ISACTIVE_ERR" >&2; fi
    n=0
    if [ -f "$POSTINST_STATE_N" ]; then read -r n < "$POSTINST_STATE_N"; fi
    echo $((n + 1)) > "$POSTINST_STATE_N"
    set -- $POSTINST_STATES
    while [ "$n" -gt 0 ] && [ "$#" -gt 1 ]; do shift; n=$((n - 1)); done
    [ "$1" = active ] && exit 0
    exit 3 ;;
  start|try-restart)
    exit "${POSTINST_START_RC:-0}" ;;
esac
exit 0
`

// debHelperStub is deb-systemd-helper. POSTINST_WAS_ENABLED_RC decides whether
// the unit still has its [Install] symlinks - i.e. whether an admin ran
// `systemctl disable` before the upgrade.
const debHelperStub = `#!/bin/sh
echo "deb-systemd-helper $*" >> "$POSTINST_LOG"
case "$*" in
  *was-enabled*) exit "${POSTINST_WAS_ENABLED_RC:-0}" ;;
esac
exit 0
`

// sleepStub keeps the tests instant while still recording that the script
// spaced its samples out - a watch that samples five times in a microsecond
// watches nothing.
const sleepStub = `#!/bin/sh
echo "sleep $*" >> "$POSTINST_LOG"
exit 0
`

// getentStub says the service account already exists, so the useradd branch
// (not under test here) is skipped; chownStub keeps the data-directory branch
// from touching a real /var/lib/pingularity on a Linux checkout.
const getentStub = `#!/bin/sh
exit 0
`

const chownStub = `#!/bin/sh
echo "chown $*" >> "$POSTINST_LOG"
exit 0
`

type postinstallEnv struct {
	states        []string // what is-active answers, one per call, last repeating
	startRC       string   // return code of start/try-restart ("" = 0)
	wasEnabledRC  string   // return code of deb-systemd-helper was-enabled ("" = 0)
	withDebHelper bool     // deb has deb-systemd-helper on PATH; rpm does not
	unbooted      bool     // systemctl is installed but systemd is not running
	isActiveErr   string   // what is-active writes to stderr beside its answer
}

// runPostinstall runs the REAL packaging/postinstall.sh under stubbed
// systemd/passwd tooling and returns its exit code, its output, and every stub
// call it made. The maintainer scripts are shell that no Go code imports, so
// running them is the only way to test what apt and dnf actually print.
func runPostinstall(t *testing.T, env postinstallEnv, args ...string) (int, string, []string) {
	t.Helper()
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"systemctl": systemctlStub,
		"sleep":     sleepStub,
		"getent":    getentStub,
		"chown":     chownStub,
	}
	if env.withDebHelper {
		stubs["deb-systemd-helper"] = debHelperStub
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(dir, "calls.log")

	script, err := filepath.Abs(postinstallPath)
	if err != nil {
		t.Fatal(err)
	}
	unbooted := ""
	if env.unbooted {
		unbooted = "1"
	}
	// The script takes the deb road wherever it finds a deb-systemd-helper, and an
	// rpm machine has none - but every Debian and Ubuntu machine running these tests
	// does. So for rpm, a directory carrying the host's own is left off PATH. The
	// stubs use nothing but shell builtins, so nothing they need goes with it.
	path := stubDir
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if !env.withDebHelper && hasDebHelper(d) {
			continue
		}
		path += string(os.PathListSeparator) + d
	}
	cmd := exec.Command("sh", append([]string{script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+path,
		"POSTINST_LOG="+logPath,
		"POSTINST_STATE_N="+filepath.Join(dir, "state.n"),
		"POSTINST_STATES="+strings.Join(env.states, " "),
		"POSTINST_START_RC="+env.startRC,
		"POSTINST_WAS_ENABLED_RC="+env.wasEnabledRC,
		"POSTINST_UNBOOTED="+unbooted,
		"POSTINST_ISACTIVE_ERR="+env.isActiveErr,
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %s: %v\n%s", postinstallPath, err, out)
		}
		code = ee.ExitCode()
	}
	var calls []string
	if b, rerr := os.ReadFile(logPath); rerr == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				calls = append(calls, l)
			}
		}
	}
	return code, string(out), calls
}

func hasDebHelper(dir string) bool {
	for _, name := range []string{"deb-systemd-helper", "deb-systemd-helper.exe"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
}

// shReachesStubSleep reports whether sh, given a PATH that starts with a stub
// sleep, runs that stub. Git for Windows' sh puts its own tools ahead of the PATH
// it is handed, so there the real sleep runs and the stub never logs a call.
func shReachesStubSleep(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleepStub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 0")
	cmd.Env = append(os.Environ(),
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"POSTINST_LOG="+logPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -c 'sleep 0': %v\n%s", err, out)
	}
	b, _ := os.ReadFile(logPath)
	return strings.Contains(string(b), "sleep 0")
}

func countCalls(calls []string, substr string) int {
	n := 0
	for _, c := range calls {
		if strings.Contains(c, substr) {
			n++
		}
	}
	return n
}

// The upgrade branch is the one an operator never watches: apt and dnf print
// whatever it prints and then say "done". A new binary that execs and dies -
// the realistic upgrade failure - leaves the machine with no monitoring, so the
// package has to say so. It cannot learn that from try-restart's return code
// (Type=simple, see systemctlStub); it has to look at the unit afterwards, and
// look more than once: here the daemon is still up for the first sample and
// gone for every one after it.
func TestPostinstallUpgradeReportsADaemonThatDidNotComeBack(t *testing.T) {
	code, out, calls := runPostinstall(t,
		postinstallEnv{states: []string{"active", "active", "activating"}, withDebHelper: true},
		"configure", "0.70.1")
	if code != 0 {
		t.Fatalf("postinstall exited %d; a package script must not fail the install:\n%s", code, out)
	}
	if countCalls(calls, "systemctl try-restart") != 1 {
		t.Fatalf("the upgrade did not try-restart the service:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(out, "systemctl status pingularity") {
		t.Fatalf("the upgrade said nothing about a daemon that did not come back, so apt/dnf report a clean success on a machine that has stopped monitoring:\noutput: %q\ncalls:\n%s", out, strings.Join(calls, "\n"))
	}
	if !shReachesStubSleep(t) {
		t.Log("this sh runs its own sleep ahead of the stub, so the wait between samples cannot be counted here")
	} else if sleeps := countCalls(calls, "sleep"); sleeps < 1 {
		t.Errorf("the unit was sampled %d times with no wait between samples - a watch that finishes instantly watches nothing:\n%s", countCalls(calls, "is-active"), strings.Join(calls, "\n"))
	}
}

// ...and stays quiet when the handover worked, which is the case every healthy
// machine takes. A package that cried wolf here would be worse than silence.
func TestPostinstallUpgradeStaysQuietWhenTheDaemonIsBack(t *testing.T) {
	code, out, calls := runPostinstall(t,
		postinstallEnv{states: []string{"active"}, withDebHelper: true},
		"configure", "0.70.1")
	if code != 0 {
		t.Fatalf("postinstall exited %d:\n%s", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("a successful upgrade printed %q; it must say nothing", out)
	}
	if countCalls(calls, "systemctl try-restart") != 1 {
		t.Errorf("the upgrade did not try-restart the service:\n%s", strings.Join(calls, "\n"))
	}
}

// A unit the admin had stopped (or that was already in 'failed') is deliberately
// left where it is: try-restart only touches a running service. That is not a
// failed handover and must not be reported as one, or every deliberately-stopped
// box gets a scary line on every upgrade.
func TestPostinstallUpgradeLeavesAStoppedServiceAlone(t *testing.T) {
	code, out, calls := runPostinstall(t,
		postinstallEnv{states: []string{"inactive"}, withDebHelper: true},
		"configure", "0.70.1")
	if code != 0 {
		t.Fatalf("postinstall exited %d:\n%s", code, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("an upgrade on a stopped service printed %q; it was not restarted because it was not running, which is the intended behaviour", out)
	}
	if countCalls(calls, "systemctl try-restart") != 1 {
		t.Errorf("try-restart must still be attempted - the decision belongs to systemd, not to us:\n%s", strings.Join(calls, "\n"))
	}
}

// The fresh branch has always claimed to report the truth. It did not: with no
// Type= in the unit, `systemctl start` returns 0 for a daemon that has already
// exited, and the install cheerfully printed a dashboard URL nothing is serving.
func TestPostinstallFreshDoesNotClaimARunningDaemonThatDied(t *testing.T) {
	code, out, _ := runPostinstall(t,
		postinstallEnv{states: []string{"active", "activating"}, withDebHelper: true},
		"configure")
	if code != 0 {
		t.Fatalf("postinstall exited %d:\n%s", code, out)
	}
	if strings.Contains(out, "is running") {
		t.Fatalf("a fresh install announced a running dashboard for a daemon that did not stay up:\n%s", out)
	}
	if !strings.Contains(out, "did not start") {
		t.Errorf("a fresh install that did not come up must say so:\n%s", out)
	}
}

// And a genuine fresh install still gets its dashboard line and its data/flags
// line - the first thing a new user reads.
func TestPostinstallFreshStillReportsAHealthyStart(t *testing.T) {
	code, out, calls := runPostinstall(t,
		postinstallEnv{states: []string{"active"}, withDebHelper: true},
		"configure")
	if code != 0 {
		t.Fatalf("postinstall exited %d:\n%s", code, out)
	}
	for _, want := range []string{"Pingularity is running", "http://localhost:9000", "data: /var/lib/pingularity", "flags: /etc/default/pingularity"} {
		if !strings.Contains(out, want) {
			t.Errorf("a healthy fresh install no longer prints %q:\n%s", want, out)
		}
	}
	if countCalls(calls, "systemctl start") != 1 {
		t.Errorf("a fresh install must start the service:\n%s", strings.Join(calls, "\n"))
	}
}

// The admin-disable survival the enable branch exists for, on both packagers:
// deb consults deb-systemd-helper's record of its own symlinks, rpm has no such
// helper and trusts the packager argument. Neither may re-enable on an upgrade.
func TestPostinstallUpgradeKeepsAnAdminDisableInPlace(t *testing.T) {
	_, _, calls := runPostinstall(t,
		postinstallEnv{states: []string{"active"}, wasEnabledRC: "1", withDebHelper: true},
		"configure", "0.70.1")
	if countCalls(calls, "deb-systemd-helper enable") != 0 {
		t.Errorf("an admin's `systemctl disable` was undone by the upgrade:\n%s", strings.Join(calls, "\n"))
	}
	if countCalls(calls, "update-state") != 1 {
		t.Errorf("the disabled unit's bookkeeping was not refreshed:\n%s", strings.Join(calls, "\n"))
	}
	_, _, rpmCalls := runPostinstall(t, postinstallEnv{states: []string{"active"}}, "2")
	if countCalls(rpmCalls, "systemctl enable") != 0 {
		t.Errorf("the rpm upgrade branch re-enabled the unit:\n%s", strings.Join(rpmCalls, "\n"))
	}
	if countCalls(rpmCalls, "systemctl try-restart") != 1 {
		t.Errorf("the rpm upgrade branch did not hand over to the new binary:\n%s", strings.Join(rpmCalls, "\n"))
	}
}

// Where systemctl is installed but systemd is not running - a chroot, a
// container that was never booted, WSL without systemd - every call fails and
// says why on stderr, and --quiet silences an answer, not an error. This script
// has always sent what systemctl says nowhere, so a package operation there
// prints only what the package means to; the look it takes at the unit before
// and after a handover has to keep to that, or an upgrade on such a box fills
// apt's or dnf's output with systemd's complaints.
func TestPostinstallKeepsSystemctlsErrorsOutOfAnUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  postinstallEnv
		args []string
	}{
		{"deb", postinstallEnv{unbooted: true, withDebHelper: true}, []string{"configure", "0.70.1"}},
		{"rpm", postinstallEnv{unbooted: true}, []string{"2"}},
	} {
		code, out, calls := runPostinstall(t, tc.env, tc.args...)
		if code != 0 {
			t.Fatalf("%s: postinstall exited %d; a package script must not fail the install:\n%s", tc.name, code, out)
		}
		if strings.TrimSpace(out) != "" {
			t.Errorf("%s: an upgrade where systemd is not running printed %q; nothing systemctl says belongs in the package's output\ncalls:\n%s", tc.name, out, strings.Join(calls, "\n"))
		}
	}
}

// ...and the watch after a fresh start reports in the install's own words: a
// complaint is-active writes to stderr is no part of the report, and the report
// still says the daemon did not start.
func TestPostinstallFreshReportsInItsOwnWordsOnly(t *testing.T) {
	const complaint = "Failed to get properties: Connection timed out"
	code, out, _ := runPostinstall(t,
		postinstallEnv{states: []string{"activating"}, isActiveErr: complaint, withDebHelper: true},
		"configure")
	if code != 0 {
		t.Fatalf("postinstall exited %d:\n%s", code, out)
	}
	if strings.Contains(out, complaint) {
		t.Errorf("a fresh install passed systemctl's own stderr through into its report:\n%s", out)
	}
	if !strings.Contains(out, "did not start") {
		t.Errorf("a fresh install whose unit is not active must still say so:\n%s", out)
	}
}

// The postinstall's watch reads a dying unit as "not active" because the unit
// spends the gap between restarts in auto-restart. That only holds while there
// IS a gap: with Restart= dropped the unit would sit in 'failed' (still caught),
// but with RestartSec=0 a fast-cycling daemon would look active at every sample.
// Keep the two files in step - if this changes, revisit service_stayed_up.
func TestPackagedUnitRestartGapBacksThePostinstallWatch(t *testing.T) {
	unit := mustReadRepoFile(t, "packaging/pingularity.service")
	for _, want := range []string{"Restart=always", "RestartSec=5"} {
		if !strings.Contains(unit, want) {
			t.Errorf("packaging/pingularity.service no longer sets %q; %s watches the unit after a restart and relies on the auto-restart gap to see a daemon that will not stay up", want, postinstallPath)
		}
	}
}

// aptDnfUpdatingBullet returns the README's "apt / dnf" bullet from the Updating
// section: the line that opens it and every continuation line under it.
func aptDnfUpdatingBullet(t *testing.T, readme string) string {
	t.Helper()
	lines := strings.Split(readme, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "- **apt / dnf**") {
			continue
		}
		bullet := []string{line}
		for _, next := range lines[i+1:] {
			if strings.HasPrefix(next, "- ") || strings.TrimSpace(next) == "" {
				break
			}
			bullet = append(bullet, next)
		}
		return strings.Join(bullet, "\n")
	}
	t.Fatal("README.md has no `- **apt / dnf**` bullet in its Updating section - this test's anchor has rotted")
	return ""
}

// The Updating section tells an operator the deb/rpm path hands the running
// service over to the new binary. That promise now has a stated failure mode,
// and the two halves have to keep saying the same thing: the line the upgrade
// branch prints is the ONLY warning anyone gets - apt and dnf report a clean
// success either way - and the README bullet is where the operator was told to
// expect one.
func TestREADMEUpgradeBulletMatchesWhatThePackageSays(t *testing.T) {
	const pointer = "systemctl status pingularity"
	script := mustReadRepoFile(t, postinstallPath)
	if !strings.Contains(script, `echo "Pingularity upgraded but is not running; check: `+pointer+`"`) {
		t.Fatalf("%s no longer tells an operator that the upgrade left the service down", postinstallPath)
	}
	bullet := aptDnfUpdatingBullet(t, mustReadRepoFile(t, "README.md"))
	if !strings.Contains(bullet, pointer) {
		t.Errorf("the README's apt / dnf Updating bullet does not say where the package sends an operator whose daemon did not come back:\n%s", bullet)
	}
	// The pointer alone is not the promise. A bullet telling operators that the
	// install stays silent and to go and look for themselves would keep it, and
	// would be the opposite of what the upgrade branch above does.
	const saysSo = "If it does not come back, the install says so"
	if !strings.Contains(unwrapped(bullet), saysSo) {
		t.Errorf("the README's apt / dnf Updating bullet never says %q, and the package prints a line when the daemon did not come back:\n%s", saysSo, bullet)
	}
}
