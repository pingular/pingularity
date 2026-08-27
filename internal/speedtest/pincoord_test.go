package speedtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// The measured case, from the user's connection. The pin is EBOX's Montreal
// server 1993; the caller is geolocated near Toronto. api/ios-config.php
// splices the caller's own ISP server into its reply at index 0 wearing the
// CALLER'S coordinates, so the by-ID resolve of this pin - and only of a pin
// that is our own ISP's server - reports Toronto.
const (
	callerLat, callerLon = "43.7154", "-79.3896" // Toronto: where WE are
	realLat, realLon     = "45.5017", "-73.5673" // Montreal: where the pin is
)

// stubPinByID swaps the pin's early by-ID resolve for a canned reply. Its
// Distance is the one the library derives from that same reply's client
// coordinate - exactly 0 when the row is the spliced one.
func stubPinByID(t *testing.T, srv *ookla.Server) {
	t.Helper()
	old := fetchServerByID
	fetchServerByID = func(_ context.Context, _ *ookla.UserConfig, id string) (*ookla.Server, error) {
		if id != srv.ID {
			return nil, errors.New("no such server")
		}
		c := *srv // a fresh copy per call: the run mutates the pin it is handed
		return &c, nil
	}
	t.Cleanup(func() { fetchServerByID = old })
}

// stubCatalogue swaps the sponsor search for canned rows and records every
// keyword it was asked for, verbatim.
func stubCatalogue(t *testing.T, rows ookla.Servers, err error) *catalogueCalls {
	t.Helper()
	c := &catalogueCalls{}
	old := fetchServersByKeyword
	fetchServersByKeyword = func(ctx context.Context, keyword string) (ookla.Servers, error) {
		dl, _ := ctx.Deadline() // zero when the fetch was handed an unbounded context
		c.mu.Lock()
		c.keywords = append(c.keywords, keyword)
		c.deadlines = append(c.deadlines, dl)
		c.mu.Unlock()
		return rows, err
	}
	t.Cleanup(func() { fetchServersByKeyword = old })
	return c
}

type catalogueCalls struct {
	mu        sync.Mutex
	keywords  []string
	deadlines []time.Time
}

func (c *catalogueCalls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.keywords...)
}

func (c *catalogueCalls) deadline(i int) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deadlines[i]
}

// stubPinProbe answers the pinned server's endpoint probe without a dial.
func stubPinProbe(t *testing.T) {
	t.Helper()
	old := probeEndpoint
	probeEndpoint = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeEndpoint = old })
}

// pinnedBestOf wires a run that pins server 1993 and asks for best-of.
func pinnedBestOf(t *testing.T) *Ookla {
	t.Helper()
	requireQuiet(t)
	stubServerList(t)
	stubPinProbe(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("e1", 1)}})
	stubRacePing(t, map[string]int{"e1": 5})
	forgetLocation(t, "pinned")
	forgetLocation(t, "auto")

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	o.ServerIDFn = func() string { return "1993" }
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Toronto, CA", Lat: 43.65, Lon: -79.38, Anchored: true}}
	}
	return o
}

// A pinned best-of run must draw its companions from beside the PIN. When the
// by-ID reply hands back our own coordinates instead, centring on them fetches
// the companions from our city; they then out-score the pin on ping-discounted
// throughput and get recorded in its place - measured, a pinned Montreal server
// produced two North York companions 500 km away.
func TestRunReasonCentresAPinnedBestOfRunOnTheServersOwnCoordinate(t *testing.T) {
	o := pinnedBestOf(t)
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: callerLat, Lon: callerLon, Distance: 0,
	})
	// The pin is not the first row: the sponsor's other servers are real rows
	// with real coordinates, so the recovery has to match on ID.
	cat := stubCatalogue(t, ookla.Servers{
		{ID: "44444", Sponsor: "EBOX", Name: "Quebec City", Lat: "46.81", Lon: "-71.21", Distance: 720},
		{ID: "1993", Sponsor: "EBOX", Name: "Montreal", Lat: realLat, Lon: realLon, Distance: 504.6},
	}, nil)
	// A saved pair is a snapshot from star time; the live catalogue outranks it
	// whenever it answers. A decoy here proves the order.
	o.SavedCoordFn = func(string) (float64, float64, bool) { return 40.0, -70.0, true }

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}

	loc := registeredLocation("pinned")
	if loc == nil {
		t.Fatal("the run centred on nothing; the pin's own coordinate never reached the fetch")
	}
	if loc.Lat == 40.0 && loc.Lon == -70.0 {
		t.Fatalf("centred on the saved snapshot %v,%v although the live catalogue answered", loc.Lat, loc.Lon)
	}
	if loc.Lat == 43.7154 && loc.Lon == -79.3896 {
		t.Fatalf("centred on %v,%v - the CALLER's coordinate out of the by-ID reply. "+
			"Best-of would fetch the pin's companions from our own city and they would "+
			"out-score the pin on most rounds", loc.Lat, loc.Lon)
	}
	if loc.Lat != 45.5017 || loc.Lon != -73.5673 {
		t.Fatalf("centred on %v,%v, want the pin's registered 45.5017,-73.5673", loc.Lat, loc.Lon)
	}
	if got := cat.all(); len(got) != 1 || got[0] != "EBOX" {
		t.Errorf("catalogue searched for %q, want exactly one search for the sponsor %q", got, "EBOX")
	}
}

// A by-ID coordinate with a real distance behind it is the server's own and is
// used as it stands. Servers genuinely 5 km away report 5 km, so a non-zero
// distance rules the splice out and the run must not spend a request proving
// what it already knows.
func TestRunReasonTrustsAByIDCoordinateWithADistance(t *testing.T) {
	o := pinnedBestOf(t)
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: realLat, Lon: realLon, Distance: 5.36,
	})
	cat := stubCatalogue(t, nil, errors.New("the catalogue must not be consulted"))

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}

	if got := cat.all(); len(got) != 0 {
		t.Errorf("catalogue searched for %q; a coordinate with a real distance behind it needs no recovery", got)
	}
	loc := registeredLocation("pinned")
	if loc == nil || loc.Lat != 45.5017 || loc.Lon != -73.5673 {
		t.Fatalf("centred on %v, want the by-ID coordinate 45.5017,-73.5673", loc)
	}
}

// Recovery can come up empty - a sponsor whose catalogue no longer lists the
// pinned ID. The untrustworthy coordinate must be dropped rather than used:
// with nothing pinned to centre on, the run races the candidate cities, which
// is what it already does for a pin Ookla gave no coordinate at all.
func TestRunReasonRacesWhenThePinsCoordinateCannotBeRecovered(t *testing.T) {
	o := pinnedBestOf(t)
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: callerLat, Lon: callerLon, Distance: 0,
	})
	stubCatalogue(t, ookla.Servers{
		{ID: "44444", Sponsor: "EBOX", Name: "Quebec City", Lat: "46.81", Lon: "-71.21"},
	}, nil)

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}

	if loc := registeredLocation("pinned"); loc != nil {
		t.Fatalf("centred on the pin at %v,%v after recovery failed; that is the caller's own "+
			"coordinate, which is exactly what must not centre the companions", loc.Lat, loc.Lon)
	}
	if loc := registeredLocation("auto"); loc == nil {
		t.Fatal("the run neither centred on the pin nor raced; its companions came from the API's guess at our address")
	}
}

// A failed catalogue fetch is not a coordinate, even when it returns rows with
// it: the library's list decode returns whatever it managed to parse ALONGSIDE
// the error, and half a document is not evidence of where a server is.
func TestRunReasonIgnoresTheRowsOfAFailedCatalogueFetch(t *testing.T) {
	o := pinnedBestOf(t)
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: callerLat, Lon: callerLon, Distance: 0,
	})
	stubCatalogue(t, ookla.Servers{
		{ID: "1993", Sponsor: "EBOX", Name: "Montreal", Lat: "1.0", Lon: "1.0"},
	}, errors.New("unexpected EOF"))

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}
	if loc := registeredLocation("pinned"); loc != nil {
		t.Fatalf("centred on %v,%v out of a fetch that errored", loc.Lat, loc.Lon)
	}
}

// Only a pinned best-of run resolves a pin early and only it can be misled by
// the by-ID coordinate. Every other shape of run must reach the network exactly
// as it did before.
func TestOnlyAPinnedBestOfRunLooksUpAPinsCoordinate(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Ookla)
	}{
		{"nothing pinned", func(o *Ookla) { o.ServerIDFn = nil }},
		// Pins an id the stubbed run list already carries: with best-of off the pin
		// is resolved from that list, and an id outside it would reach the network.
		{"pinned with best-of off", func(o *Ookla) {
			o.BestOfCountFn = func() int { return 1 }
			o.ServerIDFn = func() string { return "1" }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := pinnedBestOf(t)
			c.setup(o)
			var byID int
			old := fetchServerByID
			fetchServerByID = func(context.Context, *ookla.UserConfig, string) (*ookla.Server, error) {
				byID++
				return nil, errors.New("no by-ID resolve was expected")
			}
			t.Cleanup(func() { fetchServerByID = old })
			cat := stubCatalogue(t, nil, errors.New("no catalogue search was expected"))

			if _, err := o.RunReason(context.Background(), "manual"); err != nil {
				t.Fatalf("run: %v", err)
			}
			if byID != 0 {
				t.Errorf("resolved the pin by ID %d times", byID)
			}
			if got := cat.all(); len(got) != 0 {
				t.Errorf("searched the catalogue for %q", got)
			}
		})
	}
}

// Ookla's search is a literal substring match, so the keyword is the sponsor
// exactly as registered. Normalising it - trimming, case-folding, dropping
// accents - turns a hit into "no server available or found".
func TestSponsorCoordSearchesTheSponsorVerbatim(t *testing.T) {
	const sponsor = "Télécoms Vidéotron ltée "
	cat := stubCatalogue(t, ookla.Servers{
		{ID: "1993", Lat: realLat, Lon: realLon},
	}, nil)

	lat, lon, err := sponsorCoord(context.Background(), sponsor, "1993")
	if err != nil {
		t.Fatalf("sponsorCoord: %v", err)
	}
	if lat != realLat || lon != realLon {
		t.Errorf("got %s,%s want %s,%s", lat, lon, realLat, realLon)
	}
	got := cat.all()
	if len(got) != 1 || got[0] != sponsor {
		t.Errorf("searched for %q, want the sponsor byte for byte: %q", got, sponsor)
	}
}

// Without a sponsor there is nothing to search by, and an empty keyword would
// fetch the whole nearby list - a request that cannot answer the question.
func TestSponsorCoordMakesNoRequestWithoutASponsor(t *testing.T) {
	cat := stubCatalogue(t, nil, nil)
	if _, _, err := sponsorCoord(context.Background(), "", "1993"); err == nil {
		t.Error("reported a coordinate with no sponsor to search by")
	}
	if got := cat.all(); len(got) != 0 {
		t.Errorf("searched for %q with no sponsor", got)
	}
}

// The recovery runs inside the run's deadline and before anything is measured,
// so a catalogue endpoint that accepts the connection and goes quiet must not
// be able to spend the measurement's time on a coordinate.
func TestSponsorCoordBoundsItsFetch(t *testing.T) {
	cat := stubCatalogue(t, nil, nil)
	start := time.Now()
	if _, _, err := sponsorCoord(context.Background(), "EBOX", "1993"); err == nil {
		t.Fatal("want an error for a pin absent from the catalogue")
	}
	dl := cat.deadline(0)
	if dl.IsZero() {
		t.Fatal("the recovery fetch was handed an unbounded context; a silent endpoint would hold the run open")
	}
	if got := dl.Sub(start); got > pinCoordBudget+time.Second {
		t.Errorf("recovery deadline is %v out, want at most %v", got, pinCoordBudget)
	}
	// An absolute ceiling as well as a relative one, or the constant could be
	// raised to anything and this test would follow it up. The measured worst
	// case is ~4.5s and every second of it is spent before the run measures
	// anything, out of the run's own deadline.
	if pinCoordBudget > 20*time.Second {
		t.Errorf("pinCoordBudget = %v, too much of a run's deadline to spend on a coordinate", pinCoordBudget)
	}
}

// The search keyword is the sponsor as registered. Ookla matches it as a literal
// substring, so trimming or case-folding turns a hit into nothing found. The
// fetch seam is stubbed everywhere else, so this is the only test that holds the
// production line to it.
func TestKeywordConfigCarriesTheSponsorByte(t *testing.T) {
	for _, sponsor := range []string{
		"Télécoms Vidéotron ltée ",
		"EBOX",
		"Claro Fibra - SPO",
		"fdcservers.net",
	} {
		if got := keywordConfig(sponsor).Keyword; got != sponsor {
			t.Errorf("keywordConfig(%q).Keyword = %q, want it unchanged", sponsor, got)
		}
	}
	if keywordConfig("EBOX").UserAgent == "" {
		t.Error("the catalogue fetch needs a user agent like every other Ookla call")
	}
}

// The saved picker list holds the catalogue coordinate a starred server was
// listed with. When the live lookup fails, that snapshot still puts the
// companions beside the pin - ahead of the searched city, which is the answer
// to "no idea where the pin is", not a better one than the pin's own row.
func TestRunReasonFallsBackToTheSavedCoordinateWhenRecoveryFails(t *testing.T) {
	o := pinnedBestOf(t)
	o.SavedCoordFn = func(id string) (float64, float64, bool) {
		if id == "1993" {
			return 45.5017, -73.5673, true
		}
		return 0, 0, false
	}
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: callerLat, Lon: callerLon, Distance: 0,
	})
	stubCatalogue(t, nil, nil)
	pools := stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("e1", 1)}})
	unrecovered := stats.Lifetime().Counters["speed.pin_coord_unrecovered"]

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}
	if n := pools.fetches(); n != 0 {
		t.Errorf("the run raced %d origin pools with a saved coordinate in hand", n)
	}
	loc := registeredLocation("pinned")
	if loc == nil || loc.Lat != 45.5017 || loc.Lon != -73.5673 {
		t.Fatalf("centred on %v, want the saved coordinate 45.5017,-73.5673", loc)
	}
	if registeredLocation("auto") != nil {
		t.Error("the searched city was used although the saved list placed the pin")
	}
	if got := stats.Lifetime().Counters["speed.pin_coord_unrecovered"]; got != unrecovered {
		t.Errorf("a pin placed from the saved list was counted as unrecovered (%d -> %d)", unrecovered, got)
	}
}

// A saved row without a coordinate (starred from a by-ID reply, stored as 0,0)
// or an ID that is not on the list changes nothing: the run falls back exactly
// as it did before the list existed.
func TestASavedCoordinateOfZeroIsNoCoordinate(t *testing.T) {
	for name, fn := range map[string]func(string) (float64, float64, bool){
		"zero pair":  func(string) (float64, float64, bool) { return 0, 0, true },
		"not listed": func(string) (float64, float64, bool) { return 0, 0, false },
	} {
		o := pinnedBestOf(t)
		o.SavedCoordFn = fn
		stubPinByID(t, &ookla.Server{
			ID: "1993", Sponsor: "EBOX", Name: "Montreal",
			Lat: callerLat, Lon: callerLon, Distance: 0,
		})
		stubCatalogue(t, nil, nil)
		unrecovered := stats.Lifetime().Counters["speed.pin_coord_unrecovered"]
		if _, err := o.RunReason(context.Background(), "manual"); err != nil {
			t.Fatalf("%s: pinned best-of run: %v", name, err)
		}
		if loc := registeredLocation("auto"); loc == nil || loc.Lat != 48.85 || loc.Lon != 2.35 {
			t.Errorf("%s: centred on %v, want the searched city - the saved list had nothing usable", name, loc)
		}
		// listCentre would refuse a 0,0 pair on its own, so the centre alone
		// cannot tell whether savedCoord rejected it: the unrecovered warn and
		// its counter are what a 0,0 accepted as a coordinate would skip.
		if got := stats.Lifetime().Counters["speed.pin_coord_unrecovered"]; got != unrecovered+1 {
			t.Errorf("%s: speed.pin_coord_unrecovered went %d -> %d, want +1: a 0,0 row must count as no coordinate", name, unrecovered, got)
		}
	}
}

// A pin under best-of is a pinned run whichever row wins its round, and the
// report has to say so: the browse centring reads the winner's reason back to
// find where AUTO last landed, and a "score" winner in the pin's city would
// pass for that.
func TestPinnedBestOfRecordsAPinnedWinReason(t *testing.T) {
	for name, tc := range map[string]struct {
		pinMbps, pinPing float64
		want             string
	}{
		"the pin wins":     {200, 5, WinReasonPinnedBestOf},
		"a companion wins": {1, 50, WinReasonPinnedCompanion},
	} {
		o := pinnedBestOf(t)
		stubPinByID(t, &ookla.Server{
			ID: "1993", Sponsor: "EBOX", Name: "Montreal",
			Lat: realLat, Lon: realLon, Distance: 504.6, // a trusted position: no recovery needed
		})
		stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
			r := Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}
			if srv.ID == "1993" {
				r.DownloadMbps, r.UploadMbps, r.PingMS = tc.pinMbps, tc.pinMbps/10, tc.pinPing
			}
			return r, nil
		})
		res, err := o.RunReason(context.Background(), "manual")
		if err != nil {
			t.Fatalf("%s: pinned best-of run: %v", name, err)
		}
		if res.Selection == nil {
			t.Fatalf("%s: no selection report", name)
		}
		var win *CandidateReport
		for i := range res.Selection.Candidates {
			if res.Selection.Candidates[i].Winner {
				win = &res.Selection.Candidates[i]
			}
		}
		if win == nil {
			t.Fatalf("%s: no winner row in %+v", name, res.Selection.Candidates)
		}
		if (win.ServerID == "1993") != (tc.want == WinReasonPinnedBestOf) {
			t.Fatalf("%s: the fixture did not make the intended row win (winner %s)", name, win.ServerID)
		}
		if win.WinReason != tc.want || !PinnedRun(win.WinReason) {
			t.Errorf("%s: winner %s recorded %q, want %q", name, win.ServerID, win.WinReason, tc.want)
		}
	}
	if PinnedRun(winReasonScore) || PinnedRun(winReasonPingBoot) || PinnedRun(winReasonFastestRank) || !PinnedRun(WinReasonPinned) {
		t.Error("PinnedRun must name exactly the reasons a pin was in effect for")
	}
}
