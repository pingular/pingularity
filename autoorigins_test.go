package main

import (
	"testing"

	"github.com/pingular/pingularity/internal/netinfo"
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

// EVERY origin is derived from the connection, and a searched city is NOT one -
// it cannot be, because autoOrigins is never given one (the city reaches the
// picker through AutoLocFn and bypasses the race). The enumeration is the three
// network-derived cities, all of them, with nothing short-circuiting the rest:
// the old cascade returned the first rung and stopped, and since the pools
// around different coordinates are disjoint, "never fetched" meant "unreachable
// at any ranking".
func TestAutoOriginsAreDerivedFromTheConnectionOnly(t *testing.T) {
	info := netinfo.Info{
		City: "Oldtown", Country: "XX", Lat: 12.345678, Lon: -76.543210,
		CFColo: "MIA",
		Exit:   &netinfo.ExitInfo{Loc: "Oldtown, XX", Lat: 12.34, Lon: -76.54},
	}

	got := autoOrigins(info)
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
	got := autoOrigins(info)

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
	got := autoOrigins(info)
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
	got := autoOrigins(netinfo.Info{})
	if len(got) != 1 || got[0].Kind != "geo" || got[0].Anchored {
		t.Fatalf("origins with nothing known = %+v, want just the unanchored \"geo\"", got)
	}
}
