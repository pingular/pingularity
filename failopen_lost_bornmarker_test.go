package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"testing"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// A birth marker records something only the daemon that created the database
// can know, so the daemon that watched it happen may still complete a stamp a
// store fault lost (see internal/settings) - but only until it exits. A write
// that fails at birth and is not completed before the process restarts leaves
// the install markerless FOREVER: from that boot on the store is established,
// no later controller witnessed anything, and an established store must never be
// stamped (a marker added later claims a birth that cannot be proven). On disk
// that install becomes byte-identical to a genuinely pre-0.62 one, which is the
// shape these tests are about.
//
// This is the test that matters: that shape must still fail CLOSED. Marker
// absence advises and nothing more - warnAmbiguousContainerAccess writes
// nothing, and TestOnlyExplicitInputPersistsAccess pins that no other path in
// main may persist an access decision - so the worst a lost stamp can do is
// cost a recurring warning, never network-reachable access. If a future change
// ever lets marker absence DECIDE something, a dropped write at birth becomes
// that decision taken wrongly and permanently; this test is the tripwire.

// denyWritesTo / allowWritesTo make a REAL store refuse writes while reads keep
// working - the exact shape of the fault (settings load fine, only the stamp
// cannot land). The :memory: pool is one connection, so the per-connection
// pragma sticks across calls.
func denyWritesTo(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only on: %v", err)
	}
}

func allowWritesTo(t *testing.T, st *store.Store) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("query_only off: %v", err)
	}
}

// quietStderr swallows what fn writes to os.Stderr (settings warns there about
// the lost stamp, before any logger exists) so the failure being modelled does
// not look like test output.
func quietStderr(t *testing.T, fn func()) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("temp stderr: %v", err)
	}
	defer f.Close()
	orig := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = orig }()
	fn()
}

// newLostMarkerContainerStore builds the store a container is left with when its
// birth stamp was LOST to a write error: born under 0.62+ (fail-closed from the
// first boot, so it was never network-reachable), yet carrying no marker, and
// then established by an ordinary life of configuration and history. Every step
// is real - the write genuinely fails, the marker is genuinely absent - so this
// reproduces the shape rather than asserting it.
//
// What makes the loss PERMANENT is the restart. The controller that watched the
// database come into existence may still complete its own stamp (a later reload
// or settings write finishes it - see the settings package), so this fixture
// drops it while the store is still markerless: from here on, every controller
// is one that never saw the store fresh, and those must refuse forever. That is
// also why the store's later life is written directly rather than through the
// witnessing controller - the daemon that witnessed the birth is gone.
//
// cfg is the boot being modelled, and it seeds the controller exactly as main
// does (establishedContainerController -> testDefaultsFor). Seeding local-only
// here while handing an explicit network scope to reconcileAccess would invent a
// disagreement production never has.
func newLostMarkerContainerStore(t *testing.T, cfg config.Config) (*store.Store, *settings.Controller) {
	t.Helper()
	ctx := context.Background()
	st := openStore(t)

	// Birth, with the database refusing writes.
	denyWritesTo(t, st)
	var born *settings.Controller
	var err error
	quietStderr(t, func() {
		born, err = settings.New(ctx, st, testDefaultsFor(cfg), settings.WithBornVersion("0.62.0-test"))
	})
	if err != nil {
		t.Fatalf("settings.New at birth = %v; a failed stamp must not fail the load", err)
	}
	if born.BornMarkerErr() == nil {
		t.Fatal("fixture precondition: the birth stamp must have failed and been recorded")
	}
	if _, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; marked {
		t.Fatal("fixture precondition: the store must be markerless")
	}
	allowWritesTo(t, st) // the hiccup passes; everything else works from here on

	// The restart, on a later release: `born` is dropped here without ever
	// completing its stamp, and the controller for the boot under test is built
	// the one sanctioned way - over a store the ordinary life of configuration
	// and history has since made ESTABLISHED.
	set := establishedContainerController(t, st, cfg)
	if set.BornMarkerErr() != nil {
		t.Fatalf("BornMarkerErr = %v on a boot that had nothing to stamp", set.BornMarkerErr())
	}
	return st, set
}

// THE PROOF. An install whose birth marker was lost to a write error, running in
// a container with no explicit -access/PINGULARITY_ACCESS, must end its boot
// LOCAL-ONLY with nothing written. The marker's absence is a missing signal, not
// permission to guess: guessing open here would put an unauthenticated dashboard
// on the LAN of an install that was born private and never answered the network.
func TestLostBirthMarkerContainerBootStaysLocalOnlyAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, set := newLostMarkerContainerStore(t, config.Config{}) // no explicit access input
	before := settingsSnapshot(t, st)

	warns := bootAccessDecision(t, config.Config{}, st, set, true) // no explicit access input

	if !set.AccessLocalOnly() {
		t.Fatal("a LOST birth-marker write opened the dashboard to the network; marker absence must decide nothing")
	}
	if storedAccessKey(t, st) {
		t.Fatal("an access choice was persisted from a missing marker; only explicit operator input may write access")
	}
	if after := settingsSnapshot(t, st); !reflect.DeepEqual(before, after) {
		t.Fatalf("the boot wrote to settings: before=%v after=%v (this path must write nothing at all)", before, after)
	}
	// Not just in memory: the next boot reads the same fail-closed state.
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !set.AccessLocalOnly() {
		t.Fatal("access did not survive a reload as local-only")
	}
	if _, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; marked {
		t.Fatal("the reload stamped the marker on an established store")
	}

	// The cost of the lost stamp, stated: this install gets the ambiguity
	// warning on every boot forever, and the operator's way out is the explicit
	// flag. That is the whole damage - a support signal, not an exposure.
	if len(warns) != 1 || warns[0].msg != accessAmbiguousWarnMsg {
		t.Fatalf("got %d warnings (%v), want exactly the ambiguity warning: a permanently markerless install must at least be explained", len(warns), warns)
	}
}

// The surfaced error must also be CONSUMED, or it is dropped again one layer up.
// Settings records WHY the stamp was lost (and warns to stderr, before any
// logger exists); main is what turns it into a log line an operator can still
// find months later, when the only remaining symptom is an ambiguity warning on
// every boot. Source-level, because run()'s boot sequence is not callable from a
// test - the same reason TestOnlyExplicitInputPersistsAccess reads main.go.
func TestBootReportsALostBirthMarker(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "BornMarkerErr" {
			found = true
		}
		return !found
	})
	if !found {
		t.Error("main.go never reads BornMarkerErr: a birth marker lost to a write error is permanent, and nothing would record why this install is treated as pre-marker for the rest of its life")
	}
}

// The way out is the documented one, unchanged by how the store lost its marker:
// an explicit PINGULARITY_ACCESS=network is authoritative at THIS boot, so the
// install is one env var away from being reachable again - and once access is
// settled, the ambiguity warning stops.
//
// What it does NOT do is write an access row, and the fixture is what makes that
// visible: main seeds the controller from the same config the boot carries (def
// := defaultSettings(p.cfg)), so with no stored choice the network scope is
// already the effective value and reconcileAccess finds nothing to reconcile.
// Persisting is the Access tab's Save - which is why the docs tell operators to
// keep the variable rather than assume one run made it stick. An older version
// of this test seeded local-only defaults while passing network here, and
// "proved" a persistence production never performs.
func TestLostBirthMarkerInstallStillRecoversWithExplicitAccess(t *testing.T) {
	ctx := context.Background()
	cfg := config.Config{Access: "network", AccessExplicit: true}
	st, set := newLostMarkerContainerStore(t, cfg)

	warns := bootAccessDecision(t, cfg, st, set, true)

	if set.AccessLocalOnly() {
		t.Fatal("explicit PINGULARITY_ACCESS=network did not open access; that is the documented way out of the lockout")
	}
	if storedAccessKey(t, st) {
		t.Fatal("the boot wrote an access row: with the scope already seeded from the same config there is nothing to reconcile, and claiming otherwise models a state production never reaches")
	}
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	// The reload re-overlays the same defaults over a store that still has no
	// access row, so the variable still governs - and still nothing is stored.
	if set.AccessLocalOnly() {
		t.Fatal("the explicit scope did not survive a reload of the same boot's config")
	}
	if storedAccessKey(t, st) {
		t.Fatal("the reload persisted an access choice")
	}
	if len(warns) != 0 {
		t.Fatalf("got %d warnings, want 0: the operator settled access, so there is no ambiguity left to report", len(warns))
	}
}
