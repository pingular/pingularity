package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A schedule switched on with no weekday selected can never be active, so the
// feature it gates never runs - and an install measuring nothing looks exactly
// like an install with nothing to report. The dashboard refuses to save that
// state, so it arrives from the settings API, a restored backup or a
// hand-edited database, where nothing else would ever mention it. Boot the REAL
// binary on a store holding one and require the boot output to name the
// schedule, what it parked, and the way out.
func TestParkedScheduleStartupOutputSaysProbingIsParked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-boot parked-schedule notice test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping build-and-boot parked-schedule notice test")
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

	// The rows an older build leaves behind, or a config script writes: the
	// toggle on, and one window that selects no day.
	parked := bootSeededInstall(t, bin, version, map[string]string{
		"sched_lat_enabled": "1",
		"sched_lat_windows": `[{"days":"0000000","start":0,"end":0}]`,
	})
	for _, want := range []string{
		"latency schedule", // which schedule
		"no active days",   // what is wrong with it, in the dashboard's own words
		"parked",           // what that costs
		"pick a day",       // the way out, also the dashboard's own words
	} {
		if !strings.Contains(parked, want) {
			t.Errorf("boot output on a parked latency schedule never says %q - the install measures nothing and says nothing.\nfull output:\n%s", want, parked)
		}
	}

	// The same schedule with a real day in it is an ordinary schedule: it gates
	// probing, as it is meant to, and a notice here would teach operators to
	// ignore the one above.
	live := bootSeededInstall(t, bin, version, map[string]string{
		"sched_lat_enabled": "1",
		"sched_lat_windows": `[{"days":"0111110","start":540,"end":1020}]`,
	})
	if strings.Contains(live, "parked") {
		t.Errorf("boot output on a working schedule announces a parked one:\n%s", live)
	}
}

// bootSeededInstall writes rows into a fresh store the way an older build or a
// config restore leaves them, boots the already-built binary against it until
// that binary is serving (see bootUntilServing), and returns everything it
// printed on both streams.
func bootSeededInstall(t *testing.T, bin, version string, rows map[string]string) string {
	t.Helper()
	out, _ := bootUntilServing(t, bin, version, func(dbPath string) {
		st, err := store.Open(dbPath)
		if err != nil {
			t.Fatalf("open %s: %v", dbPath, err)
		}
		if _, err := st.SetSettingsDiff(context.Background(), rows); err != nil {
			st.Close()
			t.Fatalf("seed: %v", err)
		}
		st.Close()
	})
	return out
}

// noticeBuildVersion is the version a boot-notice test stamps into the binary
// it builds: unique to this process and this build, so that daemon can be told
// apart from every other one on the machine.
func noticeBuildVersion() string {
	return fmt.Sprintf("notice-test-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// bootUntilServing boots the already-built binary on a store that seed
// prepares, on a free high port, and waits until THAT binary answers
// /api/status - so boot is past every startup line - then kills it and returns
// everything it printed on both streams plus the -listen address it used.
//
// Any 200 on the port is not the same answer. reserveHighPort hands every test
// process the lowest free port from 19000 up and lets go of it just before the
// child binds, so two processes running at once can be handed the same one:
// one child loses the bind and exits, and a poll that takes any 200 gets it
// from the OTHER process's daemon, kills its own child before that child has
// printed a word, and reports the notice missing from an empty output. So the
// answer has to carry the version this test stamped into its own build, and a
// child that exits before it is seen serving is booted again - on a fresh port
// and a fresh store - rather than reported as a boot that said nothing. A daemon
// that never serves at all still fails, with what it printed.
func bootUntilServing(t *testing.T, bin, version string, seed func(dbPath string)) (output, listenAddr string) {
	t.Helper()
	const attempts = 5
	last := ""
	for i := 0; i < attempts; i++ {
		dbPath := filepath.Join(t.TempDir(), "seeded.db")
		seed(dbPath)
		port, releasePort := reserveHighPort(t)
		addr := "127.0.0.1:" + port
		// -quick-setup=skip answers the first-run offer: these installs have been
		// answered already, and the point is the boot output, not the hold.
		cmd := exec.Command(bin, "run", "-listen", addr, "-db", dbPath, "-quick-setup=skip")
		// Buffers rather than pipes: Wait closes a pipe the moment the child exits,
		// which can beat the reader to the boot lines and hand the assertions an
		// empty output that reads as "the notice was never printed". Wait drains a
		// buffer's copier before it returns, and nothing here reads the buffers
		// until it has.
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
		releasePort()
		if err := cmd.Start(); err != nil {
			t.Fatalf("start failed: %v", err)
		}
		exited := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(exited)
		}()
		ready := servesAsVersion(addr, version, exited)
		select {
		case <-exited:
		default:
			_ = cmd.Process.Kill()
			<-exited
		}
		if ready {
			return outBuf.String() + errBuf.String(), addr
		}
		last = "stdout:\n" + outBuf.String() + "\nstderr:\n" + errBuf.String()
	}
	t.Fatalf("the daemon built as %q never served /api/status in %d boots; the last one printed\n%s", version, attempts, last)
	return "", ""
}

// servesAsVersion polls addr until the daemon answering there reports version,
// the child exits, or thirty seconds pass.
func servesAsVersion(addr, version string, exited <-chan struct{}) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return false
		default:
		}
		if resp, err := client.Get("http://" + addr + "/api/status"); err == nil {
			var status struct {
				Version string `json:"version"`
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decodeErr == nil && status.Version == version {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// The notice must be ONE line, in the startup-line voice, and it must name the
// schedule, what it parked and the way out - the point of the line is that
// nobody should have to go read the docs to find out why the install is
// measuring nothing. The exit is worded as the dashboard words it, so the two
// places an operator meets this state say the same thing.
func TestSchedParkedLineIsOneActionableLine(t *testing.T) {
	for feature, parked := range map[string]string{
		"latency":   "latency probing is parked",
		"speedtest": "no automatic speedtest will run",
	} {
		line := schedParkedLine(feature)
		if strings.Contains(line, "\n") {
			t.Errorf("parked-schedule notice for %q is not a single line: %q", feature, line)
		}
		if !strings.HasPrefix(line, "pingularity") {
			t.Errorf("parked-schedule notice for %q breaks the startup-line style (no \"pingularity\" prefix): %q", feature, line)
		}
		for _, want := range []string{
			feature + " schedule", // which schedule is at fault
			"no active days",      // what is wrong with it
			parked,                // what that costs, in this feature's own terms
			"pick a day or turn it off",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("parked-schedule notice for %q missing %q: %q", feature, want, line)
			}
		}
	}
	// The two features must not render the same sentence: an operator with both
	// schedules parked gets two lines, and two identical lines say nothing about
	// which is which.
	if schedParkedLine("latency") == schedParkedLine("speedtest") {
		t.Error("both schedules render the same notice - neither line says which feature stopped")
	}
}
