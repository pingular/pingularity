package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/store"
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

// seedRound records a Best-of round the way "Discard losers" off leaves one: a
// winner row, and losers kept as rows of their own a second apart beneath it,
// each carrying the winner's timestamp as round_ts.
func seedRound(t *testing.T, s *Server, winner int64, losers int) {
	t.Helper()
	if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{
		TS: winner, DownMbps: 900, UpMbps: 90, PingMS: 9, Server: "win"}); err != nil {
		t.Fatalf("insert winner: %v", err)
	}
	for k := 1; k <= losers; k++ {
		w := winner
		if err := s.store.InsertSpeed(t.Context(), store.SpeedSample{
			TS: winner - int64(k), DownMbps: 300, UpMbps: 30, PingMS: 20, Server: "loser", RoundTS: &w}); err != nil {
			t.Fatalf("insert loser: %v", err)
		}
	}
}

// X-Total-Count counts the ROWS the chart's read covers - a Best-of round's kept
// losers are rows to the thinning like any other - while the client's "N runs
// charted, of M in range" caveat counts runs, so comparing the two described a
// coverage that was never true. X-Total-Runs is the run-only count, sent on a
// thinned answer only (it is one more query on a polled path). The page reads
// it; nothing on the daemon's side pinned it, so the header could have gone
// and only a text match on index.html would have noticed.
func TestSpeedHistoryCountsRowsInTotalAndRunsInTotalRuns(t *testing.T) {
	t.Run("unthinned", func(t *testing.T) {
		s := newTestServer(t)
		seedRuns(t, s, 10, time.Minute)
		seedRound(t, s, time.Now().Add(-2*time.Hour).Unix(), 2)

		rr, arr := getJSON(t, s, "/api/speed?mins=100000")
		if got := rr.Header().Get("X-Sampled"); got != "false" {
			t.Fatalf("X-Sampled = %q, want false: 13 rows fit the budget whole", got)
		}
		if total, _ := strconv.Atoi(rr.Header().Get("X-Total-Count")); total != 13 {
			t.Errorf("X-Total-Count = %d, want 13: 11 runs plus the 2 kept losers, which are rows to the chart like any other", total)
		}
		if len(arr) != 13 {
			t.Errorf("returned %d points, want 13", len(arr))
		}
		if v := rr.Header().Get("X-Total-Runs"); v != "" {
			t.Errorf("X-Total-Runs = %q on an unthinned answer; it is sent only when the answer was thinned", v)
		}
	})
	t.Run("thinned", func(t *testing.T) {
		s := newTestServer(t)
		const plain = maxSeriesPoints + 50
		seedRuns(t, s, plain, time.Second)
		seedRound(t, s, time.Now().Add(-2*time.Hour).Unix(), 3)

		rr, arr := getJSON(t, s, "/api/speed?mins=100000")
		if rr.Header().Get("X-Sampled") != "true" || len(arr) >= plain+4 {
			t.Fatalf("nothing was thinned (%d points for %d rows); this test is guarding nothing", len(arr), plain+4)
		}
		if total, _ := strconv.Atoi(rr.Header().Get("X-Total-Count")); total != plain+4 {
			t.Errorf("X-Total-Count = %d, want %d rows (%d runs plus 3 kept losers)", total, plain+4, plain+1)
		}
		runs, err := strconv.Atoi(rr.Header().Get("X-Total-Runs"))
		if err != nil {
			t.Fatalf("X-Total-Runs = %q on a thinned answer, want the run count: the client's caveat compares runs with runs", rr.Header().Get("X-Total-Runs"))
		}
		if runs != plain+1 {
			t.Errorf("X-Total-Runs = %d, want %d: the runs alone, without the 3 kept losers", runs, plain+1)
		}
	})
}

// Every disclosure header /api/speed sets has to be in docs/api.md's entry for
// the endpoint: the reference is the half of the contract a third-party
// consumer works from, and X-Total-Runs shipped without reaching it - while
// the entry went on calling X-Total-Count "runs in the window", the very
// reading the new header was added to correct.
func TestSpeedHistoryDisclosureHeadersAreDocumented(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatalf("read web.go: %v", err)
	}
	const from, to = "func (s *Server) handleSpeedHistory(", "func (s *Server) handleSpeedRuns("
	start, end := strings.Index(string(src), from), strings.Index(string(src), to)
	if start < 0 || end < start {
		t.Fatalf("web.go no longer holds %q ... %q in that order; this test reads the wrong region", from, to)
	}
	var headers []string
	for _, m := range regexp.MustCompile(`w\.Header\(\)\.Set\("(X-[A-Za-z-]+)"`).FindAllStringSubmatch(string(src[start:end]), -1) {
		headers = append(headers, m[1])
	}
	if len(headers) < 3 {
		t.Fatalf("found only %d X- headers in handleSpeedHistory; the disclosure moved and this test reads nothing", len(headers))
	}

	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}
	const item = "- `GET /api/speed?"
	at := strings.Index(string(doc), item)
	if at < 0 {
		t.Fatalf("docs/api.md has no entry starting %q", item)
	}
	entry := string(doc[at:])
	if next := strings.Index(entry[1:], "\n- `"); next >= 0 {
		entry = entry[:next+1]
	}
	for _, h := range headers {
		if !strings.Contains(entry, "`"+h+"`") {
			t.Errorf("/api/speed sets %s and docs/api.md's entry for the endpoint never names it - a consumer reading the reference cannot know it exists:\n%s", h, entry)
		}
	}
	if strings.Contains(entry, "`X-Total-Count` (runs in the window)") {
		t.Errorf("docs/api.md calls X-Total-Count the runs in the window; it counts rows, kept Best-of losers included, which is what X-Total-Runs exists to tell apart")
	}
}
