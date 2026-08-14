package speedtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A destination that routes DIRECT still has to be vetted, because the dial
// guard deliberately EXEMPTS the operator's configured proxy address - it has
// to, since a proxied dial only ever names the proxy.
//
// That exemption is an attack surface if the routing decision is made first: a
// hostile catalogue entry or redirect naming the proxy's OWN host:port routes
// direct (loopback / NO_PROXY), skips destination vetting, and is then waved
// through the dial guard because the address matches the proxy. The result is
// an origin-form request, with an attacker-chosen path, delivered to a service
// on the operator's own machine by the daemon.
//
// So the destination must be judged on WHAT IT IS, before deciding HOW to
// reach it.
func TestProxyAddressIsNotReachableAsADirectDestination(t *testing.T) {
	var hits atomic.Int64
	var gotPath atomic.Value
	gotPath.Store("")
	// Stands in for the operator's local proxy. Anything that arrives here
	// arrived because the daemon dialed it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		w.WriteHeader(200)
	}))
	defer proxy.Close()
	host := proxy.Listener.Addr().String()

	t.Setenv("HTTP_PROXY", "http://"+host)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	flushDestResolveCache()

	// The real guard, as production runs it - not the loopback relaxation.
	if probeDialControl == nil {
		t.Skip("dial guard disarmed in this build")
	}

	tr := &http.Transport{
		Proxy:       guardedEnvProxy,
		DialContext: (&net.Dialer{Timeout: 2 * time.Second, Control: probeDialControl}).DialContext,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// Exactly what a hostile redirect would name: the proxy's own address, with
	// a path of the attacker's choosing.
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+host+"/attacker-path", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
	}

	if n := hits.Load(); n != 0 {
		t.Fatalf("the daemon delivered %d request(s) to the local proxy port as an ORIGIN-FORM request (path %q): "+
			"a destination that merely LOOKS like the configured proxy must not inherit the proxy's dial exemption",
			n, gotPath.Load())
	}
	if err == nil {
		t.Fatal("the request succeeded without reaching the listener, which means it went somewhere unexpected")
	}
}

// The guard must not have been bought by breaking real proxying: an ordinary
// public destination still routes through the configured proxy.
func TestOrdinaryDestinationStillRoutesThroughTheProxy(t *testing.T) {
	var proxied atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(200)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", "http://"+proxy.Listener.Addr().String())
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NO_PROXY", "")
	flushDestResolveCache()

	tr := &http.Transport{
		Proxy:       guardedEnvProxy,
		DialContext: (&net.Dialer{Timeout: 2 * time.Second, Control: probeDialControl}).DialContext,
	}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// A public IP LITERAL: no DNS needed, and it is not in any blocked range,
	// so this is the ordinary case the guard must leave alone.
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://93.184.216.34/latency.txt", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("a public destination must still be proxied: %v", err)
	}
	resp.Body.Close()
	if proxied.Load() == 0 {
		t.Fatal("the request did not go through the configured proxy at all")
	}
}
