package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// The speedtest times behind the latency chart's "During a speedtest" hover
// note: /api/speed/spans answers them for the chart's own window, /api/status
// says when they may have changed, and a backup leaves them out.

func putSpeedSpan(t *testing.T, st *store.Store, ts, dur int64) {
	t.Helper()
	if stored, err := st.InsertSpeedSpan(context.Background(), time.Unix(ts, 0), dur); err != nil || !stored {
		t.Fatalf("InsertSpeedSpan(%d, %d) = %v, %v", ts, dur, stored, err)
	}
}

func getSpans(t *testing.T, s *Server, query string) ([]store.SpeedSpan, string) {
	t.Helper()
	w := do(t, s.Handler(), "GET", "/api/speed/spans"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/speed/spans%s -> %d: %s", query, w.Code, w.Body.String())
	}
	var out []store.SpeedSpan
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out, strings.TrimSpace(w.Body.String())
}

func TestSpeedSpansEndpointWindows(t *testing.T) {
	s := newTestServer(t)
	if _, body := getSpans(t, s, "?mins=60"); body != "[]" {
		t.Fatalf("no spans answered %q, want [] - the chart iterates it", body)
	}
	now := time.Now().Unix()
	putSpeedSpan(t, s.store, now-3*3600, 40) // outside the last hour
	putSpeedSpan(t, s.store, now-1800, 45)
	putSpeedSpan(t, s.store, now-300, 30)

	got, _ := getSpans(t, s, "?mins=60")
	if len(got) != 2 || got[0].Start != now-1800 || got[0].End != now-1755 || got[1].Start != now-300 {
		t.Fatalf("?mins=60 answered %+v, want the two spans of the last hour", got)
	}
	// The default window is the series endpoint's: two hours.
	if got, _ := getSpans(t, s, ""); len(got) != 2 {
		t.Errorf("no window answered %+v, want the series default (two hours)", got)
	}
	got, _ = getSpans(t, s, fmt.Sprintf("?from=%d&to=%d", now-4*3600, now-1000))
	if len(got) != 2 || got[0].Start != now-3*3600 || got[1].Start != now-1800 {
		t.Errorf("?from&to answered %+v, want the two spans inside it", got)
	}
	// A reversed pair falls back to the default window, as /api/series does.
	if got, _ := getSpans(t, s, fmt.Sprintf("?from=%d&to=%d", now, now-3600)); len(got) != 2 {
		t.Errorf("reversed ?from&to answered %+v, want the default window's two", got)
	}
}

// Bounded by construction: spans are merged at the window's bucket width, so a
// year answers about as many as the chart has points. The fill is the densest
// that merges nothing, so the answer sits right at the bound.
func TestSpeedSpansEndpointIsBounded(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now()
	since := now.Add(-365 * 24 * time.Hour)
	mergeS := int64(seriesBucket(since, time.Time{}, now))
	tx, err := s.store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for ts := since.Unix() + 60; ts+1 <= now.Unix(); ts += mergeS + 2 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO speed_spans (ts, duration_s) VALUES (?, 1)`, ts); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got, _ := getSpans(t, s, fmt.Sprintf("?from=%d", since.Unix()))
	if len(got) > maxSeriesPoints+1 {
		t.Errorf("a year answered %d spans, want at most %d", len(got), maxSeriesPoints+1)
	}
	if len(got) < maxSeriesPoints-10 {
		t.Errorf("a year answered only %d spans: the fill no longer reaches the bound, so this proves nothing", len(got))
	}
}

func TestStatusReportsTheWireCounter(t *testing.T) {
	s := newTestServer(t)
	s.status = func() LiveStatus { return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)} }
	read := func() map[string]any {
		w := do(t, s.Handler(), "GET", "/api/status", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		var st map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return st
	}
	st := read()
	if st["speedtest_activity"] != float64(0) {
		t.Errorf("unwired: activity=%v, want 0", st["speedtest_activity"])
	}
	s.SpeedActivityFn = func() speedtest.Activity {
		return speedtest.Activity{Seq: 7, Trigger: "reconnect"}
	}
	st = read()
	if st["speedtest_activity"] != float64(7) {
		t.Errorf("activity=%v, want 7", st["speedtest_activity"])
	}
	if _, ok := st["speedtest_started_ts"]; ok {
		t.Error("status still carries speedtest_started_ts; nothing reads it")
	}
}

// Spans stay out of backups (like server_health): carrying them would stamp
// nearly every export with a newer schema, since reconnect tests are on by
// default, and older releases refuse such a file whole. A restore loses the
// hover notes and no number anywhere.
func TestExportLeavesSpeedSpansOut(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	now := time.Now().Unix()
	b := int64(1000)
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 120, DownMbps: 90, UpMbps: 10, Server: "Real", Engine: "ookla", DownBytes: &b}); err != nil {
		t.Fatal(err)
	}
	export := func() (map[string]json.RawMessage, int) {
		rr := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/export?config=1&latency=1&speed=1&downtime=1", nil)
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "127.0.0.1:54321"
		s.Handler().ServeHTTP(rr, r)
		if rr.Code != http.StatusOK {
			t.Fatalf("export: HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var ver int
		_ = json.Unmarshal(env["pingularity_export"], &ver)
		return env, ver
	}
	_, before := export()
	putSpeedSpan(t, s.store, now-200, 60)
	env, after := export()
	if _, ok := env["speed_spans"]; ok {
		t.Error("the backup carries speed_spans")
	}
	if after != before {
		t.Errorf("a stored span moved the backup's stamp from %d to %d", before, after)
	}
}

func TestSpeedtestHoldFamiliesRenderByTrigger(t *testing.T) {
	stats.ResetForTest()
	stats.Inc("monitor.speedtest_rounds.manual")
	stats.Inc("monitor.speedtest_bad_rounds.manual")
	stats.Inc("monitor.speedtest_tail_bad_rounds.scheduled")
	stats.Inc("monitor.speedtest_downs_suppressed.reconnect")
	stats.Inc("monitor.speedtest_downs_delayed.reconnect")
	stats.AddF("monitor.speedtest_delay_s_sum.reconnect", 30)
	body := scrape(t, newMetricsServer(t))
	for _, want := range []string{
		`pingularity_probe_speedtest_rounds_total{trigger="manual"} 1`,
		`pingularity_probe_speedtest_failed_rounds_total{trigger="manual"} 1`,
		`pingularity_probe_speedtest_tail_failed_rounds_total{trigger="scheduled"} 1`,
		`pingularity_outages_suppressed_during_speedtest_total{trigger="reconnect"} 1`,
		`pingularity_outages_delayed_by_speedtest_total{trigger="reconnect"} 1`,
		`pingularity_outage_speedtest_delay_seconds_total{trigger="reconnect"} 30`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

// The dashboard refetches the speedtest times when speedtest_activity or
// speedtest_run_id moves, and a finished run's time is stored between the two
// edges. That only closes if the counter is read before the run id: then a poll
// that sees the counter past the engine and no claim read the run id after the
// time was stored.
func TestStatusReadsTheWireCounterBeforeTheRunID(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatalf("read web.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (s *Server) handleStatus(")
	if start < 0 {
		t.Fatal("handleStatus not found")
	}
	body = body[start:]
	wire, run := strings.Index(body, "s.SpeedActivityFn()"), strings.Index(body, "speedRunStatus(s.speed)")
	if wire < 0 || run < 0 || wire > run {
		t.Errorf("handleStatus must read SpeedActivityFn before speedRunStatus (at %d and %d)", wire, run)
	}
}
