// Helpers that only tests call. They live here so the shipped binary does not
// carry them; the production paths are rankedServersRaced, bestIndex and the
// resolve cache's own expiry.

package speedtest

import (
	"context"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// rankedServers returns the ranked candidates, their ranking pings, and the IDs
// dropped for having no HTTP Legacy Fallback (see fallbackHealth) so the caller
// can say so in the log.
func rankedServers(ctx context.Context, servers ookla.Servers, isp string) (ookla.Servers, map[string]*float64, []string, bool) {
	return rankedServersRaced(ctx, servers, isp, nil, false, nil, 1)
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

// flushDestResolveCache empties the memo. Tests only: a scripted resolver in
// one test must not answer for the previous one's names.
func flushDestResolveCache() {
	destResolveMu.Lock()
	clear(destResolveCache)
	destResolveMu.Unlock()
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

// raceOrigins is raceCities over an already-picked field (see candidateOrigins),
// reduced to the verdict the run acts on; runRace is the whole result.
func (o *Ookla) raceOrigins(ctx context.Context, origins []Origin) (Origin, bool) {
	r := o.runRace(ctx, origins, cityPoolSize)
	return r.Origin, r.OK
}
