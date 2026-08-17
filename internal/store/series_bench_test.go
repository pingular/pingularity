package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The chart aggregate had no benchmark, so every cost figure argued about the
// Series cache was arithmetic. These measure the thing the cache exists to
// avoid: ONE seriesQuery scan, over a database seeded the way the shipped
// defaults fill it - a probe round every 5s (config.Default, config.go:164)
// across the six default targets (config.DefaultTargets, config.go:104) plus the
// one DNS row per round the chart's second line comes from - for the windows a
// dashboard actually asks for.
//
// seriesQuery is called directly, NOT Series: Series would serve the second and
// later iterations from the cache, and the benchmark would then measure a map
// lookup.
//
// The default ns/op is wall time; cpu-ms/op is whole-process CPU (user+system,
// see processCPU), which is what a chart poll takes away from the prober on a
// small box, and is absent on platforms without getrusage(2).

// benchTarget is one seeded probe target.
type benchTarget struct{ name, family string }

// benchStack is one address-family shape of the same install: a dual-stack box
// writes twice the sample rows per round of an IPv4-only one, and the aggregate
// groups by family (famExpr, store.go:1161), so the two are genuinely different
// scans rather than one scaled by two.
type benchStack struct {
	name    string
	targets []benchTarget
}

func benchStacks() []benchStack {
	v4 := []benchTarget{{"cloudflare", "ipv4"}, {"google", "ipv4"}, {"quad9", "ipv4"}}
	dual := append(append([]benchTarget{}, v4...),
		benchTarget{"cloudflare-v6", "ipv6"}, benchTarget{"google-v6", "ipv6"}, benchTarget{"quad9-v6", "ipv6"})
	return []benchStack{{"ipv4", v4}, {"dual", dual}}
}

// seedSeriesDB writes span of probe history ending at end, one round every
// intervalSec, into a fresh file-backed store. File-backed, not ":memory:",
// because production reads a file: an in-memory database hides the page cache
// and the I/O the real scan does, and thirty days of five-second rounds is
// millions of rows to hold in the test process besides.
func seedSeriesDB(tb testing.TB, stack benchStack, span time.Duration, end time.Time, intervalSec int) *Store {
	tb.Helper()
	st, err := Open(filepath.Join(tb.TempDir(), "bench.db"))
	if err != nil {
		tb.Fatalf("open: %v", err)
	}
	tb.Cleanup(func() { st.Close() })
	tx, err := st.db.Begin()
	if err != nil {
		tb.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below; this is only the failure path
	ins, err := tx.Prepare(`INSERT INTO samples (ts, target, latency_ms, success, family) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare samples: %v", err)
	}
	defer ins.Close()
	insDNS, err := tx.Prepare(`INSERT INTO dns (ts, latency_ms, success) VALUES (?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare dns: %v", err)
	}
	defer insDNS.Close()
	// A deterministic LCG, not math/rand: two runs of the benchmark must scan
	// byte-identical data, or the before/after comparison it exists for is noise.
	seed := uint32(1)
	next := func() uint32 { seed = seed*1664525 + 1013904223; return seed }
	for ts := end.Add(-span).Unix(); ts < end.Unix(); ts += int64(intervalSec) {
		// One round in ~200 fails outright, so the MIN over successes and the
		// per-family quorum each have a mixture to chew on instead of a column of
		// identical rows.
		down := next()%200 == 0
		for _, t := range stack.targets {
			var lat any
			ok := 0
			if !down {
				lat = 5 + float64(next()%9500)/100 // ~5-100ms
				ok = 1
			}
			if _, err := ins.Exec(ts, t.name, lat, ok, t.family); err != nil {
				tb.Fatalf("insert sample: %v", err)
			}
		}
		if _, err := insDNS.Exec(ts, 1+float64(next()%4000)/100, 1); err != nil {
			tb.Fatalf("insert dns: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit: %v", err)
	}
	var rows int64
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM samples`).Scan(&rows); err != nil {
		tb.Fatalf("count: %v", err)
	}
	if rows == 0 {
		tb.Fatal("seeded no samples")
	}
	tb.Logf("%s: %d sample rows over %v at %ds rounds", stack.name, rows, span, intervalSec)
	return st
}

func BenchmarkSeriesQuery(b *testing.B) {
	ctx := context.Background()
	// The seed reaches back the full default latency retention (30 days,
	// config.Default's Retention field, config.go:177), which is also the widest
	// window measured.
	const retention = 30 * 24 * time.Hour
	// One fixed end for every sub-benchmark, so each window covers exactly the
	// rows it covered on the previous run.
	end := time.Now().Truncate(time.Hour)
	for _, stack := range benchStacks() {
		st := seedSeriesDB(b, stack, retention, end, 5)
		for _, win := range []struct {
			name string
			span time.Duration
		}{
			{"1d", 24 * time.Hour},
			{"7d", 7 * 24 * time.Hour},
			{"30d", retention},
		} {
			// The width the server itself would pick: span/maxSeriesPoints, 1500
			// (seriesBucket web.go:1193, maxSeriesPoints web.go:1177) - 57s at 1d,
			// 403s at 7d, 1728s at 30d. A round number instead would measure a
			// bucket count no window ever asks for.
			bucket := int(win.span/time.Second) / 1500
			b.Run(stack.name+"/"+win.name, func(b *testing.B) {
				since := end.Add(-win.span)
				var pts []SeriesPoint
				b.ReportAllocs()
				b.ResetTimer()
				cpu0, cpuOK := processCPU()
				for i := 0; i < b.N; i++ {
					var err error
					pts, err = st.seriesQuery(ctx, since, end, bucket, nil)
					if err != nil {
						b.Fatalf("seriesQuery: %v", err)
					}
				}
				b.StopTimer()
				if cpu1, ok := processCPU(); cpuOK && ok {
					b.ReportMetric(float64(cpu1-cpu0)/float64(b.N)/float64(time.Millisecond), "cpu-ms/op")
				}
				// A window that returned nothing would benchmark an empty scan and
				// publish a flattering number for work never done.
				if len(pts) == 0 {
					b.Fatal("empty result: the seed does not cover the window")
				}
				b.ReportMetric(float64(len(pts)), "points")
			})
		}
	}
}

// Guard the benchmark's own seeder. A schema drift (renamed column, a new NOT
// NULL) would leave every future figure a measurement of an empty table, and a
// benchmark cannot fail a test run - nothing else would notice. Ten minutes of
// rounds, so it costs a test run nothing.
func TestSeedSeriesDBProducesQueryableRows(t *testing.T) {
	end := time.Now().Truncate(time.Minute)
	st := seedSeriesDB(t, benchStacks()[1], 10*time.Minute, end, 5)
	pts, err := st.seriesQuery(context.Background(), end.Add(-10*time.Minute), end, 60, nil)
	if err != nil {
		t.Fatalf("seriesQuery over seeded rows: %v", err)
	}
	if len(pts) == 0 {
		t.Fatal("seeded 10 minutes of dual-stack rounds but the aggregate returned no points")
	}
	dns := false
	for _, p := range pts {
		if p.DNSms != nil {
			dns = true
			break
		}
	}
	if !dns {
		t.Fatal("no bucket carried a DNS mean: the seeder's dns rows are not joining, so the benchmark would be measuring the latency scan alone")
	}
}
