package speedtest

import "testing"

// A proxy value naming a scheme but no HOST parses cleanly - url.Parse fills Host
// with ":3128" while Hostname() is empty - so it slips through the clean-parse
// branch and becomes an endpoint with no host. proxyHostPort then has nothing to
// put in the dial guard's trust set, and an EMPTY trust set is how
// guardProxiedDestination and guardedServers decide there is no proxy to police:
// both stand down. So this shape does not merely fail to route, it silently
// disarms the SSRF guard for the whole run.
func TestAProxySchemeWithNoHostIsRefusedRatherThanDisarmingTheGuard(t *testing.T) {
	for _, v := range []string{"http://:3128", "https://:8443", "socks5://:1080"} {
		u, err := parseProxyEnvURL(v)
		if err == nil {
			t.Errorf("%q accepted as a proxy (endpoint %v, hostname %q): it names no host, so the dial "+
				"guard's trust set is empty and the guard stands down for every request in the run",
				v, u, u.Hostname())
		}
	}
}
