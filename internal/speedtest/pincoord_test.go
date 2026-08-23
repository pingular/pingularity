package speedtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
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
	o.BestOfFn = func() bool { return true }
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

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}

	loc := registeredLocation("pinned")
	if loc == nil {
		t.Fatal("the run centred on nothing; the pin's own coordinate never reached the fetch")
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

// Recovery must not reach past the pin. A run with a searched city already has
// a centre to fall back on, so a pin whose coordinate cannot be recovered
// centres on the city and races nothing - unchanged from what a pin with no
// coordinate at all has always done.
func TestRunReasonFallsBackToTheSearchedCityWhenRecoveryFails(t *testing.T) {
	o := pinnedBestOf(t)
	o.AutoLocFn = func() (float64, float64, bool) { return 48.85, 2.35, true }
	stubPinByID(t, &ookla.Server{
		ID: "1993", Sponsor: "EBOX", Name: "Montreal",
		Lat: callerLat, Lon: callerLon, Distance: 0,
	})
	stubCatalogue(t, nil, nil)
	pools := stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("e1", 1)}})

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned best-of run: %v", err)
	}
	if n := pools.fetches(); n != 0 {
		t.Errorf("the run raced %d origin pools; a searched city is a stated centre and overrides the race", n)
	}
	loc := registeredLocation("auto")
	if loc == nil || loc.Lat != 48.85 || loc.Lon != 2.35 {
		t.Fatalf("centred on %v, want the searched city 48.85,2.35", loc)
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
			o.BestOfFn = func() bool { return false }
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
