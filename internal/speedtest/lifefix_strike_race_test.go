package speedtest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// fallbackHealth used to be check-then-act across the fbMu unlock gap: two
// concurrent callers for one expired server ID both probed and both wrote
// fails=prevFails+1 from the same stale snapshot - losing a consecutive strike
// (so two-strike retirement never accumulated under concurrency) and
// duplicating probe traffic. The in-flight guard serializes per ID: exactly
// one caller probes per burst, everyone else waits and reuses the verdict, and
// strikes accumulate across bursts to retirement. Run with -race.
func TestFallbackHealthSerializesConcurrentProbes(t *testing.T) {
	orig := probeFallback
	t.Cleanup(func() { probeFallback = orig })
	var probes atomic.Int32
	probeFallback = func(_ context.Context, _ *ookla.Server) endpointState {
		probes.Add(1)
		// Hold the probe open long enough that every concurrent caller has read
		// the expired cache and committed to its path before a result lands.
		time.Sleep(150 * time.Millisecond)
		return endpointRetired
	}

	const id = "strike-race"
	s := &ookla.Server{ID: id, Host: "s.example:8080", URL: "http://s.example:8080/speedtest/upload.php"}
	expire := func(v fallbackVerdict) {
		v.expires = time.Now().Add(-time.Second)
		fbMu.Lock()
		fbMap[id] = v
		fbMu.Unlock()
	}
	verdict := func() fallbackVerdict {
		fbMu.Lock()
		defer fbMu.Unlock()
		return fbMap[id]
	}
	burst := func() []endpointState {
		const callers = 8
		out := make([]endpointState, callers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				out[i] = fallbackHealth(context.Background(), s)
			}(i)
		}
		close(start)
		wg.Wait()
		return out
	}

	expire(fallbackVerdict{state: endpointUnknown})
	for i, st := range burst() {
		if st != endpointUnknown {
			t.Fatalf("caller %d saw %v after the first strike, want unknown (held, not yet retired)", i, st)
		}
	}
	if got := probes.Load(); got != 1 {
		t.Fatalf("first burst ran %d probes for one server ID, want 1 - concurrent callers must wait and reuse the in-flight result", got)
	}
	if v := verdict(); v.fails != 1 {
		t.Fatalf("after one failing burst fails=%d, want 1", v.fails)
	}

	// Expire the first-strike hold; the next concurrent burst is the second
	// consecutive definite failure and must RETIRE the server - exactly the
	// accumulation the stale-snapshot race lost.
	expire(verdict())
	for i, st := range burst() {
		if st != endpointRetired {
			t.Fatalf("caller %d saw %v on the second strike, want retired", i, st)
		}
	}
	if got := probes.Load(); got != 2 {
		t.Fatalf("second burst brought the probe total to %d, want 2", got)
	}
	if v := verdict(); v.fails != fallbackStrikes || v.state != endpointRetired {
		t.Fatalf("verdict = %+v, want retired with %d consecutive strikes", v, fallbackStrikes)
	}
	fbMu.Lock()
	inflight := len(fbProbing)
	fbMu.Unlock()
	if inflight != 0 {
		t.Fatalf("%d in-flight probe entries left registered after every caller returned", inflight)
	}
}
