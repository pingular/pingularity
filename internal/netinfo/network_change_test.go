package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// cymruRecorder stands in for every Team Cymru resolver: it answers nothing (a
// timeout, as a filtered port 53 produces) and records each query under a
// lock, because the configured-resolver labelling asks from one goroutine per
// resolver the host has set - and those are the host's real resolvers, so a
// test counts the query for ONE name rather than the total.
type cymruRecorder struct {
	mu sync.Mutex
	qs []string
}

func (c *cymruRecorder) record(_ context.Context, q string) ([]string, error) {
	c.mu.Lock()
	c.qs = append(c.qs, q)
	c.mu.Unlock()
	return nil, errTimeout
}

// asked counts the queries recorded for name q.
func (c *cymruRecorder) asked(q string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, x := range c.qs {
		if x == q {
			n++
		}
	}
	return n
}

// hermetic fails every lookup that rides net.DefaultResolver (rDNS, the exit
// target) instantly, answers nothing for the resolver egress echo and the
// reverse lookup, and routes Team Cymru to a failing recorder - so a test can
// say whether the ISP lookup ran without a single packet leaving the machine.
func hermetic(t *testing.T) *cymruRecorder {
	t.Helper()
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	oldEgress, oldPTR := resolverEgress, ptrLookup
	resolverEgress = func(context.Context) string { return "" }
	ptrLookup = func(context.Context, string) string { return "" }
	rec := &cymruRecorder{}
	oldSys, oldFB := cymruSystemTXT, cymruFallbackTXT
	cymruSystemTXT = rec.record
	cymruFallbackTXT = []func(context.Context, string) ([]string, error){rec.record, rec.record}
	cymruMu.Lock()
	cymruSuspectUntil = time.Time{}
	cymruMu.Unlock()
	t.Cleanup(func() {
		net.DefaultResolver, resolverEgress, ptrLookup = oldRes, oldEgress, oldPTR
		cymruSystemTXT, cymruFallbackTXT = oldSys, oldFB
		cymruMu.Lock()
		cymruSuspectUntil = time.Time{}
		cymruMu.Unlock()
	})
	return rec
}

// A public IP that has not changed keeps everything derived from it. The ISP
// lookup fails on its own (Team Cymru unreachable through the resolver, a
// prefix with no origin announced) and the snapshot is then flagged "isp lookup
// failed" and retried every errRetryStale. Each retry used to fall through to
// the IP-change path, which re-runs geo and rDNS with no guard: one failed
// round-trip published an empty city at 0,0 - and autoOrigins drops the ISP
// origin from auto server selection on a zero coordinate - while the geo
// providers were asked again on every retry for an address whose placement was
// already known. Only the lookup that failed is worth retrying, and once it has
// answered nothing is asked again. No network.
func TestFetchKeepsCachedGeoWhileOnlyTheISPLookupFails(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.5")
	ipv6Client = canned(500, "")
	cymru := hermetic(t)
	origin := cymruOriginQuery("203.0.113.5") // the ISP lookup's own query, apart from the resolver labelling
	ctx := context.Background()

	// Fetch 1: the geo provider answers, Cymru does not.
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Sixtown","country_code":"NL","latitude":52.37,"longitude":4.9}`)
	first := m.fetch(ctx)
	if first.ISP != "" || first.Error != "isp lookup failed" {
		t.Fatalf("setup: ISP=%q Error=%q, want a blank ISP flagged \"isp lookup failed\"", first.ISP, first.Error)
	}
	if first.City != "Sixtown" || first.Lat != 52.37 {
		t.Fatalf("setup: geo not cached (city=%q lat=%v)", first.City, first.Lat)
	}
	first.Hostname = "cust-5.example.net" // as an earlier reverse lookup would have left it
	m.mu.Lock()
	m.info = first
	m.mu.Unlock()

	// Fetch 2, the errRetryStale retry: same IP, the geo providers down this
	// time and the reverse lookup failing.
	geo := &countingGeo{body: `{"success":false}`}
	m.http = &http.Client{Transport: geo}
	cymruBefore := cymru.asked(origin)
	second := m.fetch(ctx)
	if second.PublicIP != "203.0.113.5" {
		t.Fatalf("public IP changed under the test: %q", second.PublicIP)
	}
	if second.City != "Sixtown" || second.Country != "NL" {
		t.Errorf("city/country after a failed geo round-trip on an unchanged IP = (%q, %q), want the cached (Sixtown, NL)",
			second.City, second.Country)
	}
	if second.Lat != 52.37 || second.Lon != 4.9 {
		t.Errorf("coordinate after a failed geo round-trip on an unchanged IP = (%v, %v), want the cached (52.37, 4.9); "+
			"a zero coordinate drops the ISP origin from auto server selection", second.Lat, second.Lon)
	}
	if second.Hostname != "cust-5.example.net" {
		t.Errorf("hostname after a failed reverse lookup on an unchanged IP = %q, want the cached cust-5.example.net", second.Hostname)
	}
	if geo.count() != 0 {
		t.Errorf("geo queries = %d on an unchanged IP whose placement is cached, want 0 - the providers were being "+
			"asked again on every retry of the ISP lookup", geo.count())
	}
	if cymru.asked(origin) == cymruBefore {
		t.Error("the ISP lookup was not retried - it is the one lookup that had failed")
	}
	if second.Error != "isp lookup failed" {
		t.Errorf("Error = %q, want \"isp lookup failed\" still (Cymru is still failing)", second.Error)
	}

	// Fetch 3: the ISP has since been found. With the whole identity cached for
	// this IP nothing is looked up at all - Cymru included.
	second.ISP = "AS64496 Example Telecom"
	m.mu.Lock()
	m.info = second
	m.mu.Unlock()
	cymruBefore = cymru.asked(origin)
	third := m.fetch(ctx)
	if third.ISP != "AS64496 Example Telecom" || third.City != "Sixtown" || third.Lat != 52.37 {
		t.Fatalf("cached identity not reused: isp=%q city=%q lat=%v", third.ISP, third.City, third.Lat)
	}
	if n := cymru.asked(origin) - cymruBefore; n != 0 {
		t.Errorf("the ISP lookup ran %d more times with the ISP already cached for this IP, want 0", n)
	}
	if geo.count() != 0 {
		t.Errorf("geo queries = %d with the whole identity cached, want 0", geo.count())
	}
}
