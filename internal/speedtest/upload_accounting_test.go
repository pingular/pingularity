package speedtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A FAILED upload attempt still pushes bytes across the (metered) link, but
// speedtest-go v1.7.11 records only server-confirmed bytes in GetTotalUpload;
// the pushed-but-unconfirmed bytes land in GetUploadBacklog. Data-usage
// accounting (upBytes) must include both - uploadSpent does. Reading
// GetTotalUpload alone silently records ~0 data for a rejected/aborted upload.
func TestUploadSpentCountsFailedAttemptBacklog(t *testing.T) {
	var read int64
	// Drain the whole body (bytes really transit), then REJECT: a non-2xx means
	// the library never marks the chunk confirmed.
	//
	// BOUNDED on purpose. An unthrottled loopback upload moves whatever the
	// machine can manage - gigabytes inside the window - and this package runs
	// under -race in CI, where that exhausted socket buffers and failed the run.
	// The property under test is that pushed bytes go UNCONFIRMED, which needs
	// bytes to move and be rejected, not bytes to be fast: so each request is
	// read up to a small cap and then rejected immediately. Capping the READ
	// (rather than throttling it) bounds wall-clock too - a rate-limited drain
	// just makes httptest.Close wait longer for the handler to finish.
	const perRequest = 256 << 10
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, perRequest))
		atomic.AddInt64(&read, n)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	srv, err := ookla.New().CustomServer(ts.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	// Bound the WINDOW as well as the per-request read. Loopback fills whatever
	// window it is given, so this is what actually decides how many bytes the
	// test costs CI; a fraction of a second still pushes plenty to be rejected.
	srv.Context.SetCaptureTime(600 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_ = srv.UploadTestContext(ctx) // every chunk is rejected; error is expected

	confirmed := srv.Context.GetTotalUpload()
	backlog := srv.Context.GetUploadBacklog()
	spent := uploadSpent(srv)
	t.Logf("server read=%d  confirmed=%d  backlog=%d  uploadSpent=%d", atomic.LoadInt64(&read), confirmed, backlog, spent)

	// The defect: real bytes were pushed but not confirmed.
	if backlog <= 0 {
		t.Fatalf("expected pushed-but-unconfirmed bytes in backlog, got %d (server read %d)", backlog, atomic.LoadInt64(&read))
	}
	// The fix: usage accounting captures them; GetTotalUpload alone would not.
	if spent <= confirmed {
		t.Errorf("uploadSpent (%d) must exceed confirmed-only (%d) by the backlog (%d) - a failed upload's data is otherwise lost", spent, confirmed, backlog)
	}
}

// The helper test above proves uploadSpent; this proves the measure() CALL SITE
// (ookla.go:1208) actually uses it. It drives measure's upload path against a
// rejecting server through the ooklaUpload seam: real bytes transit but nothing
// is confirmed, so they land in backlog. measure must report those bytes as
// UploadBytes even though the attempt failed. Reverting the call site to
// srv.Context.GetTotalUpload() makes UploadBytes==0 here (test fails) - the
// call-site coverage the helper-only test was missing.
func TestMeasureCountsFailedUploadBytesAtCallSite(t *testing.T) {
	// measure() re-homes srv onto a fresh measurement client, which now carries
	// the SSRF dial guard; this test's rejecting server is on loopback, exactly
	// what the guard refuses in production. Relax it for the loopback fake (the
	// same seam upload_na_test.go uses) so the accounting path can be exercised.
	allowLoopbackProbes(t)
	origPing, origUp := ooklaPing, ooklaUpload
	defer func() { ooklaPing, ooklaUpload = origPing, origUp }()
	ooklaPing = func(context.Context, *ookla.Server, func(time.Duration)) error { return nil } // no ping network

	// Neutralize the idle-latency + load-sampler probes (they default to dialing
	// one.one.one.one) so the test makes no external network call. Set under the
	// resolve mutex - the sampler goroutine reads it concurrently.
	setResolved := func(v string) string {
		lulResolveMu.Lock()
		defer lulResolveMu.Unlock()
		old := lulResolved
		lulResolved = v
		return old
	}
	origResolved := setResolved("127.0.0.1:1") // refused instantly, no external net
	defer setResolved(origResolved)

	var read int64
	// Same bound as the helper test above: read a little, reject, keep CI cheap.
	const perRequest = 256 << 10
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, perRequest))
		atomic.AddInt64(&read, n)
		w.WriteHeader(http.StatusInternalServerError) // never confirmed -> bytes land in backlog
	}))
	defer ts.Close()

	// Push real bytes at the rejecting server for a bounded window, then RETURN an
	// error. runTransfer sees the goroutine finish while measure's ctx is still
	// alive, so it reports finished=true and measure reaches the byte tally with
	// backlog left on srv.Context. Bounding the inner upload (not measure's ctx)
	// keeps the finished=true path race-free.
	ooklaUpload = func(ctx context.Context, srv *ookla.Server) error {
		srv.Context.SetCaptureTime(600 * time.Millisecond)
		upctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_ = srv.UploadTestContext(upctx)
		return errors.New("upload rejected")
	}

	srv, err := ookla.New().CustomServer(ts.URL)
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	o := &Ookla{LossFn: func() bool { return false }}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := o.measure(ctx, srv, "up", 0)
	if err == nil {
		t.Fatal("expected the rejected upload to error")
	}
	t.Logf("server read=%d  confirmed=%d  backlog=%d  UploadBytes=%d",
		atomic.LoadInt64(&read), srv.Context.GetTotalUpload(), srv.Context.GetUploadBacklog(), res.UploadBytes)
	if res.UploadBytes <= 0 {
		t.Fatalf("measure reported UploadBytes=%d for a rejected upload that pushed %d bytes; a failed attempt's data usage was dropped (call site must use uploadSpent, not GetTotalUpload)", res.UploadBytes, atomic.LoadInt64(&read))
	}
}
