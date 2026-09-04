package speedtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/util"
)

// Iperf is a Tester that runs the iperf3 CLI (with -J for JSON) against a server
// you run yourself (`iperf3 -s` on a LAN box, homelab, or VPS). Unlike Ookla there
// is no server network; it covers what Ookla can't: LAN/internal links and accurate
// upload.
//
// Bufferbloat reuses the shared LUL sampler (latency to an anchor during each
// transfer), so a LAN test reads ~0 bloat - saturating a local link doesn't congest
// the internet path the sampler probes. TCP can't measure jitter/loss (the UDP pass
// does that), so those stay nil unless UDP runs.
type Iperf struct {
	// Log records the failures this engine otherwise swallows: a direction that
	// failed while the other survived, and the UDP pass. Both leave a usable
	// result behind, so neither reaches the scheduler's error path or the
	// speed.fail counters - without a line here they leave no trace at all.
	// Optional: nil logs nothing.
	Log *slog.Logger
	// EnvHint, when set, maps a transfer/UDP-pass failure's text to an
	// environment-specific explanation appended to the surfaced error or warn
	// line (main.go injects a container-networking matcher when it knows it runs
	// in one; the engine itself stays environment-blind). Text only - it never
	// changes behavior or retry classification. An empty return appends nothing.
	EnvHint func(errText string) string
	// congestionSkipOnce coalesces the "-C ignored" warning so it logs once per
	// Iperf, not once per transfer.
	congestionSkipOnce sync.Once
	ServerFn           func() string // "host" or "host:port" - the user's iperf3 server
	LabelFn            func() string // friendly name for the active server; "" -> fall back to the host
	// OnServer, if set, is called with the display name ("iperf3: <label>", falling back
	// to the host when unlabelled) once it's known, so the UI can show which server the
	// running test is hitting. The recorded Result.Server uses the same name - see Run.
	OnServer    func(server string)
	DurationFn  func() int    // seconds per direction (clamped); nil -> default
	StreamsFn   func() int    // parallel TCP streams (clamped); nil -> 1
	OmitFn      func() int    // warm-up seconds discarded (clamped); nil -> default
	DirectionFn func() string // "both"|"down"|"up"|"bidir"; nil -> both
	UDPFn       func() bool   // run the UDP loss/jitter pass; nil -> true
	UDPRateFn   func() int    // UDP probe Mbps; <=0 -> auto
	BindFn      func() string // source address/interface (--bind); "" -> default route
	WindowFn    func() int    // TCP window / socket-buffer size in KB (-w); <=0 -> auto
	IPVersionFn func() string // "auto"|"4"|"6" force -4/-6; nil/"auto" -> let iperf3 choose
	RetriesFn   func() int    // extra attempts per direction on a transient failure; nil -> default
	// Advanced per-transfer knobs; each is omitted when blank/zero so iperf3 keeps its
	// default. Congestion/MSS/NoDelay are TCP-only; DSCP marks both the TCP and UDP probes.
	CongestionFn func() string // -C TCP congestion algorithm (cubic|bbr|reno|...); "" -> system default
	NoDelayFn    func() bool   // -N disable Nagle's algorithm; nil/false -> leave Nagle on
	DSCPFn       func() string // --dscp IP DiffServ value (0-63 or symbolic, e.g. ef, cs5); "" -> none
	MSSFn        func() int    // -M TCP max segment size in bytes; <=0 -> auto
	// RSA auth: when AuthFn is on, all fields below are required - a missing one
	// fails the run closed rather than falling back to unauthenticated (see resolveAuth).
	AuthFn     func() bool   // authentication enabled
	UsernameFn func() string // --username
	PasswordFn func() string // IPERF3_PASSWORD env
	RSAKeyFn   func() string // server's RSA public key PEM
	PKCS1Fn    func() bool   // force --use-pkcs1-padding (legacy/unpatched iperf3 servers)
}

// IperfAvailable reports whether the iperf3 binary is on PATH. When it isn't, the UI
// greys out the engine and the runtime never picks it. NOTE: presence on PATH is NOT
// the same as capability - see IperfVersion; an old or feature-limited build is on
// PATH yet cannot complete Pingularity's command line.
func IperfAvailable() bool { _, err := exec.LookPath("iperf3"); return err == nil }

var (
	iperfVerOnce sync.Once
	iperfVerStr  string
)

// IperfVersion returns the local iperf3's version (e.g. "3.20"), or "" if the binary
// is absent or its --version output can't be parsed. Cached once. This exists because
// Pingularity's feature surface depends on the build, not just its presence: the
// unambiguous bidir reverse field arrived in 3.11, authentication padding changed in
// 3.17 and its CLI role changed again in 3.20, and --connect-timeout/DSCP need newer
// builds - so the UI/diagnostics can surface the real version and warn instead of
// presenting an unusable binary as capable.
func IperfVersion() string {
	iperfVerOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "iperf3", "--version").CombinedOutput()
		if err != nil {
			return
		}
		line := string(out) // first line is "iperf 3.20 (cJSON 1.7.15)"
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}
		if f := strings.Fields(line); len(f) >= 2 && strings.EqualFold(f[0], "iperf") {
			iperfVerStr = f[1]
		}
	})
	return iperfVerStr
}

// congestionForOS resolves the -C value actually passed to iperf3 and reports
// whether a requested value was DROPPED because the platform cannot set it.
// Linux and FreeBSD iperf3 have the -C socket option; on macOS and Windows
// passing it aborts the whole run at startup, so a value that arrived via an
// imported backup from a Linux box is dropped and the run uses the system default.
func congestionForOS(requested, goos string) (effective string, dropped bool) {
	// Narrowing this to Linux once stripped a working knob on FreeBSD.
	if requested != "" && goos != "linux" && goos != "freebsd" {
		return "", true
	}
	return requested, false
}

// AvailableCongestionControl returns TCP congestion-control algorithms to offer in the
// congestion field. On Linux it reads the ones the kernel lets an unprivileged process
// set (nil if the sysctl can't be read); elsewhere the kernel has no such enumerable
// sysctl, so it returns a small curated list of widely available algorithms. Note -C
// applies on the SENDER: for a download that's the remote server, so this list only
// really governs uploads, not downloads (where the server's kernel decides).
func AvailableCongestionControl() []string { return availableCongestionControlFor(runtime.GOOS) }

// availableCongestionControlFor is AvailableCongestionControl with the OS as a
// parameter, so the FreeBSD sysctl-reading + table-parsing branch is testable on
// ANY host (through the injectable ccSysctl seam) rather than only when
// GOOS==freebsd. Only macOS and Windows iperf3 lack the -C socket option; passing
// it there aborts the whole run at startup, so offer nothing and let the field mean
// "system default". (congestionForOS mirrors this at run time for a value that
// arrived via an imported backup from a Linux box.)
func availableCongestionControlFor(goos string) []string {
	switch goos {
	case "linux":
		b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_allowed_congestion_control")
		if err != nil {
			return nil
		}
		return strings.Fields(string(b))
	case "freebsd":
		// FreeBSD enumerates its available algorithms in a sysctl; iperf3 accepts
		// -C for any of them. A read failure just means no dropdown (Auto only).
		out, err := ccSysctl()
		if err != nil {
			return nil
		}
		return parseFreeBSDCC(string(out))
	default:
		// macOS/Windows iperf3 has no -C; offering it aborts the run.
		return nil
	}
}

// ccSysctl reads net.inet.tcp.cc.available. A package var so tests can inject
// either release's output shape without a FreeBSD host.
var ccSysctl = func() ([]byte, error) {
	return exec.Command("sysctl", "-n", "net.inet.tcp.cc.available").Output()
}

// parseFreeBSDCC extracts algorithm names from net.inet.tcp.cc.available, whose
// shape changed across releases:
//
//	13.x: a single comma-separated line          "newreno, cubic, htcp"
//	14.x: a header line + one row per algorithm, with extra columns (a "*"
//	      default marker and PCB counts):
//	          CCmod       D PCB
//	          newreno       0
//	          cubic     *   0
//	          htcp          0
//
// The old strings.Fields parser turned the 14.x table into garbage tokens
// ("CCmod", "D", "PCB", "*", "0", ...) and handed them to iperf3 as algorithm
// names. Discriminate on line count: one line is the 13.x list; more is the
// table, whose first column carries the names (isCCName drops the "CCmod"
// header and any "*"/count cells).
func parseFreeBSDCC(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	seen := map[string]bool{}
	var out []string
	add := func(tok string) {
		tok = strings.Trim(tok, "*")
		if isCCName(tok) && !seen[tok] {
			seen[tok] = true
			out = append(out, tok)
		}
	}
	if len(lines) == 1 {
		// 13.x: split the single line on commas and whitespace.
		for _, tok := range strings.FieldsFunc(lines[0], func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		}) {
			add(tok)
		}
		return out
	}
	// 14.x table: the algorithm name is the first column of each row; the
	// header row's first cell ("CCmod") is rejected by isCCName.
	for _, ln := range lines {
		if f := strings.Fields(ln); len(f) > 0 {
			add(f[0])
		}
	}
	return out
}

// isCCName reports whether tok is a plausible congestion-control algorithm name:
// a lowercase identifier (newreno, cubic, htcp, cdg, ...). This is what lets one
// rule reject the 14.x table's header cell ("CCmod"), single-letter column
// headers, the "*" default marker, and numeric PCB counts.
func isCCName(tok string) bool {
	if tok == "" {
		return false
	}
	for i, r := range tok {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '-'):
		default:
			return false
		}
	}
	return true
}

const (
	iperfDefaultDur     = 5
	iperfMinDur         = 1
	iperfMaxDur         = 30
	iperfDefaultStreams = 1
	// One ceiling, not two: the settings layer clamps to this on the way in and
	// the dashboard offers it as the input's max, so a second literal here would
	// accept a value everywhere else and then quietly drop it before --parallel.
	iperfMaxStreams  = settings.MaxIperfStreams
	iperfDefaultOmit = 1
	iperfMaxOmit     = 5
	iperfMaxWindow   = 65536 // KB (64 MB) ceiling for -w; 0 = auto
	iperfMaxMSS      = 9000  // bytes ceiling for -M (jumbo-frame headroom); 0 = auto
)

// iperfRetryDelay is the backoff before a retry; iperfUploadSettle pauses before the
// upload so it doesn't reconnect while the server is still releasing the just-finished
// download (which surfaces as "server is busy"). Vars, not consts, so tests can shrink
// them.
var (
	iperfRetryDelay   = time.Second
	iperfUploadSettle = 750 * time.Millisecond
)

func iperfDur(fn func() int) int {
	d := iperfDefaultDur
	if fn != nil && fn() > 0 {
		d = fn()
	}
	if d < iperfMinDur {
		d = iperfMinDur
	}
	if d > iperfMaxDur {
		d = iperfMaxDur
	}
	return d
}

// iperfStreams resolves the parallel-stream count (>1 better saturates fast or
// high-RTT links), clamped to [1, iperfMaxStreams]; nil/non-positive -> 1.
func iperfStreams(fn func() int) int {
	n := iperfDefaultStreams
	if fn != nil && fn() > 0 {
		n = fn()
	}
	if n > iperfMaxStreams {
		n = iperfMaxStreams
	}
	return n
}

// iperfWindow resolves the TCP window / socket-buffer size in KB, clamped to
// [0, iperfMaxWindow]; nil/non-positive -> 0 (let the OS auto-tune).
func iperfWindow(fn func() int) int {
	if fn == nil {
		return 0
	}
	w := fn()
	if w < 0 {
		return 0
	}
	if w > iperfMaxWindow {
		return iperfMaxWindow
	}
	return w
}

// iperfToken sanitizes a short free-form CLI value (congestion algorithm, DSCP marking)
// down to a bare word: it rejects empty, a leading '-' (flag-injection guard - defence
// in depth, since we exec with args not a shell), embedded whitespace, or absurd length.
// Returns "" (omit the flag) when unusable; iperf3 validates the value itself.
func iperfToken(fn func() string) string {
	if fn == nil {
		return ""
	}
	s := strings.TrimSpace(fn())
	if s == "" || strings.HasPrefix(s, "-") || strings.IndexFunc(s, unicode.IsSpace) >= 0 || len(s) > 32 {
		return ""
	}
	return s
}

// iperfNoDelay reports whether to disable Nagle (-N); nil -> false (Nagle stays on).
func iperfNoDelay(fn func() bool) bool { return fn != nil && fn() }

// iperfMSS resolves the TCP max-segment-size (-M) in bytes, clamped to [0, iperfMaxMSS];
// nil/non-positive -> 0 (let iperf3/the OS pick).
func iperfMSS(fn func() int) int {
	if fn == nil {
		return 0
	}
	m := fn()
	if m <= 0 {
		return 0
	}
	if m > iperfMaxMSS {
		return iperfMaxMSS
	}
	return m
}

// iperfIPVersion resolves the address-family pin; anything but "4"/"6" -> "auto"
// (iperf3 picks whatever the host resolves to).
func iperfIPVersion(fn func() string) string {
	if fn != nil {
		switch v := fn(); v {
		case "4", "6":
			return v
		}
	}
	return "auto"
}

// iperfUDP reports whether to run the loss/jitter UDP pass; nil -> true.
func iperfUDP(fn func() bool) bool { return fn == nil || fn() }

// isTransientIperfErr reports whether an iperf3 failure is worth retrying: a busy
// server, or a refused/reset/timed-out connection - failures that hit fast (at connect
// or a mid-transfer blip) and may clear on a second try. Hard rejections (bad auth, a
// host that isn't running iperf3) are not transient. "no data transferred" is
// deliberately excluded: a clean exit-0 with zero bytes means the whole window ran, so a
// retry costs a full transfer and the cause (black-holed path, one-way firewall) rarely
// self-heals - better to fail fast and let best-effort drop just that direction.
func isTransientIperfErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sub := range []string{
		"busy", // "the server is busy running a test. try again later"
		"connection refused",
		"connection reset",
		"reset by peer",
		"broken pipe",
		"i/o timeout", "timed out", "timeout",
		"unable to connect",
		"temporarily unavailable",
		"control socket has closed unexpectedly",
	} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// withRetry runs attempt up to 1+retries times, re-running only while the error is
// transient and ctx is still live, pausing iperfRetryDelay between tries. It returns
// the final attempt's error (nil on success).
func withRetry(ctx context.Context, retries int, attempt func() error) error {
	return withRetryPred(ctx, retries, isTransientIperfErr, attempt)
}

// withRetryPred is withRetry with a caller-supplied "is this worth retrying"
// predicate, so other engines (Ookla) can reuse the same retry/backoff loop with
// their own notion of a transient error.
func withRetryPred(ctx context.Context, retries int, transient func(error) bool, attempt func() error) error {
	for i := 0; ; i++ {
		err := attempt()
		if err == nil || i >= retries || ctx.Err() != nil || !transient(err) {
			return err
		}
		if !sleepCtx(ctx, iperfRetryDelay) {
			return err // cancelled mid-backoff
		}
	}
}

// iperfSettle pauses iperfUploadSettle (or until ctx is done) so the upload doesn't
// reconnect into the server's post-download busy window.
func iperfSettle(ctx context.Context) { sleepCtx(ctx, iperfUploadSettle) }

// sleepCtx waits d or until ctx is done, returning true if the full delay elapsed.
// It stops its timer on cancel so a pending timer isn't left to fire.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// iperfServerName is the display name for an iperf3 run - "iperf3: <label>" (engine +
// friendly name, no address), falling back to the host when there's no label. Used for
// both the live "Running speedtest on ..." status and the recorded run name.
func iperfServerName(label, host string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		label = host
	}
	return "iperf3: " + label
}

// withEnvHint appends the injected environment hint (EnvHint) to a transfer or
// UDP-pass failure, keeping the original error in the %w chain - callers rely on
// errors.Is (context cancellation) and on the raw text prefix (speedFailStage).
// Applied only at the surfacing points, after withRetry, so the hint text can
// never change retry classification.
func (i *Iperf) withEnvHint(err error) error {
	if err == nil || i.EnvHint == nil {
		return err
	}
	if h := i.EnvHint(err.Error()); h != "" {
		return fmt.Errorf("%w (%s)", err, h)
	}
	return err
}

// iperfBind resolves the source address/interface; nil/empty -> "" (default route).
func iperfBind(fn func() string) string {
	if fn == nil {
		return ""
	}
	return strings.TrimSpace(fn())
}

// iperfOmit resolves the warm-up seconds to discard (skips TCP slow-start),
// clamped to [0, iperfMaxOmit]; nil -> default. 0 is valid (off).
func iperfOmit(fn func() int) int {
	o := iperfDefaultOmit
	if fn != nil {
		o = fn()
	}
	if o < 0 {
		o = 0
	}
	if o > iperfMaxOmit {
		o = iperfMaxOmit
	}
	return o
}

// iperfAddrSpace is what counts as whitespace inside an address, and it is one
// character wider than Go's own idea of it: unicode.IsSpace does not count
// U+FEFF, the byte-order mark a line pasted out of a file often starts with.
// It is invisible, it makes a host nothing can resolve, and the dashboard's
// address check already refused it - so the two sides disagreed about the same
// string, which is exactly the disagreement that check exists to end. The
// reverse case, U+0085, Go has always counted and the page has not; the page
// counts it now.
func iperfAddrSpace(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }

// parseIperfServer splits and validates a user-supplied "host" or "host:port", so a
// typo or pasted command tail fails here with a clear message instead of as a cryptic
// failed transfer. It rejects:
//   - empty input, or a host/port starting with '-' (flag-injection guard - defence in
//     depth, since we exec with args not a shell),
//   - whitespace in the host (a pasted "host --logfile x"), and
//   - a non-numeric or out-of-range port ("host:5201 junk", "host:abc").
//
// A bare host or bare IPv6 with no port is allowed (iperf3 defaults to 5201).
func parseIperfServer(s string) (host, port string, err error) {
	s = strings.TrimFunc(s, iperfAddrSpace)
	if s == "" {
		return "", "", errors.New("no iperf3 server set")
	}
	host = s
	if h, p, e := net.SplitHostPort(s); e == nil {
		host, port = h, p
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") && isIPLiteral(s[1:len(s)-1]) {
		// A bracketed IPv6 literal with no port ("[2001:db8::1]"). Unwrap it:
		// left bracketed, JoinHostPort downstream would double-bracket it into
		// an undialable address.
		host = s[1 : len(s)-1]
	}
	bad := host == "" || strings.HasPrefix(host, "-") || strings.HasPrefix(port, "-") ||
		strings.ContainsAny(host, "[]") || // a surviving bracket is a malformed literal
		strings.IndexFunc(host, iperfAddrSpace) >= 0 || // any whitespace (incl. newline/CR, and a byte-order mark) is malformed
		// A colon can only survive into host as a bare IPv6 literal (SplitHostPort
		// failed). Anything colon-y that isn't a real IP ("1.2.3.4:5201:9") must
		// fail HERE with a clear message, not as a cryptic dial error retried as
		// transient. Zone-scoped literals (fe80::1%eth0) are valid - LAN iperf3
		// boxes on link-local addresses are a real setup.
		(strings.Contains(host, ":") && !isIPLiteral(host))
	if !bad && port != "" {
		if n, e := strconv.Atoi(port); e != nil || n < 1 || n > 65535 {
			bad = true
		}
	}
	if bad {
		return "", "", fmt.Errorf("invalid iperf3 server %q", s)
	}
	return host, port, nil
}

// isIPLiteral reports whether s is a bare IP address, INCLUDING zone-scoped
// IPv6 ("fe80::1%eth0") - net.ParseIP rejects zones, netip accepts them, and
// both iperf3's getaddrinfo and Go's dialer handle them fine.
func isIPLiteral(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// warnCongestionSkipped says why a requested -C value is not taking effect. Takes
// goos rather than reading it, so the message is testable on any host.
func (i *Iperf) warnCongestionSkipped(requested, goos string) {
	i.congestionSkipOnce.Do(func() {
		if i.Log != nil {
			i.Log.Warn("iperf3 congestion control ignored: -C needs Linux or FreeBSD; running with the system default",
				"requested", requested, "os", goos)
		}
	})
}

// Run measures download then upload against the configured iperf3 server, with the
// bufferbloat sampler running during each transfer.
func (i *Iperf) Run(ctx context.Context) (Result, error) {
	server := ""
	if i.ServerFn != nil {
		server = i.ServerFn()
	}
	host, port, err := parseIperfServer(server)
	if err != nil {
		return Result{}, err
	}
	// The recorded name and the live status read the same now: "iperf3: <label>".
	label := callStr(i.LabelFn)
	name := iperfServerName(label, host)
	if i.OnServer != nil {
		i.OnServer(name)
	}
	tp := iperfTunables{
		dur: iperfDur(i.DurationFn), streams: iperfStreams(i.StreamsFn), omit: iperfOmit(i.OmitFn),
		window: iperfWindow(i.WindowFn), bind: iperfBind(i.BindFn), ipver: iperfIPVersion(i.IPVersionFn),
		congestion: iperfToken(i.CongestionFn), noDelay: iperfNoDelay(i.NoDelayFn),
		dscp: iperfToken(i.DSCPFn), mss: iperfMSS(i.MSSFn),
	}
	// A congestion algorithm saved on Linux can ride an imported backup onto a
	// macOS/Windows host, where -C aborts the run at startup. Drop it and run
	// with the system default instead of failing every test; log once so the
	// operator can see why their setting is not taking effect.
	if eff, dropped := congestionForOS(tp.congestion, runtime.GOOS); dropped {
		i.warnCongestionSkipped(tp.congestion, runtime.GOOS)
		tp.congestion = eff
	}
	dir := speedDirection(i.DirectionFn)
	auth, cleanup, err := i.resolveAuth()
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	// RSA padding is an EXPLICIT per-server choice (PKCS1Fn), never auto-negotiated.
	// The previous design flipped OAEP->legacy on any authorization failure and cached
	// it per server - which meant a wrong password (or clock skew, or a transient
	// reject) could silently downgrade to the weaker pre-3.17 PKCS#1 padding without
	// the operator opting in. That auto-flip and its cache are gone: the user picks
	// legacy deliberately, and the flag is emitted whenever they opt in (emit-and-
	// reject) - version caveats are surfaced to the UI via IperfVersion, not gated
	// here (see iperfArgs).
	res := Result{Engine: "iperf3", Server: name}
	probeAddr := lulRunEndpoint(ctx)
	res.IdleMS = measureIdleLatency(ctx, probeAddr)
	// Unloaded ping to the server, taken now while the link is idle (see measureServerRTT
	// for why iperf3's own min_rtt isn't trusted here).
	serverRTT := measureServerRTT(ctx, host, port, tp)

	// Each transfer (and the UDP pass) is retried on a transient failure - usually a busy
	// server - so a flaky direction gets another shot instead of being dropped. The load
	// sampler runs inside each attempt, so the bloat we keep is from the attempt that
	// actually measured.
	retries := speedRetries(i.RetriesFn)
	var rttMS float64
	// Bytes this run actually moved, summed over EVERY attempt including the failed
	// ones - a run that dies still billed the link, and the failure exits below hand
	// this out with the error so the usage row is honest (Scheduler.recordFailedUsage).
	// Deliberately separate from res.Download/UploadBytes, which record only the
	// MEASURED transfer: on the success path a byte count is what tells the scheduler
	// a direction ran, so a failed direction's spend must never land there.
	var spentDown, spentUp int64
	if dir == "bidir" {
		// One transfer loads both directions at once, exposing contention a sequential run
		// can't (half-duplex links, shared buffers). A failure sinks the whole run - no
		// partial direction to keep.
		var bd iperfBidir
		var load *loadStat
		err := withRetry(ctx, retries, func() error {
			stop := startLoadSampler(ctx, probeAddr)
			var e error
			bd, e = runIperfBidir(ctx, host, port, tp, auth)
			spentDown, spentUp = spentDown+bd.downBytes, spentUp+bd.upBytes
			load = stop()
			return e
		})
		if err != nil {
			res.DownloadBytes, res.UploadBytes = spentDown, spentUp
			return res, fmt.Errorf("bidir: %w", i.withEnvHint(err))
		}
		res.DownloadMbps, res.DownloadBytes = bd.downMbps, bd.downBytes
		res.UploadMbps, res.UploadBytes = bd.upMbps, bd.upBytes
		res.IPFamily = bd.family
		// Both directions loaded at once, so the single sampled bloat applies to each -
		// report it under both so the chart reads "latency under full load".
		res.LoadedDownMS, res.LoadedDownP95MS = load.medPtr(), load.tailPtr()
		res.LoadedUpMS, res.LoadedUpP95MS = load.medPtr(), load.tailPtr()
		rttMS = bd.rttMS
	} else {
		// Run the requested direction(s), best-effort: some servers are download-only and
		// reject the upload, so a failed upload must NOT sink a run whose download worked.
		// Keep whatever succeeded; fail only when no attempted direction measured anything.
		var dn, up iperfRun
		var dnErr, upErr error
		if dir != "up" {
			var dnLoad *loadStat
			dnErr = withRetry(ctx, retries, func() error {
				stop := startLoadSampler(ctx, probeAddr) // bufferbloat sampled during the transfer
				var e error
				dn, e = runIperf(ctx, host, port, tp, auth, true)
				spentDown += dn.bytes
				dnLoad = stop()
				return e
			})
			if dnErr == nil {
				res.DownloadMbps, res.DownloadBytes = dn.mbps, dn.bytes
				res.LoadedDownMS, res.LoadedDownP95MS = dnLoad.medPtr(), dnLoad.tailPtr()
			}
		}
		if dir != "down" {
			if dir == "both" {
				iperfSettle(ctx) // let the server release after the download before reconnecting
			}
			var upLoad *loadStat
			upErr = withRetry(ctx, retries, func() error {
				stop := startLoadSampler(ctx, probeAddr)
				var e error
				up, e = runIperf(ctx, host, port, tp, auth, false)
				spentUp += up.bytes
				upLoad = stop()
				return e
			})
			if upErr == nil {
				res.UploadMbps, res.UploadBytes = up.mbps, up.bytes
				res.LoadedUpMS, res.LoadedUpP95MS = upLoad.medPtr(), upLoad.tailPtr()
			}
		}
		if (dir == "down" && dnErr != nil) || (dir == "up" && upErr != nil) || (dir == "both" && dnErr != nil && upErr != nil) {
			res.DownloadBytes, res.UploadBytes = spentDown, spentUp
			if dnErr != nil {
				return res, fmt.Errorf("download: %w", i.withEnvHint(dnErr))
			}
			return res, fmt.Errorf("upload: %w", i.withEnvHint(upErr))
		}
		// A cancelled or timed-out context must NOT be laundered into a "partial
		// success": if a direction failed while the run's context was already done, the
		// real cause is the cancellation, not a one-direction outage - surface it so a
		// shutdown mid-run isn't recorded as a healthy partial. A run that
		// finished cleanly and is only cancelled afterward has no direction error, so it
		// is unaffected.
		if ctx.Err() != nil && (dnErr != nil || upErr != nil) {
			// The direction that COMPLETED before the cancellation moved real data;
			// spent* carries it (and the killed direction's salvage) so an aborted run
			// still bills what it used.
			res.DownloadBytes, res.UploadBytes = spentDown, spentUp
			return res, fmt.Errorf("iperf3 run cancelled before completion: %w", ctx.Err())
		}
		// A "both" run that lost ONE direction is kept as a partial success, so the
		// surviving error never reaches the caller. Without a signal here, "upload has
		// been blank for weeks" has no evidence on any surface: the run takes the success
		// path, so speed.fail never increments. Count partials on their own counter and
		// log which direction dropped, so a persistently failing direction shows up as a
		// rate instead of being invisible.
		if dir == "both" && (dnErr != nil || upErr != nil) {
			stats.Inc("speed.iperf_partial")
			// Recorded on the Result so a configured threshold on the FAILED
			// direction reads as a breach instead of being silenced by the
			// very failure it watches (see Result.UploadFailed).
			res.DownloadFailed = dnErr != nil
			res.UploadFailed = upErr != nil
			if i.Log != nil {
				if dnErr != nil {
					i.Log.Warn("iperf3 direction failed, partial result kept", "direction", "down", "err", i.withEnvHint(dnErr))
				}
				if upErr != nil {
					i.Log.Warn("iperf3 direction failed, partial result kept", "direction", "up", "err", i.withEnvHint(upErr))
				}
			}
		}
		// Unloaded RTT (min_rtt) from whichever direction reported it; else idle.
		rttMS = pickRTT(dn.rttMS, up.rttMS)
		// Family from whichever direction measured (a failed direction's zero
		// struct carries ""); an "auto" run records what it actually resolved to.
		// These are two separate processes, so a dual-stack hostname can land
		// them on DIFFERENT families - recording the download's would silently
		// misdescribe the upload, so a real disagreement is stored as "mixed".
		// An unknown side (no start.connected block) is not a disagreement: the
		// known family speaks alone, and "" stays "" - never guessed.
		switch {
		case dn.family == "":
			res.IPFamily = up.family
		case up.family == "" || up.family == dn.family:
			res.IPFamily = dn.family
		default:
			res.IPFamily = "mixed"
		}
	}
	// Ping is the UNLOADED latency to the server. Prefer the pre-transfer handshake probe
	// (empty queue); fall back to iperf3's min_rtt (upload/bidir and Linux only), then the
	// idle anchor baseline. Never the loaded average - that bakes in bufferbloat.
	if serverRTT != nil {
		res.PingMS = *serverRTT
	} else if rttMS > 0 {
		res.PingMS = rttMS
	} else if res.IdleMS != nil {
		res.PingMS = *res.IdleMS
	}
	// Best-effort UDP pass for packet loss + jitter, which TCP can't measure (skipped if
	// the user turns it off). Sized to a fraction of throughput so it samples the path
	// without flooding it; a server that blocks UDP leaves these blank.
	if iperfUDP(i.UDPFn) {
		rate := udpRate(res.DownloadMbps, res.UploadMbps) // auto: ~half the TCP rate, bounded
		if i.UDPRateFn != nil {
			if r := i.UDPRateFn(); r > 0 {
				rate = r // explicit user cap bypasses the auto ceiling (e.g. to sample gigabit loss)
				// The UDP pass moves real bytes that the usage figure never counts (it
				// records only the final TCP aggregates). At the auto ceiling
				// that's negligible, but a high manual cap can dwarf the tracked transfer
				// (10 Gbps x 2s ~= 2.5 GB/run), so warn once the operator opts into it.
				if r >= iperfUDPRateWarn && i.Log != nil {
					i.Log.Warn("iperf3 udp probe rate is high; its bytes are not counted in usage",
						"rate_mbps", r, "approx_uncounted_mb_per_run", r*iperfUDPSecs/8)
				}
			}
		}
		// Probe the direction the run actually measured: a "both" run whose
		// download failed but upload survived (keep-partial) records upload
		// data only, so its loss/jitter must sample upstream too.
		downstream := dir != "up" && !(dir == "both" && res.DownloadMbps == 0 && res.UploadMbps > 0)
		var loss, jit *float64
		udpErr := withRetry(ctx, retries, func() error {
			var e error
			loss, jit, e = measureUDP(ctx, host, port, rate, downstream, tp, auth)
			return e
		})
		if loss != nil {
			res.PacketLoss, res.JitterMS = loss, jit
			// Record which way the probe sampled: loss on an asymmetric path
			// differs by direction, so a stored sample is ambiguous without it.
			if downstream {
				res.UDPDirection = "down"
			} else {
				res.UDPDirection = "up"
			}
		} else if udpErr != nil && i.Log != nil {
			// The run still succeeds without loss/jitter, so this failure is
			// invisible everywhere else. The message distinguishes the cases that
			// actually happen: UDP blocked upstream ("no datagrams"), the server
			// rejecting the UDP pass ("authorization failed"), and a busy server -
			// which need different answers from support.
			//
			// "no datagrams" only catches a server that ANSWERS and reports zero
			// packets - iperf3 has to finish and print its JSON to be counted that
			// way. A firewall that DROPs rather than REJECTs (the default for ufw
			// and most cloud security groups) sends the datagrams into silence, so
			// iperf3 never returns, the per-run deadline fires, and the failure
			// arrives here as a stall instead. That is why the stall is worth a
			// hint of its own: on its own it reads like a busy server.
			//
			// What makes it diagnosable is the pair. TCP and UDP use the same host
			// and port here, so TCP moving data while UDP stalls leaves almost
			// nothing but a filter that treats the two protocols differently. Both
			// figures are the run's own, measured moments earlier.
			hint := ""
			if errors.Is(udpErr, errStalled) && (res.DownloadMbps > 0 || res.UploadMbps > 0) {
				hint = "TCP moved data on this host and port while the UDP pass stalled - " +
					"the far end is most likely dropping UDP (open the same port for UDP, e.g. ufw allow 5201/udp)"
			}
			if hint != "" {
				i.Log.Warn("iperf3 udp pass failed, loss and jitter unrecorded",
					"err", i.withEnvHint(udpErr), "downstream", downstream, "rate_mbps", rate,
					"likely_cause", hint)
			} else {
				i.Log.Warn("iperf3 udp pass failed, loss and jitter unrecorded",
					"err", i.withEnvHint(udpErr), "downstream", downstream, "rate_mbps", rate)
			}
		}
	}
	// Anything spent beyond what got MEASURED - a retried direction's earlier
	// attempts, or a direction that failed while the other succeeded - is real
	// traffic that a successful run would otherwise drop from data usage
	// entirely (only the winning attempt's bytes reach Download/UploadBytes,
	// deliberately, because a byte count there is what says a direction ran).
	// Hand it out separately so the scheduler can bill it without inventing a
	// measurement.
	if d := spentDown - res.DownloadBytes; d > 0 {
		res.ExtraDownBytes = d
	}
	if u := spentUp - res.UploadBytes; u > 0 {
		res.ExtraUpBytes = u
	}
	return res, nil
}

const (
	iperfUDPSecs     = 2
	iperfUDPMinRate  = 5    // Mbps floor so a near-zero TCP reading still samples
	iperfUDPMaxRate  = 50   // Mbps ceiling so the loss probe never floods a fast link
	iperfUDPRateWarn = 1000 // Mbps: explicit rates at/above this get an uncounted-usage warning
	iperfUDPBudget   = 8 * time.Second
)

// udpRate sizes the loss/jitter probe to ~half the measured TCP throughput,
// bounded, so it samples the path without saturating it. The 5 Mbps floor keeps
// a near-zero reading meaningful, but is applied only when capacity comfortably
// clears it - on a slow link the floor would exceed the path and its own queue
// drops would masquerade as real packet loss, so there the probe stays below the
// measured capacity instead.
func udpRate(downMbps, upMbps float64) int {
	v := downMbps
	if v <= 0 {
		v = upMbps
	}
	if v <= 0 {
		return iperfUDPMinRate // nothing measured; the floor still yields a sample
	}
	r := int(v * 0.5)
	if r < iperfUDPMinRate && float64(iperfUDPMinRate) <= v*0.8 {
		r = iperfUDPMinRate // capacity comfortably clears the floor -> floor it
	}
	if r < 1 {
		r = 1 // a sub-2-Mbps link still gets a below-capacity probe, not the floor
	}
	if r > iperfUDPMaxRate {
		return iperfUDPMaxRate
	}
	return r
}

// measureUDP runs a short UDP transfer to read packet loss and jitter, which TCP
// can't report. The probe matches the tested path: downstream (--reverse) for
// down/both/bidir runs, upstream for an upload-only run - a user testing one
// direction on an asymmetric path gets loss for THAT direction. Best-effort: on
// failure it returns (nil, nil, err) so the caller leaves loss/jitter blank, and
// the error lets the retry tell a transient hiccup (busy/reset) from a UDP-less
// server ("no datagrams" - don't retry). A clean run returns the values with a
// nil error.
func measureUDP(ctx context.Context, host, port string, rateMbps int, downstream bool, tp iperfTunables, auth iperfAuth) (loss, jitter *float64, err error) {
	pctx, cancel := context.WithTimeout(ctx, iperfUDPBudget)
	defer cancel()
	args := []string{"--client", host, "--json", "--udp",
		"--bitrate", strconv.Itoa(rateMbps) + "M", "--time", strconv.Itoa(iperfUDPSecs),
		// 1200-byte datagrams: 1200 + 8 (UDP) + 40 (IPv6) = 1248 stays under the
		// 1280 IPv6 floor MTU, so the probe never fragments on any sane path.
		// iperf3's ~1460-byte default fragments wherever the effective MTU dips
		// below Ethernet (tunnel uplinks behind a bridge pinned at 1500), and
		// dropped or reassembly-delayed fragments read back as loss/jitter.
		"--length", "1200", "--connect-timeout", "4000"}
	if downstream {
		args = append(args, "--reverse")
	}
	if port != "" {
		args = append(args, "--port", port)
	}
	if v := ipVersionFlag(tp.ipver); v != "" {
		args = append(args, v)
	}
	if tp.bind != "" {
		args = append(args, iperfBindArgs(tp.bind)...)
	}
	if tp.dscp != "" {
		args = append(args, "--dscp", tp.dscp) // IP-layer marking applies to UDP too
	}
	if auth.on() {
		args = append(args, "--username", auth.username, "--rsa-public-key-path", auth.keyPath)
		if auth.pkcs1 { // explicit opt-in only; see iperfArgs for the version caveats
			args = append(args, "--use-pkcs1-padding")
		}
	}
	out, runErr := auth.run(pctx, args)
	runErr = stalledErr(runErr, pctx, ctx, iperfUDPBudget)
	var j iperfJSON
	jErr := json.Unmarshal(out, &j) // the error field is the primary signal; jErr guards a truncated body
	if j.Error != "" {
		return nil, nil, errors.New(strings.TrimSpace(j.Error)) // e.g. "server is busy" -> retryable
	}
	if runErr != nil {
		return nil, nil, runErr
	}
	if jErr != nil {
		// A decode error must sink the reading, not the half-decoded fields behind it.
		// encoding/json keeps whatever it parsed before the error, so a body that is
		// valid JSON but carries an overflowing/mistyped field (jitter_ms:1e400) still
		// leaves packets and lost_percent populated - and the old "_ = json.Unmarshal"
		// then recorded that as a genuine low-loss / 0ms-jitter sample. (A truncated
		// body fails json's up-front validity check and zeroes everything, which the
		// packets<=0 guard below would catch - but this is the honest place to reject.)
		return nil, nil, errors.New("unparseable iperf3 output")
	}
	if j.End.Sum.Packets <= 0 {
		return nil, nil, errors.New("no datagrams") // blocked / UDP unsupported - not transient
	}
	lp := j.End.Sum.LostPercent
	jt := j.End.Sum.JitterMS
	// A remote iperf3 server is untrusted (see plausibleMbps). A NaN slips straight
	// past the range clamps below (it compares false to both bounds) and would be
	// stored and plotted verbatim; ±Inf clamps to a bound but is no more real. Reject
	// any non-finite figure instead of fabricating a value from it. json.Unmarshal
	// already rejects the literal nan/inf forms and an overflow like 1e400 (caught by
	// jErr above), so this is defence in depth against a value that arrives finite-
	// looking but isn't.
	if math.IsNaN(lp) || math.IsInf(lp, 0) || math.IsNaN(jt) || math.IsInf(jt, 0) {
		return nil, nil, errors.New("non-finite loss/jitter")
	}
	if lp < 0 {
		lp = 0
	} else if lp > 100 {
		lp = 100
	}
	if jt < 0 {
		jt = 0
	} else if jt > maxJitterMS {
		jt = maxJitterMS // a hostile/broken server can report an absurd 1e308 jitter
	}
	return &lp, &jt, nil
}

func f64p(v float64) *float64 { return &v }

// Sanity bounds on a remote iperf3 server's self-reported numbers. The server is
// a host the operator points at but does not control the software of, so a
// negative, NaN/Inf, or absurd reading is rejected instead of being stored and
// plotted verbatim. maxPlausibleMbps sits far above any real link (10 Tbps); a
// finite value below it is trusted. maxJitterMS caps the UDP jitter clamp.
const (
	maxPlausibleMbps = 1e7
	maxJitterMS      = 1e6
)

// plausibleMbps reports whether an iperf3 throughput is a usable measurement:
// positive, finite, and below the absurd ceiling. NaN and +Inf fail the
// comparisons, so no explicit finiteness check is needed.
func plausibleMbps(mbps float64) bool {
	return mbps > 0 && mbps <= maxPlausibleMbps
}

// billableBytes returns a transfer's byte total when the body reporting it is
// self-consistent enough to put on the data-usage ledger: a positive count whose
// rate clears the same plausibility bound the success path applies. A remote
// iperf3 server is untrusted, and a failed run's totals get no second look from
// the measurement checks - so an absurd body contributes nothing rather than
// inflating "data used" (and, on a metered link, the operator's alarm).
func billableBytes(sum iperfSum) int64 {
	if sum.Bytes > 0 && plausibleMbps(sum.BitsPerSecond/1e6) {
		return sum.Bytes
	}
	return 0
}

func pickRTT(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}

const (
	iperfPingProbes  = 5                     // unloaded-RTT handshakes to the server
	iperfPingGap     = 75 * time.Millisecond // gap between probes
	iperfPingTimeout = 3 * time.Second       // per-probe connect cap
	iperfPingBudget  = 6 * time.Second       // total cap (a firewalled host can time out every probe)
	iperfDefaultPort = "5201"                // iperf3's default control/data port
)

// dialNetwork maps the address-family pin to a net dial network for the RTT probe.
func dialNetwork(ipver string) string {
	switch ipver {
	case "4":
		return "tcp4"
	case "6":
		return "tcp6"
	}
	return "tcp"
}

// CheckIperfServer is a quick reachability probe for the UI's per-server status light:
// one bounded TCP handshake to the iperf3 port. Returns the handshake RTT on success, or
// an error if the address is malformed or unreachable within ctx.
//
// It sends no iperf3 control cookie and closes right after the handshake, so a normal
// persistent `iperf3 -s` logs a stray/aborted control connection ("unable to receive
// cookie") - cosmetic noise, not a failed test. The one case where it is
// NOT free: a one-shot server (`iperf3 -s -1`) accepts exactly one connection, so this
// probe consumes it. That mode is incompatible with pingularity's repeated scheduled
// runs anyway (it dies after a single test), so the status light assumes a persistent
// server. A green light therefore means "the port accepted a TCP connection", which is
// weaker than "a real test would authenticate and run".
func CheckIperfServer(ctx context.Context, addr string) (rttMS float64, err error) {
	host, port, err := parseIperfServer(addr)
	if err != nil {
		return 0, err
	}
	if port == "" {
		port = iperfDefaultPort
	}
	start := time.Now()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return 0, err
	}
	c.Close()
	return util.DurMS(time.Since(start)), nil
}

// measureServerRTT samples the UNLOADED round-trip to the iperf3 server by timing a few
// bare TCP handshakes (SYN -> SYN/ACK) to its port BEFORE any transfer loads the link -
// the bufferbloat sampler's privilege-free method, aimed at the server instead of the
// anycast anchor. This is the real ping, and is why iperf3's TCP_INFO min_rtt isn't
// trusted: min_rtt is 0 for a download (the client is the receiver, gets no sender-side
// TCP_INFO) and 0 off Linux, and even when present it's the minimum over a LOADED
// transfer, so on a fast-filling buffer slow-start has already queued by the first sample
// and it settles near the bloat. Each probe opens a bare TCP connection and closes before
// sending iperf3's control cookie, timing only the SYN->SYN/ACK handshake. On a persistent
// server that is cosmetic (a logged "unable to receive cookie" per probe, no test run), but
// it is NOT a protocol-clean transaction and would consume a one-shot `-s -1` server - see
// CheckIperfServer for why that mode is out of scope here. Returns nil when too
// few probes land (host unreachable) so the caller falls back to min_rtt, then the idle
// baseline.
func measureServerRTT(ctx context.Context, host, port string, tp iperfTunables) *float64 {
	if port == "" {
		port = iperfDefaultPort
	}
	addr := net.JoinHostPort(host, port)
	ctx, cancel := context.WithTimeout(ctx, iperfPingBudget)
	defer cancel()
	d := net.Dialer{Timeout: iperfPingTimeout}
	if tp.bind != "" {
		if ip := net.ParseIP(tp.bind); ip != nil {
			d.LocalAddr = &net.TCPAddr{IP: ip} // honor --bind only when it's a source IP
		}
		// An interface NAME (iperf3 also accepts e.g. "eth0") can't be a dial LocalAddr
		// without resolving it to an address first, so this probe takes the default route
		// while the transfer binds the interface. Accepted: the probe is only a fallback,
		// so any path divergence shows up in the ping figure only when it's actually used.
	}
	network := dialNetwork(tp.ipver)
	var ms []float64
	for i := 0; i < iperfPingProbes; i++ {
		if ctx.Err() != nil {
			break
		}
		start := time.Now()
		c, err := d.DialContext(ctx, network, addr)
		if err == nil {
			ms = append(ms, util.DurMS(time.Since(start)))
			c.Close()
		}
		if i < iperfPingProbes-1 { // no need to wait after the final probe
			select {
			case <-ctx.Done():
			case <-time.After(iperfPingGap):
			}
		}
	}
	if len(ms) == 0 {
		return nil
	}
	m := median(ms)
	return &m
}

type iperfRun struct {
	mbps   float64
	bytes  int64
	rttMS  float64
	family string // "4"/"6" from start.connected; "" unknown (see iperfFamily)
}

// iperfTunables are the per-run TCP knobs resolved from the configured settings.
type iperfTunables struct {
	dur     int    // -t seconds per direction
	streams int    // -P parallel streams
	omit    int    // -O warm-up seconds discarded
	window  int    // -w TCP window / socket-buffer in KB (0 = auto; TCP only)
	bind    string // --bind source address ("" = default route)
	ipver   string // "auto"|"4"|"6" address-family pin (-4/-6)
	// Advanced knobs (see Iperf fields). congestion/mss/noDelay are TCP-only; dscp is
	// an IP-layer marking applied to both the TCP transfers and the UDP probe.
	congestion string // -C congestion algorithm ("" = system default)
	noDelay    bool   // -N disable Nagle
	dscp       string // --dscp IP DiffServ value ("" = none)
	mss        int    // -M max segment size in bytes (0 = auto)
}

// iperfAuth carries resolved RSA-auth credentials for a run. Username and key path go on
// the command line; the password rides the IPERF3_PASSWORD env so it stays out of the
// process listing.
type iperfAuth struct {
	username, password, keyPath string
	pkcs1                       bool // force --use-pkcs1-padding (legacy iperf3 servers)
}

func (a iperfAuth) on() bool { return a.username != "" && a.keyPath != "" }

// iperfExec is the exec seam every iperf3 transfer goes through: production runs
// the real binary; tests substitute a fake that returns canned JSON keyed off the
// argv. A nil env inherits the process environment (exec's default), so the
// production path is byte-identical to the direct call it replaced.
var iperfExec = func(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	return cmd.Output()
}

// run execs iperf3, injecting the password via env when authenticating. Transfer and
// connection failures come back as a JSON {"error":...} body on stdout (the callers' real
// signal), but argument-level failures - a bad RSA key, an unavailable congestion
// algorithm, a bad flag - print to STDERR and exit non-zero with no JSON. cmd.Output()
// captures that stderr on the ExitError; we surface it so callers see the real message,
// not an opaque "exit status N".
func (a iperfAuth) run(ctx context.Context, args []string) ([]byte, error) {
	var env []string
	if a.on() && a.password != "" {
		env = append(os.Environ(), "IPERF3_PASSWORD="+a.password)
	}
	out, err := iperfExec(ctx, "iperf3", args, env)
	// A per-run deadline that fired means the watchdog killed a stalled iperf3; the
	// context, not the OS exit code/text, is the portable signal for that.
	return iperfExecErr(out, err, errors.Is(ctx.Err(), context.DeadlineExceeded))
}

// iperfExecErr shapes an iperf3 exec failure into the caller-facing error. A normal
// non-zero exit gets its real STDERR message instead of an opaque "exit status N". When
// deadlineHit is set the per-run watchdog killed the process (SIGKILL on Unix, a forced
// terminate on Windows), so the raw exec error is kept untouched even if the doomed
// process had already printed a warning to stderr - letting stalledErr recognise the
// stall structurally instead of reporting the stale warning (or an OS-specific kill
// text) as the failure.
func iperfExecErr(out []byte, err error, deadlineHit bool) ([]byte, error) {
	if deadlineHit {
		return out, err
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if msg := strings.TrimSpace(string(ee.Stderr)); msg != "" {
			return out, errors.New(msg)
		}
	}
	return out, err
}

func callStr(fn func() string) string {
	if fn == nil {
		return ""
	}
	return fn()
}

// resolveAuth reads the auth settings and, when enabled and complete, writes the server's
// public key to a temp file (--rsa-public-key-path needs a path); the returned cleanup
// removes it. When auth is enabled but a field is missing it FAILS CLOSED with an error
// rather than silently running unauthenticated: a half-configured credential
// is an operator mistake, and running without it would leak an unauthenticated test to a
// server the operator meant to gate. Auth left off entirely returns an empty config and no
// error, as before.
func (i *Iperf) resolveAuth() (iperfAuth, func(), error) {
	noop := func() {}
	if i.AuthFn == nil || !i.AuthFn() {
		return iperfAuth{}, noop, nil
	}
	user, pass, key := callStr(i.UsernameFn), callStr(i.PasswordFn), strings.TrimSpace(callStr(i.RSAKeyFn))
	if user == "" || pass == "" || key == "" {
		// Fail CLOSED: auth was explicitly enabled, so a missing field is a
		// misconfiguration, not licence to run unauthenticated. Running without
		// credentials would silently defeat the saved security intent (and, on a
		// server that happens to accept anonymous clients, quietly measure it
		// anyway). Return a precise error so the run fails visibly instead.
		var missing []string
		if user == "" {
			missing = append(missing, "username")
		}
		if pass == "" {
			missing = append(missing, "password")
		}
		if key == "" {
			missing = append(missing, "RSA public key")
		}
		return iperfAuth{}, noop, fmt.Errorf("iperf3 authentication is enabled but incomplete (%s missing) - refusing to run unauthenticated", strings.Join(missing, ", "))
	}
	f, err := os.CreateTemp("", "pingularity-iperf-key-*.pem")
	if err != nil {
		return iperfAuth{}, noop, fmt.Errorf("iperf3 auth: temp key: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(key); err != nil {
		f.Close()
		os.Remove(name)
		return iperfAuth{}, noop, fmt.Errorf("iperf3 auth: write key: %w", err)
	}
	f.Close()
	pkcs1 := i.PKCS1Fn != nil && i.PKCS1Fn()
	return iperfAuth{username: user, password: pass, keyPath: name, pkcs1: pkcs1}, func() { os.Remove(name) }, nil
}

// iperfMode is the transfer direction for one iperf3 invocation.
type iperfMode int

const (
	modeForward iperfMode = iota // client sends (upload)
	modeReverse                  // --reverse: server sends (download)
	modeBidir                    // --bidir: both directions at once
)

// ipVersionFlag maps the address-family pin to the iperf3 flag ("" for auto).
func ipVersionFlag(ipver string) string {
	switch ipver {
	case "4":
		return "-4"
	case "6":
		return "-6"
	}
	return ""
}

// iperfArgs builds the iperf3 CLI args for one transfer. --connect-timeout bounds
// control-connection setup; --parallel aggregates into end.sum_* (so the parser is
// unchanged); --omit's discarded stats never reach end.sum; -w sizes the socket buffer.
func iperfArgs(host, port string, tp iperfTunables, auth iperfAuth, mode iperfMode) []string {
	args := []string{"--client", host, "--json", "--time", strconv.Itoa(tp.dur), "--connect-timeout", "5000"}
	if port != "" {
		args = append(args, "--port", port)
	}
	if v := ipVersionFlag(tp.ipver); v != "" {
		args = append(args, v)
	}
	if tp.bind != "" {
		args = append(args, iperfBindArgs(tp.bind)...)
	}
	if tp.streams > 1 {
		args = append(args, "--parallel", strconv.Itoa(tp.streams))
	}
	if tp.omit > 0 {
		args = append(args, "--omit", strconv.Itoa(tp.omit))
	}
	if tp.window > 0 {
		args = append(args, "--window", strconv.Itoa(tp.window)+"K")
	}
	if tp.congestion != "" {
		args = append(args, "--congestion", tp.congestion)
	}
	if tp.mss > 0 {
		args = append(args, "--set-mss", strconv.Itoa(tp.mss))
	}
	if tp.noDelay {
		args = append(args, "--no-delay")
	}
	if tp.dscp != "" {
		args = append(args, "--dscp", tp.dscp)
	}
	if auth.on() {
		args = append(args, "--username", auth.username, "--rsa-public-key-path", auth.keyPath)
		// Legacy PKCS#1 padding is emitted only on the operator's explicit per-server
		// opt-in (never auto-negotiated - see Run). Caveats surfaced to the UI via
		// IperfVersion rather than silently swallowed here: iperf3 <=3.16 lacks the
		// flag (but already defaults to PKCS#1, so the toggle is unnecessary there),
		// and 3.20+ reclassified it as server-only so a 3.20+ client can reject it.
		if auth.pkcs1 {
			args = append(args, "--use-pkcs1-padding")
		}
	}
	switch mode {
	case modeReverse:
		args = append(args, "--reverse")
	case modeBidir:
		args = append(args, "--bidir")
	}
	return args
}

// iperfBindArgs maps a source-binding value to the correct iperf3 option. A source
// ADDRESS (or "address%dev") is --bind host[%dev]; a bare interface NAME must be
// --bind-dev dev - passing a device name to --bind makes iperf3 try to resolve it
// as a hostname and fail ("Name or service not known"). --bind-dev needs a recent
// iperf3 (and usually elevated privilege); an older build errors clearly, which is
// the honest outcome rather than a misparsed --bind.
func iperfBindArgs(bind string) []string {
	addr := bind
	if i := strings.IndexByte(bind, '%'); i >= 0 {
		addr = bind[:i] // "10.0.0.5%eth0" -> address part decides
	}
	if net.ParseIP(addr) != nil {
		return []string{"--bind", bind}
	}
	return []string{"--bind-dev", bind}
}

// stalledErr rewrites the error from a watchdog kill. When the per-run deadline fires,
// exec force-kills a stalled iperf3 before it writes its JSON, so the caller would see an
// opaque OS-specific failure ("signal: killed" on Unix, "exit status 1" on Windows) -
// indistinguishable from a crash. Detection is structural and portable: the per-run
// deadline expired (rctx) while the caller's ctx is still live, so it never depends on a
// Unix signal exit code or text. A caller cancellation (shutdown, browser disconnect)
// keeps its own error. The message deliberately avoids the transient-error keywords: the
// stall already ate the full window, so a retry costs another one and rarely helps (same
// policy as "no data transferred").
//
// errStalled carries that verdict for callers who can say something useful about
// WHY it stalled. The wording is unchanged - the sentinel is the whole message
// text, so "%w (killed after 8s)" still reads "transfer stalled (killed after
// 8s)" - it is just matchable now instead of only printable.
var errStalled = errors.New("transfer stalled")

func stalledErr(err error, rctx, ctx context.Context, budget time.Duration) error {
	if err != nil && errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		return fmt.Errorf("%w (killed after %v)", errStalled, budget)
	}
	return err
}

// runIperf execs one iperf3 transfer and parses the JSON; reverse=true measures download
// (the server sends). It parses the body even on a non-zero exit, since iperf3 reports
// connection failures as {"error": "..."} on stdout. A per-run deadline bounds a
// black-holed host that never connects.
func runIperf(ctx context.Context, host, port string, tp iperfTunables, auth iperfAuth, reverse bool) (iperfRun, error) {
	budget := time.Duration(tp.dur+tp.omit+10) * time.Second
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	mode := modeForward
	if reverse {
		mode = modeReverse
	}
	out, runErr := auth.run(rctx, iperfArgs(host, port, tp, auth, mode))
	runErr = stalledErr(runErr, rctx, ctx, budget)
	var j iperfJSON
	jErr := json.Unmarshal(out, &j) // the error field is the primary signal; jErr guards a truncated body
	sum := iperfUploadSum(j)        // forward: upload, the server's received aggregate
	if reverse {
		sum = j.End.SumReceived // download: this client is the receiver
	}
	if j.Error != "" {
		return iperfRun{bytes: billableBytes(sum)}, errors.New(strings.TrimSpace(j.Error))
	}
	if runErr != nil {
		// A nonzero exit can still follow a body carrying real totals (data moved,
		// then the control connection dropped). Those bytes are on the user's bill,
		// so hand them back with the error for the failed-run usage row; mbps stays
		// zero, because this is accounting and not a measurement.
		return iperfRun{bytes: billableBytes(sum)}, runErr
	}
	if jErr != nil {
		// A body that won't parse (or a field that overflows float64) leaves the
		// numeric fields at zero; fail rather than record a confident 0 Mbps.
		return iperfRun{}, errors.New("unparseable iperf3 output")
	}
	if sum.Bytes <= 0 {
		// A stalled transfer can exit 0 with no data; treat it as a failure rather
		// than recording a real 0 Mbps (which would trip speed thresholds).
		return iperfRun{}, errors.New("no data transferred")
	}
	mbps := sum.BitsPerSecond / 1e6
	if !plausibleMbps(mbps) {
		// A remote iperf3 server is untrusted; a negative/absurd throughput would
		// otherwise be stored verbatim and rescale the speed chart.
		return iperfRun{}, errors.New("implausible iperf3 throughput")
	}
	r := iperfRun{mbps: mbps, bytes: sum.Bytes, rttMS: iperfStreamRTT(j), family: iperfFamily(j)}
	return r, nil
}

// iperfBidir holds both directions from a single --bidir transfer.
type iperfBidir struct {
	downMbps, upMbps   float64
	downBytes, upBytes int64
	rttMS              float64
	family             string // "4"/"6" from start.connected; "" unknown
}

// runIperfBidir execs one --bidir transfer (download and upload at once) and splits the
// two flows: forward (client->server) is the upload, read from the server's received
// aggregate (see iperfUploadSum); reverse (server->client) is the download, in
// end.sum_received_bidir_reverse. Both must carry data or the run failed.
func runIperfBidir(ctx context.Context, host, port string, tp iperfTunables, auth iperfAuth) (iperfBidir, error) {
	budget := time.Duration(tp.dur+tp.omit+10) * time.Second
	rctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, runErr := auth.run(rctx, iperfArgs(host, port, tp, auth, modeBidir))
	runErr = stalledErr(runErr, rctx, ctx, budget)
	var j iperfJSON
	jErr := json.Unmarshal(out, &j)
	up := iperfUploadSum(j)             // forward: client -> server (upload), receiver side
	dn := j.End.SumReceivedBidirReverse // reverse: server -> client (download)
	// Same rule as runIperf: a failed transfer still hands out the bytes it moved,
	// so the usage row bills them; only the measurement is discarded.
	spent := iperfBidir{downBytes: billableBytes(dn), upBytes: billableBytes(up)}
	if j.Error != "" {
		return spent, errors.New(strings.TrimSpace(j.Error))
	}
	if runErr != nil {
		return spent, runErr
	}
	if jErr != nil {
		return iperfBidir{}, errors.New("unparseable iperf3 output")
	}
	if up.Bytes <= 0 || dn.Bytes <= 0 {
		return iperfBidir{}, errors.New("no data transferred")
	}
	downMbps, upMbps := dn.BitsPerSecond/1e6, up.BitsPerSecond/1e6
	if !plausibleMbps(downMbps) || !plausibleMbps(upMbps) {
		return iperfBidir{}, errors.New("implausible iperf3 throughput")
	}
	b := iperfBidir{
		downMbps: downMbps, downBytes: dn.Bytes,
		upMbps: upMbps, upBytes: up.Bytes,
		rttMS: iperfStreamRTT(j), family: iperfFamily(j),
	}
	return b, nil
}

// iperfJSON is the minimal slice of `iperf3 -J` output we read.
type iperfJSON struct {
	Error string `json:"error"`
	Start struct {
		// The control connection's peer as actually connected - the only record
		// of which address family a family-"auto" run resolved to (see iperfFamily).
		Connected []struct {
			RemoteHost string `json:"remote_host"`
		} `json:"connected"`
	} `json:"start"`
	End struct {
		SumSent     iperfSum `json:"sum_sent"`
		SumReceived iperfSum `json:"sum_received"`
		// --bidir splits the two flows out into the *_bidir_reverse aggregates: the
		// reverse flow (server->client = download) lands in sum_received_bidir_reverse.
		SumReceivedBidirReverse iperfSum `json:"sum_received_bidir_reverse"`
		// UDP aggregate: loss/jitter live here, not in sum_sent/sum_received.
		Sum struct {
			LostPercent float64 `json:"lost_percent"`
			JitterMS    float64 `json:"jitter_ms"`
			Packets     int64   `json:"packets"`
		} `json:"sum"`
		Streams []struct {
			Sender struct {
				// All microseconds, Linux TCP_INFO (0 elsewhere). MinRTT is the lowest
				// round-trip seen - the UNLOADED ping to this server, taken before the
				// transfer fills the queue. MeanRTT is the average over the whole loaded
				// transfer, so it carries the bufferbloat and is NOT the ping.
				MinRTT  int64 `json:"min_rtt"`
				MeanRTT int64 `json:"mean_rtt"`
			} `json:"sender"`
		} `json:"streams"`
	} `json:"end"`
}

// iperfFamily classifies the address family a run actually used ("4"/"6") from
// the control connection's peer literal (start.connected[0].remote_host). Family
// "auto" resolves invisibly - a dual-stack native host measures IPv6 where an
// IPv6-less network (a default Docker bridge) silently measures IPv4 - and
// nothing else in the JSON records which happened. Returns "" when the block is
// absent (an error body) or the literal doesn't parse. A v4-mapped literal
// (::ffff:a.b.c.d, from a dual-bound server socket) is IPv4 on the wire.
func iperfFamily(j iperfJSON) string {
	if len(j.Start.Connected) == 0 {
		return ""
	}
	a, err := netip.ParseAddr(j.Start.Connected[0].RemoteHost)
	if err != nil {
		return ""
	}
	if a.Is4() || a.Is4In6() {
		return "4"
	}
	return "6"
}

// iperfStreamRTT returns the lowest non-zero TCP_INFO min_rtt (ms) across streams - a
// FALLBACK ping source behind the direct handshake probe (see measureServerRTT). It's 0
// for a download (reverse: the client receives, gets no sender-side TCP_INFO) and 0 off
// Linux, and even when present it's the minimum over a loaded transfer, so it can read
// high on fast-filling links. Used only when the handshake probe failed.
func iperfStreamRTT(j iperfJSON) float64 {
	// min_rtt is per-stream, from the sender's TCP_INFO (Linux only). A --bidir run's
	// stream array holds both directions, and the leading stream may be the reverse flow
	// with no sender-side RTT (0); scanning for the lowest non-zero value keeps a zero
	// leading stream from masking a real measurement.
	best := 0.0
	for _, s := range j.End.Streams {
		if ms := float64(s.Sender.MinRTT) / 1000.0; ms > 0 && (best == 0 || ms < best) {
			best = ms
		}
	}
	return best
}

type iperfSum struct {
	BitsPerSecond float64 `json:"bits_per_second"`
	Bytes         int64   `json:"bytes"`
}

// iperfUploadSum picks the upload aggregate for a forward flow: the RECEIVER
// side (sum_received - what the server actually got, reported back at test
// end). sum_sent counts every byte written to the socket, including data still
// sitting in the send buffer when the clock stops, which inflates short tests
// on slow uplinks. Falls back to the sender side when the server didn't report
// a received aggregate. Works for --bidir too: its forward flow uses the same
// sum_sent/sum_received pair.
func iperfUploadSum(j iperfJSON) iperfSum {
	if j.End.SumReceived.Bytes > 0 {
		return j.End.SumReceived
	}
	return j.End.SumSent
}
