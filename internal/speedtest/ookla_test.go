package speedtest

import (
	"errors"
	"strconv"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// serverCoord parses Ookla's string lat/lon and rejects a nil, blank, or
// malformed pair (and the 0,0 null-island placeholder) so a run centres on
// something usable instead of the Gulf of Guinea.
func TestServerCoord(t *testing.T) {
	if _, _, ok := serverCoord(nil); ok {
		t.Error("nil server must not yield a coordinate")
	}
	if lat, lon, ok := serverCoord(&ookla.Server{Lat: " 45.5 ", Lon: "-73.6"}); !ok || lat != 45.5 || lon != -73.6 {
		t.Errorf("serverCoord good pair = %v,%v,%v, want 45.5,-73.6,true", lat, lon, ok)
	}
	if _, _, ok := serverCoord(&ookla.Server{Lat: "", Lon: "-73.6"}); ok {
		t.Error("blank lat must be rejected")
	}
	if _, _, ok := serverCoord(&ookla.Server{Lat: "north", Lon: "-73.6"}); ok {
		t.Error("malformed lat must be rejected")
	}
	if _, _, ok := serverCoord(&ookla.Server{Lat: "0", Lon: "0"}); ok {
		t.Error("0,0 null-island placeholder must be rejected")
	}
}

// serverLabel joins the sponsor and city Ookla advertises, the human name shown
// in the runs table.
func TestServerLabel(t *testing.T) {
	if got := serverLabel(&ookla.Server{Sponsor: "EBOX", Name: "Montreal"}); got != "EBOX, Montreal" {
		t.Errorf("serverLabel = %q, want %q", got, "EBOX, Montreal")
	}
}

// naErr turns the library's -1 "N/A" sentinel into errMeasurementNA - so retries
// engage and a failed transfer can't pass through as a bogus ~0 reading - while a
// real zero or positive rate stays a valid measurement.
func TestNaErr(t *testing.T) {
	if err := naErr(-1); !errors.Is(err, errMeasurementNA) {
		t.Errorf("naErr(-1) = %v, want errMeasurementNA", err)
	}
	if err := naErr(0); err != nil {
		t.Errorf("naErr(0) = %v, want nil (a measured zero is a real reading)", err)
	}
	if err := naErr(812.5); err != nil {
		t.Errorf("naErr(812.5) = %v, want nil", err)
	}
}

// Metro geography: Ookla pins most city servers to the same coordinate, so the
// candidate cut must cover the whole distance-tie band (not an arbitrary first
// five of it) and prefer sponsor diversity when trimming to the cap - the
// user's own ISP's on-net server must make the race.
func TestAutoCandidates(t *testing.T) {
	mk := func(id int, sponsor string, km float64) *ookla.Server {
		return &ookla.Server{ID: strconv.Itoa(id), Sponsor: sponsor, Distance: km}
	}
	sponsors := func(list ookla.Servers) []string {
		var out []string
		for _, s := range list {
			out = append(out, s.Sponsor)
		}
		return out
	}

	// A 15-way tie at 1.0 km with duplicate sponsors; the 11th unique sponsor
	// (the user's ISP) must be raced, and dupes must not crowd uniques out.
	tie := ookla.Servers{
		mk(1, "Cooptel", 1), mk(2, "Netcrawler", 1), mk(3, "Tata", 1),
		mk(4, "Bell", 1), mk(5, "Rogers", 1), mk(6, "TELUS", 1),
		mk(7, "TELUS", 1), mk(8, "Bell", 1), mk(9, "Beanfield", 1),
		mk(10, "Rogers", 1), mk(11, "EBOX", 1), mk(12, "Cronomagic", 1),
		mk(13, "Fibrenoire", 1), mk(14, "Connexio", 1), mk(15, "Vif", 1),
	}
	got := autoCandidates(tie, "")
	if len(got) != autoPingMax {
		t.Fatalf("tie band: %d candidates, want the cap %d", len(got), autoPingMax)
	}
	found := map[string]bool{}
	for _, s := range got {
		found[s.Sponsor] = true
	}
	if !found["EBOX"] {
		t.Errorf("the 11th unique sponsor (the user's ISP) missed the race: %v", sponsors(got))
	}
	if len(found) != autoPingMax {
		t.Errorf("dupes crowded out unique sponsors: %d unique of %d, %v", len(found), autoPingMax, sponsors(got))
	}

	// Fewer uniques than the cap: room left is filled with nearest duplicates.
	few := ookla.Servers{
		mk(1, "A", 1), mk(2, "A", 1), mk(3, "B", 1), mk(4, "A", 1),
		mk(5, "B", 1), mk(6, "C", 1), mk(7, "C", 1), mk(8, "A", 1),
		mk(9, "B", 1), mk(10, "C", 1), mk(11, "A", 1), mk(12, "B", 1),
		mk(13, "C", 1), mk(14, "A", 1),
	}
	if got := autoCandidates(few, ""); len(got) != autoPingMax {
		t.Errorf("dupe fill: %d candidates, want %d", len(got), autoPingMax)
	}

	// Rural spread: only the nearest is within the margin, but the floor keeps
	// the race at the old nearest-5.
	rural := ookla.Servers{
		mk(1, "A", 40), mk(2, "B", 90), mk(3, "C", 120),
		mk(4, "D", 130), mk(5, "E", 140), mk(6, "F", 200),
	}
	got = autoCandidates(rural, "")
	if len(got) != autoPingMin {
		t.Fatalf("rural: %d candidates, want the floor %d", len(got), autoPingMin)
	}
	if got[len(got)-1].Sponsor != "E" {
		t.Errorf("rural floor must be the nearest %d in distance order, got %v", autoPingMin, sponsors(got))
	}

	// A suburb inside the margin of the nearest joins the race beyond the floor.
	band := ookla.Servers{
		mk(1, "A", 2), mk(2, "B", 3), mk(3, "C", 5), mk(4, "D", 8),
		mk(5, "E", 12), mk(6, "F", 26), mk(7, "G", 80),
	}
	got = autoCandidates(band, "")
	if len(got) != 6 || got[5].Sponsor != "F" {
		t.Errorf("margin band: want 6 candidates ending at F (26 km <= 2+25), got %v", sponsors(got))
	}

	// Tiny lists pass through whole.
	if got := autoCandidates(ookla.Servers{mk(1, "A", 1)}, ""); len(got) != 1 {
		t.Errorf("single server: got %d candidates", len(got))
	}
	if got := autoCandidates(nil, ""); len(got) != 0 {
		t.Errorf("empty list: got %d candidates", len(got))
	}
}

// The user's own ISP's server must ALWAYS make the ping race when Ookla lists
// one nearby: it is the most likely winner, and neither the sponsor-diversity
// cap nor the distance margin may cut it. It gets a lane, not a win.
func TestAutoCandidatesISPGuarantee(t *testing.T) {
	mk := func(id int, sponsor string, km float64) *ookla.Server {
		return &ookla.Server{ID: strconv.Itoa(id), Sponsor: sponsor, Distance: km}
	}
	hasSponsor := func(list ookla.Servers, want string) bool {
		for _, s := range list {
			if s.Sponsor == want {
				return true
			}
		}
		return false
	}

	// 14 unique sponsors in one tie band; the ISP's server sits 14th, past the
	// 12-lane cap - without the guarantee it would be cut.
	var tie ookla.Servers
	for i, sp := range []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9", "S10", "S11", "S12", "S13", "EBOX"} {
		tie = append(tie, mk(i+1, sp, 1))
	}
	got := autoCandidates(tie, "AS1403 EBOX - EBOX")
	if len(got) != autoPingMax {
		t.Fatalf("cap must hold: %d candidates, want %d", len(got), autoPingMax)
	}
	if !hasSponsor(got, "EBOX") {
		t.Errorf("ISP server missing from the race despite the guarantee")
	}
	// Without the ISP name, the same list cuts it (the pre-guarantee behavior).
	// A hard error, not a skip: t.Skip would abort the whole test and silently
	// drop every assertion below it, disarming the guarantees they police.
	if hasSponsor(autoCandidates(tie, ""), "EBOX") {
		t.Error("fixture no longer exercises the cap: EBOX survives without the ISP name; adjust the tie band")
	}

	// ISP server outside the distance margin (geolocation drift): still raced.
	drift := ookla.Servers{
		mk(1, "S1", 1), mk(2, "S2", 1), mk(3, "S3", 1), mk(4, "S4", 1),
		mk(5, "S5", 1), mk(6, "EBOX", 60),
	}
	if got := autoCandidates(drift, "AS1403 EBOX - EBOX"); !hasSponsor(got, "EBOX") {
		t.Errorf("ISP server beyond the margin must still be raced")
	}

	// No matching sponsor anywhere: the guarantee must not fire - the set is
	// exactly the 5 nearest, with the 60km EBOX left out.
	got = autoCandidates(drift, "AS0000 Some Other ISP")
	if len(got) != autoPingMin {
		t.Errorf("non-matching ISP changed the candidate count: %d, want %d", len(got), autoPingMin)
	}
	if hasSponsor(got, "EBOX") {
		t.Error("guarantee fired for a non-matching ISP")
	}

	// Already-included ISP server: no duplicate added, cap intact.
	included := ookla.Servers{mk(1, "EBOX", 1), mk(2, "S2", 1), mk(3, "S3", 1)}
	got = autoCandidates(included, "AS1403 EBOX - EBOX")
	count := 0
	for _, s := range got {
		if s.Sponsor == "EBOX" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ISP server duplicated in the race: %d entries", count)
	}
}

// Word-level ISP matching: real provider shapes must match, generic industry
// words must not create false positives.
func TestSponsorMatchesISP(t *testing.T) {
	cases := []struct {
		sponsor, isp string
		want         bool
	}{
		{"EBOX", "AS1403 EBOX - EBOX", true},
		{"Bell Canada", "Bell Canada", true},
		{"TELUS Mobility", "AS852 TELUS Communications", true},
		{"Rogers Wireless", "AS1403 EBOX - EBOX", false},
		{"Internet Services", "AS1234 Fancy Internet Services", false}, // generic-only words never match
		{"Videotron", "AS5769 Videotron Ltee", true},
		{"Cogeco", "", false},
		{"", "AS1403 EBOX - EBOX", false},
	}
	for _, c := range cases {
		if got := sponsorMatchesISP(c.sponsor, c.isp); got != c.want {
			t.Errorf("sponsorMatchesISP(%q, %q) = %v, want %v", c.sponsor, c.isp, got, c.want)
		}
	}
}

// When the user's ISP sponsors several nearby servers, they ALL race (up to
// autoISPMax) - on-net boxes differ in load and each is a likely winner - while
// other providers still keep their diversity lanes.
func TestAutoCandidatesISPMultipleLanes(t *testing.T) {
	mk := func(id int, sponsor string, km float64) *ookla.Server {
		return &ookla.Server{ID: strconv.Itoa(id), Sponsor: sponsor, Distance: km}
	}
	countISP := func(list ookla.Servers) int {
		n := 0
		for _, s := range list {
			if s.Sponsor == "EBOX" {
				n++
			}
		}
		return n
	}

	// 3 EBOX entries scattered through a 16-server tie band: all 3 race.
	var band ookla.Servers
	sponsors := []string{"S1", "EBOX", "S2", "S3", "S4", "EBOX", "S5", "S6", "S7", "S8", "EBOX", "S9", "S10", "S11", "S12", "S13"}
	for i, sp := range sponsors {
		band = append(band, mk(i+1, sp, 1))
	}
	got := autoCandidates(band, "AS1403 EBOX - EBOX")
	if len(got) != autoPingMax {
		t.Fatalf("cap must hold: %d, want %d", len(got), autoPingMax)
	}
	if n := countISP(got); n != 3 {
		t.Errorf("all 3 ISP servers should race, got %d lanes", n)
	}

	// 6 EBOX entries: capped at autoISPMax so diversity survives.
	var many ookla.Servers
	for i := 0; i < 6; i++ {
		many = append(many, mk(i+1, "EBOX", 1))
	}
	for i := 0; i < 10; i++ {
		many = append(many, mk(100+i, "S"+strconv.Itoa(i), 1))
	}
	got = autoCandidates(many, "AS1403 EBOX - EBOX")
	if n := countISP(got); n != autoISPMax {
		t.Errorf("ISP lanes must cap at %d, got %d", autoISPMax, n)
	}
	uniqueOthers := map[string]bool{}
	for _, s := range got {
		if s.Sponsor != "EBOX" {
			uniqueOthers[s.Sponsor] = true
		}
	}
	if len(uniqueOthers) != autoPingMax-autoISPMax {
		t.Errorf("other providers should fill the remaining %d lanes uniquely, got %d", autoPingMax-autoISPMax, len(uniqueOthers))
	}

	// Under the cap nothing is trimmed: every EBOX entry races regardless.
	small := ookla.Servers{mk(1, "EBOX", 1), mk(2, "EBOX", 1), mk(3, "S1", 1)}
	if n := countISP(autoCandidates(small, "AS1403 EBOX - EBOX")); n != 2 {
		t.Errorf("small pool: both ISP entries should race, got %d", n)
	}
}
