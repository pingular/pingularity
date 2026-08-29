package speedtest

import (
	"context"
	"errors"
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

// swapFallbackMap gives a test the selection health cache to itself.
func swapFallbackMap(t *testing.T) {
	t.Helper()
	fbMu.Lock()
	saved := fbMap
	fbMap = map[string]fallbackVerdict{}
	fbMu.Unlock()
	t.Cleanup(func() { fbMu.Lock(); fbMap = saved; fbMu.Unlock() })
}

// A server convicted of refusing every upload is excluded for twelve hours -
// but the cache holding that is in-process, so a restart used to forget it and
// the next round paid the whole failed turn again. The conviction is handed to
// a sink the daemon can persist, and seeding it back restores the exclusion
// without re-probing.
func TestAnUploadConvictionOutlivesTheProcess(t *testing.T) {
	swapFallbackMap(t)
	var saved []ServerHealthRow
	old := PersistServerHealth
	PersistServerHealth = func(r ServerHealthRow) { saved = append(saved, r) }
	t.Cleanup(func() { PersistServerHealth = old })

	before := time.Now().Unix()
	noteUploadRejection(&ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"})
	if len(saved) != 1 || saved[0].ServerID != "16045" {
		t.Fatalf("handed the sink %+v, want one row for the convicted server", saved)
	}
	if saved[0].Expires < before+int64(fallbackTTL.Seconds())-5 {
		t.Errorf("expires %d, want about twelve hours out", saved[0].Expires)
	}

	// The restart: a fresh cache, and the probe must not be consulted.
	swapFallbackMap(t)
	probes := 0
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { probes++; return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	LoadServerHealth(saved)
	if st := fallbackHealth(context.Background(), &ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"}); st != endpointRetired {
		t.Errorf("health %v after reloading the conviction, want retired - the round would seat it again", st)
	}
	if probes != 0 {
		t.Errorf("%d probes: a reloaded conviction must stand on its own, not be re-derived from the GET that never sees the problem", probes)
	}
	// An expired row is not reloaded: the twelve hours are the second chance.
	swapFallbackMap(t)
	LoadServerHealth([]ServerHealthRow{{ServerID: "16045", Expires: time.Now().Add(-time.Minute).Unix(), Fails: fallbackStrikes}})
	if st := fallbackHealth(context.Background(), &ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"}); st != endpointOK {
		t.Errorf("health %v from an expired conviction, want the probe's own answer", st)
	}
}

// A server that answered every upload POST and accepted none answers the retry
// the same way. Spending the second window proving it costs the round about
// twenty seconds and the server a few hundred more doomed requests.
func TestARefusedUploadIsNotRetried(t *testing.T) {
	o := NewOokla()
	if !o.uploadRetryable(errors.New("boom")) {
		t.Error("no recorder: a failed upload is retried, as it always was")
	}
	o.upRec = &uploadRecorder{}
	for i := 0; i < uploadRejectMinRefusals; i++ {
		o.upRec.note(500, nil)
	}
	if o.uploadRetryable(errors.New("boom")) {
		t.Error("every POST refused: the retry is another window spent on the same answer")
	}
	// A server that accepted anything, or failed transiently, keeps its retry.
	o.upRec = &uploadRecorder{}
	o.upRec.note(200, nil)
	o.upRec.note(500, nil)
	if !o.uploadRetryable(errors.New("boom")) {
		t.Error("a server that accepted a chunk is not refusing")
	}
	o.upRec = &uploadRecorder{}
	for i := 0; i < uploadRejectMinRefusals; i++ {
		o.upRec.note(503, nil)
	}
	if !o.uploadRetryable(errors.New("boom")) {
		t.Error("503 is transient by the recorder's own rule; the retry stands")
	}
}

// A candidate that transferred nothing in either direction measured nothing.
// Keeping it as a member of the round writes a history row with no data in it,
// which reads as a broken test rather than as the broken server it was.
func TestARoundMemberThatMeasuredNothingIsNotRecorded(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{"1": 5 * time.Millisecond, "2": 6 * time.Millisecond, "3": 7 * time.Millisecond})
	stubServerList(t) // servers 1, 2, 3
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if srv.ID == "3" { // the Beanfield shape: no error, no bytes, no speeds
			return Result{Server: "S3", ServerID: "3", PingMS: 27}, nil
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9,
			DownloadBytes: 1000, UploadBytes: 200}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.BestOfCountFn = func() int { return 3 }
	o.DiscardLosersFn = func() bool { return false }
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range res.Losers {
		if l.ServerID == "3" {
			t.Errorf("the server that transferred nothing was kept as a round member: %+v", l)
		}
	}
	if len(res.Losers) != 1 {
		t.Errorf("%d members kept, want the one that actually measured something", len(res.Losers))
	}
	// It still belongs to the round's own record of what was tried.
	if res.Selection == nil || len(res.Selection.Candidates) != 3 {
		t.Errorf("the selection report must still list all three: %+v", res.Selection)
	}
}

// A Best-of round measures its servers back to back. Each transfer saturates
// the link, and the next server's turn should not start into whatever the last
// one left behind - queues, lingering sockets, a shaper's recovery window.
func TestBestOfRoundLetsTheLinkSettleBetweenServers(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	defer func(d time.Duration) { bestOfServerSettle = d }(bestOfServerSettle)
	bestOfServerSettle = 40 * time.Millisecond

	countingPing(t, map[string]time.Duration{"1": 5 * time.Millisecond, "2": 6 * time.Millisecond, "3": 7 * time.Millisecond})
	stubServerList(t) // servers 1, 2, 3
	var at []time.Time
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		at = append(at, time.Now())
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9,
			DownloadBytes: 1000, UploadBytes: 200}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.BestOfCountFn = func() int { return 3 }
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(at) != 3 {
		t.Fatalf("%d servers measured, want 3", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < bestOfServerSettle {
			t.Errorf("server %d started %v after the last one, want at least %v of settle", i+1, gap, bestOfServerSettle)
		}
	}

	// A single-server run has nothing to settle between: it must not pay it.
	at = nil
	o.BestOfCountFn = func() int { return 1 }
	start := time.Now()
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatal(err)
	}
	if len(at) != 1 {
		t.Fatalf("single: %d measured, want 1", len(at))
	}
	if el := time.Since(start); el >= bestOfServerSettle {
		t.Errorf("single run spent %v, want no settle at all", el)
	}
}
