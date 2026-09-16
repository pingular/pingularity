package speedtest

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeIperf writes an executable named iperf3 into dir that prints one version
// line, the way the real one does, and returns its path.
func fakeIperf(t *testing.T, dir, version string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake iperf3 is a shell script")
	}
	p := filepath.Join(dir, "iperf3")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'iperf "+version+" (cJSON 1.7.15)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A SERVICE DOES NOT SEE THE SHELL'S PATH. launchd starts a system daemon with
// /usr/bin:/bin:/usr/sbin:/sbin, systemd with much the same, and Homebrew puts
// iperf3 in /opt/homebrew/bin - so the dashboard said iperf3 was not installed on
// a Mac where `iperf3 --version` answered from every terminal. The binary is
// looked for on PATH first and then where the package managers put it.
func TestIperfIsFoundWherePackageManagersPutItWhenPathLacksIt(t *testing.T) {
	bare, brew := t.TempDir(), t.TempDir()
	t.Setenv("PATH", bare)
	want := fakeIperf(t, brew, "3.21")
	restore := IperfExtraDirs
	t.Cleanup(func() { IperfExtraDirs = restore })
	IperfExtraDirs = []string{filepath.Join(bare, "nowhere"), brew}
	if got := iperfPath(); got != want {
		t.Fatalf("iperfPath() = %q, want the package location %q", got, want)
	}
	if !IperfAvailable() {
		t.Fatal("IperfAvailable() = false with iperf3 in a package location")
	}
	if got := iperfVersionProbe(); got != "3.21" {
		t.Errorf("iperfVersionProbe() = %q, want 3.21 read from the package location", got)
	}
	// A run execs that same path, not the bare name the service cannot resolve.
	orig := iperfExec
	t.Cleanup(func() { iperfExec = orig })
	var ran string
	iperfExec = func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
		ran = name
		return []byte("{}"), nil
	}
	if _, err := (iperfAuth{}).run(context.Background(), []string{"-c", "h"}); err != nil {
		t.Fatal(err)
	}
	if ran != want {
		t.Errorf("a run execs %q, want %q", ran, want)
	}
}

// PATH wins where it has one, so an operator's own choice is honoured.
func TestIperfOnPathWinsOverPackageLocations(t *testing.T) {
	onPath, brew := t.TempDir(), t.TempDir()
	t.Setenv("PATH", onPath)
	want := fakeIperf(t, onPath, "3.20")
	fakeIperf(t, brew, "3.21")
	restore := IperfExtraDirs
	t.Cleanup(func() { IperfExtraDirs = restore })
	IperfExtraDirs = []string{brew}
	if got := iperfPath(); got != want {
		t.Fatalf("iperfPath() = %q, want the PATH one %q", got, want)
	}
}

// And with no iperf3 anywhere, it is honestly absent.
func TestIperfAbsentEverywhereIsAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	restore := IperfExtraDirs
	t.Cleanup(func() { IperfExtraDirs = restore })
	IperfExtraDirs = []string{t.TempDir()}
	if p := iperfPath(); p != "" {
		t.Fatalf("iperfPath() = %q with no iperf3 anywhere", p)
	}
	if IperfAvailable() {
		t.Fatal("IperfAvailable() = true with no iperf3 anywhere")
	}
}
