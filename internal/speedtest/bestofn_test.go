package speedtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The run's budget grows with the round: each server keeps its bounded turn
// and the whole is their sum plus selection - no fixed ceiling that would
// starve the last servers of a large round.
func TestRunBudgetScalesWithTheRound(t *testing.T) {
	per1, total1 := runBudget(1, 1)
	if per1 != total1 || total1 != ooklaRunTimeout(1) {
		t.Errorf("single: %v/%v, want the full run timeout for its one server", per1, total1)
	}
	for _, want := range []int{2, 3, 8, 16} {
		per, total := runBudget(1, want)
		if per != bestOfServerTimeout {
			t.Errorf("want=%d: per-server %v, want %v", want, per, bestOfServerTimeout)
		}
		if exp := time.Duration(want)*bestOfServerTimeout + bestOfSelectionBudget; total != exp {
			t.Errorf("want=%d: total %v, want %v (N turns plus selection, uncapped)", want, total, exp)
		}
	}
}

func TestRacePoolSizeWidensWithTheRound(t *testing.T) {
	for want, exp := range map[int]int{1: cityPoolSize, 3: cityPoolSize, 6: 6, 7: 7, 16: 16} {
		if got := racePoolSize(want); got != exp {
			t.Errorf("want=%d: pool %d, want %d", want, got, exp)
		}
	}
	o := NewOokla()
	if o.bestOfCount() != 1 {
		t.Error("no BestOfCountFn: a single server")
	}
	o.BestOfCountFn = func() int { return 40 }
	if o.bestOfCount() != maxBestOfServers {
		t.Errorf("count %d, want the ceiling %d", o.bestOfCount(), maxBestOfServers)
	}
	o.BestOfCountFn = func() int { return 0 }
	if o.bestOfCount() != 1 {
		t.Error("a count below one is one")
	}
}

// An automatic Best-of round is drawn from the WHOLE race: every candidate
// city's pool, widened to the round's size, ranked together by ping - so a
// Toronto server that pings well sits in the same round as Montréal's.
func TestAutoBestOfRoundSpansTheRacesCities(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{})
	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	// Montréal: eight servers; Toronto: eight. A round of eight must draw from both.
	mtl, tor := ookla.Servers{}, ookla.Servers{}
	pings := map[string]int{}
	for i := 1; i <= 8; i++ {
		m, tt := srv("m"+string(rune('0'+i)), 1), srv("t"+string(rune('0'+i)), 1)
		mtl, tor = append(mtl, m), append(tor, tt)
		pings[m.ID], pings[tt.ID] = 7+i, 9+i // m1..m8 = 8..15ms, t1..t8 = 10..17ms
	}
	fetches := stubOriginPools(t, map[string]ookla.Servers{"exit": mtl, "isp": tor})
	stubRacePing(t, pings)
	old := fetchServerList
	fetchServerList = func(context.Context, *ookla.Speedtest) (ookla.Servers, error) {
		t.Error("the round must come from the race's union, not a fresh list fetch")
		return nil, errors.New("refetch")
	}
	t.Cleanup(func() { fetchServerList = old })
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Montréal", Lat: 45.5, Lon: -73.57, Anchored: true},
			{Kind: "isp", Label: "Toronto", Lat: 43.65, Lon: -79.38, Anchored: true},
		}
	}
	o.BestOfCountFn = func() int { return 8 }
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if n := fetches.fetches(); n != 2 {
		t.Errorf("origin fetches %d, want 2", n)
	}
	if len(measured) != 8 {
		t.Fatalf("measured %v, want a round of eight", measured)
	}
	// The eight fastest across both cities by ping: m1..m6 (8-13ms), t1, t2 (10, 11ms)... in ping order:
	// m1 8, m2 9, m3 10, t1 10, m4 11, t2 11, m5 12, t3 12 -> six Montréal (m1-m5 plus one of the 12ms tie) and Toronto's t1-t3.
	torontoSeats := 0
	for _, id := range measured {
		if id[0] == 't' {
			torontoSeats++
		}
	}
	if torontoSeats < 2 {
		t.Errorf("measured %v: a round of eight ranked across both cities should seat Toronto's fastest too", measured)
	}
	if res.Race == nil || res.Race.Racers != 16 {
		t.Errorf("race verdict %+v, want sixteen racers: eight per city, the pools widened to the round", res.Race)
	}
	if res.Selection == nil || len(res.Selection.Candidates) != 16 {
		t.Errorf("the report must rank the whole union, got %d rows", len(res.Selection.Candidates))
	}
}

// Starred servers are seated in every round after the pin: the ones the race
// listed are marked; the rest are resolved by ID and pinged for the round.
// A star that scores highest is recorded as such.
func TestBestOfRoundSeatsTheFavouritesFirst(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{})
	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		down := 100.0
		if srv.ID == "fav-far" {
			down = 150 // the far favourite is the fastest to transfer (under the 2x "not believed" guard)
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: down, UploadMbps: 20, PingMS: 9}, nil
	})
	mtl := ookla.Servers{srv("m1", 1), srv("m2", 1), srv("m3", 1), srv("fav-near", 1), srv("m5", 1), srv("m6", 1)}
	stubOriginPools(t, map[string]ookla.Servers{"exit": mtl})
	stubRacePing(t, map[string]int{"m1": 8, "m2": 9, "m3": 10, "fav-near": 14, "m5": 12, "m6": 13, "fav-far": 30})
	oldFetch := fetchServerByID
	fetchServerByID = func(_ context.Context, _ *ookla.UserConfig, id string) (*ookla.Server, error) {
		if id == "fav-far" {
			return srv("fav-far", 500), nil
		}
		return nil, errors.New("no such server")
	}
	t.Cleanup(func() { fetchServerByID = oldFetch })
	oldProbe := probeEndpoint
	probeEndpoint = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeEndpoint = oldProbe })
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Montréal", Lat: 45.5, Lon: -73.57, Anchored: true}}
	}
	o.BestOfCountFn = func() int { return 4 }
	o.FavouritesFn = func() []string { return []string{"fav-near", "fav-far", "gone"} }
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if len(measured) != 4 || measured[0] != "fav-near" || measured[1] != "fav-far" || measured[2] != "m1" || measured[3] != "m2" {
		t.Errorf("round %v, want the favourites fastest first (near 14ms, far 30ms), then the field's fastest (m1, m2)", measured)
	}
	if res.ServerID != "fav-far" {
		t.Errorf("winner %s, want the far favourite - it transferred fastest", res.ServerID)
	}
	if w := winnerRow(res); w.WinReason != WinReasonFavourite {
		t.Errorf("reason %q, want favourite", w.WinReason)
	}
	rows := 0
	for _, c := range res.Selection.Candidates {
		if c.ServerID == "fav-far" && c.RankPingMS != nil && *c.RankPingMS == 30 {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("the resolved favourite must be in the report with the ping the round measured: %+v", res.Selection.Candidates)
	}
	// A single-server run seats no favourites.
	measured = nil
	o.BestOfCountFn = func() int { return 1 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 1 || measured[0] != "m1" {
		t.Errorf("single: measured %v, want the fastest alone", measured)
	}
}

// Under a pin, the round is the pin, then the favourites, then the pin's
// neighbours - and the pin's own reasons stand.
func TestPinnedBestOfRoundSeatsFavouritesAfterThePin(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubServerList(t)           // servers 1, 2, 3
	stubPinByID(t, srv("2", 1)) // a pinned round resolves the pin by ID first, to centre the list on it
	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.ServerIDFn = func() string { return "2" }
	o.BestOfCountFn = func() int { return 3 }
	o.FavouritesFn = func() []string { return []string{"3", "2"} } // 3 is slow but starred; 2 is the pin
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if len(measured) != 3 || measured[0] != "2" || measured[1] != "3" || measured[2] != "1" {
		t.Errorf("round %v, want the pin, then the starred 3 despite its 20ms, then the fastest neighbour", measured)
	}
	if w := winnerRow(res); w.WinReason != WinReasonPinnedBestOf {
		t.Errorf("reason %q, want pinned_bestof: a pinned round keeps the pin's reasons", w.WinReason)
	}
}

// A pinned round is centred on the pin and never raced, so its list goes
// through the single run's distance window - which must not cut a starred
// server in another city (the star the round resolved and pinged for
// itself), nor stop at the single run's twelve when the round wants more.
func TestPinnedBestOfRoundKeepsAFarFavouriteAndFillsItsCount(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	lat := map[string]time.Duration{}
	for i := 1; i <= 20; i++ {
		lat[fmt.Sprint(i)] = time.Duration(i) * time.Millisecond
	}
	countingPing(t, lat)
	stubServerListN(t, 20) // 1..20 km from the pin
	oldFetch := fetchServerByID
	fetchServerByID = func(_ context.Context, _ *ookla.UserConfig, id string) (*ookla.Server, error) {
		switch id {
		case "2":
			return srv("2", 2), nil
		case "far":
			return srv("far", 500), nil // starred, in another city
		}
		return nil, errors.New("no such server")
	}
	t.Cleanup(func() { fetchServerByID = oldFetch })
	oldProbe := probeEndpoint
	probeEndpoint = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeEndpoint = oldProbe })
	stubRacePing(t, map[string]int{"far": 40})
	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.ServerIDFn = func() string { return "2" }
	o.FavouritesFn = func() []string { return []string{"far"} }
	o.BestOfCountFn = func() int { return 3 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 3 || measured[0] != "2" || measured[1] != "far" || measured[2] != "1" {
		t.Errorf("round %v, want the pin, the far star (500 km is no reason to drop a server the user starred), then the nearest", measured)
	}

	measured = nil
	o.BestOfCountFn = func() int { return 16 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 16 || measured[0] != "2" || measured[1] != "far" {
		t.Errorf("round of %d (%v), want sixteen: the window grows to the count instead of stopping at the single run's twelve", len(measured), measured)
	}
}

// A starred server that did not answer its ping does not take a seat from
// one that did: it waits for the second tier, where the unanswered sort last.
func TestUnansweredFavouriteWaitsItsTurn(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{})
	var measured []string
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("m1", 1), srv("m2", 1), srv("m3", 1)}})
	stubRacePing(t, map[string]int{"m1": 8, "m2": 9, "m3": 10}) // "dead" is absent: it answers nothing
	oldFetch := fetchServerByID
	fetchServerByID = func(_ context.Context, _ *ookla.UserConfig, id string) (*ookla.Server, error) {
		if id == "dead" {
			return srv("dead", 3), nil
		}
		return nil, errors.New("no such server")
	}
	t.Cleanup(func() { fetchServerByID = oldFetch })
	oldProbe := probeEndpoint
	probeEndpoint = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeEndpoint = oldProbe })
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Montréal", Lat: 45.5, Lon: -73.57, Anchored: true}}
	}
	o.FavouritesFn = func() []string { return []string{"dead"} }
	o.BestOfCountFn = func() int { return 2 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 2 || measured[0] != "m1" || measured[1] != "m2" {
		t.Errorf("round %v, want the two that answered: a silent star is not seated ahead of them", measured)
	}
	measured = nil
	o.BestOfCountFn = func() int { return 4 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 4 || measured[3] != "dead" {
		t.Errorf("round %v, want the silent star last, measured only because the round had room", measured)
	}
}

// The ISP's lanes keep their share of a widened pool.
func TestPoolISPLanesScaleWithThePool(t *testing.T) {
	for _, c := range []struct{ pool, want int }{{6, 2}, {3, 2}, {8, 2}, {9, 3}, {16, 5}} {
		if got := poolISPMax(c.pool); got != c.want {
			t.Errorf("pool %d: %d ISP lanes, want %d", c.pool, got, c.want)
		}
	}
}

// The race never has more than racePingParallel pings in flight, however
// wide the pools.
func TestRacePingsAreBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	old := racePing
	racePing = func(_ context.Context, s *ookla.Server) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		s.Latency = time.Millisecond
	}
	t.Cleanup(func() { racePing = old })
	union := ookla.Servers{}
	for i := 0; i < 96; i++ {
		union = append(union, srv(fmt.Sprint("r", i), 1))
	}
	<-pingRacers(context.Background(), union)
	if peak > racePingParallel || peak == 0 {
		t.Errorf("peak %d pings in flight, want at most %d", peak, racePingParallel)
	}
	for _, s := range union {
		if s.Latency == 0 {
			t.Fatalf("%s never pinged", s.ID)
		}
	}
}

// With Discard losers off, the round's other measurements ride the result as
// Losers - each with its own bytes and the second it finished - and the
// winner's row carries only its own transfer plus the round's overhead, so
// the bytes across the rows add up to the round's spend once. On (the
// default) the winner absorbs the round as it always did.
func TestBestOfKeepsTheLosersWhenAsked(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{"1": 5 * time.Millisecond, "2": 6 * time.Millisecond, "3": 7 * time.Millisecond})
	stubServerList(t) // servers 1, 2, 3
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		down := 100.0
		if srv.ID == "2" {
			down = 150
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: down, UploadMbps: 20, PingMS: 9,
			DownloadBytes: 1000, UploadBytes: 200, ExtraDownBytes: 10}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.BestOfCountFn = func() int { return 3 }
	o.DiscardLosersFn = func() bool { return false }
	before := time.Now().Unix()
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerID != "2" || len(res.Losers) != 2 {
		t.Fatalf("winner %s with %d losers, want 2 winning over two kept losers: %+v", res.ServerID, len(res.Losers), res.Losers)
	}
	if res.DownloadBytes != 1000 || res.UploadBytes != 200 {
		t.Errorf("winner bytes %d/%d, want its own 1000/200: the losers carry theirs", res.DownloadBytes, res.UploadBytes)
	}
	if res.ExtraDownBytes != 30 {
		t.Errorf("winner extra bytes %d, want 30: every server's retries ride the winner's usage-only channel", res.ExtraDownBytes)
	}
	total := res.DownloadBytes
	for _, l := range res.Losers {
		if l.DownloadBytes != 1000 || l.ServerID == "2" || l.MeasuredTS < before {
			t.Errorf("loser %+v: want its own bytes, not the winner, and the second it finished", l)
		}
		if l.Losers != nil || l.Selection != nil {
			t.Errorf("a loser carries no round of its own: %+v", l)
		}
		total += l.DownloadBytes
	}
	if total != 3000 {
		t.Errorf("bytes across the rows %d, want 3000: the round's spend counted once", total)
	}
	// A reading the round refused to believe is held to the round middle on a
	// kept loser's row too, as it is on the winner's.
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		down := 100.0
		if srv.ID == "3" {
			down = 950 // three times what the others saw: not believed
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: down, UploadMbps: 20, PingMS: 9, DownloadBytes: 1000, UploadBytes: 200}, nil
	})
	res, err = o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range append([]Result{res}, res.Losers...) {
		if l.DownloadMbps > 100 {
			t.Errorf("%s stored at %.0f Mbps: a disbelieved reading reaches history on no row", l.ServerID, l.DownloadMbps)
		}
	}

	// Discarding (the default, Fn nil): no losers, the winner absorbs the round.
	o.DiscardLosersFn = nil
	res, err = o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	if res.Losers != nil || res.DownloadBytes != 3000 {
		t.Errorf("discarding: %d losers, winner bytes %d; want none and 3000", len(res.Losers), res.DownloadBytes)
	}
	// Explicitly on behaves the same.
	o.DiscardLosersFn = func() bool { return true }
	if res, err = o.RunReason(context.Background(), "manual"); err != nil || res.Losers != nil {
		t.Errorf("discard on: losers %v err %v, want none", res.Losers, err)
	}
	// A single-server run has no round to keep.
	o.BestOfCountFn = func() int { return 1 }
	o.DiscardLosersFn = func() bool { return false }
	if res, err = o.RunReason(context.Background(), "manual"); err != nil || res.Losers != nil {
		t.Errorf("single: losers %v err %v, want none", res.Losers, err)
	}
}
