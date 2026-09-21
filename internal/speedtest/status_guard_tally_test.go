package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The status guard's own parts: the tally it keeps per context, the download
// retry predicate that reads it, and the edges of what the guard touches that
// only a context marked by hand can reach (status_guard_test.go drives the
// same guard through whole runs). Canned answers are planted beneath the real
// chain with ooklaTransportHook, as there.

func tallyOf(accepted, noAnswer int, refusals map[int]int) *guardedGets {
	g := &guardedGets{}
	for i := 0; i < accepted; i++ {
		g.note(200)
	}
	for i := 0; i < noAnswer; i++ {
		g.note(0)
	}
	for status, n := range refusals {
		for i := 0; i < n; i++ {
			g.note(status)
		}
	}
	return g
}

// The download's retry predicate mirrors the upload's conviction rule
// (refusedByServer): a window that was answered and served nothing is not run
// again - unless what it was told is a load symptom, or the refusals are too
// few to judge. Requests that got no answer neither convict nor acquit.
func TestARefusedDownloadWindowIsRetriedOnlyOnALoadSymptomOrAThinSample(t *testing.T) {
	cases := []struct {
		name string
		g    *guardedGets
		want bool
	}{
		{"nothing refused: retried as it always was", tallyOf(0, 0, nil), true},
		{"nothing refused, nothing answered: retried", tallyOf(0, 9, nil), true},
		{"every request 404", tallyOf(0, 0, map[int]int{404: 300}), false},
		{"every request 500", tallyOf(0, 0, map[int]int{500: 300}), false},
		{"exactly the floor", tallyOf(0, 0, map[int]int{403: uploadRejectMinRefusals}), false},
		{"one under the floor", tallyOf(0, 0, map[int]int{403: uploadRejectMinRefusals - 1}), true},
		{"the floor counts refusals across statuses", tallyOf(0, 0, map[int]int{403: 2, 500: 2}), false},
		{"one chunk was served", tallyOf(1, 0, map[int]int{404: 300}), true},
		{"unanswered requests do not acquit", tallyOf(0, 50, map[int]int{404: 300}), false},
		{"unanswered requests do not convict", tallyOf(0, 50, map[int]int{404: uploadRejectMinRefusals - 1}), true},
	}
	for _, status := range []int{408, 429, 502, 503, 504} {
		cases = append(cases, struct {
			name string
			g    *guardedGets
			want bool
		}{http.StatusText(status) + " among the refusals acquits the window", tallyOf(0, 0, map[int]int{404: 300, status: 1}), true})
	}
	for _, tc := range cases {
		if got := tc.g.retryable(); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v [%s]", tc.name, got, tc.want, tc.g.summary("download"))
		}
	}
}

// "Every latency request was answered with an HTTP error" is a claim about
// ALL of them: one accepted echo, or one that never got through, and the run's
// error must not make it.
func TestOnlyRefusalsMeansEveryRequestWasAnsweredAndNoneAccepted(t *testing.T) {
	cases := []struct {
		name string
		g    *guardedGets
		want bool
	}{
		{"nothing happened", tallyOf(0, 0, nil), false},
		{"every request refused", tallyOf(0, 0, map[int]int{500: 11}), true},
		{"refused with two statuses", tallyOf(0, 0, map[int]int{500: 6, 503: 5}), true},
		{"one was accepted", tallyOf(1, 0, map[int]int{429: 10}), false},
		{"some got no answer", tallyOf(0, 8, map[int]int{500: 3}), false},
		{"nothing got an answer", tallyOf(0, 11, nil), false},
		{"everything accepted", tallyOf(11, 0, nil), false},
	}
	for _, tc := range cases {
		if got := tc.g.onlyRefusals(); got != tc.want {
			t.Errorf("%s: onlyRefusals = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Behind an accepted warm-up echo the question is only whether anything went
// unanswered: the one acceptance is expected there, and a single echo that
// never got through makes it a ping a download file does not settle.
func TestAllAnsweredMeansNoRequestFailedBeneathTheGuard(t *testing.T) {
	cases := []struct {
		name string
		g    *guardedGets
		want bool
	}{
		{"the warm-up accepted, the rest refused", tallyOf(1, 0, map[int]int{429: 10}), true},
		{"every request refused", tallyOf(0, 0, map[int]int{500: 11}), true},
		{"the warm-up accepted, one never got through", tallyOf(1, 1, map[int]int{429: 9}), false},
		{"refusals and silence", tallyOf(1, 5, map[int]int{429: 5}), false},
		{"nothing got an answer", tallyOf(0, 11, nil), false},
	}
	for _, tc := range cases {
		if got := tc.g.allAnswered(); got != tc.want {
			t.Errorf("%s: allAnswered = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The tally reaches the UI banner and the log line, so it reads the same
// whatever order the answers came in, names everything that happened, and is
// empty - leaving the error as it always was - when nothing was refused.
func TestTheTallyNamesEveryOutcomeInAStableOrder(t *testing.T) {
	g := &guardedGets{}
	for _, status := range []int{503, 0, 404, 200, 503, 500, 0, 0} {
		g.note(status)
	}
	if got, want := g.summary("download"), "8 download requests: 1 accepted, 1x HTTP 404, 1x HTTP 500, 2x HTTP 503, 3 got no answer"; got != want {
		t.Errorf("summary = %q\n   want   %q", got, want)
	}
	if got := tallyOf(7, 4, nil).summary("latency"); got != "" {
		t.Errorf("summary with nothing refused = %q, want empty", got)
	}
}

// The upload POSTs are the recorder's, and the library reads their status
// itself. Even under a context marked for the guard, a refused POST must come
// back as the RESPONSE it was: the recorder counts a 500, not a transport
// error, and the guard's tally stays empty.
func TestTheGuardLeavesUploadPostsAlone(t *testing.T) {
	requireQuiet(t)
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		slowly()
		return cannedText(req, http.StatusInternalServerError, "boom"), nil
	})
	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	client.SetNThread(2)
	client.SetCaptureTime(200 * time.Millisecond)
	srv, err := client.CustomServer("http://203.0.113.7:8080")
	if err != nil {
		t.Fatal(err)
	}
	g := &guardedGets{}
	if finished, err := runTransfer(guardGets(context.Background(), g), srv, ooklaUpload); !finished || err != nil {
		t.Fatalf("upload window: finished=%v err=%v", finished, err)
	}
	// Not "no transport errors": the POSTs a closing window cancels are
	// recorded as exactly that, and how many there are is the scheduler's call.
	rec.mu.Lock()
	refused := rec.byStatus[500]
	rec.mu.Unlock()
	if refused == 0 {
		t.Errorf("the recorder saw no POST answered 500 (%s): the refusals did not reach it as responses", rec.summary())
	}
	if len(g.refused) != 0 || g.accepted != 0 || g.noAnswer != 0 {
		t.Errorf("the guard tallied upload POSTs: accepted=%d refused=%v noAnswer=%d", g.accepted, g.refused, g.noAnswer)
	}
}

// countedPages hands out error-page bodies and keeps count of what became of
// them: how many were opened, how many closed, how many bytes were read.
type countedPages struct {
	size                 int64
	opened, closed, read atomic.Int64
}

type countedPage struct {
	io.Reader
	pages *countedPages
}

func (p *countedPages) open() io.ReadCloser {
	p.opened.Add(1)
	return &countedPage{Reader: io.LimitReader(zeroes{}, p.size), pages: p}
}

func (b *countedPage) Read(buf []byte) (int, error) {
	n, err := b.Reader.Read(buf)
	b.pages.read.Add(int64(n))
	return n, err
}

func (b *countedPage) Close() error { b.pages.closed.Add(1); return nil }

type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// A refused download request's error page is read off - bounded, like the
// probes - and closed, so the connection goes back to the pool; none of it is
// payload; and a window of nothing else is the library's N/A.
func TestARefusedDownloadRequestHasItsErrorPageReadOffBoundedAndClosed(t *testing.T) {
	requireQuiet(t)
	pages := &countedPages{size: 16 * probeDrainCap}
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		slowly()
		return canned(req, http.StatusNotFound, pages.open(), ""), nil
	})
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	client.SetNThread(2)
	client.SetCaptureTime(200 * time.Millisecond)
	srv, err := client.CustomServer("http://203.0.113.7:8080")
	if err != nil {
		t.Fatal(err)
	}
	g := &guardedGets{}
	if finished, err := runTransfer(guardGets(context.Background(), g), srv, ooklaDownload); !finished || err != nil {
		t.Fatalf("download window: finished=%v err=%v", finished, err)
	}
	opened, closed, read := pages.opened.Load(), pages.closed.Load(), pages.read.Load()
	if opened == 0 {
		t.Fatal("no download request was made - this run proves nothing")
	}
	if closed != opened {
		t.Errorf("%d error pages opened, %d closed: a refused response was left open", opened, closed)
	}
	if want := opened * probeDrainCap; read != want {
		t.Errorf("%d bytes read off %d error pages, want exactly %d each (%d): drained, and no further than the cap",
			read, opened, probeDrainCap, want)
	}
	if got := srv.Context.GetTotalDownload(); got != 0 {
		t.Errorf("%d bytes of error page were counted as download payload", got)
	}
	if srv.DLSpeed != -1 {
		t.Errorf("DLSpeed = %v, want the library's N/A (-1) for a window of refusals", srv.DLSpeed)
	}
	if !g.onlyRefusals() || !strings.Contains(g.summary("download"), "x HTTP 404") {
		t.Errorf("tally = %q onlyRefusals=%v, want nothing but 404s", g.summary("download"), g.onlyRefusals())
	}
}

// A request its own context ended - the chunks a closing download window
// cancels, an abort - says nothing about the far end, and is not tallied as
// one that "got no answer".
func TestARequestEndedByItsOwnContextIsNotTallied(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		cancel()
		return nil, req.Context().Err()
	})
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://203.0.113.7:8080")
	if err != nil {
		t.Fatal(err)
	}
	g := &guardedGets{}
	if err := ooklaPing(guardGets(ctx, g), srv, nil); err == nil {
		t.Fatal("a cancelled ping returned nil")
	}
	if g.noAnswer != 0 || g.accepted != 0 || len(g.refused) != 0 {
		t.Errorf("tally after a cancelled request: accepted=%d refused=%v noAnswer=%d, want nothing",
			g.accepted, g.refused, g.noAnswer)
	}
}

// pingUntilFirstSample pings a canned server under a tally and hangs up at the
// first sample - a whole ping costs two seconds of the library's pacing - then
// reports how many samples the library took and what the guard tallied.
func pingUntilFirstSample(t *testing.T) (samples int, g *guardedGets) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://203.0.113.7:8080") // never dialled: the script answers
	if err != nil {
		t.Fatal(err)
	}
	g = &guardedGets{}
	_ = ooklaPing(guardGets(ctx, g), srv, func(time.Duration) { samples++; cancel() })
	return samples, g
}

// The edges of the verdict, one status either side of each line it draws.
// 400 is the first status past the redirect range and a refusal like any other
// - let through, it is the original defect for exactly one status. A 3xx the
// doer does NOT follow (a 300 with no Location, up to 399) reaches the library
// as it always did, sampled and untallied: a transport cannot tell it from a
// hop about to be followed, and the probes likewise refuse to condemn on one.
func TestTheStatusVerdictAtTheEdgesOfTheRedirectRange(t *testing.T) {
	answering := func(status int) func(*http.Request, int64) (*http.Response, error) {
		return func(req *http.Request, _ int64) (*http.Response, error) {
			return cannedText(req, status, "test=test"), nil
		}
	}
	for _, status := range []int{299, 300, 399} {
		t.Run(fmt.Sprintf("%d reaches the library", status), func(t *testing.T) {
			plantCanned(t, answering(status))
			samples, g := pingUntilFirstSample(t)
			if samples == 0 {
				t.Errorf("no latency sample was taken from an echo answered %d [%s]", status, g.summary("latency"))
			}
			if got := g.summary("latency"); got != "" {
				t.Errorf("an echo answered %d was tallied as a refusal: %q", status, got)
			}
		})
	}
	t.Run("400 is refused", func(t *testing.T) {
		plantCanned(t, answering(http.StatusBadRequest))
		_, err := measureCanned(t, context.Background())
		assertPingRefused(t, err, http.StatusBadRequest)
	})
}

// silentTransport answers with no response and no error - a RoundTripper
// breaking net/http's contract, which the real transport never does but a
// stand-in can.
type silentTransport struct{}

func (silentTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, nil }

// The guard reads a status off the response on a library worker's stack, above
// the panic containment. An answer with nothing in it is handed on as it came,
// the way the containment beneath treats it, and is not tallied: it says
// nothing about the far end.
func TestAnAnswerWithNoResponseIsHandedOnUntouched(t *testing.T) {
	g := &guardedGets{}
	req, err := http.NewRequestWithContext(guardGets(context.Background(), g),
		http.MethodGet, "http://203.0.113.7:8080/speedtest/random1000x1000.jpg", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := statusGuardTransport{base: silentTransport{}}.RoundTrip(req)
	if resp != nil || err != nil {
		t.Fatalf("RoundTrip = (%v, %v), want the empty answer handed on", resp, err)
	}
	if g.noAnswer != 0 || g.accepted != 0 || len(g.refused) != 0 {
		t.Errorf("tally after an empty answer: accepted=%d refused=%v noAnswer=%d, want nothing",
			g.accepted, g.refused, g.noAnswer)
	}
}

// refusedPing stands in for a ping whose eleven echoes were all answered with
// status, without the library's pacing or a server to answer them: it writes
// the refusals into the tally measure() hung on the context, the way the guard
// does, and fails the way the library does. sample, when set, is reported as a
// latency sample first - which the library never does for a refused echo.
func refusedPing(t *testing.T, status int, sample time.Duration) {
	t.Helper()
	old := ooklaPing
	ooklaPing = func(ctx context.Context, _ *ookla.Server, cb func(time.Duration)) error {
		g, _ := ctx.Value(getGuardKey{}).(*guardedGets)
		if g == nil {
			t.Error("measure() pinged under no tally")
		}
		if sample > 0 {
			cb(sample)
		}
		for i := 0; i < 11; i++ {
			g.note(status)
		}
		return ookla.ErrConnectTimeout
	}
	t.Cleanup(func() { ooklaPing = old })
}

// cannedServer is a server on a client whose requests are answered by
// whatever plantCanned planted, the way measureCanned builds one.
func cannedServer(t *testing.T) *ookla.Server {
	t.Helper()
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://203.0.113.7:8080") // never dialled: the script answers
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// A blank ping is blanked by name. On that path the ping wrote nothing to the
// server object, so its Latency and Jitter are whatever it arrived with: the
// catalogue fetch times its own one-shot echo against any answer at all, an
// error page included, and the ranking and the city race write there too. None
// of it may reach the result - nor may a sample, should a ping ever report one
// beside nothing but refusals.
func TestAFigureLeftOnTheServerDoesNotReachABlankPing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sample time.Duration
	}{
		{"as the library leaves it", 0},
		{"whatever sample the ping reported", 7 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubTransfersOnly(t)
			refusedPing(t, http.StatusNotFound, tc.sample)
			plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
				return cannedText(req, http.StatusOK, "jpeg"), nil
			})
			srv := cannedServer(t)
			srv.Latency, srv.Jitter = 3*time.Millisecond, time.Millisecond
			o := &Ookla{LossFn: func() bool { return false }}
			res, err := o.measure(context.Background(), srv, "both", 0)
			if err != nil {
				t.Fatalf("the run failed though the download file was served: %v", err)
			}
			assertBlankPing(t, "result", res.PingMS, res.PingBestMS, res.JitterMS)
			if res.DownloadMbps <= 0 {
				t.Errorf("down=%.2f Mbps, want the transfer's figure kept", res.DownloadMbps)
			}
		})
	}
}

// Only the status of the download file is read. The body is closed unread -
// the file is not downloaded, and none of it is counted as data - and it IS
// closed, or the connection it came on is never released.
func TestTheDownloadFileIsAskedForAndNotDownloaded(t *testing.T) {
	stubTransfersOnly(t)
	refusedPing(t, http.StatusInternalServerError, 0)
	file := &countedPages{size: 245 << 10}
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return canned(req, http.StatusOK, file.open(), ""), nil
	})
	o := &Ookla{LossFn: func() bool { return false }}
	res, err := o.measure(context.Background(), cannedServer(t), "both", 0)
	if err != nil {
		t.Fatalf("the run failed though the download file was served: %v", err)
	}
	if opened, closed, read := file.opened.Load(), file.closed.Load(), file.read.Load(); opened != 1 || closed != 1 || read != 0 {
		t.Errorf("file bodies opened=%d closed=%d, %d bytes read; want one opened, closed, and not a byte read", opened, closed, read)
	}
	if res.DownloadBytes != 0 {
		t.Errorf("%d bytes counted as downloaded with both transfers stubbed: the file that was only asked for was counted", res.DownloadBytes)
	}
}

// A server that accepts the request for the download file and then says
// nothing costs the check's own few seconds, not the slice the run was given:
// the run fails at the ping, as not served, with its own context still alive.
func TestAStalledDownloadFileIsCutShortByItsOwnDeadline(t *testing.T) {
	old := downloadCheckTimeout
	if old < time.Second || old > 10*time.Second {
		t.Errorf("the check's own deadline is %v, want a few seconds: long enough for a slow server to answer one request, short next to a run's slice", old)
	}
	downloadCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { downloadCheckTimeout = old })
	stubTransfersOnly(t)
	refusedPing(t, http.StatusInternalServerError, 0)
	plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		<-req.Context().Done() // accepted, never answered
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	o := &Ookla{LossFn: func() bool { return false }}
	_, err := o.measure(ctx, cannedServer(t), "both", 0)
	if ctx.Err() != nil {
		t.Fatalf("the stalled request was only ended by the run's own context: %v", err)
	}
	assertPingRefused(t, err, http.StatusInternalServerError)
	if want := "]; a download file got no answer within 50ms either"; !strings.HasSuffix(err.Error(), want) {
		t.Errorf("error = %q\nwant it to end %q", err, want)
	}
	if errors.Is(err, context.DeadlineExceeded) || failStage(err) != "ping" {
		t.Errorf("error = %q (stage %s), want a ping-stage failure that does not read as the run timing out", err, failStage(err))
	}
}

// The download file is asked for through the client a transfer attempt gets,
// so the SSRF dial guard stands in front of it as it does in front of the
// transfer: a server whose URL names an internal address is never sent the
// request, whatever its latency requests appeared to be answered with.
func TestTheDownloadFileIsAskedForBehindTheDialGuard(t *testing.T) {
	stubTransfersOnly(t)
	refusedPing(t, http.StatusInternalServerError, 0)
	b := &legacyBundle{}
	addr := b.start(t) // loopback, and the guard is NOT relaxed here
	client := newOoklaClient(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})
	srv, err := client.CustomServer("http://" + addr)
	if err != nil {
		t.Fatal(err)
	}
	o := &Ookla{LossFn: func() bool { return false }}
	_, err = o.measure(context.Background(), srv, "both", 0)
	assertPingRefused(t, err, http.StatusInternalServerError)
	// Nothing was sent, so the text must not say the server stayed silent.
	if msg := err.Error(); !strings.Contains(msg, "]; a download file could not be fetched either: ") || strings.Contains(msg, "no answer") {
		t.Errorf("error = %q, want the download file reported as one that could not be fetched", err)
	}
	if n := b.jpg.Load(); n != 0 {
		t.Errorf("%d requests reached a loopback server past the dial guard", n)
	}
}

// The request for the download file is built the way the library builds a
// chunk's: beside whatever the server's URL names, so an adopted redirect
// target or a bundle outside /speedtest/ is asked where the transfer will ask.
// A URL nothing can be built from fails the run as not served, and sends
// nothing.
func TestTheDownloadFileIsAskedForBesideTheServersURL(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"http://203.0.113.7:8080/speedtest/upload.php", "http://203.0.113.7:8080/speedtest/" + downloadCheckFile},
		{"http://203.0.113.7:8080/mini/backend/upload.php", "http://203.0.113.7:8080/mini/backend/" + downloadCheckFile},
		{"http://203.0.113.7:8080/speedtest/upload.php/", "http://203.0.113.7:8080/speedtest/upload.php/" + downloadCheckFile},
	} {
		req, err := downloadCheckRequest(context.Background(), &ookla.Server{URL: tc.url})
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if got := req.URL.String(); got != tc.want || req.Method != http.MethodGet {
			t.Errorf("%s: %s %s, want GET %s", tc.url, req.Method, got, tc.want)
		}
	}
	if downloadCheckFile != smallestDownloadFile {
		t.Errorf("the file asked for is %q, want the library's smallest, %q", downloadCheckFile, smallestDownloadFile)
	}
	stubTransfersOnly(t)
	refusedPing(t, http.StatusInternalServerError, 0)
	asked := plantCanned(t, func(req *http.Request, _ int64) (*http.Response, error) {
		return cannedText(req, http.StatusOK, "jpeg"), nil
	})
	srv := cannedServer(t)
	srv.URL = "http://203.0.113.7:8080/%zz/upload.php"
	o := &Ookla{LossFn: func() bool { return false }}
	_, err := o.measure(context.Background(), srv, "both", 0)
	assertPingRefused(t, err, http.StatusInternalServerError)
	if !strings.Contains(err.Error(), "]; a download file could not be asked for: ") {
		t.Errorf("error = %q, want it to say the download file could not be asked for", err)
	}
	if n := asked.n.Load(); n != 0 {
		t.Errorf("%d requests were sent for a URL nothing can be built from", n)
	}
}

// The check's context carries one request, so its tally holds one status at
// most; should it ever hold more, the text it feeds must not depend on map
// order.
func TestTheRefusedStatusOfATallyIsStable(t *testing.T) {
	if got := tallyOf(3, 2, nil).refusedStatus(); got != 0 {
		t.Errorf("refusedStatus with nothing refused = %d, want 0", got)
	}
	if got := tallyOf(0, 0, map[int]int{500: 1}).refusedStatus(); got != 500 {
		t.Errorf("refusedStatus = %d, want 500", got)
	}
	for i := 0; i < 20; i++ {
		if got := tallyOf(1, 0, map[int]int{503: 2, 404: 1, 500: 3}).refusedStatus(); got != 404 {
			t.Fatalf("refusedStatus = %d, want the lowest of several, 404", got)
		}
	}
}
