package speedtest

import (
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

const pinLat, pinLon = 45.50, -73.57 // Montreal: the server the user pinned

func pinAt(lat, lon string) *ookla.Server {
	return &ookla.Server{ID: "1993", Lat: lat, Lon: lon, Sponsor: "EBOX", Name: "Montreal"}
}

// A pinned server plus best-of centres on the PIN, not the exit. Centring on the
// exit made the pin nearly pointless: the winner is chosen on throughput alone,
// so exit-local servers out-run the pin on most rounds and get stored in its
// place, which is just Auto with one extra racer.
func TestListCentreUsesPinnedServerForBestOf(t *testing.T) {
	lat, lon, label, ok := listCentre("1993", 3, pinAt("45.50", "-73.57"))
	if !ok {
		t.Fatal("no centre chosen for a pinned best-of run")
	}
	if lat != pinLat || lon != pinLon {
		t.Errorf("centred on %v,%v want the pinned server %v,%v", lat, lon, pinLat, pinLon)
	}
	if label != "pinned" {
		t.Errorf("label = %q, want %q", label, "pinned")
	}
}

// Nothing pinned: no centre of its own - the caller races the candidate
// cities instead (see raceCities).
func TestListCentreYieldsNothingWhenNothingPinned(t *testing.T) {
	for _, want := range []int{1, 3} {
		if _, _, _, ok := listCentre("", want, nil); ok {
			t.Errorf("want=%d: chose a centre with nothing pinned; the race decides that", want)
		}
	}
}

// A pin without best-of has nothing to centre - it is the only target, and the
// list it came from is irrelevant.
func TestListCentreSkipsCentringForLonePin(t *testing.T) {
	if _, _, _, ok := listCentre("1993", 1, pinAt("45.50", "-73.57")); ok {
		t.Error("centred a single-server pinned run; nothing should be centred")
	}
}

// An unusable coordinate on the pin yields no centre: the caller races the
// candidate cities for one rather than letting the library guess from the
// public IP.
func TestListCentreYieldsNothingOnBadPinCoord(t *testing.T) {
	for _, s := range []*ookla.Server{
		pinAt("", ""),
		pinAt("not-a-number", "-73.57"),
		pinAt("0", "0"),
		nil,
	} {
		if _, _, _, ok := listCentre("1993", 3, s); ok {
			t.Errorf("bad pin coord %+v: chose a centre; the race decides that", s)
		}
	}
}
