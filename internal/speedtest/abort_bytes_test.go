package speedtest

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A cancelled speedtest must still report the data it already pushed across the
// link. The bytes are gone off the user's allowance whether or not the
// measurement survived, and the usage row is the only place they can be
// recorded - ErrAborted's own documentation promises exactly that.
//
// The abort lands mid-transfer, which is where the promise was broken: the
// transfer goroutine is abandoned rather than stopped (see runTransfer), and the
// abandoned branch returned without tallying anything. measure then reported
// 0/0, RunReason accumulated 0, and the scheduler's own zero-usage guard dropped
// the row - hundreds of megabytes moved, nothing recorded.
//
// The existing abort coverage (abort_usage_test.go) drives a fake tester that
// hands back a prefilled byte count, so it proves the SCHEDULER's contract and is
// blind to this path. These tests are engine-level: a real library transfer
// against a real HTTP server, cancelled while the bytes are flowing.

// Traffic budget. This package has already shipped an upload test that moved
// gigabytes in CI, so the harness is paced rather than left to run at loopback
// speed: every connection in both directions draws from ONE shared token bucket,
// so no arrangement of workers can exceed abortRateBPS x abortWindow = 16 MiB
// per direction however fast the runner is. What each case actually costs is
// ~3 MiB, logged per run: the abort lands at abortAfterBytes and the abandoned
// transfer adds almost nothing after it, because the library's chunk requests
// inherit the cancelled context and fail instantly (see runTransfer) even though
// its workers spin on until the capture window closes. abortHardCap is the
// belt-and-braces stop in case the pacing itself regresses.
const (
	abortRateBPS    = 4 << 20         // 4 MiB/s shared across every connection
	abortWindow     = 4 * time.Second // capture window, in place of the library's 15s
	abortAfterBytes = 3 << 20         // cancel once the link has really carried this much
	abortHardCap    = 64 << 20        // per-direction ceiling the harness refuses to cross
	abortConns      = 1               // pinned worker count; see runAbortedTransfer
)

// abortServer is an Ookla-shaped endpoint for both directions: it serves the
// library's random<N>x<N>.jpg GET and consumes its upload.php POST, counting the
// bytes it really moved on each. Counting on the SERVER is the point - it is the
// independent witness the engine's own tally is checked against.
type abortServer struct {
	bw    *bandwidth
	stop  chan struct{}
	wrote atomic.Int64 // download bytes handed to the client
	read  atomic.Int64 // upload bytes consumed off the wire
}

// moved reports what the harness carried in the direction under test.
func (s *abortServer) moved(dir string) int64 {
	if dir == "up" {
		return s.read.Load()
	}
	return s.wrote.Load()
}

func (s *abortServer) start(t *testing.T) string {
	t.Helper()
	s.stop = make(chan struct{})
	s.bw = newBandwidth(abortRateBPS, s.stop)
	t.Cleanup(func() { close(s.stop) })

	mux := http.NewServeMux()
	// The library POSTs to the server URL - upload.php, what CustomServer builds
	// - and GETs random<N>x<N>.jpg from the same directory (request.go's
	// downloadRequest takes path.Dir of it and joins the sized name). So the
	// exact pattern is the upload and everything else under it is a download.
	mux.HandleFunc("/speedtest/upload.php", s.serveUpload)
	mux.HandleFunc("/speedtest/", s.serveDownload)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// serveDownload streams until the client goes away. The body is open-ended on
// purpose: the library's DownloadHandler reads one response until EOF or until
// the capture window closes, so a short finite body would turn the transfer into
// a stream of quick requests and the abort would keep landing between them
// instead of inside a live one - which is the case under test.
func (s *abortServer) serveDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/jpeg")
	w.WriteHeader(http.StatusOK)
	flush, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		if r.Context().Err() != nil || s.wrote.Load() >= abortHardCap {
			return
		}
		if err := s.bw.take(r.Context(), len(buf)); err != nil {
			return // client gave up; stop consuming the link
		}
		n, err := w.Write(buf)
		if flush != nil {
			// Count only what has reached the socket, not what is sitting in
			// net/http's response buffer: this counter is the yardstick the
			// engine's tally is measured against, so it must not credit the link
			// with bytes the client could not possibly have received.
			flush.Flush()
		}
		s.wrote.Add(int64(n))
		if err != nil {
			return
		}
	}
}

// serveUpload consumes the whole body and answers 200, the healthy case - so a
// missing tally can never be blamed on a server that rejected the data.
func (s *abortServer) serveUpload(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, 32*1024)
	for {
		if r.Context().Err() != nil || s.read.Load() >= abortHardCap {
			return
		}
		n, err := r.Body.Read(buf)
		if n > 0 {
			if err := s.bw.take(r.Context(), n); err != nil {
				return
			}
			s.read.Add(int64(n))
		}
		if err != nil {
			break // EOF, or the client went away at the abort
		}
	}
	w.WriteHeader(http.StatusOK)
}

// runAbortedTransfer runs one direction against the harness and cancels it
// mid-transfer. It returns what the engine reported and what the server had
// really moved by the time measure() returned.
func runAbortedTransfer(t *testing.T, dir string) (Result, error, int64) {
	t.Helper()
	allowLoopbackProbes(t)
	s := &abortServer{}
	base := s.start(t)
	// Reported once the drain below has let the abandoned transfer finish, so the
	// figure is this case's WHOLE traffic cost, orphan included - the bound the
	// pacing constants above claim, stated rather than assumed.
	t.Cleanup(func() {
		t.Logf("%s harness traffic including the orphan: %.2f MiB (bound %.0f MiB)", dir,
			float64(s.moved(dir))/(1<<20), float64(abortRateBPS)*abortWindow.Seconds()/(1<<20))
	})
	// Registered after the server's own cleanups so it runs BEFORE them (LIFO):
	// the abandoned transfer has to drain while it still has a server to talk to.
	requireQuiet(t)

	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer(base)
	if err != nil {
		t.Fatal(err)
	}
	srv.Context.SetCaptureTime(abortWindow)

	// Ping is not under test and the harness serves no ping endpoint.
	oldPing := ooklaPing
	ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	t.Cleanup(func() { ooklaPing = oldPing })

	o := &Ookla{
		LossFn: func() bool { return false }, // no UDP probe against a fake server
		upRec:  rec,
		Log:    slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelWarn})),
		// Pin the worker count through o.uc, the only channel freshManager
		// preserves (SetNThread on the client above is thrown away by the
		// per-attempt rebuild). Left unpinned the library uses min(NumCPU, 8),
		// and the worker count is exactly what sizes the gap the upload
		// assertion has to tolerate: each worker holds one ~1 MB chunk that the
		// transport has already drained into the socket but the server has not
		// read yet, so an 8-core box would show ~8 MB of that in flight and a
		// 2-core CI runner ~2 MB. One worker makes the gap one chunk on every
		// machine, and the tally under test is per-manager, not per-worker.
		uc: &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: abortConns},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The abort is triggered by BYTES MOVED, not by a timer. A fixed sleep would
	// fire before the transfer even started on a loaded runner (the idle-latency
	// burst runs first) and after the capture window had closed on a slow one -
	// either way the test would stop exercising the abandoned path and start
	// passing for the wrong reason. Waiting for the harness to really carry
	// abortAfterBytes pins the cancellation to the thing being asserted.
	go func() {
		tk := time.NewTicker(10 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if s.moved(dir) >= abortAfterBytes {
					cancel()
					return
				}
			}
		}
	}()

	res, err := o.measure(ctx, srv, dir, 0)
	// Sampled as close to the return as possible: the abandoned transfer can
	// still move bytes after the engine walked away (measured on this harness:
	// none on the download case, whose GETs die with the cancelled context, and
	// ~0.7 MiB on the upload one), and a later read would be checking the
	// engine's tally against traffic it could not have seen.
	moved := s.moved(dir)
	t.Logf("%s aborted after the server really moved %.2f MiB; run recorded down=%d up=%d err=%v",
		dir, float64(moved)/(1<<20), res.DownloadBytes, res.UploadBytes, err)
	return res, err, moved
}

// assertAccounted states the harm rather than the mechanism: bytes that crossed
// the link but never reached the user's data-usage total.
func assertAccounted(t *testing.T, dir string, recorded, moved int64, lo, hi float64) {
	t.Helper()
	if recorded <= 0 {
		t.Fatalf("an aborted %s recorded %d bytes of data usage after the server really moved %.2f MiB: "+
			"the run contributes nothing, the scheduler's zero-usage guard then drops the row entirely, "+
			"and \"aborted runs still count toward your data usage\" is untrue at the likeliest moment to abort",
			dir, recorded, float64(moved)/(1<<20))
	}
	ratio := float64(recorded) / float64(moved)
	if ratio < lo || ratio > hi {
		t.Fatalf("an aborted %s recorded %.2f MiB of data usage but the server really moved %.2f MiB (%.0f%%): "+
			"the figure the user is billed against does not describe the traffic that happened",
			dir, float64(recorded)/(1<<20), float64(moved)/(1<<20), ratio*100)
	}
	t.Logf("%s accounted: %.2f MiB recorded vs %.2f MiB moved by the server (%.0f%%)",
		dir, float64(recorded)/(1<<20), float64(moved)/(1<<20), ratio*100)
}

// TestAnAbortedDownloadStillCountsTheDataItMoved. A download abort is the
// likeliest one - it is the first phase of every run - and it was the total
// loss: the library's own GetTotalDownload was sitting on the manager with the
// exact figure, and the abandoned branch returned without reading it.
func TestAnAbortedDownloadStillCountsTheDataItMoved(t *testing.T) {
	res, err, moved := runAbortedTransfer(t, "down")
	if !errors.Is(err, errTransferAbandoned) {
		t.Fatalf("the run did not take the abandoned path (err=%v) - harness fault, not the defect", err)
	}
	if moved < abortAfterBytes {
		t.Fatalf("the harness moved only %d bytes before the abort, want at least %d - harness fault",
			moved, abortAfterBytes)
	}
	// Download is the exact direction: GetTotalDownload counts bytes the client
	// actually received, and the server's counter is flushed per write, so the
	// only gap is what was in the socket buffer at the instant of the abort.
	assertAccounted(t, "download", res.DownloadBytes, moved, 0.75, 1.05)
	if res.UploadBytes != 0 {
		t.Errorf("a download-only run recorded %d upload bytes", res.UploadBytes)
	}
}

// TestAnAbortedUploadStillCountsTheDataItMoved. Upload has a second hole under
// it: v1.7.11 credits GetTotalUpload only when a POST comes back 2xx, so the
// chunk in flight when the abort lands is confirmed by nothing and the confirmed
// total alone would drop it. That is why the tally is uploadSpent() - confirmed
// plus backlog - and why the yardstick here is what the server really read
// rather than what the library was willing to confirm.
func TestAnAbortedUploadStillCountsTheDataItMoved(t *testing.T) {
	res, err, moved := runAbortedTransfer(t, "up")
	if !errors.Is(err, errTransferAbandoned) {
		t.Fatalf("the run did not take the abandoned path (err=%v) - harness fault, not the defect", err)
	}
	if moved < abortAfterBytes {
		t.Fatalf("the harness moved only %d bytes before the abort, want at least %d - harness fault",
			moved, abortAfterBytes)
	}
	// Upload is allowed to read HIGH, and only a bounded amount high. uploadSpent
	// counts bytes the transport pulled out of the chunk, which on a loopback
	// link runs ahead of what the server has read: with one worker exactly one
	// ~1 MB chunk can be in that state, so the ceiling is one chunk over the 3
	// MiB floor (~1.33x, measured 1.26x) and 1.5 leaves headroom without letting
	// a second chunk's worth of over-count pass. Undercounting - the direction
	// the defect failed in - is what the floor pins.
	assertAccounted(t, "upload", res.UploadBytes, moved, 0.95, 1.5)
	if res.DownloadBytes != 0 {
		t.Errorf("an upload-only run recorded %d download bytes", res.DownloadBytes)
	}
}
