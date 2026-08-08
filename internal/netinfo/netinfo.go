// Package netinfo discovers the connection's public identity (IP, ISP, geo) and
// DNS resolver using only keyless public services (no API token). Public
// IPv4/IPv6 come from ipify; ISP is the origin ASN/name from Team Cymru. The
// host's own public IP is geolocated with eyeball providers (ipwho.is, then
// geojs.io), which carry residential addresses; RIPE IPmap handles the
// infrastructure IPs (DNS resolver, router hops) it's built for. The upstream
// resolver is found via a resolver-echo lookup, then labelled the same way.
package netinfo

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// DNSEntry is a single DNS server, who operates it, and where it is.
type DNSEntry struct {
	IP       string `json:"ip"`
	Host     string `json:"host,omitempty"` // reverse-DNS of the egress IP (often carries the service brand, e.g. dns.nextdns.io)
	Provider string `json:"provider"`
	Location string `json:"location,omitempty"`
}

// Info is the full connection snapshot served to the UI.
type Info struct {
	PublicIP           string    `json:"public_ip"`
	PublicIPv6         string    `json:"public_ipv6,omitempty"`
	Hostname           string    `json:"hostname,omitempty"`
	ISP                string    `json:"isp"`
	City               string    `json:"city"`
	Country            string    `json:"country"`
	Lat                float64   `json:"lat,omitempty"` // where the geo provider puts City - a candidate centre for auto server selection (see publicIPGeo)
	Lon                float64   `json:"lon,omitempty"` // 0,0 = the provider named a city but no position; not a coordinate
	DNSUpstream        *DNSEntry `json:"dns_upstream,omitempty"`
	DNSConfigured      []string  `json:"dns_configured,omitempty"`       // resolvers set on the host (resolv.conf), vs the egress above
	DNSConfiguredLabel string    `json:"dns_configured_label,omitempty"` // those resolvers' provider names, deduped (e.g. "NextDNS, Inc."); IPs when un-nameable
	CFColo             string    `json:"cf_colo,omitempty"`              // Cloudflare PoP serving this connection
	Exit               *ExitInfo `json:"exit,omitempty"`                 // ISP exit router / handoff (traceroute)
	ExitUnavailable    string    `json:"exit_unavailable,omitempty"`     // why exit discovery can't run (IPv4-only traceroute; or a platform without a native trace - not Linux/macOS/Windows)
	UpdatedAt          int64     `json:"updated_at"`
	Error              string    `json:"error,omitempty"`
}

// CarriedIdentity reports whether this snapshot's IP/ISP identity is a
// carried-forward last-known (the live IP echo failed) rather than fresh data.
// A snapshot whose only failure is the ISP enrichment lookup ("isp lookup
// failed") is NOT carried - its IP/DNS/colo are current.
func (i Info) CarriedIdentity() bool { return i.Error == "ip lookup failed" }

// Manager fetches and caches the connection info.
type Manager struct {
	http *http.Client
	log  *slog.Logger

	// LastKnownFn, if set, returns a persisted last-known identity (e.g. from
	// speedtest history), used as a fallback when a live lookup fails and the
	// in-memory cache is empty - so ISP/IP/DNS survive restarts and prolonged
	// rate-limiting. nil when nothing is available.
	LastKnownFn func() *Info

	// ExitTargetFn, if set, returns the host/IP the exit-router traceroute aims
	// at (the user-chosen path out). Empty/unresolvable falls back to 1.1.1.1.
	ExitTargetFn func() string

	// EnabledFn gates the AUTOMATIC lookups this Manager makes. Nil = always on.
	// The daemon wires it to "monitoring is on AND connection info is enabled",
	// so a paused monitor makes no third-party requests on its own and the
	// setting can stop them for good. It deliberately does NOT gate RefreshNow:
	// the dashboard's refresh button is an explicit request, and "stop doing this
	// on your own" is not "refuse when I ask".
	EnabledFn func() bool

	// WakeFn, if set, supplies the settings-change wake channel (see the
	// comment on enabled/WakeFn above the Loop).
	WakeFn func() <-chan struct{}

	// nudge (capacity 1) lets Nudge poke Loop into the same re-evaluation a
	// wake triggers, for enable edges that produce NO settings broadcast -
	// the Quick Setup hold expiring 48h after boot is a pure clock event.
	// Routed through Loop rather than calling Refresh directly so the resume
	// one-shot dedupes it: on an install that already refreshed it is a no-op,
	// never a second set of third-party lookups.
	nudge chan struct{}

	mu   sync.RWMutex
	info Info

	// gen and published order overlapping refreshes (background Loop, speedtest,
	// reconnect, manual button) so they publish by START, not by finish. Each
	// refresh claims the next gen at its start (nextGen); an externally-visible
	// effect - m.info, the Exit patch, the IPv4-miss run state below, the IP-change
	// exit-cache bust - commits only while no LATER-started refresh has already
	// published (claimPublish/published). So a slow refresh on a dying network
	// can't clobber a newer snapshot when it finally returns. Atomic so the gate
	// reads coherently under either mu or traceMu with no lock-ordering rule.
	gen       atomic.Uint64
	published atomic.Uint64

	// v4MissSince marks the start of the current run of fetches where the public
	// IPv4 echo failed while IPv6 answered (zero when no such run); v4MissLast is
	// the most recent such observation, used to spot a stale run after a suspend
	// or blackout. Guarded by mu; see fetch's IPv6-only flip rule.
	v4MissSince time.Time
	v4MissLast  time.Time

	// coordTriedIP/coordTriedAt throttle the cached-city COORDINATE recovery:
	// the public IP it last ran for, and when. Without a throttle the recovery
	// never cancels, because publicIPGeo answers ok=true for a city it cannot
	// place (a null latitude, geojs' unparseable strings), leaving the "no
	// coordinate yet" condition still true after the query - a keyless
	// third-party GET per refresh, forever. The same trap egressGeo's per-IP
	// cache exists to avoid, and it is throttled the same way rather than spent
	// once, because "the provider did not answer" is not "the provider cannot
	// place this address": a 429, a timeout or an outage must not disable a
	// load-bearing origin for the life of the IP.
	//
	// Recorded only when the attempt did NOT yield a usable coordinate, so a
	// lookup that succeeded but whose refresh was superseded before it could
	// publish (see claimPublish) leaves the next fetch free to ask again.
	// Guarded by mu.
	coordTriedIP string
	coordTriedAt time.Time

	// egressGeo caches the DNS-egress location per IP (an IP's geo doesn't move),
	// so a flipping/rotating egress doesn't re-hit rate-limited RIPE IPmap or
	// flicker. See egressLocation.
	egressMu  sync.Mutex
	egressGeo map[string]*egressGeoEntry

	// Exit discovery is traceroute-based (seconds), so it caches separately
	// rather than rerunning on every netinfo refresh (see cachedExit). tracing
	// is non-nil while a discovery is in flight, so concurrent callers dedupe
	// onto it (single-flight) instead of starting a second trace.
	traceMu      sync.Mutex
	traceAt      time.Time
	exit         *ExitInfo
	tracedFor    string // ExitTargetFn() value the cached exit answers for (success only)
	attemptedFor string // ExitTargetFn() value of the last completed attempt (success or failure)
	tracing      chan struct{}

	// traceGen counts deliberate exit-cache invalidations (an IP-change bust in
	// refresh, or a manual RefreshNow). A trace captures it when it claims the
	// single-flight slot; if it changes before the trace commits, a bust landed
	// mid-trace and the result is from the OLD network/target, so cachedExit drops
	// it instead of overwriting the bust and re-stamping it fresh. Guarded by traceMu.
	traceGen uint64
}

// NewManager builds a Manager.
func NewManager(log *slog.Logger) *Manager {
	return &Manager{
		// Every netinfo fetch hits a direct JSON/text endpoint over HTTPS; refuse
		// redirects so a 3xx can't bounce a lookup to http:// or another host.
		http: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		nudge: make(chan struct{}, 1),
		log:   log,
	}
}

// Get returns the most recently fetched info (zero value until first refresh).
func (m *Manager) Get() Info {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.info
}

// Refresh fetches fresh info and caches it. Fast fields are published first;
// the slow exit traceroute is resolved afterward and patched in, so the
// Connection panel isn't blank for the duration of the trace.
//
// This is the AUTOMATIC path - the background loop, and the speedtest and
// reconnect refreshes - so it obeys EnabledFn and makes no request when
// automatic lookups are off. Gating here rather than at each call site means a
// new automatic caller is gated by default; forcing is the explicit route below.
func (m *Manager) Refresh(ctx context.Context) { m.refresh(ctx, false) }

// refresh does the work. force is for a lookup the operator asked for by hand,
// which is allowed even when automatic lookups are off - "stop doing this on
// your own" is not "refuse when I click the button". Off means the last-known
// info stands rather than being blanked: a paused monitor should stop looking,
// not forget what it already knew.
func (m *Manager) refresh(ctx context.Context, force bool) {
	if !force && !m.enabled() {
		return
	}
	gen := m.nextGen()
	prevIP := m.Get().PublicIP
	i := m.fetchGen(ctx, gen)
	// A changed public IP is a changed network: the last known exit answers for
	// the OLD path, so do not carry it into this snapshot - publish with no exit
	// (row hidden) until the retrace below lands on the new network. Carrying it
	// forward and relying on the bust alone left the old router on display
	// whenever the retrace failed, because a nil retrace patches nothing.
	ipChanged := prevIP != "" && i.PublicIP != "" && prevIP != i.PublicIP
	if i.ExitUnavailable == "" && !ipChanged {
		i.Exit = m.snapshotExit() // carry the last known exit forward (no blank flicker)
	}
	m.mu.Lock()
	// Publish only while this remains the newest-started refresh: a slower, older
	// one that returns after a newer snapshot already landed must not overwrite it.
	if m.claimPublish(gen) {
		m.info = i
		markFresh(i)
	}
	m.mu.Unlock()
	m.log.Info("netinfo refreshed", "ip", i.PublicIP, "isp", i.ISP)

	if i.ExitUnavailable != "" {
		return // the IPv4-only traceroute can't run here - don't burn a doomed trace
	}
	if i.CarriedIdentity() {
		return // a carried-forward ASN must not drive the boundary walk
	}
	// A changed public IP is a changed network (hotspot, new Wi-Fi): the cached
	// exit answers for the OLD path, so bust it instead of serving it for up to
	// the cache window.
	if ipChanged {
		m.traceMu.Lock()
		// Only bust while this is still the newest published snapshot: an older,
		// slower refresh detecting a now-superseded IP change must not wipe the
		// exit cache a newer generation just established.
		if m.published.Load() == gen {
			m.traceAt = time.Time{}
			// Drop the cached VALUE too, not just its freshness stamp. cachedExit
			// deliberately keeps the last-known exit when a trace fails, so the row
			// does not flicker out on a silent hop - but that rule is for a failure
			// on the SAME network. This is a confirmed network change, and with the
			// target setting unchanged that rule would keep the OLD network's router
			// on display and re-stamp it fresh every cache window, so it would never
			// recover. An empty Exit row is correct here; a wrong one is not.
			m.exit, m.tracedFor = nil, ""
			// Invalidate any trace already in flight (started on the OLD network),
			// so its result can't overwrite this bust when it lands (cachedExit).
			m.traceGen++
		}
		m.traceMu.Unlock()
	}
	// Run the (multi-second) exit traceroute now that the fast fields are
	// visible, then patch its result into the published snapshot - but only if
	// the snapshot is still the one this Refresh produced. A concurrent Refresh
	// may have published a newer one (possibly flipped IPv6-only with
	// ExitUnavailable set), and a stale IPv4-path exit must not be written into
	// it. The generation check is exact, unlike the old second-granularity
	// UpdatedAt compare that could alias two refreshes in the same second.
	if ex := m.cachedExit(ctx, asnFromOrg(i.ISP)); ex != nil {
		m.mu.Lock()
		if m.published.Load() == gen {
			m.info.Exit = ex
		}
		m.mu.Unlock()
	}
}

// markFresh stamps the freshness gauge for a published snapshot whose lookup
// chain succeeded. netinfo.last_ok_ts is the /metrics answer to "is the
// Connection panel stale?" - a box whose lookups keep failing serves old
// ISP/exit data indistinguishably without it (the counters say lookups fail,
// only the timestamp says since when). Matches dns.last_ok_ts's convention.
func markFresh(i Info) {
	if i.Error == "" {
		stats.Set("netinfo.last_ok_ts", time.Now().Unix())
	}
}

// nextGen claims this refresh's generation at its start. A later-started refresh
// gets a higher number and so outranks an earlier one regardless of finish order.
func (m *Manager) nextGen() uint64 { return m.gen.Add(1) }

// coordRetryInterval is how long a fruitless coordinate recovery stands before
// it is worth asking again. It matches egressLocRevalidate's reasoning: an IP's
// placement does not move, so re-asking buys nothing on the timescale of a
// fetch, while never re-asking turns one bad minute at a provider into a
// permanently missing origin.
const coordRetryInterval = time.Hour

// coordRetryDue reports whether the coordinate recovery for ip may run now.
// Read-only: the attempt is recorded by noteCoordMiss, and only when it failed
// to produce a coordinate, so nothing is spent before its outcome is known.
func (m *Manager) coordRetryDue(ip string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.coordTriedIP != ip || time.Since(m.coordTriedAt) >= coordRetryInterval
}

// noteCoordMiss records that a recovery for ip ran and did not come back with a
// usable coordinate - whether because the providers place the address nowhere,
// or because they did not answer at all. Both are retried on the interval; the
// difference between them is not observable from here, and treating the second
// as permanent is what made a transient outage disable the ISP origin for good.
func (m *Manager) noteCoordMiss(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.coordTriedIP, m.coordTriedAt = ip, time.Now()
}

// claimPublish reports whether a refresh at generation g may still commit an
// externally-visible effect - true only while no later-started refresh has
// already published. On true it records g as the newest published generation.
// Idempotent, so a refresh's several effects (m.info, then the Exit patch) all
// pass while it remains current.
func (m *Manager) claimPublish(g uint64) bool {
	for {
		cur := m.published.Load()
		if cur > g {
			return false // a newer generation already published
		}
		if cur == g || m.published.CompareAndSwap(cur, g) {
			return true
		}
	}
}

// RefreshNow forces a full refresh - including re-running exit discovery, which
// is otherwise cached for minutes - and returns the fresh info. Backs the
// dashboard's manual refresh button, so it FORCES past EnabledFn: the button is
// an explicit request, and the setting only stops lookups happening on their own.
func (m *Manager) RefreshNow(ctx context.Context) Info {
	m.traceMu.Lock()
	m.traceAt = time.Time{} // bust the exit cache so the traceroute re-runs
	m.traceGen++            // and drop any trace already in flight, so a stale in-flight result can't be served for the re-run
	m.traceMu.Unlock()
	m.refresh(ctx, true)
	// A manual refresh often follows a network change (new Wi-Fi, tethering),
	// and the first fetch can land while the link is still settling - DNS not
	// answering yet, the old route half-dead. One short-delay retry absorbs
	// that window instead of bouncing an error at the person who just clicked
	// Refresh; the background Loop keeps its own slower error cadence.
	if m.Get().Error != "" {
		select {
		case <-ctx.Done():
		case <-time.After(refreshRetryDelay):
			// Re-bust the exit cache too: the first attempt's FAILED trace was
			// cached (attemptedFor/traceAt stamp on failure), and the retry must
			// honor RefreshNow's re-run-exit-discovery contract, not serve it.
			m.traceMu.Lock()
			m.traceAt = time.Time{}
			m.traceGen++
			m.traceMu.Unlock()
			m.refresh(ctx, true)
		}
	}
	return m.Get()
}

// refreshRetryDelay is how long a failed manual refresh waits before its single
// in-request retry: long enough for a just-switched network to settle, short
// enough that the Refresh button still feels responsive. Var so tests can
// shrink it.
var refreshRetryDelay = 2 * time.Second

// enabled reports whether connection-info lookups may run. Nil means yes, so a
// Manager built without the hook (tests, callers that always want it) behaves
// as it always did.
func (m *Manager) enabled() bool { return m.EnabledFn == nil || m.EnabledFn() }

// Nudge pokes Loop into the same immediate re-evaluation a settings wake
// causes. Safe from any goroutine; a pending nudge already covers it.
func (m *Manager) Nudge() {
	select {
	case m.nudge <- struct{}{}:
	default:
	}
}

// WakeFn, if set, returns a channel closed when settings change, so Loop
// re-evaluates enabled() the moment it flips instead of at the next
// minute tick. What it buys is the resume one-shot firing IMMEDIATELY at
// power-on: a fresh install's first speedtest auto-selects its server from the
// cities netinfo names, and before this wake the ten-second consent run raced
// a refresh that had up to a minute of poll latency before it even started -
// so the first selection fell back to the Ookla API's placement of the source
// address (speed.cityrace_unanchored) while every later run raced real
// origins. Wakes are cheap: no network call happens unless the resume edge or
// staleness says so.

// Loop refreshes immediately, then acts as a staleness backstop: it refreshes
// only when the cached info exceeds maxStale. Speedtests, reconnects, and manual
// refreshes are the primary drivers (connection info rarely changes except
// alongside a reconnect); this just guarantees the data never sits staler than
// maxStale when none of those fire - e.g. speedtests disabled or set longer than
// maxStale.
func (m *Manager) Loop(ctx context.Context, maxStale time.Duration) {
	// Evaluate-then-sleep, subscribing FIRST each round. Changed() hands out a
	// channel the next broadcast closes and replaces, so the channel must be
	// fetched BEFORE any state read the sleep depends on: fetched after, a
	// broadcast landing between read and fetch is lost - the loop would sleep
	// its full minute with an enable pending, which is exactly the latency the
	// wake exists to remove (the consent speedtest's bounded readiness wait is
	// shorter than that minute). Evaluating at the top also covers the
	// pre-loop window the old boot-time refresh had: iteration one IS the boot
	// refresh (resumed starts false).
	//
	// resumed guards the resume one-shot: refreshing the moment lookups are
	// allowed again is what keeps the Connection panel from showing stale data
	// after an unpause, and the one-shot keeps an already-running Manager from
	// being refreshed twice.
	resumed := false
	for {
		var wake <-chan struct{}
		if m.WakeFn != nil {
			wake = m.WakeFn()
		}
		// Staleness cap: a failed fetch still stamps UpdatedAt (it carried
		// last-known data forward), so after an error staleness is capped at
		// errRetryStale - otherwise a transient 429 would suppress any retry
		// for a full maxStale.
		stale := maxStale
		if m.Get().Error != "" && stale > errRetryStale {
			stale = errRetryStale
		}
		switch {
		case !m.enabled():
			// Off: make no network call, and arm the one-shot so the next
			// enabled evaluation refreshes immediately however fresh the cache
			// looks.
			resumed = false
		case !resumed:
			resumed = true
			m.Refresh(ctx)
		case m.age() >= stale:
			m.Refresh(ctx)
		}
		// Sleep until the data would reach the cap. If something else
		// refreshed it meanwhile, UpdatedAt advanced and the next evaluation
		// is a no-op, so the next sleep recomputes from the newer time.
		wait := stale - m.age()
		if wait < time.Minute {
			wait = time.Minute
		}
		// While lookups are off the timer is only a backstop (the wake and
		// nudge are the real resume signals) - but keep it at a minute so a
		// missed edge still recovers quickly.
		if !m.enabled() {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
			// Settings changed - re-evaluate. An unrelated save is a no-op
			// (enabled and fresh); the resume one-shot is what makes power-on
			// refresh NOW.
		case <-m.nudge:
			// A broadcast-less enable edge (see Nudge) - same re-evaluation.
		case <-time.After(wait):
		}
	}
}

// errRetryStale caps staleness before Loop retries after a failed fetch (vs. the
// much longer maxStale for healthy data).
const errRetryStale = 5 * time.Minute

// v6FlipAfter is how long the public IPv4 echo must keep failing - while IPv6
// keeps answering - before a host with a prior IPv4 identity is treated as
// genuinely IPv6-only instead of suffering a transient IPv4 blip. Failed fetches
// retry every errRetryStale, so the flip lands a few fetches after a permanent
// IPv4 loss.
const v6FlipAfter = 15 * time.Minute

// v4MissRunGap bounds the gap between consecutive IPv4-miss observations for
// them to count as one continuous run. Miss snapshots carry an Error, so Loop
// retries at errRetryStale; a gap past twice that means fetches stopped
// (suspend, outage) and the run is stale evidence - it restarts rather than
// letting a single post-recovery miss flip a dual-stack host to IPv6-only.
const v4MissRunGap = 2 * errRetryStale

// ptrLookup (reverse DNS) and resolverEgress (the recursive resolver's egress
// IP) are package vars so tests can stub them, like ipv4Client/ipv6Client.
var (
	ptrLookup      = rdns
	resolverEgress = resolverEgressIP
)

// age reports time since the cached info was last refreshed (very large when
// never refreshed).
func (m *Manager) age() time.Duration {
	u := m.Get().UpdatedAt
	if u == 0 {
		return time.Duration(1) << 60
	}
	d := time.Since(time.Unix(u, 0))
	if d < 0 {
		// A future UpdatedAt (the clock stepped back after the fetch) would make
		// Loop wait out the whole skew before refreshing; treat it as fully stale
		// so the next tick refreshes now.
		return time.Duration(1) << 60
	}
	return d
}

// fetch gathers the fast connection fields under a fresh generation. It backs
// direct callers (tests); the refresh path calls fetchGen with the generation it
// already claimed so the miss-state gate lines up with its later publishes.
func (m *Manager) fetch(ctx context.Context) Info { return m.fetchGen(ctx, m.nextGen()) }

// fetchGen gathers the fast connection fields (public IP/ISP/geo, IPv6, DNS
// resolver, Cloudflare PoP) concurrently - independent round-trips, so the
// refresh is bounded by the slowest single lookup, not their sum. The slow exit
// traceroute is handled separately by Refresh so it can't hold up publishing.
// gen is this fetch's refresh generation: it gates the IPv4-miss run-state write
// so an out-of-order older fetch can't corrupt the run a newer one advanced.
func (m *Manager) fetchGen(ctx context.Context, gen uint64) Info {
	prev := m.Get() // last-good snapshot, to carry forward on transient failures
	info := Info{UpdatedAt: time.Now().Unix()}

	var (
		ip4     string // public IPv4 (ipify); ISP/geo/hostname derived below
		ip4ISP  string // Team Cymru: origin ASN -> AS name
		ip4Host string // reverse DNS of ip4
		ip4City string
		ip4Ctry string
		ip4Lat  float64 // the geo provider's coordinate for ip4City (0,0 = none)
		ip4Lon  float64
		v6      string
		colo    string
		dns     *DNSEntry
	)
	var wg sync.WaitGroup
	wg.Add(4)
	// Public IPv4, then its ISP (Cymru) / geo (eyeball providers) / hostname (rDNS).
	go func() {
		defer wg.Done()
		ip4 = publicIPv4(ctx)
		if ip4 == "" {
			return
		}
		// Reuse cached ISP/geo/hostname while the IP is unchanged - they move only
		// when the IP does, so the Cymru/geo/rDNS lookups run rarely. Geo and rDNS
		// re-run while blank: a provider outage at the IP-change moment must not
		// lock in an empty city/hostname until the next IP change.
		//
		// A city with NO COORDINATE gets ONE recovery lookup, because the
		// coordinate is load-bearing now (it anchors the ISP city in auto server
		// selection) and there are ordinary ways to cache one without it: a geo
		// provider that names a city but returns no position, and - structurally
		// - the persisted last-known identity, which is built from a speed sample
		// and so has no coordinate to carry at all. Without any retry, one failed
		// IP echo publishes a city at 0,0 and the ISP city is dropped from the
		// race for the life of that IP. Bounded rather than repeated (see
		// claimCoordRetry): an answer that names a city and cannot place it is
		// the provider's final word, and re-asking it every fetch would buy
		// nothing at a keyless third party's expense. A still-blank CITY keeps
		// retrying unbounded as before - || short-circuits, so it never spends
		// the claim - because a blank city means the lookup did not land at all.
		if prev.PublicIP == ip4 && prev.ISP != "" {
			ip4ISP, ip4Host = prev.ISP, prev.Hostname
			ip4City, ip4Ctry = prev.City, prev.Country
			ip4Lat, ip4Lon = prev.Lat, prev.Lon
			if ip4City == "" || (ip4Lat == 0 && ip4Lon == 0 && m.coordRetryDue(ip4)) {
				city, ctry, la, lo, ok := publicIPGeo(ctx, m, ip4)
				if ok {
					ip4City, ip4Ctry, ip4Lat, ip4Lon = city, ctry, la, lo
				}
				if la == 0 && lo == 0 {
					m.noteCoordMiss(ip4)
				}
			}
			if ip4Host == "" {
				ip4Host = ptrLookup(ctx, ip4)
			}
			return
		}
		// Team Cymru origin ASN + name as "AS#### Name" (the conventional whois
		// AS-org form) so asnFromOrg (exit discovery, below) keeps working.
		if asn := cymruASN(ctx, ip4); asn != "" {
			ip4ISP = "AS" + asn
			if name := cymruASNName(ctx, asn); name != "" {
				ip4ISP += " " + name
			}
		}
		if city, ctry, la, lo, ok := publicIPGeo(ctx, m, ip4); ok {
			ip4City, ip4Ctry, ip4Lat, ip4Lon = city, ctry, la, lo
		}
		// rdns has its own 1.5s timeout. ctx has no deadline on the Loop or
		// post-speedtest paths, so without it a blackholed reverse zone would
		// stall the whole fetch at wg.Wait() - right at the reconnect/IP-change
		// moment this cache-miss branch runs.
		ip4Host = ptrLookup(ctx, ip4)
	}()
	// Public IPv6 (independent).
	go func() { defer wg.Done(); v6 = publicIPv6(ctx) }()
	// Cloudflare PoP (independent).
	go func() { defer wg.Done(); colo = m.cfColo(ctx) }()
	// Upstream DNS resolver: egress IP, then its provider/geo.
	go func() {
		defer wg.Done()
		eip := resolverEgress(ctx)
		if eip == "" {
			return
		}
		e := &DNSEntry{IP: eip, Provider: "unknown"}
		// Reuse the cached provider/host while the resolver IP is unchanged - they
		// change only when the egress IP does. Provider is Team Cymru (origin ASN ->
		// name); host is reverse-DNS (the service brand the egress network operator
		// hides, e.g. dns.nextdns.io behind AS-VULTR). Location is handled by
		// egressLocation, which keeps its own per-IP cache + IPmap/Cymru fallback.
		if prev.DNSUpstream != nil && prev.DNSUpstream.IP == eip && prev.DNSUpstream.Provider != "" && prev.DNSUpstream.Provider != "unknown" {
			e.Provider, e.Host = prev.DNSUpstream.Provider, prev.DNSUpstream.Host
			if e.Host == "" {
				e.Host = ptrLookup(ctx, eip) // rDNS failed before; retry while blank so a one-off timeout doesn't drop the brand for the process life
			}
		} else {
			if p := dnsProvider(ctx, eip); p != "" {
				e.Provider = p
			}
			e.Host = ptrLookup(ctx, eip)
		}
		e.Location = m.egressLocation(ctx, eip)
		dns = e
	}()
	wg.Wait()

	// Track how long the IPv4 echo has been failing while IPv6 keeps answering -
	// the signal that a host with an IPv4 history really moved to an IPv6-only
	// network. An IPv4 answer ends the run; a fetch where neither family answers
	// is a blackout, not evidence, and leaves the run untouched. The flip needs
	// SUSTAINED evidence: when the gap since the previous miss observation
	// exceeds v4MissRunGap (fetches stopped - suspend or outage), the run is
	// stale and restarts, so one post-recovery miss (IPv6 RA up while DHCPv4 is
	// still negotiating) can't flip the identity on its own. Once flipped (prev
	// has no IPv4) misses arrive at the slow healthy cadence, so the run is kept
	// and the IPv6-only identity doesn't flap.
	m.mu.Lock()
	// Fold this fetch's IPv4-miss observation into the run tracker - but only while
	// this generation may still publish, so a slower, older fetch's write can't
	// corrupt a run a newer one already advanced (the generation contract; see
	// refresh). The v4LostFor read below is unconditional: it only feeds this
	// snapshot's own flip decision, which is itself gated at publish.
	if m.claimPublish(gen) {
		switch {
		case ip4 != "":
			m.v4MissSince, m.v4MissLast = time.Time{}, time.Time{}
		case v6 != "":
			now := time.Now()
			if m.v4MissSince.IsZero() || (prev.PublicIP != "" && now.Sub(m.v4MissLast) > v4MissRunGap) {
				m.v4MissSince = now
			}
			m.v4MissLast = now
		}
	}
	var v4LostFor time.Duration
	if !m.v4MissSince.IsZero() {
		v4LostFor = time.Since(m.v4MissSince)
	}
	m.mu.Unlock()

	// lastKnown is the persisted fallback (speed history), fetched at most once
	// and only when something is missing.
	var lastKnown *Info
	getLast := func() *Info {
		if lastKnown == nil && m.LastKnownFn != nil {
			lastKnown = m.LastKnownFn()
		}
		return lastKnown
	}
	// persistedHasIPv4 reports whether the speed-history fallback carries a prior
	// public IPv4 - proof this host is really dual-stack, so a current IPv4 miss
	// is a transient blip rather than a genuine IPv6-only network.
	persistedHasIPv4 := func(lk *Info) bool { return lk != nil && lk.PublicIP != "" }

	switch {
	case ip4 != "":
		info.PublicIP, info.Hostname, info.ISP = ip4, ip4Host, ip4ISP
		info.City, info.Country = ip4City, ip4Ctry
		info.Lat, info.Lon = ip4Lat, ip4Lon
		// IP echo succeeded but the Cymru ISP lookup failed (blank ISP): fill it
		// from speed history for this same IP, and if still blank flag the snapshot
		// so Loop retries at the faster errRetryStale cadence instead of waiting a
		// full maxStale hour - and so a blank-ISP row doesn't shadow a good one.
		if info.ISP == "" {
			if lk := getLast(); lk != nil && lk.PublicIP == ip4 && lk.ISP != "" {
				info.ISP = lk.ISP
			}
			if info.ISP == "" {
				info.Error = "isp lookup failed"
			}
		}
	case v6 != "" && (v4LostFor >= v6FlipAfter || (prev.PublicIP == "" && !persistedHasIPv4(getLast()))):
		// IPv6-only host. Two ways in: no IPv4 identity exists anywhere - neither
		// in memory nor in the persisted speed history - or a prior IPv4 identity
		// exists but the IPv4 echo has now been failing, with IPv6 still
		// answering, for v6FlipAfter (the host moved to an IPv6-only network).
		// The time bound is the veto that keeps a transient IPv4 blip on a
		// dual-stack host (boot/PPPoE still negotiating IPv4 while IPv6 RA is up,
		// or a one-off echo failure) from flipping the identity. Derive
		// ISP/geo/hostname from the IPv6 (Cymru's origin6 zone; the geo providers
		// take IPv6) - a healthy snapshot, not an error, so Loop keeps its cadence.
		if prev.PublicIPv6 == v6 && prev.PublicIP == "" {
			// Reuse the cache only when prev was itself IPv6-derived: at the flip
			// moment prev's ISP/geo/hostname came from the dead IPv4.
			info.Hostname, info.ISP = prev.Hostname, prev.ISP
			info.City, info.Country = prev.City, prev.Country
			info.Lat, info.Lon = prev.Lat, prev.Lon
		}
		if info.ISP == "" {
			if asn := cymruASN(ctx, v6); asn != "" {
				info.ISP = "AS" + asn
				if name := cymruASNName(ctx, asn); name != "" {
					info.ISP += " " + name
				}
			}
		}
		// Cymru failed (still-blank ISP): mirror the ip4 branch - fill it from
		// speed history for this same IPv6, and if still blank flag the snapshot
		// so Loop retries at errRetryStale instead of waiting a full maxStale hour.
		if info.ISP == "" {
			if lk := getLast(); lk != nil && lk.PublicIPv6 == v6 && lk.ISP != "" {
				info.ISP = lk.ISP
			}
			if info.ISP == "" {
				info.Error = "isp lookup failed"
			}
		}
		// Same rule as the IPv4 branch, bounded the same way: a cached city with
		// no coordinate is not a complete answer, because the coordinate anchors
		// the ISP city in auto server selection - but one lookup settles whether
		// this address can be placed at all.
		if info.City == "" || (info.Lat == 0 && info.Lon == 0 && m.coordRetryDue(v6)) {
			city, ctry, la, lo, ok := publicIPGeo(ctx, m, v6)
			if ok {
				info.City, info.Country = city, ctry
				info.Lat, info.Lon = la, lo
			}
			if la == 0 && lo == 0 {
				m.noteCoordMiss(v6)
			}
		}
		if info.Hostname == "" {
			info.Hostname = ptrLookup(ctx, v6)
		}
		// The exit traceroute is IPv4-only; record the reason. ExitUnavailable is
		// internal/API-visible only - it stops Refresh from re-running the doomed
		// trace; the dashboard hides the Exit row entirely when Exit is empty.
		info.ExitUnavailable = "needs IPv4 (this host is IPv6-only)"
	default:
		// Don't blank the panel on a transient failure - keep the last-known
		// identity: in-memory first, then the persisted speed-history fallback.
		info.Error = "ip lookup failed"
		src := prev
		if src.ISP == "" && src.PublicIP == "" {
			if lk := getLast(); lk != nil {
				src = *lk
			}
		}
		info.PublicIP, info.Hostname, info.ISP = src.PublicIP, src.Hostname, src.ISP
		info.City, info.Country = src.City, src.Country
		info.Lat, info.Lon = src.Lat, src.Lon
	}
	// Carry forward fields whose empty value only means "lookup failed" (not a
	// real state), so a transient failure doesn't vanish a row. IPv6 is excluded:
	// "" there legitimately means "no IPv6". Both fallbacks copy the entry:
	// prev.DNSUpstream aliases the pointer inside the published snapshot (Get
	// copies the struct, not the entry), so mutating it here would race readers.
	if dns == nil {
		if prev.DNSUpstream != nil { // resolver egress lookup failed
			cp := *prev.DNSUpstream
			dns = &cp
		} else if lk := getLast(); lk != nil && lk.DNSUpstream != nil {
			cp := *lk.DNSUpstream
			dns = &cp
		}
	}
	// Live resolver IP known but a sub-lookup failed: fill the missing field from
	// speed history if that entry is for the same resolver IP. Provider (Cymru) and
	// location (egressLocation, which may be empty during its grace window) fill
	// independently - so a 429 on one doesn't blank the other.
	if dns != nil {
		if dns.Provider == "" || dns.Provider == "unknown" {
			if lk := getLast(); lk != nil && lk.DNSUpstream != nil && lk.DNSUpstream.IP == dns.IP && lk.DNSUpstream.Provider != "" {
				dns.Provider = lk.DNSUpstream.Provider
			}
		}
		if dns.Location == "" {
			if lk := getLast(); lk != nil && lk.DNSUpstream != nil && lk.DNSUpstream.IP == dns.IP && lk.DNSUpstream.Location != "" {
				dns.Location = lk.DNSUpstream.Location
			}
		}
	}
	if colo == "" {
		colo = prev.CFColo // Cloudflare /cdn-cgi/trace failed
	}
	// Exit discovery is traceroute-based and runs on Linux, macOS, and Windows; on
	// platforms without a native trace, record why (like the IPv6-only case above).
	// ExitUnavailable is internal/API-visible only - it short-circuits Refresh's
	// doomed trace instead of climbing trace_fail forever; the dashboard hides the
	// Exit row entirely. A more specific reason already set (IPv6-only) wins.
	if !traceSupported && info.ExitUnavailable == "" {
		info.ExitUnavailable = "exit discovery is unsupported on this platform"
	}
	info.PublicIPv6 = v6 // "" when no IPv6
	info.DNSUpstream = dns
	// Configured resolvers (local read) labelled with their provider names, deduped.
	// Naming needs Cymru lookups, so reuse the label while the configured set holds -
	// but retry while a public resolver is still labelled by its bare IP (its naming
	// lookup failed), so a Cymru hiccup at startup doesn't stick for the process life.
	configured := configuredResolvers()
	cfgLabel := prev.DNSConfiguredLabel
	if cfgLabel == "" || !equalStrings(configured, prev.DNSConfigured) || labelNeedsRetry(cfgLabel) {
		cfgLabel = configuredResolverLabel(ctx, configured)
	}
	info.DNSConfigured = configured
	info.DNSConfiguredLabel = cfgLabel
	info.CFColo = colo
	dnsIP := ""
	if dns != nil {
		dnsIP = dns.IP
	}
	m.log.Debug("netinfo lookup", "ip", info.PublicIP, "ipv6", v6, "isp", info.ISP,
		"host", info.Hostname, "colo", colo, "dns", dnsIP)
	return info
}

// ipv6Client forces outbound requests over IPv6. Built once (reused) so repeated
// lookups don't leak a fresh Transport's idle connections each time. Refuses
// redirects for the same reason NewManager's client does: a 3xx must not bounce
// the echo lookup to http:// or another host.
var ipv6Client = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp6", addr)
		},
	},
}

// ipv4Client forces outbound requests over IPv4 (built once and reused, like
// ipv6Client, so repeated lookups don't leak idle connections; same no-redirect
// policy).
var ipv4Client = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp4", addr)
		},
	},
}

// publicIPv4 returns this host's public IPv4 (via ipify, forced over IPv4), or
// "" on failure. ISP/geo/hostname are derived from it via Team Cymru, the
// eyeball geo providers (publicIPGeo), and reverse DNS.
func publicIPv4(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return ""
	}
	resp, err := ipv4Client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if p := net.ParseIP(ip); p == nil || p.To4() == nil {
		return "" // not a valid IPv4 address
	}
	return ip
}

// publicIPv6 returns the host's public IPv6 address by forcing an outbound IPv6
// request to an echo service, or "" if IPv6 is unavailable.
func publicIPv6(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api64.ipify.org", nil)
	if err != nil {
		return ""
	}
	resp, err := ipv6Client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if p := net.ParseIP(ip); p == nil || p.To4() != nil {
		return "" // not a valid IPv6 address
	}
	return ip
}

// resolverEgressIP returns the public egress IP of the recursive resolver
// actually serving this host, via the well-known akamai echo record.
func resolverEgressIP(ctx context.Context) string {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(c, "whoami.akamai.net")
	if err != nil || len(addrs) == 0 {
		return ""
	}
	for _, a := range addrs { // prefer IPv4
		if strings.Contains(a, ".") {
			return a
		}
	}
	return addrs[0]
}

// configuredResolvers returns the DNS nameservers the host is set to use. The
// raw list comes from a per-OS source (rawResolvers, build-tagged); the shared
// filtering below is identical on every platform. Empty on a host we can't read.
func configuredResolvers() []string {
	return filterResolvers(rawResolvers())
}

// filterResolvers drops loopback stubs (e.g. systemd-resolved's 127.0.0.53) and
// duplicates from the raw nameserver strings, preserving order. Loopback entries
// carry no useful identity and the egress IP already shows where queries exit.
func filterResolvers(ns []string) []string {
	out := make([]string, 0, len(ns))
	seen := map[string]bool{}
	for _, n := range ns {
		if seen[n] {
			continue
		}
		if ip := net.ParseIP(n); ip != nil && !ip.IsLoopback() {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// configuredResolverLabel labels the host's configured resolvers with their
// network-operator names (Team Cymru ASN org, cleaned), deduped - so a single
// provider's primary+secondary collapse to one name and a mixed config lists each.
// Resolvers with no public ASN (router/private) fall back to their IP. " + "-joined.
func configuredResolverLabel(ctx context.Context, ips []string) string {
	if len(ips) == 0 {
		return ""
	}
	// Phase 1: each IP's origin ASN, concurrently (configured resolvers are few).
	asns := make([]string, len(ips))
	var wg sync.WaitGroup
	for i, ip := range ips {
		wg.Add(1)
		go func(i int, ip string) { defer wg.Done(); asns[i] = cymruASN(ctx, ip) }(i, ip)
	}
	wg.Wait()
	// Name each distinct public ASN once.
	nameByASN := map[string]string{}
	for _, asn := range asns {
		if asn != "" {
			if _, ok := nameByASN[asn]; !ok {
				nameByASN[asn] = asnDisplayName(cymruASNName(ctx, asn))
			}
		}
	}
	// Phase 2: a local resolver with no public name (a LAN Pi-hole / router /
	// Unbound) - fingerprint its software via a CHAOS version.bind probe, concurrently.
	sw := make([]string, len(ips))
	var wg2 sync.WaitGroup
	for i, ip := range ips {
		if asns[i] != "" && nameByASN[asns[i]] != "" {
			continue // already named by its ASN org
		}
		if a := net.ParseIP(ip); a == nil || !(a.IsPrivate() || a.IsLinkLocalUnicast()) {
			continue // only probe local/LAN resolvers
		}
		wg2.Add(1)
		go func(i int, ip string) { defer wg2.Done(); sw[i] = resolverSoftware(ctx, ip) }(i, ip)
	}
	wg2.Wait()
	// Deduped labels: public ASN org > local software fingerprint > bare IP.
	var labels []string
	seen := map[string]bool{}
	for i, ip := range ips {
		label := ip
		if name := nameByASN[asns[i]]; asns[i] != "" && name != "" {
			label = name
		} else if sw[i] != "" {
			label = sw[i]
		}
		if !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	return strings.Join(labels, " + ")
}

// resolverSoftware probes a local resolver via a CHAOS-class "version.bind" query
// and maps the reported version to a friendly software name (Pi-hole's FTL reports
// "dnsmasq-pi-hole-vX.Y"; also dnsmasq, Unbound, AdGuard Home, ...). "" when it
// doesn't answer or isn't recognised. One extra UDP query, to a resolver the host
// already uses.
func resolverSoftware(ctx context.Context, ip string) string {
	return softwareFromVersion(chaosVersionBind(ctx, ip))
}

// chaosVersionBind sends a CHAOS-class TXT "version.bind" query to ip:53 over UDP
// and returns the answer string ("" on timeout / refusal / parse failure).
func chaosVersionBind(ctx context.Context, ip string) string {
	// Fixed query: 12-byte header (RD set, one question) + the version.bind
	// question. parseChaosTXT validates the response against this same id/question.
	q := append([]byte{
		chaosQueryID >> 8, chaosQueryID & 0xff, // ID
		0x01, 0x00, // flags: RD
		0x00, 0x01, // QDCOUNT
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // AN/NS/AR counts
	}, chaosVersionQuestion...)
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(c, "udp", net.JoinHostPort(ip, "53"))
	if err != nil {
		return ""
	}
	defer conn.Close()
	if dl, ok := c.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(q); err != nil {
		return ""
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return ""
	}
	return parseChaosTXT(buf[:n])
}

// chaosQueryID is the fixed DNS transaction ID chaosVersionBind sends; a genuine
// response must echo it. chaosVersionQuestion is the query's question section
// (QNAME "version.bind", QTYPE TXT 0x0010, QCLASS CHAOS 0x0003) - appended to the
// header to form the query, and checked byte-for-byte against the response.
const chaosQueryID = 0x1234

var chaosVersionQuestion = []byte{
	7, 'v', 'e', 'r', 's', 'i', 'o', 'n', 4, 'b', 'i', 'n', 'd', 0,
	0x00, 0x10, // QTYPE = TXT
	0x00, 0x03, // QCLASS = CHAOS
}

// parseChaosTXT extracts the first TXT answer string from a response to our fixed
// version.bind query, after checking the response actually answers THAT query -
// its transaction ID, QR bit, and question must match what we sent, so a stray
// datagram (a late reply to another query on this ephemeral port) or a spoofed one
// isn't trusted as the resolver's answer. Beyond that it stays lenient: the peer
// is a resolver the host already uses. "" when there's no usable answer.
func parseChaosTXT(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	if int(b[0])<<8|int(b[1]) != chaosQueryID { // transaction ID must echo our query
		return ""
	}
	if b[2]&0x80 == 0 { // QR bit: must be a response, not a query
		return ""
	}
	if int(b[4])<<8|int(b[5]) != 1 { // exactly the one question we asked
		return ""
	}
	if int(b[6])<<8|int(b[7]) == 0 { // ANCOUNT: no answer, nothing to read
		return ""
	}
	if len(b) < 12+len(chaosVersionQuestion) || !bytes.Equal(b[12:12+len(chaosVersionQuestion)], chaosVersionQuestion) {
		return "" // the question doesn't echo our version.bind/TXT/CHAOS query
	}
	off := skipName(b, 12) // question name
	off += 4               // qtype + qclass
	off = skipName(b, off) // answer name
	if off+10 > len(b) {   // TYPE(2) CLASS(2) TTL(4) RDLENGTH(2)
		return ""
	}
	rdlen := int(b[off+8])<<8 | int(b[off+9])
	off += 10
	if rdlen < 1 || off+rdlen > len(b) {
		return ""
	}
	slen := int(b[off]) // TXT character-string length
	if off+1+slen > len(b) {
		return ""
	}
	return string(b[off+1 : off+1+slen])
}

// skipName advances past a DNS name (a compression pointer or a label sequence).
func skipName(b []byte, off int) int {
	for off < len(b) {
		l := int(b[off])
		if l == 0 {
			return off + 1
		}
		if l&0xc0 == 0xc0 { // compression pointer (2 bytes)
			return off + 2
		}
		off += 1 + l
	}
	return off
}

// softwareFromVersion maps a version.bind string to a friendly resolver-software
// label. Pi-hole's FTL reports "dnsmasq-pi-hole-vX.Y", so check pi-hole before
// dnsmasq. "" when unrecognised (caller falls back to the IP).
func softwareFromVersion(v string) string {
	s := strings.ToLower(v)
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "pi-hole") || strings.Contains(s, "pihole"):
		return "Pi-hole"
	case strings.Contains(s, "adguard"):
		return "AdGuard Home"
	case strings.Contains(s, "dnsmasq"):
		return "dnsmasq"
	case strings.Contains(s, "unbound"):
		return "Unbound"
	case strings.Contains(s, "knot"):
		return "Knot Resolver"
	case strings.Contains(s, "powerdns") || strings.Contains(s, "pdns"):
		return "PowerDNS"
	case strings.Contains(s, "technitium"):
		return "Technitium"
	case strings.Contains(s, "bind") || strings.Contains(s, "named"):
		return "BIND"
	default:
		return ""
	}
}

// labelNeedsRetry reports whether a cached resolver label still contains a bare
// PUBLIC IP - the fallback used when that resolver's Cymru naming lookup failed -
// so the naming is retried on later refreshes. Private/LAN resolvers legitimately
// label as IPs and don't trigger a retry.
func labelNeedsRetry(label string) bool {
	for _, part := range strings.Split(label, " + ") {
		ip := net.ParseIP(part)
		if ip == nil {
			continue
		}
		if !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
			return true
		}
	}
	return false
}

// equalStrings reports whether two string slices have identical contents in order.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

const (
	// egressLocStale is how long IPmap may be transiently failing for an egress IP
	// before we degrade its location to the coarse Cymru country - long enough that
	// a brief 429 doesn't flicker the precise city to a country code.
	egressLocStale = 10 * time.Minute
	// egressLocRevalidate bounds how often a non-precise (country fallback) entry
	// re-queries IPmap: it's cached and served in between, so a no-data or
	// persistently-failing IP doesn't re-hit rate-limited IPmap every refresh - while
	// still letting a recovered IPmap upgrade the entry to a precise city eventually.
	egressLocRevalidate = time.Hour
	// maxEgressGeo caps the per-IP cache. Egress IPs barely churn (the IP isn't
	// attacker-controlled), so this is a safety bound, not a working limit.
	maxEgressGeo = 256
)

type egressGeoEntry struct {
	loc     string // display location: precise "City, CC", or a country-code fallback ("" if even that is unknown)
	precise bool   // true if loc is the IPmap city (permanent); false if it's a country fallback (revalidated)
	at      int64  // unix time loc was last resolved; 0 until resolved at least once
	missAt  int64  // unix time first seen city-less via a transient IPmap error (grace timer); 0 once resolved
}

// egressLocation resolves the DNS egress IP's display location. It prefers the
// precise IPmap city, cached per IP (an IP's geo doesn't move) so a flipping
// egress neither re-hits rate-limited IPmap nor flickers. It falls back to the
// Team Cymru country code - immediately when IPmap has no data for the IP, or
// after egressLocStale of transient IPmap failures (429/timeout) - and CACHES that
// fallback too, re-validating only every egressLocRevalidate (so a no-data IP
// isn't re-queried on every refresh). Returns "" while a transient failure is
// still within the grace window.
func (m *Manager) egressLocation(ctx context.Context, ip string) string {
	now := time.Now().Unix()
	m.egressMu.Lock()
	if m.egressGeo == nil {
		m.egressGeo = map[string]*egressGeoEntry{}
	}
	e := m.egressGeo[ip]
	if e == nil {
		if len(m.egressGeo) >= maxEgressGeo { // evict the oldest before inserting
			var oldIP string
			var oldAt int64
			for k, v := range m.egressGeo {
				if oldIP == "" || v.at < oldAt {
					oldIP, oldAt = k, v.at
				}
			}
			delete(m.egressGeo, oldIP)
		}
		e = &egressGeoEntry{}
		m.egressGeo[ip] = e
	}
	// Serve cached: a precise city forever; a country fallback (or known-empty) until
	// it's due for revalidation. at==0 means never resolved, so fall through to look up.
	if e.at > 0 && (e.precise || now-e.at < int64(egressLocRevalidate/time.Second)) {
		loc := e.loc
		m.egressMu.Unlock()
		return loc
	}
	missAt := e.missAt
	m.egressMu.Unlock()

	city, country, _, _, st := ipmapGeo(ctx, m, ip)
	switch st {
	case ipmapOK:
		loc := city
		if country != "" {
			loc += ", " + country
		}
		m.egressMu.Lock()
		e.loc, e.precise, e.at, e.missAt = loc, true, now, 0
		m.egressMu.Unlock()
		return loc
	case ipmapNoData:
		c := cymruCountry(ctx, ip) // permanent gap for this IP -> country, cached + revalidated
		m.egressMu.Lock()
		e.loc, e.precise, e.at, e.missAt = c, false, now, 0
		m.egressMu.Unlock()
		return c
	default: // ipmapError (transient: 429/timeout)
		if missAt == 0 {
			missAt = now
			m.egressMu.Lock()
			e.missAt = now
			m.egressMu.Unlock()
		}
		if now-missAt >= int64(egressLocStale/time.Second) {
			c := cymruCountry(ctx, ip) // failing past the grace window -> country, cached + revalidated
			m.egressMu.Lock()
			e.loc, e.precise, e.at = c, false, now
			m.egressMu.Unlock()
			return c
		}
		return "" // within grace window - no location yet
	}
}
