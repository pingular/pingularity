// Auto server selection used to centre on the first rung of a priority cascade
// that measured nothing: a searched city, else the exit router, else the
// Cloudflare PoP, else the public IP. That shape is wrong because Ookla's server
// list is fetched AROUND a coordinate and the lists around different coordinates
// are disjoint, not reordered - measured on a live connection, three candidate
// cities (one abroad, two domestic) returned lists with an EMPTY pairwise
// intersection.
// The cascade's chosen centre did not rank a pool; it DECIDED WHICH POOL
// EXISTED, and every server outside it was unreachable at any ranking. On that
// link the cascade centred a country away from the city whose server pinged a
// third of the winner's.
//
// So the centre is now measured. Each candidate city - the exit router's, the
// ISP geolocation's, and the one the server operator places us in - contributes
// its closest few servers to one pool; the pool is deduplicated and ping-raced,
// and the city whose server answers fastest becomes the centre. A searched city
// and a pinned server still override the whole race: they are statements of
// intent, not hypotheses to be measured (see RunReason / listCentre).
package speedtest

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/util"
)

// Origin is one candidate centre: a city we have some reason to believe fast
// servers are near. Kind names the evidence, Label the human place name, and
// Anchored says whether Lat/Lon are usable.
//
// An UNANCHORED origin is not a missing coordinate - it is the distinct origin
// "no centre of ours at all", which makes the Ookla API geolocate our source
// address itself and return the pool IT thinks we belong to. That pool is often
// the same as the ISP geolocation's, in which case the union dedupe collapses it
// for free; on CGNAT or a tunnelled link it is not, and it is the only origin
// that reflects the server operator's own opinion of where we are.
type Origin struct {
	Kind     string // stable identifier: "exit" | "isp" | "geo"
	Label    string // human place name for the log ("Oldtown, XX"); may be empty
	Lat, Lon float64
	Anchored bool // false = no coordinate; let the Ookla API centre on our source address
}

const (
	// cityPoolSize is how many of each city's closest servers enter the race.
	// Six per city, three cities, minus overlap: at most 18 RACERS, once per
	// run.
	//
	// That 18 bounds the race, not the run's probe traffic, and the difference
	// is worth stating because trimming this constant would not change it. Each
	// list fetch echoes EVERY server it returns before we see one of them
	// (speedtest-go server.go:286-311, two GETs apiece - HTTPPing adds one to
	// warm the connection), so the concurrent fetches burst ~2N per city first:
	// measured, 150 GETs peaking at 75 concurrent for three lists of 25, spread
	// over as many distinct hosts with nothing to throttle them. The race's own
	// pings are the larger share of the volume but the smaller share of the
	// burst, being capped at 18 lanes and paced 200ms apart.
	//
	// Within one metro Ookla often geolocates every server to the same
	// city-centre coordinate, so the tail of this cut is a distance tie - which
	// is exactly why the pool is raced on ping rather than trimmed further on
	// distance.
	cityPoolSize = 6

	// cityOriginDedupeKM folds two origins that name effectively the same
	// place. 25 km of fiber is well under half a millisecond of round trip, so
	// two centres that close return interchangeable pools and the second fetch
	// buys nothing. Without it the COMMON case - exit router and ISP
	// geolocation resolving to one metro - pays two identical list fetches and
	// enters that metro's servers twice into the union.
	cityOriginDedupeKM = 25

	// cityOriginMax bounds the concurrent list fetches. The enumeration the
	// daemon supplies has three kinds and dedupe usually leaves two, so this
	// never fires in practice; it is here because OriginsFn is caller-supplied
	// and an unbounded caller would turn one race into an unbounded fan-out of
	// requests at a third party.
	cityOriginMax = 4

	// cityPingTimeout bounds ONE candidate's probe set. The ping client is
	// &http.Client{} with no Timeout (it cannot have one - the same transport
	// serves the 15s-per-direction transfers, see newOoklaClient), so without
	// this the only bound on a server that accepted the connection and then
	// went quiet would be the whole race's budget, shared with every other
	// racer. A healthy probe set is 11 GETs paced 200ms apart - ~2.5s at 25ms
	// RTT, ~5.5s even at 300ms - so 8s clears it with room and still cuts a
	// stalled server off long before it can starve the race. Being cut off
	// costs the server precision, not its whole score: the samples it did
	// produce are kept (see racePing).
	cityPingTimeout = 8 * time.Second
)

// cityRaceBudget bounds one whole race - the concurrent list fetches plus the
// ping round. Without it a single list endpoint that accepts the connection and
// then goes silent would spend the run's whole budget on choosing a target
// instead of measuring one. Generous: when every fetch answers, the race is
// over in a few seconds and this changes nothing.
//
// A var, not a const, only so a test can shrink it: what happens when this
// expires is a branch worth pinning (see raceOrigins' select), and pinning it
// at 30s would mean a 30s test.
var cityRaceBudget = 30 * time.Second

// fetchOriginServers fetches the Ookla server list for one origin. A package var
// for the same reason ooklaDownload is one: it is the only seam through which a
// test can put pools in front of the race without a network.
var fetchOriginServers = func(ctx context.Context, o Origin) (ookla.Servers, error) {
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	if o.Anchored {
		uc.Location = newAnchoredLocation(o.Kind, o.Lat, o.Lon)
	}
	return newOoklaClient(uc).FetchServerListContext(ctx)
}

// newAnchoredLocation builds a UserConfig.Location under the same lock
// newOoklaClient uses, because ookla.NewLocation is not a constructor: it also
// writes the library's package-global `Locations` map, unsynchronized
// (speedtest/location.go). One origin at a time never noticed. Fetching a pool
// PER ORIGIN concurrently is the code that calls it from several goroutines,
// and unsynchronized that takes the process down with "fatal error: concurrent
// map writes" - not a data race the detector reports, a runtime abort.
//
// It is the second process-global in this library that has to be serialized
// (see ooklaClientMu's own comment for the first, http.DefaultClient.Transport),
// so the same mutex covers both rather than inventing a second one. The lock is
// released before newOoklaClient, which takes it itself - sync.Mutex is not
// reentrant.
func newAnchoredLocation(name string, lat, lon float64) *ookla.Location {
	ooklaClientMu.Lock()
	defer ooklaClientMu.Unlock()
	return ookla.NewLocation(name, lat, lon)
}

// racePing measures one candidate's latency onto s.Latency. A package var for
// the same reason ooklaDownload is one: PingTestContext is a method on the
// library's server type, so without this seam no test can put a latency in
// front of the race without opening a socket - and the whole point of the race
// is which latency wins.
//
// It does NOT keep the latency PingTestContext computes, for two reasons
// measured on a live link:
//
//  1. That figure is the MEAN of the probes (speedtest-go request.go), so one
//     stalled sample lands a tenth of itself on the score. Latency has a hard
//     floor and an unbounded tail - the floor is the physical path, which is
//     what actually differs between one metro and another, while the tail is
//     transient queueing on our own link, noise common to every candidate. The
//     minimum estimates the floor, so it is what gets compared. Under load the
//     difference is the whole decision: in one measured window every
//     candidate's mean inflated 3-20x and the race lost all discrimination;
//     the minima stayed separated.
//
//  2. PingTestContext ASSIGNS NOTHING when it returns an error - including the
//     context error it returns after collecting nine clean samples and then
//     hitting a deadline. s.Latency would keep whatever it held, and the race
//     sorts it as a measurement, so a good server merely cut off short would
//     lose to every mediocre one that finished. The callback has already fired
//     for each successful probe by then, so collecting through it recovers
//     exactly the samples the library was about to discard.
//
// The score REPLACES whatever Latency held, and that is load-bearing rather
// than incidental: FetchServerListContext one-shot-pings every server in the
// list it returns and writes the result onto Latency before we ever see it
// (speedtest-go server.go:299-306, or PingTimeout=-1 when that echo failed).
// Leaving a silent server's Latency alone therefore did not mean "unmeasured" -
// it meant "scored on a single unpaced echo taken outside the race". Measured:
// a server that answered the fetch echo and then stalled through the whole
// cityPingTimeout won the race against a healthy server scored on the minimum
// of ten paced probes. Only this race's own samples decide this race; nothing
// measured here scores 0 and sorts last, which is what the winner loop and the
// unanswered-race fallback in raceCities both read.
var racePing = func(ctx context.Context, s *ookla.Server) {
	ctx, cancel := context.WithTimeout(ctx, cityPingTimeout)
	defer cancel()
	var best time.Duration
	// The error is deliberately ignored: a probe set that ended early still
	// measured whatever it measured, and that is a real reading of this server.
	_ = s.PingTestContext(ctx, keepFastestPing(&best))
	s.Latency = best
}

// keepFastestPing builds a PingTestContext callback that records the fastest
// sample it sees. The library hands back a MEAN over its ten samples, which has
// no resistance to a single stalled handshake - one ~225ms sample among nine
// ~4.6ms ones reports 30ms. The floor is the honest answer to "how fast can
// this server be", which is the only question a server CHOICE asks, so both
// places that choose - this race and the run's own measurement - take it from
// here rather than each rolling their own and drifting apart.
func keepFastestPing(best *time.Duration) func(time.Duration) {
	return func(l time.Duration) {
		// The library invokes this callback ONLY after a probe succeeded (a failed
		// request `continue`s before reaching it, request.go), so every value here
		// is a real measurement - including 0, which is what a sub-millisecond
		// round trip reads as on a platform whose clock is coarser than the
		// latency being measured. Windows is that platform.
		//
		// Rejecting `l <= 0` therefore threw away every FAST sample and kept the
		// slow ones, leaving the score equal to the worst probe - the exact
		// inversion this function exists to prevent. It only showed up in CI,
		// because loopback on a fast macOS/Linux runner still measures above zero.
		//
		// 0 remains reserved for "nothing measured" everywhere downstream
		// (validMS, msIfPositive, the race's own winner loop), so a real zero is
		// clamped to the smallest positive duration instead of admitted as 0:
		// indistinguishable as a latency, distinguishable as evidence.
		if l < 0 {
			return
		}
		if l == 0 {
			l = time.Nanosecond
		}
		if *best == 0 || l < *best {
			*best = l
		}
	}
}

// pickOrigins collapses the caller's candidates into the set actually worth
// fetching: drops the ones with no usable position, folds near-duplicates, and
// caps the fan-out. Order is preserved, so the caller's most-specific candidate
// survives a fold - order decides what gets FETCHED and who the silent-race
// fallback centres on (see raceCities), never who wins.
func pickOrigins(in []Origin) []Origin {
	out := make([]Origin, 0, len(in))
	for _, o := range in {
		// 0,0 is the "no fix" value every provider here uses (see publicIPGeo and
		// ExitInfo.Lat). Taken literally it is a point in the Gulf of Guinea, and
		// Ookla will happily return a pool for it - servers in Ghana would then
		// race a user in Quebec and lose, wasting a fetch and six of the pool's
		// lanes. The rest of the check is defence in depth against a caller
		// handing us a half-pair or a nonfinite value: the coordinate crosses a
		// process boundary as JSON, a NaN would sort as neither near nor far in
		// the dedupe and would not survive the encode, and an out-of-range pair
		// is a pool nobody lives in.
		if o.Anchored && !usableCoord(o.Lat, o.Lon) {
			continue
		}
		dup := false
		for _, k := range out {
			if k.Anchored != o.Anchored {
				continue
			}
			// Two unanchored origins are the same request; two anchored ones within
			// the dedupe radius return interchangeable pools.
			if !o.Anchored || kmBetween(k.Lat, k.Lon, o.Lat, o.Lon) <= cityOriginDedupeKM {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, o)
		if len(out) == cityOriginMax {
			break
		}
	}
	return out
}

// usableCoord reports whether a coordinate can centre a server search: a real
// position, finite, and on the globe. Mirrors netinfo.validCoord, which screens
// the same thing at the provider end; this is the last gate before a coordinate
// reaches the Ookla API.
func usableCoord(lat, lon float64) bool {
	if lat == 0 && lon == 0 {
		return false
	}
	if math.IsNaN(lat) || math.IsNaN(lon) || math.IsInf(lat, 0) || math.IsInf(lon, 0) {
		return false
	}
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// anyAnchored reports whether the field holds a coordinate a fetch could be
// centred on - i.e. whether the race has anything to decide at all.
func anyAnchored(origins []Origin) bool {
	for _, o := range origins {
		if o.Anchored {
			return true
		}
	}
	return false
}

// unionCityCandidates merges the per-city candidate lists into the one set that
// gets pinged, and reports which origin surfaced each survivor.
//
// The merge is round-robin BY RANK - every city's closest candidate, then every
// city's second-closest, and so on. Taking them city by city instead would put
// the whole of the first city's pool ahead of the second's, and since that
// first city is whatever the caller listed first, the priority cascade would be
// back, wearing a race as a disguise. Dedupe is by server ID, so a server two
// cities both surfaced is pinged once and credited to the city that reached it
// first - which, by round-robin, is the one it sits closer to the front of.
func unionCityCandidates(pools []ookla.Servers) (ookla.Servers, map[*ookla.Server]int) {
	deepest := 0
	for _, p := range pools {
		if len(p) > deepest {
			deepest = len(p)
		}
	}
	out := make(ookla.Servers, 0, cityOriginMax*cityPoolSize)
	owner := make(map[*ookla.Server]int, cityOriginMax*cityPoolSize)
	seen := make(map[string]bool, cityOriginMax*cityPoolSize)
	for lane := 0; lane < deepest; lane++ {
		for i, p := range pools {
			if lane >= len(p) {
				continue
			}
			s := p[lane]
			if s == nil || seen[s.ID] {
				continue
			}
			seen[s.ID] = true
			out = append(out, s)
			owner[s] = i
		}
	}
	return out, owner
}

// raceCities picks the city auto-select centres on, by measurement: each
// origin's pool of closest servers is fetched concurrently, the pools are
// deduplicated into one union, the union is ping-raced, and the origin whose
// pool surfaced the fastest answer is the winner. ok is false when there is
// nothing to race (no origins, or no pool could be fetched) - the caller then
// falls back to an uncentred fetch, which is the pre-race behaviour.
//
// A race nobody answered (pings blocked network-wide, or the link died
// mid-evaluation) is a failed measurement, not a verdict: it falls back to the
// first anchored origin, which by the caller's ordering is the exit router -
// the same centre the old cascade chose - so a ping-hostile network degrades to
// exactly the behaviour it had before the race existed.
//
// It runs on EVERY auto run, deliberately, and is not trigger-gated the way
// best-of is (see bestOfReasons). The costs are different in kind: best-of
// triples a run's DATA, hundreds of megabytes per extra server, which is what a
// reconnect run must not spend; the race spends seconds and requests - measured
// ~4.5s and on the order of 150-250 GETs, most of them the library's own echo
// of each fetched list (see cityPoolSize) - against a run already moving
// hundreds of megabytes. That is a real cost and it is bounded by the run's own
// budget, not a negligible one; what makes it acceptable is that it buys the
// centre the whole feature turns on, every time, on current evidence.
//
// Caching the winner instead was tried in an earlier design, and the TTL, the
// probation window and the dispute counter that grew around it were all patches
// for the same thing: a stale centre is exactly the failure this feature exists
// to remove. So it is measured fresh rather than remembered. If the request
// volume ever needs cutting, gate it by trigger (scheduled and manual, as
// best-of is) rather than by staleness - note that the adaptive scheduler can
// clamp the SCHEDULED cadence down to one minute, so a trigger gate alone does
// not bound it either.
func (o *Ookla) raceCities(ctx context.Context) (Origin, bool) {
	return o.raceOrigins(ctx, o.candidateOrigins())
}

// candidateOrigins is the ONE place a run reads OriginsFn. A run reads it once
// and feeds the same slice to the deadline sizing and to the race, because
// OriginsFn is a LIVE read of the netinfo snapshot - republished by the hourly
// netinfo loop and by the reconnect refresh - so reading it twice could size
// the deadline for a field with nothing anchored (which costs nothing) and then
// race a field that gained a coordinate in between, taking the race back out of
// the measurement's budget.
func (o *Ookla) candidateOrigins() []Origin {
	if o.OriginsFn == nil {
		return nil
	}
	return pickOrigins(o.OriginsFn())
}

// raceOrigins is raceCities over an already-picked field (see candidateOrigins).
func (o *Ookla) raceOrigins(ctx context.Context, origins []Origin) (Origin, bool) {
	if len(origins) == 0 {
		return Origin{}, false
	}
	// Nothing anchored means the race has nothing to decide. pickOrigins folds
	// every unanchored origin into ONE - two of them are literally the same
	// request - and the caller acts only on an ANCHORED winner, so all three
	// outcomes (winner, silence, dead fetch) leave the fetch uncentred and the
	// run then re-fetches the identical uncentred list the race just fetched.
	// Measured on a live link: ~4.5s and ~107 requests to decide something
	// already decided. Deliberately NOT "fewer than two origins": a single
	// ANCHORED origin still has an outcome to decide, because a failed pool
	// fetch there must leave the run uncentred rather than centre on it.
	if !anyAnchored(origins) {
		stats.Inc("speed.cityrace_unanchored")
		return origins[0], true
	}
	runCtx := ctx // kept unshadowed so the select below can tell the two deaths apart
	ctx, cancel := context.WithTimeout(ctx, cityRaceBudget)
	defer cancel()

	// One pool per origin, fetched concurrently: they are independent
	// round-trips, so the race is bounded by the slowest single fetch rather
	// than their sum. Each goroutine writes only its own index, so no lock is
	// needed. Per-pool trimming happens here, on a sorted copy: Distance is
	// measured from THIS origin's coordinate, so "closest" only means something
	// inside one pool.
	pools := make([]ookla.Servers, len(origins))
	var wg sync.WaitGroup
	for i, org := range origins {
		wg.Add(1)
		go func(i int, org Origin) {
			defer wg.Done()
			srv, err := fetchOriginServers(ctx, org)
			if err != nil || len(srv) == 0 {
				return // one dead origin must not sink the race; the others still ran
			}
			sorted := append(ookla.Servers(nil), srv...)
			sort.Slice(sorted, func(x, y int) bool { return sorted[x].Distance < sorted[y].Distance })
			if len(sorted) > cityPoolSize {
				sorted = sorted[:cityPoolSize]
			}
			pools[i] = sorted
		}(i, org)
	}
	// Abandoned, not awaited, when the run is cancelled: the library echoes the
	// WHOLE list it fetched on a context of its OWN (speedtest-go server.go:288
	// builds it from context.Background with a 4s deadline), so these goroutines
	// outlive our cancellation by up to that long. Waiting for them held the
	// scheduler's single-flight flag open past an abort, so every competing run
	// got ErrBusy meanwhile - the same failure runTransfer already abandons its
	// transfers to avoid.
	//
	// The early return is the safety argument, not tidiness: the abandoned
	// goroutines go on writing pools, so nothing after it may read pools or a
	// union built from it.
	//
	// WHICH context died decides the answer. The caller's cancellation means the
	// run is over and there is nothing left to centre. The race's OWN budget
	// expiring is a failed measurement on a still-live run - precisely what
	// cityRaceBudget exists to bound, and the run has its whole measurement
	// budget left - so the promise above applies and it degrades to the first
	// anchored origin instead of silently handing the run the API's guess at our
	// address. The ping round already behaves that way when the same budget
	// expires on its side; without this the two adjacent expiries disagreed on
	// the same cause.
	fetched := make(chan struct{})
	go func() { wg.Wait(); close(fetched) }()
	select {
	case <-fetched:
	case <-ctx.Done():
	}
	// Classify AFTER the select, never inside it. A ctx-aware fetch returns
	// BECAUSE the deadline fired, so `fetched` and `ctx.Done()` become ready at
	// the same instant and select picks between them at random - which decided,
	// per run, whether the same timeout took the fallback below or fell through
	// to an empty union and left the run uncentred. Asking the contexts
	// afterwards makes the outcome depend on what actually happened rather than
	// on which case the runtime drew.
	if stop, res, ok := o.raceHalted(runCtx, ctx, origins, 0); stop {
		return res, ok
	}

	union, owner := unionCityCandidates(pools)
	if len(union) == 0 {
		return Origin{}, false
	}

	var pg sync.WaitGroup
	for _, s := range union {
		pg.Add(1)
		go func(s *ookla.Server) {
			defer pg.Done()
			racePing(ctx, s)
		}(s)
	}
	// Bounded the same way the fetch round is, and for the same two reasons: a
	// ping implementation that never returns would otherwise hold the whole
	// race open past its own budget, and an aborted RUN must not come back
	// claiming a centre - it used to reach the fallback below and hand a dead
	// run an origin, after which RunReason wrote the library's global location
	// for a fetch that could only fail.
	pinged := make(chan struct{})
	go func() { pg.Wait(); close(pinged) }()
	select {
	case <-pinged:
	case <-ctx.Done():
	}
	if stop, res, ok := o.raceHalted(runCtx, ctx, origins, len(union)); stop {
		return res, ok
	}

	var win *ookla.Server
	for _, s := range union {
		if s.Latency > 0 && (win == nil || s.Latency < win.Latency) {
			win = s
		}
	}
	if win == nil {
		return o.silentRace(origins, len(union))
	}
	winOrigin := origins[owner[win]]
	stats.Inc("speed.cityrace_decided")
	o.logf("auto city race",
		"origins", len(origins), "racers", len(union),
		"winner_city", winOrigin.Kind, "winner_label", winOrigin.Label,
		"server", serverLabel(win), "server_id", win.ID,
		"ping_ms", util.Round2(util.DurMS(win.Latency)))
	return winOrigin, true
}

// raceHalted asks whether the race must stop here, and with what answer. It is
// the single place the two contexts are told apart, so both rounds of the race
// answer the same cause the same way.
//
//	the CALLER's context died -> the run is over; there is nothing to centre.
//	the race's OWN budget died -> a failed measurement on a live run, which is
//	                              what that budget exists to bound, so degrade
//	                              to the first anchored origin.
//	neither -> stop=false; carry on.
//
// On either stopping path the caller must not touch pools, the union, or any
// server's latency: the goroutines abandoned by the deadline are still writing
// them. silentRace reads only origins, which is what makes it safe here.
func (o *Ookla) raceHalted(runCtx, ctx context.Context, origins []Origin, racers int) (stop bool, res Origin, ok bool) {
	if runCtx.Err() != nil {
		return true, Origin{}, false
	}
	if ctx.Err() != nil {
		r, k := o.silentRace(origins, racers)
		return true, r, k
	}
	return false, Origin{}, false
}

// silentRace is the failed-measurement fallback: nobody answered, or the race
// spent its own budget before anyone could. Neither is a verdict, so it centres
// on the first anchored origin - by the caller's ordering the exit router, the
// centre the pre-race cascade chose.
//
// It reads only origins, never pools or a union built from them, which is what
// makes it safe to call while abandoned pool fetches are still writing (see the
// select above). racers is 0 on that path because the union genuinely does not
// exist yet, and that is information rather than a placeholder: it separates
// "the fetches never finished" from "N servers raced and none answered".
func (o *Ookla) silentRace(origins []Origin, racers int) (Origin, bool) {
	stats.Inc("speed.cityrace_silent")
	org, ok := FirstAnchoredOrigin(origins)
	if !ok {
		return Origin{}, false
	}
	o.logf("auto city race unanswered; centring on the first candidate",
		"origins", len(origins), "racers", racers, "centre", org.Kind, "label", org.Label)
	return org, true
}

// FirstAnchoredOrigin returns the first candidate carrying a usable coordinate
// - the first place in the caller's enumeration a server list can actually be
// centred on. ok=false when nothing is anchored, which is not a failure: it is
// the third candidate itself, "no centre of ours at all", which an uncentred
// fetch produces by letting the Ookla API geolocate our source address.
//
// Two callers share it so they cannot drift. The race uses it when nobody
// answered (silentRace). The settings server-BROWSING list uses it because it
// cannot race at all - it runs whenever the settings pane opens, and one race
// is a list fetch per city plus a round of pings at third parties. Sharing is
// what stops the picker centring on a place the race could never choose: it
// used to fall through to the Cloudflare PoP, which is not a candidate and
// cannot become one - a PoP is where Cloudflare's building is, not where the
// subscriber is - and those pools are disjoint from the candidate cities'
// (see this file's header), so the picker could not show the server auto was
// actually testing from at any scroll position.
//
// It screens the coordinate because the browse caller passes a RAW enumeration;
// on the race's already-picked field the screen is a no-op.
func FirstAnchoredOrigin(origins []Origin) (Origin, bool) {
	for _, o := range origins {
		if o.Anchored && usableCoord(o.Lat, o.Lon) {
			return o, true
		}
	}
	return Origin{}, false
}

// kmBetween is the great-circle distance between two coordinates, used only to
// decide whether two origins name the same place. Haversine on a spherical
// Earth is good to a fraction of a percent, and the thing it feeds is a 25 km
// bucket, so the ellipsoid correction would be noise.
func kmBetween(lat1, lon1, lat2, lon2 float64) float64 {
	const earthKM = 6371.0
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(lat2-lat1), rad(lon2-lon1)
	h := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(rad(lat1))*math.Cos(rad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthKM * math.Asin(math.Sqrt(math.Min(1, h)))
}
