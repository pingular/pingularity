package speedtest

import (
	"context"
	"errors"
	"strconv"
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

// A server that fails, serves its exclusion and fails again is not having a bad
// moment. Re-admitting it on the same twelve hours forever costs a measurement
// turn every day, so each repeat buys a longer exclusion - and because the
// first rung expires overnight, the count has to survive both the expiry and
// the restart, or every morning starts at the bottom again.
func TestARepeatOffenderIsExcludedForLonger(t *testing.T) {
	for _, c := range []struct {
		fails int
		want  time.Duration
	}{
		{fallbackStrikes, fallbackTTL},
		{fallbackStrikes + 1, fallbackTTL2},
		{fallbackStrikes + 2, fallbackTTL3},
		{fallbackStrikes + 9, fallbackTTL3}, // the ladder has a top
	} {
		if got := retiredTTL(c.fails); got != c.want {
			t.Errorf("%d strikes -> %v, want %v", c.fails, got, c.want)
		}
	}

	srv := &ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"}
	var saved []ServerHealthRow
	old := PersistServerHealth
	PersistServerHealth = func(r ServerHealthRow) { saved = append(saved, r) }
	t.Cleanup(func() { PersistServerHealth = old })

	// First conviction: the bottom rung.
	swapFallbackMap(t)
	now := time.Now()
	noteUploadRejection(srv)
	if len(saved) != 1 || saved[0].Fails != fallbackStrikes {
		t.Fatalf("first conviction saved %+v, want %d strikes", saved, fallbackStrikes)
	}
	if d := time.Unix(saved[0].Expires, 0).Sub(now); d < fallbackTTL-time.Minute || d > fallbackTTL+time.Minute {
		t.Errorf("first exclusion runs %v, want about %v", d, fallbackTTL)
	}

	// It lapses, the daemon restarts, and it fails again: the reload must carry
	// the count even though the exclusion itself is over.
	swapFallbackMap(t)
	lapsed := []ServerHealthRow{{ServerID: srv.ID, Expires: now.Add(-time.Hour).Unix(), Fails: fallbackStrikes}}
	LoadServerHealth(lapsed)
	probes := 0
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { probes++; return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	if st := fallbackHealth(context.Background(), srv); st != endpointOK || probes != 1 {
		t.Fatalf("a lapsed conviction excluded the server (state %v, %d probes) - the second chance has to be real", st, probes)
	}

	// Reload the history and convict again. Selection's OK probe runs in between
	// on the real path and must NOT wipe the count - see
	// TestAnOKProbeCannotClearWhatOnlyARunCanSee, which is the sequence this one
	// leaves out.
	swapFallbackMap(t)
	LoadServerHealth(lapsed)
	saved = nil
	now = time.Now()
	noteUploadRejection(srv)
	if len(saved) != 1 || saved[0].Fails != fallbackStrikes+1 {
		t.Fatalf("second conviction saved %+v, want %d strikes", saved, fallbackStrikes+1)
	}
	if d := time.Unix(saved[0].Expires, 0).Sub(now); d < fallbackTTL2-time.Minute || d > fallbackTTL2+time.Minute {
		t.Errorf("second exclusion runs %v, want about %v", d, fallbackTTL2)
	}

	// Third time: the top rung, and it still expires.
	swapFallbackMap(t)
	LoadServerHealth([]ServerHealthRow{{ServerID: srv.ID, Expires: now.Add(-time.Hour).Unix(), Fails: fallbackStrikes + 1}})
	saved = nil
	now = time.Now()
	noteUploadRejection(srv)
	if len(saved) != 1 || saved[0].Fails != fallbackStrikes+2 {
		t.Fatalf("third conviction saved %+v, want %d strikes", saved, fallbackStrikes+2)
	}
	if d := time.Unix(saved[0].Expires, 0).Sub(now); d < fallbackTTL3-time.Minute || d > fallbackTTL3+time.Minute {
		t.Errorf("third exclusion runs %v, want about %v", d, fallbackTTL3)
	}

	// A conviction nobody has seen for a week is not evidence: the ladder starts
	// over rather than punishing a server that has since behaved.
	swapFallbackMap(t)
	LoadServerHealth([]ServerHealthRow{{ServerID: srv.ID, Expires: now.Add(-HealthMemory - time.Hour).Unix(), Fails: fallbackStrikes + 2}})
	saved = nil
	now = time.Now()
	noteUploadRejection(srv)
	if len(saved) != 1 || saved[0].Fails != fallbackStrikes {
		t.Fatalf("after %v of quiet the conviction saved %+v, want the bottom rung again", HealthMemory, saved)
	}
	if d := time.Unix(saved[0].Expires, 0).Sub(now); d > fallbackTTL+time.Minute {
		t.Errorf("exclusion runs %v, want about %v", d, fallbackTTL)
	}
}

// The GET probe's own verdicts climb the same ladder. Two strikes retire a
// server; a server that is still broken when that expires is retired for
// longer, without waiting for a run to spend a measurement turn on it.
func TestTheProbeLadderClimbsToo(t *testing.T) {
	swapFallbackMap(t)
	srv := &ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"}
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointRetired }
	t.Cleanup(func() { probeFallback = oldProbe })

	// One strike is not a retirement - it is retried in minutes.
	if st := fallbackHealth(context.Background(), srv); st != endpointUnknown {
		t.Fatalf("one strike gave %v, want unknown: a single 5xx is not a broken server", st)
	}
	expire := func() {
		fbMu.Lock()
		v := fbMap[srv.ID]
		v.expires = time.Now().Add(-time.Minute)
		fbMap[srv.ID] = v
		fbMu.Unlock()
	}
	for _, want := range []time.Duration{fallbackTTL, fallbackTTL2, fallbackTTL3, fallbackTTL3} {
		expire()
		now := time.Now()
		if st := fallbackHealth(context.Background(), srv); st != endpointRetired {
			t.Fatalf("state %v, want retired", st)
		}
		fbMu.Lock()
		d := fbMap[srv.ID].expires.Sub(now)
		fails := fbMap[srv.ID].probeFails
		fbMu.Unlock()
		if d < want-time.Minute || d > want+time.Minute {
			t.Errorf("at %d strikes the exclusion runs %v, want about %v", fails, d, want)
		}
	}
	// Repaired: the next honest OK clears the record, so nothing carries a grudge.
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	expire()
	if st := fallbackHealth(context.Background(), srv); st != endpointOK {
		t.Fatalf("state %v after the server came back, want ok", st)
	}
	fbMu.Lock()
	fails := fbMap[srv.ID].probeFails
	fbMu.Unlock()
	if fails != 0 {
		t.Errorf("%d strikes still on the record after a clean probe, want none", fails)
	}
}

// Remembering lapsed convictions puts entries in a map that is capped, and the
// cap protects live exclusions. History must be the first thing evicted, and a
// standing exclusion must never be downgraded by one.
func TestRememberedHistoryNeverCostsALiveExclusion(t *testing.T) {
	swapFallbackMap(t)
	now := time.Now()

	// A live conviction, then more lapsed history than the map can hold.
	rows := []ServerHealthRow{{ServerID: "live", Expires: now.Add(6 * time.Hour).Unix(), Fails: fallbackStrikes}}
	for i := 0; i < fallbackMapCap+50; i++ {
		rows = append(rows, ServerHealthRow{
			ServerID: "old-" + strconv.Itoa(i),
			Expires:  now.Add(-time.Hour).Unix(),
			Fails:    fallbackStrikes,
		})
	}
	LoadServerHealth(rows)

	fbMu.Lock()
	n := len(fbMap)
	v, ok := fbMap["live"]
	fbMu.Unlock()
	if n > fallbackMapCap {
		t.Errorf("cache holds %d entries, cap is %d", n, fallbackMapCap)
	}
	if !ok || v.state != endpointRetired {
		t.Fatalf("the live exclusion was evicted by history (%+v, present=%v) - the excluded server would be seated again", v, ok)
	}

	// And the standing exclusion cannot be replaced by a lapsed record of the
	// same server, whichever order they arrive in.
	LoadServerHealth([]ServerHealthRow{{ServerID: "live", Expires: now.Add(-time.Hour).Unix(), Fails: fallbackStrikes}})
	fbMu.Lock()
	v = fbMap["live"]
	fbMu.Unlock()
	if v.state != endpointRetired || !v.expires.After(now) {
		t.Errorf("history overwrote a standing exclusion: %+v", v)
	}
}

// The GET probe fetches latency.txt. A server whose UPLOAD endpoint refuses
// everything serves that file perfectly - which is the entire reason a run has
// to convict it in the first place. So an OK from that probe is not evidence
// the fault is gone, and it must not clear the record.
//
// This is not hypothetical: selection probes every candidate immediately before
// seating one, so the OK landed microseconds before the conviction read the
// count. Every conviction saw zero strikes and the ladder never left 12h.
func TestAnOKProbeCannotClearWhatOnlyARunCanSee(t *testing.T) {
	srv := &ookla.Server{ID: "16045", URL: "http://beanfield.example:8080/speedtest/upload.php"}
	var saved []ServerHealthRow
	old := PersistServerHealth
	PersistServerHealth = func(r ServerHealthRow) { saved = append(saved, r) }
	t.Cleanup(func() { PersistServerHealth = old })
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })

	want := []time.Duration{fallbackTTL, fallbackTTL2, fallbackTTL3}
	carried := []ServerHealthRow{}
	for i, w := range want {
		swapFallbackMap(t)        // the daemon restarts
		LoadServerHealth(carried) // the convictions it reads back

		// Selection: the cheap probe says OK, because it always will.
		if st := fallbackHealth(context.Background(), srv); st != endpointOK {
			t.Fatalf("round %d: probe state %v, want ok - the harness is not exercising the real path", i+1, st)
		}
		// The run seats it and every upload is refused.
		saved = nil
		now := time.Now()
		got, _ := noteUploadRejection(srv)
		if len(saved) != 1 {
			t.Fatalf("round %d saved %d rows", i+1, len(saved))
		}
		if d := time.Unix(saved[0].Expires, 0).Sub(now); d < w-time.Minute || d > w+time.Minute {
			t.Errorf("round %d: exclusion %v, want about %v - an OK probe wiped the strikes the run earned", i+1, d, w)
		}
		if got.Round(time.Hour) != w {
			t.Errorf("round %d: reported %v to the caller that logs it, want %v", i+1, got.Round(time.Hour), w)
		}
		carried = []ServerHealthRow{{ServerID: srv.ID, Expires: time.Now().Add(-time.Hour).Unix(), Fails: saved[0].Fails}}
	}

	// The memory still ends. A conviction whose window has fully elapsed counts
	// for nothing, however many OK probes have run over it since.
	swapFallbackMap(t)
	LoadServerHealth([]ServerHealthRow{{ServerID: srv.ID, Expires: time.Now().Add(-fallbackTTL - HealthMemory - time.Hour).Unix(), Fails: 4}})
	fallbackHealth(context.Background(), srv)
	saved = nil
	got, _ := noteUploadRejection(srv)
	if got.Round(time.Hour) != fallbackTTL || saved[0].Fails != fallbackStrikes {
		t.Errorf("after the memory elapsed: %v at %d strikes, want the bottom rung", got, saved[0].Fails)
	}
}

// Two judges, two records. The GET probe fetches latency.txt; a run measures the
// upload. Neither can see what the other sees, so neither's strikes may count
// towards the other's verdict - in EITHER direction.
//
// Both directions were live at once when the two shared a counter: a repaired
// server carrying two run strikes met a single 503 and the probe read 2+1=3,
// sailing past the two-strike rule to retire it for 24h on one response the
// code itself calls indistinguishable from a maintenance window; and two
// transient 5xx turned a server's FIRST upload conviction into a 24h one.
func TestNeitherJudgeCountsTheOthersStrikes(t *testing.T) {
	oldProbe := probeFallback
	t.Cleanup(func() { probeFallback = oldProbe })
	lapse := func(id string) {
		fbMu.Lock()
		v := fbMap[id]
		v.expires = time.Now().Add(-time.Minute)
		fbMap[id] = v
		fbMu.Unlock()
	}

	// The probe's way: a REPAIRED server still carrying its run strikes.
	swapFallbackMap(t)
	srv := &ookla.Server{ID: "16045", URL: "http://x.example/upload.php"}
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	noteUploadRejection(srv)
	lapse(srv.ID)
	fallbackHealth(context.Background(), srv) // the operator fixed it; the GET is clean
	fbMu.Lock()
	kept := fbMap[srv.ID].runFails
	fbMu.Unlock()
	if kept != fallbackStrikes {
		t.Fatalf("a clean GET cleared %d run strikes, want them kept - it cannot see what earned them", fallbackStrikes-kept)
	}
	lapse(srv.ID)
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointRetired }
	if st := fallbackHealth(context.Background(), srv); st != endpointUnknown {
		t.Errorf("one 503 on a repaired server gave %v: the two-strike rule was bypassed by strikes the probe never earned", st)
	}
	fbMu.Lock()
	v := fbMap[srv.ID]
	fbMu.Unlock()
	if v.probeFails != 1 || v.runFails != fallbackStrikes {
		t.Errorf("records bled into each other: probeFails=%d runFails=%d, want 1 and %d", v.probeFails, v.runFails, fallbackStrikes)
	}

	// The run's way: transient 5xx must not climb the conviction ladder.
	swapFallbackMap(t)
	srv2 := &ookla.Server{ID: "16046", URL: "http://y.example/upload.php"}
	fallbackHealth(context.Background(), srv2)
	lapse(srv2.ID)
	fallbackHealth(context.Background(), srv2) // two strikes: the probe retires it on its own
	lapse(srv2.ID)
	got, _ := noteUploadRejection(srv2)
	if got.Round(time.Hour) != fallbackTTL {
		t.Errorf("a FIRST conviction cost %v, want %v - the probe's strikes climbed the run's ladder", got.Round(time.Hour), fallbackTTL)
	}
}

// A count the memory window has released is not evidence. Every reader of it
// has to say so - the conviction path as much as the probe path.
func TestAReleasedCountIsNotEvidence(t *testing.T) {
	swapFallbackMap(t)
	srv := &ookla.Server{ID: "16045", URL: "http://x.example/upload.php"}
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })

	// Four strikes, but the window closed an hour ago.
	fbMu.Lock()
	fbMap[srv.ID] = fallbackVerdict{
		state:    endpointUnknown,
		expires:  time.Now().Add(-2 * time.Hour),
		runFails: fallbackStrikes + 2,
		forget:   time.Now().Add(-time.Hour),
	}
	fbMu.Unlock()
	fallbackHealth(context.Background(), srv) // the probe must not carry it forward
	fbMu.Lock()
	carried := fbMap[srv.ID].runFails
	fbMu.Unlock()
	if carried != 0 {
		t.Errorf("the probe carried %d released strikes forward, want none", carried)
	}

	// And the conviction path on its own. It needs its own case: run after a
	// probe, the probe has already dropped the count and this gate never runs -
	// which is how a test can pass while the rule it names is absent.
	swapFallbackMap(t)
	fbMu.Lock()
	fbMap[srv.ID] = fallbackVerdict{
		state:    endpointUnknown,
		expires:  time.Now().Add(-2 * time.Hour),
		runFails: fallbackStrikes + 2,
		forget:   time.Now().Add(-time.Hour),
	}
	fbMu.Unlock()
	got, _ := noteUploadRejection(srv)
	if got.Round(time.Hour) != fallbackTTL {
		t.Errorf("the conviction charged %v off a released count, want the bottom rung %v", got.Round(time.Hour), fallbackTTL)
	}
}

// A probe crosses the network with the lock released. A run can convict in that
// gap, and the probe's snapshot is then stale: writing it blind handed back the
// escalation and shortened the exclusion the conviction had just earned.
func TestAProbeLandingAfterAConvictionCannotUndoIt(t *testing.T) {
	swapFallbackMap(t)
	srv := &ookla.Server{ID: "16045", URL: "http://x.example/upload.php"}
	oldProbe := probeFallback
	t.Cleanup(func() { probeFallback = oldProbe })

	// The probe has a strike of its own already, so its verdict this time is a
	// genuine retirement - which storeFallbackVerdictLocked ALLOWS to replace a
	// standing one (retired may replace retired). Without that, the existing
	// guard rejects the write and this test passes without exercising anything.
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointRetired }
	fallbackHealth(context.Background(), srv)
	fbMu.Lock()
	v0 := fbMap[srv.ID]
	v0.expires = time.Now().Add(-time.Minute)
	fbMap[srv.ID] = v0
	fbMu.Unlock()

	// Now the second probe, and while it is "in flight" a run convicts the same
	// server three times over - the top rung, three days.
	probeFallback = func(context.Context, *ookla.Server) endpointState {
		for i := 0; i < 3; i++ {
			noteUploadRejection(srv)
			fbMu.Lock() // each conviction has been here before
			v := fbMap[srv.ID]
			v.forget = time.Now().Add(HealthMemory)
			fbMap[srv.ID] = v
			fbMu.Unlock()
		}
		return endpointRetired
	}
	fallbackHealth(context.Background(), srv)
	fbMu.Lock()
	v := fbMap[srv.ID]
	fbMu.Unlock()
	if v.runFails < fallbackStrikes+2 {
		t.Errorf("the landing probe stripped the conviction back to %d strikes", v.runFails)
	}
	if d := time.Until(v.expires); d < fallbackTTL3-time.Minute {
		t.Errorf("the landing probe shortened a %v exclusion to %v", fallbackTTL3, d.Round(time.Minute))
	}
}

// A run that MEASURES the upload endpoint without being refused is the strongest
// evidence there is that the fault is gone - the GET probe cannot see this
// endpoint at all. So it clears the run's record, and the next bad day starts at
// the bottom rung. But not while the exclusion is standing: a server is only
// measured during its own bench because someone pinned it, and a pin must not
// become a way to erase a conviction.
func TestAGoodRunClearsTheRecordOnceTheBenchHasLapsed(t *testing.T) {
	srv := &ookla.Server{ID: "16045", URL: "http://x.example/upload.php"}
	var forgotten []string
	oldForget := ForgetServerHealth
	ForgetServerHealth = func(id string) { forgotten = append(forgotten, id) }
	t.Cleanup(func() { ForgetServerHealth = oldForget })
	oldPersist := PersistServerHealth
	PersistServerHealth = func(ServerHealthRow) {}
	t.Cleanup(func() { PersistServerHealth = oldPersist })

	// Standing exclusion: a pinned server measured mid-bench changes nothing.
	swapFallbackMap(t)
	noteUploadRejection(srv)
	noteUploadAccepted(srv)
	fbMu.Lock()
	v := fbMap[srv.ID]
	fbMu.Unlock()
	if v.runFails != fallbackStrikes || v.state != endpointRetired {
		t.Errorf("a run during the bench cleared the record (%d strikes, %v) - a pin would erase any conviction", v.runFails, v.state)
	}
	if len(forgotten) != 0 {
		t.Errorf("and it reached the database: %v", forgotten)
	}

	// Lapsed: the server is back on merit, and this is the evidence it deserves.
	fbMu.Lock()
	v = fbMap[srv.ID]
	v.expires = time.Now().Add(-time.Minute)
	fbMap[srv.ID] = v
	fbMu.Unlock()
	noteUploadAccepted(srv)
	fbMu.Lock()
	v = fbMap[srv.ID]
	fbMu.Unlock()
	if v.runFails != 0 || !v.forget.IsZero() {
		t.Errorf("after a measured upload the record still reads %d strikes (forget %v)", v.runFails, v.forget)
	}
	if len(forgotten) != 1 || forgotten[0] != srv.ID {
		t.Errorf("the stored row was not dropped: %v - a restart would charge the server again", forgotten)
	}

	// And the ladder really did start over.
	got, _ := noteUploadRejection(srv)
	if got.Round(time.Hour) != fallbackTTL {
		t.Errorf("the next conviction cost %v, want the bottom rung %v", got.Round(time.Hour), fallbackTTL)
	}

	// The GET probe's own strikes are not this run's to clear.
	swapFallbackMap(t)
	fbMu.Lock()
	fbMap[srv.ID] = fallbackVerdict{state: endpointOK, expires: time.Now().Add(time.Hour), probeFails: 1, runFails: 2, forget: time.Now().Add(HealthMemory)}
	fbMu.Unlock()
	noteUploadAccepted(srv)
	fbMu.Lock()
	v = fbMap[srv.ID]
	fbMu.Unlock()
	if v.probeFails != 1 {
		t.Errorf("the run cleared the probe's strikes too (probeFails=%d) - each judge clears its own record", v.probeFails)
	}
}
