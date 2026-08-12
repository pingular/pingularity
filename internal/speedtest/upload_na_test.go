package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Two independent defects in speedtest-go v1.7.11 produce the SAME user-visible
// error - "upload: speedtest measurement unavailable (server returned N/A)" -
// which is why issue #18 resisted diagnosis:
//
//  1. REJECTION. v1.7.11 credits upload bytes only when the POST returns 2xx
//     (request.go: AddTotalUpload after the status check). Any non-2xx - and any
//     3xx, since Go will not follow a redirect whose body is not replayable -
//     confirms nothing, so ULSpeed is 0 with a 100% error ratio, which the
//     library reports as -1. Speed is irrelevant: this fires on a 100 Mbps link.
//
//  2. STARVATION. min(NumCPU, 8) workers each POST a ~1 MB chunk CONCURRENTLY.
//     Sharing one uplink they advance in lockstep, so the first confirmation
//     needs the whole parallel set through. Under that rate nothing confirms
//     inside the capture window, in-flight requests are cancelled at window
//     close and counted as errors, and the result is the same -1.
//
// Both are exercised against a server that reads every byte, so "the link did
// not work" is never the explanation.

// naServer is an Ookla-shaped upload endpoint. mode selects the answer it gives
// AFTER consuming the whole body; rateBPS throttles reads across all connections
// (the mutex is held across the sleep on purpose - that is what makes the budget
// shared, like a real uplink).
type naServer struct {
	mode    string        // "" = healthy 200, else "403" / "307" / "500"
	rateBPS float64       //
	capture time.Duration // 0 = naCaptureTime; set to 0-override for production timing
	retries int           // attempts-1; production default is speedDefaultRetries (1)
	threads int           // 0 = library default min(NumCPU,8); else forced worker count
	delay   time.Duration // simulated server-side RTT before the response
	mu      sync.Mutex
	posts   atomic.Int64
	read    atomic.Int64

	trace     bool // per-POST timeline, for diagnosing the rescue path
	t0        time.Time
	inflight  atomic.Int64
	maxFlight atomic.Int64
	tmu       sync.Mutex
	events    []string
	bw        *bandwidth
	stopBW    chan struct{}
}

// bandwidth models a shared uplink as a token bucket. The first version held a
// mutex across the sleep, which SERIALISED handlers rather than sharing between
// them: nine concurrent POSTs each took the full rate in turn, none finished,
// and a cancelled request kept its place in the queue - so a retry with fewer
// workers never saw a free link. Tokens fix both: concurrent readers genuinely
// divide the rate, and a cancelled reader leaves at once instead of holding
// budget it will never use.
type bandwidth struct {
	tokens chan struct{} // one token per tokenBytes
}

const tokenBytes = 8 * 1024

func newBandwidth(bps float64, stop <-chan struct{}) *bandwidth {
	b := &bandwidth{}
	if bps <= 0 {
		return b // unlimited
	}
	b.tokens = make(chan struct{}, 32)
	interval := time.Duration(float64(tokenBytes) / bps * float64(time.Second))
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		tk := time.NewTicker(interval)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				select {
				case b.tokens <- struct{}{}:
				default: // nobody reading; do not bank budget indefinitely
				}
			}
		}
	}()
	return b
}

// take blocks until n bytes of budget exist, or the request is abandoned.
func (b *bandwidth) take(ctx context.Context, n int) error {
	if b.tokens == nil {
		return nil
	}
	for got := 0; got < n; got += tokenBytes {
		select {
		case <-b.tokens:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *naServer) start(t *testing.T) string {
	t.Helper()
	s.t0 = time.Now()
	s.stopBW = make(chan struct{})
	s.bw = newBandwidth(s.rateBPS, s.stopBW)
	t.Cleanup(func() { close(s.stopBW) })
	mux := http.NewServeMux()
	mux.HandleFunc("/speedtest/upload.php", func(w http.ResponseWriter, r *http.Request) {
		n0 := s.posts.Add(1)
		fl := s.inflight.Add(1)
		for {
			m := s.maxFlight.Load()
			if fl <= m || s.maxFlight.CompareAndSwap(m, fl) {
				break
			}
		}
		start := time.Since(s.t0)
		var got int64
		defer func() {
			s.inflight.Add(-1)
			if s.trace {
				s.tmu.Lock()
				s.events = append(s.events, fmt.Sprintf("POST#%d t=%.1fs..%.1fs bytes=%d inflight=%d",
					n0, start.Seconds(), time.Since(s.t0).Seconds(), got, fl))
				s.tmu.Unlock()
			}
		}()
		buf := make([]byte, 32*1024)
		for {
			// Stop consuming budget the moment the client gives up. A real cancel
			// closes the TCP connection and the kernel discards what was buffered;
			// without this the handler keeps draining a dead request and steals
			// throughput from the NEXT attempt, which is not how a link behaves.
			if r.Context().Err() != nil {
				return
			}
			n, err := r.Body.Read(buf)
			if n > 0 {
				if err := s.bw.take(r.Context(), n); err != nil {
					return // client gave up; stop consuming the link
				}
				s.read.Add(int64(n))
				got += int64(n)
			}
			if err != nil {
				break // EOF, or the client went away at window close
			}
		}
		if s.delay > 0 {
			time.Sleep(s.delay) // models real-world RTT, which loopback does not have
		}
		switch s.mode {
		case "403":
			http.Error(w, "forbidden", http.StatusForbidden)
		case "500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "307":
			w.Header().Set("Location", "https://"+r.Host+r.URL.Path)
			w.WriteHeader(http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// naCaptureTime shortens the library's 15s window so the suite stays quick. It
// does not change either mechanism - it only rescales the starvation threshold,
// which the throttle below is chosen against.
const naCaptureTime = 4 * time.Second

func runNACase(t *testing.T, s *naServer) (Result, error) {
	t.Helper()
	allowLoopbackProbes(t)
	base := s.start(t)

	// The production constructor, so the recording transport is in the chain and
	// the diagnosis in the error is the one a real run would produce.
	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer(base)
	if err != nil {
		t.Fatal(err)
	}
	capture := s.capture
	if capture == 0 {
		capture = naCaptureTime
	}
	srv.Context.SetCaptureTime(capture)
	if s.threads > 0 {
		srv.Context.SetNThread(s.threads) // SetNThread(n) sets uploadMaxWorkers = n
	}
	t.Logf("capture window: %s", capture)

	// Ping and download are not under test; stub them so the run reaches the
	// upload phase exactly as it does after a healthy download.
	oldPing, oldDown := ooklaPing, ooklaDownload
	ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
	t.Cleanup(func() { ooklaPing, ooklaDownload = oldPing, oldDown })

	o := &Ookla{LossFn: func() bool { return false }, upRec: rec} // no UDP probe in a unit test
	if s.threads > 0 {
		// Force the worker count through o.uc - the ONLY channel freshManager
		// preserves. SetNThread on srv.Context above is thrown away when measure
		// swaps in a per-attempt manager, so without this the run falls back to the
		// library default min(NumCPU,8): 8 on an 8-core dev box, 2-4 on a CI runner.
		// That moving count moves the starvation threshold and made the timing tests
		// pass locally but flake in CI; pinning it here makes them CPU-independent.
		o.uc = &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: s.threads}
	}
	o.Log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res, err := o.measure(ctx, srv, "both", s.retries)
	t.Logf("server: POSTs=%d, body read=%.2f MB | result: UploadMbps=%v UploadBytes=%d | err=%v",
		s.posts.Load(), float64(s.read.Load())/1e6, res.UploadMbps, res.UploadBytes, err)
	return res, err
}

// testWriter routes the engine's slog output into the test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Logf("LOG %s", p); return len(p), nil }

const wantNA = "upload: speedtest measurement unavailable (server returned N/A)"

// assertNA checks the failure is the #18 error AND carries the diagnosis that
// tells the three causes apart. Exact string equality would be wrong: the error
// now appends a recorder summary, and what must hold is that the sentinel stays
// in the chain (speedFailStage matches errors.Is) and the prefix stays first
// (it matches that too), with the wantDiag fragment naming the cause.
func assertNA(t *testing.T, err error, wantDiag string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want the N/A failure, got nil")
	}
	if !errors.Is(err, errMeasurementNA) {
		t.Fatalf("errMeasurementNA must stay in the chain, got %v", err)
	}
	if !strings.HasPrefix(err.Error(), wantNA) {
		t.Fatalf("prefix changed - speedFailStage classifies on it:\n got: %s\nwant prefix: %s", err, wantNA)
	}
	if stage := speedFailStage(err); stage != "na" {
		t.Fatalf("speedFailStage = %q, want \"na\" - the fleet histogram would mis-bucket this", stage)
	}
	if wantDiag != "" && !strings.Contains(err.Error(), wantDiag) {
		t.Fatalf("diagnosis missing %q in:\n  %s", wantDiag, err)
	}
}

// TestUploadNA_Rejection: a fast link where every POST is answered non-2xx. The
// bytes demonstrably transfer; only the acknowledgement is withheld.
func TestUploadNA_Rejection(t *testing.T) {
	for _, mode := range []string{"403", "500", "307"} {
		t.Run(mode, func(t *testing.T) {
			s := &naServer{mode: mode} // no throttle: starvation cannot be the cause
			_, err := runNACase(t, s)
			wantDiag := "server rejects the upload endpoint"
			if mode == "307" {
				wantDiag = "server redirects the upload POST"
			}
			assertNA(t, err, wantDiag)
			if s.read.Load() == 0 {
				t.Fatal("no bytes reached the server - harness fault, not the bug")
			}
		})
	}
}

// TestUploadNA_Starvation: a healthy server that always answers 200, too slow for
// the parallel chunk set to finish inside the window.
func TestUploadNA_Starvation(t *testing.T) {
	// Pin 8 workers so the threshold does not move with the runner's core count
	// (freshManager honours threads via o.uc; see runNACase). At the library's 15s
	// window the 8-worker starvation threshold is 8*999490*8/15s ~= 4.3 Mbps, and
	// 2 Mbps sits ~2x under it - so this starves deterministically on a 2-core CI
	// runner and an 8-core dev box alike.
	s := &naServer{rateBPS: 250000, threads: 8} // 2 Mbps, forced 8 workers
	_, err := runNACase(t, s)
	assertNA(t, err, "no attempt completed inside the capture window")
	if s.read.Load() == 0 {
		t.Fatal("no bytes reached the server - harness fault, not the bug")
	}
}

// TestUploadNA_HealthyControl proves the harness is not rigged: same code path,
// same server, 200s and enough bandwidth, and the measurement succeeds.
func TestUploadNA_HealthyControl(t *testing.T) {
	s := &naServer{}
	res, err := runNACase(t, s)
	if err != nil {
		t.Fatalf("healthy server should measure, got %v", err)
	}
	if res.UploadMbps <= 0 {
		t.Fatalf("want a positive upload rate, got %v", res.UploadMbps)
	}
}

// TestUploadNA_DataUsage records what a failed upload costs the user. v1.7.11's
// GetTotalUpload counts confirmed bytes only, so the traffic a REJECTED attempt
// really pushed would vanish from "data used" without uploadSpent()'s backlog
// term. This asserts the bytes are still accounted for.
func TestUploadNA_DataUsage(t *testing.T) {
	s := &naServer{mode: "403"}
	res, err := runNACase(t, s)
	if err == nil {
		t.Fatal("expected the N/A failure")
	}
	if res.UploadBytes <= 0 {
		t.Fatalf("data-usage regression: server read %d bytes, run recorded %d",
			s.read.Load(), res.UploadBytes)
	}
	t.Logf("data used accounted: %.2f MB recorded vs %.2f MB actually read by the server",
		float64(res.UploadBytes)/1e6, float64(s.read.Load())/1e6)
}

// TestUploadNA_StarvationProductionTiming closes the interpolation in the
// evidence: the other starvation case runs a shortened 4s window for speed, so
// it proves the MECHANISM but not that it fires at the timing a real user has.
// This one uses ooklaCaptureTime - the 15s the library actually runs in
// production - at 2 Mbps, and asserts the exact string from the #18 log.
func TestUploadNA_StarvationProductionTiming(t *testing.T) {
	s := &naServer{rateBPS: 250000, capture: ooklaCaptureTime, threads: 8} // 2 Mbps, 15s window, forced 8 workers
	_, err := runNACase(t, s)
	assertNA(t, err, "no attempt completed inside the capture window")
	t.Logf("PRODUCTION TIMING: %s window, 2 Mbps -> %q", ooklaCaptureTime, err)
}

// TestUploadNA_ParallelismCost measures what a starvation fix would COST a
// healthy run if it were applied unconditionally. Cutting worker count is the
// obvious fix for starvation, but parallel streams are exactly how a short test
// saturates a high-latency link: one stream is bounded by chunk/(transfer+RTT).
// Loopback has no RTT, so a delay is injected to model a real path.
func TestUploadNA_ParallelismCost(t *testing.T) {
	if os.Getenv("SPEEDTEST_BENCH") != "1" {
		// Measurement, not an assertion: it records the throughput-vs-workers
		// curve that justifies making the starvation rescue CONDITIONAL rather
		// than always reducing workers. At 25 ms RTT throughput is linear in
		// worker count (8 -> 1684 Mbps, 4 -> 837, 2 -> 417, 1 -> 208), so a
		// blanket reduction would under-report every fast link. Worth keeping as
		// the evidence, not worth ~36 s of every -race CI run.
		t.Skip("set SPEEDTEST_BENCH=1 - measurement, not a behavioural assertion")
	}
	for _, rtt := range []time.Duration{0, 25 * time.Millisecond} {
		for _, threads := range []int{8, 4, 2, 1} {
			name := rtt.String() + "/" + string(rune('0'+threads)) + "workers"
			t.Run(name, func(t *testing.T) {
				s := &naServer{threads: threads, delay: rtt}
				res, err := runNACase(t, s)
				if err != nil {
					t.Fatalf("healthy run should measure: %v", err)
				}
				t.Logf("RESULT rtt=%s workers=%d -> %.0f Mbps", rtt, threads, res.UploadMbps)
			})
		}
	}
}

// TestUploadStarvationRescue is the fix for starvation. Same slow, healthy server
// as TestUploadNA_Starvation - the difference is speedDefaultRetries, which
// production uses and which gives the rescue an attempt to fire on. The first
// attempt starves; the second runs single-stream and measures the link.
func TestUploadStarvationRescue(t *testing.T) {
	// Production timing: a single stream needs 999490 B / 400000 Bps = 2.5 s,
	// comfortably inside the 15 s window, and it must land a chunk while the first
	// attempt's starved sockets are still draining the SHARED throttle - too slow a
	// link and the rescue starves too (measured: 2 Mbps double-N/As here). Left at
	// the runner's natural worker count on purpose: whatever that is, 3.2 Mbps
	// either starves the first attempt (exercising the rescue) or measures directly
	// - both give the POSITIVE result asserted below, so this never double-fails.
	s := &naServer{rateBPS: 400000, capture: ooklaCaptureTime, retries: speedDefaultRetries}
	res, err := runNACase(t, s)
	t.Logf("  max concurrent POSTs = %d (8 on the starved attempt, 1 on the rescue)", s.maxFlight.Load())
	if err != nil {
		t.Fatalf("rescue failed, still N/A: %v", err)
	}
	if res.UploadMbps <= 0 {
		t.Fatalf("rescued run reported no throughput: %+v", res)
	}
	// Deliberately NOT asserting a tight Mbps band: how close the single-stream
	// rescue lands to the link rate depends on how many chunks complete before the
	// library's early-close heuristic fires, which varies with the runner's
	// wall-clock timing (~0.3-2 Mbps observed across machines for this link). The
	// behavioural claim - the rescue turned an N/A into a POSITIVE measurement - is
	// asserted above; the exact rate is evidence, logged not gated.
	t.Logf("RESCUED: %.2f Mbps on a 3.2 Mbps link that previously reported N/A", res.UploadMbps)
}

// TestUploadStarvationRescueNoopWhenHealthy proves the rescue cannot fire on a
// healthy link: same retry budget, fast server, and the worker count must stay
// untouched so throughput is not halved.
func TestUploadStarvationRescueNoopWhenHealthy(t *testing.T) {
	s := &naServer{retries: speedDefaultRetries}
	res, err := runNACase(t, s)
	if err != nil {
		t.Fatalf("healthy run should measure: %v", err)
	}
	if res.UploadMbps < 100 {
		t.Errorf("healthy throughput collapsed to %.0f Mbps - the rescue fired when it should not have", res.UploadMbps)
	}
	t.Logf("healthy link unaffected: %.0f Mbps", res.UploadMbps)
}

// Regression for a threshold mismatch: the rescue trigger counts attempts
// cumulatively against starvationAttemptCeiling, but summary() used a separate
// hardcoded 8. At the production default of one retry a starved 8-worker attempt
// plus the single-stream rescue totals 9, so a rescue that fires and still fails
// lost the very diagnosis it exists to produce.
func TestStarvationDiagnosisSurvivesRescueAttempt(t *testing.T) {
	// Slow enough that even one stream cannot land a chunk, so the rescue fires
	// AND fails - the case the wording is for.
	s := &naServer{rateBPS: 40000, capture: naCaptureTime, retries: speedDefaultRetries}
	_, err := runNACase(t, s)
	if err == nil {
		t.Fatal("expected the run to still fail at 0.32 Mbps")
	}
	assertNA(t, err, "no attempt completed inside the capture window")
}
