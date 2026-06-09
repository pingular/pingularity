package netinfo

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// C-20: the refresh-ordering contract. A later-STARTED refresh (higher generation)
// outranks an earlier one, so once it publishes, the older/slower one is refused -
// it can't clobber the newer snapshot when it finally returns. A refresh's own
// several effects (m.info, then the exit patch) still pass while it stays current.
func TestGenerationPublishOrdering(t *testing.T) {
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	g1 := m.nextGen()
	g2 := m.nextGen()
	if g2 <= g1 {
		t.Fatalf("nextGen not monotonic: g1=%d g2=%d", g1, g2)
	}
	if !m.claimPublish(g2) {
		t.Fatal("newer generation g2 should be allowed to publish")
	}
	if m.claimPublish(g1) {
		t.Fatal("older, slower generation g1 must not publish after g2 already did")
	}
	if !m.claimPublish(g2) {
		t.Fatal("current generation must be able to commit a second effect (info, then the exit patch)")
	}
}

// C-58: parseChaosTXT must validate the response against our fixed query - a
// mismatched transaction id, a query masquerading as a response, the wrong
// question count, a question that doesn't echo version.bind, or the wrong QCLASS
// must all be ignored rather than trusted as the resolver's answer. A genuine
// response still parses.
func TestParseChaosTXTRejectsForeignResponse(t *testing.T) {
	build := func() []byte {
		// header: our id 0x1234, flags QR+RD+RA, QD=1 AN=1 NS=0 AR=0.
		b := []byte{0x12, 0x34, 0x81, 0x80, 0, 1, 0, 1, 0, 0, 0, 0}
		b = append(b, chaosVersionQuestion...)
		// answer: name pointer -> 0x0c, TYPE TXT, CLASS CHAOS, TTL 0, RDLENGTH, RDATA.
		b = append(b, 0xc0, 0x0c, 0x00, 0x10, 0x00, 0x03, 0, 0, 0, 0)
		txt := "dnsmasq"
		rd := append([]byte{byte(len(txt))}, txt...)
		b = append(b, byte(len(rd)>>8), byte(len(rd)))
		b = append(b, rd...)
		return b
	}
	if got := parseChaosTXT(build()); got != "dnsmasq" {
		t.Fatalf("valid response = %q, want dnsmasq (a legit answer was rejected)", got)
	}
	cases := []struct {
		name   string
		mutate func([]byte)
	}{
		{"wrong transaction id", func(b []byte) { b[1] = 0x35 }},
		{"query not response (QR=0)", func(b []byte) { b[2] &^= 0x80 }},
		{"qdcount not one", func(b []byte) { b[5] = 2 }},
		{"question name mismatch", func(b []byte) { b[13] = 'x' }}, // corrupt the 'v' of "version"
		{"wrong qclass (IN not CHAOS)", func(b []byte) { b[12+len(chaosVersionQuestion)-1] = 0x01 }},
	}
	for _, c := range cases {
		b := build()
		c.mutate(b)
		if got := parseChaosTXT(b); got != "" {
			t.Errorf("%s: parseChaosTXT trusted a foreign response = %q, want \"\"", c.name, got)
		}
	}
}

// C-55: a bare three-letter code is weak evidence (several collide with words -
// "sea", "den", "van", "was"), so cityFromRDNS must raise a low-confidence marker
// while still returning the city; an indexed code ("fra10") is stronger and must
// not raise it. The city string is unchanged either way (display only, zero coords).
func TestCityFromRDNSLowConfidenceMarker(t *testing.T) {
	stats.ResetForTest()
	if got := cityFromRDNS("core.sea.example.net"); got != "Seattle" {
		t.Fatalf("cityFromRDNS(bare sea) = %q, want Seattle", got)
	}
	if n := stats.Lifetime().Counters["netinfo.rdns_city_lowconf"]; n != 1 {
		t.Fatalf("bare three-letter token: lowconf counter = %d, want 1", n)
	}
	stats.ResetForTest()
	if got := cityFromRDNS("ae1-cr2.fra10.isp.net"); got != "Frankfurt" {
		t.Fatalf("cityFromRDNS(indexed fra10) = %q, want Frankfurt", got)
	}
	if n := stats.Lifetime().Counters["netinfo.rdns_city_lowconf"]; n != 0 {
		t.Fatalf("indexed token fra10: lowconf counter = %d, want 0 (stronger evidence)", n)
	}
}

// C-21 (part two): a trace started on the OLD network must not overwrite the
// deliberate IP-change cache-bust when it finally lands. The bust bumps traceGen
// mid-trace; at commit the trace sees the generation moved and drops its result,
// leaving the cache cleared so the next caller re-traces the current network.
func TestCachedExitDropsOutOfGenerationTrace(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	stubTrace(t, func(context.Context, [4]byte, int, time.Duration) ([]tHop, error) {
		close(entered)
		<-release // hold the trace in flight, against the OLD network
		return []tHop{{TTL: 1, IP: "10.0.0.1"}, {TTL: 2, IP: "not-an-ip"}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(404, "")
	m.ExitTargetFn = func() string { return "1.1.1.1" }

	got := make(chan *ExitInfo, 1)
	go func() { got <- m.cachedExit(context.Background(), "1403") }()
	<-entered
	// The IP-change bust lands mid-trace (as Refresh does): clear the cache and
	// bump the generation the in-flight trace was started under.
	m.traceMu.Lock()
	m.traceAt, m.exit, m.tracedFor = time.Time{}, nil, ""
	m.traceGen++
	m.traceMu.Unlock()
	close(release)
	<-got

	m.traceMu.Lock()
	exit, at := m.exit, m.traceAt
	m.traceMu.Unlock()
	if exit != nil {
		t.Errorf("out-of-generation trace overwrote the cache-bust: %+v", exit)
	}
	if !at.IsZero() {
		t.Error("out-of-generation trace re-stamped traceAt, hiding the bust and suppressing a re-trace")
	}
}

// C-21 (part one): while a trace toward target A is in flight, a caller wanting a
// newly-selected target B must NOT be handed A's path by the single-flight waiter.
// After waiting it re-validates the cache against its own target and traces B.
func TestCachedExitWaiterRetracesForNewTarget(t *testing.T) {
	var mu sync.Mutex
	var traced [][4]byte
	entered := make(chan struct{})
	release := make(chan struct{})
	stubTrace(t, func(_ context.Context, dst [4]byte, _ int, _ time.Duration) ([]tHop, error) {
		mu.Lock()
		traced = append(traced, dst)
		first := len(traced) == 1
		mu.Unlock()
		if first {
			close(entered)
			<-release // hold trace A (target 9.9.9.9) in flight
		}
		return []tHop{{TTL: 1, IP: "10.0.0.1"}, {TTL: 2, IP: "not-an-ip"}}, nil
	})
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(404, "")
	var tmu sync.Mutex
	target := "9.9.9.9"
	m.ExitTargetFn = func() string {
		tmu.Lock()
		defer tmu.Unlock()
		return target
	}

	go m.cachedExit(context.Background(), "1403") // caller 1 -> trace A (9.9.9.9)
	<-entered
	tmu.Lock()
	target = "8.8.4.4" // the exit target changes while A is in flight
	tmu.Unlock()

	entered2 := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(entered2)
		m.cachedExit(context.Background(), "1403") // caller 2 -> wants B (8.8.4.4)
		close(done)
	}()
	<-entered2
	for i := 0; i < 200; i++ { // let caller 2 reach the single-flight waiter
		runtime.Gosched()
	}
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	var sawA, sawB bool
	for _, d := range traced {
		sawA = sawA || d == [4]byte{9, 9, 9, 9}
		sawB = sawB || d == [4]byte{8, 8, 4, 4}
	}
	if !sawA || !sawB {
		t.Fatalf("traced %v, want both 9.9.9.9 and 8.8.4.4 - a waiter for the new target must re-trace, not return the in-flight old-target result", traced)
	}
}
