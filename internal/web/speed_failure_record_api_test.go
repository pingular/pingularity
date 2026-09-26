package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A failed test kept by Record failed tests is listed by the runs table's
// endpoints and read by nothing else. These go through the real handlers, the
// surface the dashboard and API consumers actually see.

// seedFailureRecord stores a measurement and, a minute later, a failure record
// that spent bytes, so the failure is the newest row on the box.
func seedFailureRecord(t *testing.T, st *store.Store) (measTS, failTS int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()
	b, fb := int64(1000), int64(400)
	if err := st.InsertSpeed(ctx, store.SpeedSample{TS: now - 120, DownMbps: 94.5, UpMbps: 12.25, PingMS: 8.5,
		Server: "Real Telecom, Montreal", ServerID: "4242", Trigger: "scheduled", Engine: "ookla",
		DownBytes: &b, UpBytes: &b}); err != nil {
		t.Fatalf("seed measurement: %v", err)
	}
	if err := st.InsertSpeed(ctx, store.SpeedSample{TS: now - 60, Server: "Dead Telecom, Nowhere", ServerID: "13",
		Trigger: "scheduled", Engine: "ookla", Failed: true, FailStage: "ping", DownBytes: &fb}); err != nil {
		t.Fatalf("seed failure record: %v", err)
	}
	return now - 120, now - 60
}

func TestRunsListingIncludesAFailureRecord(t *testing.T) {
	s := newTestServer(t)
	measTS, failTS := seedFailureRecord(t, s.store)
	h := s.Handler()

	w := do(t, h, "GET", "/api/speed/runs?limit=10", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/api/speed/runs: %d %s", w.Code, w.Body)
	}
	var page struct {
		Runs  []map[string]any `json:"runs"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Total != 2 || len(page.Runs) != 2 {
		t.Fatalf("listing = %d rows, total %d; want both rows and a total that counts them", len(page.Runs), page.Total)
	}
	f := runAtTS(page.Runs, failTS)
	if f == nil || f["failed"] != true || f["fail_stage"] != "ping" {
		t.Errorf("failure record in the listing = %v, want failed:true and fail_stage:ping", f)
	}
	if m := runAtTS(page.Runs, measTS); m == nil || m["failed"] != nil || m["fail_stage"] != nil {
		t.Errorf("measurement in the listing = %v, want no failed or fail_stage keys", m)
	}

	// The chart and its run count never see it.
	w = do(t, h, "GET", "/api/speed?mins=1440", "")
	if w.Code != http.StatusOK {
		t.Fatalf("/api/speed: %d", w.Code)
	}
	var chart []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &chart); err != nil {
		t.Fatalf("decode /api/speed: %v", err)
	}
	if len(chart) != 1 || runAtTS(chart, measTS) == nil {
		t.Errorf("/api/speed returned %d points, want only the measurement", len(chart))
	}
	// The thinning disclosure counts only what the chart could draw (X-Total-Runs
	// rides the same predicate and is sent only on a thinned answer; the store's
	// SpeedRunCount test holds it).
	if got := w.Header().Get("X-Total-Count"); got != "1" {
		t.Errorf("X-Total-Count = %q, want 1 - a failed test is not a point the chart has", got)
	}
	if got := w.Header().Get("X-Sampled"); got != "false" {
		t.Errorf("X-Sampled = %q, want false - a hidden failed test must not make a whole answer look thinned", got)
	}
}

func TestLatestReadsIgnoreANewerFailureRecord(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	measTS, _ := seedFailureRecord(t, st)
	s := statusServer(t, st)

	status := getStatus(t, s, "")
	sp, _ := status["speed"].(map[string]any)
	if sp == nil || int64(sp["ts"].(float64)) != measTS {
		t.Errorf("/api/status speed = %v, want the measurement (ts %d) - a failed test is never the latest run", sp, measTS)
	}
	body := scrape(t, s)
	if !strings.Contains(body, "pingularity_speed_download_mbps 94.5\n") {
		t.Errorf("/metrics download gauge is not the measurement's 94.5 - a failed test must not reach it:\n%s", body)
	}
}

func TestRunsLocateCountsANewerFailureRecord(t *testing.T) {
	s := newTestServer(t)
	measTS, _ := seedFailureRecord(t, s.store)
	w := do(t, s.Handler(), "GET", "/api/speed/runs?locate="+strconv.FormatInt(measTS, 10), "")
	if w.Code != http.StatusOK {
		t.Fatalf("locate: %d %s", w.Code, w.Body)
	}
	var loc struct {
		Offset int `json:"offset"`
		Total  int `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &loc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if loc.Offset != 1 || loc.Total != 2 {
		t.Errorf("locate = offset %d total %d, want 1 and 2: the table lists the newer failed test above the run, "+
			"so a jump that left it out would open the wrong page", loc.Offset, loc.Total)
	}
}

func TestRunServersIsEmptyNotMissingForAFailureRecord(t *testing.T) {
	s := newTestServer(t)
	_, failTS := seedFailureRecord(t, s.store)
	h := s.Handler()
	w := do(t, h, "GET", "/api/speed/runs/servers?ts="+strconv.FormatInt(failTS, 10), "")
	if w.Code != http.StatusOK {
		t.Fatalf("a listed failed test answered %d, want 200 - it is a run with no selection report", w.Code)
	}
	var rep struct {
		Servers []any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil || rep.Servers == nil || len(rep.Servers) != 0 {
		t.Errorf("servers = %v (%v), want an empty array", rep.Servers, err)
	}

	// A plain accounting row is still not a run.
	b := int64(10)
	acctTS := time.Now().Unix() - 30
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{TS: acctTS, Server: "x", Failed: true, DownBytes: &b}); err != nil {
		t.Fatal(err)
	}
	if w := do(t, h, "GET", "/api/speed/runs/servers?ts="+strconv.FormatInt(acctTS, 10), ""); w.Code != http.StatusNotFound {
		t.Errorf("an accounting row answered %d, want 404", w.Code)
	}
}

func TestSpeedRunDeleteRemovesAFailureRecord(t *testing.T) {
	s := newTestServer(t)
	measTS, failTS := seedFailureRecord(t, s.store)
	if got := dataUsedAll(t, s); got != 2400 {
		t.Fatalf("precondition: data used %d, want 2400", got)
	}
	w := do(t, s.Handler(), "POST", "/api/speed/runs/delete", `{"ts":`+strconv.FormatInt(failTS, 10)+`}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deleted":1`) {
		t.Fatalf("delete: %d %s, want deleted:1", w.Code, w.Body)
	}
	if runs := listedSpeedRuns(t, s); len(runs) != 1 || runAtTS(runs, measTS) == nil {
		t.Errorf("after the delete the table lists %d rows, want only the measurement", len(runs))
	}
	if got := dataUsedAll(t, s); got != 2000 {
		t.Errorf("data used after the delete = %d, want 2000 - the failed test's bytes go with it", got)
	}
}
