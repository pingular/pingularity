package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func newMetricsServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	status := func() LiveStatus {
		return LiveStatus{
			Online: true, Paused: true, Since: time.Unix(1_700_000_000, 0),
			Families: []FamilyStatus{{Family: "ipv4", Online: true, LatencyMS: 5}},
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, status, nil, set, nil, "v9.9.9", log)
}

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.Host = "127.0.0.1:9000" // pass the DNS-rebinding guard
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics: code=%d body=%q", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// /metrics exposes the always-on operational registry plus the
// build/runtime/7d-uptime/data series.
func TestMetricsExposesRegistry(t *testing.T) {
	stats.ResetForTest()
	stats.Inc("monitor.blips")
	stats.Add("speed.run.scheduled", 3)
	stats.AddF("notify.discord.lat_ms_sum", 12.5)
	stats.Set("monitor.blip_streak_max", 5)

	body := scrape(t, newMetricsServer(t))
	must := []string{
		`pingularity_build_info{version="v9.9.9",goversion="`, // goversion is runtime.Version(); match the stable prefix
		"pingularity_runtime_seconds ",
		"pingularity_monitoring_paused 1",
		"pingularity_probing_active 0", // the metrics-server stub isn't probing
		"pingularity_state_since_timestamp_seconds 1700000000",
		"pingularity_goroutines ",
		"pingularity_memory_heap_bytes ",
		// This stub isn't probing and its store is empty, so it observed nothing:
		// uptime_ratio is correctly OMITTED (coverage 0) and only the coverage series
		// renders. A store with observed data is exercised in TestMetricsUptimeObserved.
		`pingularity_uptime_coverage_ratio{window="7d"} 0`,
		"pingularity_speed_data_used_bytes ",
		"# TYPE pingularity_stat_total counter",
		`pingularity_stat_total{stat="monitor.blips"} 1`,
		`pingularity_stat_total{stat="speed.run.scheduled"} 3`,
		`pingularity_stat_total{stat="notify.discord.lat_ms_sum"} 12.5`,
		`pingularity_stat{stat="monitor.blip_streak_max"} 5`,
	}
	for _, m := range must {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics missing %q\n--- body ---\n%s", m, body)
		}
	}
}

// Product/usage counters (which settings change, dashboard usage) must NOT
// appear in the operator's Prometheus endpoint; operational + security
// counters must.
func TestMetricsExcludesProductAnalytics(t *testing.T) {
	stats.ResetForTest()
	// Product analytics - must be filtered out.
	stats.Inc("settings.changed.webhook_url")
	stats.Inc("web.ui_loads")
	stats.Inc("web.exports")
	stats.Inc("web.manual_speedtests")
	// Operational / security - must be kept.
	stats.Inc("web.login_fail")
	stats.Inc("web.limiter_trips")
	stats.Inc("db.busy")

	body := scrape(t, newMetricsServer(t))
	for _, bad := range []string{"settings.changed", "web.ui_loads", "web.exports", "web.manual_speedtests"} {
		if strings.Contains(body, bad) {
			t.Errorf("product-analytics counter %q leaked into /metrics", bad)
		}
	}
	for _, good := range []string{
		`pingularity_stat_total{stat="web.login_fail"} 1`,
		`pingularity_stat_total{stat="web.limiter_trips"} 1`,
		`pingularity_stat_total{stat="db.busy"} 1`,
	} {
		if !strings.Contains(body, good) {
			t.Errorf("operational counter missing from /metrics: %q", good)
		}
	}
}

func boolp(b bool) *bool { return &b }

// The connectivity + speed gauges: DNS (gated on the probe), per-family latency,
// per-target latency only for successful probes, the headline latency, and the
// speed engine/health gauges.
func TestMetricsConnectivityAndSpeedGauges(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now, Target: "cloudflare", Family: "ipv4", Success: true, LatencyMS: 20},
		{TS: now, Target: "quad9", Family: "ipv4", Success: false, LatencyMS: 0},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: 1_700_000_123, DownMbps: 500, UpMbps: 400, PingMS: 5, Engine: "iperf3", Healthy: boolp(true),
	}); err != nil {
		t.Fatalf("speed: %v", err)
	}
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1, DNSProbe: true,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	status := func() LiveStatus {
		return LiveStatus{
			Online: true, Since: time.Unix(1_700_000_000, 0),
			Families: []FamilyStatus{{Family: "ipv4", Online: true, LatencyMS: 20}, {Family: "ipv6", Online: false}},
			DNSms:    40, DNSok: true, DNSactive: true,
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, status, nil, set, nil, "t", log)
	body := scrape(t, srv)

	for _, m := range []string{
		"pingularity_dns_up 1",
		"pingularity_dns_resolve_seconds 0.04",
		`pingularity_family_latency_seconds{family="ipv4"} 0.02`,
		`pingularity_target_up{target="cloudflare"} 1`,
		`pingularity_target_up{target="quad9"} 0`,
		`pingularity_target_latency_seconds{target="cloudflare"} 0.02`,
		"pingularity_latency_seconds 0.02",
		`pingularity_speed_info{engine="iperf3"} 1`,
		"pingularity_speed_healthy 1",
	} {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics missing %q\n--- body ---\n%s", m, body)
		}
	}
	// A failed probe / offline family must have NO latency line (not a fake 0).
	for _, bad := range []string{
		`pingularity_target_latency_seconds{target="quad9"}`,
		`pingularity_family_latency_seconds{family="ipv6"}`,
	} {
		if strings.Contains(body, bad) {
			t.Errorf("/metrics should not emit a latency line for a down target/family: %q", bad)
		}
	}

	// A DNS probe that isn't running (monitoring paused / schedule closed / probe
	// off) must be ABSENT, not a fake "resolver down" 0 - the gate is the live
	// DNSactive flag, not the stored setting (which is still on here).
	srv.status = func() LiveStatus {
		return LiveStatus{Online: true, Paused: true, Since: time.Unix(1_700_000_000, 0)}
	}
	if paused := scrape(t, srv); strings.Contains(paused, "pingularity_dns_up") {
		t.Error("pingularity_dns_up must be absent while the DNS probe isn't running")
	}
}

// While offline, current-outage duration is exposed and pingularity_up is 0.
func TestMetricsCurrentOutage(t *testing.T) {
	stats.ResetForTest()
	set, err := settings.New(context.Background(), newMetricsServer(t).store, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Offline AND actively probing: the outage is real and in progress, so the
	// gauge is present.
	probing := func() LiveStatus {
		return LiveStatus{Online: false, Probing: true, Since: time.Now().Add(-90 * time.Second)}
	}
	body := scrape(t, New(st, probing, nil, set, nil, "t", log))
	if !strings.Contains(body, "pingularity_up 0") {
		t.Error("pingularity_up should be 0 while offline")
	}
	if !strings.Contains(body, "pingularity_current_outage_seconds ") {
		t.Error("current-outage gauge missing while offline and probing")
	}

	// Offline but probing PAUSED: the stored outage will exclude this paused span,
	// so the live gauge must be absent (not a value that races past history and can
	// misfire the README alert). state_since still shows when the outage began.
	paused := func() LiveStatus {
		return LiveStatus{Online: false, Probing: false, Since: time.Now().Add(-90 * time.Second)}
	}
	body = scrape(t, New(st, paused, nil, set, nil, "t", log))
	if strings.Contains(body, "pingularity_current_outage_seconds ") {
		t.Error("current-outage gauge must be absent while probing is paused")
	}
	if !strings.Contains(body, "pingularity_state_since_timestamp_seconds ") {
		t.Error("state_since should still be present while paused")
	}
}

// File-backed concerns: the DB-size gauge (main file + sidecars) and the
// speed-staleness timestamp that anchors every speed gauge.
func TestMetricsDBSizeAndSpeedTimestamp(t *testing.T) {
	stats.ResetForTest()
	dir := t.TempDir()
	path := dir + "/db.sqlite"
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.InsertSpeed(context.Background(), store.SpeedSample{
		TS: 1_700_000_123, DownMbps: 500, UpMbps: 400, PingMS: 5,
	}); err != nil {
		t.Fatalf("insert speed: %v", err)
	}
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(st, func() LiveStatus { return LiveStatus{Online: true} }, nil, set, nil, "t", log)
	s.DBPath = path

	body := scrape(t, s)
	if !strings.Contains(body, "pingularity_speed_last_run_timestamp_seconds 1700000123") {
		t.Error("speed last-run timestamp missing or wrong")
	}
	if !strings.Contains(body, "pingularity_db_bytes ") {
		t.Error("db size gauge missing for a file-backed store")
	}
}

// Each metric family's samples must form one contiguous block (all family_up
// lines together, then all family_latency lines, ...): the exposition format
// requires it, and strict parsers reject interleaving. Checked generically
// over every sample line in the scrape.
func TestMetricsFamiliesAreContiguous(t *testing.T) {
	stats.ResetForTest()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	// Two online families so the per-family gauges each emit several lines.
	status := func() LiveStatus {
		return LiveStatus{
			Online: true, Since: time.Unix(1_700_000_000, 0),
			Families: []FamilyStatus{
				{Family: "ipv4", Online: true, LatencyMS: 5, Since: time.Unix(1_700_000_100, 0)},
				{Family: "ipv6", Online: true, LatencyMS: 7, Since: time.Unix(1_700_000_200, 0)},
			},
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(st, status, nil, set, nil, "v9.9.9", log)
	body := scrape(t, s)

	seen := map[string]bool{} // family name -> its block has ended
	last := ""
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
		}
		if name != last {
			if seen[name] {
				t.Errorf("metric family %q re-opens after other families (interleaved exposition)", name)
			}
			if last != "" {
				seen[last] = true
			}
			last = name
		}
	}

	// The new per-family state-since gauge is present for both families.
	for _, m := range []string{
		`pingularity_family_state_since_timestamp_seconds{family="ipv4"} 1700000100`,
		`pingularity_family_state_since_timestamp_seconds{family="ipv6"} 1700000200`,
	} {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics missing %q", m)
		}
	}
}

// The windowed data-usage and last-run byte gauges expose what the dashboard
// already computes; healthy's HELP documents its absence rule.
func TestMetricsSpeedByteGauges(t *testing.T) {
	stats.ResetForTest()
	s := newMetricsServer(t)
	dn, up := int64(1_000_000), int64(500_000)
	healthy := true
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 100, UpMbps: 50, PingMS: 10,
		DownBytes: &dn, UpBytes: &up, Healthy: &healthy,
	}); err != nil {
		t.Fatalf("seed speed: %v", err)
	}
	body := scrape(t, s)
	for _, m := range []string{
		`pingularity_speed_last_run_bytes{direction="down"} 1000000`,
		`pingularity_speed_last_run_bytes{direction="up"} 500000`,
		`pingularity_speed_data_used_window_bytes{window="24h"} 1500000`,
		`pingularity_speed_data_used_window_bytes{window="30d"} 1500000`,
		"absent when no thresholds are configured",
	} {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics missing %q\n--- body ---\n%s", m, body)
		}
	}
}

// #7 unmeasured-is-absent: the per-run speed_download_mbps/upload_mbps/ping_ms
// gauges must appear ONLY when that quantity was measured (download/upload gated
// on their *_bytes evidence, ping on a non-zero reading). A direction the engine
// skipped must NOT emit a 0.0 gauge, which would fire a permanent false
// "below threshold" alert.
func TestMetricsSpeedMbpsGatedOnMeasurement(t *testing.T) {
	stats.ResetForTest()

	// A download-only run (speed_direction="down"): download measured, upload
	// skipped (no upload_bytes), latency probed.
	s := newMetricsServer(t)
	dn := int64(1_000_000)
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 300, UpMbps: 0, PingMS: 8, DownBytes: &dn,
	}); err != nil {
		t.Fatalf("seed down-only speed: %v", err)
	}
	body := scrape(t, s)
	if !strings.Contains(body, "pingularity_speed_download_mbps 300") {
		t.Errorf("download_mbps should be present for a measured download\n%s", body)
	}
	if strings.Contains(body, "pingularity_speed_upload_mbps") {
		t.Errorf("upload_mbps must be ABSENT when upload was skipped (no upload_bytes)\n%s", body)
	}
	if !strings.Contains(body, "pingularity_speed_ping_ms 8") {
		t.Errorf("ping_ms should be present for a probed latency\n%s", body)
	}

	// A full run: both directions measured -> both mbps gauges present.
	s2 := newMetricsServer(t)
	up := int64(500_000)
	if err := s2.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 100, UpMbps: 50, PingMS: 10, DownBytes: &dn, UpBytes: &up,
	}); err != nil {
		t.Fatalf("seed full speed: %v", err)
	}
	full := scrape(t, s2)
	for _, m := range []string{"pingularity_speed_download_mbps 100", "pingularity_speed_upload_mbps 50", "pingularity_speed_ping_ms 10"} {
		if !strings.Contains(full, m) {
			t.Errorf("full run missing %q\n%s", m, full)
		}
	}
}

func i64p(v int64) *int64 { return &v }

// metricsSet builds a minimal settings controller for a scrape test.
func metricsSet(t *testing.T, st *store.Store) *settings.Controller {
	t.Helper()
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	return set
}

// A control byte or a backslash/quote in an imported target must not emit a Go
// %q-style escape (\t, \xNN) that fails the whole Prometheus scrape (F1a): control
// bytes are dropped, and \ and " get the two escapes the text format allows.
func TestMetricsLabelEscaping(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now, Target: "bad\tname", Family: "ipv4", Success: true, LatencyMS: 1},
		{TS: now, Target: `a"b\c`, Family: "ipv4", Success: true, LatencyMS: 1},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: now} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, metricsSet(t, st), nil, "t", log))

	if strings.Contains(body, `\t`) {
		t.Errorf("output contains an invalid \\t escape (would fail the scrape):\n%s", body)
	}
	if !strings.Contains(body, `pingularity_target_up{target="badname"}`) {
		t.Errorf("tab not dropped from target label; got:\n%s", body)
	}
	if !strings.Contains(body, `pingularity_target_up{target="a\"b\\c"}`) {
		t.Errorf("quote/backslash not escaped per Prometheus rules; got:\n%s", body)
	}
}

// Target names ride in from the samples table, which an import can back-fill
// with arbitrarily many, arbitrarily long crafted names. The metrics endpoint
// must cap the distinct per-target series and truncate long labels so a hostile
// backup can't explode the operator's Prometheus (audit #8).
func TestMetricsCapsTargetCardinality(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	var sms []store.Sample
	for i := 0; i < metricsMaxTargets*3; i++ { // far past the cap
		sms = append(sms, store.Sample{
			TS: now, Target: fmt.Sprintf("crafted-%04d", i), Family: "ipv4", Success: true, LatencyMS: 1,
		})
	}
	longName := strings.Repeat("A", metricsMaxLabelLen*4)
	sms = append(sms, store.Sample{TS: now, Target: longName, Family: "ipv4", Success: true, LatencyMS: 1})
	if err := st.InsertSamples(ctx, sms); err != nil {
		t.Fatalf("samples: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: now} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, metricsSet(t, st), nil, "t", log))

	if n := strings.Count(body, "pingularity_target_up{target="); n > metricsMaxTargets {
		t.Errorf("emitted %d target_up series, exceeds cap %d", n, metricsMaxTargets)
	}
	if strings.Contains(body, longName) {
		t.Errorf("over-long target label was not truncated (full %d-byte name present)", len(longName))
	}
}

// avg_run_bytes must be emitted only for a direction that has samples: a
// download-only history has a zero upload average, which must be ABSENT, not a
// fake measured 0 (F9b).
func TestMetricsSpeedAvgOmitsEmptyDirection(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 500, DownBytes: i64p(600_000_000), // download-only: no UpBytes
	}); err != nil {
		t.Fatalf("speed: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: time.Now()} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, metricsSet(t, st), nil, "t", log))

	if !strings.Contains(body, `pingularity_speed_avg_run_bytes{direction="down"}`) {
		t.Error("download average missing")
	}
	if strings.Contains(body, `pingularity_speed_avg_run_bytes{direction="up"}`) {
		t.Errorf("upload average must be absent with no upload samples (no fake 0); got:\n%s", body)
	}
}

// A future-dated row for the same target/speed must not win the "latest" read and
// freeze current metrics on a not-yet-real value (F6).
func TestMetricsExcludesFutureRows(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	future := now.Add(time.Hour)
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now, Target: "gw", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("current sample: %v", err)
	}
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: future, Target: "gw", Family: "ipv4", Success: false, LatencyMS: 0}, // future failure for the SAME target
	}); err != nil {
		t.Fatalf("future sample: %v", err)
	}
	if err := st.InsertSpeed(ctx, store.SpeedSample{TS: now.Unix(), DownMbps: 500, DownBytes: i64p(6e8)}); err != nil {
		t.Fatalf("current speed: %v", err)
	}
	if err := st.InsertSpeed(ctx, store.SpeedSample{TS: future.Unix(), DownMbps: 999, DownBytes: i64p(9e8)}); err != nil {
		t.Fatalf("future speed: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: now} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, metricsSet(t, st), nil, "t", log))

	if !strings.Contains(body, `pingularity_target_up{target="gw"} 1`) {
		t.Errorf("current sample should win (up=1); the future failure must not freeze it. Got:\n%s", body)
	}
	if !strings.Contains(body, "pingularity_speed_download_mbps 500") {
		t.Errorf("current speed (500) should win over the future row (999). Got:\n%s", body)
	}
}

// /metrics is a read: GET and HEAD succeed; other verbs get 405 + Allow.
func TestMetricsMethodGuard(t *testing.T) {
	stats.ResetForTest()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: time.Now()} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, status, nil, metricsSet(t, st), nil, "t", log)

	for _, m := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(m, "/metrics", nil)
		r.Host = "127.0.0.1:9000"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("%s /metrics: code=%d, want 200", m, w.Code)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "OPTIONS"} {
		r := httptest.NewRequest(m, "/metrics", nil)
		r.Host = "127.0.0.1:9000"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /metrics: code=%d, want 405", m, w.Code)
		}
		if got := w.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s /metrics Allow=%q, want \"GET, HEAD\"", m, got)
		}
	}
}

// With observed data (a sample anchoring monitoring), uptime_ratio is present with
// non-zero coverage, and the coverage + since-timestamp series render (F2/F3).
func TestMetricsUptimeObserved(t *testing.T) {
	stats.ResetForTest()
	ctx := context.Background()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-4 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}, // monitoringSince = now-4h
		{TS: now, Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("samples: %v", err)
	}
	// 2h paused inside the observed 4h span -> the 24h window (clamped to the 4h of
	// monitoring) observed 2h of 4h: coverage 0.5.
	if err := st.InsertPause(ctx, now.Add(-3*time.Hour), int64((2 * time.Hour).Seconds())); err != nil {
		t.Fatalf("pause: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: now} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, metricsSet(t, st), nil, "t", log))

	if !strings.Contains(body, `pingularity_uptime_ratio{window="24h"}`) {
		t.Errorf("uptime_ratio missing for an observed window:\n%s", body)
	}
	if !strings.Contains(body, `pingularity_uptime_coverage_ratio{window="24h"} 0.5`) {
		t.Errorf("24h coverage should be 0.5 (2h of 4h observed):\n%s", body)
	}
	if !strings.Contains(body, "pingularity_uptime_since_timestamp_seconds ") {
		t.Error("uptime_since_timestamp missing")
	}
}

// The new well-named families, worker health, collector health, and histograms all
// render (complementing the promtool CI parse check).
func TestMetricsObservabilityFamilies(t *testing.T) {
	stats.ResetForTest()
	stats.Inc("probe.fail.timeout")
	stats.Inc("speed.run.scheduled")
	stats.Set("worker.pruner.up", 1)
	stats.Inc("worker.pruner.restarts")
	stats.Observe("probe.latency", 0.012)
	stats.Observe("probe.latency", 0.4)

	body := scrape(t, newMetricsServer(t))
	for _, m := range []string{
		"pingularity_process_start_time_seconds ",
		"pingularity_gc_cycles_total ",
		`pingularity_probe_failures_total{reason="timeout"} 1`,
		`pingularity_speed_runs_total{trigger="scheduled"} 1`,
		`pingularity_worker_up{worker="pruner"} 1`,
		`pingularity_worker_restarts_total{worker="pruner"} 1`,
		`pingularity_probe_latency_seconds_bucket{le="0.025"} 1`, // 0.012 <= 0.025, 0.4 is not
		`pingularity_probe_latency_seconds_bucket{le="+Inf"} 2`,
		"pingularity_probe_latency_seconds_count 2",
		"pingularity_metrics_data_valid ",
		`pingularity_metrics_collector_success{collector="targets"}`,
	} {
		if !strings.Contains(body, m) {
			t.Errorf("/metrics missing %q", m)
		}
	}
}

// /healthz is always 200 (liveness); /readyz is 200 once the store answers and the
// aggregate cache is warm.
func TestHealthEndpoints(t *testing.T) {
	stats.ResetForTest()
	srv := newMetricsServer(t)
	hit := func(path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = "example.com" // a public Host: proves the endpoints bypass the DNS-rebinding guard
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if c := hit("/healthz"); c != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", c)
	}
	if c := hit("/readyz"); c != http.StatusOK { // newMetricsServer store is open; readyz warms the cache
		t.Errorf("/readyz = %d, want 200", c)
	}
}

// A configured metrics token authenticates a scrape (Bearer or Basic password) when
// Require login is on, without the admin credential.
func TestMetricsTokenAuth(t *testing.T) {
	stats.ResetForTest()
	st, _ := store.Open(":memory:")
	t.Cleanup(func() { st.Close() })
	// Require login on: a bcrypt hash makes AuthActive() true.
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
		AuthEnabled: true, AuthUser: "admin", AuthHash: "$2a$10$abcdefghijklmnopqrstuv",
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	status := func() LiveStatus { return LiveStatus{Online: true, Probing: true, Since: time.Now()} }
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(st, status, nil, set, nil, "t", log)
	srv.MetricsToken = "s3cr3t-scrape-token"

	do := func(setAuth func(*http.Request)) int {
		r := httptest.NewRequest("GET", "/metrics", nil)
		r.Host = "127.0.0.1:9000"
		if setAuth != nil {
			setAuth(r)
		}
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		return w.Code
	}
	if c := do(nil); c != http.StatusUnauthorized {
		t.Errorf("no credentials: got %d, want 401", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cr3t-scrape-token") }); c != http.StatusOK {
		t.Errorf("bearer token: got %d, want 200", c)
	}
	if c := do(func(r *http.Request) { r.SetBasicAuth("prometheus", "s3cr3t-scrape-token") }); c != http.StatusOK {
		t.Errorf("basic-password token: got %d, want 200", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("Authorization", "Bearer wrong") }); c != http.StatusUnauthorized {
		t.Errorf("wrong token: got %d, want 401", c)
	}
}
