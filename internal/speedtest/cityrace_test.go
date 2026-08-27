package speedtest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// stubOriginPools swaps the per-origin list fetch for canned pools keyed by
// origin kind. Kinds absent from the map fail their fetch, which is the
// one-dead-origin case production must survive.
func stubOriginPools(t *testing.T, pools map[string]ookla.Servers) *originFetchCount {
	t.Helper()
	c := &originFetchCount{}
	old := fetchOriginServers
	fetchOriginServers = func(_ context.Context, o Origin) (ookla.Servers, error) {
		c.mu.Lock()
		c.kinds = append(c.kinds, o.Kind)
		c.mu.Unlock()
		p, ok := pools[o.Kind]
		if !ok {
			return nil, errors.New("origin list unavailable")
		}
		return p, nil
	}
	t.Cleanup(func() { fetchOriginServers = old })
	return c
}

type originFetchCount struct {
	mu    sync.Mutex
	kinds []string
}

func (c *originFetchCount) fetches() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.kinds)
}

// stubRacePing swaps the race's probe for a table of latencies by server ID and
// counts how often each candidate was pinged. IDs absent from the table stay
// unanswered (Latency 0).
func stubRacePing(t *testing.T, ms map[string]int) *racePingCount {
	// Replace semantics, like the real racePing: an ID absent from the table
	// reads as unanswered (Latency 0) even if the fixture carried a fetch echo.
	t.Helper()
	c := &racePingCount{pinged: map[string]int{}}
	old := racePing
	racePing = func(_ context.Context, s *ookla.Server) {
		c.mu.Lock()
		c.pinged[s.ID]++
		c.mu.Unlock()
		if m, ok := ms[s.ID]; ok {
			s.Latency = time.Duration(m) * time.Millisecond
		} else {
			s.Latency = 0
		}
	}
	t.Cleanup(func() { racePing = old })
	return c
}

type racePingCount struct {
	mu     sync.Mutex
	pinged map[string]int
}

func (c *racePingCount) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinged[id]
}

func srv(id string, dist float64) *ookla.Server {
	return &ookla.Server{ID: id, Sponsor: "S" + id, Name: "N" + id, Distance: dist}
}

// THE LOCK HAS TO BE EXERCISED, NOT MERELY WRITTEN. ookla.NewLocation writes
// the library's package-global Locations map with no synchronization of its
// own, so the concurrent pool fetches this file's other tests stub out are
// exactly the code that can take the daemon down with "fatal error: concurrent
// map writes" - a runtime throw, not a recoverable panic. Every other test here
// replaces fetchOriginServers, so without this one the -race gate stays green
// with the lock deleted.
//
// It drives the REAL fetchOriginServers (and the real ListOoklaServers, the
// other map writer, on a web handler's behalf) from concurrent goroutines. The
// context is cancelled up front, so the map writes happen and the HTTP fetches
// behind them do not - no network, no flake.
func TestConcurrentOriginFetchesSerializeTheLibraryLocationMap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	origins := []Origin{
		{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
		{Kind: "isp", Lat: 12.34, Lon: -76.54, Anchored: true},
		{Kind: "geo"}, // unanchored: no map write, but it still races the others
		{Kind: "spare", Lat: 45.50, Lon: -73.57, Anchored: true},
	}
	// Registering a centre is the point here, so put the keys back afterwards
	// rather than leaving them for whatever runs next.
	for _, o := range origins {
		forgetLocation(t, o.Kind)
	}
	forgetLocation(t, "custom")
	var wg sync.WaitGroup
	for _, o := range origins {
		wg.Add(1)
		go func(o Origin) {
			defer wg.Done()
			_, _ = fetchOriginServers(ctx, o)
		}(o)
	}
	// The picker endpoint, which a user can open at any moment during a run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ListOoklaServers(ctx, 45.5, -73.57)
	}()
	wg.Wait()
}

// A field with no coordinate in it has nothing to race: whatever the race
// decided, the caller acts only on an anchored winner, so the fetch stays
// uncentred either way and the run re-fetches the same list the race just
// fetched. Measured on a live link, that costs ~4.5s and ~107 requests to
// decide something already decided. It is a real steady state, not a boot
// transient: with connection-info lookups switched off, netinfo publishes
// nothing and every auto run lands here for the life of the install.
func TestRaceCitiesSkipsAFieldWithNothingAnchored(t *testing.T) {
	fetches := stubOriginPools(t, map[string]ookla.Servers{"geo": {srv("g1", 1)}})
	pings := stubRacePing(t, map[string]int{"g1": 5})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 0, Lon: 0, Anchored: true}, // no fix: dropped by pickOrigins
			{Kind: "geo", Label: "your connection"},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok || win.Kind != "geo" {
		t.Fatalf("winner = %+v ok=%v, want the sole unanchored origin", win, ok)
	}
	if n := fetches.fetches(); n != 0 {
		t.Errorf("fetched %d pools deciding nothing; the outcome is uncentred whatever the race says", n)
	}
	if n := pings.count("g1"); n != 0 {
		t.Errorf("pinged %d servers deciding nothing", n)
	}
}

// ...but a single ANCHORED origin is NOT a foregone conclusion, so it must
// still race: if its pool cannot be fetched the run has to stay uncentred,
// which is not the same answer as centring on it.
func TestRaceCitiesStillRacesASingleAnchoredOrigin(t *testing.T) {
	fetches := stubOriginPools(t, map[string]ookla.Servers{"isp": {srv("i1", 1)}})
	stubRacePing(t, map[string]int{"i1": 7})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "isp", Lat: 12.34, Lon: -76.54, Anchored: true}}
	}
	if win, ok := o.raceCities(context.Background()); !ok || win.Kind != "isp" {
		t.Fatalf("winner = %+v ok=%v, want isp", win, ok)
	}
	if n := fetches.fetches(); n != 1 {
		t.Errorf("origin fetches = %d, want 1 - a lone anchored origin still has an outcome to decide", n)
	}

	// The outcome that makes it worth racing: no pool, so no centre.
	stubOriginPools(t, nil)
	if _, ok := o.raceCities(context.Background()); ok {
		t.Error("a lone anchored origin whose pool could not be fetched must leave the run uncentred")
	}
}

// A cancelled run must not be held open by fetches it cannot cancel. The
// library echoes the whole list it fetched on a context of its own, so those
// goroutines outlive our cancellation by up to 4s - and waiting for them held
// the scheduler's single-flight flag, so every competing run got ErrBusy.
func TestRaceCitiesDoesNotWaitForUncancellableFetches(t *testing.T) {
	release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	old, oldPing := fetchOriginServers, racePing
	fetchOriginServers = func(ctx context.Context, o Origin) (ookla.Servers, error) {
		close(entered)
		<-release // ignores ctx, exactly as the library's echo round does
		close(finished)
		return ookla.Servers{srv("late", 1)}, nil
	}
	// The orphaned fetch must not reach the REAL probe. srv() carries no URL and
	// a nil library context, and PingTestContext dereferences that context on its
	// first statement - before any ctx check - so the probe would nil-deref and
	// take the whole test binary down. That path is dead while this fix holds and
	// live the moment it regresses, i.e. exactly when this test is meant to
	// report: unstubbed, a regression here destroys two thirds of the package's
	// results instead of failing one test. Assigned directly rather than through
	// stubRacePing, whose own LIFO cleanup would restore the real probe first.
	racePing = func(context.Context, *ookla.Server) {}
	// Ordered against the abandoned goroutine below: the seams AND t itself have
	// to outlive it, or a regression reports as a crash in some later test.
	t.Cleanup(func() { <-finished; <-done; fetchOriginServers, racePing = old, oldPing })

	ctx, cancel := context.WithCancel(context.Background())
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}
	go func() {
		defer close(done)
		if _, ok := o.raceCities(ctx); ok {
			t.Error("a cancelled race must not claim a winner")
		}
	}()
	<-entered // the fetch is in flight and unresponsive to ctx
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("raceCities waited on a fetch it cannot cancel, holding the run's single-flight open")
	}
	// The race returned without it; now let the orphan retire, as the library's
	// own 4s deadline would.
	close(release)
}

// THE RACE'S OWN BUDGET EXPIRING IS A FAILED MEASUREMENT, NOT AN ABORT. A list
// endpoint that accepts the connection and then goes silent - the exact case
// cityRaceBudget exists to bound - leaves the run alive with its whole
// measurement budget intact. Returning "no centre" there would hand that run
// the Ookla API's guess at our address, silently, on a link where the docstring
// promises the exit router instead. Only the CALLER's cancellation means there
// is nothing left to centre.
func TestRaceCitiesFallsBackWhenItsOwnBudgetExpires(t *testing.T) {
	oldBudget := cityRaceBudget
	cityRaceBudget = 150 * time.Millisecond
	t.Cleanup(func() { cityRaceBudget = oldBudget })

	release, finished := make(chan struct{}), make(chan struct{})
	old, oldPing := fetchOriginServers, racePing
	fetchOriginServers = func(ctx context.Context, o Origin) (ookla.Servers, error) {
		<-release // a silent list endpoint: outlives the race's budget
		close(finished)
		return ookla.Servers{srv("late", 1)}, nil
	}
	// The orphan must not reach the real probe (see the cancellation test).
	racePing = func(context.Context, *ookla.Server) {}
	t.Cleanup(func() { <-finished; fetchOriginServers, racePing = old, oldPing })
	// Released on a timer rather than after the call, so that a regression which
	// makes the race WAIT for this fetch fails the assertion below instead of
	// deadlocking the package. Far longer than the shortened budget, so it
	// cannot mask the behaviour under test.
	time.AfterFunc(2*time.Second, func() { close(release) })

	o := NewOokla()
	// One origin, so the stub runs once and the channel is closed once.
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}
	// The caller's context stays alive throughout - only the race's own budget
	// runs out. Run it off the test goroutine so the RETURN TIME is assertable:
	// checking only the winner would pass even if the race waited for the fetch,
	// because the timer hands back the same anchored fallback two seconds later.
	type outcome struct {
		win   Origin
		ok    bool
		after time.Duration
	}
	res := make(chan outcome, 1)
	start := time.Now()
	go func() {
		w, k := o.raceCities(context.Background())
		res <- outcome{w, k, time.Since(start)}
	}()

	var got outcome
	select {
	case got = <-res:
	case <-time.After(time.Second):
		t.Fatal("the race did not return on its own budget; it is waiting for a fetch it cannot cancel")
	}
	if got.after >= 2*time.Second {
		t.Errorf("the race returned after %v, i.e. only once the fetch was released - it waited "+
			"rather than acting on its own %v budget", got.after, cityRaceBudget)
	}
	if !got.ok || got.win.Kind != "exit" {
		t.Fatalf("winner = %+v ok=%v; want the first anchored origin. The race spent its own budget "+
			"on a live run, so the run is centred on the Ookla API's guess at our address instead",
			got.win, got.ok)
	}
}

// The mirror of the cancellation test, on the OTHER round: a caller who gives
// up while the pings are in flight must not come back with a centre. It used to
// reach the unanswered-race fallback and hand a dead run an anchored origin,
// after which RunReason wrote the library's global location for a fetch that
// could only fail.
func TestRaceCitiesClaimsNoCentreWhenTheCallerCancelsDuringPinging(t *testing.T) {
	pinging := make(chan struct{})
	var once sync.Once
	old, oldPing := fetchOriginServers, racePing
	fetchOriginServers = func(context.Context, Origin) (ookla.Servers, error) {
		return ookla.Servers{srv("s1", 1)}, nil
	}
	racePing = func(ctx context.Context, s *ookla.Server) {
		once.Do(func() { close(pinging) })
		<-ctx.Done() // still probing when the caller gives up
	}
	t.Cleanup(func() { fetchOriginServers, racePing = old, oldPing })

	ctx, cancel := context.WithCancel(context.Background())
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}
	done := make(chan struct{})
	var win Origin
	var ok bool
	go func() { defer close(done); win, ok = o.raceCities(ctx) }()

	<-pinging
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled ping round held the race open")
	}
	if ok {
		t.Fatalf("winner = %+v ok=true; a run the caller abandoned must not claim a centre", win)
	}
}

func TestPickOriginsDropsNoFixFoldsNearDuplicatesAndCaps(t *testing.T) {
	in := []Origin{
		{Kind: "exit", Lat: 0, Lon: 0, Anchored: true},           // "no fix", not the Gulf of Guinea
		{Kind: "isp", Lat: 45.50, Lon: -73.57, Anchored: true},   // Montreal
		{Kind: "colo", Lat: 45.47, Lon: -73.74, Anchored: true},  // ~14km away: same place, fold it
		{Kind: "geo", Anchored: false},                           // the operator's own placement
		{Kind: "geo2", Anchored: false},                          // a second unanchored ask is the same request
		{Kind: "other", Lat: 43.65, Lon: -79.38, Anchored: true}, // Toronto: genuinely distinct
	}
	got := pickOrigins(in)
	want := []string{"isp", "geo", "other"}
	if len(got) != len(want) {
		t.Fatalf("kept %d origins %v, want %v", len(got), got, want)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("origin %d = %q, want %q", i, got[i].Kind, k)
		}
	}

	// The fan-out cap: a hostile enumeration must not turn one race into an
	// unbounded burst of list fetches at a third party.
	var many []Origin
	for i := 0; i < 20; i++ {
		many = append(many, Origin{Kind: "k", Lat: float64(i + 1), Lon: 100, Anchored: true})
	}
	if got := pickOrigins(many); len(got) != cityOriginMax {
		t.Errorf("cap: kept %d origins, want %d", len(got), cityOriginMax)
	}

	// The daemon's real field: exit and ISP distinct, stars in two more cities,
	// and Ookla's own placement last. All five must be fetched - the fifth lane
	// is what the cap was widened for - and the unanchored placement is never
	// the one the cap evicts, even with stars in three or four cities.
	field := []Origin{
		{Kind: "exit", Lat: 45.51, Lon: -73.59, Anchored: true},
		{Kind: "isp", Lat: 43.70, Lon: -79.40, Anchored: true},
		{Kind: "saved", Lat: 46.81, Lon: -71.21, Anchored: true}, // Quebec City
		{Kind: "saved", Lat: 45.42, Lon: -75.70, Anchored: true}, // Ottawa
		{Kind: "geo", Anchored: false},
	}
	if got := originKinds(pickOrigins(field)); len(got) != 5 || got[4] != "geo" {
		t.Errorf("five-origin field kept %v, want all five with geo last", got)
	}
	six := append(append([]Origin{}, field[:4]...), Origin{Kind: "saved", Lat: 44.23, Lon: -76.49, Anchored: true}, field[4]) // Kingston
	if got := originKinds(pickOrigins(six)); len(got) != 6 || got[5] != "geo" {
		t.Errorf("a fifth anchored origin evicted Ookla's placement: kept %v", got)
	}
	seven := append(append([]Origin{}, six[:5]...), Origin{Kind: "saved", Lat: 48.42, Lon: -71.06, Anchored: true}, field[4]) // Saguenay: past the cap
	if got := originKinds(pickOrigins(seven)); len(got) != 6 || got[5] != "geo" {
		t.Errorf("stars past the cap must be the ones dropped, never geo: kept %v", got)
	}
}

func originKinds(os []Origin) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.Kind)
	}
	return out
}

// A metro whose servers all sit at the city-centre point is a distance tie, and
// "the six nearest" of a tie used to be whichever six the sort left first -
// measured, that dropped the fastest-echoing server and the user's own ISP's
// from the Montreal pool. The cut now breaks the tie by the fetch's echo,
// prefers one server per sponsor, and seats the ISP's server, so the race
// field is the six worth racing rather than six by chance.
func TestPoolCutBreaksATieByEchoAndSeatsTheISP(t *testing.T) {
	tied := func(id, sponsor string, echoMS int) *ookla.Server {
		s := srv(id, 1)
		s.Sponsor = sponsor
		s.Latency = time.Duration(echoMS) * time.Millisecond
		return s
	}
	// Listed slowest-first, so an order-of-arrival cut would take the wrong six.
	pool := ookla.Servers{
		tied("slow1", "Alpha", 40), tied("slow2", "Bravo", 39), tied("slow3", "Charlie", 38),
		tied("slow4", "Delta", 37), tied("slow5", "Echo", 36), tied("slow6", "Foxtrot", 35),
		tied("bell2", "Bell Canada", 16), tied("bell1", "Bell Canada", 15), // one provider, two boxes: one seat, for diversity
		tied("fast", "Golf", 9),
		tied("ebox3", "EBOX", 24), tied("ebox2", "EBOX", 23), // the ISP's boxes: cityPoolISPMax lanes, its fastest echoes first...
		tied("1993", "EBOX", 21),
	}
	pool[len(pool)-1].Latency = -1 // ...though 1993's echo failed (the library's PingTimeout), so it yields to its siblings here
	fetches := stubOriginPools(t, map[string]ookla.Servers{"exit": pool})
	pings := stubRacePing(t, map[string]int{"fast": 9, "bell1": 15, "bell2": 16, "ebox2": 12, "ebox3": 13, "1993": 11, "slow6": 35, "slow5": 36})
	o := NewOokla()
	o.ISPFn = func() string { return "AS1403 EBOX - EBOX" }
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Montréal, CA", Lat: 45.51, Lon: -73.59, Anchored: true}}
	}
	win, ok := o.raceCities(context.Background())
	if !ok || fetches.fetches() != 1 {
		t.Fatalf("race: ok=%v fetches=%d", ok, fetches.fetches())
	}
	for _, id := range []string{"ebox2", "ebox3", "fast", "bell1", "slow6", "slow5"} {
		if pings.count(id) != 1 {
			t.Errorf("%s pinged %d times, want 1: %d ISP lanes, then the fastest echoes with one box per sponsor, then the rest by echo", id, pings.count(id), cityPoolISPMax)
		}
	}
	for _, id := range []string{"1993", "slow1", "slow2", "slow3", "slow4", "bell2"} {
		if pings.count(id) != 0 {
			t.Errorf("%s pinged %d times, want 0: a third ISP box past its %d lanes, a second box of one sponsor, or cut by echo", id, pings.count(id), cityPoolISPMax)
		}
	}
	if win.Kind != "exit" {
		t.Errorf("winner = %v", win)
	}
}

// A small city's pool is its own servers. Ookla pads every list out to ~25
// rows from whatever is next, so seeding over the whole list let the
// one-per-sponsor pass seat another metro's boxes over the city's own
// duplicates - and a server two cities both listed was then credited to the
// wrong one. The pool is drawn from the run's distance window, never fewer
// than the pool size, and its lanes stay in distance order.
func TestPoolCutStaysWithinTheCitysOwnDistanceWindow(t *testing.T) {
	at := func(id, sponsor string, km float64, echoMS int) *ookla.Server {
		s := srv(id, km)
		s.Sponsor = sponsor
		s.Latency = time.Duration(echoMS) * time.Millisecond
		return s
	}
	pool := ookla.Servers{
		at("v1", "Videotron", 1, 12), at("v2", "Videotron", 1, 13), at("v3", "Videotron", 1, 14), at("bell", "Bell Canada", 1, 15),
		at("tr", "Cogeco", 130, 20), at("m1", "Alpha", 250, 9), at("m2", "Bravo", 250, 10), at("m3", "Charlie", 250, 11), at("m4", "Delta", 250, 12),
	}
	stubOriginPools(t, map[string]ookla.Servers{"exit": pool})
	pings := stubRacePing(t, map[string]int{"v1": 12, "v2": 13, "v3": 14, "bell": 15, "tr": 20, "m1": 9, "m2": 10, "m3": 11, "m4": 12})
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "exit", Label: "Québec, CA", Lat: 46.81, Lon: -71.21, Anchored: true}}
	}
	if _, ok := o.raceCities(context.Background()); !ok {
		t.Fatal("race returned no winner")
	}
	for _, id := range []string{"v1", "v2", "v3", "bell", "tr", "m1"} {
		if pings.count(id) != 1 {
			t.Errorf("%s pinged %d times, want 1: the city's own four, then the nearest two to fill the pool", id, pings.count(id))
		}
	}
	for _, id := range []string{"m2", "m3", "m4"} {
		if pings.count(id) != 0 {
			t.Errorf("%s pinged %d times, want 0: 250 km away and past the pool - a faster echo there must not displace the city's own duplicate sponsors", id, pings.count(id))
		}
	}
}

// The picker's Auto button shows the race's field: every pool fetched, the
// union deduplicated, every racer pinged, fastest first, each row naming the
// origin that surfaced it - and the winner is the origin a run started now
// would centre on. Same seams as the race itself, so what the user sees is
// what a run would decide over.
func TestRaceListingIsTheRacesOwnFieldFastestFirst(t *testing.T) {
	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })
	fetches := stubOriginPools(t, map[string]ookla.Servers{
		"isp":   {srv("t1", 4), srv("t2", 4), srv("t3", 4)},
		"saved": {srv("m1", 1), srv("m2", 1), srv("1993", 1), srv("t1", 500)}, // t1 surfaces from two pools: listed once
		"geo":   {srv("t1", 4)},
	})
	stubRacePing(t, map[string]int{"t1": 27, "t2": 28, "m1": 10, "m2": 11, "1993": 12}) // t3 never answers
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "isp", Label: "Toronto, CA", Lat: 43.70, Lon: -79.40, Anchored: true},
			{Kind: "saved", Label: "Montréal, QC", Lat: 45.5, Lon: -73.5, Anchored: true},
			{Kind: "geo", Label: "your connection"},
		}
	}
	l, err := o.RaceListing(context.Background())
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if fetches.fetches() != 3 {
		t.Errorf("fetched %d pools, want one per origin", fetches.fetches())
	}
	var ids []string
	for _, c := range l.Servers {
		ids = append(ids, c.ID)
	}
	want := []string{"m1", "m2", "1993", "t1", "t2", "t3"}
	if len(ids) != len(want) {
		t.Fatalf("listing = %v, want %v: deduplicated, fastest first, the unanswered last", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("listing = %v, want %v", ids, want)
		}
	}
	if l.Servers[0].Origin != "saved" || l.Servers[0].OriginLabel != "Montréal, QC" || l.Servers[0].PingMS == nil || *l.Servers[0].PingMS != 10 {
		t.Errorf("fastest row = %+v, want the starred city's server at 10 ms, credited to that origin", l.Servers[0])
	}
	if last := l.Servers[5]; last.ID != "t3" || last.PingMS != nil {
		t.Errorf("an unanswered racer must carry no ping and sort last: %+v", last)
	}
	if l.Winner == nil || l.Winner.Kind != "saved" {
		t.Errorf("winner = %+v, want the starred city: a run now would centre there", l.Winner)
	}
	if len(l.Origins) != 3 {
		t.Errorf("origins = %v, want the three that raced", l.Origins)
	}
}

// A listing has nothing to degrade to: when its caller gives up mid-race it
// fails, rather than inventing a field from half-written pools. Shaped like
// TestRaceCitiesDoesNotWaitForUncancellableFetches, and for the same reasons:
// the abandoned fetch outlives the call, so the seams and t must outlive it,
// and the orphan must never reach the real probe (see that test's comment).
func TestRaceListingFailsWhenItsBudgetDies(t *testing.T) {
	release, entered, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	old, oldPing := fetchOriginServers, racePing
	fetchOriginServers = func(ctx context.Context, o Origin) (ookla.Servers, error) {
		close(entered)
		<-release
		close(finished)
		return ookla.Servers{srv("late", 1)}, nil
	}
	racePing = func(context.Context, *ookla.Server) {}
	t.Cleanup(func() { <-finished; <-done; fetchOriginServers, racePing = old, oldPing })

	ctx, cancel := context.WithCancel(context.Background())
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{{Kind: "isp", Label: "Toronto, CA", Lat: 43.70, Lon: -79.40, Anchored: true}}
	}
	go func() {
		defer close(done)
		if _, err := o.RaceListing(ctx); err == nil {
			t.Error("a listing whose caller gave up must fail, not invent a field from half-written pools")
		}
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("RaceListing waited on a fetch it cannot cancel")
	}
	close(release)
}

func TestUnionCityCandidatesRoundRobinDedupeAndOwner(t *testing.T) {
	shared := srv("shared", 2)
	a1, a2 := srv("a1", 1), srv("a2", 3)
	b1 := srv("b1", 1)
	sharedB := srv("shared", 1) // same server surfaced by pool B, closer there
	pools := []ookla.Servers{
		{a1, shared, a2}, // pool 0
		{sharedB, b1},    // pool 1
	}
	union, owner := unionCityCandidates(pools)

	// Round-robin by rank: a1 (0,lane0), shared (1,lane0 - B reached it first
	// by rank), b1 (1,lane1), a2 (0,lane2). The pool-0 copy of "shared" is a
	// duplicate ID and must not enter twice.
	wantIDs := []string{"a1", "shared", "b1", "a2"}
	if len(union) != len(wantIDs) {
		t.Fatalf("union = %d servers, want %d", len(union), len(wantIDs))
	}
	for i, id := range wantIDs {
		if union[i].ID != id {
			t.Errorf("union[%d] = %q, want %q", i, union[i].ID, id)
		}
	}
	if owner[sharedB] != 1 {
		t.Errorf("shared server credited to pool %d, want 1 (the pool that surfaced it first by rank)", owner[sharedB])
	}
}

// The whole point: the city whose pool holds the fastest-pinging server wins,
// no matter where it sits in the caller's priority order; each city enters only
// its closest cityPoolSize servers; and a server two cities both surfaced is
// pinged once.
func TestRaceCitiesFastestCityWins(t *testing.T) {
	exitPool := ookla.Servers{
		srv("e1", 1), srv("e2", 2), srv("e3", 3), srv("e4", 4),
		srv("e5", 5), srv("e6", 6), srv("e7", 7), srv("e8", 8), // 7 and 8 are past the top six
	}
	// The isp pool shares e1 (same ID, its own pointer) with the exit pool.
	ispPool := ookla.Servers{srv("e1", 1), srv("i1", 2), srv("i2", 3)}
	geoPool := ookla.Servers{srv("g1", 1)}
	fetches := stubOriginPools(t, map[string]ookla.Servers{"exit": exitPool, "isp": ispPool, "geo": geoPool})
	pings := stubRacePing(t, map[string]int{
		"e1": 20, "e2": 25, "e3": 30, "e4": 30, "e5": 30, "e6": 30,
		"i1": 6, // the ISP city's server is the fastest on the link
		"i2": 21,
		"g1": 35,
	})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Label: "Oldtown, XX", Lat: 12.34, Lon: -76.54, Anchored: true},
			{Kind: "geo", Label: "your connection"},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok {
		t.Fatal("race with three healthy pools returned no winner")
	}
	if win.Kind != "isp" {
		t.Fatalf("winner = %q, want isp - the fastest server was in the ISP city's pool, "+
			"and priority order must not decide a race", win.Kind)
	}
	if fetches.fetches() != 3 {
		t.Errorf("origin fetches = %d, want 3 (one per city)", fetches.fetches())
	}
	for _, id := range []string{"e7", "e8"} {
		if n := pings.count(id); n != 0 {
			t.Errorf("server %s was pinged %d times; it is outside its city's closest %d and must not race", id, n, cityPoolSize)
		}
	}
	if n := pings.count("e1"); n != 1 {
		t.Errorf("shared server e1 pinged %d times, want 1 (deduplicated across cities)", n)
	}
}

// A starred city is an ordinary racer: its pool is fetched and pinged like any
// connection-derived one, and it wins on measurement alone. The measured case:
// the only connection-derived origin left was the ISP geolocation (Toronto,
// 27 ms to everything there) while the starred server's city (Montreal) had
// 10 ms servers that never entered the race.
func TestRaceCitiesAStarredCityCanWin(t *testing.T) {
	fetches := stubOriginPools(t, map[string]ookla.Servers{
		"isp":   {srv("t1", 4), srv("t2", 4), srv("t3", 4)},
		"saved": {srv("m1", 1), srv("m2", 1), srv("1993", 1)},
		"geo":   {srv("t1", 4)},
	})
	pings := stubRacePing(t, map[string]int{"t1": 27, "t2": 28, "t3": 29, "m1": 10, "m2": 11, "1993": 12})
	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "isp", Label: "Toronto, CA", Lat: 43.70, Lon: -79.40, Anchored: true},
			{Kind: "saved", Label: "Montréal, QC", Lat: 45.5, Lon: -73.5, Anchored: true},
			{Kind: "geo", Label: "your connection"},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok || win.Kind != "saved" {
		t.Fatalf("winner = %+v ok=%v, want the starred city: its servers answered fastest", win, ok)
	}
	if fetches.fetches() != 3 || pings.count("1993") != 1 {
		t.Errorf("fetches = %d, star's server pinged %d times; want one fetch per origin and the starred server raced once", fetches.fetches(), pings.count("1993"))
	}
}

// An unanchored winner is a real answer: the pool the Ookla API picks for our
// source address held the fastest server, so the fetch stays uncentred.
func TestRaceCitiesUnanchoredWinnerIsARealAnswer(t *testing.T) {
	stubOriginPools(t, map[string]ookla.Servers{
		"exit": {srv("e1", 1)},
		"geo":  {srv("g1", 1)},
	})
	stubRacePing(t, map[string]int{"e1": 30, "g1": 4})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "geo"},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok || win.Kind != "geo" || win.Anchored {
		t.Fatalf("winner = %+v ok=%v, want the unanchored geo origin", win, ok)
	}
}

// A race nobody answered is a failed measurement, not a verdict: it centres on
// the first anchored origin - the exit router, the old cascade's answer - so a
// ping-hostile network degrades to the pre-race behaviour.
func TestRaceCitiesUnansweredFallsBackToFirstAnchored(t *testing.T) {
	stubOriginPools(t, map[string]ookla.Servers{
		"exit": {srv("e1", 1)},
		"isp":  {srv("i1", 1)},
	})
	stubRacePing(t, nil) // pings blocked network-wide: nobody answers

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Lat: 12.34, Lon: -76.54, Anchored: true},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok || win.Kind != "exit" {
		t.Fatalf("winner = %+v ok=%v, want the first anchored origin (exit)", win, ok)
	}
}

// No origins, or no pool fetched at all, means there is nothing to centre on:
// ok=false, and the caller's uncentred fetch takes over. One dead origin,
// however, must not sink the race.
func TestRaceCitiesSurvivesADeadOriginAndFailsOnlyWhenAllAre(t *testing.T) {
	stubOriginPools(t, map[string]ookla.Servers{"isp": {srv("i1", 1)}}) // exit's fetch errors
	stubRacePing(t, map[string]int{"i1": 9})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Lat: 12.34, Lon: -76.54, Anchored: true},
		}
	}
	if win, ok := o.raceCities(context.Background()); !ok || win.Kind != "isp" {
		t.Fatalf("winner = %+v ok=%v, want isp - one dead origin must not sink the race", win, ok)
	}

	stubOriginPools(t, nil) // every fetch errors
	if _, ok := o.raceCities(context.Background()); ok {
		t.Fatal("race with no fetched pool claimed a winner")
	}

	o.OriginsFn = func() []Origin { return nil }
	if _, ok := o.raceCities(context.Background()); ok {
		t.Fatal("race with no origins claimed a winner")
	}
}

// raceHalted is the single place the two contexts are told apart, and it is
// deliberately called AFTER each select rather than inside it. That matters
// because a context-aware fetch returns BECAUSE the deadline fired, so
// `fetched` and ctx.Done() can be ready at the same instant and Go's select
// picks between ready cases at random - which made one budget expiry sometimes
// take the anchored fallback and sometimes fall through to an empty union and
// leave the run uncentred, per run, unreproducibly. Asking afterwards makes the
// answer depend on what happened rather than on which case the runtime drew.
func TestRaceHaltedClassifiesTheTwoDeadlines(t *testing.T) {
	origins := []Origin{
		{Kind: "geo", Label: "your connection"},
		{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
	}
	live := context.Background()
	dead, cancel := context.WithCancel(context.Background())
	cancel()
	o := NewOokla()

	// Neither died: carry on.
	if stop, _, _ := o.raceHalted(live, live, origins, 0); stop {
		t.Error("halted a race whose contexts are both alive")
	}
	// The caller gave up: the run is over, so there is nothing to centre - even
	// though an anchored origin is right there.
	if stop, _, ok := o.raceHalted(dead, dead, origins, 0); !stop || ok {
		t.Errorf("caller cancellation: stop=%v ok=%v, want stop with no centre", stop, ok)
	}
	// Only the race's own budget died, on a live run: a failed measurement, so
	// degrade to the first ANCHORED origin - not origins[0], which is unanchored.
	stop, win, ok := o.raceHalted(live, dead, origins, 3)
	if !stop || !ok || win.Kind != "exit" {
		t.Errorf("own-budget expiry: stop=%v win=%+v ok=%v, want the exit fallback", stop, win, ok)
	}
	// ...and with nothing anchored to fall back to, it reports no centre.
	if stop, _, ok := o.raceHalted(live, dead, origins[:1], 0); !stop || ok {
		t.Errorf("own-budget expiry with no anchor: stop=%v ok=%v, want stop with no centre", stop, ok)
	}
}

// The whole zero-ping pipeline, together: a real measured 0 (Windows-coarse
// clock) survives as a positive value from the race's clamp through the
// millisecond conversion into the decision figure. Regression: DurMS truncates
// to whole microseconds, so the 1ns clamp used to store 0.0 - which validMS
// reads as "nothing measured", silently sending decisions back to the mean.
func TestZeroPingSurvivesToDecision(t *testing.T) {
	var best time.Duration
	keep := keepFastestPing(&best)
	keep(400 * time.Millisecond)
	keep(0) // measured, sub-clock-resolution round trip
	if best <= 0 {
		t.Fatalf("clamp lost the zero sample: best=%v", best)
	}
	ms := msIfPositive(best)
	if ms == nil || *ms <= 0 {
		t.Fatalf("conversion lost the floor: msIfPositive(%v)=%v", best, ms)
	}
	r := Result{PingMS: 40, PingBestMS: ms}
	if got := decisionPingMS(r); got != *ms {
		t.Fatalf("decisionPingMS=%v fell back to the mean; want the %v floor", got, *ms)
	}
}
