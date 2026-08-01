package store

import (
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// repairUnreadableIntColumns is a full scan of samples/dns/speed (typeof() on every
// row) that ran unconditionally at every Open, before the listener binds - a cost
// paid on every start for a migration that only needs to happen once. A persisted,
// versioned generation marker (PRAGMA user_version, which is per-DB and NOT carried
// by export/import) gates it: once the current generation is stamped the scan is
// skipped, and only a DB that has never reached this generation pays for it.

// setUserVersion writes the raw PRAGMA the way a build stamps (or a pre-gate build
// left it: 0). PRAGMA takes no bound parameter; the value is a test literal.
func setUserVersion(t *testing.T, s *Store, v int64) {
	t.Helper()
	if _, err := s.db.Exec(`PRAGMA user_version = ` + strconv.FormatInt(v, 10)); err != nil {
		t.Fatalf("set user_version %d: %v", v, err)
	}
}

func userVersion(t *testing.T, s *Store) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func TestOpenIntColumnRepairRunsOncePerGeneration(t *testing.T) {
	path := t.TempDir() + "/gen.db"
	now := time.Now().Unix()

	// A legacy DB: on-disk residue a pre-gate build left, with no generation marker
	// (a pre-gate build never stamped one - model that with user_version = 0).
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedRaw(t, s, `INSERT INTO speed (ts, server, healthy) VALUES (?, 'srv', 'yes')`, now-600)
	setUserVersion(t, s, 0)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// First Open with work to do: the poison is repaired and the marker stamped.
	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	var healthy any
	if err := s2.db.QueryRow(`SELECT healthy FROM speed`).Scan(&healthy); err != nil {
		t.Fatalf("read healthy: %v", err)
	}
	if healthy != nil {
		t.Errorf("first Open left healthy = %v, want the unreadable flag stripped to NULL", healthy)
	}
	if got := stats.Lifetime().Counters["db.int_columns_repaired"]; got != 1 {
		t.Errorf("db.int_columns_repaired = %d on the first Open of a poisoned DB, want 1", got)
	}
	if uv := userVersion(t, s2); uv == 0 {
		t.Fatalf("first Open repaired the DB but left the generation marker unstamped (user_version=0); " +
			"the full scan will re-run on every future start")
	}

	// Fresh poison written directly AFTER the stamp (a raw-SQL write, the accepted
	// limitation): the gate must now SKIP the scan, so this survives untouched.
	seedRaw(t, s2, `INSERT INTO speed (ts, server, healthy) VALUES (?, 'srv', 'no')`, now-300)
	if err := s2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("third open: %v", err)
	}
	defer s3.Close()
	var poisoned int
	if err := s3.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE typeof(healthy) = 'text'`).Scan(&poisoned); err != nil {
		t.Fatalf("count poisoned: %v", err)
	}
	if poisoned != 1 {
		t.Errorf("the gate did not skip: fresh poison was scanned and repaired on a stamped DB (found %d text "+
			"healthy flags, want 1); the once-per-generation marker must stop the full scan re-running every start", poisoned)
	}
	if got := stats.Lifetime().Counters["db.int_columns_repaired"]; got != 0 {
		t.Errorf("db.int_columns_repaired = %d on a reopen whose generation is already stamped, want 0", got)
	}
}

// A clean DB stamps the marker on its first Open too, so the scan never runs on the
// second Open even after poison is written directly afterwards.
func TestOpenStampsRepairGenerationOnACleanDB(t *testing.T) {
	path := t.TempDir() + "/clean_gen.db"
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if uv := userVersion(t, s); uv == 0 {
		t.Fatalf("a clean first Open left the generation marker at 0; the int-column scan will re-run on every start forever")
	}
	// Poison written directly after the clean stamp: the gate skips it.
	now := time.Now().Unix()
	seedRaw(t, s, `INSERT INTO speed (ts, server, healthy) VALUES (?, 'srv', 'yes')`, now-600)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stats.ResetForTest()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if got := stats.Lifetime().Counters["db.int_columns_repaired"]; got != 0 {
		t.Errorf("db.int_columns_repaired = %d: a clean DB stamped the marker on first Open, so the scan must not run again", got)
	}
	var poisoned int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM speed WHERE typeof(healthy) = 'text'`).Scan(&poisoned); err != nil {
		t.Fatalf("count poisoned: %v", err)
	}
	if poisoned != 1 {
		t.Errorf("the gate did not skip on a clean-stamped DB: found %d text healthy flags, want 1", poisoned)
	}
}
