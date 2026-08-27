package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// idleSpeed is a SpeedTrigger that is, or is not, mid-run.
type idleSpeed struct{ id uint64 }

func (s idleSpeed) RunOnce(context.Context, string) (store.SpeedSample, error) {
	return store.SpeedSample{}, nil
}
func (s idleSpeed) RunID() uint64         { return s.id }
func (s idleSpeed) Abort(uint64) bool     { return false }
func (s idleSpeed) CurrentServer() string { return "" }
func (s idleSpeed) NextRun() time.Time    { return time.Time{} }

func askCandidates(t *testing.T, s *Server, method string, ct bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/speedtest/candidates", nil)
	if ct {
		req.Header.Set("Content-Type", "application/json")
	}
	s.handleSpeedtestCandidates(rec, req)
	return rec
}

// The picker's Auto button asks for the race's own field and shows it as the
// daemon ranked it: fastest first, each row carrying its ping and the origin
// that surfaced it, the catalogue coordinate riding along so a star saves it.
func TestCandidatesEndpointShowsTheRaceFieldFastestFirst(t *testing.T) {
	ms := func(v float64) *float64 { return &v }
	s := &Server{netinfo: stubNetInfo{}, speed: idleSpeed{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.RaceListingFn = func(ctx context.Context) (speedtest.RaceListing, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("the listing must run under a deadline; a stalled race must not hold the request open")
		}
		saved := speedtest.Origin{Kind: "saved", Label: "Montréal, QC", Lat: 45.5, Lon: -73.5, Anchored: true}
		return speedtest.RaceListing{
			Origins: []speedtest.Origin{{Kind: "isp", Label: "Toronto, CA", Anchored: true}, saved, {Kind: "geo", Label: "your connection"}},
			Servers: []speedtest.RaceCandidate{
				{ServerInfo: speedtest.ServerInfo{ID: "1993", Sponsor: "EBOX", Name: "Montréal, QC", DistanceKM: 1, Lat: 45.5, Lon: -73.5, PingMS: ms(10.4)}, Origin: "saved", OriginLabel: "Montréal, QC"},
				{ServerInfo: speedtest.ServerInfo{ID: "17568", Sponsor: "Bell Canada", Name: "North York, ON", DistanceKM: 4, Lat: 43.76, Lon: -79.41, PingMS: ms(27.7)}, Origin: "isp", OriginLabel: "Toronto, CA"},
				{ServerInfo: speedtest.ServerInfo{ID: "9", Sponsor: "Silent", Name: "Nowhere", DistanceKM: 8}, Origin: "geo"},
			},
			Winner: &saved,
		}, nil
	}
	rec := askCandidates(t, s, http.MethodPost, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Winner  map[string]any   `json:"winner"`
		Origins []map[string]any `json:"origins"`
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Winner["kind"] != "saved" || body.Winner["label"] != "Montréal, QC" || len(body.Origins) != 3 {
		t.Errorf("winner = %v origins = %v", body.Winner, body.Origins)
	}
	if len(body.Servers) != 3 || body.Servers[0]["id"] != "1993" || body.Servers[0]["ping_ms"] != 10.4 || body.Servers[0]["origin"] != "saved" {
		t.Fatalf("servers = %v, want the daemon's order with ping and origin on the wire", body.Servers)
	}
	if body.Servers[0]["lat"] != 45.5 || body.Servers[0]["lon"] != -73.5 {
		t.Errorf("the catalogue coordinate must ride along, or a star from this list saves the server at 0,0: %v", body.Servers[0])
	}
	if _, ok := body.Servers[2]["ping_ms"]; ok {
		t.Errorf("an unanswered racer must carry no ping_ms, not a zero: %v", body.Servers[2])
	}
	if _, ok := body.Servers[2]["lat"]; ok {
		t.Errorf("a racer with no coordinate must send none: %v", body.Servers[2])
	}
}

// The endpoint reaches out, so it takes the same guards as the browse list -
// POST and a JSON content type - and it refuses to race through a running
// transfer, whose traffic the pings would measure instead of the servers.
func TestCandidatesEndpointGuards(t *testing.T) {
	s := &Server{netinfo: stubNetInfo{}, speed: idleSpeed{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.RaceListingFn = func(context.Context) (speedtest.RaceListing, error) { return speedtest.RaceListing{}, nil }
	if rec := askCandidates(t, s, http.MethodGet, true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", rec.Code)
	}
	if rec := askCandidates(t, s, http.MethodPost, false); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type: status %d, want 415", rec.Code)
	}
	s.speed = idleSpeed{id: 7}
	if rec := askCandidates(t, s, http.MethodPost, true); rec.Code != http.StatusConflict {
		t.Errorf("mid-run: status %d, want 409", rec.Code)
	}
	s.speed = idleSpeed{}
	s.RaceListingFn = func(context.Context) (speedtest.RaceListing, error) {
		return speedtest.RaceListing{}, errors.New("ookla unreachable")
	}
	if rec := askCandidates(t, s, http.MethodPost, true); rec.Code != http.StatusBadGateway {
		t.Errorf("race failed: status %d, want 502", rec.Code)
	}
	s.RaceListingFn = nil
	if rec := askCandidates(t, s, http.MethodPost, true); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no tester wired: status %d, want 503", rec.Code)
	}
}
