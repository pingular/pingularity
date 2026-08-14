package speedtest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The probes must ride the operator's proxy exactly like the transfers do.
// probeClient used to build its Transport WITHOUT a Proxy, so in a proxy-only
// network every fallback check and by-ID redirect probe dialed DIRECT: the
// probe timed out as endpointUnknown with zero proxy requests, a migrated
// pinned server kept its stale URL, and the proxied upload then hit the
// non-replayable 307 - issues #17/#18, alive behind proxies.
//
// The fake proxy below answers absolute-URI (proxy-form) requests itself, so
// no packet ever leaves loopback once routing works; the origin hosts are
// public literals that are unreachable directly, which is exactly the
// proxy-only network shape.

// recordingProxy is a plain-HTTP forward proxy stand-in on a loopback 58xx
// port: it records the logical destination of every proxy-form request and
// serves the scripted answer itself.
type recordingProxy struct {
	addr   string
	mu     sync.Mutex
	hosts  []string
	handle func(w http.ResponseWriter, r *http.Request)
}

func (p *recordingProxy) hostHits(host string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, h := range p.hosts {
		if h == host {
			n++
		}
	}
	return n
}

func (p *recordingProxy) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.hosts)
}

// startRecordingProxy serves the fake proxy on the first free port in the
// task-allowed 5801-5899 loopback range.
func startRecordingProxy(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *recordingProxy {
	t.Helper()
	var ln net.Listener
	var err error
	for port := 5801; port <= 5899; port++ {
		ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			break
		}
		ln = nil
	}
	if ln == nil {
		t.Fatalf("no free loopback port in 5801-5899: %v", err)
	}
	p := &recordingProxy{addr: ln.Addr().String(), handle: handle}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proxy-form requests carry the absolute target; the logical host is
		// what the guard exists to vet, so that is what gets recorded.
		host := r.URL.Host
		if host == "" {
			host = r.Host
		}
		p.mu.Lock()
		p.hosts = append(p.hosts, host)
		p.mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		p.handle(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return p
}

// The fallback health probe must reach its target THROUGH the configured
// proxy, with the dial guard fully armed: the only dial is the proxy's own
// (allowed) endpoint, and the origin - unreachable directly - answers via the
// proxy. Before the fix this returned endpointUnknown with zero proxy hits.
func TestProbeFallbackRoutesThroughConfiguredProxy(t *testing.T) {
	clearProxyEnv(t)
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latency.txt") {
			_, _ = w.Write([]byte("test=test"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)

	srv := &ookla.Server{ID: "proxied", URL: "http://93.184.216.34:8080/speedtest/upload.php"}
	st := probeFallback(context.Background(), srv)
	if st != endpointOK {
		t.Fatalf("verdict = %v, want ok - the probe did not reach the origin through the proxy (%d proxy hits)",
			st, proxy.total())
	}
	if got := proxy.hostHits("93.184.216.34:8080"); got == 0 {
		t.Fatal("the probe never asked the proxy for the origin - it must have dialed direct")
	}
}

// The by-ID endpoint probe must follow the migration 307 through the proxy and
// adopt the redirect target, or a migrated pinned server keeps its stale URL
// and every proxied upload 307s forever - the #17/#18 failure behind a proxy.
func TestProbeEndpointFollowsRedirectThroughProxy(t *testing.T) {
	clearProxyEnv(t)
	const stale, current = "93.184.216.34:8080", "93.184.216.35:8080"
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == stale {
			w.Header().Set("Location", "https://"+current+"/speedtest/upload.php")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)

	srv := &ookla.Server{ID: "migrated-pin", URL: "http://" + stale + "/speedtest/upload.php"}
	st := probeEndpoint(context.Background(), srv)
	if st != endpointOK {
		t.Fatalf("verdict = %v, want ok (probe hits: stale=%d current=%d)",
			st, proxy.hostHits(stale), proxy.hostHits(current))
	}
	if want := "http://" + current + "/speedtest/upload.php"; srv.URL != want {
		t.Fatalf("s.URL = %q, want the adopted redirect target %q - a stale pin keeps 307ing on upload", srv.URL, want)
	}
	if proxy.hostHits(stale) == 0 || proxy.hostHits(current) == 0 {
		t.Fatalf("both hops must ride the proxy: stale=%d current=%d hits",
			proxy.hostHits(stale), proxy.hostHits(current))
	}
}

// Proxy or not, an internal logical destination stays refused: the proxied
// request's dial only names the proxy, so the destination check has to fire
// before the proxy is ever asked for it.
func TestProxiedProbeRefusesInternalDestination(t *testing.T) {
	clearProxyEnv(t)
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // a proxy that happily tunnels anywhere
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)

	srv := &ookla.Server{ID: "internal", URL: "http://10.88.99.7:8080/speedtest/upload.php"}
	if st := probeFallback(context.Background(), srv); st != endpointUnknown {
		t.Fatalf("verdict = %v, want unknown (refused, not answered)", st)
	}
	if got := proxy.hostHits("10.88.99.7:8080"); got != 0 {
		t.Fatalf("the proxy was asked for the internal destination %d time(s); the guard must refuse first", got)
	}
}

// A NO_PROXY-excluded host is dialed DIRECT - net/http never consults the
// proxy for it - and the dial guard still applies to that direct dial.
func TestNoProxyExcludedHostStaysDirectAndGuarded(t *testing.T) {
	clearProxyEnv(t)
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)
	t.Setenv("NO_PROXY", "10.88.99.7")

	srv := &ookla.Server{ID: "noproxy", URL: "http://10.88.99.7:8080/speedtest/upload.php"}
	if st := probeFallback(context.Background(), srv); st != endpointUnknown {
		t.Fatalf("verdict = %v, want unknown - the direct dial must be refused by the dial guard", st)
	}
	if got := proxy.total(); got != 0 {
		t.Fatalf("proxy saw %d request(s) for a NO_PROXY-excluded host; it must be dialed direct", got)
	}
}

// The measurement client follows GET redirects itself (ping/download), so a
// proxied 302 to an internal literal used to reach an unchecked target: the
// pre-measure check vets only the FIRST destination, and with a proxy the dial
// guard only ever sees the proxy. Every hop must be vetted at the transport.
func TestMeasurementClientRefusesProxiedRedirectHop(t *testing.T) {
	clearProxyEnv(t)
	const origin, internal = "93.184.216.34:8080", "10.88.99.7:8080"
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == origin {
			w.Header().Set("Location", "http://"+internal+"/latency.txt")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("SECRET"))
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)

	// The same transport chain the library rides: New stamps uc.T and the doer
	// wraps it. An http.Client over uc.T follows redirects exactly as the doer
	// does, so this drives the production redirect-hop path without a transfer.
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	_, _ = newOoklaClientRec(uc)
	if uc.T == nil {
		t.Fatal("harness: the library did not stamp uc.T")
	}
	c := &http.Client{Transport: uc.T, Timeout: 10 * time.Second}
	resp, err := c.Get("http://" + origin + "/latency.txt")
	if err == nil {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("proxied GET followed the redirect to %s and read %q; the hop must be refused", internal, body)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("refusal must come from the destination guard, got: %v", err)
	}
	if got := proxy.hostHits(internal); got != 0 {
		t.Fatalf("the proxy was asked for the internal redirect target %d time(s)", got)
	}
	if got := proxy.hostHits(origin); got == 0 {
		t.Fatal("harness: the first hop never reached the proxy - nothing was exercised")
	}
}

// envProxyURL must make the same per-request decision net/http's
// ProxyFromEnvironment makes - scheme selection, the loopback/localhost
// exemption, and NO_PROXY in httpproxy's dialects - just read fresh instead of
// once-cached. This is the decision the dial guard's trust set and both
// per-request guards hang off, so each rule is pinned here.
func TestEnvProxyURLSemantics(t *testing.T) {
	mustURL := func(s string) *url.URL {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	proxied := func(raw string) bool { return envProxyURL(mustURL(raw)) != nil }

	clearProxyEnv(t)
	if proxied("http://example.com/x") {
		t.Fatal("proxied with no environment configured")
	}

	t.Setenv("HTTP_PROXY", "http://192.168.1.10:3128")
	if !proxied("http://example.com/x") {
		t.Error("http request must ride HTTP_PROXY")
	}
	if proxied("https://example.com/x") {
		t.Error("https request must NOT ride HTTP_PROXY - scheme selection is per-variable")
	}
	if proxied("http://127.0.0.1:9000/x") || proxied("http://[::1]:9000/x") || proxied("http://localhost:9000/x") {
		t.Error("loopback/localhost destinations are never proxied")
	}

	t.Setenv("HTTPS_PROXY", "https://192.168.1.11:3129")
	if got := envProxyURL(mustURL("https://example.com/x")); got == nil || got.Host != "192.168.1.11:3129" {
		t.Errorf("https request proxy = %v, want HTTPS_PROXY's endpoint", got)
	}

	// NO_PROXY dialects, per x/net/http/httpproxy.
	for _, c := range []struct {
		noProxy, target string
		wantDirect      bool
	}{
		{"*", "http://example.com/x", true},
		{"example.com", "http://example.com/x", true},
		{"example.com", "http://sub.example.com/x", true},   // bare domain covers subdomains
		{".example.com", "http://sub.example.com/x", true},  // leading dot: subdomains...
		{".example.com", "http://example.com/x", false},     // ...but not the apex
		{"*.example.com", "http://sub.example.com/x", true}, // wildcard = leading dot
		{"example.com", "http://notexample.com/x", false},   // suffix match is label-bounded
		{"10.0.0.0/8", "http://10.1.2.3:8080/x", true},      // CIDR vs IP literal
		{"10.0.0.0/8", "http://example.com/x", false},       // CIDR never matches a name
		{"93.184.216.34", "http://93.184.216.34:8080/x", true},
		{"example.com:8080", "http://example.com:8080/x", true},
		{"example.com:8081", "http://example.com:8080/x", false}, // port-qualified entry
	} {
		t.Setenv("NO_PROXY", c.noProxy)
		if got := !proxied(c.target); got != c.wantDirect {
			t.Errorf("NO_PROXY=%q target %s: direct=%v, want %v", c.noProxy, c.target, got, c.wantDirect)
		}
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "example.com")
	if proxied("http://example.com/x") {
		t.Error("lowercase no_proxy must be honoured")
	}

	// ALL_PROXY configures nothing: net/http never consults it.
	clearProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks5://10.11.12.13:1080")
	if proxied("http://example.com/x") || proxied("https://example.com/x") {
		t.Error("ALL_PROXY proxied a request net/http would send direct")
	}

	// A bare host:port value gets scheme http, like httpproxy's parse.
	clearProxyEnv(t)
	t.Setenv("HTTP_PROXY", "192.168.1.10:3128")
	if got := envProxyURL(mustURL("http://example.com/x")); got == nil || got.Scheme != "http" || got.Host != "192.168.1.10:3128" {
		t.Errorf("bare host:port proxy value parsed as %v, want http://192.168.1.10:3128", got)
	}
}

// Direct (unproxied) redirect behavior is unchanged: with no proxy configured
// the transport passes redirect hops straight through to the dial-time guard,
// which the loopback relaxation stands down for loopback-served suites.
func TestMeasurementClientDirectRedirectUnchanged(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)

	final, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fsrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})}
	go func() { _ = fsrv.Serve(final) }()
	t.Cleanup(func() { _ = fsrv.Close() })

	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rsrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+final.Addr().String()+"/f", http.StatusFound)
	})}
	go func() { _ = rsrv.Serve(first) }()
	t.Cleanup(func() { _ = rsrv.Close() })

	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	_, _ = newOoklaClientRec(uc)
	c := &http.Client{Transport: uc.T, Timeout: 10 * time.Second}
	resp, err := c.Get("http://" + first.Addr().String() + "/r")
	if err != nil {
		t.Fatalf("direct redirect must still be followed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if body, _ := io.ReadAll(resp.Body); string(body) != "ok" {
		t.Fatalf("redirect target answered %q, want ok", body)
	}
}
