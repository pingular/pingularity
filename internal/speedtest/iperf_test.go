package speedtest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

func TestParseIperfServer(t *testing.T) {
	cases := []struct {
		in         string
		host, port string
		wantErr    bool
	}{
		{"10.0.0.5", "10.0.0.5", "", false},
		{"  iperf.example.com  ", "iperf.example.com", "", false}, // trimmed
		{"10.0.0.5:5202", "10.0.0.5", "5202", false},
		{"[2001:db8::1]:5201", "2001:db8::1", "5201", false}, // bracketed IPv6
		{"2001:db8::1", "2001:db8::1", "", false},            // bare IPv6, no port (iperf3 defaults 5201)
		{"[2001:db8::1]", "2001:db8::1", "", false},          // bracketed IPv6, no port - unwrapped
		{"[not-an-ip]", "", "", true},                        // brackets around a non-literal
		{"[2001:db8::1", "", "", true},                       // unclosed bracket
		{"", "", "", true},                                   // empty
		{"   ", "", "", true},                                // whitespace only
		{"-R", "", "", true},                                 // flag-injection: bare flag as host
		{"-host:5201", "", "", true},                         // flag-injection: host starts with '-'
		{"host:-1", "", "", true},                            // flag-injection: port starts with '-'
		{"host --logfile /tmp/x", "", "", true},              // pasted command tail (whitespace in host)
		{"host:5201 --logfile=/tmp/x", "", "", true},         // junk after the port
		{"host:abc", "", "", true},                           // non-numeric port
		{"127.0.0.1:15201:9", "", "", true},                  // multi-colon garbage: colon-y host that isn't an IPv6 literal
		{"10.0.0.5:99999", "", "", true},                     // port out of range
		{"host\nx", "", "", true},                            // newline in host
		{"host\rx", "", "", true},                            // carriage return in host
	}
	for _, c := range cases {
		host, port, err := parseIperfServer(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseIperfServer(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if host != c.host || port != c.port {
			t.Errorf("parseIperfServer(%q) = (%q,%q), want (%q,%q)", c.in, host, port, c.host, c.port)
		}
	}
}

// A representative `iperf3 -J` body (trimmed to the fields we read). For a
// forward (upload) transfer, sum_sent is what the client wrote to the socket and
// sum_received is what the server actually got back-reported (the gap is the
// send-buffer occupancy at test end); mean_rtt is microseconds.
const iperfSampleJSON = `{
  "end": {
    "streams": [{ "sender": { "min_rtt": 18250, "mean_rtt": 41000, "max_rtt": 95000 } }],
    "sum_sent":     { "bytes": 125000000, "bits_per_second": 942000000 },
    "sum_received": { "bytes": 118000000, "bits_per_second": 905000000 }
  }
}`

func TestIperfJSONParse(t *testing.T) {
	var j iperfJSON
	if err := json.Unmarshal([]byte(iperfSampleJSON), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.Error != "" {
		t.Fatalf("unexpected error field: %q", j.Error)
	}
	// Sender-side aggregate (bytes written to the socket).
	if got := j.End.SumSent.BitsPerSecond / 1e6; got != 942 {
		t.Errorf("sum_sent Mbps = %v, want 942", got)
	}
	if j.End.SumSent.Bytes != 125000000 {
		t.Errorf("sum_sent bytes = %d, want 125000000", j.End.SumSent.Bytes)
	}
	// Receiver-side aggregate: download reads it directly, upload via iperfUploadSum.
	if got := j.End.SumReceived.BitsPerSecond / 1e6; got != 905 {
		t.Errorf("sum_received Mbps = %v, want 905", got)
	}
	// Ping is the UNLOADED rtt: min_rtt 18250 µs -> 18.25 ms (NOT mean_rtt 41 ms,
	// which is the loaded average and would carry bufferbloat).
	if got := iperfStreamRTT(j); got != 18.25 {
		t.Errorf("ping rtt ms = %v, want 18.25 (min_rtt, not mean_rtt)", got)
	}
}

// Upload throughput must read the RECEIVER side (sum_received, what the server
// actually got): sum_sent also counts bytes still sitting in the send buffer
// when the clock stops, inflating short tests on slow uplinks. Falls back to
// the sender side when the server reported no received aggregate.
func TestIperfUploadSum(t *testing.T) {
	var j iperfJSON
	if err := json.Unmarshal([]byte(iperfSampleJSON), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := iperfUploadSum(j); got.BitsPerSecond/1e6 != 905 || got.Bytes != 118000000 {
		t.Errorf("upload sum = %+v, want the receiver side (905 Mbps / 118000000 bytes)", got)
	}
	j.End.SumReceived = iperfSum{} // server didn't report -> sender-side fallback
	if got := iperfUploadSum(j); got.BitsPerSecond/1e6 != 942 || got.Bytes != 125000000 {
		t.Errorf("fallback upload sum = %+v, want the sender side (942 Mbps / 125000000 bytes)", got)
	}
}

func TestIperfJSONError(t *testing.T) {
	// iperf3 reports a connection failure as an {"error": ...} body on stdout.
	var j iperfJSON
	if err := json.Unmarshal([]byte(`{"error":"unable to connect to server: Connection refused"}`), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.Error == "" {
		t.Fatal("expected error field to be populated")
	}
}

// TestSchedulerEngineSelection verifies curTester honors TesterFn: the engine the
// hook returns is used, and a nil hook (or one returning nil) falls back to the
// base Ookla tester. This is the seam main.go wires to switch ookla<->iperf3 live.
func TestSchedulerEngineSelection(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	base := testerFunc(func(context.Context) (Result, error) { return Result{Engine: "ookla"}, nil })
	alt := testerFunc(func(context.Context) (Result, error) { return Result{Engine: "iperf3"}, nil })
	s := NewScheduler(base, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// No hook -> base tester.
	if got, _ := s.curTester().Run(context.Background()); got.Engine != "ookla" {
		t.Errorf("nil TesterFn: engine=%q, want ookla", got.Engine)
	}
	// Hook returns nil -> fall back to base.
	s.TesterFn = func() Tester { return nil }
	if got, _ := s.curTester().Run(context.Background()); got.Engine != "ookla" {
		t.Errorf("nil-returning TesterFn: engine=%q, want ookla", got.Engine)
	}
	// Hook returns the alt tester -> alt used.
	useAlt := true
	s.TesterFn = func() Tester {
		if useAlt {
			return alt
		}
		return base
	}
	if got, _ := s.curTester().Run(context.Background()); got.Engine != "iperf3" {
		t.Errorf("alt TesterFn: engine=%q, want iperf3", got.Engine)
	}
	useAlt = false
	if got, _ := s.curTester().Run(context.Background()); got.Engine != "ookla" {
		t.Errorf("switched-back TesterFn: engine=%q, want ookla", got.Engine)
	}
}

func TestIperfUDPParse(t *testing.T) {
	// Loss + jitter live in end.sum (UDP), not sum_sent/sum_received.
	const body = `{"end":{"sum":{"lost_percent":1.5,"jitter_ms":2.3,"packets":1000,"lost_packets":15}}}`
	var j iperfJSON
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if j.End.Sum.LostPercent != 1.5 || j.End.Sum.JitterMS != 2.3 || j.End.Sum.Packets != 1000 {
		t.Errorf("got loss=%v jitter=%v packets=%d", j.End.Sum.LostPercent, j.End.Sum.JitterMS, j.End.Sum.Packets)
	}
}

func TestUDPRate(t *testing.T) {
	cases := []struct {
		down, up float64
		want     int
	}{
		{1000, 0, iperfUDPMaxRate}, // half of 1000 capped to the ceiling
		{40, 0, 20},                // half of measured download
		{0, 30, 15},                // no download -> fall back to upload
		{0, 0, iperfUDPMinRate},    // nothing measured -> floor
		{20, 0, 10},                // half; floor doesn't bind
		{8, 0, iperfUDPMinRate},    // half (4) < floor, capacity clears it -> floor
		{6, 0, 3},                  // half (3), floor (5) would be >0.8*capacity -> stay at half
		{3, 0, 1},                  // slow link: sample below capacity, NOT the 5 Mbps floor
		{2.8, 0, 1},                // rural DSL: probe stays under the pipe
		{2, 0, 1},                  // half rounds to 1, below capacity
		{0.5, 0, 1},                // near-zero but positive: minimal probe, still <floor
	}
	for _, c := range cases {
		if got := udpRate(c.down, c.up); got != c.want {
			t.Errorf("udpRate(%v,%v) = %d, want %d", c.down, c.up, got, c.want)
		}
	}
}

func TestIperfToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"cubic", "cubic"},
		{"  bbr  ", "bbr"}, // trimmed
		{"ef", "ef"},
		{"", ""},                      // empty
		{"-C", ""},                    // flag injection
		{"a b", ""},                   // embedded whitespace
		{strings.Repeat("x", 40), ""}, // absurd length
	}
	for _, c := range cases {
		if got := iperfToken(func() string { return c.in }); got != c.want {
			t.Errorf("iperfToken(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := iperfToken(nil); got != "" {
		t.Errorf("nil -> %q, want \"\"", got)
	}
}

func TestIperfMSSClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, 0},
		{func() int { return 0 }, 0},
		{func() int { return -5 }, 0}, // negative -> 0 (auto)
		{func() int { return 1400 }, 1400},
		{func() int { return 99999 }, iperfMaxMSS}, // over ceiling
	}
	for _, c := range cases {
		if got := iperfMSS(c.fn); got != c.want {
			t.Errorf("iperfMSS() = %d, want %d", got, c.want)
		}
	}
}

// iperfArgs emits the advanced flags only when set, with their values.
func TestIperfAdvancedArgs(t *testing.T) {
	join := func(a []string) string { return strings.Join(a, " ") }
	bare := join(iperfArgs("h", "", iperfTunables{dur: 5, streams: 1}, iperfAuth{}, modeForward))
	for _, f := range []string{"--congestion", "--set-mss", "--no-delay", "--dscp"} {
		if strings.Contains(bare, f) {
			t.Errorf("bare args should not contain %q: %s", f, bare)
		}
	}
	tp := iperfTunables{dur: 5, streams: 1, congestion: "bbr", mss: 1400, noDelay: true, dscp: "ef"}
	got := join(iperfArgs("h", "", tp, iperfAuth{}, modeForward))
	for _, want := range []string{"--congestion bbr", "--set-mss 1400", "--no-delay", "--dscp ef"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
}

// --use-pkcs1-padding rides only with authentication and only when the flag is set.
func TestIperfPKCS1Args(t *testing.T) {
	join := func(a []string) string { return strings.Join(a, " ") }
	tp := iperfTunables{dur: 5, streams: 1}
	// pkcs1 set but no auth -> not emitted (the flag only affects the auth handshake)
	if got := join(iperfArgs("h", "", tp, iperfAuth{pkcs1: true}, modeForward)); strings.Contains(got, "--use-pkcs1-padding") {
		t.Errorf("pkcs1 without auth should not emit the flag: %s", got)
	}
	// auth on, pkcs1 off -> not emitted
	auth := iperfAuth{username: "bob", keyPath: "/tmp/k.pem"}
	if got := join(iperfArgs("h", "", tp, auth, modeForward)); strings.Contains(got, "--use-pkcs1-padding") {
		t.Errorf("auth without pkcs1 should not emit the flag: %s", got)
	}
	// auth on + pkcs1 on -> emitted
	auth.pkcs1 = true
	if got := join(iperfArgs("h", "", tp, auth, modeForward)); !strings.Contains(got, "--use-pkcs1-padding") {
		t.Errorf("auth+pkcs1 should emit the flag: %s", got)
	}
}

func TestAvailableCongestionControl(t *testing.T) {
	got := AvailableCongestionControl()
	if runtime.GOOS != "linux" && runtime.GOOS != "freebsd" {
		// macOS/Windows iperf3 has no -C option: offering ANY algorithm there
		// just advertises a value that aborts every run, so the list is empty.
		// (FreeBSD DOES support -C and enumerates via sysctl - see
		// TestParseFreeBSDCC, which exercises that parser independent of GOOS.)
		if len(got) != 0 {
			t.Errorf("macOS/Windows should offer nothing (setting -C aborts the run), got %v", got)
		}
		return
	}
	if runtime.GOOS == "freebsd" {
		return // live sysctl output varies by release; parser covered separately
	}
	// On Linux the sysctl (when present) lists >=1 algorithm (cubic/reno at minimum);
	// a sandbox without /proc yields nil, never a panic.
	if _, err := os.Stat("/proc/sys/net/ipv4/tcp_allowed_congestion_control"); err == nil {
		if len(got) == 0 {
			t.Error("sysctl present but no algorithms parsed")
		}
	} else if got != nil {
		t.Errorf("sysctl absent but got %v, want nil", got)
	}
}

// iperfServerName is the display name for both the live status and the recorded run:
// "iperf3: <label>" (engine + friendly name, no address), falling back to the host.
func TestIperfServerName(t *testing.T) {
	cases := []struct {
		label, host, want string
	}{
		{"NAS", "10.0.0.5", "iperf3: NAS"},
		{"  VPS  ", "vps.example.com", "iperf3: VPS"}, // label trimmed, no address
		{"", "10.0.0.5", "iperf3: 10.0.0.5"},          // no label -> host
		{"   ", "10.0.0.5", "iperf3: 10.0.0.5"},       // blank label -> host
	}
	for _, c := range cases {
		if got := iperfServerName(c.label, c.host); got != c.want {
			t.Errorf("iperfServerName(%q,%q) = %q, want %q", c.label, c.host, got, c.want)
		}
	}
}

// measureServerRTT must return a real (non-nil, small) RTT against a reachable
// listener, and nil when nothing accepts - so Run falls back to min_rtt/idle.
func TestMeasureServerRTT(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { // accept and immediately close, like an iperf3 server awaiting a cookie
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	rtt := measureServerRTT(context.Background(), "127.0.0.1", port, iperfTunables{})
	if rtt == nil {
		t.Fatal("reachable listener returned nil RTT")
	}
	if *rtt < 0 || *rtt > 1000 {
		t.Errorf("loopback RTT = %v ms, want a small non-negative value", *rtt)
	}

	// Nothing listening on the closed port -> nil (caller falls back).
	ln.Close()
	if rtt := measureServerRTT(context.Background(), "127.0.0.1", port, iperfTunables{}); rtt != nil {
		t.Errorf("unreachable server returned %v, want nil", *rtt)
	}
}

// CheckIperfServer connects to a reachable listener (returning an RTT) and errors on a
// closed port or a malformed address - the three states the UI status light renders.
func TestCheckIperfServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	if rtt, err := CheckIperfServer(context.Background(), "127.0.0.1:"+port); err != nil || rtt < 0 {
		t.Errorf("reachable: rtt=%v err=%v, want non-negative rtt, nil err", rtt, err)
	}
	if _, err := CheckIperfServer(context.Background(), "-bad"); err == nil {
		t.Error("malformed address should error")
	}
	ln.Close()
	if _, err := CheckIperfServer(context.Background(), "127.0.0.1:"+port); err == nil {
		t.Error("closed port should error (unreachable)")
	}
}

func TestResolveAuth(t *testing.T) {
	// auth off -> no auth.
	if a, c, err := (&Iperf{AuthFn: func() bool { return false }}).resolveAuth(); err != nil || a.on() {
		t.Errorf("auth off: on=%v err=%v", a.on(), err)
		c()
	} else {
		c()
	}
	// enabled but incomplete (no password) -> no auth.
	it := &Iperf{AuthFn: func() bool { return true }, UsernameFn: func() string { return "bob" },
		PasswordFn: func() string { return "" }, RSAKeyFn: func() string { return "k" }}
	if a, c, _ := it.resolveAuth(); a.on() {
		t.Error("incomplete creds should not authenticate")
		c()
	} else {
		c()
	}
	// complete -> writes the PEM to a temp file; cleanup removes it.
	const pem = "-----BEGIN PUBLIC KEY-----\nABC\n-----END PUBLIC KEY-----"
	it = &Iperf{AuthFn: func() bool { return true }, UsernameFn: func() string { return "bob" },
		PasswordFn: func() string { return "pw" }, RSAKeyFn: func() string { return pem }}
	a, cleanup, err := it.resolveAuth()
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if !a.on() || a.username != "bob" || a.password != "pw" {
		t.Errorf("auth = %+v", a)
	}
	if b, err := os.ReadFile(a.keyPath); err != nil || string(b) != pem {
		t.Errorf("temp key content = %q (err %v)", b, err)
	}
	cleanup()
	if _, err := os.Stat(a.keyPath); !os.IsNotExist(err) {
		t.Error("cleanup did not remove the temp key file")
	}
}

func TestSpeedDirection(t *testing.T) {
	cases := map[string]string{"both": "both", "down": "down", "up": "up", "bidir": "bidir", "": "both", "bogus": "both"}
	for in, want := range cases {
		if got := speedDirection(func() string { return in }); got != want {
			t.Errorf("speedDirection(%q) = %q, want %q", in, got, want)
		}
	}
	if got := speedDirection(nil); got != "both" {
		t.Errorf("nil -> %q, want both", got)
	}
}

func TestIperfWindowClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, 0},
		{func() int { return 0 }, 0},
		{func() int { return -8 }, 0}, // negative -> 0 (auto)
		{func() int { return 512 }, 512},
		{func() int { return 1 << 20 }, iperfMaxWindow}, // over ceiling
	}
	for _, c := range cases {
		if got := iperfWindow(c.fn); got != c.want {
			t.Errorf("iperfWindow() = %d, want %d", got, c.want)
		}
	}
}

func TestIperfIPVersion(t *testing.T) {
	cases := map[string]string{"4": "4", "6": "6", "auto": "auto", "": "auto", "v4": "auto", "9": "auto"}
	for in, want := range cases {
		if got := iperfIPVersion(func() string { return in }); got != want {
			t.Errorf("iperfIPVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if got := iperfIPVersion(nil); got != "auto" {
		t.Errorf("nil -> %q, want auto", got)
	}
}

// TestIperfBidirParse verifies the --bidir JSON split: the forward flow (client
// upload) reads the server's received aggregate (sum_received, sender-side
// fallback); the reverse flow (download) reads sum_received_bidir_reverse.
func TestIperfBidirParse(t *testing.T) {
	const body = `{"end":{
		"streams":[{"sender":{"min_rtt":12000,"mean_rtt":33000}}],
		"sum_sent":                  {"bytes":200000000,"bits_per_second":800000000},
		"sum_received":              {"bytes":199000000,"bits_per_second":796000000},
		"sum_sent_bidir_reverse":    {"bytes":510000000,"bits_per_second":2040000000},
		"sum_received_bidir_reverse":{"bytes":505000000,"bits_per_second":2020000000}
	}}`
	var j iperfJSON
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rtt := iperfStreamRTT(j); rtt != 12 { // unloaded min_rtt 12000 µs, not mean 33 ms
		t.Errorf("ping rtt ms = %v, want 12 (min_rtt)", rtt)
	}
	if up := iperfUploadSum(j).BitsPerSecond / 1e6; up != 796 {
		t.Errorf("upload Mbps = %v, want 796 (forward flow, receiver side)", up)
	}
	if dn := j.End.SumReceivedBidirReverse.BitsPerSecond / 1e6; dn != 2020 {
		t.Errorf("download Mbps = %v, want 2020 (sum_received_bidir_reverse)", dn)
	}
	if j.End.SumReceivedBidirReverse.Bytes != 505000000 {
		t.Errorf("download bytes = %d, want 505000000", j.End.SumReceivedBidirReverse.Bytes)
	}
}

func TestIperfStreamsClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, 1},
		{func() int { return 0 }, 1},  // non-positive -> 1
		{func() int { return -3 }, 1}, // negative -> 1
		{func() int { return 4 }, 4},  // in range
		{func() int { return 99 }, iperfMaxStreams},
	}
	for _, c := range cases {
		if got := iperfStreams(c.fn); got != c.want {
			t.Errorf("iperfStreams() = %d, want %d", got, c.want)
		}
	}
}

// The tester's ceiling and the one the rest of the product enforces must be the
// same number. They were 8 and 32: settings accepted a configured 12, the API
// echoed it and the dashboard offered it, and then --parallel got 8 - a control
// that reads as working and does nothing.
func TestIperfStreamCeilingMatchesSettings(t *testing.T) {
	if iperfMaxStreams != settings.MaxIperfStreams {
		t.Fatalf("tester ceiling %d != settings ceiling %d: values in between are accepted and then dropped",
			iperfMaxStreams, settings.MaxIperfStreams)
	}
	for _, n := range []int{9, 12, 16, settings.MaxIperfStreams} {
		if got := iperfStreams(func() int { return n }); got != n {
			t.Errorf("iperfStreams(%d) = %d, want %d - the configured count never reaches --parallel", n, got, n)
		}
	}
}

func TestIperfOmitClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, iperfDefaultOmit},
		{func() int { return 0 }, 0}, // 0 is valid (off)
		{func() int { return 3 }, 3},
		{func() int { return -1 }, 0}, // negative -> 0
		{func() int { return 99 }, iperfMaxOmit},
	}
	for _, c := range cases {
		if got := iperfOmit(c.fn); got != c.want {
			t.Errorf("iperfOmit() = %d, want %d", got, c.want)
		}
	}
}

// iperfArgs appends --parallel only when >1, --omit only when >0, --window only
// when >0, -4/-6 only when pinned, and the right direction flag for the mode.
func TestIperfArgs(t *testing.T) {
	join := func(a []string) string { return strings.Join(a, " ") }
	// defaults: single stream, no omit/window, auto IP, forward upload
	if got := join(iperfArgs("h", "5201", iperfTunables{dur: 5, streams: 1, omit: 0}, iperfAuth{}, modeForward)); strings.Contains(got, "--parallel") || strings.Contains(got, "--omit") || strings.Contains(got, "--window") || strings.Contains(got, "--reverse") || strings.Contains(got, "--bidir") || strings.Contains(got, "--bind") || strings.Contains(got, "--username") || strings.Contains(got, " -4") || strings.Contains(got, " -6") {
		t.Errorf("default args should be bare: %s", got)
	}
	tp := iperfTunables{dur: 10, streams: 4, omit: 2, window: 512, bind: "10.0.0.1", ipver: "6"}
	got := join(iperfArgs("h", "5201", tp, iperfAuth{username: "bob", keyPath: "/tmp/k.pem"}, modeReverse))
	for _, want := range []string{"--time 10", "--parallel 4", "--omit 2", "--window 512K", "-6", "--reverse", "--port 5201", "--bind 10.0.0.1", "--username bob", "--rsa-public-key-path /tmp/k.pem"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q missing %q", got, want)
		}
	}
	// IPv4 pin emits -4 (not -6); bidir emits --bidir (not --reverse).
	got = join(iperfArgs("h", "", iperfTunables{dur: 5, streams: 1, ipver: "4"}, iperfAuth{}, modeBidir))
	if !strings.Contains(got, "-4") || strings.Contains(got, "-6") {
		t.Errorf("ipv4 pin args = %q", got)
	}
	if !strings.Contains(got, "--bidir") || strings.Contains(got, "--reverse") {
		t.Errorf("bidir args = %q", got)
	}
}

func TestIperfDurClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, iperfDefaultDur},
		{func() int { return 0 }, iperfDefaultDur},  // 0 -> default
		{func() int { return -5 }, iperfDefaultDur}, // negative -> default (then clamped, but default wins)
		{func() int { return 3 }, 3},                // in range
		{func() int { return 999 }, iperfMaxDur},    // over max
		{func() int { return 1 }, iperfMinDur},      // at min
	}
	for _, c := range cases {
		if got := iperfDur(c.fn); got != c.want {
			t.Errorf("iperfDur() = %d, want %d", got, c.want)
		}
	}
}

func TestSpeedRetriesClamp(t *testing.T) {
	cases := []struct {
		fn   func() int
		want int
	}{
		{nil, speedDefaultRetries},
		{func() int { return 0 }, 0},  // explicit 0 -> no retry
		{func() int { return 2 }, 2},  // in range
		{func() int { return -1 }, 0}, // negative -> 0
		{func() int { return 99 }, speedMaxRetries},
	}
	for _, c := range cases {
		if got := speedRetries(c.fn); got != c.want {
			t.Errorf("speedRetries() = %d, want %d", got, c.want)
		}
	}
}

func TestIsTransientIperfErr(t *testing.T) {
	transient := []string{
		"the server is busy running a test. Try again later",
		"unable to connect to server: Connection refused",
		"read tcp 10.0.0.1: connection reset by peer",
		"dial tcp: i/o timeout",
		"write: broken pipe",
	}
	for _, s := range transient {
		if !isTransientIperfErr(errors.New(s)) {
			t.Errorf("expected transient: %q", s)
		}
	}
	hard := []string{
		"test authorization failed", // bad creds - retry won't help
		`invalid iperf3 server "x"`,
		"unable to read public key", // our own pre-flight, not transient
		"no data transferred",       // clean exit-0, zero bytes: paid at full duration, rarely self-heals
		"",
	}
	for _, s := range hard {
		if isTransientIperfErr(errors.New(s)) {
			t.Errorf("expected NOT transient: %q", s)
		}
	}
	if isTransientIperfErr(nil) {
		t.Error("nil must not be transient")
	}
}

// A watchdog kill must not surface as an opaque OS-specific failure: when OUR per-run
// deadline fired (and the caller's ctx is still live) the error is rewritten to name the
// stall - detected structurally from the contexts, with no dependence on Unix signal
// text. A caller cancellation, or any error without a fired deadline, passes through.
func TestStalledErr(t *testing.T) {
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()
	live := context.Background()
	gone, cancelGone := context.WithCancel(context.Background())
	cancelGone()
	killed := errors.New("signal: killed")

	if err := stalledErr(killed, expired, live, 8*time.Second); err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("deadline kill = %v, want a 'transfer stalled' rewrite", err)
	}
	// Windows-shaped kill: no "signal: killed" text, but the fired deadline still
	// identifies the stall structurally.
	winKill := errors.New("exit status 1")
	if err := stalledErr(winKill, expired, live, 8*time.Second); err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Errorf("non-signal deadline kill = %v, want a 'transfer stalled' rewrite", err)
	}
	if err := stalledErr(killed, expired, gone, 8*time.Second); err != killed {
		t.Errorf("caller-cancelled kill = %v, want the original error", err)
	}
	// An error with no fired per-run deadline (rctx still live) passes through.
	boom := errors.New("boom")
	if err := stalledErr(boom, live, live, 8*time.Second); err != boom {
		t.Errorf("error without a fired deadline = %v, want passthrough", err)
	}
	if err := stalledErr(nil, expired, live, 8*time.Second); err != nil {
		t.Errorf("nil error = %v, want nil", err)
	}
}

// TestIperfExecErr checks that a normal non-zero exit surfaces its stderr text, while a
// watchdog kill (signalled by the fired per-run deadline, not an exit code/text) keeps
// the raw exec error so stalledErr can still detect the stalled transfer even when the
// process had written a stderr warning first.
func TestIperfExecErr(t *testing.T) {
	// Normal non-zero exit (deadline not hit): stderr replaces the opaque "exit status N".
	cmd := exec.Command("sh", "-c", "echo 'bad flag' >&2; exit 1")
	out, err := cmd.Output()
	if _, got := iperfExecErr(out, err, false); got == nil || got.Error() != "bad flag" {
		t.Fatalf("non-zero exit = %v, want 'bad flag'", got)
	}

	// Watchdog kill with a prior stderr warning: the fired deadline suppresses the
	// stderr substitution, so the stale warning does NOT become the error and
	// stalledErr rewrites the kill to a stall.
	rctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	killed := exec.CommandContext(rctx, "sh", "-c", "echo warning >&2; exec sleep 10")
	out, err = killed.Output()
	deadlineHit := errors.Is(rctx.Err(), context.DeadlineExceeded)
	if !deadlineHit {
		t.Fatalf("expected the per-run deadline to have fired, rctx.Err()=%v", rctx.Err())
	}
	_, got := iperfExecErr(out, err, deadlineHit)
	if got == nil || strings.Contains(got.Error(), "warning") {
		t.Fatalf("watchdog kill = %v, want the stale stderr warning suppressed", got)
	}
	if e := stalledErr(got, rctx, context.Background(), 8*time.Second); e == nil || !strings.Contains(e.Error(), "stalled") {
		t.Fatalf("stalledErr after kill = %v, want a 'transfer stalled' rewrite", e)
	}
}

func TestWithRetry(t *testing.T) {
	old := iperfRetryDelay
	iperfRetryDelay = time.Millisecond // keep the test fast
	defer func() { iperfRetryDelay = old }()

	t.Run("transient then succeeds within the cap", func(t *testing.T) {
		n := 0
		err := withRetry(context.Background(), 3, func() error {
			n++
			if n < 3 {
				return errors.New("the server is busy")
			}
			return nil
		})
		if err != nil || n != 3 {
			t.Errorf("err=%v attempts=%d, want nil/3", err, n)
		}
	})
	t.Run("gives up at 1+retries on a persistent transient", func(t *testing.T) {
		n := 0
		err := withRetry(context.Background(), 2, func() error { n++; return errors.New("connection reset by peer") })
		if err == nil || n != 3 { // 1 initial + 2 retries
			t.Errorf("err=%v attempts=%d, want err/3", err, n)
		}
	})
	t.Run("non-transient fails fast, no retry", func(t *testing.T) {
		n := 0
		err := withRetry(context.Background(), 3, func() error { n++; return errors.New("test authorization failed") })
		if err == nil || n != 1 {
			t.Errorf("err=%v attempts=%d, want err/1", err, n)
		}
	})
	t.Run("retries=0 means a single attempt", func(t *testing.T) {
		n := 0
		_ = withRetry(context.Background(), 0, func() error { n++; return errors.New("busy") })
		if n != 1 {
			t.Errorf("attempts=%d, want 1", n)
		}
	})
	t.Run("cancelled ctx stops further retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		n := 0
		_ = withRetry(ctx, 3, func() error { n++; return errors.New("busy") })
		if n != 1 { // attempt once, then ctx.Err() breaks the loop
			t.Errorf("cancelled ctx attempts=%d, want 1", n)
		}
	})
}

// withRetryPred gates retries on its predicate (the Ookla engine reuses the loop
// with its own notion of a transient error).
func TestWithRetryPred(t *testing.T) {
	old := iperfRetryDelay
	iperfRetryDelay = 0
	defer func() { iperfRetryDelay = old }()
	n := 0
	_ = withRetryPred(context.Background(), 3, func(error) bool { return false }, func() error { n++; return errors.New("x") })
	if n != 1 {
		t.Errorf("predicate false: attempts=%d, want 1 (no retry)", n)
	}
	n = 0
	_ = withRetryPred(context.Background(), 2, func(error) bool { return true }, func() error { n++; return errors.New("x") })
	if n != 3 {
		t.Errorf("predicate true: attempts=%d, want 3 (1 + 2 retries)", n)
	}
}

// Zone-scoped IPv6 literals are valid iperf3 servers (a LAN box on link-local):
// the colon-garbage rule must not reject them.
func TestParseIperfServerZonedIPv6(t *testing.T) {
	for _, in := range []string{"fe80::1%eth0", "[fe80::1%eth0]:5201"} {
		if _, _, err := parseIperfServer(in); err != nil {
			t.Errorf("parseIperfServer(%q): unexpected error %v", in, err)
		}
	}
	if _, _, err := parseIperfServer("1.2.3.4:5201:9"); err == nil {
		t.Error("multi-colon garbage must still be rejected")
	}
}

// TestParseFreeBSDCC pins the net.inet.tcp.cc.available parser against BOTH
// release shapes. The 14.x table form regressed a prior strings.Fields parser
// into feeding "CCmod"/"D"/"*"/"0" to iperf3 as algorithm names.
func TestParseFreeBSDCC(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"13.x comma line", "newreno, cubic, htcp\n", []string{"newreno", "cubic", "htcp"}},
		{"13.x no trailing newline", "newreno, cubic", []string{"newreno", "cubic"}},
		{"single algorithm", "newreno", []string{"newreno"}},
		{
			"14.x table with header and default marker",
			"CCmod       D PCB\nnewreno       0\ncubic     *   0\nhtcp          0\n",
			[]string{"newreno", "cubic", "htcp"},
		},
		{
			"14.x table, default marker glued to name column",
			"CC\n*cubic\nnewreno\n",
			[]string{"cubic", "newreno"},
		},
		{"empty", "", nil},
		{"whitespace only", "   \n\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseFreeBSDCC(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
			// No parsed token may contain table cruft.
			for _, g := range got {
				if !isCCName(g) {
					t.Errorf("parsed %q is not a valid algorithm name", g)
				}
			}
		})
	}
}

// Parser-only: pins that table noise never leaks (see TestAvailableCongestionControlFreeBSD
// for the reader+branch composition through the ccSysctl seam).
func TestParseFreeBSDCC_DropsAllTableNoise(t *testing.T) {
	got := parseFreeBSDCC("CCmod   D PCB\nnewreno  *  1\ncubic       0\n")
	for _, bad := range []string{"CCmod", "D", "PCB", "*", "1", "0"} {
		for _, g := range got {
			if g == bad {
				t.Errorf("table noise %q leaked into %v", bad, got)
			}
		}
	}
	if len(got) != 2 {
		t.Fatalf("want 2 algorithms, got %v", got)
	}
}

// TestAvailableCongestionControlFreeBSD actually drives the FreeBSD branch
// (reader + table parser) through the injectable ccSysctl seam, on any host - the
// coverage the "injected" comment previously claimed but never delivered.
// Disconnecting the freebsd case now fails here instead of leaving the suite green.
func TestAvailableCongestionControlFreeBSD(t *testing.T) {
	orig := ccSysctl
	defer func() { ccSysctl = orig }()

	// 14.x table shape, via the seam -> parsed algorithm names, noise dropped.
	ccSysctl = func() ([]byte, error) {
		return []byte("CCmod   D PCB\nnewreno  *  1\ncubic       0\nhtcp        0\n"), nil
	}
	got := availableCongestionControlFor("freebsd")
	want := []string{"newreno", "cubic", "htcp"}
	if len(got) != len(want) {
		t.Fatalf("freebsd branch: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("freebsd branch: got %v, want %v", got, want)
		}
	}

	// A read failure yields no dropdown, not a crash.
	ccSysctl = func() ([]byte, error) { return nil, errors.New("sysctl unavailable") }
	if got := availableCongestionControlFor("freebsd"); got != nil {
		t.Errorf("read error: got %v, want nil", got)
	}

	// macOS/Windows offer nothing (the -C option aborts there).
	if got := availableCongestionControlFor("darwin"); got != nil {
		t.Errorf("darwin: got %v, want nil", got)
	}
}
