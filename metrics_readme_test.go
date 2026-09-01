package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// promMetricName matches one Prometheus metric name, in README prose or in Go
// source. The trailing `+` is load-bearing: the install instructions carry
// package globs like pingularity_*.deb, and `*` is not part of a name, so those
// never match.
var promMetricName = regexp.MustCompile(`pingularity_[a-z0-9_]+`)

// TestREADMEMetricsInventoryMatchesTheExposition holds the metrics inventory in
// docs/metrics.md to the families /metrics actually exports. The inventory lived
// in README.md until it was split out; the README now carries only a summary and
// a link, so this test follows the prose rather than the filename.
//
// That inventory is maintained BY HAND - one bullet per family, carrying the
// prose that says when a series goes absent and what its labels mean - and
// nothing regenerates it, yet it is the only place those names are explained:
// the endpoint itself ships one line of HELP each. So a family added to the
// exposition and forgotten here is invisible to everyone who did not read the
// diff. That is not hypothetical - it is the drift this test was written after,
// where the six chart-aggregate cache counters and the
// pingularity_series_query_seconds histogram shipped while the inventory named
// neither, reachable in the meantime only as pingularity_stat_total{stat}.
//
// The check runs both ways round on purpose. A family the exposition emits with
// no mention in the README is an undocumented metric; a name the README carries
// that the exposition no longer contains is a bullet for a series nobody can
// scrape. Either is a defect in the same document, and only the pair of them
// pins it.
//
// Two limits, stated so they are not mistaken for coverage. The forward
// direction reads the two hand-written tables, writeHistograms and
// writeNamedStats, because those are lists a new entry is appended to and
// forgotten; the gauges handleMetrics prints inline are held only by the reverse
// direction. And nothing here checks the PROSE - whether a bullet still
// describes when its series is absent needs a human to read it. This only makes
// a NAME impossible to leave behind.
//
// The README is deliberately NOT read here. Its summary names no metric, so
// checking it either way round would fail on every family or pass on anything.
func TestREADMEMetricsInventoryMatchesTheExposition(t *testing.T) {
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

	inventory := read(filepath.Join("docs", "metrics.md"))
	webSrc := read(filepath.Join("internal", "web", "web.go"))

	exported := exportedMetricNames(t, webSrc)
	if len(exported) < 20 {
		t.Fatalf("found only %d metric names in the exposition tables - the slice has lost its shape, and this test would pass on anything", len(exported))
	}
	for _, name := range exported {
		if !strings.Contains(inventory, name) {
			t.Errorf("/metrics exports %s but docs/metrics.md never names it - add it to the metrics inventory, beside the family it belongs with", name)
		}
	}

	for _, name := range uniqueSorted(promMetricName.FindAllString(inventory, -1)) {
		if !strings.Contains(webSrc, trimSampleSuffix(name)) {
			t.Errorf("docs/metrics.md documents %s, which internal/web/web.go no longer exports - drop the entry, or restore the metric", name)
		}
	}
}

// exportedMetricNames returns the metric names the two hand-written exposition
// tables emit, deduplicated and with the _sum/_count/_bucket sample suffixes
// trimmed back to the family name the README documents.
//
// Comments are stripped first. Both functions carry doc comments that MENTION
// families they do not emit (writeNamedStats explains itself against
// pingularity_stat_total, which writeStatMetrics owns), and a name discussed in
// prose is not a name exported here.
func exportedMetricNames(t *testing.T, webSrc string) []string {
	t.Helper()
	const from, to = "func writeHistograms(", "func writeStatMetrics("
	start, end := strings.Index(webSrc, from), strings.Index(webSrc, to)
	if start < 0 || end < start {
		t.Fatalf("internal/web/web.go no longer holds %q ... %q in that order - the exposition was restructured, and this test now reads the wrong region", from, to)
	}

	var code []string
	for _, line := range strings.Split(webSrc[start:end], "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		code = append(code, line)
	}

	var names []string
	for _, n := range promMetricName.FindAllString(strings.Join(code, "\n"), -1) {
		names = append(names, trimSampleSuffix(n))
	}
	return uniqueSorted(names)
}

// trimSampleSuffix reduces one sample's name to its family's: a histogram and a
// summary are documented under one name in the README, but reach the wire as
// _bucket/_sum/_count.
func trimSampleSuffix(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if trimmed, ok := strings.CutSuffix(name, suffix); ok {
			return trimmed
		}
	}
	return name
}

// uniqueSorted deduplicates and orders, so failures read in a stable order
// rather than the order the file happens to mention things in.
func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
