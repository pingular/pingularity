// Package prober determines connectivity by dialing several anchor endpoints
// concurrently and applying a quorum rule, so that a single endpoint flapping
// cannot produce a false outage.
package prober

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
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
// four wire queries instead of two, since network "ip" asks A and AAAA for each.
// Go walks that name list in order and stops at the first name that comes back
// with addresses (the `len(addrs) > 0` break closing goLookupIPCNAMEOrder's loop,
// same file) - so the walk continues past a name that draws NXDOMAIN, which is
// what the random label is meant to draw and what ResolveTime counts as healthy
// below. The doomed second lookup therefore falls inside the window ResolveTime
// times. (A resolver that hijacks NXDOMAIN into an address answer, as ResolveTime
// notes, ends the walk on the first name instead.) The second lookup also hands
// the random probe label, search domain appended, to the recursive resolver the
// host is configured to use. Whether that name travels any further depends on the
// search domain being a delegated zone, which this code cannot know.
//
// TestResolveTimeLooksUpARootedName pins the dot on the name ResolveTime hands
// the resolver, on any host; TestResolveTimeAsksOneNamePerProbe pins the wire
// count, though only a host whose search list contributes a name can fail it;
// TestResolveTimeNameIsFreshEachProbe pins the per-probe label. The A/AAAA pair
// itself is inherent to LookupHost and stays.
func probeName() string {
	var b [6]byte
	_, _ = crand.Read(b[:])
	return "pp" + hex.EncodeToString(b[:]) + "." + dnsProbeDomain + "."
}

// lookupHost is the resolver call ResolveTime makes. It is a var, and a closure
// rather than a method value, so it re-reads net.DefaultResolver on every call:
// that keeps a test's swapped-in resolver effective, and lets a test read the
// exact name string ResolveTime passes, which no wire capture can recover (Go
// roots every name before it reaches the wire).
var lookupHost = func(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, name)
}

// ResolveTime times one DNS lookup of a freshly randomized name (see probeName).
// ok is true when the resolver answered - including the expected NXDOMAIN for the
// random name (DNS is healthy, just no such record) - and false only when
// resolution actually fails (timeout, SERVFAIL, no resolver). On failure it also
// returns the underlying error so the caller can classify/log WHY (via
// DialErrClass); err is nil whenever ok is true.
func ResolveTime(ctx context.Context) (time.Duration, bool, error) {
	rctx, cancel := context.WithTimeout(ctx, dnsProbeTimeout)
	defer cancel()
	name := probeName() // fresh and rooted; see probeName for why the dot matters
	start := time.Now()
	// lookupHost calls DefaultResolver, the host's OS-configured resolver
	// (/etc/resolv.conf or libc) - deliberately NOT the latency anchors and not a
	// Pingularity setting, so this times the resolution path the machine actually
	// uses. A resolver that hijacks NXDOMAIN (returns an A record for the random
	// name) reads as a healthy answer in the err==nil branch below.
	_, err := lookupHost(rctx, name)
	d := time.Since(start)
	if err == nil {
		return d, true, nil
	}
	var de *net.DNSError
	if errors.As(err, &de) && de.IsNotFound {
		return d, true, nil // NXDOMAIN: the resolver answered; the path works
	}
	return d, false, err // timeout / SERVFAIL / no resolver -> DNS unhealthy
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
