package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// These tests pin the fail-CLOSED contract for container access. The daemon
// used to persist access_local_only=false for a container whose store merely
// LOOKED pre-0.62 (established, no birth marker, no stored access key). The
// marker only exists as of 0.62-rc.4, but the fail-closed default shipped in
// rc.1 - so a container first installed on rc.1/rc.2/rc.3 was born PRIVATE and
// still has no marker, leaving it byte-identical on disk to a genuine pre-0.62
// install. Every gate passed, and the upgrade silently put an unauthenticated
// dashboard on the LAN (default listen is every interface, auth usually off).
//
// Nothing on disk separates the two populations, so both now fail closed: the
// daemon WARNS and leaves access local-only. Recovery is explicit and
// authoritative at every boot - PINGULARITY_ACCESS=network via reconcileAccess.

// warnCall is one captured WARN from warnAmbiguousContainerAccess.
type warnCall struct {
	msg  string
	args []any
}

// bootAccessDecision replays main's boot-time access sequence in the same order
// run() does: the ambiguity warning (which must never write) followed by
// reconcileAccess (the explicit-input recovery path, which may). It returns
// every warning raised, so a test can assert both the state the boot ends in
// and what the operator was told.
func bootAccessDecision(t *testing.T, cfg config.Config, st *store.Store, set *settings.Controller, inContainer bool) []warnCall {
	t.Helper()
	ctx := context.Background()
	var warns []warnCall
	if err := warnAmbiguousContainerAccess(ctx, cfg, st, set, inContainer, func(msg string, args ...any) {
		warns = append(warns, warnCall{msg: msg, args: args})
	}); err != nil {
		t.Fatalf("warnAmbiguousContainerAccess: %v", err)
	}
	if _, err := reconcileAccess(ctx, cfg, set); err != nil {
		t.Fatalf("reconcileAccess: %v", err)
	}
	return warns
}

// settingsSnapshot is every persisted settings key/value, for proving a boot
// wrote NOTHING - the access key included, but not only it.
func settingsSnapshot(t *testing.T, st *store.Store) map[string]string {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	return all
}

// storedAccessKey reports whether access_local_only is PRESENT in the store -
// key presence, the signal the daemon gates on, distinct from the value the
// controller's overlay answers when nothing is stored.
func storedAccessKey(t *testing.T, st *store.Store) bool {
	t.Helper()
	_, ok := settingsSnapshot(t, st)["access_local_only"]
	return ok
}

// testDefaults are 0.62's fresh-install seeds in miniature: AccessLocalOnly
// true (fail closed) seeded as a DEFAULT, never stored - exactly what every
// boot looks like before anyone chooses.
func testDefaults() settings.Values { return testDefaultsFor(config.Config{}) }

// testDefaultsFor models how main SEEDS the controller: `def :=
// defaultSettings(p.cfg)`, so the access scope in the DEFAULTS comes from the
// same -access/PINGULARITY_ACCESS the boot is running with. A fixture that
// hardcodes local-only while passing network to reconcileAccess manufactures a
// disagreement production never has, and would "prove" a persistence that does
// not happen. The mirror is checked against defaultSettings itself below, so it
// cannot drift.
func testDefaultsFor(cfg config.Config) settings.Values {
	return settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: defaultSettings(cfg).AccessLocalOnly, Monitoring: true,
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// newPreMarkerContainerStore builds the ambiguous shape: operator configuration
// and measurement history (so the store reads as ESTABLISHED), NO birth marker,
// and NO stored access key. Every population in the header lands here - a
// pre-0.62 upgrade, a build that predates the marker, a lost birth stamp.
// Ordering is load-bearing: the store is established BEFORE settings.New runs,
// so New refuses to stamp the marker (ensureBornMarker only stamps a brand-new
// store), reproducing the on-disk shape rather than asserting it.
func newPreMarkerContainerStore(t *testing.T) (*store.Store, *settings.Controller) {
	t.Helper()
	return newAmbiguousContainerStore(t, config.Config{})
}

// newAmbiguousContainerStore is the same shape seeded as main would seed it for
// the boot under test: cfg decides the controller's default access scope, so a
// boot carrying PINGULARITY_ACCESS=network starts with the value already in
// effect - which is exactly why the explicit recovery writes nothing.
func newAmbiguousContainerStore(t *testing.T, cfg config.Config) (*store.Store, *settings.Controller) {
	t.Helper()
	st := openStore(t)
	return st, establishedContainerController(t, st, cfg)
}

// establishedContainerController gives st the ordinary life that makes a store
// read ESTABLISHED - operator configuration and measurement history - and then
// builds the controller for the boot under test. It is the ONLY way the
// container-access tests build one, deliberately: the defaults come from
// testDefaultsFor(cfg), mirroring main's `def := defaultSettings(p.cfg)`, and
// the mirror is checked here rather than trusted. A fixture that hardcoded
// local-only defaults while passing an explicit network scope to reconcileAccess
// would manufacture a disagreement production never has, and would "prove" a
// persistence that does not happen - so that shape is not reachable from here.
//
// Ordering is load-bearing: the store is established BEFORE settings.New runs,
// so New refuses to stamp the marker and the on-disk shape is reproduced rather
// than asserted.
func establishedContainerController(t *testing.T, st *store.Store, cfg config.Config) *settings.Controller {
	t.Helper()
	ctx := context.Background()
	if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
		t.Fatalf("seed operator configuration: %v", err)
	}
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now().Add(-12 * time.Hour), Target: "cloudflare", Family: "ipv4", LatencyMS: 11, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	set, err := settings.New(ctx, st, testDefaultsFor(cfg), settings.WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	// Fixture preconditions: this is the ambiguous shape, or the tests below
	// prove nothing.
	all := settingsSnapshot(t, st)
	if _, born := all[settings.KeyInstallBornVersion]; born {
		t.Fatal("fixture stamped a birth marker; a pre-marker store has none")
	}
	if _, stored := all["access_local_only"]; stored {
		t.Fatal("fixture stored an access choice; a pre-marker store has none")
	}
	if want := defaultSettings(cfg).AccessLocalOnly; set.AccessLocalOnly() != want {
		t.Fatalf("fixture seeded AccessLocalOnly=%v, but main would seed %v for this config", set.AccessLocalOnly(), want)
	}
	return set
}

// THE REPRO. A container whose store carries history but no birth marker and no
// stored access choice must end the boot still local-only, with NOTHING
// written. Against the removed grandfather this store satisfied every gate - in
// a container, established, no marker, no stored key, no explicit flag - so it
// persisted access_local_only=false and published the dashboard to the LAN.
//
// ONE test covers every install that reaches this shape, deliberately: a
// genuine pre-0.62 upgrade, an install from a build that predates the marker,
// and one whose birth stamp failed to write. They are the same bytes on disk,
// which is the entire finding - no rule can open one without opening the
// others, so the only safe answer is to open none of them. (A separate root
// test covers the lost-stamp fixture end to end; a per-population copy here
// would assert the same state twice and imply a distinction that does not
// exist.)
func TestAmbiguousContainerUpgradeFailsClosedAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, set := newPreMarkerContainerStore(t)
	before := settingsSnapshot(t, st)

	warns := bootAccessDecision(t, config.Config{}, st, set, true) // no -access/PINGULARITY_ACCESS

	if !set.AccessLocalOnly() {
		t.Fatal("a pre-marker container was opened to the network on upgrade; ambiguous provenance must fail CLOSED")
	}
	if storedAccessKey(t, st) {
		t.Fatal("an access choice was persisted from an inference about the store's age; nothing may write access but explicit operator input")
	}
	if after := settingsSnapshot(t, st); !reflect.DeepEqual(before, after) {
		t.Fatalf("the boot wrote to settings: before=%v after=%v (the ambiguity path must write nothing at all)", before, after)
	}
	// Not just held in memory: the next boot must read the same fail-closed state.
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !set.AccessLocalOnly() {
		t.Fatal("access did not survive a reload as local-only")
	}
	if len(warns) != 1 {
		t.Fatalf("got %d warnings, want exactly 1 - the operator must be told why their published port 403s", len(warns))
	}
}

// THE RECOVERY. The same ambiguous container, restarted with an explicit
// PINGULARITY_ACCESS=network, is opened - by reconcileAccess, on operator
// input, and persisted. This is what makes failing closed affordable: an
// upgraded container that really was reachable gets back with one env var and
// no shell in the image.
func TestExplicitAccessStillDecidesTheAmbiguousContainer(t *testing.T) {
	ctx := context.Background()

	// Seeded the way main seeds it, the env var is already the effective value,
	// so reconcileAccess finds nothing to change and writes NOTHING. The port
	// opens for this boot and every later one that carries the variable - but
	// the choice is not in the database, which is why the docs tell operators to
	// keep the variable (or make it stick from the Access tab) rather than
	// assuming one run was enough. Asserting a write here would have pinned a
	// persistence production does not do.
	t.Run("explicit network opens the boot without writing a stored choice", func(t *testing.T) {
		cfg := config.Config{Access: "network", AccessExplicit: true}
		st, set := newAmbiguousContainerStore(t, cfg)

		warns := bootAccessDecision(t, cfg, st, set, true)

		if set.AccessLocalOnly() {
			t.Fatal("explicit PINGULARITY_ACCESS=network did not open access; that is the documented way out of the lockout")
		}
		if storedAccessKey(t, st) {
			t.Fatal("the boot wrote an access row: with the scope already seeded from the same config there is nothing to reconcile, and claiming otherwise is what made the old test model a state production never reaches")
		}
		if len(warns) != 0 {
			t.Fatalf("got %d warnings, want 0: access is settled, so there is no ambiguity to report", len(warns))
		}
	})

	// The write DOES happen where there is a real disagreement to settle: a
	// stored local-only that the operator overrides with an explicit network
	// scope. That is reconcileAccess earning its keep, and it must persist.
	t.Run("explicit network overrides a disagreeing stored choice, and persists", func(t *testing.T) {
		cfg := config.Config{Access: "network", AccessExplicit: true}
		st, set := newAmbiguousContainerStore(t, cfg)
		if err := st.SetSetting(ctx, "access_local_only", "true"); err != nil {
			t.Fatalf("seed a stored local-only choice: %v", err)
		}
		if err := set.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if !set.AccessLocalOnly() {
			t.Fatal("precondition: the stored local-only choice must be in effect before the override")
		}

		bootAccessDecision(t, cfg, st, set, true)

		if set.AccessLocalOnly() {
			t.Fatal("the explicit network scope did not override the stored local-only choice")
		}
		if err := set.Reload(ctx); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if set.AccessLocalOnly() {
			t.Fatal("the override did not survive a reload; it was never written")
		}
	})

	t.Run("explicit local keeps it private, silently", func(t *testing.T) {
		st, set := newPreMarkerContainerStore(t)
		cfg := config.Config{Access: "local", AccessExplicit: true}

		warns := bootAccessDecision(t, cfg, st, set, true)

		if !set.AccessLocalOnly() {
			t.Fatal("explicit -access local must keep the install private")
		}
		if len(warns) != 0 {
			t.Fatalf("got %d warnings, want 0 when the operator stated the access mode", len(warns))
		}
	})
}

// A container BORN on 0.62+ carries the birth marker, so its missing access key
// means "never chose", not "used to be reachable". It boots untouched, private,
// and unlectured - even after consenting without an access choice (Quick Setup
// dismissal) and accruing history, which is what makes it look established from
// its second boot on.
func TestContainerBornOn062StaysPrivateAndUnwarned(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)

	// Boot one: brand-new store, initialized fail-closed and marked.
	set1, err := settings.New(ctx, st, testDefaults(), settings.WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("new settings (boot one): %v", err)
	}
	if _, born := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; !born {
		t.Fatal("a brand-new store must carry the birth marker; the provenance gate depends on it")
	}
	if err := set1.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatalf("EnsureQuickSetupOffer: %v", err)
	}
	// Dismissed Quick Setup: answered, but no access choice stored.
	if err := set1.SetQuickSetupDone(ctx, true); err != nil {
		t.Fatalf("SetQuickSetupDone: %v", err)
	}
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now(), Target: "cloudflare", Family: "ipv4", LatencyMS: 12, Success: true},
	}); err != nil {
		t.Fatalf("InsertSamples: %v", err)
	}

	// Boot two: same store, same defaults, no explicit -access.
	set2, err := settings.New(ctx, st, testDefaults(), settings.WithBornVersion("0.62.0-test"))
	if err != nil {
		t.Fatalf("new settings (boot two): %v", err)
	}
	before := settingsSnapshot(t, st)

	warns := bootAccessDecision(t, config.Config{}, st, set2, true)

	if !set2.AccessLocalOnly() {
		t.Fatal("a container born on 0.62 lost its fail-closed access")
	}
	if storedAccessKey(t, st) {
		t.Fatal("an access key was stored for an install whose operator never chose")
	}
	if after := settingsSnapshot(t, st); !reflect.DeepEqual(before, after) {
		t.Fatalf("the boot wrote to settings: before=%v after=%v", before, after)
	}
	if len(warns) != 0 {
		t.Fatalf("got %d warnings, want 0: a marked store's provenance is known, so there is nothing ambiguous to warn about", len(warns))
	}
}

// The firing matrix: exactly which shapes get the WARN, and its content. A
// warning that fires everywhere is noise operators learn to skip, and this one
// is the only notice an upgraded container gets before its port starts
// answering 403.
func TestWarnAmbiguousContainerAccessFires(t *testing.T) {
	ctx := context.Background()

	// ambiguous: established container store, no marker, no stored access key.
	ambiguous := func(t *testing.T) (*store.Store, *settings.Controller) { return newPreMarkerContainerStore(t) }
	// marked: born under 0.62+, established by later history.
	marked := func(t *testing.T) (*store.Store, *settings.Controller) {
		t.Helper()
		st := openStore(t)
		set, err := settings.New(ctx, st, testDefaults(), settings.WithBornVersion("0.62.0-test"))
		if err != nil {
			t.Fatalf("new settings: %v", err)
		}
		if err := st.InsertSamples(ctx, []store.Sample{
			{TS: time.Now(), Target: "cloudflare", Family: "ipv4", LatencyMS: 8, Success: true},
		}); err != nil {
			t.Fatalf("InsertSamples: %v", err)
		}
		return st, set
	}
	// chosen: the ambiguous shape, except the operator already stored a choice.
	chosen := func(t *testing.T) (*store.Store, *settings.Controller) {
		t.Helper()
		st, set := newPreMarkerContainerStore(t)
		if err := set.SetAccessLocalOnly(ctx, true); err != nil {
			t.Fatalf("SetAccessLocalOnly: %v", err)
		}
		return st, set
	}
	// fresh: brand-new container, nothing at all in the store.
	fresh := func(t *testing.T) (*store.Store, *settings.Controller) {
		t.Helper()
		st := openStore(t)
		set, err := settings.New(ctx, st, testDefaults(), settings.WithBornVersion("0.62.0-test"))
		if err != nil {
			t.Fatalf("new settings: %v", err)
		}
		return st, set
	}

	cases := []struct {
		name      string
		fixture   func(*testing.T) (*store.Store, *settings.Controller)
		cfg       config.Config
		container bool
		want      bool
	}{
		{"ambiguous container: the one shape that gets it", ambiguous, config.Config{}, true, true},
		{"born on 0.62+ (marker present): provenance known", marked, config.Config{}, true, false},
		{"access already chosen and stored", chosen, config.Config{}, true, false},
		{"explicit -access network settles it", ambiguous, config.Config{Access: "network", AccessExplicit: true}, true, false},
		{"explicit -access local settles it", ambiguous, config.Config{Access: "local", AccessExplicit: true}, true, false},
		{"native install: the filter always defaulted on", ambiguous, config.Config{}, false, false},
		{"fresh container: never was reachable", fresh, config.Config{}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, set := c.fixture(t)
			var warns []warnCall
			if err := warnAmbiguousContainerAccess(ctx, c.cfg, st, set, c.container, func(msg string, args ...any) {
				warns = append(warns, warnCall{msg: msg, args: args})
			}); err != nil {
				t.Fatalf("warnAmbiguousContainerAccess: %v", err)
			}
			if got := len(warns) > 0; got != c.want {
				t.Fatalf("warned = %v, want %v", got, c.want)
			}
			if !c.want {
				return
			}
			if warns[0].msg != accessAmbiguousWarnMsg {
				t.Errorf("warning message = %q, want %q", warns[0].msg, accessAmbiguousWarnMsg)
			}
			// The line has to be actionable on its own: it must name the state
			// and every way out, because the log is all the operator has once
			// the dashboard 403s them.
			line := warns[0].msg
			for _, a := range warns[0].args {
				if s, ok := a.(string); ok {
					line += " " + s
				}
			}
			line = strings.ToLower(line)
			for _, want := range []string{"local-only", "-access network", "pingularity_access=network", "password", "access tab"} {
				if !strings.Contains(line, want) {
					t.Errorf("warning does not mention %q; operators recover from this line alone:\n%s", want, line)
				}
			}
		})
	}
}

// A store read decides nothing: an unreadable store returns the error (the
// caller logs it and re-judges next boot) and neither warns nor writes.
func TestWarnAmbiguousContainerAccessReadErrorDecidesNothing(t *testing.T) {
	ctx := context.Background()
	st, set := newPreMarkerContainerStore(t)
	st.Close() // the store goes away under the daemon

	var warns int
	err := warnAmbiguousContainerAccess(ctx, config.Config{}, st, set, true, func(string, ...any) { warns++ })
	if err == nil {
		t.Fatal("a failed store read must be reported, not swallowed")
	}
	if warns != 0 {
		t.Fatalf("warned %d times on a read error; nothing was established", warns)
	}
	if !set.AccessLocalOnly() {
		t.Fatal("access changed on a read error")
	}
}
