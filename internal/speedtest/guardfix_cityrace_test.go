package speedtest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// stubRawOriginFetch swaps the INNER city-race fetch - the one production
// wraps with the proxied-destination filter - so calls through the production
// fetchOriginServers var exercise the real splice, not a test replacement of
// it.
func stubRawOriginFetch(t *testing.T, pool ookla.Servers, err error) {
	t.Helper()
	old := rawOriginFetch
	rawOriginFetch = func(context.Context, Origin) (ookla.Servers, error) { return pool, err }
	t.Cleanup(func() { rawOriginFetch = old })
}

// hostileCityPool is a catalogue as a compromised list endpoint would shape it
// for the city race: an entry whose Host names internal space, one honest
// public entry, and one wearing a benign Host over a hostile URL - the URL is
// what racePing GETs, and this path never applies the currentEndpoints rewrite
// that elsewhere makes the two name the same endpoint. IP literals throughout
// so no verdict depends on DNS.
func hostileCityPool() ookla.Servers {
	return ookla.Servers{
		{ID: "internal-host", Host: "127.0.0.1:9000", URL: "http://127.0.0.1:9000/speedtest/upload.php"},
		{ID: "public", Host: "93.184.216.34:8080", URL: "http://93.184.216.34:8080/speedtest/upload.php"},
		{ID: "hostile-url", Host: "93.184.216.34:8080", URL: "http://169.254.169.254/speedtest/upload.php"},
	}
}

// The city race must fetch its pools through the proxied-destination filter in
// production. Identity, not behaviour: guardedFetchOriginServers is a named
// function precisely so the wiring itself is assertable, and this is the check
// that fails if the init splice is ever dropped.
func TestCityRaceFetchIsGuardWrapped(t *testing.T) {
	if rawOriginFetch == nil {
		t.Fatal("rawOriginFetch was never captured - the init splice is gone")
	}
	got := reflect.ValueOf(fetchOriginServers).Pointer()
	want := reflect.ValueOf(guardedFetchOriginServers).Pointer()
	if got != want {
		t.Fatal("fetchOriginServers is not guardedFetchOriginServers - the city race fetches unguarded pools")
	}
}

// With a proxy configured, the wrapped fetch must drop every entry whose
// logical destination - Host OR URL - names internal space, and must stay a
// pure pass-through without one. Driven through the production var so the
// whole splice is under test.
func TestCityRaceFetchFiltersProxiedInternalDestinations(t *testing.T) {
	clearProxyEnv(t)
	ctx := context.Background()
	stubRawOriginFetch(t, hostileCityPool(), nil)

	got, err := fetchOriginServers(ctx, Origin{Kind: "exit"})
	if err != nil {
		t.Fatalf("unproxied fetch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("no-proxy filter dropped %d entrie(s), want 0 - direct dials are covered at dial time", 3-len(got))
	}

	t.Setenv("HTTPS_PROXY", "http://192.168.1.10:3128")
	got, err = fetchOriginServers(ctx, Origin{Kind: "exit"})
	if err != nil {
		t.Fatalf("proxied fetch: %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if len(got) != 1 || got[0].ID != "public" {
		t.Fatalf("proxied fetch kept %v, want only [public]", ids)
	}
}

// A dead origin must still read as dead through the splice: raceOrigins keys
// its one-dead-origin handling on the fetch error, so the filter may not eat
// it.
func TestCityRaceFetchPropagatesErrors(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://192.168.1.10:3128")
	stubRawOriginFetch(t, nil, errors.New("origin list unavailable"))
	if _, err := fetchOriginServers(context.Background(), Origin{Kind: "exit"}); err == nil {
		t.Fatal("fetch error was swallowed by the guard splice")
	}
}

// End to end through the production path the verifier flagged: raceOrigins ->
// fetchOriginServers -> racePing. With a proxy configured, no hostile entry
// may receive a ranking probe, and the race must still complete on the honest
// remainder.
func TestRaceOriginsNeverPingsGuardedDestinations(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://192.168.1.10:3128")
	stubRawOriginFetch(t, hostileCityPool(), nil)

	var mu sync.Mutex
	pinged := map[string]int{}
	oldPing := racePing
	racePing = func(_ context.Context, s *ookla.Server) {
		mu.Lock()
		pinged[s.ID]++
		mu.Unlock()
		s.Latency = 5 * time.Millisecond
	}
	t.Cleanup(func() { racePing = oldPing })

	o := NewOokla()
	win, ok := o.raceOrigins(context.Background(), []Origin{{Kind: "exit", Anchored: true, Lat: 1, Lon: 2}})
	if !ok || !win.Anchored {
		t.Fatalf("race returned (%+v, %v), want the anchored origin to win on the surviving server", win, ok)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"internal-host", "hostile-url"} {
		if n := pinged[id]; n != 0 {
			t.Errorf("guarded entry %q was ping-probed %d time(s) through the proxy", id, n)
		}
	}
	if pinged["public"] == 0 {
		t.Error("the honest public entry was never pinged - the filter over-blocked")
	}
}
