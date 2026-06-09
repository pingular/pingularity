package netinfo

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
)

// egressLocation prefers the precise IPmap city, caches it per IP (so a flipping
// egress never re-hits rate-limited IPmap), and holds off the coarse country
// fallback during the grace window after a transient failure. The country paths
// (no-data / stale) delegate to cymruCountry (live DNS) and aren't unit-tested here.
func TestEgressLocation(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// IPmap returns a city -> "City, CC", cached per IP: a later failing lookup
	// still returns the cached value rather than re-hitting IPmap.
	m := NewManager(log)
	m.http = canned(200, `{"location":{"cityName":"Denison","countryCodeAlpha2":"US"}}`)
	if loc := m.egressLocation(ctx, "203.0.113.9"); loc != "Denison, US" {
		t.Fatalf("egressLocation OK = %q, want \"Denison, US\"", loc)
	}
	m.http = canned(429, "") // would yield no city if re-queried
	if loc := m.egressLocation(ctx, "203.0.113.9"); loc != "Denison, US" {
		t.Errorf("egressLocation cached = %q, want cached \"Denison, US\"", loc)
	}

	// A transient IPmap failure (429) within the grace window yields no location
	// yet - the coarse country must not flicker in on the first miss.
	m2 := NewManager(log)
	m2.http = canned(429, "")
	if loc := m2.egressLocation(ctx, "203.0.113.10"); loc != "" {
		t.Errorf("egressLocation within grace = %q, want empty", loc)
	}
}

// A no-data IPmap result (200/null - permanent for that IP) is cached as a country
// fallback, so later refreshes don't re-hit rate-limited IPmap (the cache's whole
// purpose). A private IP keeps the Cymru country lookup empty + fast.
func TestEgressLocationCachesFallback(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(log)
	var calls int
	m.http = &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return geoResp(200, `{"location":{}}`), nil // no-data
	})}
	m.egressLocation(ctx, "192.168.0.1") // first call resolves + caches the fallback
	m.egressLocation(ctx, "192.168.0.1") // second call must serve from cache
	if calls != 1 {
		t.Fatalf("IPmap hit %d times; want 1 (the no-data fallback should be cached)", calls)
	}
}
