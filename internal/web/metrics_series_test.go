package web

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// sampleValue returns the value of an unlabelled sample, or -1 if absent.
func sampleValue(body, name string) float64 {
	for _, ln := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(ln, name+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err != nil {
				return -1
			}
			return v
		}
	}
	return -1
}

// famBlock returns one metric family's exposition block: its # HELP line
// through its last sample, i.e. up to the next family's # HELP.
func famBlock(body, fam string) string {
	i := strings.Index(body, "# HELP "+fam+" ")
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest[1:], "\n# HELP "); j >= 0 {
		return rest[:j+2]
	}
	return rest
}

// THE CHART-AGGREGATE CACHE MUST BE OBSERVABLE, AND AT FIXED CARDINALITY. Every
// series.* counter exists for this endpoint alone - nothing else in the process
// reads the registry (handleMetrics makes the sole non-test stats.Lifetime
// call) - so a key no exporter carries is recorded into a black hole. Each is
// carried twice over: writeNamedStats gives it a well-named family from a
// hand-written row, without ever consulting promStat ("is the cache working"
// should not require decoding stat labels), and writeStatMetrics carries it
// again as a stat label, which promStat does gate. This test pins the named
// families; TestMetricsSeriesUnnamedKeyReachesOnlyStatTotal below pins the
// gated path - the only one a future series.* key would have.
//
// Every recorded state needs a row of its own here: the values below are all
// distinct, so a row wired to a sibling's key reads as that sibling's number
// rather than passing silently.
func TestMetricsSeriesCacheCountersExposed(t *testing.T) {
	stats.ResetForTest()
	stats.Add("series.cache.hit", 7)
	stats.Add("series.cache.expired", 3)
	stats.Add("series.cache.new", 2)
	stats.Add("series.cache.empty", 13)
	stats.Add("series.bypass", 11)
	stats.Add("series.query", 5)

	body := scrape(t, newMetricsServer(t))
	for _, want := range []string{
		"pingularity_series_cache_hits_total 7",
		"pingularity_series_cache_expired_total 3",
		"pingularity_series_cache_new_total 2",
		"pingularity_series_cache_empty_total 13",
		"pingularity_series_bypass_total 11",
		"pingularity_series_queries_total 5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s - the recorder exists but the exposition never names it", want)
		}
	}
	// Fixed cardinality: one sample per family. A bucket-width, window or
	// target label here would mint a new series per dashboard range.
	for _, fam := range []string{
		"pingularity_series_cache_hits_total",
		"pingularity_series_cache_expired_total",
		"pingularity_series_cache_new_total",
		"pingularity_series_cache_empty_total",
		"pingularity_series_bypass_total",
		"pingularity_series_queries_total",
	} {
		if n := strings.Count(body, "\n"+fam+" "); n != 1 {
			t.Errorf("%s emitted %d unlabelled samples, want exactly 1", fam, n)
		}
		if strings.Contains(body, fam+"{") {
			t.Errorf("%s carries a label - these families are fixed-cardinality by contract", fam)
		}
	}
}

// A SEEDED COUNTER IS ONLY WORTH SEEDING IF ZERO IS EXPOSED. The daemon creates
// the whole set at 0 at startup (seedKnownCounters, main.go) so the first cache
// outcome after a restart reads as a 0->1 step that rate()/increase() can see.
// That buys nothing if the exposition skips a key still sitting at 0: the series
// would again appear only with the first event, at 1, exactly the hole seeding
// exists to close. writeNamedStats tests each key for PRESENCE in the snapshot,
// never for a nonzero value - pinned here, because "don't emit zeros" is a
// tempting and silent regression.
func TestMetricsSeriesCountersExposedAtZero(t *testing.T) {
	stats.ResetForTest()
	stats.Seed("series.cache.hit", "series.cache.expired", "series.cache.new",
		"series.cache.empty", "series.bypass", "series.query")

	body := scrape(t, newMetricsServer(t))
	for _, name := range []string{
		"pingularity_series_cache_hits_total",
		"pingularity_series_cache_expired_total",
		"pingularity_series_cache_new_total",
		"pingularity_series_cache_empty_total",
		"pingularity_series_bypass_total",
		"pingularity_series_queries_total",
	} {
		switch v := sampleValue(body, name); {
		case v < 0:
			t.Errorf("%s absent though seeded at 0 - its first increment would be invisible to rate()", name)
		case v != 0:
			t.Errorf("%s = %g, want 0 (nothing ran a query in this test)", name, v)
		}
	}
}

// THE GATED PATH IS THE ONLY ONE A FUTURE series.* KEY HAS. The named families
// above come from hand-written rows in writeNamedStats, which builds its
// accumulator straight from the snapshot and never calls promStat; a key with no
// row there reaches the exposition only as a pingularity_stat_total stat label,
// and THAT writer filters through promStat. So dropping the "series." prefix
// from promStat costs the named families nothing visible while erasing every
// later key - recorded, never exposed, no log and no error - and those families
// still reporting is what would hide it.
func TestMetricsSeriesUnnamedKeyReachesOnlyStatTotal(t *testing.T) {
	stats.ResetForTest()
	stats.Add("series.cache.hit", 4) // has a named family, and a stat label too
	// Stand-in for the next series.* counter someone adds: no row in
	// writeNamedStats, so the label family is its whole exposition.
	stats.Add("series.unnamed_probe", 9)

	body := scrape(t, newMetricsServer(t))
	for _, want := range []string{
		`pingularity_stat_total{stat="series.unnamed_probe"} 9`,
		`pingularity_stat_total{stat="series.cache.hit"} 4`, // the classified prefix carries both
		"pingularity_series_cache_hits_total 4",             // and the named family is independent of it
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q - a series.* key promStat does not classify is exposed nowhere at all", want)
		}
	}
	if strings.Contains(body, "pingularity_series_unnamed_probe") {
		t.Error("an unnamed series.* key grew a family of its own; writeNamedStats emits only the keys its tables name")
	}
}

// THE OUTCOMES MUST ADD UP ON THE WIRE, NOT ONLY IN THE REGISTRY. Store.Series
// books exactly one outcome per call, and every outcome but the cache hit goes
// on to run a scan, so a scrape has to satisfy queries = new + empty + expired +
// bypass. An outcome the exposition does not carry does not merely go missing,
// it unbalances that sum: the four polls below book four queries, and without a
// row for the empty-window re-scan only two of them have a visible cause, with
// nothing on the endpoint to say where the other two came from.
//
// The store here is empty, which is where that outcome is the common case and
// not a corner: an empty result is never pinned (Store.Series, in
// internal/store/store.go), so the first wide poll is .new and every wide poll
// after it is .empty. All three address one entry - the start is floored to the
// bucket and the end is left open, so the key is the same for all of them.
func TestMetricsSeriesOutcomesReconcileWithQueries(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	ctx, now := context.Background(), time.Now()
	for i := 0; i < 3; i++ {
		if _, err := s.store.Series(ctx, now.Add(-24*time.Hour), time.Time{}, 300, nil); err != nil {
			t.Fatalf("wide-bucket series %d: %v", i, err)
		}
	}
	if _, err := s.store.Series(ctx, now.Add(-5*time.Minute), time.Time{}, 1, nil); err != nil {
		t.Fatalf("sub-minute series: %v", err)
	}

	body := scrape(t, s)
	for _, m := range []struct {
		name string
		want float64
		why  string
	}{
		{"pingularity_series_cache_new_total", 1, "the first wide poll had no cache entry"},
		{"pingularity_series_cache_empty_total", 2, "the two wide polls after it re-scanned a window with nothing to pin"},
		{"pingularity_series_bypass_total", 1, "the sub-minute window never consulted the cache"},
		{"pingularity_series_queries_total", 4, "every one of those four polls ran an aggregate"},
	} {
		if got := sampleValue(body, m.name); got != m.want {
			t.Errorf("%s = %g, want %g (%s)", m.name, got, m.want, m.why)
		}
	}
	// An outcome family the scrape does not carry counts as 0 here, which is
	// precisely how a missing row breaks the identity below rather than being
	// caught as an absence. Hits and expiries are legitimately absent in this
	// run: nothing was ever pinned, so nothing could be hit or expire.
	val := func(name string) float64 {
		if v := sampleValue(body, name); v >= 0 {
			return v
		}
		return 0
	}
	scans := val("pingularity_series_cache_new_total") + val("pingularity_series_cache_empty_total") +
		val("pingularity_series_cache_expired_total") + val("pingularity_series_bypass_total")
	if q := val("pingularity_series_queries_total"); scans != q {
		t.Errorf("pingularity_series_queries_total = %g but the scan-running outcomes sum to %g "+
			"(new %g, empty %g, expired %g, bypass %g) - a reader cannot account for the difference",
			q, scans, val("pingularity_series_cache_new_total"), val("pingularity_series_cache_empty_total"),
			val("pingularity_series_cache_expired_total"), val("pingularity_series_bypass_total"))
	}
}

// THE WHOLE WIRE, END TO END. The counters above are injected by hand; this one
// drives the real recorder - two Store.Series calls, one wide-bucket window and
// one sub-minute window that skips the cache - and reads the endpoint. It is the
// seam that fails silently: a recorder naming a key the exposition does not
// carry costs nothing at compile time and shows up as a permanently absent
// series.
func TestMetricsSeriesRecordersReachTheEndpoint(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	ctx, now := context.Background(), time.Now()
	if _, err := s.store.Series(ctx, now.Add(-24*time.Hour), time.Time{}, 300, nil); err != nil {
		t.Fatalf("wide-bucket series: %v", err)
	}
	if _, err := s.store.Series(ctx, now.Add(-5*time.Minute), time.Time{}, 1, nil); err != nil {
		t.Fatalf("sub-minute series: %v", err)
	}

	body := scrape(t, s)
	for _, m := range []struct{ name, why string }{
		{"pingularity_series_cache_new_total", "the wide window had no cache entry"},
		{"pingularity_series_queries_total", "the wide window ran an aggregate"},
		{"pingularity_series_bypass_total", "the sub-minute window never consulted the cache"},
		{"pingularity_series_query_seconds_count", "each executed aggregate is timed"},
	} {
		if v := sampleValue(body, m.name); v < 1 {
			t.Errorf("%s = %g, want >= 1 (%s) - recorded, but the endpoint does not carry it", m.name, v, m.why)
		}
	}
}

// A SLOW AGGREGATE MUST LAND IN A BUCKET, NOT ONLY IN +Inf. The chart aggregate
// re-scans the raw samples table (the Series doc comment, internal/store) and
// on slow hardware runs well past the 5s where LatencyBucketsSeconds stops, so
// with the latency bounds every interesting duration is indistinguishable: same
// +Inf bucket, no p95, no way to see a window degrade.
func TestMetricsSeriesQueryHistogramResolvesSlowAggregates(t *testing.T) {
	stats.ResetForTest()
	stats.Observe("series.query.seconds", 0.4)
	stats.Observe("series.query.seconds", 12)

	body := scrape(t, newMetricsServer(t))
	block := famBlock(body, "pingularity_series_query_seconds")
	if block == "" {
		t.Fatal("series query histogram recorded but absent from /metrics - the exposition table is the only reader of snap.Histos")
	}
	for _, want := range []string{
		"# TYPE pingularity_series_query_seconds histogram",
		`pingularity_series_query_seconds_bucket{le="5"} 1`,    // only the 0.4s query is that fast
		`pingularity_series_query_seconds_bucket{le="10"} 1`,   // 12s is still above this bound
		`pingularity_series_query_seconds_bucket{le="20"} 2`,   // and resolves HERE rather than falling off the end
		`pingularity_series_query_seconds_bucket{le="60"} 2`,   // the widest bound, per AggregationBucketsSeconds
		`pingularity_series_query_seconds_bucket{le="+Inf"} 2`, // nothing is beyond a minute here
		"pingularity_series_query_seconds_sum 12.4",
		"pingularity_series_query_seconds_count 2",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("missing %q\n--- block ---\n%s", want, block)
		}
	}
}

// The exposition of the two existing latency histograms, byte for byte. Adding a
// third histogram with its own wider bounds must not move a single byte of
// probe/DNS output: their bounds are a recorded Prometheus contract, and a
// changed bucket set silently invalidates every stored series and every
// histogram_quantile over it.
func TestMetricsLatencyHistogramExpositionUnchanged(t *testing.T) {
	stats.ResetForTest()
	stats.Observe("probe.latency", 0.012)
	stats.Observe("probe.latency", 7) // past the widest latency bound: +Inf only
	stats.Observe("dns.latency", 0.03)

	body := scrape(t, newMetricsServer(t))
	const wantProbe = `# HELP pingularity_probe_latency_seconds Anchor round-trip latency distribution (seconds), per successful target probe.
# TYPE pingularity_probe_latency_seconds histogram
pingularity_probe_latency_seconds_bucket{le="0.001"} 0
pingularity_probe_latency_seconds_bucket{le="0.002"} 0
pingularity_probe_latency_seconds_bucket{le="0.005"} 0
pingularity_probe_latency_seconds_bucket{le="0.01"} 0
pingularity_probe_latency_seconds_bucket{le="0.025"} 1
pingularity_probe_latency_seconds_bucket{le="0.05"} 1
pingularity_probe_latency_seconds_bucket{le="0.1"} 1
pingularity_probe_latency_seconds_bucket{le="0.25"} 1
pingularity_probe_latency_seconds_bucket{le="0.5"} 1
pingularity_probe_latency_seconds_bucket{le="1"} 1
pingularity_probe_latency_seconds_bucket{le="2.5"} 1
pingularity_probe_latency_seconds_bucket{le="5"} 1
pingularity_probe_latency_seconds_bucket{le="+Inf"} 2
pingularity_probe_latency_seconds_sum 7.012
pingularity_probe_latency_seconds_count 2
`
	const wantDNS = `# HELP pingularity_dns_latency_seconds DNS resolve-time distribution (seconds), per successful DNS probe.
# TYPE pingularity_dns_latency_seconds histogram
pingularity_dns_latency_seconds_bucket{le="0.001"} 0
pingularity_dns_latency_seconds_bucket{le="0.002"} 0
pingularity_dns_latency_seconds_bucket{le="0.005"} 0
pingularity_dns_latency_seconds_bucket{le="0.01"} 0
pingularity_dns_latency_seconds_bucket{le="0.025"} 0
pingularity_dns_latency_seconds_bucket{le="0.05"} 1
pingularity_dns_latency_seconds_bucket{le="0.1"} 1
pingularity_dns_latency_seconds_bucket{le="0.25"} 1
pingularity_dns_latency_seconds_bucket{le="0.5"} 1
pingularity_dns_latency_seconds_bucket{le="1"} 1
pingularity_dns_latency_seconds_bucket{le="2.5"} 1
pingularity_dns_latency_seconds_bucket{le="5"} 1
pingularity_dns_latency_seconds_bucket{le="+Inf"} 1
pingularity_dns_latency_seconds_sum 0.03
pingularity_dns_latency_seconds_count 1
`
	if got := famBlock(body, "pingularity_probe_latency_seconds"); got != wantProbe {
		t.Errorf("probe latency exposition changed\n--- got ---\n%s\n--- want ---\n%s", got, wantProbe)
	}
	if got := famBlock(body, "pingularity_dns_latency_seconds"); got != wantDNS {
		t.Errorf("dns latency exposition changed\n--- got ---\n%s\n--- want ---\n%s", got, wantDNS)
	}
}
