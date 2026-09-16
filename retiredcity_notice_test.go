package main

import (
	"context"
	"github.com/pingular/pingularity/internal/speedtest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// retiredScopeRows are the two rows a pre-picker install left in its settings
// table when its operator scoped automatic selection to a city. Spelled out,
// not taken from a constant: they are what an upgraded database really carries.
var retiredScopeRows = map[string]string{
	"speed_auto_loc":   "49.2827,-123.1207",
	"speed_auto_label": "Vancouver, BC",
}

// An upgraded install whose auto-selection city stopped steering anything looks
// exactly like a healthy one: it serves, it measures, its health endpoint
// answers 200 - and the server behind its speed history has changed, with no
// setting altered to explain it. Boot the REAL binary on such a database, which
// is the only place run()'s wiring is observable, and require the boot output to
// carry the notice. The install that has settled it must NOT get the line, or
// operators learn to scroll past it.
func TestUpgradedInstallStartupOutputNamesTheRetiredCityScope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-boot retired-scope notice test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping build-and-boot retired-scope notice test")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pingularity")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	// Stamped with a version of its own, so the boots below can tell this
	// binary's daemon from any other answering on the same port.
	version := noticeBuildVersion()
	build := exec.Command(goBin, "build", "-ldflags=-X main.version="+version, "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// The needle is the WHOLE line, rendered by the same function run() prints,
	// against the address this boot was given - a facet substring would go on
	// passing with the notice reworded into something that says nothing.
	out, addr := bootOnUpgradedStore(t, bin, version, retiredScopeRows)
	notice := retiredCityScopeLine(retiredScopeRows["speed_auto_label"], addr)
	if !strings.Contains(out, notice) {
		t.Errorf("an install whose stored selection city no longer steers anything boots with no word about it.\nwant this line:\n%s\nfull output:\n%s", notice, out)
	}

	// Same database plus a pinned server: the pin overrode the city then and
	// overrides the race now, so nothing about this install's measurements
	// changed and there is nothing to announce. Sharing the needle with the
	// assertion above is what keeps this guard honest - reword the notice and
	// both follow it.
	pinned := map[string]string{"speed_server_id": "1993"}
	for k, v := range retiredScopeRows {
		pinned[k] = v
	}
	quiet, quietAddr := bootOnUpgradedStore(t, bin, version, pinned)
	if unwanted := retiredCityScopeLine(retiredScopeRows["speed_auto_label"], quietAddr); strings.Contains(quiet, unwanted) {
		t.Errorf("an install with a pinned server measures the same server it always did, and still gets told its selection changed:\n%s\nfull output:\n%s", unwanted, quiet)
	}
}

// bootOnUpgradedStore seeds a database the way an older build left it, boots the
// already-built binary on it until that binary is serving (see
// bootUntilServing), and returns everything it printed on both streams plus the
// -listen address it used, so a caller can render what the daemon rendered.
func bootOnUpgradedStore(t *testing.T, bin, version string, rows map[string]string) (output, listenAddr string) {
	t.Helper()
	return bootUntilServing(t, bin, version, func(db string) {
		st, err := store.Open(db)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		if _, err := st.SetSettingsDiff(context.Background(), rows); err != nil {
			st.Close()
			t.Fatalf("seed the settings table: %v", err)
		}
		st.Close()
	})
}

// Who is told, and who is not. The judgement is made against the real settings
// controller over a real store, because the wiring is the feature: a copy of
// the rule in a test would go on agreeing with itself after the rule moved.
func TestRetiredCityScopeSpeaksOnlyForInstallsThatHaveNotChosen(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T, rows map[string]string) *settings.Controller {
		t.Helper()
		st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { st.Close() })
		if len(rows) > 0 {
			if _, err := st.SetSettingsDiff(ctx, rows); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
		set, err := settings.New(ctx, st, settings.Values{})
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		return set
	}
	with := func(extra map[string]string) map[string]string {
		rows := map[string]string{}
		for k, v := range retiredScopeRows {
			rows[k] = v
		}
		for k, v := range extra {
			rows[k] = v
		}
		return rows
	}

	if got := retiredCityScope(seed(t, retiredScopeRows)); got != "Vancouver, BC" {
		t.Errorf("a scoped install that has chosen nothing since: got %q, want the stored city - this is the install whose server moved", got)
	}
	// A pin resolves by ID and overrode the city then, so its measurements did
	// not change.
	if got := retiredCityScope(seed(t, with(map[string]string{"speed_server_id": "1993"}))); got != "" {
		t.Errorf("a pinned install got %q; it measures the server it always did", got)
	}
	// A starred server's city races every run, which is this build's way of
	// saying "look for servers there" - the operator has already made the choice
	// the line would ask for.
	starred := `[{"id":"1993","sponsor":"EBOX","name":"Montreal, QC","lat":45.5,"lon":-73.5}]`
	if got := retiredCityScope(seed(t, with(map[string]string{"speed_servers": starred}))); got != "" {
		t.Errorf("an install with starred servers got %q; its cities are in the race already", got)
	}
	if got := retiredCityScope(seed(t, nil)); got != "" {
		t.Errorf("an install that never had a scope got %q, want \"\"", got)
	}

	// An install whose speedtests run on iperf3 measures the server it names
	// itself. The Ookla city never chose that server, then or now, so nothing
	// behind its speed history moved and a line saying otherwise is untrue. With
	// iperf3 missing from PATH, though, every run falls back to Ookla - and there
	// the city is exactly what stopped counting.
	if runtime.GOOS == "windows" {
		return // the stand-in below is a shell script
	}
	withIperf := t.TempDir()
	if err := os.WriteFile(filepath.Join(withIperf, "iperf3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	onIperf := with(map[string]string{"speed_engine": "iperf3"})
	t.Setenv("PATH", withIperf)
	if got := retiredCityScope(seed(t, onIperf)); got != "" {
		t.Errorf("an install whose speedtests run on iperf3 got %q; the city never chose its server", got)
	}
	// No iperf3 anywhere: a bare PATH is not enough, the daemon also looks where
	// the package managers put one, and this machine may have one there.
	t.Setenv("PATH", t.TempDir())
	restoreDirs := speedtest.IperfExtraDirs
	speedtest.IperfExtraDirs = nil
	t.Cleanup(func() { speedtest.IperfExtraDirs = restoreDirs })
	if got := retiredCityScope(seed(t, onIperf)); got != "Vancouver, BC" {
		t.Errorf("an iperf3 install with no iperf3 on PATH runs Ookla and got %q, want the stored city", got)
	}
}

// A daemon whose first settings read fails does not stop: run() warns, arms a
// retry, and carries on booting on the controller settings.New handed back -
// through both boot notices, before it serves. Neither may take that boot down,
// and neither may announce a city or a parked schedule that nothing was able to
// read.
func TestBootNoticesSurviveASettingsLoadThatFailed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.SetSettingsDiff(ctx, retiredScopeRows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st.Close() // the first read fails, as it can on a real boot
	set, err := settings.New(ctx, st, settings.Values{})
	if err == nil || set == nil {
		t.Fatalf("settings.New on an unreadable store = (%v, %v); this test no longer models the boot that keeps going on a failed load", set, err)
	}
	if got := retiredCityScope(set); got != "" {
		t.Errorf("a boot whose settings could not be read announces the retired city %q", got)
	}
	if got := set.NeverActiveSchedules(); len(got) != 0 {
		t.Errorf("a boot whose settings could not be read announces parked schedules %v", got)
	}
}

// The notice must be ONE line in the startup-line voice, and it has to quote the
// scope back, say what picks the server now, and name both ways to take the
// choice back - nobody should have to go and read the docs to find out why
// their speed history stepped.
func TestRetiredCityScopeLineIsOneActionableLine(t *testing.T) {
	line := retiredCityScopeLine("Vancouver, BC", ":9000")
	if strings.Contains(line, "\n") {
		t.Errorf("the retired-scope notice is not a single line: %q", line)
	}
	if !strings.HasPrefix(line, "pingularity") {
		t.Errorf("the retired-scope notice breaks the startup-line style (no \"pingularity\" prefix): %q", line)
	}
	for _, want := range []string{
		"Vancouver, BC",  // the scope, quoted back so it is recognisable
		"no longer used", // that it stopped steering anything
		// What picks the server instead, all of it: the cities raced, where
		// they come from, and how the winner is decided - and what that does to
		// the history the operator is looking at.
		"race the cities this connection names",
		"plus the cities of servers you star",
		"the one that answers fastest",
		"the server behind your speed history may have changed",
		"Star",                  // the softer of the two ways back
		"pin",                   // and the absolute one
		"http://localhost:9000", // where both of them live
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the retired-scope notice missing %q: %q", want, line)
		}
	}
	if other := retiredCityScopeLine("Vancouver, BC", "127.0.0.1:19123"); !strings.Contains(other, "http://localhost:19123") {
		t.Errorf("the retired-scope notice ignores the listen address: %q", other)
	}
}
