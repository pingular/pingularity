package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// TestSeriesCountersAreSeededAndExported guards the two lists that have to move
// together whenever the chart-aggregate cache learns a new outcome, and which
// nothing else checks: seedKnownCounters in this file's package (main.go) and
// the named-family table in writeNamedStats (internal/web/web.go).
//
// The failure this exists to catch has already happened twice. A state is added
// to Store.Series, the store's own tests pass because they read the counter
// directly, and the two lists are left behind - so the new counter is absent
// from a scrape until its first increment (unseeded counters spring into
// existence at 1, which rate() and increase() cannot see across that boundary)
// and, if it is also missing from writeNamedStats, has no named family at all.
// The visible symptom is a scrape whose numbers do not reconcile: main.go's
// seeding comment states the relation the families are read against each other
// with, queries = outcomes - hits, and a missing member silently breaks it.
//
// Scanning the store's source rather than hardcoding the names is the point.
// A fixed list here would need the same manual update as the two lists it
// guards, and would therefore go stale in exactly the case the test exists for.
func TestSeriesCountersAreSeededAndExported(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root

	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	// Every stats.Inc("series...") the store records. The store is the only
	// producer: these counters describe its cache, and a counter incremented
	// anywhere else would not be part of the queries = outcomes - hits relation.
	storeSrc := read(filepath.Join("internal", "store", "store.go"))
	recorded := regexp.MustCompile(`stats\.Inc\("(series\.[a-z.]+)"\)`).FindAllStringSubmatch(storeSrc, -1)
	if len(recorded) == 0 {
		t.Fatal("no stats.Inc(\"series.*\") calls found in internal/store/store.go: " +
			"either the counters were removed, or the call shape changed and this guard now " +
			"scans for something that no longer exists, which would let it pass vacuously")
	}

	seen := map[string]bool{}
	var keys []string
	for _, m := range recorded {
		if !seen[m[1]] {
			seen[m[1]] = true
			keys = append(keys, m[1])
		}
	}
	sort.Strings(keys)

	mainSrc := read("main.go")
	webSrc := read(filepath.Join("internal", "web", "web.go"))
	for _, k := range keys {
		quoted := `"` + k + `"`
		if !strings.Contains(mainSrc, quoted) {
			t.Errorf("internal/store records %s but main.go never names it: an unseeded counter is "+
				"absent from /metrics until its first increment, so its family appears mid-series and "+
				"rate() cannot see the step. Add it to seedKnownCounters.", k)
		}
		if !strings.Contains(webSrc, quoted) {
			t.Errorf("internal/store records %s but internal/web/web.go never names it: with no entry "+
				"in writeNamedStats it has no pingularity_series_* family, so a scrape carries a query "+
				"count its outcome counters cannot account for. Add it to the named-family table.", k)
		}
	}
}

// callerFile isolates the runtime.Caller call so the test body reads plainly.
func callerFile() (uintptr, string, bool) {
	pc, file, _, ok := runtime.Caller(1)
	return pc, file, ok
}
