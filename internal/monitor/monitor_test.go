package monitor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// newTestMonitor builds a Monitor backed by an in-memory database. Logs are
// discarded; newTestMonitorLog captures them.
func newTestMonitor(t *testing.T, downAfter, upAfter int) (*Monitor, *store.Store) {
	return newTestMonitorLog(t, downAfter, upAfter, io.Discard)
}

func newTestMonitorLog(t *testing.T, downAfter, upAfter int, logDst io.Writer) (*Monitor, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Config{DownAfter: downAfter, UpAfter: upAfter}
	log := slog.New(slog.NewTextHandler(logDst, nil))
	m := New(cfg, nil, st, log)
	m.since = time.Unix(0, 0)
	return m, st
}

func feed(m *Monitor, online bool, ts time.Time) {
	m.advance(context.Background(), prober.Result{TS: ts, Online: online})
}

func eventCount(t *testing.T, st *store.Store, typ string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM events WHERE type = ?`, typ).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", typ, err)
	}
	return n
}

// A single failed round must NOT trip a DOWN when DownAfter is 2 (debounce).
func TestDebounceSuppressesFlap(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	feed(m, false, time.Unix(5, 0)) // one bad round
	if m.online != true {
		t.Fatal("flipped to DOWN after a single failure; debounce failed")
	}
	feed(m, true, time.Unix(8, 0)) // recovered before threshold
	if got := eventCount(t, st, "down"); got != 0 {
		t.Fatalf("expected no down events, got %d", got)
	}
	// The suppressed flap is exactly what the blip counters must capture.
	s := stats.Lifetime()
	if got := s.Counters["monitor.bad_rounds"]; got != 1 {
		t.Errorf("monitor.bad_rounds = %d, want 1", got)
	}
	if got := s.Counters["monitor.blips"]; got != 1 {
		t.Errorf("monitor.blips = %d, want 1", got)
	}
	if got := s.Gauges["monitor.blip_streak_max"]; got != 1 {
		t.Errorf("monitor.blip_streak_max = %d, want 1", got)
	}
}

// Two consecutive failures trip DOWN; recovery records an UP with the outage
// duration.
func TestDownThenUpRecordsDuration(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	feed(m, false, time.Unix(10, 0)) // bad 1
	feed(m, false, time.Unix(20, 0)) // bad 2 -> DOWN at ts=20 (since was 0)
	if m.online {
		t.Fatal("expected DOWN after two failures")
	}
	if got := eventCount(t, st, "down"); got != 1 {
		t.Fatalf("expected 1 down event, got %d", got)
	}

	feed(m, true, time.Unix(95, 0)) // recover -> UP, outage = 95-20 = 75s
	if !m.online {
		t.Fatal("expected UP after recovery")
	}
	var dur int
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up'`).Scan(&dur); err != nil {
		t.Fatalf("read up duration: %v", err)
	}
	if dur != 75 {
		t.Fatalf("expected outage duration 75s, got %d", dur)
	}
	// The failures became a confirmed outage, so the recovery is not a blip.
	s := stats.Lifetime()
	if got := s.Counters["monitor.blips"]; got != 0 {
		t.Errorf("monitor.blips = %d, want 0 (confirmed outage)", got)
	}
	if got := s.Counters["monitor.bad_rounds"]; got != 2 {
		t.Errorf("monitor.bad_rounds = %d, want 2", got)
	}
	// Confirmed-transition accounting: one down, 75s of outage at recovery.
	if got := s.Counters["monitor.downs"]; got != 1 {
		t.Errorf("monitor.downs = %d, want 1", got)
	}
	if got := s.Floats["monitor.outage_s_sum"]; got != 75 {
		t.Errorf("monitor.outage_s_sum = %v, want 75", got)
	}
}

// A backward WALL-clock step during an outage (NTP correcting a fast RTC) must
// not corrupt either half of the recovery: the recorded outage duration still
// comes from the MONOTONIC clock (which never steps back), while the persisted
// event timestamp is clamped nondecreasing so an 'up' can never be ordered before
// the 'down' it closes (ORDER BY ts DESC would otherwise read the 'down' as newest
// and book a phantom outage).
//
// Go couples a time.Time's wall and monotonic readings: Add moves both together
// and there is no wall-only setter, so no single time.Time can have a forward
// monotonic reading AND a backward wall reading - exactly the decoupling a real
// NTP step produces. We therefore exercise the two guarantees in two sub-cases:
// (A) real time.Now()-derived values (monotonic preserved) prove the duration is
// the TRUE elapsed, not zero; (B) wall-only values - the only way a unit test can
// present a backward wall to a clamp that decides on .Unix() - prove the stored
// ordering holds. The old test conflated these, feeding monotonic-free values a
// production round never produces and asserting the duration must be zero.
func TestBackwardClockStepClampsRecovery(t *testing.T) {
	// (A) Monotonic preserved: the true outage length survives even as the wall
	// clock is (conceptually) yanked back, because ts.Sub uses the monotonic clock.
	stats.ResetForTest()
	m, st := newTestMonitor(t, 1, 1)

	down := time.Now()               // real, monotonic-carrying
	up := down.Add(75 * time.Second) // 75 monotonic seconds of outage, wall+75s too
	feed(m, false, down)             // DOWN
	if m.online {
		t.Fatal("expected DOWN after the failing round")
	}
	feed(m, true, up) // recovery 75s later by the monotonic clock
	if !m.online {
		t.Fatal("expected UP after recovery")
	}

	var downTSa, upTSa, dura int
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTSa); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTSa, &dura); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if upTSa < downTSa {
		t.Fatalf("up ts=%d stamped before down ts=%d", upTSa, downTSa)
	}
	if dura != 75 {
		t.Fatalf("outage duration = %d, want 75 (true monotonic elapsed, not zero)", dura)
	}
	if got := stats.Lifetime().Floats["monitor.outage_s_sum"]; got != 75 {
		t.Fatalf("monitor.outage_s_sum = %v, want 75", got)
	}

	// (B) Backward WALL step reaching the persistence clamp: the recovery's wall
	// second reads an hour BEFORE the down's. The clamp (which compares .Unix(), so
	// it fires regardless of monotonic presence, unlike the old .Before() check)
	// pins the stored up ts to the down's second, never earlier. With no monotonic
	// reading the elapsed goes negative and is clamped to 0 defensively.
	stats.ResetForTest()
	m2, st2 := newTestMonitor(t, 1, 1)
	feed(m2, false, time.Unix(3600, 0)) // DOWN stamped at wall second 3600
	if m2.online {
		t.Fatal("expected DOWN after the failing round")
	}
	feed(m2, true, time.Unix(60, 0)) // wall stepped back an hour, then recovered
	if !m2.online {
		t.Fatal("expected UP after recovery")
	}

	var downTSb, upTSb, durb int
	if err := st2.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTSb); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st2.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTSb, &durb); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if upTSb < downTSb {
		t.Fatalf("up ts=%d stamped before down ts=%d; ORDER BY ts DESC would read the down as newest", upTSb, downTSb)
	}
	if durb != 0 {
		t.Fatalf("outage duration = %d, want 0 (monotonic-free backward step clamped)", durb)
	}
	if got := stats.Lifetime().Floats["monitor.outage_s_sum"]; got != 0 {
		t.Fatalf("monitor.outage_s_sum = %v, want 0 (never negative)", got)
	}
}

// The clamp above keeps the ORDER intact, but ordering alone is not enough:
// when the wall steps back by at least the outage's length, pinning the 'up' to
// the down's second stores a ZERO-WIDTH pair whose duration_s still carries
// the true monotonic elapsed. The observed-span model reads the pair as the
// outage's wall interval (completedOutagesSince), and observedOutageSpans
// yields nothing for a zero-width one - so uptime and the digest booked ZERO
// downtime for an outage the digest still counted and the outage table still
// showed at its real length, while the heatmap's fallback booked all of it.
// The stored pair must be at least as wide as the duration it claims.
func TestBackwardStepCannotZeroAnOutageOutOfDowntime(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	// A monitoring anchor an hour before the outage, so UptimeSince has an
	// observed window to score.
	if err := st.InsertSamples(ctx, []store.Sample{{
		TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	feed(m, false, down) // DOWN, stamped at wall second `down`
	if m.online {
		t.Fatal("expected DOWN after the failing round")
	}
	// Mid-outage the wall steps back a minute while two monotonic minutes pass.
	// No synthesized time.Time can carry that decoupling (Add moves both
	// readings), so fabricate the halves separately, exactly as production leaves
	// them: m.since anchors the MONOTONIC measurement - move it so ts.Sub(m.since)
	// reads the true 120s elapsed - while the recovery's WALL second lands before
	// the down's, which is what reaches the .Unix() clamp.
	m.mu.Lock()
	m.since = down.Add(-3 * time.Minute)
	m.mu.Unlock()
	feed(m, true, down.Add(-time.Minute))
	if !m.online {
		t.Fatal("expected UP after recovery")
	}

	var downTS, upTS, dur int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if dur != 120 {
		t.Fatalf("outage duration = %d, want 120 (the monotonic elapsed)", dur)
	}
	if upTS-downTS < dur {
		t.Errorf("stored pair is %ds wide for a %ds outage; a zero-width pair yields no observed "+
			"spans, so the downtime vanishes from uptime and the digest while the outage table "+
			"still shows it", upTS-downTS, dur)
	}
	// And the figure every surface derives: the outage must be booked, not zeroed.
	o, err := st.UptimeSince(ctx, down.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != 120*time.Second {
		t.Errorf("UptimeSince booked %v of downtime for the 120s outage the events table records; "+
			"a backward wall step must not delete an outage from the headline figure", o.Down)
	}
}

// The widen above stamps a recovery ahead of a stepped-back wall clock, so for
// the whole catch-up window the stored 'up' second sits in the wall FUTURE. Any
// NEW outage confirmed during that window has its 'down' clamped nondecreasing
// to exactly that second - and completedOutagesSince breaks ts ties
// down-before-up, so a tied 'down' slots between the widened 'up' and its own
// 'down' and steals the pairing: the first outage collapses to the zero-width
// shape the widen exists to eliminate, one event later. A 'down' must land
// strictly after the 'up' it follows.
func TestOutageDuringCatchUpDoesNotCollapseTheWidenedPair(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	// A monitoring anchor for the observed window, and a real quorum-up second in
	// the up-stretch between the outages: without it the mispaired first outage
	// is partly re-booked by the orphan down->down heuristic and the loss hides.
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
		{TS: down.Add(35 * time.Second), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
	}); err != nil {
		t.Fatalf("insert samples: %v", err)
	}

	feed(m, false, down) // outage 1 DOWN at wall `down`
	// The wall steps back mid-outage: 120 monotonic seconds elapse while the
	// wall advances 30. Decouple the halves as production leaves them (see the
	// test above): m.since anchors the monotonic measurement, the fed ts carries
	// the wall second.
	m.mu.Lock()
	m.since = down.Add(30*time.Second - 120*time.Second)
	m.mu.Unlock()
	feed(m, true, down.Add(30*time.Second)) // recovery: widened to down+120, ahead of the wall
	// Outage 2 is confirmed while the wall (down+90) is still behind the widened
	// recovery (down+120), and closes cleanly 60 monotonic seconds later.
	feed(m, false, down.Add(90*time.Second))
	feed(m, true, down.Add(150*time.Second))

	var up1, down2 int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='up' ORDER BY ts LIMIT 1`).Scan(&up1); err != nil {
		t.Fatalf("read first up ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down' ORDER BY ts DESC LIMIT 1`).Scan(&down2); err != nil {
		t.Fatalf("read second down ts: %v", err)
	}
	if down2 <= up1 {
		t.Errorf("second outage's down ts=%d does not clear the widened up ts=%d; the tie sorts "+
			"down-before-up and steals the first outage's pairing", down2, up1)
	}
	// The figure the pairing feeds: both outages' observed downtime, 120s + 60s.
	o, err := st.UptimeSince(ctx, down.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := 180 * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v of downtime, want %v: a down tied to the widened up "+
			"collapses the first outage to zero width", o.Down, want)
	}
}

// The tie-breaking above rests on m.lastEventWall, which used to exist only in
// memory: a RESTART inside the catch-up window came up with the guard empty, the
// IsZero check short-circuited the clamp, and the next outage's 'down' was
// stored at or before the on-disk widened 'up' - recreating the very
// same-second tie/overlap the previous process's guard had just eliminated. Run
// must seed the guard from the newest stored event before its first round can
// transition. (LastObservedTS cannot serve as the source: it caps at wall now,
// and a future-dated 'up' - the one case that needs the guard - is precisely
// what it excludes.)
func TestWidenGuardSurvivesARestart(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	// The same anchors as the test above: an observed window, and a real
	// quorum-up second so a mispaired first outage can't hide behind the orphan
	// down->down heuristic.
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
		{TS: down.Add(35 * time.Second), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
	}); err != nil {
		t.Fatalf("insert samples: %v", err)
	}

	feed(m, false, down) // outage 1 DOWN at wall `down`
	// The wall steps back mid-outage (see the tests above for the fabrication):
	// the recovery is widened to down+120, ahead of the stepped-back wall.
	m.mu.Lock()
	m.since = down.Add(30*time.Second - 120*time.Second)
	m.mu.Unlock()
	feed(m, true, down.Add(30*time.Second))

	// The process restarts while the wall (down+90) is still behind the widened
	// 'up' (down+120): a fresh Monitor over the SAME store, started the way
	// production starts one - through Run, whose startup must seed the guard.
	// Probing is gated off so the harness's loop measures nothing on its own and
	// the outage below can be fed deterministically once Run has exited.
	clk := newWallClock(down.Add(90 * time.Second))
	swapNow(t, clk)
	buf := &syncBuf{}
	m2 := New(config.Config{DownAfter: 1, UpAfter: 1}, prober.New(nil, time.Second), st,
		slog.New(slog.NewTextHandler(buf, nil)))
	m2.DNSFn = func() bool { return false }
	m2.LatencyFn = func() bool { return false }
	m2.IntervalFn = func() time.Duration { return time.Hour }
	_, stop := startLoop(t, m2)
	// The paused line is logged by the first loop iteration, which startup
	// strictly precedes: once it appears, the seeding has happened (or never will).
	waitFor(t, "the restarted loop to come up", func() bool {
		return buf.has("monitor recording paused")
	})
	stop()

	// Outage 2 is confirmed during the catch-up window and closes 60 monotonic
	// seconds later.
	feed(m2, false, down.Add(90*time.Second))
	feed(m2, true, down.Add(150*time.Second))

	var up1, down2 int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='up' ORDER BY ts LIMIT 1`).Scan(&up1); err != nil {
		t.Fatalf("read first up ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down' ORDER BY ts DESC LIMIT 1`).Scan(&down2); err != nil {
		t.Fatalf("read second down ts: %v", err)
	}
	if down2 <= up1 {
		t.Errorf("after a restart, the second outage's down ts=%d does not clear the widened up ts=%d; "+
			"the guard did not survive the process boundary", down2, up1)
	}
	o, err := st.UptimeSince(ctx, down.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := 180 * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v of downtime, want %v: a down at or before the widened up "+
			"collapses the first outage exactly as it did before f133282, one restart later", o.Down, want)
	}
}

// The seed above is only ever a RECOVERY of this process's own guard, so its
// source must be a row the system still believes in. An event stamped years
// ahead of the wall is not: Prune deletes events beyond now+48h on its hourly
// pass, so seeding from one clamps every genuine transition that follows into
// the condemned zone - the outage is stored in the far future, erased by the
// next prune, and because the guard lives in memory it survives that deletion
// and drags the transition after it out there too. Nothing bounds an event's ts
// on the way in (eventRowSane vets type and duration only, InsertEvent not at
// all), so a crafted backup or a boot clock years fast both reach the seed.
func TestAFutureDatedEventDoesNotSeedTheGuard(t *testing.T) {
	_, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	// The poisoned row: an 'up' a year out, arriving by the door with no bound.
	if err := st.InsertEvent(ctx, now.AddDate(1, 0, 0), "up", 5, ""); err != nil {
		t.Fatalf("insert future event: %v", err)
	}
	// An observed window, and a real quorum-up second after the recovery so the
	// pairing can't hide behind the orphan down->down heuristic.
	if err := st.InsertSamples(ctx, []store.Sample{
		{TS: now.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
		{TS: now.Add(65 * time.Second), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12},
	}); err != nil {
		t.Fatalf("insert samples: %v", err)
	}

	// Started the way production starts one - through Run, whose startup seeds -
	// with probing gated off so the outage below is fed deterministically once
	// Run has stopped.
	clk := newWallClock(now)
	swapNow(t, clk)
	buf := &syncBuf{}
	m := New(config.Config{DownAfter: 1, UpAfter: 1}, prober.New(nil, time.Second), st,
		slog.New(slog.NewTextHandler(buf, nil)))
	m.DNSFn = func() bool { return false }
	m.LatencyFn = func() bool { return false }
	m.IntervalFn = func() time.Duration { return time.Hour }
	_, stop := startLoop(t, m)
	waitFor(t, "the loop to come up", func() bool { return buf.has("monitor recording paused") })
	stop()

	// A real 60-second outage, entirely in the present.
	feed(m, false, now)
	feed(m, true, now.Add(60*time.Second))

	var downTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if downTS != now.Unix() {
		t.Errorf("the outage's down was stored at ts=%d, want %d (wall now): a future-dated event "+
			"seeded the guard and clamped a real transition %v ahead of the clock that saw it",
			downTS, now.Unix(), time.Duration(downTS-now.Unix())*time.Second)
	}
	o, err := st.UptimeSince(ctx, now.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := 60 * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v of downtime, want %v: the outage was stamped outside every "+
			"window it belongs to (and beyond the horizon the pruner will erase)", o.Down, want)
	}
}

// The widen must stretch the pair to down + ELAPSED (the raw monotonic wall
// window), not down + duration: duration already has the outage's pause fold
// subtracted, and observedOutageSpans subtracts the stored pause rows from the
// stored pair AGAIN before trimming to duration_s. A pair widened only to the
// duration therefore books duration-minus-pause on uptime, the digest and the
// heatmap while duration_s claims the full length - the cross-surface
// disagreement the one-interval model exists to prevent.
func TestWidenedPairIsWideEnoughForTheReadModelsPauseSubtraction(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := st.InsertSamples(ctx, []store.Sample{{
		TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	feed(m, false, down) // DOWN at wall `down`
	// A 20-minute monitoring pause inside the outage, recorded the way Run does:
	// the row persisted for the read model, the fold accumulated for transition.
	pauseStart := down.Add(10 * time.Minute)
	m.notePause(pauseStart)
	if stored, err := st.InsertPause(ctx, pauseStart, 1200); err != nil || !stored {
		t.Fatalf("insert pause: stored=%v err=%v", stored, err)
	}
	m.noteResume(pauseStart.Add(20 * time.Minute))
	// The wall steps back 2400s across a 3600s-monotonic outage: the recovery's
	// wall second reads down+1200 while m.since anchors the true hour elapsed.
	m.mu.Lock()
	m.since = down.Add(1200*time.Second - 3600*time.Second)
	m.mu.Unlock()
	feed(m, true, down.Add(1200*time.Second))
	if !m.online {
		t.Fatal("expected UP after recovery")
	}

	var dur int64
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up'`).Scan(&dur); err != nil {
		t.Fatalf("read up duration: %v", err)
	}
	if dur != 2400 {
		t.Fatalf("outage duration = %d, want 2400 (3600s elapsed minus the 1200s pause fold)", dur)
	}
	// The invariant: what the read model books must equal what duration_s claims.
	o, err := st.UptimeSince(ctx, down.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := time.Duration(dur) * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v for an outage whose duration_s says %v: the pair was "+
			"widened into an interval the pause subtraction shrinks below its own claim", o.Down, want)
	}
}

// Streaks accumulated before a monitoring pause must not survive it: a single
// post-resume failure must not combine with pre-pause failures to confirm a
// transition (Run resets the streaks whenever it skips a round while paused).
func TestPauseResetsDebounceStreaks(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	feed(m, false, time.Unix(10, 0)) // bad 1 of 2 before the pause
	m.advanceFamily(prober.FamilyResult{Family: "ipv4", Online: false}, time.Unix(10, 0))

	m.resetStreaks() // what Run does when a round is skipped while paused

	if fs := m.fams["ipv4"]; fs.badStreak != 0 || fs.okStreak != 0 {
		t.Fatalf("family streaks survived the pause: %+v", fs)
	}
	feed(m, false, time.Unix(1000, 0)) // first bad round after resume
	if !m.online {
		t.Fatal("single post-resume failure flipped DOWN; streak survived the pause")
	}
	feed(m, false, time.Unix(1010, 0)) // second consecutive failure confirms
	if m.online {
		t.Fatal("expected DOWN after two consecutive post-resume failures")
	}
	if got := eventCount(t, st, "down"); got != 1 {
		t.Fatalf("expected 1 down event, got %d", got)
	}
	// The pause-path streak reset must not register as a blip (no recovery happened).
	if got := stats.Lifetime().Counters["monitor.blips"]; got != 0 {
		t.Errorf("monitor.blips = %d, want 0 (pause is not a recovery)", got)
	}
}

// An outage that spans a monitoring pause is recorded only up to the pause
// start: the link may have recovered while nobody watched, so paused time
// must not count as confirmed downtime.
func TestPauseCapsOutageDuration(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)

	m.notePause(time.Unix(5, 0)) // paused while up: no mark to consume
	if !m.downPausedAt.IsZero() {
		t.Fatal("pause while online must not set the outage cap")
	}

	feed(m, false, time.Unix(10, 0)) // bad 1
	feed(m, false, time.Unix(20, 0)) // bad 2 -> DOWN at ts=20
	if m.online {
		t.Fatal("expected DOWN after two failures")
	}

	// Paused at ts=50 (what Run does on the running->paused edge), resumed at
	// ts=490 (Run's resume edge calls noteResume), and the link recovered
	// unwatched during the pause: the first post-resume round at ts=500 is ok.
	m.notePause(time.Unix(50, 0))
	m.resetStreaks()
	m.noteResume(time.Unix(490, 0)) // 440s of unwatched pause folded out

	feed(m, true, time.Unix(500, 0)) // first post-resume ok round -> UP
	if !m.online {
		t.Fatal("expected UP after recovery")
	}
	var dur int
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up'`).Scan(&dur); err != nil {
		t.Fatalf("read up duration: %v", err)
	}
	if dur != 40 { // observed: 30 pre-pause (20->50) + 10 post-resume (490->500); not 480
		t.Fatalf("outage duration = %ds, want 40 (observed only, pause excluded)", dur)
	}

	// The mark is consumed: a later, fully-watched outage records its true length.
	feed(m, false, time.Unix(600, 0))
	feed(m, false, time.Unix(610, 0)) // DOWN at ts=610
	feed(m, true, time.Unix(700, 0))  // UP, 90s
	var dur2 int
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up' ORDER BY ts DESC LIMIT 1`).Scan(&dur2); err != nil {
		t.Fatalf("read second up duration: %v", err)
	}
	if dur2 != 90 {
		t.Fatalf("second outage duration = %ds, want 90 (stale pause mark leaked)", dur2)
	}
}

// A pause during an outage that is STILL down after monitoring resumes must
// count the observed downtime on both sides of the pause, excluding only the
// unwatched gap (regression: the earlier cap discarded post-resume downtime).
func TestPauseExcludesOnlyUnwatchedGap(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)

	feed(m, false, time.Unix(10, 0))
	feed(m, false, time.Unix(20, 0)) // DOWN at ts=20

	m.notePause(time.Unix(50, 0)) // pause begins 30s into the outage
	m.resetStreaks()
	m.noteResume(time.Unix(300, 0)) // 250s unwatched pause

	feed(m, false, time.Unix(310, 0)) // still down after resume
	feed(m, false, time.Unix(360, 0)) // still down
	feed(m, true, time.Unix(400, 0))  // recovers at ts=400
	if !m.online {
		t.Fatal("expected UP after recovery")
	}
	var dur int
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up'`).Scan(&dur); err != nil {
		t.Fatalf("read up duration: %v", err)
	}
	// observed: 30 pre-pause (20->50) + 100 post-resume (300->400) = 130; not 380 (uncapped) or 30 (over-capped)
	if dur != 130 {
		t.Fatalf("outage duration = %ds, want 130 (pre-pause + post-resume observed)", dur)
	}
}

// famResult builds a one-round prober result for the given online families.
func famResult(ts time.Time, fams ...string) prober.Result {
	res := prober.Result{TS: ts, Online: true, Families: map[string]prober.FamilyResult{}}
	for _, f := range fams {
		res.Families[f] = prober.FamilyResult{Family: f, Online: true, OK: 3, Total: 3}
	}
	return res
}

// feedFamilies pushes one round's family results through the monitor the same
// way round() does (advance each family, then record which were probed).
func feedFamilies(m *Monitor, res prober.Result) {
	for _, fr := range res.Families {
		m.advanceFamily(fr, res.TS)
	}
	m.noteFamilies(res)
}

// A family that starts being probed mid-run (IPv6 toggled on live) must appear
// in the snapshot, and disappear again when it stops being probed.
func TestFamilyAppearsAndDisappearsLive(t *testing.T) {
	m, _ := newTestMonitor(t, 2, 1) // no targets -> no seeded families

	feedFamilies(m, famResult(time.Unix(100, 0), "ipv4"))
	st := m.Snapshot()
	if len(st.Families) != 1 || st.Families[0].Family != "ipv4" {
		t.Fatalf("expected only ipv4, got %+v", st.Families)
	}

	// IPv6 toggled on: it must appear, optimistically online, ordered after v4.
	feedFamilies(m, famResult(time.Unix(105, 0), "ipv4", "ipv6"))
	st = m.Snapshot()
	if len(st.Families) != 2 || st.Families[0].Family != "ipv4" || st.Families[1].Family != "ipv6" {
		t.Fatalf("expected [ipv4 ipv6], got %+v", st.Families)
	}
	if !st.Families[1].Online {
		t.Fatal("a newly-probed family should start online")
	}

	// IPv6 toggled off: it must vanish rather than freeze at its last state.
	feedFamilies(m, famResult(time.Unix(110, 0), "ipv4"))
	st = m.Snapshot()
	if len(st.Families) != 1 || st.Families[0].Family != "ipv4" {
		t.Fatalf("expected ipv6 to disappear, got %+v", st.Families)
	}
}

// A family's streaks must not survive it being unprobed: a bad round before
// IPv6 is toggled off can't combine with a bad round after it returns (hours
// later) to confirm a flip.
func TestUnprobedFamilyLosesStreaks(t *testing.T) {
	m, _ := newTestMonitor(t, 2, 1)

	bad6 := func(ts time.Time) prober.Result {
		return prober.Result{TS: ts, Online: true, Families: map[string]prober.FamilyResult{
			"ipv4": {Family: "ipv4", Online: true},
			"ipv6": {Family: "ipv6", Online: false},
		}}
	}
	feedFamilies(m, bad6(time.Unix(10, 0)))              // ipv6 bad 1/2
	feedFamilies(m, famResult(time.Unix(20, 0), "ipv4")) // ipv6 unprobed: streaks reset
	feedFamilies(m, bad6(time.Unix(30, 0)))              // back on, bad: only 1/2 again
	if !m.fams["ipv6"].online {
		t.Fatal("stale pre-gap round combined with a post-gap round to confirm ipv6 down")
	}
	feedFamilies(m, bad6(time.Unix(40, 0))) // second consecutive probed bad round
	if m.fams["ipv6"].online {
		t.Fatal("expected ipv6 down after two consecutive probed bad rounds")
	}
}

// DNSactive reports whether a LIVE DNS reading is available (probing running,
// DNS sub-toggle on, and at least one result seen), so consumers can tell
// "resolver down" from "probe off" AND from "no result yet".
func TestSnapshotDNSActive(t *testing.T) {
	m, _ := newTestMonitor(t, 2, 1)
	// All gates on but no result yet: absent, not a fake resolver-down.
	if m.Snapshot().DNSactive {
		t.Error("DNSactive must be false before the first DNS result")
	}
	m.mu.Lock()
	m.dnsMS, m.dnsOK, m.dnsSeen = 12, true, true
	m.mu.Unlock()
	if !m.Snapshot().DNSactive {
		t.Error("DNSactive should be true when every gate is on and a reading exists")
	}
	m.DNSFn = func() bool { return false }
	if m.Snapshot().DNSactive {
		t.Error("DNSactive must be false when the DNS sub-toggle is off")
	}
	m.DNSFn = nil
	m.LatencyFn = func() bool { return false }
	if m.Snapshot().DNSactive {
		t.Error("DNSactive must be false when latency probing is off")
	}
	m.LatencyFn = nil
	m.EnabledFn = func() bool { return false }
	if m.Snapshot().DNSactive {
		t.Error("DNSactive must be false when monitoring is paused")
	}
}

// A v6-only outage across dual-stack rounds must accumulate v6_only_down_s,
// and each confirmed per-family transition must bump that family's flap counter.
func TestFamilyFlapAndAsymmetryCounters(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	m.IntervalFn = func() time.Duration { return 10 * time.Second }

	mixed := func(ts time.Time, v4, v6 bool) prober.Result {
		return prober.Result{TS: ts, Online: v4 || v6, Families: map[string]prober.FamilyResult{
			"ipv4": {Family: "ipv4", Online: v4},
			"ipv6": {Family: "ipv6", Online: v6},
		}}
	}
	feedFamilies(m, mixed(time.Unix(10, 0), true, false)) // v6 bad 1/2: asymmetric
	feedFamilies(m, mixed(time.Unix(20, 0), true, false)) // v6 bad 2/2: confirmed down
	feedFamilies(m, mixed(time.Unix(30, 0), true, true))  // v6 ok 1/1: confirmed up

	s := stats.Lifetime()
	if got := s.Counters["monitor.flap.ipv6"]; got != 2 {
		t.Errorf("monitor.flap.ipv6 = %d, want 2 (down + up)", got)
	}
	if got := s.Counters["monitor.flap.ipv4"]; got != 0 {
		t.Errorf("monitor.flap.ipv4 = %d, want 0", got)
	}
	if got := s.Floats["monitor.v6_only_down_s"]; got != 20 {
		t.Errorf("monitor.v6_only_down_s = %v, want 20 (2 asymmetric rounds × 10s)", got)
	}
	if got := s.Floats["monitor.v4_only_down_s"]; got != 0 {
		t.Errorf("monitor.v4_only_down_s = %v, want 0", got)
	}
}

// A paused monitor must count exactly one pause episode and accumulate paused
// wall-time at the recheck cadence.
func TestPauseCounters(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	m.EnabledFn = func() bool { return false }
	m.IntervalFn = func() time.Duration { return time.Millisecond }
	// A fresh wake channel swapped+closed rapidly: every settings broadcast
	// re-enters the paused branch, and each visit must only accrue the time
	// that truly passed - the old code pre-credited a full 30s per visit, so a
	// broadcast storm inflated paused_s by orders of magnitude.
	var wmu sync.Mutex
	wake := make(chan struct{})
	m.WakeFn = func() <-chan struct{} { wmu.Lock(); defer wmu.Unlock(); return wake }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	start := time.Now()
	for i := 0; i < 50; i++ {
		wmu.Lock()
		old := wake
		wake = make(chan struct{})
		close(old)
		wmu.Unlock()
		time.Sleep(time.Millisecond)
	}
	elapsed := time.Since(start)
	cancel()
	<-done

	s := stats.Lifetime()
	if got := s.Counters["monitor.pauses"]; got != 1 {
		t.Fatalf("monitor.pauses = %d, want 1 (one episode)", got)
	}
	if got := s.Floats["monitor.paused_s"]; got > elapsed.Seconds()+1 {
		t.Fatalf("monitor.paused_s = %v after only %v of real pause - wake storm inflated it", got, elapsed)
	}
	// Lower bound: a regression accruing ZERO paused time would pass the upper
	// bound alone, defeating the counter's purpose.
	if got := s.Floats["monitor.paused_s"]; got <= 0 {
		t.Fatalf("monitor.paused_s = %v, want > 0 (paused time must accrue)", got)
	}
}

// swapResolveTime stubs the package-level resolveTime seam for one test.
func swapResolveTime(t *testing.T, fn func(context.Context) (time.Duration, bool, error)) {
	t.Helper()
	old := resolveTime
	resolveTime = fn
	t.Cleanup(func() { resolveTime = old })
}

// swapInsertEvent stubs the package-level insertEvent seam for one test, so a
// failing store can be injected to exercise the pending-event retry buffer.
func swapInsertEvent(t *testing.T, fn func(*store.Store, context.Context, time.Time, string, int, string) error) {
	t.Helper()
	old := insertEvent
	insertEvent = fn
	t.Cleanup(func() { insertEvent = old })
}

// The per-round DNS probe is single-flighted: while one lookup is still in
// flight, the next round skips its DNS sample instead of piling up goroutines.
func TestRoundDNSSingleFlight(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 10, 1)
	m.prober = prober.New(nil, time.Second) // no targets: rounds touch no network

	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	swapResolveTime(t, func(ctx context.Context) (time.Duration, bool, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return 25 * time.Millisecond, true, nil
	})

	ctx := context.Background()
	m.round(ctx)
	<-started    // the first round's lookup is now in flight
	m.round(ctx) // must skip DNS: the previous lookup is still running
	close(release)
	m.dnsWG.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("resolveTime calls = %d, want 1 (second round must single-flight skip)", got)
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&n); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if n != 1 {
		t.Fatalf("dns samples persisted = %d, want 1", n)
	}
	// Every completed round moves the liveness counter, DNS-skipped or not.
	if got := stats.Lifetime().Counters["monitor.rounds"]; got != 2 {
		t.Errorf("monitor.rounds = %d, want 2", got)
	}
}

// A monitoring pause bumps dnsGen; a DNS probe already in flight when the pause
// happened must DISCARD its now-stale result rather than resurrect the seed the
// pause cleared. Regression test for the pause / in-flight-probe race: without
// the generation guard the late goroutine sets dnsSeen=true (and inserts a
// sample) against a span that is supposed to read as "no reading yet".
func TestRoundDNSProbeDiscardedAfterPause(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 10, 1)
	m.prober = prober.New(nil, time.Second) // no targets: rounds touch no network

	started := make(chan struct{})
	release := make(chan struct{})
	swapResolveTime(t, func(ctx context.Context) (time.Duration, bool, error) {
		close(started)
		<-release
		return 25 * time.Millisecond, true, nil
	})

	ctx := context.Background()
	m.round(ctx) // launches the DNS probe goroutine
	<-started    // it is now blocked inside resolveTime (in flight)

	// Simulate the pause branch: drop the seed AND bump the generation, exactly
	// as Run does when monitoring switches off mid-probe.
	m.mu.Lock()
	m.dnsSeen = false
	m.dnsGen++
	m.mu.Unlock()

	close(release) // let the in-flight probe finish and attempt its write
	m.dnsWG.Wait()

	m.mu.Lock()
	seen := m.dnsSeen
	m.mu.Unlock()
	if seen {
		t.Fatal("in-flight DNS probe wrote a stale result after a pause bumped dnsGen; dnsSeen should stay false")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&n); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if n != 0 {
		t.Fatalf("the discarded probe still inserted %d dns row(s)", n)
	}
}

// A failing resolver logs 'dns down' exactly once, counts dns.fail.<class> per
// failed lookup, and 'dns recovered' fires exactly once when it comes back;
// every sample (failed and ok) is persisted via InsertDNS.
func TestRoundDNSFailAndRecoverTransitionsOnce(t *testing.T) {
	stats.ResetForTest()
	var buf bytes.Buffer
	m, st := newTestMonitorLog(t, 10, 1, &buf)
	m.prober = prober.New(nil, time.Second)

	var mu sync.Mutex
	fail := true
	swapResolveTime(t, func(ctx context.Context) (time.Duration, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return 3 * time.Second, false, context.DeadlineExceeded
		}
		return 20 * time.Millisecond, true, nil
	})

	ctx := context.Background()
	roundAndWait := func() {
		m.round(ctx)
		m.dnsWG.Wait() // the DNS sample lands off the round goroutine
	}
	roundAndWait() // fail 1: dns down
	roundAndWait() // fail 2: still down, no repeat warning
	mu.Lock()
	fail = false
	mu.Unlock()
	roundAndWait() // ok: dns recovered
	roundAndWait() // ok again: no repeat recovery line

	if got := strings.Count(buf.String(), "dns down"); got != 1 {
		t.Errorf("'dns down' logged %d times, want exactly 1", got)
	}
	if got := strings.Count(buf.String(), "dns recovered"); got != 1 {
		t.Errorf("'dns recovered' logged %d times, want exactly 1", got)
	}
	s := stats.Lifetime()
	if got := s.Counters["dns.fail.timeout"]; got != 2 {
		t.Errorf("dns.fail.timeout = %d, want 2 (one per failed lookup)", got)
	}
	var total, okN int
	if err := st.DB().QueryRow(`SELECT COUNT(*), COALESCE(SUM(success),0) FROM dns`).Scan(&total, &okN); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if total != 4 || okN != 2 {
		t.Errorf("dns rows = %d with %d ok, want 4 with 2 ok", total, okN)
	}
	if snap := m.Snapshot(); !snap.DNSok || snap.DNSms != 20 {
		t.Errorf("snapshot after recovery: ok=%v ms=%v, want ok=true ms=20", snap.DNSok, snap.DNSms)
	}
	if got := s.Counters["monitor.rounds"]; got != 4 {
		t.Errorf("monitor.rounds = %d, want 4", got)
	}
}

// Switching monitoring off mid-wait (a settings wake) must be noticed at once,
// not only when the next round deadline elapses: with a long probe interval the
// pause episode still has to register promptly, otherwise up to one interval of
// switched-off time is miscounted as observed.
func TestPauseDetectedOnMidWaitWake(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	// An empty prober makes the immediate first round a harmless no-op (no
	// targets, no network); DNS off so no background resolver lookup.
	m.prober = prober.New(nil, time.Second)
	m.DNSFn = func() bool { return false }

	var mu sync.Mutex
	enabled := true
	wake := make(chan struct{})
	m.EnabledFn = func() bool { mu.Lock(); defer mu.Unlock(); return enabled }
	m.WakeFn = func() <-chan struct{} { mu.Lock(); defer mu.Unlock(); return wake }
	// A long interval: if the pause were only noticed at the round deadline, the
	// test would time out well before it registered.
	m.IntervalFn = func() time.Duration { return time.Hour }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()

	time.Sleep(20 * time.Millisecond) // let the immediate round settle into the wait

	mu.Lock()
	enabled = false
	old := wake
	wake = make(chan struct{})
	close(old) // wake the loop mid-wait
	mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if stats.Lifetime().Counters["monitor.pauses"] == 1 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if got := stats.Lifetime().Counters["monitor.pauses"]; got != 1 {
		t.Fatalf("monitor.pauses = %d, want 1: mid-wait pause not noticed before the round deadline", got)
	}
}

// A transition whose durable InsertEvent fails must still be observed (state
// flips, callbacks fire) but its record must be buffered and retried on a later
// round so nothing is lost: a missing 'down' cannot be reconstructed later.
// Injecting failure for BOTH the 'down' and the 'up', then letting the store
// recover, must land each event exactly once, in order, with the duration intact.
func TestPendingEventRetryLandsExactlyOnce(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)

	var mu sync.Mutex
	failing := true // the store is unavailable until we flip this
	swapInsertEvent(t, func(s *store.Store, ctx context.Context, ts time.Time, typ string, dur int, detail string) error {
		mu.Lock()
		f := failing
		mu.Unlock()
		if f {
			return errors.New("store unavailable")
		}
		return s.InsertEvent(ctx, ts, typ, dur, detail)
	})

	// DOWN at ts=100: its write fails, so nothing is persisted yet, but the
	// transition is still observed and the record is buffered for retry.
	feed(m, false, time.Unix(90, 0))
	feed(m, false, time.Unix(100, 0))
	if m.online {
		t.Fatal("expected DOWN observed even though its event write failed")
	}
	if got := eventCount(t, st, "down"); got != 0 {
		t.Fatalf("down event persisted while the store was failing: %d", got)
	}

	// UP at ts=150: its write ALSO fails while the store is still down; outage is
	// 50s. Both records are now buffered, the 'down' ahead of the 'up'.
	feed(m, true, time.Unix(150, 0))
	if !m.online {
		t.Fatal("expected UP observed")
	}
	if got := eventCount(t, st, "up"); got != 0 {
		t.Fatalf("up event persisted while the store was failing: %d", got)
	}

	// The store recovers. A round flushes the buffer at its start; a high DownAfter
	// keeps the empty no-network round from confirming a spurious transition.
	mu.Lock()
	failing = false
	mu.Unlock()
	m.prober = prober.New(nil, time.Second) // no targets: round() touches no network
	m.DNSFn = func() bool { return false }  // no background resolver lookup
	m.round(context.Background())

	if got := eventCount(t, st, "down"); got != 1 {
		t.Fatalf("down events after recovery = %d, want exactly 1", got)
	}
	if got := eventCount(t, st, "up"); got != 1 {
		t.Fatalf("up events after recovery = %d, want exactly 1", got)
	}
	var downTS, upTS, dur int
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if upTS < downTS {
		t.Fatalf("up ts=%d landed before down ts=%d after the flush", upTS, downTS)
	}
	if dur != 50 {
		t.Fatalf("up duration_s = %d, want 50 (outage 100->150 preserved through the retry)", dur)
	}

	// A second flush must not double-write: the buffer was drained on success, so
	// there is nothing left to replay.
	m.flushPendingEvents(context.Background())
	if got := eventCount(t, st, "down") + eventCount(t, st, "up"); got != 2 {
		t.Fatalf("events after a second flush = %d, want 2 (no double-write)", got)
	}
}

// A DNS probe whose lookup fails because the monitor is shutting down (its round
// ctx is cancelled) must be neutral: no "dns down" warning and no dns.fail.* bump,
// so a normal stop can't log a phantom resolver outage or poison the recovered
// counters. Regression for C-64.
func TestRoundDNSCancelledLookupIsNeutral(t *testing.T) {
	stats.ResetForTest()
	var buf bytes.Buffer
	m, st := newTestMonitorLog(t, 10, 1, &buf)
	m.prober = prober.New(nil, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	// The lookup fails, but only after the round's ctx is cancelled - exactly the
	// shutdown ordering: Run cancels, then an in-flight lookup errors out.
	swapResolveTime(t, func(c context.Context) (time.Duration, bool, error) {
		cancel()
		return 0, false, context.Canceled
	})

	m.round(ctx)
	m.dnsWG.Wait()

	if strings.Contains(buf.String(), "dns down") {
		t.Error("cancelled lookup logged a phantom 'dns down'")
	}
	for k, v := range stats.Lifetime().Counters {
		if strings.HasPrefix(k, "dns.fail.") {
			t.Errorf("cancellation bumped a failure counter: %s = %d, want none", k, v)
		}
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&n); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if n != 0 {
		t.Fatalf("cancelled probe inserted %d dns row(s), want 0", n)
	}
}

// Toggling the DNS sub-toggle OFF must clear the seed so re-enabling starts as
// "no reading yet" rather than resurfacing the pre-disable value as live.
// Regression for C-65.
func TestRoundDNSToggleOffClearsSeed(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 10, 1)
	m.prober = prober.New(nil, time.Second)

	swapResolveTime(t, func(ctx context.Context) (time.Duration, bool, error) {
		return 12 * time.Millisecond, true, nil
	})
	m.round(context.Background()) // seeds a live reading
	m.dnsWG.Wait()
	if !m.Snapshot().DNSactive {
		t.Fatal("expected a live DNS reading after the first probe")
	}

	// Toggle DNS off: the next round detects the edge and drops the seed.
	m.DNSFn = func() bool { return false }
	m.round(context.Background())
	m.mu.Lock()
	seen := m.dnsSeen
	m.mu.Unlock()
	if seen {
		t.Fatal("DNS seed survived the toggle-off; re-enabling would surface a stale reading")
	}

	// Re-enable: with no fresh probe yet, the old reading must NOT resurface.
	m.DNSFn = nil
	if m.Snapshot().DNSactive {
		t.Fatal("re-enabling DNS surfaced the stale pre-disable seed as live")
	}
}

// A DNS probe already in flight when the DNS sub-toggle is switched OFF must
// DISCARD its now-disallowed result rather than publish it. Regression for C-65:
// without bumping the generation on the toggle-off edge, the late goroutine sets
// dnsSeen=true and inserts a sample under a feature the operator turned off.
func TestRoundDNSInflightDiscardedAfterToggleOff(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 10, 1)
	m.prober = prober.New(nil, time.Second)

	started := make(chan struct{})
	release := make(chan struct{})
	swapResolveTime(t, func(ctx context.Context) (time.Duration, bool, error) {
		close(started)
		<-release
		return 25 * time.Millisecond, true, nil
	})

	m.round(context.Background()) // launches the DNS probe goroutine (DNS on)
	<-started                     // it is blocked inside resolveTime (in flight)

	// Operator switches the DNS sub-toggle off; the next round detects the edge and
	// bumps the generation, exactly as Run's round loop does.
	m.DNSFn = func() bool { return false }
	m.round(context.Background())

	close(release) // let the in-flight probe finish and attempt its write
	m.dnsWG.Wait()

	m.mu.Lock()
	seen := m.dnsSeen
	m.mu.Unlock()
	if seen {
		t.Fatal("in-flight DNS probe published after the toggle-off bumped dnsGen")
	}
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM dns`).Scan(&n); err != nil {
		t.Fatalf("count dns: %v", err)
	}
	if n != 0 {
		t.Fatalf("the discarded probe still inserted %d dns row(s)", n)
	}
}
