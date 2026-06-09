// Exit-node discovery: where traffic leaves the ISP's network. Two signals:
//
//   - A traceroute toward 1.1.1.1, walked until the AS changes. The last hop
//     inside the ISP (its ASN, or private/CGNAT space) is the exit router; the
//     first hop beyond it is the peering/transit handoff. Per-hop ASNs come from
//     Team Cymru's IP->ASN DNS zone, so no extra HTTP dependency.
//   - Cloudflare's /cdn-cgi/trace "colo" field - the PoP this connection lands
//     at, a readable proxy for the exit city.
package netinfo

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/util"
)

const (
	traceMaxTTL       = 16                     // 1.1.1.1 is virtually always within 16 hops
	traceProbeTimeout = 400 * time.Millisecond // per-hop reply wait
	traceBudget       = 12 * time.Second       // ceiling for the trace itself; boundary enrichment (ASN/geo lookups) adds a few bounded seconds after it
)

// traceTarget is the default traceroute destination - the same anycast address
// the colo lookup uses, so both describe the same path. Overridden by ExitTarget.
var traceTarget = [4]byte{1, 1, 1, 1}

// traceFn points at the platform traceroute (trace_{linux,darwin,windows}.go, or
// the stub); a package var so tests can stub the hop list.
var traceFn = traceroute

// resolveIPv4 resolves a host (IPv4 literal or name) to a 4-byte address for the
// IPv4-only traceroute, with a short timeout. ok is false when it doesn't
// resolve to an IPv4 in time, letting the caller fall back to the default target.
func resolveIPv4(ctx context.Context, host string) ([4]byte, bool) {
	var z [4]byte
	rctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(rctx, "ip4", host)
	if err != nil || len(ips) == 0 {
		return z, false
	}
	v4 := ips[0].To4()
	if v4 == nil {
		return z, false
	}
	return [4]byte{v4[0], v4[1], v4[2], v4[3]}, true
}

// isInternalTraceTarget reports whether a resolved IPv4 exit target is a loopback
// (127.0.0.0/8) or link-local (169.254.0.0/16) address - neither is ever a real
// internet exit path, and tracing them probes the host or the cloud-metadata
// endpoint. RFC1918 is intentionally NOT rejected (see the caller).
func isInternalTraceTarget(v [4]byte) bool {
	ip := net.IP(v[:])
	return ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

// tHop is one responsive traceroute hop.
type tHop struct {
	TTL int
	IP  string
	RTT time.Duration
}

// ExitInfo describes the ISP boundary on the path out: the last hop inside the
// ISP's network (the exit router) and the first hop beyond it (the handoff).
type ExitInfo struct {
	IP        string  `json:"ip,omitempty"`
	Name      string  `json:"name,omitempty"` // reverse DNS, often encodes the city
	Hop       int     `json:"hop,omitempty"`
	RTTms     float64 `json:"rtt_ms,omitempty"`
	Loc       string  `json:"loc,omitempty"` // city of the exit router
	Lat       float64 `json:"lat,omitempty"` // exit-router coordinates (speedtest anchor)
	Lon       float64 `json:"lon,omitempty"`
	NextIP    string  `json:"next_ip,omitempty"`
	NextName  string  `json:"next_name,omitempty"`
	NextRTTms float64 `json:"next_rtt_ms,omitempty"`
	NextASN   string  `json:"next_asn,omitempty"`
	NextLoc   string  `json:"next_loc,omitempty"` // city of the handoff hop
	// TargetFallback is true when the configured exit target didn't resolve to
	// an IPv4 address, so this is the DEFAULT path (1.1.1.1), not the user's.
	TargetFallback bool `json:"target_fallback,omitempty"`
}

// discoverExit traces toward target (resolved by cachedExit) and locates the
// ISP boundary. ourASN (bare digits, may be empty) is the ISP's ASN from the
// public-IP's AS string; when empty, the first public hop's ASN is assumed to
// be the ISP's.
func (m *Manager) discoverExit(ctx context.Context, ourASN string, target [4]byte) (*ExitInfo, error) {
	tctx, cancel := context.WithTimeout(ctx, traceBudget)
	defer cancel()
	hops, err := traceFn(tctx, target, traceMaxTTL, traceProbeTimeout)
	if err != nil {
		return nil, err
	}
	if len(hops) == 0 {
		return nil, fmt.Errorf("no responsive hops")
	}
	// The trace must reveal at least one hop that isn't the destination itself.
	// In a restrictive network (a container's userspace NAT, e.g. Docker Desktop)
	// only the destination's echo reply returns while every intermediate
	// Time-Exceeded is dropped, so the sole "hop" is the target masquerading as the
	// handoff. That is a failed discovery, not an exit: return an error so the Exit
	// line is hidden instead of showing the target as the exit. A trace that DOES
	// reach a real exit (e.g. docker run --network=host --cap-add=NET_RAW) still has
	// intermediate hops and passes.
	tgt := net.IP(target[:]).String()
	pathRevealed := false
	for _, h := range hops {
		if h.IP != tgt {
			pathRevealed = true
			break
		}
	}
	if !pathRevealed {
		return nil, fmt.Errorf("trace revealed no path hops (only the destination responded)")
	}

	// ASN per public hop (private/CGNAT count as inside the ISP), looked up
	// concurrently - under packet loss the per-hop DNS timeouts would serialize.
	asns := make([]string, len(hops))
	asnErrs := make([]error, len(hops))
	var awg sync.WaitGroup
	for i, h := range hops {
		if insideAddr(h.IP) {
			continue
		}
		awg.Add(1)
		go func(i int, ip string) {
			defer awg.Done()
			asns[i], asnErrs[i] = cymruASNLookup(tctx, ip) // distinct index per goroutine - race-free
		}(i, h.IP)
	}
	awg.Wait()
	exitIdx, nextIdx := ispBoundary(hops, asns, ourASN)
	if exitIdx < 0 && nextIdx < 0 {
		return nil, fmt.Errorf("no AS boundary found")
	}
	// With no exit router found, the handoff must be a real transit hop, not the
	// destination itself (see handoffIsDest): fail so the Exit line hides instead
	// of naming the destination as the handoff.
	if handoffIsDest(hops, exitIdx, nextIdx, tgt) {
		return nil, fmt.Errorf("trace revealed no handoff (only local hops and the destination responded)")
	}
	// The boundary walk stops at the first public hop with no ASN. When that
	// blank came from a FAILED lookup (timeout, resolver hiccup) rather than a
	// genuinely unannounced prefix (an IXP LAN answers NXDOMAIN), the "handoff"
	// may really be an in-ISP router - fail the round instead of caching it.
	if nextIdx >= 0 && asnErrs[nextIdx] != nil {
		return nil, fmt.Errorf("asn lookup failed at boundary hop: %w", asnErrs[nextIdx])
	}

	// Enrich the two boundary hops (reverse DNS + geolocation) concurrently -
	// each is a couple of independent round-trips. The two goroutines write
	// disjoint fields of e, so no lock is needed.
	e := &ExitInfo{}
	var ewg sync.WaitGroup
	if exitIdx >= 0 {
		ewg.Add(1)
		go func() {
			defer ewg.Done()
			h := hops[exitIdx]
			name := rdns(ctx, h.IP)
			loc, lat, lon := geolocateHop(ctx, m, h.IP, name)
			e.IP, e.Hop, e.RTTms = h.IP, h.TTL, util.DurMS(h.RTT)
			e.Name, e.Loc, e.Lat, e.Lon = name, loc, lat, lon
		}()
	}
	if nextIdx >= 0 {
		ewg.Add(1)
		go func() {
			defer ewg.Done()
			h := hops[nextIdx]
			name := rdns(ctx, h.IP)
			loc, _, _ := geolocateHop(ctx, m, h.IP, name)
			e.NextIP, e.NextRTTms, e.NextASN = h.IP, util.DurMS(h.RTT), asns[nextIdx]
			e.NextName, e.NextLoc = name, loc
		}()
	}
	ewg.Wait()
	return e, nil
}

// ispBoundary walks the trace and returns the ISP exit router (last hop inside
// the ISP) and the first hop beyond it - the handoff. A hop is "inside" when it
// is private/CGNAT or in the ISP's ASN; the first hop that is neither (including
// IXP LANs, whose prefixes often have no origin ASN) is the handoff. Empty ourASN
// means assume the first public hop's ASN is the ISP's. A private/CGNAT hop is
// never the ISP's exit router, so when the walk ends on one the exit falls back
// to the last PUBLIC in-ISP hop (ISPs run RFC1918/CGNAT hops between the real
// exit and the handoff); with no public in-ISP hop at all (ISP edge ICMP-filtered,
// or CGNAT/upstream space) the exit is dropped. Either index is -1 when not found.
func ispBoundary(hops []tHop, asns []string, ourASN string) (exitIdx, nextIdx int) {
	if ourASN == "" {
		for _, a := range asns {
			if a != "" {
				ourASN = a
				break
			}
		}
	}
	exitIdx, nextIdx = -1, -1
	pubExit := -1 // last public hop in the ISP's ASN, in case the walk ends on a private hop
	for i := range hops {
		if insideAddr(hops[i].IP) {
			exitIdx = i
			continue
		}
		if asns[i] != "" && asns[i] == ourASN {
			exitIdx, pubExit = i, i
			continue
		}
		nextIdx = i
		break
	}
	if exitIdx >= 0 && insideAddr(hops[exitIdx].IP) {
		exitIdx = pubExit
	}
	return exitIdx, nextIdx
}

// handoffIsDest reports whether the discovered handoff is really the trace's own
// destination rather than a transit hop beyond the ISP - and no exit router was
// found either. In a container's userspace NAT (Docker Desktop) every intermediate
// Time-Exceeded is dropped, so the only replies are local hops plus the target's
// own echo, which then masquerades as the "first hop beyond the ISP". That is a
// completed trace, not an observed ISP boundary. A trace that reaches a genuine
// exit router (exitIdx >= 0) is a real discovery and is kept.
func handoffIsDest(hops []tHop, exitIdx, nextIdx int, target string) bool {
	return exitIdx < 0 && nextIdx >= 0 && hops[nextIdx].IP == target
}

// geolocateHop returns a city for a router hop. RIPE IPmap is tried first - it's
// built for infrastructure IPs, unlike eyeball geo databases that misplace
// backbone routers at the ISP's registered HQ. The reverse-DNS name (often with
// an airport/city code) is the offline fallback. "" when neither yields anything.
func geolocateHop(ctx context.Context, m *Manager, ip, name string) (city string, lat, lon float64) {
	// /metrics counter: how often IPmap answers vs falling back to rDNS.
	if c, la, lo, ok := ipmapLoc(ctx, m, ip); ok {
		stats.Inc("netinfo.ipmap_hit")
		return c, la, lo
	}
	stats.Inc("netinfo.ipmap_miss")
	return cityFromRDNS(name), 0, 0 // rDNS fallback has no coordinates
}

// ipmapStatus distinguishes the two ways an IPmap lookup can come up empty, which
// the egress-location fallback treats differently: ipmapNoData (a 200 with no
// location) is permanent for that IP, so we degrade to the country immediately;
// ipmapError (timeout / 429 / non-200) is transient, so we wait it out.
type ipmapStatus int

const (
	ipmapOK     ipmapStatus = iota // got a city
	ipmapNoData                    // 200, but IPmap has no location for this IP
	ipmapError                     // transport error, timeout, or non-200 (e.g. 429)
)

// ipmapGeo asks RIPE IPmap for ip's best-estimate location, returning the raw
// city / country-code / coordinate fields plus a status. The "best" endpoint
// returns the single highest-confidence location, no API key. Backs the
// DNS-resolver and router-hop geo (infrastructure IPs); the host's own public IP
// uses publicIPGeo (eyeball providers) instead.
func ipmapGeo(ctx context.Context, m *Manager, ip string) (city, country string, lat, lon float64, st ipmapStatus) {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet,
		"https://ipmap-api.ripe.net/v1/locate/"+ip+"/best", nil)
	if err != nil {
		return "", "", 0, 0, ipmapError
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return "", "", 0, 0, ipmapError
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", 0, 0, ipmapError
	}
	var r struct {
		Location struct {
			City    string  `json:"cityName"`
			Country string  `json:"countryCodeAlpha2"`
			Lat     float64 `json:"latitude"`
			Lon     float64 `json:"longitude"`
		} `json:"location"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&r); err != nil {
		return "", "", 0, 0, ipmapError
	}
	if r.Location.City == "" {
		return "", "", 0, 0, ipmapNoData
	}
	return r.Location.City, r.Location.Country, r.Location.Lat, r.Location.Lon, ipmapOK
}

// ipmapLoc wraps ipmapGeo into the joined "City, CC" string the DNS/exit rows
// display; ok is false when the location is unknown or unreachable.
func ipmapLoc(ctx context.Context, m *Manager, ip string) (city string, lat, lon float64, ok bool) {
	c, country, la, lo, st := ipmapGeo(ctx, m, ip)
	if st != ipmapOK {
		return "", 0, 0, false
	}
	if country != "" {
		c += ", " + country
	}
	return c, la, lo, true
}

// publicIPGeo geolocates the host's OWN public IP to a city + ISO country code.
// RIPE IPmap (ipmapGeo, above) is tuned for infrastructure IPs and routinely has
// no fix for the residential/FTTH address a host actually sits behind, so this
// uses "eyeball" providers instead: ipwho.is first, then geojs.io. Both are
// keyless HTTPS and city-accurate for home IPs. The caller only runs this on an
// IP change (see fetch's per-IP cache), so it stays far below any rate limit.
// ok is false when neither provider resolves a city.
func publicIPGeo(ctx context.Context, m *Manager, ip string) (city, country string, ok bool) {
	if c, cc, ok := ipwhoisGeo(ctx, m, ip); ok {
		return c, cc, true
	}
	return geojsGeo(ctx, m, ip)
}

// ipwhoisGeo geolocates ip via ipwho.is. The "success" flag guards error payloads
// (rate-limit / reserved range), which the API still returns with HTTP 200.
func ipwhoisGeo(ctx context.Context, m *Manager, ip string) (city, country string, ok bool) {
	var r struct {
		Success bool   `json:"success"`
		City    string `json:"city"`
		Country string `json:"country_code"`
	}
	if !getJSON(ctx, m, "https://ipwho.is/"+ip, &r) || !r.Success || r.City == "" {
		return
	}
	return r.City, r.Country, true
}

// geojsGeo geolocates ip via geojs.io - the fallback when ipwho.is is down or empty.
func geojsGeo(ctx context.Context, m *Manager, ip string) (city, country string, ok bool) {
	var r struct {
		City    string `json:"city"`
		Country string `json:"country_code"`
	}
	if !getJSON(ctx, m, "https://get.geojs.io/v1/ip/geo/"+ip+".json", &r) || r.City == "" {
		return
	}
	return r.City, r.Country, true
}

// getJSON GETs url with a 3s timeout and decodes the JSON body into v, returning
// false on any transport, status, or decode failure. The read is bounded so an
// oversized or hostile response can't exhaust memory.
func getJSON(ctx context.Context, m *Manager, url string, v any) bool {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(v) == nil
}

// cityFromRDNS extracts a city from a router's PTR name by matching a known
// airport/city code among its dot/hyphen-separated tokens
// ("ae1-cr2.fra10.isp.net" -> "Frankfurt"). "" when no token is known.
func cityFromRDNS(name string) string {
	if name == "" {
		return ""
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '.' || r == '-' || r == '_'
	}) {
		// Tokens often carry a trailing index ("fra10", "lon1"); try the whole
		// token, then its leading-letters prefix, against the code table.
		if p, ok := iataCity[tok]; ok {
			// A bare three-letter token is weak evidence: every code in the table
			// is three letters, and several collide with common words or device
			// roles ("den", "sea", "van", "was"), so this can mislabel a city. It
			// only feeds the display string (zero coords), never server selection,
			// so still return it - but flag the low confidence so a mislabel is
			// observable. An indexed token (below) carries the stronger evidence.
			if len(tok) <= 3 {
				stats.Inc("netinfo.rdns_city_lowconf")
			}
			return p.City
		}
		letters := tok
		for i, r := range tok {
			if r < 'a' || r > 'z' {
				letters = tok[:i]
				break
			}
		}
		// Only the indexed form ("fra10" -> "fra") reaches here as stronger
		// evidence: a bare token was already handled (or rejected) above, so the
		// trailing POP index is what distinguishes a location code from a word.
		if len(letters) >= 3 && letters != tok {
			if p, ok := iataCity[letters]; ok {
				return p.City
			}
		}
	}
	return ""
}

// exitCacheFor bounds how often the traceroute reruns - the path is stable, and
// netinfo refreshes also fire right after every speedtest, where a multi-second
// trace would delay the recorded result.
const exitCacheFor = 10 * time.Minute

// cachedExit returns the exit info, re-discovering at most every exitCacheFor. A
// failed discovery (e.g. no raw-socket privilege) is cached too, so it isn't
// retried on every refresh.
//
// Single-flight: the lock is held only to inspect/claim, never across the trace,
// so a caller hitting a fresh cache returns immediately rather than blocking
// behind an in-flight trace, and callers finding it stale wait for the one
// running trace instead of starting duplicates. A waiter does NOT blindly return
// the in-flight result: that trace may be for a DIFFERENT target (a concurrent
// caller changed the setting) or from the OLD network (an IP-change bust landed
// after it started), so after waiting the loop re-validates the cache against the
// waiter's own target and re-traces if it doesn't match.
func (m *Manager) cachedExit(ctx context.Context, ourASN string) *ExitInfo {
	want := ""
	if m.ExitTargetFn != nil {
		want = strings.TrimSpace(m.ExitTargetFn())
	}
	var (
		done     chan struct{}
		startGen uint64
	)
	for {
		m.traceMu.Lock()
		// Serve the cache only when fresh AND the last attempt aimed at the current
		// target - changing the target must force a re-trace, while a target whose
		// discovery keeps failing waits out the window like any other failure
		// instead of re-tracing on every refresh.
		if !m.traceAt.IsZero() && m.attemptedFor == want && time.Since(m.traceAt) < exitCacheFor {
			ex := m.exit
			m.traceMu.Unlock()
			return ex
		}
		if ch := m.tracing; ch != nil {
			// A trace is already running, but its result isn't necessarily ours -
			// it may aim at a different target or predate an IP-change bust. Wait
			// for it, then loop back to re-check the cache against our target rather
			// than returning it blind.
			m.traceMu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return m.snapshotExit()
			}
		}
		// Claim the single-flight slot and capture the generation it runs under, so
		// a deliberate cache-bust (IP change / manual refresh) that lands while the
		// trace is in flight can be spotted at commit and its stale result dropped.
		done = make(chan struct{})
		m.tracing = done
		startGen = m.traceGen
		m.traceMu.Unlock()
		break
	}

	// Resolve the target here, not in discoverExit, so the result can record
	// what was actually traced: an unresolvable target (transient DNS failure,
	// or an IPv6-only name the IPv4 traceroute can't use) falls back to the
	// default path, flagged so the UI doesn't present it as the chosen one.
	target, fellBack := traceTarget, false
	if want != "" {
		v, ok := resolveIPv4(ctx, want)
		switch {
		case !ok:
			fellBack = true
			if ctx.Err() == nil { // don't blame the target when the caller aborted
				m.log.Warn("exit target did not resolve to IPv4; tracing the default path", "target", want)
			}
		case isInternalTraceTarget(v):
			// A target resolving to loopback or link-local would traceroute the host
			// itself or the 169.254.169.254 cloud-metadata endpoint, never a real
			// internet exit path - and a crafted DNS name could aim it there. Refuse it
			// and trace the default. RFC1918 stays allowed on purpose: tracing a LAN
			// gateway is legitimate and the trust model already equates dashboard
			// access with local-network reach.
			fellBack = true
			m.log.Warn("exit target resolves to a loopback/link-local address; tracing the default path", "target", want)
		default:
			target = v
		}
	}
	ex, err := m.discoverExit(ctx, ourASN, target)
	if err == nil && ex != nil {
		ex.TargetFallback = fellBack
	}
	// A trace cut short by the CALLER (browser abort mid-refresh, shutdown) is not
	// a real failure: it must neither skew the trace_ok/trace_fail counters nor
	// burn the retry window below.
	cancelled := err != nil && ctx.Err() != nil
	// /metrics counters: does traceroute-based exit discovery succeed, or do
	// privileges/filtering kill it?
	if err != nil {
		if !cancelled {
			stats.Inc("netinfo.trace_fail")
		}
		m.log.Debug("exit discovery unavailable", "err", err)
	} else {
		stats.Inc("netinfo.trace_ok")
		if ex != nil {
			m.log.Debug("exit discovered", "router", ex.Name, "router_ip", ex.IP,
				"handoff", ex.NextName, "handoff_asn", ex.NextASN)
		}
	}

	m.traceMu.Lock()
	defer m.traceMu.Unlock()
	// Release the single-flight slot first so a waiter (or the next refresh) can
	// re-trace if we discard below; both re-check the cache under this same lock,
	// which we hold until the deferred unlock, so they can't observe a half-updated
	// state.
	m.tracing = nil
	close(done)
	// A deliberate cache-bust (IP change / manual refresh) bumped traceGen while
	// this trace was in flight: it ran against the OLD network/target, so drop its
	// result instead of writing it over the bust and re-stamping it fresh. Leaving
	// traceAt/attemptedFor untouched keeps the cache "stale" so the next caller
	// re-traces the current network.
	if m.traceGen != startGen {
		return m.exit
	}
	// Replace the cached exit only on success - a transient failure (silent hop,
	// no AS boundary this round) must not wipe the last-known exit, or the Exit
	// row would flicker out and back. Still stamp traceAt/attemptedFor so we wait
	// out the cache window before retrying instead of re-tracing every refresh -
	// except for caller-cancelled traces, which leave both alone so the next
	// refresh retries immediately.
	if err == nil {
		m.exit = ex
		m.tracedFor = want // the target setting this exit answers for
	} else if !cancelled && m.tracedFor != want {
		// Failed while aiming at a target the cached exit was NOT traced for:
		// keeping the old target's path on display under the new setting would
		// be wrong, so drop it.
		m.exit, m.tracedFor = nil, ""
	}
	if !cancelled {
		m.attemptedFor = want
		m.traceAt = time.Now()
	}
	return m.exit
}

// snapshotExit returns the last discovered exit info under the lock.
func (m *Manager) snapshotExit() *ExitInfo {
	m.traceMu.Lock()
	defer m.traceMu.Unlock()
	return m.exit
}

// cfColo asks Cloudflare which PoP serves this connection (the "colo" line of
// /cdn-cgi/trace) - where the ISP hands traffic to Cloudflare's network.
func (m *Manager) cfColo(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://1.1.1.1/cdn-cgi/trace", nil)
	if err != nil {
		return ""
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4096))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "colo="); ok {
			return v
		}
	}
	return ""
}

// cymruASN resolves an IP to its origin ASN (bare digits) via Team Cymru's DNS
// zone - origin.asn.cymru.com for IPv4, origin6.asn.cymru.com for IPv6; "" when
// unknown or the lookup fails.
func cymruASN(ctx context.Context, ip string) string {
	asn, _ := cymruASNLookup(ctx, ip)
	return asn
}

// cymruASNLookup is cymruASN, but reports a FAILED lookup (timeout, resolver
// error) separately from a prefix with no origin ASN (NXDOMAIN - e.g. an IXP
// LAN). Callers that cache their conclusion need to tell the two apart.
func cymruASNLookup(ctx context.Context, ip string) (string, error) {
	q := cymruOriginQuery(ip)
	if q == "" {
		return "", nil
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	txts, err := net.DefaultResolver.LookupTXT(c, q)
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return "", nil // NXDOMAIN: genuinely no origin ASN announced for this prefix
		}
		return "", err
	}
	if len(txts) == 0 {
		return "", nil
	}
	asn, _ := pickCymruASN(txts)
	return asn, nil
}

// cymruCountry returns the registration country code for ip from Team Cymru's
// origin zone (the same TXT cymruASN reads, so the host resolver usually has it
// cached). "" when unknown. Used as the coarse egress-location fallback.
func cymruCountry(ctx context.Context, ip string) string {
	q := cymruOriginQuery(ip)
	if q == "" {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	txts, err := net.DefaultResolver.LookupTXT(c, q)
	if err != nil || len(txts) == 0 {
		return ""
	}
	_, country := pickCymruASN(txts)
	return country
}

// cymruOriginQuery builds the Team Cymru origin-lookup name for ip: reversed
// octets under origin.asn.cymru.com (IPv4), or reversed nibbles under
// origin6.asn.cymru.com (IPv6). "" when ip doesn't parse.
func cymruOriginQuery(ip string) string {
	a := net.ParseIP(ip)
	if a == nil {
		return ""
	}
	if v4 := a.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", v4[3], v4[2], v4[1], v4[0])
	}
	v6 := a.To16()
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 15; i >= 0; i-- {
		b.WriteByte(hex[v6[i]&0xf])
		b.WriteByte('.')
		b.WriteByte(hex[v6[i]>>4])
		b.WriteByte('.')
	}
	b.WriteString("origin6.asn.cymru.com")
	return b.String()
}

// pickCymruASN chooses the origin AS for an IP from Team Cymru's TXT answer. An
// IP can fall under several overlapping announcements - e.g. an ISP's /22 nested
// in an upstream's /19 aggregate - one record each, in non-deterministic order.
// BGP forwards by the most-specific prefix, so that's the operative AS; taking
// txts[0] made the answer flap between the ISP and the aggregate holder, breaking
// the ISP label and exit detection. Pick the longest-prefix record; fall back to
// the first parseable ASN when none carries a prefix.
func pickCymruASN(txts []string) (asn, country string) {
	bestLen := -1
	for _, t := range txts {
		a, plen, ctry := parseCymruASN(t)
		if a == "" {
			continue
		}
		if asn == "" || plen > bestLen { // seed on the first ASN, then prefer longer prefixes
			asn, bestLen, country = a, plen, ctry
		}
	}
	return asn, country
}

// parseCymruASN extracts the origin ASN, prefix length, and registration country
// from one Cymru TXT record ("13335 | 1.1.1.0/24 | AU | apnic | 2011-08-11" ->
// "13335", 24, "AU"). prefixLen is -1 when the record carries no parseable prefix;
// for a true multi-origin record ("1403 577 | ...") the first ASN is returned.
func parseCymruASN(txt string) (asn string, prefixLen int, country string) {
	parts := strings.Split(txt, "|")
	f := strings.Fields(parts[0])
	if len(f) == 0 {
		return "", -1, ""
	}
	asn, prefixLen = f[0], -1
	if len(parts) > 1 {
		if _, plen, ok := strings.Cut(strings.TrimSpace(parts[1]), "/"); ok {
			if n, err := strconv.Atoi(plen); err == nil {
				prefixLen = n
			}
		}
	}
	if len(parts) > 2 {
		country = strings.TrimSpace(parts[2])
	}
	return asn, prefixLen, country
}

// cymruASNName resolves an AS number (bare digits) to its registered name via
// Team Cymru's asn.cymru.com zone; "" when unknown.
func cymruASNName(ctx context.Context, asn string) string {
	if asn == "" {
		return ""
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	txts, err := net.DefaultResolver.LookupTXT(c, "AS"+asn+".asn.cymru.com")
	if err != nil || len(txts) == 0 {
		return ""
	}
	return parseCymruASNName(txts[0])
}

// parseCymruASNName extracts the AS name from an asn.cymru.com TXT record
// ("13335 | US | arin | 2010-07-14 | CLOUDFLARENET, US" -> "CLOUDFLARENET"),
// stripping the trailing ", CC" country code Cymru appends to the name.
func parseCymruASNName(txt string) string {
	fields := strings.Split(txt, "|")
	name := strings.TrimSpace(fields[len(fields)-1])
	if i := strings.LastIndex(name, ","); i >= 0 && len(strings.TrimSpace(name[i+1:])) == 2 {
		name = strings.TrimSpace(name[:i])
	}
	return name
}

// asnDisplayName turns a Team Cymru AS name into a friendlier org label by
// dropping the ARIN-style "HANDLE - " prefix (a single spaceless token before
// " - ") when present: "CLOUDFLARENET - Cloudflare, Inc." -> "Cloudflare, Inc.",
// "nextdns - NextDNS, Inc." -> "NextDNS, Inc.". RIPE/APNIC names without that
// prefix are returned unchanged.
func asnDisplayName(name string) string {
	if h, rest, ok := strings.Cut(name, " - "); ok && rest != "" && !strings.Contains(h, " ") {
		return rest
	}
	return name
}

// dnsProvider derives an "AS<n> NAME" label for a resolver egress IP via Team
// Cymru (origin ASN -> name), matching the ISP "org" format used elsewhere.
// Falls back to bare "AS<n>" when only the number resolves; "" when neither does.
func dnsProvider(ctx context.Context, ip string) string {
	asn := cymruASN(ctx, ip)
	if asn == "" {
		return ""
	}
	if name := cymruASNName(ctx, asn); name != "" {
		return "AS" + asn + " " + name
	}
	return "AS" + asn
}

// asnFromOrg extracts the bare ASN from an AS org string
// ("AS3320 Deutsche Telekom AG" -> "3320").
func asnFromOrg(org string) string {
	f := strings.Fields(org)
	if len(f) == 0 || !strings.HasPrefix(f[0], "AS") {
		return ""
	}
	n := f[0][2:]
	if n == "" {
		return ""
	}
	for _, r := range n {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return n
}

// insideAddr reports whether ip is address space that can only be inside the
// home/ISP network: RFC1918, CGNAT (100.64/10), link-local, loopback.
func insideAddr(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil {
		return false
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() {
		return true
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1]&0xc0 == 64 {
		return true
	}
	return false
}

// rdns returns the PTR name for ip ("" when none) - router names usually encode
// the city/site, the readable part of the answer.
func rdns(ctx context.Context, ip string) string {
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	names, err := net.DefaultResolver.LookupAddr(c, ip)
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}
