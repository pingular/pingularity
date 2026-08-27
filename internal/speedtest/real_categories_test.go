package speedtest

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The three states a catalogue entry can be in, one real server each, end to
// end. Opt-in (PROBE_REAL=1) because it talks to third-party infrastructure.
//
//	OLD       not migrated - Host equals the url's host, the rewrite is a no-op
//	MIGRATED  behind prod.hosts.ooklaserver.net - the legacy url 307s, Host works
//	NOFALLBACK the optional HTTP Legacy Fallback is absent - 500 on the bundle,
//	           while the server is fine for Ookla's own client
//
// Each case re-verifies its category against the live server FIRST and skips if
// the world moved, so a fixed or newly-broken server never turns this red. The
// categories are the contract; the specific hosts are only witnesses.
type realCategory struct {
	name      string
	id        string
	legacyURL string // the catalogue's `url` field
	host      string // the catalogue's `host` field
	wantState endpointState
	migrated  bool
}

var realCategories = []realCategory{
	{"OLD/ebox", "1993",
		"http://speedtest.ebox.ca:8080/speedtest/upload.php",
		"speedtest.ebox.ca:8080", endpointOK, false},
	{"OLD/rogers", "46433",
		"http://stpickering.rogers.com:8080/speedtest/upload.php",
		"stpickering.rogers.com:8080", endpointOK, false},
	{"MIGRATED/b4rn-issue17", "59030",
		"http://speedtest3.b4rn.org.uk:8080/speedtest/upload.php",
		"speedtest3.b4rn.org.uk.prod.hosts.ooklaserver.net:8080", endpointOK, true},
	{"MIGRATED/expertos-issue18", "72887",
		"http://speedtest.xpert-tic.com:8080/speedtest/upload.php",
		"speedtest.xpert-tic.com.prod.hosts.ooklaserver.net:8080", endpointOK, true},
	{"NOFALLBACK/frontier", "14236",
		"http://losangeles.ca.speedtest.frontier.com:8080/speedtest/upload.php",
		"losangeles.ca.speedtest.frontier.com:8080", endpointRetired, false},
	{"NOFALLBACK/windstream", "18401",
		"http://la02.speedtest.windstream.net:8080/speedtest/upload.php",
		"la02.speedtest.windstream.net:8080", endpointRetired, false},
}

func (c realCategory) server() *ookla.Server {
	return &ookla.Server{ID: c.id, Sponsor: c.name, Name: c.name, URL: c.legacyURL, Host: c.host}
}

// liveState asks the server directly, bypassing the cache, so the category
// check cannot be satisfied by a stale verdict.
func liveState(t *testing.T, host string) endpointState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+host+"/speedtest/latency.txt", nil)
	if err != nil {
		return endpointUnknown
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return endpointUnknown
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return endpointOK
	}
	return endpointRetired
}

// discoverWitness finds a LIVE server in the wanted state from the catalogue,
// so this suite keeps covering a category after Ookla repairs (or breaks) the
// hardcoded example. Without it the coverage silently decays into skips - the
// hardcoded hosts are witnesses, and witnesses move.
func discoverWitness(t *testing.T, want endpointState, wantMigrated bool) *ookla.Server {
	t.Helper()
	// Look locally first, then further afield. The catalogue is geo-sorted, and a
	// healthy metro yields no no-fallback witness at all: measured from Toronto,
	// the 40 nearest servers were all fine while Frontier LA, Windstream and
	// Claro BR were not. Searching only nearby would quietly lose that category.
	origins := append([]struct {
		name     string
		lat, lon float64
	}{{"local", 0, 0}}, fleetProbeCities...)

	for _, o := range origins {
		uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent}
		if o.name != "local" {
			uc.Location = newAnchoredLocation("probe", o.lat, o.lon)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		servers, err := newOoklaClient(uc).FetchServerListContext(ctx)
		cancel()
		if err != nil || len(servers) == 0 {
			continue
		}
		const maxProbe = 30 // bounded: discovery must not become a fleet sweep
		if len(servers) > maxProbe {
			servers = servers[:maxProbe]
		}
		type found struct {
			s  *ookla.Server
			st endpointState
		}
		results := make([]found, len(servers))
		var wg sync.WaitGroup
		sem := make(chan struct{}, 12)
		for i, srv := range servers {
			wg.Add(1)
			go func(i int, srv *ookla.Server) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i] = found{srv, liveState(t, srv.Host)}
			}(i, srv)
		}
		wg.Wait()
		for _, r := range results {
			if r.st != want {
				continue
			}
			if strings.Contains(r.s.Host, "prod.hosts.ooklaserver.net") != wantMigrated {
				continue
			}
			t.Logf("discovery: found via %s", o.name)
			return r.s
		}
	}
	return nil
}

// settledHealth drives fallbackHealth to a STABLE verdict. A definite failure
// takes fallbackStrikes consecutive probes to retire a server - deliberately,
// so a transient 429/503 cannot exclude a healthy one for 12h - which means a
// single call reports unknown for a server that really has no fallback. The
// consequence in production is that such a server survives one ranking pass
// before being excluded; here we just drive past it.
func settledHealth(t *testing.T, srv *ookla.Server) endpointState {
	t.Helper()
	var st endpointState
	for i := 0; i < fallbackStrikes+1; i++ {
		st = fallbackHealth(context.Background(), srv)
		if st == endpointOK || st == endpointRetired {
			if st == endpointRetired || i > 0 {
				return st
			}
		}
		fbMu.Lock() // expire the short hold so the next call re-probes
		if v, ok := fbMap[srv.ID]; ok {
			v.expires = time.Now().Add(-time.Second)
			fbMap[srv.ID] = v
		}
		fbMu.Unlock()
	}
	return st
}

func requireProbeReal(t *testing.T) {
	t.Helper()
	if os.Getenv("PROBE_REAL") != "1" {
		t.Skip("set PROBE_REAL=1 to probe live Ookla servers")
	}
}

// TestRealCategoriesClassified: the health oracle must put each real server in
// the right bucket, since every guard decision downstream rests on it.
func TestRealCategoriesClassified(t *testing.T) {
	requireProbeReal(t)
	for _, c := range realCategories {
		t.Run(c.name, func(t *testing.T) {
			srv := c.server()
			if live := liveState(t, c.host); live != c.wantState {
				// The witness moved. Find a current one rather than lose the
				// category - a repaired server must not quietly delete coverage.
				t.Logf("witness %s is now %v (was %v); discovering a replacement",
					c.host, live, c.wantState)
				srv = discoverWitness(t, c.wantState, c.migrated)
				if srv == nil {
					t.Skipf("no live %v/migrated=%v server found in the sampled catalogue",
						c.wantState, c.migrated)
				}
				t.Logf("using discovered witness %s", srv.Host)
			}
			fbMu.Lock()
			delete(fbMap, srv.ID) // force a real probe, not a cached verdict
			fbMu.Unlock()

			got := settledHealth(t, srv)
			if got != c.wantState {
				t.Fatalf("fallbackHealth = %v, want %v for %s", got, c.wantState, srv.Host)
			}
			t.Logf("classified %s as %v", srv.Host, got)
		})
	}
}

// TestRealCategoriesEndpointRewrite: the rewrite must fire on migrated entries
// and be a provable no-op on the rest. This is pure logic over REAL catalogue
// field values, so it needs no network.
func TestRealCategoriesEndpointRewrite(t *testing.T) {
	for _, c := range realCategories {
		t.Run(c.name, func(t *testing.T) {
			s := c.server()
			before := s.URL
			currentEndpoint(s)
			changed := s.URL != before
			if changed != c.migrated {
				t.Fatalf("rewrite changed=%v, want %v\n  before: %s\n  after:  %s",
					changed, c.migrated, before, s.URL)
			}
			u, err := url.Parse(s.URL)
			if err != nil || u.Host != c.host || u.Path != "/speedtest/upload.php" {
				t.Fatalf("rewritten URL is malformed: %s", s.URL)
			}
		})
	}
}

// TestRealCategoriesRankingGuard: ranking must drop the real no-fallback servers
// and keep the real working ones - the behaviour the stubbed guard tests assert,
// here against live verdicts.
func TestRealCategoriesRankingGuard(t *testing.T) {
	requireProbeReal(t)

	var servers ookla.Servers
	var wantDropped, wantKept []string
	for i, c := range realCategories {
		if live := liveState(t, c.host); live != c.wantState {
			t.Skipf("category moved: %s is now %v - update the witness", c.host, live)
		}
		s := c.server()
		s.Distance = float64(i + 1)
		servers = append(servers, s)
		if c.wantState == endpointRetired {
			wantDropped = append(wantDropped, c.id)
		} else {
			wantKept = append(wantKept, c.id)
		}
	}
	if len(wantDropped) == 0 || len(wantKept) == 0 {
		t.Skip("need at least one live server on each side to be meaningful")
	}

	// Real health verdicts; only the ranking ping is stubbed, since latency is
	// not what this asserts.
	oldPing := ooklaPing
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		s.Latency = 10 * time.Millisecond
		return nil
	}
	t.Cleanup(func() { ooklaPing = oldPing })
	// Prime each verdict to its settled state first: rankedServers probes once
	// per pass, and a first-strike unknown is deliberately kept rather than
	// excluded, so a single pass would not yet have dropped anything.
	for _, c := range realCategories {
		fbMu.Lock()
		delete(fbMap, c.id)
		fbMu.Unlock()
	}
	for _, srv := range servers {
		settledHealth(t, srv)
	}

	ranked, _, dropped, _ := rankedServers(context.Background(), servers, "")
	got := fbIDs(ranked)
	for _, id := range wantDropped {
		if fbContains(got, id) {
			t.Errorf("ranked %s, which has no HTTP legacy fallback: %v", id, got)
		}
		if !fbContains(dropped, id) {
			t.Errorf("dropped list missing %s: %v", id, dropped)
		}
	}
	for _, id := range wantKept {
		if !fbContains(got, id) {
			t.Errorf("dropped healthy server %s: %v", id, got)
		}
	}
	t.Logf("ranked %v, dropped %v", got, dropped)
}

// TestRealCategoriesPinSurvives: a user who searches out a broken server by ID
// still gets it. The guard governs automatic selection only.
func TestRealCategoriesPinSurvives(t *testing.T) {
	requireProbeReal(t)
	var dead *realCategory
	for i := range realCategories {
		if realCategories[i].wantState == endpointRetired &&
			liveState(t, realCategories[i].host) == endpointRetired {
			dead = &realCategories[i]
			break
		}
	}
	if dead == nil {
		t.Skip("no live no-fallback witness available")
	}

	oldPing := ooklaPing
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		s.Latency = 10 * time.Millisecond
		return nil
	}
	t.Cleanup(func() { ooklaPing = oldPing })

	var servers ookla.Servers
	for i, c := range realCategories {
		s := c.server()
		s.Distance = float64(i + 1)
		servers = append(servers, s)
	}
	o := &Ookla{}
	targets, _, _, err := o.pickServers(context.Background(), nil, servers, dead.id, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fbIDs(targets); len(got) != 1 || got[0] != dead.id {
		t.Fatalf("pinning %s by ID must be honoured, got %v", dead.id, got)
	}
	t.Logf("pin of no-fallback server %s honoured", dead.id)
}
