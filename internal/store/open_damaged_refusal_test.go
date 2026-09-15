package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The other half of the set-aside: what Open does with a damaged database
// nobody asked it to replace. It leaves it alone. A database is the only copy
// of everything an install has - a year of history, the login, the webhook
// URLs - and moving it aside to start over on an empty one cannot be undone,
// while refusing to start can: the bytes stay put, sqlite3's .recover reads
// most of them back, and the operator decides. That the refusal costs a
// restart loop is the point; a loop is visible from outside, and a daemon
// serving an empty dashboard is not.
func TestOpenRefusesADamagedDatabaseAndLeavesItInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.InsertSamples(ctx, []Sample{{TS: time.Now().Add(-time.Minute), Target: "cloudflare", Family: "ipv4", LatencyMS: 9, Success: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "auth_hash", "$2a$10$notarealhashbutlongenough"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st2, openErr := Open(path)
	if openErr == nil {
		st2.Close()
		t.Fatal("Open started on a damaged database nobody asked it to replace")
	}
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("Open set the database aside unasked (%v): the history and every saved setting go with that file, and only the operator may spend them", matches)
	}
	// This fixture is already at the current schema, so the open it refused had
	// no migration to write: the file must come out of it byte for byte. (A
	// legacy file's migration commits what it managed before it meets the
	// damage; what must never happen either way is the file leaving.)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Open rewrote a database it had nothing to migrate and refused to open")
	}
	// The message is read in `systemctl status` with the daemon down, so it has
	// to carry the file, the way to the data, the check that comes before the
	// data replaces anything - and the row in it that tells a copy which kept the
	// login - and the way to the other road.
	msg := openErr.Error()
	for _, want := range []string{path, ".recover", "SELECT key FROM settings", "auth_hash among them", "do not put it in place", "-on-corrupt rebuild"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q, so an operator reading it has nothing to act on: %s", want, msg)
		}
	}
}

// The shape an upgrade meets: damage sitting in a table no earlier release read
// at startup, dormant until a build adds an index over it and reads the whole
// thing. That is the first read the damage ever gets, and it must not be the
// read that spends the file.
func TestOpenRefusesDamageTheIndexMigrationIsFirstToTouch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := st.InsertSpeedServers(ctx, []SpeedServerRow{
		{RunTS: time.Now().Add(-time.Hour).Unix(), ServerID: "1234", Server: "Sponsor, City", RankOrder: 1, Selected: true, Measured: true, Winner: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Put the file back in the shape the release before this one left: without
	// the per-server index, so opening it is what builds the index, and building
	// it is what reads the table from end to end for the first time.
	dropIndex(t, path, "idx_speed_servers_server")
	tearTableRoot(t, path, "speed_servers")

	st2, openErr := Open(path)
	if openErr == nil {
		st2.Close()
		t.Fatal("Open started on a database whose speed_servers table is damaged, having just read every row of it to build an index")
	}
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("Open set the database aside unasked (%v): an upgrade that is the first read of a damaged table must not also be the thing that spends it", matches)
	}
	if !strings.Contains(openErr.Error(), "idx_speed_servers_server") {
		t.Errorf("the refusal came from somewhere other than the index migration this test is about: %v", openErr)
	}
}

// dropIndex removes one index from a closed database and folds the WAL back, so
// the file on disk is what the next Open reads - the shape a release that never
// had that index left behind.
func dropIndex(t *testing.T, path, index string) {
	t.Helper()
	db, err := sql.Open("sqlite", buildDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + index); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("WAL still holds %d bytes; the main file is not the authoritative copy", fi.Size())
	}
}

// And with the recovery armed - the daemon's -on-corrupt rebuild - the same
// file is set aside and the store says out loud that it is the empty one built
// in its place. Nothing else in the process can tell.
func TestArmedOpenSetsTheDamagedFileAsideAndSaysTheStoreIsRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("an armed Open must set the damaged file aside and rebuild, got: %v", err)
	}
	defer st2.Close()
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("the damaged database was not set aside")
	}
	if !st2.RebuiltAfterCorruption() {
		t.Fatal("the rebuilt store does not report itself as rebuilt: readiness answers 'ready' and the access posture keeps the flag's network reach, over a store with no login in it")
	}
	// The replacement was built beside the database and renamed into place, and
	// nothing of the build is left behind for a later Open to mistake for an
	// interrupted one.
	if left, _ := filepath.Glob(path + rebuiltSuffix + "*"); len(left) != 0 {
		t.Errorf("the rebuild left its build files beside the database: %v", left)
	}
}

// The ordinary case, which is every start on every healthy install: nothing new
// is asked of it and nothing is said about it.
func TestAHealthyDatabaseOpensAndIsNotReportedRebuilt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("a healthy database must open with no flag and no prompt: %v", err)
	}
	defer st2.Close()
	if st2.RebuiltAfterCorruption() {
		t.Fatal("a healthy store reports itself rebuilt: every start would answer /readyz 503 and refuse the network")
	}
	all, err := st2.AllSettings(context.Background())
	if err != nil || all["k"] != "v" {
		t.Fatalf("the store the operator had is not the store they got back: %v %v", all, err)
	}
}

// The refusal is read with the daemon down and its commands are pasted into a
// shell, so the path in them has to survive being pasted. The default database
// directory on macOS is "Application Support": bare, the space splits the
// command in half and sqlite3 answers with a SQL syntax error about the second
// half instead of recovering anything. This asks a real shell how it reads what
// the message prints on this machine - which is also the machine the rendering
// is chosen for.
func TestTheRefusalPrintsCommandsAShellCanRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the oracle here is /bin/sh")
	}
	dir := filepath.Join(t.TempDir(), "Application Support", "pingularity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pingularity.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "auth_hash", "$2a$10$notarealhashbutlongenough"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, openErr := Open(path)
	if openErr == nil {
		st2.Close()
		t.Fatal("Open started on a damaged database nobody asked it to replace")
	}
	cmd := nthBackticked(t, openErr.Error(), 0)
	for _, p := range []string{path, path + ".recovered"} {
		if want := shellQuotePath(runtime.GOOS, p); !strings.Contains(cmd, want) {
			t.Errorf("the recovery command does not name %s the way this machine's shell reads it (%s): %s", p, want, cmd)
		}
	}
	halves := strings.Split(cmd, "|")
	if len(halves) != 2 {
		t.Fatalf("the recovery command is no longer a two-stage pipe, so this test reads it wrong: %q", cmd)
	}
	for i, want := range []string{path, path + ".recovered"} {
		words := shellWords(t, halves[i])
		if len(words) < 2 || words[1] != want {
			t.Errorf("a shell reads stage %d of the recovery command as %q; the file it would open is %q, not %q - the command the operator pastes fails and recovers nothing",
				i+1, strings.TrimSpace(halves[i]), strings.Join(words[1:], " "), want)
		}
	}
	// And the check that has to come before the copy replaces anything, which
	// opens the recovered file: pasted, it must name that file and carry its query
	// whole, or it errors for a reason that is not the copy's and reads as one.
	check := shellWords(t, nthBackticked(t, openErr.Error(), 1))
	if len(check) != 3 || check[1] != path+".recovered" || check[2] != "SELECT key FROM settings" {
		t.Errorf("a shell reads the check command as %q; want sqlite3 opening %q with the query \"SELECT key FROM settings\"", check, path+".recovered")
	}
}

// nthBackticked returns the n-th (from 0) command the message quotes between
// backticks.
func nthBackticked(t *testing.T, msg string, n int) string {
	t.Helper()
	parts := strings.Split(msg, "`")
	if len(parts) < 2*n+3 {
		t.Fatalf("the refusal carries no command %d to run: %s", n+1, msg)
	}
	return parts[2*n+1]
}

// shellWords is the shell's own answer to "what arguments is this line", got by
// asking it rather than by reimplementing quoting here.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	out, err := exec.Command("/bin/sh", "-c", "printf '%s\\n' "+line).Output()
	if err != nil {
		t.Fatalf("/bin/sh could not even parse %q: %v", line, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// And the quoting itself, at the shapes a database path really takes on a
// system whose shell is a POSIX one: the ordinary one stays bare so the message
// reads as prose, and the ones a shell would take apart - or expand - come back
// out of it whole.
func TestShellQuotePathSurvivesAShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the oracle here is /bin/sh")
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, p := range []string{
			"/var/lib/pingularity/pingularity.db",
			"/Users/mu/Library/Application Support/pingularity/pingularity.db",
			"/Library/Application Support/pingularity/pingularity.db",
			"/home/$USER/db/pingularity.db",
			"/tmp/it's mine/pingularity.db",
			"/tmp/`hostname`/pingularity.db",
		} {
			words := shellWords(t, "printf-marker "+shellQuotePath(goos, p))
			if len(words) != 2 || words[1] != p {
				t.Errorf("on %s a shell reads %q as %q, so the recovery command would open a file the operator did not name", goos, shellQuotePath(goos, p), words[1:])
			}
		}
		if got := shellQuotePath(goos, "/var/lib/pingularity/pingularity.db"); strings.Contains(got, "'") {
			t.Errorf("on %s an ordinary path came back quoted (%s); the message every Linux install reads should be prose, not a script", goos, got)
		}
	}
}

// Windows reads the same message in cmd.exe or PowerShell, and neither is a
// POSIX shell: cmd.exe keeps a single quote as part of the name, so the POSIX
// rendering handed sqlite3 a file that does not exist - on every Windows path,
// the default included, because a backslash is outside the POSIX safe set.
// There is no Windows shell where this suite runs, so these are the renderings
// the two shells' quoting rules call for, written out: bare where nothing needs
// quoting (the default, under %ProgramData%, has no space in it), double quotes
// - which both shells strip, and which no Windows file name can contain - for a
// space or a character either shell would split on, and PowerShell's literal
// single quotes where double quotes would still let PowerShell expand a $ or a
// backtick, or cmd.exe a %NAME%.
func TestShellQuotePathOnWindowsIsWhatItsShellsRead(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		// The default as config.defaultDBPath builds it on Windows, and the file
		// the recovery command writes beside it.
		{`C:\ProgramData\pingularity\pingularity.db`, `C:\ProgramData\pingularity\pingularity.db`},
		{`C:\ProgramData\pingularity\pingularity.db.recovered`, `C:\ProgramData\pingularity\pingularity.db.recovered`},
		{`D:/data/pingularity-1/pingularity_db.db`, `D:/data/pingularity-1/pingularity_db.db`},
		{`C:\My Data\pingularity.db`, `"C:\My Data\pingularity.db"`},
		{`C:\Program Files (x86)\pingularity\pingularity.db`, `"C:\Program Files (x86)\pingularity\pingularity.db"`},
		{`C:\Users\Ann O'Neil\pingularity\pingularity.db`, `"C:\Users\Ann O'Neil\pingularity\pingularity.db"`},
		{`C:\R&D;data\pingularity.db`, `"C:\R&D;data\pingularity.db"`},
		{`C:\Users\$build\pingularity.db`, `'C:\Users\$build\pingularity.db'`},
		{`C:\it's 100%\pingularity.db`, `'C:\it''s 100%\pingularity.db'`},
		{"C:\\`tick`\\pingularity.db", "'C:\\`tick`\\pingularity.db'"},
	} {
		if got := shellQuotePath("windows", tc.path); got != tc.want {
			t.Errorf("shellQuotePath(windows, %s) = %s, want %s", tc.path, got, tc.want)
		}
	}
}

// A store Open rebuilt after finding the database damaged has no login in it,
// and says so in a row of its own rather than only on the handle: the handle is
// gone at the next start, which finds the rebuilt database healthy and would
// have nothing else to tell it that -access network has no password to stand
// on. So the hold holds at the start that rebuilt the store and at every start
// after, until a login is set - and the first start to see one lets it go for
// good, so a login turned off later is the operator's call and not the
// rebuild's.
func TestARebuiltStoreHoldsTheNetworkUntilALoginIsSet(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	rebuilt, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("armed open: %v", err)
	}
	held, err := rebuilt.AccessHoldAfterRebuild(ctx)
	rebuilt.Close()
	if err != nil || !held {
		t.Fatalf("the start that rebuilt the store does not hold the network (held=%v, err=%v)", held, err)
	}
	for start := 2; start <= 3; start++ {
		later, err := Open(path)
		if err != nil {
			t.Fatalf("start %d: %v", start, err)
		}
		if later.RebuiltAfterCorruption() {
			later.Close()
			t.Fatalf("fixture: start %d rebuilt the store again", start)
		}
		held, err := later.AccessHoldAfterRebuild(ctx)
		later.Close()
		if err != nil || !held {
			t.Fatalf("start %d on the rebuilt store does not hold the network (held=%v, err=%v): -access network puts an empty store with no password on the LAN one restart after the rebuild", start, held, err)
		}
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "auth_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "auth_hash", "$2a$10$notarealhashbutlongenough"); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logged)
	held, err = st.AccessHoldAfterRebuild(ctx)
	log.SetOutput(orig)
	if err != nil || held {
		t.Fatalf("a store with a login set on it still holds the network (held=%v, err=%v): the operator did what the warning asked and the network never comes back", held, err)
	}
	// The release is said, because the login need not have been set at this
	// machine: an older build the store was rolled back to does not know the hold,
	// and whoever set a login there ended it. The log is the only place that shows.
	if !strings.Contains(logged.String(), "is released: a login is set on it") {
		t.Errorf("the start that found a login released the hold without a word:\n%s", logged.String())
	}
	if all, _ := st.AllSettings(ctx); all[accessHoldKey] != "" {
		t.Fatal("the start that found a login left the hold in place, so switching the login off later would bring the rebuild's hold back")
	}
	if err := st.SetSetting(ctx, "auth_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	logged.Reset()
	log.SetOutput(&logged)
	held, err = st.AccessHoldAfterRebuild(ctx)
	log.SetOutput(orig)
	if err != nil || held {
		t.Fatalf("a login switched off after the hold was released brought the hold back (held=%v, err=%v)", held, err)
	}
	if logged.Len() != 0 {
		t.Errorf("a store with no hold left to release says it released one:\n%s", logged.String())
	}
}

// What counts as a login is what the settings layer enforces: a password hash
// with the login switched on - "1", or "true" as older builds wrote it. A hash
// with the login off checks nobody, so the flag would still be standing on
// nothing.
func TestTheAccessHoldWantsALoginNotJustAPassword(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		settings map[string]string
		held     bool
	}{
		{"no login at all", map[string]string{}, true},
		{"a password with the login off", map[string]string{"auth_hash": "$2a$10$notarealhashbutlongenough", "auth_enabled": "0"}, true},
		{"the login on with no password", map[string]string{"auth_hash": "", "auth_enabled": "1"}, true},
		{"a login", map[string]string{"auth_hash": "$2a$10$notarealhashbutlongenough", "auth_enabled": "1"}, false},
		{"a login an older build wrote", map[string]string{"auth_hash": "$2a$10$notarealhashbutlongenough", "auth_enabled": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Open(rebuiltStore(t))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.SetSettings(ctx, tc.settings); err != nil {
				t.Fatal(err)
			}
			if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || held != tc.held {
				t.Fatalf("held=%v (err=%v), want %v", held, err, tc.held)
			}
		})
	}
}

// reset-auth's half of the hold: releasing it says whether there was one, so
// the command can tell the operator what it just opened. And a store that
// cannot be read answers held - this decides who reaches the dashboard, and it
// fails closed.
func TestReleaseAccessHoldSaysWhetherThereWasOne(t *testing.T) {
	ctx := context.Background()
	st, err := Open(rebuiltStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if released, err := st.ReleaseAccessHold(ctx); err != nil || !released {
		t.Fatalf("releasing the hold on a rebuilt store reported released=%v err=%v", released, err)
	}
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || held {
		t.Fatalf("the hold is still there after it was released (held=%v, err=%v)", held, err)
	}
	if released, err := st.ReleaseAccessHold(ctx); err != nil || released {
		t.Fatalf("a second release reported released=%v err=%v; there was nothing left to release", released, err)
	}
	st.Close()
	if held, err := st.AccessHoldAfterRebuild(ctx); err == nil || !held {
		t.Fatalf("a store whose settings cannot be read answered held=%v err=%v; an access decision must fail closed", held, err)
	}
}

// The hold describes one store's missing login, not the history in it, so it
// rides a backup in neither direction: exported, restoring the file onto a
// healthy install would hold that install off its own network; imported, a
// crafted file could close a published port with nothing saying why.
func TestTheAccessHoldNeverLeavesOrEntersInABackup(t *testing.T) {
	ctx := context.Background()
	src, err := Open(rebuiltStore(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	rows, err := src.ExportTable(ctx, "settings")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r["key"] == accessHoldKey {
			t.Fatalf("the export carries the rebuilt store's access hold: %v", r)
		}
	}

	dst, err := Open(filepath.Join(t.TempDir(), "dst.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if _, err := dst.ImportTable(ctx, "settings", []map[string]any{{"key": accessHoldKey, "value": "1700000000"}}); err != nil {
		t.Fatalf("import: %v", err)
	}
	if held, err := dst.AccessHoldAfterRebuild(ctx); err != nil || held {
		t.Fatalf("an imported file put an access hold on a healthy install (held=%v, err=%v)", held, err)
	}
}

// rebuiltStore leaves, at a fresh path, a database an armed Open rebuilt after
// finding it damaged - closed, the way the next start finds it.
func rebuiltStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	rebuilt, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("armed open: %v", err)
	}
	if !rebuilt.RebuiltAfterCorruption() {
		t.Fatal("fixture: nothing was rebuilt")
	}
	if err := rebuilt.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A rebuild builds its replacement beside the damaged file before that file
// moves. Anything that stops the build part-way - a full disk, an I/O error -
// has to leave the damaged file where it was: moved first, a failed build left
// an empty store with no hold row in it at the database's path, which the next
// start took for a healthy brand-new install and, under -access network, put on
// the network with no login.
func TestAFailedRebuildLeavesTheDamagedDatabaseWhereItIs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "auth_hash", "$2a$10$notarealhashbutlongenough"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	// What stops this build: the name it builds under is taken by something it
	// cannot clear.
	if err := os.MkdirAll(filepath.Join(path+rebuiltSuffix, "in-the-way"), 0o700); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path, RebuildOnCorruption())
	if err == nil {
		st2.Close()
		t.Fatal("the rebuild reported success with no replacement built")
	}
	if matches, _ := filepath.Glob(path + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("a rebuild that could not build its replacement set the damaged database aside anyway (%v): the next start finds an empty store at its path", matches)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var hash string
	err = db.QueryRow(`SELECT value FROM settings WHERE key = 'auth_hash'`).Scan(&hash)
	db.Close()
	if err != nil || hash == "" {
		t.Fatalf("the damaged database no longer holds the install's settings after the failed rebuild (auth_hash %q, %v)", hash, err)
	}

	// Cleared, the next armed start rebuilds, and the store it builds holds.
	if err := os.RemoveAll(path + rebuiltSuffix); err != nil {
		t.Fatal(err)
	}
	st3, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("the rebuild after the obstacle was cleared: %v", err)
	}
	defer st3.Close()
	if held, err := st3.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Fatalf("the rebuilt store does not hold the network (held=%v, err=%v)", held, err)
	}
}

// The one step between setting the damaged file aside and the replacement being
// in place is a rename, and a start can still die across it - a power cut, a
// kill. The next Open, with no flag at all, has to finish that rename rather
// than create a brand-new store at the empty path: the finished store beside it
// carries the hold on network access, and a new one does not. A WAL the damaged
// file still has at the path is set aside first, as the rebuild would have:
// renamed in beside it, the rebuilt store would be read through it.
func TestAnInterruptedRebuildIsFinishedByTheNextOpen(t *testing.T) {
	ctx := context.Background()
	path := rebuiltStore(t)
	if fi, err := os.Stat(path + "-wal"); err == nil && fi.Size() > 0 {
		t.Fatalf("fixture: the closed rebuilt store still has %d bytes in its WAL", fi.Size())
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	// The shape a start leaves when it dies between the two: the finished store
	// under its build name, nothing at the path, and the damaged file's WAL where
	// the set-aside had not yet reached it.
	if err := os.Rename(path, path+rebuiltSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("what is left of the damaged database's WAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logged)
	st, err := Open(path)
	log.SetOutput(orig)
	if err != nil {
		t.Fatalf("the next start could not open: %v", err)
	}
	defer st.Close()
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Fatalf("the next start made a new store at the empty path instead of finishing the rebuild (held=%v, err=%v): -access network puts that store on the network with no login", held, err)
	}
	// The start that put the store in place says so: it is the only record that
	// an earlier start was cut short in the middle of a rebuild.
	if !strings.Contains(logged.String(), "that store is in place now") {
		t.Errorf("the start that finished an interrupted rebuild said nothing about it:\n%s", logged.String())
	}
	if _, err := os.Stat(path + rebuiltSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Error("the finished replacement is still sitting beside the database")
	}
	if matches, _ := filepath.Glob(path + "-wal.*.corrupt"); len(matches) != 1 {
		t.Errorf("the damaged database's WAL was not set aside before the rebuilt store took its path (%v)", matches)
	}
}

// Only a missing database is finished from a replacement beside it. A database
// at its path - the damaged one a build died beside, or one the operator put
// back - opens as it is, and a directory under the replacement's name is
// nothing a rebuild made.
func TestAReplacementBesideADatabaseIsNotPutInItsPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "the operator's"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(rebuiltStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+rebuiltSuffix, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	all, err := st2.AllSettings(ctx)
	st2.Close()
	if err != nil || all["k"] != "the operator's" {
		t.Fatalf("a replacement beside a database that was still there took its place (k=%q, err=%v): the operator's store is gone", all["k"], err)
	}

	path = filepath.Join(t.TempDir(), "p.db")
	if err := os.MkdirAll(path+rebuiltSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("a directory under the replacement's name stopped a fresh open: %v", err)
	}
	st3.Close()
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("a directory under the replacement's name was put at the database's path (%v)", err)
	}
}

// A rebuild clears whatever an earlier one left under the name it builds
// under - a build a start died in the middle of, beside a damaged database that
// is still there - rather than fail on it or build on top of it.
func TestARebuildClearsWhatAnEarlierOneLeftBeside(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	for sfx, what := range map[string]string{"": "half a store: a build that died part-way", "-wal": "and its WAL"} {
		if err := os.WriteFile(path+rebuiltSuffix+sfx, []byte(what), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	st2, err := Open(path, RebuildOnCorruption())
	if err != nil {
		t.Fatalf("a rebuild over what an earlier one left behind failed: %v", err)
	}
	defer st2.Close()
	if held, err := st2.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Fatalf("the store rebuilt over an earlier build's leftovers does not hold the network (held=%v, err=%v)", held, err)
	}
	if left, _ := filepath.Glob(path + rebuiltSuffix + "*"); len(left) != 0 {
		t.Errorf("the earlier build's leftovers are still beside the database: %v", left)
	}
}

// The last step of a rebuild puts the store it built at the database's path, and
// until that rename lands the rebuild has not happened: the damaged file is set
// aside, nothing is at the path, and an Open that carried on would create a
// brand-new store there - no install anchor, no hold on network access - for a
// daemon told -access network to put on the network with no login. It stops
// instead, and the next Open finishes the rename; that Open's own rename is held
// to the same rule.
func TestARebuildThatCouldNotBePutInPlaceStartsNothingInItsPlace(t *testing.T) {
	ctx := context.Background()
	refuse := func(string, string) error { return errors.New("the rename was refused") }
	defer func() { renameIntoPlaceFn = os.Rename }()

	path := filepath.Join(t.TempDir(), "p.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	renameIntoPlaceFn = refuse
	st2, err := Open(path, RebuildOnCorruption())
	renameIntoPlaceFn = os.Rename
	if err == nil {
		st2.Close()
		t.Fatal("a rebuild whose store could not be put in place started anyway, on whatever it made at the empty path")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something was made at the database's path after the rebuilt store could not be put there (%v)", err)
	}
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("the next start: %v", err)
	}
	held, err := st3.AccessHoldAfterRebuild(ctx)
	st3.Close()
	if err != nil || !held {
		t.Fatalf("the next start did not finish the rebuild it found beside the empty path (held=%v, err=%v)", held, err)
	}

	// The next start's own rename, refused.
	path = rebuiltStore(t)
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(path, path+rebuiltSuffix); err != nil {
		t.Fatal(err)
	}
	renameIntoPlaceFn = refuse
	st4, err := Open(path)
	renameIntoPlaceFn = os.Rename
	if err == nil {
		st4.Close()
		t.Fatal("a start that could not put a finished rebuild in place started anyway, on a brand-new store at the empty path")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("something was made at the database's path after the finished rebuild could not be put there (%v)", err)
	}
	st5, err := Open(path)
	if err != nil {
		t.Fatalf("the start after that: %v", err)
	}
	defer st5.Close()
	if held, err := st5.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Fatalf("the start after that did not finish the rebuild (held=%v, err=%v)", held, err)
	}
}

// Before it puts a finished rebuild in place, the next Open sets aside what the
// damaged database left at the path - a WAL its own set-aside had not reached -
// because renamed in beside it, the rebuilt store would be read through it. If
// that set-aside fails, the rebuilt store stays where it is.
func TestAnInterruptedRebuildIsNotFinishedOverAWALThatCouldNotBeSetAside(t *testing.T) {
	path := rebuiltStore(t)
	for _, sfx := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + sfx); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.Rename(path, path+rebuiltSuffix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", []byte("what is left of the damaged database's WAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What stops the set-aside: every name it could take for the next half
	// minute is a directory with something in it.
	now := time.Now().UTC()
	for s := -2; s <= 30; s++ {
		taken := path + "-wal." + now.Add(time.Duration(s)*time.Second).Format("20060102T150405Z") + ".corrupt"
		if err := os.MkdirAll(filepath.Join(taken, "in-the-way"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := Open(path)
	if err == nil {
		st.Close()
		t.Fatal("the next start finished the rebuild over a WAL it could not set aside")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rebuilt store was put in place beside a WAL that is not its own (%v)", err)
	}
	if fi, err := os.Lstat(path + rebuiltSuffix); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("the finished rebuild is no longer beside the database, to be put in place once the WAL can be set aside (%v)", err)
	}
}

// The release line says what this call did. Two loads can judge the same store at
// once - a reload signal landing beside the settings retry, or the Access tab's
// release beside a load - and the one that reads the hold after the other has
// deleted it finds nothing left to remove; the line is the other call's to give.
// A trigger that skips the delete gives this call that same answer: the row it
// read, and nothing removed.
func TestTheReleaseLineIsGivenOnlyByTheCallThatReleased(t *testing.T) {
	ctx := context.Background()
	st, err := Open(rebuiltStore(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetSettings(ctx, map[string]string{"auth_enabled": "1", "auth_hash": "$2a$10$notarealhashbutlongenough"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER released_elsewhere BEFORE DELETE ON settings
		WHEN OLD.key = 'access_hold_after_rebuild'
		BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&logged)
	held, err := st.AccessHoldAfterRebuild(ctx)
	log.SetOutput(orig)
	if err != nil || held {
		t.Fatalf("a store with a login set on it answered held=%v, err=%v", held, err)
	}
	if logged.Len() != 0 {
		t.Errorf("a call whose delete removed nothing says it released the hold:\n%s", logged.String())
	}
}
