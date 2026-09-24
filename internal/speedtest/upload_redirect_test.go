package speedtest

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// A LISTED server can lag its own move: the catalogue still names the old
// host, so currentEndpoint changes nothing, while the daemon behind that host
// already answers every upload with a redirect to its new one. Nothing probed
// such a server before a run, and a redirect is not a refusal, so a user
// pinned to one lost every upload - two doomed windows per run - until the
// catalogue caught up. The weekly fleet probe finds 0-5 of ~760 servers in
// that state. Now a window answered with nothing but redirects sends the
// endpoint probe after the new home, and the retry goes there.

// movedUpload is the pair: the OLD host, which redirects every upload POST to
// the NEW one the way the real daemons do (absolute, https, new host), and
// the NEW host, which accepts them. Each counts what reached it.
type movedUpload struct {
	oldAddr, newAddr   string
	oldPosts, newPosts atomic.Int64
	newBytes           atomic.Int64
}

func startMovedUpload(t *testing.T, oldTarget func(newAddr string) string) *movedUpload {
	t.Helper()
	m := &movedUpload{}
	serve := func(h http.HandlerFunc) string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h}
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(func() { _ = srv.Close() })
		return ln.Addr().String()
	}
	m.newAddr = serve(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		if r.Method == http.MethodPost {
			m.newPosts.Add(1)
			m.newBytes.Add(n)
		}
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	m.oldAddr = serve(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // keep the connection reusable: loopback port budget
		if r.Method == http.MethodPost {
			m.oldPosts.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Location", oldTarget(m.newAddr))
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	return m
}

// measureListed runs an upload-only measurement against a LISTED server (Host
// set, URL the Host form) through the production client, with the default one
// retry, and reports how many upload windows ran.
func measureListed(t *testing.T, m *movedUpload) (Result, error, int, *ookla.Server) {
	t.Helper()
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	stats.ResetForTest()
	client, rec := newOoklaClientRec(&ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: 2})
	srv, err := client.CustomServer("http://" + m.oldAddr)
	if err != nil {
		t.Fatal(err)
	}
	srv.ID, srv.Host = "moved-"+t.Name(), m.oldAddr
	srv.URL = "http://" + m.oldAddr + "/speedtest/upload.php"
	currentEndpoint(srv) // the list path's rewrite: a no-op for a server whose Host lags
	t.Cleanup(func() { fbMu.Lock(); delete(fbMap, srv.ID); fbMu.Unlock() })
	srv.Context.SetCaptureTime(700 * time.Millisecond)
	oldPing, oldDown, oldUp := ooklaPing, ooklaDownload, ooklaUpload
	ooklaPing = func(ctx context.Context, sv *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		sv.Latency = 10 * time.Millisecond
		return nil
	}
	ooklaDownload = func(ctx context.Context, sv *ookla.Server) error { sv.DLSpeed = 1e7; return nil }
	windows := 0
	ooklaUpload = func(ctx context.Context, sv *ookla.Server) error { windows++; return oldUp(ctx, sv) }
	t.Cleanup(func() { ooklaPing, ooklaDownload, ooklaUpload = oldPing, oldDown, oldUp })
	o := &Ookla{LossFn: func() bool { return false }, upRec: rec}
	o.uc = &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := o.measure(ctx, srv, "up", speedDefaultRetries)
	awaitQuietTransfers(context.Background(), 3*time.Second)
	return res, err, windows, srv
}

func TestAListedServerWhoseUploadMovedIsMeasuredAtItsNewHome(t *testing.T) {
	m := startMovedUpload(t, func(newAddr string) string { return "https://" + newAddr + "/speedtest/upload.php" })
	res, err, windows, srv := measureListed(t, m)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.UploadFailed || res.UploadMbps <= 0 {
		t.Fatalf("upload lost: UploadFailed=%v Up=%.1f", res.UploadFailed, res.UploadMbps)
	}
	if windows != 2 {
		t.Errorf("upload windows = %d, want 2: the doomed one, then the retry at the new home", windows)
	}
	if want := "http://" + m.newAddr + "/speedtest/upload.php"; srv.URL != want {
		t.Errorf("srv.URL = %q, want %q: the probe adopts the host and keeps http and its own path", srv.URL, want)
	}
	if m.oldPosts.Load() < uploadRejectMinRefusals {
		t.Errorf("the old host saw %d POSTs, want at least the %d that make a window all-redirect", m.oldPosts.Load(), uploadRejectMinRefusals)
	}
	if m.newPosts.Load() == 0 || m.newBytes.Load() == 0 {
		t.Errorf("the new home accepted %d POSTs / %d bytes, want the retry's chunks", m.newPosts.Load(), m.newBytes.Load())
	}
	if got := stats.Lifetime().Counters["speed.upload_redirect_resolved"]; got != 1 {
		t.Errorf("speed.upload_redirect_resolved = %d, want 1", got)
	}
}

func TestARedirectWithNowhereNewToGoIsNotRetried(t *testing.T) {
	// The daemon redirects to ITSELF (https form of the same host): the probe
	// adopts nothing new, so the second window would be answered the same way.
	var self *movedUpload
	self = startMovedUpload(t, func(string) string { return "https://" + self.oldAddr + "/speedtest/upload.php" })
	res, err, windows, srv := measureListed(t, self)
	assertNA(t, err, "server redirects the upload POST")
	if windows != 1 {
		t.Errorf("upload windows = %d, want 1: a retry at the same address is doomed", windows)
	}
	if u, perr := url.Parse(srv.URL); perr != nil || u.Host != self.oldAddr || u.Scheme != "http" {
		t.Errorf("srv.URL = %q, want it still http on the old host", srv.URL)
	}
	if res.UploadMbps != 0 {
		t.Errorf("UploadMbps = %.1f from a server that accepted nothing", res.UploadMbps)
	}
	if got := stats.Lifetime().Counters["speed.upload_redirect_resolved"]; got != 0 {
		t.Errorf("speed.upload_redirect_resolved = %d, want 0", got)
	}
	if self.newPosts.Load() != 0 {
		t.Errorf("the unused second listener saw %d POSTs", self.newPosts.Load())
	}
}

// The recorder's verdict that a window was redirects and nothing else: any
// answer that is not a redirect, or too few answers to rule out an abort
// edge, means the retry rule stays what it was.
func TestOnlyRedirectsMeansEveryAnswerWasARedirectAndEnoughOfThem(t *testing.T) {
	cases := []struct {
		name  string
		notes []int // status per POST; 0 = transport error
		want  bool
	}{
		{"four 307s", []int{307, 307, 307, 307}, true},
		{"three 307s is under the floor", []int{307, 307, 307}, false},
		{"redirects of mixed codes", []int{301, 302, 307, 308}, true},
		{"transport errors beside redirects are ignored", []int{0, 0, 307, 307, 307, 307}, true},
		{"one acceptance acquits", []int{307, 307, 307, 307, 200}, false},
		{"one refusal is not a redirect window", []int{307, 307, 307, 500}, false},
		{"a 4xx among redirects is not a redirect window", []int{307, 307, 307, 403}, false},
		{"only transport errors", []int{0, 0, 0, 0, 0}, false},
		{"nothing", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &uploadRecorder{}
			for _, st := range c.notes {
				if st == 0 {
					r.note(0, io.ErrUnexpectedEOF)
				} else {
					r.note(st, nil)
				}
			}
			if got := r.redirectedOnly(); got != c.want {
				t.Errorf("redirectedOnly(%v) = %v, want %v", c.notes, got, c.want)
			}
		})
	}
}

// The doer's redirect policy on its own: a request that began as a POST is
// handed back its 3xx; a GET keeps net/http's ten-hop default.
func TestTheMeasurementClientNeverFollowsARedirectForAnUpload(t *testing.T) {
	post := &http.Request{Method: http.MethodPost}
	get := &http.Request{Method: http.MethodGet}
	if err := keepUploadRedirect(get, []*http.Request{post}); err != http.ErrUseLastResponse {
		t.Errorf("a POST's redirect was followed: %v", err)
	}
	if err := keepUploadRedirect(get, []*http.Request{get}); err != nil {
		t.Errorf("a GET's first redirect was refused: %v", err)
	}
	ten := make([]*http.Request, 10)
	for i := range ten {
		ten[i] = get
	}
	if err := keepUploadRedirect(get, ten); err == nil || !strings.Contains(err.Error(), "10 redirects") {
		t.Errorf("the tenth GET hop was not stopped: %v", err)
	}
}

// The rule the redirect retry sits on top of is untouched: a window the
// server REFUSED (answered and accepted nothing) is not run a second time,
// and no probe is sent after it.
func TestARefusedWindowIsStillNotRetried(t *testing.T) {
	clearProxyEnv(t)
	allowLoopbackProbes(t)
	probes := 0
	oldProbe := probeEndpoint
	probeEndpoint = func(ctx context.Context, s *ookla.Server) endpointState { probes++; return oldProbe(ctx, s) }
	t.Cleanup(func() { probeEndpoint = oldProbe })
	s := &naServer{mode: "500", dir: "up", retries: speedDefaultRetries} // one retry allowed, as in production
	windows := 0
	oldUp := ooklaUpload
	ooklaUpload = func(ctx context.Context, sv *ookla.Server) error { windows++; return oldUp(ctx, sv) }
	t.Cleanup(func() { ooklaUpload = oldUp })
	_, err := runNACaseOpts(t, s, naRunOpts{})
	assertNA(t, err, "server rejects the upload endpoint")
	if windows != 1 {
		t.Errorf("upload windows = %d, want 1: a refused window is not retried", windows)
	}
	if probes != 0 {
		t.Errorf("the endpoint probe ran %d times after a refusal; it is for redirects only", probes)
	}
}
