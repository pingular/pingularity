package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"path"
	"reflect"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	ookla "github.com/showwin/speedtest-go/speedtest"
	"github.com/showwin/speedtest-go/speedtest/transport"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/util"
)

// Ookla measures against the Speedtest.net server network, so results line up
// with speedtest.net.
type Ookla struct {
	// ServerIDFn, if set, supplies a specific Ookla server ID (live). Empty string
	// means auto-select the fastest server.
	ServerIDFn func() string

	// AutoLocFn, if set, supplies a coordinate the USER chose to centre
	// auto-select on (a searched city). It overrides the city race below:
	// a typed city is a statement of intent, not a hypothesis to be measured.
	// ok=false means no city is set, which the race answers.
	AutoLocFn func() (lat, lon float64, ok bool)

	// OriginsFn, if set, enumerates the candidate cities auto-select races when
	// nothing is pinned and no city is searched: each origin's closest servers
	// enter one deduplicated ping race and the city with the fastest answer
	// becomes the centre (see raceCities). Nil = no race; the fetch is
	// uncentred and the Ookla API places our source address.
	OriginsFn func() []Origin

	// ISPFn, if set, supplies the user's ISP display name (e.g. "AS1403 EBOX -
	// EBOX"), so auto-select can guarantee the ISP's own server a lane in the
	// ping race (see autoCandidates). Nil / "" = no guarantee.
	ISPFn func() string

	// OnServer, if set, is called with the chosen server label as soon as it is
	// picked (before the transfers), so callers can show it mid-run.
	OnServer func(server string)

	// DirectionFn / ConnectionsFn / RetriesFn are engine-agnostic test knobs (read
	// live): which directions to run ("both"|"down"|"up"; "bidir" is treated as
	// "both" since Ookla is sequential), how many parallel connections (0 = the
	// library default), and extra attempts per transfer on a transient failure.
	// Nil -> defaults.
	DirectionFn   func() string
	ConnectionsFn func() int
	RetriesFn     func() int

	// LossFn gates the packet-loss probe (read live): a few extra seconds of UDP
	// after the transfers. Nil -> on. The Ookla analogue of Iperf.UDPFn, except
	// jitter already comes from the ping phase, so this only adds loss.
	LossFn func() bool

	// PriorDataFn, if set, reports whether this install has speed history to judge
	// a measurement against. Nil = assume it does, which is the behaviour every
	// caller had before this existed.
	//
	// It gates the FIRST run's selection (see firstRunByPing). Read live rather
	// than sampled once, because "first" stops being true after one run.
	PriorDataFn func() bool

	// BestOfFn gates "best of 3 servers" (read live): measure against the chosen
	// server AND the next two by ping, then keep the best result (see bestResult).
	// Nil -> off. Only scheduled and manual runs honour it - see RunReason.
	BestOfFn func() bool

	// Log, if set, records which servers a best-of-N run tried and which won.
	// Nil is fine - the losing runs are discarded either way.
	Log *slog.Logger

	// upRec records upload POST statuses for the run in flight, so a failed
	// upload can say WHY (see uploadRecorder). Set by RunReason; safe as a plain
	// field because the scheduler runs one measurement at a time (single-flight)
	// and best-of measures its candidates sequentially on one client.
	upRec *uploadRecorder

	// uc is the run's UserConfig, kept so a per-attempt client can be rebuilt
	// with the same location, user agent and connection count. Set by RunReason;
	// safe as a plain field for the same single-flight reason as upRec.
	uc *ookla.UserConfig

	// attemptT is the http.Transport behind the CURRENT attempt's client (see
	// freshManager), kept so the next rebuild can CloseIdleConnections on the
	// client it abandons - its keep-alive sockets would otherwise linger for the
	// library transport's full 90s IdleConnTimeout. Safe as a plain field for
	// the same single-flight reason as upRec.
	attemptT *http.Transport
}

// logf records a best-of-N step when a logger is wired, and is a no-op otherwise.
func (o *Ookla) logf(msg string, args ...any) {
	if o.Log != nil {
		o.Log.Debug(msg, args...)
	}
}

// warnf is logf at Warn, for the few conditions that must reach the default
// (Warn-threshold) ring: they change or degrade what becomes history, and the
// 2026-08-02 incident proved Debug-only evidence explains nothing after the
// fact.
func (o *Ookla) warnf(msg string, args ...any) {
	if o.Log != nil {
		o.Log.Warn(msg, args...)
	}
}

// bestOfServers is how many servers a best-of-N run measures. Each one is a full
// sequential speedtest, so this multiplies both run time and data used.
const bestOfServers = 3

// bestOfReasons are the triggers allowed to spend 3x the time and data. Reconnect
// and degraded runs are asking "is the link back", not "which server is fastest",
// and they can fire repeatedly on a flapping link; startup keeps first boot quick.
var bestOfReasons = map[string]bool{"scheduled": true, "manual": true}

// NewOokla builds an Ookla-backed tester.
func NewOokla() *Ookla {
	return &Ookla{}
}

func (o *Ookla) serverID() string {
	if o.ServerIDFn != nil {
		return o.ServerIDFn()
	}
	return ""
}

// newOoklaClient builds a fresh speedtest client that owns its http.Client.
// Without WithDoer the library reuses the process-global http.DefaultClient and
// stamps its own RoundTripper onto it - an unsynchronized write racing with
// every other DefaultClient user, and it appends the library's User-Agent to
// unrelated requests (e.g. geocoding). WithDoer must come before WithUserConfig:
// the config write lands on whatever doer is current. New still stamps the
// global once while loading its built-in default config (before the options
// run), so the tail check undoes exactly that stamp - and only that stamp.
func newOoklaClient(uc *ookla.UserConfig) *ookla.Speedtest {
	c, _ := newOoklaClientRec(uc)
	return c
}

// newOoklaClientRec is newOoklaClient plus the upload recorder wired into the
// transport chain. Only the measurement path needs the recorder; every other
// caller (server browse, search, city race) uses the plain constructor.
func newOoklaClientRec(uc *ookla.UserConfig) (*ookla.Speedtest, *uploadRecorder) {
	// New writes http.DefaultClient.Transport (the stamp) and the tail check
	// clears it; both are unsynchronized writes to a process-global. Serialize
	// them so concurrent newOoklaClient calls don't race each other on it -
	// go test -race stays clean.
	ooklaClientMu.Lock()
	defer ooklaClientMu.Unlock()
	// Install the SSRF dial guard on the library's OWN dialer before New runs.
	// UserConfig.DialerControl feeds both the tcp and ip dialers the library
	// builds (speedtest.go NewUserConfig), so this guards every destination this
	// client reaches - the upload/download transfer, the ranking ping, and the
	// catalogue fetches - not just the probes. Those destinations are all
	// THIRD-PARTY chosen (a catalogue Host, a by-ID resolve, a redirect target
	// probeEndpoint copied into s.URL), so without this a hostile entry could
	// steer the bytes-moving transfer at loopback (incl. the daemon's own UI), a
	// LAN service, or cloud metadata - the probe guard alone never covered the
	// transfer. Enforced at dial time, like probeClient, so a name that RESOLVES
	// to an internal address is caught too. probeDialControl (not probeDialGuard)
	// so allowLoopbackProbes relaxes it for loopback-served tests; only set when
	// unset, so a caller-supplied control (e.g. a future source-interface bind)
	// still wins.
	if uc != nil && uc.DialerControl == nil {
		uc.DialerControl = probeDialControl
	}
	doer := &http.Client{}
	client := ookla.New(ookla.WithDoer(doer), ookla.WithUserConfig(uc))
	if _, ok := http.DefaultClient.Transport.(*ookla.Speedtest); ok {
		http.DefaultClient.Transport = nil
	}
	// The transport New stamped carries net/http's ProxyFromEnvironment, whose
	// environment read is cached once per process. Replace it with the
	// fresh-reading equivalent that also vets the LOGICAL destination of every
	// proxied request (see guardedEnvProxy): the doer follows GET redirects
	// itself, so a ping or download 3xx re-enters the transport with a
	// third-party URL the pre-measure check never saw - and behind a proxy the
	// dial guard only ever sees the proxy's address. Direct requests keep their
	// dial-time guard. An explicit UserConfig.Proxy (never set here) keeps the
	// library's own routing.
	if uc != nil && uc.T != nil && uc.Proxy == "" {
		uc.T.Proxy = guardedEnvProxy
	}
	// WithUserConfig stamped doer.Transport = client (the library's UA-adding
	// RoundTripper over its own http.Transport), overwriting anything set before
	// New - which is why the panic containment goes on afterwards, on TOP of the
	// stamp. Every request the library makes on our behalf, transfer chunks on
	// its own worker goroutines included, then rides panicSafeTransport (see
	// there for why that matters).
	base := doer.Transport
	if ooklaTransportHook != nil {
		base = ooklaTransportHook(base)
	}
	// Beneath the panic containment, so a panic is still converted to an error
	// before it reaches us, and above nothing else - this must see the real
	// status the server returned.
	// ABOVE the panic containment, not below it. panicSafeTransport converts a
	// panic under the real transport into an error; a recorder beneath it would
	// be unwound straight past (it has no recover of its own) and would report
	// "no upload requests were issued" for a run whose POSTs all panicked.
	rec := &uploadRecorder{}
	doer.Transport = recordingTransport{
		base: panicSafeTransport{base: base, panics: &panicThrottle{}},
		rec:  rec,
	}
	return client, rec
}

// currentEndpoint rewrites a server's URL to the location the catalogue says is
// CURRENT (the Host field) instead of the legacy `url` field the library uses
// verbatim.
//
// Ookla is migrating servers behind *.prod.hosts.ooklaserver.net while `url`
// keeps pointing at the old hostname, which now answers the upload POST with a
// 307. Go will not follow that for a POST - the body (io.NopCloser over the
// library's chunk reader) has no GetBody, so it cannot be replayed - and the 3xx
// comes back to the caller. Downloads survive because a GET body IS replayable
// and Go follows the redirect; only upload breaks. That asymmetry is issues
// #17/#18: speedtest-go v1.7.10 ignored the status and reported the bytes it had
// pushed into the socket (an inflated number), v1.7.11 checks the status and
// reports N/A.
//
// Measured across 1127 catalogue entries in 38 countries: on all 408
// non-migrated servers Host == the url's host, so this is a provable NO-OP
// there; on all 719 migrated ones it differs. That partition is why this needs
// no feature flag or guard - it cannot change a server that has not moved.
// serverfleet_probe_test.go asserts the partition still holds.
func currentEndpoint(s *ookla.Server) {
	if s == nil || s.Host == "" {
		return
	}
	// Scheme stays http: every catalogue entry is http on :8080, and rewriting to
	// https breaks hosts with no working TLS listener (measured: server 1993
	// answers 200 on http and fails the TLS handshake outright).
	u := "http://" + s.Host + "/speedtest/upload.php"
	if s.URL != u {
		s.URL = u
	}
}

// currentEndpoints applies currentEndpoint across a fetched list.
func currentEndpoints(servers ookla.Servers) ookla.Servers {
	for _, s := range servers {
		currentEndpoint(s)
	}
	return servers
}

// uploadRecorder counts what actually happened on the wire during an upload
// phase. It exists because the library throws the information away: its handler
// does `if err := uploadRequest(...); err != nil { errorTimes++ }` and discards
// err, so a failed run reports "server returned N/A" with no status code, no
// attempt count, and no way to tell WHICH failure occurred.
//
// Three different faults produce that identical message:
//
//   - rejection - every POST answered non-2xx (retired endpoint, redirect, WAF)
//   - starvation - a slow uplink where no chunk finishes inside the capture
//     window, so all in-flight requests are cancelled at window close
//   - a genuinely dead link
//
// Attempt count separates them for free, and this is where it becomes visible:
// rejection makes THOUSANDS of attempts (each fails instantly), starvation makes
// exactly min(NumCPU, 8) - one per worker, none ever finishing. Measured: 2336
// vs 8.
type uploadRecorder struct {
	mu         sync.Mutex
	ceiling    int // starvation signature bound; 0 = not set
	attempts   int
	byStatus   map[int]int
	lastStatus int
	lastErr    string
}

func (r *uploadRecorder) note(status int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.byStatus == nil {
		r.byStatus = map[int]int{}
	}
	r.byStatus[status]++
	if err != nil {
		r.lastErr = err.Error()
		return
	}
	if status < 200 || status >= 300 {
		r.lastStatus = status
	}
}

// snapshot returns attempts so far and how many of them were confirmed (2xx).
// The retry loop uses the delta across one attempt to tell STARVATION (nothing
// ever completed) from REJECTION (thousands completed and were refused).
func (r *uploadRecorder) snapshot() (attempts, confirmed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for c, n := range r.byStatus {
		if c >= 200 && c < 300 {
			confirmed += n
		}
	}
	return r.attempts, confirmed
}

func (r *uploadRecorder) reset(ceiling int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts, r.byStatus, r.lastStatus, r.lastErr = 0, map[int]int{}, 0, ""
	r.ceiling = ceiling
}

// summary renders the diagnosis. Deliberately short: it is appended to an error
// that reaches the UI banner and the log line, so it has to earn its width.
func (r *uploadRecorder) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attempts == 0 {
		return "no upload requests were issued"
	}
	codes := make([]int, 0, len(r.byStatus))
	for c := range r.byStatus {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, 0, len(codes))
	for _, c := range codes {
		if c == 0 {
			parts = append(parts, fmt.Sprintf("%d transport-error", r.byStatus[c]))
			continue
		}
		parts = append(parts, fmt.Sprintf("%dx HTTP %d", r.byStatus[c], c))
	}
	out := fmt.Sprintf("%d upload attempts: %s", r.attempts, strings.Join(parts, ", "))
	// The interpretation, not just the numbers - this is the line that turns a
	// bug report into a diagnosis.
	switch {
	case r.lastStatus >= 300 && r.lastStatus < 400:
		out += "; server redirects the upload POST (Go cannot follow it - body not replayable)"
	case r.lastStatus != 0:
		out += "; server rejects the upload endpoint"
	case r.ceiling > 0 && r.attempts <= r.ceiling && r.lastErr != "":
		out += "; no attempt completed inside the capture window - the uplink is too slow for the parallel chunk set, or the server accepted and stalled"
	case r.lastErr != "":
		out += "; last transport error: " + r.lastErr
	}
	return out
}

// recordingTransport feeds the recorder. It only watches upload POSTs; download
// GETs and the ping probes ride through untouched.
type recordingTransport struct {
	base http.RoundTripper
	rec  *uploadRecorder
}

func (t recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/speedtest/upload.php") {
		return t.base.RoundTrip(req)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		t.rec.note(0, err)
		return resp, err
	}
	t.rec.note(resp.StatusCode, nil)
	return resp, err
}

// starvationCeiling bounds what counts as the starvation signature: at most one
// attempt per worker, i.e. nothing ever finished and came back for more.
//
// Derived, never constant. The library runs min(NumCPU, uploadMaxWorkers)
// uploaders by default, but MaxConnections overrides it and settings allow up to
// MaxOoklaConnections (16). A fixed 12 silently excluded every 13-16 connection
// configuration from the rescue AND from the diagnosis - a starved 16-stream
// transfer made ~16 attempts, missed the predicate, and retried into the same
// starvation. The slack absorbs a straggler that started just before the window
// closed.
func starvationCeiling(conns int) int {
	// SetNThread(n) for n >= 1 sets BOTH nThread and uploadMaxWorkers to n, so a
	// configured value is the worker count outright - the 8 cap applies only to
	// the default path, where the library uses min(NumCPU, 8). Clamping a
	// configured 16 down to 8 was exactly the bug this function exists to fix.
	workers := conns
	if conns < 1 {
		workers = runtime.NumCPU()
		if workers > uploadDefaultWorkerCap {
			workers = uploadDefaultWorkerCap
		}
	}
	return workers + starvationSlack
}

const (
	// uploadDefaultWorkerCap mirrors the library's uploadMaxWorkers default,
	// which applies ONLY when no connection count is configured.
	uploadDefaultWorkerCap = 8
	starvationSlack        = 4
)

// configuredConnections is the parallelism the user asked for: 0 means "library
// default", which is what SetNThread(0) restores.
func (o *Ookla) configuredConnections() int {
	if o.ConnectionsFn == nil {
		return 0
	}
	return o.ConnectionsFn()
}

// runConnections is the parallelism THIS run's transfers actually use. RunReason
// snapshots the live ConnectionsFn once into o.uc.MaxConnections, and every
// freshManager reuses that snapshot; the starvation ceiling and diagnosis must
// be derived from the SAME value, not a second live read. Otherwise a setting
// saved mid-run (discovery or the download phase can be in flight when the user
// lowers the count) would size the ceiling for a different worker count than the
// upload ran with - so a genuinely starved run slips past the rescue predicate
// (it retries into the same starvation) and is mislabelled a transport error
// instead of starvation. Direct measure() callers (tests, non-RunReason) leave
// o.uc nil, where the live configured value is the right stand-in.
func (o *Ookla) runConnections() int {
	if o.uc != nil {
		return o.uc.MaxConnections
	}
	return o.configuredConnections()
}

// uploadRescueSettle is how long the rescue waits for the failed attempt's
// transfers to drain before measuring. Measured: they linger ~5 s past their
// window close.
const uploadRescueSettle = 5 * time.Second

// ---- HTTP Legacy Fallback health -------------------------------------------
//
// /speedtest/upload.php is not part of the OoklaServer daemon. It ships in the
// optional "HTTP Legacy Fallback" bundle an operator installs separately (it
// wants a web server; Apache+PHP is typical). The daemon's own native protocol
// on TCP/UDP 5060+8080 is what the official Speedtest CLI uses and needs none of
// it. speedtest-go, and therefore we, can ONLY talk to the fallback.
//
// So a sizeable slice of the catalogue is unusable to us while being perfectly
// healthy for Ookla's own client: measured 2026-08-11, ~12% of migrated and ~18%
// of non-migrated servers answer 500, and the official CLI measures those same
// hosts at 170-290 Mbps. This is attrition (a component never installed, or a
// web server that rotted), NOT a deprecation programme - migrated servers are
// HEALTHIER than non-migrated ones, which is the opposite of what a planned
// retirement would look like.
//
// Ranking cannot see any of this on its own. speedtest-go's HTTPPing GETs
// /speedtest/latency.txt - the same bundle - and NEVER CHECKS THE STATUS CODE,
// so a server answering 500 to everything returns a fast, plausible latency and
// sorts normally. Left alone it wins the race, and every run against it fails.
//
// The check is therefore just reading the status of a request ranking already
// makes. Measured over 200 random servers, latency.txt and upload.php agreed on
// health 200/200 - no misses, no false exclusions - so the cheap GET stands in
// for the POST.
type fallbackVerdict struct {
	state   endpointState
	expires time.Time
	fails   int // consecutive definite failures; see fallbackHealth
}

// probeDialGuard refuses to connect anywhere a speedtest server has no business
// sending us. Both probes act on a destination a THIRD PARTY chose - the
// catalogue's host, or a Location header from a server that may be compromised
// or simply listed by anyone - and probeEndpoint follows that redirect with a
// POST. Without this, a hostile entry could steer the daemon at 127.0.0.1:9000
// (its own UI), a private LAN service, or cloud metadata.
//
// Enforced at DIAL time rather than by parsing the URL, so a name that resolves
// to a blocked address is caught too - URL inspection alone loses to DNS
// rebinding. Mirrors internal/notify's dialGuard, widened to loopback and
// private ranges because unlike a webhook these destinations are never
// legitimate for a speedtest server.
func probeDialGuard(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if isConfiguredProxy(address) {
			return nil
		}
		return fmt.Errorf("blocked probe to unresolved address %q", address)
	}
	if err := blockedIP(ip); err != nil {
		// The ONE internal endpoint a dial may legitimately reach: the
		// operator's own forward proxy. HTTP(S)_PROXY are trusted local
		// configuration, and when they are set net/http routes every library
		// request THROUGH that endpoint, so refusing its (often loopback or
		// LAN) address broke every catalogue fetch, ranking ping and transfer
		// behind such a proxy. Exact host:port only - the rest of the proxy's
		// network stays refused. Consulted only on a refusal, so the public
		// fast path never reads the environment.
		if isConfiguredProxy(address) {
			return nil
		}
		return err
	}
	return nil
}

// blockedIP is the guard's verdict on one resolved IP, shared by the dial-time
// guard and the proxied-destination pre-check (see guardProxiedDestination).
func blockedIP(ip net.IP) error {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback():
		return fmt.Errorf("blocked probe to loopback %s", ip)
	case ip.IsPrivate():
		return fmt.Errorf("blocked probe to private address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return fmt.Errorf("blocked probe to link-local %s", ip)
	case ip.IsUnspecified(), ip.IsMulticast():
		return fmt.Errorf("blocked probe to %s", ip)
	}
	for _, n := range blockedProbeNets {
		if n.Contains(ip) {
			return fmt.Errorf("blocked probe to reserved address %s", ip)
		}
	}
	return nil
}

// blockedProbeNets are ranges net.IP.IsPrivate does NOT cover but that a
// speedtest server still has no business steering us to. Chiefly RFC 6598
// shared address space (100.64.0.0/10) - Tailscale's default CGNAT range and
// common in the self-host/homelab deployments pingularity targets, so a
// redirect into it would otherwise sail past the guard on its way to an
// internal tailnet service. The reserved documentation/benchmark blocks come
// along for the same reason: never a legitimate speedtest destination. Parsed
// once; the loopback/link-local/RFC1918/multicast cases stay on net.IP's own
// predicates above. To4() has already normalized IPv4-mapped forms (e.g.
// ::ffff:100.64.0.5), so Contains catches those too.
var blockedProbeNets = func() []*net.IPNet {
	nets := make([]*net.IPNet, 0, 7)
	for _, cidr := range []string{
		"100.64.0.0/10",   // RFC 6598 shared address space (CGNAT / Tailscale)
		"192.0.0.0/24",    // RFC 6890 IETF protocol assignments (DS-Lite CPE, ...)
		"192.0.2.0/24",    // RFC 5737 TEST-NET-1
		"198.18.0.0/15",   // RFC 2544 benchmarking
		"198.51.100.0/24", // RFC 5737 TEST-NET-2
		"203.0.113.0/24",  // RFC 5737 TEST-NET-3
		"240.0.0.0/4",     // RFC 1112 class E reserved (incl. 255.255.255.255)
	} {
		if _, n, err := net.ParseCIDR(cidr); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

// ---- Operator proxy handling -------------------------------------------------
//
// HTTP(S)_PROXY are operator-trusted local configuration. Two sides:
//
//   - the dial guard must ALLOW the configured proxy endpoint (see
//     probeDialGuard), or nothing works behind a loopback/LAN proxy;
//   - a proxied request's dial only ever names the proxy - the real
//     destination rides inside the request (CONNECT target / absolute URL) -
//     so the LOGICAL destination must be validated separately (see
//     guardProxiedDestination), or a hostile catalogue entry naming an
//     internal host tunnels straight past the dial guard.
//
// The trust set is exactly what net/http consults per request, no wider.
// ALL_PROXY deliberately is NOT here: net/http's ProxyFromEnvironment never
// reads it (x/net/http/httpproxy reads HTTP_PROXY, HTTPS_PROXY and NO_PROXY
// only), so no request can ever ride an ALL_PROXY endpoint - allowing dials to
// one was pure attack surface. Which requests are EXEMPT from proxying, and
// which are refused outright under CGI, do follow the same per-request rules
// net/http applies - scheme selection, NO_PROXY, the loopback/localhost
// exemption (see envProxyEndpoint) and the CGI check (see cgiProxyRefusal) -
// except read FRESH from the environment on every call, because net/http's own
// copy is cached once per process and t.Setenv-driven tests cannot reach it.
//
// What follows net/http is those exemption rules, NOT the whole decision: this
// is not a drop-in ProxyFromEnvironment, and anyone changing either side needs
// to know it. net/http has two outcomes, proxy or direct. There are three
// here, and the third is a refusal: a value this daemon cannot use - an
// unsupported scheme, or one that names no host:port (see parseProxyEnvURL) -
// fails every request that would have ridden it. httpproxy has no third
// outcome, because it validates no scheme at all and config.init drops a parse
// error and leaves that variable unset, sending the same request direct.
// Failing instead is deliberate: the operator configured a proxy, and traffic
// leaving by a route they did not choose - direct, or through an endpoint the
// "http://" + value retry invented - is the harm this section exists to
// prevent. Divergence details sit on parseProxyEnvURL (which values are
// usable) and envProxyEndpoint (routing).

// proxyEnvVars are the variables net/http's ProxyFromEnvironment consults,
// uppercase preferred - the same precedence it applies.
var proxyEnvVars = [][2]string{
	{"HTTP_PROXY", "http_proxy"},
	{"HTTPS_PROXY", "https_proxy"},
}

// getenvEither reads one proxy variable in both spellings, uppercase preferred.
func getenvEither(upper, lower string) string {
	if v := os.Getenv(upper); v != "" {
		return v
	}
	return os.Getenv(lower)
}

// inCGI reports what x/net/http/httpproxy infers from REQUEST_METHOD: this
// process is serving a CGI request, where "HTTP_PROXY" is not operator config
// at all - it is the caller's Proxy request header, which the CGI convention
// maps into the environment. net/http therefore ERRORS an http request instead
// of honouring it (see cgiProxyRefusal), and never routes one through that
// endpoint, so the dial guard's trust set drops it too.
func inCGI() bool { return os.Getenv("REQUEST_METHOD") != "" }

// cgiProxyRefusal is httpproxy's CGI check, in its order: it fires on the
// http scheme alone, BEFORE the loopback and NO_PROXY exemptions, whenever
// HTTP_PROXY parses to a usable endpoint. Matching that order matters - a
// request net/http refuses must not be sent here just because NO_PROXY happens
// to cover its host.
func cgiProxyRefusal(u *url.URL) error {
	if !inCGI() || strings.ToLower(u.Scheme) != "http" {
		return nil
	}
	// A value that yields no usable endpoint carries no request anywhere, so
	// there is no egress choice here to take away from a CGI caller. httpproxy
	// stands down for the same reason: its CGI branch needs a non-nil parsed
	// proxy, and config.init leaves that nil for any value parseProxy rejected.
	//
	// It does NOT follow that the caller always hears something instead.
	// envProxyEndpoint applies the loopback/localhost and NO_PROXY exemptions
	// BEFORE it parses the value, so on an exempt destination an unusable value
	// produces no error at all and the request goes direct in silence - while a
	// USABLE value is CGI-refused on that same destination, this check being
	// deliberately ahead of those exemptions. Measured both ways in
	// TestProxyUnusableValueRefusesOnlyWhatWouldHaveBeenProxied: an operator
	// who typo'd HTTP_PROXY hears about it only on the requests that would have
	// been proxied.
	if p, err := parseProxyEnvURL(getenvEither("HTTP_PROXY", "http_proxy")); err != nil || p == nil {
		return nil
	}
	return errors.New("refusing to use HTTP_PROXY value in CGI environment; see golang.org/s/cgihttpproxy")
}

// proxyAddrs returns the configured proxy endpoints as host:port, read fresh
// from the environment on every call (a few Getenvs - cheap, and it keeps
// t.Setenv-driven tests honest where net/http's own once-cached copy is not).
func proxyAddrs() []string {
	var out []string
	cgi := inCGI()
	for _, kv := range proxyEnvVars {
		if cgi && kv[0] == "HTTP_PROXY" {
			continue // no request rides it in CGI; see inCGI
		}
		v := getenvEither(kv[0], kv[1])
		if v == "" {
			continue
		}
		hp := proxyHostPort(v)
		if hp == "" {
			continue
		}
		dup := false
		for _, have := range out {
			if have == hp {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, hp)
		}
	}
	return out
}

// leadingScheme returns the scheme a proxy value spells out in the
// "scheme://host" form, lowercased, or "" when the value names none.
//
// The "://" is the whole discriminator, and it is judged on the spelling
// rather than on RFC 3986's scheme grammar. url.Parse reports a scheme for the
// bare "host:port" form too ("proxy.example:3128" parses as scheme
// "proxy.example", "ftp:3128" as scheme "ftp") and those are hosts, so the
// colon alone proves nothing; conversely a value url.Parse refuses to read as
// a scheme was still WRITTEN as one, and reading it as a host is the mistake
// this exists to stop. Measured: "1http://proxy.example:21" - a scheme may not
// start with a digit, so url.Parse takes the whole value for a path - came
// back from the "http://" + value retry as the endpoint "1http:80". A
// host:port value never contains "//" at all, so nothing legitimate is caught
// here.
func leadingScheme(v string) string {
	i := strings.Index(v, "://")
	if i <= 0 || strings.ContainsAny(v[:i], "/@:") {
		return ""
	}
	return strings.ToLower(v[:i])
}

// parseProxyEnvURL parses one proxy environment value into the endpoint the
// daemon may route through, or an error naming what is wrong with it. It is
// deliberately NOT httpproxy's parseProxy (see the section comment above); the
// two differ in two respects, both of them here.
//
// First: the "http://" + value retry, which is what makes the bare "host:port"
// spelling - the form most operators write - mean http://host:port. httpproxy
// retries whenever the first parse fails, or yields no scheme, or yields no
// host, and that is far too wide, because url.Parse reads the SCHEME NAME of
// the retried string as its host: the authority of
// "http://" + "ftp://proxy.example:notaport" ends at the next "/", leaving host
// "ftp:" - hostname "ftp", no port. Measured, every one of these came back as
// an endpoint nobody configured, and the dial guard (whose trust set comes from
// this same call) took each one for the operator's own proxy:
//
//	"ftp://proxy.example:21"       -> ftp:80   (the original report, and the
//	                                            only one the clean-parse scheme
//	                                            check below ever caught)
//	"ftp://proxy.example:notaport" -> ftp:80   (first parse fails, so that
//	                                            check never sees it)
//	"ftp:///proxy.example"         -> ftp:80   (parses clean, but host is empty)
//	"http://"                      -> http:80
//	"unix:///var/run/proxy.sock"   -> unix:80
//	"http://proxy.example:3128 "   -> http:80  (one trailing space, and the
//	                                            configured proxy is never used)
//
// So a value that spells out "scheme://" is used exactly as written or
// refused - never retried. Values with no scheme still get the retry, which is
// what the schemeless forms need: "192.168.1.10:3128" and "[::1]:3128" do not
// parse at all, and "user:pass@proxy.example:3128" parses with an empty host.
// The retry additionally requires a non-empty HOSTNAME, where httpproxy takes
// any parse at all: ":3128" otherwise comes back as an endpoint (host ":3128")
// that proxyHostPort cannot express - measured, it yields "" - so net/http
// would be handed a proxy with no matching entry in the dial guard's trust
// set, and the operator would see a blocked dial rather than the real problem.
//
// Second: the outcome for a value this daemon cannot use. httpproxy discards
// the parse error in config.init and connects DIRECT; an unsupported scheme it
// does not detect at all. Here both are errors - an operator who configured a
// proxy must not have the traffic quietly sent out by a route they did not
// choose, and a typo'd scheme must be visible as a typo. No endpoint comes
// back either way, so an unusable value can never enter the trust set.
//
// One ambiguity is left alone, because it is genuine: "mailto:ops@example.com"
// has the same shape as "user:pass@proxy.example:3128" and is read the same
// way, credentials plus host. Nothing is invented there - the host is written
// in the value - and net/http reads it identically.
//
// An empty value means no proxy is configured and is not an error.
func parseProxyEnvURL(v string) (*url.URL, error) {
	if v == "" {
		return nil, nil
	}
	u, err := url.Parse(v)
	// Hostname(), not Host: url.Parse fills Host with ":3128" for a value like
	// "http://:3128", so gating on Host lets a scheme-with-no-host through as a
	// usable endpoint. It is not usable - proxyHostPort has no host to record, so
	// the dial guard's trust set comes out EMPTY, and an empty trust set is how
	// guardProxiedDestination and guardedServers decide there is no proxy to
	// police. Both then stand down for the whole run. A typo would not merely fail
	// to route, it would silently disarm the guard, so this shape falls through to
	// the refusal below with every other value that names a scheme it cannot use.
	if err == nil && u.Scheme != "" && u.Hostname() != "" {
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
			return u, nil
		}
		return nil, fmt.Errorf("unsupported proxy scheme %q in proxy address %q: use http, https, socks5 or socks5h, or a bare host:port", u.Scheme, v)
	}
	// The value named a scheme and still yielded no endpoint, so there is
	// nothing to fall back to: retrying it here is what fabricated the
	// endpoints listed above.
	if s := leadingScheme(v); s != "" {
		if err != nil {
			return nil, fmt.Errorf("invalid proxy address %q: %v (a value spelled %s:// is used as written, never read as a bare host:port)", v, err, s)
		}
		return nil, fmt.Errorf("invalid proxy address %q: the %q scheme names no host to connect to", v, s)
	}
	if retry, rerr := url.Parse("http://" + v); rerr == nil && retry.Hostname() != "" {
		return retry, nil
	}
	return nil, fmt.Errorf("invalid proxy address %q: not a usable host:port", v)
}

// proxyHostPort extracts host:port from one proxy environment value, with a
// missing port getting the scheme's default (80/443/1080).
func proxyHostPort(v string) string {
	u, err := parseProxyEnvURL(v)
	if err != nil || u == nil {
		return ""
	}
	host, port := u.Hostname(), u.Port()
	if host == "" {
		return ""
	}
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

// noProxyExcludes reports whether NO_PROXY exempts host:port from proxying,
// matching x/net/http/httpproxy's rules: "*" exempts everything; a CIDR or IP
// entry matches an IP-literal host; a domain entry matches the host and its
// subdomains ("example.com"), subdomains only when it leads with a dot or
// "*." (".example.com" / "*.example.com"); an entry's :port, when present,
// must match the request's.
func noProxyExcludes(host, port string) bool {
	nv := getenvEither("NO_PROXY", "no_proxy")
	if nv == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	ip := net.ParseIP(host)
	for _, entry := range strings.Split(nv, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		if _, cidr, err := net.ParseCIDR(entry); err == nil {
			if ip != nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		ehost, eport := entry, ""
		if h, p, err := net.SplitHostPort(entry); err == nil {
			ehost, eport = h, p
		}
		if ehost == "" || (eport != "" && eport != port) {
			continue
		}
		if eip := net.ParseIP(ehost); eip != nil {
			if ip != nil && eip.Equal(ip) {
				return true
			}
			continue
		}
		if strings.HasPrefix(ehost, "*.") {
			ehost = ehost[1:]
		}
		matchHost := false
		if !strings.HasPrefix(ehost, ".") {
			matchHost, ehost = true, "."+ehost
		}
		if strings.HasSuffix(host, ehost) || (matchHost && host == ehost[1:]) {
			return true
		}
	}
	return false
}

// envProxyEndpoint returns the proxy this daemon routes a request for u
// through, nil for a direct connection, or an error for a configured value it
// cannot use. Which requests are exempt is net/http's rule set, read fresh:
// HTTP_PROXY for http, HTTPS_PROXY for https, nothing for other schemes, never
// for localhost/loopback destinations, and never for a NO_PROXY-excluded host.
// The CGI refusal, httpproxy's one other rule, sits in cgiProxyRefusal because
// it returns an error rather than a routing decision.
//
// The ENDPOINT is not ProxyFromEnvironment's, and neither is the third
// outcome, so this is not a drop-in replacement for it (see the section
// comment above): parseProxyEnvURL refuses values httpproxy accepts - an
// unsupported scheme, and anything that becomes a URL only by rewriting a
// scheme the operator wrote into a hostname - and returns those as errors
// where httpproxy discards the value and connects direct.
//
// One divergence inside the exemption rules themselves remains, deliberately:
// httpproxy punycodes both sides of the NO_PROXY comparison (idnaASCII, from
// canonicalAddr and its own init), so
// there a Unicode entry matches a punycode host and vice versa, while here the
// two must be written the same way. Closing it means taking on
// golang.org/x/net/idna - a dependency this module does not otherwise have -
// for a mixed-spelling case, and the divergence is fail-safe in both
// directions: the request rides the proxy where net/http would send it direct,
// and a proxied request is destination-vetted either way. Pinned by
// TestEnvProxyNoProxyIDNAFormsMustMatch.
//
// The error is a value this request would have had to ride and cannot (an
// unsupported scheme, or one that will not parse - see parseProxyEnvURL), and
// it is reached only after every exemption above: a request the environment
// sends direct anyway, and a request whose scheme selects the OTHER variable,
// are unaffected by a broken one.
func envProxyEndpoint(u *url.URL) (*url.URL, error) {
	var v string
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "http":
		v = getenvEither("HTTP_PROXY", "http_proxy")
	case "https":
		v = getenvEither("HTTPS_PROXY", "https_proxy")
	default:
		return nil, nil
	}
	if v == "" {
		return nil, nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || host == "localhost" {
		return nil, nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil, nil
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if noProxyExcludes(host, port) {
		return nil, nil
	}
	return parseProxyEnvURL(v)
}

// guardedEnvProxy is the Proxy function on every HTTP client this file builds:
// the probes' client and the transport the library stamps for the measurement
// client alike (see probeClient, newOoklaClientRec). A proxied request's dial
// only ever names the proxy, so the LOGICAL destination is vetted here, on
// EVERY request - redirect hops included, because the client re-enters the
// transport (and therefore this function) per hop - before anything is dialed.
// A direct request returns (nil, nil) and keeps its dial-time guard; the
// vetting itself stands down with the loopback relaxation exactly as the dial
// guard does (guardProxiedDestination is inert when probeDialControl is nil).
func guardedEnvProxy(req *http.Request) (*url.URL, error) {
	if err := cgiProxyRefusal(req.URL); err != nil {
		return nil, err
	}
	// Vet the destination BEFORE deciding how to reach it, not after. Routing
	// first left one hole open: the dial guard has to EXEMPT the configured
	// proxy address (a proxied dial only ever names the proxy), so a hostile
	// catalogue entry or redirect naming the proxy's OWN host:port routed
	// DIRECT - loopback, or NO_PROXY-excluded - skipped this check entirely,
	// and was then waved through the dial guard by that same exemption. The
	// daemon delivered an origin-form request, attacker-chosen path and all, to
	// a service on the operator's machine. Judging the destination on what it
	// IS, before how we route it, closes that without weakening the exemption
	// the proxy hop genuinely needs. Inert unless a proxy is configured (see
	// guardProxiedDestination), so a direct-only install is unaffected.
	if err := guardProxiedDestination(req.Context(), req.URL.Host); err != nil {
		return nil, err
	}
	return envProxyEndpoint(req.URL)
}

// isConfiguredProxy reports whether a dial address is one of the operator's
// configured proxy endpoints. Dialer.Control sees the POST-DNS ip:port, so a
// proxy configured by name is compared against both the name:port form and the
// name's resolved addresses.
func isConfiguredProxy(address string) bool {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	dip := net.ParseIP(host)
	for _, hp := range proxyAddrs() {
		phost, pport, err := net.SplitHostPort(hp)
		if err != nil || pport != port {
			continue
		}
		if strings.EqualFold(phost, host) {
			return true
		}
		if pip := net.ParseIP(phost); pip != nil {
			if dip != nil && pip.Equal(dip) {
				return true
			}
			continue
		}
		if dip == nil {
			continue
		}
		for _, rip := range resolvedProxyIPs(phost) {
			if rip.Equal(dip) {
				return true
			}
		}
	}
	return false
}

// proxyResolveTTL bounds the proxy-hostname cache below: long enough that a
// refused dial does not pay a lookup every time, short enough that a proxy
// that moves is picked up promptly.
const proxyResolveTTL = time.Minute

var (
	proxyResolveMu    sync.Mutex
	proxyResolveCache = map[string]proxyResolved{}
)

type proxyResolved struct {
	ips     []net.IP
	expires time.Time
}

// resolvedProxyIPs resolves a proxy HOSTNAME from the environment, cached (see
// proxyResolveTTL). The key set is operator-controlled - at most one host per
// proxy variable - so the cap below is belt-and-braces, not a working bound.
// A failed lookup is cached too: a dead proxy name would otherwise re-resolve
// on every refused dial.
func resolvedProxyIPs(host string) []net.IP {
	now := time.Now()
	proxyResolveMu.Lock()
	if e, ok := proxyResolveCache[host]; ok && now.Before(e.expires) {
		proxyResolveMu.Unlock()
		return e.ips
	}
	proxyResolveMu.Unlock()
	ips, err := net.LookupIP(host)
	if err != nil {
		ips = nil
	}
	proxyResolveMu.Lock()
	if len(proxyResolveCache) >= 16 {
		clear(proxyResolveCache)
	}
	proxyResolveCache[host] = proxyResolved{ips: ips, expires: now.Add(proxyResolveTTL)}
	proxyResolveMu.Unlock()
	return ips
}

// lookupDestIPs resolves a proxied request's destination. A var so the guard's
// own tests can count and script resolutions without touching real DNS.
var lookupDestIPs = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// destResolveTTL bounds how long one destination's resolution is reused (see
// resolveProxiedDest). A var only so tests can shrink it.
var destResolveTTL = 30 * time.Second

// destResolveMax caps the memo. Sized for a catalogue pass (guardedServers
// vets every entry it fetches, the city race a pool per origin), and cleared
// wholesale rather than evicted by age - the entries are cheap to rebuild and
// the alternative is bookkeeping nobody reads.
const destResolveMax = 512

var (
	destResolveMu    sync.Mutex
	destResolveCache = map[string]destResolved{}
)

type destResolved struct {
	ips     []net.IP
	err     error
	expires time.Time
}

// flushDestResolveCache empties the memo. Tests only: a scripted resolver in
// one test must not answer for the previous one's names.
func flushDestResolveCache() {
	destResolveMu.Lock()
	clear(destResolveCache)
	destResolveMu.Unlock()
}

// resolveProxiedDest resolves a proxied request's destination host, memoized
// for destResolveTTL.
//
// The VETTING stays per request - that is the security property, since each
// redirect hop re-enters the transport with a new URL - but re-RESOLVING put a
// resolver round-trip inline with the bytes being timed: a proxied transfer
// issues one request per chunk (this package's own upload harness logs ~2,300
// POSTs in ~4.5s, all to one hostname), so the guard hammered the resolver and
// its added latency landed inside the measured throughput. The memo keeps the
// same verdict for the same name and costs the accuracy of a name that changes
// mid-window - already the accepted residual for proxied requests, whose
// destination the proxy resolves separately from us anyway (see
// docs/security-model.md).
//
// Both outcomes are memoized: a refusal re-resolving would hammer the resolver
// exactly as hard as an allowance. A ctx-driven failure is NOT, because it
// describes the run that ended rather than the host, and caching it would
// refuse that destination for the rest of the window.
func resolveProxiedDest(ctx context.Context, host string) ([]net.IP, error) {
	destResolveMu.Lock()
	e, ok := destResolveCache[host]
	destResolveMu.Unlock()
	if ok && time.Now().Before(e.expires) {
		return e.ips, e.err
	}
	// Resolved OUTSIDE the lock: holding it would queue every proxied request
	// in the process behind one slow lookup, which is the latency this memo
	// exists to remove. Concurrent first-callers for the same host can
	// duplicate that one lookup - bounded by the transfer's worker count, and
	// every request after it hits the memo.
	ips, err := lookupDestIPs(ctx, host)
	if err != nil && ctx.Err() != nil {
		return ips, err
	}
	destResolveMu.Lock()
	if len(destResolveCache) >= destResolveMax {
		clear(destResolveCache)
	}
	destResolveCache[host] = destResolved{ips: ips, err: err, expires: time.Now().Add(destResolveTTL)}
	destResolveMu.Unlock()
	return ips, err
}

// guardProxiedDestination validates the LOGICAL destination of a request that
// will traverse the operator's proxy: internal IP-literals are refused
// outright, hostnames are resolved here and every address checked before the
// name is used. Our lookup and the proxy's are two separate resolutions, so a
// DNS-rebinding window remains (documented in docs/security-model.md) - the
// literal check, which is what a catalogue entry realistically carries, has no
// such window. A hostname that does not resolve is refused rather than waved
// through: allowing it would let a split-horizon name pass unvetted.
//
// hostport is "host" or "host:port". Inert when no proxy is configured - every
// dial then carries the real destination ip:port and probeDialGuard covers it -
// and under the same relaxation the dial guard honours (probeDialControl set
// nil by allowLoopbackProbes), so loopback-served tests keep working.
func guardProxiedDestination(ctx context.Context, hostport string) error {
	if probeDialControl == nil || len(proxyAddrs()) == 0 {
		return nil
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "" {
		return fmt.Errorf("blocked proxied request with empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		return blockedIP(ip)
	}
	ips, err := resolveProxiedDest(ctx, host)
	if err != nil {
		// Fail closed, and say why in the terms the operator can act on. A
		// proxied request names its destination only INSIDE the request, so the
		// dial guard cannot see it - resolving here is the only way to know
		// whether the proxy is about to be pointed at something internal. When
		// the name will not resolve locally we cannot tell, and guessing "fine"
		// is how a hostile catalogue entry reaches a LAN service.
		//
		// The honest cost: a proxy-only network with no local resolver (a
		// socks5h setup where DNS is meant to happen AT the proxy) cannot vet
		// anything, so speedtests stop rather than run unchecked. That is the
		// intended trade, but it is invisible without saying so - the symptom is
		// otherwise "every server refused" with no cause.
		return fmt.Errorf("blocked proxied request to %q: its address could not be resolved locally, so the daemon cannot tell whether the proxy would be pointed at an internal service (a proxy-only network with no local DNS hits this; give the daemon a resolver, or set NO_PROXY for destinations it should reach directly): %w", host, err)
	}
	for _, ip := range ips {
		if e := blockedIP(ip); e != nil {
			return fmt.Errorf("host %q resolves internal: %w", host, e)
		}
	}
	return nil
}

// serverDestination is the network destination a server's requests will name:
// the catalogue Host when present (currentEndpoint copies it into s.URL, and
// the packet-loss analyzer dials it), else the URL's own host.
func serverDestination(s *ookla.Server) string {
	if s == nil {
		return ""
	}
	if s.Host != "" {
		return s.Host
	}
	if u, err := url.Parse(s.URL); err == nil {
		return u.Host
	}
	return ""
}

// guardedServers drops catalogue entries whose logical destination a proxied
// request must not name (see guardProxiedDestination): with a proxy configured
// the ranking pings and transfers would otherwise carry a hostile entry's
// internal Host through the proxy, past the dial guard. Without one the list
// passes through untouched - direct dials stay covered at dial time.
//
// Both destinations an entry carries are vetted, not just serverDestination's
// pick: the city race pings s.URL WITHOUT the currentEndpoints rewrite that
// makes URL and Host name the same endpoint on the fetchServerList path, so a
// benign Host paired with a hostile URL would otherwise keep its ping.
func guardedServers(ctx context.Context, servers ookla.Servers) ookla.Servers {
	if probeDialControl == nil || len(proxyAddrs()) == 0 {
		return servers
	}
	out := make(ookla.Servers, 0, len(servers))
	for _, s := range servers {
		dest := serverDestination(s)
		err := guardProxiedDestination(ctx, dest)
		if err == nil {
			if u, uerr := url.Parse(s.URL); uerr == nil && u.Host != "" && u.Host != dest {
				err = guardProxiedDestination(ctx, u.Host)
			}
		}
		if err != nil {
			stats.Inc("speed.server_dest_blocked")
			slog.Debug("dropping speedtest server with blocked destination",
				"server_id", s.ID, "err", err)
			continue
		}
		out = append(out, s)
	}
	return out
}

// rawOriginFetch is the city race's pool fetch as cityrace.go declared it,
// captured before guardedFetchOriginServers is spliced in front (see init
// below). Its own tests keep stubbing fetchOriginServers wholesale - their
// fakes are the servers under test - so the splice must not sit between a
// stub and the race.
var rawOriginFetch func(context.Context, Origin) (ookla.Servers, error)

// guardedFetchOriginServers is rawOriginFetch behind the proxied-destination
// filter. The city race fetches its per-origin pools through its own seam
// (fetchOriginServers) rather than fetchServerList, and racePing then GETs
// each entry's URL - so without this splice a hostile catalogue entry naming
// an internal host got its ranking probe carried through the operator's proxy,
// past the dial guard, on this one path. A named function rather than a
// closure so a test can assert the production wiring by identity.
func guardedFetchOriginServers(ctx context.Context, o Origin) (ookla.Servers, error) {
	servers, err := rawOriginFetch(ctx, o)
	return guardedServers(ctx, servers), err
}

func init() {
	rawOriginFetch = fetchOriginServers
	fetchOriginServers = guardedFetchOriginServers
}

// probeDialControl is the dial guard the probes AND the measurement client
// install (see probeClient and newOoklaClientRec). A package var for the same
// reason as ooklaPing and fetchServerList: the offline tests serve their fakes
// on loopback, which the guard exists to refuse, so allowLoopbackProbes relaxes
// this one var to cover both the probes and the real transfer. Production never
// reassigns it, and TestProbeRefusesInternalDestinations exercises the real
// probeDialGuard directly so relaxing this in a test cannot hide a regression
// in the guard itself.
var probeDialControl = probeDialGuard

// probeClient is the only client the endpoint probes use. Its dialer refuses
// internal destinations (see probeDialGuard) on every hop, redirects included -
// enforced at dial time, so a hostname that RESOLVES to an internal address is
// caught too. Proxied requests take the same route the transfers do.
func probeClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Proxied exactly when net/http would proxy the request, with the
			// logical destination vetted per hop (see guardedEnvProxy). Built
			// bare, this transport dialed DIRECT even when the transfer client
			// rode the operator's proxy: in a proxy-only network every fallback
			// check and by-ID redirect probe timed out as endpointUnknown with
			// zero proxy requests, so a migrated pinned server kept its stale
			// URL and the proxied upload still hit the non-replayable 307 -
			// issues #17/#18, alive behind proxies.
			Proxy:       guardedEnvProxy,
			DialContext: (&net.Dialer{Timeout: timeout, Control: probeDialControl}).DialContext,
			// Every caller builds this client for a single request and drops it,
			// so a kept-alive socket can never be reused - it would only sit in
			// the abandoned transport's idle pool (zero IdleConnTimeout: forever)
			// with its read/write-loop goroutines, until the REMOTE peer closes.
			// rankedServers probes dozens of servers per scheduled pass and
			// annotateFallback fans out over a whole picker page, so in a
			// long-lived daemon that stranded fds and goroutines without bound.
			DisableKeepAlives: true,
		},
	}
}

const (
	// fallbackTTL: a missing component is stable, but an operator CAN install it,
	// so this expires rather than being permanent. Long enough that a scheduled
	// run never re-probes the same server twice in a day.
	fallbackTTL = 12 * time.Hour
	// fallbackUnknownTTL: a probe that failed at transport level proves nothing
	// about the endpoint - retry it soon rather than trusting the non-answer.
	fallbackUnknownTTL = 10 * time.Minute
	// fallbackProbeTimeout keeps one silent server from stalling selection. The
	// probes run concurrently with the ping race, so this is a ceiling on the
	// phase, not a per-server addition to it.
	fallbackProbeTimeout = 6 * time.Second
	// annotateFallbackBudget caps the picker's total annotation wait (see
	// annotateFallback). Short on purpose: a decorated list arriving late is
	// worse than an undecorated one arriving now.
	annotateFallbackBudget = 3 * time.Second
	// fallbackStrikes is how many consecutive definite failures retire a server.
	// One is not enough: transient 5xx and rate limiting are indistinguishable
	// from a missing bundle in a single response.
	fallbackStrikes = 2
	// fallbackMapCap bounds the cache the way plMapCap bounds the loss map:
	// cycling through many servers must not grow it forever.
	fallbackMapCap = 512
)

// fbFlight is one in-flight fallback probe. It exists because the expiry
// check, fails snapshot and network probe cannot all run under fbMu, and
// unserialized they were check-then-act: two concurrent callers for one
// expired ID both probed and both wrote fails=prevFails+1 from the same stale
// snapshot - losing a consecutive strike (so two-strike retirement never
// accumulated under concurrency) and doubling probe traffic. The first caller
// registers a flight and probes; later callers wait on done and reuse st. st
// is written before done is closed, so the close is the happens-before edge
// waiters read through.
type fbFlight struct {
	done chan struct{}
	st   endpointState // valid once done is closed
}

var (
	fbMu      sync.Mutex
	fbMap     = map[string]fallbackVerdict{} // server ID -> verdict
	fbProbing = map[string]*fbFlight{}       // server ID -> in-flight probe (see fbFlight)
)

// fallbackHealth returns the cached verdict for a server, probing if needed.
// One probe per server ID at a time (see fbFlight): concurrent callers wait
// for the in-flight result instead of duplicating it, and the fails
// read-modify-write below is therefore serialized - only the registered
// flight's owner writes this ID until it deregisters, so a strike can never
// be lost to a stale snapshot.
// A package var so tests can drive selection without a network.
var fallbackHealth = func(ctx context.Context, s *ookla.Server) endpointState {
	if s == nil || s.ID == "" {
		return endpointUnknown
	}
	now := time.Now()
	fbMu.Lock()
	prevFails := 0
	if v, ok := fbMap[s.ID]; ok {
		prevFails = v.fails
		if now.Before(v.expires) {
			fbMu.Unlock()
			return v.state
		}
	}
	if fl, ok := fbProbing[s.ID]; ok {
		fbMu.Unlock()
		select {
		case <-fl.done:
			return fl.st
		case <-ctx.Done():
			return endpointUnknown // this caller gave up; the probe still lands in the cache
		}
	}
	fl := &fbFlight{done: make(chan struct{})}
	fbProbing[s.ID] = fl
	fbMu.Unlock()

	st := probeFallback(ctx, s)
	if ctx.Err() != nil {
		// The caller gave up (annotation budget, aborted run). That is a fact
		// about us, not the server: returning it is fine, caching it would let a
		// picker timeout suppress a real probe for the next several minutes.
		fbMu.Lock()
		delete(fbProbing, s.ID)
		fl.st = endpointUnknown
		close(fl.done)
		fbMu.Unlock()
		return endpointUnknown
	}

	// A single definite failure is NOT a retirement. 429, 500, 502, 503 and a
	// maintenance window all look like a missing bundle for one request, and a
	// 12h exclusion on one bad moment removes a healthy server for half a day.
	// Two consecutive strikes are required, the same rule plState uses for the
	// loss probe; the first is held briefly so the retry happens soon.
	fails := 0
	ttl := fallbackTTL
	switch st {
	case endpointRetired:
		fails = prevFails + 1
		if fails < fallbackStrikes {
			st, ttl = endpointUnknown, fallbackUnknownTTL
		}
	case endpointUnknown:
		fails = prevFails // a probe that never landed is not a strike either way
		ttl = fallbackUnknownTTL
	}
	fbMu.Lock()
	if len(fbMap) >= fallbackMapCap {
		for k, v := range fbMap { // expired entries first - they cost nothing to lose
			if now.After(v.expires) {
				delete(fbMap, k)
			}
		}
		// Then evict until there is room. A single eviction per insert holds the
		// steady state, but cannot recover if the map is ever ALREADY over cap,
		// so drain rather than nudge.
		for k := range fbMap {
			if len(fbMap) < fallbackMapCap {
				break
			}
			delete(fbMap, k)
		}
	}
	fbMap[s.ID] = fallbackVerdict{state: st, expires: now.Add(ttl), fails: fails}
	delete(fbProbing, s.ID)
	fl.st = st
	close(fl.done)
	fbMu.Unlock()
	return st
}

// probeFallback GETs the bundle's latency probe and judges by STATUS, which is
// exactly what the library's ping omits.
var probeFallback = func(ctx context.Context, s *ookla.Server) endpointState {
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" {
		return endpointUnknown
	}
	// Derive the path the way the library's HTTPPing does rather than hardcoding
	// /speedtest/latency.txt: a catalogue entry with a non-standard path and an
	// empty Host (so currentEndpoint no-ops) would otherwise be probed at a path
	// it does not serve, and cached as unusable for fallbackTTL despite
	// measuring fine.
	probeURL := *u
	probeURL.Scheme = "http"
	probeURL.Path = path.Join(path.Dir(u.Path), "latency.txt")
	pctx, cancel := context.WithTimeout(ctx, fallbackProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, probeURL.String(), nil)
	if err != nil {
		return endpointUnknown
	}
	req.Header.Set("User-Agent", ookla.DefaultUserAgent)
	// Follow redirects. A 3xx means the bundle MOVED, not that it is gone -
	// treating it as gone would exclude a perfectly usable server for
	// fallbackTTL, and "the legacy hostname redirects to the current one" is
	// exactly the situation that produced issues #17/#18. Go follows a GET
	// redirect natively (unlike the POST, whose body is not replayable), so this
	// only needs the default client behaviour rather than a manual hop.
	resp, err := probeClient(fallbackProbeTimeout).Do(req)
	if err != nil {
		return endpointUnknown // says nothing about the endpoint; retry soon
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return endpointOK
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		// A redirect that was NOT followed (a loop, or too many hops). Unresolved
		// rather than absent: do not condemn the server on it.
		return endpointUnknown
	default:
		return endpointRetired
	}
}

// endpointState is what a probe learned about one server's upload endpoint.
type endpointState int

const (
	endpointOK      endpointState = iota // answered 2xx: usable
	endpointRetired                      // answered non-2xx, non-3xx: the legacy
	// /speedtest/upload.php is gone. Measured 2026-08-10: ~14% of the catalogue,
	// and those servers are NOT broken - the official Ookla CLI measures them at
	// full speed over its own protocol. They have dropped the legacy PHP endpoint
	// speedtest-go requires, permanently. Retrying them is pure waste.
	endpointUnknown // the probe itself failed (timeout, DNS, reset): decide nothing
)

// String keeps log lines and test failures readable - "retired" rather than "1".
func (e endpointState) String() string {
	switch e {
	case endpointOK:
		return "ok"
	case endpointRetired:
		return "no-legacy-fallback"
	default:
		return "unknown"
	}
}

// probeEndpointBody is deliberately tiny. A 1 KB POST returns the same status as
// the ~1 MB chunk a real transfer sends - verified against migrated,
// non-migrated and redirecting hosts - so a probe costs a round trip, not a
// measurement.
const probeEndpointBody = 1024

// probeEndpointTimeout bounds a probe so a silent server cannot stall selection.
const probeEndpointTimeout = 8 * time.Second

// probeEndpoint asks a server whether its upload endpoint still works, and
// follows ONE redirect hop by hand.
//
// The manual hop exists because Go will not redirect a POST whose body is not
// replayable, which is exactly the shape the library builds (io.NopCloser over
// its chunk reader, so GetBody is nil). That is the whole of issues #17/#18. On
// a 3xx the server's Location names its current home, so we rewrite s.URL to it
// - self-correcting, unlike deriving a hostname from the ID, which was measured
// to fail (server-46433.prod.hosts.ooklaserver.net does not resolve).
//
// Only needed where the catalogue's Host field is unavailable: the by-ID
// endpoint (api/ios-config.php) returns Host="" and cannot be fixed by
// currentEndpoint. List-derived servers carry Host and need no probe.
//
// A package var so tests can drive selection without a network.
var probeEndpoint = func(ctx context.Context, s *ookla.Server) endpointState {
	if s == nil || s.URL == "" {
		return endpointUnknown
	}
	pctx, cancel := context.WithTimeout(ctx, probeEndpointTimeout)
	defer cancel()

	do := func(target string) (int, string) {
		req, err := http.NewRequestWithContext(pctx, http.MethodPost, target,
			strings.NewReader(strings.Repeat("\xAA", probeEndpointBody)))
		if err != nil {
			return 0, ""
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		c := probeClient(probeEndpointTimeout)
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			return 0, ""
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode, resp.Header.Get("Location")
	}

	code, loc := do(s.URL)
	if code >= 300 && code < 400 && loc != "" {
		// Take the redirect's HOST but keep our own scheme and path: the target
		// advertises https, and plain http on the same host works everywhere it
		// was measured, while https fails outright on hosts with no TLS listener.
		if u, err := url.Parse(loc); err == nil && u.Host != "" {
			// Adopt the redirect target into s.URL only AFTER the guarded re-probe
			// gets a real response (code != 0). A guard refusal or transport
			// failure leaves code == 0; writing s.URL then would poison it with an
			// internal address that the separately-built measurement client could
			// later dial - the retained-URL half of the SSRF. Holding the candidate
			// local until it answers closes that even if the measurement-side dial
			// guard were ever removed. A public host answering non-2xx still gets
			// adopted and classified retired, exactly as before.
			// The logical-destination pre-check comes first: a proxied transfer
			// would carry an adopted internal host past the dial guard, so a
			// refused target is treated exactly like a refused dial (code 0) -
			// never probed, never adopted, verdict unknown.
			if guardProxiedDestination(pctx, u.Host) != nil {
				code = 0
			} else {
				candidate := "http://" + u.Host + "/speedtest/upload.php"
				if code, _ = do(candidate); code != 0 {
					s.URL = candidate
				}
			}
		}
	}
	switch {
	case code >= 200 && code < 300:
		return endpointOK
	case code == 0:
		return endpointUnknown // transport failure says nothing about the endpoint
	default:
		return endpointRetired
	}
}

// ooklaTransportHook, if set, interposes on the transport the library stamped
// onto our doer, BENEATH the panic containment. Test seam only (nil in
// production): the transfer requests run on the library's own worker
// goroutines, and this is how a test raises a panic there - the same
// swap-a-var idiom as ooklaDownload.
var ooklaTransportHook func(http.RoundTripper) http.RoundTripper

// errTransportPanicked marks a panic contained in the HTTP path of the client
// we hand the library (a round trip, or a read of the response body). It
// surfaces as an ordinary request error, so the library counts the chunk as
// failed and the run goes on instead of the process ending.
var errTransportPanicked = errors.New("speedtest http round trip panicked")

// panicSafeTransport converts a panic under RoundTrip - or under a read of the
// response body it returns - into an error. It exists because the library runs
// the actual transfer work on per-CPU worker goroutines plus a rate-capture
// goroutine of its own (data_manager.go, TestDirection.Start), none of which
// has a recover: recovery is goroutine-local, so neither runTransfer's recover
// nor any other boundary of ours wraps those stacks. The client we hand the
// library is the one piece of OUR code those goroutines run, so this is the
// widest fence we can put there without forking the library - panics raised
// beneath the request or body-read frames are contained; one raised in the
// library's own bookkeeping outside them still ends the process. Every
// containment site ends in panics.contain(): the brake that keeps a
// persistently panicking transport from spinning the worker loops (see
// panicThrottle).
type panicSafeTransport struct {
	base   http.RoundTripper
	panics *panicThrottle
}

// panicThrottle is the brake behind one client's panic containment. Containment
// alone made a persistent panic worse in a new way: the library's worker loops
// (data_manager.go, TestDirection.Start) have no backoff and end only on the
// capture-window timer, so converting each panic into an INSTANT error
// re-invoked the failing transport as fast as the cores could go - ~1.9M
// contained panics measured in one second across ten spinning cores - and every
// one bumped speed.transport_panic, so the counter measured loop iterations
// rather than panic events. One throttle is shared by a client's transport and
// every body it wraps, so the whole HTTP path brakes and counts as a unit.
type panicThrottle struct {
	mu   sync.Mutex
	last time.Time // last counted panic; zero = none yet
}

// ooklaPanicBackoff is how long a contained panic sleeps before surfacing as an
// error. Sleeping ON the worker's own stack is what bounds the spin: the loop
// cannot re-invoke the transport until contain returns, so a persistent panic
// costs ~captureTime/backoff iterations per worker instead of a saturated core.
// (Latching the first panic and failing fast instead would not help - an
// instantly erroring transport spins the same bare loop.) A one-off panic pays
// the sleep once, on a request that had already failed.
const ooklaPanicBackoff = 500 * time.Millisecond

// ooklaPanicCountEvery rate-limits the speed.transport_panic increments so the
// counter approximates panic EVENTS: the first contained panic counts
// immediately, and a persistent one keeps counting once per interval for as
// long as it goes on - rather than once per loop iteration.
const ooklaPanicCountEvery = time.Second

// contain is the recovery tail of every containment site: count the panic
// (rate-limited, see ooklaPanicCountEvery), then brake (see ooklaPanicBackoff)
// before the error returns. The sleep sits outside the lock so parallel
// workers stall concurrently instead of convoying on the mutex. Nil-safe
// because it runs inside a recover: a panic here would re-raise on a library
// worker goroutine and end the process - the exact outcome this fence exists
// to prevent.
func (p *panicThrottle) contain() {
	if p == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	if p.last.IsZero() || now.Sub(p.last) >= ooklaPanicCountEvery {
		p.last = now
		stats.Inc("speed.transport_panic")
	}
	p.mu.Unlock()
	time.Sleep(ooklaPanicBackoff)
}

func (t panicSafeTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			t.panics.contain()
			resp, err = nil, fmt.Errorf("%w: %v", errTransportPanicked, rec)
		}
	}()
	resp, err = t.base.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		// The library reads the body on the same worker goroutine AFTER RoundTrip
		// has returned, outside this frame - keep the containment across it.
		resp.Body = panicSafeBody{rc: resp.Body, panics: t.panics}
	}
	return resp, err
}

// panicSafeBody is panicSafeTransport's containment stretched over the response
// body (see there). It shares the transport's throttle: a body that panics on
// every Read feeds the same bare retry loop as a transport that panics on
// every request.
type panicSafeBody struct {
	rc     io.ReadCloser
	panics *panicThrottle
}

func (b panicSafeBody) Read(p []byte) (n int, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			b.panics.contain()
			n, err = 0, fmt.Errorf("%w: %v", errTransportPanicked, rec)
		}
	}()
	return b.rc.Read(p)
}

func (b panicSafeBody) Close() (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			b.panics.contain()
			err = fmt.Errorf("%w: %v", errTransportPanicked, rec)
		}
	}()
	return b.rc.Close()
}

// ooklaClientMu serializes newOoklaClient's stamp/unstamp of the process-global
// http.DefaultClient.Transport (see newOoklaClient).
var ooklaClientMu sync.Mutex

// ooklaCaptureTime mirrors the library's fixed per-transfer capture window (the
// DataManager default) - the only duration knob the Ookla engine has.
const ooklaCaptureTime = 15 * time.Second

// ooklaRunTimeout bounds one whole run. The library's HTTP requests carry no
// response timeout of their own, so a server that accepts a connection and then
// stalls without sending anything would hang the run forever - and with it the
// scheduler's single-flight guard, blocking every future speedtest until a
// restart. Sized from the engine's parameters: both directions times the
// attempts each may take (capture window + connect/backoff overhead), plus a
// generous fixed margin for the server-list fetch, server pings, the idle
// baseline and the loss probe.
func ooklaRunTimeout(retries int) time.Duration {
	perAttempt := ooklaCaptureTime + 15*time.Second
	return 2*time.Duration(1+retries)*perAttempt + 90*time.Second
}

// bestOfServerTimeout caps how long ONE server in a best-of run may take before
// it is abandoned and the next is tried. A healthy Ookla run is ~40s (15s of
// capture per direction - a fixed window, so this holds on slow links too - plus
// ping, idle baseline and the 5s loss probe). 90s leaves room for the slow parts
// that do vary - connect/TLS, the ping phase, and a retried transfer or two - so
// a working-but-unlucky server still gets to finish rather than being cut off.
//
// It is deliberately much tighter than ooklaRunTimeout, which still governs a
// normal single-server run. The difference is what waiting costs: in a best-of
// run a slow server starves the others, so giving up early is a win; in a
// single-server run there is nothing else waiting, and cutting a slow-but-working
// test off would turn a usable result into no data at all.
const bestOfServerTimeout = 90 * time.Second

// bestOfSelectionBudget covers the parts of a best-of run that happen once
// regardless of server count: the server-list fetch and the candidate ping race.
const bestOfSelectionBudget = 90 * time.Second

// bestOfTotalCap is the hard ceiling on a whole best-of run, however many servers
// it measures. Three 90s servers plus selection comes to 6m, so this is headroom
// rather than the binding constraint - it exists so raising bestOfServers can
// never quietly turn a speedtest into a ten-minute job again.
const bestOfTotalCap = 7 * time.Minute

// runBudget sizes a run's deadlines: how long any single server may take, and
// how long the whole run may take. A single-server run is unchanged from before
// best-of existed - one server, the full ooklaRunTimeout.
func runBudget(retries, want int) (perServer, total time.Duration) {
	total = ooklaRunTimeout(retries)
	if want <= 1 {
		return total, total
	}
	total = time.Duration(want)*bestOfServerTimeout + bestOfSelectionBudget
	if total > bestOfTotalCap {
		total = bestOfTotalCap
	}
	return bestOfServerTimeout, total
}

// errMeasurementNA: the library's transfers never return an error - a failed
// transfer is signalled by a speed of -1 (N/A: measured 0 with >10% of chunk
// requests failing). Surfacing that as an error inside each attempt makes the
// retry knob actually engage, and stops a failed download from running the
// full upload phase before the run is discarded. Left unchecked, the -1 would
// pass through .Mbps() as a bogus ~0 reading checked against thresholds.
var errMeasurementNA = errors.New("speedtest measurement unavailable (server returned N/A)")

// naErr maps the library's failed-transfer sentinel (a negative rate, see
// errMeasurementNA) to that error; a zero or positive rate is a real measurement
// and returns nil. Used on both the download and upload results.
func naErr(rate ookla.ByteRate) error {
	if rate < 0 {
		return errMeasurementNA
	}
	return nil
}

// Run selects a server (the configured one, or the fastest near the auto
// location / your IP) and measures ping/down/up. Equivalent to RunReason with no
// trigger, so best-of-N never engages - callers that want it use RunReason.
func (o *Ookla) Run(ctx context.Context) (Result, error) { return o.RunReason(ctx, "") }

// RunReason measures and reports the trigger, so the scheduler's closed enum
// (scheduled|manual|reconnect|startup|degraded) can gate best-of-N. Unknown or
// empty reasons behave exactly like a plain single-server Run.
func (o *Ookla) RunReason(ctx context.Context, reason string) (Result, error) {
	// Direction ("bidir" -> "both"; Ookla is sequential) + per-transfer retry - the
	// engine-agnostic knobs shared with iperf3. A skipped direction stays at 0.
	dir := speedDirection(o.DirectionFn)
	if dir == "bidir" {
		dir = "both"
	}
	retries := speedRetries(o.RetriesFn)

	// Wait out any transfer abandoned by a previous run before measuring anything.
	// Aborting releases the scheduler's single-flight immediately - which is the
	// point, the user wants control back - but the abandoned transfer keeps pulling
	// bytes until its own capture window closes. Measuring through that reports a
	// link slower than it is and spends the data twice. The bound is just past the
	// orphan's own lifetime, so a stuck one costs a short delay rather than a hang.
	if !awaitQuietTransfers(ctx, ooklaCaptureTime+5*time.Second) {
		// Went ahead anyway: a run that never happens is worse than one measured
		// alongside a straggler, but the numbers deserve the caveat in the log.
		stats.Inc("speed.overlapped_orphan")
		if o.Log != nil {
			o.Log.Warn("starting a speedtest while an abandoned transfer is still running",
				"reason", reason, "live_transfers", liveTransfers.Load())
		}
	}

	// How many servers this run measures. Best-of-N is opt-in AND trigger-gated:
	// a reconnect run must stay cheap and quick.
	want := 1
	if o.BestOfFn != nil && bestOfReasons[reason] && o.BestOfFn() {
		want = bestOfServers
	}

	// Deadlines (see runBudget): without them a single stalled request wedges the
	// run - and the single-flight guard - forever. perServer bounds each target so
	// one bad server can't eat the others' turns; total bounds the whole run. Both
	// bound how long we WAIT, not how long the work runs: a transfer ignores
	// cancellation and stops only on its own capture window (see runTransfer), so a
	// deadline landing mid-transfer releases the run at once and leaves the
	// library's goroutines to finish on their timer. It also ends the run there -
	// the abandoned transfer keeps the client every target shares - so perServer's
	// "the next target still gets its turn" holds at every stage except a transfer.
	//
	// A run that has to MEASURE its centre first gets the race's own budget on
	// top, because runBudget's arithmetic is exact and predates the race: at
	// want=1 the sum of selection and both transfer attempts already equals
	// ooklaRunTimeout, and at want=3 the three server slices plus selection
	// already equal the total. Spending the race out of that means the transfers
	// finish the run short - measured, a raced best-of-3 gave its third server
	// 80s of the 90s it is designed to get. The allowance is added here rather
	// than inside runBudget because only this scope knows whether a race can
	// happen at all: runBudget cannot see the pin or the searched city, and its
	// result is clamped by bestOfTotalCap, which would swallow the allowance.
	id := o.serverID()
	origins := o.candidateOrigins()
	// The searched city is snapshotted for the same reason the origins are: it
	// is a LIVE settings read, it decides both the deadline below and the centre
	// later on, and a pinned best-of run does a network fetch between those two
	// points. Read twice, a city cleared in that window sizes the deadline as
	// "no race" and then races anyway - paying for the race out of the exact
	// base budget, the defect this allowance exists to prevent.
	searchedCity := snapshotAutoLoc(o.AutoLocFn)
	perServer, total := runBudget(retries, want)
	if o.mayRaceCities(id, want, origins, searchedCity) {
		total += cityRaceBudget
	}
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	// Auto with a configured location (a searched city) fetches servers near that
	// coordinate; otherwise near the city that wins the race (see raceCities),
	// else near the caller's own IP. Fresh client per run: the
	// library's client.Context accumulates per-chunk DataChunk snapshots for the
	// client's life, so reusing one leaks a run's memory every test.
	// One UserConfig carries the parallel-connection count (0 -> the library's
	// NumCPU default) and, for auto-select with a searched city, the location.
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	if o.ConnectionsFn != nil {
		uc.MaxConnections = o.ConnectionsFn()
	}
	// The location centres the fetched list, which is where best-of-N draws its
	// companion servers from. A pinned server is resolved by ID and doesn't care
	// about the centring itself, so what matters here is only where its COMPANIONS
	// come from:
	//   - nothing pinned: the searched city, else the raced city (see raceCities).
	//   - pinned + best-of: the PINNED server, so the extras are its neighbours.
	//     Centring those on the exit instead made a pin nearly pointless - the
	//     winner is chosen on ping-weighted throughput (see bestIndex), so
	//     exit-local servers would out-score the pin on most rounds and get
	//     stored in its place, which is just Auto with one extra racer.
	//   - pinned, no best-of: nothing to centre; the pin is the only target.
	var pinned *ookla.Server
	if id != "" && want > 1 {
		// Resolve the pin first so the list can be centred on it. Passed into
		// pickServers afterwards so this fetch is not repeated.
		p, err := newOoklaClient(uc).FetchServerByIDContext(ctx, id)
		if err != nil {
			return Result{}, fmt.Errorf("fetch server %s: %w", id, err)
		}
		currentEndpoint(p) // a pinned server needs the same rewrite as a listed one
		pinned = p
	}
	if lat, lon, label, ok := listCentre(id, want, pinned, searchedCity); ok {
		uc.Location = newAnchoredLocation(label, lat, lon)
	} else if shouldRaceCities(id, want) {
		// An unanchored winner is a real answer, not a failure - it means the
		// pool the Ookla API picks for our source address held the fastest
		// server, so Location stays nil and that same pool is what gets fetched
		// below. A failed race leaves Location nil too, which is the pre-race
		// behaviour.
		if win, ok := o.raceOrigins(ctx, origins); ok && win.Anchored {
			uc.Location = newAnchoredLocation("auto", win.Lat, win.Lon)
		}
	}
	client, rec := newOoklaClientRec(uc)
	o.upRec = rec
	o.uc = uc

	// Re-home the pin onto THIS client. It had to be resolved earlier, before the
	// location was known, so it was fetched with a throwaway client - which left
	// two holes: its uploads rode a transport whose recorder nobody reads (so
	// diagnostics and the starvation rescue were blind to the pinned candidate),
	// and it never met probeEndpoint, because pickServers only probes when it has
	// to resolve the pin ITSELF. The by-ID endpoint returns Host="" so
	// currentEndpoint cannot help it either - a migrated pinned server therefore
	// kept the legacy URL and 307'd on every upload, which is issue #18 on the
	// one path that looked covered.
	if pinned != nil {
		pinned.Context = client
		switch probeEndpoint(ctx, pinned) {
		case endpointRetired:
			o.logf("pinned speedtest server has no HTTP legacy fallback",
				"server", serverLabel(pinned), "server_id", pinned.ID)
		case endpointUnknown:
			o.logf("pinned speedtest server did not answer the endpoint probe",
				"server", serverLabel(pinned), "server_id", pinned.ID)
		}
	}

	servers, err := fetchServerList(ctx, client)
	if err != nil {
		return Result{}, fmt.Errorf("fetch server list: %w", err)
	}

	// Bound just the candidate selection - the by-ID resolve and, above all, the
	// concurrent ping race in rankedServers, which waits on EVERY candidate
	// (wg.Wait). The per-run http.Client and PingTestContext carry no timeout of
	// their own, so one server that accepts the connection then goes silent would
	// otherwise block selection until the whole-run deadline, starving the
	// measurement it was picked to feed. A blanket http.Client.Timeout can't stand
	// in here: it would also cap the 15s-per-direction transfers. bestOfSelectionBudget
	// is generous, so when every candidate answers this changes nothing - the
	// stalled one just finishes unpinged and loses the race. The measurement below
	// runs on the original ctx (the full run budget), not selCtx.
	selCtx, selCancel := context.WithTimeout(ctx, bestOfSelectionBudget)
	targets, sel, err := o.pickServers(selCtx, client, servers, id, want, pinned)
	selCancel()
	if err != nil {
		return Result{}, err
	}

	// Identity snapshot before anything measures: a failed measurement's server
	// object is off-limits afterwards (an abandoned transfer's goroutines still
	// own it - see runTransfer), so the report's failure rows and the resume
	// path's log label must never read srv fields after the fact.
	targetIDs := make([]string, len(targets))
	targetLabels := make([]string, len(targets))
	for i, s := range targets {
		targetIDs[i] = s.ID
		targetLabels[i] = serverLabel(s)
	}
	// Per-candidate failure text for the selection report. firstErr keeps its
	// existing job (the error the run returns when nothing measured); this map
	// exists because every LATER candidate's error used to vanish entirely.
	errByID := make(map[string]string, len(targets))

	// Sequentially, always: two speedtests at once would saturate the link and
	// each would measure the other's traffic as congestion.
	var results []Result
	var firstErr error
	var spentDown, spentUp int64 // real bytes moved by FAILED candidates (their Results are discarded)
	for i, srv := range targets {
		// Report the server now so the UI can show it during the run.
		if o.OnServer != nil {
			o.OnServer(serverLabel(srv))
		}
		// Each server gets its own slice of the budget. Sharing one deadline let a
		// single accept-then-stall server (the ping and loss probes have no request
		// timeout of their own) burn the whole window, so the other targets were
		// never contacted - the exact failure best-of-N exists to survive.
		sctx, scancel := context.WithTimeout(ctx, perServer)
		res, err := measureServer(o, sctx, srv, dir, retries)
		scancel()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			errByID[targetIDs[i]] = err.Error()
			// A failed candidate's traffic was still spent - count it toward the
			// run's data-used total (its Result never reaches `results`, so the
			// success sum and this are disjoint; no double count).
			spentDown += res.DownloadBytes
			spentUp += res.UploadBytes
			// Best-of rounds only: a failed want=1 target IS the run failing,
			// and the scheduler's speed.fail.* already counts that - moving
			// both counters for one event would bury the question this one
			// answers (is a dead nearby server silently degrading rounds that
			// otherwise succeed?). Within a round it counts every failed
			// candidate, whether or not the round survives them.
			if len(targets) > 1 {
				stats.Inc("speed.bestof_candidate_failed")
			}
			// A dead server (or one that ate its own slice) shouldn't sink the run
			// while targets remain; an expired OUTER budget means they'd all fail.
			if ctx.Err() != nil {
				break
			}
			// An abandoned transfer (see runTransfer) is still running, and it holds
			// the client every target here shares - ookla.Server.Context is the client
			// that fetched the server, not per-server state. Measuring the next one
			// would Reset that DataManager under the orphan's workers (an unlocked
			// swap of the snapshot and both directions, which they read on every
			// chunk), and the orphan's capture timer would then stop the new transfer
			// mid-flight.
			//
			// That is a reason to WAIT, though, not a reason to give up: the orphan
			// closes itself within its capture window, and once it is gone the client
			// is ours again. Ending the run here meant one slow server - the exact
			// thing best-of-N exists to route around - discarded the servers behind
			// it and returned nothing. So drain, then carry on; only give up if the
			// orphan outlives the wait, or the run budget is spent, or there is
			// nothing left to try.
			if errors.Is(err, errTransferAbandoned) {
				// The snapshot, NOT serverLabel(srv): the orphan's goroutines
				// still own srv here (the fence above).
				if !o.resumeAfterAbandon(ctx, i, len(targets), targetLabels[i], err) {
					break
				}
				continue
			}
			if len(targets) > 1 {
				// Warn, not Debug: a mid-round failure is degraded-but-continuing
				// (the "iperf3 direction failed, partial result kept" bar) and the
				// motivating incident showed Debug-only evidence is no evidence at
				// the default level. Deliberate exception to the ~0-Warn-volume
				// norm: a persistently dead nearby server will repeat this every
				// round, and that is a condition worth seeing.
				o.warnf("speedtest server failed, trying the next", "server", serverLabel(srv), "err", err)
			}
			continue
		}
		if want > 1 {
			// Every server measured, not just the survivor: the losing runs are
			// discarded from the DATA, but they are the whole point of the feature,
			// and without them a log can't explain why a server keeps losing.
			o.logf("best-of server measured", "server", res.Server, "server_id", res.ServerID,
				"down_mbps", res.DownloadMbps, "up_mbps", res.UploadMbps,
				"ping_ms", res.PingMS, "jitter_ms", f64v(res.JitterMS),
				"bufferbloat_ms", f64v(bufferbloatMS(res)),
				"capacity_mbps", util.Round2(resultCapacity(res, dir)))
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		// Even a total failure carries its usage out: every candidate's failed
		// transfers still moved spentDown/spentUp real bytes, and returning an
		// empty Result here erased them - the scheduler's error path recorded
		// 0/0 for traffic that lands on the user's bill (see recordFailedUsage).
		// Engine too: it is the only field of a total failure that is knowable,
		// and the usage row's engine column is otherwise empty - which the
		// metrics path then reads as the DEFAULT engine (a guess that happens to
		// be right here and wrong for an iperf3 failure, whose Result carries its
		// own engine on the same path).
		spent := Result{Engine: "ookla", DownloadBytes: spentDown, UploadBytes: spentUp}
		if firstErr != nil {
			return spent, firstErr
		}
		return spent, fmt.Errorf("no speedtest servers available")
	}

	// Say so when the round refused to believe a direction: the guard changes
	// which measurement becomes history, so it must never do that silently.
	badDown, badUp := implausibleDirections(results)
	if badDown || badUp {
		stats.Inc("speed.implausible_direction")
		for _, r := range results {
			if (badDown && r.DownloadMbps > middleOf(results, func(x Result) float64 { return x.DownloadMbps })) ||
				(badUp && r.UploadMbps > middleOf(results, func(x Result) float64 { return x.UploadMbps })) {
				// Warn: the guard changes which measurement becomes history
				// (~once per 150 runs in the field), and it must be visible at
				// the default level - the motivating incident's only trace was
				// this line's Debug predecessor, which nothing captured.
				o.warnf("best-of result not believed: a direction exceeds what the rest of the round measured",
					"server", r.Server, "server_id", r.ServerID,
					"down_mbps", r.DownloadMbps, "up_mbps", r.UploadMbps, "ping_ms", r.PingMS,
					"mid_down_mbps", util.Round2(middleOf(results, func(x Result) float64 { return x.DownloadMbps })),
					"mid_up_mbps", util.Round2(middleOf(results, func(x Result) float64 { return x.UploadMbps })),
					"factor", implausibleFactor)
			}
		}
	}

	win := bestIndex(results, dir)
	bootstrap := o.firstRunByPing(want)
	if bootstrap {
		win = lowestPingIndex(results)
		stats.Inc("speed.first_run_by_ping")
		o.logf("no speed history yet: deciding this round on ping alone, so an unverifiable "+
			"throughput reading cannot seed the baseline",
			"winner", results[win].Server, "ping_ms", results[win].PingMS, "servers", len(results))
	}
	best := results[win]
	// Store the BELIEVED reading, not the verbatim one. The guard already held an
	// implausible winning direction to the round middle for the DECISION above
	// (believableCapacity); this caps what is STORED, so the chart, thresholds and
	// data-used forecast can't read a number the round itself rejected. math.Min
	// only ever lowers a value already above the middle, so an honest winner is
	// untouched and the chosen server never changes (win is fixed above). The
	// selection report keeps the raw reading plus CappedDirection, documenting
	// why the speed row differs.
	if badDown {
		best.DownloadMbps = math.Min(best.DownloadMbps, middleOf(results, func(r Result) float64 { return r.DownloadMbps }))
	}
	if badUp {
		best.UploadMbps = math.Min(best.UploadMbps, middleOf(results, func(r Result) float64 { return r.UploadMbps }))
	}
	if len(results) > 1 {
		// One line per loser under the `server` key (logfilter masks by key;
		// the old joined `discarded` attr leaked every loser's label in the
		// masked form, and a joined list can only mask to one useless blob).
		for i, r := range results {
			if i != win {
				o.logf("best-of result discarded", "server", r.Server, "server_id", r.ServerID)
			}
		}
		o.logf("best-of run finished", "servers", len(results), "winner", best.Server,
			"down_mbps", best.DownloadMbps, "up_mbps", best.UploadMbps,
			"capacity_mbps", util.Round2(believableCapacity(best, dir, results)),
			"score", util.Round2(roundScore(best, dir, results)))
	}
	// The selection report rides the winner Result to the Scheduler, which
	// persists it next to the speed row. Built BEFORE the totalBytes overwrite
	// below so each row keeps its candidate's own traffic (best is a copy, but
	// ordering here keeps that independence obvious).
	winReason := winReasonScore
	switch {
	case id != "" && want <= 1:
		winReason = WinReasonPinned
	case want <= 1:
		winReason = winReasonFastestRank
	case bootstrap:
		winReason = winReasonPingBoot
	}
	best.Selection = finishSelection(sel, results, errByID, dir, best.ServerID, winReason)
	dn, up := totalBytes(results)
	best.DownloadBytes, best.UploadBytes = dn+spentDown, up+spentUp
	// Always name the server whose numbers are being returned. Without this a run
	// whose last target failed would leave the mid-run label - a server that
	// contributed nothing - showing as the current one.
	if o.OnServer != nil {
		o.OnServer(best.Server)
	}
	return best, nil
}

// serverCoord parses a server's advertised position. Ookla carries lat/lon as
// strings, and a blank or malformed pair is not worth failing a run over - the
// caller centres on something else instead.
func serverCoord(s *ookla.Server) (lat, lon float64, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	la, e1 := strconv.ParseFloat(strings.TrimSpace(s.Lat), 64)
	lo, e2 := strconv.ParseFloat(strings.TrimSpace(s.Lon), 64)
	if e1 != nil || e2 != nil || (la == 0 && lo == 0) {
		return 0, 0, false
	}
	return la, lo, true
}

// listCentre decides what the fetched server list is centred on, which is the
// same thing as deciding where best-of-N draws its companion servers from. Kept
// as a plain function so the rule itself is tested, not a copy of it.
//
//	pinned + best-of  -> the pinned server, so the extras are ITS neighbours
//	pinned, no best-of -> nothing; the pin is the only target and centring is moot
//	nothing pinned    -> the searched city; with none, ok=false and the caller
//	                     races the candidate cities instead (see raceCities)
//
// A pin whose coordinate Ookla did not supply falls back to the searched city;
// with no city the caller races instead, so the companions still come from a
// measured place rather than the public-IP guess the library would make.
func listCentre(id string, want int, pinned *ookla.Server, autoFn func() (float64, float64, bool)) (lat, lon float64, label string, ok bool) {
	if id != "" && want > 1 {
		if la, lo, good := serverCoord(pinned); good {
			return la, lo, "pinned", true
		}
	} else if id != "" {
		return 0, 0, "", false
	}
	if autoFn != nil {
		if la, lo, good := autoFn(); good {
			return la, lo, "auto", true
		}
	}
	return 0, 0, "", false
}

// firstRunByPing reports whether this round must be decided on ping alone: a
// best-of round on an install with NO speed history behind it.
//
// The reason is what such a round becomes rather than what it is. It is the run
// that SEEDS the history every later plausibility check reads, and the two ways
// of getting it wrong do not cost the same. Seed it low and the error corrects
// itself, because a genuinely faster line lifts every server and the baseline
// follows. Seed it high with a number nothing could vet - a server counting
// bytes it handed to the socket but never delivered - and the baseline sits
// above the artefact permanently, so the very mechanism meant to catch that
// class never fires again.
//
// So the bootstrap round refuses to rank on throughput it cannot check, and
// takes the one figure a server cannot inflate: its own handshake latency,
// timed on this side of the connection. Capacity is deliberately not consulted -
// not weighed less, not held at a cap, but ignored - because with nothing to
// compare against there is no way to tell a fast server from a lying one.
//
// It applies to best-of rounds only. A single-server run has nothing to choose
// between, and auto-selection has already picked that server by ping upstream.
func (o *Ookla) firstRunByPing(want int) bool {
	return want > 1 && o.PriorDataFn != nil && !o.PriorDataFn()
}

// lowestPingIndex picks the candidate with the lowest MEASURED latency. A run
// that never got a ping cannot win by absence - the same rule the tie-breaks
// use - and an exact tie keeps the earlier candidate, which is the pinned or
// higher-ranked server.
func lowestPingIndex(rs []Result) int {
	win := 0
	for i, r := range rs {
		rp, bp := decisionPingMS(r), decisionPingMS(rs[win])
		switch {
		case !validMS(rp):
			continue
		case !validMS(bp):
			win = i
		case rp < bp:
			win = i
		}
	}
	return win
}

// shouldRaceCities reports whether a run whose centre listCentre could not
// supply should go and MEASURE one (see raceCities). Kept as a plain function
// so the rule itself is tested rather than a copy of it - the same reason
// listCentre is one, and reaching this decision through RunReason would need a
// live Ookla fetch for the pinned case.
//
// Two ways in. Nothing pinned is the ordinary auto run. A pinned server with
// best-of is the other: listCentre only declines there when Ookla supplied no
// usable coordinate for the pin, and the companions then have nowhere to come
// from but the API's guess at our address - the placement this whole feature
// exists because it can be a country away. The pin itself is measured either
// way; the race only decides where its COMPANIONS are drawn from.
//
// A pin WITHOUT best-of is excluded: the pin is the only target, so centring
// decides nothing and racing would spend a fetch per city and a round of pings
// at other people's servers to answer a question nobody asked.
func shouldRaceCities(id string, want int) bool { return id == "" || want > 1 }

// mayRaceCities reports whether this run COULD reach the race, so the deadline
// can carry its budget. It is asked about the SAME origins the race will run
// on (see candidateOrigins) rather than re-reading them: a field with nothing
// anchored short-circuits inside raceOrigins without fetching or pinging
// anything, and granting it the race's budget would loosen the deadline of
// every auto run on a box that has no coordinate at all - a documented steady
// state, not a boot transient.
//
// It is still an upper bound in one place: whether a pinned best-of run races
// depends on the coordinate Ookla supplies for the pin, and that is not known
// until the pin is fetched, which happens under the very deadline being sized.
// Sizing cannot depend on a fetch the size governs, so that run is allowed the
// budget whether or not it ends up racing. The cost of the over-allowance is a
// rare run permitted to take 30s longer than it needs; the cost of guessing the
// other way is a run cut off mid-transfer.
func (o *Ookla) mayRaceCities(id string, want int, origins []Origin, searchedCity func() (float64, float64, bool)) bool {
	if !anyAnchored(origins) || !shouldRaceCities(id, want) {
		return false
	}
	// A searched city centres the run without measuring anything, so there is
	// no race to pay for. Asked with no resolved pin, which is what makes this
	// an upper bound (see above), and against the run's SNAPSHOT of the city so
	// the answer cannot differ from the one the centring decision gets.
	_, _, _, searched := listCentre(id, want, nil, searchedCity)
	return !searched
}

// snapshotAutoLoc reads the searched city once and hands back a function that
// keeps answering with that reading. Every deadline-sensitive branch in a run
// must see one immutable set of inputs; see RunReason.
func snapshotAutoLoc(fn func() (float64, float64, bool)) func() (float64, float64, bool) {
	if fn == nil {
		return nil
	}
	lat, lon, ok := fn()
	return func() (float64, float64, bool) { return lat, lon, ok }
}

// pickServers chooses what a run measures. pre is the already-resolved pinned
// server when the caller had to fetch it early to centre the list on it; nil
// means resolve it here. The second return is the selection report skeleton:
// one row per considered candidate, identity and ranking snapshotted NOW
// (the returned servers are pointers the measurement phase mutates).
func (o *Ookla) pickServers(ctx context.Context, client *ookla.Speedtest, servers ookla.Servers, id string, want int, pre *ookla.Server) (ookla.Servers, []CandidateReport, error) {
	pinned := pre
	if id != "" && pinned == nil {
		for _, s := range servers {
			if s.ID == id {
				pinned = s
				break
			}
		}
		if pinned == nil { // not in the nearby list; fetch it directly
			s, err := client.FetchServerByIDContext(ctx, id)
			if err != nil {
				return nil, nil, fmt.Errorf("fetch server %s: %w", id, err)
			}
			// This endpoint (api/ios-config.php) returns Host="" - measured - so
			// currentEndpoint cannot help and a migrated pin would 307 on every
			// upload forever. Probe it and follow the hop instead. Only this
			// fallback pays the round trip; a pin found in the list above already
			// carries the catalogue's current Host.
			switch probeEndpoint(ctx, s) {
			case endpointRetired:
				o.logf("pinned speedtest server has retired its upload endpoint",
					"server", serverLabel(s), "server_id", s.ID, "url", s.URL)
			case endpointUnknown:
				o.logf("pinned speedtest server did not answer the endpoint probe",
					"server", serverLabel(s), "server_id", s.ID)
			}
			pinned = s
		}
		if want <= 1 {
			return ookla.Servers{pinned}, []CandidateReport{pinnedRow(pinned, true)}, nil
		}
	}

	isp := ""
	if o.ISPFn != nil {
		isp = o.ISPFn()
	}
	ranked, rankPings, noFallback, allNoFallback := rankedServers(ctx, servers, isp)
	if allNoFallback {
		// Every nearby server lacks the fallback, so none was excluded - the run
		// goes ahead and will probably fail. Silence here would make the worst
		// case the only one with no signal.
		o.logf("every nearby speedtest server lacks the HTTP legacy fallback; measuring anyway",
			"candidates", len(ranked))
		stats.Inc("speed.all_servers_no_fallback")
	}
	for _, id := range noFallback {
		// Not slow, not flaky: the optional HTTP Legacy Fallback is absent, so
		// every transfer would fail. Excluded from ranking - an explicit pin is
		// unaffected, it never passes through here.
		o.logf("skipping speedtest server with no HTTP legacy fallback", "server_id", id)
		stats.Inc("speed.server_no_fallback")
	}
	// One Debug line per candidate - the phase had zero logging before the
	// 2026-08-02 incident. Per-candidate rather than one joined line so the
	// `server` key masks correctly (logfilter masks by key).
	for i, s := range ranked {
		o.logf("speedtest candidate ranked", "server", serverLabel(s), "server_id", s.ID,
			"rank", i+1, "distance_km", util.Round1(s.Distance), "ping_ms", util.Round1(f64v(rankPings[s.ID])))
	}
	sel := candidateRows(ranked, rankPings)

	if want <= 1 {
		if len(ranked) == 0 {
			return nil, nil, fmt.Errorf("no speedtest servers available")
		}
		sel[0].Selected = true
		return ookla.Servers{ranked[0]}, sel, nil
	}

	out := make(ookla.Servers, 0, want)
	if pinned != nil {
		out = append(out, pinned)
	}
	for _, s := range ranked {
		if len(out) == want {
			break
		}
		if pinned != nil && s.ID == pinned.ID { // already leading; don't race itself
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no speedtest servers available")
	}
	// Mark the targets in the report; a pin that was never in the ranked list
	// gets its own unranked row at the front (RankOrder 0).
	chosen := make(map[string]bool, len(out))
	for _, s := range out {
		chosen[s.ID] = true
	}
	inRanked := false
	for i := range sel {
		if pinned != nil && sel[i].ServerID == pinned.ID {
			inRanked = true
		}
		sel[i].Selected = chosen[sel[i].ServerID]
	}
	if pinned != nil && !inRanked {
		sel = append([]CandidateReport{pinnedRow(pinned, true)}, sel...)
	}
	return out, sel, nil
}

// freshManager gives the NEXT attempt its own DataManager instead of resetting
// the shared one.
//
// Reset() rebuilds the direction state but keeps the manager, so two Start()
// calls touch the same manager.running - and Start writes that field WITHOUT the
// RWMutex every reader holds (data_manager.go:224 vs :314). The previous
// attempt's adaptUploadWorkers goroutine only notices the window closed on its
// own 1s tick, so it is still reading while the next attempt writes: a data race
// on the production default of one retry. No amount of waiting fixes it - the
// missing happens-before edge is the defect, not the timing.
//
// Swapping in a new client removes the SHARED STATE the race needs, from our
// side, without patching the library. srv.Context is only ever the client that
// fetched the server (see runTransfer's note), and everything a transfer touches
// goes through it, so the swap is total.
//
// The caller must read any per-attempt totals off the OLD manager first - they
// do not carry over. That is the same discipline Reset() already required.
func freshManager(o *Ookla, srv *ookla.Server, uc *ookla.UserConfig) {
	if uc == nil {
		// A direct measure() call (tests, or any caller that did not come through
		// RunReason) has no run config. The library's own default is the right
		// stand-in; the alternative is a nil deref inside NewUserConfig.
		uc = &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	}
	// Read the capture window off the client being replaced BEFORE the swap:
	// SetCaptureTime lands on the MANAGER, not the UserConfig, so a rebuild
	// that only copies the config silently reverts to the library's 15s
	// default - a caller-configured window (the live-probe suites' 3s, the
	// offline harness's shorter ones) advertised one duration and transferred,
	// and billed, at another.
	carry := managerCaptureTime(srv.Context)
	// Each attempt gets its OWN UserConfig value. The library aliases the
	// pointer it is handed (NewUserConfig does s.config = uc, then writes
	// uc.T), so passing the run's shared config into every rebuild meant
	// constructing attempt N+1 repointed EVERY earlier client - including a
	// not-yet-drained orphan reading s.config.T from its worker goroutines - at
	// the new transport: an unsynchronized write of exactly the shared-state
	// class this function exists to remove. The value copy severs the alias;
	// the pointer fields it shares (Location, DialerControl) are only read by
	// the library on the paths a rebuilt client uses.
	attempt := *uc
	client, rec := newOoklaClientRec(&attempt)
	if carry > 0 {
		client.SetCaptureTime(carry)
	}
	srv.Context = client
	if o != nil {
		if o.attemptT != nil {
			// The client this rebuild replaces is abandoned: nothing sends on it
			// again, so release its idle sockets now rather than after the
			// library's 90s idle timeout. Safe under an orphaned transfer too -
			// CloseIdleConnections never touches an in-flight request's conn.
			o.attemptT.CloseIdleConnections()
		}
		o.attemptT = attempt.T // the transport New stamped for THIS attempt
		o.upRec = rec          // diagnostics follow the client the transfer actually uses
	}
}

// managerCaptureTime reads the transfer capture window configured on a
// client's DataManager, or 0 when it cannot be determined. speedtest-go
// v1.7.11's Manager interface has SetCaptureTime but no getter, and
// freshManager must CARRY a caller-configured window onto the per-attempt
// client it builds (see there) - reflection is the least-bad access short of
// forking the library. A library upgrade that renames the field degrades to 0
// (keep the library's default window, today's pre-carry behaviour), and
// TestFreshManagerCarriesCaptureWindowState pins the read so that degradation
// fails loudly instead of silently.
func managerCaptureTime(c *ookla.Speedtest) time.Duration {
	if c == nil {
		return 0
	}
	dm, ok := c.Manager.(*ookla.DataManager)
	if !ok || dm == nil {
		return 0
	}
	f := reflect.ValueOf(dm).Elem().FieldByName("captureTime")
	if !f.IsValid() || f.Kind() != reflect.Int64 {
		return 0
	}
	return time.Duration(f.Int())
}

// ooklaTransfer is one direction's library transfer. The two implementations are
// package vars so a test can stand in for the network without one - the same
// swap-a-var seam as iperfExec - and so measure's two directions read alike.
type ooklaTransfer func(ctx context.Context, srv *ookla.Server) error

var (
	ooklaDownload ooklaTransfer = func(ctx context.Context, srv *ookla.Server) error {
		return srv.DownloadTestContext(ctx)
	}
	ooklaUpload ooklaTransfer = func(ctx context.Context, srv *ookla.Server) error {
		return srv.UploadTestContext(ctx)
	}
)

// ooklaPing is the ranking latency probe, a swap-a-var seam (like ooklaDownload/
// ooklaUpload) so rankedServers' selection logic - which reachable/failed server
// wins - is testable without a live server.
var ooklaPing = func(ctx context.Context, srv *ookla.Server, cb func(time.Duration)) error {
	return srv.PingTestContext(ctx, cb)
}

// uploadSpent is the data an upload attempt actually pushed across the (possibly
// metered) link, for data-usage accounting. It is confirmed bytes (GetTotalUpload)
// PLUS the backlog (GetUploadBacklog): bytes read into the socket but not
// server-confirmed. speedtest-go v1.7.11 counts only confirmed bytes in
// GetTotalUpload - correct for the RATE (ULSpeed) but it drops the bytes a FAILED
// or aborted attempt already sent, which "data used" must still include. Download
// has no equivalent gap (GetTotalDownload counts bytes actually received).
func uploadSpent(srv *ookla.Server) int64 {
	return srv.Context.GetTotalUpload() + srv.Context.GetUploadBacklog()
}

// errTransferAbandoned marks a transfer we walked away from because the context
// died first (see runTransfer). It travels alongside the context error - both are
// wrapped, so callers testing for context.Canceled still match - and it is what
// RunReason breaks the target loop on.
var errTransferAbandoned = errors.New("transfer abandoned on cancellation")

// errTransferPanicked marks a transfer whose goroutine panicked inside the
// measurement library. It is a measurement failure like any other - the run
// retries or moves to the next server - rather than the end of the process.
var errTransferPanicked = errors.New("speedtest transfer panicked")

// runTransfer runs one library transfer and returns as soon as EITHER it
// finishes or ctx is done, whichever comes first; finished says which.
//
// It exists because speedtest-go's transfers do not observe cancellation. Its
// worker goroutines loop until the DataManager's running flag is cleared, and
// only the transfer's own time.AfterFunc(captureTime) clears it
// (speedtest/data_manager.go, TestDirection.Start). runTransfer wraps BOTH
// directions, and they do not run the same number of workers: with no connection
// count configured the download leg runs one per logical CPU (runtime.NumCPU)
// while the upload leg is capped at uploadDefaultWorkerCap, and a configured
// count applies to both legs - the same split starvationCeiling derives from.
// Cancelling ctx merely makes every chunk request fail instantly, so the rest
// of the ooklaCaptureTime window becomes a hot spin. Waiting for the call to
// return therefore held the caller for up to that whole window after an abort:
// the UI spinner kept turning, the scheduler's single-flight flag stayed set so
// every new run got ErrBusy, and the shutdown drain gave up with "background
// workers did not stop in time".
//
// Ownership: when finished is false the transfer goroutine is STILL RUNNING and
// owns both srv - the library writes DLSpeed/ULSpeed/TestDuration on its way out
// - and srv.Context, which is not per-server state but the client that fetched
// it, shared by every target of a best-of run. After this returns false the
// caller must read no unsynchronized FIELD of either and must not Reset that
// manager (Reset swaps the snapshot and both directions with no lock while the
// workers read them). Hence errTransferAbandoned: measure stops at the failed
// attempt rather than retrying, and RunReason ends the run rather than measuring
// the next target on the same client.
//
// The manager's data-volume counters are the exception, and measure does read
// them: GetTotalDownload / GetTotalUpload / GetUploadBacklog are atomic loads of
// counters the workers only ever add to, so a caller can ask what the abandoned
// transfer had moved so far without racing it. That is not an optional nicety -
// those bytes came off the user's metered allowance, and nothing else records
// them once this returns false.
//
// The orphan is bounded and self-collecting: its capture window closes it at
// most ooklaCaptureTime after the transfer began, and the goroutine then exits
// and releases everything it holds. done is buffered so it can always deliver
// that last result and exit even though nobody is left listening. The real fix
// belongs upstream - a context.AfterFunc(ctx, td.closeFunc) beside that timer -
// and until a release carries it, letting the orphan die on its own timer costs
// less than vendoring the library.
// liveTransfers counts transfer goroutines that have not returned, including
// orphans nobody is waiting on any more. It exists so the NEXT run can tell
// whether the link is still busy with the last one: the orphan dies on its own
// timer, but the scheduler's single-flight is released the moment RunOnce
// returns, so a user who aborts and immediately re-runs starts measuring while
// the abandoned transfer is still pulling bytes. That understates the new result
// and spends the data twice, and repeating it stacks orphans. See
// awaitQuietTransfers.
var liveTransfers atomic.Int64

// awaitQuietTransfers blocks until no transfer goroutine is in flight, or until
// the bound elapses. The bound is what makes this safe rather than a new place to
// hang: an orphan is self-collecting within ooklaCaptureTime, so waiting slightly
// longer than that means a genuinely stuck one is measured around rather than
// waited on forever. Returns whether the link was quiet when it gave up.
func awaitQuietTransfers(ctx context.Context, bound time.Duration) bool {
	if liveTransfers.Load() == 0 {
		return true
	}
	deadline := time.Now().Add(bound)
	t := time.NewTicker(50 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return liveTransfers.Load() == 0
		case <-t.C:
			if liveTransfers.Load() == 0 {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
		}
	}
}

func runTransfer(ctx context.Context, srv *ookla.Server, fn ooklaTransfer) (finished bool, err error) {
	// Nothing to start on a run that is already over. The library ignores the
	// context, so a transfer launched here could not be stopped: it would run to its
	// own capture deadline, count against liveTransfers the whole time, and make the
	// NEXT run wait for a link nobody is measuring. The select below cannot prevent
	// that - by then the goroutine is away.
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("%w: %w", errTransferAbandoned, err)
	}
	done := make(chan error, 1)
	liveTransfers.Add(1)
	go func() {
		var err error
		// One deferred block doing three things in a fixed order: recover, release
		// the in-flight count, deliver exactly one outcome.
		//
		// The recover has to live HERE. Panic recovery is goroutine-local, and this
		// goroutine exists precisely so an abort can walk away from the transfer - so
		// it also walks away from every recover() the daemon has: main.go's spawn
		// wrappers and the HTTP handler's recoverPanics can only catch panics on
		// their own stacks. On main the transfer ran inline and was covered by those
		// boundaries; the goroutine is what removed them, and this restores exactly
		// that much: panics on THIS stack - the srv.*TestContext call, its handler
		// registration, the worker spawn loop, the rate arithmetic on the way out.
		// It is NOT all of the library: the transfer work itself runs on per-CPU
		// worker goroutines plus a rate-capture goroutine the library spawns
		// (data_manager.go, TestDirection.Start), which no recover here can reach.
		// Panics those goroutines raise inside the HTTP path are contained
		// separately, by the recovering transport newOoklaClient hands the library
		// (see panicSafeTransport); one raised in the library's own bookkeeping
		// outside that path still ends the process.
		//
		// The count is released BEFORE the send, so a caller that receives this
		// result and immediately asks whether the link is quiet gets a truthful yes.
		// done is buffered, so the send always completes even when the select below
		// has already abandoned us.
		defer func() {
			if rec := recover(); rec != nil {
				stats.Inc("speed.transfer_panic")
				err = fmt.Errorf("%w: %v\n%s", errTransferPanicked, rec, debug.Stack())
			}
			liveTransfers.Add(-1)
			done <- err
		}()
		err = fn(ctx, srv)
	}()
	select {
	case e := <-done:
		return true, e
	case <-ctx.Done():
		// A transfer that finishes in the same instant the context dies can land
		// here too (select picks at random). The run is discarded either way, and
		// the bytes are not: measure tallies the manager's volume counters on
		// this branch as well as the finished one, so which way the select fell
		// changes the reported speed - already thrown away - but not the data
		// usage the user is billed against.
		return false, fmt.Errorf("%w: %w", errTransferAbandoned, ctx.Err())
	}
}

// resumeAfterAbandon decides whether a run whose transfer was abandoned may go on
// to the next target, and waits for the straggler when it may. Reports false to
// end the run.
//
// It is a named method rather than an inline branch so the decision is testable
// on its own: reaching it through RunReason needs real transfers against real
// servers, and a test that re-implements the loop would be checking a copy of the
// rule rather than the rule.
func (o *Ookla) resumeAfterAbandon(ctx context.Context, index, total int, label string, cause error) bool {
	if index >= total-1 {
		return false // nothing left to measure anyway
	}
	// The abandoned transfer still owns the client every target shares, so the next
	// one cannot start until it has gone. It closes itself within its capture
	// window; wait slightly past that, and give up only if it outlives even
	// that or the run's own budget expires first.
	if !awaitQuietTransfers(ctx, ooklaCaptureTime+5*time.Second) {
		o.logf("speedtest transfer abandoned and still running; ending the run", "server", label, "err", cause)
		return false
	}
	o.logf("speedtest transfer abandoned; the link is quiet again, trying the next server",
		"server", label, "err", cause)
	stats.Inc("speed.abandoned_resumed")
	return true
}

// measureServer indirects (*Ookla).measure so a test can drive RunReason's
// per-server loop - which server errors end a run and which move to the next -
// without a network. The loop's error handling is the part with the interesting
// decisions in it, and it was previously reachable only through real transfers.
var measureServer = func(o *Ookla, ctx context.Context, srv *ookla.Server, dir string, retries int) (Result, error) {
	return o.measure(ctx, srv, dir, retries)
}

// fetchServerList indirects the discovery fetch the same way, and for the same
// reason: it is the one discovery step that cannot degrade offline (the list
// URL is fixed and HTTPS, so no test can stand in for it from outside), where
// everything after it - the ping race - falls back to nearest-first order on
// its own. Stubbing it lets a test reach measureServer THROUGH RunReason, so
// the loop around both is exercised rather than mirrored.
var fetchServerList = func(ctx context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
	servers, err := client.FetchServerListContext(ctx)
	// guardedServers before anything pings or measures them: with a proxy
	// configured the dial guard never sees these third-party destinations.
	return guardedServers(ctx, currentEndpoints(servers)), err
}

// connFamilies records which IP families one measurement's transfer
// connections ACTUALLY used, so Result.IPFamily can be derived from a real
// recorded connection rather than guessed from configuration. Fed by the
// httptrace hook traceConnFamilies installs on the transfer contexts; the
// library's workers obtain connections concurrently, hence the lock.
type connFamilies struct {
	mu     sync.Mutex
	v4, v6 bool
}

// note classifies one transfer connection's remote address. A connection to
// the operator's configured proxy is skipped on purpose: that hop's family
// describes the local leg to the proxy, not the path that carried the
// transfer beyond it, so recording it would claim knowledge we don't have -
// a proxied run honestly stays unrecorded.
func (c *connFamilies) note(remote string) {
	if isConfiguredProxy(remote) {
		return
	}
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return // not an IP literal; nothing honest to record
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// A v4-mapped literal (dual-bound listener) is IPv4 on the wire - the same
	// rule iperfFamily applies to iperf3's start.connected block.
	if a.Is4() || a.Is4In6() {
		c.v4 = true
	} else {
		c.v6 = true
	}
}

// family reports what the recorded connections allow us to say: "4"/"6" when
// every transfer connection agreed, "mixed" when both families really carried
// transfer bytes (the same vocabulary the iperf engine stores when its two
// processes disagree), "" when no connection was recorded at all.
func (c *connFamilies) family() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.v4 && c.v6:
		return "mixed"
	case c.v4:
		return "4"
	case c.v6:
		return "6"
	}
	return ""
}

// traceConnFamilies returns ctx with a client trace that feeds every
// connection obtained under it into rec. Applied ONLY to the transfer
// contexts (the runTransfer calls): a wider scope would also record the
// latency-under-load probe's endpoint, which is a different host entirely.
func traceConnFamilies(ctx context.Context, rec *connFamilies) context.Context {
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			if info.Conn == nil {
				return
			}
			if ra := info.Conn.RemoteAddr(); ra != nil {
				rec.note(ra.String())
			}
		},
	})
}

// ooklaLoss indirects measurePacketLoss the same way ooklaPing/ooklaDownload/
// ooklaUpload stand in for the network: the loss probe needs a live UDP
// protocol no offline test can serve, and what measure() records about a
// SUCCESSFUL probe (UDPDirection) was otherwise unreachable in tests.
var ooklaLoss = measurePacketLoss

// measure runs one full measurement against an already-chosen server.
func (o *Ookla) measure(ctx context.Context, srv *ookla.Server, dir string, retries int) (Result, error) {
	// The target is third-party data on every path here (catalogue Host, by-ID
	// resolve, an adopted redirect). When an operator proxy carries the
	// requests the dial guard only ever sees the proxy's address, so the
	// logical destination is vetted before the first request names it. Direct
	// (unproxied) runs are covered at dial time and skip this.
	if err := guardProxiedDestination(ctx, serverDestination(srv)); err != nil {
		return Result{}, fmt.Errorf("refusing speedtest destination: %w", err)
	}
	var err error
	// Same ten samples the library already sends - the callback only keeps the
	// fastest alongside the mean it returns, so this costs no extra probe.
	var bestPing time.Duration
	if err := ooklaPing(ctx, srv, keepFastestPing(&bestPing)); err != nil {
		return Result{}, fmt.Errorf("ping: %w", err)
	}
	// Idle baseline for latency-under-load: same method/target as the loaded
	// samplers below (NOT the Ookla ping above), taken while the link is quiet, so
	// the idle-vs-loaded delta isolates the load effect.
	probeAddr := lulRunEndpoint()
	idleMS := measureIdleLatency(ctx, probeAddr)
	anyErr := func(error) bool { return true } // a failed transfer is worth retrying

	// The family the transfers ACTUALLY used, from their real connections (see
	// connFamilies). One recorder spans every attempt of both directions: the
	// per-attempt client rebuilds (freshManager) must not lose the download's
	// record before the upload adds its own.
	fams := &connFamilies{}

	// Each retry attempt must start from a clean DataManager: the library's
	// Register*Handler only appends while the handler stack has room, so a retry
	// on the same client would otherwise drive the previous attempt's cancelled
	// closures - transferring nothing and reporting a phantom 0 instead of -1
	// (N/A). Reset() rebuilds the direction state so every attempt re-registers.
	// It also zeroes the byte totals: reading the total only after the loop would
	// count the surviving attempt alone and silently drop the bytes a FAILED
	// attempt already pushed across the (possibly metered) link. So each attempt's
	// volume is tallied right after its transfer, before the next attempt's Reset
	// clears it, and the running total spans every attempt. That total is the run's
	// real data usage - independent of which attempt supplied the kept performance
	// numbers - so a retried transfer no longer understates "data used".
	var loadedDown, loadedUp *loadStat
	var downBytes, upBytes int64
	if dir != "up" {
		err = withRetryPred(ctx, retries, anyErr, func() error {
			freshManager(o, srv, o.uc) // per-attempt manager; see freshManager
			stop := startLoadSampler(ctx, probeAddr)
			// traceConnFamilies scopes the family record to the transfer's own
			// requests - the load sampler above probes a different host.
			finished, e := runTransfer(traceConnFamilies(ctx, fams), srv, ooklaDownload)
			loadedDown = stop() // our own sampler, always joined
			// Tally what THIS attempt really pulled, whatever became of it. The
			// bytes came off the user's allowance either way, and the figure is
			// about to become unreachable: the next attempt's freshManager hands
			// out a manager whose counter starts at zero. An ABANDONED attempt is
			// counted here too - it used to return having counted nothing, so a
			// user who cancelled during the download phase (the FIRST phase of
			// every run, and so the likeliest moment to cancel) had the whole
			// run's usage reported as 0 bytes after moving hundreds of MB, and
			// the scheduler's zero-usage guard then dropped the row entirely.
			downBytes += srv.Context.GetTotalDownload()
			if !finished {
				// srv and the shared client now belong to the orphaned transfer
				// (see runTransfer), and this attempt does not come back for
				// another go - the rebuild above would race its workers.
				//
				// The volume counter read just above is the ONE thing that may
				// still be read off an abandoned manager. GetTotalDownload and
				// GetTotalUpload are atomic loads of counters the library's
				// workers only ever add to, and srv.Context is only ever assigned
				// from the run's own goroutine (freshManager here, and server
				// selection before any transfer exists), never by the transfer, so
				// the read races nothing. It reports the total as of now and
				// misses whatever the orphan pulls after we walked away, which is
				// the honest answer to "what had this run spent when the user
				// cancelled it".
				//
				// GetUploadBacklog, which uploadSpent adds below, is not one load
				// but two - total read volume minus total upload, clamped at zero.
				// Both operands are atomic so it still races nothing, but a
				// confirmation landing between them makes the sum miss that
				// delta. Bounded, and it errs toward under-reporting usage rather
				// than over-reporting it, which is the right direction for a
				// figure that bills the operator.
				//
				// Nothing else on an abandoned manager is safe: DLSpeed/ULSpeed
				// and TestDuration are plain fields the library writes without
				// synchronisation on its way out, and Reset swaps the snapshot and
				// both directions with no lock at all.
				return e
			}
			if e == nil && ctx.Err() != nil { // the lib can return nil on cancellation
				e = ctx.Err()
			}
			if e == nil { // -1 = failed transfer (see naErr)
				e = naErr(srv.DLSpeed)
			}
			return e
		})
		if err != nil {
			// Carry the safely-tallied bytes out with the error: the traffic was
			// really spent (and lands on the user's bill) whether or not the
			// measurement survived. An ABANDONED transfer's bytes are in there
			// too - its counter is read before the guard above returns - so a
			// cancelled run reports what it moved instead of 0.
			return Result{DownloadBytes: downBytes, UploadBytes: upBytes}, fmt.Errorf("download: %w", err)
		}
	}
	if dir != "down" {
		// The rescue predicate and the diagnosis must agree, so both take the same
		// derived bound (see starvationCeiling), sized to the connection count THIS
		// run's transfers actually use (see runConnections) rather than a fresh
		// live read that a mid-run settings change could have moved.
		wantConns := o.runConnections()
		ceiling := starvationCeiling(wantConns)
		// Starvation rescue. min(NumCPU, 8) workers each POST a ~1 MB chunk
		// CONCURRENTLY into a fixed capture window; sharing one uplink they
		// advance in lockstep, so the first confirmation needs the whole parallel
		// set through - about 6.4 Mbps on an 8-worker host. Below that nothing
		// ever confirms and the run reports N/A for a link that is uploading
		// perfectly well, just slowly.
		//
		// The rescue is DEFERRED to a retry rather than applied up front, because
		// fewer workers is actively harmful on a healthy link: measured at 25 ms
		// RTT, throughput is linear in worker count (8 -> 1684 Mbps, 4 -> 837,
		// 2 -> 417, 1 -> 208), so a blanket reduction would under-report every
		// fast connection - recreating #17 inverted. The trigger below cannot
		// fire on a healthy run: it requires ZERO confirmed chunks, and a healthy
		// run confirms thousands within seconds.
		//
		// One worker rescues down to ~0.53 Mbps, and a single stream saturates a
		// slow link fine (the parallelism penalty is a high-BDP effect), so the
		// rescued measurement is accurate rather than merely non-empty.
		// The window in force before any rescue changes it. Restoring a hardcoded
		// constant would clobber a caller that set its own (tests do).
		attempt := 0
		rescued := false
		err = withRetryPred(ctx, retries, anyErr, func() error {
			attempt++
			starved := false
			if attempt > 1 && o.upRec != nil {
				if a, c := o.upRec.snapshot(); c == 0 && a > 0 && a <= ceiling {
					starved = true
					// Nothing completed, and attempts never exceeded one per
					// worker - the signature of starvation, not rejection
					// (rejection racks up thousands of instant failures).
					o.logf("retrying upload with a single stream after no chunk completed",
						"server", serverLabel(srv), "attempts", a)

					// A longer window as well as fewer streams. One stream needs
					// chunk/uplink seconds to land - 4 s at 2 Mbps - and it does not
					// get a clean link to do it in: the previous attempt's transfers
					// keep pulling bytes after their window closes (the same orphan
					// behaviour awaitQuietTransfers exists for), so the rescue starts
					// while the link is still busy. Doubling the window absorbs that
					// without changing anything for a healthy run, which never
					// reaches this branch.
					// NOT extended past ooklaCaptureTime: both orphan-drain waits are
					// sized at ooklaCaptureTime+5s, so a longer window would let an
					// abandoned rescue outlive the wait meant to outlast it.

					// Let the link settle first. The previous attempt's transfers
					// keep pulling bytes after their window closed, and the rescue
					// has a hard deadline of its own: the library ends the window
					// early once its rate series looks converged, and a series of
					// zeroes converges fast. Starting the single stream into a busy
					// link means it may not land a chunk before that fires. Measured:
					// without this wait the rescue still reported N/A.
					//
					select {
					case <-ctx.Done():
					case <-time.After(uploadRescueSettle):
					}
					stats.Inc("speed.upload_starvation_retry")
				}
			}
			// Fresh manager per attempt, not Reset() on a shared one - see
			// freshManager. This is what removes the v1.7.11 data race from our
			// side: two Start() calls no longer touch the same manager.running.
			// It also means there is nothing to "restore" afterwards, because the
			// next attempt and the next best-of candidate each start from the
			// run's UserConfig rather than inheriting a rescue's settings.
			freshManager(o, srv, o.uc)
			if o.upRec != nil {
				// The swap installs a NEW recorder, so the signature bound has to
				// travel with it - otherwise the diagnosis silently falls through to
				// the generic transport-error wording in exactly the starved case
				// it exists to describe. Counts are per-attempt now rather than
				// cumulative, which matches the bound (one attempt per worker).
				o.upRec.reset(ceiling)
			}
			if starved {
				// Applied to the NEW client; setting it before the swap would have
				// been thrown away with the old one.
				srv.Context.SetNThread(1)
				rescued = true
			}
			stop := startLoadSampler(ctx, probeAddr)
			finished, e := runTransfer(traceConnFamilies(ctx, fams), srv, ooklaUpload)
			loadedUp = stop()
			// Confirmed + backlog: a FAILED or ABANDONED attempt's pushed bytes
			// aren't in GetTotalUpload alone. Counted before the branch below for
			// the same reason as the download's tally - an abort left the user's
			// data usage claiming 0 for traffic that really went out.
			upBytes += uploadSpent(srv)
			if !finished {
				// Abandoned: srv belongs to the orphan now, and uploadSpent's
				// atomic counters are all that may be read off it (see above).
				return e
			}
			if e == nil && ctx.Err() != nil {
				e = ctx.Err()
			}
			if e == nil {
				e = naErr(srv.ULSpeed)
			}
			return e
		})
		if rescued && err == nil {
			// The rescue fired AND the run survived. Without this the only trace
			// is the retry counter, which cannot distinguish a rescue that saved
			// a measurement from one that merely tried.
			o.logf("single-stream retry rescued the upload measurement",
				"server", serverLabel(srv))
			stats.Inc("speed.upload_starvation_rescued")
		}
		if err != nil {
			// As above: the download that preceded this failed upload moved real
			// bytes; they travel out with the error.
			//
			// The recorder's summary is APPENDED, not substituted: %w keeps
			// errMeasurementNA in the chain and the "upload:" prefix stays first,
			// so speedFailStage still classifies this exactly as before.
			// Name the retry budget when it is what made the rescue unreachable.
			// The single-stream rescue is gated on being a SECOND attempt, and
			// withRetryPred runs the closure once when the budget is zero, so an
			// install that set retries to 0 can never reach it - on an uplink slow
			// enough to starve, that install loses every run and the diagnostic
			// above says only that no chunk finished inside the window. Nothing
			// pointed at the one field that fixes it, so say it here. Only when
			// the budget really is the blocker: with retries available the rescue
			// was tried, and blaming the setting would be wrong.
			hint := ""
			if retries <= 0 {
				hint = " (no retry budget: the single-stream fallback for a starved upload only runs on a second attempt - raise Retries above 0 in Settings to allow it)"
			}
			if o.upRec != nil {
				return Result{DownloadBytes: downBytes, UploadBytes: upBytes},
					fmt.Errorf("upload: %w [%s]%s", err, o.upRec.summary(), hint)
			}
			return Result{DownloadBytes: downBytes, UploadBytes: upBytes}, fmt.Errorf("upload: %w%s", err, hint)
		}
	}

	// Packet loss needs its own UDP pass; it stays nil when the user turns it off.
	var loss *float64
	var udpDir string
	if o.LossFn == nil || o.LossFn() {
		loss = ooklaLoss(ctx, srv)
		// The probe SENDS its datagrams (speedtest-go's PacketLossSender), so a
		// successful one measured the upstream path - the only direction this
		// engine can honestly claim for loss.
		if loss != nil {
			udpDir = "up"
		}
	}

	return Result{
		Engine:          "ookla",
		DownloadMbps:    srv.DLSpeed.Mbps(),
		UploadMbps:      srv.ULSpeed.Mbps(),
		PingMS:          util.DurMS(srv.Latency),
		PingBestMS:      msIfPositive(bestPing),
		JitterMS:        f64p(util.DurMS(srv.Jitter)),
		Server:          fmt.Sprintf("%s, %s", srv.Sponsor, srv.Name),
		ServerID:        srv.ID,
		PacketLoss:      loss,
		UDPDirection:    udpDir,
		IPFamily:        fams.family(),
		DownloadBytes:   downBytes,
		UploadBytes:     upBytes,
		IdleMS:          idleMS,
		LoadedDownMS:    loadedDown.medPtr(),
		LoadedUpMS:      loadedUp.medPtr(),
		LoadedDownP95MS: loadedDown.tailPtr(),
		LoadedUpP95MS:   loadedUp.tailPtr(),
	}, nil
}

// packetLossSampleDuration bounds the best-effort loss measurement. Loss uses a
// UDP protocol many networks/servers don't support, so this is kept short and
// failures are silently ignored (nil result).
const packetLossSampleDuration = 5 * time.Second

// plCooldown: where the UDP loss protocol is blocked, the probe wastes seconds
// per run for nothing. After two consecutive failures against a server we skip
// it for this long, then re-probe in case conditions changed. Keyed per host so
// one server's failures don't penalise another.
const plCooldown = 6 * time.Hour

type plState struct {
	fails     int
	skipUntil int64 // unix seconds; 0 = enabled
}

// plMapCap bounds plMap so cycling through many Ookla servers can't grow it
// forever; entries not in an active cooldown are evicted past it.
const plMapCap = 512

var (
	plMu  sync.Mutex
	plMap = map[string]*plState{}
)

// measurePacketLoss returns the loss percentage (0..100) against the chosen
// server, or nil when the measurement is unsupported or yields no data.
func measurePacketLoss(ctx context.Context, srv *ookla.Server) *float64 {
	plMu.Lock()
	st := plMap[srv.Host]
	if st == nil {
		// Bound the map: it'd otherwise grow one entry per distinct server forever.
		// When large, drop entries not in an active cooldown (no state worth keeping).
		if len(plMap) >= plMapCap {
			now := time.Now().Unix()
			for host, s := range plMap {
				if s.skipUntil == 0 || s.skipUntil < now {
					delete(plMap, host)
				}
			}
		}
		st = &plState{}
		plMap[srv.Host] = st
	}
	skip := st.skipUntil != 0 && time.Now().Unix() < st.skipUntil
	plMu.Unlock()
	if skip {
		stats.Inc("speed.loss_skip") // still in cooldown
		return nil                   // server recently didn't support it - skip the slow probe
	}

	pctx, cancel := context.WithTimeout(ctx, packetLossSampleDuration+time.Second)
	defer cancel()
	// Both dialers carry the SSRF dial guard: srv.Host is third-party catalogue
	// data like every other destination this file reaches, and left nil the
	// analyzer builds bare dialers of its own, so its TCP sampler connect and
	// UDP sends would bypass the guard entirely. probeDialControl rather than
	// probeDialGuard so allowLoopbackProbes relaxes this path too. The timeout
	// mirrors the library's own default (PacketSendingTimeout).
	analyzer := ookla.NewPacketLossAnalyzer(&ookla.PacketLossAnalyzerOptions{
		SamplingDuration: packetLossSampleDuration,
		TCPDialer:        &net.Dialer{Timeout: 5 * time.Second, Control: probeDialControl},
		UDPDialer:        &net.Dialer{Timeout: 5 * time.Second, Control: probeDialControl},
	})
	var loss *float64
	// Upstream leak (speedtest-go v1.7.11): RunWithContext opens a TCP sampler conn
	// and a UDP sender conn internally and never Disconnect()s them - on pctx.Done()
	// its sampler/sender loops just return, leaving both sockets for the Go runtime's
	// netpoll finalizer to close on the next GC rather than closing them promptly.
	// The conns are locals inside RunWithContext and never surface through the API,
	// so there is no handle to close them from here - a clean fix needs an upstream
	// Disconnect on ctx cancellation (or vendoring one). At one probe per run this is
	// a couple of fds living until the next GC, bounded and finalizer-reclaimed, not
	// an unbounded leak, so we accept it rather than fork.
	// TODO: drop this note once a released speedtest-go closes those conns on cancel.
	_ = analyzer.RunWithContext(pctx, srv.Host, func(pl *transport.PLoss) {
		if v := pl.LossPercent(); v >= 0 { // -1 means no packets acknowledged yet
			f := v
			loss = &f
		}
	})

	// A probe cut short by the caller (aborted run, shutdown) says nothing about
	// the server's UDP support: don't advance the cooldown or count the outcome.
	if loss == nil && ctx.Err() != nil {
		return nil
	}
	plMu.Lock()
	if loss == nil {
		if st.fails++; st.fails >= 2 {
			st.skipUntil = time.Now().Add(plCooldown).Unix()
		}
	} else {
		st.fails, st.skipUntil = 0, 0
	}
	plMu.Unlock()
	// Capability accounting: did the UDP loss protocol yield data? Per-outcome
	// counters (not a last-writer gauge) so the fleet view can size how much of
	// the install base gets trustworthy loss numbers.
	if loss != nil {
		stats.Inc("speed.loss_ok")
	} else {
		stats.Inc("speed.loss_none")
	}
	return loss
}

// Auto-select candidate bounds. Ookla geolocates most metro servers to the
// same city-centre coordinate, so "the nearest N" degenerates into an
// arbitrary N-of-a-tie that can exclude the best server (even the user's own
// ISP's on-net one, the most likely winner). Instead, every server that is
// latency-equivalent by distance gets to race: within autoMarginKM of the
// nearest (25 km of fiber is well under half a millisecond of round trip),
// never fewer than the nearest autoPingMin, capped at autoPingMax concurrent
// pings with one-server-per-sponsor preferred when trimming to the cap.
const (
	autoPingMin  = 5
	autoPingMax  = 12
	autoMarginKM = 25
	// autoISPMax is how many lanes the user's OWN ISP may hold when the pool is
	// over the cap: its on-net boxes are each likely winners and can differ in
	// load, so several race - but not so many that provider diversity dies.
	autoISPMax = 4
)

// autoCandidates picks which of the distance-sorted servers to ping-race. isp
// is the user's ISP display name ("" = unknown): the ISP's own server is the
// most likely winner (the path never leaves its network), so when Ookla lists
// one it is guaranteed a lane even when the distance margin or the diversity
// trim would have cut it - it still has to WIN the race on ping like everyone
// else.
func autoCandidates(sorted ookla.Servers, isp string) ookla.Servers {
	n := 0
	for n < len(sorted) && (n < autoPingMin || sorted[n].Distance <= sorted[0].Distance+autoMarginKM) {
		n++
	}
	pool := sorted[:n]
	cand := pool
	if len(pool) > autoPingMax {
		// Over the cap, in priority order: the user's own ISP's servers first (up
		// to autoISPMax - each on-net box is a likely winner), then the first
		// server of every other sponsor (distance order, so one provider's
		// equidistant entries can't crowd others out), then remaining nearest.
		cand = make(ookla.Servers, 0, autoPingMax)
		seen := map[string]bool{}
		taken := map[*ookla.Server]bool{}
		if isp != "" {
			ispLanes := 0
			for _, s := range pool {
				if ispLanes == autoISPMax || len(cand) == autoPingMax {
					break
				}
				if sponsorMatchesISP(s.Sponsor, isp) {
					cand = append(cand, s)
					taken[s] = true
					seen[s.Sponsor] = true
					ispLanes++
				}
			}
		}
		var dupes ookla.Servers
		for _, s := range pool {
			if len(cand) == autoPingMax {
				break
			}
			if taken[s] {
				continue
			}
			if seen[s.Sponsor] {
				// ISP entries beyond their lane cap stay OUT of the fill too, or
				// the cap would quietly leak whenever unique sponsors run short.
				if isp == "" || !sponsorMatchesISP(s.Sponsor, isp) {
					dupes = append(dupes, s)
				}
				continue
			}
			seen[s.Sponsor] = true
			cand = append(cand, s)
		}
		for _, s := range dupes {
			if len(cand) == autoPingMax {
				break
			}
			cand = append(cand, s)
		}
	}
	// ISP guarantee: if the user's own ISP sponsors a server anywhere in the
	// fetched (already nearby) list and none made the cut, swap it in for the
	// last pick - the least-diverse / farthest one.
	if isp == "" {
		return cand
	}
	// cand may alias sorted's backing array (the under-cap path); take a copy
	// before the append/overwrite below can write through into the caller's
	// slice. rankedServers passes a private copy today, but that's its
	// business, not a contract.
	cand = append(ookla.Servers(nil), cand...)
	for _, s := range cand {
		if sponsorMatchesISP(s.Sponsor, isp) {
			return cand
		}
	}
	for _, s := range sorted {
		if sponsorMatchesISP(s.Sponsor, isp) {
			if len(cand) < autoPingMax {
				cand = append(cand, s)
			} else {
				cand[len(cand)-1] = s
			}
			break
		}
	}
	return cand
}

// ispGenericWords are tokens too common in provider names to identify one:
// matching on them ("wireless", "canada", ...) would make half the server list
// look like the user's ISP.
var ispGenericWords = map[string]bool{
	"internet": true, "communications": true, "telecom": true, "telecommunications": true,
	"network": true, "networks": true, "wireless": true, "mobility": true, "mobile": true,
	"cable": true, "fibre": true, "fiber": true, "broadband": true, "services": true,
	"solutions": true, "group": true, "canada": true, "the": true, "inc": true,
	"llc": true, "ltd": true, "corp": true, "co": true, "and": true,
}

// sponsorMatchesISP reports whether an Ookla server sponsor plausibly IS the
// user's ISP (sponsor "EBOX" vs ISP "AS1403 EBOX - EBOX"). Word-level and
// case-insensitive: some sponsor word of 3+ characters, not a generic industry
// word, must appear as a whole word in the ISP name. Best-effort - a false
// negative just loses the guaranteed lane, a false positive only grants one
// extra racer.
func sponsorMatchesISP(sponsor, isp string) bool {
	words := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		})
	}
	ispWords := map[string]bool{}
	for _, w := range words(isp) {
		ispWords[w] = true
	}
	for _, w := range words(sponsor) {
		if len(w) >= 3 && !ispGenericWords[w] && ispWords[w] {
			return true
		}
	}
	return false
}

// rankedServers pings the auto-select candidates concurrently and returns them
// best-first: successfully pinged servers by ascending latency, then any that
// never answered, still in candidate (nearest-first) order. That trailing group
// is why the head is a safe blind pick when no ping succeeds at all - it is the
// nearest, or over the cap the nearest ISP lane (on-net).
//
// Best-of-N reads ranks 2 and 3 straight off this list: the pings already
// happened to choose rank 1, so the extra servers cost nothing to identify.
// rankPingLatency decides what a ranking ping recorded and whether it counts as
// answered. A SUCCESSFUL ping (nil error, at least one sample) whose mean rounds
// to zero on a coarse clock - the sub-millisecond Windows case - is clamped to
// the smallest positive duration, so the fastest server still ranks first and
// its selection-report row reads answered (~0ms) rather than unanswered below
// every slower server. A failed or unsampled ping is not answered: its Latency
// holds a stale list-fetch echo that must never enter the ranking.
func rankPingLatency(err error, sampled bool, latency time.Duration) (time.Duration, bool) {
	if err != nil || !sampled {
		return 0, false
	}
	if latency <= 0 {
		return time.Nanosecond, true
	}
	return latency, true
}

// rankedServers returns the ranked candidates, their ranking pings, and the IDs
// dropped for having no HTTP Legacy Fallback (see fallbackHealth) so the caller
// can say so in the log.
func rankedServers(ctx context.Context, servers ookla.Servers, isp string) (ookla.Servers, map[string]*float64, []string, bool) {
	if len(servers) == 0 {
		return nil, nil, nil, false
	}
	// Sort a copy so the caller's slice order is untouched.
	sorted := append(ookla.Servers(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Distance < sorted[j].Distance })
	cand := autoCandidates(sorted, isp)

	// Reserves: the next servers past the candidate cap, so health filtering can
	// REPLENISH rather than merely subtract. autoCandidates caps at autoPingMax,
	// and filtering afterwards used to shrink that set with nothing to refill it
	// - if the nearest 12 all lacked the fallback, a working 13th was never
	// considered and the run fell back to measuring the known-bad 12. Partial
	// attrition could likewise leave a best-of-3 with fewer than three targets.
	// Reserves are contacted LAZILY, in shortfall-sized batches, only after
	// exclusions actually leave the pool short: a healthy pool sends them zero
	// traffic, and N exclusions ping/probe only ~N of them (a further batch per
	// dead reserve), instead of doubling every selection pass's third-party
	// fan-out unconditionally.
	inPool := make(map[string]bool, len(cand))
	for _, s := range cand {
		inPool[s.ID] = true
	}
	reserve := make(ookla.Servers, 0, autoPingMax)
	for _, s := range sorted {
		if len(reserve) == cap(reserve) {
			break
		}
		if !inPool[s.ID] {
			reserve = append(reserve, s)
		}
	}

	// probe pings one batch concurrently and judges each server's fallback in
	// the same fan-out. rankPings is the explicit per-server ranking outcome,
	// for the selection report: the Latency FIELD alone cannot carry it - the
	// library assigns the ~10-sample mean only on success, and a failed ranking
	// ping leaves whatever the list fetch wrote there, often a stale positive
	// echo sample. applyRankPing both records the honest outcome AND fixes the
	// field (measured on success, ZERO on failure) so the sort below can't rank
	// a stale value first. Only contacted servers get an entry.
	rankPings := make(map[string]*float64, len(cand))
	probe := func(set ookla.Servers) []endpointState {
		pings := make([]*float64, len(set))
		health := make([]endpointState, len(set))
		var wg sync.WaitGroup
		for i, s := range set {
			wg.Add(1)
			go func(i int, s *ookla.Server) {
				defer wg.Done()
				// The per-sample callback is the only honest success signal: the
				// library can return a nil error WITHOUT collecting a single sample
				// (every measured echo failing at transport level after a good
				// warm-up), leaving Latency untouched at its stale fetch-echo
				// value - so "err == nil && Latency > 0" would record a one-shot
				// echo as an answered ten-sample ranking ping.
				sampled := false
				err := ooklaPing(ctx, s, func(time.Duration) { sampled = true }) // sets s.Latency on success
				pings[i] = applyRankPing(s, err, sampled)
				// Same fan-out, so this costs the phase nothing it was not already
				// spending: judge the fallback by STATUS, which the library's ping
				// does not do (see fallbackHealth).
				health[i] = fallbackHealth(ctx, s)
			}(i, s)
		}
		wg.Wait()
		for i, s := range set {
			rankPings[s.ID] = pings[i]
		}
		return health
	}

	// Drop servers whose legacy fallback is gone. They are not slow or flaky -
	// every transfer against them fails - so ranking them at all only guarantees
	// a wasted run, and because their 500 is served instantly they otherwise sort
	// as if healthy.
	health := probe(cand)
	usable := make(ookla.Servers, 0, len(cand))
	var dropped []string
	for i, s := range cand {
		if health[i] == endpointRetired {
			dropped = append(dropped, s.ID)
			continue
		}
		usable = append(usable, s) // endpointUnknown stays: a failed probe is not a verdict
	}
	// Replenish only what the exclusions actually cost, one shortfall-sized
	// batch at a time; a dead reserve just widens the next batch. Reserves
	// dropped here are not reported - dropped names pool exclusions only, which
	// is what the caller logs.
	for len(usable) < len(cand) && len(reserve) > 0 {
		n := len(cand) - len(usable)
		if n > len(reserve) {
			n = len(reserve)
		}
		batch := reserve[:n]
		reserve = reserve[n:]
		for i, st := range probe(batch) {
			if st != endpointRetired {
				usable = append(usable, batch[i])
			}
		}
	}
	// Never rank nothing. A whole region can lack the fallback, and a measurement
	// that probably fails still beats "no speedtest servers available" - the
	// failure will at least carry a diagnosis naming the cause.
	allDead := false
	if len(usable) == 0 {
		usable = append(ookla.Servers(nil), cand...)
		dropped = nil
		allDead = true // reported below; silence here would hide the worst case
	}

	out := usable
	// Stable so unpinged servers keep their nearest-first order among themselves.
	sort.SliceStable(out, func(i, j int) bool { return rankLess(out[i], out[j]) })
	return out, rankPings, dropped, allDead
}

// applyRankPing records a ranking-ping outcome on the server and returns the
// measured latency (ms) for the selection report, or nil on failure. On success
// it writes the measured value (a clamped near-zero, never 0, so the fastest
// server still sorts first); on FAILURE it zeros Latency, so a stale discovery
// echo can't rank an unreachable server ahead of a reachable one measured slower.
func applyRankPing(s *ookla.Server, err error, sampled bool) *float64 {
	if lat, answered := rankPingLatency(err, sampled, s.Latency); answered {
		s.Latency = lat
		ms := pingMSOf(lat)
		return &ms
	}
	s.Latency = 0
	return nil
}

// rankLess orders ranked servers: a measured server (Latency>0) always before an
// unreachable one (Latency==0, including a failed ranking ping), lowest latency
// first among the measured.
func rankLess(a, b *ookla.Server) bool {
	la, lb := a.Latency, b.Latency
	switch {
	case la > 0 && lb > 0:
		return la < lb
	case la > 0:
		return true
	default:
		return false
	}
}

// serverLabel is the human name for a server, as stored on the run.
func serverLabel(s *ookla.Server) string {
	return fmt.Sprintf("%s, %s", s.Sponsor, s.Name)
}

// bestResult picks the winner of a best-of-N run, by the user's stated rule:
// total throughput discounted by ping (see resultScore) first, then latency,
// then jitter, then bufferbloat. Every tie-break is a strict improvement test,
// so an exact tie keeps the earlier result - and the earlier result is the
// higher-ranked server (the pinned one, or the lowest ping), which is the
// right thing to fall back on.
//
// Later keys are near-impossible to reach in practice: two separate runs would
// have to agree to the full float precision. They exist so the choice is
// deterministic rather than accidental.
func bestResult(rs []Result, dir string) Result { return rs[bestIndex(rs, dir)] }

// pingWeightMS sets how much latency discounts throughput in resultScore: a
// result's down+up is multiplied by pingWeightMS/(pingWeightMS+ping). At 100,
// one millisecond of ping is worth about 1% of throughput at typical last-mile
// pings, so a near-tie on speed (950 total at 10ms vs 940 at 6ms) goes to the
// snappier server, while a genuinely faster server still wins through - the
// weight must never turn a speedtest into a ping contest.
const pingWeightMS = 100

// maxScorePingMS caps the ping the score charges for. The discount exists to
// settle near-ties, not to bury a demonstrated line speed: uncapped, a 20ms+
// spread inside one round (a pinned far server, a latency blip on one target)
// let a measurement 15%+ slower win and understate the connection. Past the
// cap the discount tops out (~17%), so widely-spread servers compare on
// throughput alone.
const maxScorePingMS = 20

// unmeasuredPingMS stands in for a run whose ping was never measured, scoring
// it as worse than any real-world path (geostationary satellite is ~600ms).
// Absence can't win - the same philosophy as the tie-breaks below - but the
// run's throughput still counts, rather than being zeroed outright. It
// deliberately bypasses maxScorePingMS: the cap protects measured pings, not
// missing ones.
const unmeasuredPingMS = 1000

// downCapacityWeight splits the capacity score between the two directions, as
// EXPONENTS rather than multipliers. That is the whole point: a weighted
// geometric mean makes each direction's weight RELATIVE, so a 1% better
// download is worth 0.70% of capacity and a 1% better upload 0.30%, on a
// symmetric line and a 20:1 asymmetric one alike.
//
// The score it replaces was download+upload, and a sum cannot do that. On a
// 500/25 line the upload is 5% of the total, so a server that could only manage
// a fifth of the real upload gave up 4% of its score and won anyway - best-of
// existed to route around exactly that server. Any fixed multiplier (down + 2*up)
// just moves the arbitrary ratio around: it means something different on every
// access plan, because it compares Mbps to Mbps.
//
// 0.70 keeps download primary, which is right for the everyday sense of "how
// fast is this connection". It is a judgement, not a measurement, and the
// capacity figure is logged per candidate so real rounds can be replayed
// against a different split before changing it.
const downCapacityWeight = 0.70

// resultCapacity is the throughput half of the ranking: a weighted geometric
// mean of the two directions for a bidirectional run, and the measured
// direction alone when only one was asked for. Carries units of Mbps
// (0.70+0.30 = 1), so it is comparable across rounds and worth logging.
//
// dir must be the direction the run was CONFIGURED for, not inferred from the
// values: under the old sum a skipped direction contributed 0 to every
// candidate equally and cancelled out, but zero is absorbing in a product, so
// inferring it would score every candidate in a down-only round at 0 and hand
// the round to the ping tie-break.
//
// Negative rates are clamped to zero first. The library reports an unusable
// transfer as -1 (see naErr), and math.Pow of a negative base with a fractional
// exponent is NaN - which every comparison in betterResult would answer false
// to, silently freezing bestIndex on whichever candidate it started with.
func resultCapacity(r Result, dir string) float64 {
	d, u := math.Max(0, r.DownloadMbps), math.Max(0, r.UploadMbps)
	switch dir {
	case "down":
		return d
	case "up":
		return u
	default:
		return math.Pow(d, downCapacityWeight) * math.Pow(u, 1-downCapacityWeight)
	}
}

// implausibleFactor is how far above the round's middle reading a single
// direction may go before it stops being a measurement of this connection.
//
// The failure it catches is one-sided and unbounded. A bad server can only
// UNDER-report the line - it runs out of capacity of its own - but a server
// that accepts bytes faster than it delivers them can appear to over-report
// without limit, because the client counts what it handed to the socket. On the
// link this was built against, 544 runs across four unrelated operators top out
// at 49.95 Mbps up (a 50 Mbps plan cap), while a handful of distant servers
// report 150-240 and claim 485-593 MB moved inside a 15-second window - four to
// six times what the line can carry. Higher-RTT servers show it worst, which is
// the tell: more data is in flight, so more sent-but-undelivered data is still
// counted when the capture closes.
//
// That matters here specifically because best-of-N is a MAX-selector. Scoring
// cannot fix a corrupt input - it can only choose it - so with one inflated
// candidate in the round the round is lost before resultScore runs. 19% of that
// link's stored history is this artefact.
//
// 2x is deliberately generous. A server genuinely routing around two poor ones
// is the case best-of exists for and must survive; being three times the middle
// of the round is not that.
const implausibleFactor = 2.0

// consensusSpread is how closely the REST of the round must agree with each
// other before their agreement is allowed to discredit the odd one out.
//
// This is the condition that separates the two shapes a round-local test cannot
// otherwise tell apart, because both are "one reading far above the middle":
//
//	48, 49, 151 -> the others are 2% apart. They have established what this
//	               line does, and 151 contradicts them             -> reject
//	10, 20, 500 -> the others are 100% apart. They have established nothing,
//	               so they cannot convict the third - and this is precisely
//	               the round best-of exists to win: two upload-limited
//	               servers and one good one                        -> believe
//
// Without it the guard suppresses the feature it is guarding. 1.25 is loose
// enough for ordinary run-to-run variation between two healthy servers and far
// tighter than the gap between a working server and a limited one.
const consensusSpread = 1.25

// implausibleDirections reports which measured directions in a round cannot be
// believed: those exceeding implausibleFactor times the round's MIDDLE reading
// while every other reading agrees with the others to within consensusSpread.
//
// Fewer than three measurements have no majority to consult, so nothing is
// rejected: two readings that disagree cannot say which of them is wrong.
// Nothing here reads history or a fixed speed limit - only the round.
func implausibleDirections(rs []Result) (down, up bool) {
	if len(rs) < 3 {
		return false, false
	}
	beyond := func(pick func(Result) float64) bool {
		vals := make([]float64, 0, len(rs))
		for _, r := range rs {
			if v := pick(r); v > 0 {
				vals = append(vals, v)
			}
		}
		if len(vals) < 3 {
			return false
		}
		sort.Float64s(vals)
		top, mid := vals[len(vals)-1], vals[len(vals)/2]
		if mid <= 0 || top <= mid*implausibleFactor {
			return false
		}
		// The others must agree with EACH OTHER, or there is no consensus for
		// the outlier to be contradicting.
		rest := vals[:len(vals)-1]
		return rest[0] > 0 && rest[len(rest)-1] <= rest[0]*consensusSpread
	}
	return beyond(func(r Result) float64 { return r.DownloadMbps }),
		beyond(func(r Result) float64 { return r.UploadMbps })
}

// believableCapacity is resultCapacity with the round's implausible directions
// held at the middle reading rather than the reported one. Held, not dropped:
// the direction WAS measured, and the rest of the round agrees on roughly what
// it measures, so the honest reading of an inflated candidate is "no better
// than everyone else here" - not zero, which would eject a server that may be
// perfectly good in the other direction.
func believableCapacity(r Result, dir string, rs []Result) float64 {
	badDown, badUp := implausibleDirections(rs)
	if !badDown && !badUp {
		return resultCapacity(r, dir)
	}
	capped := r
	if badDown {
		capped.DownloadMbps = math.Min(r.DownloadMbps, middleOf(rs, func(x Result) float64 { return x.DownloadMbps }))
	}
	if badUp {
		capped.UploadMbps = math.Min(r.UploadMbps, middleOf(rs, func(x Result) float64 { return x.UploadMbps }))
	}
	return resultCapacity(capped, dir)
}

// middleOf is the round's middle positive reading for one direction, or 0 when
// nothing measured it.
func middleOf(rs []Result, pick func(Result) float64) float64 {
	vals := make([]float64, 0, len(rs))
	for _, r := range rs {
		if v := pick(r); v > 0 {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	return vals[len(vals)/2]
}

// resultScore is the primary ranking key of a best-of-N run: capacity
// discounted by ping (see resultCapacity, pingWeightMS, maxScorePingMS).
// Higher is better.
//
// Nothing here is round-relative, deliberately. Normalising each direction by
// the round's maximum was considered and is inert: the capacity form is
// multiplicative, so a common divisor cancels out of every pairwise comparison
// and cannot reorder anything. It would also make the logged capacity relative
// to one round's field, so rounds could no longer be compared with each other -
// losing the one thing the log is for.
//
// A candidate that failed a required direction needs no special case: it scores
// 0 and loses to any candidate that measured both. If EVERY candidate is
// incomplete they all score 0 and the tie-breaks below decide, which is a
// defined outcome rather than a fallback to invent.
func resultScore(r Result, dir string) float64 { return roundScore(r, dir, nil) }

// roundScore is resultScore judged against the round r belongs to, so a
// direction the rest of the round contradicts cannot decide the winner (see
// believableCapacity). A nil round scores the result on its own, which is what
// a single-server run has: one measurement, nothing to check it against.
func roundScore(r Result, dir string, rs []Result) float64 {
	ping := decisionPingMS(r)
	if !validMS(ping) {
		ping = unmeasuredPingMS
	} else if ping > maxScorePingMS {
		ping = maxScorePingMS
	}
	capacity := resultCapacity(r, dir)
	if rs != nil {
		capacity = believableCapacity(r, dir, rs)
	}
	return capacity * pingWeightMS / (pingWeightMS + ping)
}

// f64v renders an optional measurement for a log line: the value, or NaN when it
// was never measured. Keeps a nil jitter from printing as a pointer address.
func f64v(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

// totalBytes is everything a run actually moved, across every server measured.
// The losing MEASUREMENTS are discarded, but their traffic was really spent and
// still lands on the user's bill, so the winner carries the whole run's volume.
// Without this "Data used" would report a third of what best-of-3 transfers,
// while the settings estimate (correctly) forecasts all of it.
func totalBytes(rs []Result) (down, up int64) {
	for _, r := range rs {
		down += r.DownloadBytes
		up += r.UploadBytes
	}
	return down, up
}

// bestIndex is bestResult's position, so callers can tell the winner apart from
// a loser that happens to carry an identical server label.
func bestIndex(rs []Result, dir string) int {
	win := 0
	for i, r := range rs[1:] {
		if betterResult(r, rs[win], dir, rs) {
			win = i + 1
		}
	}
	return win
}

// betterResult reports whether a beats b on the ranking. Unmeasured values (nil
// jitter, no bufferbloat samples) lose their tie-break rather than winning it by
// being absent - a run that could not measure jitter has not proved it is
// steadier than one that did.
func betterResult(a, b Result, dir string, round []Result) bool {
	// 1. Score: capacity discounted by ping, judged against the round (see
	// roundScore, believableCapacity).
	if as, bs := roundScore(a, dir, round), roundScore(b, dir, round); as != bs {
		return as > bs
	}
	// 2. Latency (lower wins). A run with no ping figure can't win here. Judged on
	// the same floor the score above uses, so the two steps can't disagree about
	// which server is the faster one.
	if ap, bp := decisionPingMS(a), decisionPingMS(b); ap != bp {
		return validMS(ap) && (!validMS(bp) || ap < bp)
	}
	// 3. Jitter (lower wins).
	if c := cmpOptLower(a.JitterMS, b.JitterMS); c != 0 {
		return c < 0
	}
	// 4. Bufferbloat (lower wins): the worst of the two loaded-vs-idle deltas.
	return cmpOptLower(bufferbloatMS(a), bufferbloatMS(b)) < 0
}

// validMS rejects the sentinel/unset latency values so they can't win a tie-break.
func validMS(v float64) bool { return v > 0 }

// msIfPositive converts a measured duration to milliseconds, or nil when nothing
// was measured. A zero here means "no sample survived", not "0 ms", and storing
// it as 0 would let an unmeasured server win every latency comparison outright.
func msIfPositive(d time.Duration) *float64 {
	if d <= 0 {
		return nil
	}
	ms := util.DurMS(d)
	if ms <= 0 {
		// DurMS truncates to whole microseconds, so the race's 1ns "measured
		// zero" clamp (keepFastestPing) came out as 0 here - and a stored 0
		// reads as "nothing measured" to validMS, sending every decision back
		// to the unfiltered mean. Keep the evidence positive end to end.
		ms = 0.001
	}
	return f64p(ms)
}

// decisionPingMS is the latency figure this run's CHOICES are made on: the
// fastest sample when the engine gave us one, else the reported mean.
//
// The split is deliberate. PingMS is the engine's own number and stays that way
// so what we display keeps matching what speedtest.net would say - but it is a
// mean over ten samples with no outlier resistance, and one stalled handshake
// moves it several-fold. Anything that DECIDES on latency (which server wins,
// whether the ping threshold breached) asks "how fast is this link", and one
// pothole is not the answer to that. iperf3 reports no per-sample values, so it
// falls back to the mean and behaves exactly as it did before.
func decisionPingMS(r Result) float64 {
	if r.PingBestMS != nil && validMS(*r.PingBestMS) {
		return *r.PingBestMS
	}
	return r.PingMS
}

// cmpOptLower orders two optional measurements, lower-is-better, with a missing
// value ranked last. Returns -1 if a wins, 1 if b wins, 0 if neither does.
func cmpOptLower(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a < *b:
		return -1
	case *a > *b:
		return 1
	}
	return 0
}

// bufferbloatMS is the added delay under load: the worse of the download and
// upload medians minus the idle baseline. nil when the run has no idle baseline
// or measured neither loaded phase - see Result's latency-under-load fields.
//
// Floored at zero, because "less delay under load than at rest" is not a
// measurement of anything - it is the anycast target answering from a nearer
// PoP during the loaded phase than during the idle burst, or ordinary noise
// between two small samples. Unfloored it was actively harmful: this is a
// lower-is-better tie-break in best-of (see betterResult), so a run whose
// baseline drifted could post a NEGATIVE delta and beat an honest zero-bloat
// run, and the corrupted run is the one that gets stored. The UI has always
// clamped the same subtraction for display; this makes the backend agree
// rather than leaving two rules for one quantity.
func bufferbloatMS(r Result) *float64 {
	if r.IdleMS == nil {
		return nil
	}
	worst := math.NaN()
	for _, loaded := range []*float64{r.LoadedDownMS, r.LoadedUpMS} {
		if loaded == nil {
			continue
		}
		if d := math.Max(0, *loaded-*r.IdleMS); math.IsNaN(worst) || d > worst {
			worst = d
		}
	}
	if math.IsNaN(worst) {
		return nil
	}
	return &worst
}

// ServerInfo is a selectable Ookla server for the UI.
type ServerInfo struct {
	ID         string  `json:"id"`
	Sponsor    string  `json:"sponsor"`
	Name       string  `json:"name"`
	Country    string  `json:"country"`
	DistanceKM float64 `json:"distance_km"`
	// Lat/Lon are not serialized: they carry the catalog's REGISTERED server
	// coordinates so the browse handler can centre a metro fetch on a server
	// it trusts. Filled by the list/search fetches, whose values proved real;
	// GetOoklaServer leaves them zero - its endpoint backfills the caller's
	// own position on sparse records and must never centre anything.
	Lat float64 `json:"-"`
	Lon float64 `json:"-"`

	// FallbackOK reports whether this server still serves the HTTP Legacy
	// Fallback that speedtest-go requires (see fallbackHealth). nil = not
	// determined. false means every speedtest against it WILL fail, while the
	// server may be perfectly healthy for Ookla's own client - so the picker
	// should mark it rather than pretend it is a normal choice. Users can still
	// pin one by ID on purpose; nothing here blocks that.
	FallbackOK *bool `json:"fallback_ok,omitempty"`
}

// annotateFallback fills FallbackOK for a browse/search list, probing verdicts
// concurrently. Cached, so the cost lands on the first listing of a server and
// not on every keystroke. Bounded concurrency keeps a picker open from fanning
// out 60+ sockets at once.
func annotateFallback(ctx context.Context, servers ookla.Servers, out []ServerInfo) {
	if len(servers) != len(out) {
		return // caller built the slices differently; do not guess at pairing
	}
	// Hard budget for the WHOLE annotation. Per-probe timeouts are not enough:
	// a 63-server list at 12 concurrent is 6 waves, so an unresponsive metro
	// would hang the picker for 6 x fallbackProbeTimeout = ~36 s. This is a
	// decoration on a list the user is waiting to see - whatever has not
	// answered by the deadline simply stays nil (undetermined), which is both
	// the honest verdict and the safe one, since ranking never excludes unknown.
	ctx, cancel := context.WithTimeout(ctx, annotateFallbackBudget)
	defer cancel()

	sem := make(chan struct{}, 12)
	states := make([]endpointState, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func(i int, srv *ookla.Server) {
			defer wg.Done()
			// Context-aware: once the annotation budget expires, queued probes must
			// abandon rather than run against a dead context. Otherwise they fail
			// instantly, cache endpointUnknown for fallbackUnknownTTL, and a real
			// speedtest moments later reuses that non-verdict instead of probing.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				states[i] = endpointUnknown
				return
			}
			if ctx.Err() != nil {
				states[i] = endpointUnknown
				return
			}
			states[i] = fallbackHealth(ctx, srv)
		}(i, srv)
	}
	wg.Wait()
	undetermined := 0
	for _, st := range states {
		if st == endpointUnknown {
			undetermined++
		}
	}
	if undetermined > 0 {
		// The picker is best-effort under a hard budget; say when it came back
		// partly unverified rather than letting the list look authoritative.
		slog.Debug("speedtest server list partially unverified",
			"undetermined", undetermined, "of", len(states), "budget", annotateFallbackBudget)
	}
	for i := range out {
		switch states[i] {
		case endpointOK:
			ok := true
			out[i].FallbackOK = &ok
		case endpointRetired:
			bad := false
			out[i].FallbackOK = &bad
		} // endpointUnknown leaves it nil - a failed probe is not a verdict
	}
}

// ListOoklaServers returns the Ookla servers the API reports for a location,
// nearest first. Non-zero lat/lon returns servers near that coordinate (a city
// search, like speedtest.net's "change server"); otherwise near the caller's
// IP. Rows carry real registered coordinates and distances - but note Ookla
// registers many metro servers at their city's canonical centre point, so a
// fetch centred exactly there reads 0 km for that whole cohort (the UI
// suppresses the label; the stable sort keeps Ookla's order for the ties)
// while differently-registered neighbours keep real distances. An earlier
// comment here claimed the API rewrites positions to the query point - wrong:
// the identical coordinates were the canonical registrations themselves.
func ListOoklaServers(ctx context.Context, lat, lon float64) ([]ServerInfo, error) {
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	if lat != 0 || lon != 0 {
		// Through newAnchoredLocation, not ookla.NewLocation: this runs on a web
		// handler goroutine while a scheduled run's city race writes the same
		// library-global map from its own goroutines (see newAnchoredLocation).
		// Unlocked, opening the server picker during an auto run can abort the
		// daemon with "fatal error: concurrent map writes" - a runtime throw, not
		// a recoverable panic.
		uc.Location = newAnchoredLocation("custom", lat, lon)
	}
	client := newOoklaClient(uc)
	servers, err := client.FetchServerListContext(ctx)
	if err != nil {
		return nil, err
	}
	currentEndpoints(servers)
	out := make([]ServerInfo, 0, len(servers))
	for _, s := range servers {
		lat, lon, _ := serverCoord(s)
		out = append(out, ServerInfo{
			ID: s.ID, Sponsor: s.Sponsor, Name: s.Name,
			Country: s.Country, DistanceKM: s.Distance, Lat: lat, Lon: lon,
		})
	}
	// Before the sort: annotateFallback pairs by index with `servers`, and the
	// sort reorders `out` only.
	annotateFallback(ctx, servers, out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DistanceKM < out[j].DistanceKM })
	return out, nil
}

// SearchOoklaServers returns the servers whose catalog entry matches a keyword
// (Ookla matches name and sponsor substrings, worldwide), nearest first, with
// real registered coordinates on every row. The browse handler uses it as a
// COORDINATE ORACLE: it finds the last-run server's own row by ID and centres
// the ordinary metro fetch on that row's coordinates - never as the list
// itself, because a city-name cohort collapses to a single row whenever the
// measured server wears a suburb's name tag (measured: an Ottawa-scoped run
// landing on "Nepean, ON", a one-server name).
func SearchOoklaServers(ctx context.Context, keyword string) ([]ServerInfo, error) {
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, Keyword: keyword}
	servers, err := newOoklaClient(uc).FetchServerListContext(ctx)
	if err != nil {
		return nil, err
	}
	currentEndpoints(servers)
	out := make([]ServerInfo, 0, len(servers))
	for _, s := range servers {
		lat, lon, _ := serverCoord(s)
		out = append(out, ServerInfo{
			ID: s.ID, Sponsor: s.Sponsor, Name: s.Name,
			Country: s.Country, DistanceKM: s.Distance, Lat: lat, Lon: lon,
		})
	}
	// Before the sort: annotateFallback pairs by index with `servers`, and the
	// sort reorders `out` only.
	annotateFallback(ctx, servers, out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].DistanceKM < out[j].DistanceKM })
	return out, nil
}

// GetOoklaServer fetches one Ookla server by numeric ID so the UI can pin (and
// confirm the name of) an exact server without a city search.
func GetOoklaServer(ctx context.Context, id string) (ServerInfo, error) {
	srv, err := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}).FetchServerByIDContext(ctx, id)
	if err != nil {
		return ServerInfo{}, err
	}
	// NOTE the endpoint behind this (api/ios-config.php) is only trustworthy
	// for identity fields (ID, sponsor, name). Measured on server 1993: it
	// returned an empty country and - worse - the CALLER'S own geolocated
	// coordinates in the server's lat/lon attributes, so nothing here may
	// treat its position or distance as the server's. The browse handler
	// centres by the Name label through SearchOoklaServers instead.
	info := ServerInfo{ID: srv.ID, Sponsor: srv.Sponsor, Name: srv.Name, Country: srv.Country, DistanceKM: srv.Distance}

	// Pinning is a deliberate act and nothing here blocks it - but the user
	// deserves to know BEFORE they pin that this server will fail every run.
	// This endpoint returns Host="" (measured), so fallbackHealth has nothing to
	// build a probe URL from; probeEndpoint works off the URL and follows the
	// migration hop, which is why the pinned selection path already uses it. One
	// extra round trip on an explicit user action is a fair trade.
	switch probeEndpoint(ctx, srv) {
	case endpointOK:
		ok := true
		info.FallbackOK = &ok
	case endpointRetired:
		bad := false
		info.FallbackOK = &bad
		// Deliberately no log line here: this is package-level code with no access
		// to the run's logger, and a package-global slog call would bypass the
		// About-page ring buffer and logfilter's masking of the `server` key while
		// still printing to stderr. The verdict reaches the user through
		// FallbackOK, which is where the decision is actually made.
		stats.Inc("speed.pinned_server_no_fallback")
	} // endpointUnknown leaves it nil - a failed probe is not a verdict
	return info, nil
}
