package speedtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// clearProxyEnv scrubs every proxy variable the dial guard consults, so a
// proxy configured in the developer's or CI's real environment cannot leak
// into a test's verdicts. t.Setenv restores the originals on cleanup.
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, kv := range proxyEnvVars {
		t.Setenv(kv[0], "")
		t.Setenv(kv[1], "")
	}
}

// The operator's configured proxy endpoint is trusted local config: the dial
// guard must allow exactly it - and nothing else it would otherwise refuse.
// Without the allowance, HTTP_PROXY at a loopback/LAN address made net/http
// dial the proxy through the guarded dialer and every catalogue fetch,
// ranking ping and transfer failed.
func TestProbeDialGuardAllowsConfiguredProxy(t *testing.T) {
	clearProxyEnv(t)

	// No proxy configured: loopback stays refused - the pre-existing rule.
	if err := probeDialGuard("tcp", "127.0.0.1:3128", nil); err == nil {
		t.Fatal("guard allowed loopback with no proxy configured")
	}

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")
	if err := probeDialGuard("tcp", "127.0.0.1:3128", nil); err != nil {
		t.Errorf("guard refused the configured proxy address: %v", err)
	}
	// The allowance is the exact endpoint, not the proxy's whole host...
	if err := probeDialGuard("tcp", "127.0.0.1:9000", nil); err == nil {
		t.Error("guard allowed a loopback port the proxy does not use")
	}
	// ...and not other internal space on the proxy's port.
	if err := probeDialGuard("tcp", "10.0.0.1:3128", nil); err == nil {
		t.Error("guard allowed an unrelated private address on the proxy's port")
	}
	// Public destinations keep working with a proxy configured.
	if err := probeDialGuard("tcp", "93.184.216.34:8080", nil); err != nil {
		t.Errorf("guard refused a public address with a proxy configured: %v", err)
	}
}

// Every variable ProxyFromEnvironment consults must feed the allowance, in
// both cases, with net/http's scheme-default ports applied.
func TestProbeDialGuardProxyEnvForms(t *testing.T) {
	cases := []struct{ key, val, dial string }{
		{"HTTP_PROXY", "http://192.168.1.10:8888", "192.168.1.10:8888"},
		{"http_proxy", "http://192.168.1.10:8888", "192.168.1.10:8888"}, // lowercase form
		{"HTTPS_PROXY", "http://10.9.8.7:3128", "10.9.8.7:3128"},
		{"https_proxy", "10.9.8.7:3128", "10.9.8.7:3128"},            // bare host:port
		{"ALL_PROXY", "socks5://100.64.0.9:1080", "100.64.0.9:1080"}, // CGNAT-hosted SOCKS
		{"all_proxy", "socks5://100.64.0.9", "100.64.0.9:1080"},      // socks5 default port
		{"HTTP_PROXY", "http://172.16.0.2", "172.16.0.2:80"},         // http default port
		{"HTTPS_PROXY", "https://172.16.0.3", "172.16.0.3:443"},      // https default port
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.val, func(t *testing.T) {
			clearProxyEnv(t)
			if err := probeDialGuard("tcp", c.dial, nil); err == nil {
				t.Fatalf("precondition: guard already allowed %s with no proxy set", c.dial)
			}
			t.Setenv(c.key, c.val)
			if err := probeDialGuard("tcp", c.dial, nil); err != nil {
				t.Errorf("guard refused configured proxy %s=%q dialing %s: %v", c.key, c.val, c.dial, err)
			}
		})
	}
}

// A proxy configured by NAME must match the post-DNS ip:port Dialer.Control
// actually sees.
func TestProbeDialGuardResolvesProxyHostname(t *testing.T) {
	clearProxyEnv(t)
	ips, err := net.LookupIP("localhost")
	if err != nil || len(ips) == 0 {
		t.Skip("cannot resolve localhost on this host")
	}
	t.Setenv("HTTP_PROXY", "http://localhost:3128")
	for _, ip := range ips {
		addr := net.JoinHostPort(ip.String(), "3128")
		if err := probeDialGuard("tcp", addr, nil); err != nil {
			t.Errorf("guard refused %s, the resolved form of the configured proxy: %v", addr, err)
		}
	}
	if err := probeDialGuard("tcp", net.JoinHostPort(ips[0].String(), "9999"), nil); err == nil {
		t.Error("guard allowed the proxy host on a port the proxy does not use")
	}
}

// Proxy use must not void destination checks: with a proxy configured the dial
// guard only ever sees the proxy's ip:port, so the logical destination has its
// own pre-check - internal literals refused outright, hostnames resolved and
// checked. Without a proxy it is inert (the dial guard is the enforcement
// point), and it honours the loopback test relaxation.
func TestGuardProxiedDestination(t *testing.T) {
	ctx := context.Background()
	clearProxyEnv(t)

	if err := guardProxiedDestination(ctx, "127.0.0.1:9000"); err != nil {
		t.Fatalf("pre-check fired with no proxy configured: %v", err)
	}

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")
	blocked := []string{
		"127.0.0.1:9000",     // loopback (the daemon's own UI)
		"10.0.0.5:8080",      // RFC1918
		"169.254.169.254:80", // cloud metadata
		"100.64.0.5:8080",    // RFC 6598 CGNAT
		"198.51.100.7:80",    // RFC 5737 TEST-NET-2
		"[::1]:8080",         // IPv6 loopback
	}
	for _, d := range blocked {
		if err := guardProxiedDestination(ctx, d); err == nil {
			t.Errorf("pre-check allowed internal destination %s despite the proxy", d)
		}
	}
	if err := guardProxiedDestination(ctx, "93.184.216.34:8080"); err != nil {
		t.Errorf("pre-check refused a public literal: %v", err)
	}
	// Hostnames are resolved and checked, not trusted.
	if err := guardProxiedDestination(ctx, "localhost:8080"); err == nil {
		t.Error("pre-check allowed a hostname that resolves to loopback")
	}

	// The relaxation the loopback-served suites rely on disables the pre-check
	// the same way it disables the dial guard.
	old := probeDialControl
	probeDialControl = nil
	t.Cleanup(func() { probeDialControl = old })
	if err := guardProxiedDestination(ctx, "127.0.0.1:9000"); err != nil {
		t.Errorf("pre-check fired despite the loopback relaxation: %v", err)
	}
}

// The measurement path must refuse an internal logical destination BEFORE any
// request names it, because with a proxy configured those requests would ride
// the proxy past the dial guard.
func TestMeasureRefusesInternalDestinationWhenProxied(t *testing.T) {
	clearProxyEnv(t)
	var hits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer internal.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")

	addr := internal.Listener.Addr().String()
	srv := &ookla.Server{ID: "internal-dest", Host: addr, URL: "http://" + addr + "/speedtest/upload.php"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := NewOokla().measure(ctx, srv, "both", 0)
	if err == nil || !strings.Contains(err.Error(), "refusing speedtest destination") {
		t.Fatalf("measure err = %v, want the destination refusal", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("measure reached the internal destination %d time(s)", n)
	}
}

// The discovery fetch drops hostile catalogue entries when a proxy would carry
// their traffic, and leaves the list untouched when no proxy is configured.
func TestGuardedServersFiltersInternalHosts(t *testing.T) {
	clearProxyEnv(t)
	ctx := context.Background()
	servers := ookla.Servers{
		{ID: "1", Host: "127.0.0.1:9000", URL: "http://127.0.0.1:9000/speedtest/upload.php"},
		{ID: "2", Host: "93.184.216.34:8080", URL: "http://93.184.216.34:8080/speedtest/upload.php"},
		{ID: "3", URL: "http://198.51.100.7:8080/speedtest/upload.php"}, // Host empty: the URL is the destination
	}
	if got := guardedServers(ctx, servers); len(got) != 3 {
		t.Fatalf("no-proxy filter dropped %d server(s), want 0", 3-len(got))
	}
	t.Setenv("HTTPS_PROXY", "http://192.168.1.10:3128")
	got := guardedServers(ctx, servers)
	if len(got) != 1 || got[0].ID != "2" {
		ids := make([]string, 0, len(got))
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("proxied filter kept %v, want only the public server (id 2)", ids)
	}
}

// probeEndpoint must not adopt a redirect target the proxied path could not be
// allowed to reach, even when the guarded re-probe would answer (here: the
// target is loopback-served and the dial control is relaxed for it, standing
// in for a public proxy that happily tunnels to internal space).
func TestProbeEndpointRefusesProxiedInternalRedirect(t *testing.T) {
	clearProxyEnv(t)
	var internalHits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&internalHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+internal.Listener.Addr().String()+"/speedtest/upload.php", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	// The dial control allows everything - the shape of a public proxy happily
	// tunneling to whatever destination it is asked for. Only the logical
	// destination pre-check stands between the redirect and adoption, and it
	// needs a proxy configured to arm itself. probeDialControl stays NON-nil so
	// the pre-check does not read it as the loopback relaxation.
	old := probeDialControl
	probeDialControl = func(string, string, syscall.RawConn) error { return nil }
	t.Cleanup(func() { probeDialControl = old })
	t.Setenv("HTTP_PROXY", "http://192.168.1.10:3128") // never dialed; probeClient goes direct

	srv := &ookla.Server{ID: "proxied-poison", URL: first.URL + "/speedtest/upload.php"}
	orig := srv.URL

	st := probeEndpoint(context.Background(), srv)

	if st != endpointUnknown {
		t.Fatalf("verdict = %v, want endpointUnknown (pre-check refused the redirect target)", st)
	}
	if srv.URL != orig {
		t.Fatalf("s.URL was POISONED to %q (want left at %q) - a refused destination must not be adopted", srv.URL, orig)
	}
	if n := atomic.LoadInt32(&internalHits); n != 0 {
		t.Fatalf("probe reached the internal redirect target %d time(s); the pre-check should have refused it", n)
	}
}
