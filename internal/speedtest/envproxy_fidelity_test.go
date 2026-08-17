package speedtest

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// The remaining divergences between this file's fresh-reading proxy decision
// and net/http's once-cached one (x/net/http/httpproxy). Both were found by
// walking httpproxy line by line; each is pinned here so a future change to
// either side shows up as a test result rather than as a routing surprise.

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// proxyDecision runs the production per-request decision for a URL.
func proxyDecision(t *testing.T, raw string) (*url.URL, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return guardedEnvProxy(req)
}

// net/http REFUSES a request rather than proxying it when HTTP_PROXY applies in
// a CGI environment: there REQUEST_METHOD is set and HTTP_PROXY is whatever the
// client put in the Proxy request header, so honouring it would let a caller
// choose our egress. Same refusal here, in httpproxy's order - before the
// loopback and NO_PROXY exemptions, and http-scheme only.
func TestEnvProxyCGIRefusesHTTPProxy(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://192.168.1.10:3128")
	t.Setenv("HTTPS_PROXY", "http://192.168.1.11:3129")

	// Not a CGI environment: unchanged.
	t.Setenv("REQUEST_METHOD", "")
	if pu, err := proxyDecision(t, "http://example.com/x"); err != nil || pu == nil {
		t.Fatalf("non-CGI http request: proxy=%v err=%v, want the configured proxy", pu, err)
	}

	t.Setenv("REQUEST_METHOD", "GET")
	for _, target := range []string{
		"http://example.com/x",
		"http://127.0.0.1:9000/x", // the loopback exemption does not pre-empt it...
		"http://localhost:9000/x",
	} {
		if _, err := proxyDecision(t, target); err == nil || !strings.Contains(err.Error(), "CGI") {
			t.Errorf("CGI http request to %s: err = %v, want the CGI refusal", target, err)
		}
	}
	t.Setenv("NO_PROXY", "example.com") // ...and neither does NO_PROXY
	if _, err := proxyDecision(t, "http://example.com/x"); err == nil {
		t.Error("CGI refusal must precede the NO_PROXY exemption, as httpproxy's does")
	}
	t.Setenv("NO_PROXY", "")

	// HTTPS_PROXY is not attacker-supplied (there is no Https-Proxy header), so
	// https requests keep riding it - exactly httpproxy's scope.
	if pu, err := proxyDecision(t, "https://example.com/x"); err != nil || pu == nil || pu.Host != "192.168.1.11:3129" {
		t.Fatalf("CGI https request: proxy=%v err=%v, want HTTPS_PROXY honoured", pu, err)
	}
}

// The dial guard's trust set is "endpoints net/http would actually route a
// request through". In a CGI environment no request ever rides HTTP_PROXY, so
// trusting its endpoint - which is caller-supplied there - would open a dial to
// an internal address for zero routing benefit, the ALL_PROXY mistake again.
func TestProbeDialGuardDropsHTTPProxyInCGI(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://10.11.12.13:3128")
	if err := probeDialGuard("tcp", "10.11.12.13:3128", nil); err != nil {
		t.Fatalf("precondition: guard refused the configured proxy outside CGI: %v", err)
	}
	t.Setenv("REQUEST_METHOD", "GET")
	if err := probeDialGuard("tcp", "10.11.12.13:3128", nil); err == nil {
		t.Fatal("guard trusted a CGI HTTP_PROXY endpoint that no request is ever routed through")
	}
	// HTTPS_PROXY still carries https requests in CGI, so it stays trusted.
	t.Setenv("HTTPS_PROXY", "http://10.11.12.14:3128")
	if err := probeDialGuard("tcp", "10.11.12.14:3128", nil); err != nil {
		t.Errorf("guard refused the CGI-valid HTTPS_PROXY endpoint: %v", err)
	}
}

// IDNA: httpproxy punycodes both the request host and every NO_PROXY entry
// (canonicalAddr / init call idnaASCII), so a Unicode entry matches a punycode
// host and vice versa. This file compares the two as written, which matches
// only when both sides use the SAME form. The divergence is fail-safe - the
// request rides the proxy where net/http would send it direct, and a proxied
// request is still destination-vetted - and closing it would mean taking on
// golang.org/x/net/idna, a dependency this module does not have, for a routing
// nicety. Pinned here so the behaviour is a documented fact rather than a
// surprise.
func TestEnvProxyNoProxyIDNAFormsMustMatch(t *testing.T) {
	const (
		unicodeHost  = "münchen.example"
		punycodeHost = "xn--mnchen-3ya.example"
	)
	proxied := func(entry, host string) bool {
		t.Setenv("NO_PROXY", entry)
		p, err := envProxyEndpoint(mustParseURL(t, "http://"+host+"/x"))
		if err != nil {
			// The configured value below is usable, so the third outcome cannot
			// arise here: reading a refusal as "direct" would let a change that
			// broke every request pass as NO_PROXY matching.
			t.Fatalf("NO_PROXY=%q host %s: envProxyEndpoint refused a usable value: %v", entry, host, err)
		}
		return p != nil
	}
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "http://192.168.1.10:3128")

	// Same form on both sides: matches, exactly as net/http does.
	if proxied(unicodeHost, unicodeHost) {
		t.Error("a Unicode NO_PROXY entry must exempt the same Unicode host")
	}
	if proxied(punycodeHost, punycodeHost) {
		t.Error("a punycode NO_PROXY entry must exempt the same punycode host")
	}
	// Mixed forms: net/http would send these DIRECT; we proxy them. Documented
	// divergence, and fail-safe in both directions.
	if !proxied(punycodeHost, unicodeHost) {
		t.Error("unexpected: mixed-form matching now works; update the divergence note in envProxyEndpoint")
	}
	if !proxied(unicodeHost, punycodeHost) {
		t.Error("unexpected: mixed-form matching now works; update the divergence note in envProxyEndpoint")
	}
}
