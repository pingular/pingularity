package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/logbuf"
)

// Two read endpoints quietly stopped returning everything: /api/speed thins a
// window to about 1500 points, and /api/logs returns the newest 500 lines. Both
// changes were right - the alternative was a 5 MB chart payload and an 8 MiB log
// poll - but neither response said so, and both look exactly like a complete
// answer. A consumer totalling bytes across /api/speed, or grepping /api/logs for
// an error it can see in the file, gets a confidently wrong result with nothing
// to hint at why.

func getJSON(t *testing.T, s *Server, path string) (*httptest.ResponseRecorder, []any) {
	t.Helper()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d: %s", path, rr.Code, rr.Body.String())
	}
	var arr []any
	_ = json.Unmarshal(rr.Body.Bytes(), &arr)
	return rr, arr
}

// A window small enough to be returned whole must say it was not thinned.
func TestSpeedHistoryReportsAnUnthinnedWindowHonestly(t *testing.T) {
	s := newTestServer(t)
	seedRuns(t, s, 20, time.Minute)

	rr, arr := getJSON(t, s, "/api/speed?mins=100000")
	if got := rr.Header().Get("X-Sampled"); got != "false" {
		t.Errorf("X-Sampled = %q, want false: 20 runs fit the budget whole", got)
	}
	total, _ := strconv.Atoi(rr.Header().Get("X-Total-Count"))
	if total != 20 {
		t.Errorf("X-Total-Count = %d, want 20", total)
	}
	if len(arr) != 20 {
		t.Errorf("returned %d points, want 20", len(arr))
	}
}

// A window that had to be thinned must say so, and must say how much it left out.
func TestSpeedHistoryDisclosesThinning(t *testing.T) {
	s := newTestServer(t)
	const runs = maxSeriesPoints + 500
	seedRuns(t, s, runs, time.Second)

	rr, arr := getJSON(t, s, "/api/speed?mins=100000")
	if got := rr.Header().Get("X-Sampled"); got != "true" {
		t.Errorf("X-Sampled = %q, want true: %d runs were thinned to %d points",
			got, runs, len(arr))
	}
	total, _ := strconv.Atoi(rr.Header().Get("X-Total-Count"))
	if total != runs {
		t.Errorf("X-Total-Count = %d, want %d - the count is what tells a client how much "+
			"of the window it is NOT looking at", total, runs)
	}
	returned, _ := strconv.Atoi(rr.Header().Get("X-Returned-Count"))
	if returned != len(arr) {
		t.Errorf("X-Returned-Count = %d but the body carries %d points", returned, len(arr))
	}
	if len(arr) >= runs {
		t.Fatalf("nothing was thinned (%d points for %d runs); this test is guarding nothing",
			len(arr), runs)
	}
}

// The body stays a bare array: the disclosure must not have been bought by
// breaking every existing consumer.
func TestSpeedHistoryBodyIsStillABareArray(t *testing.T) {
	s := newTestServer(t)
	seedRuns(t, s, 5, time.Minute)
	rr, _ := getJSON(t, s, "/api/speed?mins=100000")
	if b := rr.Body.Bytes(); len(b) == 0 || b[0] != '[' {
		t.Errorf("response no longer starts with '[': %.60s", b)
	}
}

// /api/logs must report the cap it applied and how much it is holding, so 500
// lines back can be told apart from "that is everything".
func TestLogsReportTheCapAndTheBufferSize(t *testing.T) {
	s := newTestServer(t)
	// Wire a ring rather than skipping: a skipped test guards nothing, and the
	// whole point here is what the response says about truncation.
	s.Logs = logbuf.New(1000)
	for i := 0; i < 50; i++ {
		s.Logs.Append("line", "line")
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/logs?limit=10", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var d struct {
		Limit    int                    `json:"limit"`
		Buffered int                    `json:"buffered"`
		Lines    []struct{ Raw string } `json:"lines"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Limit != 10 {
		t.Errorf("limit = %d, want 10 (the cap that was applied)", d.Limit)
	}
	if d.Buffered < 50 {
		t.Errorf("buffered = %d, want at least the 50 lines appended: without it, a short "+
			"response cannot be told from a complete one", d.Buffered)
	}
	if len(d.Lines) != 10 {
		t.Errorf("returned %d lines, want 10", len(d.Lines))
	}
}
