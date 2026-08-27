package speedtest

import (
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A listing is read fastest-first: answered servers by ping, then the ones
// that did not answer, by distance. The library's PingTimeout sentinel and a
// zero both mean "no ping".
func TestSortByPingAnsweredFirstThenDistance(t *testing.T) {
	ms := func(v float64) *float64 { return &v }
	out := []ServerInfo{
		{ID: "far-silent", DistanceKM: 300},
		{ID: "slow", DistanceKM: 1, PingMS: ms(27)},
		{ID: "near-silent", DistanceKM: 2},
		{ID: "fast", DistanceKM: 50, PingMS: ms(10)},
		{ID: "fast-too", DistanceKM: 20, PingMS: ms(10)},
	}
	sortByPing(out)
	var ids []string
	for _, s := range out {
		ids = append(ids, s.ID)
	}
	want := []string{"fast-too", "fast", "slow", "near-silent", "far-silent"}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("order = %v, want %v", ids, want)
		}
	}
	if pingMS(ookla.PingTimeout) != nil || pingMS(0) != nil {
		t.Error("the library's timeout sentinel and zero must both read as no ping")
	}
	if p := pingMS(12500 * time.Microsecond); p == nil || *p != 12.5 {
		t.Errorf("pingMS(12.5ms) = %v", p)
	}
}

// A centred listing measures each row's distance from the centre itself: the
// catalogue's figure is whole kilometres, so a city's own servers all read 0.
func TestListInfosMeasureDistanceFromTheCentre(t *testing.T) {
	near := &ookla.Server{ID: "n", Lat: "45.5040", Lon: "-73.5700", Distance: 0} // ~0.1 km: the catalogue rounds it to 0
	far := &ookla.Server{ID: "f", Lat: "43.65", Lon: "-79.38", Distance: 504}
	none := &ookla.Server{ID: "x", Distance: 7} // no coordinate: keeps the catalogue's figure
	out := listInfos(ookla.Servers{near, far, none}, 45.5032, -73.5698)
	if d := out[0].DistanceKM; d <= 0 || d >= 0.5 {
		t.Errorf("near: %.3f km, want a real fraction of a kilometre, not the catalogue's 0", d)
	}
	if d := out[1].DistanceKM; d < 480 || d > 520 {
		t.Errorf("far: %.1f km, want ~500", d)
	}
	if out[2].DistanceKM != 7 {
		t.Errorf("no coordinate: %v, want the catalogue's 7", out[2].DistanceKM)
	}
	if out := listInfos(ookla.Servers{near}, 0, 0); out[0].DistanceKM != 0 {
		t.Errorf("uncentred: %v, want the catalogue's figure untouched", out[0].DistanceKM)
	}
}
