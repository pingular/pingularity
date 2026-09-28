package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// TestWALTrimCountersAreSeeded holds the store's trim outcomes and
// seedKnownCounters together. The three are read against each other (trims
// blocked over trims tried), so one that appears at 1 while the others sit at
// 0 skews the ratio, and its first step is invisible to rate() and increase().
//
// It scans the store's source for the names, like
// TestSeriesCountersAreSeededAndExported: a fixed list here would need the
// same manual update as the list it guards. Naming a counter in main.go is
// not seeding it, so the seeding is run as well and each name looked up at 0.
// These counters have no named family. They reach a scrape as
// pingularity_stat_total{stat="db.wal_..."}, which internal/web pins.
func TestWALTrimCountersAreSeeded(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root
	storeSrc, err := os.ReadFile(filepath.Join(root, "internal", "store", "store.go"))
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	recorded := regexp.MustCompile(`stats\.Inc\("(db\.wal_[a-z_.]+)"\)`).FindAllStringSubmatch(string(storeSrc), -1)
	seen := map[string]bool{}
	var keys []string
	for _, m := range recorded {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	sort.Strings(keys)
	if len(keys) < 3 {
		t.Fatalf("found %d stats.Inc(\"db.wal_*\") names in internal/store/store.go, want the three trim outcomes: "+
			"either they were removed, or the call shape changed and this guard now passes on anything", len(keys))
	}
	mainSrc, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	stats.ResetForTest()
	seedKnownCounters()
	snap := stats.Lifetime()
	for _, k := range keys {
		if !strings.Contains(string(mainSrc), `"`+k+`"`) {
			t.Errorf("internal/store records %s but main.go never names it: add it to seedKnownCounters", k)
		}
		if v, ok := snap.Counters[k]; !ok || v != 0 {
			t.Errorf("%s is %d, present %v after seeding; want it present at 0", k, v, ok)
		}
	}
}

// TestEveryDatabaseConnectionCarriesTheLogLimit holds the size limit of the
// write-ahead log to every place a connection is opened. The limit belongs to
// the connection, not to the file: a commit from a connection opened without
// it restarts the log and leaves the file as big as it grew. So every
// sql.Open in shipped code has to build its DSN with the store's buildDSN,
// which is what appends the pragmas.
func TestEveryDatabaseConnectionCarriesTheLogLimit(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root
	opens := regexp.MustCompile(`sql\.Open\(`)
	viaDSN := regexp.MustCompile(`sql\.Open\("sqlite", buildDSN\(`)
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			if j := strings.Index(line, "//"); j >= 0 {
				line = line[:j]
			}
			if !opens.MatchString(line) {
				continue
			}
			found++
			if filepath.ToSlash(rel) != "internal/store/store.go" || !viaDSN.MatchString(line) {
				t.Errorf("%s:%d opens a database without buildDSN: its connections carry no journal_size_limit (and no busy_timeout), "+
					"so a commit from one of them leaves the write-ahead log at whatever size it grew to", rel, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if found < 3 {
		t.Fatalf("found %d sql.Open calls, want the store's three at least: the scan is reading the wrong text", found)
	}
}
