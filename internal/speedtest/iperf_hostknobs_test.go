package speedtest

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two iperf3 knobs the dashboard offers on every platform are refused by the
// kernel on some of them, and iperf3 answers a refused socket option by
// aborting the run before a byte moves. These tests drive the real engine
// against a real local `iperf3 -s`, so they say what an operator on this host
// would see rather than what a table claims; they skip where the binary or the
// host's own limits make the question moot.

// localIperfServer starts `iperf3 -s` on a free port from a small private range
// and returns its address. The server is this test's own child and is killed
// when the test ends - by handle, never by name.
func localIperfServer(t *testing.T) (host, port string) {
	t.Helper()
	bin, err := exec.LookPath("iperf3")
	if err != nil {
		t.Skip("iperf3 not on PATH")
	}
	for p := 9630; p <= 9639; p++ {
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		port = strconv.Itoa(p)
		break
	}
	if port == "" {
		t.Skip("no free port in 9630-9639")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "-s", "-p", port)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start iperf3 -s: %v", err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 200*time.Millisecond)
		if err == nil {
			c.Close()
			// The probe connect above is logged by the server as an aborted
			// control connection; give it a moment to return to accept.
			time.Sleep(300 * time.Millisecond)
			return "127.0.0.1", port
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("iperf3 -s on port %s never came up", port)
	return "", ""
}

// hostSocketBufKB is the largest -w (in KB) this host's kernel will grant, or
// 0 when the test cannot tell. iperf3 sets SO_SNDBUF and SO_RCVBUF to the
// window and aborts when the kernel hands back less: kern.ipc.maxsockbuf on
// macOS and FreeBSD; on Linux the smaller of net.core.rmem_max and wmem_max,
// doubled, because Linux reports twice what it granted.
func hostSocketBufKB(t *testing.T) int {
	t.Helper()
	switch runtime.GOOS {
	case "darwin", "freebsd":
		out, err := exec.Command("sysctl", "-n", "kern.ipc.maxsockbuf").Output()
		if err != nil {
			return 0
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			return 0
		}
		return n / 1024
	case "linux":
		read := func(p string) int {
			b, err := os.ReadFile(p)
			if err != nil {
				return 0
			}
			n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			return n
		}
		r, w := read("/proc/sys/net/core/rmem_max"), read("/proc/sys/net/core/wmem_max")
		if r == 0 || w == 0 {
			return 0
		}
		return 2 * min(r, w) / 1024
	}
	return 0
}

func localIperf(host, port string) *Iperf {
	return &Iperf{
		ServerFn:    func() string { return host + ":" + port },
		DurationFn:  func() int { return 1 },
		OmitFn:      func() int { return 0 },
		DirectionFn: func() string { return "down" },
		UDPFn:       func() bool { return false },
		RetriesFn:   func() int { return 0 },
	}
}

// A saved max segment size must not stop every run on a platform whose kernel
// refuses to set one. Linux takes -M as a ceiling and uses the smaller of it
// and the path MTU; macOS only lets a socket lower its segment size below the
// 512-byte pre-connect default, so 1400 - the VPN case the tip suggests - is
// EINVAL and iperf3 aborts before the transfer; Windows cannot set it at all.
// The engine already drops -C on those platforms and warns once; -M gets the
// same treatment, and the run measures.
func TestIperfRunIgnoresMSSWhereTheKernelCannotSetIt(t *testing.T) {
	if runtime.GOOS == "linux" || runtime.GOOS == "freebsd" {
		t.Skip("-M is honoured on this platform; nothing to drop")
	}
	host, port := localIperfServer(t)
	var logBuf bytes.Buffer
	i := localIperf(host, port)
	i.Log = slog.New(slog.NewTextHandler(&logBuf, nil))
	i.MSSFn = func() int { return 1400 }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := i.Run(ctx)
	if err != nil {
		t.Fatalf("Run with MSS=1400 on %s failed: %v (the saved MSS aborted the run)", runtime.GOOS, err)
	}
	if res.DownloadMbps <= 0 {
		t.Fatalf("Run with MSS=1400 measured nothing: %+v", res)
	}
	got := logBuf.String()
	if !strings.Contains(got, "segment size") || !strings.Contains(got, runtime.GOOS) {
		t.Errorf("no warning naming the ignored MSS setting and the OS; log = %q", got)
	}
	if n := strings.Count(got, "segment size"); n != 1 {
		t.Errorf("MSS warning logged %d times, want once per Iperf; log = %q", n, got)
	}
}

// The ceiling the dashboard offers is what iperf3 accepts, and a kernel tuned
// for a long-distance link can use all of it - so the ceiling is not the place
// to teach an operator what their kernel grants. The run is. Whichever side of
// the host's own limit the ceiling falls on, the operator must end up with an
// answer: a measurement, or a refusal that names the setting and the sysctl.
func TestIperfRunAtTheWindowCeilingExplainsItself(t *testing.T) {
	capKB := hostSocketBufKB(t)
	if capKB == 0 {
		t.Skip("cannot read this host's socket-buffer ceiling")
	}
	host, port := localIperfServer(t)
	i := localIperf(host, port)
	i.WindowFn = func() int { return 1 << 20 } // clamps to iperfMaxWindow
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := i.Run(ctx)

	if capKB >= iperfMaxWindow {
		// A kernel that grants the whole range must measure at the ceiling -
		// the reason the ceiling stays where iperf3 puts it.
		if err != nil {
			t.Fatalf("Run at the window ceiling (%d KB, host grants %d KB) failed: %v", iperfMaxWindow, capKB, err)
		}
		if res.DownloadMbps <= 0 {
			t.Fatalf("Run at the window ceiling measured nothing: %+v", res)
		}
		return
	}
	if err == nil {
		t.Fatalf("a %d KB window ran on a host that grants %d KB; the sysctl reading is wrong", iperfMaxWindow, capKB)
	}
	msg := err.Error()
	if !strings.Contains(msg, "socket buffer size not set correctly") {
		t.Fatalf("unexpected failure (not the kernel refusing the window): %v", err)
	}
	if !strings.Contains(msg, "Window size") || !strings.Contains(msg, strconv.Itoa(iperfMaxWindow)+" KB") {
		t.Errorf("the run an operator gets does not name the Window size setting and its %d KB: %q", iperfMaxWindow, msg)
	}
	if sysctl := hostWindowSysctl(); sysctl != "" && !strings.Contains(msg, sysctl) {
		t.Errorf("the run an operator gets does not name %s, the sysctl that decides: %q", sysctl, msg)
	}
}

// hostWindowSysctl is the knob that decides the socket-buffer ceiling on this
// platform, or "" where the test knows of none to demand.
func hostWindowSysctl() string {
	return map[string]string{
		"darwin":  "kern.ipc.maxsockbuf",
		"freebsd": "kern.ipc.maxsockbuf",
		"linux":   "net.core.rmem_max",
	}[runtime.GOOS]
}

// When the kernel does refuse a window, the error must say which setting did it
// and which sysctl decides, not the bare "socket buffer size not set correctly"
// iperf3 emits - that text names neither, and nothing else in the run does.
func TestIperfWindowRefusalNamesTheSetting(t *testing.T) {
	capKB := hostSocketBufKB(t)
	if capKB == 0 {
		t.Skip("cannot read this host's socket-buffer ceiling")
	}
	host, port := localIperfServer(t)
	tp := iperfTunables{dur: 1, streams: 1, window: capKB + 1024}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := runIperf(ctx, host, port, tp, iperfAuth{}, true)
	if err == nil {
		t.Fatalf("a %d KB window ran on a host that grants %d KB; the sysctl reading is wrong", tp.window, capKB)
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "socket buffer size not set correctly") {
		t.Fatalf("unexpected failure (not the kernel refusing the window): %v", err)
	}
	sysctl := hostWindowSysctl()
	check := func(t *testing.T, msg string) {
		t.Helper()
		if !strings.Contains(msg, "Window size") || !strings.Contains(msg, strconv.Itoa(tp.window)+" KB") {
			t.Errorf("error does not attribute the failure to the Window size setting (%d KB): %q", tp.window, msg)
		}
		if sysctl != "" && !strings.Contains(msg, sysctl) {
			t.Errorf("error does not name %s, the sysctl that decides: %q", sysctl, msg)
		}
	}
	check(t, msg)
	if isTransientIperfErr(err) {
		t.Errorf("a refused window must stay non-transient (a retry cannot help): %q", msg)
	}

	// The bidirectional transfer reports through its own parser and must
	// attribute the same refusal the same way.
	time.Sleep(iperfUploadSettle)
	_, err = runIperfBidir(ctx, host, port, tp, iperfAuth{})
	if err == nil {
		t.Fatalf("a %d KB window ran bidirectionally on a host that grants %d KB", tp.window, capKB)
	}
	if !strings.HasPrefix(err.Error(), "socket buffer size not set correctly") {
		t.Fatalf("unexpected bidir failure (not the kernel refusing the window): %v", err)
	}
	check(t, err.Error())
}
