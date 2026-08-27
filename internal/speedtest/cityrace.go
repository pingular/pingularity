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
// ISP geolocation's, the cities the user has starred a server in, the city
// that won the last race, and the one the server operator places us in -
// contributes its nearest few servers,
// seeded as a run's own race is (see cityPoolSize), to one pool; the pool is deduplicated and ping-raced, and the city whose server
// answers fastest becomes the centre. A searched city and a pinned server still
// override the whole race: they are statements of intent, not hypotheses to be
// measured (see RunReason / listCentre).
package speedtest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
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
	Kind     string // stable identifier: "exit" | "isp" | "saved" | "recent" | "geo"
	Label    string // human place name for the log ("Oldtown, XX"); may be empty
	Lat, Lon float64
	Anchored bool // false = no coordinate; let the Ookla API centre on our source address
}

const (
	// cityPoolSize is how many of each city's closest servers enter the race.
	// Six per city, up to cityOriginMax anchored cities plus the unanchored one,
	// minus overlap: at most 36 RACERS, once per run - 18 for the usual field
	// of exit, ISP and geo, and six more for every distinct city the user has
	// starred a server in or that won the last race (usually folded into one
	// of the others).
	//
	// That bounds the race, not the run's probe traffic, and the difference
	// is worth stating because trimming this constant would not change it. Each
	// list fetch echoes EVERY server it returns before we see one of them
	// (speedtest-go server.go:286-311, two GETs apiece - HTTPPing adds one to
	// warm the connection), so the concurrent fetches burst ~2N per city first:
	// measured, 150 GETs peaking at 75 concurrent for three lists of 25, spread
	// over as many distinct hosts with nothing to throttle them - ~50 more per
	// extra starred city. The race's own pings are the larger share of the
	// volume but the smaller share of the burst, being capped at the lanes
	// above and paced 200ms apart.
	//
	// Within one metro Ookla often geolocates every server to the same
	// city-centre coordinate - measured, all 25 Montreal servers at "1 km" - so
	// "the six nearest" is a tie, and an arbitrary six of it can leave out the
	// fastest server, or the user's own ISP's. The cut therefore breaks a
	// distance tie by the fetch's own echo (the library pings every listed
	// server once during the fetch, so it costs nothing), prefers one server
	// per sponsor, and seats the ISP's server - the run's own trim, trimToCap,
	// over the run's own distance window. The race then decides on its own
	// pings.
	cityPoolSize = 6

	// cityPoolISPMax is how many lanes the user's own ISP may hold in one
	// city's pool of cityPoolSize: enough that its on-net box is always in the
	// race, not so many that one provider fills a city's six.
	cityPoolISPMax = 2

	// cityOriginDedupeKM folds two origins that name effectively the same
	// place. 25 km of fiber is well under half a millisecond of round trip, so
	// two centres that close return interchangeable pools and the second fetch
	// buys nothing. Without it the COMMON case - exit router and ISP
	// geolocation resolving to one metro - pays two identical list fetches and
	// enters that metro's servers twice into the union.
	cityOriginDedupeKM = 25

	// cityOriginMax bounds the concurrent ANCHORED list fetches; the single
	// unanchored origin (Ookla's own placement of our address) rides beside
	// them, never displaced by it, because it is the one pool that reflects the
	// server operator's opinion of where we are. The enumeration the daemon
	// supplies is two anchored connection-derived kinds, one "saved" origin
	// per city the user has starred a server in, and the last race's winning
	// city ("recent", which dedupe usually folds into one of those); dedupe
	// usually folds the lot to one or two, and five leaves room for stars in
	// three cities beside the derived ones - the recent winner comes last in
	// main.autoOrigins, so it is the anchored origin the cap drops when stars
	// fill it, never a star. It is here because OriginsFn is caller-supplied and an
	// unbounded caller would turn one race into an unbounded fan-out of
	// requests at a third party.
	cityOriginMax = 5

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
	servers, err := newOoklaClient(uc).FetchServerListContext(ctx)
	// The same endpoint rewrite fetchServerList applies, BEFORE the race
	// pings: the library's ping GETs latency.txt off s.URL, so a migrated
	// server pinged on its legacy URL pays a 307 to the current host on every
	// sample - a floor the run's own ranking (which pings the rewritten URL)
	// never measures. The run reuses these floors (raceResult.Raced), so they
	// must be taken where the run would take them.
	return currentEndpoints(servers), err
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
// caps the anchored fan-out. Order is preserved, so the caller's most-specific
// candidate survives a fold - order decides what gets FETCHED and who the
// silent-race fallback centres on (see raceCities), never who wins.
func pickOrigins(in []Origin) []Origin {
	out := make([]Origin, 0, len(in))
	anchored := 0
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
		if o.Anchored {
			if anchored == cityOriginMax {
				continue // over the cap: a later anchored origin is dropped, the unanchored one never is
			}
			anchored++
		}
		out = append(out, o)
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
// origin's pool of nearest servers (seeded as cityPoolSize describes) is fetched concurrently, the pools are
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
//
// test seam: production enters via candidateOrigins + raceOrigins separately
// (see the call sites); this composed wrapper is intentionally test-only.
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

// RaceVerdict is what one run's city race decided and why: the record the
// speed row carries (store.SpeedSample's race_* columns) so a surprising
// centre is explainable from the DB alone - the argument that created
// speed_servers (see SelectionReport), one stage earlier. The 2026-08-26
// "why Toronto tonight?" was answered from memory notes because the verdict
// lived only in a Debug line. Observability, with one exception: WinnerLat/Lon
// is read back by store.LastDecidedRace so the next run can enter the winning
// city as its "recent" origin (main's autoOrigins); nothing else feeds a
// selection.
type RaceVerdict struct {
	Outcome string // one of the Race* constants below
	// Origins is the field in one line, per origin in the caller's order:
	// "exit:Montréal, QC(8.4ms) | isp:Toronto(15.1ms) | geo(-)" - kind, label
	// when there is one, and the fastest answer credited to that origin's
	// pool, or "-" when none of its racers answered. " | " separates origins
	// because labels carry commas. Empty unless the pings ran.
	Origins     string
	WinnerKind  string // Origin.Kind the run centred on; empty when nothing was chosen
	WinnerLabel string // that origin's place name; may be empty
	// WinnerLat/Lon is the winning origin's coordinate, kept so the city can
	// be entered in the NEXT run's race as the "recent" origin (main's
	// autoOrigins) - the one candidate that survives every lookup going dark
	// with nothing starred. Set only when the race DECIDED on an anchored
	// origin: zero for an unanchored winner, and zero on a silent race even
	// though WinnerKind/Label name the stand-in, because a stand-in never won
	// on ping and must not be re-entered as "recent".
	WinnerLat, WinnerLon float64
	WinnerMS             *float64 // the fastest racer's floor; nil unless Outcome is RaceDecided
	Racers               int      // servers pinged; 0 when the pings never ran
}

// Race outcomes (RaceVerdict.Outcome). Persisted, so the literals are a
// contract: the runs table renders them and a reader may filter on them.
const (
	RaceDecided     = "decided"      // racers answered: the fastest one's origin is the centre
	RaceSilent      = "silent"       // nobody answered, or the race's budget expired: the first anchored origin stood in
	RaceUnanchored  = "unanchored"   // no origin carried a coordinate: nothing to race, the API's placement stands
	RaceFailed      = "failed"       // no origin's list could be fetched: the run fetched uncentred
	RaceSkipped     = "skipped"      // no origins at all (OriginsFn unset or empty)
	RaceBypassedPin = "bypassed_pin" // a pinned server: the pin is the target, or its own position the centre
)

// raceResult is everything one race produced. Origin/OK are the verdict the
// run acts on (see raceOrigins); Verdict is the record; Field and Raced are
// the work the race already did, handed on so the run does not do it twice.
type raceResult struct {
	Origin  Origin
	OK      bool
	Verdict RaceVerdict
	// Field is the winning origin's WHOLE fetched list, distance measured from
	// that origin - the very list the run used to fetch again around the same
	// coordinate (~25 echo GETs to re-learn it, plus the API's nondeterminism).
	// nil unless the race decided.
	Field ookla.Servers
	// Raced is the race's own ping per racer, the floor of its samples (see
	// racePing); 0 for a racer that never answered. The run ranks on these
	// instead of pinging the same servers again seconds later. nil unless the
	// race decided.
	Raced map[string]time.Duration
}

// raceOrigins is raceCities over an already-picked field (see candidateOrigins),
// reduced to the verdict the run acts on; runRace is the whole result.
func (o *Ookla) raceOrigins(ctx context.Context, origins []Origin) (Origin, bool) {
	r := o.runRace(ctx, origins)
	return r.Origin, r.OK
}

func (o *Ookla) runRace(ctx context.Context, origins []Origin) raceResult {
	if len(origins) == 0 {
		return raceResult{Verdict: RaceVerdict{Outcome: RaceSkipped}}
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
		o.logf("auto city race skipped: no anchored origin")
		return raceResult{Origin: origins[0], OK: true, Verdict: RaceVerdict{
			Outcome: RaceUnanchored, WinnerKind: origins[0].Kind, WinnerLabel: origins[0].Label}}
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
	pools, full, fetched := fetchOriginPools(ctx, origins, o.ispName())
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
		return haltedResult(runCtx, res, ok, 0)
	}

	union, owner := unionCityCandidates(pools)
	if len(union) == 0 {
		return raceResult{Verdict: RaceVerdict{Outcome: RaceFailed}}
	}

	// Bounded the same way the fetch round is, and for the same two reasons: a
	// ping implementation that never returns would otherwise hold the whole
	// race open past its own budget, and an aborted RUN must not come back
	// claiming a centre - it used to reach the fallback below and hand a dead
	// run an origin, after which RunReason wrote the library's global location
	// for a fetch that could only fail.
	pinged := pingRacers(ctx, union)
	select {
	case <-pinged:
	case <-ctx.Done():
	}
	if stop, res, ok := o.raceHalted(runCtx, ctx, origins, len(union)); stop {
		return haltedResult(runCtx, res, ok, len(union))
	}

	win := fastestRacer(union)
	if win == nil {
		org, ok := o.silentRace(origins, len(union))
		return raceResult{Origin: org, OK: ok, Verdict: RaceVerdict{Outcome: RaceSilent,
			Origins:    formatRaceOrigins(origins, union, owner),
			WinnerKind: org.Kind, WinnerLabel: org.Label, Racers: len(union)}}
	}
	winOrigin := origins[owner[win]]
	ms := util.Round2(util.DurMS(win.Latency))
	raced := make(map[string]time.Duration, len(union))
	for _, s := range union {
		raced[s.ID] = s.Latency
	}
	stats.Inc("speed.cityrace_decided")
	o.logf("auto city race",
		"origins", len(origins), "racers", len(union),
		"winner_city", winOrigin.Kind, "winner_label", winOrigin.Label,
		"server", serverLabel(win), "server_id", win.ID,
		"ping_ms", ms)
	return raceResult{
		Origin: winOrigin, OK: true,
		Verdict: RaceVerdict{Outcome: RaceDecided, Origins: formatRaceOrigins(origins, union, owner),
			WinnerKind: winOrigin.Kind, WinnerLabel: winOrigin.Label,
			WinnerLat: anchoredLat(winOrigin), WinnerLon: anchoredLon(winOrigin),
			WinnerMS: &ms, Racers: len(union)},
		Field: full[owner[win]], Raced: raced,
	}
}

// anchoredLat / anchoredLon are the origin's coordinate when it has one, so an
// unanchored winner (the API's own placement, coordinate unknown) records 0.
func anchoredLat(o Origin) float64 {
	if o.Anchored {
		return o.Lat
	}
	return 0
}

func anchoredLon(o Origin) float64 {
	if o.Anchored {
		return o.Lon
	}
	return 0
}

// haltedResult wraps raceHalted's stop for runRace: a dead RUN has no verdict
// (there is nothing left to record it on), a dead race is the silent fallback
// raceHalted already took.
func haltedResult(runCtx context.Context, org Origin, ok bool, racers int) raceResult {
	if runCtx.Err() != nil {
		return raceResult{}
	}
	return raceResult{Origin: org, OK: ok, Verdict: RaceVerdict{Outcome: RaceSilent,
		WinnerKind: org.Kind, WinnerLabel: org.Label, Racers: racers}}
}

// fastestRacer is the union's fastest answered server, nil when nobody
// answered. Only this race's own samples count (see racePing): an unanswered
// racer holds 0 and never wins.
func fastestRacer(union ookla.Servers) *ookla.Server {
	var win *ookla.Server
	for _, s := range union {
		if s.Latency > 0 && (win == nil || s.Latency < win.Latency) {
			win = s
		}
	}
	return win
}

// formatRaceOrigins renders the field for RaceVerdict.Origins: every origin in
// the caller's order with the fastest answer credited to its pool. A server two
// pools both surfaced counts for the one that reached it first (see
// unionCityCandidates), which is the race's own accounting.
func formatRaceOrigins(origins []Origin, union ookla.Servers, owner map[*ookla.Server]int) string {
	best := make([]time.Duration, len(origins))
	for _, s := range union {
		i := owner[s]
		if s.Latency > 0 && (best[i] == 0 || s.Latency < best[i]) {
			best[i] = s.Latency
		}
	}
	parts := make([]string, 0, len(origins))
	for i, org := range origins {
		p := org.Kind
		if org.Label != "" {
			p += ":" + org.Label
		}
		if best[i] > 0 {
			p += fmt.Sprintf("(%.1fms)", util.DurMS(best[i]))
		} else {
			p += "(-)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " | ")
}

// fetchOriginPools fetches every origin's pool concurrently - each its closest
// cityPoolSize servers, distance ties broken by the fetch's own echo and the
// ISP's server seated (see cityPoolSize) - and returns the pools beside a
// channel closed once every fetch has returned. full is each origin's WHOLE
// fetched list, nearest first, the pool's superset: the run reuses the
// winner's rather than fetching the same coordinate again (see raceResult).
// Each goroutine writes only its own index, so no lock is needed; the caller
// must not read pools or full before the channel closes, because a fetch
// abandoned by a deadline goes on writing (see raceOrigins).
func fetchOriginPools(ctx context.Context, origins []Origin, isp string) (pools, full []ookla.Servers, fetched <-chan struct{}) {
	pools = make([]ookla.Servers, len(origins))
	full = make([]ookla.Servers, len(origins))
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
			sort.SliceStable(sorted, byDistanceThenEcho(sorted))
			full[i] = sorted
			// The same window the run's ranking draws its candidates from: every
			// server latency-equivalent by distance to the nearest (autoMarginKM),
			// never fewer than the pool. Trimming the WHOLE list instead let a
			// small city's diversity pass seat other metros' servers over its own
			// duplicates - the list Ookla returns is padded out to ~25 rows from
			// wherever is next - and a shared server then got credited to the
			// wrong city.
			n := 0
			for n < len(sorted) && (n < cityPoolSize || sorted[n].Distance <= sorted[0].Distance+autoMarginKM) {
				n++
			}
			pool := append(ookla.Servers(nil), trimToCap(sorted[:n], sorted[:n], isp, cityPoolSize, cityPoolISPMax)...)
			// trimToCap's order is a seeding priority (ISP first); the lanes are
			// distance order, because unionCityCandidates credits a server two
			// cities both surfaced to the one that lists it earliest.
			sort.SliceStable(pool, byDistanceThenEcho(pool))
			pools[i] = pool
		}(i, org)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	return pools, full, done
}

// byDistanceThenEcho is the pool's order: nearest first, a distance tie broken
// by the fetch's own echo.
func byDistanceThenEcho(s ookla.Servers) func(i, j int) bool {
	return func(i, j int) bool {
		if s[i].Distance != s[j].Distance {
			return s[i].Distance < s[j].Distance
		}
		return echoLess(s[i], s[j])
	}
}

// echoLess orders two servers by the echo the list fetch recorded on them:
// answered before unanswered (the library writes PingTimeout, -1, for those),
// faster first. Only a tie-break: nothing here outranks distance. A fetched
// row always carries an echo or the -1 sentinel; a literal 0 is a row nobody
// pinged (test fixtures), or a sub-tick echo on a coarse clock, and files as
// unanswered.
func echoLess(a, b *ookla.Server) bool {
	if (a.Latency > 0) != (b.Latency > 0) {
		return a.Latency > 0
	}
	return a.Latency > 0 && a.Latency < b.Latency
}

// ispName is ISPFn with the nil case folded in.
func (o *Ookla) ispName() string {
	if o.ISPFn == nil {
		return ""
	}
	return o.ISPFn()
}

// pingRacers pings every server in the union concurrently and returns a channel
// closed once every ping has returned. Same reading rule as fetchOriginPools:
// no server's Latency is stable before the channel closes.
func pingRacers(ctx context.Context, union ookla.Servers) <-chan struct{} {
	var pg sync.WaitGroup
	for _, s := range union {
		pg.Add(1)
		go func(s *ookla.Server) {
			defer pg.Done()
			racePing(ctx, s)
		}(s)
	}
	pinged := make(chan struct{})
	go func() { pg.Wait(); close(pinged) }()
	return pinged
}

// RaceCandidate is one racer in a RaceListing: the server as the picker lists
// it, plus the origin whose pool surfaced it.
type RaceCandidate struct {
	ServerInfo
	Origin      string `json:"origin"`       // the Origin.Kind whose pool surfaced this server
	OriginLabel string `json:"origin_label"` // that origin's place name; may be empty
	// InField says the server is in the RUN's field: what a run centred where
	// this race centres would rank and choose from (see autoCandidates). A
	// racer from a losing city is listed for what it measured, but a run
	// would never measure it.
	InField bool `json:"in_field"`
}

// RaceListing is the field an automatic run would race right now: every
// origin's pool, deduplicated, pinged, fastest first. Winner is the origin the
// fastest answer came from - the city a run started this instant would centre
// on - or nil when nothing answered.
type RaceListing struct {
	Origins []Origin
	Servers []RaceCandidate
	Winner  *Origin
}

// RaceListing runs the city race's two rounds - the pool fetches and the pings
// - exactly as a run does, and hands back the whole field instead of a verdict.
// It is what the picker's Auto button shows: not a browse list around one city
// but the candidates the RUN button would choose between, with the pings the
// race ranks on. Same seams, same budget, same dedupe as raceOrigins; the one
// difference is that a deadline is an error here rather than a fallback,
// because a listing has nothing to degrade to. Pings only; no transfer.
//
// The run's field is deeper than the race's: around the winning origin it
// ranks every server within autoMarginKM of the nearest, up to autoPingMax
// (autoCandidates), where the race pinged only that origin's cityPoolSize
// pool. So after the race the listing pings the rest of the winner's field
// too - the same servers a run would go on to rank, with the same statistic -
// and InField marks the rows a run would actually choose between.
func (o *Ookla) RaceListing(ctx context.Context) (RaceListing, error) {
	origins := o.candidateOrigins()
	if len(origins) == 0 {
		return RaceListing{}, errors.New("nothing to race: no origin has a position")
	}
	ctx, cancel := context.WithTimeout(ctx, cityRaceBudget)
	defer cancel()
	pools, full, fetched := fetchOriginPools(ctx, origins, o.ispName())
	select {
	case <-fetched:
	case <-ctx.Done():
		return RaceListing{}, ctx.Err() // pools are still being written; read nothing
	}
	union, owner := unionCityCandidates(pools)
	if len(union) == 0 {
		return RaceListing{Origins: origins}, errors.New("no server list could be fetched for any origin")
	}
	pinged := pingRacers(ctx, union)
	select {
	case <-pinged:
	case <-ctx.Done():
		return RaceListing{}, ctx.Err()
	}
	win := fastestRacer(union)
	inField := map[string]bool{}
	var extras ookla.Servers
	if win != nil {
		raced := make(map[string]bool, len(union))
		for _, s := range union {
			raced[s.ID] = true
		}
		sorted := append(ookla.Servers(nil), full[owner[win]]...)
		sort.SliceStable(sorted, byDistanceThenEcho(sorted))
		for _, s := range autoCandidates(sorted, o.ispName()) {
			inField[s.ID] = true
			if !raced[s.ID] {
				extras = append(extras, s)
			}
		}
		if len(extras) > 0 {
			pinged := pingRacers(ctx, extras)
			select {
			case <-pinged:
			case <-ctx.Done():
				return RaceListing{}, ctx.Err()
			}
		}
	}
	all := append(append(ookla.Servers(nil), union...), extras...)
	// Each racer's Distance is from the origin whose pool surfaced it, so the
	// column would compare Montréal-from-Montréal with Toronto-from-Toronto.
	// Re-measured from the WINNING origin for every row that has a position:
	// the centre a run would use, so the list reads as one distance. An
	// unanchored winner (the API's placement) has no coordinate to measure
	// from, and then each row keeps its own.
	var centreLat, centreLon float64
	recentre := false
	if win != nil {
		org := origins[owner[win]]
		centreLat, centreLon, recentre = org.Lat, org.Lon, org.Anchored && usableCoord(org.Lat, org.Lon)
	}
	infos := make([]ServerInfo, 0, len(all))
	for _, s := range all {
		lat, lon, ok := serverCoord(s)
		dist := s.Distance
		if recentre && ok {
			dist = measuredKM(centreLat, centreLon, lat, lon)
		}
		infos = append(infos, ServerInfo{
			ID: s.ID, Sponsor: s.Sponsor, Name: s.Name, Country: s.Country,
			DistanceKM: dist, Lat: lat, Lon: lon, PingMS: pingMS(s.Latency),
		})
	}
	annotateFallback(ctx, all, infos) // pairs by index, before any reordering
	out := make([]RaceCandidate, 0, len(all))
	for i, s := range all {
		var org Origin
		if i < len(union) {
			org = origins[owner[s]]
		} else {
			org = origins[owner[win]] // an extra is the winner's field; it exists only when win != nil
		}
		out = append(out, RaceCandidate{ServerInfo: infos[i], Origin: org.Kind, OriginLabel: org.Label, InField: inField[s.ID]})
	}
	// Answered by ping; the unanswered keep the union's round-robin order -
	// their distances are each measured from a different origin and cannot be
	// compared across pools.
	sort.SliceStable(out, func(i, j int) bool { return lessByPing(&out[i].ServerInfo, &out[j].ServerInfo) })
	l := RaceListing{Origins: origins, Servers: out}
	if win != nil {
		w := origins[owner[win]]
		l.Winner = &w
	}
	return l, nil
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

// ServerPing is one kept server's refresh: its ping (nil when it could not be
// resolved or never answered) and, because the refresh has to probe the
// upload endpoint anyway to follow a migrated server's hop, the health verdict
// that probe produced - nil when it could not tell, the same three-state rule
// ServerInfo.FallbackOK follows. A starred server outside every listing earns
// its Unsupported mark this way and no other.
type ServerPing struct {
	PingMS     *float64
	FallbackOK *bool
}

// PingServersByID measures the servers named, the way the race measures a
// racer (racePing: the floor of the probe set), and answers per ID with the
// ping and the endpoint verdict (see ServerPing). It is the saved pane's
// refresh (see internal/web handleSpeedtestPing): the kept servers are mostly
// outside whatever list was last fetched, so nothing else ever pings or judges
// them. Resolved through the same seam the pin's early resolve uses, pinged
// concurrently - the caller bounds how many - and the by-ID position is not
// read (see GetOoklaServer: that endpoint reports the caller's coordinate).
func PingServersByID(ctx context.Context, ids []string) map[string]ServerPing {
	out := make(map[string]ServerPing, len(ids))
	res := make([]ServerPing, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			// One UserConfig per client, never shared across goroutines: the
			// library writes its transport ONTO the config (NewUserConfig sets
			// config.T) and reads it back on every request, so a shared one is
			// a data race between concurrent clients.
			uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
			s, err := fetchServerByID(ctx, uc, id)
			if err != nil || s == nil {
				return
			}
			// The by-ID record carries the legacy URL and Host="" (measured;
			// see GetOoklaServer), so currentEndpoint cannot rewrite it. Follow
			// the migration hop the way the pinned path does, or a migrated
			// server is pinged through its 307 on every sample - a figure a run
			// never measures. One extra round trip on an explicit user action,
			// and the verdict it produces is the pane's health mark.
			switch probeEndpoint(ctx, s) {
			case endpointOK:
				ok := true
				res[i].FallbackOK = &ok
			case endpointRetired:
				bad := false
				res[i].FallbackOK = &bad
			}
			racePing(ctx, s)
			res[i].PingMS = pingMS(s.Latency)
		}(i, id)
	}
	wg.Wait()
	for i, id := range ids {
		out[id] = res[i]
	}
	return out
}
