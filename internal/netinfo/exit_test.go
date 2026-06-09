package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// When a snapshot carries ExitUnavailable (IPv6-only host, or a platform without
// a native trace - not Linux/macOS/Windows), Refresh must short-circuit the
// traceroute and leave trace_fail
// untouched - otherwise the doomed trace mislabels an unsupported case as a
// failure and the counter climbs forever. Exercised here via the IPv6-only
// gate, which sets ExitUnavailable the same way the platform gate does.
func TestRefreshGatedExitLeavesTraceFail(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	stats.ResetForTest()
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	m.Refresh(ctx)

	if got := m.Get().ExitUnavailable; got == "" {
		t.Fatal("ExitUnavailable not set - the gate that suppresses trace_fail never engaged")
	}
	if n := stats.Lifetime().Counters["netinfo.trace_fail"]; n != 0 {
		t.Errorf("netinfo.trace_fail = %d, want 0 (gated exit must not run the doomed trace)", n)
	}
}

// stubTrace swaps the platform traceroute (traceFn) for the test and restores
// it on cleanup - no raw sockets, no network.
func stubTrace(t *testing.T, fn func(context.Context, [4]byte, int, time.Duration) ([]tHop, error)) {
	t.Helper()
	old := traceFn
	traceFn = fn
	t.Cleanup(func() { traceFn = old })
}

// A trace whose only responsive "hop" is the destination itself (a container's
// userspace NAT, e.g. Docker Desktop) is a failed discovery, not an exit:
// discoverExit must return the no-path error instead of naming the target as
// the handoff.
func TestDiscoverExitOnlyDestinationResponds(t *testing.T) {
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		return []tHop{{TTL: 5, IP: "1.1.1.1", RTT: 8 * time.Millisecond}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ex, err := m.discoverExit(context.Background(), "1403", [4]byte{1, 1, 1, 1})
	if err == nil || !strings.Contains(err.Error(), "no path hops") {
		t.Fatalf("discoverExit = (%+v, %v), want the no-path error", ex, err)
	}
}

// Two cachedExit calls within the cache window must run a single trace - a
// failed discovery is cached too, so it isn't retried on every refresh.
func TestCachedExitCachesWithinWindow(t *testing.T) {
	var calls int
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		calls++
		return nil, errors.New("no raw socket")
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.cachedExit(context.Background(), "1403")
	m.cachedExit(context.Background(), "1403")
	if calls != 1 {
		t.Fatalf("traceFn ran %d times, want 1 (second call must hit the cache)", calls)
	}
}

// Changing the exit target between calls must force a re-trace even inside the
// cache window - the cached result answers for the old target. IP-literal
// targets resolve without DNS.
func TestCachedExitRetracesOnTargetChange(t *testing.T) {
	var calls int
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		calls++
		return nil, errors.New("no raw socket")
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	target := "9.9.9.9"
	m.ExitTargetFn = func() string { return target }
	m.cachedExit(context.Background(), "1403")
	target = "8.8.4.4"
	m.cachedExit(context.Background(), "1403")
	if calls != 2 {
		t.Fatalf("traceFn ran %d times, want 2 (target change must re-trace)", calls)
	}
}

// Two concurrent cachedExit calls with a slow trace must run ONE trace, the
// second caller waiting on the in-flight result instead of starting a duplicate,
// and both callers must get the same exit. The stub's hops avoid the network
// entirely: the unparseable "hop" is treated as public with no ASN (the handoff),
// so no Cymru/rDNS lookup can leave the process; hop geolocation stays canned.
func TestCachedExitSingleFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return []tHop{{TTL: 1, IP: "10.0.0.1"}, {TTL: 2, IP: "not-an-ip"}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(404, "")
	results := make(chan *ExitInfo, 2)
	for range 2 {
		go func() { results <- m.cachedExit(context.Background(), "1403") }()
	}
	<-entered // the first trace is in flight; the other caller must not start one
	close(release)
	a, b := <-results, <-results
	if n := calls.Load(); n != 1 {
		t.Fatalf("traceFn ran %d times, want 1 (single-flight)", n)
	}
	if a == nil || b == nil || a != b {
		t.Fatalf("callers got %p and %p, want the same non-nil exit", a, b)
	}
}

func TestResolveIPv4(t *testing.T) {
	// IP literals resolve without DNS (Go short-circuits them) - deterministic.
	if v, ok := resolveIPv4(context.Background(), "8.8.4.4"); !ok || v != [4]byte{8, 8, 4, 4} {
		t.Errorf("literal: got %v ok=%v, want [8 8 4 4] true", v, ok)
	}
	if _, ok := resolveIPv4(context.Background(), ""); ok {
		t.Error("empty host should not resolve")
	}
}

func TestAsnFromOrg(t *testing.T) {
	cases := []struct{ org, want string }{
		{"AS3320 Deutsche Telekom AG", "3320"},
		{"AS13335 Cloudflare, Inc.", "13335"},
		{"Deutsche Telekom AG", ""},
		{"ASN-FOO Something", ""},
		{"AS", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := asnFromOrg(c.org); got != c.want {
			t.Errorf("asnFromOrg(%q) = %q, want %q", c.org, got, c.want)
		}
	}
}

func TestParseCymruASN(t *testing.T) {
	cases := []struct {
		txt      string
		wantASN  string
		wantPlen int
		wantCC   string
	}{
		{"13335 | 1.1.1.0/24 | AU | apnic | 2011-08-11", "13335", 24, "AU"},
		{"3320 6939 | 80.81.192.0/21 | DE | ripencc | 1999", "3320", 21, "DE"}, // multi-origin: first ASN
		{"1403 | 66.254.60.0/22 | CA | arin | 2003-05-13", "1403", 22, "CA"},
		{"26480 | 66.254.32.0/19 | CA | arin | 2003-05-13", "26480", 19, "CA"},
		{"15169 | 8.8.8.0/24", "15169", 24, ""}, // no country field
		{"7018", "7018", -1, ""},                // ASN only, no prefix/country
		{"", "", -1, ""},
		{"   | x | y", "", -1, ""},
	}
	for _, c := range cases {
		asn, plen, cc := parseCymruASN(c.txt)
		if asn != c.wantASN || plen != c.wantPlen || cc != c.wantCC {
			t.Errorf("parseCymruASN(%q) = (%q, %d, %q), want (%q, %d, %q)", c.txt, asn, plen, cc, c.wantASN, c.wantPlen, c.wantCC)
		}
	}
}

// pickCymruASN must deterministically return the most-specific prefix's origin
// AS. The real eBOX case: 66.254.61.133 falls under AS1403's /22 and AS26480's
// /19 aggregate, returned by Cymru in random order - the /22 (AS1403) is the
// operative origin and must win regardless of record order.
func TestPickCymruASN(t *testing.T) {
	specific := "1403 | 66.254.60.0/22 | CA | arin | 2003-05-13"
	aggregate := "26480 | 66.254.32.0/19 | CA | arin | 2003-05-13"
	if got, cc := pickCymruASN([]string{specific, aggregate}); got != "1403" || cc != "CA" {
		t.Errorf("pickCymruASN(specific-first) = (%q, %q), want (1403, CA)", got, cc)
	}
	if got, _ := pickCymruASN([]string{aggregate, specific}); got != "1403" {
		t.Errorf("pickCymruASN(aggregate-first) = %q, want 1403", got)
	}
	if got, cc := pickCymruASN([]string{"13335 | 1.1.1.0/24 | AU | apnic | 2011-08-11"}); got != "13335" || cc != "AU" {
		t.Errorf("pickCymruASN(single) = (%q, %q), want (13335, AU)", got, cc)
	}
	if got, _ := pickCymruASN([]string{"7018", "174"}); got != "7018" { // no prefixes: first ASN
		t.Errorf("pickCymruASN(no-prefix) = %q, want 7018", got)
	}
	if got, _ := pickCymruASN(nil); got != "" {
		t.Errorf("pickCymruASN(nil) = %q, want empty", got)
	}
}

func TestParseCymruASNName(t *testing.T) {
	cases := []struct{ txt, want string }{
		{"13335 | US | arin | 2010-07-14 | CLOUDFLARENET, US", "CLOUDFLARENET"},
		{"13335 | US | arin | 2010-07-14 | CLOUDFLARENET - Cloudflare, Inc., US", "CLOUDFLARENET - Cloudflare, Inc."}, // real verbose form: handle + org, internal comma kept
		{"1403 | CA | arin | 2000-05-04 | EBOX, CA", "EBOX"},
		{"15169 | US | arin | 2000-03-30 | GOOGLE, US", "GOOGLE"},
		{"64500 | ZZ | other | 2020-01-01 | SOME, NAME, US", "SOME, NAME"}, // comma in name, trailing CC stripped
		{"99999 | US | arin | 2021-01-01 | NOCOUNTRY", "NOCOUNTRY"},        // no trailing CC
		{"", ""},
	}
	for _, c := range cases {
		if got := parseCymruASNName(c.txt); got != c.want {
			t.Errorf("parseCymruASNName(%q) = %q, want %q", c.txt, got, c.want)
		}
	}
}

func TestInsideAddr(t *testing.T) {
	inside := []string{"192.168.1.1", "10.0.0.1", "172.16.5.5", "100.64.0.1", "100.127.255.254", "169.254.1.1", "127.0.0.1"}
	outside := []string{"1.1.1.1", "100.63.255.255", "100.128.0.1", "80.81.192.1", "8.8.8.8"}
	for _, ip := range inside {
		if !insideAddr(ip) {
			t.Errorf("insideAddr(%q) = false, want true", ip)
		}
	}
	for _, ip := range outside {
		if insideAddr(ip) {
			t.Errorf("insideAddr(%q) = true, want false", ip)
		}
	}
	if insideAddr("not-an-ip") {
		t.Error("insideAddr should be false for garbage")
	}
}

// A per-hop geolocation must count an IPmap hit when RIPE answers and a miss
// when it falls back to the rDNS heuristic. Uses the canned transport from
// fetch_fallback_test.go - no network.
func TestGeolocateHopCounters(t *testing.T) {
	stats.ResetForTest()
	m := &Manager{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	m.http = canned(200, `{"location":{"cityName":"London","countryCodeAlpha2":"GB","latitude":51.5,"longitude":-0.12}}`)
	if city, _, _ := geolocateHop(ctx, m, "203.0.113.1", ""); city != "London, GB" {
		t.Fatalf("ipmap hit returned %q, want %q", city, "London, GB")
	}
	m.http = canned(404, "")
	if city, _, _ := geolocateHop(ctx, m, "203.0.113.1", "ae1-cr2.fra10.isp.net"); city != "Frankfurt" {
		t.Fatalf("rDNS fallback returned %q, want %q", city, "Frankfurt")
	}

	s := stats.Lifetime()
	if hit, miss := s.Counters["netinfo.ipmap_hit"], s.Counters["netinfo.ipmap_miss"]; hit != 1 || miss != 1 {
		t.Fatalf("ipmap_hit = %d, ipmap_miss = %d, want 1 and 1", hit, miss)
	}
}

func TestCityFromRDNS(t *testing.T) {
	cases := []struct{ name, want string }{
		{"ae1-cr2.fra10.isp.net", "Frankfurt"},
		{"bng4.tor.ebox.net", "Toronto"},
		{"xe-0-0-1.lon1.example.co.uk", "London"},
		{"eqix-yyz.cloudflare.com", "Toronto"},
		{"host.unknownville.net", ""},
		{"", ""},
		{"10.0.0.1", ""},
	}
	for _, c := range cases {
		if got := cityFromRDNS(c.name); got != c.want {
			t.Errorf("cityFromRDNS(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// ColoCoord backs exit-based auto server-selection on hosts that can't traceroute
// (containers, non-Linux): a Cloudflare colo code must resolve to its city and a
// plausible coordinate, and an unknown code must report not-ok so the caller falls
// through to the public IP.
func TestColoCoord(t *testing.T) {
	city, lat, lon, ok := ColoCoord("YUL") // case-insensitive
	if !ok || city != "Montreal" {
		t.Fatalf("ColoCoord(YUL) = (%q, ok=%v), want Montreal, ok", city, ok)
	}
	if lat < 45 || lat > 46 || lon > -73 || lon < -74 {
		t.Errorf("ColoCoord(YUL) coord = (%v, %v), not near Montreal", lat, lon)
	}
	if _, _, _, ok := ColoCoord("zzz"); ok {
		t.Error("ColoCoord(zzz) ok = true, want false for an unknown code")
	}
	if _, _, _, ok := ColoCoord(""); ok {
		t.Error("ColoCoord(\"\") ok = true, want false")
	}
}

// A public-IP change means a different network, so the cached exit must go -
// the VALUE, not just its freshness stamp. cachedExit deliberately keeps the
// last-known exit when a trace fails (a silent hop must not make the row
// flicker), and with the target setting unchanged that rule would keep serving
// the OLD network's router and re-stamp it fresh on every failed retry, so it
// could never recover.
// This drives the real Refresh path rather than restating the rule: revert the
// fix in netinfo.go and it fails.
func TestPublicIPChangeDropsCachedExit(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	// Every trace fails, which is the case that matters: on failure cachedExit
	// keeps whatever is cached unless the network change cleared it.
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		return nil, errors.New("no raw socket")
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.ExitTargetFn = func() string { return "1.1.1.1" }

	// First refresh establishes an IP, then plant a cached exit for it.
	ipv4Client, ipv6Client = canned(200, "198.51.100.7"), canned(500, "")
	m.Refresh(context.Background())
	m.traceMu.Lock()
	m.exit = &ExitInfo{Name: "old-isp-router", IP: "203.0.113.9"}
	m.tracedFor, m.attemptedFor, m.traceAt = "1.1.1.1", "1.1.1.1", time.Now()
	m.traceMu.Unlock()

	// The network changes under us.
	ipv4Client = canned(200, "203.0.113.77")
	m.Refresh(context.Background())

	m.traceMu.Lock()
	got := m.exit
	m.traceMu.Unlock()
	if got != nil {
		t.Errorf("cached exit survived a network change: %+v - the old network's router stays on display, "+
			"and every failed retry re-stamps it fresh so it never recovers", got)
	}
}
