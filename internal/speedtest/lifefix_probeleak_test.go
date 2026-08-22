package speedtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Every endpoint probe builds a throwaway http.Client. With keep-alives on and
// no IdleConnTimeout, each successfully probed server stranded an idle socket
// plus the abandoned transport's read/write-loop goroutines until the REMOTE
// peer closed - and rankedServers plus annotateFallback probe dozens of
// servers per pass, so a long-lived daemon accumulated fds without bound
// against peers that never close. The probe transport disables keep-alives, so
// a probe's socket must die with its response: the server side sees every
// accepted connection reach StateClosed shortly after the probe returns.
func TestProbeLeavesNoIdleConnections(t *testing.T) {
	allowLoopbackProbes(t)

	var mu sync.Mutex
	opened, closed := 0, 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test=test"))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			opened++
		case http.StateClosed:
			closed++
		}
	}
	srv.Start()
	defer srv.Close()

	host := srv.Listener.Addr().String()
	const probes = 8
	for i := 0; i < probes; i++ {
		// probeFallback directly, not fallbackHealth: the cache would collapse
		// the repetition this test exists to exercise.
		s := &ookla.Server{ID: "leak", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
		if got := probeFallback(context.Background(), s); got != endpointOK {
			t.Fatalf("probe %d = %v, want ok", i, got)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		o, c := opened, closed
		mu.Unlock()
		if o >= probes && c == o {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe connections leaked: %d opened, only %d closed - kept-alive sockets outlive the throwaway probe client", o, c)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The sockets are not the only thing a probe can leak. Both probes decide on
// the STATUS LINE (and, for probeEndpoint, the Location header), which arrive
// before the body does - yet both used to copy the whole body to io.Discard
// with no ceiling. Nothing obliges a catalogue entry or a redirect target to
// answer with the 9-byte "test=test" the real bundle serves, so a broken or
// hostile peer simply kept the probe reading at line rate until the 6s/8s
// timeout expired, and the drain buys nothing back: the probe transport sets
// DisableKeepAlives and is dropped after one request, so there is no socket to
// return to a pool. Worse, probeClient carries no recorder, so not one of those
// bytes reaches the run's accounting - on a metered link they show up only as
// an unexplained gap between our "Data used" and the ISP's meter, which is the
// one number this release exists to get right. Measured Aug 2026 against the
// paced peer below: probeFallback pulled 33.6 MB and probeEndpoint 33.6 MB
// before the peer's own valve stopped it - without that valve the probe reads
// for the whole timeout instead; see probeDrainCap in ookla.go for what that
// measured. Verdict "ok" both ways.
const (
	// streamChunk / streamPace: ~16 MB/s, an ordinary home downlink. Paced
	// rather than blasted so the failure mode is measured in wall clock the
	// probe spent reading, not in whatever a loopback burst happens to manage.
	streamChunk = 64 << 10
	streamPace  = 4 * time.Millisecond
	// streamValve stops a regression from flooding the machine running the
	// suite: the point is already made at 32 MiB, and without a valve the real
	// ceiling is the timeout times the link.
	streamValve = 512 * streamChunk
	// drainCeiling is the whole allowance: what the probe reads, plus what the
	// peer may still push AFTER the capped read hangs up - chunks already in
	// flight or in a socket buffer, plus the write or two it takes for the reset
	// to surface. Deliberately generous, because the invariant is "kilobytes, not
	// tens of megabytes" and it must not decay into an assertion about someone
	// else's kernel buffer sizes. Just as deliberately an ABSOLUTE number rather
	// than probeDrainCap plus that slack: a ceiling derived from the constant
	// under test moves with it, so raising the cap - the one edit that reopens
	// this leak - could never fail this test at any value. Measured Aug 2026: the
	// peer gets 65,536 bytes out (one chunk) on 10/10 runs of each probe, so this
	// sits 16x above what the fix lets through and 32x below the 32 MiB an
	// unbounded drain took.
	drainCeiling = 1 << 20
)

// startStreamingPeer serves any path with an endless 200 body and reports how
// many bytes it managed to push before the client hung up.
func startStreamingPeer(t *testing.T) (host string, pushed func() int64) {
	t.Helper()
	var sent atomic.Int64
	chunk := make([]byte, streamChunk)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for sent.Load() < streamValve {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(streamPace):
			}
			n, err := w.Write(chunk)
			sent.Add(int64(n))
			if err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), sent.Load
}

func TestProbeDrainIsBounded(t *testing.T) {
	allowLoopbackProbes(t)

	// The ceiling below is absolute, so the cap's own value has to be pinned here
	// or nothing in the repo holds it: raising probeDrainCap into the megabytes
	// would leave every test in the package green while the probe spends that much
	// of a metered downlink per candidate, which is the whole leak. The bounds
	// are this test's own, not a range ookla.go states: 1 KiB sits comfortably
	// above the 9-byte "test=test" body a well-behaved upload endpoint returns,
	// and 64 KiB is small enough that a hostile peer cannot make the drain
	// matter. Same shape as maxPageLimit's guard in internal/web/logwindow_test.go.
	if probeDrainCap < 1<<10 || probeDrainCap > 64<<10 {
		t.Fatalf("probeDrainCap=%d, outside the 1..64 KiB this test holds it to - above that a probe is back to spending the caller's downlink on a body no verdict reads, below it a real response body is truncated", probeDrainCap)
	}

	for _, c := range []struct {
		name    string
		timeout time.Duration
		probe   func(context.Context, *ookla.Server) endpointState
	}{
		{"fallback GET", fallbackProbeTimeout, probeFallback},
		{"endpoint POST", probeEndpointTimeout, probeEndpoint},
	} {
		t.Run(c.name, func(t *testing.T) {
			host, pushed := startStreamingPeer(t)
			s := &ookla.Server{ID: "drain", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
			start := time.Now()
			got := c.probe(context.Background(), s)
			elapsed := time.Since(start)
			// Capping the drain must not move a single verdict: the status line
			// this is judged on is parsed before the body is touched.
			if got != endpointOK {
				t.Fatalf("a peer that answers 200 and then streams classified as %v, want ok - the verdict lives in the status line, not the body", got)
			}
			if n := pushed(); n > drainCeiling {
				t.Fatalf("the probe took %d bytes off a streaming peer in %v (ceiling %d): the drain is unbounded again, so any third-party candidate can spend the caller's downlink for the whole %v timeout - and none of it is billed to the run, because probeClient carries no recorder",
					n, elapsed.Round(time.Millisecond), int64(drainCeiling), c.timeout)
			}
		})
	}
}
