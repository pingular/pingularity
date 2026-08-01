package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
}

// logf records a best-of-N step when a logger is wired, and is a no-op otherwise.
func (o *Ookla) logf(msg string, args ...any) {
	if o.Log != nil {
		o.Log.Debug(msg, args...)
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
	// New writes http.DefaultClient.Transport (the stamp) and the tail check
	// clears it; both are unsynchronized writes to a process-global. Serialize
	// them so concurrent newOoklaClient calls don't race each other on it -
	// go test -race stays clean.
	ooklaClientMu.Lock()
	defer ooklaClientMu.Unlock()
	doer := &http.Client{}
	client := ookla.New(ookla.WithDoer(doer), ookla.WithUserConfig(uc))
	if _, ok := http.DefaultClient.Transport.(*ookla.Speedtest); ok {
		http.DefaultClient.Transport = nil
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
	doer.Transport = panicSafeTransport{base: base, panics: &panicThrottle{}}
	return client
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
	client := newOoklaClient(uc)

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
	targets, err := o.pickServers(selCtx, client, servers, id, want, pinned)
	selCancel()
	if err != nil {
		return Result{}, err
	}

	// Sequentially, always: two speedtests at once would saturate the link and
	// each would measure the other's traffic as congestion.
	var results []Result
	var firstErr error
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
				if !o.resumeAfterAbandon(ctx, i, len(targets), serverLabel(srv), err) {
					break
				}
				continue
			}
			if len(targets) > 1 {
				o.logf("speedtest server failed, trying the next", "server", serverLabel(srv), "err", err)
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
		if firstErr != nil {
			return Result{}, firstErr
		}
		return Result{}, fmt.Errorf("no speedtest servers available")
	}

	// Say so when the round refused to believe a direction: the guard changes
	// which measurement becomes history, so it must never do that silently.
	if badDown, badUp := implausibleDirections(results); badDown || badUp {
		stats.Inc("speed.implausible_direction")
		for _, r := range results {
			if (badDown && r.DownloadMbps > middleOf(results, func(x Result) float64 { return x.DownloadMbps })) ||
				(badUp && r.UploadMbps > middleOf(results, func(x Result) float64 { return x.UploadMbps })) {
				o.logf("best-of result not believed: a direction exceeds what the rest of the round measured",
					"server", r.Server, "server_id", r.ServerID,
					"down_mbps", r.DownloadMbps, "up_mbps", r.UploadMbps, "ping_ms", r.PingMS,
					"mid_down_mbps", util.Round2(middleOf(results, func(x Result) float64 { return x.DownloadMbps })),
					"mid_up_mbps", util.Round2(middleOf(results, func(x Result) float64 { return x.UploadMbps })),
					"factor", implausibleFactor)
			}
		}
	}

	win := bestIndex(results, dir)
	if o.firstRunByPing(want) {
		win = lowestPingIndex(results)
		stats.Inc("speed.first_run_by_ping")
		o.logf("no speed history yet: deciding this round on ping alone, so an unverifiable "+
			"throughput reading cannot seed the baseline",
			"winner", results[win].Server, "ping_ms", results[win].PingMS, "servers", len(results))
	}
	best := results[win]
	if len(results) > 1 {
		discarded := make([]string, 0, len(results)-1)
		for i, r := range results {
			if i != win {
				discarded = append(discarded, r.Server)
			}
		}
		o.logf("best-of run finished", "servers", len(results), "winner", best.Server,
			"down_mbps", best.DownloadMbps, "up_mbps", best.UploadMbps,
			"capacity_mbps", util.Round2(believableCapacity(best, dir, results)),
			"score", util.Round2(roundScore(best, dir, results)), "discarded", strings.Join(discarded, " | "))
	}
	best.DownloadBytes, best.UploadBytes = totalBytes(results)
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
// means resolve it here.
func (o *Ookla) pickServers(ctx context.Context, client *ookla.Speedtest, servers ookla.Servers, id string, want int, pre *ookla.Server) (ookla.Servers, error) {
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
				return nil, fmt.Errorf("fetch server %s: %w", id, err)
			}
			pinned = s
		}
		if want <= 1 {
			return ookla.Servers{pinned}, nil
		}
	}

	isp := ""
	if o.ISPFn != nil {
		isp = o.ISPFn()
	}
	ranked := rankedServers(ctx, servers, isp)

	if want <= 1 {
		if len(ranked) == 0 {
			return nil, fmt.Errorf("no speedtest servers available")
		}
		return ookla.Servers{ranked[0]}, nil
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
		return nil, fmt.Errorf("no speedtest servers available")
	}
	return out, nil
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
// worker goroutines - one per CPU - loop until the DataManager's running flag is
// cleared, and only the transfer's own time.AfterFunc(captureTime) clears it
// (speedtest/data_manager.go, TestDirection.Start). Cancelling ctx merely makes
// every chunk request fail instantly, so the rest of the ooklaCaptureTime window
// becomes a hot spin. Waiting for the call to return therefore held the caller
// for up to that whole window after an abort: the UI spinner kept turning, the
// scheduler's single-flight flag stayed set so every new run got ErrBusy, and
// the shutdown drain gave up with "background workers did not stop in time".
//
// Ownership: when finished is false the transfer goroutine is STILL RUNNING and
// owns both srv - the library writes DLSpeed/ULSpeed/TestDuration on its way out
// - and srv.Context, which is not per-server state but the client that fetched
// it, shared by every target of a best-of run. After this returns false the
// caller must read no field of either and must not Reset that manager (Reset
// swaps the snapshot and both directions with no lock while the workers read
// them). Hence errTransferAbandoned: measure stops at the failed attempt rather
// than retrying, and RunReason ends the run rather than measuring the next
// target on the same client.
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
		// here too (select picks at random). The run is discarded either way, so
		// the only cost is not counting bytes that were about to be thrown out
		// with it - and a failed attempt's bytes were never counted anyway.
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
	return client.FetchServerListContext(ctx)
}

// measure runs one full measurement against an already-chosen server.
func (o *Ookla) measure(ctx context.Context, srv *ookla.Server, dir string, retries int) (Result, error) {
	var err error
	// Same ten samples the library already sends - the callback only keeps the
	// fastest alongside the mean it returns, so this costs no extra probe.
	var bestPing time.Duration
	if err := srv.PingTestContext(ctx, keepFastestPing(&bestPing)); err != nil {
		return Result{}, fmt.Errorf("ping: %w", err)
	}
	// Idle baseline for latency-under-load: same method/target as the loaded
	// samplers below (NOT the Ookla ping above), taken while the link is quiet, so
	// the idle-vs-loaded delta isolates the load effect.
	probeAddr := lulRunEndpoint()
	idleMS := measureIdleLatency(ctx, probeAddr)
	anyErr := func(error) bool { return true } // a failed transfer is worth retrying

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
			srv.Context.Reset()
			stop := startLoadSampler(ctx, probeAddr)
			finished, e := runTransfer(ctx, srv, ooklaDownload)
			loadedDown = stop() // our own sampler, always joined
			if !finished {
				// srv and the shared client now belong to the orphaned transfer
				// (see runTransfer): read nothing off them, and don't come back
				// for another attempt - the Reset above would race its workers.
				return e
			}
			downBytes += srv.Context.GetTotalDownload() // count this attempt before the next Reset zeroes it
			if e == nil && ctx.Err() != nil {           // the lib can return nil on cancellation
				e = ctx.Err()
			}
			if e == nil { // -1 = failed transfer (see naErr)
				e = naErr(srv.DLSpeed)
			}
			return e
		})
		if err != nil {
			return Result{}, fmt.Errorf("download: %w", err)
		}
	}
	if dir != "down" {
		err = withRetryPred(ctx, retries, anyErr, func() error {
			srv.Context.Reset()
			stop := startLoadSampler(ctx, probeAddr)
			finished, e := runTransfer(ctx, srv, ooklaUpload)
			loadedUp = stop()
			if !finished { // abandoned: srv belongs to the orphan now (see above)
				return e
			}
			upBytes += srv.Context.GetTotalUpload() // count this attempt before the next Reset zeroes it
			if e == nil && ctx.Err() != nil {
				e = ctx.Err()
			}
			if e == nil {
				e = naErr(srv.ULSpeed)
			}
			return e
		})
		if err != nil {
			return Result{}, fmt.Errorf("upload: %w", err)
		}
	}

	// Packet loss needs its own UDP pass; it stays nil when the user turns it off.
	var loss *float64
	if o.LossFn == nil || o.LossFn() {
		loss = measurePacketLoss(ctx, srv)
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
	analyzer := ookla.NewPacketLossAnalyzer(&ookla.PacketLossAnalyzerOptions{
		SamplingDuration: packetLossSampleDuration,
	})
	var loss *float64
	// Upstream leak (speedtest-go v1.7.10): RunWithContext opens a TCP sampler conn
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
	// slice. fastestServer passes a private copy today, but that's its
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

// fastestServer pings the candidate servers concurrently and returns the
// lowest-latency one (like speedtest.net's auto-select), falling back to the
// nearest if none respond. isp ("" = unknown) grants the user's own ISP's
// server a guaranteed lane in the race.
func fastestServer(ctx context.Context, servers ookla.Servers, isp string) *ookla.Server {
	ranked := rankedServers(ctx, servers, isp)
	if len(ranked) == 0 {
		return nil
	}
	return ranked[0]
}

// rankedServers pings the auto-select candidates concurrently and returns them
// best-first: successfully pinged servers by ascending latency, then any that
// never answered, still in candidate (nearest-first) order. That trailing group
// is why the head is a safe blind pick when no ping succeeds at all - it is the
// nearest, or over the cap the nearest ISP lane (on-net).
//
// Best-of-N reads ranks 2 and 3 straight off this list: the pings already
// happened to choose rank 1, so the extra servers cost nothing to identify.
func rankedServers(ctx context.Context, servers ookla.Servers, isp string) ookla.Servers {
	if len(servers) == 0 {
		return nil
	}
	// Sort a copy so the caller's slice order is untouched.
	sorted := append(ookla.Servers(nil), servers...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Distance < sorted[j].Distance })
	cand := autoCandidates(sorted, isp)

	var wg sync.WaitGroup
	for _, s := range cand {
		wg.Add(1)
		go func(s *ookla.Server) {
			defer wg.Done()
			_ = s.PingTestContext(ctx, nil) // sets s.Latency
		}(s)
	}
	wg.Wait()

	out := append(ookla.Servers(nil), cand...)
	// Stable so unpinged servers keep their nearest-first order among themselves.
	sort.SliceStable(out, func(i, j int) bool {
		li, lj := out[i].Latency, out[j].Latency
		switch {
		case li > 0 && lj > 0:
			return li < lj
		case li > 0:
			return true // a measured server always beats an unreachable one
		default:
			return false
		}
	})
	return out
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
	return f64p(util.DurMS(d))
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
}

// ListOoklaServers returns the Ookla servers the API reports for a location,
// nearest first. Non-zero lat/lon returns servers near that coordinate (a city
// search, like speedtest.net's "change server"); otherwise near the caller's IP.
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
	out := make([]ServerInfo, 0, len(servers))
	for _, s := range servers {
		out = append(out, ServerInfo{
			ID: s.ID, Sponsor: s.Sponsor, Name: s.Name,
			Country: s.Country, DistanceKM: s.Distance,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DistanceKM < out[j].DistanceKM })
	return out, nil
}

// GetOoklaServer fetches one Ookla server by numeric ID so the UI can pin (and
// confirm the name of) an exact server without a city search.
func GetOoklaServer(ctx context.Context, id string) (ServerInfo, error) {
	srv, err := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}).FetchServerByIDContext(ctx, id)
	if err != nil {
		return ServerInfo{}, err
	}
	return ServerInfo{ID: srv.ID, Sponsor: srv.Sponsor, Name: srv.Name, Country: srv.Country, DistanceKM: srv.Distance}, nil
}
