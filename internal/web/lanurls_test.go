package web

import (
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A synthetic dual-stack host in the shape hostAddrs() returns: one Wi-Fi interface
// carrying a private IPv4, a ULA, a stable global IPv6 and a temporary (privacy)
// one, plus a link-local and a docker bridge. lanEntriesFor takes the address list
// as an argument so a test can pose a host net.Interface.Addrs() would never report.
//
// docker0 is first on purpose: enumeration order is the kernel's, not "the LAN
// first", so leading with the default-route address would let the primary-first sort
// be deleted with every test still green. Do not re-sort this block.
//
// The literals are documentation ranges (RFC 3849, a fd00::/8 ULA, an EUI-64
// link-local from the QEMU MAC range, RFC 1918), because a public repo must not
// carry a real home network's routable /64. Only how they classify matters here.
var testHost = []hostAddr{
	{iface: "docker0", ip: net.ParseIP("172.17.0.1")},
	{iface: "en0", ip: net.ParseIP("192.168.1.24")},
	{iface: "en0", ip: net.ParseIP("fe80::5054:ff:fe12:3456")},
	{iface: "en0", ip: net.ParseIP("fd00:db8:1::24")},
	{iface: "en0", ip: net.ParseIP("2001:db8:1:2::1")},
	{iface: "en0", ip: net.ParseIP("2001:db8:1:2::a7c9")},
}

func urlsOf(entries []lanEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.URL)
	}
	return out
}

func sameURLs(got []lanEntry, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i].URL != want[i] {
			return false
		}
	}
	return true
}

// The "Reachable at" list must follow the socket. Two ways it used not to: a
// loopback-only bind still advertised the LAN address, which refuses the connection
// the operator then makes from a phone; and IPv4-only enumeration hid the globally
// routable IPv6 address a wildcard bind also answers on.
func TestLanEntriesFollowTheBindAndCoverBothFamilies(t *testing.T) {
	const (
		v4       = "http://192.168.1.24:9000"
		ula      = "http://[fd00:db8:1::24]:9000"
		gua      = "http://[2001:db8:1:2::1]:9000"
		guaTemp  = "http://[2001:db8:1:2::a7c9]:9000"
		docker   = "http://172.17.0.1:9000"
		routeSrc = "192.168.1.24" // what defaultRouteIP() reports on this synthetic host
	)
	// Primary first, then enumeration order - and testHost enumerates docker0 ahead
	// of en0, so this order is a claim about the sort, not about the fixture.
	all := []string{v4, docker, ula, gua, guaTemp}

	cases := []struct {
		name   string
		listen string
		want   []string
		why    string
	}{
		{"default wildcard", ":9000", all,
			"a wildcard bind answers on every address in both families (measured Aug 2026)"},
		{"IPv4-spelled wildcard", "0.0.0.0:9000", all,
			"Go rewrites a 0.0.0.0 wildcard to :: with a v4-mapped socket, so it answers on IPv6 too"},
		{"IPv6-spelled wildcard", "[::]:9000", all,
			"an unspecified IPv6 bind is dual-stack, not IPv6-only"},
		{"loopback pin", "127.0.0.1:9000", nil,
			"a loopback-only socket refuses every network address, so there is nothing to advertise"},
		{"loopback pin, IPv6", "[::1]:9000", nil, "same socket, other family"},
		{"loopback by name", "localhost:9000", nil, "the name binds loopback like the literal does"},
		{"loopback by name, as the operator typed it", "LocalHost:9000", nil,
			"DNS is case-insensitive and so is Go's resolver - net.Listen binds \"LocalHost:9000\" to 127.0.0.1 (measured Aug 2026, darwin, Go 1.27), so the capitalisation an operator happened to type must not change what is advertised"},
		{"loopback by name, shouted", "LOCALHOST:9000", nil, "same, at the other extreme of the casing"},
		{"zoned loopback literal", "[::1%lo0]:9000", nil,
			"net.ParseIP rejects the zone, but the socket is still loopback-only (it binds, measured Aug 2026, darwin, Go 1.27), so the zone-stripped address has to answer the loopback question"},
		{"pinned to one LAN address", "192.168.1.24:9000", []string{v4},
			"a pinned socket answers on that address and refuses the rest"},
		{"pinned to one IPv6 address", "[2001:db8:1:2::1]:9000", []string{gua},
			"same, and the URL has to bracket the literal"},
		{"hostname bind", "myhost.example:9000", all,
			"unresolved here (a DNS round-trip in a UI request); the old full enumeration stands"},
		{"zoned link-local literal", "[fe80::1%en0]:9000", all,
			"the other unfiltered case: stripping the zone to pin it would echo an unzoned fe80:: URL, which no reader can use, so the full enumeration stands instead"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port, got := lanEntriesFor(c.listen, testHost, routeSrc)
			if port != "9000" {
				t.Errorf("port = %q, want 9000: the port drives every advertised URL and the UI's localhost fallback", port)
			}
			if !sameURLs(got, c.want) {
				t.Errorf("-listen %s advertised\n  %v\nwant\n  %v\n%s\nThe list is what the operator copies into another device: an address the socket does not answer on cannot connect, and one it does answer on that is missing conceals where the dashboard can be reached from.",
					c.listen, urlsOf(got), c.want, c.why)
			}
		})
	}
}

// Which addresses are fit to advertise, and which entry leads: no link-local or
// loopback in either family, the default-route address first because the UI accents
// the first row, and no second entry claiming primary.
func TestLanEntriesExcludeLinkLocalAndLeadWithTheDefaultRoute(t *testing.T) {
	_, got := lanEntriesFor(":9000", testHost, "192.168.1.24")
	if len(got) == 0 {
		t.Fatal("wildcard bind advertised nothing at all")
	}
	for _, e := range got {
		ip := net.ParseIP(e.IP)
		if ip == nil {
			t.Fatalf("entry %+v carries an unparseable IP", e)
		}
		if ip.IsLinkLocalUnicast() || ip.IsLoopback() {
			t.Errorf("the wildcard enumeration advertised %s: link-local and loopback addresses cannot be typed into another device (an IPv6 link-local URL needs the reader's own zone index), so the enumeration must never emit one. A PIN is a separate case and is echoed unfiltered on purpose - the socket really is bound there.", e.IP)
		}
	}
	// testHost enumerates docker0 first, so this checks the sort, not the fixture:
	// without it the panel would accent an address reachable only inside containers.
	if !got[0].Primary || got[0].IP != "192.168.1.24" {
		t.Errorf("first entry = %+v, want the default-route address 192.168.1.24 flagged primary: the UI accents the first row as the address to hand to another device, and enumeration order is the kernel's, not the LAN's", got[0])
	}
	for _, e := range got[1:] {
		if e.Primary {
			t.Errorf("%s is flagged primary as well as the default-route address; primary means the IPv4 default-route source, so exactly one entry may carry it - and never an IPv6 one, because the kernel's outbound IPv6 source is a privacy address that rotates (RFC 8981)", e.IP)
		}
	}
}

// familyMix counts advertised addresses by family - "1 IPv4, 1 ULA, 2 global IPv6"
// - for the one test here that runs against the real host. Counts, not addresses,
// because that log line goes wherever the suite runs, CI included, and it used to
// carry this machine's routable /64. They still answer what the log is for: whether
// a dual-stack host exercised the bracketing rule or an IPv4-only runner passed it
// vacuously. The failure messages below still name addresses, as diagnostics.
func familyMix(entries []lanEntry) string {
	var v4, ula, gua, other int
	for _, e := range entries {
		ip := net.ParseIP(e.IP)
		switch {
		case ip == nil:
			// Unreachable in practice - IP comes from net.IP.String() - but counted
			// rather than dropped, so the labels always add up to len(entries).
			other++
		case ip.To4() != nil:
			v4++
		case ip.IsPrivate(): // for an IPv6 address IsPrivate means fc00::/7, the ULA range
			ula++
		case ip.IsGlobalUnicast():
			gua++
		default:
			other++
		}
	}
	parts := make([]string, 0, 4)
	for _, c := range []struct {
		n     int
		label string
	}{{v4, "IPv4"}, {ula, "ULA"}, {gua, "global IPv6"}, {other, "other"}} {
		if c.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c.n, c.label))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// Every advertised URL must survive a URL parser with its address and port intact
// - the check that catches an unbracketed IPv6 literal, where the last group parses
// as the port. Runs against the real host through the production path, so a
// dual-stack machine exercises it for real and an IPv4-only one still passes.
func TestLanURLsAreParseableOnThisHost(t *testing.T) {
	s := newTestServer(t)
	s.listenAddr = ":9000"
	port, entries := s.lanURLs()
	// Counts only - see familyMix for why this host's addresses do not go to a log.
	t.Logf("this host advertises %d address(es): %s", len(entries), familyMix(entries))
	for _, e := range entries {
		u, err := url.Parse(e.URL)
		if err != nil {
			t.Errorf("advertised URL %q does not parse (%v); the UI puts it in an href and a copy button", e.URL, err)
			continue
		}
		if u.Hostname() != e.IP || u.Port() != port {
			t.Errorf("advertised URL %q parses as host %q port %q, want %q and %q: an IPv6 literal must be bracketed or the port is read out of the address",
				e.URL, u.Hostname(), u.Port(), e.IP, port)
		}
	}
}

// The production path, on the real host: with a loopback-only bind the API must
// advertise nothing, whatever interfaces this machine happens to have. It is
// /api/access, not lanURLs, that the Access tab reads.
func TestAccessStatusAdvertisesNothingOnALoopbackBind(t *testing.T) {
	s := newTestServer(t)
	s.listenAddr = "127.0.0.1:19734"
	out := s.accessStatus(httptest.NewRequest("GET", "/api/access", nil))
	if out["port"] != "19734" {
		t.Errorf("port = %v, want 19734", out["port"])
	}
	entries, ok := out["lan_urls"].([]lanEntry)
	if !ok {
		t.Fatalf("lan_urls = %#v, want []lanEntry", out["lan_urls"])
	}
	if len(entries) != 0 {
		t.Errorf("a 127.0.0.1 bind advertised %v; that socket answers on loopback only, so every one of those addresses refuses the connection an operator makes after copying it", urlsOf(entries))
	}
}
