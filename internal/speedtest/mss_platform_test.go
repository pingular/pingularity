package speedtest

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// mssForOS keeps a requested segment size on Linux and FreeBSD and drops it
// (running with the system default) elsewhere: macOS refuses every value above
// its 512-byte pre-connect default and Windows cannot set one at all, and
// iperf3 aborts the run on the refusal. Same platform set as congestionForOS,
// for the same import-a-backup reason.
func TestMSSForOS(t *testing.T) {
	cases := []struct {
		req         int
		goos        string
		wantEff     int
		wantDropped bool
	}{
		{1400, "linux", 1400, false},
		{9000, "linux", 9000, false},
		{1400, "freebsd", 1400, false},
		{1400, "darwin", 0, true},
		{300, "darwin", 0, true}, // even a value macOS would take: the knob is all-or-nothing, like -C
		{1400, "windows", 0, true},
		{0, "darwin", 0, false}, // nothing requested: nothing dropped
		{0, "linux", 0, false},
	}
	for _, c := range cases {
		eff, dropped := mssForOS(c.req, c.goos)
		if eff != c.wantEff || dropped != c.wantDropped {
			t.Errorf("mssForOS(%d,%q) = (%d,%v), want (%d,%v)",
				c.req, c.goos, eff, dropped, c.wantEff, c.wantDropped)
		}
	}
}

// The warning names the setting, the value and the OS, once per Iperf, and
// like the -C one must not call the knob Linux-only (FreeBSD keeps it).
func TestWarnMSSSkipped(t *testing.T) {
	var buf bytes.Buffer
	i := &Iperf{Log: slog.New(slog.NewTextHandler(&buf, nil))}
	i.warnMSSSkipped(1400, "darwin")
	i.warnMSSSkipped(1400, "darwin")

	got := buf.String()
	for _, want := range []string{"segment size", "requested=1400", "os=darwin", "FreeBSD"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "Linux-only") {
		t.Errorf("warning calls -M Linux-only, which would invite narrowing the guard: %s", got)
	}
	if n := strings.Count(got, "\n"); n != 1 {
		t.Errorf("warned %d times, want once per Iperf", n)
	}
	var quiet Iperf
	quiet.warnMSSSkipped(1400, "darwin") // nil Log: nothing to write to, nothing to panic on
}

// attributeWindow rewrites only iperf3's refused-socket-buffer message, only
// when a window was asked for, naming the setting, the value and the sysctl
// that decides on that OS - and never a word isTransientIperfErr would read as
// a reason to retry.
func TestAttributeWindow(t *testing.T) {
	const refused = "socket buffer size not set correctly"
	cases := []struct {
		text   string
		window int
		goos   string
		want   []string // substrings of the result; nil means "returned unchanged"
	}{
		{refused, 16384, "darwin", []string{refused + " (", "Window size setting, 16384 KB", "kern.ipc.maxsockbuf"}},
		{refused, 16384, "freebsd", []string{"kern.ipc.maxsockbuf"}},
		{refused, 1024, "linux", []string{"Window size setting, 1024 KB", "net.core.rmem_max and net.core.wmem_max"}},
		{refused, 1024, "windows", []string{"Window size setting, 1024 KB", "socket-buffer limit"}},
		{refused, 0, "darwin", nil},                          // no window asked for: the message is iperf3's to explain
		{"unable to connect to server", 1024, "darwin", nil}, // a different failure keeps its own text
	}
	for _, c := range cases {
		got := attributeWindow(c.text, c.window, c.goos)
		if c.want == nil {
			if got != c.text {
				t.Errorf("attributeWindow(%q,%d,%q) = %q, want unchanged", c.text, c.window, c.goos, got)
			}
			continue
		}
		if !strings.HasPrefix(got, c.text) {
			t.Errorf("attributeWindow(%q,%d,%q) = %q; the original text must stay the prefix", c.text, c.window, c.goos, got)
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("attributeWindow(%q,%d,%q) = %q, lacks %q", c.text, c.window, c.goos, got, w)
			}
		}
		if isTransientIperfErr(errorString(got)) {
			t.Errorf("attributed text reads as transient, so the run would retry a refusal that cannot clear: %q", got)
		}
	}
}

// errorString is a plain error over a string, so the transient classifier can
// be asked about text the way withRetry asks it about a real failure.
type errorString string

func (e errorString) Error() string { return string(e) }
