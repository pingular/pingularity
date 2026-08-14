package speedtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The logical-destination guard runs on EVERY proxied request (that is the
// point: a redirect hop re-enters the transport and must be vetted again). It
// resolved the destination name inline each time, so a proxied transfer - one
// chunk request per POST, thousands in a few seconds - paid a resolver lookup
// per request, inside the window whose bytes are being timed. The vetting stays
// per-request; only the RESOLUTION is memoized.

// stubDestLookup replaces the guard's resolver with a counting fake and returns
// the counter. The fake answers every name with one public address, so a test
// can use a hostname destination without touching real DNS.
func stubDestLookup(t *testing.T, ips []net.IP, err error) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	old := lookupDestIPs
	lookupDestIPs = func(ctx context.Context, host string) ([]net.IP, error) {
		n.Add(1)
		return ips, err
	}
	t.Cleanup(func() { lookupDestIPs = old })
	flushDestResolveCache()
	t.Cleanup(flushDestResolveCache)
	return &n
}

// The hot path: 50 proxied GETs over ONE reused keep-alive connection. Each is
// a separate guard call - the security property - but they must share one
// resolution, or a real transfer's thousands of chunk POSTs each pay a lookup
// in the middle of the measurement.
func TestProxiedDestinationResolvesOncePerHotPath(t *testing.T) {
	clearProxyEnv(t)
	lookups := stubDestLookup(t, []net.IP{net.ParseIP("93.184.216.34")}, nil)

	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)

	// The production transport chain: New stamps uc.T, newOoklaClientRec puts
	// guardedEnvProxy on it. Keep-alives left on, so all 50 requests ride one
	// connection and only the per-REQUEST work can explain a per-request count.
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
	_, _ = newOoklaClientRec(uc)
	if uc.T == nil {
		t.Fatal("harness: the library did not stamp uc.T")
	}
	c := &http.Client{Transport: uc.T, Timeout: 10 * time.Second}

	const n = 50
	for i := 0; i < n; i++ {
		resp, err := c.Get("http://speedtest.example.net:8080/upload.php")
		if err != nil {
			t.Fatalf("proxied GET %d failed: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	if got := proxy.hostHits("speedtest.example.net:8080"); got != n {
		t.Fatalf("harness: proxy served %d of %d requests - the hot path was not exercised", got, n)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("%d proxied requests performed %d resolver lookups, want 1 - the lookup lands inside the timed transfer",
			n, got)
	}
}

// The memo must not weaken the verdict: a name that resolves internal is
// refused on the first call and on every later one, and the refusal is not
// paid for with a fresh lookup each time either.
func TestProxiedDestinationMemoStaysFailClosed(t *testing.T) {
	clearProxyEnv(t)
	lookups := stubDestLookup(t, []net.IP{net.ParseIP("10.88.99.7")}, nil)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:5801")

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := guardProxiedDestination(ctx, "rebind.example.net:8080"); err == nil {
			t.Fatalf("call %d allowed a host resolving to internal space", i)
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("5 refusals cost %d lookups, want 1", got)
	}
}

// A memoized entry expires, so a destination that moves is picked up. The
// window is the documented DNS-rebinding residual, not an indefinite one.
func TestProxiedDestinationMemoExpires(t *testing.T) {
	clearProxyEnv(t)
	lookups := stubDestLookup(t, []net.IP{net.ParseIP("93.184.216.34")}, nil)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:5801")

	old := destResolveTTL
	destResolveTTL = time.Millisecond
	t.Cleanup(func() { destResolveTTL = old })

	ctx := context.Background()
	if err := guardProxiedDestination(ctx, "moving.example.net:8080"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := guardProxiedDestination(ctx, "moving.example.net:8080"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("lookups = %d across an expired entry, want 2", got)
	}
}

// Distinct destinations keep distinct verdicts - the memo is per host, not a
// single last-answer slot, and a public host must never inherit an internal
// one's cached addresses (or the reverse).
func TestProxiedDestinationMemoIsPerHost(t *testing.T) {
	clearProxyEnv(t)
	byHost := map[string][]net.IP{
		"public.example.net":   {net.ParseIP("93.184.216.34")},
		"internal.example.net": {net.ParseIP("169.254.169.254")},
	}
	var n atomic.Int64
	oldLookup := lookupDestIPs
	lookupDestIPs = func(_ context.Context, host string) ([]net.IP, error) {
		n.Add(1)
		if ips, ok := byHost[host]; ok {
			return ips, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { lookupDestIPs = oldLookup })
	flushDestResolveCache()
	t.Cleanup(flushDestResolveCache)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:5801")

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := guardProxiedDestination(ctx, "public.example.net:8080"); err != nil {
			t.Fatalf("public host refused on call %d: %v", i, err)
		}
		if err := guardProxiedDestination(ctx, "internal.example.net:8080"); err == nil {
			t.Fatalf("internal host allowed on call %d", i)
		}
		if err := guardProxiedDestination(ctx, "nxdomain.example.net:8080"); err == nil {
			t.Fatalf("unresolvable host allowed on call %d", i)
		}
	}
	if got := n.Load(); got != 3 {
		t.Fatalf("lookups = %d for 3 distinct hosts probed 3 times each, want 3", got)
	}
}

// A cancelled/expired context must not poison the memo: the lookup failed
// because the RUN ended, which says nothing about the host, and caching that
// would refuse the destination for the whole TTL on the next run.
func TestProxiedDestinationMemoSkipsContextFailures(t *testing.T) {
	clearProxyEnv(t)
	var n atomic.Int64
	oldLookup := lookupDestIPs
	lookupDestIPs = func(ctx context.Context, _ string) ([]net.IP, error) {
		n.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("lookup: %w", err)
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	t.Cleanup(func() { lookupDestIPs = oldLookup })
	flushDestResolveCache()
	t.Cleanup(flushDestResolveCache)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:5801")

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guardProxiedDestination(dead, "later.example.net:8080"); err == nil {
		t.Fatal("a cancelled lookup must refuse, not wave the host through")
	}
	if err := guardProxiedDestination(context.Background(), "later.example.net:8080"); err != nil {
		t.Fatalf("the next run inherited the cancelled run's failure: %v", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2 - the cancelled result must not be cached", got)
	}
}
