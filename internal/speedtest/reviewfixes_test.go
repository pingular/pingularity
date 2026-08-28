package speedtest

import (
	"context"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A single-server run force-seats the user's own ISP even when its box sits
// outside the distance window (trimToCap's tail scan reaches the whole fetched
// list). A Best-of round draws its field from the city pools instead, so the
// pools have to make the same guarantee - otherwise the thorough mode silently
// omits the on-net server the cheap mode always measures.
func TestCityPoolSeatsTheISPFromOutsideTheWindow(t *testing.T) {
	list := ookla.Servers{}
	for i := 1; i <= 8; i++ {
		list = append(list, srv(string(rune('a'+i-1)), 1)) // eight sponsors at the city centre
	}
	onNet := srv("ebox", 60) // the subscriber's own PoP, past autoMarginKM
	onNet.Sponsor = "EBOX"
	list = append(list, onNet)
	stubOriginPools(t, map[string]ookla.Servers{"exit": list})

	origins := []Origin{{Kind: "exit", Label: "Montréal", Lat: 45.5, Lon: -73.57, Anchored: true}}
	pools, _, fetched := fetchOriginPools(context.Background(), origins, "AS1403 EBOX - EBOX", cityPoolSize)
	<-fetched
	seated := false
	for _, s := range pools[0] {
		if s.ID == "ebox" {
			seated = true
		}
	}
	if !seated {
		ids := []string{}
		for _, s := range pools[0] {
			ids = append(ids, s.ID)
		}
		t.Errorf("pool %v: the user's own ISP has no lane, so no Best-of round can ever measure it", ids)
	}
}

// Seating a starred server must not write through into the ranking's own
// sorted copy: the servers past the candidate window are the reserves health
// filtering replenishes from, and overwriting one with a duplicate of the star
// deletes the replacement silently.
func TestSeatingAStarLeavesTheReservesIntact(t *testing.T) {
	stubFallback(t, map[string]endpointState{
		"n1": endpointRetired, "n2": endpointRetired, "n3": endpointRetired,
		"n4": endpointRetired, "n5": endpointRetired,
	})
	servers := ookla.Servers{}
	for i := 1; i <= 5; i++ {
		servers = append(servers, fbServer("n"+string(rune('0'+i)), 1)) // near, all with a dead fallback
	}
	servers = append(servers, fbServer("reserve", 100)) // the nearest healthy stand-in
	for i := 1; i <= 5; i++ {
		servers = append(servers, fbServer("f"+string(rune('0'+i)), 200+float64(i)))
	}
	servers = append(servers, fbServer("star", 500)) // starred, in another city

	// isp "" is the path where trimToCap hands its pool back un-copied.
	ranked, _, _, _ := rankedServersRaced(context.Background(), servers, "", nil, false,
		map[string]bool{"star": true}, 3)
	found := false
	for _, s := range ranked {
		if s.ID == "reserve" {
			found = true
		}
	}
	if !found {
		ids := []string{}
		for _, s := range ranked {
			ids = append(ids, s.ID)
		}
		t.Errorf("ranked %v: the nearest healthy reserve was overwritten by the star, so nothing replaced the dead candidates", ids)
	}
}

// The ranking phase opens one probe per candidate. That was bounded while the
// candidate set was the distance window; a union round hands it every racer of
// every city, so it needs the same cap the race itself uses - the fan-out that
// exhausted this host's socket buffers is the reason both other fan-outs have
// one.
func TestRankingProbesAreBounded(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	enter := func() {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
	}
	leave := func() { mu.Lock(); inFlight--; mu.Unlock() }
	oldPing, oldHealth := ooklaPing, fallbackHealth
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		enter()
		time.Sleep(3 * time.Millisecond)
		leave()
		cb(9 * time.Millisecond)
		s.Latency = 9 * time.Millisecond
		return nil
	}
	fallbackHealth = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { ooklaPing, fallbackHealth = oldPing, oldHealth })

	servers := ookla.Servers{}
	for i := 0; i < 108; i++ { // Best of 16 across five cities plus twelve stars
		servers = append(servers, fbServer("u"+string(rune('0'+i%10))+string(rune('a'+i/10)), float64(i)))
	}
	rankedServersRaced(context.Background(), servers, "", nil, true, nil, 16)
	if peak > racePingParallel {
		t.Errorf("peak %d ranking probes in flight, want at most %d - the cap the race's own pings carry", peak, racePingParallel)
	}
	if peak == 0 {
		t.Fatal("nothing was probed")
	}
}
