package monitor

import (
	"bytes"
	"context"
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

// A backward clock step during an outage (NTP correcting a fast RTC) must not
// stamp the recovery 'up' before the 'down' it closes, nor accrue a negative
// outage into the monotonic counter. The clamp pins the up event to the down's
// timestamp and the duration to zero.
func TestBackwardClockStepClampsRecovery(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 1, 1)
	feed(m, false, time.Unix(3600, 0)) // DOWN at ts=3600 (since was 0)
	if m.online {
		t.Fatal("expected DOWN after the failing round")
	}
	feed(m, true, time.Unix(60, 0)) // clock stepped back an hour, then recovered
	if !m.online {
		t.Fatal("expected UP after recovery")
	}

	var upTS, dur int
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	var downTS int
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down event: %v", err)
	}
	if upTS < downTS {
		t.Fatalf("up event ts=%d stamped before down ts=%d; ORDER BY ts DESC would read the down as newest", upTS, downTS)
	}
	if dur != 0 {
		t.Fatalf("outage duration = %d, want 0 (backward step clamped)", dur)
	}
	if got := stats.Lifetime().Floats["monitor.outage_s_sum"]; got != 0 {
		t.Fatalf("monitor.outage_s_sum = %v, want 0 (never negative)", got)
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
