package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pingular/pingularity/internal/netinfo"
	"github.com/pingular/pingularity/internal/speedtest"
)

// THE FALLBACK CENTRE MUST BE A CITY AUTO COULD CHOOSE. This pins the
// FALLBACK path only - the nil store below confines the handler to it; the
// primary path (centring on the last auto run's server) is pinned in
// lastruncentre_test.go. Historically this handler ran its own cascade - exit
// coordinate, else the Cloudflare PoP - and the PoP rung fired on any missing
// exit COORDINATE, not only where a traceroute could not run. On a residential
// link whose last hop RIPE cannot place, that centred the list on a distant
// PoP while every city auto races was domestic; the two pools are disjoint, so
// the server auto actually tests from could not be found in the picker at any
// scroll position, and the panel captioned it "near your ISP's exit".
// A starred server's city is an origin like any other, so with no exit and a
// coordinateless ISP line the browse list centres there rather than nowhere -
// the same reason it races (see main.autoOrigins).
func TestBrowseFallbackCentresOnAStarredCityWhenNothingElseIsPlaced(t *testing.T) {
	var gotLat, gotLon float64
	old := listOoklaServers
	listOoklaServers = func(_ context.Context, lat, lon float64) ([]speedtest.ServerInfo, error) {
		gotLat, gotLon = lat, lon
		return []speedtest.ServerInfo{}, nil
	}
	t.Cleanup(func() { listOoklaServers = old })
	s := &Server{netinfo: stubNetInfo{}, AutoOriginsFn: func() []speedtest.Origin {
		return []speedtest.Origin{
			{Kind: "isp", Label: "Toronto, CA"}, // geolocated by name only: no coordinate
			{Kind: "saved", Label: "Montréal, QC", Lat: 45.5, Lon: -73.5, Anchored: true},
			{Kind: "geo", Label: "your connection"},
		}
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers", nil)
	req.Header.Set("Content-Type", "application/json")
	s.handleSpeedtestServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if gotLat != 45.5 || gotLon != -73.5 {
		t.Errorf("centred on %v,%v; want the starred city, the first origin with a position", gotLat, gotLon)
	}
}

func TestBrowseFallbackCentresOnACityAutoWouldRace(t *testing.T) {
	var gotLat, gotLon float64
	old := listOoklaServers
	listOoklaServers = func(_ context.Context, lat, lon float64) ([]speedtest.ServerInfo, error) {
		gotLat, gotLon = lat, lon
		return []speedtest.ServerInfo{}, nil
	}
	t.Cleanup(func() { listOoklaServers = old })

	ask := func(t *testing.T, origins []speedtest.Origin) string {
		t.Helper()
		gotLat, gotLon = -1, -1
		s := &Server{
			netinfo:       stubNetInfo{}, // permitted-nil guard only; the default branch no longer reads it
			AutoOriginsFn: func() []speedtest.Origin { return origins },
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers", nil)
		req.Header.Set("Content-Type", "application/json") // the endpoint reaches out; see handleSpeedtestServers
		s.handleSpeedtestServers(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Location string `json:"location"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Location
	}

	// The live shape on the motivating link: RIPE could not place the exit, so
	// no exit origin is offered at all and the ISP city is the first anchored
	// candidate.
	loc := ask(t, []speedtest.Origin{
		{Kind: "isp", Label: "Oldtown, XX", Lat: 12.345678, Lon: -76.543210, Anchored: true},
		{Kind: "geo", Label: "your connection"},
	})
	if gotLat != 12.345678 || gotLon != -76.543210 {
		t.Errorf("centred the list on %v,%v; want the ISP city auto would race, not a Cloudflare PoP",
			gotLat, gotLon)
	}
	if loc != "Oldtown, XX" {
		t.Errorf("location = %q, want the ISP city's label", loc)
	}

	// Ordering is load-bearing: an exit router RIPE DID place is the truer
	// origin and comes first.
	ask(t, []speedtest.Origin{
		{Kind: "exit", Label: "Oldtown, XX", Lat: 12.34, Lon: -76.54, Anchored: true},
		{Kind: "isp", Label: "Oldtown, XX", Lat: 12.345678, Lon: -76.543210, Anchored: true},
	})
	if gotLat != 12.34 || gotLon != -76.54 {
		t.Errorf("centred on %v,%v; want the exit router, which is listed first", gotLat, gotLon)
	}

	// Nothing anchored is not a gap: it is the remaining candidate. An
	// uncentred fetch makes the Ookla API place our address, which is exactly
	// what that candidate means - and is what auto would do too.
	if loc := ask(t, []speedtest.Origin{{Kind: "geo", Label: "your connection"}}); loc != "" {
		t.Errorf("location = %q with nothing anchored, want empty", loc)
	}
	if gotLat != 0 || gotLon != 0 {
		t.Errorf("centred on %v,%v with nothing anchored; want an uncentred fetch", gotLat, gotLon)
	}
}

// stubNetInfo satisfies the handler's permitted-nil guard without doing any
// lookups. The default browse branch no longer reads netinfo at all - that is
// the point of the change - so nothing here needs to answer meaningfully.
type stubNetInfo struct{}

func (stubNetInfo) Get() netinfo.Info                       { return netinfo.Info{} }
func (stubNetInfo) RefreshNow(context.Context) netinfo.Info { return netinfo.Info{} }
