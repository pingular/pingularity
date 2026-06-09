package netinfo

import (
	"context"
	"strings"
	"testing"
)

// ipmapGeo parses RIPE IPmap's "best" response into raw city/country/coords.
// ipmapLoc now wraps it, so this one canned test covers both. Uses the canned
// transport from fetch_fallback_test.go - no network.
func TestIpmapGeoCanned(t *testing.T) {
	m := &Manager{http: canned(200, `{"location":{"cityName":"London","countryCodeAlpha2":"GB","latitude":51.5,"longitude":-0.12}}`)}
	city, country, lat, lon, st := ipmapGeo(context.Background(), m, "1.2.3.4")
	if st != ipmapOK || city != "London" || country != "GB" || lat != 51.5 || lon != -0.12 {
		t.Fatalf("ipmapGeo = (%q, %q, %v, %v, %d)", city, country, lat, lon, st)
	}
	// ipmapLoc joins "City, CC".
	if loc, _, _, ok := ipmapLoc(context.Background(), m, "1.2.3.4"); !ok || loc != "London, GB" {
		t.Fatalf("ipmapLoc = %q, %v; want \"London, GB\"", loc, ok)
	}
	// 404 (and other non-200) is a transient error; a 200 with no location is a
	// permanent gap for that IP - the egress fallback treats them differently.
	if _, _, _, _, st := ipmapGeo(context.Background(), &Manager{http: canned(404, "")}, "1.2.3.4"); st != ipmapError {
		t.Errorf("404 should be ipmapError, got %d", st)
	}
	if _, _, _, _, st := ipmapGeo(context.Background(), &Manager{http: canned(200, `{"location":{}}`)}, "1.2.3.4"); st != ipmapNoData {
		t.Errorf("empty location should be ipmapNoData, got %d", st)
	}
}

// ispBoundary is the heart of exit-router detection; it was inlined in
// discoverExit (raw-socket, network-gated) and so unprotected. This pins the
// walk: ISP-ASN seeding, the private-stranded-exit fixup, and IXP-LAN handoff.
func TestISPBoundary(t *testing.T) {
	hop := func(ips ...string) []tHop {
		h := make([]tHop, len(ips))
		for i, ip := range ips {
			h[i] = tHop{IP: ip}
		}
		return h
	}
	cases := []struct {
		name               string
		hops               []tHop
		asns               []string
		ourASN             string
		wantExit, wantNext int
	}{
		{"private -> ISP -> handoff", hop("192.168.1.1", "203.0.113.1", "8.8.8.8"), []string{"", "1403", "15169"}, "1403", 1, 2},
		{"empty ourASN seeds from first public hop", hop("192.168.1.1", "203.0.113.1", "8.8.8.8"), []string{"", "1403", "15169"}, "", 1, 2},
		{"private-stranded exit is dropped", hop("192.168.1.1", "8.8.8.8"), []string{"", "15169"}, "1403", -1, 1},
		{"IXP LAN (no origin AS) is the handoff", hop("192.168.1.1", "203.0.113.1", "80.81.192.1", "8.8.8.8"), []string{"", "1403", "", "15169"}, "1403", 1, 2},
		{"ISP-internal private hop keeps the public exit", hop("192.168.1.1", "203.0.113.1", "10.20.0.1", "8.8.8.8"), []string{"", "1403", "", "15169"}, "1403", 1, 3},
		{"direct public first hop in ISP", hop("203.0.113.1", "8.8.8.8"), []string{"1403", "15169"}, "1403", 0, 1},
		{"all inside -> no handoff", hop("192.168.1.1", "203.0.113.1"), []string{"", "1403"}, "1403", 1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, n := ispBoundary(c.hops, c.asns, c.ourASN)
			if e != c.wantExit || n != c.wantNext {
				t.Errorf("ispBoundary = (exit %d, next %d), want (exit %d, next %d)", e, n, c.wantExit, c.wantNext)
			}
		})
	}
}

// handoffIsDest guards the Docker Desktop failure mode: no exit router, and the
// "handoff" is really the trace's own destination echoing back. It must fire only
// then - a real transit handoff, or any run that found an exit router, is kept.
func TestHandoffIsDest(t *testing.T) {
	hop := func(ips ...string) []tHop {
		h := make([]tHop, len(ips))
		for i, ip := range ips {
			h[i] = tHop{IP: ip}
		}
		return h
	}
	const dst = "1.1.1.1"
	cases := []struct {
		name             string
		hops             []tHop
		exitIdx, nextIdx int
		want             bool
	}{
		{"docker: only local hops + destination", hop("192.168.65.1", dst), -1, 1, true},
		{"real transit handoff is not the destination", hop("192.168.1.1", "203.0.113.1"), -1, 1, false},
		{"exit router found, keep even if handoff is destination", hop("203.0.113.1", dst), 0, 1, false},
		{"no handoff at all", hop("192.168.1.1"), -1, -1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := handoffIsDest(c.hops, c.exitIdx, c.nextIdx, dst); got != c.want {
				t.Errorf("handoffIsDest = %v, want %v", got, c.want)
			}
		})
	}
}

// These parsers run on untrusted DNS/router-name data, so a panic would crash
// the netinfo refresh - an availability risk worth fuzzing.
func FuzzCityFromRDNS(f *testing.F) {
	for _, s := range []string{"", "ae1.cr2.fra10.de.example.net", "host-1-2-3-4.lon1.isp.com", "192.168.1.1", "....", "a.b-c_d", "---"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if got := cityFromRDNS(name); len(got) > 128 {
			t.Errorf("cityFromRDNS(%q) returned an absurdly long value", name)
		}
	})
}

func FuzzPickCymruASN(f *testing.F) {
	for _, s := range []string{
		"13335 | 1.1.1.0/24 | AU | apnic | 2011-08-11",
		"1403 | 66.254.60.0/22 | CA | arin;26480 | 66.254.32.0/19 | CA",
		"", "| | |", "999 888 | x/8", "  /  ",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, rec string) {
		_, _ = pickCymruASN([]string{rec})           // must never panic
		_, _ = pickCymruASN(strings.Split(rec, ";")) // multi-record form
	})
}
