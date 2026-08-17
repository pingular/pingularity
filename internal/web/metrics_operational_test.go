package web

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"io"
	"log/slog"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/update"
)

// THE ALLOWLIST MUST NOT EAT SIGNALS RECORDED FOR /metrics. Three recorders
// increment counters whose stated purpose is operator visibility - the
// /metrics target-cap disclosure, the step-up security counter (sibling of the
// exported web.login_fail), and the import.* repair counters whose code
// comment says "so it is visible on /metrics" - and promStat filtered every
// one of them, with no other consumer of the registry in the process. The
// series.* chart-aggregate cache counters are the same shape of signal: they
// are recorded for this endpoint alone, so an unclassified prefix loses them
// silently - no log, no error, just a counter nobody can read.
func TestMetricsExposesOperationalWebAndImportCounters(t *testing.T) {
	stats.ResetForTest()
	stats.Inc("web.metrics_targets_capped")
	stats.Inc("web.stepup_fail")
	stats.Inc("import.event_duration_dropped")
	stats.Inc("import.pause_dropped")
	stats.Inc("series.cache.hit")
	stats.Inc("series.cache.expired")
	stats.Inc("series.cache.new")
	stats.Inc("series.cache.empty")
	stats.Inc("series.bypass")
	stats.Inc("series.query")
	body := scrape(t, newMetricsServer(t))
	for _, want := range []string{
		`pingularity_stat_total{stat="web.metrics_targets_capped"} 1`,
		`pingularity_stat_total{stat="web.stepup_fail"} 1`,
		`pingularity_stat_total{stat="import.event_duration_dropped"} 1`,
		`pingularity_stat_total{stat="import.pause_dropped"} 1`,
		`pingularity_stat_total{stat="series.cache.hit"} 1`,
		`pingularity_stat_total{stat="series.cache.expired"} 1`,
		`pingularity_stat_total{stat="series.cache.new"} 1`,
		`pingularity_stat_total{stat="series.cache.empty"} 1`,
		`pingularity_stat_total{stat="series.bypass"} 1`,
		`pingularity_stat_total{stat="series.query"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s - the recorder exists but promStat filters it into a black hole", want)
		}
	}
}

// A WORKER THAT FINISHED ITS JOB IS NOT DEAD. The documented alert is
// pingularity_worker_up == 0; a one-shot worker (settings-retry) that
// completes leaves the gauge at 0 forever, firing that alert on a healthy
// install. Completion must remove the series so 0 means only death.
func TestCompletedWorkerLeavesNoZeroUpGauge(t *testing.T) {
	stats.ResetForTest()
	// The terminal writes spawnLoop performs now: completion deletes the
	// gauge; death (give-up, shutdown) writes the alertable 0. Before the
	// fix, completion could only be expressed as the death write - the
	// defect this test originally proved red.
	stats.Set("worker.settings-retry.up", 1)
	stats.Delete("worker.settings-retry.up")
	stats.Set("worker.doomed.up", 1)
	stats.Set("worker.doomed.up", 0)
	body := scrape(t, newMetricsServer(t))
	if strings.Contains(body, `pingularity_worker_up{worker="settings-retry"}`) {
		t.Error(`a completed worker still reports an up series - the worker_up==0 alert would false-fire forever`)
	}
	if !strings.Contains(body, `pingularity_worker_up{worker="doomed"} 0`) {
		t.Error("a dead worker must keep its alertable 0 - deletion is for completion only")
	}
}

// COLLECTOR HEALTH MUST TRACK REFRESH REALITY. Once the aggregates cache has
// warmed a single time, aggValid stays true even if every later refresh
// fails, so collector_success{aggregates} and metrics_data_valid read green
// while the served numbers age without bound.
func TestMetricsAggregatesCollectorReportsFailedRefresh(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	_ = scrape(t, s) // warm the cache while the store works
	s.store.Close()  // every refresh from here on fails
	s.aggMu.Lock()
	s.aggAt = time.Now().Add(-time.Minute) // expire the cache so the next scrape must refresh
	s.aggMu.Unlock()
	body := scrape(t, s)
	if !strings.Contains(body, `pingularity_metrics_collector_success{collector="aggregates"} 0`) {
		t.Error("aggregates refresh failed but collector_success stays green")
	}
	if !strings.Contains(body, "pingularity_metrics_data_valid 0") {
		t.Error("every store read failed yet metrics_data_valid claims the scrape is whole")
	}
}

// EVERY STORE READ ON THE SCRAPE PATH IS ACCOUNTED. UptimeFloor is read
// directly in the exposition writer with its error discarded - no collector
// row, no log, no data_valid effect: the silent-200 case the collector
// machinery exists to eliminate.
func TestMetricsUptimeFloorIsCollectorAccounted(t *testing.T) {
	stats.ResetForTest()
	body := scrape(t, newMetricsServer(t))
	if !strings.Contains(body, `pingularity_metrics_collector_success{collector="uptime_floor"} 1`) {
		t.Error("the UptimeFloor store read has no collector accounting - its failure would be invisible")
	}
}

// ONE RAW TARGET, ONE SERIES - EVEN AFTER LABEL NORMALIZATION. Truncation to
// 96 bytes and control-byte stripping are many-to-one, and the emit loops
// apply them without dedup, so two imported names that collapse to the same
// label emit duplicate series (dropped by Prometheus with a warning).
func TestMetricsCollidingTargetLabelsDoNotDuplicate(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	long := strings.Repeat("a", 96)
	now := time.Now()
	if err := s.store.InsertSamples(context.Background(), []store.Sample{
		{TS: now, Target: long + "-one", Family: "ipv4", Success: true, LatencyMS: 5},
		{TS: now, Target: long + "-two", Family: "ipv4", Success: true, LatencyMS: 7},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	body := scrape(t, s)
	if n := strings.Count(body, `pingularity_target_up{target="`+long+`"}`); n > 1 {
		t.Errorf("target label %q emitted %d times - Prometheus drops duplicate series", long[:12]+"...", n)
	}
	if !strings.Contains(body, `pingularity_stat_total{stat="web.metrics_label_collisions"} 1`) {
		t.Error("a collision happened but no counter discloses that a target's series was dropped")
	}
}

// ONE SCRAPE, ONE REGISTRY SNAPSHOT. The three writer sections each taking
// their own stats.Lifetime() lets the same counter disagree with its own
// back-compat duplicate inside a single exposition. Pinned structurally: the
// scrape path takes exactly one snapshot.
func TestMetricsSingleRegistrySnapshotPerScrape(t *testing.T) {
	src, err := os.ReadFile("web.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "stats.Lifetime("); n != 1 {
		t.Errorf("web.go calls stats.Lifetime() %d times; one scrape must observe one snapshot", n)
	}
}

// LABELS FROM DB TEXT GET THE SAME BOUND AS TARGET NAMES. The speed engine
// label is emitted from whatever the speed row holds; an import can make it
// arbitrarily long, and the truncation defense built for targets skips it.
func TestMetricsEngineLabelBounded(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	huge := strings.Repeat("x", 300)
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 1, UpMbps: 1, PingMS: 1, Engine: huge,
	}); err != nil {
		t.Fatalf("speed: %v", err)
	}
	body := scrape(t, s)
	if strings.Contains(body, `engine="`+huge+`"`) {
		t.Error("a 300-byte engine label reached the exposition unbounded")
	}
	if !strings.Contains(body, `pingularity_speed_info{engine="`+huge[:96]+`"`) {
		t.Error("expected the engine label truncated to the same 96-byte bound as target labels")
	}
}

// RESERVED SUFFIXES BELONG TO SUMMARY FAMILIES. _sum/_count as standalone
// TYPE-counter families is what promtool lints; the pair is a quantile-less
// summary and must be typed as one (sample names unchanged, so existing
// queries keep working).
func TestMetricsSpeedDurationIsSummaryTyped(t *testing.T) {
	stats.ResetForTest()
	stats.AddF("speed.duration_s_sum", 12.5)
	stats.Inc("speed.duration_n")
	body := scrape(t, newMetricsServer(t))
	if !strings.Contains(body, "# TYPE pingularity_speed_run_duration_seconds summary") {
		t.Error("speed run duration _sum/_count pair is not typed as a summary family")
	}
	if strings.Contains(body, "# TYPE pingularity_speed_run_duration_seconds_sum counter") {
		t.Error("_sum still emitted as a standalone counter family - reserved suffix on the wrong type")
	}
	if !strings.Contains(body, "pingularity_speed_run_duration_seconds_sum 12.5") ||
		!strings.Contains(body, "pingularity_speed_run_duration_seconds_count 1") {
		t.Error("summary samples missing - the retype must not change sample names or values")
	}
}

// GAP A: THE UPDATE STATE THE DASHBOARD BADGE USES IS INVISIBLE TO PROMETHEUS.
// "An update has been pending for a week" and "the feed has been unreachable
// for days" are both alertable only if the gauges exist.
func TestMetricsUpdateGauges(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	s.Update = update.New("0.1.0", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := scrape(t, s)
	if !strings.Contains(body, "pingularity_update_available 0") {
		t.Error("update check is enabled but pingularity_update_available is absent")
	}
	if strings.Contains(body, "pingularity_update_check_timestamp_seconds") {
		t.Error("no successful poll has happened - the freshness timestamp must be absent, not 0")
	}
}

// GAP C: NOTIFICATION LATENCY EXISTS ONLY AS RAW MS SUMS UNDER MAGIC KEYS.
// "Webhook deliveries got slow" needs a well-named per-destination summary in
// base units.
func TestMetricsNotifyLatencySummary(t *testing.T) {
	stats.ResetForTest()
	stats.AddF("notify.discord.lat_ms_sum", 250)
	stats.Inc("notify.discord.lat_n")
	body := scrape(t, newMetricsServer(t))
	if !strings.Contains(body, "# TYPE pingularity_notification_delivery_duration_seconds summary") {
		t.Error("no well-named notification latency family")
	}
	if !strings.Contains(body, `pingularity_notification_delivery_duration_seconds_sum{destination="discord"} 0.25`) {
		t.Error("latency sum not emitted in seconds per destination")
	}
	if !strings.Contains(body, `pingularity_notification_delivery_duration_seconds_count{destination="discord"} 1`) {
		t.Error("timed-delivery count not emitted per destination")
	}
}
