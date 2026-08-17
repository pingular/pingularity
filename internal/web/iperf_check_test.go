package web

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// The iperf-check endpoint dials arbitrary hosts (LAN iperf3 servers are a
// legitimate target, so it is not SSRF-blocked). Its response time must not leak
// reachable/refused (fast) vs filtered (slow timeout) as a host-probing oracle:
// every outcome must take at least the uniform budget.
func TestIperfCheckUniformTiming(t *testing.T) {
	old := iperfCheckBudget
	iperfCheckBudget = 150 * time.Millisecond
	t.Cleanup(func() { iperfCheckBudget = old })
	s := newTestServer(t)

	call := func(addr string) (bool, time.Duration) {
		r := httptest.NewRequest("POST", "/api/iperf/check?addr="+url.QueryEscape(addr), nil)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		start := time.Now()
		s.handleIperfCheck(w, r)
		elapsed := time.Since(start)
		var body struct {
			Reachable bool `json:"reachable"`
		}
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		return body.Reachable, elapsed
	}

	// Reachable: a live local listener answers in ~1 RTT (near-instant).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	okReach, okDur := call(ln.Addr().String())
	if !okReach {
		t.Errorf("live listener reported unreachable")
	}

	// Unreachable: a blackholed TEST-NET-1 address (RFC 5737). Whether it fails
	// fast (no route) or slow (timeout), the floor must equalise it with the fast
	// reachable path above.
	badReach, badDur := call("192.0.2.1:9")
	if badReach {
		t.Errorf("blackhole reported reachable")
	}

	floor := iperfCheckBudget - 40*time.Millisecond // scheduling slack
	if okDur < floor {
		t.Errorf("reachable response in %v, below the %v floor - timing leaks reachability", okDur, iperfCheckBudget)
	}
	if badDur < floor {
		t.Errorf("unreachable response in %v, below the %v floor - timing leaks reachability", badDur, iperfCheckBudget)
	}
}
