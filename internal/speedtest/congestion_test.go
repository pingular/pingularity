package speedtest

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// Only macOS/Windows iperf3 lacks -C, where passing it aborts every run.
// congestionForOS keeps a requested algorithm on Linux and FreeBSD and drops it
// (running with the system default) elsewhere - the guard for a congestion value
// that rode an imported backup onto the wrong platform.
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

// The warning must not call -C Linux-only: FreeBSD takes it, and a maintainer who
// believes the old wording would narrow congestionForOS and break that platform.
func TestWarnCongestionSkipped(t *testing.T) {
	var buf bytes.Buffer
	i := &Iperf{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	i.warnCongestionSkipped("bbr", "darwin")
	i.warnCongestionSkipped("bbr", "darwin")

	got := buf.String()
	if strings.Contains(got, "Linux-only") {
		t.Errorf("warning still claims -C is Linux-only: %s", got)
	}
	if !strings.Contains(got, "FreeBSD") {
		t.Errorf("warning does not name FreeBSD, where -C works: %s", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("warned %d times, want once per Iperf", n)
	}
}
