package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

func bornMarkerValue(t *testing.T, st *store.Store) (string, bool) {
	t.Helper()
	m, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	v, ok := m[KeyInstallBornVersion]
	return v, ok
}

var bornMarkerDefaults = Values{
	Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
	DownAfter: 3, UpAfter: 2,
}

// A brand-new store is stamped with its birth version by New, exactly once,
// and the marker is immutable: a later New or Reload must never rewrite it -
// the value a store was born with is the value it dies with.
func TestBornMarkerStampedOnceAndImmutable(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	c, err := New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.62.0-test" {
		t.Fatalf("marker after first New = %q, %v; want the WithBornVersion value stamped", v, ok)
	}

	// Sentinel overwrite, then every re-initialization path: none may touch it.
	if err := st.SetSetting(ctx, KeyInstallBornVersion, "sentinel"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ctx, st, bornMarkerDefaults, WithBornVersion("0.63.0-test")); err != nil {
		t.Fatal(err)
	}
	if v, _ := bornMarkerValue(t, st); v != "sentinel" {
		t.Fatalf("a second New rewrote the marker to %q; it must be written exactly once", v)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := bornMarkerValue(t, st); v != "sentinel" {
		t.Fatalf("Reload rewrote the marker to %q; it must be written exactly once", v)
	}
}

// Without WithBornVersion the marker still lands (presence is the contract),
// with an honest "unknown" rather than an invented version.
func TestBornMarkerWithoutVersionSaysUnknown(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := New(ctx, st, bornMarkerDefaults); err != nil {
		t.Fatal(err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "unknown" {
		t.Fatalf("marker = %q, %v; want \"unknown\" when no version was named", v, ok)
	}
}

// An ESTABLISHED store must never gain the marker: stamping one later would
// claim a birth that cannot be proven, and its absence is what main's
// ambiguous-container-access warning reads (a signal that advises - it can no
// longer open access, because absence cannot separate a store carried up from
// 0.61 or earlier from one born private under a build that predates the
// marker). All three establishment legs - prior configuration, measurement
// history, the install anchor - and the Reload path too.
func TestBornMarkerNeverStampedOnEstablishedStore(t *testing.T) {
	ctx := context.Background()
	seed := map[string]func(t *testing.T, st *store.Store){
		"prior configuration": func(t *testing.T, st *store.Store) {
			if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
				t.Fatal(err)
			}
		},
		"measurement history": func(t *testing.T, st *store.Store) {
			if err := st.InsertSamples(ctx, []store.Sample{
				{TS: time.Now(), Target: "cloudflare", Family: "ipv4", LatencyMS: 9, Success: true},
			}); err != nil {
				t.Fatal(err)
			}
		},
		"install anchor": func(t *testing.T, st *store.Store) {
			if err := st.SetSetting(ctx, "first_seen_ts", "123456"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, plant := range seed {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			plant(t, st)
			c, err := New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := bornMarkerValue(t, st); ok {
				t.Fatal("New stamped the birth marker on an established (pre-marker-shaped) store")
			}
			if err := c.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			if _, ok := bornMarkerValue(t, st); ok {
				t.Fatal("Reload stamped the birth marker on an established (pre-marker-shaped) store")
			}
		})
	}
}

// The marker is bookkeeping, not configuration: stamping it must flip neither
// EstablishedInStore nor the fresh-install consent-hold machinery. A store
// whose ONLY key is the marker still reads fresh, still gets its offer clock
// seeded (not marked answered), and still holds.
func TestBornMarkerExemptFromEstablishmentAndConsentHold(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(ctx, st, bornMarkerDefaults, WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bornMarkerValue(t, st); !ok {
		t.Fatal("precondition: fresh store must carry the marker after New")
	}
	if hasPriorConfiguration(map[string]string{KeyInstallBornVersion: "0.62.0-test"}) {
		t.Fatal("the marker alone reads as operator configuration; every fresh install would skip the consent hold")
	}
	if est, err := c.EstablishedInStore(ctx); err != nil || est {
		t.Fatalf("EstablishedInStore = %v, %v after stamping; the marker must not un-freshen a fresh install", est, err)
	}
	now := time.Now().Unix()
	if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
		t.Fatal(err)
	}
	if c.QuickSetupDone() {
		t.Fatal("offer decision marked a marker-only store answered; the first-run dialog is lost")
	}
	since, err := c.QuickSetupOfferSinceErr(ctx)
	if err != nil || since == 0 {
		t.Fatalf("offer clock = %d, %v; a fresh install must be offered, marker or not", since, err)
	}
	if !QuickSetupHold(c.QuickSetupDone(), since, now) {
		t.Fatal("consent hold not in force on a fresh install carrying only the marker")
	}
}
