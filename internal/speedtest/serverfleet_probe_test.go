package speedtest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Fleet probe: aggregate Ookla's server list and check that the URL our code
// builds actually accepts an upload, across the real world rather than one
// developer's nearest servers.
//
// It exists because of issues #17/#18. Ookla is migrating servers behind
// *.prod.hosts.ooklaserver.net, and the list's legacy `url` field keeps pointing
// at the OLD hostname, which now answers the upload POST with a 307. Go will not
// follow that redirect for a POST (the body is not replayable), so uploads fail
// against every migrated server - silently as an inflated number on
// speedtest-go v1.7.10, loudly as "server returned N/A" on v1.7.11. The `host`
// field carries the current location, and building the URL from it works in both
// states. This probe is what stops that regressing, and what tells us when
// Ookla changes the shape of the list again.
//
// Opt-in: it talks to hundreds of third-party servers, so it never runs in the
// normal test job. FLEET_PROBE=1 enables it; the CI workflow schedules it.

const (
	// fleetProbeBody is deliberately tiny. A 1 KB POST returns the SAME status as
	// the ~1 MB chunk production sends (verified against migrated, non-migrated
	// and redirecting hosts), so the whole fleet costs ~1 MB of egress instead of
	// ~1 GB. We are guests on someone else's infrastructure.
	fleetProbeBodyBytes = 1024
	fleetProbeWorkers   = 16
	fleetProbeTimeout   = 20 * time.Second

	// fleetMinSuccess is a CANARY, not the real gate. A first run measured 86.4%,
	// with ~14% of servers answering 500 to the legacy upload path. Those servers
	// are NOT broken: the official Ookla CLI measures them fine (verified against
	// Frontier 14236, Windstream 18401 and Charter 16976 - all 500 here, all
	// 170-290 Mbps up on the real client). /speedtest/upload.php belongs to the
	// OPTIONAL "HTTP Legacy Fallback" bundle an operator installs beside the
	// OoklaServer daemon; the daemon's own protocol needs none of it, and the
	// modern client never touches it. So a 500 means the component was never
	// installed or its web server rotted.
	//
	// This is attrition, NOT a deprecation programme - measured 2026-08-11 over a
	// random 300-server split, MIGRATED servers were HEALTHIER (12.0% failing)
	// than non-migrated ones (18.0%), which is the opposite of what a planned
	// retirement would look like. So do not read a falling number here as Ookla
	// switching the protocol off; read it as more operators letting an optional
	// component lapse.
	//
	// A floor set near the observed rate would flap; this is deliberately loose
	// and only catches total collapse. The real gate is the RELATIVE assertion
	// below.
	fleetMinSuccess = 0.60
)

// fleetProbeCities seeds the sweep. The list API caps at 63 results per query
// and returns the nearest to the given point, so coverage comes from asking
// from many places rather than asking for more.
var fleetProbeCities = []struct {
	name     string
	lat, lon float64
}{
	{"London", 51.51, -0.13}, {"Paris", 48.86, 2.35}, {"Berlin", 52.52, 13.40},
	{"Madrid", 40.42, -3.70}, {"Warsaw", 52.23, 21.01}, {"Istanbul", 41.01, 28.98},
	{"Lagos", 6.52, 3.38}, {"Johannesburg", -26.20, 28.05}, {"Dubai", 25.20, 55.27},
	{"Mumbai", 19.08, 72.88}, {"Singapore", 1.35, 103.82}, {"Tokyo", 35.68, 139.69},
	{"Sydney", -33.87, 151.21}, {"NewYork", 40.71, -74.01}, {"LosAngeles", 34.05, -118.24},
	{"MexicoCity", 19.43, -99.13}, {"Bogota", 4.71, -74.07}, {"SaoPaulo", -23.55, -46.63},
	{"Toronto", 43.65, -79.38}, {"Lima", -12.05, -77.04},
}

type fleetServer struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Host    string `json:"host"`
	Sponsor string `json:"sponsor"`
	CC      string `json:"cc"`
}

// migrated reports whether Ookla has moved this server behind its new hostname.
func (s fleetServer) migrated() bool {
	return strings.Contains(s.Host, "prod.hosts.ooklaserver.net")
}

// urlHost is the host embedded in the legacy `url` field.
func (s fleetServer) urlHost() string {
	u, err := url.Parse(s.URL)
	if err != nil {
		return ""
	}
	return u.Host
}

// currentUploadURL asks PRODUCTION what URL it would build, by handing
// currentEndpoint a server carrying this catalogue entry's fields. An earlier
// version rebuilt the string here, which meant breaking currentEndpoint left
// this canary - the stated safety net for an unguarded rewrite - perfectly
// green. The point is to exercise the rewrite, not to agree with it.
func (s fleetServer) currentUploadURL() string {
	srv := &ookla.Server{ID: s.ID, URL: s.URL, Host: s.Host}
	currentEndpoint(srv)
	return srv.URL
}

func fetchFleet(t *testing.T) []fleetServer {
	t.Helper()
	client := &http.Client{Timeout: 45 * time.Second}
	seen := map[string]fleetServer{}
	for _, c := range fleetProbeCities {
		u := fmt.Sprintf("https://www.speedtest.net/api/js/servers?engine=js&lat=%f&lon=%f&limit=63", c.lat, c.lon)
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("User-Agent", "speedtest-go/1.7.11")
		resp, err := client.Do(req)
		if err != nil {
			t.Logf("  %-14s list fetch failed: %v", c.name, err)
			continue
		}
		var batch []fleetServer
		err = json.NewDecoder(resp.Body).Decode(&batch)
		_ = resp.Body.Close()
		if err != nil {
			t.Logf("  %-14s decode failed: %v", c.name, err)
			continue
		}
		before := len(seen)
		for _, s := range batch {
			if _, ok := seen[s.ID]; !ok {
				seen[s.ID] = s
			}
		}
		t.Logf("  %-14s +%-4d (total %d)", c.name, len(seen)-before, len(seen))
		time.Sleep(300 * time.Millisecond) // be a polite client of the list API
	}
	out := make([]fleetServer, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// probeUpload POSTs a tiny body and returns the status code, or 0 plus the
// error text when the request never completed. Redirects are NOT followed -
// following them here would hide the exact failure production cannot recover
// from.
// is2xxCode is shared so the probe loop and the verdict cannot drift apart.
func is2xxCode(c int) bool { return c >= 200 && c < 300 }

func probeUpload(target string) (int, string) {
	client := &http.Client{
		Timeout:       fleetProbeTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(strings.Repeat("\xAA", fleetProbeBodyBytes)))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("User-Agent", "speedtest-go/1.7.11") // match production exactly
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, ""
}

func TestServerFleetProbe(t *testing.T) {
	if os.Getenv("FLEET_PROBE") != "1" {
		t.Skip("set FLEET_PROBE=1 to probe the live Ookla server fleet")
	}

	t.Log("=== aggregating server list ===")
	servers := fetchFleet(t)
	if len(servers) < 100 {
		t.Fatalf("only %d servers aggregated - the list API shape or access has changed", len(servers))
	}

	// ---- Structural invariants. These need no network and are the real
	// regression detector: they encode WHY currentUploadURL is correct.
	var migrated, nonMigrated int
	var badShape, badPartition []string
	for _, s := range servers {
		u, err := url.Parse(s.URL)
		if err != nil || u.Scheme != "http" || u.Path != "/speedtest/upload.php" {
			badShape = append(badShape, fmt.Sprintf("%s url=%s", s.ID, s.URL))
		}
		// The rewrite is "http://" + Host + path, so a Host attribute missing its
		// port would silently redirect every request to :80. The partition check
		// below only compares the two hosts, which would not notice.
		if _, port, splitErr := net.SplitHostPort(s.Host); splitErr != nil || port == "" {
			badShape = append(badShape, fmt.Sprintf("%s host has no port: %q", s.ID, s.Host))
		}
		if s.migrated() {
			migrated++
			// A migrated server MUST differ from its legacy url host, or the
			// rewrite would be a no-op and uploads would still hit the redirect.
			if strings.EqualFold(s.urlHost(), s.Host) {
				badPartition = append(badPartition, fmt.Sprintf("%s migrated but host==urlHost (%s)", s.ID, s.Host))
			}
		} else {
			nonMigrated++
			// A non-migrated server MUST match, which is what makes the rewrite a
			// provable no-op rather than something needing a guard.
			if !strings.EqualFold(s.urlHost(), s.Host) {
				badPartition = append(badPartition, fmt.Sprintf("%s non-migrated but host!=urlHost (%s vs %s)", s.ID, s.Host, s.urlHost()))
			}
		}
	}
	t.Logf("\n=== fleet: %d servers | migrated %d (%.1f%%) | non-migrated %d ===",
		len(servers), migrated, float64(migrated)/float64(len(servers))*100, nonMigrated)

	if len(badShape) > 0 {
		t.Errorf("STRUCTURE: %d servers are not http //host:port/speedtest/upload.php - URL building assumes that shape:\n  %s",
			len(badShape), strings.Join(capList(badShape, 10), "\n  "))
	}
	if len(badPartition) > 0 {
		t.Errorf("PARTITION BROKEN: %d servers violate the host/url invariant that makes the rewrite safe without a guard:\n  %s",
			len(badPartition), strings.Join(capList(badPartition, 10), "\n  "))
	}

	// ---- Live probe. BOTH URLs are probed: the legacy `url` field speedtest-go
	// uses today, and the `host`-derived URL we want it to use. Comparing the two
	// is what makes this robust - absolute success rate is dominated by fleet rot
	// we do not control, but the DIFFERENCE between the two is entirely about our
	// URL construction.
	type result struct {
		s                fleetServer
		code, legacyCode int
		errTxt           string
	}
	results := make([]result, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, fleetProbeWorkers)
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s fleetServer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			cur := s.currentUploadURL()
			code, errTxt := probeUpload(cur)
			legacy := code
			if cur != s.URL {
				// Only worth a second request when the rewrite actually changed
				// something. For a non-migrated entry the two URLs are identical,
				// and probing twice invented a comparison that could disagree with
				// itself on a transient failure.
				legacy, _ = probeUpload(s.URL)
			}
			if is2xxCode(legacy) && !is2xxCode(code) {
				// Retry once before calling it a regression: a single dropped
				// request against a fleet this size is routine, and this is the
				// assertion the whole job gates on.
				code, errTxt = probeUpload(cur)
			}
			results[i] = result{s, code, legacy, errTxt}
		}(i, s)
	}
	wg.Wait()

	is2xx := is2xxCode
	var ok, legacyOK, regressed int
	var failures, regressions []string
	byCode := map[int]int{}
	for _, r := range results {
		byCode[r.code]++
		if is2xx(r.legacyCode) {
			legacyOK++
		}
		if is2xx(r.code) {
			ok++
		} else {
			failures = append(failures, fmt.Sprintf("%s %s (%s) -> %d %s", r.s.ID, r.s.CC, r.s.Host, r.code, r.errTxt))
		}
		// The only genuinely damning case: legacy worked and ours did not.
		if is2xx(r.legacyCode) && !is2xx(r.code) {
			regressed++
			regressions = append(regressions,
				fmt.Sprintf("%s %s legacy=%d host=%d (%s -> %s)", r.s.ID, r.s.CC, r.legacyCode, r.code, r.s.urlHost(), r.s.Host))
		}
	}
	rate := float64(ok) / float64(len(results))
	legacyRate := float64(legacyOK) / float64(len(results))
	t.Logf("\n=== legacy url vs host-derived url ===")
	t.Logf("  legacy `url` field : %.1f%% (%d/%d)", legacyRate*100, legacyOK, len(results))
	t.Logf("  host-derived       : %.1f%% (%d/%d)", rate*100, ok, len(results))
	t.Logf("  servers the rewrite RESCUES: %d", ok-legacyOK+regressed)
	t.Logf("  servers the rewrite BREAKS : %d", regressed)

	// THE GATE. Fleet rot hits both columns equally, so this is immune to it.
	if regressed > 0 {
		t.Errorf("REWRITE REGRESSION: %d servers accept the legacy URL but reject the host-derived one - "+
			"the host field is not a safe substitute for those:\n  %s",
			regressed, strings.Join(capList(regressions, 15), "\n  "))
	}

	codes := make([]int, 0, len(byCode))
	for c := range byCode {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	t.Log("\n=== upload probe against host-derived URL ===")
	for _, c := range codes {
		label := fmt.Sprintf("HTTP %d", c)
		if c == 0 {
			label = "transport error"
		}
		t.Logf("  %-16s %d", label, byCode[c])
	}
	t.Logf("  success rate: %.1f%% (%d/%d)", rate*100, ok, len(results))

	if len(failures) > 0 {
		t.Logf("\n  failing servers (first 20):\n  %s", strings.Join(capList(failures, 20), "\n  "))
	}
	if rate < fleetMinSuccess {
		t.Errorf("SYSTEMIC: only %.1f%% of host-derived upload URLs accepted a POST (floor %.0f%%) - "+
			"either Ookla changed the endpoint contract or our URL construction is wrong",
			rate*100, fleetMinSuccess*100)
	}

	// Informational: how far migration has spread. Not an assertion - it is the
	// number that tells us how urgent the fix is and how stale a rollback gets.
	t.Logf("\nMIGRATION WATCH: %.1f%% of the sampled fleet is behind prod.hosts.ooklaserver.net",
		float64(migrated)/float64(len(servers))*100)
}

func capList(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	out := append([]string{}, in[:n]...)
	return append(out, fmt.Sprintf("... and %d more", len(in)-n))
}
