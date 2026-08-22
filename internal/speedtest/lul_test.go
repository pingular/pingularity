package speedtest

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{[]float64{5}, 5},
		{[]float64{3, 1}, 2},
		{[]float64{9, 1, 5}, 5},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{1000, 5, 6, 5, 5}, 5}, // one RTO outlier doesn't move the median
	}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// The loaded sampler must return nil when the phase is too short or yields too
// few samples - a number from an unsaturated instant would be misleading.
func TestLoadSamplerGating(t *testing.T) {
	origFails := lulFails
	t.Cleanup(func() { lulFails = origFails })
	// Real network unavailable in tests isn't assumed; we only exercise the
	// gating path: stop immediately, far under both thresholds. The empty address
	// is the offline part - the dial fails without touching a resolver.
	stop := startLoadSampler(context.Background(), "")
	time.Sleep(50 * time.Millisecond)
	if got := stop(); got != nil {
		t.Fatalf("sampler returned %v for a %v phase, want nil (< %v / < %d samples)", *got, 50*time.Millisecond, lulMinPhase, lulMinSamples)
	}
}

// A failed resolve must NOT be cached: lulDialAddr falls back to the hostname for
// that call but retries the resolve next time, and caches only once it succeeds.
// (The old sync.Once cached the miss and then dialed the hostname - paying DNS in
// every timed handshake - for the whole process.)
func TestLulDialAddrRetriesAfterResolveFailure(t *testing.T) {
	origTarget, origResolved, origFails, origDial := lulTarget, lulResolved, lulFails, lulResolveDial
	origBurst, origFam := lulProbeBurst, lulFamilyDial
	t.Cleanup(func() {
		lulTarget, lulResolved, lulFails, lulResolveDial = origTarget, origResolved, origFails, origDial
		lulProbeBurst, lulFamilyDial = origBurst, origFam
	})

	lulTarget = "lul.invalid:443" // a hostname, so lulDialAddr takes the resolve path
	lulResolved, lulFails = "", 0
	// Selection is not under test here: report the candidate clean so the
	// caching semantics stay the subject, and keep both seams off the network.
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		return []float64{1, 1, 1, 1, 1}, 0
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("not under test")
	}
	calls := 0
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("simulated transient resolve failure")
		}
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 443}}, nil
	}

	// 1st: resolve fails -> hostname fallback, nothing cached.
	if got := lulDialAddr(context.Background()); got != lulTarget {
		t.Fatalf("after failed resolve = %q, want hostname fallback %q", got, lulTarget)
	}
	// 2nd: the miss was NOT cached, so the resolve retries and now succeeds.
	if got, want := lulDialAddr(context.Background()), "203.0.113.7:443"; got != want {
		t.Fatalf("after recovered resolve = %q, want resolved literal %q", got, want)
	}
	// 3rd: served from cache, no further dial.
	if got, want := lulDialAddr(context.Background()), "203.0.113.7:443"; got != want {
		t.Fatalf("cached resolve = %q, want %q", got, want)
	}
	if calls != 2 {
		t.Fatalf("resolve dialed %d times, want 2 (one failed retry, then one cached success)", calls)
	}
}

// A RUN PINS ONE ENDPOINT EVEN WHEN THE RESOLVE FAILS. Handing back "" would
// leave every sample to resolve for itself: on an idle link that is a whole
// family selection per probe, so the idle burst spends its budget on failed
// selections instead of probes and reports no baseline at all; inside a load
// phase it is a DNS lookup in every timed handshake, inflating the bufferbloat
// number being measured. The hostname is the honest fallback - worse than a
// literal, but one name for every sample of the run.
func TestLulRunEndpointPinsHostnameWhenResolveFails(t *testing.T) {
	lulSelectSeams(t)
	resolves := 0
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		resolves++
		return nil, errors.New("simulated resolve failure")
	}
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		t.Error("validated a candidate although the resolve failed")
		return nil, lulSelectProbes
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("no candidate: must not be reached")
	}

	if got := lulRunEndpoint(context.Background()); got != lulTarget {
		t.Fatalf("lulRunEndpoint = %q with the resolve failing, want the hostname %q: \"\" "+
			"leaves every sample to resolve for itself", got, lulTarget)
	}
	// And a sample dials what it was handed, whatever the cache holds: no
	// selection, no DNS, nothing but the timed handshake.
	before := resolves
	if _, ok := connectRTT(context.Background(), ""); ok {
		t.Fatal("connectRTT reported a successful sample without an address to dial")
	}
	if resolves != before {
		t.Fatalf("one sample ran %d extra resolve dials, want 0: a sample must dial the "+
			"endpoint the run pinned, never re-enter the selection", resolves-before)
	}
}

// A CACHED LITERAL IS DROPPED AT A RUN BOUNDARY, NOT MID-RUN. The cache must not
// outlive the stack it belongs to: after lulFailInvalidate consecutive failed
// connects the literal is dead and the next run must re-resolve (the first
// resolve picked IPv6, IPv6 later died while IPv4 still works). But dropping it
// the moment the streak lands drops it INSIDE a saturated phase - congestion
// produces those failures too - and the phases after it are then left dialing a
// hostname, paying DNS inside the very handshakes being timed. So lulNoteConnect
// only counts and lulDialAddr, reached once per run on an idle link, does the
// dropping. A successful connect clears the streak: a path that recovered is not
// dead.
func TestLulDeadLiteralDroppedAtRunBoundary(t *testing.T) {
	lulSelectSeams(t)
	lulResolved = "203.0.113.7:443"
	resolves := 0
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		resolves++
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("2001:db8::7"), Port: 443}}, nil
	}
	lulProbeBurst = func(context.Context, string, int, time.Duration) ([]float64, int) {
		return []float64{7.0, 7.2, 6.9, 7.1, 7.0}, 0
	}
	lulFamilyDial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("clean winner: must not consult the other family")
	}

	for i := 0; i < lulFailInvalidate-1; i++ {
		lulNoteConnect(false)
	}
	lulNoteConnect(true) // a success clears the streak
	for i := 0; i < lulFailInvalidate-1; i++ {
		lulNoteConnect(false)
	}
	if got := lulRunEndpoint(context.Background()); got != "203.0.113.7:443" {
		t.Fatalf("lulRunEndpoint = %q below the failure threshold, want the cached literal: "+
			"only a streak that reaches lulFailInvalidate=%d means dead", got, lulFailInvalidate)
	}
	if resolves != 0 {
		t.Fatalf("re-resolved %d times below the failure threshold, want 0", resolves)
	}

	lulNoteConnect(false) // the streak lands, mid-run
	if lulResolved == "" {
		t.Fatal("the cached literal was dropped the moment the streak landed; want it held to " +
			"the run boundary, so no later phase is left dialing a hostname mid-run")
	}
	if got := lulRunEndpoint(context.Background()); got != v6Winner {
		t.Fatalf("lulRunEndpoint = %q on the next run, want the re-selected literal %q: a dead "+
			"literal must not survive a run boundary either", got, v6Winner)
	}
	if resolves != 1 {
		t.Fatalf("re-resolved %d times at the run boundary, want exactly 1", resolves)
	}
}

// stubConn is a net.Conn whose only methods lulDialAddr uses are RemoteAddr and
// Close; the embedded nil net.Conn would panic if anything else were called.
type stubConn struct {
	net.Conn
	remote net.Addr
}

func (c stubConn) RemoteAddr() net.Addr { return c.remote }
func (c stubConn) Close() error         { return nil }

// A cancelled context must end the sampler promptly and yield nil.
func TestLoadSamplerHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop := startLoadSampler(ctx, "")
	done := make(chan *loadStat, 1)
	go func() { done <- stop() }()
	select {
	case v := <-done:
		if v != nil {
			t.Fatalf("cancelled sampler returned %v, want nil", *v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not stop after context cancellation")
	}
}
