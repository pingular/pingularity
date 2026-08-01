package netinfo

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The coordinate the eyeball geo provider returns has to reach the published
// snapshot, because that coordinate is an ORIGIN in the auto server-selection
// race and the only one that describes where the SUBSCRIBER is. Both provider
// structs used to declare City/Country only and drop lat/lon on the floor, and
// the cost was not a missing label: Ookla's server list is fetched around a
// coordinate and the lists around different coordinates are disjoint, so the
// nearer servers were never fetched at all.
//
// The figures are the ones measured live for 203.0.113.79. No network: the
// canned clients answer the IP echo and the geo provider, and the cancelled
// context fails the DNS-based lookups instantly.
func TestFetchCarriesTheGeoCoordinate(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.79")
	ipv6Client = canned(500, "")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(200, `{"success":true,"city":"Oldtown","country_code":"XX","latitude":12.345678,"longitude":-76.543210}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.City != "Oldtown" || info.Country != "XX" {
		t.Fatalf("geo = (%q, %q), want (Oldtown, XX)", info.City, info.Country)
	}
	if info.Lat != 12.345678 || info.Lon != -76.543210 {
		t.Fatalf("coordinate = %v,%v, want 12.345678,-76.543210 - the provider returns it and the snapshot must carry it",
			info.Lat, info.Lon)
	}
}

// The per-IP cache reuses the previous snapshot's geo rather than re-querying,
// so the coordinate has to ride that path too - otherwise the origin exists for
// exactly one fetch after an IP change and silently vanishes on every fetch
// after it, which is worse than never having had it.
func TestFetchCarriesTheGeoCoordinateThroughTheIPCache(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.79")
	ipv6Client = canned(500, "")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	// Same IP and a non-empty ISP: exactly the state that takes fetch's cache-hit
	// branch and skips the geo lookup entirely.
	m.mu.Lock()
	m.info = Info{
		PublicIP: "203.0.113.79", ISP: "AS64496 Example Telecom",
		City: "Oldtown", Country: "XX", Lat: 12.345678, Lon: -76.543210,
		UpdatedAt: time.Now().Unix(),
	}
	m.mu.Unlock()
	m.http = canned(500, "") // any geo query would fail; the cache must supply it
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.Lat != 12.345678 || info.Lon != -76.543210 {
		t.Fatalf("coordinate after a cache hit = %v,%v, want the carried 12.345678,-76.543210", info.Lat, info.Lon)
	}
}

// countingGeo answers every request with the same body and counts the ones
// aimed at an eyeball geo provider, so a test can prove that lookup was made -
// or was skipped. The manager's client serves other lookups too (the DNS egress
// location), which is why the count is filtered by host rather than total.
// The counter is guarded because this transport backs the manager's shared
// client, which fetchGen drives from several concurrent lookup goroutines.
type countingGeo struct {
	mu   sync.Mutex
	n    int
	body string
}

func (c *countingGeo) RoundTrip(r *http.Request) (*http.Response, error) {
	if h := r.URL.Hostname(); h == "ipwho.is" || h == "get.geojs.io" {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(c.body)), Header: http.Header{}}, nil
}

// count reads the tally under the same lock.
func (c *countingGeo) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// A CACHED CITY WITH NO COORDINATE MUST NOT LOCK THE ISP ORIGIN OUT FOR THE
// LIFE OF THE IP. The cache-hit branch used to re-query only while the city was
// blank, which was right when the city was the only thing being cached and
// wrong once the coordinate became load-bearing.
//
// The route in is structural, not exotic: the persisted last-known identity is
// built from a speed sample, which has no coordinate columns at all, so it can
// only ever restore a city at 0,0. One failed IP echo publishes that, and every
// later fetch on the same IP then took the cache branch and skipped geo - so
// the ISP city, the origin this feature exists to add, silently never raced
// again.
func TestFetchRetriesGeoWhenTheCachedCityHasNoCoordinate(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.79")
	ipv6Client = canned(500, "")

	geo := &countingGeo{body: `{"success":true,"city":"Oldtown","country_code":"XX","latitude":12.345678,"longitude":-76.543210}`}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = &http.Client{Transport: geo}
	// Exactly what the persisted fallback restores: a city, an ISP, no position.
	m.mu.Lock()
	m.info = Info{
		PublicIP: "203.0.113.79", ISP: "AS64496 Example Telecom",
		City: "Oldtown, XX", UpdatedAt: time.Now().Unix(),
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.Lat != 12.345678 || info.Lon != -76.543210 {
		t.Fatalf("coordinate = %v,%v after a cache hit on a city with no position; the ISP city is "+
			"dropped from the race until the public IP changes", info.Lat, info.Lon)
	}
	if geo.count() != 1 {
		t.Fatalf("geo queries = %d, want exactly 1 recovery lookup", geo.count())
	}

	// ...and the retry stops as soon as it succeeds: the next fetch is served
	// from the cache, so a recovered coordinate costs one query, not one per
	// fetch forever.
	m.mu.Lock()
	m.info = info
	m.mu.Unlock()
	if info2 := m.fetch(ctx); geo.count() != 1 || info2.Lat != 12.345678 {
		t.Errorf("geo queries = %d and coordinate = %v after the cache held a position; "+
			"the retry must stop once it has one", geo.count(), info2.Lat)
	}
}

// The IPv6 half of the SAME gate, which the IPv4 test above only claimed. An
// IPv6-only host reuses its own cached identity, so a city cached without a
// position locks the ISP origin out of the race on that branch too - and
// reverting that gate alone left the whole repo suite green.
func TestFetchRetriesGeoOnIPv6WhenTheCachedCityHasNoCoordinate(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(500, "")
	ipv6Client = canned(200, "2001:db8::1234")

	geo := &countingGeo{body: `{"success":true,"city":"Sixtown","country_code":"NL","latitude":52.3676,"longitude":4.9041}`}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = &http.Client{Transport: geo}
	// An IPv6-only identity already published: city and ISP, no position. No
	// persisted IPv4 exists (LastKnownFn nil), so the IPv6-only branch is taken
	// immediately rather than waiting out v6FlipAfter.
	m.mu.Lock()
	m.info = Info{
		PublicIPv6: "2001:db8::1234", ISP: "AS64499 SIXNET",
		City: "Sixtown", UpdatedAt: time.Now().Unix(),
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.PublicIPv6 != "2001:db8::1234" || info.PublicIP != "" {
		t.Fatalf("ips = (%q, %q); the IPv6-only branch was not taken", info.PublicIP, info.PublicIPv6)
	}
	if info.Lat != 52.3676 || info.Lon != 4.9041 {
		t.Fatalf("coordinate = %v,%v after a cache hit on an IPv6 city with no position; "+
			"the ISP city is dropped from the race until the IPv6 changes", info.Lat, info.Lon)
	}
	if geo.count() != 1 {
		t.Fatalf("geo queries = %d, want exactly 1 recovery lookup", geo.count())
	}
}

// AND THE RECOVERY MUST BE BOUNDED. A provider that names a city it cannot
// place has given its final answer, so re-asking buys nothing - but the gate
// that spots "no coordinate" is still true afterwards, so without a per-IP
// claim it fires on every fetch: one keyless third-party GET per refresh, for
// the life of the IP, for a coordinate that is never coming. Providers that are
// merely unreachable cost the same. The ISP origin cannot be rescued either
// way, so the traffic buys nothing at all.
func TestFetchStopsRetryingACityTheProvidersCannotPlace(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.79")
	ipv6Client = canned(500, "")

	// Names the city, carries no position - ok=true, coordinate still 0,0.
	geo := &countingGeo{body: `{"success":true,"city":"Oldtown","country_code":"XX"}`}
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = &http.Client{Transport: geo}
	m.mu.Lock()
	m.info = Info{
		PublicIP: "203.0.113.79", ISP: "AS64496 Example Telecom",
		City: "Oldtown, XX", UpdatedAt: time.Now().Unix(),
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for i := 0; i < 5; i++ {
		info := m.fetch(ctx)
		m.mu.Lock()
		m.info = info // publish, as refresh does, so the next fetch sees it
		m.mu.Unlock()
	}
	// One recovery attempt, which asks BOTH providers: the primary named the
	// city and could not place it, so the secondary is asked for a position
	// (that is the whole point - re-asking the provider that already declined
	// would recover nothing). Unthrottled this would be 10.
	if geo.count() != 2 {
		t.Fatalf("geo queries = %d over 5 fetches on one unchanged IP, want 2 (one attempt across "+
			"both providers). A provider that cannot place this address will not place it on the "+
			"sixth ask either", geo.count())
	}
}

// ...BUT A PROVIDER THAT DID NOT ANSWER IS NOT A PROVIDER THAT CANNOT PLACE US.
// The throttle above must not turn a 429, a timeout or an outage into a
// permanently missing ISP origin: that is a load-bearing input to auto server
// selection, and the difference between "nowhere" and "no answer" is not
// visible from the call site. So the attempt is recorded, not spent - once the
// providers recover, the next due retry takes the coordinate.
func TestFetchRecoversTheCoordinateAfterAProviderOutage(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()
	ipv4Client = canned(200, "203.0.113.79")
	ipv6Client = canned(500, "")

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(503, "") // both providers down
	m.mu.Lock()
	m.info = Info{
		PublicIP: "203.0.113.79", ISP: "AS64496 Example Telecom",
		City: "Oldtown, XX", UpdatedAt: time.Now().Unix(),
	}
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	info := m.fetch(ctx)
	if info.Lat != 0 {
		t.Fatalf("coordinate = %v while the providers were down, want none yet", info.Lat)
	}
	m.mu.Lock()
	m.info = info
	// The outage is over, and enough time has passed for the next attempt.
	m.coordTriedAt = time.Now().Add(-2 * coordRetryInterval)
	m.mu.Unlock()
	m.http = canned(200, `{"success":true,"city":"Oldtown","country_code":"XX","latitude":12.345678,"longitude":-76.543210}`)

	if got := m.fetch(ctx); got.Lat != 12.345678 || got.Lon != -76.543210 {
		t.Fatalf("coordinate = %v,%v after the providers recovered; a transient outage spent the "+
			"only recovery and the ISP city is gone for the life of this IP", got.Lat, got.Lon)
	}
}
