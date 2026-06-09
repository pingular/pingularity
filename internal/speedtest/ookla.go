package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
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

	// AutoLocFn, if set, supplies a coordinate to centre auto-select on (e.g. a
	// searched city). ok=false means use the caller's own IP location.
	AutoLocFn func() (lat, lon float64, ok bool)

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
	client := ookla.New(ookla.WithDoer(&http.Client{}), ookla.WithUserConfig(uc))
	if _, ok := http.DefaultClient.Transport.(*ookla.Speedtest); ok {
		http.DefaultClient.Transport = nil
	}
	return client
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

	// How many servers this run measures. Best-of-N is opt-in AND trigger-gated:
	// a reconnect run must stay cheap and quick.
	want := 1
	if o.BestOfFn != nil && bestOfReasons[reason] && o.BestOfFn() {
		want = bestOfServers
	}

	// Deadlines (see runBudget): without them a single stalled request wedges the
	// run - and the single-flight guard - forever. perServer bounds each target so
	// one bad server can't eat the others' turns; total bounds the whole run.
	perServer, total := runBudget(retries, want)
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	// Auto with a configured location (a searched city) fetches servers near that
	// coordinate; otherwise near the caller's own IP. Fresh client per run: the
	// library's client.Context accumulates per-chunk DataChunk snapshots for the
	// client's life, so reusing one leaks a run's memory every test.
	// One UserConfig carries the parallel-connection count (0 -> the library's
	// NumCPU default) and, for auto-select with a searched city, the location.
	id := o.serverID()
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	if o.ConnectionsFn != nil {
		uc.MaxConnections = o.ConnectionsFn()
	}
	// The location centres the fetched list, which is where best-of-N draws its
	// companion servers from. A pinned server is resolved by ID and doesn't care
	// about the centring itself, so what matters here is only where its COMPANIONS
	// come from:
	//   - nothing pinned: the auto location (a searched city, else the exit router).
	//   - pinned + best-of: the PINNED server, so the extras are its neighbours.
	//     Centring those on the exit instead made a pin nearly pointless - the
	//     winner is chosen on throughput alone (see bestIndex), so exit-local
	//     servers would out-run the pin on most rounds and get stored in its place,
	//     which is just Auto with one extra racer.
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
	if lat, lon, label, ok := listCentre(id, want, pinned, o.AutoLocFn); ok {
		uc.Location = ookla.NewLocation(label, lat, lon)
	}
	client := newOoklaClient(uc)

	servers, err := client.FetchServerListContext(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("fetch server list: %w", err)
	}

	targets, err := o.pickServers(ctx, client, servers, id, want, pinned)
	if err != nil {
		return Result{}, err
	}

	// Sequentially, always: two speedtests at once would saturate the link and
	// each would measure the other's traffic as congestion.
	var results []Result
	var firstErr error
	for _, srv := range targets {
		// Report the server now so the UI can show it during the run.
		if o.OnServer != nil {
			o.OnServer(serverLabel(srv))
		}
		// Each server gets its own slice of the budget. Sharing one deadline let a
		// single accept-then-stall server (the ping and loss probes have no request
		// timeout of their own) burn the whole window, so the other targets were
		// never contacted - the exact failure best-of-N exists to survive.
		sctx, scancel := context.WithTimeout(ctx, perServer)
		res, err := o.measure(sctx, srv, dir, retries)
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
				"bufferbloat_ms", f64v(bufferbloatMS(res)))
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		if firstErr != nil {
			return Result{}, firstErr
		}
		return Result{}, fmt.Errorf("no speedtest servers available")
	}

	win := bestIndex(results)
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
			"discarded", strings.Join(discarded, " | "))
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
//	nothing pinned    -> the auto location (searched city, else the exit router)
//
// A pin whose coordinate Ookla did not supply falls back to the auto location,
// which still beats the public-IP guess the library would otherwise make.
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

// measure runs one full measurement against an already-chosen server.
func (o *Ookla) measure(ctx context.Context, srv *ookla.Server, dir string, retries int) (Result, error) {
	var err error
	if err := srv.PingTestContext(ctx, nil); err != nil {
		return Result{}, fmt.Errorf("ping: %w", err)
	}
	// Idle baseline for latency-under-load: same method/target as the loaded
	// samplers below (NOT the Ookla ping above), taken while the link is quiet, so
	// the idle-vs-loaded delta isolates the load effect.
	idleMS := measureIdleLatency(ctx)
	anyErr := func(error) bool { return true } // a failed transfer is worth retrying

	// Each retry attempt must start from a clean DataManager: the library's
	// Register*Handler only appends while the handler stack has room, so a retry
	// on the same client would otherwise drive the previous attempt's cancelled
	// closures - transferring nothing and reporting a phantom 0 instead of -1
	// (N/A). Reset() rebuilds the direction state so every attempt re-registers.
	// It also zeroes the byte totals, so each phase's volume is captured right
	// after that phase, before the next phase's Reset can clear it.
	var loadedDown, loadedUp *loadStat
	var downBytes, upBytes int64
	if dir != "up" {
		err = withRetryPred(ctx, retries, anyErr, func() error {
			srv.Context.Reset()
			stop := startLoadSampler(ctx)
			e := srv.DownloadTestContext(ctx)
			loadedDown = stop()
			if e == nil && ctx.Err() != nil { // the lib can return nil on cancellation
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
		downBytes = srv.Context.GetTotalDownload()
	}
	if dir != "down" {
		err = withRetryPred(ctx, retries, anyErr, func() error {
			srv.Context.Reset()
			stop := startLoadSampler(ctx)
			e := srv.UploadTestContext(ctx)
			loadedUp = stop()
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
		upBytes = srv.Context.GetTotalUpload()
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
		JitterMS:        f64p(util.DurMS(srv.Jitter)),
		Server:          fmt.Sprintf("%s, %s", srv.Sponsor, srv.Name),
		ServerID:        srv.ID,
		PacketLoss:      loss,
		DownloadBytes:   downBytes,
		UploadBytes:     upBytes,
		IdleMS:          idleMS,
		LoadedDownMS:    loadedDown.medPtr(),
		LoadedUpMS:      loadedUp.medPtr(),
		LoadedDownMaxMS: loadedDown.maxPtr(),
		LoadedUpMaxMS:   loadedUp.maxPtr(),
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

// bestResult picks the winner of a best-of-N run, by the user's stated order:
// total throughput first, then latency, then jitter, then bufferbloat. Every
// tie-break is a strict improvement test, so an exact tie keeps the earlier
// result - and the earlier result is the higher-ranked server (the pinned one,
// or the lowest ping), which is the right thing to fall back on.
//
// Later keys are near-impossible to reach in practice: two separate runs would
// have to agree to the full float precision. They exist so the choice is
// deterministic rather than accidental.
func bestResult(rs []Result) Result { return rs[bestIndex(rs)] }

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
func bestIndex(rs []Result) int {
	win := 0
	for i, r := range rs[1:] {
		if betterResult(r, rs[win]) {
			win = i + 1
		}
	}
	return win
}

// betterResult reports whether a beats b on the ranking. Unmeasured values (nil
// jitter, no bufferbloat samples) lose their tie-break rather than winning it by
// being absent - a run that could not measure jitter has not proved it is
// steadier than one that did.
func betterResult(a, b Result) bool {
	// 1. Throughput: download + upload. A skipped direction is 0 for both runs,
	//    so a down-only or up-only configuration still compares fairly.
	at, bt := a.DownloadMbps+a.UploadMbps, b.DownloadMbps+b.UploadMbps
	if at != bt {
		return at > bt
	}
	// 2. Latency (lower wins). A run with no ping figure can't win here.
	if a.PingMS != b.PingMS {
		return validMS(a.PingMS) && (!validMS(b.PingMS) || a.PingMS < b.PingMS)
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
func bufferbloatMS(r Result) *float64 {
	if r.IdleMS == nil {
		return nil
	}
	worst := math.NaN()
	for _, loaded := range []*float64{r.LoadedDownMS, r.LoadedUpMS} {
		if loaded == nil {
			continue
		}
		if d := *loaded - *r.IdleMS; math.IsNaN(worst) || d > worst {
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
		uc.Location = ookla.NewLocation("custom", lat, lon)
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
