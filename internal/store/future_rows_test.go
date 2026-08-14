package store

import (
	"context"
	"testing"
	"time"
)

// futureRowAhead is how far past the wall clock the fixture's future rows sit:
// far beyond metricsFutureSkew, so no read may mistake them for a slightly-fast
// writer's just-now rows, and far inside pruneFutureSlack, so Prune deliberately
// KEEPS them. That gap is the whole scenario - between an import from a
// clock-ahead machine and every current surface there is nothing but the readers'
// own upper bounds.
const futureRowAhead = time.Hour

// futureEvent is the shape of the events row the fixture stamps an hour ahead.
// It is a parameter and not a constant because nothing bounds an event's ts on
// the way in - eventRowSane vets type and duration, never time - so a clock-ahead
// import lands any of these just as easily, and each one is read by a DIFFERENT
// branch: the orphan-gap scan (down->down), the completed-outage scan ('up' with
// a duration), the newest-event decision (all three).
type futureEvent struct {
	name string
	typ  string
	dur  int // <0 writes NULL duration_s, as InsertEvent does for a 'down'
}

// futureEventShapes is every shape a single future-dated events row can take.
// The truth is identical for all of them - the fixture's outage is one outage of
// 540 observed seconds - so they are asserted against one expectation.
var futureEventShapes = []futureEvent{
	{"up-without-duration", "up", -1},
	{"down", "down", -1},
	{"up-with-duration", "up", 300},
}

// bareFutureUp is the shape used by the surfaces that never read the events log
// (the pills, the charts, the usage bubble): they need SOME future event row for
// the fixture to be one scenario, and any shape would do, so name one instead of
// indexing the table at every call site.
var bareFutureUp = futureEventShapes[0]

// seedFutureRows plants, in every table a "current" read touches, one row the
// clock has already reached and one it has not: the same shape, an hour apart.
// One fixture for every surface on purpose - the bug this guards was not any
// single missing bound but that the bound was applied on some reads and not
// others, so the surfaces have to be asked the SAME question off the SAME data.
//
// The events pair is the sharpest form of it: a 'down' at now-600 that never got
// its closing 'up' is an outage the monitor is still watching, and ev is the
// future row that must not be allowed to describe it. If that row answers "what
// happened most recently?", the ongoing outage vanishes from uptime, the digest
// and the heatmap for a full hour; if a scan that books the same seconds still
// sees it, the outage is counted twice instead.
func seedFutureRows(t *testing.T, st *Store, now time.Time, ev futureEvent) {
	t.Helper()
	ctx := context.Background()
	fut := now.Add(futureRowAhead)

	for _, s := range []struct {
		ts  time.Time
		lat float64
	}{{now.Add(-60 * time.Second), 10}, {fut, 999}} {
		if err := st.InsertSamples(ctx, []Sample{{
			TS: s.ts, Target: "cf", Family: "ipv4", Success: true, LatencyMS: s.lat,
		}}); err != nil {
			t.Fatalf("insert sample at %v: %v", s.ts, err)
		}
		if err := st.InsertDNS(ctx, s.ts, s.lat, true); err != nil {
			t.Fatalf("insert dns at %v: %v", s.ts, err)
		}
	}

	nowBytes, futBytes := int64(1_000), int64(9_000_000)
	if err := st.InsertSpeed(ctx, SpeedSample{
		TS: now.Add(-60 * time.Second).Unix(), DownMbps: 100, Server: "s",
		ISP: "Present ISP", PublicIPv4: "192.0.2.1", DownBytes: &nowBytes,
	}); err != nil {
		t.Fatalf("insert present speed run: %v", err)
	}
	if err := st.InsertSpeed(ctx, SpeedSample{
		TS: fut.Unix(), DownMbps: 900, Server: "s",
		ISP: "Future ISP", PublicIPv4: "198.51.100.1", DownBytes: &futBytes,
	}); err != nil {
		t.Fatalf("insert future speed run: %v", err)
	}

	if err := st.InsertEvent(ctx, now.Add(-600*time.Second), "down", -1, ""); err != nil {
		t.Fatalf("insert down: %v", err)
	}
	if err := st.InsertEvent(ctx, fut, ev.typ, ev.dur, ""); err != nil {
		t.Fatalf("insert future %s: %v", ev.typ, err)
	}
}

// A row the clock has not reached yet must not answer for now on ANY current
// surface: not the connection panel, not the charts, not the usage bubble, not
// the uptime/outage figures.
func TestFutureRowsExcludedFromCurrentReads(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	fut := now.Add(futureRowAhead)

	t.Run("LatestConnInfo", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		sp, err := st.LatestConnInfo(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sp == nil {
			t.Fatal("LatestConnInfo returned nil; the present run records an ISP")
		}
		if sp.ISP != "Present ISP" {
			t.Errorf("LatestConnInfo = %q at ts %d; a run an hour in the future replaced the "+
				"last-known connection context the panel falls back on during an outage", sp.ISP, sp.TS)
		}
	})

	// LatestPerTarget and LatestSpeed carried the bound before the fixture existed,
	// and nothing tested it: both bounds could be deleted with the whole suite still
	// green. They are the two reads the status pills are drawn from, so a future row
	// winning them freezes the visible latency and speed until the clock catches up.
	t.Run("LatestPerTarget", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		// A grace shorter than the future row's lead: its own cutoff is anchored to
		// the newest sample CLAMPED to wall now, so the present row stays inside the
		// window - the upper bound is the only thing that can exclude the future one.
		got, err := st.LatestPerTarget(ctx, 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("LatestPerTarget returned %d targets, want 1", len(got))
		}
		if got[0].TS != now.Add(-60*time.Second).Unix() || got[0].LatencyMS != 10 {
			t.Errorf("LatestPerTarget = %vms at ts %d, want 10ms at %d: a probe result an hour "+
				"ahead is showing as the target's current latency",
				got[0].LatencyMS, got[0].TS, now.Add(-60*time.Second).Unix())
		}
	})

	t.Run("LatestSpeed", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		sp, err := st.LatestSpeed(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sp == nil {
			t.Fatal("LatestSpeed returned nil; the present run is a completed speedtest")
		}
		if sp.DownMbps != 100 {
			t.Errorf("LatestSpeed = %v Mbps at ts %d, want 100: a run an hour in the future is "+
				"pinning the speed pill and the metrics gauge", sp.DownMbps, sp.TS)
		}
	})

	t.Run("Series", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		pts, err := st.Series(ctx, now.Add(-time.Hour), time.Time{}, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pts {
			if p.TS >= fut.Unix() {
				t.Errorf("open-ended Series returned a point at ts %d (now+%v); the latency/DNS "+
					"chart is drawing a reading that has not happened", p.TS, futureRowAhead)
			}
		}
	})

	t.Run("SpeedHistory", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		runs, err := st.SpeedHistory(ctx, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.TS >= fut.Unix() {
				t.Errorf("open-ended SpeedHistory returned a run at ts %d (now+%v); the digest "+
					"reads every run it is handed", r.TS, futureRowAhead)
			}
		}
	})

	t.Run("SpeedHistoryBudget", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		runs, total, err := st.SpeedHistoryBudget(ctx, now.Add(-time.Hour), time.Time{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range runs {
			if r.TS >= fut.Unix() {
				t.Errorf("open-ended SpeedHistoryBudget returned a run at ts %d (now+%v)", r.TS, futureRowAhead)
			}
		}
		if total != 1 {
			t.Errorf("SpeedHistoryBudget total = %d, want 1: the disclosure header must count "+
				"only rows a pass could return", total)
		}
	})

	t.Run("SpeedDataUsageSince", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		b, err := st.SpeedDataUsageSince(ctx, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if b != 1_000 {
			t.Errorf("SpeedDataUsageSince = %d bytes, want 1000: the custom-window data bubble "+
				"billed the user for a transfer an hour in the future", b)
		}
	})

	// The preset windows already carried the bound; keep them honest beside the
	// custom one so the two can never drift apart again.
	t.Run("SpeedDataUsage", func(t *testing.T) {
		st := open(t)
		seedFutureRows(t, st, now, bareFutureUp)
		u, err := st.SpeedDataUsage(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if u.H6 != 1_000 || u.All != 1_000 {
			t.Errorf("SpeedDataUsage 6h=%d all=%d, want 1000 for both", u.H6, u.All)
		}
	})

	// The three outage surfaces are asked together, off each shape of the future
	// row, because they share their scans: uptime, the digest and the heatmap read
	// the same events log through different branches, and a bound applied to one
	// branch and not its neighbours MOVES the disagreement rather than fixing it -
	// bounding only the newest-event decision left the orphan-gap and
	// completed-outage scans still counting the future row, which then booked the
	// same 540 seconds twice on the 'down' and duration-carrying-'up' shapes while
	// the heatmap read them once.
	for _, ev := range futureEventShapes {
		t.Run("outages/"+ev.name, func(t *testing.T) {
			st := open(t)
			seedFutureRows(t, st, now, ev)

			// down at now-600, first quorum recovery at now-60: 540 observed seconds,
			// one outage, whatever the future row claims.
			o, err := st.UptimeSince(ctx, now.Add(-time.Hour), 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := int64(o.Down / time.Second); got != 540 {
				t.Errorf("UptimeSince booked %ds of downtime, want 540: a row stamped an hour ahead "+
					"either hides the outage that is on record or books it a second time, for as "+
					"long as the clock takes to catch up", got)
			}

			count, downS, err := st.ResolvedOutagesSince(ctx, now.Add(-time.Hour).Unix())
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 || downS != 540 {
				t.Errorf("ResolvedOutagesSince = %d outage(s), %ds down; want 1 and 540 - the digest "+
					"prints this beside the uptime%% in one sentence", count, downS)
			}

			days, err := st.DowntimeByDay(ctx, now.Add(-time.Hour), time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			var down, outages int
			for _, d := range days {
				down += d.DowntimeS
				outages += d.Outages
			}
			if down != 540 || outages != 1 {
				t.Errorf("DowntimeByDay = %d outage(s), %ds down; want 1 and 540 (matching UptimeSince "+
					"and ResolvedOutagesSince)", outages, down)
			}
		})
	}
}

// seriesQuery bounds the dns aggregate separately from the samples one, and that
// half is only observable in a bucket wide enough to hold BOTH a present ping row
// and a future dns row. The dns line is LEFT JOINed onto the ping buckets, so at
// chart resolutions the future dns row sits in a bucket with no ping row (the
// samples bound removed its sample) and is dropped by the join - the fixture above
// therefore says nothing about it. Widen the bucket and the same row is averaged
// into the PRESENT bucket's value instead: the point's timestamp is right and its
// DNS reading is a number nothing has measured.
func TestFutureDNSRowExcludedFromWideBucket(t *testing.T) {
	ctx := context.Background()
	st := open(t)
	nowU := time.Now().Unix()

	// Bucket boundaries are absolute ((ts/B)*B), so the two rows share a bucket
	// only if no boundary falls between them - which a fixed width cannot promise
	// at 23:59. Widen until this bucket holds the present row and still has a
	// second left past the horizon for the future one.
	horizon := currentHorizon(nowU)
	bucket := int64(24 * 3600)
	for (nowU/bucket)*bucket+bucket <= horizon+1 {
		bucket *= 2
	}
	start := (nowU / bucket) * bucket
	fut := start + bucket - 1

	if err := st.InsertSamples(ctx, []Sample{{
		TS: time.Unix(nowU, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10,
	}}); err != nil {
		t.Fatalf("insert present sample: %v", err)
	}
	if err := st.InsertDNS(ctx, time.Unix(nowU, 0), 10, true); err != nil {
		t.Fatalf("insert present dns: %v", err)
	}
	if err := st.InsertDNS(ctx, time.Unix(fut, 0), 999, true); err != nil {
		t.Fatalf("insert future dns: %v", err)
	}

	pts, err := st.Series(ctx, time.Unix(start, 0), time.Time{}, int(bucket), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 {
		t.Fatalf("Series returned %d points over one %ds bucket, want 1", len(pts), bucket)
	}
	if pts[0].DNSms == nil {
		t.Fatal("Series dropped the DNS value entirely; the present resolve is in this bucket")
	}
	if *pts[0].DNSms != 10 {
		t.Errorf("Series DNS = %vms, want 10: a resolve stamped at %v (now+%v) was averaged into "+
			"the present bucket", *pts[0].DNSms, time.Unix(fut, 0), time.Duration(fut-nowU)*time.Second)
	}
}

// A caller that ASKS for a future window still gets it. The UI accepts a typed
// absolute range that ends ahead of now (it clamps at now+366d, not now), so the
// upper bound above belongs to the open-ended "up to the present" reads only -
// silently trimming an explicit range would answer a different question than the
// one asked.
func TestExplicitFutureRangeStillReturned(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	fut := now.Add(futureRowAhead)
	until := fut.Add(time.Hour)

	st := open(t)
	seedFutureRows(t, st, now, bareFutureUp)

	pts, err := st.Series(ctx, now.Add(-time.Hour), until, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var seriesTS []int64
	for _, p := range pts {
		seriesTS = append(seriesTS, p.TS)
	}
	if !hasTS(seriesTS, fut.Unix()) {
		t.Errorf("Series over an explicit range ending at %v dropped the point at %v", until, fut)
	}

	runs, err := st.SpeedHistoryRange(ctx, now.Add(-time.Hour), until, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTS(tsOfSamples(runs), fut.Unix()) {
		t.Errorf("SpeedHistoryRange over an explicit range ending at %v dropped the run at %v", until, fut)
	}

	budget, total, err := st.SpeedHistoryBudget(ctx, now.Add(-time.Hour), until, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !hasTS(tsOfSamples(budget), fut.Unix()) || total != 2 {
		t.Errorf("SpeedHistoryBudget over an explicit range returned %d of %d runs; both belong to it",
			len(budget), total)
	}
}
