package speedtest

import (
	"context"
	"math"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/pingular/pingularity/internal/util"
)

// Latency under load ("bufferbloat"). The speedtest's transfers saturate the
// link; this samples round-trip latency DURING them and subtracts an idle
// baseline taken just before. The increase is queueing delay in oversized
// buffers (modem, ISP) - the metric behind Apple Responsiveness, Cloudflare AIM,
// and every "bufferbloat grade".
//
// Method: each sample is one TCP connect (SYN -> SYN/ACK) to a fixed anycast
// target - the same privilege-free technique the prober uses, no raw sockets.
// The idle baseline uses the SAME method and target, so the delta isolates the
// load effect; the Ookla ping (Result.PingMS) is a different method on a
// different path and is never compared against these. Probe traffic is a few
// handshakes per second: negligible against a saturated link.
//
// Known limits, by design (medians over many samples keep these tolerable):
//   - A SYN lost to saturation retransmits after ~1s, so a sample can read as
//     RTO quantization, not queue depth. The LOADED phases keep such samples on
//     purpose - genuine bloat reaches a second, the median absorbs the odd
//     retransmit, and the tail is a p95 rather than a maximum for the same
//     reason (see loadStat). The IDLE baseline instead discards them
//     (summarizeIdle): bloat is loaded minus idle, so a retransmit inflating
//     the baseline silently erases real bloat. What no phase can absorb is
//     loss so heavy that the retransmitted handshake exceeds lulConnTimeout:
//     connectRTT discards it, so the very worst congestion is the one case
//     this metric cannot see at all.
//   - The resolve picks the cleaner address family once, from one short burst -
//     or two, when the first one comes back lossy (lulSelectAddr). A host
//     reachable in only one family keeps that path however lossy - lossy data
//     beats none - and a path that turns lossy after being cached is only
//     dropped after lulFailInvalidate CONSECUTIVE failures, which random loss
//     rarely strings together, and then only at the next run boundary: a family
//     cannot be graded on a saturated link.
//   - If the transfer bottlenecked remotely (slow server, far peering), the
//     last-mile buffer never fills and the link looks cleaner than it is. Read
//     results as "no bloat at the measured throughput", never "no bufferbloat".
//   - During a download the probe's SYN shares the upstream with the download's
//     ACK stream, so very asymmetric links can leak upstream queueing into "down".
//   - Weak hardware at high throughput adds scheduler jitter to timestamps, as
//     with any software measurer.

// lulTarget is the dual-stack host for a latency sample - Cloudflare's resolver
// name, reachable over IPv4 OR IPv6 (the old IPv4 literal silently dropped all
// bufferbloat data on IPv6-only/NAT64 links). lulRunEndpoint resolves it to a
// literal once per run and every sample of that run dials the literal, so no
// timed handshake pays DNS - which during a load phase would suffer the very
// bufferbloat we measure. The family that wins the resolve's dial race is not
// blindly trusted: one surviving SYN can hand the win to a path that drops
// nearly half of them, so the literal is burst-validated and compared against
// the other family before it is cached (lulSelectAddr). A var so tests can point
// it at a local listener literal and stay off the real network (a literal skips
// the resolve).
var lulTarget = "one.one.one.one:443"

var (
	lulResolveMu sync.Mutex
	lulResolved  string // lulTarget resolved to a reachable "ip:port"; "" until a resolve succeeds
	lulFails     int    // consecutive failed connects, capped at lulFailInvalidate (lulNoteConnect)
)

// lulResolveDial performs the one-time dial that resolves lulTarget to a reachable
// literal. A var so a test can drive the success-only caching without real DNS.
var lulResolveDial = func(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: lulConnTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// lulFamilyDial dials lulTarget restricted to one address family ("tcp4" or
// "tcp6"), which is how lulSelectAddr resolves the OTHER family's literal when
// the race winner looks lossy. A var so tests can hand back a controlled
// literal without real DNS.
var lulFamilyDial = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: lulConnTimeout}
	return d.DialContext(ctx, network, addr)
}

// lulProbeBurst takes up to n handshake samples against addr, gap apart, and
// returns the RTTs that succeeded plus the count that failed outright. It is
// the one sampling loop behind both the idle baseline and resolve-time
// validation. A var because loopback cannot be made to drop SYNs: tests inject
// synthetic outcomes at this result boundary while the judging (summarizeIdle,
// lulBurstScore) stays the production rule.
//
// A probe the context cuts short reports NOTHING - no sample and no failure -
// whether it never started or was still in flight when the cancel landed. A
// cancel is not evidence about the path, so a cut burst comes back with fewer
// than n outcomes and its callers grade what it did report (lulValidateAddr,
// summarizeIdle).
var lulProbeBurst = realLulProbeBurst

func realLulProbeBurst(ctx context.Context, addr string, n int, gap time.Duration) (ms []float64, fails int) {
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		if v, ok := connectRTT(ctx, addr); ok {
			ms = append(ms, v)
		} else if ctx.Err() == nil { // a cancelled probe says nothing about the path
			fails++
		}
		if i < n-1 { // no need to wait after the final probe
			select {
			case <-ctx.Done():
			case <-time.After(gap):
			}
		}
	}
	return ms, fails
}

// lulDialAddr returns the address a run's samples dial: lulTarget verbatim when
// it is already an IP literal (tests), otherwise lulTarget resolved to a
// validated IP literal (lulSelectAddr), or the hostname while that resolve is
// still failing.
//
// The selection is cached, but only ONCE IT SUCCEEDS: a resolve that failed, or
// that ctx abandoned, is not cached, so the next run retries instead of dialing
// the hostname forever (which would re-pay DNS inside every timed handshake).
// The selection spends a deadline of its own rather than the caller's, so a
// cancelled caller (Abort, a browser disconnect) neither poisons the cache nor
// waits for it - see lulSelectAddr.
//
// This is also where a dead literal is dropped. lulNoteConnect only counts
// failures, so the drop lands here - which is to say at a run boundary, on an
// idle link - instead of inside the phase whose congestion produced the streak.
//
// Production reaches this only through lulRunEndpoint (tests call it directly),
// so a selection is paid once per run and never inside a timed handshake, and a
// run boundary is the one moment the link is quiet enough to grade a family on.
//
// The mutex guards only the cache: selection probes with connectRTT, whose
// lulNoteConnect takes the same mutex, so holding it across lulSelectAddr would
// deadlock.
func lulDialAddr(ctx context.Context) string {
	if host, _, err := net.SplitHostPort(lulTarget); err == nil && net.ParseIP(host) != nil {
		return lulTarget
	}
	lulResolveMu.Lock()
	if lulFails >= lulFailInvalidate {
		lulResolved, lulFails = "", 0 // the cached literal stopped answering
	}
	cached := lulResolved
	lulResolveMu.Unlock()
	if cached != "" {
		return cached
	}
	addr := lulSelectAddr(ctx)
	if addr == "" {
		return lulTarget // failed or abandoned; retry next run rather than caching the miss
	}
	lulResolveMu.Lock()
	defer lulResolveMu.Unlock()
	lulResolved, lulFails = addr, 0
	return addr
}

// lulSelectAddr resolves lulTarget to the literal the sampler should trust, or
// "" when the resolve fails or ctx gives up on it. Go's dial race hands the win
// to whichever family's first SYN survives - which on a path randomly dropping
// near half its SYNs still happens constantly - so the winner is validated with
// a short real burst before it is believed. A winner showing loss or
// retransmit-shaped samples is compared against the other family's literal and
// the strictly cleaner of the two wins.
//
// The measuring runs on a deadline of its own (lulSelectBudget from
// context.Background()) because a caller's cancel would truncate a burst in
// flight, and the selection would then be grading a family on evidence the
// caller destroyed. ctx is answered by ABANDONING the selection instead: "" is
// returned at once so an Abort is not left waiting out the budget, and because
// no address is cached the next run selects again. Its verdict is dropped, but
// not its side effects: the abandoned probes keep feeding the failure streak
// for up to lulSelectBudget, so they can still condemn a literal a later run
// caches (lulNoteConnect, and the invalidation in lulDialAddr).
func lulSelectAddr(ctx context.Context) string {
	done := make(chan string, 1) // buffered: an abandoned selection must not block on the send
	go func() { done <- lulSelectOnBudget() }()
	select {
	case addr := <-done:
		return addr
	case <-ctx.Done():
		return ""
	}
}

// lulSelectOnBudget is the selection itself: a resolve dial, a validation burst,
// and - only when that burst grades lossy - the same pair for the other family.
// lulSelectBudget is the sum of the step caps, so a step cannot spend another
// step's cap; each step gets the smaller of its own cap and the budget's
// remainder.
func lulSelectOnBudget() string {
	ctx, cancel := context.WithTimeout(context.Background(), lulSelectBudget)
	defer cancel()
	dctx, dcancel := context.WithTimeout(ctx, lulSelectDialStep)
	c, err := lulResolveDial(dctx, lulTarget)
	dcancel()
	if err != nil {
		return ""
	}
	cand := c.RemoteAddr().String()
	c.Close()
	score := lulValidateAddr(ctx, cand)
	if score == 0 {
		return cand // clean winner: skip the second resolve and burst entirely
	}
	other := lulOtherFamilyAddr(ctx, cand)
	if other == "" {
		return cand // single-family host: a lossy path still beats no data (see header)
	}
	if lulValidateAddr(ctx, other) < score {
		return other
	}
	return cand
}

// lulValidateAddr grades one literal with a short real burst; 0 is clean. The
// burst gets its own step of the selection budget so one silent family cannot
// spend the time the other one needs, and only the probes that reported are
// graded: a path slower than about lulSelectBurstStep/lulSelectProbes per
// handshake loses its last probes to that cap, and counting those as loss would
// grade an honest satellite link lossy. On a path fast enough to finish its
// burst, loss shows up in what did report - an outright failure, or a sample
// carrying an RTO. Two families this cannot grade: one too slow to land a
// single probe, which comes back as an empty burst and is unusable rather than
// clean (lulBurstScore); and one slow enough for the cap to bite, where loss
// landing on a cut probe reports nothing at all, so it grades like a clean slow
// family.
func lulValidateAddr(ctx context.Context, addr string) float64 {
	ctx, cancel := context.WithTimeout(ctx, lulSelectBurstStep)
	defer cancel()
	return lulBurstScore(lulProbeBurst(ctx, addr, lulSelectProbes, lulIdleGap))
}

// lulBurstScore is the fraction of a validation burst that went bad: probes
// that failed outright plus samples shaped like retransmits (dropRetransmits'
// rule - the same rule the idle baseline uses, so selection and summarization
// cannot disagree about what a retransmit looks like). 0 is clean, 1 is
// nothing usable; an empty burst counts as unusable, not clean.
func lulBurstScore(ms []float64, fails int) float64 {
	total := len(ms) + fails
	if total == 0 {
		return 1
	}
	bad := fails + len(ms) - len(dropRetransmits(ms))
	return float64(bad) / float64(total)
}

// lulOtherFamilyAddr resolves lulTarget restricted to the family cand is NOT.
// It returns "" whenever that family cannot be produced: cand is not a
// host:port, cand's host is not an IP literal, or the family dial fails - no
// record, unreachable, or out of time on its own step. Callers read every one of
// those the same way - cand keeps the job whatever its burst looked like - so
// the single-family case needs no separate signal.
func lulOtherFamilyAddr(ctx context.Context, cand string) string {
	host, _, err := net.SplitHostPort(cand)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ""
	}
	network := "tcp6"
	if ip.To4() == nil {
		network = "tcp4"
	}
	ctx, cancel := context.WithTimeout(ctx, lulSelectDialStep)
	defer cancel()
	c, err := lulFamilyDial(ctx, network, lulTarget)
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.RemoteAddr().String()
}

// lulRunEndpoint resolves the probe target ONCE for a whole run, so the idle
// baseline and both loaded phases are provably the same endpoint. The delta is
// only meaningful if the two halves measured the same path, and a sample that
// resolved for itself could not promise that: a literal dropped mid-run after
// lulFailInvalidate failures - exactly what congestion produces - would move
// later samples onto a different address, or a different family, than the
// baseline they are subtracted from. Resolving here makes a run's selection a
// single idle-time act: nothing after this point resolves anything, so no timed
// handshake carries DNS or a family selection - except while the resolve is
// still failing, when this hands back the hostname (below) - and an
// invalidation takes effect at the next run boundary rather than inside a run.
//
// It never returns "": while the resolve is still failing this hands back the
// hostname, and the samples pay DNS in the handshake. That pins the same NAME
// for every sample of the run rather than the same address - a dual-stack
// hostname is re-raced on every dial, so those samples can still land on
// different families - but it beats a per-sample re-resolve, which would put a
// whole lulSelectBudget selection inside the idle burst's own cap.
func lulRunEndpoint(ctx context.Context) string { return lulDialAddr(ctx) }

// lulNoteConnect counts connect outcomes for the resolve cache. The cached
// literal never expires on its own, so if the stack it belongs to dies later
// (the first resolve picked IPv6 and IPv6 then breaks while IPv4 still works -
// exactly the failure class this tool monitors), every future sample would fail
// silently for the rest of the process. lulFailInvalidate consecutive failures
// mark it dead and lulDialAddr drops it at the next run boundary, where the
// re-selection is graded on an idle link rather than on the congestion that
// produced the streak. A run in flight loses nothing by that wait - its samples
// dial the endpoint lulRunEndpoint pinned - and a success before the boundary
// clears the streak, because a path that recovered is not dead.
func lulNoteConnect(ok bool) {
	lulResolveMu.Lock()
	defer lulResolveMu.Unlock()
	if ok {
		lulFails = 0
		return
	}
	if lulFails < lulFailInvalidate { // past the threshold a higher count means nothing more
		lulFails++
	}
}

const (
	lulConnTimeout    = 3 * time.Second        // per-sample cap so a hang can't stall the cadence
	lulInterval       = 150 * time.Millisecond // self-clocking gap between samples
	lulIdleProbes     = 8                      // baseline burst size
	lulIdleGap        = 75 * time.Millisecond  // gap between baseline probes
	lulMinSamples     = 10                     // loaded median needs at least this many samples
	lulMinPhase       = 3 * time.Second        // ... over at least this much saturated time
	lulIdleMin        = 5                      // idle median needs at least this many probes
	lulIdleBudget     = 8 * time.Second        // cap on the baseline burst alone (a firewalled LAN can time out every probe); a validation burst is capped by lulSelectBurstStep
	lulFailInvalidate = 5                      // consecutive failed connects that mark the cached literal dead (dropped at the next run boundary)
	lulSelectProbes   = 5                      // validation burst size: loss heavy enough to matter almost always shows in five

	// An unloaded sample this far above its burst's minimum is a retransmit, not
	// latency: a lost SYN retransmits on a one-second initial RTO at the defaults
	// of the systems we ship on (RFC 6298), so honest samples and retransmits sit
	// ~1s apart and this splits the gap. A host tuned below that RTO is not
	// something this code can see. Min-relative on purpose (see dropRetransmits).
	lulRetransmitGuardMS = 500.0

	// A handshake cannot reach this without at least one SYN retransmit: it is
	// the initial RTO above a zero-RTT path. dropRetransmits uses it only as a
	// whole-burst backstop, never per sample - a real 600 ms link must keep its
	// real samples, and its retransmits land near 1600 where the min-relative
	// rule already takes them.
	lulRetransmitFloorMS = 1000.0

	// Each step of a selection is sized from the work it holds, and the budget for
	// the whole of it is their sum: a resolve dial is DNS plus one handshake, so
	// it gets what one sample gets, and a burst is lulSelectProbes handshakes
	// lulIdleGap apart - ~0.5s on a healthy path, ~3.3s on a 600 ms satellite
	// link - so 4s lets a slow but honest family finish while a silent one is cut
	// off. The steps size the budget and not the other way round, because a share
	// of some round total is what expires before a slow link can answer. The sum
	// has no slack, so what the steps spend outside their own work - scheduling,
	// the gaps between them - comes out of the last one.
	lulSelectDialStep  = lulConnTimeout
	lulSelectBurstStep = 4 * time.Second
	lulSelectBudget    = 2*lulSelectDialStep + 2*lulSelectBurstStep
)

// connectRTT takes one latency sample: dial, time the handshake, close. Dials
// "tcp" (v4 or v6) against exactly the addr it is handed - the endpoint
// lulRunEndpoint pinned for the run, or the literal a validation burst is
// grading - so the timed handshake is pure connect RTT with no resolve of any
// kind inside it.
func connectRTT(ctx context.Context, addr string) (float64, bool) {
	d := net.Dialer{Timeout: lulConnTimeout}
	start := time.Now()
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		if ctx.Err() == nil { // a cancelled sample says nothing about the target
			lulNoteConnect(false)
		}
		return 0, false
	}
	rtt := util.DurMS(time.Since(start))
	c.Close()
	lulNoteConnect(true)
	return rtt, true
}

// summarizeLoad reduces one phase's samples to the pair the product stores. It
// is a named function, not an inline literal in the sampler, so a test can pin
// WHICH statistic the tail is: asserting p95's arithmetic proves nothing about
// the sampler if the sampler still reduces with a maximum.
func summarizeLoad(ms []float64) loadStat {
	return loadStat{med: median(ms), tail: p95(ms)}
}

// p95 is the nearest-rank 95th percentile of an unsorted slice: the smallest
// sample at least 95% of the others fall at or below. Nearest-rank rather than
// interpolated so the value is always one that was actually measured - an
// interpolated tail between a real 40 ms and a retransmitted 1022 ms would be a
// latency the link never produced. With this sampler's ~90 samples per phase
// the index is ~86, so it is well clear of both ends; below ~20 samples it
// degenerates towards the maximum, which is the honest behaviour when there is
// too little data to have a tail.
func p95(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(0.95*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

// median of an unsorted slice (averages the middle pair for even lengths).
func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// dropRetransmits discards retransmit-shaped samples from an UNLOADED burst:
// anything more than lulRetransmitGuardMS above the burst's own minimum. With
// no load, an honest sample and a retransmitted one sit one RTO apart - a
// second at the defaults we ship on - so the guard splits that gap. It is
// relative to the minimum because an absolute cutoff would eat a link whole: a
// 600 ms satellite path's real samples cluster near 600 with its retransmits
// near 1600. Its callers apply it to unloaded bursts only (summarizeIdle,
// lulBurstScore); a loaded phase's near-1s sample is genuine bufferbloat - the
// signal itself - so summarizeLoad does not filter at all (see loadStat).
func dropRetransmits(ms []float64) []float64 {
	if len(ms) == 0 {
		return nil
	}
	lo := ms[0]
	for _, v := range ms[1:] {
		if v < lo {
			lo = v
		}
	}
	// The min-relative rule needs one honest sample to anchor on. When the whole
	// burst sits at or above the RTO floor the minimum is itself a retransmit,
	// min+guard then covers all the others and nothing is dropped - which is how
	// a fabricated ~1013 ms idle baseline reached production. Anchoring the
	// backstop on the MINIMUM keeps a slow link out of it (a 600 ms satellite
	// path is nowhere near the floor); the exception is a link whose genuine
	// idle RTT is at or above the floor, which gets no baseline at all rather
	// than one that would zero its bloat.
	if lo >= lulRetransmitFloorMS {
		return nil
	}
	var kept []float64
	for _, v := range ms {
		if v <= lo+lulRetransmitGuardMS {
			kept = append(kept, v)
		}
	}
	return kept
}

// summarizeIdle reduces the baseline burst to the stored idle median - nil
// when too few probes remain after dropRetransmits to call it a baseline. The
// lulIdleMin gate counts FILTERED samples: a burst that was mostly retransmits
// keeps too few to hold a baseline, and one that was ENTIRELY retransmits keeps
// none at all (dropRetransmits' backstop). Bloat is max(0, loaded-idle), so
// publishing a retransmit-inflated idle would silently zero it. Named and pure
// for the same reason as summarizeLoad: a test of the arithmetic proves nothing
// unless the sampler reduces with this exact function.
func summarizeIdle(ms []float64) *float64 {
	kept := dropRetransmits(ms)
	if len(kept) < lulIdleMin {
		return nil
	}
	m := median(kept)
	return &m
}

// measureIdleLatency takes the pre-transfer baseline burst. nil when too few
// trustworthy probes succeed to call it a baseline (target unreachable, dying
// link, or a burst that was mostly retransmits).
func measureIdleLatency(ctx context.Context, addr string) *float64 {
	// Cap the whole burst: on a firewalled LAN every connect can hit the full
	// lulConnTimeout, stretching this to ~probes*3s and making a manual Run Now
	// look hung. connectRTT's DialContext honors this. addr is the run's pinned
	// endpoint, so the cap covers probes and nothing else - the selection was
	// already paid, outside it (lulRunEndpoint).
	ctx, cancel := context.WithTimeout(ctx, lulIdleBudget)
	defer cancel()
	ms, _ := lulProbeBurst(ctx, addr, lulIdleProbes, lulIdleGap)
	return summarizeIdle(ms)
}

// loadStat summarizes one load phase's samples: the median (typical added
// latency) and the p95 (the tail - what real-time apps feel; a call stutters on
// the peaks, not the median).
//
// The tail was the raw MAXIMUM until it was measured against real history, and
// the maximum turned out to describe the operating system rather than the link.
// A single lost SYN is retransmitted on a fixed 1000 ms RTO, so on a link whose
// base RTT is 22 ms the largest of ~90 handshakes lands at ~1022 ms - the same
// number on any connection, for any severity, whenever one packet is lost.
// Measured over 30 days of runs: 32% of phases had a max in [900,1100] ms, 96%
// of those inside [1020,1045], with an EMPTY valley at 500-900 ms and a second
// rung at ~2030 ms. A continuous queueing delay cannot produce that shape; a
// retransmit ladder is the only thing that can.
//
// So the maximum was a coin-flip on packet loss dressed as a latency
// measurement. The p95 of the same samples is contaminated by an RTO in 0.005%
// of phases against the max's 31.6%, and it is also STRICTER about real bloat:
// under this sampler's response-paced cadence a p95 rises once a 1022 ms
// episode occupies 26% of the phase, where the median needs 87%.
type loadStat struct{ med, tail float64 }

// medPtr/tailPtr return the stat as a *float64, nil-safe so the caller can chain
// off a possibly-nil sampler result.
func (s *loadStat) medPtr() *float64 {
	if s == nil {
		return nil
	}
	m := s.med
	return &m
}
func (s *loadStat) tailPtr() *float64 {
	if s == nil {
		return nil
	}
	m := s.tail
	return &m
}

// startLoadSampler samples in one sequential goroutine (samples never overlap;
// a slow connect just delays the next). The returned stop function ends
// sampling, waits for the goroutine, and reduces with summarizeLoad - or nil
// when the phase was too short or too few samples succeeded to mean anything.
//
// addr is the endpoint lulRunEndpoint pinned for the run, so a phase resolves
// nothing: every phase of a run measures the same path, and no DNS lookup lands
// inside a handshake whose timing is the measurement.
func startLoadSampler(ctx context.Context, addr string) (stop func() *loadStat) {
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var ms []float64
	start := time.Now()
	go func() {
		defer close(doneCh)
		for {
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			default:
			}
			if v, ok := connectRTT(ctx, addr); ok {
				ms = append(ms, v)
			}
			select {
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(lulInterval):
			}
		}
	}()
	return func() *loadStat {
		close(stopCh)
		<-doneCh
		if time.Since(start) < lulMinPhase || len(ms) < lulMinSamples {
			return nil
		}
		st := summarizeLoad(ms)
		return &st
	}
}
