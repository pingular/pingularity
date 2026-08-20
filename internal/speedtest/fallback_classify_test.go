package speedtest

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Classification against locally emulated servers. This is the half of the
// category coverage that must NEVER depend on the internet: whether
// fallbackHealth reads a status correctly is our logic, not Ookla's, so it
// should not be able to degrade to a skip because someone repaired a server.
//
// real_categories_test.go covers the other half - that live servers really do
// behave the way these fakes pretend to.

// fakeFallback serves the HTTP Legacy Fallback bundle, or refuses to.
type fakeFallback struct {
	status  int // status for /speedtest/*
	hits    atomic.Int64
	latency time.Duration // delay before answering, to exercise the timeout
}

func (f *fakeFallback) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/speedtest/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.latency > 0 {
			select {
			case <-time.After(f.latency):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(f.status)
		if f.status < 300 {
			_, _ = w.Write([]byte("test=test"))
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func classifyFake(t *testing.T, id string, f *fakeFallback) endpointState {
	t.Helper()
	allowLoopbackProbes(t)
	host := f.start(t)
	s := &ookla.Server{ID: id, Host: host, URL: "http://" + host + "/speedtest/upload.php"}
	fbMu.Lock()
	delete(fbMap, id)
	fbMu.Unlock()
	return fallbackHealth(context.Background(), s)
}

// probeFake exercises the raw status->state mapping in probeFallback, WITHOUT
// fallbackHealth's two-strike rule on top. The two are separate concerns: this
// asserts how one response is read, TestClassifyTransientFailureNeedsTwoStrikes
// asserts how repeated failures are treated.
func probeFake(t *testing.T, id string, f *fakeFallback) endpointState {
	t.Helper()
	allowLoopbackProbes(t)
	host := f.start(t)
	s := &ookla.Server{ID: id, Host: host, URL: "http://" + host + "/speedtest/upload.php"}
	return probeFallback(context.Background(), s)
}

func TestClassifyFallbackStates(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   endpointState
	}{
		{"200 bundle present", http.StatusOK, endpointOK},
		{"204 also fine", http.StatusNoContent, endpointOK},
		{"500 bundle absent", http.StatusInternalServerError, endpointRetired},
		{"404 bundle absent", http.StatusNotFound, endpointRetired},
		{"403 refused", http.StatusForbidden, endpointRetired},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := probeFake(t, "fake"+string(rune('a'+i)), &fakeFallback{status: c.status})
			if got != c.want {
				t.Fatalf("status %d classified as %v, want %v", c.status, got, c.want)
			}
		})
	}
}

// A server that never answers must be UNKNOWN, not condemned: the fault could be
// our own link, and ranking keeps unknowns.
func TestClassifyUnreachableIsUnknown(t *testing.T) {
	// Listener that accepts and then says nothing until the probe times out.
	f := &fakeFallback{status: http.StatusOK, latency: 2 * fallbackProbeTimeout}
	got := classifyFake(t, "fakeslow", f)
	if got != endpointUnknown {
		t.Fatalf("a silent server classified as %v, want unknown", got)
	}
}

// The cache must actually cache - a scheduled run should not re-probe every
// server on every pass - and a verdict must expire so a repaired server can
// come back.
func TestClassifyCachesAndExpires(t *testing.T) {
	allowLoopbackProbes(t)
	// A HEALTHY server: caching and the two-strike classification are separate
	// concerns, and a 500 now legitimately reads as unknown on first sight.
	f := &fakeFallback{status: http.StatusOK}
	host := f.start(t)
	s := &ookla.Server{ID: "cachetest", Host: host, URL: "http://" + host + "/speedtest/upload.php"}

	fbMu.Lock()
	delete(fbMap, s.ID)
	fbMu.Unlock()

	if got := fallbackHealth(context.Background(), s); got != endpointOK {
		t.Fatalf("first probe = %v", got)
	}
	first := f.hits.Load()
	for i := 0; i < 5; i++ {
		_ = fallbackHealth(context.Background(), s)
	}
	if f.hits.Load() != first {
		t.Fatalf("cache miss: %d probes for 6 lookups", f.hits.Load())
	}

	// Expire it by hand and confirm the next lookup goes back to the wire.
	fbMu.Lock()
	fbMap[s.ID] = fallbackVerdict{state: endpointOK, expires: time.Now().Add(-time.Second)}
	fbMu.Unlock()
	_ = fallbackHealth(context.Background(), s)
	if f.hits.Load() <= first {
		t.Fatal("an expired verdict was not re-probed - a repaired server could never recover")
	}
}

// A transport-level failure is retried soon; a definite verdict is held long.
// Getting these the wrong way round would either hammer dead servers or pin a
// blip for half a day.
func TestClassifyUnknownExpiresSooner(t *testing.T) {
	if fallbackUnknownTTL >= fallbackTTL {
		t.Fatalf("unknown TTL %v must be shorter than the definite TTL %v",
			fallbackUnknownTTL, fallbackTTL)
	}
}

// The cache must not grow without bound as a long-lived daemon cycles through
// servers.
func TestClassifyCacheBounded(t *testing.T) {
	fbMu.Lock()
	for k := range fbMap {
		delete(fbMap, k)
	}
	for i := 0; i < fallbackMapCap+50; i++ {
		fbMap[string(rune(i))+"x"] = fallbackVerdict{state: endpointOK, expires: time.Now().Add(time.Hour)}
	}
	fbMu.Unlock()

	f := &fakeFallback{status: http.StatusOK}
	_ = classifyFake(t, "overflow", f)

	fbMu.Lock()
	n := len(fbMap)
	fbMu.Unlock()
	if n > fallbackMapCap {
		t.Fatalf("cache grew to %d, cap is %d", n, fallbackMapCap)
	}
}

// The picker must never wait on the fleet. A list of unresponsive servers has
// to return within the annotation budget, undecorated, rather than hanging the
// UI for waves of per-probe timeouts.
func TestAnnotateFallbackIsBounded(t *testing.T) {
	allowLoopbackProbes(t)
	slow := &fakeFallback{status: http.StatusOK, latency: 30 * time.Second}
	host := slow.start(t)

	const n = 63 // a full catalogue page
	servers := make(ookla.Servers, 0, n)
	out := make([]ServerInfo, 0, n)
	for i := 0; i < n; i++ {
		id := "slow" + string(rune('A'+i%26)) + string(rune('a'+i/26))
		servers = append(servers, &ookla.Server{ID: id, Host: host,
			URL: "http://" + host + "/speedtest/upload.php"})
		out = append(out, ServerInfo{ID: id})
		fbMu.Lock()
		delete(fbMap, id)
		fbMu.Unlock()
	}

	start := time.Now()
	annotateFallback(context.Background(), servers, out)
	elapsed := time.Since(start)

	if elapsed > annotateFallbackBudget+2*time.Second {
		t.Fatalf("annotation took %s against unresponsive servers; budget is %s",
			elapsed.Round(time.Millisecond), annotateFallbackBudget)
	}
	for i, si := range out {
		if si.FallbackOK != nil {
			t.Fatalf("server %d got a verdict %v from a server that never answered", i, *si.FallbackOK)
		}
	}
	t.Logf("63 unresponsive servers annotated in %s (budget %s), all left undetermined",
		elapsed.Round(time.Millisecond), annotateFallbackBudget)
}

// A redirect means the bundle MOVED, not that it is gone. Condemning a server
// on a 3xx would exclude a usable one for fallbackTTL - and "the legacy name
// redirects to the current one" is the exact shape of issues #17/#18, so this
// is the case the guard is most likely to meet on the next migration wave.
func TestClassifyFollowsRedirect(t *testing.T) {
	allowLoopbackProbes(t)
	good := &fakeFallback{status: http.StatusOK}
	target := good.start(t)

	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/speedtest/", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://"+target+r.URL.Path, code)
			})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv := &http.Server{Handler: mux}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			id := "redir" + http.StatusText(code)
			host := ln.Addr().String()
			s := &ookla.Server{ID: id, Host: host, URL: "http://" + host + "/speedtest/upload.php"}
			fbMu.Lock()
			delete(fbMap, id)
			fbMu.Unlock()

			if got := fallbackHealth(context.Background(), s); got != endpointOK {
				t.Fatalf("HTTP %d to a healthy target classified as %v, want ok", code, got)
			}
		})
	}
}

// A redirect LOOP is unresolved, not absent - unknown, so ranking keeps it.
func TestClassifyRedirectLoopIsUnknown(t *testing.T) {
	allowLoopbackProbes(t)
	mux := http.NewServeMux()
	var host string
	mux.HandleFunc("/speedtest/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+host+r.URL.Path, http.StatusTemporaryRedirect)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host = ln.Addr().String()
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	s := &ookla.Server{ID: "loop", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
	fbMu.Lock()
	delete(fbMap, s.ID)
	fbMu.Unlock()
	if got := fallbackHealth(context.Background(), s); got == endpointRetired {
		t.Fatal("a redirect loop must not condemn the server as having no fallback")
	}
}

// A single definite failure must NOT retire a server: 429/500/502/503 and a
// maintenance window are indistinguishable from a missing bundle in one
// response, and a 12h exclusion on one bad moment removes a healthy server for
// half a day. Two consecutive strikes are required (fallbackStrikes).
func TestClassifyTransientFailureNeedsTwoStrikes(t *testing.T) {
	allowLoopbackProbes(t)
	for _, code := range []int{http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := &fakeFallback{status: code}
			host := f.start(t)
			id := "strike" + http.StatusText(code)
			s := &ookla.Server{ID: id, Host: host, URL: "http://" + host + "/speedtest/upload.php"}
			fbMu.Lock()
			delete(fbMap, id)
			fbMu.Unlock()

			if got := fallbackHealth(context.Background(), s); got != endpointUnknown {
				t.Fatalf("first %d = %v, want unknown - one bad response is not a retirement", code, got)
			}
			// Expire the short hold and fail again: now it is persistent.
			fbMu.Lock()
			v := fbMap[id]
			v.expires = time.Now().Add(-time.Second)
			fbMap[id] = v
			fbMu.Unlock()
			if got := fallbackHealth(context.Background(), s); got != endpointRetired {
				t.Fatalf("second consecutive %d = %v, want retired", code, got)
			}
		})
	}
}

// A probe abandoned by OUR deadline is a fact about us, not the server. Caching
// it would let a picker timeout suppress a real probe for minutes.
func TestClassifyDoesNotCacheCallerCancellation(t *testing.T) {
	allowLoopbackProbes(t)
	f := &fakeFallback{status: http.StatusOK, latency: 2 * fallbackProbeTimeout}
	host := f.start(t)
	s := &ookla.Server{ID: "cancelled", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
	fbMu.Lock()
	delete(fbMap, s.ID)
	fbMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := fallbackHealth(ctx, s); got != endpointUnknown {
		t.Fatalf("got %v, want unknown", got)
	}
	fbMu.Lock()
	_, cached := fbMap[s.ID]
	fbMu.Unlock()
	if cached {
		t.Fatal("a caller-cancelled probe was cached - it says nothing about the server")
	}
}

// Every probe destination is chosen by a third party (a catalogue host, or a
// Location header). None of them may be internal.
func TestProbeRefusesInternalDestinations(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9000", "10.0.0.5:8080", "192.168.1.1:8080",
		"169.254.169.254:80", "[::1]:8080"} {
		if err := probeDialGuard("tcp", addr, nil); err == nil {
			t.Errorf("probe to %s was allowed - a hostile server list entry could redirect us there", addr)
		}
	}
	if err := probeDialGuard("tcp", "93.184.216.34:8080", nil); err != nil {
		t.Errorf("a public address must still be reachable: %v", err)
	}
}

// The starvation signature must scale with the configured stream count, or the
// supported 13-16 connection settings can never trigger the rescue.
func TestStarvationCeilingTracksWorkers(t *testing.T) {
	if got := starvationCeiling(16); got <= 16 {
		t.Errorf("ceiling(16) = %d, must exceed the attempt count a 16-stream starve produces", got)
	}
	if got := starvationCeiling(0); got < 1 {
		t.Errorf("ceiling(0) = %d, want the library default worker count", got)
	}
	if starvationCeiling(4) >= starvationCeiling(16) {
		t.Error("the ceiling must grow with the worker count")
	}
}

// allowLoopbackProbes relaxes the probe dial guard for tests that serve their
// fakes on 127.0.0.1. The guard itself is asserted separately against the real
// probeDialGuard, so this cannot mask a regression in it.
func allowLoopbackProbes(t *testing.T) {
	t.Helper()
	old := probeDialControl()
	setProbeDialControl(nil)
	t.Cleanup(func() { setProbeDialControl(old) })
}
