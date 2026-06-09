package web

import (
	"context"
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
		`pingularity_uptime_ratio{window="7d"}`,
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
	status := func() LiveStatus {
		return LiveStatus{Online: false, Since: time.Now().Add(-90 * time.Second)}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	body := scrape(t, New(st, status, nil, set, nil, "t", log))
	if !strings.Contains(body, "pingularity_up 0") {
		t.Error("pingularity_up should be 0 while offline")
	}
	if !strings.Contains(body, "pingularity_current_outage_seconds ") {
		t.Error("current-outage gauge missing while offline")
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
