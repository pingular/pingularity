package speedtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// speedtest-go v1.7.11 takes a latency sample from ANY HTTP answer and feeds
// ANY response body to its download byte counter. So a server that answers 500
// to its whole legacy bundle (about one in seven of the catalogue), or an
// operator's forward proxy answering 502 for a server that is down, was stored
// as a genuine result: a plausible ping, a "download" that was the error page's
// throughput, a failed upload kept as a partial, and a false below-threshold
// alert. The status guard in the measurement client (statusGuardTransport)
// turns such an answer into a failed request, which the library and the run
// already know how to handle.
//
// One server needs more than that: the one that refuses ONLY its latency file
// and serves its download files and its upload. The guard alone fails it at
// the ping and throws away transfers that would have measured, so when
// refusals are what left a run with no latency figure, the run asks for one
// download file - the library's smallest, smallestDownloadFile below - before
// giving up. Served, the run goes on and is stored with its real speeds and
// the ping left blank; refused too, it fails as the guard alone failed it.
//
// These tests drive the production path - Scheduler.RunOnce -> RunReason ->
// pickServers -> measure, with only the server-list fetch stubbed - or
// measure() itself, against loopback servers, the repo's fake forward proxy,
// or canned answers planted beneath the real transport chain through
// ooklaTransportHook. They pin OUTCOMES (what was stored, what the error says,
// which server was measured, whether a transfer was ever started) and never a
// request count that depends on how fast the machine is.

// legacyBundle is a loopback HTTP Legacy Fallback bundle whose answers can be
// scripted per request class. With nothing set it is a healthy server: a paced
// download, an upload that drains, a nine-byte latency.txt.
type legacyBundle struct {
	everything int               // non-zero: answer this status to EVERY request
	pingStatus func(n int64) int // nil = 200; n counts latency.txt requests from 1
	jpgStatus  func(n int64) int // nil = 200; n counts download requests from 1
	onPing     func(n int64)     // called before a latency.txt request is answered
	delay      time.Duration     // server-side think time per request
	page       int               // bytes of error page sent with a refusal

	ping, jpg, post atomic.Int64 // requests seen, by class
	// check counts the jpg requests that asked for smallestDownloadFile: the one
	// request a run makes to find out whether a server that refused its latency
	// requests serves its files. Counted in jpg as well.
	check atomic.Int64
}

// smallestDownloadFile is the file that check asks for. The transfer itself
// only ever asks for random1000x1000.jpg (speedtest-go v1.7.11 request.go), so
// the name tells the two apart.
const smallestDownloadFile = "random350x350.jpg"

// transfers is the download requests that were part of a transfer: every jpg
// request but the check.
func (b *legacyBundle) transfers() int64 { return b.jpg.Load() - b.check.Load() }

func (b *legacyBundle) start(t *testing.T) (addr string) {
	t.Helper()
	chunk := bytes.Repeat([]byte{0xAB}, 64<<10)
	page := strings.Repeat("x", b.page)
	refuse := func(w http.ResponseWriter, status int) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, page)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		status := http.StatusOK
		switch {
		case r.Method == http.MethodPost:
			b.post.Add(1)
		case strings.HasSuffix(r.URL.Path, "latency.txt"):
			n := b.ping.Add(1)
			if b.onPing != nil {
				b.onPing(n)
			}
			if b.pingStatus != nil {
				status = b.pingStatus(n)
			}
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			n := b.jpg.Add(1)
			if strings.HasSuffix(r.URL.Path, smallestDownloadFile) {
				b.check.Add(1)
			}
			if b.jpgStatus != nil {
				status = b.jpgStatus(n)
			}
		default:
			status = http.StatusNotFound
		}
		if b.delay > 0 {
			time.Sleep(b.delay)
		}
		if b.everything != 0 {
			status = b.everything
		}
		switch {
		case status != http.StatusOK:
			refuse(w, status)
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, "size=1")
		case strings.HasSuffix(r.URL.Path, ".jpg"):
			// 4 MiB per request, paced, so loopback does not melt a shared machine.
			for i := 0; i < 64; i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		default:
			_, _ = io.WriteString(w, "test=test")
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: h}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return ln.Addr().String()
}

// listed is one catalogue entry the way FetchServerListContext hands it over.
func listed(client *ookla.Speedtest, id, addr string, distance float64) *ookla.Server {
	return &ookla.Server{ID: id, URL: "http://" + addr + "/speedtest/upload.php", Host: addr,
		Lat: "52.1", Lon: "4.1", Sponsor: "Sponsor " + id, Name: id, Distance: distance, Context: client}
}

// stubGuardCatalogue serves the run a canned server list - the one step of a run
// that cannot be served offline - and shortens the capture window on the run's
// own client, which freshManager then carries onto every attempt.
func stubGuardCatalogue(t *testing.T, window time.Duration, entries func(client *ookla.Speedtest) ookla.Servers) {
	t.Helper()
	old := fetchServerList
	fetchServerList = func(_ context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		client.SetCaptureTime(window)
		return entries(client), nil
	}
	t.Cleanup(func() { fetchServerList = old })
}

// scheduledRun is everything one Scheduler.RunOnce left behind.
type scheduledRun struct {
	sample store.SpeedSample
	err    error
	alerts [][]string
	latest *store.SpeedSample // what /metrics and the dashboard read
	rows   int                // measurement rows in the history
	// The stored selection report of a run that succeeded: who was ranked
	// where, on what ranking ping, and why the winner won.
	servers []store.SpeedServerRow
	history []store.SpeedSample // every stored row, a kept Best-of member included
	log     string              // everything the run logged, Debug and up
}

// lockedLog collects a run's log lines. Locked because a straggling goroutine
// of the run may still be logging when the test reads it.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func runScheduled(t *testing.T, o *Ookla, th settings.Thresholds) scheduledRun {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logged := &lockedLog{}
	lg := slog.New(slog.NewTextHandler(io.MultiWriter(testWriter{t}, logged), &slog.HandlerOptions{Level: slog.LevelDebug}))
	o.Log = lg
	s := NewScheduler(o, st, time.Hour, lg)
	s.ThresholdsFn = func() settings.Thresholds { return th }
	s.BreachStreakFn = func() int { return 1 }
	var out scheduledRun
	s.OnUnhealthy = func(_ store.SpeedSample, failures []string) { out.alerts = append(out.alerts, failures) }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out.sample, out.err = s.RunOnce(ctx, "scheduled")
	out.latest, _ = st.LatestSpeed(context.Background())
	runs, _ := st.SpeedRuns(context.Background(), 50, 0)
	out.rows, out.history, out.log = len(runs), runs, logged.String()
	if out.err == nil {
		out.servers, _ = st.SpeedServers(context.Background(), out.sample.TS)
	}
	return out
}

// assertNothingStored is the heart of the defect: an answer that was an HTTP
// error must not become a history row, the latest result, or an alert.
func assertNothingStored(t *testing.T, r scheduledRun) {
	t.Helper()
	if r.err == nil {
		t.Fatalf("HTTP error answers were stored as a genuine run: server=%q down=%.2f Mbps up=%.2f Mbps ping=%.2f ms",
			r.sample.Server, r.sample.DownMbps, r.sample.UpMbps, r.sample.PingMS)
	}
	if r.latest != nil || r.rows != 0 {
		t.Errorf("latest=%v rows=%d, want nothing stored", r.latest, r.rows)
	}
	if len(r.alerts) != 0 {
		t.Errorf("alerts=%v: a threshold alert fired on a run that measured nothing", r.alerts)
	}
}

// assertPingRefused checks the error a run returns when every echo was
// answered with status: the text, the statuses, and the fail stage.
func assertPingRefused(t *testing.T, err error, status int) {
	t.Helper()
	want := fmt.Sprintf("ping: every latency request was answered with an HTTP error [11 latency requests: 11x HTTP %d]", status)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v\nwant it to say %q", err, want)
	}
	if strings.Contains(err.Error(), "timeout") {
		t.Errorf("error still reads as a timeout, which is the opposite of what happened: %v", err)
	}
}

// assertFileRefusedToo checks the second half of that error: the one download
// file the run asked for before giving up, and what it was answered with.
func assertFileRefusedToo(t *testing.T, err error, status int) {
	t.Helper()
	want := fmt.Sprintf("]; a download file was refused too: HTTP %d", status)
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error = %v\nwant it to end %q", err, want)
	}
}

// assertBlankPing checks a result carries no latency figure of any kind: the
// "not measured" zero, no floor, no jitter.
func assertBlankPing(t *testing.T, what string, pingMS float64, best, jitter *float64) {
	t.Helper()
	if pingMS != 0 || best != nil || jitter != nil {
		t.Errorf("%s: ping=%v ms best=%s jitter=%s, want a blank ping: every latency request was answered with an HTTP error, and the time to an error answer is not a latency",
			what, pingMS, rankPingText(best), rankPingText(jitter))
	}
}

// blankPingWarning is the log line a run writes when it goes on without a
// ping, or "" when the run logged none.
func blankPingWarning(log string) string {
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "run continues without a ping") {
			return line
		}
	}
	return ""
}

// assertWarnedOfABlankPing checks that warning: a WARN, naming the server,
// what its latency requests were answered with, and that a download file was
// served - the only record of why the stored row has no ping.
func assertWarnedOfABlankPing(t *testing.T, log, server, tally string) {
	t.Helper()
	line := blankPingWarning(log)
	if line == "" {
		t.Error("no warning says the run continues without a ping")
		return
	}
	for _, want := range []string{"level=WARN", server, tally, "a download file was served"} {
		if !strings.Contains(line, want) {
			t.Errorf("the warning does not carry %q: %s", want, line)
		}
	}
}

// failStage classifies the way the scheduler does; RunOnce wraps the engine's
// error once ("speedtest: ...").
func failStage(err error) string {
	if inner := errors.Unwrap(err); inner != nil && strings.HasPrefix(err.Error(), "speedtest: ") {
		err = inner
	}
	return speedFailStage(err)
}

func pinnedTo(id string) *Ookla {
	o := NewOokla()
	o.ServerIDFn = func() string { return id }
	o.LossFn = func() bool { return false }
	o.ConnectionsFn = func() int { return 2 }
	return o
}

// A PINNED server answering 500, with a page, to its whole bundle. Before
// the guard every scheduled run stored it (measured with this test: 3.7 Mbps
// down / 6 ms, with a below-threshold alert each time). Now the run fails at the ping, says
// what was answered - by the latency file and by the one download file it
// asked for before giving up - and never starts a transfer.
func TestAPinnedServerAnsweringHTTPErrorsIsNotStoredAsARun(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	swapFallbackMap(t)
	b := &legacyBundle{everything: 500, page: 2048, delay: 5 * time.Millisecond}
	addr := b.start(t)
	stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
		return ookla.Servers{listed(c, "guard-pin", addr, 1)}
	})
	for run := 1; run <= 2; run++ { // it was EVERY run, so more than one
		r := runScheduled(t, pinnedTo("guard-pin"), settings.Thresholds{DownMbps: 50, UpMbps: 10})
		assertNothingStored(t, r)
		assertPingRefused(t, r.err, 500)
		assertFileRefusedToo(t, r.err, 500)
		if got := failStage(r.err); got != "ping" {
			t.Errorf("run %d: fail stage = %q, want ping", run, got)
		}
		if j, p := b.transfers(), b.post.Load(); j != 0 || p != 0 {
			t.Errorf("run %d: transfers were started against a server that refused its ping (download requests=%d, upload POSTs=%d)", run, j, p)
		}
		if c := b.check.Load(); c != int64(run) {
			t.Errorf("run %d: %d download files asked for so far, want exactly one a run", run, c)
		}
		if line := blankPingWarning(r.log); line != "" {
			t.Errorf("run %d failed, yet the log says it continues without a ping: %s", run, line)
		}
		requireQuiet(t)
	}
	if b.ping.Load() == 0 {
		t.Error("the server was never asked - this run proves nothing")
	}
}

// A PINNED server that refuses ONLY its latency file: the download files and
// the upload are served. Before the guard it was stored with a "ping" that was
// the time to an error answer (measured through this path, latency.txt
// answering 404: 274.66 Mbps down / 6.70 ms); with the guard alone it failed
// every run at the ping and its transfers were never measured. Now the run
// asks for one download file, finds it served, and stores the speeds the
// server really delivers with the ping left blank - and says so in a warning.
// With no Max ping set that row is judged on its speeds like any other; with
// one set, a threshold that could not be checked is no verdict, not a pass and
// not an alert.
func TestAServerRefusingOnlyItsLatencyFileIsStoredWithItsSpeedsAndABlankPing(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		th          settings.Thresholds
		wantVerdict bool
	}{
		{"404, no Max ping: judged on its speeds", 404, settings.Thresholds{DownMbps: 1, UpMbps: 1}, true},
		// Max ping 1 ms: the time to an error answer (the bundle takes 5 ms
		// over each) would breach it, were it still taken for a latency.
		{"500, Max ping set: no verdict and no alert", 500, settings.Thresholds{DownMbps: 1, UpMbps: 1, PingMS: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowLoopbackProbes(t)
			requireQuiet(t)
			swapFallbackMap(t)
			b := &legacyBundle{page: 2048, delay: 5 * time.Millisecond, pingStatus: func(int64) int { return tc.status }}
			addr := b.start(t)
			stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
				srv := listed(c, "guard-latency-only", addr, 1)
				// What the catalogue's own echo leaves on a listed server: a
				// figure taken from whatever answered, this error page included.
				srv.Latency, srv.Jitter = 3*time.Millisecond, time.Millisecond
				return ookla.Servers{srv}
			})
			r := runScheduled(t, pinnedTo("guard-latency-only"), tc.th)
			if r.err != nil {
				t.Fatalf("the run failed though the server serves its files and its upload: %v", r.err)
			}
			if r.rows != 1 || r.latest == nil {
				t.Fatalf("rows=%d latest=%v, want the run stored", r.rows, r.latest)
			}
			if r.sample.DownMbps <= 0 || r.sample.UpMbps <= 0 || r.sample.DownBytes == nil || r.sample.UpBytes == nil {
				t.Errorf("stored down=%.2f up=%.2f Mbps (bytes %v/%v), want both directions really measured",
					r.sample.DownMbps, r.sample.UpMbps, r.sample.DownBytes, r.sample.UpBytes)
			}
			assertBlankPing(t, "stored run", r.sample.PingMS, r.sample.PingBestMS, r.sample.JitterMS)
			assertBlankPing(t, "latest result", r.latest.PingMS, r.latest.PingBestMS, r.latest.JitterMS)
			if c := b.check.Load(); c != 1 {
				t.Errorf("%d download files were asked for ahead of the transfer, want exactly one", c)
			}
			if b.transfers() == 0 || b.post.Load() == 0 {
				t.Errorf("download requests=%d upload POSTs=%d, want both transfers run", b.transfers(), b.post.Load())
			}
			assertWarnedOfABlankPing(t, r.log, "guard-latency-only", fmt.Sprintf("11 latency requests: 11x HTTP %d", tc.status))
			if len(r.alerts) != 0 {
				t.Errorf("alerts=%v, want none: both speeds pass, and a ping that was not measured cannot breach", r.alerts)
			}
			switch {
			case tc.wantVerdict && (r.sample.Healthy == nil || !*r.sample.Healthy):
				t.Errorf("healthy=%v, want the run judged on its speeds and passing", r.sample.Healthy)
			case !tc.wantVerdict && r.sample.Healthy != nil:
				t.Errorf("healthy=%v, want no verdict: Max ping is set and the ping was not measured", *r.sample.Healthy)
			}
		})
	}
}

// The same server on an upload-only run. The one download file is still what
// is asked for - it is the cheapest proof the bundle is alive at all - and
// then only the upload runs; the file is not counted as downloaded data.
func TestAnUploadOnlyRunBehindARefusedLatencyFileAsksForOneDownloadFile(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	b := &legacyBundle{page: 256, pingStatus: func(int64) int { return 404 }}
	addr := b.start(t)
	res, err := measureAgainst(t, context.Background(), addr, "up", 0, time.Second)
	if err != nil {
		t.Fatalf("the upload-only run failed though the server serves its files and its upload: %v", err)
	}
	if res.UploadMbps <= 0 || res.UploadBytes <= 0 {
		t.Errorf("upload=%.2f Mbps (%d bytes), want it measured", res.UploadMbps, res.UploadBytes)
	}
	assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
	if c, tr := b.check.Load(), b.transfers(); c != 1 || tr != 0 {
		t.Errorf("download files asked for=%d, download transfer requests=%d, want the one file and no download", c, tr)
	}
	if res.DownloadMbps != 0 || res.DownloadBytes != 0 {
		t.Errorf("download=%.2f Mbps, %d bytes: the file that was only asked for was counted as a download", res.DownloadMbps, res.DownloadBytes)
	}
}

// One download file a run, whatever the retry budget. The file is served, so
// the run goes on; the transfer behind it is answered 503 throughout, which is
// the busy kind and earns its second window - and neither window asks for the
// file again.
func TestARetriedDownloadBehindARefusedPingAsksForItsFileOnce(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	quickRetries(t)
	b := &legacyBundle{page: 256, delay: 2 * time.Millisecond, pingStatus: func(int64) int { return 404 }}
	b.jpgStatus = func(n int64) int {
		if n == 1 { // the file asked for ahead of the transfer
			return 200
		}
		return 503
	}
	addr := b.start(t)
	windows := countDownloadWindows(t)
	_, err := measureAgainst(t, context.Background(), addr, "down", 1, 300*time.Millisecond)
	assertDownloadRefused(t, err, 503)
	if got := windows.Load(); got != 2 {
		t.Fatalf("%d download windows, want 2 - this run proves nothing about retries", got)
	}
	if c := b.check.Load(); c != 1 {
		t.Errorf("%d download files were asked for across two download windows, want exactly one a run", c)
	}
}

// cannedBundle scripts a bundle beneath the real chain: the latency file is
// answered by latency (n counts its requests from 1), the one download file a
// run asks for behind a refused ping by file. Transfers are stubbed by
// measureCanned, so nothing else is asked.
func cannedBundle(t *testing.T, latency func(n int64) int, file func(req *http.Request) (*http.Response, error)) (files *atomic.Int64, fileURL *atomic.Value) {
	t.Helper()
	files, fileURL = &atomic.Int64{}, &atomic.Value{}
	var echoes atomic.Int64
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, ".jpg") {
			files.Add(1)
			fileURL.Store(req.URL.String())
			return file(req)
		}
		return cannedText(req, latency(echoes.Add(1)), "test=test"), nil
	})
	return files, fileURL
}

func everyEcho(status int) func(int64) int { return func(int64) int { return status } }

// What the one download file decides, answer by answer. Served - after a
// redirect too - the run goes on with a blank ping. Anything else fails the run
// with the error it always had and what became of the file after it: refused,
// a redirect that was never followed or never ended, or no answer at all.
func TestOneDownloadFileSettlesAPingThatWasRefused(t *testing.T) {
	const wantURL = "http://203.0.113.7:8080/speedtest/" + smallestDownloadFile
	served := func(req *http.Request) (*http.Response, error) { return cannedText(req, 200, "jpeg"), nil }
	t.Run("served: the run goes on with a blank ping", func(t *testing.T) {
		files, fileURL := cannedBundle(t, everyEcho(500), served)
		res, err := measureCanned(t, context.Background())
		if err != nil {
			t.Fatalf("the run failed though the download file was served: %v", err)
		}
		assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
		if res.DownloadMbps <= 0 || res.UploadMbps <= 0 {
			t.Errorf("down=%.2f up=%.2f Mbps, want the transfers measured", res.DownloadMbps, res.UploadMbps)
		}
		if files.Load() != 1 || fileURL.Load() != wantURL {
			t.Errorf("%d files asked for, the last at %v; want one, at %s - beside the server's URL, where the transfer asks", files.Load(), fileURL.Load(), wantURL)
		}
	})
	t.Run("served behind a redirect", func(t *testing.T) {
		files, _ := cannedBundle(t, everyEcho(500), func(req *http.Request) (*http.Response, error) {
			if strings.HasPrefix(req.URL.Path, "/speedtest/") {
				return canned(req, http.StatusFound, http.NoBody, "/moved/"+smallestDownloadFile), nil
			}
			return served(req)
		})
		res, err := measureCanned(t, context.Background())
		if err != nil {
			t.Fatalf("the run failed though the download file was served one hop on: %v", err)
		}
		assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
		if files.Load() != 2 {
			t.Errorf("%d file requests, want the one request and its one hop", files.Load())
		}
	})
	t.Run("only the warm-up echo was accepted, and the file is served", func(t *testing.T) {
		files, _ := cannedBundle(t, func(n int64) int {
			if n == 1 {
				return 200
			}
			return 429
		}, served)
		res, err := measureCanned(t, context.Background())
		if err != nil {
			t.Fatalf("the run failed though the download file was served: %v", err)
		}
		assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
		if files.Load() != 1 {
			t.Errorf("%d files asked for, want one", files.Load())
		}
	})
	t.Run("the last 2xx is a file", func(t *testing.T) {
		files, _ := cannedBundle(t, everyEcho(500), func(req *http.Request) (*http.Response, error) {
			return cannedText(req, 299, "jpeg"), nil
		})
		res, err := measureCanned(t, context.Background())
		if err != nil {
			t.Fatalf("the run failed though the download file was answered 299: %v", err)
		}
		assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
		if files.Load() != 1 {
			t.Errorf("%d files asked for, want one", files.Load())
		}
	})
	// The guard hands a 3xx on for the doer to follow. One the doer does not
	// follow - a 300 with nowhere to go is the first status past the 2xx range,
	// a 304 the usual one - comes back as the answer, and it is not a file.
	for _, status := range []int{http.StatusMultipleChoices, http.StatusNotModified} {
		t.Run(fmt.Sprintf("a %d nothing followed is not a file", status), func(t *testing.T) {
			files, _ := cannedBundle(t, everyEcho(500), func(req *http.Request) (*http.Response, error) {
				return canned(req, status, http.NoBody, ""), nil
			})
			_, err := measureCanned(t, context.Background())
			assertPingRefused(t, err, 500)
			if want := fmt.Sprintf("]; a download file was not served either: HTTP %d", status); err == nil || !strings.HasSuffix(err.Error(), want) {
				t.Errorf("error = %v\nwant it to end %q", err, want)
			}
			if files.Load() != 1 {
				t.Errorf("%d files asked for, want one", files.Load())
			}
		})
	}
	// A server that answered every hop is not one that "got no answer": the
	// text stays neutral and the client's own error says what happened.
	t.Run("a redirect loop", func(t *testing.T) {
		cannedBundle(t, everyEcho(500), func(req *http.Request) (*http.Response, error) {
			return canned(req, http.StatusFound, http.NoBody, req.URL.Path), nil
		})
		_, err := measureCanned(t, context.Background())
		assertPingRefused(t, err, 500)
		if msg := err.Error(); !strings.Contains(msg, "]; a download file could not be fetched either: ") ||
			!strings.HasSuffix(msg, "stopped after 10 redirects") || strings.Contains(msg, "no answer") {
			t.Errorf("error = %q, want it to say the download file could not be fetched, and why", msg)
		}
	})
	t.Run("no answer", func(t *testing.T) {
		files, _ := cannedBundle(t, everyEcho(500), func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		})
		_, err := measureCanned(t, context.Background())
		assertPingRefused(t, err, 500)
		if failStage(err) != "ping" {
			t.Errorf("not a ping-stage error: %v", err)
		}
		if msg := err.Error(); !strings.Contains(msg, "]; a download file could not be fetched either: ") || !strings.HasSuffix(msg, "connection reset by peer") {
			t.Errorf("error = %q, want it to say the download file could not be fetched, and why", msg)
		}
		if files.Load() != 1 {
			t.Errorf("%d files asked for, want one: the check is not retried", files.Load())
		}
	})
}

// A run cut short while the download file is being asked for is a cancelled
// run. The echoes before it were refused, but the cause is the cancellation
// and the error must say only that.
func TestARunCancelledWhileAskingForTheDownloadFileIsNotReportedAsARefusal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	files, _ := cannedBundle(t, everyEcho(500), func(req *http.Request) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	})
	_, err := measureCanned(t, ctx)
	if files.Load() != 1 {
		t.Fatalf("%d files asked for - this run proves nothing", files.Load())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if msg := err.Error(); strings.Contains(msg, "HTTP 500") || strings.Contains(msg, "answered with an HTTP error") || strings.Contains(msg, "download file") {
		t.Errorf("a run the user cut short was reported as a refusal: %q", msg)
	}
	// Reported the way a run cut short during the ping itself is.
	if msg := err.Error(); !strings.HasPrefix(msg, "ping: ") {
		t.Errorf("error = %q, want the ping-stage form a cancelled ping has", msg)
	}
}

// A Best-of round with such a server among its members. The member is
// measured like the others and kept as a row of the round with a blank ping,
// and a missing ping cannot win anything: on an install's first round, which
// is decided on latency alone, the member that measured one wins although the
// blank one is the pin and was measured first.
func TestABestOfMemberWithARefusedLatencyFileIsMeasuredAndCannotWinOnLatency(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	swapFallbackMap(t)
	defer func(d time.Duration) { bestOfServerSettle = d }(bestOfServerSettle)
	bestOfServerSettle = 10 * time.Millisecond
	blank := &legacyBundle{page: 256, delay: 5 * time.Millisecond, pingStatus: func(int64) int { return 404 }}
	whole := &legacyBundle{delay: 5 * time.Millisecond}
	blankAddr, wholeAddr := blank.start(t), whole.start(t)
	stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
		return ookla.Servers{listed(c, "guard-blank", blankAddr, 1), listed(c, "guard-whole", wholeAddr, 2)}
	})
	oldByID := fetchServerByID
	fetchServerByID = func(_ context.Context, uc *ookla.UserConfig, id string) (*ookla.Server, error) {
		return listed(newOoklaClient(uc), id, blankAddr, 1), nil
	}
	t.Cleanup(func() { fetchServerByID = oldByID })
	o := pinnedTo("guard-blank")
	o.BestOfCountFn = func() int { return 2 }
	o.DiscardLosersFn = func() bool { return false }
	o.PriorDataFn = func() bool { return false } // the first round: decided on ping alone
	r := runScheduled(t, o, settings.Thresholds{})
	if r.err != nil {
		t.Fatalf("round failed: %v", r.err)
	}
	if r.sample.ServerID != "guard-whole" || r.sample.PingMS <= 0 {
		t.Errorf("the round went to %q (ping %.2f ms), want the member whose ping was measured", r.sample.ServerID, r.sample.PingMS)
	}
	var member *store.SpeedSample
	for i := range r.history {
		if r.history[i].ServerID == "guard-blank" {
			member = &r.history[i]
		}
	}
	if member == nil {
		t.Fatalf("the member with the refused latency file has no row: %d rows stored", len(r.history))
	}
	if member.DownMbps <= 0 || member.UpMbps <= 0 {
		t.Errorf("member row down=%.2f up=%.2f Mbps, want it really measured", member.DownMbps, member.UpMbps)
	}
	assertBlankPing(t, "member row", member.PingMS, member.PingBestMS, member.JitterMS)
	if c := blank.check.Load(); c != 1 {
		t.Errorf("%d download files were asked for ahead of the member's transfer, want exactly one", c)
	}
	if c := whole.check.Load(); c != 0 {
		t.Errorf("%d download files were asked of the member whose ping answered, want none", c)
	}
}

// AUTO, first encounter. The refusing server answers at once, so it pings
// "fastest", and fallbackHealth's two-strike rule has not retired it yet.
// Before the guard run 1 measured it. A refused echo is no sample, so it ranks
// unanswered and the FIRST run already measures the healthy server.
func TestAutoMeasuresTheHealthyServerOnFirstMeetingARefusingOne(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	swapFallbackMap(t)
	bad := &legacyBundle{everything: 500, page: 2048}
	good := &legacyBundle{delay: 10 * time.Millisecond}
	badAddr, goodAddr := bad.start(t), good.start(t)
	stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
		return ookla.Servers{listed(c, "guard-bad", badAddr, 1), listed(c, "guard-good", goodAddr, 2)}
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.ConnectionsFn = func() int { return 2 }
	o.DirectionFn = func() string { return "down" }
	r := runScheduled(t, o, settings.Thresholds{})
	if r.err != nil {
		t.Fatalf("run failed: %v", r.err)
	}
	if r.sample.ServerID != "guard-good" {
		t.Errorf("first encounter measured %q, want the healthy server", r.sample.ServerID)
	}
	if r.sample.DownMbps <= 0 || r.sample.PingMS <= 0 {
		t.Errorf("stored down=%.2f Mbps ping=%.2f ms, want a real measurement", r.sample.DownMbps, r.sample.PingMS)
	}
	if j, p := bad.jpg.Load(), bad.post.Load(); j != 0 || p != 0 {
		t.Errorf("the refusing server was sent transfers (download requests=%d, upload POSTs=%d)", j, p)
	}
	if bad.ping.Load() == 0 {
		t.Error("the refusing server was never pinged - this run proves nothing about ranking")
	}
	// Everything above also holds when the RANKING ping is not judged at all:
	// the refusing server then ranks first on its instant 500s, measure()'s
	// own ping fails it as the head, and the run falls through to the healthy
	// server - the same server measured and the same zero transfers, by way of
	// a failed head, a speed.head_failed count and a "fallback" win. (Seen by
	// unmarking ooklaPing's context: the log read "candidate ranked
	// server_id=guard-bad rank=1 ping_ms=0.3", then "auto-select head could
	// not be measured", and every assertion above passed.) So the stored
	// selection report is pinned too: the refusing server ranked UNANSWERED,
	// behind the healthy one, and was never tried.
	report := map[string]store.SpeedServerRow{}
	for _, row := range r.servers {
		report[row.ServerID] = row
	}
	badRow, goodRow := report["guard-bad"], report["guard-good"]
	if len(r.servers) != 2 || badRow.ServerID == "" || goodRow.ServerID == "" {
		t.Fatalf("stored selection report = %+v, want a row for each of the two servers", r.servers)
	}
	if badRow.RankPingMS != nil {
		t.Errorf("the refusing server's ranking ping was stored as %.2f ms: an HTTP error was taken for a latency sample", *badRow.RankPingMS)
	}
	if goodRow.RankPingMS == nil || goodRow.RankOrder != 1 || badRow.RankOrder != 2 {
		t.Errorf("ranked healthy=#%d (ping %s) refusing=#%d, want the healthy server first on an answered ping and the refusing one behind it",
			goodRow.RankOrder, rankPingText(goodRow.RankPingMS), badRow.RankOrder)
	}
	if badRow.Selected || badRow.Err != "" {
		t.Errorf("the refusing server was tried as a target (selected=%v, err=%q), want it never selected", badRow.Selected, badRow.Err)
	}
	if !goodRow.Winner || goodRow.WinReason != winReasonFastestRank {
		t.Errorf("the healthy server won as %q (winner=%v), want %q: it led the ranking, it did not stand in for a failed head",
			goodRow.WinReason, goodRow.Winner, winReasonFastestRank)
	}
}

// rankPingText renders a selection report's ranking ping, nil being a ping
// that went unanswered.
func rankPingText(ms *float64) string {
	if ms == nil {
		return "unanswered"
	}
	return fmt.Sprintf("%.2f ms", *ms)
}

// The ranking ping on its own: a server answering 500 to every echo ranks
// UNANSWERED - no latency in the report, Latency zeroed so the sort puts it
// behind every server that answered - where the library alone handed it ten
// fast samples. The health cache is held at "fine" so the ping's verdict is the
// only thing being read. (The ranking ping is judged through the same context
// mark as its body cap, set in ooklaPing; unmark it, or have the guard read
// only its own tally key, and this is the test that goes red.)
func TestARankingPingAnsweredWithHTTPErrorsRanksUnanswered(t *testing.T) {
	oldHealth := fallbackHealth
	fallbackHealth = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { fallbackHealth = oldHealth })
	asked := plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return cannedText(req, http.StatusInternalServerError, "boom"), nil
	})
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv := listed(client, "guard-rank", "203.0.113.7:8080", 1) // never dialled: the script answers
	srv.Latency = 3 * time.Millisecond                         // the list fetch's one-shot echo
	ranked, pings, _, _ := rankedServersRaced(context.Background(), ookla.Servers{srv}, "", nil, true, nil, 1)
	if asked.n.Load() == 0 {
		t.Fatal("no echo was sent - this run proves nothing")
	}
	ms, contacted := pings[srv.ID]
	if !contacted || ms != nil {
		t.Errorf("ranking ping recorded as %s (contacted=%v), want contacted and unanswered", rankPingText(ms), contacted)
	}
	if len(ranked) != 1 || ranked[0].Latency != 0 {
		t.Errorf("ranked %v with Latency %v, want the server kept and unscored (0) so it sorts behind any that answered",
			fbIDs(ranked), srv.Latency)
	}
}

// measureAgainst runs measure() against a loopback bundle the way a direct
// caller does: the production client chain, a short capture window, and the
// connection count on o.uc, the only channel freshManager preserves.
func measureAgainst(t *testing.T, ctx context.Context, addr, dir string, retries int, window time.Duration) (Result, error) {
	t.Helper()
	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	srv.Context.SetCaptureTime(window)
	o := &Ookla{LossFn: func() bool { return false }, upRec: rec}
	o.uc = &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: 2}
	o.Log = slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { fbMu.Lock(); delete(fbMap, srv.ID); fbMu.Unlock() })
	return o.measure(ctx, srv, dir, retries)
}

// countSamples wraps the real ping so a test can see how many samples the
// library took on the way through measure().
func countSamples(t *testing.T) *int {
	t.Helper()
	n := new(int)
	real := ooklaPing
	ooklaPing = func(ctx context.Context, srv *ookla.Server, cb func(time.Duration)) error {
		return real(ctx, srv, func(d time.Duration) { *n++; cb(d) })
	}
	t.Cleanup(func() { ooklaPing = real })
	return n
}

// stubTransfersOnly leaves the ping real and stands in for both transfers.
func stubTransfersOnly(t *testing.T) {
	t.Helper()
	oldDown, oldUp := ooklaDownload, ooklaUpload
	ooklaDownload = func(_ context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
	ooklaUpload = func(_ context.Context, sv *ookla.Server) error { sv.ULSpeed = 1e7; return nil }
	t.Cleanup(func() { ooklaDownload, ooklaUpload = oldDown, oldUp })
}

// One refused echo among good ones - a busy server saying 503 once - costs
// that one sample and nothing else. Nine samples, not ten: the refused answer
// used to BE a sample.
func TestOneRefusedEchoCostsOneSampleAndTheRunStands(t *testing.T) {
	allowLoopbackProbes(t)
	b := &legacyBundle{pingStatus: func(n int64) int {
		if n == 5 {
			return 503
		}
		return 200
	}}
	addr := b.start(t)
	stubTransfersOnly(t)
	samples := countSamples(t)
	res, err := measureAgainst(t, context.Background(), addr, "both", 0, time.Second)
	if err != nil {
		t.Fatalf("one 503 among ten good echoes failed the run: %v", err)
	}
	if *samples != 9 {
		t.Errorf("%d latency samples, want 9: eleven echoes, less the warm-up, less the one answered 503", *samples)
	}
	if res.PingBestMS == nil || res.PingMS <= 0 {
		t.Errorf("PingBestMS=%v PingMS=%v, want the ping the other echoes measured", res.PingBestMS, res.PingMS)
	}
}

// slowly holds each canned answer back a little, so the library's no-backoff
// worker loop makes a few hundred requests in a test window instead of a few
// hundred thousand.
func slowly() { time.Sleep(2 * time.Millisecond) }

// countDownloadWindows counts how many times a download window was run - the
// machine-independent way to see a retry.
func countDownloadWindows(t *testing.T) *atomic.Int64 {
	t.Helper()
	n := &atomic.Int64{}
	real := ooklaDownload
	ooklaDownload = func(ctx context.Context, srv *ookla.Server) error {
		n.Add(1)
		return real(ctx, srv)
	}
	t.Cleanup(func() { ooklaDownload = real })
	return n
}

func quickRetries(t *testing.T) {
	t.Helper()
	old := iperfRetryDelay
	iperfRetryDelay = time.Millisecond
	t.Cleanup(func() { iperfRetryDelay = old })
}

func assertDownloadRefused(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatal("error pages were measured as a download")
	}
	if !errors.Is(err, errMeasurementNA) {
		t.Fatalf("errMeasurementNA must stay in the chain, got %v", err)
	}
	if got := failStage(err); got != "na" {
		t.Errorf("fail stage = %q, want na", got)
	}
	msg := err.Error()
	if i := strings.Index(msg, "download: speedtest measurement unavailable (server returned N/A) ["); i < 0 {
		t.Errorf("the download prefix and the N/A text must come first, with the tally after them: %q", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("x HTTP %d", status)) || !strings.Contains(msg, " download requests: ") {
		t.Errorf("error does not name what the download requests were answered with: %q", msg)
	}
	// The requests a closing window cancels were ended by us, not by the far
	// end, and must not be reported as unanswered.
	if strings.Contains(msg, "got no answer") {
		t.Errorf("requests cancelled by the window's own close were tallied as unanswered: %q", msg)
	}
}

// latency.txt answers, every download file is refused. Through the
// scheduler: nothing stored, no alert, N/A naming the statuses, stage "na",
// one window only, and no upload after it.
func TestARefusedDownloadIsNotStoredAndNamesTheStatuses(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	swapFallbackMap(t)
	quickRetries(t)
	b := &legacyBundle{jpgStatus: func(int64) int { return 404 }, page: 2048, delay: 5 * time.Millisecond}
	addr := b.start(t)
	stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
		return ookla.Servers{listed(c, "guard-nojpg", addr, 1)}
	})
	windows := countDownloadWindows(t)
	o := pinnedTo("guard-nojpg")
	o.RetriesFn = func() int { return 2 }
	r := runScheduled(t, o, settings.Thresholds{DownMbps: 50})
	assertNothingStored(t, r)
	assertDownloadRefused(t, r.err, 404)
	if got := windows.Load(); got != 1 {
		t.Errorf("%d download windows against a server answering 404 to every file, want 1: it will answer the retry the same way", got)
	}
	if b.jpg.Load() == 0 {
		t.Error("no download request reached the server - this run proves nothing")
	}
	if p := b.post.Load(); p != 0 {
		t.Errorf("the upload phase ran after a refused download (%d POSTs)", p)
	}
}

// The retry rule, window by window. A refusal that will not change (404,
// 500) is not run twice; a load symptom (503) is; and each window is judged on
// its OWN answers - a 503 in the first window must not keep a server that then
// answers 404 to everything on the retry list.
func TestARefusedDownloadWindowIsRetriedOnlyForALoadSymptom(t *testing.T) {
	allowLoopbackProbes(t)
	quickRetries(t)
	const retries = 2
	cases := []struct {
		name        string
		status      func(window int64) int
		wantWindows int64
		wantStatus  int
		notInError  string
	}{
		{"500 is not retried", func(int64) int { return 500 }, 1, 500, ""},
		{"503 is retried", func(int64) int { return 503 }, 1 + retries, 503, ""},
		{"each window is judged on its own answers", func(w int64) int {
			if w == 1 {
				return 503
			}
			return 404
		}, 2, 404, "HTTP 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requireQuiet(t)
			stubOoklaPingOK(t)
			windows := countDownloadWindows(t)
			b := &legacyBundle{page: 512, delay: 5 * time.Millisecond}
			b.jpgStatus = func(int64) int { return tc.status(windows.Load()) }
			addr := b.start(t)
			_, err := measureAgainst(t, context.Background(), addr, "down", retries, 500*time.Millisecond)
			assertDownloadRefused(t, err, tc.wantStatus)
			if got := windows.Load(); got != tc.wantWindows {
				t.Errorf("%d download windows, want %d", got, tc.wantWindows)
			}
			if tc.notInError != "" && strings.Contains(err.Error(), tc.notInError) {
				t.Errorf("the error carries an earlier window's answers (%s): %q", tc.notInError, err)
			}
		})
	}
}

// A refused window's error carries the run's spent bytes out like every other
// failed download's. The refused window itself counts nothing - its error
// pages are not payload - so the bytes here are an EARLIER attempt's: a window
// that moved real data and then failed (the stand-in returns the error a
// panicked or broken transfer would), followed by a retry the server refused
// outright. Dropped, a metered link is told the run cost nothing.
func TestARefusedRetryStillReportsWhatTheEarlierWindowMoved(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	quickRetries(t)
	stubOoklaPingOK(t)
	var window, moved atomic.Int64
	real := ooklaDownload
	ooklaDownload = func(ctx context.Context, srv *ookla.Server) error {
		if window.Add(1) > 1 {
			return real(ctx, srv)
		}
		err := real(ctx, srv)
		moved.Store(srv.Context.GetTotalDownload())
		if err == nil {
			err = errors.New("the transfer broke after moving data")
		}
		return err
	}
	t.Cleanup(func() { ooklaDownload = real })
	b := &legacyBundle{page: 512, delay: 5 * time.Millisecond}
	b.jpgStatus = func(int64) int {
		if window.Load() <= 1 {
			return 200
		}
		return 404
	}
	addr := b.start(t)
	res, err := measureAgainst(t, context.Background(), addr, "down", 1, 300*time.Millisecond)
	assertDownloadRefused(t, err, 404)
	if got := window.Load(); got != 2 {
		t.Fatalf("%d download windows, want 2: one that moved data and failed, one refused", got)
	}
	if moved.Load() == 0 {
		t.Fatal("the first window moved nothing - this run proves nothing")
	}
	if res.DownloadBytes < moved.Load() {
		t.Errorf("DownloadBytes = %d on the refused result, want at least the %d the first window moved", res.DownloadBytes, moved.Load())
	}
}

// stubOoklaPingOK stands in for a healthy ping where the ping is not under
// test (a real one costs two seconds of the library's pacing).
func stubOoklaPingOK(t *testing.T) {
	t.Helper()
	old := ooklaPing
	ooklaPing = func(_ context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	t.Cleanup(func() { ooklaPing = old })
}

// cannedTransport answers the measurement client's requests from a script,
// planted BENEATH the real chain (recorder, status guard, panic containment,
// ping cap) through ooklaTransportHook, so nothing leaves the process.
type cannedTransport struct {
	n      atomic.Int64
	answer func(req *http.Request, n int64) (*http.Response, error)
}

func (c *cannedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err // what a real transport does with a dead context
	}
	return c.answer(req, c.n.Add(1))
}

func plantCanned(t *testing.T, answer func(req *http.Request, n int64) (*http.Response, error)) *cannedTransport {
	t.Helper()
	c := &cannedTransport{answer: answer}
	ooklaTransportHook = func(http.RoundTripper) http.RoundTripper { return c }
	t.Cleanup(func() { ooklaTransportHook = nil })
	return c
}

func canned(req *http.Request, status int, body io.ReadCloser, location string) *http.Response {
	h := http.Header{}
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{
		StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: body, ContentLength: -1, Request: req,
	}
}

func cannedText(req *http.Request, status int, body string) *http.Response {
	return canned(req, status, io.NopCloser(strings.NewReader(body)), "")
}

// measureCanned runs measure() on a client whose transport answers from the
// script, with both transfers stubbed: the ping is what is under test.
func measureCanned(t *testing.T, ctx context.Context) (Result, error) {
	t.Helper()
	stubTransfersOnly(t)
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://203.0.113.7:8080") // never dialled: the script answers
	if err != nil {
		t.Fatal(err)
	}
	o := &Ookla{LossFn: func() bool { return false }}
	return o.measure(ctx, srv, "both", 0)
}

// A redirected echo is followed by the doer, and it is the FINAL answer
// that is judged: a 302 to a working bundle is a ping like any other, a 307 to
// a 500 is a refusal - counted once per echo, not once per hop.
func TestARedirectedEchoIsJudgedByItsFinalAnswer(t *testing.T) {
	redirectTo := func(final int) func(*http.Request, int64) (*http.Response, error) {
		return func(req *http.Request, _ int64) (*http.Response, error) {
			if strings.HasPrefix(req.URL.Path, "/speedtest/") {
				return canned(req, http.StatusTemporaryRedirect, http.NoBody, "/moved/latency.txt"), nil
			}
			return cannedText(req, final, "test=test"), nil
		}
	}
	t.Run("to a working bundle", func(t *testing.T) {
		plantCanned(t, redirectTo(200))
		samples := countSamples(t)
		if _, err := measureCanned(t, context.Background()); err != nil {
			t.Fatalf("a redirect to a working latency.txt failed the ping: %v", err)
		}
		if *samples != 10 {
			t.Errorf("%d samples, want all 10", *samples)
		}
	})
	t.Run("to an HTTP error", func(t *testing.T) {
		plantCanned(t, redirectTo(500))
		_, err := measureCanned(t, context.Background())
		assertPingRefused(t, err, 500)
		assertFileRefusedToo(t, err, 500) // redirected like the echoes, and judged the same way
	})
}

// The guard judges marked measurement GETs and nothing else. The by-ID
// resolve is an unmarked GET on the same client, and the library reads its
// body whatever the status: a 500 carrying a server entry must still resolve.
func TestTheGuardLeavesTheServerLookupAlone(t *testing.T) {
	const entry = `<settings><servers><server url="http://203.0.113.7:8080/speedtest/upload.php" ` +
		`lat="52.1" lon="4.1" name="Somewhere" country="NL" sponsor="Someone" id="4242" host=""/></servers></settings>`
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return cannedText(req, http.StatusInternalServerError, entry), nil
	})
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.FetchServerByIDContext(context.Background(), "4242")
	if err != nil {
		t.Fatalf("the lookup's response never reached the library: %v", err)
	}
	if srv.ID != "4242" || srv.Sponsor != "Someone" {
		t.Errorf("resolved %+v, want the entry the response carried", srv)
	}
}

// The city race's ping carries the same mark as every other ping, so a
// host serving instant 500s scores nothing there instead of the fastest time.
func TestTheCityRaceDoesNotScoreAnHTTPErrorAsALatency(t *testing.T) {
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return cannedText(req, http.StatusInternalServerError, "boom"), nil
	})
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://203.0.113.7:8080")
	if err != nil {
		t.Fatal(err)
	}
	srv.Latency = 3 * time.Millisecond // the list fetch's one-shot echo
	racePing(context.Background(), srv)
	if srv.Latency != 0 {
		t.Errorf("a server answering 500 to every echo raced at %v, want unscored (0)", srv.Latency)
	}
}

// When some echoes were refused and the rest never got through, the library's
// error stands and the tally says both. It must not claim every request was
// answered: part of this may well be the user's network. And no download file
// is asked for - that question is only put when refusals are the whole story.
func TestAPingOfRefusalsAndSilenceSaysBoth(t *testing.T) {
	asked := plantCanned(t, func(req *http.Request, n int64) (*http.Response, error) {
		if n <= 3 {
			return cannedText(req, http.StatusInternalServerError, "boom"), nil
		}
		return nil, errors.New("connection reset by peer")
	})
	_, err := measureCanned(t, context.Background())
	if err == nil {
		t.Fatal("a ping with no accepted echo was taken as a measurement")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "ping: ") || failStage(err) != "ping" {
		t.Errorf("not a ping-stage error: %q", msg)
	}
	if !errors.Is(err, ookla.ErrConnectTimeout) {
		t.Errorf("the library's own error must stand when not every echo was answered: %q", msg)
	}
	if want := "[11 latency requests: 3x HTTP 500, 8 got no answer]"; !strings.Contains(msg, want) {
		t.Errorf("error = %q, want the whole tally %q", msg, want)
	}
	if strings.Contains(msg, "every latency request") {
		t.Errorf("error claims every request was answered when eight were not: %q", msg)
	}
	if n := asked.n.Load(); n != 11 || strings.Contains(msg, "download file") {
		t.Errorf("%d requests were made and the error reads %q, want the eleven echoes and nothing more: a ping that partly never got through is not settled by a download file", n, msg)
	}
}

// The same mix behind an accepted warm-up echo: the first echo is answered,
// five of the ten behind it are refused and five never get through. The
// library returns nil with no sample, so this arrives by the warm-up door and
// not as an error - and it is the same ping as the one above. Refusals are not
// the whole story of it, so it fails with the whole tally and no download file
// is asked for, although this server would have served one.
func TestAnUnsampledPingOfRefusalsAndSilenceIsNotSettledByADownloadFile(t *testing.T) {
	asked := plantCanned(t, func(req *http.Request, n int64) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, ".jpg"):
			return cannedText(req, http.StatusOK, "jpeg"), nil
		case n == 1:
			return cannedText(req, http.StatusOK, "test=test"), nil
		case n <= 6:
			return cannedText(req, http.StatusTooManyRequests, "slow down"), nil
		}
		return nil, errors.New("connection reset by peer")
	})
	res, err := measureCanned(t, context.Background())
	if err == nil {
		t.Fatalf("a ping that took no sample, five of its echoes never getting through, was stored (ping %.2f ms, down %.2f Mbps) after %d requests",
			res.PingMS, res.DownloadMbps, asked.n.Load())
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "ping: ") || failStage(err) != "ping" {
		t.Errorf("not a ping-stage error: %q", msg)
	}
	if want := "no latency sample was taken, only the warm-up request was accepted [11 latency requests: 1 accepted, 5x HTTP 429, 5 got no answer]"; !strings.HasSuffix(msg, want) {
		t.Errorf("error = %q, want it to end with the whole tally: %q", msg, want)
	}
	if n := asked.n.Load(); n != 11 || strings.Contains(msg, "download file") {
		t.Errorf("%d requests were made and the error reads %q, want the eleven echoes and nothing more: a ping that partly never got through is not settled by a download file", n, msg)
	}
}

// A ping that simply never got through has no refusal in it, and keeps the
// error it always had, to the letter: no tally, no empty brackets.
func TestAPingThatNeverGotThroughKeepsTheErrorItAlwaysHad(t *testing.T) {
	asked := plantCanned(t, func(*http.Request, int64) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	_, err := measureCanned(t, context.Background())
	if err == nil || err.Error() != "ping: "+ookla.ErrConnectTimeout.Error() {
		t.Errorf("error = %v, want exactly %q", err, "ping: "+ookla.ErrConnectTimeout.Error())
	}
	if n := asked.n.Load(); n != 11 {
		t.Errorf("%d requests were made, want the eleven echoes and no download file: nothing was refused", n)
	}
}

// The download's twin of the test above: a window in which every chunk request
// failed beneath us and NOTHING was refused is the library's N/A with the text
// it always had, to the letter - no tally, no empty brackets in the banner.
func TestADownloadThatNeverGotThroughKeepsTheErrorItAlwaysHad(t *testing.T) {
	requireQuiet(t)
	stubOoklaPingOK(t)
	asked := plantCanned(t, func(*http.Request, int64) (*http.Response, error) {
		slowly()
		return nil, errors.New("connection reset by peer")
	})
	_, err := measureAgainst(t, context.Background(), "203.0.113.7:8080", "down", 0, 300*time.Millisecond)
	if asked.n.Load() == 0 {
		t.Fatal("no download request was made - this run proves nothing")
	}
	if want := "download: " + errMeasurementNA.Error(); err == nil || err.Error() != want {
		t.Errorf("error = %v, want exactly %q", err, want)
	}
}

// The guard sits ABOVE the panic containment (see newOoklaClientRec), so a
// panic beneath it arrives as an error and is tallied as a request that got no
// answer. Beneath the containment the panic would unwind straight past the
// tally: the echo below would vanish from it, and the run would claim that
// "every latency request was answered with an HTTP error" about a ping whose
// last request blew up in the transport. One panic only - each contained panic
// costs the containment's 500 ms brake.
func TestAPanickingEchoIsTalliedAsOneThatGotNoAnswer(t *testing.T) {
	asked := plantCanned(t, func(req *http.Request, n int64) (*http.Response, error) {
		if n == 11 {
			panic("transport exploding on the last echo")
		}
		return cannedText(req, http.StatusInternalServerError, "boom"), nil
	})
	_, err := measureCanned(t, context.Background())
	if err == nil {
		t.Fatal("a ping with no accepted echo was taken as a measurement")
	}
	msg := err.Error()
	if want := "[11 latency requests: 10x HTTP 500, 1 got no answer]"; !strings.Contains(msg, want) {
		t.Errorf("error = %q, want the whole tally %q", msg, want)
	}
	if strings.Contains(msg, "every latency request") || !errors.Is(err, ookla.ErrConnectTimeout) {
		t.Errorf("error claims every request was answered when one panicked beneath the guard: %q", msg)
	}
	if failStage(err) != "ping" {
		t.Errorf("not a ping-stage error: %q", msg)
	}
	if n := asked.n.Load(); n != 11 {
		t.Errorf("%d requests were made, want the eleven echoes and no download file", n)
	}
}

// The library's warm-up edge, now reachable by status: the first echo is
// accepted and the ten behind it are refused (a rate limiter - the library
// sends them unpaced once one has failed). The library returns nil with no
// sample. Refusals left the run with no latency figure, so it is settled the
// way a wholly refused ping is, by one download file: served, the run is
// stored with its real speeds and a blank ping; refused too, it fails at the
// ping and no transfer starts.
func TestARunWhoseOnlyAcceptedEchoWasTheWarmUpIsSettledByADownloadFile(t *testing.T) {
	limited := func(jpgStatus func(int64) int) *legacyBundle {
		return &legacyBundle{page: 64, delay: 5 * time.Millisecond, jpgStatus: jpgStatus, pingStatus: func(n int64) int {
			if n == 1 {
				return 200
			}
			return 429
		}}
	}
	run := func(t *testing.T, b *legacyBundle) scheduledRun {
		allowLoopbackProbes(t)
		requireQuiet(t)
		swapFallbackMap(t)
		addr := b.start(t)
		stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
			srv := listed(c, "guard-limited", addr, 1)
			// What the catalogue's own echo leaves behind: a figure taken from
			// whatever answered. It must not be what the stored row shows.
			srv.Latency, srv.Jitter = 3*time.Millisecond, time.Millisecond
			return ookla.Servers{srv}
		})
		return runScheduled(t, pinnedTo("guard-limited"), settings.Thresholds{})
	}
	t.Run("the file is served: stored with a blank ping", func(t *testing.T) {
		b := limited(nil)
		r := run(t, b)
		if r.err != nil {
			t.Fatalf("the run failed though the server serves its files: %v", r.err)
		}
		if r.rows != 1 || r.sample.DownMbps <= 0 || r.sample.UpMbps <= 0 {
			t.Errorf("rows=%d down=%.2f up=%.2f Mbps, want one row with both speeds measured", r.rows, r.sample.DownMbps, r.sample.UpMbps)
		}
		assertBlankPing(t, "stored run", r.sample.PingMS, r.sample.PingBestMS, r.sample.JitterMS)
		if c := b.check.Load(); c != 1 {
			t.Errorf("%d download files were asked for ahead of the transfer, want exactly one", c)
		}
		assertWarnedOfABlankPing(t, r.log, "guard-limited", "11 latency requests: 1 accepted, 10x HTTP 429")
	})
	t.Run("the file is refused too: fails at the ping", func(t *testing.T) {
		b := limited(func(int64) int { return 429 })
		r := run(t, b)
		assertNothingStored(t, r)
		msg := r.err.Error()
		if got := failStage(r.err); got != "ping" {
			t.Errorf("fail stage = %q, want ping: %q", got, msg)
		}
		if !strings.Contains(msg, "no latency sample was taken") ||
			!strings.Contains(msg, "[11 latency requests: 1 accepted, 10x HTTP 429]") {
			t.Errorf("error = %q, want it to say no sample was taken and name the answers", msg)
		}
		assertFileRefusedToo(t, r.err, 429)
		if j, p := b.transfers(), b.post.Load(); j != 0 || p != 0 {
			t.Errorf("transfers ran behind a ping that took no sample (download requests=%d, upload POSTs=%d)", j, p)
		}
		if c := b.check.Load(); c != 1 {
			t.Errorf("%d download files were asked for, want exactly one", c)
		}
		if line := blankPingWarning(r.log); line != "" {
			t.Errorf("the run failed, yet the log says it continues without a ping: %s", line)
		}
	})
}

// The test above is scoped to refusals. A ping that returns nil with no
// sample and NO refusal in it - the library's old transport-level edge, or a
// stand-in ping that reports nothing - is left exactly as it was, and no
// download file is asked for.
func TestAnUnsampledPingWithNoRefusalIsLeftAsItWas(t *testing.T) {
	stubOoklaTransfers(t)
	asked := plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return cannedText(req, http.StatusOK, "test=test"), nil
	})
	ooklaPing = func(context.Context, *ookla.Server, func(time.Duration)) error { return nil }
	o := &Ookla{LossFn: func() bool { return false }}
	srv := &ookla.Server{ID: "guard-unsampled", Context: ookla.New()}
	res, err := o.measure(context.Background(), srv, "both", 0)
	if err != nil {
		t.Fatalf("an unsampled ping with nothing refused now fails the run: %v", err)
	}
	if res.PingBestMS != nil {
		t.Errorf("PingBestMS = %v, want none", *res.PingBestMS)
	}
	if n := asked.n.Load(); n != 0 {
		t.Errorf("%d requests were made for a ping with nothing refused in it, want none", n)
	}
}

// A proxied install. The pinned server is down and the operator's forward
// proxy answers 502, with a page, for it. Before the guard that was stored at
// the PROXY's round trip (measured with this test: 2.7 Mbps down / 6 ms).
func TestAProxyAnswering502ForADeadServerIsNotStoredAsARun(t *testing.T) {
	clearProxyEnv(t)
	requireQuiet(t)
	swapFallbackMap(t)
	var transfers, checks atomic.Int64
	proxy := startRecordingProxy(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, smallestDownloadFile):
			checks.Add(1)
		case r.Method == http.MethodPost || strings.HasSuffix(r.URL.Path, ".jpg"):
			transfers.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(make([]byte, 1500))
	})
	t.Setenv("HTTP_PROXY", "http://"+proxy.addr)
	const origin = "93.184.216.34:8080" // public literal, unreachable directly: only the proxy answers
	stubGuardCatalogue(t, time.Second, func(c *ookla.Speedtest) ookla.Servers {
		return ookla.Servers{listed(c, "guard-proxied", origin, 1)}
	})
	r := runScheduled(t, pinnedTo("guard-proxied"), settings.Thresholds{DownMbps: 50})
	assertNothingStored(t, r)
	assertPingRefused(t, r.err, 502)
	assertFileRefusedToo(t, r.err, 502)
	if got := failStage(r.err); got != "ping" {
		t.Errorf("fail stage = %q, want ping", got)
	}
	if proxy.hostHits(origin) == 0 {
		t.Error("the proxy was never asked for the server - this run proves nothing")
	}
	if n := transfers.Load(); n != 0 {
		t.Errorf("%d transfer requests were sent through the proxy for a server whose ping it refused", n)
	}
	// The one download file the run asks for before giving up goes the way
	// the transfer would have gone: through the proxy, which answers for the
	// dead origin as it answered the echoes.
	if n := checks.Load(); n != 1 {
		t.Errorf("%d download files were asked for through the proxy, want exactly one", n)
	}
}

// A cancelled run is a cancelled run. The echoes it had time for were
// refused, but the cause is the cancellation and the error must say only that.
func TestACancelledPingIsNotReportedAsARefusal(t *testing.T) {
	allowLoopbackProbes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &legacyBundle{everything: 500, page: 256, onPing: func(n int64) {
		if n == 3 {
			cancel()
		}
	}}
	addr := b.start(t)
	_, err := measureAgainst(t, ctx, addr, "both", 0, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if msg := err.Error(); strings.Contains(msg, "HTTP 500") || strings.Contains(msg, "answered with an HTTP error") {
		t.Errorf("a run the user cut short was reported as a refusal: %q", msg)
	}
	// It is the ping's own error, for the echo that was cut short - the run
	// does not go on to ask for a download file under a context that is over.
	if msg := err.Error(); !strings.HasPrefix(msg, "ping: ") || !strings.Contains(msg, "latency.txt") || b.check.Load() != 0 {
		t.Errorf("error = %q (download files asked for: %d), want the cancelled echo's own error and nothing asked after it", msg, b.check.Load())
	}
}

// The same for the download: an abort in the middle of a refused window
// reports the abort. The refusals it had time for are not what ended it.
func TestAnAbortedDownloadIsNotReportedAsARefusal(t *testing.T) {
	allowLoopbackProbes(t)
	requireQuiet(t)
	stubOoklaPingOK(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := &legacyBundle{page: 256, delay: 2 * time.Millisecond}
	b.jpgStatus = func(n int64) int {
		if n == 8 { // well past the refusal floor, long before the window closes
			cancel()
		}
		return 404
	}
	addr := b.start(t)
	_, err := measureAgainst(t, ctx, addr, "down", 1, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if msg := err.Error(); strings.Contains(msg, "HTTP 404") || errors.Is(err, errMeasurementNA) {
		t.Errorf("an aborted download was reported as a refusal: %q", msg)
	}
}

// And for the warm-up check: a ping that returned nil with no sample on a
// run whose context died with its last echo is a cancelled run, not a "ping:"
// failure to count against the server.
func TestACancelledRunIsNotReportedAsAnUnsampledPing(t *testing.T) {
	requireQuiet(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plantCanned(t, func(req *http.Request, n int64) (*http.Response, error) {
		switch {
		case n == 1:
			return cannedText(req, 200, "test=test"), nil
		case n == 11:
			cancel() // lands with the last echo's answer
		}
		return cannedText(req, http.StatusTooManyRequests, "slow down"), nil
	})
	_, err := measureCanned(t, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if msg := err.Error(); strings.HasPrefix(msg, "ping:") || strings.Contains(msg, "HTTP 429") {
		t.Errorf("a cancelled run was reported as a ping the server refused: %q", msg)
	}
}
