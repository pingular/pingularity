package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// The birth marker (settings.KeyInstallBornVersion) is forward provenance: its
// presence tells warnAmbiguousContainerAccess that a store's missing access key
// means "never chose", not "was reachable before the upgrade". Settings can only
// read the store's contents, where an install stopped before it ever measured
// looks exactly like a brand-new one, so main supplies the missing half: whether
// the database file was there before it opened it.

// A container upgraded from a release whose dashboard answered the network,
// whose database was created back then and never written to: it must not be
// stamped, and once it starts measuring it must get the warning naming its 403.
func TestUpgradedEmptyContainerStoreIsNotStampedAndStaysExplained(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")

	// The 0.61 era: the daemon ran once, created the database, and was stopped at
	// the consent prompt. The offer clock is all it left behind.
	old, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open the 0.61 database: %v", err)
	}
	if err := old.SetSetting(ctx, "quick_setup_offer_since", "1754400000"); err != nil {
		t.Fatalf("seed the first-run offer clock: %v", err)
	}
	old.Close()

	// The upgrade boot, taking main's own reading of the path before the open.
	created := dbCreatedNow(dbPath)
	if created {
		t.Fatalf("precondition: %s exists, so this boot did not create it; the test proves nothing if main thinks otherwise", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen on the new release: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, testDefaultsFor(config.Config{}),
		settings.WithBornVersion("0.80.0-test"), settings.WithDatabaseCreated(created))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if v, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; marked {
		t.Fatalf("the upgrade stamped %s=%q on a database an earlier release created. The marker claims this build watched the store come into existence, and downstream that claim is what stops the daemon explaining a 403 to the operator whose published port used to answer.", settings.KeyInstallBornVersion, v)
	}

	// History is what makes the store established, the shape the warning aims at.
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now().Add(-2 * time.Hour), Target: "cloudflare", Family: "ipv4", LatencyMS: 11, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}

	warns := bootAccessDecision(t, config.Config{}, st, set, true)
	if len(warns) != 1 || warns[0].msg != accessAmbiguousWarnMsg {
		t.Fatalf("got %d warnings (%v), want exactly the ambiguity warning. This install's dashboard did answer the network before the upgrade; without this line the operator sees only a 403 on a port that worked yesterday, with nothing in the log naming the cause or the fix.", len(warns), warns)
	}
	if set.AccessLocalOnly() != true || storedAccessKey(t, st) {
		t.Fatal("the boot decided or persisted access; the warning explains, it never writes")
	}
}

// The other population: a genuinely new install must still get stamped, or every
// fresh container would collect the ambiguity warning on every boot.
func TestFreshInstallIsStampedAndNeverWarned(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")

	created := dbCreatedNow(dbPath) // nothing there yet: this boot is the birth
	if !created {
		t.Fatalf("precondition: %s must not exist before the first boot", dbPath)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx, st, testDefaultsFor(config.Config{}),
		settings.WithBornVersion("0.80.0-test"), settings.WithDatabaseCreated(created))
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if v, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; !marked || v != "0.80.0-test" {
		t.Fatalf("marker = %q, %v on an install this boot really did create; want it stamped. Its absence is what makes an install look ambiguous forever.", v, marked)
	}
	if err := set.BornMarkerErr(); err != nil {
		t.Fatalf("BornMarkerErr = %v on a boot that created the database and could write to it", err)
	}

	// Established too, like the upgrade above, but born private: still silent.
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: time.Now().Add(-2 * time.Hour), Target: "cloudflare", Family: "ipv4", LatencyMS: 11, Success: true},
	}); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if warns := bootAccessDecision(t, config.Config{}, st, set, true); len(warns) != 0 {
		t.Fatalf("got %d warnings (%v) on an install born under the fail-closed default; its marker says its missing access key means \"never chose\", so there is nothing ambiguous to report", len(warns), warns)
	}
}

// `pingularity reset-auth` refuses to create a database, so it can never be the
// process that witnessed a birth and must record none.
func TestResetAuthNeverStampsABirthMarker(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
	old, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old.Close()

	if err := resetAuthCmd([]string{"-db", dbPath}); err != nil {
		t.Fatalf("resetAuthCmd: %v", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	if v, marked := settingsSnapshot(t, st)[settings.KeyInstallBornVersion]; marked {
		t.Fatalf("reset-auth stamped %s=%q; a command that refuses to create a database has witnessed no birth and must record none", settings.KeyInstallBornVersion, v)
	}
}

// Only a definite "the path is not there" may answer yes: an unproven birth
// left unrecorded costs a warning, while a birth recorded that never happened
// silences one.
func TestDbCreatedNowOnlyAnswersYesToADefiniteAbsence(t *testing.T) {
	dir := t.TempDir()

	existing := filepath.Join(dir, "pingularity.db")
	if err := os.WriteFile(existing, []byte("not really sqlite"), 0o600); err != nil {
		t.Fatalf("seed an existing database: %v", err)
	}
	asDir := filepath.Join(dir, "mounted-over")
	if err := os.Mkdir(asDir, 0o700); err != nil {
		t.Fatalf("seed a directory at the db path: %v", err)
	}

	cases := []struct {
		name string
		path string
		want bool
		why  string
	}{
		{"nothing at the path", filepath.Join(dir, "brand-new.db"), true,
			"a boot that finds no database is the one boot that creates it; refusing here leaves every fresh install unmarked and permanently ambiguous"},
		{"an existing database", existing, false,
			"an earlier release created this file - the exact case the marker must not claim"},
		{"a directory at the path", asDir, false,
			"a volume mounted over the database path is not a birth, and stat succeeds here so an err != nil test would not catch it"},
	}
	if runtime.GOOS != "windows" {
		// A path under a regular file: the stat fails for a reason that is not
		// absence - except on Windows, which reports it as not found, so skip there.
		cases = append(cases, struct {
			name string
			path string
			want bool
			why  string
		}{"a stat that cannot answer", filepath.Join(existing, "pingularity.db"), false,
			"the stat failed for a reason that is not absence, so nothing was witnessed; answering yes on any error at all would mint provenance out of a failed check"})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dbCreatedNow(c.path); got != c.want {
				t.Errorf("dbCreatedNow(%q) = %v, want %v: %s", c.path, got, c.want, c.why)
			}
		})
	}
}

// run()'s boot sequence is not callable from a test, so this checks the wiring
// in the source, as TestOnlyExplicitInputPersistsAccess does: every function in
// main.go that builds a controller must also pass the creation verdict, or
// settings silently falls back to the emptiness reading that stamped upgrades as
// newly born. Per-function rather than per-argument because run() spreads an
// options slice, where an argument-level check would find nothing to look at.
func TestMainAlwaysTellsSettingsWhetherItCreatedTheDatabase(t *testing.T) {
	const verdict = "WithDatabaseCreated"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	// settingsCall reports whether body calls settings.<name> anywhere inside it.
	settingsCall := func(body *ast.BlockStmt, name string) (token.Pos, bool) {
		var at token.Pos
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || at.IsValid() {
				return !at.IsValid()
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "settings" {
				at = call.Pos()
				return false
			}
			return true
		})
		return at, at.IsValid()
	}

	builders := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		at, builds := settingsCall(fn.Body, "New")
		if !builds {
			continue
		}
		builders++
		if _, told := settingsCall(fn.Body, verdict); !told {
			t.Errorf("%s: %s builds a settings controller without settings.%s; main is the only layer that can see whether the database file existed, and without that verdict settings falls back to reading the store as empty - which an install stopped before its first measurement satisfies just as well as a brand-new one",
				fset.Position(at), fn.Name.Name, verdict)
		}
	}
	if builders == 0 {
		t.Fatal("no settings.New call found in main.go; this test is checking nothing")
	}
}
