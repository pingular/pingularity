package main

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// newGrandfatherFixture builds the boot-time state grandfatherContainerAccess
// judges: an in-memory store plus a controller whose AccessLocalOnly DEFAULT is
// seeded true (0.62's fail-closed default) but NOT stored - exactly what an
// upgrade or a fresh install looks like before anyone chooses. established=true
// plants one persisted operator key ("monitoring", not access-related), which
// is settings.EstablishedInStore's configured-install signal - the same one
// the quick-setup upgrade gate reads.
func newGrandfatherFixture(t *testing.T, established bool) (*store.Store, *settings.Controller) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if established {
		if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
			t.Fatalf("seed established install: %v", err)
		}
	}
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	return st, set
}

// storedAccessKey reports whether access_local_only is PRESENT in the store -
// the key-presence signal the grandfather gates on, distinct from the overlaid
// value the controller answers.
func storedAccessKey(t *testing.T, st *store.Store) bool {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	_, ok := all["access_local_only"]
	return ok
}

// The upgrade arm: an established container install with no stored access key
// and no explicit -access is grandfathered ONCE - AccessLocalOnly persisted
// false so its published port keeps answering - and the very write that opens
// it stores the key, making every later boot a no-op, including after the
// operator flips it back to local-only by hand.
func TestGrandfatherPre062ContainerOnceAndIdempotent(t *testing.T) {
	ctx := context.Background()
	st, set := newGrandfatherFixture(t, true)
	cfg := config.Config{} // no explicit -access/PINGULARITY_ACCESS

	migrated, err := grandfatherContainerAccess(ctx, cfg, st, set, true)
	if err != nil {
		t.Fatalf("grandfather: %v", err)
	}
	if !migrated {
		t.Fatal("established container install with no stored access key must be grandfathered")
	}
	if set.AccessLocalOnly() {
		t.Fatal("grandfathered install must keep network-reachable access (AccessLocalOnly=false)")
	}
	if !storedAccessKey(t, st) {
		t.Fatal("the grandfather must PERSIST its decision (access_local_only stored) - that is what makes it one-time")
	}
	// Persisted, not just flipped in memory: a reload must read it back.
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if set.AccessLocalOnly() {
		t.Fatal("grandfathered access did not survive a reload (not persisted)")
	}

	// Second boot: the stored key short-circuits the migration.
	migrated, err = grandfatherContainerAccess(ctx, cfg, st, set, true)
	if err != nil {
		t.Fatalf("grandfather (second boot): %v", err)
	}
	if migrated {
		t.Fatal("grandfather ran twice; the stored key must make later boots a no-op")
	}

	// The operator flips local-only back ON in the Access tab; a later boot must
	// respect that stored choice, never re-grandfather it open.
	if err := set.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}
	migrated, err = grandfatherContainerAccess(ctx, cfg, st, set, true)
	if err != nil {
		t.Fatalf("grandfather (after operator choice): %v", err)
	}
	if migrated || !set.AccessLocalOnly() {
		t.Fatalf("migrated=%v localOnly=%v: a stored operator choice must never be overridden", migrated, set.AccessLocalOnly())
	}
}

// The fresh arm: a genuinely fresh container (no history, no anchor, no
// persisted configuration) is NOT grandfathered - it was born under the
// fail-closed default and stays private, with nothing written.
func TestGrandfatherSkipsFreshContainer(t *testing.T) {
	ctx := context.Background()
	st, set := newGrandfatherFixture(t, false)

	migrated, err := grandfatherContainerAccess(ctx, config.Config{}, st, set, true)
	if err != nil {
		t.Fatalf("grandfather: %v", err)
	}
	if migrated {
		t.Fatal("fresh container was grandfathered; new installs must stay fail-closed")
	}
	if !set.AccessLocalOnly() {
		t.Fatal("fresh container lost its loopback-only default")
	}
	if storedAccessKey(t, st) {
		t.Fatal("fresh container gained a stored access key; the skip must write nothing")
	}
}

// The native arm: outside a container the filter always defaulted ON, so an
// established native install has nothing to grandfather.
func TestGrandfatherSkipsNative(t *testing.T) {
	ctx := context.Background()
	st, set := newGrandfatherFixture(t, true)

	migrated, err := grandfatherContainerAccess(ctx, config.Config{}, st, set, false)
	if err != nil {
		t.Fatalf("grandfather: %v", err)
	}
	if migrated || !set.AccessLocalOnly() || storedAccessKey(t, st) {
		t.Fatalf("migrated=%v localOnly=%v stored=%v: native installs must be untouched",
			migrated, set.AccessLocalOnly(), storedAccessKey(t, st))
	}
}

// The explicit arm, both directions: a passed -access/PINGULARITY_ACCESS always
// beats the grandfather. Explicit "local" keeps an otherwise-grandfatherable
// install private; explicit "network" opens it via reconcileAccess's own
// persisted write, with the grandfather never firing - so the boot ordering of
// the two can never matter.
func TestGrandfatherYieldsToExplicitAccess(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit local stays private", func(t *testing.T) {
		st, set := newGrandfatherFixture(t, true)
		cfg := config.Config{Access: "local", AccessExplicit: true}
		migrated, err := grandfatherContainerAccess(ctx, cfg, st, set, true)
		if err != nil {
			t.Fatalf("grandfather: %v", err)
		}
		if migrated || !set.AccessLocalOnly() {
			t.Fatalf("migrated=%v localOnly=%v: explicit -access local must beat the grandfather", migrated, set.AccessLocalOnly())
		}
	})

	t.Run("explicit network opens via reconcile, not the grandfather", func(t *testing.T) {
		st, set := newGrandfatherFixture(t, true)
		cfg := config.Config{Access: "network", AccessExplicit: true}
		migrated, err := grandfatherContainerAccess(ctx, cfg, st, set, true)
		if err != nil {
			t.Fatalf("grandfather: %v", err)
		}
		if migrated {
			t.Fatal("grandfather fired despite explicit -access network; explicit input owns the decision")
		}
		changed, err := reconcileAccess(ctx, cfg, set)
		if err != nil {
			t.Fatalf("reconcileAccess: %v", err)
		}
		if !changed || set.AccessLocalOnly() {
			t.Fatalf("changed=%v localOnly=%v: explicit network must open access through reconcileAccess", changed, set.AccessLocalOnly())
		}
	})
}
