package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tearPastHeader turns a closed database into the shape a hard power-off
// leaves: the 100-byte SQLite file header intact, every page after it garbage.
// Such a file is unmistakably a database and just as unmistakably unusable.
func tearPastHeader(t *testing.T, path string) {
	t.Helper()
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("WAL still holds %d bytes after Close; the main file is not the authoritative copy", fi.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) <= 100 {
		t.Fatalf("database is only %d bytes", len(b))
	}
	if _, err := rand.Read(b[100:]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// tearTableRoot trashes the b-tree root page of ONE table in a closed database,
// leaving every other page - and the file header - exactly as it was. That is
// the shape of a single bad sector: the file is unmistakably a database, most
// of it still reads, and the one table that does not is enough to fail an Open.
func tearTableRoot(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(path))
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

// The README promises that a database which will not open is set aside, not
// repaired, for an install that asked for that road (-on-corrupt rebuild, which
// is what every reopen below arms; the default refusal has its own file), so
// the service cannot crash-loop on it. Open kept that promise only
// when the damage sat where the very first statement looks (the schema page):
// a torn page in a table the at-Open repairs scan - events here - passed the
// schema step and then failed the repair, and Open returned the error with the
// file left in place. Under Restart=always that is the crash loop the set-aside
// exists to end.
func TestOpenSetsAsideCorruptionPastTheSchemaStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	marker := strings.Repeat("CORRUPTME", 8)
	base := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 3000; i++ {
		typ := "down"
		if i%2 == 1 {
			typ = "up"
		}
		if err := st.InsertEvent(ctx, base.Add(time.Duration(i)*time.Minute), typ, 30, marker); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("WAL still holds %d bytes after Close", fi.Size())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := int(b[16])<<8 | int(b[17])
	if ps == 1 {
		ps = 65536
	}
	// The first page past page 1 that holds an events row: trash its b-tree
	// header, which makes every read of that page SQLITE_CORRUPT.
	idx := bytes.Index(b[ps:], []byte(marker))
	if idx < 0 {
		t.Fatal("marker not found past page 1")
	}
	off := ((idx + ps) / ps) * ps
	for i := 0; i < 64; i++ {
		b[off+i] = 0xFF
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, RebuildOnCorruption())
	matches, _ := filepath.Glob(path + ".*.corrupt")
	if err != nil {
		t.Fatalf("Open returned %v and left the file in place (%d set-aside files): the service restarts on the same file forever", err, len(matches))
	}
	defer st2.Close()
	if len(matches) == 0 {
		t.Fatal("Open succeeded on a corrupt file without setting it aside")
	}
	if err := st2.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatalf("rebuilt store not usable: %v", err)
	}
}

// A store rebuilt after its database was set aside belongs to an install that
// already existed - the daemon just moved that install's database out of the
// way. It must carry the evidence, or the first-run decision reads the empty
// store as a brand-new install and holds monitoring for the consent grace.
func TestRebuiltStoreCarriesAnInstallAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.InsertSamples(ctx, []Sample{{TS: time.Now().Add(-time.Minute), Target: "cloudflare", Family: "ipv4", LatencyMS: 9, Success: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearPastHeader(t, path)

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open must set a torn database aside and rebuild, got: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	if st2.InstallBornAt(ctx) == 0 {
		t.Fatal("a store rebuilt after corruption carries no install anchor: it reads as a brand-new install, and the first-run hold stops monitoring for the whole consent grace on an install that consented long ago")
	}
}

// The set-aside exists for a file a power cut tore, and a tear does not spare
// the first page: a filesystem handing back zeros for a block it never wrote,
// or an overwrite that started at byte 0, leaves a real database with no
// SQLite header on it. Refusing those bytes because they no longer spell
// "SQLite format 3" would hand the daemon back the crash loop - the file is
// still there next boot, and the boot after that.
func TestOpenSetsAsideADatabaseWhoseHeaderPageIsGone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
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
	for i := 0; i < ps && i < len(b); i++ {
		b[i] = 0
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open refused a database whose header page was lost instead of setting it aside: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("the damaged database was not set aside")
	}
	if err := st2.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatalf("rebuilt store not usable: %v", err)
	}
}

// The other direction. An install still inside its first-run offer has
// consented to nothing: it holds monitoring, and the dialog is the only thing
// that releases it early. Anchoring a store rebuilt from THAT file would start
// it probing on the defaults and retire a dialog nobody was ever shown - so
// when the old file can still say which it was, it decides.
func TestRebuildKeepsTheFirstRunHoldWhenTheOldFileWasStillOnIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Exactly what a first boot leaves behind: the offer clock, and nothing
	// that says anyone answered it or that this box ever measured.
	if err := st.SetSetting(ctx, "quick_setup_offer_since", "1757000000"); err != nil {
		t.Fatal(err)
	}
	if st.InstallBornAt(ctx) != 0 {
		t.Fatal("fixture: the store already carries an install anchor")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open must set a torn database aside and rebuild, got: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	if st2.InstallBornAt(ctx) != 0 {
		t.Fatal("a store rebuilt from a file that was still inside its first-run offer carries an install anchor: the box starts probing on the defaults and is never shown the Quick Setup dialog it never answered")
	}
}

// And the same fixture with the install past its first run: the rebuilt store
// must be anchored, or an install that answered Quick Setup months ago stops
// measuring for the whole consent grace.
func TestRebuildAnchorsWhenTheOldFileWasPastItsFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.SetSetting(ctx, "quick_setup_offer_since", "1757000000"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "quick_setup_done", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open must set a torn database aside and rebuild, got: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	if st2.InstallBornAt(ctx) == 0 {
		t.Fatal("a store rebuilt from the database of an install that had answered Quick Setup carries no anchor: the first-run hold stops its monitoring for the whole consent grace")
	}
}

// And the ambiguous file, which is the reason the offer clock has to be there
// rather than merely the answer be missing. A settings table that reads but
// carries none of the three keys says nothing: it is what a store torn between
// its creation and its first settings write looks like, and equally what a
// file whose settings rows were part of the damage looks like. Absence is not
// consent withheld, so it keeps monitoring - the same answer a file too torn
// to read at all gets.
func TestRebuildAnchorsWhenTheOldFileSaidNothingEitherWay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Something unrelated, so the settings table is certainly there to read:
	// what is missing from it is the point.
	if err := st.SetSetting(ctx, "theme", "dark"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("Open must set a torn database aside and rebuild, got: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("fixture: the torn database was not set aside")
	}
	if st2.InstallBornAt(ctx) == 0 {
		t.Fatal("a store rebuilt from a file that said nothing either way carries no anchor: silence is being read as proof, so an install that had been measuring for months comes back on the first-run hold and loses two days of it")
	}
}
