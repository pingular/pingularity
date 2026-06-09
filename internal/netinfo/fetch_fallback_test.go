package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchFallbackLive exercises the real fetch() path. When the live public-IP
// lookup fails, the fallback (LastKnownFn) should fill ISP/IP. Network-gated, and
// skips when the live lookup succeeds (the common case - nothing to fall back to).
//
//	PINGULARITY_LIVE_TRACE=1 go test ./internal/netinfo -run FetchFallbackLive -v
func TestFetchFallbackLive(t *testing.T) {
	if os.Getenv("PINGULARITY_LIVE_TRACE") == "" {
		t.Skip("set PINGULARITY_LIVE_TRACE=1")
	}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.LastKnownFn = func() *Info {
		return &Info{PublicIP: "1.2.3.4", ISP: "AS-TEST EXAMPLE", City: "Townsville",
			DNSUpstream: &DNSEntry{IP: "9.9.9.9", Provider: "AS-DNS-TEST"}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	info := m.fetch(ctx)
	t.Logf("error=%q isp=%q ip=%q", info.Error, info.ISP, info.PublicIP)
	if info.Error == "" {
		t.Skip("live IP lookup succeeded - can't exercise the fallback right now")
	}
	if info.ISP != "AS-TEST EXAMPLE" || info.PublicIP != "1.2.3.4" {
		t.Fatalf("fallback not applied: isp=%q ip=%q", info.ISP, info.PublicIP)
	}
}

// TestFetchDNSFallbackCopies guards against fetch mutating the *DNSEntry inside
// the published snapshot in place: the fallback must take a private copy before
// the fill-in writes Provider/Location, since Get() shares the pointer with
// concurrent readers (web handler, ConnInfoFn). Run with -race to catch the
// in-place mutation. No network: the cancelled context fails every lookup
// instantly, forcing the pure fallback + fill-in path.
func TestFetchDNSFallbackCopies(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	shared := &DNSEntry{IP: "9.9.9.9", Provider: "unknown"}
	m.mu.Lock()
	m.info = Info{PublicIP: "1.2.3.4", ISP: "AS-TEST", DNSUpstream: shared, UpdatedAt: time.Now().Unix()}
	m.mu.Unlock()
	m.LastKnownFn = func() *Info {
		return &Info{DNSUpstream: &DNSEntry{IP: "9.9.9.9", Provider: "AS-DNS-TEST", Location: "Townsville"}}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Concurrent readers of the published snapshot, as the web handler and
	// speedtest ConnInfoFn do while a refresh is running.
	done := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if d := m.Get().DNSUpstream; d != nil {
					_ = d.Provider + d.Location
				}
			}
		}()
	}
	got := m.fetch(ctx)
	close(done)
	wg.Wait()

	if shared.Provider != "unknown" || shared.Location != "" {
		t.Fatalf("published DNSEntry mutated in place: %+v", *shared)
	}
	if got.DNSUpstream == nil || got.DNSUpstream.Provider != "AS-DNS-TEST" {
		t.Fatalf("fill-in not applied: %+v", got.DNSUpstream)
	}
	if got.DNSUpstream == shared {
		t.Fatal("fetch returned the published entry instead of a copy")
	}
}

// On an IPv6-only host (public IPv4 lookup fails, IPv6 succeeds) fetch must
// derive the identity from the IPv6 and publish a HEALTHY snapshot - no Error,
// so Loop keeps its normal cadence instead of the 5-minute error retry - and
// mark exit discovery unavailable (the traceroute is IPv4-only). The cancelled
// context fails the DNS-based lookups (Cymru/rDNS/resolver echo) instantly,
// so the ISP comes from the speed-history fallback for this same IPv6; the
// canned HTTP clients ignore the context, so the IP echo and geo provider
// still answer. No network.
func TestFetchIPv6Only(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	m.LastKnownFn = func() *Info {
		return &Info{PublicIPv6: "2001:db8::1234", ISP: "AS64499 SIXNET"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.Error != "" {
		t.Fatalf("IPv6-only fetch flagged an error: %q", info.Error)
	}
	if info.PublicIP != "" || info.PublicIPv6 != "2001:db8::1234" {
		t.Fatalf("ips = (%q, %q), want (\"\", \"2001:db8::1234\")", info.PublicIP, info.PublicIPv6)
	}
	if info.ISP != "AS64499 SIXNET" {
		t.Errorf("isp = %q, want the persisted same-IPv6 fallback AS64499 SIXNET", info.ISP)
	}
	if info.City != "Sixtown" || info.Country != "NL" {
		t.Errorf("geo from IPv6 = (%q, %q), want (Sixtown, NL)", info.City, info.Country)
	}
	if info.ExitUnavailable == "" {
		t.Error("ExitUnavailable not set - the doomed IPv4 trace would rerun forever")
	}
}

// The IPv6-only branch mirrors the ip4 one when the Cymru ISP lookup fails and
// no fallback holds this same IPv6: the snapshot must carry an Error so Loop
// retries at the faster errRetryStale cadence instead of sitting on a blank
// ISP for a full maxStale hour. No network.
func TestFetchIPv6OnlyISPBlankFlagsError(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	for _, tc := range []struct {
		name string
		last func() *Info
	}{
		{"no fallback", nil},
		{"fallback different IPv6", func() *Info { return &Info{PublicIPv6: "2001:db8::9999", ISP: "AS-OTHER"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.http = canned(500, "")
			m.LastKnownFn = tc.last
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			info := m.fetch(ctx)
			if info.PublicIPv6 != "2001:db8::1234" {
				t.Fatalf("ipv6 = %q, want 2001:db8::1234", info.PublicIPv6)
			}
			if info.ISP != "" {
				t.Fatalf("isp = %q, want blank (no usable fallback)", info.ISP)
			}
			if info.Error == "" {
				t.Error("Error empty - Loop won't retry the ISP at the faster cadence")
			}
		})
	}
}

// A dual-stack host that permanently loses IPv4 (moved to an IPv6-only network)
// must not keep its stale IPv4 identity forever. Within v6FlipAfter of the loss
// the veto holds: the carried IPv4 identity plus an Error. Once the IPv4 echo
// has been failing for v6FlipAfter while IPv6 keeps answering, fetch flips to
// the IPv6-only identity - a healthy snapshot with fresh IPv6-derived lookups
// (no reuse of the IPv4-derived ISP/geo) and ExitUnavailable set so the doomed
// IPv4 trace stops. An IPv4 answer restores the identity and resets the run.
// No network.
func TestFetchFlipsToIPv6OnlyAfterPersistentV4Loss(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	// Speed history holds the dual-stack identity: its IPv6 matches, so the
	// flipped fetch fills its ISP from it (Cymru fails under the cancelled ctx).
	m.LastKnownFn = func() *Info {
		return &Info{PublicIP: "203.0.113.5", PublicIPv6: "2001:db8::1234", ISP: "AS1403 EBOX"}
	}
	// prev: a dual-stack identity with speed history - the state that used to
	// veto the IPv6-only branch forever.
	m.mu.Lock()
	m.info = Info{PublicIP: "203.0.113.5", ISP: "AS1403 EBOX", City: "Oldtown",
		PublicIPv6: "2001:db8::1234", UpdatedAt: time.Now().Unix()}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Fresh loss: the veto carries the old identity forward, flagged as an error.
	info := m.fetch(ctx)
	if info.PublicIP != "203.0.113.5" || info.Error == "" {
		t.Fatalf("fresh loss = (ip %q, error %q), want the carried IPv4 identity with an error", info.PublicIP, info.Error)
	}
	// Age the run past the bound with a fresh last-miss stamp (continuous
	// misses, not a suspend gap): the next fetch must flip.
	m.mu.Lock()
	m.v4MissSince = time.Now().Add(-v6FlipAfter - time.Minute)
	m.v4MissLast = time.Now()
	m.mu.Unlock()
	info = m.fetch(ctx)
	if info.Error != "" {
		t.Fatalf("flipped fetch flagged an error: %q", info.Error)
	}
	if info.PublicIP != "" || info.PublicIPv6 != "2001:db8::1234" {
		t.Fatalf("flipped ips = (%q, %q), want (\"\", \"2001:db8::1234\")", info.PublicIP, info.PublicIPv6)
	}
	if info.City != "Sixtown" {
		t.Errorf("flipped city = %q, want the fresh IPv6 geo Sixtown, not the stale Oldtown", info.City)
	}
	if info.ExitUnavailable == "" {
		t.Error("ExitUnavailable not set after the flip - the doomed IPv4 trace would rerun forever")
	}
	// IPv4 answers again: the identity restores immediately and the run resets.
	ipv4Client = canned(200, "203.0.113.5")
	info = m.fetch(ctx)
	if info.PublicIP != "203.0.113.5" || info.Error != "" {
		t.Fatalf("recovered = (ip %q, error %q), want the IPv4 identity back with no error", info.PublicIP, info.Error)
	}
	m.mu.Lock()
	zero := m.v4MissSince.IsZero() && m.v4MissLast.IsZero()
	m.mu.Unlock()
	if !zero {
		t.Error("v4MissSince/v4MissLast not reset after IPv4 returned")
	}
}

// One IPv4 miss observed before a suspend/outage, then another right after
// recovery, is NOT sustained evidence: the gap between miss observations
// exceeds v4MissRunGap (fetches stopped in between), so the run restarts and
// the identity must NOT flip - even though the wall-clock span passes
// v6FlipAfter. Continuous misses (fresh last-miss stamp) past the bound still
// flip. No network.
func TestFetchStaleMissRunRestartsInsteadOfFlipping(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	m.LastKnownFn = func() *Info {
		return &Info{PublicIP: "203.0.113.5", PublicIPv6: "2001:db8::1234", ISP: "AS1403 EBOX"}
	}
	m.mu.Lock()
	m.info = Info{PublicIP: "203.0.113.5", ISP: "AS1403 EBOX", City: "Oldtown",
		PublicIPv6: "2001:db8::1234", UpdatedAt: time.Now().Unix()}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A single miss 20 minutes ago, then silence (suspend - no fetches at all),
	// then this miss: stale run, so restart it and keep the IPv4 identity.
	miss := time.Now().Add(-v6FlipAfter - 5*time.Minute)
	m.mu.Lock()
	m.v4MissSince, m.v4MissLast = miss, miss
	m.mu.Unlock()
	info := m.fetch(ctx)
	if info.PublicIP != "203.0.113.5" || info.Error == "" {
		t.Fatalf("post-gap miss = (ip %q, error %q), want the carried IPv4 identity with an error (no flip)", info.PublicIP, info.Error)
	}
	m.mu.Lock()
	restarted := m.v4MissSince
	m.mu.Unlock()
	if time.Since(restarted) > time.Minute {
		t.Fatalf("run not restarted: v4MissSince still %v old", time.Since(restarted))
	}
	// Continuous misses: run older than the bound, last miss recent - sustained
	// evidence, so the flip proceeds.
	m.mu.Lock()
	m.v4MissSince = time.Now().Add(-v6FlipAfter - time.Minute)
	m.v4MissLast = time.Now().Add(-time.Minute)
	m.mu.Unlock()
	info = m.fetch(ctx)
	if info.Error != "" {
		t.Fatalf("flipped fetch flagged an error: %q", info.Error)
	}
	if info.PublicIP != "" || info.PublicIPv6 != "2001:db8::1234" {
		t.Fatalf("sustained-miss ips = (%q, %q), want the flipped (\"\", \"2001:db8::1234\")", info.PublicIP, info.PublicIPv6)
	}
	if info.ExitUnavailable == "" {
		t.Error("ExitUnavailable not set after the flip")
	}
}

// A short IPv4 blip on a dual-stack host must NOT flip the identity: repeated
// fetches inside the v6FlipAfter window keep the carried IPv4 identity (the
// boot/PPPoE-renegotiation veto), and a blackout fetch where neither family
// answers is no evidence either way, so it leaves the run's start untouched.
// No network.
func TestFetchV4BlipDoesNotFlip(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(500, "")
	m.mu.Lock()
	m.info = Info{PublicIP: "203.0.113.5", ISP: "AS1403 EBOX", UpdatedAt: time.Now().Unix()}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := range 3 {
		if info := m.fetch(ctx); info.PublicIP != "203.0.113.5" || info.Error == "" {
			t.Fatalf("fetch %d = (ip %q, error %q), want the carried IPv4 identity with an error", i, info.PublicIP, info.Error)
		}
	}
	m.mu.Lock()
	started := m.v4MissSince
	m.mu.Unlock()
	ipv6Client = canned(500, "")
	m.fetch(ctx)
	m.mu.Lock()
	after := m.v4MissSince
	m.mu.Unlock()
	if !after.Equal(started) {
		t.Errorf("blackout fetch moved v4MissSince from %v to %v", started, after)
	}
}

// When the IP echo succeeds but the Cymru ISP lookup fails (blank ISP), fetch
// must fill the ISP from speed history for the SAME IP - not publish a blank
// one. The cancelled context fails the DNS-based Cymru/rDNS lookups instantly,
// while the canned ipv4Client still answers the IP echo. No network.
func TestFetchISPFallbackSameIP(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.5")
	ipv6Client = canned(500, "")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(500, "")
	m.LastKnownFn = func() *Info {
		return &Info{PublicIP: "203.0.113.5", ISP: "AS1403 EBOX"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.PublicIP != "203.0.113.5" {
		t.Fatalf("ip = %q, want 203.0.113.5", info.PublicIP)
	}
	if info.ISP != "AS1403 EBOX" {
		t.Fatalf("isp = %q, want the persisted fallback AS1403 EBOX", info.ISP)
	}
	if info.Error != "" {
		t.Errorf("error = %q, want empty (fallback filled the ISP)", info.Error)
	}
}

// When the IP echo succeeds, the Cymru ISP lookup fails, and there is no usable
// fallback (no LastKnownFn, or it holds a different IP), fetch must flag the
// snapshot with an Error so Loop retries at the faster errRetryStale cadence
// instead of sitting on a blank ISP for a full maxStale hour.
func TestFetchISPBlankFlagsError(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.5")
	ipv6Client = canned(500, "")

	for _, tc := range []struct {
		name string
		last func() *Info
	}{
		{"no fallback", nil},
		{"fallback different IP", func() *Info { return &Info{PublicIP: "198.51.100.9", ISP: "AS-OTHER"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
			m.http = canned(500, "")
			m.LastKnownFn = tc.last
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			info := m.fetch(ctx)
			if info.PublicIP != "203.0.113.5" {
				t.Fatalf("ip = %q, want 203.0.113.5", info.PublicIP)
			}
			if info.ISP != "" {
				t.Fatalf("isp = %q, want blank (no usable fallback)", info.ISP)
			}
			if info.Error == "" {
				t.Error("Error empty - Loop won't retry the ISP at the faster cadence")
			}
		})
	}
}

// A one-time rdns timeout must not lock the DNS-egress Host (brand label) blank
// for the process lifetime. When the resolver egress IP is unchanged and its
// Provider is known but the cached Host is blank, fetch must re-run the reverse
// lookup rather than copy the blank forward. ptrLookup is stubbed so no network
// is needed; the resolver egress lookup itself is stubbed via prev reuse.
func TestFetchDNSHostRetriedWhileBlank(t *testing.T) {
	oldPTR := ptrLookup
	defer func() { ptrLookup = oldPTR }()

	var mu sync.Mutex
	calls := map[string]int{}
	ptrLookup = func(_ context.Context, ip string) string {
		mu.Lock()
		defer mu.Unlock()
		calls[ip]++
		if ip == "9.9.9.9" {
			return "dns.example.net" // PTR now resolves fine
		}
		return ""
	}

	oldEgress := resolverEgress
	defer func() { resolverEgress = oldEgress }()
	resolverEgress = func(context.Context) string { return "9.9.9.9" }

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(500, "")
	// prev: same egress IP, Provider known, Host blank (rDNS failed once before).
	m.mu.Lock()
	m.info = Info{DNSUpstream: &DNSEntry{IP: "9.9.9.9", Provider: "AS19281 QUAD9", Host: ""}, UpdatedAt: time.Now().Unix()}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.DNSUpstream == nil {
		t.Fatal("DNSUpstream nil")
	}
	if info.DNSUpstream.Provider != "AS19281 QUAD9" {
		t.Errorf("provider = %q, want the reused AS19281 QUAD9", info.DNSUpstream.Provider)
	}
	if info.DNSUpstream.Host != "dns.example.net" {
		t.Fatalf("host = %q, want the re-fetched dns.example.net (blank Host must be retried)", info.DNSUpstream.Host)
	}
	mu.Lock()
	got := calls["9.9.9.9"]
	mu.Unlock()
	if got == 0 {
		t.Error("rdns was not retried for the blank Host")
	}
}

// rtFunc lets tests stub the Manager's HTTP transport with a canned response -
// no sockets, no network.
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func canned(status int, body string) *http.Client {
	return &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	})}
}

// A manual refresh right after a network switch (tethering, new Wi-Fi) often
// fails its first fetch while the link settles; RefreshNow must retry once
// after a short delay instead of bouncing the error at the user. Two failures
// still surface the error (exactly one retry).
func TestRefreshNowRetriesOnceAfterFailure(t *testing.T) {
	oldV4, oldV6, oldDelay := ipv4Client, ipv6Client, refreshRetryDelay
	defer func() { ipv4Client, ipv6Client, refreshRetryDelay = oldV4, oldV6, oldDelay }()
	refreshRetryDelay = time.Millisecond
	// Hermetic: fail every real DNS lookup (Team Cymru ASN, rDNS) instantly, so
	// the test never touches the network. The ISP sub-lookup therefore always
	// fails - the assertions below check the RETRY mechanic (second attempt,
	// echo recovered), not a fully-clean fetch.
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	defer func() { net.DefaultResolver = oldRes }()

	// First echo round fails (network still settling), second succeeds.
	var v4Calls atomic.Int32
	ipv4Client = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		if v4Calls.Add(1) == 1 {
			return nil, errors.New("network is unreachable")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("203.0.113.9")), Header: http.Header{}}, nil
	})}
	ipv6Client = canned(500, "")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL"}`)
	info := m.RefreshNow(context.Background())
	if info.PublicIP != "203.0.113.9" {
		t.Fatalf("retry should have recovered the echo: ip %q, error %q", info.PublicIP, info.Error)
	}
	if v4Calls.Load() < 2 {
		t.Fatalf("expected a second fetch attempt, got %d echo calls", v4Calls.Load())
	}

	// Persistent failure: exactly one retry, then the error surfaces.
	v4Calls.Store(0)
	ipv4Client = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		v4Calls.Add(1)
		return nil, errors.New("network is unreachable")
	})}
	m2 := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m2.http = canned(200, `{"success":true}`)
	if info := m2.RefreshNow(context.Background()); info.Error == "" {
		t.Fatal("persistent failure must still surface an error")
	}
	if v4Calls.Load() != 2 {
		t.Fatalf("want exactly 2 echo attempts (1 retry), got %d", v4Calls.Load())
	}
}
