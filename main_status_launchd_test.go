package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kardianos/service"

	"github.com/pingular/pingularity/internal/config"
)

// These run against the launchd daemon `sudo pingularity install` put on THIS
// Mac, from an ordinary shell - the invocation the README shows. kardianos asks
// `launchctl list pingularity`, which answers for the caller's own domain: an
// unelevated caller looks in its login session, misses the system daemon, and
// the library turns the miss into "stopped" because the plist exists. Only
// Status() is ever called here (a read-only `launchctl list`); nothing starts,
// stops or loads the user's service. Skipped anywhere the setup is absent.
func liveLaunchdDaemon(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("launchd only")
	}
	if os.Geteuid() == 0 {
		t.Skip("root sees the system domain; the blind reading needs an ordinary user")
	}
	if _, err := os.Stat("/Library/LaunchDaemons/pingularity.plist"); err != nil {
		t.Skip("no installed launchd daemon on this Mac")
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://127.0.0.1:9000/healthz")
	if err != nil {
		t.Skipf("no daemon answering on :9000 to prove the service is alive: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		t.Skipf("/healthz on :9000 did not answer ok (%d %q)", resp.StatusCode, body)
	}
}

// captureStatus runs `pingularity status` with os.Stdout pointed at a temp
// file and returns what it printed.
func captureStatus(t *testing.T) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "status")
	if err != nil {
		t.Fatalf("temp stdout: %v", err)
	}
	defer f.Close()
	orig := os.Stdout
	defer func() { os.Stdout = orig }()
	os.Stdout = f
	cmdErr := controlCmd("status", nil)
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read captured status: %v", err)
	}
	return string(b), cmdErr
}

// A daemon answering /healthz is not "stopped". Unelevated on macOS the state
// cannot be read, and the command must say that - and where to look - rather
// than print a confident wrong answer with exit 0.
func TestStatusUnelevatedOnMacDoesNotCallALiveDaemonStopped(t *testing.T) {
	liveLaunchdDaemon(t)
	out, err := captureStatus(t)
	if err != nil {
		t.Fatalf("status returned an error: %v (output %q)", err, out)
	}
	if strings.Contains(out, "stopped") {
		t.Fatalf("status called a daemon that is answering /healthz stopped: %q", out)
	}
	if !strings.Contains(out, "sudo") || !strings.Contains(out, "healthz") {
		t.Errorf("status does not tell the operator how to learn the real state (sudo, or healthz): %q", out)
	}
}

// The same blind reading must not turn `restart` into `start`: that downgrade
// exists for a service KNOWN to be stopped, and here nothing is known.
func TestRestartIsNotDowngradedOnABlindLaunchdReading(t *testing.T) {
	liveLaunchdDaemon(t)
	prg := &program{cfg: config.Default(), log: slog.Default()}
	s, err := service.New(prg, svcConfig(nil))
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	st, serr := s.Status()
	t.Logf("kardianos Status() from uid %d: %v, err=%v", os.Geteuid(), st, serr)
	if got := effectiveControlAction("restart", s); got != "restart" {
		t.Fatalf("restart downgraded to %q on a reading that cannot see the daemon", got)
	}
}
