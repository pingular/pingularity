package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// A pending birth stamp is a claim about ROWS, not about a file: "the database I
// read was empty". Restoring a backup replaces those rows, so the claim stops
// being true - and completing the stamp afterwards would mark a genuinely older
// install as born under this version. That is false provenance, which is worse
// than no marker: an absent marker reads as "unknown" and fails closed, while a
// wrong one is trusted by everything downstream, including future migrations.
func TestBirthWitnessIsVoidedByARestore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Birth witnessed, stamp lost to a write failure that outlasts both attempts.
	denyStoreWrites(t, st)
	c, err := New(ctx, st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	}, WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	allowStoreWrites(t, st)
	if !c.bornPending.Load() {
		t.Fatal("precondition: the controller must be holding a pending stamp, or this test proves nothing")
	}

	// The operator restores a backup: rows replaced, so the witness is void.
	c.ForgetBirthWitness()

	// Whatever happens next - a settings write, a reload - must NOT stamp.
	if err := c.SetMonitoring(ctx, true); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if v, ok := all[KeyInstallBornVersion]; ok {
		t.Fatalf("the restored store was stamped %q: this daemon witnessed the birth of rows that a restore has since replaced, so the stamp claims a birth nobody saw for the data now in the store", v)
	}
	if c.bornPending.Load() {
		t.Fatal("the witness survived the restore; it must be dropped, not merely skipped once")
	}
}

// The guard on the other side: forgetting the witness must not be a way to
// suppress a legitimate stamp on a store that is still genuinely brand-new. A
// fresh controller re-reads the store, sees no history, and stamps it.
func TestForgettingTheWitnessDoesNotBlockAGenuineBirth(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	c, err := New(ctx, st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	}, WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.ForgetBirthWitness()
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if _, ok := all[KeyInstallBornVersion]; !ok {
		t.Fatal("a still-empty store lost its stamp: the witness is a shortcut for a store that has since filled up, never the only route to stamping a brand-new one")
	}
}
