package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// stampUptime installs a window set into the aggregate cache and marks it fresh,
// so a scrape renders exactly these Observations instead of scanning the store.
// It is the only way to sit a test ON the publish/omit boundary: the store works
// in whole seconds, and the boundary is the smallest representable observation.
func stampUptime(s *Server, u store.Uptime) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	s.uptime = u
	s.aggAt = time.Now()
}

// The publish/omit boundary of pingularity_uptime_ratio, driven through the real
// handler.
//
// The rule is DEFINEDNESS, not quality: the smallest slice of observation there
// is publishes a ratio, and only literally nothing withholds one. The coverage
// series renders either way, at every value, so a consumer always learns which
// case they are in and can impose their own floor (`and on(window)
// pingularity_uptime_coverage_ratio > X`). Nothing lets them recover a value the
// exporter refused to emit, which is why this gate must never become a threshold:
// a monitor watched 8h/day sits at coverage 0.3333 on every window forever and
// would lose all six ratio series to any non-zero cutoff.
func TestMetricsUptimeRatioBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed time.Duration
		publish  bool
	}{
		{"the smallest possible observation publishes", 1, true},
		{"no observation at all does not", 0, false},
		{"a negative (corrupt) observation does not", -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stats.ResetForTest()
			s := newMetricsServer(t)
			stampUptime(s, store.Uptime{H24: store.Observation{Window: 24 * time.Hour, Observed: tc.observed}})
			body := scrape(t, s)

			if got := strings.Contains(body, `pingularity_uptime_ratio{window="24h"}`); got != tc.publish {
				t.Errorf("uptime_ratio{24h} present = %v, want %v (observed = %v)", got, tc.publish, tc.observed)
			}
			// The coverage series is unconditional at every value - it is how a
			// consumer tells "thin" from "absent", so it can never be the thing
			// that goes missing.
			if !strings.Contains(body, `pingularity_uptime_coverage_ratio{window="24h"}`) {
				t.Errorf("uptime_coverage_ratio{24h} must render at every coverage, including %v:\n%s", tc.observed, body)
			}
			// All six windows always get a coverage series, even the five left zero.
			for _, w := range []string{"6h", "24h", "7d", "30d", "1y", "all"} {
				if !strings.Contains(body, `pingularity_uptime_coverage_ratio{window="`+w+`"}`) {
					t.Errorf("coverage series missing for window %q:\n%s", w, body)
				}
			}
		})
	}
}

// statusServer returns a Server whose /api/status handler is wired (newTestServer
// passes a nil status func, which degrades to 503).
func statusServer(t *testing.T, st *store.Store) *Server {
	t.Helper()
	status := func() LiveStatus {
		return LiveStatus{Online: true, Probing: true, Since: time.Now().Add(-time.Hour)}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(st, status, nil, metricsSet(t, st), nil, "test", log)
}

// getStatus runs one /api/status request through the full handler chain.
func getStatus(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/status"+query, nil)
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/status%s: code=%d body=%q", query, w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	return out
}

// /api/status must not present a confident percentage for a window that observed
// nothing.
//
// The fixture is the documented speedtest-only deployment (`-latency=false`) and
// its sibling `-ipv4=off -ipv6=off`: probing never runs, so the monitor records
// the whole span as unobserved and coverage is PERMANENTLY 0. The dashboard used
// to render "100.000%" for it while /metrics, in the same second, published no
// uptime series at all - two surfaces of one process telling opposite stories.
// Both the preset windows and the ?upMins= custom pill are checked; the custom one
// is a fourth uptime figure that the "two of four consumers" framing missed.
func TestStatusShipsCoverageForEveryUptimeFigure(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	// A speedtest run anchors the install two days back without ever probing...
	if err := st.InsertSpeed(ctx, store.SpeedSample{
		TS: now.Add(-48 * time.Hour).Unix(), DownMbps: 100, UpMbps: 10, PingMS: 20, Server: "S", ServerID: "1",
	}); err != nil {
		t.Fatalf("speed: %v", err)
	}
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-48 * time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
	}); err != nil {
		t.Fatalf("sample: %v", err)
	}
	// ...and nothing was watched since.
	if _, err := st.InsertPause(ctx, now.Add(-48*time.Hour), int64(48*time.Hour/time.Second)+60); err != nil {
		t.Fatalf("pause: %v", err)
	}

	out := getStatus(t, statusServer(t, st), "?upMins=1440")

	cov, ok := out["uptime_coverage"].(map[string]any)
	if !ok {
		t.Fatalf("/api/status must carry uptime_coverage beside uptime; got keys %v", keysOf(out))
	}
	ratios, ok := out["uptime"].(map[string]any)
	if !ok {
		t.Fatalf("/api/status uptime = %T, want an object per window", out["uptime"])
	}
	for _, w := range []string{"6h", "24h", "7d", "30d", "1y", "all"} {
		c, ok := cov[w].(float64)
		if !ok {
			t.Errorf("uptime_coverage is missing window %q: every published ratio needs the coverage that qualifies it", w)
			continue
		}
		if c != 0 {
			t.Errorf("coverage[%s] = %v, want 0: this install has never observed anything", w, c)
		}
		if _, ok := ratios[w]; !ok {
			t.Errorf("uptime is missing window %q", w)
		}
	}
	// The custom-window pill ships its own coverage. Without it the pill renders
	// 100.000% for a window nobody watched, exactly like the preset ones did.
	if _, ok := out["uptime_custom"]; !ok {
		t.Fatal("?upMins= must still return uptime_custom")
	}
	c, ok := out["uptime_custom_coverage"].(float64)
	if !ok {
		t.Fatalf("uptime_custom_coverage missing; got keys %v", keysOf(out))
	}
	if c != 0 {
		t.Errorf("uptime_custom_coverage = %v, want 0", c)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
