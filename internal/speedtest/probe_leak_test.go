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

	"github.com/pingular/pingularity/internal/stats"
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
//
// The ranking and race pings are the same leak on a larger scale: every
// candidate gets one on every run, up to racePingParallel at once, on the
// run's own client - no Timeout of its own, so nothing but the context bounds
// it: cityPingTimeout for a racer, the whole bestOfSelectionBudget for a
// ranking ping - and the library's HTTPPing copies each echo's body whole
// after it has already taken the time. Same peer, measured: 108 MB off ONE
// racer inside cityPingTimeout, with the server left unscored because the
// first copy ate its whole probe set. See pingDrainTransport in ookla.go.
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
	// drainCeiling is the whole allowance: what the caller reads, plus what the
	// peer still gets out AFTER the capped read hangs up. Most of that is not
	// socket slack but net/http's own doing: a body closed short of EOF is
	// drained for up to 256 KiB or 50 ms in the hope of keeping the connection
	// (maxPostCloseReadBytes in transport.go's readLoop), probe and echo alike,
	// and the peer's writes still in flight land on top. Deliberately generous,
	// because the invariant is "hundreds of kilobytes, not tens of megabytes"
	// and it must not decay into an assertion about the toolchain's drain
	// constant or someone else's kernel buffer sizes. Just as deliberately an
	// ABSOLUTE number rather than probeDrainCap plus that slack: a ceiling
	// derived from the constant under test moves with it, so raising the cap -
	// the one edit that reopens this leak - could never fail this test at any
	// value. Measured on go1.27, counted once the peer has stopped pushing:
	// about 330 KB out per request on every run of every row - the 4 KiB read,
	// the 256 KiB drain and the one chunk that tips it, 327,680 bytes exactly
	// on the GETs - so this sits 3x above what the cap lets through and 32x
	// below the 32 MiB an unbounded drain took.
	drainCeiling = 1 << 20
)

// startStreamingPeer serves any path with an endless 200 body and reports how
// many bytes it managed to push before the client hung up, and how many of the
// connections it accepted are still open - a cut-off body has to take its
// connection with it, or a client whose pool keeps connections alive holds one
// per echo, with its transport goroutines, for as long as the peer cares to
// keep it.
func startStreamingPeer(t *testing.T) (host string, pushed func() int64, open func() int) {
	t.Helper()
	var sent atomic.Int64
	var conns atomic.Int64
	chunk := make([]byte, streamChunk)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		switch st {
		case http.StateNew:
			conns.Add(1)
		case http.StateClosed:
			conns.Add(-1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), sent.Load, func() int { return int(conns.Load()) }
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

	// pingEchoes is what one PingTestContext costs in requests: HTTPPing's ten
	// samples plus the warm-up echo it sends first (speedtest-go request.go).
	// drainCeiling is per request, so a ping's allowance is that many of them;
	// an unbounded ping's FIRST echo alone takes the peer's whole 32 MiB valve,
	// three times that allowance.
	const pingEchoes = 11
	for _, c := range []struct {
		name     string
		timeout  time.Duration
		requests int
		probe    func(context.Context, *ookla.Server) endpointState
	}{
		{"fallback GET", fallbackProbeTimeout, 1, probeFallback},
		{"endpoint POST", probeEndpointTimeout, 1, probeEndpoint},
		// The two pings ride the run's own client rather than probeClient and
		// return no verdict, so they are mapped onto one: ANSWERED - the sample
		// callback fired, so the server has a real floor to rank on - reads as
		// ok, unanswered as unknown. Each is driven exactly as its caller
		// drives it: the ranking ping through applyRankPing, as
		// rankedServersRaced's probe goroutine does, the race's through racePing.
		{"ranking ping", bestOfSelectionBudget, pingEchoes, func(ctx context.Context, s *ookla.Server) endpointState {
			s.Context, _ = newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
			sampled := false
			var floor time.Duration
			keep := keepFastestPing(&floor)
			err := ooklaPing(ctx, s, func(d time.Duration) { sampled = true; keep(d) })
			if applyRankPing(s, err, sampled, floor) == nil {
				return endpointUnknown
			}
			return endpointOK
		}},
		{"race ping", cityPingTimeout, pingEchoes, func(ctx context.Context, s *ookla.Server) endpointState {
			s.Context = newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
			racePing(ctx, s)
			if s.Latency <= 0 {
				return endpointUnknown
			}
			return endpointOK
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			host, pushed, open := startStreamingPeer(t)
			s := &ookla.Server{ID: "drain", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
			// The probes bound themselves; the ranking ping is bounded only by
			// what its caller hands it, so hand it the same.
			ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
			defer cancel()
			start := time.Now()
			got := c.probe(ctx, s)
			elapsed := time.Since(start)
			// Capping the drain must not move a single verdict: the status line
			// this is judged on is parsed before the body is touched - and a
			// ping's sample is taken when the headers land, before it too.
			if got != endpointOK {
				t.Fatalf("a peer that answers 200 and then streams classified as %v, want ok - the verdict lives in the status line, not the body", got)
			}
			// What the peer got out is counted once it has stopped pushing, not at
			// the return: the transport goes on draining for up to 50 ms after the
			// caller has its answer (see drainCeiling), and a count taken at the
			// return reads one chunk where the peer got five out.
			n := pushed()
			for quiet := time.Now(); time.Since(quiet) < 100*time.Millisecond; {
				time.Sleep(10 * time.Millisecond)
				if m := pushed(); m != n {
					n, quiet = m, time.Now()
				}
			}
			allowed := int64(drainCeiling) * int64(c.requests)
			t.Logf("%s: the peer got %d bytes out over %d requests in %v (allowed %d)", c.name, n, c.requests, elapsed.Round(time.Millisecond), allowed)
			if n > allowed {
				t.Fatalf("the probe took %d bytes off a streaming peer in %v (ceiling %d over %d requests): the drain is unbounded again, so any third-party candidate can spend the caller's downlink for the whole %v timeout - and none of it is billed to the run, because probeClient carries no recorder and the manager's counters see only transfer chunks",
					n, elapsed.Round(time.Millisecond), allowed, c.requests, c.timeout)
			}
			// Cutting the body off is only half of hanging up: the connection it
			// came on must close too, or the peer keeps a socket and the client's
			// transport goroutines for every echo it was cut off from. The
			// probes' throwaway transport closes on its own; the run's client
			// pools connections and relies on the early close to do it.
			deadline := time.Now().Add(5 * time.Second)
			for open() > 0 {
				if time.Now().After(deadline) {
					t.Fatalf("%d of the peer's connections are still open after the probe returned: a body cut off at the cap was not closed with it, so every echo against a streaming peer strands a socket and its transport goroutines", open())
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

// The cap wraps the body of a response that arrived; a round trip that failed
// - nothing listening, a context cancelled before the headers - has no response
// to wrap, and touching one anyway would be a nil dereference on the library's
// own goroutine: caught by the panic containment above the cap, counted as a
// transport panic, and braked for half a second on every one of the eleven
// echoes. A marked ping against a port nobody answers must fail the way it
// always did - an error, no sample, and speed.transport_panic untouched.
func TestPingCapLeavesAFailedEchoAlone(t *testing.T) {
	allowLoopbackProbes(t)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	_ = l.Close() // refused from here on
	s := &ookla.Server{ID: "refused", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
	s.Context, _ = newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})

	before := stats.Lifetime().Counters["speed.transport_panic"]
	sampled := false
	start := time.Now()
	err = ooklaPing(context.Background(), s, func(time.Duration) { sampled = true })
	elapsed := time.Since(start)
	if err == nil || sampled {
		t.Fatalf("ping against a refused port: err=%v sampled=%v, want an error and no sample", err, sampled)
	}
	if got := stats.Lifetime().Counters["speed.transport_panic"] - before; got != 0 {
		t.Fatalf("speed.transport_panic advanced by %d for a ping nothing answered (took %v): a failed round trip is being handled as though it had a body", got, elapsed.Round(time.Millisecond))
	}
	t.Logf("refused ping: %v in %v, no panic counted", err, elapsed.Round(time.Millisecond))
}
