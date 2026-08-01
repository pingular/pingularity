package store

import (
	"context"
	"math"
	"testing"
	"time"
)

// powerCutFixture builds the history that turns an unobserved gap into phantom
// downtime at prune time: the monitor confirms an outage, the host loses power
// for most of the gap (booked as ONE pause span by the startup-gap accounting in
// Monitor.Run), quorum recovers after the restart, and a later ordinary outage
// closes normally so the leading 'down' is an orphan (down->down) rather than the
// trailing event. Only 200 of the leading outage's 10000 wall seconds were ever
// observed - 100s before the power cut and 100s after the restart.
//
// Being an ORPHAN is a property of this fixture, not a detail: it routes all
// three consumers through orphanGapDowntime. The trailing-down branches they each
// carry separately are a different code path with the same invariant, and are
// covered by trailingPowerCutFixture below - which is why a defect in one of them
// survived this file's first version.
func powerCutFixture(t *testing.T, now time.Time) *Store {
	t.Helper()
	st := open(t)
	sampleAt(t, st, now, 100000, "cf", "ipv4", true) // monitoring anchor
	eventAt(t, st, now, 90000, "down", -1)           // outage confirmed, then the host dies
	pauseAt(t, st, now, 89900, 9800)                 // unobserved [now-89900, now-80100)
	sampleAt(t, st, now, 80000, "cf", "ipv4", true)  // quorum recovery, proven only by samples
	eventAt(t, st, now, 1000, "down", -1)            // an ordinary later outage...
	eventAt(t, st, now, 500, "up", 500)              // ...closed by its own 'up'
	return st
}

// trailingPowerCutFixture is powerCutFixture's other shape, and the one it does
// NOT cover: the same power cut, but with no later outage after it, so the
// dangling 'down' is the NEWEST event. That routes through a different branch in
// every consumer - UptimeSince, DowntimeByDay and ResolvedOutagesSince each have
// a separate trailing-down arm, distinct from the orphan (down->down) path
// orphanGapDowntime serves - and each of those arms has to subtract the pause
// overlap independently. The arithmetic is identical to the orphan fixture: 10000
// wall seconds between the 'down' and the quorum recovery, 9800 of them
// unobserved, so 200s were observed down.
func trailingPowerCutFixture(t *testing.T, now time.Time) *Store {
	t.Helper()
	st := open(t)
	sampleAt(t, st, now, 100000, "cf", "ipv4", true) // monitoring anchor
	eventAt(t, st, now, 90000, "down", -1)           // outage confirmed, then the host dies
	pauseAt(t, st, now, 89900, 9800)                 // unobserved [now-89900, now-80100)
	sampleAt(t, st, now, 80000, "cf", "ipv4", true)  // quorum recovery, proven only by samples
	return st                                        // ...and nothing after it: the 'down' is the newest event
}

// uptimeReadings is what the three outage consumers report over one window: the
// status pill (UptimeSince), the heatmap (DowntimeByDay) and the digest
// (ResolvedOutagesSince). They derive downtime by different routes and must agree.
type uptimeReadings struct {
	ratio, coverage float64
	hmDownS, hmOut  int
	dgOut, dgDownS  int
}

// readAll snapshots all three consumers over [since, now].
func readAll(t *testing.T, st *Store, since time.Time) uptimeReadings {
	t.Helper()
	ctx := context.Background()
	var r uptimeReadings
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	r.ratio, r.coverage = o.Ratio(), o.Coverage()
	rows, err := st.DowntimeByDay(ctx, since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	r.hmDownS, r.hmOut = sumDowntimeDays(rows)
	if r.dgOut, r.dgDownS, err = st.ResolvedOutagesSince(ctx, since.Unix()); err != nil {
		t.Fatalf("ResolvedOutagesSince: %v", err)
	}
	return r
}

// Pruning must be an accounting no-op. resolveDanglingDowns rewrites a dangling
// outage into a completed one so the events log survives the loss of its sample
// evidence, and the synthetic 'up' it writes carries duration_s - which is
// OBSERVED seconds everywhere in this store (Monitor.transition subtracts the
// paused gap before writing a real 'up'; UptimeSince removes pause spans from the
// DENOMINATOR only). Stamping the raw wall gap on it instead books the unobserved
// power-cut stretch as downtime AND removes it from observed time - double-booked,
// and permanently, because the synthetic event is thereafter an ordinary completed
// outage that nothing re-derives.
//
// The bug is visible precisely as cross-component DISAGREEMENT: the uptime pill
// collapses while the heatmap (which reprorates against the pause rows and so
// still reports the honest 200s) does not move. Asserting all three readings are
// unchanged across the prune is the invariant, not the arithmetic.
func TestPruneSyntheticUpDoesNotDoubleBookPausedTime(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := powerCutFixture(t, now)
	since := now.Add(-100000 * time.Second)

	before := readAll(t, st, since)
	// While the samples survive, every consumer books only observed downtime:
	// 200s of the power-cut outage plus the 500s ordinary one, over two outages.
	if before.hmDownS != 700 || before.hmOut != 2 {
		t.Fatalf("fixture: heatmap = %ds over %d outages, want 700s over 2 "+
			"(the 9800s power cut is unobserved, not downtime)", before.hmDownS, before.hmOut)
	}
	if before.dgDownS != 700 || before.dgOut != 2 {
		t.Fatalf("fixture: digest = %ds over %d outages, want 700s over 2", before.dgDownS, before.dgOut)
	}

	// The hourly pruner reaches the sample retention: the recovery evidence goes,
	// so resolveDanglingDowns closes the leading outage first. Pauses and events
	// keep their (much longer) retention.
	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	st.invalidateReadCaches() // cold recCache, as after a restart past the horizon

	after := readAll(t, st, since)
	if math.Abs(after.ratio-before.ratio) > 1e-4 {
		t.Errorf("uptime ratio moved across the prune: %.6f -> %.6f; the synthetic 'up' "+
			"booked the paused gap as downtime (duration_s must be pause-EXCLUDED)",
			before.ratio, after.ratio)
	}
	if math.Abs(after.coverage-before.coverage) > 1e-4 {
		t.Errorf("observation coverage moved across the prune: %.6f -> %.6f", before.coverage, after.coverage)
	}
	if after.hmDownS != before.hmDownS || after.hmOut != before.hmOut {
		t.Errorf("heatmap moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.hmDownS, before.hmOut, after.hmDownS, after.hmOut)
	}
	if after.dgDownS != before.dgDownS || after.dgOut != before.dgOut {
		t.Errorf("digest moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.dgDownS, before.dgOut, after.dgDownS, after.dgOut)
	}
	// And the three still agree with each other afterwards - the point of closing
	// the outage at prune time is that the events log alone reproduces the reading.
	if after.hmDownS != after.dgDownS {
		t.Errorf("after the prune the heatmap (%ds) and digest (%ds) disagree", after.hmDownS, after.dgDownS)
	}
}

// The synthesized closing 'up' itself: its duration_s is the OBSERVED span
// (wall gap minus the pause overlap), the same quantity Monitor.transition writes
// for a real recovery. Asserted directly so a future change that reintroduces the
// raw gap fails here with an unambiguous number, not only through the consumers.
func TestResolveDanglingDownsSyntheticDurationExcludesPause(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := powerCutFixture(t, now)

	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	rows, err := st.db.QueryContext(ctx,
		`SELECT ts, COALESCE(duration_s, -1) FROM events
		 WHERE type = 'up' AND detail = 'recovered while unmonitored' ORDER BY ts`)
	if err != nil {
		t.Fatalf("read synthetic events: %v", err)
	}
	defer rows.Close()
	type ev struct{ ts, dur int64 }
	var synth []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.ts, &e.dur); err != nil {
			t.Fatalf("scan synthetic event: %v", err)
		}
		synth = append(synth, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scan synthetic events: %v", err)
	}
	if len(synth) != 1 {
		t.Fatalf("got %d synthetic 'up' events, want exactly 1 (only the orphaned power-cut outage)", len(synth))
	}
	if want := now.Add(-80000 * time.Second).Unix(); synth[0].ts != want {
		t.Errorf("synthetic 'up' stamped at %d, want %d (the quorum-recovery second)", synth[0].ts, want)
	}
	// 10000s wall gap, 9800s of it an unobserved pause: 200s were observed down.
	if synth[0].dur != 200 {
		t.Errorf("synthetic duration_s = %ds, want 200s (wall gap 10000s minus the 9800s pause); "+
			"raw wall time here is double-booked - counted as downtime and removed from observed time",
			synth[0].dur)
	}
}

// The TRAILING shape of the same power cut - the one the orphan fixture above
// cannot reach, because a later outage there makes the dangling 'down' an orphan
// and routes every consumer through orphanGapDowntime, which already subtracts
// the pause. With nothing after it, the 'down' is the newest event and each
// consumer takes its own trailing-down branch instead; ResolvedOutagesSince's
// branch was adding the RAW wall gap while UptimeSince's and DowntimeByDay's
// subtracted the pause first.
//
// What that shipped: a digest sentence that cannot be true - "Uptime 100.00% ·
// 1 outage · 168h 0m down", fully up and fully down over the same week - and an
// operator who opens the dashboard to investigate finds a spotless heatmap and a
// 100% pill, because those two were right. Asserted as agreement between the
// three consumers, before AND after a prune converts the dangling 'down' into an
// ordinary completed outage.
func TestTrailingDanglingDownReadsTheSameEverywhere(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	st := trailingPowerCutFixture(t, now)
	since := now.Add(-100000 * time.Second)

	// 200 of the outage's 10000 wall seconds were observed; the other 9800 are
	// unobserved time, which is neither up nor down.
	const wantDown = 200
	before := readAll(t, st, since)
	if before.hmDownS != wantDown || before.hmOut != 1 {
		t.Fatalf("fixture: heatmap = %ds over %d outages, want %ds over 1",
			before.hmDownS, before.hmOut, wantDown)
	}
	if before.dgDownS != wantDown || before.dgOut != 1 {
		t.Errorf("digest = %ds over %d outages, want %ds over 1: the trailing-down branch booked "+
			"the unobserved power cut as downtime while the uptime%% printed beside it did not",
			before.dgDownS, before.dgOut, wantDown)
	}
	// The pill agrees: 200s down over the 90200s this window actually observed.
	if implied := impliedDowntime(t, st, since, 100000-9800); abs(implied-wantDown) > 3 {
		t.Errorf("UptimeSince implies %ds of downtime, want ~%ds (and %ds from the digest)",
			implied, wantDown, before.dgDownS)
	}

	// The hourly pruner reaches the sample retention, so the quorum-recovery
	// evidence goes and resolveDanglingDowns closes the outage with a synthetic
	// 'up'. The trailing branch stops firing at that point - the reading must not
	// change when the route to it does.
	if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	st.invalidateReadCaches() // cold recCache, as after a restart past the horizon

	after := readAll(t, st, since)
	if math.Abs(after.ratio-before.ratio) > 1e-4 || math.Abs(after.coverage-before.coverage) > 1e-4 {
		t.Errorf("uptime moved across the prune: ratio %.6f -> %.6f, coverage %.6f -> %.6f",
			before.ratio, after.ratio, before.coverage, after.coverage)
	}
	if after.hmDownS != before.hmDownS || after.hmOut != before.hmOut {
		t.Errorf("heatmap moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.hmDownS, before.hmOut, after.hmDownS, after.hmOut)
	}
	if after.dgDownS != before.dgDownS || after.dgOut != before.dgOut {
		t.Errorf("digest moved across the prune: %ds/%d outages -> %ds/%d outages",
			before.dgDownS, before.dgOut, after.dgDownS, after.dgOut)
	}
	if after.hmDownS != after.dgDownS || after.hmOut != after.dgOut {
		t.Errorf("after the prune the heatmap (%ds/%d) and digest (%ds/%d) disagree",
			after.hmDownS, after.hmOut, after.dgDownS, after.dgOut)
	}
}
