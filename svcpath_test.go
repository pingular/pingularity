package main

import (
	"strings"
	"testing"
)

// The launchd service gets a PATH that reaches the package managers' bin
// directories, so anything the daemon looks up by name - iperf3 - is found the
// way it is from a terminal. Nothing is written for the other platforms:
// systemd's default PATH already covers /usr/local/bin, and the Windows SCM
// inherits the machine PATH.
func TestServicePathReachesPackageManagersOnMacOS(t *testing.T) {
	p := servicePath("darwin")
	for _, want := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/opt/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if !strings.Contains(":"+p+":", ":"+want+":") {
			t.Errorf("servicePath(darwin) = %q lacks %s", p, want)
		}
	}
	for _, goos := range []string{"linux", "windows", "freebsd"} {
		if got := servicePath(goos); got != "" {
			t.Errorf("servicePath(%s) = %q, want nothing written", goos, got)
		}
	}
}
