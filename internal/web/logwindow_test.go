package web

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/logbuf"
)

// logResp is the /api/logs wire shape the viewer reads.
type logResp struct {
	Level    string         `json:"level"`
	Redact   bool           `json:"redact"`
	Epoch    string         `json:"epoch"`
	FirstSeq uint64         `json:"first_seq"`
	NextSeq  uint64         `json:"next_seq"`
	Dropped  uint64         `json:"dropped"`
	Lines    []logbuf.Entry `json:"lines"`
}

// filledLogServer returns a server whose ring holds n lines, the state a daemon
// reaches after ~1.8h of debug logging and keeps thereafter (logs.txt restores
// the ring across restarts).
func filledLogServer(t *testing.T, capacity, n int) *Server {
	t.Helper()
	s := newTestServer(t)
	ring := logbuf.New(capacity)
	for i := 0; i < n; i++ {
		ring.Append(fmt.Sprintf("time=2026-07-25T18:22:27.825-05:00 level=DEBUG msg=http path=/api/logs seq=%d", i),
			fmt.Sprintf("time=2026-07-25T18:22:27.825-05:00 level=DEBUG msg=http path=/api/logs seq=%d", i))
	}
	s.Logs = ring
	return s
}

func getLogs(t *testing.T, s *Server, q string) logResp {
	t.Helper()
	w := do(t, s.Handler(), "GET", "/api/logs"+q, "")
	if w.Code != 200 {
		t.Fatalf("GET /api/logs%s -> %d", q, w.Code)
	}
	var out logResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

// A bare read must return a bounded tail, not the whole ring. The viewer polls
// this every 2.5s; at the shipped 4000-line cap the full ring is ~1.1 MB
// uncompressed with no-store, which is invisible on a LAN and unfinishable
// inside the viewer's own 5s deadline over the degraded link being diagnosed.
func TestLogsDefaultsToABoundedTail(t *testing.T) {
	s := filledLogServer(t, 4000, 4000)
	d := getLogs(t, s, "")
	if len(d.Lines) != defaultLogLines {
		t.Fatalf("bare GET /api/logs returned %d lines, want the newest %d", len(d.Lines), defaultLogLines)
	}
	if !strings.HasSuffix(d.Lines[len(d.Lines)-1].Raw, "seq=3999") {
		t.Fatalf("tail ends at %q, want the NEWEST line", d.Lines[len(d.Lines)-1].Raw)
	}
	if d.NextSeq != 4000 || d.FirstSeq != 0 || d.Dropped != 0 {
		t.Fatalf("cursor first=%d next=%d dropped=%d, want 0/4000/0", d.FirstSeq, d.NextSeq, d.Dropped)
	}
	if d.Epoch == "" {
		t.Fatal("no epoch: a client cannot tell a cursor from this ring from one issued before a restart")
	}
	// The ceiling is the same one parsePage puts on every other paged read.
	if n := len(getLogs(t, s, "?limit=99999").Lines); n != maxPageLimit {
		t.Fatalf("?limit=99999 returned %d lines, want the shared %d ceiling", n, maxPageLimit)
	}
	// ...and ?limit=0 is the documented escape hatch for a scripted caller that
	// really does want the whole buffer.
	if n := len(getLogs(t, s, "?limit=0").Lines); n != 4000 {
		t.Fatalf("?limit=0 returned %d lines, want the whole 4000-line ring", n)
	}
}

// The steady-state poll costs what actually happened, not what the ring holds.
func TestLogsCursorReturnsOnlyWhatArrived(t *testing.T) {
	s := filledLogServer(t, 4000, 4000)
	d := getLogs(t, s, "")
	cur := fmt.Sprintf("?since=%d&epoch=%s", d.NextSeq, url.QueryEscape(d.Epoch))

	// Nothing new: an empty response, and the cursor holds.
	d2 := getLogs(t, s, cur)
	if len(d2.Lines) != 0 || d2.NextSeq != d.NextSeq {
		t.Fatalf("caught-up poll returned %d lines / next=%d, want 0 / %d", len(d2.Lines), d2.NextSeq, d.NextSeq)
	}

	s.Logs.Append("time=… level=INFO msg=probe target=1.1.1.1", "time=… level=INFO msg=probe target=1.1.1.x")
	s.Logs.Append("time=… level=INFO msg=dns target=example.com", "time=… level=INFO msg=dns target=example.com")
	d3 := getLogs(t, s, cur)
	if len(d3.Lines) != 2 {
		t.Fatalf("after 2 new lines the cursor poll returned %d lines, want exactly 2", len(d3.Lines))
	}
	if !strings.Contains(d3.Lines[0].Raw, "msg=probe") || !strings.Contains(d3.Lines[1].Raw, "msg=dns") {
		t.Fatalf("delta returned the wrong lines: %q, %q", d3.Lines[0].Raw, d3.Lines[1].Raw)
	}
	if d3.Lines[0].Masked != "time=… level=INFO msg=probe target=1.1.1.x" {
		t.Fatalf("the masked form must ride along so the viewer can flip the mask with no round-trip; got %q", d3.Lines[0].Masked)
	}
	if d3.NextSeq != d.NextSeq+2 {
		t.Fatalf("next=%d after 2 lines, want %d", d3.NextSeq, d.NextSeq+2)
	}
	if d3.Dropped != 0 {
		t.Fatalf("dropped=%d on a contiguous read, want 0", d3.Dropped)
	}
}

// A cursor that fell out of the ring must be reported, not silently spliced
// over: the viewer draws a seam so the operator knows the history it is reading
// is not contiguous.
func TestLogsReportsEvictedCursorAsDropped(t *testing.T) {
	s := filledLogServer(t, 10, 10)
	d := getLogs(t, s, "")
	for i := 0; i < 25; i++ { // turn the ring over twice
		s.Logs.Append(fmt.Sprintf("later %d", i), "")
	}
	d2 := getLogs(t, s, fmt.Sprintf("?since=%d&epoch=%s", d.NextSeq, url.QueryEscape(d.Epoch)))
	if d2.Dropped != 15 {
		t.Fatalf("dropped=%d after 15 lines were evicted past the cursor, want 15", d2.Dropped)
	}
	if len(d2.Lines) != 10 || d2.Lines[0].Raw != "later 15" {
		t.Fatalf("evicted cursor returned %d lines starting %q, want the 10 still held from 'later 15'", len(d2.Lines), d2.Lines[0].Raw)
	}
}

// The epoch is load-bearing, not decoration. A restart reseeds the ring from
// logs.txt with sequences starting at 0 again, so a cursor an open tab has held
// across the restart names a DIFFERENT line. Honouring it would render a wrong
// window that looks exactly like a right one.
func TestLogsIgnoresACursorFromAnotherRing(t *testing.T) {
	s := filledLogServer(t, 4000, 4000)
	d := getLogs(t, s, "")

	// The tab holds sequence 3000 from the ring that ran before the restart.
	stale := fmt.Sprintf("?since=3000&epoch=%s", url.QueryEscape("some-other-ring"))
	d2 := getLogs(t, s, stale)
	if len(d2.Lines) != defaultLogLines {
		t.Fatalf("a cursor from another ring returned %d lines, want a full %d-line resync tail", len(d2.Lines), defaultLogLines)
	}
	if !strings.HasSuffix(d2.Lines[len(d2.Lines)-1].Raw, "seq=3999") {
		t.Fatalf("resync must answer with the NEWEST lines; got %q", d2.Lines[len(d2.Lines)-1].Raw)
	}
	if d2.Epoch != d.Epoch {
		t.Fatalf("epoch %q != %q: the client can only detect the change if the response names the CURRENT ring", d2.Epoch, d.Epoch)
	}
	// A cursor beyond the end (same ring, e.g. held across a Clear) reads as
	// "nothing new" rather than as an error or a wrong window.
	if n := len(getLogs(t, s, fmt.Sprintf("?since=99999&epoch=%s", url.QueryEscape(d.Epoch))).Lines); n != 0 {
		t.Fatalf("a cursor past the end returned %d lines, want 0", n)
	}
	// Garbage is not fatal either: read endpoints default gracefully.
	if n := len(getLogs(t, s, "?since=not-a-number&epoch="+url.QueryEscape(d.Epoch)).Lines); n != defaultLogLines {
		t.Fatalf("an unparseable ?since returned %d lines, want the default tail", n)
	}
}

// The bug-report path is the one caller that legitimately wants everything, and
// it is a one-shot user action rather than a 2.5s poll. It must stay complete.
func TestLogsDownloadStaysWhole(t *testing.T) {
	s := filledLogServer(t, 4000, 4000)
	w := do(t, s.Handler(), "GET", "/api/logs?download=1", "")
	if w.Code != 200 {
		t.Fatalf("download -> %d", w.Code)
	}
	if n := strings.Count(w.Body.String(), "\n"); n != 4000 {
		t.Fatalf("?download=1 wrote %d lines, want the complete 4000-line buffer", n)
	}
}

// A POST is a view flip or a level change; it must not cost a full-ring response
// to acknowledge itself, and the clear branch must still empty the buffer.
func TestLogsPostAnswersWithTheSameBoundedWindow(t *testing.T) {
	s := filledLogServer(t, 4000, 4000)
	w := do(t, s.Handler(), "POST", "/api/logs", `{"redact":true}`)
	if w.Code != 200 {
		t.Fatalf("POST -> %d: %s", w.Code, w.Body.String())
	}
	var d logResp
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Lines) != defaultLogLines {
		t.Fatalf("POST returned %d lines, want the same bounded %d-line window a GET gives", len(d.Lines), defaultLogLines)
	}
	w = do(t, s.Handler(), "POST", "/api/logs", `{"clear":true}`)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(d.Lines) != 0 {
		t.Fatalf("clear left %d lines", len(d.Lines))
	}
	if d.FirstSeq != 4000 {
		t.Fatalf("first_seq=%d after clearing a 4000-line ring, want 4000 - a cursor from before the clear must not look valid", d.FirstSeq)
	}
}

// A server with no ring wired up (headless, tests) must answer, not panic.
func TestLogsWithoutARing(t *testing.T) {
	s := newTestServer(t)
	d := getLogs(t, s, "?since=5&epoch=x")
	if len(d.Lines) != 0 || d.Epoch != "" {
		t.Fatalf("nil ring returned %d lines / epoch %q, want 0 / \"\"", len(d.Lines), d.Epoch)
	}
}

// The window ceiling is literally the one parsePage uses - the point of the
// cluster is one sizing rule, not a fourth dialect of it.
func TestLogLimitSharesThePageCeiling(t *testing.T) {
	if maxPageLimit != 1000 {
		t.Fatalf("maxPageLimit=%d; the README documents 1000", maxPageLimit)
	}
	s := filledLogServer(t, 4000, 4000)
	if n := len(getLogs(t, s, "?limit="+strconv.Itoa(maxPageLimit+1)).Lines); n != maxPageLimit {
		t.Fatalf("limit above the ceiling returned %d lines", n)
	}
}
