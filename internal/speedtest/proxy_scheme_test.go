package speedtest

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Every proxy value an operator can write must land in one of three places: a
// usable endpoint, the http default for the schemeless host:port form, or a
// refusal that names what is wrong. The fourth outcome this pins against is
// the dangerous one - a value carrying a scheme this daemon cannot speak being
// rewritten into a DIFFERENT, reachable endpoint. HTTP_PROXY=ftp://proxy.example:21
// used to become "http://" + the whole value, which url.Parse reads as host
// "ftp" with no port, so the daemon would proxy every speedtest request
// through whatever answers on the name "ftp" at port 80, and the dial guard -
// whose trust set is built from this same parse - would admit that dial as
// "the operator's configured proxy".
func TestProxyEnvValueEitherRoutesOrIsRefusedByName(t *testing.T) {
	cases := []struct {
		name string
		// value is the raw HTTP_PROXY/HTTPS_PROXY spelling.
		value string
		// scheme is the scheme the endpoint must be reached with; empty when
		// the value must be refused instead.
		scheme string
		// hostPort is what the dial guard may add to its trust set; empty
		// means nothing at all may be trusted from this value.
		hostPort string
		// errHas is a substring the refusal must carry so the operator can see
		// which value, and which scheme, was rejected; empty means the value
		// must be accepted.
		errHas string
	}{
		{
			// The schemeless form is what most people put in these variables,
			// and net/http reads it as http://host:port (x/net/http/httpproxy's
			// parseProxy retries any value whose parse fails or that carries no
			// scheme or no host - "proxy.example:3128" lands there because
			// url.Parse takes "proxy.example" for the scheme and leaves the
			// host empty). Refusing it would break working installs, so it
			// keeps the http default: no scheme was written, so none is wrong.
			name:     "schemeless host:port still means http",
			value:    "proxy.example:3128",
			scheme:   "http",
			hostPort: "proxy.example:3128",
		},
		{
			name:     "http keeps the port it was given",
			value:    "http://proxy.example:3128",
			scheme:   "http",
			hostPort: "proxy.example:3128",
		},
		{
			// A supported scheme with no port takes that scheme's default, and
			// the dial guard has to trust exactly that endpoint - 443 here, not
			// 80, or the proxy dial it is about to see gets refused.
			name:     "https with no port defaults to 443",
			value:    "https://proxy.example",
			scheme:   "https",
			hostPort: "proxy.example:443",
		},
		{
			name:     "socks5 with no port defaults to 1080",
			value:    "socks5://proxy.example",
			scheme:   "socks5",
			hostPort: "proxy.example:1080",
		},
		{
			// The defect: this parses cleanly, the scheme is real, and we
			// cannot proxy through it. It must be refused with "ftp" in the
			// message - a typo is invisible to the operator otherwise - and it
			// must leave the trust set empty rather than filling it with the
			// invented endpoint the rewrite produced.
			name:   "an unsupported scheme is refused by name, never rewritten",
			value:  "ftp://proxy.example:21",
			errHas: "ftp",
		},
		{
			// Unparseable in both forms (an unterminated IPv6 literal, which
			// also fails as "http://[::1"): no endpoint exists, so nothing may
			// be trusted, and the value is named so the operator is not left
			// wondering why connections went out direct.
			name:   "a value that cannot be parsed at all is refused",
			value:  "[::1",
			errHas: "[::1",
		},

		// ---- the schemeless forms, which must all keep working -------------
		// These are the reason the "http://" + value retry exists at all. Note
		// how differently url.Parse treats them: "proxy.example:3128" parses
		// CLEANLY as scheme "proxy.example", while "192.168.1.10:3128" and
		// "[::1]:3128" fail outright ("first path segment in URL cannot contain
		// colon"). A rule that keyed off "did it parse" would refuse both of
		// those, which is every proxy written as an IP address.
		{
			name:     "an IPv4 host:port fails to parse and still means http",
			value:    "192.168.1.10:3128",
			scheme:   "http",
			hostPort: "192.168.1.10:3128",
		},
		{
			name:     "a bracketed IPv6 host:port fails to parse and still means http",
			value:    "[::1]:3128",
			scheme:   "http",
			hostPort: "[::1]:3128",
		},
		{
			name:     "a bare host with no port takes port 80",
			value:    "proxy.example",
			scheme:   "http",
			hostPort: "proxy.example:80",
		},
		{
			// Credentials without a scheme: url.Parse reads "user" as the
			// scheme and leaves the host empty, so this only survives via the
			// retry.
			name:     "schemeless credentials keep their host",
			value:    "user:pass@proxy.example:3128",
			scheme:   "http",
			hostPort: "proxy.example:3128",
		},
		{
			// The discriminator between "this value names a scheme" and "this
			// value is host:port" is the "//", never the token before the
			// colon: a host really named "ftp" on port 3128 is an ordinary
			// schemeless value and must not be mistaken for the ftp scheme.
			name:     "a host named like a scheme is still a host",
			value:    "ftp:3128",
			scheme:   "http",
			hostPort: "ftp:3128",
		},

		// ---- values that named a scheme and did NOT yield an endpoint ------
		// Every one of these used to be rewritten to "http://" + value, and
		// url.Parse reads the SCHEME NAME as the host of the result: the
		// authority of "http://ftp://proxy.example:notaport" ends at the next
		// "/", leaving host "ftp:" - hostname "ftp", no port - so the daemon
		// proxied through whatever answers the name "ftp" on port 80 and the
		// dial guard, built from this same parse, trusted "ftp:80". The value
		// declared a scheme, so it is used as written or refused; it is never
		// retried as a host:port.
		{
			// The plausible operator typo, and the sharpest case: a value that
			// is correct but for one trailing space. Measured before the fix:
			// the endpoint became "http:80", i.e. every speedtest request left
			// for a host named "http", and the operator's real proxy at
			// proxy.example:3128 was never contacted.
			name:   "a trailing space is refused, not rerouted to a host named http",
			value:  "http://proxy.example:3128 ",
			errHas: "http://proxy.example:3128 ",
		},
		{
			// Unsupported scheme whose first parse FAILS (invalid port), so the
			// scheme check on a clean parse never sees it. This is the residue
			// of the original defect: same harm, same "ftp:80" endpoint.
			name:   "an unsupported scheme with a bad port is refused, not rewritten",
			value:  "ftp://proxy.example:notaport",
			errHas: "ftp",
		},
		{
			// Unsupported scheme, empty host: parses cleanly, so the clean-parse
			// scheme check does not fire either (it needs a host).
			name:   "an unsupported scheme with no authority is refused, not rewritten",
			value:  "ftp:///proxy.example",
			errHas: "ftp",
		},
		{
			// A supported scheme is no safer: "http://" alone became the
			// endpoint "http:80" too.
			name:   "a scheme with nothing after it is refused",
			value:  "http://",
			errHas: "http://",
		},
		{
			// A unix socket path is not something this daemon can proxy
			// through, and a TCP endpoint named "unix" on port 80 is not what
			// the operator asked for either.
			name:   "a unix socket value is refused, not turned into a TCP endpoint",
			value:  "unix:///var/run/proxy.sock",
			errHas: "unix",
		},
		{
			// url.Parse will not read this as a scheme at all - a scheme may
			// not start with a digit, so the whole value is taken for a path -
			// yet it was plainly written as one, and the retry turned it into
			// the endpoint "1http:80". What counts is that the operator wrote
			// "://", not whether url.Parse agrees it is a scheme.
			name:   "something written as a scheme is refused even where url.Parse sees none",
			value:  "1http://proxy.example:21",
			errHas: "1http",
		},
		{
			// No hostname at all: url.Parse("http://:3128") yields host ":3128"
			// with an empty hostname, which proxyHostPort turns into "" - no
			// trust entry at all. Accepting it would hand net/http an endpoint
			// the dial guard has nothing on file for, so the operator would be
			// left reading a blocked-dial message instead of this one.
			name:   "a port with no host is refused",
			value:  ":3128",
			errHas: ":3128",
		},
		{
			// The one ambiguity we keep, and it is genuine: this is the same
			// shape as "user:pass@proxy.example:3128" above, so nothing can
			// tell the two apart - "mailto:ops" is read as credentials for the
			// host the value literally names. net/http reads it identically.
			// No host is invented here; the host is written in the value.
			name:     "an opaque value with credentials keeps the host it names",
			value:    "mailto:ops@example.com",
			scheme:   "http",
			hostPort: "example.com:80",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := parseProxyEnvURL(tc.value)
			if tc.errHas != "" {
				if err == nil {
					t.Fatalf("%q was accepted as proxy %v; want a refusal mentioning %q", tc.value, u, tc.errHas)
				}
				if !strings.Contains(err.Error(), tc.errHas) {
					t.Errorf("refusal of %q does not mention %q: %v", tc.value, tc.errHas, err)
				}
				if u != nil {
					t.Errorf("refused %q but still returned an endpoint %v", tc.value, u)
				}
			} else {
				if err != nil {
					t.Fatalf("%q was refused: %v", tc.value, err)
				}
				if u == nil {
					t.Fatalf("%q yielded no endpoint at all", tc.value)
				}
				if u.Scheme != tc.scheme {
					t.Errorf("%q reaches its proxy over %q, want %q", tc.value, u.Scheme, tc.scheme)
				}
			}
			// proxyHostPort feeds the dial guard's trust set directly, so a
			// refused value must contribute nothing to it.
			if got := proxyHostPort(tc.value); got != tc.hostPort {
				t.Errorf("dial guard would trust %q from %q, want %q", got, tc.value, tc.hostPort)
			}
		})
	}
}

// Where a refusal reaches the operator and where it does not. cgiProxyRefusal
// stands down for a value that yields no endpoint - nothing rides such a value,
// so there is no egress choice to take away from a CGI caller - on the
// understanding that envProxyEndpoint's refusal arrives in its place. That
// substitution is not total, and this pins the boundary: envProxyEndpoint
// applies the loopback/localhost and NO_PROXY exemptions BEFORE it parses the
// value, so on an exempt destination an unusable value produces no refusal at
// all and the request goes direct in silence - whereas the CGI refusal, when it
// does fire, deliberately precedes those same exemptions
// (TestEnvProxyCGIRefusesHTTPProxy). An operator with a typo in HTTP_PROXY
// therefore hears about it only on the requests that would have been proxied.
func TestProxyUnusableValueRefusesOnlyWhatWouldHaveBeenProxied(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "ftp://proxy.example:21")
	t.Setenv("REQUEST_METHOD", "GET") // CGI, so cgiProxyRefusal is in play

	decide := func(t *testing.T, raw string) (*url.URL, error) {
		t.Helper()
		// IP-literal or loopback destinations only: with no usable value
		// proxyAddrs is empty, so guardProxiedDestination is inert and nothing
		// here touches the network or DNS.
		req, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		return guardedEnvProxy(req)
	}

	// A destination that would have been proxied: the value is named, and the
	// message is the parse refusal, not the CGI one.
	pu, err := decide(t, "http://93.184.216.34/x")
	if err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Errorf("proxied-destination request: proxy=%v err=%v, want a refusal naming the ftp value", pu, err)
	}
	// An exempt destination: no refusal, no proxy - a silent direct connection.
	for _, target := range []string{"http://127.0.0.1:9000/x", "http://localhost:9000/x"} {
		if pu, err := decide(t, target); pu != nil || err != nil {
			t.Errorf("%s: proxy=%v err=%v, want a silent direct connection", target, pu, err)
		}
	}
	// NO_PROXY leaves by the same branch, before the value is ever parsed.
	t.Setenv("NO_PROXY", "198.51.100.7")
	if pu, err := decide(t, "http://198.51.100.7/x"); pu != nil || err != nil {
		t.Errorf("NO_PROXY-excluded destination: proxy=%v err=%v, want a silent direct connection", pu, err)
	}
}

// The harm, end to end: with an unsupported scheme configured the daemon has to
// stop and say so, not quietly send the operator's speedtest traffic to some
// other machine. The rewrite handed net/http a proxy of http://ftp (port 80)
// and put "ftp:80" in the dial guard's trust set, so the request left the
// machine for a host nobody configured and the guard waved the dial through.
func TestUnsupportedProxySchemeStopsRequestsInsteadOfRedirectingThem(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "ftp://proxy.example:21")

	// An IP-literal destination keeps this test off the network: the proxied
	// destination guard checks a literal address directly and never resolves a
	// name (see guardProxiedDestination).
	req, err := http.NewRequest(http.MethodGet, "http://93.184.216.34/speedtest", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	proxy, err := guardedEnvProxy(req)
	if err == nil {
		t.Fatalf("request accepted an ftp:// proxy value and would be routed via %v; want a refusal naming the scheme", proxy)
	}
	if !strings.Contains(err.Error(), "ftp") {
		t.Errorf("refusal does not name the offending scheme: %v", err)
	}
	if proxy != nil {
		t.Errorf("refused the value but still routed the request via %v", proxy)
	}
	if got := proxyAddrs(); len(got) != 0 {
		t.Errorf("dial guard trusts %v built from an unusable proxy value, want nothing trusted", got)
	}
}
