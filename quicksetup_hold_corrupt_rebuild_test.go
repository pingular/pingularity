package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// tearDatabase leaves the SQLite header intact and overwrites everything after
// it - a torn file, the fault the store's set-aside recovery exists for.
func tearDatabase(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(b[100:]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCorruptRebuildDoesNotReholdAnEstablishedInstall: asked for the rebuild
// (`-on-corrupt rebuild`, which is what the reopens here arm - without it the
// start refuses and there is no rebuilt store to rehold), the store sets a torn
// database aside and rebuilds an empty one "so the daemon comes back up
// monitoring". Booted the way the packaged unit and the container boot it
// otherwise - `run -db <path>`, no consent flag - the first-run decision then
// saw an empty store, seeded a fresh offer clock, and held monitoring for the
// whole 48h consent grace, on an install that had consented long ago. The
// daemon holds the evidence that this is no first run: it just moved a
// database aside.
func TestCorruptRebuildDoesNotReholdAnEstablishedInstall(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")

	// An established install that answered Quick Setup and has been measuring.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var sms []store.Sample
	for i := 0; i < 20; i++ {
		sms = append(sms, store.Sample{TS: time.Now().Add(-time.Duration(i+1) * time.Second), Target: "cloudflare", Family: "ipv4", LatencyMS: 12, Success: true})
	}
	if err := st.InsertSamples(ctx, sms); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	set, err := settings.New(ctx, st, testDefaultsFor(config.Config{}))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if err := set.SetQuickSetupDone(ctx, true); err != nil {
		t.Fatalf("mark answered: %v", err)
	}
	if got := quickSetupHoldState(ctx, set); got != qsReleased {
		t.Fatalf("fixture: an answered install reads %v, want released", got)
	}
	st.Close()

	// The disk fault, then the restart in the packaged shape.
	tearDatabase(t, dbPath)
	cfg := config.Config{DBPath: dbPath}
	created := dbCreatedNow(dbPath) // what Start records: the file was there
	st2, err := store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("reopen after the fault: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	set2, err := settings.New(ctx, st2, testDefaultsFor(cfg), settings.WithBornVersion("test"), settings.WithDatabaseCreated(created))
	if err != nil {
		t.Fatalf("settings after the rebuild: %v", err)
	}
	p := &program{cfg: cfg}
	p.materializeQuickSetup(ctx, set2, func(msg string, e error) { t.Logf("%s: %v", msg, e) })

	if got := quickSetupHoldState(ctx, set2); got != qsReleased {
		t.Fatalf("after its corrupt database was set aside and rebuilt, the install is back on the first-run hold (%v, want released): nothing is measured for up to 48h while /healthz answers 200, on an install that consented long ago", got)
	}
	if !newMonitoringLiveFn(ctx, set2, nil)() {
		t.Fatal("monitoring is not live after the rebuild")
	}
}

// tearTableRoot trashes one table's b-tree root page in a closed database and
// leaves the rest of the file - header, settings table and all - intact: a
// single bad sector rather than a wiped file, so what the old database still
// says about itself can be read after the fault.
func tearTableRoot(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var root int
	err = db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&root)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := int(b[16])<<8 | int(b[17])
	if ps == 1 {
		ps = 65536
	}
	off := (root - 1) * ps
	if root < 2 || off+ps > len(b) {
		t.Fatalf("%s sits on page %d of a %d-byte file", table, root, len(b))
	}
	for i := 0; i < ps; i++ {
		b[off+i] = 0xFF
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The mirror of the test above, and the reason the rebuild does not simply
// declare every rebuilt store established. An install whose database is torn
// three hours after it was installed has answered nothing: releasing its hold
// would start it probing on the defaults and mark a Quick Setup dialog answered
// that nobody was ever shown. When the old file survives well enough to say so,
// the hold stays.
func TestCorruptRebuildKeepsAFreshInstallOnItsHold(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
	cfg := config.Config{DBPath: dbPath}

	// A first boot in the packaged shape: no history, no consent flag, so the
	// offer clock is seeded and monitoring is held.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	set, err := settings.New(ctx, st, testDefaultsFor(cfg), settings.WithBornVersion("test"), settings.WithDatabaseCreated(true))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	p := &program{cfg: cfg}
	p.materializeQuickSetup(ctx, set, func(msg string, e error) { t.Logf("%s: %v", msg, e) })
	if got := quickSetupHoldState(ctx, set); got != qsHeld {
		t.Fatalf("fixture: a fresh install reads %v, want held", got)
	}
	st.Close()

	// The disk fault, then the restart in the same shape.
	tearTableRoot(t, dbPath, "pauses")
	st2, err := store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("reopen after the fault: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	set2, err := settings.New(ctx, st2, testDefaultsFor(cfg), settings.WithBornVersion("test"), settings.WithDatabaseCreated(dbCreatedNow(dbPath)))
	if err != nil {
		t.Fatalf("settings after the rebuild: %v", err)
	}
	p.materializeQuickSetup(ctx, set2, func(msg string, e error) { t.Logf("%s: %v", msg, e) })

	if got := quickSetupHoldState(ctx, set2); got != qsHeld {
		t.Fatalf("a fresh install that never answered Quick Setup came back from a disk fault %v (want held): it starts probing on the defaults and is never shown the dialog", got)
	}
	if newMonitoringLiveFn(ctx, set2, nil)() {
		t.Fatal("monitoring is live on an install that never answered Quick Setup")
	}
}
