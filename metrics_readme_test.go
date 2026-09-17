package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/web"
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

// TestSpeedHealthyBulletDescribesTheVerdictTheScrapeCarries holds the one bullet
// an operator builds an alert on - and the HELP line the scrape ships beside the
// gauge, which repeats it - to what pingularity_speed_healthy really carries.
// The inventory test above pins only NAMES, and this bullet's prose was wrong in
// the way that matters: it said the series is absent "when no thresholds are
// configured", present tense, as though the scrape judged the last run against
// the thresholds of the moment. It does not. The verdict is decided once, when
// the run happens, against the thresholds in force then, and stored on the row
// - the rule the README's Alerts bullet states - so clearing every threshold to
// retire the `== 0` recipe left the old 0 exported until the next run landed
// (with scheduled tests off, never), and a threshold tightened since reported a
// pass that the same scrape's download figure contradicted. Whichever way the
// exposition goes, the three surfaces have to describe the same moment.
func TestSpeedHealthyBulletDescribesTheVerdictTheScrapeCarries(t *testing.T) {
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
	bullet := bulletContaining(t, read(filepath.Join("docs", "metrics.md")), "`pingularity_speed_healthy` - ")
	alerts := bulletContaining(t, read(filepath.Join("docs", "dashboard.md")), "*Thresholds* (min download/upload")

	body := scrapeWithThresholdsCleared(t)
	line, present := metricLine(body, "pingularity_speed_healthy")
	if !present {
		// The scrape drops the series with the thresholds: then the bullet's
		// promise is true as written and has to stay.
		if !strings.Contains(plainProse(bullet), "absent when no thresholds are configured") {
			t.Errorf("with every threshold cleared the scrape no longer carries pingularity_speed_healthy, but the docs/metrics.md bullet no longer says so:\n%s", bullet)
		}
		return
	}
	if line != "pingularity_speed_healthy 0" {
		t.Fatalf("scrape carries %q with every threshold cleared and an unhealthy verdict stored on the last run; expected the stored 0", line)
	}
	help := speedHealthyHelp(t, body)
	for _, s := range []struct{ name, text string }{
		{"the docs/metrics.md bullet", bullet},
		{"the # HELP line the scrape ships", help},
		{"the README's Alerts bullet", alerts},
	} {
		if !strings.Contains(plainProse(s.text), "when it ran") {
			t.Errorf("%s never says the verdict is the one judged when the run happened, but that is what the gauge carries: with every threshold cleared the scrape still exported %q\n%s", s.name, line, s.text)
		}
	}
	for _, s := range []struct{ name, text string }{
		{"the docs/metrics.md bullet", bullet},
		{"the # HELP line the scrape ships", help},
	} {
		if strings.Contains(plainProse(s.text), "absent when no thresholds are configured") {
			t.Errorf("%s promises the series is absent when no thresholds are configured, but with every threshold cleared the scrape still exported %q - the promise names the wrong moment, and clearing thresholds is how the recipes below the bullet suggest retiring the alert\n%s", s.name, line, s.text)
		}
	}
}

// scrapeWithThresholdsCleared serves /metrics for the install an operator reaches
// by clearing thresholds to silence the `== 0` alert: no threshold configured,
// and a last run whose stored verdict is "unhealthy".
func scrapeWithThresholdsCleared(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	unhealthy := false
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Unix() - 60, Server: "Sponsor, City", Trigger: "scheduled", Engine: "ookla",
		DownMbps: 94.5, UpMbps: 12.5, PingMS: 8.5, Healthy: &unhealthy,
	}); err != nil {
		t.Fatalf("insert judged run: %v", err)
	}
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if set.Thresholds().Any() {
		t.Fatal("the fixture has a threshold configured; it is meant to have none")
	}
	status := func() web.LiveStatus {
		return web.LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	h := web.New(st, status, nil, set, nil, "test", slog.New(slog.DiscardHandler)).Handler()
	req := httptest.NewRequest("GET", "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:54321" // local-only access is the default
	req.Host = "127.0.0.1:9000"        // and the DNS-rebinding guard refuses example.com
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: status %d, body %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// metricLine returns an unlabelled sample's line from a scrape, and whether the
// scrape carried the series at all.
func metricLine(body, name string) (string, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, name+" ") {
			return line, true
		}
	}
	return "", false
}

// speedHealthyHelp is the HELP line beside the gauge - the only sentence about
// it a reader of the endpoint alone ever sees.
func speedHealthyHelp(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "# HELP pingularity_speed_healthy ") {
			return line
		}
	}
	t.Fatalf("scrape carries pingularity_speed_healthy with no HELP line:\n%s", body)
	return ""
}

// plainProse strips markdown emphasis and line wrapping so a phrase can be
// matched across "**absent** when no\n  thresholds" and the HELP line alike.
func plainProse(text string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("**", "", "*", "", "`", "").Replace(text)), " ")
}
