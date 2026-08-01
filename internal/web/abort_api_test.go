package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// abortSpy stands in for a scheduler whose run the user stopped: RunOnce reports
// ErrAborted (the "cancelled before any server produced a result" case) and
// Abort records that the endpoint actually reached the scheduler. abortReturns
// mirrors what the real Scheduler.Abort reports - false when it refused (nothing
// in flight, or the named run already ended), which the handler answers with a
// 409 (see the refusal case below).
type abortSpy struct {
	aborts       atomic.Int32
	abortReturns bool
	runID        uint64        // what /api/status publishes as speedtest_run_id
	abortedID    atomic.Uint64 // the id the handler passed through
}

func (a *abortSpy) RunOnce(ctx context.Context, reason string) (store.SpeedSample, error) {
	return store.SpeedSample{}, speedtest.ErrAborted
}
func (a *abortSpy) Running() bool { return false }
func (a *abortSpy) RunID() uint64 { return a.runID }
func (a *abortSpy) Abort(id uint64) bool {
	a.abortedID.Store(id)
	a.aborts.Add(1)
	return a.abortReturns
}
func (a *abortSpy) CurrentServer() string { return "" }
func (a *abortSpy) NextRun() time.Time    { return time.Time{} }

// speedReq fires a body-less request with an EXPLICIT content-type (none when ct
// is ""), which `do` cannot express - it always sends application/json whenever
// there is a body. These endpoints carry no JSON payload, so that header is the
// entire CSRF guard and its rejection path has to be reachable from a test.
func speedReq(t *testing.T, h http.Handler, method, path, ct string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(""))
	if ct != "" {
		r.Header.Set("Content-Type", ct)
	}
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// serverWithSpeed wires a spy scheduler into a test server and returns the full
// handler chain, so these cases exercise routing and the guards as shipped -
// not just the handler method.
func serverWithSpeed(t *testing.T, spy *abortSpy) http.Handler {
	t.Helper()
	s := newTestServer(t)
	s.speed = spy
	return s.Handler()
}

// A run the user stopped is a clean stop, not a failure: the in-flight POST must
// answer 200 {"aborted":true}. Before this the same ErrAborted fell through to
// the generic error branch and the dashboard showed a red "speedtest failed"
// toast for something the user asked for.
func TestSpeedtestAbortedRunAnswers200NotAnError(t *testing.T) {
	h := serverWithSpeed(t, &abortSpy{})

	w := speedReq(t, h, "POST", "/api/speedtest", "application/json")
	if w.Code != http.StatusOK {
		t.Fatalf("aborted run: got %d, want 200 (not the 502 failure path); body: %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body, err)
	}
	if out["aborted"] != true {
		t.Fatalf(`body = %v, want {"aborted":true} - the UI keys off this flag to clear the run without an error`, out)
	}
}

// The abort endpoint itself: the documented 204, and proof the click actually
// reached the scheduler rather than being swallowed by the chain.
func TestSpeedtestAbortEndpointSignalsTheScheduler(t *testing.T) {
	spy := &abortSpy{abortReturns: true}
	h := serverWithSpeed(t, spy)

	w := speedReq(t, h, "POST", "/api/speedtest/abort", "application/json")
	if w.Code != http.StatusNoContent {
		t.Fatalf("POST /api/speedtest/abort: got %d, want 204; body: %s", w.Code, w.Body)
	}
	if w.Body.Len() != 0 {
		t.Errorf("204 carried a body: %q", w.Body)
	}
	if got := spy.aborts.Load(); got != 1 {
		t.Fatalf("scheduler Abort() called %d times, want 1 - the endpoint never reached the run", got)
	}
}

// A mutating endpoint must not be reachable by navigation, a prefetch, or a
// link, so everything but POST is refused before the scheduler is touched.
func TestSpeedtestAbortRejectsNonPOST(t *testing.T) {
	spy := &abortSpy{abortReturns: true}
	h := serverWithSpeed(t, spy)

	for _, m := range []string{"GET", "PUT", "DELETE"} {
		if w := speedReq(t, h, m, "/api/speedtest/abort", "application/json"); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/speedtest/abort: got %d, want 405", m, w.Code)
		}
	}
	if got := spy.aborts.Load(); got != 0 {
		t.Fatalf("a non-POST reached Abort() %d times", got)
	}
}

// The security-relevant case. requireJSONCT is the whole CSRF guard here: a
// cross-site page can fire a body-less no-cors POST with no preflight, but it
// cannot set application/json. Without the header the request must be refused
// BEFORE the side effect - a hostile page that could reach Abort() would be able
// to kill the user's speedtest on every page load.
func TestSpeedtestAbortRequiresJSONContentType(t *testing.T) {
	spy := &abortSpy{abortReturns: true}
	h := serverWithSpeed(t, spy)

	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		if w := speedReq(t, h, "POST", "/api/speedtest/abort", ct); w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("content-type %q: got %d, want 415", ct, w.Code)
		}
	}
	if got := spy.aborts.Load(); got != 0 {
		t.Fatalf("the CSRF guard let %d Abort() calls through", got)
	}
}

// Speedtests disabled (or not wired): the endpoint reports unavailable rather
// than pretending it stopped something.
func TestSpeedtestAbortWithoutASchedulerIsUnavailable(t *testing.T) {
	h := newTestServer(t).Handler() // speed is nil

	w := speedReq(t, h, "POST", "/api/speedtest/abort", "application/json")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil scheduler: got %d, want 503; body: %s", w.Code, w.Body)
	}
	// Guard order: the content-type check runs before the nil check, so a
	// cross-site probe cannot learn whether speedtests are configured without
	// first producing a content-type it is not allowed to set.
	if w := speedReq(t, h, "POST", "/api/speedtest/abort", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("nil scheduler without a content-type: got %d, want 415 (the CSRF guard runs first)", w.Code)
	}
}

// Scheduler.Abort() returns false when it refused: nothing in flight, or the
// named run has already ended (a stale id from a previous boot looks exactly the
// same). The handler used to discard that bool and answer 204 unconditionally,
// so a refusal was byte-identical to a kill and no caller could tell whether
// anything was actually stopped. A refusal is now an honest 409 with a short
// reason. The dashboard is unaffected: its abort fetch ignores the response
// entirely (try/await/catch{}) and re-syncs from the status poll, so the
// distinct status is free for the UI and load-bearing for everyone else.
func TestSpeedtestAbortRefusalIsNotDressedUpAsAKill(t *testing.T) {
	spy := &abortSpy{abortReturns: false} // nothing in flight, or the run ended
	h := serverWithSpeed(t, spy)

	w := speedReq(t, h, "POST", "/api/speedtest/abort", "application/json")
	if w.Code != http.StatusConflict {
		t.Fatalf("refused abort: got %d, want 409 - a refusal must not be byte-identical to a kill; body: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "run") {
		t.Errorf("409 carries no reason naming the run: %q", w.Body)
	}
	if got := spy.aborts.Load(); got != 1 {
		t.Fatalf("Abort() called %d times, want 1", got)
	}
	// Still idempotent at the scheduler: a second click asks again and is
	// refused the same way, not short-circuited before the scheduler.
	if w := speedReq(t, h, "POST", "/api/speedtest/abort", "application/json"); w.Code != http.StatusConflict {
		t.Fatalf("second refused abort: got %d, want 409", w.Code)
	}
	if got := spy.aborts.Load(); got != 2 {
		t.Fatalf("Abort() called %d times after two clicks, want 2", got)
	}
}

// The two answers must stay distinct end to end: the same wiring, flipped only
// by what the scheduler reports, yields 204 for a signalled run and 409 for a
// refusal - the byte-identical-response defect pinned from the other side.
func TestSpeedtestAbortStatusFollowsTheSchedulersAnswer(t *testing.T) {
	killed := &abortSpy{abortReturns: true}
	if w := speedReq(t, serverWithSpeed(t, killed), "POST", "/api/speedtest/abort?run=7", "application/json"); w.Code != http.StatusNoContent {
		t.Errorf("signalled abort: got %d, want 204; body: %s", w.Code, w.Body)
	}
	refused := &abortSpy{abortReturns: false}
	if w := speedReq(t, serverWithSpeed(t, refused), "POST", "/api/speedtest/abort?run=7", "application/json"); w.Code != http.StatusConflict {
		t.Errorf("refused abort: got %d, want 409; body: %s", w.Code, w.Body)
	}
}

// The stop button can only name a run if the API tells it the run's name and then
// accepts it back. Both halves have to hold or the daemon is once again resolving
// a late click against whoever holds the flag when it lands.
func TestStatusPublishesTheRunIDAndAbortAcceptsItBack(t *testing.T) {
	spy := &abortSpy{abortReturns: true, runID: 4242}
	srv := newTestServer(t)
	srv.speed = spy
	srv.status = func() LiveStatus { // handleStatus degrades to 503 without one
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	h := srv.Handler()

	rr := speedReq(t, h, "GET", "/api/status", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	var st map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got, ok := st["speedtest_run_id"]
	if !ok {
		t.Fatal("/api/status has no speedtest_run_id, so the dashboard has nothing to send back " +
			"and a stop can only ever mean \"whatever is running now\"")
	}
	if n, _ := got.(float64); uint64(n) != spy.runID {
		t.Errorf("speedtest_run_id = %v, want %d", got, spy.runID)
	}

	rr = speedReq(t, h, "POST", "/api/speedtest/abort?run=4242", "application/json")
	if rr.Code != http.StatusNoContent {
		t.Fatalf("abort: HTTP %d: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if id := spy.abortedID.Load(); id != 4242 {
		t.Errorf("the handler passed run id %d to the scheduler, want 4242: the id the operator "+
			"decided against is dropped on the way through, so the daemon is back to cancelling "+
			"whichever run happens to hold the flag when the click arrives", id)
	}

	// A malformed id must be refused rather than silently degrading to "abort
	// whatever is running", which is exactly the behaviour being fixed.
	rr = speedReq(t, h, "POST", "/api/speedtest/abort?run=notanumber", "application/json")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a malformed run id returned HTTP %d, want 400", rr.Code)
	}
}
