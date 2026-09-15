package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// olderReleaseFile hand-creates the file a release before 0.100 leaves: a
// settings table holding rows, and none of the tables this release adds.
func olderReleaseFile(t *testing.T, stmts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "older.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);` + stmts); err != nil {
		t.Fatalf("seed the older release's file: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// Such a file can hold the settings layer's split marker only because that
// release restored it from a backup of a database this release had started, so
// it describes that other database and not this one. Open deletes it, before
// the schema adds server_health, and leaves every other row alone. Once this
// release has opened the file, a marker in it is its own and stays.
func TestOpenForgetsASplitMarkerOnlyOnAFileNoSplitBuildHasOpened(t *testing.T) {
	ctx := context.Background()
	path := olderReleaseFile(t, `INSERT INTO settings (key, value) VALUES ('engine_split_done', '1'), ('speed_direction', 'up');`)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all["engine_split_done"]; ok {
		t.Error("a split marker on a file no build of this release had opened survived Open")
	}
	if all["speed_direction"] != "up" {
		t.Errorf("speed_direction = %q after Open, want the up the file held", all["speed_direction"])
	}
	if _, err := st.SetSettingsDiff(ctx, map[string]string{"engine_split_done": "1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	if all, err := st.AllSettings(ctx); err != nil || all["engine_split_done"] != "1" {
		t.Errorf("the marker written after this release opened the file did not survive the next Open: %q (%v)", all["engine_split_done"], err)
	}
}

// Open does not go on past a marker it meant to delete and could not, since the
// settings load after it would take that marker at its word. It fails as the
// schema step fails on a write it cannot make, in the same words, and leaves
// the file as it found it.
func TestOpenGoesNoFurtherThanASplitMarkerItCouldNotForget(t *testing.T) {
	path := olderReleaseFile(t, `INSERT INTO settings (key, value) VALUES ('engine_split_done', '1');
		CREATE TRIGGER hold_the_marker BEFORE DELETE ON settings BEGIN SELECT RAISE(ABORT, 'held'); END;`)
	if st, err := Open(path); err == nil {
		st.Close()
		t.Fatal("Open went on past a split marker it could not delete")
	} else if !strings.HasPrefix(err.Error(), "migrate: ") {
		t.Errorf("Open failed with %q, want it worded as the schema step's own (migrate: ...)", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var marker, health int
	if err := raw.QueryRow(`SELECT (SELECT COUNT(*) FROM settings WHERE key = 'engine_split_done'),
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'server_health')`).Scan(&marker, &health); err != nil {
		t.Fatal(err)
	}
	if marker != 1 || health != 0 {
		t.Errorf("after the failed Open: split marker rows = %d, server_health tables = %d; want the file as it was, 1 and 0", marker, health)
	}
}

// tearIndexRoot is tearTableRoot for an index: it trashes the root page of one
// index in a closed database and leaves the table it serves, the header and
// every other page as they were.
func tearIndexRoot(t *testing.T, path, index string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var root int
	err = raw.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&root)
	raw.Close()
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
		t.Fatalf("%s sits on page %d of a %d-byte file", index, root, len(b))
	}
	for i := 0; i < ps; i++ {
		b[off+i] = 0xFF
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The delete finds its row through the settings table's key index, a page
// nothing else at Open reads: the schema step never touches the table, and the
// settings load reads it whole. So an older release's file whose only damage is
// that index opened on every build before the check, and ran on the settings
// it held. It still does - nothing set aside, every row there to read - and a
// marker such a file restored stays, as it did before the check existed.
func TestOpenStillOpensAnOlderReleaseFileWhoseSettingsIndexIsTorn(t *testing.T) {
	for _, tc := range []struct {
		name, rows string
		marker     bool
	}{
		{"no marker", `('speed_direction', 'up'), ('speed_retries', '3')`, false},
		{"a restored marker", `('engine_split_done', '1'), ('speed_direction', 'up'), ('speed_retries', '3')`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := olderReleaseFile(t, `INSERT INTO settings (key, value) VALUES `+tc.rows+`;`)
			tearIndexRoot(t, path, "sqlite_autoindex_settings_1")
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			var n int
			err = raw.QueryRow(`SELECT COUNT(*) FROM settings WHERE key = 'engine_split_done'`).Scan(&n)
			raw.Close()
			if !dbCorrupt(err) {
				t.Fatalf("precondition: a lookup by key through the torn index reads as corrupt; got %d, %v", n, err)
			}

			st, err := Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer st.Close()
			ents, err := os.ReadDir(filepath.Dir(path))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				if strings.Contains(e.Name(), "corrupt") {
					t.Errorf("Open set the file aside as %s", e.Name())
				}
			}
			all, err := st.AllSettings(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if all["speed_direction"] != "up" || all["speed_retries"] != "3" {
				t.Errorf("Ookla pair after Open = %q/%q, want the up/3 the file held", all["speed_direction"], all["speed_retries"])
			}
			if _, kept := all["engine_split_done"]; kept != tc.marker {
				t.Errorf("split marker present after Open = %v, want %v", kept, tc.marker)
			}
		})
	}
}

// A torn file now fails at that check, the first statement Open runs, rather
// than in applySchema. It fails in the words it always did, which are the ones
// a recovery command typed by hand prints for it.
func TestOpenExistingNamesATornFileInTheSchemaStepsWords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "torn.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearPastHeader(t, path)
	if st, err := OpenExisting(path); err == nil {
		st.Close()
		t.Fatal("a torn database opened")
	} else if !strings.Contains(err.Error(), "migrate: ") {
		t.Errorf("OpenExisting on a torn file failed with %q, want the schema step's words (migrate: ...)", err)
	}
}

// A start refused on damage the migration meets leaves an older release's file
// byte for byte as it found it. Building an index is the first read of a table
// that release never read at startup, so that is where dormant damage surfaces
// on an upgrade. Had the steps before it committed on their own, the refused
// file would carry server_health - how this release tells a file it has opened
// from one it has not - and a split marker restored into that file later, under
// the older release, would be believed at the next upgrade instead of forgotten.
func TestARefusedStartLeavesAnOlderReleaseFileAsItFoundIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "older.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	// What a release before 0.100 leaves: no server_health, and no index across
	// speed_servers for this release to build.
	raw, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`DROP TABLE server_health; DROP INDEX idx_speed_servers_server`); err != nil {
		raw.Close()
		t.Fatalf("shape the older release's file: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "speed_servers")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err == nil {
		st.Close()
		t.Fatal("Open went on past a torn speed_servers table")
	}
	if !strings.Contains(err.Error(), "is damaged") {
		t.Fatalf("Open failed with %v, want the refusal of a damaged database", err)
	}
	// The refusal is what an operator reads with the daemon down, and the rebuild
	// it offers holds the network until a password is set and the daemon restarts.
	if !strings.Contains(err.Error(), "until a password is set on it and the daemon restarted or reloaded") {
		t.Errorf("the refusal no longer says the rebuilt store's hold ends when a password is set and the daemon restarted:\n%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused start changed the file (%d bytes before, %d after); a refusal has to leave an older release's file as it went in", len(before), len(after))
	}
	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var health int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'server_health'`).Scan(&health); err != nil {
		t.Fatal(err)
	}
	if health != 0 {
		t.Error("the refused start left server_health in the older release's file, so a split marker restored into it under that release would be believed at the next upgrade")
	}
	// Nor does the refused start keep hold of the file. A migration abandoned
	// without being rolled back keeps its connection and its write lock after
	// Open returns: nothing else can write to the file, and on Windows the
	// rebuild road cannot rename it aside.
	if _, err := raw.Exec(`CREATE TABLE lock_probe (x); DROP TABLE lock_probe`); err != nil {
		t.Errorf("the file is still locked after the refused start: %v", err)
	}
}

// An upgrade that has to migrate takes the write lock before it reads anything,
// so a writer busy on the same file makes it wait its turn - as the statements it
// replaced did - instead of failing the start. Begun as an ordinary transaction,
// its first read took a shared lock that SQLite then will not upgrade while
// another connection writes, and it does not call the busy handler for that: the
// first start on an older release's file exited "database is locked" whenever
// something else was writing to it - a sqlite3 session, a replication tool, a
// second daemon on the same path - and so did reset-auth.
func TestAnUpgradeWaitsForAWriterInsteadOfFailing(t *testing.T) {
	for _, open := range []struct {
		name string
		fn   func(string) (*Store, error)
	}{
		{"Open", func(p string) (*Store, error) { return Open(p) }},
		{"OpenExisting", OpenExisting},
	} {
		t.Run(open.name, func(t *testing.T) {
			for i := 0; i < 12; i++ {
				path := filepath.Join(t.TempDir(), "older.db")
				st, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				st.Close()
				writer, err := sql.Open("sqlite", buildDSN(path))
				if err != nil {
					t.Fatal(err)
				}
				writer.SetMaxOpenConns(1)
				// Shaped like an older release's file, so the upgrade has something
				// to write, plus a table for the other writer to keep busy.
				if _, err := writer.Exec(`DROP TABLE server_health; DROP INDEX idx_speed_servers_server; CREATE TABLE writer_probe (x)`); err != nil {
					writer.Close()
					t.Fatal(err)
				}
				stop, done := make(chan struct{}), make(chan struct{})
				go func() {
					defer close(done)
					for {
						select {
						case <-stop:
							return
						default:
							_, _ = writer.Exec(`INSERT INTO writer_probe VALUES (randomblob(64))`)
						}
					}
				}()
				time.Sleep(time.Duration(10+i*7) * time.Millisecond)
				st, err = open.fn(path)
				close(stop)
				<-done
				writer.Close()
				if err != nil {
					t.Fatalf("attempt %d: the upgrade failed while another connection was writing to the file: %v", i+1, err)
				}
				st.Close()
			}
		})
	}
}

// A migration that fails hands its connection back with no transaction open. A
// connection returned mid-transaction goes back into the pool and takes the next
// statement into that transaction, and the pool's close then rolls back whatever
// it wrote - invisible while every failed Open closes the pool at once, and lost
// data the first time a caller keeps using it.
func TestAFailedMigrationLeavesNoTransactionOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "older.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	raw, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	// An older release's shape, plus a table squatting on the name of an index
	// the migration creates, so the migration writes and then fails.
	if _, err := raw.Exec(`DROP TABLE server_health; DROP INDEX idx_speed_servers_server; CREATE TABLE idx_speed_servers_server (x)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	raw.Close()

	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err := migrateInOneTransaction(db); err == nil {
		db.Close()
		t.Fatal("fixture: the migration did not fail")
	}
	if _, err := db.Exec(`CREATE TABLE written_after (x)`); err != nil {
		db.Close()
		t.Fatalf("write after the failed migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	check, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var after, health int
	if err := check.QueryRow(`SELECT
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'written_after'),
		(SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'server_health')`).Scan(&after, &health); err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Error("a write made after the failed migration was lost: the migration handed its connection back with its transaction still open")
	}
	if health != 0 {
		t.Error("the failed migration's own writes were kept")
	}
}
