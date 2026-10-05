package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// A store that was never given a save interval writes every round at once,
// and everything about batched saves passes its tests on such a store. So
// whether the daemon batches at all hangs on a few lines of run(), which no
// unit test reaches. They are guarded literally, the way the restore drain is
// (TestMainWiresTheRestoreDrain).
func TestMainWiresBatchedSaving(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		"p.store.SetSaveEveryFn(set.SaveEvery)", // without it no round is ever held
		"p.store.SetSaveLogFn(",                 // a failed save has no other way into the log the dashboard shows
		"p.store.SaveBuffered(store.SaveStop)",  // the save at shutdown that does not wait for the workers
		"defer p.store.Close()",                 // and the close, which saves what that one could not
		"SaveEvery:            5 * time.Minute", // on by default
		"2*store.FinalSaveBudget",               // Stop waits for both saves of a shutdown
		`"save_every", set.SaveEvery()`,         // the startup line says what is in force
		"range store.SaveReasons()",             // the counters are seeded from the list they are booked from
	} {
		if !strings.Contains(src, want) {
			t.Errorf("main.go no longer has %q", want)
		}
	}
	// The interval is wired before the load's outcome is looked at. A load
	// that failed still returned a controller, and the daemon runs on it.
	load := strings.Index(src, "set, err := settings.New(ctx, p.store, def, setOpts...)")
	wire := strings.Index(src, "p.store.SetSaveEveryFn(set.SaveEvery)")
	judge := strings.Index(src, "if errors.Is(err, settings.ErrLegacyReseal) {")
	if load < 0 || judge < 0 || wire < load || wire > judge {
		t.Error("the save interval is no longer wired between the settings load and the first look at its error: " +
			"one of the ways out of the load would leave the daemon writing every round")
	}
	// The save at shutdown comes once the monitor has returned, which is
	// when no more readings can arrive, and before run returns into the wait
	// for the workers.
	ran := strings.Index(src, "m.Run(ctx)")
	saved := strings.Index(src, "p.store.SaveBuffered(store.SaveStop)")
	stopped := strings.Index(src, `p.log.Info("pingularity stopped")`)
	if ran < 0 || stopped < 0 || saved < ran || saved > stopped {
		t.Error("the save at shutdown is no longer between the monitor's return and the end of run")
	}
}

// TestSampleSaveCountersAreSeeded holds the counters the store books for
// batched saves and seedKnownCounters together, like
// TestWALTrimCountersAreSeeded and for its reason: a counter that first
// appears at 1 hides its first step from rate() and increase(), and the first
// failed save is the one to alert on.
//
// The gauge beside them is the other way round. Seeding creates counters, so
// a seeded db.sample_rows_waiting would put one name in both families of a
// scrape.
func TestSampleSaveCountersAreSeeded(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root
	storeSrc, err := os.ReadFile(filepath.Join(root, "internal", "store", "store.go"))
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	recorded := regexp.MustCompile(`stats\.(?:Inc|Add)\("(db\.sample_[a-z_.]+)"`).FindAllStringSubmatch(string(storeSrc), -1)
	seen := map[string]bool{}
	var keys []string
	for _, m := range recorded {
		if k := m[1]; !seen[k] && !strings.HasSuffix(k, ".") {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	// The saves that went through are booked under a name built from the
	// reason, which the scan above cannot read.
	if !strings.Contains(string(storeSrc), `stats.Inc("db.sample_saves." + string(reason))`) {
		t.Fatal("internal/store no longer books a save as db.sample_saves.<reason>: this guard is reading the wrong text")
	}
	for _, r := range store.SaveReasons() {
		keys = append(keys, "db.sample_saves."+string(r))
	}
	sort.Strings(keys)
	if len(keys) < 4+len(store.SaveReasons()) {
		t.Fatalf("found %v in internal/store/store.go, want the four fixed counters and one for every reason: "+
			"either they were removed, or the call shape changed and this guard now passes on anything", keys)
	}
	stats.ResetForTest()
	seedKnownCounters()
	snap := stats.Lifetime()
	for _, k := range keys {
		if v, ok := snap.Counters[k]; !ok || v != 0 {
			t.Errorf("%s is %d, present %v after seeding; want it present at 0", k, v, ok)
		}
	}
	gauges := regexp.MustCompile(`stats\.Set\("(db\.sample_[a-z_.]+)"`).FindAllStringSubmatch(string(storeSrc), -1)
	if len(gauges) == 0 {
		t.Fatal("found no db.sample_* gauge in internal/store/store.go")
	}
	for _, m := range gauges {
		if _, ok := snap.Counters[m[1]]; ok {
			t.Errorf("%s is a gauge and seedKnownCounters seeds it as a counter: a scrape would carry the name in both families", m[1])
		}
	}
}
