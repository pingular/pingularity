package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A synthetic dual-stack host in the shape hostAddrs() returns: one Wi-Fi interface
// carrying a private IPv4, a ULA, a stable global IPv6 and a temporary (privacy)
// one, plus a link-local, a docker bridge and a Tailscale address. lanEntriesFor
// takes the address list as an argument so a test can pose a host
// net.Interface.Addrs() would never report.
//
// docker0 is first on purpose: enumeration order is the kernel's, not "the LAN
// first", so leading with the default-route address would let the primary-first sort
// be deleted with every test still green. Do not re-sort this block.
//
// The literals are documentation ranges (RFC 3849, a fd00::/8 ULA, an EUI-64
// link-local from the QEMU MAC range, RFC 1918), because a public repo must not
// carry a real home network's routable /64. tailscale0 is the exception and is
// carrier-grade NAT space, which is not routable from the internet either. Only how
// they classify matters here.
var testHost = []hostAddr{
	{iface: "docker0", ip: net.ParseIP("172.17.0.1")},
	{iface: "en0", ip: net.ParseIP("192.168.1.24")},
	{iface: "en0", ip: net.ParseIP("fe80::5054:ff:fe12:3456")},
	{iface: "en0", ip: net.ParseIP("fd00:db8:1::24")},
	{iface: "en0", ip: net.ParseIP("2001:db8:1:2::1")},
	{iface: "en0", ip: net.ParseIP("2001:db8:1:2::a7c9")},
	{iface: "tailscale0", ip: net.ParseIP("100.101.102.103")},
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
		docker   = "http://172.17.0.1:9000"
		tailnet  = "http://100.101.102.103:9000"
		routeSrc = "192.168.1.24" // what defaultRouteIP() reports on this synthetic host
	)
	// Primary first, then enumeration order - and testHost enumerates docker0 ahead
	// of en0, so this order is a claim about the sort, not about the fixture. en0's
	// second global IPv6 is absent because it shares a /64 with the first; see the
	// dedupe test below.
	all := []string{v4, docker, ula, gua, tailnet}

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
		{"loopback by name, rooted", "localhost.:9000", nil,
			"a trailing dot only roots the name, so this binds loopback too - nonLoopbackListen in main treats it the same way, and the two must agree about what counts as this-machine-only"},
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

// Several IPv6 addresses from one /64 on one interface are the same host on the
// same network (RFC 8981 privacy addressing produces them by the handful), so the
// list advertises the first and drops the rest - they are duplicate answers to "what
// do I type", not extra ones. What must survive: a second /64, the same /64 on
// another interface, and every IPv4 address, including aliases on one interface.
func TestLanEntriesKeepOneIPv6PerPrefixPerInterface(t *testing.T) {
	_, got := lanEntriesFor(":9000", testHost, "192.168.1.24")
	var fromGUA []string
	for _, e := range got {
		if strings.HasPrefix(e.IP, "2001:db8:1:2:") {
			fromGUA = append(fromGUA, e.IP)
		}
	}
	if len(fromGUA) != 1 || fromGUA[0] != "2001:db8:1:2::1" {
		t.Errorf("en0's 2001:db8:1:2::/64 advertised %v, want only 2001:db8:1:2::1: two addresses in one /64 on one interface reach the same dashboard over the same network, and ::1 enumerates first", fromGUA)
	}

	// A host whose duplicates and non-duplicates are all on one fixture, so a dedupe
	// that reaches too far shows up as a missing row rather than a passing test. Two
	// of the addresses sit right on the boundary and pin it from either side: the
	// pair that differs only in the FOURTH hextet is the last thing a shorter prefix
	// (a /48, say) would wrongly collapse, and the one that differs only past bit 64
	// is the first thing a longer one (a /80) would wrongly keep.
	host := []hostAddr{
		{iface: "en0", ip: net.ParseIP("192.168.1.24")},
		{iface: "en0", ip: net.ParseIP("192.168.1.25")},
		{iface: "en0", ip: net.ParseIP("2001:db8:1:2::1")},
		{iface: "en0", ip: net.ParseIP("2001:db8:1:2::a7c9")},
		{iface: "en0", ip: net.ParseIP("2001:db8:1:2:8000::1")},
		{iface: "en0", ip: net.ParseIP("2001:db8:1:3::1")},
		{iface: "en0", ip: net.ParseIP("2001:db8:9:9::5")},
		{iface: "eth1", ip: net.ParseIP("2001:db8:1:2::2")},
	}
	_, got = lanEntriesFor(":9000", host, "")
	want := []string{
		"http://192.168.1.24:9000",
		"http://192.168.1.25:9000",
		"http://[2001:db8:1:2::1]:9000",
		"http://[2001:db8:1:3::1]:9000",
		"http://[2001:db8:9:9::5]:9000",
		"http://[2001:db8:1:2::2]:9000",
	}
	if !sameURLs(got, want) {
		t.Errorf("advertised\n  %v\nwant\n  %v\nOnly a repeated (interface, /64) may collapse: two IPv4 addresses on one interface are separate answers, a second /64 is a second network, and the same /64 on another interface is a second path to reach it. 2001:db8:1:3::1 differs from 2001:db8:1:2::1 only in the fourth hextet, so a prefix shorter than /64 swallows a whole separate network; 2001:db8:1:2:8000::1 differs only past the 64th bit, so a prefix longer than /64 re-admits the privacy addresses this collapse exists to fold away.",
			urlsOf(got), want)
	}
}

// Public on a row means globally routable, which is the part this machine can know.
// It does not mean anything outside can connect: that is the router's inbound
// policy, and nothing here can observe it.
func TestLanEntriesFlagGloballyRoutableAddresses(t *testing.T) {
	want := map[string]bool{
		"192.168.1.24":    false, // RFC 1918
		"172.17.0.1":      false, // RFC 1918 as well, the docker bridge
		"fd00:db8:1::24":  false, // ULA, fc00::/7
		"2001:db8:1:2::1": true,  // global unicast, the case the flag exists for
		"100.101.102.103": false, // CGNAT: a tailnet peer can reach it, the internet cannot
	}
	_, got := lanEntriesFor(":9000", testHost, "192.168.1.24")
	if len(got) != len(want) {
		t.Fatalf("advertised\n  %v\nwant one row for each of the %d addresses classified above: the per-row check below only ever sees rows that ARE advertised, so a fixture that grew or a filter that swallowed a row would let an address ship with its public flag never looked at",
			urlsOf(got), len(want))
	}
	for _, e := range got {
		w, ok := want[e.IP]
		if !ok {
			t.Errorf("advertised %s, which this test has no expectation for: every address the panel lists needs a stated public/not-public verdict, or the flag that drives the exposure warning goes untested for it", e.IP)
			continue
		}
		if e.Public != w {
			t.Errorf("%s on %s: public = %v, want %v. This flag decides whether the Access tab tells the operator their dashboard may be reachable from outside, so calling a private or CGNAT address public spends that warning on a network nobody outside can enter, and missing a globally routable one leaves a plain-HTTP dashboard unannounced.",
				e.IP, e.Iface, e.Public, w)
		}
	}

	// A pinned bind returns from its own branch before the enumeration runs, so it
	// classifies separately and has to agree - one address, one answer.
	for _, c := range []struct {
		listen string
		want   bool
	}{
		{"[2001:db8:1:2::1]:9000", true},
		{"192.168.1.24:9000", false},
		{"100.101.102.103:9000", false},
	} {
		_, got := lanEntriesFor(c.listen, testHost, "192.168.1.24")
		if len(got) != 1 || got[0].Public != c.want {
			t.Errorf("-listen %s advertised %+v, want a single row with public = %v: a pinned socket takes the branch above the enumeration, and the same address must not be described one way there and another way here", c.listen, got, c.want)
		}
	}
}

// The two binds that fall through to the full enumeration - a hostname other than
// localhost, and a zoned literal - serve ONE of the listed addresses without saying
// which, so a row there is a candidate rather than a confirmed answer. The public tag
// still follows the address: the row already advertises that URL, and a globally
// routable plain-HTTP address listed with no tag is described exactly like the LAN
// ones beside it, which is the case the flag was added for. Private and CGNAT rows
// stay untagged on both, so the fall-through does not tag the whole list either.
func TestLanEntriesFlagPublicOnTheFallThroughBinds(t *testing.T) {
	want := map[string]bool{
		"2001:db8:1:2::1": true,  // the candidate that would go unannounced
		"192.168.1.24":    false, // RFC 1918
		"172.17.0.1":      false, // the docker bridge
		"fd00:db8:1::24":  false, // ULA
		"100.101.102.103": false, // CGNAT
	}
	for _, listen := range []string{"myhost.example:9000", "[fe80::1%en0]:9000"} {
		_, got := lanEntriesFor(listen, testHost, "192.168.1.24")
		if len(got) != len(want) {
			t.Fatalf("-listen %s advertised\n  %v\nwant the full enumeration of %d addresses: these two binds deliberately fall back to it, and a shorter list means this case stopped being a fall-through and no longer tests one",
				listen, urlsOf(got), len(want))
		}
		for _, e := range got {
			w, ok := want[e.IP]
			if !ok {
				t.Errorf("-listen %s advertised %s, which this test has no expectation for: every row the panel lists needs a stated verdict, or the flag that drives the exposure warning goes untested for it", listen, e.IP)
				continue
			}
			if e.Public != w {
				t.Errorf("-listen %s: %s public = %v, want %v. A hostname or a zoned literal cannot be narrowed to one address here, so every row stays a candidate - dropping the tag hides a routable plain-HTTP address among the LAN ones, and tagging a private or CGNAT row spends the warning on a network nobody outside can enter",
					listen, e.IP, e.Public, w)
			}
		}
	}
}

// The Access tab reads these rows straight off the JSON, so the field names are an
// interface rather than an implementation detail. Renaming one blanks the part of
// the row it drives instead of failing anywhere a reader would notice.
func TestLanEntryJSONKeepsTheNamesTheAccessTabReads(t *testing.T) {
	b, err := json.Marshal(lanEntry{URL: "http://192.0.2.1:9000", IP: "192.0.2.1", Iface: "en0", Primary: true, Public: true})
	if err != nil {
		t.Fatalf("lanEntry does not marshal: %v. /api/access carries these rows to the Access tab as JSON, so a row that cannot be encoded is a panel with no addresses in it at all", err)
	}
	for _, k := range []string{`"url"`, `"ip"`, `"iface"`, `"primary"`, `"public"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("lanEntry marshalled as %s, which has no %s key: the Access tab reads that exact name, so renaming the field silently blanks whatever it drives - the link, the accent on the row to hand to another device, or the public tag - instead of failing anywhere a reader would notice", b, k)
		}
	}
}

// Where isPublicIP's line falls. The stdlib gets every case here right except one:
// 100.64.0.0/10 is global unicast and not private, so a predicate built on those two
// calls alone reports every Tailscale host as reachable from the internet.
func TestIsPublicIPExcludesCGNATAndPrivateSpace(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		{"2001:db8:1:2::1", true, "a global unicast IPv6 address is what the flag is for"},
		{"8.8.8.8", true, "so is a routable IPv4 address, as a VPS has"},
		{"172.67.1.1", true, "172.16.0.0/12 is private but the rest of 172/8 is not, and this one's second octet also falls inside the CGNAT block's, so it fails if that block is matched without its first octet"},
		{"192.168.1.24", false, "RFC 1918"},
		{"10.0.0.5", false, "RFC 1918"},
		{"172.17.0.1", false, "RFC 1918, the shape a docker bridge takes"},
		{"fd00:db8:1::24", false, "ULA, fc00::/7"},
		{"fd7a:115c:a1e0::1", false, "Tailscale's IPv6 range is a ULA, so the stdlib already answers this one"},
		{"fe80::1", false, "link-local is not global unicast"},
		{"127.0.0.1", false, "neither is loopback"},
		{"100.64.0.0", false, "first address of the CGNAT block"},
		{"100.101.102.103", false, "a Tailscale address, the case the carve-out exists for"},
		{"100.127.255.255", false, "last address of the CGNAT block"},
		{"100.63.255.255", true, "one below the block - 100.0.0.0/8 is otherwise ordinary public space"},
		{"100.128.0.0", true, "one above it, same reason"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("test fixture %q does not parse, so isPublicIP is never asked about it: a typo in one of these literals retires the boundary it was added to hold without failing anything", c.ip)
		}
		if got := isPublicIP(ip); got != c.want {
			t.Errorf("isPublicIP(%s) = %v, want %v: %s", c.ip, got, c.want, c.why)
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

// The reverse-proxy caveat describes a CHANGE of posture, so it rides the transition
// into local-only and nothing else. The Settings drawer echoes local_only on every
// Save, so without that gate an unrelated change on an install that is already
// local-only answers "Access is now limited to this machine, but ..." - a sentence
// that says "now" about nothing, on save after save, until the operator stops reading
// the one save where it matters.
func TestLocalOnlyProxyCaveatRidesTheTransitionOnly(t *testing.T) {
	const caveat = "CANNOT block"             // the wording the response carries
	const logged = "declares a reverse proxy" // the wording the server log carries

	post := func(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
		t.Helper()
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/access", strings.NewReader(body))
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "127.0.0.1:54321"
		r.Header.Set("Content-Type", "application/json")
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST /api/access %s: HTTP %d: %s. The Settings drawer aborts the whole Save when this call fails, so an access POST has to succeed before anything can be said about what it warned",
				body, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		return rr
	}

	// Turning it on with a proxy declared: the change does less than its name says,
	// and the operator making it is the person who needs to hear so.
	var onLog bytes.Buffer
	on := newTestServerLog(t, &onLog)
	on.AllowedHosts = []string{"ping.example.com"}
	rr := post(t, on, `{"local_only":true}`)
	if w := strings.Join(warningsOf(t, rr), " "); !strings.Contains(w, caveat) {
		t.Errorf("switching to local-only with -allow-host declared answered warnings %q, want the %q caveat: proxied visitors arrive as loopback connections, so this save did NOT make the dashboard private and the person who just made it believes it did",
			w, caveat)
	}
	if !strings.Contains(onLog.String(), logged) {
		t.Errorf("switching to local-only with -allow-host declared logged nothing containing %q; the response reaches whoever was at the keyboard, the log is what anyone reading back the box's history has", logged)
	}

	// Already local-only, and the save is about something else entirely: the posture
	// did not move, so neither the response nor the log may claim it did.
	var keepLog bytes.Buffer
	keep := newTestServerLog(t, &keepLog)
	keep.AllowedHosts = []string{"ping.example.com"}
	if err := keep.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("seed local-only: %v. The case under test is a save made on a box that was ALREADY local-only, so the seed has to land before the POST", err)
	}
	rr = post(t, keep, `{"local_only":true,"auth_enabled":true}`)
	if w := strings.Join(warningsOf(t, rr), " "); w != "" {
		t.Errorf("a save that only turned the login switch on answered warnings %q; local-only was already on, so nothing about network reach changed and a caveat saying access is \"now\" limited is a warning the operator learns to dismiss before the save that means it", w)
	}
	if strings.Contains(keepLog.String(), logged) {
		t.Errorf("a save that changed no network reach still logged the %q caveat; repeating it on every save buries the one line that recorded the actual change", logged)
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
