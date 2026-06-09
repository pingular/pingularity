package speedtest

import (
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The exit and the pinned server sit in different cities on purpose - that is
// the whole case under test.
const (
	exitLat, exitLon = 43.65, -79.38 // Toronto: where the connection leaves the ISP
	pinLat, pinLon   = 45.50, -73.57 // Montreal: the server the user pinned
)

func exitFn() (float64, float64, bool) { return exitLat, exitLon, true }

func pinAt(lat, lon string) *ookla.Server {
	return &ookla.Server{ID: "1993", Lat: lat, Lon: lon, Sponsor: "EBOX", Name: "Montreal"}
}

// A pinned server plus best-of centres on the PIN, not the exit. Centring on the
// exit made the pin nearly pointless: the winner is chosen on throughput alone,
// so exit-local servers out-run the pin on most rounds and get stored in its
// place, which is just Auto with one extra racer.
func TestListCentreUsesPinnedServerForBestOf(t *testing.T) {
	lat, lon, label, ok := listCentre("1993", 3, pinAt("45.50", "-73.57"), exitFn)
	if !ok {
		t.Fatal("no centre chosen for a pinned best-of run")
	}
	if lat != pinLat || lon != pinLon {
		t.Errorf("centred on %v,%v want the pinned server %v,%v", lat, lon, pinLat, pinLon)
	}
	if lat == exitLat && lon == exitLon {
		t.Error("centred on the exit; companions would come from the wrong city")
	}
	if label != "pinned" {
		t.Errorf("label = %q, want %q", label, "pinned")
	}
}

// Nothing pinned is unchanged: the auto location still centres the list.
func TestListCentreUsesAutoWhenNothingPinned(t *testing.T) {
	for _, want := range []int{1, 3} {
		lat, lon, label, ok := listCentre("", want, nil, exitFn)
		if !ok || lat != exitLat || lon != exitLon || label != "auto" {
			t.Errorf("want=%d: got %v,%v %q ok=%v, want the exit as %q", want, lat, lon, label, ok, "auto")
		}
	}
}

// A pin without best-of has nothing to centre - it is the only target, and the
// list it came from is irrelevant.
func TestListCentreSkipsCentringForLonePin(t *testing.T) {
	if _, _, _, ok := listCentre("1993", 1, pinAt("45.50", "-73.57"), exitFn); ok {
		t.Error("centred a single-server pinned run; nothing should be centred")
	}
}

// An unusable coordinate on the pin falls back to the auto location rather than
// letting the library guess from the public IP.
func TestListCentreFallsBackToAutoOnBadPinCoord(t *testing.T) {
	for _, s := range []*ookla.Server{
		pinAt("", ""),
		pinAt("not-a-number", "-73.57"),
		pinAt("0", "0"),
		nil,
	} {
		lat, lon, label, ok := listCentre("1993", 3, s, exitFn)
		if !ok || lat != exitLat || lon != exitLon || label != "auto" {
			t.Errorf("bad pin coord: got %v,%v %q ok=%v, want the exit as %q", lat, lon, label, ok, "auto")
		}
	}
}

// No pin and no auto location: the library centres on the public IP itself.
func TestListCentreYieldsNothingWithoutAnAutoLocation(t *testing.T) {
	if _, _, _, ok := listCentre("", 3, nil, nil); ok {
		t.Error("chose a centre with no auto location available")
	}
	none := func() (float64, float64, bool) { return 0, 0, false }
	if _, _, _, ok := listCentre("", 3, nil, none); ok {
		t.Error("chose a centre when the auto location reported nothing")
	}
}
