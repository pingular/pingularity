package main

import (
	"testing"

	"github.com/pingular/pingularity/internal/netinfo"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
)

func originKinds(os []speedtest.Origin) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.Kind)
	}
	return out
}

func findOrigin(os []speedtest.Origin, kind string) (speedtest.Origin, bool) {
	for _, o := range os {
		if o.Kind == kind {
			return o, true
		}
	}
	return speedtest.Origin{}, false
}

// The connection's own cities all race. With nothing starred the enumeration
// is the three network-derived cities, all of them, with nothing
// short-circuiting the rest:
// the old cascade returned the first rung and stopped, and since the pools
// around different coordinates are disjoint, "never fetched" meant "unreachable
// at any ranking".
func TestAutoOriginsAreDerivedFromTheConnectionOnly(t *testing.T) {
	info := netinfo.Info{
		City: "Oldtown", Country: "XX", Lat: 12.345678, Lon: -76.543210,
		CFColo: "MIA",
		Exit:   &netinfo.ExitInfo{Loc: "Oldtown, XX", Lat: 12.34, Lon: -76.54},
	}

	got := autoOrigins(info, nil, nil)
	kinds := originKinds(got)
	for _, want := range []string{"exit", "isp", "geo"} {
		if _, ok := findOrigin(got, want); !ok {
			t.Errorf("origin %q missing from %v - it was never given a chance to race", want, kinds)
		}
	}
	if len(got) != 3 {
		t.Fatalf("origins = %v, want exactly the three derived cities", kinds)
	}
	if _, ok := findOrigin(got, "city"); ok {
		t.Errorf("origins = %v: a searched city was enumerated as a racer", kinds)
	}
}

// The ISP city only becomes an origin because netinfo keeps the coordinate its
// geo providers always returned. This is the enabling change: on the measured
// connection the exit router had no coordinate at all, so without this the old
// cascade fell through to the Cloudflare PoP - a building in Miami - and the
// domestic pool was unreachable.
func TestAutoOriginsIncludeTheISPCity(t *testing.T) {
	info := netinfo.Info{City: "Oldtown", Country: "XX", Lat: 12.345678, Lon: -76.543210}
	got := autoOrigins(info, nil, nil)

	o, ok := findOrigin(got, "isp")
	if !ok {
		t.Fatalf("no isp origin in %v", originKinds(got))
	}
	if o.Lat != 12.345678 || o.Lon != -76.543210 || !o.Anchored {
		t.Errorf("isp origin = %+v, want the geo provider's coordinate", o)
	}
	if o.Label != "Oldtown, XX" {
		t.Errorf("isp label = %q, want %q", o.Label, "Oldtown, XX")
	}
}

// An exit router RIPE has no coordinate for contributes nothing but must not
// remove anything either - the rung that fell through on the measured link.
func TestAutoOriginsSkipACoordinatelessExitButKeepTheRest(t *testing.T) {
	info := netinfo.Info{
		City: "Oldtown", Country: "XX", Lat: 12.34, Lon: -76.54,
		Exit: &netinfo.ExitInfo{Name: "host-203-0-113-240.example.net", RTTms: 4.6},
	}
	got := autoOrigins(info, nil, nil)
	if _, ok := findOrigin(got, "exit"); ok {
		t.Error("an exit router with no lat/lon was entered as an origin; its pool would be the Gulf of Guinea's")
	}
	for _, want := range []string{"isp", "geo"} {
		if _, ok := findOrigin(got, want); !ok {
			t.Errorf("origin %q missing from %v", want, originKinds(got))
		}
	}
}

// The geo city is always offered: it is the pool the Ookla API itself returns
// for our source address, and it is what the race degrades to when every
// coordinate we have is missing or wrong.
func TestAutoOriginsAlwaysOfferTheGeoCity(t *testing.T) {
	got := autoOrigins(netinfo.Info{}, nil, nil)
	if len(got) != 1 || got[0].Kind != "geo" || got[0].Anchored {
		t.Fatalf("origins with nothing known = %+v, want just the unanchored \"geo\"", got)
	}
}

// A starred server's city races too. The measured failure: exit discovery down
// for an evening (the host resolver timing out on the ASN zone), so the field
// was the ISP geolocation and Ookla's placement - both Toronto - while every
// starred server, and every faster one, sat in Montreal. With the star as an
// origin the Montreal pool is in the race whatever the connection says, and the
// on-net server keeps its ISP lane inside it.
func TestAutoOriginsRaceTheStarredServersCities(t *testing.T) {
	info := netinfo.Info{City: "Toronto", Country: "CA", Lat: 43.70, Lon: -79.40}
	saved := []settings.SavedServer{
		{ID: "1993", Sponsor: "EBOX", Name: "Montréal, QC", Lat: 45.5, Lon: -73.5},
		{ID: "7", Sponsor: "ByID", Name: "Nowhere"}, // starred from a by-ID reply: no coordinate, nothing to race
	}
	got := autoOrigins(info, saved, nil)
	if kinds := originKinds(got); len(kinds) != 3 || kinds[0] != "isp" || kinds[1] != "saved" || kinds[2] != "geo" {
		t.Fatalf("origins = %v, want isp, saved, geo - the star between the measured cities and Ookla's guess", kinds)
	}
	o, _ := findOrigin(got, "saved")
	if o.Lat != 45.5 || o.Lon != -73.5 || !o.Anchored || o.Label != "Montréal, QC" {
		t.Errorf("saved origin = %+v, want the starred server's catalogue coordinate, anchored, named by its city", o)
	}
	// Exit known and elsewhere: the star still races, after the measured cities.
	info.Exit = &netinfo.ExitInfo{Loc: "Toronto, CA", Lat: 43.65, Lon: -79.38}
	if kinds := originKinds(autoOrigins(info, saved, nil)); len(kinds) != 4 || kinds[2] != "saved" {
		t.Errorf("origins with an exit = %v, want exit, isp, saved, geo", kinds)
	}
}

// The last race's winner is entered again: after the stars, before geo, and
// only when it carries a coordinate. It is one candidate among the others -
// pickOrigins folds it into an origin already naming that city.
func TestAutoOriginsEnterTheLastRacesWinner(t *testing.T) {
	info := netinfo.Info{Lat: 43.65, Lon: -79.38, City: "Toronto", Country: "CA"}
	saved := []settings.SavedServer{{ID: "1993", Name: "Montréal, QC", Lat: 45.5, Lon: -73.57}}
	recent := &speedtest.Origin{Kind: "recent", Label: "Montréal, CA", Lat: 45.5017, Lon: -73.5673, Anchored: true}
	kinds := originKinds(autoOrigins(info, saved, recent))
	want := []string{"isp", "saved", "recent", "geo"}
	if len(kinds) != len(want) {
		t.Fatalf("kinds %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds %v, want %v", kinds, want)
		}
	}
	if kinds := originKinds(autoOrigins(info, nil, nil)); len(kinds) != 2 {
		t.Errorf("no recent winner: %v, want isp and geo only", kinds)
	}
	if kinds := originKinds(autoOrigins(info, nil, &speedtest.Origin{Kind: "recent", Anchored: true})); len(kinds) != 2 {
		t.Errorf("a recent winner with no coordinate must not be entered: %v", kinds)
	}
	// (Entered on a coordinate another origin already names, the race's own
	// pickOrigins folds it into that one - pinned by the speedtest package's
	// pickOrigins tests - so on an ordinary day it costs nothing.)
}
