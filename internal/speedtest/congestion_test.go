package speedtest

import (
	"testing"
)

// -C is a Linux-only iperf3 option; on macOS/Windows passing it aborts every
// run. congestionForOS keeps a requested algorithm on Linux and drops it
// (running with the system default) anywhere else - the guard for a congestion
// value that rode an imported backup onto the wrong platform.
func TestCongestionForOS(t *testing.T) {
	cases := []struct {
		req, goos   string
		wantEff     string
		wantDropped bool
	}{
		{"bbr", "linux", "bbr", false},
		{"cubic", "linux", "cubic", false},
		{"bbr", "freebsd", "bbr", false},
		{"bbr", "darwin", "", true},
		{"bbr", "windows", "", true},
		{"", "darwin", "", false}, // nothing requested: nothing dropped
		{"", "linux", "", false},
	}
	for _, c := range cases {
		eff, dropped := congestionForOS(c.req, c.goos)
		if eff != c.wantEff || dropped != c.wantDropped {
			t.Errorf("congestionForOS(%q,%q) = (%q,%v), want (%q,%v)",
				c.req, c.goos, eff, dropped, c.wantEff, c.wantDropped)
		}
	}
}
