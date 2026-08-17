package prober

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
)

// Covers the POSIX errno classification (dialErrno in dialerr_other.go). The
// Windows twin (dialerr_windows.go) mirrors these classes against the Winsock
// WSA errnos; the syscall.E* cases below also exercise its fallback path there.
func TestDialErrClass(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{context.DeadlineExceeded, "timeout"},
		{syscall.ECONNREFUSED, "refused"},
		{syscall.ENETUNREACH, "net_unreachable"},
		{syscall.EHOSTUNREACH, "host_unreachable"},
		{&net.DNSError{Err: "no such host", IsNotFound: true}, "dns"},
		{errors.New("something else"), "other"},
		// Real dials wrap the syscall in *net.OpError - errors.Is must still match.
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "refused"},
		{fmt.Errorf("dial tcp: %w", context.DeadlineExceeded), "timeout"},
	}
	for _, c := range cases {
		if got := DialErrClass(c.err); got != c.want {
			t.Errorf("DialErrClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func tr(name, family string, ok bool, ms int) TargetResult {
	return TargetResult{
		Target:  config.Target{Name: name, Family: family},
		OK:      ok,
		Latency: time.Duration(ms) * time.Millisecond,
	}
}

func TestAggregateEitherFamilyOnline(t *testing.T) {
	now := time.Unix(0, 0)

	t.Run("both families up", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", true, 6), tr("q4", "ipv4", true, 7),
			tr("c6", "ipv6", true, 8), tr("g6", "ipv6", true, 9), tr("q6", "ipv6", true, 10),
		})
		if !r.Online || !r.Families["ipv4"].Online || !r.Families["ipv6"].Online {
			t.Fatal("expected all online")
		}
	})

	t.Run("ipv6 down does not take overall offline", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", true, 6), tr("q4", "ipv4", false, 0),
			tr("c6", "ipv6", false, 0), tr("g6", "ipv6", false, 0), tr("q6", "ipv6", false, 0),
		})
		if !r.Online {
			t.Fatal("overall should stay online while IPv4 has quorum")
		}
		if !r.Families["ipv4"].Online {
			t.Fatal("ipv4 should be online (2 of 3)")
		}
		if r.Families["ipv6"].Online {
			t.Fatal("ipv6 should be offline (0 of 3)")
		}
	})

	t.Run("both families down -> offline", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", false, 0), tr("g4", "ipv4", false, 0),
			tr("c6", "ipv6", false, 0), tr("g6", "ipv6", false, 0),
		})
		if r.Online {
			t.Fatal("expected overall offline when both families are down")
		}
	})

	t.Run("single-stack (ipv4 only)", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", true, 6), tr("q4", "ipv4", true, 7),
		})
		if !r.Online {
			t.Fatal("ipv4-only host should be online")
		}
		if _, ok := r.Families["ipv6"]; ok {
			t.Fatal("no ipv6 family should be present when not probed")
		}
	})
}

// A family needs a strict majority of its targets up (SUM(success)*2 > COUNT):
// an exactly-half round is NOT online, so a 1-of-2 or 2-of-4 tie reads as down.
// This is the live event-writing path's copy of the quorum rule at prober.go:221.
func TestAggregateStrictMajority(t *testing.T) {
	now := time.Unix(0, 0)

	t.Run("half is not a majority (1 of 2)", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", false, 0),
		})
		if r.Families["ipv4"].Online {
			t.Fatal("1 of 2 is a tie, not a strict majority; ipv4 must be offline")
		}
		if r.Online {
			t.Fatal("overall must be offline when the only family is a tie")
		}
	})

	t.Run("half is not a majority (2 of 4)", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", true, 6),
			tr("q4", "ipv4", false, 0), tr("o4", "ipv4", false, 0),
		})
		if r.Families["ipv4"].Online {
			t.Fatal("2 of 4 is a tie, not a strict majority; ipv4 must be offline")
		}
	})

	t.Run("one past half is a majority (2 of 3)", func(t *testing.T) {
		r := aggregate(now, []TargetResult{
			tr("c4", "ipv4", true, 5), tr("g4", "ipv4", true, 6), tr("q4", "ipv4", false, 0),
		})
		if !r.Families["ipv4"].Online {
			t.Fatal("2 of 3 is a strict majority; ipv4 must be online")
		}
	})
}

// Targets of a disabled family must not be dialed and must not contribute a
// family to the result (the live IPv6 on/off/auto setting).
func TestFamilyEnabledFnSkipsDisabledFamily(t *testing.T) {
	p := New([]config.Target{
		{Name: "v4", Network: "tcp4", Address: "127.0.0.1:1", Family: config.IPv4},
		{Name: "v6", Network: "tcp6", Address: "[::1]:1", Family: config.IPv6},
	}, 500*time.Millisecond)
	p.FamilyEnabledFn = func(f string) bool { return f != config.IPv6 }

	r := p.Probe(context.Background(), time.Unix(0, 0))
	if len(r.Targets) != 1 || r.Targets[0].Target.Name != "v4" {
		t.Fatalf("expected only the v4 target to be dialed, got %d results", len(r.Targets))
	}
	if _, ok := r.Families[config.IPv6]; ok {
		t.Fatal("ipv6 family should be absent when disabled")
	}
}

// A filter that excludes everything would read as a global outage; Probe must
// fail open and dial the full set instead.
func TestFamilyEnabledFnFailsOpen(t *testing.T) {
	p := New([]config.Target{
		{Name: "v4", Network: "tcp4", Address: "127.0.0.1:1", Family: config.IPv4},
	}, 500*time.Millisecond)
	p.FamilyEnabledFn = func(string) bool { return false }

	r := p.Probe(context.Background(), time.Unix(0, 0))
	if len(r.Targets) != 1 {
		t.Fatalf("expected the full target set to be probed, got %d results", len(r.Targets))
	}
}

func dualStackTargets() []config.Target {
	return []config.Target{
		{Name: "v4", Network: "tcp4", Address: "127.0.0.1:1", Family: config.IPv4},
		{Name: "v6", Network: "tcp6", Address: "[::1]:1", Family: config.IPv6},
	}
}

// An explicitly-off family must be excluded even from the fail-open set: with
// auto-detection filtering everything out, Probe fails open into the eligible
// (non-off) targets only - `-ipv4 off` must mean off.
func TestFamilyOffFnExcludedFromFailOpen(t *testing.T) {
	p := New(dualStackTargets(), 500*time.Millisecond)
	p.FamilyEnabledFn = func(string) bool { return false }          // auto-detect: nothing live
	p.FamilyOffFn = func(f string) bool { return f == config.IPv4 } // operator: ipv4 off

	r := p.Probe(context.Background(), time.Unix(0, 0))
	if len(r.Targets) != 1 || r.Targets[0].Target.Name != "v6" {
		t.Fatalf("fail-open should dial only the v6 target, got %+v", r.Targets)
	}
	if _, ok := r.Families[config.IPv4]; ok {
		t.Fatal("an explicitly-off family must not appear in the result")
	}
}

// With every family explicitly off, nothing is dialed at all (main stops the
// loop in that mode, but the prober itself must never resurrect off targets).
func TestFamilyOffFnAllOffDialsNothing(t *testing.T) {
	p := New(dualStackTargets(), 500*time.Millisecond)
	p.FamilyEnabledFn = func(string) bool { return false }
	p.FamilyOffFn = func(string) bool { return true }

	r := p.Probe(context.Background(), time.Unix(0, 0))
	if len(r.Targets) != 0 {
		t.Fatalf("expected no dials with every family off, got %+v", r.Targets)
	}
	if len(r.Families) != 0 || r.Online {
		t.Fatalf("expected an empty offline result, got %+v", r)
	}
}

// dnsStub runs a minimal in-process UDP DNS server. Every query gets a reply
// carrying the same ID and question, no answer records, and the given RCODE;
// respond=false swallows queries so the lookup can only time out. The returned
// queries func reports the question name of every query the server received, in
// order, so a test can count the wire queries one probe actually costs.
func dnsStub(t *testing.T, rcode byte, respond bool) (addr string, queries func() []string) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	var (
		mu   sync.Mutex
		seen []string
	)
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // listener closed by Cleanup
			}
			if n >= 12 {
				mu.Lock()
				seen = append(seen, dnsQName(buf[:n]))
				mu.Unlock()
			}
			if !respond || n < 12 {
				continue
			}
			// The question section: length-prefixed labels up to the 0 byte, then
			// 4 bytes of type+class. Truncate there so the reply carries only the
			// echoed question (drops the query's EDNS0 additional record).
			end := 12
			for end < n && buf[end] != 0 {
				end += int(buf[end]) + 1
			}
			end += 1 + 4
			if end > n {
				continue
			}
			resp := make([]byte, end)
			copy(resp, buf[:end])
			resp[2] = 0x81         // QR=1 (response), RD kept
			resp[3] = 0x80 | rcode // RA=1 plus the response code
			resp[6], resp[7] = 0, 0
			resp[8], resp[9] = 0, 0
			resp[10], resp[11] = 0, 0 // ANCOUNT/NSCOUNT/ARCOUNT all zero
			pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String(), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// dnsQName decodes the question name of a DNS query as dotted labels. On the
// wire every name is rooted, so the trailing dot here says nothing about how the
// caller spelled it; what distinguishes a rooted lookup is that the resolver
// asks for ONE such name instead of also appending its search domain.
func dnsQName(msg []byte) string {
	var b strings.Builder
	for i := 12; i < len(msg); {
		l := int(msg[i])
		if l == 0 {
			return b.String()
		}
		if l > 63 || i+1+l > len(msg) {
			break
		}
		b.Write(msg[i+1 : i+1+l])
		b.WriteByte('.')
		i += 1 + l
	}
	return "<malformed>"
}

// stubResolver points net.DefaultResolver (what ResolveTime deliberately uses)
// at the in-process stub for the test's duration.
func stubResolver(t *testing.T, addr string) {
	t.Helper()
	old := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		},
	}
	t.Cleanup(func() { net.DefaultResolver = old })
}

// The cache-busting random label never exists, so NXDOMAIN is the healthy
// answer: the resolver responded and the resolution path works.
func TestResolveTimeNXDOMAINIsHealthy(t *testing.T) {
	addr, _ := dnsStub(t, 3, true) // RCODE 3 = NXDOMAIN
	stubResolver(t, addr)
	_, ok, err := ResolveTime(context.Background())
	if !ok || err != nil {
		t.Fatalf("NXDOMAIN: ok=%v err=%v, want ok=true err=nil", ok, err)
	}
}

// The trailing dot on the probe name is what keeps one probe to one lookup, and
// it has to hold on every host, which is more than the wire-count test below can
// check. Where the host's search list contributes no name - no resolv.conf
// search line and a hostname with no domain part for Go to fall back on
// (dnsDefaultSearch, GOROOT/src/net/dnsconfig_unix.go) - nameList returns a
// single name for a relative name too, so that test counts 2 queries whether the
// dot is there or not. Measured here: with the dot deleted from probeName and
// the probe domain stretched so the search suffix no longer fits nameList's
// 254-byte cap, the wire-count test passed.
//
// This test reads probeName's own string. TestResolveTimeLooksUpARootedName
// reads the name ResolveTime actually hands the resolver, which is the property
// that matters and the one a rewrite that stops calling probeName would break.
func TestProbeNameIsRooted(t *testing.T) {
	name := probeName()
	if !strings.HasSuffix(name, ".") {
		t.Fatalf("probe name %q is not rooted: a resolver takes a relative name as a hint, re-asks it with the host's search domain appended, and so doubles both the probe's query count and the lookup ResolveTime is timing", name)
	}
	base := "." + dnsProbeDomain + "."
	label, ok := strings.CutSuffix(name, base)
	if !ok {
		t.Fatalf("probe name %q does not end in %q, so it is not a name under the probe domain at all", name, base)
	}
	if !strings.HasPrefix(label, "pp") || strings.Contains(label, ".") {
		t.Fatalf("probe name %q: want a single pp-prefixed random label in front of %q, got %q", name, dnsProbeDomain, label)
	}
}

// One probe must put exactly one question name on the wire. The A/AAAA pair is
// unavoidable for network "ip" (LookupHost); a second pair for a search-domain
// spelling of the same name is not. What that costs at the shipped defaults: the
// DNS probe runs at most once per probe round (the dnsBusy CompareAndSwap in
// Monitor.round, internal/monitor/monitor.go, launches nothing while the previous
// lookup is still in flight), and the default probe interval is 5s (shippedDefaults'
// def.Latency, internal/settings/settings.go), so up to 86400/5 = 17,280 probes a
// day. Rooted, that ceiling is 34,560 wire queries; unrooted on a host with a
// search domain it is 69,120, and the extra lookup is doomed.
//
// Only a host whose search list contributes a name can fail this test; see
// TestResolveTimeLooksUpARootedName for the guard that holds on any host.
func TestResolveTimeAsksOneNamePerProbe(t *testing.T) {
	addr, queries := dnsStub(t, 3, true) // RCODE 3 = NXDOMAIN, the healthy answer
	stubResolver(t, addr)

	if _, ok, err := ResolveTime(context.Background()); !ok || err != nil {
		t.Fatalf("probe: ok=%v err=%v, want ok=true err=nil", ok, err)
	}

	qs := queries()
	if len(qs) != 2 {
		t.Errorf("probe sent %d wire queries, want 2 (the A/AAAA pair): %q", len(qs), qs)
	}
	want := "." + dnsProbeDomain + "."
	for _, q := range qs {
		if !strings.HasSuffix(q, want) {
			t.Errorf("probe asked %q, which is not a random label under %q: the name went out relative and the resolver appended its search domain (all queries: %q)", q, want, qs)
		}
	}
}

// captureLookupNames records the exact name string each ResolveTime call hands
// the resolver, in order, for the test's duration. The wrapper still performs
// the real lookup, so the probe runs end to end against whatever resolver
// stubResolver installed - the capture observes the path, it does not replace it.
func captureLookupNames(t *testing.T) func() []string {
	t.Helper()
	var (
		mu   sync.Mutex
		sent []string
	)
	prev := lookupHost
	lookupHost = func(ctx context.Context, name string) ([]string, error) {
		mu.Lock()
		sent = append(sent, name)
		mu.Unlock()
		return prev(ctx, name)
	}
	t.Cleanup(func() { lookupHost = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// The dot has to survive on the name ResolveTime hands the resolver, which is a
// stricter claim than probeName returning a rooted string: a ResolveTime that
// builds its own name inline leaves probeName untouched, so TestProbeNameIsRooted
// keeps passing. The wire-count test cannot see that either on a host with an
// empty search list, because Go roots every name before it reaches the wire and a
// relative name then produces the same single query (dnsConfig.nameList). So read
// the argument itself: that depends on no resolver configuration at all. Measured
// here, with the probe domain stretched past nameList's 254-byte cap to emulate a
// host whose search list contributes nothing: a ResolveTime that built the name
// inline and unrooted, leaving probeName's dot alone, left TestProbeNameIsRooted
// and the wire-count test both passing, and only this test failed.
//
// The last check ties the captured argument to the wire, so a seam that reported
// one name while a different one went out would not pass either.
func TestResolveTimeLooksUpARootedName(t *testing.T) {
	addr, queries := dnsStub(t, 3, true) // RCODE 3 = NXDOMAIN, the healthy answer
	stubResolver(t, addr)
	sent := captureLookupNames(t)

	if _, ok, err := ResolveTime(context.Background()); !ok || err != nil {
		t.Fatalf("probe: ok=%v err=%v, want ok=true err=nil", ok, err)
	}

	names := sent()
	if len(names) != 1 {
		t.Fatalf("one probe made %d resolver lookups %q, want exactly 1: ResolveTime no longer resolves through lookupHost, so nothing here can see the name it sends", len(names), names)
	}
	name := names[0]
	if !strings.HasSuffix(name, ".") {
		t.Fatalf("ResolveTime looked up %q, which is not rooted: the resolver takes a relative name as a hint, re-asks it with the host's search domain appended, and so doubles both the probe's query count and the lookup ResolveTime is timing", name)
	}
	qs := queries()
	if len(qs) == 0 {
		t.Fatalf("the stub resolver saw no query, so %q never reached the wire and this test proved nothing", name)
	}
	for _, q := range qs {
		if q != name {
			t.Errorf("ResolveTime handed the resolver %q but the wire carried %q, so the captured name is not the one that goes out (all queries: %q)", name, q, qs)
		}
	}
}

// A repeated name is one a resolver can answer out of the entry it cached for the
// previous probe, so a probe that reused a name would time a cache hit rather than
// a resolution. Pinned on the name ResolveTime actually sends, for the same reason
// as the test above: a rewrite that stops calling probeName takes the fresh label
// with it while probeName itself still reads correctly.
func TestResolveTimeNameIsFreshEachProbe(t *testing.T) {
	addr, _ := dnsStub(t, 3, true) // RCODE 3 = NXDOMAIN, the healthy answer
	stubResolver(t, addr)
	sent := captureLookupNames(t)

	for i := range 2 {
		if _, ok, err := ResolveTime(context.Background()); !ok || err != nil {
			t.Fatalf("probe %d: ok=%v err=%v, want ok=true err=nil", i, ok, err)
		}
	}

	names := sent()
	if len(names) != 2 {
		t.Fatalf("two probes made %d resolver lookups %q, want one each", len(names), names)
	}
	if names[0] == names[1] {
		t.Fatalf("two probes both asked %q: the label is not redrawn per probe, so a resolver holding the first answer can serve the second from cache instead of resolving it", names[0])
	}
}

// SERVFAIL means the resolver could not resolve: unhealthy, with the error
// returned so the caller can classify why.
func TestResolveTimeSERVFAILIsUnhealthy(t *testing.T) {
	addr, _ := dnsStub(t, 2, true) // RCODE 2 = SERVFAIL
	stubResolver(t, addr)
	_, ok, err := ResolveTime(context.Background())
	if ok || err == nil {
		t.Fatalf("SERVFAIL: ok=%v err=%v, want ok=false with an error", ok, err)
	}
}

// A resolver that never answers must fail within the probe timeout.
func TestResolveTimeTimeoutIsUnhealthy(t *testing.T) {
	addr, _ := dnsStub(t, 0, false) // queries are swallowed
	stubResolver(t, addr)
	oldTimeout := dnsProbeTimeout
	dnsProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { dnsProbeTimeout = oldTimeout })

	start := time.Now()
	_, ok, err := ResolveTime(context.Background())
	if ok || err == nil {
		t.Fatalf("timeout: ok=%v err=%v, want ok=false with an error", ok, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("lookup took %v; the shrunk %v timeout was not honored", elapsed, dnsProbeTimeout)
	}
}
