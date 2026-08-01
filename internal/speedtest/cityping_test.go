package speedtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// pingProbes stands a real HTTP server in for an Ookla server's latency.txt and
// lets a test dictate how long each successive probe takes. The production
// racePing then runs against it unmodified - the library's real HTTPPing, its
// real probes-paced-200ms loop, its real error handling - because the two
// things under test (which statistic is kept, and what happens to samples
// collected before an error) live inside that loop and cannot be observed from
// a reimplementation of it.
type pingProbes struct {
	mu     sync.Mutex
	n      int
	delays []time.Duration // per-probe delay; beyond the end, no delay
}

func (p *pingProbes) handler(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	i := p.n
	p.n++
	var d time.Duration
	if i < len(p.delays) {
		d = p.delays[i]
	}
	p.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	_, _ = w.Write([]byte("test=test\n"))
}

// pingOne runs the production racePing against a server whose probes take the
// given delays, and returns the score it wrote.
func pingOne(t *testing.T, ctx context.Context, delays []time.Duration) time.Duration {
	t.Helper()
	p := &pingProbes{delays: delays}
	ts := httptest.NewServer(http.HandlerFunc(p.handler))
	t.Cleanup(ts.Close)

	s := &ookla.Server{ID: "probe", URL: ts.URL + "/speedtest/upload.php", Context: ookla.New()}
	racePing(ctx, s)
	return s.Latency
}

// ONE STALLED PROBE MUST NOT DECIDE THE RACE. The library scores a server by the
// MEAN of its probes, so a single sample that queued behind something adds a
// tenth of itself to the score. Measured on a live link: a server's samples were
// 98 84 174 68 68 70 327 111 100 107 - mean 120.6ms, minimum 67.7ms - and
// ranking by the mean reordered the top eight of that race.
//
// Latency has a hard floor and an unbounded tail. The floor is the physical
// path, which is the thing that actually differs between one city and another;
// the tail is transient queueing, which is noise every candidate shares. The
// test asserts the score tracks the floor, so the outlier is not merely diluted
// - it is excluded.
func TestRacePingScoreIsTheFloorNotTheMean(t *testing.T) {
	// Probe 0 is the library's discarded warm-up. One later probe stalls 400ms;
	// every other is as fast as loopback allows.
	delays := make([]time.Duration, 11)
	delays[5] = 400 * time.Millisecond

	got := pingOne(t, context.Background(), delays)
	if got <= 0 {
		t.Fatal("no score recorded at all")
	}
	// The mean of ten samples one of which is 400ms is at least 40ms. The floor
	// is loopback, i.e. sub-millisecond. 20ms separates them by a wide margin
	// either way, so this cannot pass by accident on a slow machine.
	if got > 20*time.Millisecond {
		t.Fatalf("score = %v, want the floor (~0ms on loopback). A 400ms stall in one probe of ten "+
			"puts the MEAN above 40ms, which is how a 5ms server loses to a steady 18ms one", got)
	}
}

// A PROBE SET CUT SHORT IS A MEASUREMENT, NOT A FAILURE. PingTestContext assigns
// NOTHING when it returns an error - including the context error it returns
// after collecting good samples and then hitting a deadline. s.Latency then
// stays 0, and the race sorts 0 behind every server that answered. So a fast
// server that was merely cut off would lose to every mediocre server that
// finished, and the samples already in hand were thrown away.
func TestRacePingKeepsSamplesCollectedBeforeADeadline(t *testing.T) {
	// Probes are paced 200ms apart by the library and there are 11 of them, so a
	// 1.5s budget lands several and then expires mid-set on any platform. It was
	// 700ms, which is only ~3 probes of headroom - and on a Windows runner, whose
	// timer granularity is ~15.6ms and whose loopback setup is slower, sometimes
	// zero probes finished and the test failed for a reason it was not about.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	got := pingOne(t, ctx, nil)
	if got <= 0 {
		t.Fatalf("score = %v after a deadline that arrived mid-probe-set; the samples already "+
			"collected were discarded, and 0 sorts this server behind everything that answered", got)
	}
	// Generous on purpose: the claim is "the floor of what landed", not a
	// specific number. A tight bound here measures the runner, not the code.
	if got > 200*time.Millisecond {
		t.Errorf("score = %v, want the loopback floor of the samples that did land", got)
	}
}

// Nothing measured at all stays nothing measured: a server that never answered
// must keep sorting last, or "unreachable" and "instant" become the same score.
func TestRacePingLeavesASilentServerUnscored(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answers
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	s := &ookla.Server{ID: "silent", URL: ts.URL + "/speedtest/upload.php", Context: ookla.New()}
	racePing(ctx, s)
	if s.Latency != 0 {
		t.Fatalf("score = %v for a server that produced no sample, want 0 (sorts last)", s.Latency)
	}
}

// A SILENT SERVER MUST NOT RACE ON THE FETCH'S ECHO. FetchServerListContext
// one-shot-pings every server it returns and writes that onto Latency before
// the race begins (speedtest-go server.go:299-306). A server that then answers
// nothing during the race must not carry that echo into the winner loop:
// measured, a stalled server beat a healthy one scored on ten paced probes,
// because one unpaced echo is not a race result.
func TestRacePingDiscardsTheFetchTimeEcho(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // answered the list fetch earlier; silent now
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	// Arrives carrying the fetch-time echo the library wrote.
	s := &ookla.Server{ID: "echoed", URL: ts.URL + "/speedtest/upload.php",
		Latency: 3 * time.Millisecond, Context: ookla.New()}
	racePing(ctx, s)
	if s.Latency != 0 {
		t.Fatalf("score = %v; a server that answered no race probe kept its fetch-time echo and "+
			"would compete - and win - on a single unpaced sample taken outside the race", s.Latency)
	}

	// The mirror case: PingTimeout (-1), which the library writes when even the
	// fetch echo failed. It must not survive as a "measurement" either.
	s = &ookla.Server{ID: "failed-echo", URL: ts.URL + "/speedtest/upload.php",
		Latency: ookla.PingTimeout, Context: ookla.New()}
	racePing(ctx, s)
	if s.Latency != 0 {
		t.Fatalf("score = %v, want 0 for a server whose fetch echo failed too", s.Latency)
	}
}

// One stalled candidate must not be able to spend the whole race's budget. The
// ping client carries no timeout of its own (it shares the transport with the
// 15s transfers, so it cannot), which would leave cityRaceBudget as the only
// bound - and that bound is shared with every other racer.
func TestRacePingBoundsOneStalledCandidate(t *testing.T) {
	if cityPingTimeout >= cityRaceBudget {
		t.Fatalf("cityPingTimeout %v does not bound a candidate inside the %v race budget",
			cityPingTimeout, cityRaceBudget)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(ts.Close)

	// No deadline of our own: the only thing that can stop this is racePing's.
	s := &ookla.Server{ID: "stalled", URL: ts.URL + "/speedtest/upload.php", Context: ookla.New()}
	done := make(chan struct{})
	go func() { defer close(done); racePing(context.Background(), s) }()
	select {
	case <-done:
	case <-time.After(cityPingTimeout + 5*time.Second):
		t.Fatal("a stalled candidate held the race open past its own timeout")
	}
}

// The end-to-end consequence, through the real race and the real probes: a city
// whose server's probe set holds one outlier must still beat a city with a
// steadier but genuinely slower server. This is the live failure in miniature -
// a 5ms server lost to an 18ms one because its mean (34.5ms) was bigger.
func TestRaceCitiesRanksOnTheFloorSoAnOutlierCannotFlipIt(t *testing.T) {
	// These two numbers are load-bearing together, and the test is worthless if
	// either drifts. The spiky city must win on the FLOOR while losing on the
	// MEAN - that gap is the whole subject - so with 10 fast probes plus one
	// outlier X against a steady D:
	//
	//	floor picks spiky:  loopback < D
	//	mean picks steady:  (10*loopback + X)/11 > D
	//
	// The original 18ms/400ms satisfied both on a fast loopback and neither on a
	// slow one: a Windows runner's ~15.6ms timer granularity swallowed an 18ms
	// gap, and the steady city won a race this test exists to prove it loses.
	// Widening D alone is worse than the bug - at 200ms the spiky mean (~38ms)
	// beats steady too, so the test passes without ever exercising the floor.
	// 100ms against 2000ms holds both inequalities from ~2ms loopback to ~30ms.
	steadyDelay := 100 * time.Millisecond
	spiky := &pingProbes{delays: func() []time.Duration {
		d := make([]time.Duration, 11)
		d[5] = 2000 * time.Millisecond
		return d
	}()}
	steady := &pingProbes{delays: func() []time.Duration {
		d := make([]time.Duration, 11)
		for i := range d {
			d[i] = steadyDelay
		}
		return d
	}()}
	sts := httptest.NewServer(http.HandlerFunc(spiky.handler))
	t.Cleanup(sts.Close)
	dts := httptest.NewServer(http.HandlerFunc(steady.handler))
	t.Cleanup(dts.Close)

	stubOriginPools(t, map[string]ookla.Servers{
		"exit": {{ID: "steady", Sponsor: "Steady", Name: "FarCity", URL: dts.URL + "/speedtest/upload.php", Context: ookla.New()}},
		"isp":  {{ID: "spiky", Sponsor: "Spiky", Name: "Nearby", URL: sts.URL + "/speedtest/upload.php", Context: ookla.New()}},
	})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Lat: 21.16, Lon: -86.85, Anchored: true},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok {
		t.Fatal("race returned no winner")
	}
	if win.Kind != "isp" {
		t.Fatalf("winner = %q; want isp - its server's floor is near zero against a steady %v, "+
			"and one stalled probe of ten must not hand the race to the slower city",
			win.Kind, steadyDelay)
	}
}

// End-to-end, through the real race and the real probes: a city whose server
// answered the list fetch and then went silent must LOSE to a city whose server
// answers the race, even though the library handed us the dead one carrying a
// flattering fetch-time echo. Measured before the fix: the dead city won.
func TestRaceCitiesIgnoresACityScoredOnlyByTheFetchEcho(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // answered the fetch echo; stalls every race probe
	}))
	t.Cleanup(dead.Close)
	live := &pingProbes{delays: func() []time.Duration {
		d := make([]time.Duration, 11)
		for i := range d {
			d[i] = 25 * time.Millisecond
		}
		return d
	}()}
	lts := httptest.NewServer(http.HandlerFunc(live.handler))
	t.Cleanup(lts.Close)

	stubOriginPools(t, map[string]ookla.Servers{
		// 1ms fetch echo, then silence - exactly what the library leaves behind.
		"exit": {{ID: "dead", Sponsor: "Dead", Name: "DeadCity", URL: dead.URL + "/speedtest/upload.php",
			Latency: time.Millisecond, Context: ookla.New()}},
		"isp": {{ID: "live", Sponsor: "Live", Name: "OkCity", URL: lts.URL + "/speedtest/upload.php",
			Latency: 50 * time.Millisecond, Context: ookla.New()}},
	})

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Lat: 21.16, Lon: -86.85, Anchored: true},
		}
	}
	win, ok := o.raceCities(context.Background())
	if !ok {
		t.Fatal("race returned no winner")
	}
	if win.Kind != "isp" {
		t.Fatalf("winner = %q; want isp. The exit city's server answered nothing in the race and "+
			"won on the single echo the list fetch left on it", win.Kind)
	}
}
