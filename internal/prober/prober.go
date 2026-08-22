// Package prober determines connectivity by dialing several anchor endpoints
// concurrently and applying a quorum rule, so that a single endpoint flapping
// cannot produce a false outage.
package prober

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/pingular/pingularity/internal/config"
)

// DNS-resolution-latency probe (see ResolveTime). Each call prepends a fresh random
// label to the base (see probeName), so no two probes ask the same name. The base is
// an anycast-NS domain (low, constant authoritative RTT), so the variation reflects
// the host's resolver, not distance.
// Vars so tests can redirect them.
var (
	dnsProbeDomain  = "cloudflare.com" // written unrooted; probeName adds the trailing dot
	dnsProbeTimeout = 3 * time.Second  // a resolver slower than this counts as failing
)

// probeName builds one name for the DNS probe. Every call draws a fresh random
// label, so no two probes ask the same name: a repeated name is one a resolver
// can answer out of the entry it cached for the previous probe, which would time
// a cache hit instead of a resolution.
//
// The trailing dot roots the name, and it is load-bearing. Go looks up a rooted
// name once; an unrooted one it also re-asks with each entry of the host's search
// list appended (GOROOT/src/net/dnsclient_unix.go, dnsConfig.nameList). So on a
// host that has a search domain, dropping the dot buys a second lookup per probe:
// two wire queries instead of one, since the probe asks a single A question per
// name (see lookupHost).
// Go walks that name list in order and stops at the first name that comes back
// with addresses (the `len(addrs) > 0` break closing goLookupIPCNAMEOrder's loop,
// same file) - so the walk continues past a name that resolves to nothing, which
// is what the random label is meant to draw and what ResolveTime counts as
// healthy below. (Against the shipped base that answer is NODATA rather than
// NXDOMAIN - cloudflare.com replies NOERROR with an SOA and no records for a name
// it does not have - but Go reports both as IsNotFound, so ResolveTime treats
// them alike.) The doomed second lookup therefore falls inside the window
// ResolveTime times. The second lookup also hands the random probe label, search
// domain appended, to the recursive resolver the host is configured to use.
// Whether that name travels any further depends on the search domain being a
// delegated zone, which this code cannot know.
//
// Rooting does NOT simply make the reading smaller, and the release note must not
// say so. It removes the doomed lookup on a host whose search domain has no
// answer for the name, which reads lower. But a search list is consulted FIRST
// for a name with fewer dots than ndots (5 in a default Kubernetes pod, where
// this name's two dots qualify), so a search domain that answers wildcards used
// to return addresses for a DIFFERENT name, fast, and end the walk there - and
// that fast wrong answer is what got timed. Rooting skips that shortcut and times
// the name actually asked for, which reads higher. Which way a given host moves
// depends on its resolver configuration, so readings either side of the change
// are not comparable.
//
// TestResolveTimeLooksUpARootedName pins the dot on the name ResolveTime hands
// the resolver, on any host; TestResolveTimeAsksOneNamePerProbe pins the wire
// count, though only a host whose search list contributes a name can fail it;
// TestResolveTimeNameIsFreshEachProbe pins the per-probe label; the wire-count
// test also pins the single-question lookup, and
// TestResolveTimeLostLaneIsNotHealthyLatency pins why it is single (below).
func probeName() string {
	var b [6]byte
	_, _ = crand.Read(b[:])
	return "pp" + hex.EncodeToString(b[:]) + "." + dnsProbeDomain + "."
}

// lookupHost is the resolver call ResolveTime makes: a single A question via
// LookupIP("ip4"), not LookupHost's A+AAAA pair. One question makes the sample
// one round trip. The pair recorded the SLOWER of two round trips - a few ms
// of steady inflation and a doubled spike tail - and, worse, one lost packet
// on either lane held the call open to the full budget while the answered
// lane's NXDOMAIN surfaced as IsNotFound, so the probe plotted a healthy
// budget-length "latency" sample on a resolver that had answered instantly
// (Go's built-in resolver path - the shipped Linux binaries; the macOS and
// Windows system-resolver builds surfaced that stall as a timeout already.
// Measured Aug 2026: 8 of 8 lost-packet runs). The record TYPE is orthogonal
// to transport - an IPv6-only host still asks its resolver for A records over
// IPv6, and the probe's random name has no records of any type anyway.
//
// It is a var, and a closure rather than a method value, so it re-reads
// net.DefaultResolver on every call: that keeps a test's swapped-in resolver
// effective, and lets a test read the exact name string ResolveTime passes,
// which no wire capture can recover (Go roots every name before it reaches
// the wire).
var lookupHost = func(ctx context.Context, name string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip4", name)
}

// ResolveTime times one DNS lookup of a freshly randomized name (see probeName).
// ok is true when the resolver answered - including the expected NXDOMAIN for the
// random name (DNS is healthy, just no such record) - and false only when
// resolution actually fails (timeout, SERVFAIL, no resolver) or the lookup
// consumed the whole probe budget, whatever the library hands back - a nil
// error and a real address included: an answer that surfaces only at the
// deadline is a timeout wearing an answer, not health. Both healthy returns
// enforce that, through overBudget. On failure it also returns the underlying
// error so the caller can classify/log WHY (via DialErrClass); err is nil
// whenever ok is true.
func ResolveTime(ctx context.Context) (time.Duration, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, dnsProbeTimeout)
	defer cancel()
	name := probeName() // fresh and rooted; see probeName for why the dot matters
	start := time.Now()
	// lookupHost calls DefaultResolver, the host's OS-configured resolver
	// (/etc/resolv.conf or libc) - deliberately NOT the latency anchors and not a
	// Pingularity setting, so this times the resolution path the machine actually
	// uses. A resolver that hijacks NXDOMAIN (returns an A record for the random
	// name) reads as a healthy answer in the err==nil branch below - as it should,
	// since the resolution path demonstrably works - but only when it answered
	// inside the budget, which is why that branch runs the same overBudget check
	// as the IsNotFound one.
	_, err := lookupHost(rctx, name)
	d := time.Since(start)
	if err == nil {
		// A synthesised A record is still an answer, so the only way this branch
		// can lie is by timing: a hijacking resolver parked on the deadline hands
		// back a positive reply just as the budget runs out, and the probe would
		// plot a truthful ~3s latency labelled HEALTHY. The reading is not wrong;
		// the verdict is, and nothing downstream debounces it.
		if berr := overBudget(rctx, d); berr != nil {
			return d, false, berr
		}
		return d, true, nil
	}
	var de *net.DNSError
	// dnsAnsweredErrno widens the answered check for Windows, whose system
	// resolver can report the probe's NODATA answer as WSANO_DATA - an errno
	// Go does not map to IsNotFound (see dialerr_windows.go).
	if errors.As(err, &de) && (de.IsNotFound || dnsAnsweredErrno(err)) {
		// Only an answer that arrived inside the budget counts. Go's resolver
		// aggregates errors across lookup lanes and retries, and a benign
		// IsNotFound from one lane used to mask another lane that never
		// answered - the call then returned at the deadline and the whole
		// budget plotted as healthy latency (one lost UDP packet was enough).
		// The single-question lookup closes the two-lane case, but retries and
		// multi-nameserver walks can still land an IsNotFound on the deadline,
		// so the rule is pinned here, where the verdict is made.
		if berr := overBudget(rctx, d); berr != nil {
			return d, false, berr
		}
		return d, true, nil // NXDOMAIN: the resolver answered; the path works
	}
	return d, false, err // timeout / SERVFAIL / no resolver -> DNS unhealthy
}

// overBudget decides whether a lookup that produced SOMETHING - a positive
// answer or a benign IsNotFound - nevertheless took the whole probe budget to
// produce it. It returns the failure error to report, or nil when the answer
// arrived in time. Both healthy returns in ResolveTime run it, because the
// contract there ("the lookup consumed the whole probe budget, whatever the
// library hands back") is about elapsed time, not about which of the two shapes
// the answer happened to arrive in.
//
// Two checks, and the second is not redundant with the first. rctx.Err() is set
// by the timer goroutine context.WithTimeout arms, and that callback runs a
// moment AFTER the nominal deadline - so an answer landing in that gap sees a
// context that still reads nil. Measured Aug 2026, spinning to the instant
// time.Since(start) >= budget and reading Err() right there: nil in 2000 of
// 2000 trials. End to end, against a hijacking resolver stub scheduled on a
// 30ms budget +/- 3ms, 600 probes each: unguarded leaked 10 healthy verdicts
// past the budget (up to 30.115ms), the context check alone still leaked 6 (up
// to 32.281ms), and both checks together leaked 0, with the healthy count
// otherwise unchanged (157 vs 173 and 153, all inside the ~50/50 split's
// noise).
//
// The arithmetic check cannot fail a legitimate answer: d is measured from
// after the deadline was armed (probeName runs in between), so the budget
// remaining at start is already less than dnsProbeTimeout, and d >=
// dnsProbeTimeout means the call outlived every bit of it. An answer that
// really arrived inside the budget has d < dnsProbeTimeout by construction.
//
// The fallback to context.DeadlineExceeded is load-bearing rather than tidy: in
// exactly the timer-lag case rctx.Err() is nil, and wrapping nil would leave
// DialErrClass no deadline to recognise, filing a budget-exhausted probe under
// dns.fail.other instead of dns.fail.timeout - the class its IsNotFound twin's
// test pins.
//
// A cancelled PARENT context arrives here as context.Canceled rather than that
// fallback: rctx is a child of the monitor's ctx and its own cancel runs only
// on the way out, so Canceled can only have come from above. For the IsNotFound
// branch that is what the old inline rctx.Err() check already did. For the
// err==nil branch it is new - that branch used to return healthy whatever rctx
// said - so a lookup that genuinely answered in 10ms while the monitor was
// stopping now returns ok=false under an error text that misdescribes it. That
// is deliberate and safe: the sole caller (monitor.go, "if !ok && ctx.Err() !=
// nil { return }") drops the sample before any dns.fail.* bump, "dns down"
// warning or InsertDNS, so a shutdown cannot fabricate a DNS failure. Note it
// is NOT dropped before every counter - dns.attempts and dns.last_attempt_ts
// are bumped above that guard, as they are for every probe - and what such a
// sample does lose is its dns.latency / dns.last_ok_ts observation, on the last
// probe of a run that is being torn down anyway.
func overBudget(rctx context.Context, d time.Duration) error {
	if d < dnsProbeTimeout && rctx.Err() == nil {
		return nil
	}
	cause := rctx.Err()
	if cause == nil {
		cause = context.DeadlineExceeded // the timer callback has not run yet
	}
	return fmt.Errorf("resolver answered only at the %v budget: %w", dnsProbeTimeout, cause)
}

// DialErrClass reduces a failed-dial error to a coarse, closed enum for the
// probe.fail.* counters, so an operator can tell WHY probes fail (link down vs
// endpoint refuses vs DNS broke), not just that they do. Never includes the
// address or any host detail.
func DialErrClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return "timeout"
	}
	// Errno-based classes (refused / *_unreachable) differ by OS: the check lives
	// in the build-tagged dialErrno so Windows can match Winsock's WSA errnos.
	if c := dialErrno(err); c != "" {
		return c
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	return "other"
}

// TargetResult is the outcome of dialing one target.
type TargetResult struct {
	Target  config.Target
	OK      bool
	Latency time.Duration
	Err     error
}

// FamilyResult is the per-address-family quorum outcome for one round.
type FamilyResult struct {
	Family  string
	Online  bool          // strict majority of this family's targets succeeded
	Latency time.Duration // lowest latency among this family's successful targets
	OK      int
	Total   int
}

// Result aggregates one probe round across all targets and families.
type Result struct {
	TS       time.Time
	Online   bool                    // any family online
	Families map[string]FamilyResult // keyed by family ("ipv4"/"ipv6")
	Targets  []TargetResult
	// Skipped: nothing was dialable (every family explicitly off - a live mode
	// flip can land between the monitor's gate and this round). The round
	// measured nothing; the monitor treats it as idle, never as an outage.
	Skipped bool
}

// Prober dials a fixed set of targets each round.
type Prober struct {
	targets []config.Target
	timeout time.Duration
	dialer  net.Dialer

	// TimeoutFn, if set, supplies the per-dial timeout live (runtime changes).
	// Falls back to the constructed timeout when nil.
	TimeoutFn func() time.Duration

	// FamilyEnabledFn, if set, reports whether a target's address family should be
	// probed this round, re-read live so e.g. the IPv6 mode setting applies without a
	// restart. Targets of disabled families are skipped. Nil means probe all.
	FamilyEnabledFn func(family string) bool

	// FamilyOffFn, if set, reports that a family is EXPLICITLY disabled by the
	// operator ("off" mode), as opposed to merely filtered out by auto-detection.
	// Explicitly-off targets are never dialed, and never pulled back in by the
	// fail-open below - `-ipv4 off` must mean off.
	FamilyOffFn func(family string) bool
}

// New builds a Prober for the given targets and per-dial timeout.
func New(targets []config.Target, timeout time.Duration) *Prober {
	return &Prober{targets: targets, timeout: timeout}
}

func (p *Prober) curTimeout() time.Duration {
	if p.TimeoutFn != nil {
		if d := p.TimeoutFn(); d > 0 {
			return d
		}
	}
	return p.timeout
}

// Probe dials every enabled target concurrently and returns the aggregated
// result. A round is Online when ANY address family has a strict majority of its
// own targets succeed (see aggregate).
func (p *Prober) Probe(ctx context.Context, now time.Time) Result {
	targets := p.targets
	if p.FamilyEnabledFn != nil {
		// eligible = everything not explicitly switched off by the operator; act =
		// the subset auto-detection says is live right now.
		act := make([]config.Target, 0, len(p.targets))
		eligible := make([]config.Target, 0, len(p.targets))
		for _, t := range p.targets {
			if p.FamilyOffFn != nil && p.FamilyOffFn(t.Family) {
				continue // explicitly disabled: never dialed, never failed-open into
			}
			eligible = append(eligible, t)
			if p.FamilyEnabledFn(t.Family) {
				act = append(act, t)
			}
		}
		// Fail open WITHIN the eligible set only: auto-detection that excludes every
		// remaining target (boot, PPPoE renegotiation, or a real total outage that
		// took the interface addresses with it) still dials those targets, so a dead
		// link is recorded as down rather than silently unprobed - but an explicit
		// "off" is never resurrected. When EVERY family is explicitly off, main.go
		// stops probing - though a live mode flip can land between that gate and
		// this read, so an empty eligible set IS possible here; the monitor treats
		// the resulting zero-target round as idle, never as an outage.
		if len(act) > 0 {
			targets = act
		} else {
			targets = eligible
		}
		// The filter emptied a non-empty target list = every configured family
		// is explicitly off. Skip the dial entirely and say so. (A prober built
		// with no targets at all - the monitor tests' no-network harness - keeps
		// the old empty-round result instead.)
		if len(targets) == 0 && len(p.targets) > 0 {
			return Result{TS: now, Skipped: true}
		}
	}
	results := make([]TargetResult, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t config.Target) {
			defer wg.Done()
			results[i] = p.dial(ctx, t)
		}(i, t)
	}
	wg.Wait()
	return aggregate(now, results)
}

// aggregate computes the per-family quorum and overall result from one round's
// target results. A family is online when a strict majority of its targets
// succeed; overall is online when any family is. Families are judged independently
// so a single-family outage (or absence) can't drag down the other.
func aggregate(now time.Time, results []TargetResult) Result {
	fams := map[string]FamilyResult{}
	for _, r := range results {
		fam := r.Target.Family
		fr := fams[fam]
		fr.Family = fam
		fr.Total++
		if r.OK {
			fr.OK++
			if fr.OK == 1 || r.Latency < fr.Latency { // first success, or a new minimum
				fr.Latency = r.Latency
			}
		}
		fams[fam] = fr
	}

	anyOnline := false
	for fam, fr := range fams {
		fr.Online = fr.OK*2 > fr.Total // strict majority within the family
		fams[fam] = fr
		if fr.Online {
			anyOnline = true
		}
	}
	return Result{
		TS:       now,
		Online:   anyOnline,
		Families: fams,
		Targets:  results,
	}
}

// famCheckTTL bounds how often the HasGlobal*Cached helpers re-enumerate
// interface addresses; the result is stable enough that per-round checks would
// be wasteful.
const famCheckTTL = 30 * time.Second

var (
	v6Mu sync.Mutex
	v6At time.Time
	v6OK bool

	v4Mu sync.Mutex
	v4At time.Time
	v4OK bool
)

// HasGlobalIPv6Cached is HasGlobalIPv6 memoized for famCheckTTL, cheap enough to
// consult every probe round. Re-checking rather than checking once at startup lets
// "auto" IPv6 mode notice IPv6 appearing or vanishing while running.
func HasGlobalIPv6Cached() bool {
	v6Mu.Lock()
	defer v6Mu.Unlock()
	if !v6At.IsZero() && time.Since(v6At) < famCheckTTL {
		return v6OK
	}
	v6OK = HasGlobalIPv6()
	v6At = time.Now()
	return v6OK
}

// HasGlobalIPv4Cached is HasGlobalIPv4 memoized for famCheckTTL, the IPv4 twin
// of HasGlobalIPv6Cached, so "auto" IPv4 mode notices IPv4 appearing or
// vanishing while running.
func HasGlobalIPv4Cached() bool {
	v4Mu.Lock()
	defer v4Mu.Unlock()
	if !v4At.IsZero() && time.Since(v4At) < famCheckTTL {
		return v4OK
	}
	v4OK = HasGlobalIPv4()
	v4At = time.Now()
	return v4OK
}

// HasGlobalIPv4 reports whether the host has a usable IPv4 address (excludes
// loopback and link-local; private NAT addresses count - that's the normal home
// setup). Used to decide whether to probe the IPv4 family at all, so an
// IPv6-only host doesn't dial doomed IPv4 anchors every round.
func HasGlobalIPv4() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP.To4()
		if ip == nil {
			continue
		}
		if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

// HasGlobalIPv6 reports whether the host has a usable global IPv6 address
// (excludes loopback, link-local, and ULA). Used to decide whether to probe
// the IPv6 family at all.
func HasGlobalIPv6() bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipn.IP
		if ip.To4() == nil && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			return true
		}
	}
	return false
}

func (p *Prober) dial(ctx context.Context, t config.Target) TargetResult {
	dctx, cancel := context.WithTimeout(ctx, p.curTimeout())
	defer cancel()
	start := time.Now()
	conn, err := p.dialer.DialContext(dctx, t.Network, t.Address)
	lat := time.Since(start)
	if err != nil {
		return TargetResult{Target: t, OK: false, Latency: lat, Err: err}
	}
	conn.Close()
	return TargetResult{Target: t, OK: true, Latency: lat}
}
