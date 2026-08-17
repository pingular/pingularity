package web

// The chart-aggregate performance matrix: what one open dashboard costs the box
// per MINUTE, driven through the real /api/series handler at the real dashboard
// poll cadence.
//
// It is per minute and not per poll on purpose. The dashboard change in this
// series alters how often a chart polls, so a per-poll figure would report the
// cadence fix as a regression (fewer, individually identical polls) instead of
// the saving it is. Rate x cost is the only comparison that survives a cadence
// change.
//
// SKIPPED unless PINGULARITY_PERF=1: the rate mode spends real wall-clock
// minutes waiting out poll intervals (that IS the measurement) and the cost mode
// seeds millions of rows. Neither belongs in `go test ./...`.
//
// Two modes:
//
//	PERF_MODE=rate  drive one scenario at one cadence for PERF_MINUTES of real
//	                time with PERF_VIEWERS independent dashboards, then report the
//	                six series.* contract counters per minute. Rates are set by
//	                cadence and cache TTL alone, so this mode is deliberately run
//	                against a SMALL database: its numbers do not depend on how
//	                long a scan takes, which is what lets the whole matrix run as
//	                parallel processes.
//	PERF_MODE=cost  seed a database the way the shipped defaults fill one and
//	                measure the wall time of ONE scan at each preset width with
//	                nothing else running. Rate x cost is aggregate wall-seconds
//	                per minute.
//
// The cadence is NOT hardcoded here. PERF_DELAYS_MS carries the delay sequence
// the page's own latPollMs produces, extracted from index.html by
// perfmatrix_cadence.mjs (in this directory), so this file cannot drift from the
// shipped dashboard.
//
// TO RUN THE MATRIX ON REAL HARDWARE (a Pi, not a laptop), from the repo root:
//
//	# 1. what the dashboard's own cadence is, per range, over a 20-minute horizon
//	node internal/web/perfmatrix_cadence.mjs internal/web/ui/index.html 20
//
//	# 2. one rate cell. Repeat per scenario (5m 1h 6h 1d 7d 30d) and per viewer
//	#    count, passing that scenario's delaysMs from step 1.
//	PINGULARITY_PERF=1 PERF_MODE=rate PERF_SCENARIO=1d PERF_MINUTES=20 \
//	  PERF_VIEWERS=1 PERF_STACK=dual PERF_DELAYS_MS=60000 \
//	  go test ./internal/web/ -run TestPerfSeriesRate -count=1 -v -timeout 90m
//
//	# 3. the cost of one scan, at the shipped defaults, on a quiet box.
//	#    Expect this to take minutes on a Pi: it writes a month of 5s rounds.
//	PINGULARITY_PERF=1 PERF_MODE=cost PERF_STACK=dual \
//	  go test ./internal/web/ -run TestPerfSeriesCost -count=1 -v -timeout 90m
//
//	# 4. what a hidden latency tile costs, before and after
//	node internal/web/perfmatrix_hidden.mjs internal/web/ui/index.html 4.1 20
//
// Aggregate wall-seconds per minute is step 2's query_per_min multiplied by step
// 3's query_seconds_median for the same window. Step 2 reports its own
// query_seconds_per_min too, but only step 3 runs on a quiet box.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// perfSkip skips unless the run was asked for explicitly.
func perfSkip(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("PINGULARITY_PERF") != "1" {
		t.Skip("set PINGULARITY_PERF=1 to run the performance matrix (minutes of wall clock)")
	}
	if os.Getenv("PERF_MODE") != mode {
		t.Skipf("PERF_MODE is not %q", mode)
	}
}

func perfEnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func perfEnvFloat(name string, def float64) float64 {
	if v := os.Getenv(name); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// perfTargets is the shipped default target set (config.DefaultTargets), by
// address family. A dual-stack box writes twice the sample rows per round and
// the aggregate groups by family, so the two stacks are different scans rather
// than one scaled by two.
func perfTargets(stack string) []store.Sample {
	v4 := []store.Sample{
		{Target: "cloudflare", Family: "ipv4"},
		{Target: "google", Family: "ipv4"},
		{Target: "quad9", Family: "ipv4"},
	}
	if stack != "dual" {
		return v4
	}
	return append(v4,
		store.Sample{Target: "cloudflare-v6", Family: "ipv6"},
		store.Sample{Target: "google-v6", Family: "ipv6"},
		store.Sample{Target: "quad9-v6", Family: "ipv6"})
}

// perfSeed fills a fresh file-backed store with one probe round every
// intervalSec over the given span, ending at end. File-backed because production
// reads a file: an in-memory database hides the page cache and the I/O a real
// scan does.
//
// Rounds go in as batches, not one InsertSamples call per round. InsertSamples
// is one transaction per call, so a per-round loop would be one commit per round
// and seeding a month of five-second rounds would outlast the measurement.
func perfSeed(t *testing.T, path, stack string, span time.Duration, end time.Time, intervalSec int) *store.Store {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	tgts := perfTargets(stack)
	// A deterministic LCG, not math/rand, so two runs scan byte-identical data
	// and the before/after comparison this exists for is not noise.
	seed := uint32(1)
	next := func() uint32 { seed = seed*1664525 + 1013904223; return seed }
	const batch = 30000
	buf := make([]store.Sample, 0, batch+len(tgts))
	var dnsTS []time.Time
	flush := func() {
		if len(buf) > 0 {
			if err := st.InsertSamples(ctx, buf); err != nil {
				t.Fatalf("insert samples: %v", err)
			}
			buf = buf[:0]
		}
		for _, ts := range dnsTS {
			if err := st.InsertDNS(ctx, ts, 1+float64(next()%4000)/100, true); err != nil {
				t.Fatalf("insert dns: %v", err)
			}
		}
		dnsTS = dnsTS[:0]
	}
	for u := end.Add(-span).Unix(); u < end.Unix(); u += int64(intervalSec) {
		ts := time.Unix(u, 0)
		// One round in ~200 fails outright, so the MIN over successes and the
		// per-family quorum each have a mixture to chew on.
		down := next()%200 == 0
		for _, tg := range tgts {
			sm := tg
			sm.TS = ts
			if !down {
				sm.LatencyMS = 5 + float64(next()%9500)/100
				sm.Success = true
			}
			buf = append(buf, sm)
		}
		dnsTS = append(dnsTS, ts)
		if len(buf) >= batch {
			flush()
		}
	}
	flush()
	return st
}

// perfServer wraps a seeded store in the real Server, so a request runs the real
// route, the real seriesBucket arithmetic and the real handler.
func perfServer(t *testing.T, st *store.Store) http.Handler {
	t.Helper()
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	return New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil))).Handler()
}

// perfGet runs one request through the full handler chain and returns its status.
func perfGet(h http.Handler, path string) int {
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// perfQuerySeconds is the total wall time spent inside seriesQuery so far, read
// off the contract histogram's Sum. Sum is independent of the bucket bounds, so
// it means the same thing on both trees.
func perfQuerySeconds() float64 { return stats.Lifetime().Histos["series.query.seconds"].Sum }

func perfCounters() map[string]int64 {
	snap := stats.Lifetime()
	out := map[string]int64{}
	for _, k := range []string{"series.cache.hit", "series.cache.expired", "series.cache.new", "series.cache.empty", "series.bypass", "series.query"} {
		out[k] = snap.Counters[k]
	}
	out["histo.count"] = int64(snap.Histos["series.query.seconds"].Count)
	return out
}

// perfPath is the query string latWindowQuery would send for this scenario. The
// five presets go over as ?mins=; a live absolute range goes over as ?from= with
// the end left open.
func perfPath(scenario string, from time.Time) string {
	switch scenario {
	case "5m":
		return "/api/series?mins=5"
	case "1h":
		return "/api/series?mins=60"
	case "6h":
		return "/api/series?mins=360"
	case "1d":
		return "/api/series?mins=1440"
	case "7d":
		return "/api/series?mins=10080"
	case "30d":
		return "/api/series?from=" + strconv.FormatInt(from.Unix(), 10)
	}
	return ""
}

func perfRound(f float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(f*p) / p
}

func perfEmit(t *testing.T, out map[string]any) {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fmt.Println("PERFJSON " + string(b))
}

// TestPerfSeriesRate drives one scenario at the dashboard's own cadence for a
// wall-clock stretch and reports the contract counters per minute.
func TestPerfSeriesRate(t *testing.T) {
	perfSkip(t, "rate")
	scenario := os.Getenv("PERF_SCENARIO")
	minutes := perfEnvFloat("PERF_MINUTES", 10)
	viewers := perfEnvInt("PERF_VIEWERS", 1)
	stack := os.Getenv("PERF_STACK")
	seedDays := perfEnvInt("PERF_SEED_DAYS", 31)
	seedInterval := perfEnvInt("PERF_SEED_INTERVAL_SEC", 60)

	var delays []time.Duration
	for _, f := range strings.Split(os.Getenv("PERF_DELAYS_MS"), ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatalf("PERF_DELAYS_MS: %v", err)
		}
		delays = append(delays, time.Duration(n)*time.Millisecond)
	}
	if len(delays) == 0 {
		t.Fatal("PERF_DELAYS_MS is empty: the cadence must come from index.html via cadence.mjs, never from this file")
	}

	start := time.Now()
	st := perfSeed(t, filepath.Join(t.TempDir(), "perf.db"), stack,
		time.Duration(seedDays)*24*time.Hour, start, seedInterval)
	defer st.Close() //nolint:errcheck // measurement fixture
	h := perfServer(t, st)
	path := perfPath(scenario, start.Add(-30*24*time.Hour))
	if path == "" {
		t.Fatalf("unknown PERF_SCENARIO %q", scenario)
	}

	// Seeding takes time; the counters must cover only the measured stretch.
	stats.ResetForTest()
	measureStart := time.Now()
	deadline := measureStart.Add(time.Duration(minutes * float64(time.Minute)))

	var reqs atomic.Int64
	var wg sync.WaitGroup
	for v := 0; v < viewers; v++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			// Independent dashboards are not opened at the same instant. Stagger
			// them across one poll interval: viewers in lockstep would be a best
			// case for the single-flight and a worst case for the cache.
			if v > 0 {
				time.Sleep(time.Duration(int64(delays[0]) * int64(v) / int64(viewers)))
			}
			for i := 0; ; i++ {
				if !time.Now().Before(deadline) {
					return
				}
				if code := perfGet(h, path); code != http.StatusOK {
					t.Errorf("viewer %d: /api/series returned %d", v, code)
					return
				}
				reqs.Add(1)
				// The shape of loopChart: poll, AWAIT it, then arm the next delay.
				d := delays[i%len(delays)]
				if time.Until(deadline) < d {
					return
				}
				time.Sleep(d)
			}
		}(v)
	}
	wg.Wait()
	elapsed := time.Since(measureStart).Minutes()

	c := perfCounters()
	per := func(n int64) float64 { return perfRound(float64(n)/elapsed, 3) }
	perfEmit(t, map[string]any{
		"mode":                  "rate",
		"scenario":              scenario,
		"viewers":               viewers,
		"stack":                 stack,
		"seed_days":             seedDays,
		"seed_interval_sec":     seedInterval,
		"delays_ms":             os.Getenv("PERF_DELAYS_MS"),
		"minutes":               perfRound(elapsed, 3),
		"requests":              reqs.Load(),
		"requests_per_min":      per(reqs.Load()),
		"query_per_min":         per(c["series.query"]),
		"bypass_per_min":        per(c["series.bypass"]),
		"cache_hit_per_min":     per(c["series.cache.hit"]),
		"cache_expired_per_min": per(c["series.cache.expired"]),
		"cache_new_per_min":     per(c["series.cache.new"]),
		"cache_empty_per_min":   per(c["series.cache.empty"]),
		"query_seconds_per_min": perfRound(perfQuerySeconds()/elapsed, 4),
		"raw":                   c,
	})

	// THE CELL HAS TO RECONCILE. Store.Series books exactly one outcome per call
	// and every outcome but the cache hit goes on to run a scan, so the outcomes
	// below have to account for series.query exactly. A mismatch means this
	// harness is not collecting every state the store records, and the query
	// rate it just printed no longer matches the causes it can name.
	if scans := c["series.cache.new"] + c["series.cache.empty"] + c["series.cache.expired"] + c["series.bypass"]; scans != c["series.query"] {
		t.Errorf("counters do not reconcile: %d queries against %d scan-running outcomes %v", c["series.query"], scans, c)
	}
	// An empty result is never pinned (Store.Series, internal/store/store.go), so
	// a window with no rows can never be hit and every poll of it scans. Those
	// rates are the shape of an empty window rather than of the seeded database
	// this cell exists to measure, and the seed is this harness's own doing - so
	// this is a broken run, not a result. Check the window against the seed span.
	if c["series.cache.empty"] > 0 {
		t.Errorf("%d polls found the %s window empty against a %d-day seed: an empty window is never cached, so these rates measure the never-cached path",
			c["series.cache.empty"], scenario, seedDays)
	}
}

// TestPerfSeriesCost measures ONE scan at each preset width on a database seeded
// the way the shipped defaults fill one, with nothing else running.
//
// Every sample is a DISTINCT cache key, so each is a real scan and not a map
// lookup: the window slides back by whole bucket widths, which keeps the span
// (and so the bucket width the server picks) identical while making the end -
// the part of the key that is never floored - unique.
func TestPerfSeriesCost(t *testing.T) {
	perfSkip(t, "cost")
	stack := os.Getenv("PERF_STACK")
	seedDays := perfEnvInt("PERF_SEED_DAYS", 30)
	seedInterval := perfEnvInt("PERF_SEED_INTERVAL_SEC", 5)
	reps := perfEnvInt("PERF_REPS", 5)

	end := time.Now().Truncate(time.Hour)
	seedStart := time.Now()
	st := perfSeed(t, filepath.Join(t.TempDir(), "perf.db"), stack,
		time.Duration(seedDays)*24*time.Hour, end, seedInterval)
	defer st.Close() //nolint:errcheck // measurement fixture
	seedFor := time.Since(seedStart)
	h := perfServer(t, st)

	rows := []map[string]any{}
	for _, w := range []struct {
		name string
		mins int
	}{{"5m", 5}, {"1h", 60}, {"6h", 360}, {"1d", 1440}, {"7d", 10080}, {"30d", 43200}} {
		span := time.Duration(w.mins) * time.Minute
		bucket := int(span/time.Second) / 1500
		if bucket < 1 {
			bucket = 1
		}
		walls := make([]float64, 0, reps)
		for i := 0; i < reps; i++ {
			to := end.Add(-time.Duration(i*bucket) * time.Second)
			from := to.Add(-span)
			p := fmt.Sprintf("/api/series?from=%d&to=%d", from.Unix(), to.Unix())
			before := perfQuerySeconds()
			if code := perfGet(h, p); code != http.StatusOK {
				t.Fatalf("%s: /api/series returned %d", w.name, code)
			}
			walls = append(walls, perfQuerySeconds()-before)
		}
		sort.Float64s(walls)
		rows = append(rows, map[string]any{
			"window":               w.name,
			"mins":                 w.mins,
			"bucket_sec":           bucket,
			"query_seconds_min":    perfRound(walls[0], 4),
			"query_seconds_median": perfRound(walls[len(walls)/2], 4),
			"query_seconds_max":    perfRound(walls[len(walls)-1], 4),
		})
	}
	perfEmit(t, map[string]any{
		"mode":              "cost",
		"stack":             stack,
		"seed_days":         seedDays,
		"seed_interval_sec": seedInterval,
		"reps":              reps,
		"seed_seconds":      perfRound(seedFor.Seconds(), 3),
		"windows":           rows,
	})
}
