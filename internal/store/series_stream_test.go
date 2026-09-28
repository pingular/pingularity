package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seriesSQLOracle is the statement seriesQuery ran before it folded rows in Go,
// kept as the oracle: the rewrite must not change a single point, and "it looks
// right" is not evidence for a change to every chart the dashboard draws. The
// window is given in unix seconds so no clock sits between the two sides.
//
// Its answer depends on its plan. A DNS mean is a sum of floats, and such a sum
// depends on the order of addition. Without statistics SQLite reads dns through
// the ts index, so the mean is added up in ts order, which is the order the fold
// uses. With them (after ANALYZE) it can scan the table in rowid order and
// return a mean that differs in the last bit. No build of pingularity has ever
// made statistics, so "identical" means identical to the plan without them, and
// the oracle refuses a store that has them rather than answer from the other
// plan. It also runs on the bundled SQLite: if a driver bump changes how AVG
// adds up, it is the oracle that moves, not the fold.
//
// One difference is known and accepted. A sample whose ts is stored as a
// fraction always fails the fold. It failed the old statement too, with one
// exception: where the floating-point arithmetic happened to give the row a
// bucket that is a whole number, and a small one (database/sql reads a float as
// an integer only below a million, a date in the first days of 1970), the old
// statement read the row. Keeping that would mean points that arrive out of
// order, for a row no build writes.
// TestSeriesStreamFailsOnEveryFractionalSampleTime pins it. A dns row with such
// a ts never failed the read and still does not, and is counted where it was.
func seriesSQLOracle(ctx context.Context, q rowQuerier, sinceU, upperU int64, bucketSec int, exclude []string) ([]SeriesPoint, error) {
	stat, err := q.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE name LIKE 'sqlite_stat%'`)
	if err != nil {
		return nil, err
	}
	analysed := stat.Next()
	if err := stat.Close(); err != nil {
		return nil, err
	}
	if analysed {
		return nil, errors.New("oracle: this store has statistics (ANALYZE ran), so the old statement would not plan as it did in production")
	}
	latFilter := ""
	args := []any{bucketSec, bucketSec}
	for _, t := range exclude {
		latFilter += "?,"
		args = append(args, t)
	}
	if latFilter != "" {
		latFilter = " AND target NOT IN (" + latFilter[:len(latFilter)-1] + ")"
	}
	args = append(args, sinceU, upperU)
	args = append(args, bucketSec, bucketSec, sinceU, upperU)
	rows, err := q.QueryContext(ctx, `
		SELECT ping.bts, MIN(ping.lat) AS lat, MAX(ping.fam_online) AS online, d.dns
		FROM (
			SELECT (ts / ?) * ? AS bts,
			       `+famExpr+` AS fam,
			       MIN(CASE WHEN success = 1`+latFilter+` THEN latency_ms END) AS lat,
			       CASE WHEN SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END) * 2 > COUNT(*) THEN 1 ELSE 0 END AS fam_online
			FROM samples
			WHERE ts >= ? AND ts < ?
			GROUP BY bts, fam
		) ping
		LEFT JOIN (
			SELECT (ts / ?) * ? AS bts, AVG(CASE WHEN success = 1 THEN latency_ms END) AS dns
			FROM dns WHERE ts >= ? AND ts < ? GROUP BY bts
		) d ON d.bts = ping.bts
		GROUP BY ping.bts
		ORDER BY ping.bts`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesPoint
	for rows.Next() {
		var p SeriesPoint
		var lat, dns sql.NullFloat64
		var online int
		if err := rows.Scan(&p.TS, &lat, &online, &dns); err != nil {
			return nil, err
		}
		if lat.Valid {
			v := lat.Float64
			p.LatencyMS = &v
		}
		if dns.Valid {
			v := dns.Float64
			p.DNSms = &v
		}
		p.Online = online == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// seriesPointsDiff compares bit for bit: a float that prints the same can still
// differ, and a nil slice is not an empty one (the handler tells them apart).
func seriesPointsDiff(want, got []SeriesPoint) string {
	if (want == nil) != (got == nil) {
		return fmt.Sprintf("oracle nil=%v, new nil=%v", want == nil, got == nil)
	}
	if len(want) != len(got) {
		return fmt.Sprintf("oracle has %d points, new has %d", len(want), len(got))
	}
	f := func(p *float64) string {
		if p == nil {
			return "nil"
		}
		return fmt.Sprintf("%v (%016x)", *p, math.Float64bits(*p))
	}
	same := func(a, b *float64) bool {
		if a == nil || b == nil {
			return a == b
		}
		return math.Float64bits(*a) == math.Float64bits(*b)
	}
	for i := range want {
		w, g := want[i], got[i]
		if w.TS != g.TS || w.Online != g.Online || !same(w.LatencyMS, g.LatencyMS) || !same(w.DNSms, g.DNSms) {
			return fmt.Sprintf("point %d: oracle {t=%d lat=%s online=%v dns=%s}, new {t=%d lat=%s online=%v dns=%s}",
				i, w.TS, f(w.LatencyMS), w.Online, f(w.DNSms), g.TS, f(g.LatencyMS), g.Online, f(g.DNSms))
		}
	}
	return ""
}

// seriesStores gives every comparison both pools: open(t) is the one-connection
// in-memory store (a file when PINGULARITY_TEST_DB_DIR is set), and the second
// is always a file with the production four connections.
func seriesStores(t *testing.T) map[string]*Store {
	t.Helper()
	f, err := Open(filepath.Join(t.TempDir(), "series.db"))
	if err != nil {
		t.Fatalf("open file store: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return map[string]*Store{"default": open(t), "file": f}
}

func seriesExec(t *testing.T, st *Store, stmts ...string) {
	t.Helper()
	for _, q := range stmts {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

type seriesEdge struct {
	name         string
	rows         []string
	since, upper int64
	exclude      [][]string
}

var seriesEdgeWidths = []int{1, 2, 7, 60, 403, -60}

func seriesEdges() []seriesEdge {
	none := [][]string{nil}
	return []seriesEdge{
		{"empty window", nil, 0, 1000, none},
		{"dns without samples makes no point", []string{
			`INSERT INTO dns VALUES (100, 5.5, 1)`,
		}, 0, 1000, none},
		{"dns in a bucket without samples is dropped", []string{
			`INSERT INTO dns VALUES (100, 5.5, 1)`,
			`INSERT INTO samples VALUES (200,'a',1.5,1,'ipv4')`,
			`INSERT INTO dns VALUES (200, 7.25, 1)`,
			`INSERT INTO dns VALUES (900, 7.25, 1)`,
		}, 0, 1000, none},
		{"dns with a fractional timestamp", []string{
			`INSERT INTO samples VALUES (-5,'a',4,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'a',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (120,'a',6,1,'ipv4')`,
			`INSERT INTO dns VALUES (100.5, 2.5, 1)`,
			`INSERT INTO dns VALUES (101, 3.5, 1)`,
			`INSERT INTO dns VALUES (119.99999999999999, 4.5, 1)`,
			`INSERT INTO dns VALUES (120.00000000000001, 5.5, 1)`,
			`INSERT INTO dns VALUES (-4.5, 6.5, 1)`,
			// So small that dividing by the width leaves zero: bucket 0, and counted.
			`INSERT INTO dns VALUES (5e-324, 7.5, 1)`,
			`INSERT INTO dns VALUES (-5e-324, 8.5, 1)`,
			`INSERT INTO dns VALUES (1e-320, 9.5, 0)`,
			`INSERT INTO dns VALUES (0, 10.5, 1)`,
			`INSERT INTO dns VALUES (1, 11.5, 1)`,
		}, -1000, 1000, none},
		{"only fractional dns timestamps", []string{
			`INSERT INTO samples VALUES (100,'a',5,1,'ipv4')`,
			`INSERT INTO dns VALUES (100.5, 2.5, 1)`,
			`INSERT INTO dns VALUES (100.25, 3.5, 1)`,
		}, 0, 1000, none},
		{"no family falls back to the -v6 suffix, in any case", []string{
			`INSERT INTO samples VALUES (100,'cf',10,1,NULL)`,
			`INSERT INTO samples VALUES (100,'cf-v6',20,0,NULL)`,
			`INSERT INTO samples VALUES (100,'gg-V6',30,0,'')`,
			`INSERT INTO samples VALUES (100,'q9-v6x',40,0,NULL)`,
			`INSERT INTO samples VALUES (100,'q8',40,0,'')`,
		}, 0, 1000, none},
		{"half up is offline", []string{
			`INSERT INTO samples VALUES (100,'a',10,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (101,'a',10,1,'ipv6')`,
			`INSERT INTO samples VALUES (101,'b',11,1,'ipv6')`,
			`INSERT INTO samples VALUES (101,'c',NULL,0,'ipv6')`,
		}, 0, 1000, none},
		{"families other than the two", []string{
			`INSERT INTO samples VALUES (100,'a',10,1,'IPV4')`,
			`INSERT INTO samples VALUES (100,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (100,'c',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (160,'a',10,1,'x')`,
			`INSERT INTO samples VALUES (160,'b',NULL,0,x'78')`,
			`INSERT INTO samples VALUES (220,'a',10,1,'x')`,
			`INSERT INTO samples VALUES (220,'b',NULL,0,'x')`,
			`INSERT INTO samples VALUES (280,'a',10,1,x'69707634')`,
			`INSERT INTO samples VALUES (280,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (340,'a',NULL,0,x'69707634')`,
			`INSERT INTO samples VALUES (340,'b',10,1,'ipv4')`,
			`INSERT INTO samples VALUES (340,'c',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (400,'a',10,1,x'')`,
			`INSERT INTO samples VALUES (400,'b-v6',NULL,0,x'')`,
			`INSERT INTO samples VALUES (460,'a',10,1,'ip' || char(0) || 'v4')`,
			`INSERT INTO samples VALUES (460,'b',NULL,0,'ip')`,
			`INSERT INTO samples VALUES (520,'a',10,1,12)`,
			`INSERT INTO samples VALUES (520,'b',NULL,0,'12')`,
		}, 0, 1000, none},
		{"families named like the fold's own codes", []string{
			`INSERT INTO samples VALUES (100,'a',10,1,'0')`,
			`INSERT INTO samples VALUES (100,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (100,'c',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (160,'a',10,1,'1')`,
			`INSERT INTO samples VALUES (160,'b',NULL,0,'ipv6')`,
			`INSERT INTO samples VALUES (160,'c',NULL,0,'ipv6')`,
			`INSERT INTO samples VALUES (220,'a',10,1,0)`,
			`INSERT INTO samples VALUES (220,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (220,'c',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (280,'a',10,1,1)`,
			`INSERT INTO samples VALUES (280,'b',NULL,0,'ipv6')`,
			`INSERT INTO samples VALUES (280,'c',NULL,0,'ipv6')`,
			`INSERT INTO samples VALUES (340,'a',10,1,1.5)`,
			`INSERT INTO samples VALUES (340,'b',NULL,0,'2')`,
			`INSERT INTO samples VALUES (340,'c',NULL,0,x'30')`,
			`INSERT INTO samples VALUES (340,'d',NULL,0,x'31')`,
		}, 0, 1000, none},
		{"exclude across families", []string{
			`INSERT INTO samples VALUES (100,'cloudflare',50,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'google',10,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'google-v6',5,1,'ipv6')`,
			`INSERT INTO samples VALUES (100,'quad9-v6',NULL,0,'ipv6')`,
			`INSERT INTO samples VALUES (100,'Google',7,1,'ipv4')`,
			`INSERT INTO samples VALUES (170,'google',3,1,'ipv4')`,
		}, 0, 1000, [][]string{nil, {"google"}, {"google", "google-v6"}, {"google-v6", "google", "cloudflare", "Google"}, {"nobody"}, {""}, {"GOOGLE"}}},
		{"success without a latency, success out of range", []string{
			`INSERT INTO samples VALUES (100,'a',NULL,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b',NULL,0,'ipv4')`,
			`INSERT INTO samples VALUES (100,'c',NULL,1,'ipv4')`,
			`INSERT INTO samples VALUES (200,'a',5,4611686018427387904,'ipv4')`,
			`INSERT INTO samples VALUES (300,'a',5,2,'ipv4')`,
			`INSERT INTO samples VALUES (300,'b',6,-1,'ipv4')`,
			`INSERT INTO samples VALUES (300,'c',7,1,'ipv4')`,
			`INSERT INTO dns VALUES (100, 5.5, 2)`,
			`INSERT INTO dns VALUES (100, NULL, 1)`,
			`INSERT INTO dns VALUES (300, 9.5, 0)`,
			`INSERT INTO dns VALUES (300, 1.5, 1)`,
			`INSERT INTO dns VALUES (300, 2.25, 1)`,
		}, 0, 1000, none},
		{"zero, infinite and cancelling values", []string{
			`INSERT INTO samples VALUES (100,'a',0.0,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b',-0.0,1,'ipv6')`,
			`INSERT INTO samples VALUES (220,'a',9e999,1,'ipv4')`,
			`INSERT INTO samples VALUES (280,'a',-9e999,1,'ipv4')`,
			`INSERT INTO samples VALUES (280,'b',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (340,'a',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (340,'b',-3.5,1,'ipv4')`,
			`INSERT INTO samples VALUES (400,'a',7,1,'ipv4')`,
			`INSERT INTO dns VALUES (100, 0.1, 1)`,
			`INSERT INTO dns VALUES (100, 0.2, 1)`,
			`INSERT INTO dns VALUES (100, 0.3, 1)`,
			`INSERT INTO dns VALUES (220, 9e999, 1)`,
			`INSERT INTO dns VALUES (220, 1, 1)`,
			`INSERT INTO dns VALUES (280, 9e999, 1)`,
			`INSERT INTO dns VALUES (280, -9e999, 1)`,
			`INSERT INTO dns VALUES (340, 1e16, 1)`,
			`INSERT INTO dns VALUES (340, 1, 1)`,
			`INSERT INTO dns VALUES (340, -1e16, 1)`,
			`INSERT INTO dns VALUES (400, -0.0, 1)`,
		}, 0, 1000, none},
		// The next two are the only cases here whose mean changes with the order
		// of addition: a cancelling pair of 2^100 around values it swallows. The
		// first has rowid order a, b, c, e, -a and ts order a, c, e, b, -a. The
		// second has one ts, so the index hands the rows over as they were written.
		// TestSeriesDNSMeanIsAddedUpInTsOrder holds them to that.
		{"dns mean depends on the order of addition", append([]string{
			`INSERT INTO samples VALUES (5,'a',5,1,'ipv4')`},
			seriesOrderRows(10, 13, 11, 12, 14)...), 0, 1000, none},
		{"dns mean depends on the order of addition, one timestamp", append([]string{
			`INSERT INTO samples VALUES (5,'a',5,1,'ipv4')`},
			seriesOrderRows(10, 10, 10, 10, 10)...), 0, 1000, none},
		{"timestamps around zero", []string{
			`INSERT INTO samples VALUES (-130,'a',4,1,'ipv4')`,
			`INSERT INTO samples VALUES (-61,'a',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (-5,'a',6,1,'ipv4')`,
			`INSERT INTO samples VALUES (0,'a',7,0,'ipv4')`,
			`INSERT INTO samples VALUES (5,'a',8,1,'ipv4')`,
			`INSERT INTO samples VALUES (59,'a',9,0,'ipv4')`,
			`INSERT INTO samples VALUES (61,'a',9,0,'ipv4')`,
			`INSERT INTO dns VALUES (-5, 1.5, 1)`,
			`INSERT INTO dns VALUES (5, 2.5, 1)`,
			`INSERT INTO dns VALUES (-100, 3.5, 1)`,
		}, -1000, 1000, none},
		{"the window is half open", []string{
			`INSERT INTO samples VALUES (99,'a',4,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'a',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (199,'a',6,1,'ipv4')`,
			`INSERT INTO samples VALUES (200,'a',7,1,'ipv4')`,
			`INSERT INTO dns VALUES (99, 1.5, 1)`,
			`INSERT INTO dns VALUES (100, 2.5, 1)`,
			`INSERT INTO dns VALUES (199, 3.5, 1)`,
			`INSERT INTO dns VALUES (200, 4.5, 1)`,
		}, 100, 200, none},
		{"text and blobs left in latency_ms", []string{
			`INSERT INTO samples VALUES (100,'a','abc',1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'c',x'31',1,'ipv6')`,
			`INSERT INTO samples VALUES (200,'a',x'3331',1,'ipv4')`,
			`INSERT INTO samples VALUES (200,'b',x'39',1,'ipv6')`,
			`INSERT INTO samples VALUES (200,'c','abc',0,'ipv6')`,
			`INSERT INTO samples VALUES (300,'a','abc',0,'ipv4')`,
			`INSERT INTO samples VALUES (300,'b',NULL,1,'ipv4')`,
			`INSERT INTO dns VALUES (100, 'abc', 1)`,
			`INSERT INTO dns VALUES (100, 4, 1)`,
			`INSERT INTO dns VALUES (100, '12abc', 1)`,
			`INSERT INTO dns VALUES (100, x'3332', 1)`,
			`INSERT INTO dns VALUES (100, 'zzz', 0)`,
		}, 0, 1000, none},
	}
}

// seriesOrderValues is a set whose compensated mean depends on the order it is
// added up in, written here in the order the rows are inserted.
var seriesOrderValues = []float64{
	1267650600228229401496703205376.0, // 2^100
	1.0,
	1.1102230246251565e-16, // 2^-53
	8.673617379884035e-19,  // 2^-60
	-1267650600228229401496703205376.0,
}

// seriesOrderRows inserts seriesOrderValues, in that order, at the given
// timestamps.
func seriesOrderRows(ts ...int64) []string {
	var out []string
	for i, v := range seriesOrderValues {
		out = append(out, fmt.Sprintf(`INSERT INTO dns VALUES (%d, %.17g, 1)`, ts[i], v))
	}
	return out
}

func TestSeriesStreamMatchesSQLOracleOnEdgeRows(t *testing.T) {
	ctx := context.Background()
	for _, c := range seriesEdges() {
		for kind, st := range seriesStores(t) {
			seriesExec(t, st, c.rows...)
			for _, w := range seriesEdgeWidths {
				for _, ex := range c.exclude {
					want, err := seriesSQLOracle(ctx, st.db, c.since, c.upper, w, ex)
					if err != nil {
						t.Fatalf("%s: the oracle failed, so the case proves nothing: %v", c.name, err)
					}
					got, err := st.seriesQuery(ctx, time.Unix(c.since, 0), time.Unix(c.upper, 0), w, ex)
					if err != nil {
						t.Errorf("%s (%s store, width %d, exclude %q): %v", c.name, kind, w, ex, err)
						continue
					}
					if d := seriesPointsDiff(want, got); d != "" {
						t.Errorf("%s (%s store, width %d, exclude %q): %s", c.name, kind, w, ex, d)
					}
				}
			}
		}
	}
}

// Where the old statement failed the read, the new one fails it too.
func TestSeriesStreamFailsWhereTheSQLFailed(t *testing.T) {
	ctx := context.Background()
	for _, c := range []seriesEdge{
		{"only text succeeded", []string{`INSERT INTO samples VALUES (100,'a','abc',1,'ipv4')`}, 0, 1000, [][]string{nil}},
		{"only text is left after the exclude", []string{
			`INSERT INTO samples VALUES (100,'a','abc',1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b',5,1,'ipv4')`,
		}, 0, 1000, [][]string{{"b"}}},
		{"text ranks below a blob that would have parsed", []string{
			`INSERT INTO samples VALUES (100,'a',x'31',1,'ipv4')`,
			`INSERT INTO samples VALUES (100,'b','zz',1,'ipv4')`,
		}, 0, 1000, [][]string{nil}},
		{"a fractional timestamp", []string{`INSERT INTO samples VALUES (100.5,'a',5,1,'ipv4')`}, 0, 1000, [][]string{nil}},
	} {
		for kind, st := range seriesStores(t) {
			seriesExec(t, st, c.rows...)
			for _, ex := range c.exclude {
				if _, err := seriesSQLOracle(ctx, st.db, c.since, c.upper, 60, ex); err == nil {
					t.Fatalf("%s: the oracle read it, so the case proves nothing", c.name)
				}
				if pts, err := st.seriesQuery(ctx, time.Unix(c.since, 0), time.Unix(c.upper, 0), 60, ex); err == nil {
					t.Errorf("%s (%s store): read %d points where the old statement failed", c.name, kind, len(pts))
				}
			}
		}
	}
	st := open(t)
	if _, err := st.seriesQuery(ctx, time.Unix(0, 0), time.Unix(1000, 0), 0, nil); err == nil {
		t.Error("a zero bucket width must be an error, not a division")
	}
}

// The one known difference from the old statement (see seriesSQLOracle), pinned
// so that it stays a decision. 119.99999999999999 over a width of 7 comes out
// of the floating-point arithmetic as 120, a whole number, and the old
// statement drew a point there, between the buckets at 119 and 126. At the
// other widths it failed the read, as the fold does at all of them.
func TestSeriesStreamFailsOnEveryFractionalSampleTime(t *testing.T) {
	ctx := context.Background()
	for kind, st := range seriesStores(t) {
		seriesExec(t, st,
			`INSERT INTO samples VALUES (119.99999999999999,'a',5,1,'ipv4')`,
			`INSERT INTO samples VALUES (130,'a',6,1,'ipv4')`)
		old, err := seriesSQLOracle(ctx, st.db, 0, 1000, 7, nil)
		if err != nil || len(old) != 2 || old[0].TS != 120 || old[1].TS != 126 {
			t.Fatalf("premise broken (%s store): the old statement no longer reads this row at width 7: %v, %v", kind, old, err)
		}
		for _, w := range []int{1, 7, 60, 403} {
			if pts, err := st.seriesQuery(ctx, time.Unix(0, 0), time.Unix(1000, 0), w, nil); err == nil {
				t.Errorf("%s store, width %d: read %d points from a sample with a fractional ts", kind, w, len(pts))
			}
		}
	}
}

// The order-sensitive edge cases only prove something while their values stay
// order-sensitive. This adds them up here, in the order the ts index hands the
// rows over and in another one, requires the two to differ, and requires the
// chart to carry the first.
func TestSeriesDNSMeanIsAddedUpInTsOrder(t *testing.T) {
	ctx := context.Background()
	mean := func(order ...int) uint64 {
		var m seriesMean
		for _, i := range order {
			m.add(seriesOrderValues[i])
		}
		v, ok := m.value()
		if !ok {
			t.Fatal("premise broken: the values have no mean")
		}
		return math.Float64bits(v)
	}
	for _, c := range []struct {
		name         string
		ts           []int64
		inTs, inRead []int // index order, and an order a scan could take instead
	}{
		{"rows written out of ts order", []int64{10, 13, 11, 12, 14}, []int{0, 2, 3, 1, 4}, []int{0, 1, 2, 3, 4}},
		{"rows of one ts", []int64{10, 10, 10, 10, 10}, []int{0, 1, 2, 3, 4}, []int{4, 3, 2, 1, 0}},
	} {
		want, other := mean(c.inTs...), mean(c.inRead...)
		if want == other {
			t.Fatalf("premise broken (%s): the mean is %016x in both orders, so the case cannot tell them apart", c.name, want)
		}
		for kind, st := range seriesStores(t) {
			seriesExec(t, st, append([]string{`INSERT INTO samples VALUES (5,'a',5,1,'ipv4')`}, seriesOrderRows(c.ts...)...)...)
			pts, err := st.seriesQuery(ctx, time.Unix(0, 0), time.Unix(1000, 0), 60, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(pts) != 1 || pts[0].DNSms == nil {
				t.Fatalf("%s (%s store): want one point with a DNS mean, got %v", c.name, kind, pts)
			}
			if got := math.Float64bits(*pts[0].DNSms); got != want {
				t.Errorf("%s (%s store): mean %016x, want %016x (ts order); the other order gives %016x", c.name, kind, got, want, other)
			}
		}
	}
}

// fracBucket repeats SQLite's floating-point arithmetic in Go. This asks SQLite
// for the same sums and requires the same bits, near whole numbers, near zero
// and far from both.
func TestSeriesFracBucketRoundsAsSQLite(t *testing.T) {
	st := open(t)
	seed := uint32(20260928)
	next := func(n uint32) uint32 { seed = seed*1664525 + 1013904223; return (seed >> 8) % n }
	widths := []int64{1, 2, 3, 7, 14, 57, 60, 61, 403, 1728, 86400, -60}
	var counted int
	for i := 0; i < 4000; i++ {
		w := widths[next(uint32(len(widths)))]
		var ts float64
		switch next(4) {
		case 0: // a hair off a bucket's start
			ts = float64((int64(next(1<<20)) - 1<<19) * w)
		case 1: // a hair off any whole number
			ts = float64(int64(next(1<<30)) - 1<<29)
		case 2: // a present-day timestamp
			ts = float64(1_788_220_800 - int64(next(1<<22)))
		default: // next to zero
			ts = 0
		}
		dir := math.Inf(1)
		if next(2) == 0 {
			dir = math.Inf(-1)
		}
		for k := next(3) + 1; k > 0; k-- {
			ts = math.Nextafter(ts, dir)
		}
		if next(5) == 0 {
			ts += 0.5
		}
		if ts == math.Trunc(ts) {
			continue // a whole number is stored as an INTEGER and never comes here
		}
		var r float64
		if err := st.db.QueryRow(`SELECT (? / ?) * ?`, ts, w, w).Scan(&r); err != nil {
			t.Fatalf("ts %v width %d: %v", ts, w, err)
		}
		a := seriesAgg{width: w}
		wantOK := r == math.Trunc(r) && int64(r)%w == 0
		b, ok := a.fracBucket(ts)
		if ok != wantOK || (ok && b != int64(r)) {
			t.Errorf("ts %.17g width %d: SQLite's bucket is %.17g, fracBucket says %d, %v", ts, w, r, b, ok)
		}
		if ok {
			counted++
		}
	}
	if counted == 0 {
		t.Error("premise broken: no timestamp here lands on a bucket, so the counted half was never compared")
	}
}

// seriesMessUp makes seeded history less regular: rows from before the family
// column, a family that is neither of the two, failed targets, failed and
// out-of-order DNS rows, and hours with no samples at all.
func seriesMessUp(t *testing.T, st *Store) {
	t.Helper()
	seriesExec(t, st,
		`UPDATE samples SET family = NULL WHERE ts % 97 = 0`,
		`UPDATE samples SET family = '' WHERE ts % 89 = 0`,
		`UPDATE samples SET family = 'ipv9' WHERE ts % 1013 = 0 AND target = 'google'`,
		`UPDATE samples SET success = 0, latency_ms = NULL WHERE ts % 31 = 0 AND target IN ('google','quad9')`,
		`UPDATE dns SET success = 0, latency_ms = NULL WHERE ts % 53 = 0`,
		`INSERT INTO dns (ts, latency_ms, success) SELECT ts + 1, latency_ms * 1.37 + 0.001, 1 FROM dns WHERE ts % 7 = 0 ORDER BY latency_ms`,
		`DELETE FROM samples WHERE ts % 3600 < 300 AND ts % 7200 < 3600`,
	)
}

func TestSeriesStreamMatchesSQLOracleOnSeededHistory(t *testing.T) {
	ctx := context.Background()
	end := time.Unix(1_788_220_800, 0)
	excludes := [][]string{nil, {"google"}, {"cloudflare", "cloudflare-v6", "quad9"}}
	stacks, widths := benchStacks(), []int{1, 2, 4, 14, 57, 60, 403, 1728, 86400}
	if raceEnabled || testing.Short() {
		// The comparison is one goroutine reading its own rows, so the race
		// detector has nothing to find in it and makes it thirty times slower.
		// The whole matrix runs in the file-backed leg, which is not instrumented.
		stacks, widths = stacks[1:], []int{2, 60, 403}
	}
	for _, stack := range stacks {
		// seedSeriesDB only ever makes a file. The same rows also go into open(t),
		// the one-connection in-memory store, where the scan's read snapshot holds
		// the only connection there is.
		mem := open(t)
		seedSeriesInto(t, mem, stack, 2*time.Hour, end, 5)
		for kind, st := range map[string]*Store{"default": mem, "file": seedSeriesDB(t, stack, 2*time.Hour, end, 5)} {
			seriesMessUp(t, st)
			n := 0
			for _, win := range []time.Duration{5 * time.Minute, time.Hour, 2 * time.Hour} {
				for _, w := range widths {
					for _, ex := range excludes {
						since, upper := end.Add(-win).Unix()+3, end.Unix()-7
						want, err := seriesSQLOracle(ctx, st.db, since, upper, w, ex)
						if err != nil {
							t.Fatalf("oracle: %v", err)
						}
						got, err := st.seriesQuery(ctx, time.Unix(since, 0), time.Unix(upper, 0), w, ex)
						if err != nil {
							t.Fatalf("seriesQuery: %v", err)
						}
						if len(want) == 0 {
							t.Fatalf("%s %v width %d: the oracle returned nothing, so the case proves nothing", stack.name, win, w)
						}
						if d := seriesPointsDiff(want, got); d != "" {
							t.Errorf("%s (%s store) %v width %d exclude %q: %s", stack.name, kind, win, w, ex, d)
						}
						n++
					}
				}
			}
			t.Logf("%s (%s store): %d windows identical", stack.name, kind, n)
		}
	}
}

// Every other comparison here writes its rows with raw INSERTs. This one reads
// what the real writers store, InsertSamples and InsertDNS: through the two
// fixtures other tests already lean on, and some probe rounds of its own with
// failures, both families and targets that carry no family.
func TestSeriesStreamMatchesSQLOracleOnWrittenRows(t *testing.T) {
	ctx := context.Background()
	for kind, st := range seriesStores(t) {
		now := time.Now()
		seedFutureRows(t, st, now, bareFutureUp)
		seedHistory(t, st, now)
		for i := 0; i < 60; i++ {
			ts := now.Add(-time.Duration(30+5*i) * time.Second)
			lat := 8 + float64(i%17)/3
			if err := st.InsertSamples(ctx, []Sample{
				{TS: ts, Target: "cloudflare", Family: "ipv4", Success: i%7 != 0, LatencyMS: lat},
				{TS: ts, Target: "google", Family: "ipv4", Success: i%3 != 0, LatencyMS: lat / 3},
				{TS: ts, Target: "quad9", Success: i%5 != 0, LatencyMS: lat * 1.1},
				{TS: ts, Target: "cloudflare-v6", Family: "ipv6", Success: i%2 == 0, LatencyMS: lat + 0.1},
				{TS: ts, Target: "google-v6", Success: i%4 == 0, LatencyMS: lat - 0.7},
			}); err != nil {
				t.Fatalf("insert round: %v", err)
			}
			if err := st.InsertDNS(ctx, ts, 1.5+float64(i)/9, i%6 != 0); err != nil {
				t.Fatalf("insert dns: %v", err)
			}
		}
		// Every row sits at least 30s from the horizon an open end stops at (two
		// minutes ahead), so a second ticking over between the two reads of the
		// clock cannot move one across it.
		since := now.Add(-2 * time.Hour)
		for _, until := range []time.Time{{}, now.Add(2 * futureRowAhead), now.Add(-3 * time.Minute)} {
			upper := currentHorizon(time.Now().Unix())
			if !until.IsZero() {
				upper = until.Unix()
			}
			for _, w := range []int{1, 14, 60, 403} {
				for _, ex := range [][]string{nil, {"google"}, {"cf", "a", "quad9"}} {
					want, err := seriesSQLOracle(ctx, st.db, since.Unix(), upper, w, ex)
					if err != nil {
						t.Fatalf("oracle: %v", err)
					}
					if len(want) == 0 {
						t.Fatalf("%s store, width %d: the oracle returned nothing, so the case proves nothing", kind, w)
					}
					got, err := st.seriesQuery(ctx, since, until, w, ex)
					if err != nil {
						t.Fatalf("seriesQuery: %v", err)
					}
					if d := seriesPointsDiff(want, got); d != "" {
						t.Errorf("%s store, until %v, width %d, exclude %q: %s", kind, until, w, ex, d)
					}
				}
			}
		}
	}
}

// The old statement's sorter started merging runs from temp files at about
// 104,000 sample rows (the 1d dual-stack window). This runs the comparison
// past that size, so the path the old answer took there is part of the oracle.
func TestSeriesStreamMatchesSQLOracleAcrossSorterSpill(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("seeds 112,320 sample rows")
	}
	ctx := context.Background()
	end := time.Unix(1_788_220_800, 0)
	st := seedSeriesDB(t, benchStacks()[1], 26*time.Hour, end, 5)
	seriesMessUp(t, st)
	for _, w := range []int{57, 403} {
		for _, ex := range [][]string{nil, {"google", "quad9-v6"}} {
			since, upper := end.Add(-26*time.Hour).Unix(), end.Unix()
			want, err := seriesSQLOracle(ctx, st.db, since, upper, w, ex)
			if err != nil {
				t.Fatalf("oracle: %v", err)
			}
			got, err := st.seriesQuery(ctx, time.Unix(since, 0), time.Unix(upper, 0), w, ex)
			if err != nil {
				t.Fatalf("seriesQuery: %v", err)
			}
			if d := seriesPointsDiff(want, got); d != "" {
				t.Errorf("width %d exclude %q: %s", w, ex, d)
			}
		}
	}
}

func TestSeriesStreamMatchesSQLOracleOnRandomRows(t *testing.T) {
	ctx := context.Background()
	seed := uint32(20260927)
	next := func(n uint32) uint32 { seed = seed*1664525 + 1013904223; return (seed >> 8) % n }
	families := []string{`'ipv4'`, `'ipv4'`, `'ipv6'`, `'ipv6'`, `NULL`, `''`, `'ipv9'`, `x'6970'`}
	targets := []string{"cloudflare", "google", "quad9", "cloudflare-v6", "google-v6", "quad9-V6"}
	excludes := [][]string{nil, {"google"}, {"google", "google-v6", "quad9-V6"}, {"cloudflare", "google", "quad9", "cloudflare-v6", "google-v6", "quad9-V6"}}
	rounds := 40
	if testing.Short() || raceEnabled {
		rounds = 8
	}
	for r := 0; r < rounds; r++ {
		for kind, st := range seriesStores(t) {
			var stmts []string
			for i, n := 0, int(next(400)); i < n; i++ {
				lat, ok := "NULL", []string{"0", "0", "1", "1", "1", "1", "1", "2"}[next(8)]
				if next(10) < 8 {
					lat = fmt.Sprintf("%d.%03d", next(200), next(1000))
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO samples VALUES (%d,'%s',%s,%s,%s)`,
					int64(next(3000))-500, targets[next(uint32(len(targets)))], lat, ok, families[next(uint32(len(families)))]))
			}
			for i, n := 0, int(next(200)); i < n; i++ {
				lat := "NULL"
				if next(10) < 8 {
					lat = fmt.Sprintf("%d.%03d", next(80), next(1000))
				}
				// One dns row in sixteen has a fractional ts, as an import of an older
				// build could leave it.
				frac := []string{"", ".5", ".25", ".000001"}[next(4)]
				if next(16) != 0 {
					frac = ""
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO dns VALUES (%d%s,%s,%d)`, int64(next(3000))-500, frac, lat, next(2)))
			}
			seriesExec(t, st, stmts...)
			for k := 0; k < 12; k++ {
				since := int64(next(2600)) - 600
				upper := since + int64(next(3000))
				w := []int{1, 2, 3, 7, 14, 57, 60, 61, 403, 1728}[next(10)]
				ex := excludes[next(uint32(len(excludes)))]
				want, err := seriesSQLOracle(ctx, st.db, since, upper, w, ex)
				if err != nil {
					t.Fatalf("oracle: %v", err)
				}
				got, err := st.seriesQuery(ctx, time.Unix(since, 0), time.Unix(upper, 0), w, ex)
				if err != nil {
					t.Fatalf("seriesQuery: %v", err)
				}
				if d := seriesPointsDiff(want, got); d != "" {
					t.Fatalf("round %d (%s store) window [%d,%d) width %d exclude %q: %s", r, kind, since, upper, w, ex, d)
				}
			}
		}
	}
}

// The same net over rows no build writes but a database can hold: text and
// blobs where a number belongs, success flags that are neither 0 nor 1, any
// family, dns rows with a fractional ts. Some of these windows the old statement
// could not read, so this compares the failures too: both read the window and
// agree, or neither reads it. Sample rows keep a whole ts (the known difference,
// see seriesSQLOracle).
func TestSeriesStreamMatchesSQLOracleOnOddRows(t *testing.T) {
	ctx := context.Background()
	seed := uint32(20260928)
	next := func(n uint32) uint32 { seed = seed*1664525 + 1013904223; return (seed >> 8) % n }
	pick := func(from []string) string { return from[next(uint32(len(from)))] }
	families := []string{`'ipv4'`, `'ipv4'`, `'ipv6'`, `NULL`, `''`, `'ipv9'`, `x'6970'`, `'0'`, `'1'`, `x''`, `'IPV4'`,
		`'ip' || char(0) || 'x'`, `x'69707634'`, `1.5`}
	targets := []string{"cloudflare", "google", "quad9", "cloudflare-v6", "google-v6", "quad9-V6", "", "a%-v6", "x-v6 "}
	odd := []string{"NULL", "0.0", "-0.0", "9e999", "-9e999", "'abc'", "'12abc'", "x'3331'", "x'39'", "x'7a7a'", "''", "x''",
		"'Inf'", "'0x10'", "' 7 '", "1e308", "5e-324", "'nan'"}
	flags := []string{"0", "1", "1", "1", "2", "-1", "4611686018427387904", "1.0", "0.5"}
	stamps := []string{"%d", "%d", "%d", "%d", "%d", "%d.5", "%d.25", "%d.0000001"}
	excludes := [][]string{nil, {"google"}, {"google", "google-v6", "quad9-V6"}, {""}, {"a%-v6", "x-v6 "}}
	rounds := 60
	if testing.Short() || raceEnabled {
		rounds = 12
	}
	var same, failed int
	for r := 0; r < rounds; r++ {
		for kind, st := range seriesStores(t) {
			var stmts []string
			// Odd latencies in one round of three: a window with one in it often
			// cannot be read at all, and the rounds without keep the rest compared.
			oddLat := r%3 == 0
			for i, n := 0, int(next(300)); i < n; i++ {
				lat := fmt.Sprintf("%d.%03d", next(200), next(1000))
				if oddLat && next(12) == 0 {
					lat = pick(odd)
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO samples VALUES (%d,'%s',%s,%s,%s)`,
					int64(next(3000))-500, pick(targets), lat, pick(flags), pick(families)))
			}
			for i, n := 0, int(next(200)); i < n; i++ {
				lat := fmt.Sprintf("%d.%03d", next(80), next(1000))
				if next(8) == 0 {
					lat = pick(odd)
				}
				ts := fmt.Sprintf(pick(stamps), int64(next(3000))-500)
				if next(40) == 0 {
					ts = pick([]string{"5e-324", "-5e-324", "1e-310", "119.99999999999999"})
				}
				stmts = append(stmts, fmt.Sprintf(`INSERT INTO dns VALUES (%s,%s,%s)`, ts, lat, pick(flags)))
			}
			seriesExec(t, st, stmts...)
			for k := 0; k < 16; k++ {
				since := int64(next(2600)) - 600
				upper := since + int64(next(3000))
				w := []int{1, 2, 3, 7, 14, 57, 60, 61, 403, 1728, -1, -60, 86400}[next(13)]
				ex := excludes[next(uint32(len(excludes)))]
				want, errOld := seriesSQLOracle(ctx, st.db, since, upper, w, ex)
				got, errNew := st.seriesQuery(ctx, time.Unix(since, 0), time.Unix(upper, 0), w, ex)
				if (errOld == nil) != (errNew == nil) {
					t.Fatalf("round %d (%s store) window [%d,%d) width %d exclude %q: the old statement said %v, the scan says %v",
						r, kind, since, upper, w, ex, errOld, errNew)
				}
				if errOld != nil {
					failed++
					continue
				}
				if d := seriesPointsDiff(want, got); d != "" {
					t.Fatalf("round %d (%s store) window [%d,%d) width %d exclude %q: %s", r, kind, since, upper, w, ex, d)
				}
				same++
			}
		}
	}
	if same == 0 || failed == 0 {
		t.Errorf("premise broken: %d windows read and %d failed; the rows must produce both", same, failed)
	}
}

// An open-ended read ends at the present. Rows are placed well clear of the
// horizon on both sides, so a second ticking over between the two reads of the
// clock cannot move a row across it.
func TestSeriesStreamOpenEndMatchesSQLOracle(t *testing.T) {
	ctx := context.Background()
	for kind, st := range seriesStores(t) {
		now := time.Now().Unix()
		seriesExec(t, st,
			fmt.Sprintf(`INSERT INTO samples VALUES (%d,'a',5,1,'ipv4')`, now-600),
			fmt.Sprintf(`INSERT INTO samples VALUES (%d,'a',6,1,'ipv4')`, now-30),
			fmt.Sprintf(`INSERT INTO samples VALUES (%d,'a',1,0,'ipv4')`, now+3600),
			fmt.Sprintf(`INSERT INTO dns VALUES (%d,2.5,1)`, now-30),
			fmt.Sprintf(`INSERT INTO dns VALUES (%d,0.5,1)`, now+3600),
		)
		for _, w := range []int{1, 60, 86400} {
			want, err := seriesSQLOracle(ctx, st.db, now-3600, currentHorizon(now), w, nil)
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.seriesQuery(ctx, time.Unix(now-3600, 0), time.Time{}, w, nil)
			if err != nil {
				t.Fatal(err)
			}
			if d := seriesPointsDiff(want, got); d != "" {
				t.Errorf("%s store, width %d: %s", kind, w, d)
			}
		}
	}
}

// Every statement seriesQuery runs must come off its ts index in order. The
// statements are captured as they are prepared rather than listed here, so one
// added later is checked too. Statistics are built for the second pass because
// a planner that has them weighs a full scan differently.
func TestSeriesStatementsNeverSort(t *testing.T) {
	st, c := countingStore(t)
	ctx := context.Background()
	end := time.Unix(1_788_220_800, 0)
	for ts := end.Add(-time.Hour).Unix(); ts < end.Unix(); ts += 5 {
		seriesExec(t, st,
			fmt.Sprintf(`INSERT INTO samples VALUES (%d,'a',5,1,'ipv4')`, ts),
			fmt.Sprintf(`INSERT INTO dns VALUES (%d,2.5,1)`, ts))
	}
	for _, pass := range []string{"no statistics", "after ANALYZE"} {
		if pass == "after ANALYZE" {
			seriesExec(t, st, `ANALYZE`)
		}
		c.reset()
		if _, err := st.seriesQuery(ctx, end.Add(-time.Hour), end, 60, []string{"x", "y"}); err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		seen := append([]string(nil), c.seen...)
		c.mu.Unlock()
		reads := map[string]int{}
		for _, q := range seen {
			args := make([]any, strings.Count(q, "?"))
			for i := range args {
				args[i] = 0
			}
			plan := queryPlan(t, st, q, args...)
			for _, table := range []string{"samples", "dns"} {
				if !strings.Contains(q, "FROM "+table+" ") {
					continue
				}
				reads[table]++
				if !strings.Contains(plan, "SEARCH "+table+" USING INDEX idx_"+table+"_ts") {
					t.Errorf("%s: the %s scan does not walk its ts index: %s", pass, table, plan)
				}
			}
			if strings.Contains(plan, "TEMP B-TREE") || strings.Contains(plan, "SCAN ") {
				t.Errorf("%s: a series statement sorts or scans a whole table: %s\n%s", pass, plan, q)
			}
		}
		if reads["samples"] != 1 || reads["dns"] != 1 {
			t.Errorf("%s: want one read of samples and one of dns, got %v in %d statements", pass, reads, len(seen))
		}
	}
}

// Both scans must see one commit. The hook writes between them, through another
// connection, so this is file-backed by construction: on the one-connection
// in-memory pool the write would wait for the scan that is waiting for it.
func TestSeriesScansShareOneSnapshot(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "snap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	seriesExec(t, st,
		`INSERT INTO samples VALUES (100,'a',5,1,'ipv4')`,
		`INSERT INTO dns VALUES (100,2.5,1)`)
	want, err := seriesSQLOracle(ctx, st.db, 0, 1000, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrote := false
	seriesScanHook = func(stage string, _ int64) {
		if stage != "dns" {
			return
		}
		seriesExec(t, st,
			`INSERT INTO dns VALUES (101,100.5,1)`,
			`INSERT INTO samples VALUES (300,'a',9,1,'ipv4')`,
			`INSERT INTO dns VALUES (300,7.5,1)`)
		wrote = true
	}
	defer func() { seriesScanHook = nil }()
	got, err := st.seriesQuery(ctx, time.Unix(0, 0), time.Unix(1000, 0), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("premise broken: the hook between the scans never ran")
	}
	if d := seriesPointsDiff(want, got); d != "" {
		t.Errorf("the dns scan saw a commit the samples scan did not: %s", d)
	}
	seriesScanHook = nil
	after, err := st.seriesQuery(ctx, time.Unix(0, 0), time.Unix(1000, 0), 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Errorf("the next scan must see the rows written meanwhile: %d points, want 2", len(after))
	}
}

// A cancelled request stops the scan. At "samples" and at "dnsrows" the
// statement is open and the cancel lands inside its scan, before the first
// row. At "dns" it lands between the two statements, and the second one does
// not open. Counted in rows, not timed: the hook cancels, waits until
// database/sql has closed the rows and taken the connection back, and from
// then on not one more row may be read.
func TestSeriesCancelStopsTheScan(t *testing.T) {
	end := time.Unix(1_788_220_800, 0)
	const span = 2 * time.Hour // 8,640 sample rows and 1,440 dns rows
	file := seedSeriesDB(t, benchStacks()[1], span, end, 5)
	mem := open(t)
	for ts := end.Add(-span).Unix(); ts < end.Unix(); ts += 60 {
		var stmts []string
		for k := 0; k < 60; k += 5 {
			stmts = append(stmts,
				fmt.Sprintf(`INSERT INTO samples VALUES (%d,'a',5,1,'ipv4')`, ts+int64(k)),
				fmt.Sprintf(`INSERT INTO dns VALUES (%d,2.5,1)`, ts+int64(k)))
		}
		seriesExec(t, mem, stmts...)
	}
	for kind, st := range map[string]*Store{"default": mem, "file": file} {
		for i, stage := range []string{"samples", "dns", "dnsrows"} {
			// Its own window end per stage, so each call has its own cache key and
			// none is answered from the scan that followed the one before.
			until := end.Add(-time.Duration(i) * time.Minute)
			ctx, cancel := context.WithCancel(context.Background())
			var atCancel, atEnd int64 = -1, -1
			seriesScanHook = func(s string, rows int64) {
				switch s {
				case stage:
					atCancel = rows
					cancel()
					for wait := time.Now(); st.db.Stats().InUse > 0; time.Sleep(time.Millisecond) {
						if time.Since(wait) > 10*time.Second {
							t.Errorf("%s store, cancelled at %q: the scan still holds its connection", kind, stage)
							return
						}
					}
				case "end":
					atEnd = rows
				}
			}
			pts, err := st.Series(ctx, end.Add(-span), until, 60, nil)
			seriesScanHook = nil
			cancel()
			if atCancel < 0 || atEnd < 0 {
				t.Fatalf("premise broken: %s store, stage %q: the hook never ran (cancel %d, end %d)", kind, stage, atCancel, atEnd)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("%s store, cancelled at %q: err = %v, want context.Canceled", kind, stage, err)
			}
			if pts != nil {
				t.Errorf("%s store, cancelled at %q: returned %d points with the error", kind, stage, len(pts))
			}
			if atEnd != atCancel {
				t.Errorf("%s store, cancelled at %q: read %d more rows after the cancel", kind, stage, atEnd-atCancel)
			}
			// The connection must be back: the in-memory pool has only the one. And
			// the failed scan must not have been cached.
			again, err := st.Series(context.Background(), end.Add(-span), until, 60, nil)
			if err != nil {
				t.Fatalf("%s store: scan after a cancelled one: %v", kind, err)
			}
			if len(again) == 0 {
				t.Errorf("%s store: scan after a cancelled one returned nothing", kind)
			}
			// A scan cut short inside the dns statement has every point and no
			// DNS value. Returned without the error it would be kept, and the
			// chart drawn without its DNS line until the entry ran out.
			withDNS := 0
			for _, p := range again {
				if p.DNSms != nil {
					withDNS++
				}
			}
			if withDNS == 0 {
				t.Errorf("%s store, cancelled at %q: the scan after it has %d points and none with a DNS value", kind, stage, len(again))
			}
		}
		// A request that was gone before the scan began: the read snapshot does not
		// open, nothing is read and the hook has nothing to report.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		ran := ""
		seriesScanHook = func(s string, _ int64) { ran += s + " " }
		pts, err := st.Series(ctx, end.Add(-span), end.Add(-time.Hour), 60, nil)
		seriesScanHook = nil
		if !errors.Is(err, context.Canceled) || pts != nil || ran != "" {
			t.Errorf("%s store, cancelled before the scan: err = %v, %d points, hook ran at %q; want context.Canceled and nothing else", kind, err, len(pts), ran)
		}
		if n := st.db.Stats().InUse; n != 0 {
			t.Errorf("%s store, cancelled before the scan: %d connections still in use", kind, n)
		}
	}
}

// The driver boxes every integer above 255 and every float it hands over: two
// allocations a row, for the timestamp and the latency. That is the floor. A
// family or a target read as text adds two more per row, which is what this
// guards against. The limit of 3 leaves room for a platform that was not
// measured; the text regression came to 3.79.
func TestSeriesScanAllocatesTwicePerRow(t *testing.T) {
	end := time.Unix(1_788_220_800, 0)
	st := seedSeriesDB(t, benchStacks()[1], 2*time.Hour, end, 5)
	const rows = 8640 + 1440
	ctx := context.Background()
	for _, ex := range [][]string{nil, {"google"}} {
		n := testing.AllocsPerRun(3, func() {
			if _, err := st.seriesQuery(ctx, end.Add(-2*time.Hour), end, 60, ex); err != nil {
				t.Fatal(err)
			}
		})
		if per := n / rows; per > 3 {
			t.Errorf("exclude %q: %.0f allocations for %d rows, %.2f a row; want about 2", ex, n, rows, per)
		}
	}
}

// Once seriesQuery holds its read snapshot, every read has to go through it. A
// read on the pool instead waits forever on the in-memory store, whose one
// connection the snapshot holds, and on a file it sees a later commit than the
// rows beside it. The snapshot test catches the statements that exist today;
// this reads store.go and refuses the next one, in seriesQuery and in every
// method of the fold.
func TestSeriesScanNeverReadsThroughThePool(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	// The body ends at the top-level `}`, and comments go, for the reasons
	// TestSpeedFilterCoversEveryMeasurementRead gives.
	decl := regexp.MustCompile(`(?m)^func \((?:s \*Store|a \*seriesAgg)\) ([A-Za-z0-9_]+)\(`)
	closing := regexp.MustCompile(`(?m)^\}$`)
	pool := regexp.MustCompile(`\b(s|st|store)\.db\b`)
	seen := map[string]bool{}
	for _, loc := range decl.FindAllStringSubmatchIndex(string(src), -1) {
		name, onFold := string(src[loc[2]:loc[3]]), strings.HasPrefix(string(src[loc[0]:loc[1]]), "func (a *seriesAgg)")
		if !onFold && name != "seriesQuery" {
			continue
		}
		c := closing.FindStringIndex(string(src[loc[0]:]))
		if c == nil {
			t.Fatalf("%s: no closing brace found", name)
		}
		body := stripLineComments(string(src[loc[0] : loc[0]+c[1]]))
		seen[name] = true
		if m := pool.FindString(body); m != "" {
			t.Errorf("%s names the pool (%s): a chart's reads all go through the scan's read snapshot", name, m)
		}
		if name == "seriesQuery" && !strings.Contains(body, "s.BeginReadSnapshot(ctx)") {
			t.Error("seriesQuery no longer opens a read snapshot, so its two scans can see two commits")
		}
	}
	for _, name := range []string{"seriesQuery", "readSamples", "readDNS"} {
		if !seen[name] {
			t.Errorf("%s was not found in store.go, so this guard checked nothing for it", name)
		}
	}
}
