package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
)

// THE DEFAULT BROWSE LIST FOLLOWS WHERE AUTO LAST LANDED. Centring on the
// first anchored origin guaranteed only "a city the race would CONSIDER" -
// measured on the motivating link, the race kept choosing Montreal while the
// browse list sat on Toronto, and the two Ookla pools are disjoint, so the
// server every auto run actually used was findable at no scroll position.
// Remembering the last auto run's server for DISPLAY (the race itself stays
// uncached) upgrades the guarantee to "the city auto last chose", and the
// prepend below makes "the last-used server is in the list" unconditional.

type lastRunFixture struct {
	st         *store.Store
	askedID    string
	gotKeyword string
	gotLat     float64
	gotLon     float64
	list       []speedtest.ServerInfo // what the coordinate fetch (fallback path) returns
	searchList []speedtest.ServerInfo // what the name search (last-run path) returns
	getErr     error
	searchErr  error
}

func newLastRunFixture(t *testing.T) *lastRunFixture {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &lastRunFixture{st: st}

	oldGet := getOoklaServer
	getOoklaServer = func(_ context.Context, id string) (speedtest.ServerInfo, error) {
		f.askedID = id
		if f.getErr != nil {
			return speedtest.ServerInfo{}, f.getErr
		}
		// Country deliberately empty: the real by-ID endpoint omits it on
		// sparse records, and the UI must cope (serverOptionText).
		return speedtest.ServerInfo{ID: id, Sponsor: "Example ISP", Name: "Newtown, QC", Country: ""}, nil
	}
	t.Cleanup(func() { getOoklaServer = oldGet })

	oldSearch := searchOoklaServers
	searchOoklaServers = func(_ context.Context, keyword string) ([]speedtest.ServerInfo, error) {
		f.gotKeyword = keyword
		return f.searchList, f.searchErr
	}
	t.Cleanup(func() { searchOoklaServers = oldSearch })

	oldList := listOoklaServers
	listOoklaServers = func(_ context.Context, lat, lon float64) ([]speedtest.ServerInfo, error) {
		f.gotLat, f.gotLon = lat, lon
		return f.list, nil
	}
	t.Cleanup(func() { listOoklaServers = oldList })
	return f
}

// seedRun inserts one speed row and, unless winReason is "", a matching
// selection report whose winner carries that reason.
func (f *lastRunFixture) seedRun(t *testing.T, ts int64, id, engine, winReason string) {
	t.Helper()
	ctx := context.Background()
	if err := f.st.InsertSpeed(ctx, store.SpeedSample{TS: ts, Server: "Example ISP, Newtown", ServerID: id, Engine: engine}); err != nil {
		t.Fatal(err)
	}
	if winReason == "" {
		return
	}
	rows := []store.SpeedServerRow{{
		RunTS: ts, ServerID: id, Server: "Example ISP, Newtown",
		RankOrder: 1, Selected: true, Measured: true, Winner: true, WinReason: winReason,
	}}
	if err := f.st.InsertSpeedServers(ctx, rows); err != nil {
		t.Fatal(err)
	}
}

type browseBody struct {
	Location string `json:"location"`
	Centre   string `json:"centre"`
	Servers  []struct {
		ID         string  `json:"id"`
		DistanceKM float64 `json:"distance_km"`
		Lat        float64 `json:"lat"`
		Lon        float64 `json:"lon"`
	} `json:"servers"`
}

func (f *lastRunFixture) ask(t *testing.T) browseBody {
	t.Helper()
	f.askedID, f.gotKeyword, f.gotLat, f.gotLon = "", "", -1, -1
	s := &Server{
		netinfo: stubNetInfo{},
		store:   f.st,
		// Anchored origins are always on offer, so every test also proves
		// whether the last-run centre beat them or fell back to them.
		AutoOriginsFn: func() []speedtest.Origin {
			return []speedtest.Origin{{Kind: "exit", Label: "Oldtown, XX", Lat: 12.34, Lon: -76.54, Anchored: true}}
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers", nil)
	req.Header.Set("Content-Type", "application/json")
	s.handleSpeedtestServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body browseBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestBrowseListCentresOnLastAutoRun(t *testing.T) {
	f := newLastRunFixture(t)
	f.seedRun(t, 1000, "777", "ookla", "fastest_ranked")
	// The search is the coordinate ORACLE: the measured server's own row -
	// matched by ID among same-named cities worldwide - supplies the real
	// catalog coordinates the metro fetch centres on. The stray same-name row
	// carries decoy coordinates that must not win.
	f.searchList = []speedtest.ServerInfo{
		{ID: "555", Name: "Newtown, QC", Lat: 33.2, Lon: -97.5, DistanceKM: 3600},
		{ID: "777", Name: "Newtown, QC", Lat: 45.4, Lon: -73.6, DistanceKM: 297},
	}
	// The metro fetch around those coordinates omitting the server itself is
	// unlikely but possible, so the stub reproduces it to pin the prepend.
	f.list = []speedtest.ServerInfo{{ID: "888", Name: "Oldtown Heights, QC", DistanceKM: 6}}

	body := f.ask(t)
	if f.askedID != "777" {
		t.Fatalf("resolved server %q, want the last auto run's 777", f.askedID)
	}
	if f.gotKeyword != "Newtown, QC" {
		t.Errorf("searched for %q, want the server's NAME label - never its by-ID coordinates, which the endpoint backfills with the caller's own position", f.gotKeyword)
	}
	if f.gotLat != 45.4 || f.gotLon != -73.6 {
		t.Errorf("metro fetch centred on %v,%v; want the measured server's OWN row's coordinates - not the decoy's, not the origin's", f.gotLat, f.gotLon)
	}
	if body.Centre != "last_run" {
		t.Errorf("centre = %q, want last_run so the panel can word it in the past tense", body.Centre)
	}
	if body.Location != "Newtown, QC" {
		t.Errorf("location = %q, want the server's name label alone (its by-ID country can be empty - a trailing comma here was shipped once)", body.Location)
	}
	if len(body.Servers) != 2 || body.Servers[0].ID != "777" || body.Servers[0].DistanceKM != 0 {
		t.Errorf("servers = %+v, want the last-used server prepended at distance 0", body.Servers)
	}
	// The prepended row was resolved by ID, which carries no coordinate, but the
	// list was centred on this server's own catalogue position - so the row
	// goes out with it. The picker stars straight out of this response, and a
	// row without a pair is saved at 0,0: the one server the caption names
	// would be the one starred without a position.
	if body.Servers[0].Lat != 45.4 || body.Servers[0].Lon != -73.6 {
		t.Errorf("prepended row carries %v,%v; want the catalogue coordinate the list was centred on", body.Servers[0].Lat, body.Servers[0].Lon)
	}

	// When the metro fetch does contain the server, nothing is prepended: the
	// guarantee is presence, not position - and the row keeps its own pair.
	f.list = []speedtest.ServerInfo{{ID: "888", Name: "Oldtown Heights, QC", DistanceKM: 6}, {ID: "777", Name: "Newtown, QC", DistanceKM: 9, Lat: 45.4, Lon: -73.6}}
	body = f.ask(t)
	n := 0
	for _, srv := range body.Servers {
		if srv.ID == "777" {
			n++
			if srv.Lat != 45.4 || srv.Lon != -73.6 {
				t.Errorf("listed row carries %v,%v; want its own catalogue pair", srv.Lat, srv.Lon)
			}
		}
	}
	if n != 1 || len(body.Servers) != 2 {
		t.Errorf("servers = %+v, want 777 exactly once and no prepend", body.Servers)
	}
}

// Pinned runs, engine mismatches, and rows-less history must not centre the
// browse list: a pinned one-off centring the list on the pin's city is the
// exact confusion last-run centring exists to remove, and a run without a
// selection report cannot prove how its server was chosen.
func TestBrowseListSkipsRunsAutoDidNotChoose(t *testing.T) {
	f := newLastRunFixture(t)
	f.seedRun(t, 1000, "777", "ookla", "fastest_ranked")                   // the last AUTO run
	f.seedRun(t, 2000, "999", "ookla", speedtest.WinReasonPinned)          // newer, but pinned
	f.seedRun(t, 2200, "998", "ookla", speedtest.WinReasonPinnedBestOf)    // a pinned best-of the pin won: still the pin's city
	f.seedRun(t, 2400, "997", "ookla", speedtest.WinReasonPinnedCompanion) // a pinned best-of a neighbour won: still beside the pin
	f.seedRun(t, 2500, "555", "ookla", "")                                 // newer still, no report rows
	f.seedRun(t, 3000, "444", "iperf3", "score")                           // newest, wrong engine

	f.ask(t)
	if f.askedID != "777" {
		t.Fatalf("resolved server %q, want 777: the newest run whose report proves auto chose it", f.askedID)
	}

	// Nothing qualifying at all: the anchored-origin fallback, labelled as such.
	g := newLastRunFixture(t)
	g.seedRun(t, 1000, "999", "ookla", speedtest.WinReasonPinned)
	body := g.ask(t)
	if g.gotLat != 12.34 || g.gotLon != -76.54 {
		t.Errorf("centred on %v,%v; want the exit origin fallback", g.gotLat, g.gotLon)
	}
	if body.Centre != "" {
		t.Errorf("centre = %q, want empty: a fallback centre must not claim to be where auto landed", body.Centre)
	}
}

// An Ookla hiccup on the ID lookup or the name search degrades to the fallback
// centre instead of failing the browse fetch - the list is worth more than its
// centring. An empty search result is the same degradation: a list of nothing
// explains nothing.
func TestBrowseListLookupFailureFallsBack(t *testing.T) {
	for name, breakIt := range map[string]func(*lastRunFixture){
		"id lookup fails":   func(f *lastRunFixture) { f.getErr = errors.New("ookla unreachable") },
		"name search fails": func(f *lastRunFixture) { f.searchErr = errors.New("ookla unreachable") },
		"own row not found": func(f *lastRunFixture) {
			f.searchList = []speedtest.ServerInfo{{ID: "999", Name: "Newtown, QC", Lat: 45.4, Lon: -73.6}}
		},
		"own row uncharted": func(f *lastRunFixture) { f.searchList = []speedtest.ServerInfo{{ID: "777", Name: "Newtown, QC"}} },
	} {
		f := newLastRunFixture(t)
		f.seedRun(t, 1000, "777", "ookla", "fastest_ranked")
		breakIt(f)

		body := f.ask(t)
		if f.gotLat != 12.34 || f.gotLon != -76.54 {
			t.Errorf("%s: centred on %v,%v; want the exit origin fallback", name, f.gotLat, f.gotLon)
		}
		if body.Centre != "" || body.Location != "Oldtown, XX" {
			t.Errorf("%s: centre = %q location = %q, want a plain fallback response", name, body.Centre, body.Location)
		}
	}
}
