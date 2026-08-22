package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The birth marker claims this build watched the database come into existence,
// but for most of its life the code only checked that the store read EMPTY - and
// an install stopped before its first measurement reads empty too (a 0.61
// container parked at the Quick Setup prompt has written only the offer clock,
// which installStateKeys classes as bookkeeping). Stamping it would say it was
// born under a build that fails access closed, which up to 0.61 it was not, and
// main takes the marker as permission to stop explaining the published port's
// new 403. So the creation half of the evidence comes from the caller, and these
// tests pin both halves.

// A store the caller says it did NOT create must never be stamped, however
// brand-new its contents read.
func TestNoBirthMarkerOnADatabaseThisProcessDidNotCreate(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// What an install stopped before its first measurement leaves behind: the
	// first-run offer clock and nothing else.
	if err := st.SetSetting(ctx, keyQuickSetupOffer, "1754400000"); err != nil {
		t.Fatalf("seed offer clock: %v", err)
	}
	if est, err := establishedReading(t, ctx, st); err != nil || est {
		t.Fatalf("precondition: this store must still read NOT established (got %v, %v), or the test proves nothing about the emptiness heuristic", est, err)
	}

	c, err := New(ctx, st, bornMarkerDefaults,
		WithBornVersion("0.80.0-test"), WithDatabaseCreated(false))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); ok {
		t.Fatalf("a database this process did not create was stamped %s=%q. The marker asserts that this build watched the store come into existence; an empty store only proves nothing has been written to it yet, and an upgrade from a release that fails access OPEN reads exactly the same. Downstream that stamp permanently silences the warning explaining the upgrade's 403.", KeyInstallBornVersion, v)
	}
	if err := c.BornMarkerErr(); err != nil {
		t.Fatalf("BornMarkerErr = %v; an upgrade is the ordinary shape, not a failure to record anything - reporting it would put a warning in every upgrade's log", err)
	}

	// And it must stay refused: the refusal is permanent, not skipped once.
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if err := c.SetMonitoring(ctx, true); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); ok {
		t.Fatalf("a reload/settings write stamped %s=%q on a database this process did not create; the refusal must be permanent, not skipped once", KeyInstallBornVersion, v)
	}
}

// The other direction: a genuinely new install must STILL be stamped, or every
// fresh container gets warned on every boot about an access ambiguity it does
// not have.
func TestBirthMarkerStillStampedWhenCreationIsWitnessed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	if _, err := New(ctx, st, bornMarkerDefaults,
		WithBornVersion("0.80.0-test"), WithDatabaseCreated(true)); err != nil {
		t.Fatalf("New: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); !ok || v != "0.80.0-test" {
		t.Fatalf("marker = %q, %v on an install whose creation WAS witnessed; want it stamped. A fresh install left unmarked is warned about ambiguous access provenance on every boot, for an ambiguity it does not have.", v, ok)
	}
}

// Creation is necessary, not sufficient: an import or a restore can fill a
// database this process really did create, so the established reading keeps its
// veto.
func TestWitnessedCreationDoesNotOverrideAnEstablishedStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now().Add(-time.Hour), Target: "cloudflare", Family: "ipv4", LatencyMS: 12, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	if _, err := New(ctx, st, bornMarkerDefaults,
		WithBornVersion("0.80.0-test"), WithDatabaseCreated(true)); err != nil {
		t.Fatalf("New: %v", err)
	}
	if v, ok := bornMarkerValue(t, st); ok {
		t.Fatalf("an established store was stamped %s=%q because creation was witnessed; both readings must agree, or restoring a backup into a file this process created would mint provenance for history it never saw", KeyInstallBornVersion, v)
	}
}

// establishedReading takes EstablishedInStore without building a controller:
// New is what does the stamping, so a precondition routed through it would be
// measuring the thing under test.
func establishedReading(t *testing.T, ctx context.Context, st *store.Store) (bool, error) {
	t.Helper()
	return (&Controller{store: st}).EstablishedInStore(ctx)
}
