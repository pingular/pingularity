package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// speedtestTriggers is RunOnce's closed trigger enum, as seedKnownCounters
// spells it for speed.run.<trigger>.
var speedtestTriggers = []string{"startup", "scheduled", "reconnect", "degraded", "manual"}

// TestSpeedtestHoldCountersAreSeededAndExported guards the per-trigger counters
// the monitor keeps about rounds our own speedtests overlapped. They are read
// against each other (failed over total, the delay sum over the delayed count),
// so every family has to exist from boot for every trigger, and each needs its
// named family on /metrics. The prefixes are scanned from the monitor's source
// rather than listed here, so a new one cannot be added and forgotten.
func TestSpeedtestHoldCountersAreSeededAndExported(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}
	monSrc := read(filepath.Join("internal", "monitor", "monitor.go"))
	found := regexp.MustCompile(`stats\.(Inc|AddF)\("(monitor\.speedtest_[a-z_]+\.)"`).FindAllStringSubmatch(monSrc, -1)
	if len(found) == 0 {
		t.Fatal("no stats.Inc/AddF(\"monitor.speedtest_*.\" + trigger) calls found in internal/monitor/monitor.go: " +
			"either the counters were removed, or the call shape changed and this guard would pass vacuously")
	}
	kinds := map[string]string{}
	for _, m := range found {
		kinds[m[2]] = m[1]
	}
	var prefixes []string
	for p := range kinds {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	stats.ResetForTest()
	seedKnownCounters()
	snap := stats.Lifetime()
	webSrc := read(filepath.Join("internal", "web", "web.go"))
	for _, p := range prefixes {
		for _, trig := range speedtestTriggers {
			k := p + trig
			var seeded bool
			if kinds[p] == "AddF" {
				_, seeded = snap.Floats[k]
			} else {
				_, seeded = snap.Counters[k]
			}
			if !seeded {
				t.Errorf("%s is recorded by the monitor but not seeded by seedKnownCounters: its family "+
					"appears mid-series and rate() cannot see the first step", k)
			}
		}
		if !strings.Contains(webSrc, `emitFamily("`+p+`"`) {
			t.Errorf("the monitor records %s<trigger> but writeNamedStats has no emitFamily for it", p)
		}
	}
	if len(prefixes) != 6 {
		t.Errorf("found %d per-trigger speedtest families (%v), want the six documented in docs/metrics.md", len(prefixes), prefixes)
	}
}

// TestSpeedtestWireIsWired holds main to connecting the scheduler's wire counter
// to the monitor before the probe loop starts (or every round counts, and our
// own tests can start outages again) and to the web server (or the speedtest
// times behind the latency chart's hover note never refresh).
func TestSpeedtestWireIsWired(t *testing.T) {
	_, thisFile, ok := callerFile()
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(b)
	wire := strings.Index(src, "m.SpeedtestActivityFn = ")
	run := strings.Index(src, "m.Run(ctx)")
	if wire < 0 || run < 0 || wire > run {
		t.Errorf("main.go must assign m.SpeedtestActivityFn before m.Run(ctx) (at %d, run at %d)", wire, run)
	}
	if !strings.Contains(src, "srv.SpeedActivityFn = sched.Activity") {
		t.Error("main.go must hand the scheduler's Activity to the web server")
	}
}
