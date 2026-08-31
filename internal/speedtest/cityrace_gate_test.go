package speedtest

import (
	"context"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The race is how AUTO finds its centre - it must never fire when the user has
// already said where to test. A pinned server bypasses the race entirely, or
// the user could state where they want to test from and be overruled by a
// tie-break between coordinates they never chose.
func TestRunReasonSkipsTheRaceWhenPinned(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	fetches := stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("e1", 1)}})
	stubRacePing(t, map[string]int{"e1": 5})

	origins := func() []Origin {
		return []Origin{{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}

	// Pinned: the pin is the only target; racing would spend fetches and pings
	// on a centre nothing uses.
	o := NewOokla()
	o.OriginsFn = origins
	o.ServerIDFn = func() string { return "2" } // resolved from the stubbed list, no network
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	if n := fetches.fetches(); n != 0 {
		t.Fatalf("pinned run fetched %d origin pools, want 0 - a pin overrides the race", n)
	}
}

// With nothing pinned, the run races and the fetch is
// centred on the winning city's coordinate. The registered "auto" location is
// how the coordinate reaches the Ookla API (newAnchoredLocation writes the
// library's Locations map), so it is where the mapping is observable.
func TestRunReasonCentresTheFetchOnTheRacedWinner(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	fetches := stubOriginPools(t, map[string]ookla.Servers{
		"exit": {srv("e1", 1)},
		"isp":  {srv("i1", 1)},
	})
	stubRacePing(t, map[string]int{"e1": 30, "i1": 4})

	forgetLocation(t, "auto")

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "isp", Label: "Oldtown, XX", Lat: 12.34, Lon: -76.54, Anchored: true},
		}
	}
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if n := fetches.fetches(); n != 2 {
		t.Fatalf("auto run fetched %d origin pools, want 2", n)
	}
	loc := registeredLocation("auto")
	if loc == nil {
		t.Fatal("auto run registered no centre; the raced winner never reached the fetch")
	}
	if loc.Lat != 12.34 || loc.Lon != -76.54 {
		t.Fatalf("centre = %v,%v; want the winning city's 12.34,-76.54 (isp), not the losing exit's", loc.Lat, loc.Lon)
	}
}

// A run that measures its centre must not pay for it out of the measurement's
// budget. runBudget's arithmetic is exact and predates the race - at want=1 the
// selection plus both transfer attempts already equal ooklaRunTimeout, and at
// want=3 the three server slices plus selection already equal the total - so
// spending the race from it finishes the run short. Measured before the fix: a
// raced best-of-3 gave its third server 80s of the 90s it is designed to get.
//
// It reads off the deadline the RUN actually creates, not a copy of the rule
// that sizes it: the fetch that follows selection carries the run context, so
// its deadline is the whole budget. Recomputing it here instead passed with the
// allowance deleted from RunReason.
func TestRunBudgetCarriesTheRaceWhenTheRunMustMeasureItsCentre(t *testing.T) {
	requireQuiet(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	stubOriginPools(t, map[string]ookla.Servers{"exit": {srv("e1", 1)}})
	// Nobody answers, so the race goes SILENT and the run fetches its list
	// itself - which is where the deadline is read off below. A decided race
	// hands the run its own list instead (see raceResult.Field) and the fetch
	// seam never runs; the allowance is sized before either outcome, so the
	// silent race is the same arithmetic with the probe still reachable.
	stubRacePing(t, map[string]int{})

	var runDeadline time.Time
	old := fetchServerList
	fetchServerList = func(ctx context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		runDeadline, _ = ctx.Deadline()
		mk := func(id string, dist float64) *ookla.Server {
			return &ookla.Server{ID: id, URL: "http://127.0.0.1:1/speedtest/upload.php",
				Lat: "52.1", Lon: "4.1", Sponsor: "S" + id, Name: "N" + id, Distance: dist, Context: client}
		}
		return ookla.Servers{mk("1", 1), mk("2", 2), mk("3", 3)}, nil
	}
	t.Cleanup(func() { fetchServerList = old })

	origins := func() []Origin {
		return []Origin{{Kind: "exit", Lat: 25.76, Lon: -80.19, Anchored: true}}
	}
	cases := []struct {
		name   string
		setup  func(*Ookla)
		reason string
		want   int
		races  bool
		// An unpinned single-server run also carries fallbackBudget, for the
		// fallback target pickServers may seat behind its head. Like the race
		// allowance it is added ON TOP, and only a fallback may spend it (the
		// head's slice is held to the deadline minus this), so a run that
		// seats none still finishes inside the base budget.
		fallback bool
	}{
		{"auto run races and is allowed for it",
			func(o *Ookla) { o.OriginsFn = origins }, "", 1, true, true},
		{"pinned without best-of never races",
			func(o *Ookla) {
				o.OriginsFn = origins
				o.ServerIDFn = func() string { return "2" }
			}, "", 1, false, false},
		{"no origins wired: nothing to race",
			func(o *Ookla) {}, "", 1, false, true},
		// Not a boot transient: with connection-info lookups off, every auto run
		// on the box is this one, so granting it the allowance would loosen the
		// deadline of every run forever for a race that short-circuits.
		{"origins wired but nothing anchored: the race decides nothing",
			func(o *Ookla) {
				o.OriginsFn = func() []Origin { return []Origin{{Kind: "geo", Label: "your connection"}} }
			}, "", 1, false, true},
		{"best-of pays for the race too",
			func(o *Ookla) {
				o.OriginsFn = origins
				o.BestOfCountFn = func() int { return 3 }
			}, "manual", bestOfServers, true, false},
	}
	for _, c := range cases {
		o := NewOokla()
		c.setup(o)
		runDeadline = time.Time{} // a case whose fetch never ran must fail, not inherit the last one's reading
		start := time.Now()
		if _, err := o.RunReason(context.Background(), c.reason); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// Same retries the run itself resolved (nil RetriesFn -> the default),
		// or the baseline would not be the one production sized against.
		_, base := runBudget(speedRetries(o.RetriesFn), c.want)
		want := base
		if c.races {
			want += cityRaceBudget
		}
		if c.fallback {
			want += fallbackBudget
		}
		got := runDeadline.Sub(start)
		// Everything between start and the deadline is stubbed, so the window is
		// tight on purpose: at 10s of slack a third of the allowance could go
		// missing and this would still pass.
		if got > want+time.Second || got < want-time.Second {
			t.Errorf("%s: run deadline is %v out, want ~%v (base %v, race allowance %v, fallback allowance %v). "+
				"Too small and the race or the fallback is being spent from the measurement's budget",
				c.name, got.Round(time.Second), want, base, c.races, c.fallback)
		}
	}
}

// Which runs go and measure a centre when listCentre could not supply one.
// The pinned + best-of case is the one worth stating: listCentre declines there
// only when Ookla gave the pin no usable coordinate, and before the race
// existed that fell through to the exit-router cascade. Without this the
// companions would be drawn from the Ookla API's guess at our address - the
// placement this feature exists because it can be a country away - while the
// docstring still promised something better.
func TestShouldRaceCities(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want int
		race bool
	}{
		{"nothing pinned, single server", "", 1, true},
		{"nothing pinned, best-of", "", 3, true},
		{"pinned without best-of: the pin is the only target", "42", 1, false},
		{"pinned with best-of: its companions need a centre", "42", 3, true},
	}
	for _, c := range cases {
		if got := shouldRaceCities(c.id, c.want); got != c.race {
			t.Errorf("%s: shouldRaceCities(%q, %d) = %v, want %v", c.name, c.id, c.want, got, c.race)
		}
	}
}

// An UNANCHORED winner means "the pool the Ookla API picks for our address won",
// so the fetch must stay uncentred. Writing that winner as a coordinate would
// centre it on 0,0 - a point in the Gulf of Guinea - and fetch West-African
// servers for every user whose geo city wins, which is the common CGNAT or
// tunnelled-link case and the ONLY origin when no coordinate is known at all.
func TestRunReasonLeavesTheFetchUncentredWhenTheGeoCityWins(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "srv", ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 10, PingMS: 9}, nil
	})
	stubOriginPools(t, map[string]ookla.Servers{
		"exit": {srv("e1", 1)},
		"geo":  {srv("g1", 1)},
	})
	stubRacePing(t, map[string]int{"e1": 30, "g1": 4}) // the geo city wins

	forgetLocation(t, "auto")

	o := NewOokla()
	o.OriginsFn = func() []Origin {
		return []Origin{
			{Kind: "exit", Label: "Miami, US", Lat: 25.76, Lon: -80.19, Anchored: true},
			{Kind: "geo", Label: "your connection"},
		}
	}
	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("auto run: %v", err)
	}
	if loc := registeredLocation("auto"); loc != nil {
		t.Fatalf("an unanchored winner registered a centre at %v,%v; the fetch must stay uncentred "+
			"so the Ookla API places our address itself", loc.Lat, loc.Lon)
	}
}

// forgetLocation clears a key from the library's Locations map, and clears it
// again at the end of the test. Under ooklaClientMu, because that map is the
// unsynchronized package-global newAnchoredLocation exists to serialize (see
// there): a test reaching around the lock is the one concurrent access the
// production code is careful never to make.
func forgetLocation(t *testing.T, name string) {
	t.Helper()
	drop := func() {
		ooklaClientMu.Lock()
		defer ooklaClientMu.Unlock()
		delete(ookla.Locations, name)
	}
	drop()
	t.Cleanup(drop)
}

// registeredLocation reads a key back under the same lock.
func registeredLocation(name string) *ookla.Location {
	ooklaClientMu.Lock()
	defer ooklaClientMu.Unlock()
	return ookla.Locations[name]
}
