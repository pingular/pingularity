package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

func askPing(t *testing.T, s *Server, method, body string, ct bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/speedtest/ping", strings.NewReader(body))
	if ct {
		req.Header.Set("Content-Type", "application/json")
	}
	s.handleSpeedtestPing(rec, req)
	return rec
}

// The saved pane's refresh: POST + JSON (it reaches out), 409 during a run,
// 503 with no engine, the IDs cleaned and capped, the answer keyed by ID with
// null for a server that did not answer.
func TestPingEndpointMeasuresTheKeptServersOnDemand(t *testing.T) {
	s := &Server{speed: idleSpeed{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if rec := askPing(t, s, http.MethodPost, `{"ids":["1"]}`, true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no engine wired: HTTP %d, want 503", rec.Code)
	}
	var asked []string
	s.PingServersFn = func(ctx context.Context, ids []string) map[string]speedtest.ServerPing {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the pings must run under a deadline")
		}
		asked = ids
		ms, ok, bad := 7.25, true, false
		return map[string]speedtest.ServerPing{"1993": {PingMS: &ms, FallbackOK: &ok}, "42": {FallbackOK: &bad}, "7": {}}
	}
	if rec := askPing(t, s, http.MethodGet, "", true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: HTTP %d, want 405", rec.Code)
	}
	if rec := askPing(t, s, http.MethodPost, `{"ids":["1"]}`, false); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no JSON content type: HTTP %d, want 415 (a cross-site form post must not make the daemon ping)", rec.Code)
	}
	if rec := askPing(t, s, http.MethodPost, `{"ids":["x", " ", ""]}`, true); rec.Code != http.StatusBadRequest {
		t.Errorf("no usable id: HTTP %d, want 400", rec.Code)
	}
	busy := &Server{speed: idleSpeed{id: 7}, PingServersFn: s.PingServersFn, log: s.log}
	if rec := askPing(t, busy, http.MethodPost, `{"ids":["1993"]}`, true); rec.Code != http.StatusConflict {
		t.Errorf("during a run: HTTP %d, want 409", rec.Code)
	}
	ids := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		ids = append(ids, `"`+strings.Repeat("9", i%3+1)+`"`)
	}
	rec := askPing(t, s, http.MethodPost, `{"ids":["1993"," 42 ","1993","abc","`+strings.Repeat("1", 13)+`",`+strings.Join(ids, ",")+`]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if len(asked) == 0 || asked[0] != "1993" || asked[1] != "42" || len(asked) > maxPingIDs {
		t.Errorf("asked %v: want trimmed, deduplicated, digits only, at most %d", asked, maxPingIDs)
	}
	for _, id := range asked {
		if strings.Trim(id, "0123456789") != "" {
			t.Errorf("non-numeric id %q reached the engine", id)
		}
	}
	var out struct {
		Pings  map[string]*float64 `json:"pings"`
		Health map[string]bool     `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Pings["1993"] == nil || *out.Pings["1993"] != 7.25 {
		t.Errorf("1993: %v, want 7.25", out.Pings["1993"])
	}
	if v, ok := out.Pings["42"]; !ok || v != nil {
		t.Errorf("42 did not answer: want an explicit null, got %v (present %v)", v, ok)
	}
	// Health rides the same answer, three-state: a determined verdict either
	// way is carried, an undetermined one is absent rather than false.
	if v, ok := out.Health["1993"]; !ok || !v {
		t.Errorf("1993 is usable: health %v (present %v), want true", v, ok)
	}
	if v, ok := out.Health["42"]; !ok || v {
		t.Errorf("42 has no endpoint: health %v (present %v), want false", v, ok)
	}
	if _, ok := out.Health["7"]; ok {
		t.Error("an undetermined verdict must be absent, never false")
	}
}

// The no-network half: what the daemon's own runs measured, per server, as a
// median of recent ranking pings; a server the runs never ranked is absent.
func TestServerPingsEndpointServesTheHistorysMedians(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour).Unix()
	f := func(v float64) *float64 { return &v }
	for i, ms := range []float64{30, 10, 12} {
		if err := s.store.InsertSpeedServers(ctx, []store.SpeedServerRow{
			{RunTS: base + int64(i)*60, ServerID: "1993", Server: "EBOX", RankOrder: 1, RankPingMS: f(ms)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/speed/server-pings?ids=1993,42,%20abc", nil)
	s.handleSpeedServerPings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Pings map[string]float64 `json:"pings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Pings["1993"] != 12 {
		t.Errorf("1993: %v, want the median 12 of 10, 12, 30", out.Pings["1993"])
	}
	if _, ok := out.Pings["42"]; ok {
		t.Error("a server never ranked must be absent")
	}
	rec = httptest.NewRecorder()
	s.handleSpeedServerPings(rec, httptest.NewRequest(http.MethodPost, "/api/speed/server-pings?ids=1993", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: HTTP %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleSpeedServerPings(rec, httptest.NewRequest(http.MethodGet, "/api/speed/server-pings", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pings":{}`) {
		t.Errorf("no ids: HTTP %d %s, want an empty map", rec.Code, rec.Body.String())
	}
}

// The Auto listing says which rows a run would actually choose from.
func TestCandidatesEndpointCarriesInField(t *testing.T) {
	s := &Server{netinfo: stubNetInfo{}, speed: idleSpeed{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.RaceListingFn = func(ctx context.Context) (speedtest.RaceListing, error) {
		return speedtest.RaceListing{Servers: []speedtest.RaceCandidate{
			{ServerInfo: speedtest.ServerInfo{ID: "1", Sponsor: "A", Name: "Montréal"}, Origin: "exit", InField: true},
			{ServerInfo: speedtest.ServerInfo{ID: "2", Sponsor: "B", Name: "Toronto"}, Origin: "isp"},
		}}, nil
	}
	rec := askCandidates(t, s, http.MethodPost, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d", rec.Code)
	}
	var out struct {
		Servers []struct {
			ID      string `json:"id"`
			InField bool   `json:"in_field"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Servers) != 2 || !out.Servers[0].InField || out.Servers[1].InField {
		t.Errorf("in_field did not reach the wire: %+v", out.Servers)
	}
}

// A backup carrying a race verdict needs the stamp the v5 readers refuse (6),
// and only then: the v5 columns alone keep stamping 5.
func TestAnExportCarryingARaceVerdictStampsSix(t *testing.T) {
	s := newTestServer(t)
	ms := 8.4
	smp := store.SpeedSample{TS: time.Now().Add(-time.Minute).Unix(), Server: "EBOX", ServerID: "1993",
		Trigger: "scheduled", Engine: "ookla", DownMbps: 100, UpMbps: 20, PingMS: 9,
		RaceOutcome: "decided", RaceWinnerKind: "exit", RaceWinnerLabel: "Montréal", RaceWinnerMS: &ms}
	if err := s.store.InsertSpeed(context.Background(), smp); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/export?speed=1", nil)
	r.Host = "127.0.0.1:9000"
	s.Handler().ServeHTTP(rr, r)
	if rr.Code != 200 {
		t.Fatalf("export HTTP %d", rr.Code)
	}
	var env struct {
		Version int              `json:"pingularity_export"`
		Speed   []map[string]any `json:"speed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Version != 6 {
		t.Errorf("stamped %d, want 6: a v5 build accepts the envelope and then aborts on race_outcome mid-restore", env.Version)
	}
	if len(env.Speed) != 1 || env.Speed[0]["race_outcome"] != "decided" || env.Speed[0]["race_winner_ms"] != 8.4 {
		t.Errorf("the verdict did not make it into the backup: %v", env.Speed)
	}
	if _, ok := env.Speed[0]["race_racers"]; ok {
		t.Error("race_racers is carried by no row and must be dropped, like every unused post-v4 column")
	}
}

// The runs listing carries each run's win reason from its report, so the table
// can tag a server name; a run without a report carries none.
func TestRunsListingCarriesTheWinReason(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	ts := time.Now().Add(-time.Minute).Unix()
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: ts, DownMbps: 100, UpMbps: 20, PingMS: 9, Server: "EBOX", ServerID: "1993", Engine: "ookla"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: ts - 60, DownMbps: 90, UpMbps: 20, PingMS: 9, Server: "old", Engine: "ookla"}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertSpeedServers(ctx, []store.SpeedServerRow{
		{RunTS: ts, ServerID: "1993", Server: "EBOX", RankOrder: 2, Selected: true, Measured: true, Winner: true, WinReason: "incumbent"},
	}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleSpeedRuns(rec, httptest.NewRequest(http.MethodGet, "/api/speed/runs?limit=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Runs []struct {
			TS        int64  `json:"ts"`
			WinReason string `json:"win_reason"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Runs) != 2 || out.Runs[0].TS != ts || out.Runs[0].WinReason != "incumbent" {
		t.Errorf("runs %+v, want the newest tagged incumbent", out.Runs)
	}
	if out.Runs[1].WinReason != "" {
		t.Errorf("a run without a report must carry no reason, got %q", out.Runs[1].WinReason)
	}
}

// An older client still posts the on/off; it is read as the count it meant.
func TestSettingsAcceptTheRetiredBestOfOnOff(t *testing.T) {
	n := 5
	yes, no := true, false
	if got := bestOfCountFrom(nil, &yes, 1); got == nil || *got != 3 {
		t.Errorf("on -> %v, want 3", got)
	}
	if got := bestOfCountFrom(nil, &yes, 8); got != nil {
		t.Errorf("on over a round of 8 -> %v, want no change: an old page echoing \"on\" must not shrink the round to three", got)
	}
	if got := bestOfCountFrom(nil, &no, 8); got == nil || *got != 1 {
		t.Errorf("off -> %v, want 1", got)
	}
	if got := bestOfCountFrom(&n, &yes, 1); got == nil || *got != 5 {
		t.Errorf("both -> %v, want the count", got)
	}
	if got := bestOfCountFrom(nil, nil, 1); got != nil {
		t.Errorf("neither -> %v, want nil", got)
	}
}
