package speedtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// healthyEndpoints makes every fallback probe answer "usable" without a
// network, and records who was probed.
func healthyEndpoints(t *testing.T) *rankContacts {
	t.Helper()
	c := &rankContacts{pinged: map[string]bool{}, probed: map[string]bool{}}
	old := fallbackHealth
	fallbackHealth = func(_ context.Context, s *ookla.Server) endpointState {
		c.mu.Lock()
		c.probed[s.ID] = true
		c.mu.Unlock()
		return endpointOK
	}
	t.Cleanup(func() { fallbackHealth = old })
	return c
}

// countingPing answers every ranking ping at the given latency and records
// which servers were pinged.
func countingPing(t *testing.T, lat map[string]time.Duration) *rankContacts {
	t.Helper()
	c := &rankContacts{pinged: map[string]bool{}, probed: map[string]bool{}}
	old := ooklaPing
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		c.mu.Lock()
		c.pinged[s.ID] = true
		c.mu.Unlock()
		l, ok := lat[s.ID]
		if !ok {
			return errors.New("unreachable")
		}
		cb(l)
		s.Latency = l
		return nil
	}
	t.Cleanup(func() { ooklaPing = old })
	return c
}

// A server the city race already pinged is ranked on the race's own floor and
// not pinged again - including one the race could not reach, which ranks
// unanswered rather than getting a second chance nobody else gets. The
// fallback probe still runs for it: the race judges nothing about the bundle.
func TestRankedServersReuseTheRacesPings(t *testing.T) {
	health := healthyEndpoints(t)
	pings := countingPing(t, map[string]time.Duration{"C": 20 * time.Millisecond})
	servers := ookla.Servers{srv("A", 1), srv("B", 1), srv("C", 1)}
	raced := map[string]time.Duration{"A": 9 * time.Millisecond, "B": 0}
	out, rank, _, _ := rankedServersRaced(context.Background(), servers, "", raced, false, nil, 1)
	if len(out) != 3 || out[0].ID != "A" || out[1].ID != "C" || out[2].ID != "B" {
		t.Fatalf("order %v, want A (raced 9ms), C (pinged 20ms), B (raced, unanswered)", fbIDs(out))
	}
	if pings.contacted("A") || pings.contacted("B") {
		t.Error("a raced server was pinged again; the race's pings are the ranking")
	}
	if !pings.contacted("C") {
		t.Error("the server the race never reached must still be pinged")
	}
	if rank["A"] == nil || *rank["A"] != 9 || rank["B"] != nil || rank["C"] == nil || *rank["C"] != 20 {
		t.Errorf("ranking pings %v %v %v, want 9, nil, 20", f64v(rank["A"]), rank["B"], f64v(rank["C"]))
	}
	for _, id := range []string{"A", "B", "C"} {
		if !health.contacted(id) {
			t.Errorf("%s: the fallback probe must run whether or not the race pinged it", id)
		}
	}
}

// The ranking ping is the FLOOR of the probes, the statistic the race and the
// winner decision use, not the library's mean: one stalled probe among nine
// fast ones must not hand the run to a stranger.
func TestRankedServersRankOnTheFloorNotTheMean(t *testing.T) {
	healthyEndpoints(t)
	old := ooklaPing
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		switch s.ID {
		case "stalled-once": // nine 9ms samples and one 225ms stall: mean ~30ms, floor 9ms
			for i := 0; i < 9; i++ {
				cb(9 * time.Millisecond)
			}
			cb(225 * time.Millisecond)
			s.Latency = 30600 * time.Microsecond
		case "steady": // ten 12ms samples
			for i := 0; i < 10; i++ {
				cb(12 * time.Millisecond)
			}
			s.Latency = 12 * time.Millisecond
		}
		return nil
	}
	t.Cleanup(func() { ooklaPing = old })
	out, rank, _, _ := rankedServers(context.Background(), ookla.Servers{srv("steady", 1), srv("stalled-once", 1)}, "")
	if out[0].ID != "stalled-once" {
		t.Fatalf("ranked %v; the server with the lower FLOOR (9ms) must lead the steadier one (12ms), the mean (30ms) is not the ranking", fbIDs(out))
	}
	if rank["stalled-once"] == nil || *rank["stalled-once"] != 9 {
		t.Errorf("recorded ranking ping %v, want the floor 9", f64v(rank["stalled-once"]))
	}
}

func TestPromoteIncumbent(t *testing.T) {
	ms := func(id string, m float64, sponsor string) *ookla.Server {
		s := srv(id, 1)
		s.Latency = time.Duration(m * float64(time.Millisecond))
		if sponsor != "" {
			s.Sponsor = sponsor
		}
		return s
	}
	cases := []struct {
		name      string
		ranked    ookla.Servers
		incumbent string
		isp       string
		wantLead  string
		wantOrder []string
	}{
		{"incumbent within 2ms is kept", ookla.Servers{ms("A", 9, ""), ms("B", 10.5, "")}, "B", "", winReasonIncumbent, []string{"B", "A"}},
		{"incumbent within 15% is kept", ookla.Servers{ms("A", 40, ""), ms("B", 45, "")}, "B", "", winReasonIncumbent, []string{"B", "A"}},
		{"incumbent outside the band loses its seat", ookla.Servers{ms("A", 9, ""), ms("B", 11.5, "")}, "B", "", "", []string{"A", "B"}},
		{"incumbent already fastest is plainly fastest_ranked", ookla.Servers{ms("A", 9, ""), ms("B", 10, "")}, "A", "", "", []string{"A", "B"}},
		{"incumbent that never answered is not promoted", ookla.Servers{ms("A", 9, ""), ms("B", 0, "")}, "B", "", "", []string{"A", "B"}},
		{"incumbent not in the field: nothing to keep", ookla.Servers{ms("A", 9, ""), ms("B", 10, "")}, "Z", "", "", []string{"A", "B"}},
		{"no incumbent: the ISP's box within the band leads", ookla.Servers{ms("A", 9, "Bell"), ms("B", 10.5, "EBOX")}, "", "EBOX - EBOX", winReasonOnNet, []string{"B", "A"}},
		{"the ISP's box outside the band does not", ookla.Servers{ms("A", 9, "Bell"), ms("B", 12, "EBOX")}, "", "EBOX - EBOX", "", []string{"A", "B"}},
		{"incumbent beats the on-net tie-break", ookla.Servers{ms("A", 9, "Bell"), ms("B", 10, "EBOX"), ms("C", 10.5, "Vidéotron")}, "C", "EBOX - EBOX", winReasonIncumbent, []string{"C", "A", "B"}},
		{"nothing answered: ranking stands", ookla.Servers{ms("A", 0, ""), ms("B", 0, "")}, "B", "", "", []string{"A", "B"}},
		{"one server: nothing to promote", ookla.Servers{ms("A", 9, "")}, "A", "", "", []string{"A"}},
	}
	for _, c := range cases {
		out, _, lead := promoteIncumbent(c.ranked, c.incumbent, c.isp)
		if lead != c.wantLead {
			t.Errorf("%s: lead %q, want %q", c.name, lead, c.wantLead)
		}
		got := fbIDs(out)
		if len(got) != len(c.wantOrder) {
			t.Errorf("%s: order %v, want %v", c.name, got, c.wantOrder)
			continue
		}
		for i := range got {
			if got[i] != c.wantOrder[i] {
				t.Errorf("%s: order %v, want %v", c.name, got, c.wantOrder)
				break
			}
		}
	}
	if m := rankMargin(9 * time.Millisecond); m != 2*time.Millisecond {
		t.Errorf("margin at 9ms = %v, want the 2ms floor", m)
	}
	if m := rankMargin(40 * time.Millisecond); m != 6*time.Millisecond {
		t.Errorf("margin at 40ms = %v, want 15%% = 6ms", m)
	}
}

// pickServers leads with the promoted server and says why, so the run's win
// reason can carry it; under a pin nothing is promoted.
func TestPickServersLeadsWithTheIncumbent(t *testing.T) {
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{"A": 9 * time.Millisecond, "B": 10 * time.Millisecond})
	o := NewOokla()
	o.IncumbentFn = func() string { return "B" }
	targets, sel, lead, err := o.pickServers(context.Background(), nil, ookla.Servers{srv("A", 1), srv("B", 1)}, "", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].ID != "B" || lead != winReasonIncumbent {
		t.Fatalf("targets %v lead %q; want the incumbent B leading with reason %q", fbIDs(targets), lead, winReasonIncumbent)
	}
	// The report keeps the PING order and marks the promoted row as the one
	// measured: "#1 A 9ms, #2 B 10ms (selected, incumbent)".
	if sel[0].ServerID != "A" || sel[0].Selected || sel[0].RankOrder != 1 ||
		sel[1].ServerID != "B" || !sel[1].Selected || sel[1].RankOrder != 2 {
		t.Errorf("the selection report must keep ping order and mark the promoted row: %+v", sel)
	}
	o.IncumbentFn = func() string { return "A" }
	targets, _, lead, _ = o.pickServers(context.Background(), nil, ookla.Servers{srv("A", 1), srv("B", 1)}, "", 1, nil)
	if targets[0].ID != "A" || lead != "" {
		t.Errorf("an incumbent that is also the fastest leads as fastest_ranked, got %v %q", fbIDs(targets), lead)
	}
}

// runRace's verdict names the outcome, the field and the winner - the record
// the speed row carries - and hands the run the winner's list and pings.
func TestRunRaceRecordsAVerdictAndHandsOnItsField(t *testing.T) {
	origins := []Origin{
		{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
		{Kind: "isp", Label: "Oldtown", Lat: 12.34, Lon: -76.54, Anchored: true},
		{Kind: "geo", Label: "your connection"},
	}
	stubOriginPools(t, map[string]ookla.Servers{
		"exit": {srv("e1", 1), srv("e2", 1)},
		"isp":  {srv("i1", 1), srv("i2", 30)},
		"geo":  {srv("g1", 1)},
	})
	stubRacePing(t, map[string]int{"e1": 30, "e2": 12, "i1": 4})
	o := NewOokla()
	r := o.runRace(context.Background(), origins, cityPoolSize)
	if !r.OK || r.Origin.Kind != "isp" {
		t.Fatalf("winner %+v ok=%v, want isp", r.Origin, r.OK)
	}
	v := r.Verdict
	if v.Outcome != RaceDecided || v.WinnerKind != "isp" || v.WinnerLabel != "Oldtown" || v.WinnerMS == nil || *v.WinnerMS != 4 || v.Racers != 5 {
		t.Errorf("verdict %+v", v)
	}
	if v.WinnerLat != 12.34 || v.WinnerLon != -76.54 {
		t.Errorf("the verdict must carry the winner's coordinate (12.34,-76.54) so the next race can enter it: %v,%v", v.WinnerLat, v.WinnerLon)
	}
	if v.Origins != "exit:Miami, US(12.0ms) | isp:Oldtown(4.0ms) | geo:your connection(-)" {
		t.Errorf("origins line %q", v.Origins)
	}
	if got := fbIDs(r.Field); len(got) != 2 || got[0] != "i1" || got[1] != "i2" {
		t.Errorf("field %v, want the winner's WHOLE fetched list i1, i2 - not just its raced pool", got)
	}
	if len(r.Raced) != 5 || r.Raced["i1"] != 4*time.Millisecond || r.Raced["g1"] != 0 {
		t.Errorf("raced %v, want every racer with its floor (g1 unanswered = 0)", r.Raced)
	}

	// Silence: nobody answers. The first anchored origin stands in, the
	// verdict says so, and there is no field to hand on.
	stubRacePing(t, map[string]int{})
	r = o.runRace(context.Background(), origins, cityPoolSize)
	if !r.OK || r.Origin.Kind != "exit" || r.Verdict.Outcome != RaceSilent || r.Verdict.Racers != 5 || r.Field != nil || r.Raced != nil {
		t.Errorf("silent race: %+v verdict %+v", r.Origin, r.Verdict)
	}
	if r.Verdict.Origins != "exit:Miami, US(-) | isp:Oldtown(-) | geo:your connection(-)" {
		t.Errorf("silent origins line %q", r.Verdict.Origins)
	}

	// Nothing anchored, nothing at all, nothing fetchable.
	if r := o.runRace(context.Background(), origins[2:], cityPoolSize); r.Verdict.Outcome != RaceUnanchored || !r.OK || r.Verdict.WinnerLat != 0 || r.Verdict.WinnerLon != 0 {
		t.Errorf("unanchored: %+v (an unanchored winner has no coordinate to record)", r.Verdict)
	}
	if r := o.runRace(context.Background(), nil, cityPoolSize); r.Verdict.Outcome != RaceSkipped || r.OK {
		t.Errorf("no origins: %+v", r.Verdict)
	}
	stubOriginPools(t, map[string]ookla.Servers{})
	if r := o.runRace(context.Background(), origins[:2], cityPoolSize); r.Verdict.Outcome != RaceFailed || r.OK {
		t.Errorf("no pool fetchable: %+v", r.Verdict)
	}
}

// An auto run whose race decided ranks the race's own list and pings: the
// server list is not fetched again, the raced servers are not pinged again,
// and the verdict rides the Result.
func TestAutoRunReusesTheRaceFieldInsteadOfRefetching(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	// Eight equidistant servers: the race's pool cut takes six (cityPoolSize),
	// so e7 and e8 are the run's to ping.
	pings := countingPing(t, map[string]time.Duration{"e7": 15 * time.Millisecond, "e8": 16 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	exit := ookla.Servers{}
	for i := 1; i <= 8; i++ {
		exit = append(exit, srv("e"+string(rune('0'+i)), 1))
	}
	stubOriginPools(t, map[string]ookla.Servers{"exit": exit})
	stubRacePing(t, map[string]int{"e1": 5, "e2": 9, "e3": 9, "e4": 9, "e5": 9, "e6": 9})
	old := fetchServerList
	fetchServerList = func(context.Context, *ookla.Speedtest) (ookla.Servers, error) {
		t.Error("the run fetched the server list the race had just fetched")
		return nil, errors.New("refetch")
	}
	t.Cleanup(func() { fetchServerList = old })

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if res.ServerID != "e1" {
		t.Errorf("measured %s, want e1 - the race's fastest, ranked on the race's ping", res.ServerID)
	}
	for i := 1; i <= 6; i++ {
		if id := "e" + string(rune('0'+i)); pings.contacted(id) {
			t.Errorf("%s was pinged again after the race measured it", id)
		}
	}
	if !pings.contacted("e7") || !pings.contacted("e8") {
		t.Error("e7 and e8, which the race's pool cut left unpinged, must be pinged by the ranking")
	}
	if res.Race == nil || res.Race.Outcome != RaceDecided || res.Race.WinnerLabel != "Miami, US" {
		t.Errorf("the verdict did not ride the Result: %+v", res.Race)
	}
	if res.Selection == nil || len(res.Selection.Candidates) != 8 || res.Selection.Candidates[0].RankPingMS == nil || *res.Selection.Candidates[0].RankPingMS != 5 {
		t.Errorf("the selection report must rank all eight, with the race's ping as e1's ranking ping: %+v", res.Selection)
	}

	// A pinned run bypasses the race and says so.
	stubServerList(t)
	o.ServerIDFn = func() string { return "2" }
	res, err = o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if res.Race == nil || res.Race.Outcome != RaceBypassedPin {
		t.Errorf("pinned run verdict %+v, want bypassed_pin", res.Race)
	}
}

// The Auto listing is the run's field: after the race it pings the rest of
// the winning city's field (what a run would rank next) and marks the rows a
// run would choose from, so a racer from a losing city is shown but not
// mistaken for a candidate.
func TestRaceListingMarksTheRunsFieldAndPingsItsExtras(t *testing.T) {
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	exit := ookla.Servers{}
	for i := 1; i <= 8; i++ {
		exit = append(exit, srv("e"+string(rune('0'+i)), 1))
	}
	stubOriginPools(t, map[string]ookla.Servers{"exit": exit, "isp": {srv("i1", 1)}})
	pings := stubRacePing(t, map[string]int{"e1": 3, "e2": 9, "e3": 9, "e4": 9, "e5": 9, "e6": 9, "e7": 8, "e8": 8, "i1": 7})
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Label: "Oldtown", Lat: 12.34, Lon: -76.54, Anchored: true},
		}
	}
	l, err := o.RaceListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.Winner == nil || l.Winner.Kind != "exit" {
		t.Fatalf("winner %+v, want exit", l.Winner)
	}
	if len(l.Servers) != 9 {
		t.Fatalf("%d rows, want 9: the seven racers plus the two of exit's field the pool cut left out", len(l.Servers))
	}
	for _, id := range []string{"e7", "e8"} {
		if pings.count(id) != 1 {
			t.Errorf("%s: pinged %d times, want once - the rest of the winner's field is measured, not echoed", id, pings.count(id))
		}
	}
	for _, c := range l.Servers {
		wantIn := c.ID != "i1"
		if c.InField != wantIn {
			t.Errorf("%s in_field=%v, want %v", c.ID, c.InField, wantIn)
		}
		if c.ID == "e7" && (c.Origin != "exit" || c.PingMS == nil || *c.PingMS != 8) {
			t.Errorf("an extra belongs to the winner's field with its own ping: %+v", c)
		}
	}
	if l.Servers[0].ID != "e1" {
		t.Errorf("fastest first: %s", l.Servers[0].ID)
	}
}

// A Best-of listing is the round's field: every city's pool widened to the
// count, all of it in the field, and none of the single-server run's deeper
// "rest of the winner's field" pinged.
func TestRaceListingForABestOfRoundIsTheWholeUnion(t *testing.T) {
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	exit := ookla.Servers{}
	for i := 1; i <= 9; i++ {
		exit = append(exit, srv("e"+string(rune('0'+i)), 1))
	}
	stubOriginPools(t, map[string]ookla.Servers{"exit": exit, "isp": {srv("i1", 1), srv("i2", 1)}})
	pings := stubRacePing(t, map[string]int{"e1": 3, "e2": 9, "e3": 9, "e4": 9, "e5": 9, "e6": 9, "e7": 8, "e8": 8, "e9": 8, "i1": 7, "i2": 7})
	o := NewOokla()
	o.BestOfCountFn = func() int { return 8 }
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Label: "Oldtown", Lat: 12.34, Lon: -76.54, Anchored: true},
		}
	}
	l, err := o.RaceListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Servers) != 10 {
		t.Fatalf("%d rows, want 10: exit's eight and isp's two, and not e9 - a Best-of round never reaches past the pools", len(l.Servers))
	}
	if pings.count("e9") != 0 {
		t.Error("e9 was pinged: a Best-of listing has no extras to measure")
	}
	for _, c := range l.Servers {
		if !c.InField {
			t.Errorf("%s not in the field: a Best-of round ranks the whole union", c.ID)
		}
	}
}

// The saved pane's refresh: each ID resolved and pinged the way the race
// pings; nil where the server cannot be resolved or does not answer.
func TestPingServersByIDAnswersPerID(t *testing.T) {
	oldFetch := fetchServerByID
	fetchServerByID = func(_ context.Context, _ *ookla.UserConfig, id string) (*ookla.Server, error) {
		if id == "3" {
			return nil, errors.New("no such server")
		}
		return srv(id, 0), nil
	}
	t.Cleanup(func() { fetchServerByID = oldFetch })
	oldProbe := probeEndpoint
	probed := map[string]bool{}
	var mu sync.Mutex
	probeEndpoint = func(_ context.Context, s *ookla.Server) endpointState {
		mu.Lock()
		probed[s.ID] = true
		mu.Unlock()
		s.URL = "http://" + s.ID + ".current.example/speedtest/upload.php" // the migration hop the pin path follows
		if s.ID == "2" {
			return endpointRetired
		}
		return endpointOK
	}
	t.Cleanup(func() { probeEndpoint = oldProbe })
	oldPing := racePing
	racePing = func(_ context.Context, s *ookla.Server) {
		if s.URL != "http://"+s.ID+".current.example/speedtest/upload.php" {
			t.Errorf("%s pinged on %q, want the endpoint the probe resolved", s.ID, s.URL)
		}
		if s.ID == "1" {
			s.Latency = 7 * time.Millisecond
		}
	}
	t.Cleanup(func() { racePing = oldPing })
	got := PingServersByID(context.Background(), []string{"1", "2", "3"})
	if !probed["1"] || !probed["2"] || probed["3"] {
		t.Errorf("probed %v: every resolved server follows the migration hop before it is pinged", probed)
	}
	if len(got) != 3 || got["1"].PingMS == nil || *got["1"].PingMS != 7 || got["2"].PingMS != nil || got["3"].PingMS != nil {
		t.Errorf("got %v %v %v; want 7, nil (unanswered), nil (unresolved)", f64v(got["1"].PingMS), got["2"].PingMS, got["3"].PingMS)
	}
	// The probe's verdict rides along: usable, retired, and unknown for a
	// server that could not be resolved at all.
	if got["1"].FallbackOK == nil || !*got["1"].FallbackOK || got["2"].FallbackOK == nil || *got["2"].FallbackOK || got["3"].FallbackOK != nil {
		t.Errorf("health %v %v %v; want true, false, nil", got["1"].FallbackOK, got["2"].FallbackOK, got["3"].FallbackOK)
	}
}

// The Auto listing's distances are measured from the winning city for every
// row, so a Toronto racer reads as ~500 km from Montréal rather than "1 km"
// from Toronto; a row without a position keeps the distance it came with.
func TestRaceListingMeasuresDistanceFromTheWinningCity(t *testing.T) {
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	mtl := srv("m1", 1)
	mtl.Lat, mtl.Lon = "45.50", "-73.57"
	tor := srv("t1", 2)
	tor.Lat, tor.Lon = "43.65", "-79.38"
	blank := srv("b1", 3) // no position: keeps its own distance
	stubOriginPools(t, map[string]ookla.Servers{"exit": {mtl, blank}, "isp": {tor}})
	stubRacePing(t, map[string]int{"m1": 8, "t1": 15, "b1": 9})
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Montréal", Lat: 45.50, Lon: -73.57, Anchored: true},
			{Kind: "isp", Label: "Toronto", Lat: 43.65, Lon: -79.38, Anchored: true},
		}
	}
	l, err := o.RaceListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]RaceCandidate{}
	for _, c := range l.Servers {
		by[c.ID] = c
	}
	if d := by["m1"].DistanceKM; d > 1 {
		t.Errorf("m1 is at the winning centre: %.1f km, want ~0", d)
	}
	if d := by["t1"].DistanceKM; d < 480 || d > 520 {
		t.Errorf("t1 from Montréal: %.1f km, want ~500 - not the 2 km Toronto's own list said", d)
	}
	if d := by["b1"].DistanceKM; d != 3 {
		t.Errorf("b1 has no position and must keep its own distance: %v", d)
	}
}
