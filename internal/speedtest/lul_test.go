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
	// Real network unavailable in tests isn't assumed; we only exercise the
	// gating path: stop immediately, far under both thresholds.
	stop := startLoadSampler(context.Background())
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
	origTarget, origResolved, origDial := lulTarget, lulResolved, lulResolveDial
	t.Cleanup(func() { lulTarget, lulResolved, lulResolveDial = origTarget, origResolved, origDial })

	lulTarget = "lul.invalid:443" // a hostname, so lulDialAddr takes the resolve path
	lulResolved = ""
	calls := 0
	lulResolveDial = func(context.Context, string) (net.Conn, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("simulated transient resolve failure")
		}
		return stubConn{remote: &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 443}}, nil
	}

	// 1st: resolve fails -> hostname fallback, nothing cached.
	if got := lulDialAddr(); got != lulTarget {
		t.Fatalf("after failed resolve = %q, want hostname fallback %q", got, lulTarget)
	}
	// 2nd: the miss was NOT cached, so the resolve retries and now succeeds.
	if got, want := lulDialAddr(), "203.0.113.7:443"; got != want {
		t.Fatalf("after recovered resolve = %q, want resolved literal %q", got, want)
	}
	// 3rd: served from cache, no further dial.
	if got, want := lulDialAddr(), "203.0.113.7:443"; got != want {
		t.Fatalf("cached resolve = %q, want %q", got, want)
	}
	if calls != 2 {
		t.Fatalf("resolve dialed %d times, want 2 (one failed retry, then one cached success)", calls)
	}
}

// A cached resolve must not outlive the stack it belongs to: after
// lulFailInvalidate consecutive failed connects the literal is dropped so the
// next sample re-resolves (the first resolve picked IPv6, IPv6 later died while
// IPv4 still works). A successful connect resets the streak.
func TestLulNoteConnectDropsDeadCache(t *testing.T) {
	origResolved, origFails := lulResolved, lulFails
	t.Cleanup(func() { lulResolved, lulFails = origResolved, origFails })

	lulResolved, lulFails = "203.0.113.7:443", 0
	for i := 0; i < lulFailInvalidate-1; i++ {
		lulNoteConnect(false)
	}
	if lulResolved == "" {
		t.Fatalf("cache dropped after %d failures, want it kept below the threshold", lulFailInvalidate-1)
	}
	lulNoteConnect(true) // success resets the streak
	for i := 0; i < lulFailInvalidate-1; i++ {
		lulNoteConnect(false)
	}
	if lulResolved == "" {
		t.Fatal("a successful connect must reset the failure streak")
	}
	lulNoteConnect(false) // streak reaches the threshold
	if lulResolved != "" {
		t.Fatalf("cache = %q after %d consecutive failures, want dropped for re-resolve", lulResolved, lulFailInvalidate)
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
	stop := startLoadSampler(ctx)
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
