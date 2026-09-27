package monitor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// Probe rounds taken while one of our own speedtests is using the network are
// HELD: kept as evidence, but not counted toward Down after for up to
// speedtestHoldMax. These tests pin what that changes (confirmation) and what it
// must not (the date an outage is stored with).

func counter(k string) int64 { return stats.Lifetime().Counters[k] }

// Timeline A: a test makes the checks fail and they stop when it does. A build
// without the hold records a 10 s outage, alerts twice and starts a reconnect
// test; with it, nothing is recorded and the counter says what was avoided.
func TestRoundsDuringASpeedtestCannotStartAnOutage(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	var transitions int
	m.OnTransition = func(bool, int) { transitions++ }
	feedDuring(m, false, time.Unix(5, 0))
	feedDuring(m, false, time.Unix(10, 0))
	feedDuring(m, false, time.Unix(15, 0))
	if !m.online {
		t.Fatal("rounds taken during a speedtest confirmed an outage")
	}
	feed(m, true, time.Unix(20, 0))
	if n := eventCount(t, st, "down") + eventCount(t, st, "up"); n != 0 || transitions != 0 {
		t.Fatalf("events = %d, transitions = %d; want none", n, transitions)
	}
	if got := counter("monitor.speedtest_downs_suppressed.manual"); got != 1 {
		t.Errorf("speedtest_downs_suppressed.manual = %d, want 1", got)
	}
	if got := counter("monitor.blips"); got != 0 {
		t.Errorf("monitor.blips = %d, want 0: every failed round was the test's", got)
	}
	if got := counter("monitor.bad_rounds"); got != 3 {
		t.Errorf("monitor.bad_rounds = %d, want 3: held rounds are still failed rounds", got)
	}
}

// Timeline B: the line dies during the test and stays dead. The outage is
// confirmed after the test, but stored from the round where the failing run
// reached Down after - exactly where it is stored without a test.
func TestOutageThatOutlastsTheTestIsDatedAsToday(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	for ts := int64(20); ts <= 45; ts += 5 {
		feedDuring(m, false, time.Unix(ts, 0))
		if !m.online {
			t.Fatalf("confirmed at %d, while the test was still running", ts)
		}
	}
	feed(m, false, time.Unix(50, 0)) // the first round after the test: one counted
	if !m.online {
		t.Fatal("confirmed on a single counted round")
	}
	feed(m, false, time.Unix(55, 0)) // two counted: confirmed
	if m.online {
		t.Fatal("not confirmed after Down after rounds outside the test")
	}
	var downTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down: %v", err)
	}
	if downTS != 25 {
		t.Errorf("down stored at %d, want 25 (the round the run reached Down after)", downTS)
	}
	if m.since.Unix() != 25 {
		t.Errorf("m.since = %d, want 25", m.since.Unix())
	}
	feed(m, true, time.Unix(600, 0))
	var dur int
	if err := st.DB().QueryRow(`SELECT duration_s FROM events WHERE type='up'`).Scan(&dur); err != nil {
		t.Fatalf("read up: %v", err)
	}
	if dur != 575 {
		t.Errorf("outage lasted %d s, want 575 - the same as without the test", dur)
	}
	if got := counter("monitor.speedtest_downs_delayed.manual"); got != 1 {
		t.Errorf("speedtest_downs_delayed.manual = %d, want 1", got)
	}
	if got := stats.Lifetime().Floats["monitor.speedtest_delay_s_sum.manual"]; got != 30 {
		t.Errorf("speedtest_delay_s_sum.manual = %v, want 30 (confirmed at 55, dated 25)", got)
	}
	if got := counter("monitor.speedtest_downs_suppressed.manual"); got != 0 {
		t.Errorf("speedtest_downs_suppressed.manual = %d, want 0: this outage was recorded", got)
	}
}

// Timeline C: a dead line keeps a long test running. Past two minutes from the
// first failed round, rounds count again even though the test is still going.
func TestHoldCapConfirmsAFailureThatOutlastsTwoMinutes(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	t0 := int64(1000)
	var confirmedAt int64
	for k := int64(0); k <= 30 && m.online; k++ {
		ts := t0 + 5*k
		feedDuring(m, false, time.Unix(ts, 0))
		if !m.online {
			confirmedAt = ts
		}
	}
	if confirmedAt != t0+125 {
		t.Fatalf("confirmed at +%d s, want +125 s: rounds from +120 s count, and Down after is 2", confirmedAt-t0)
	}
	var downTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down: %v", err)
	}
	if downTS != t0+5 {
		t.Errorf("down stored at +%d s, want +5 s (the second failed round)", downTS-t0)
	}
	if got := stats.Lifetime().Floats["monitor.speedtest_delay_s_sum.manual"]; got != 120 {
		t.Errorf("delay = %v s, want 120", got)
	}
}

func TestBlipCountsOnlyRoundsOutsideTheTest(t *testing.T) {
	for _, c := range []struct {
		downAfter      int
		wantSuppressed int64
	}{{3, 0}, {2, 1}} {
		t.Run(fmt.Sprintf("down after %d", c.downAfter), func(t *testing.T) {
			stats.ResetForTest()
			m, _ := newTestMonitor(t, c.downAfter, 1)
			feed(m, false, time.Unix(5, 0))        // counted
			feedDuring(m, false, time.Unix(10, 0)) // held
			feed(m, true, time.Unix(15, 0))
			if got := counter("monitor.blips"); got != 1 {
				t.Errorf("blips = %d, want 1", got)
			}
			if got := stats.Lifetime().Gauges["monitor.blip_streak_max"]; got != 1 {
				t.Errorf("blip_streak_max = %d, want 1: only the round outside the test is the line's", got)
			}
			if got := counter("monitor.speedtest_downs_suppressed.manual"); got != c.wantSuppressed {
				t.Errorf("suppressed = %d, want %d", got, c.wantSuppressed)
			}
		})
	}
}

// Down after is read once per round. With two reads, a settings save landing
// between them dated the run at one round without confirming it there, so the
// outage was confirmed later, dated back, and counted as delayed by a speedtest
// when none was running. Here the save (2 to 5) lands inside the second failed
// round: whichever side of it the round reads, the date and the flip agree.
func TestDownAfterIsReadOncePerRound(t *testing.T) {
	downAfterSavedMidRound := func(m *Monitor) (next func()) {
		round, reads := 0, 0
		m.DownAfterFn = func() int {
			reads++
			if round < 2 || (round == 2 && reads == 1) {
				return 2
			}
			return 5
		}
		return func() { round++; reads = 0 }
	}
	quietCounters := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{"monitor.speedtest_downs_delayed.", "monitor.speedtest_downs_suppressed."} {
			if got := counter(k); got != 0 {
				t.Errorf("%s = %d with no speedtest running", k, got)
			}
		}
		if got := stats.Lifetime().Floats["monitor.speedtest_delay_s_sum."]; got != 0 {
			t.Errorf("speedtest_delay_s_sum. = %v with no speedtest running", got)
		}
	}

	t.Run("link", func(t *testing.T) {
		stats.ResetForTest()
		m, st := newTestMonitor(t, 2, 1)
		next := downAfterSavedMidRound(m)
		for _, ts := range []int64{5, 10, 15, 20, 25} {
			next()
			feed(m, false, time.Unix(ts, 0))
			if ts == 10 && m.online {
				t.Fatal("the round that dated the run did not confirm it")
			}
		}
		var downTS int64
		if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
			t.Fatalf("read down: %v", err)
		}
		if downTS != 10 {
			t.Errorf("down stored at %d, want 10", downTS)
		}
		quietCounters(t)
	})

	t.Run("family", func(t *testing.T) {
		stats.ResetForTest()
		m, _ := newTestMonitor(t, 2, 1)
		next := downAfterSavedMidRound(m)
		fail := prober.FamilyResult{Family: "ipv4", Online: false}
		for _, ts := range []int64{5, 10} {
			next()
			m.advanceFamily(fail, time.Unix(ts, 0), false)
		}
		fs := m.fams["ipv4"]
		if fs.online || fs.since.Unix() != 10 {
			t.Errorf("family online=%v since=%d, want down since 10: the round that dated it confirms it", fs.online, fs.since.Unix())
		}
	})
}

// A gap (pause, suspend, cancelled or skipped round) ends a held run like any
// streak, so an outage is never dated across one.
func TestGapEndsAHeldRun(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	feedDuring(m, false, time.Unix(10, 0))
	feedDuring(m, false, time.Unix(15, 0)) // reached Down after: due 15
	m.resetStreaks()
	feed(m, false, time.Unix(500, 0))
	if !m.online {
		t.Fatal("held rounds from before the gap counted after it")
	}
	feed(m, false, time.Unix(505, 0))
	if m.online {
		t.Fatal("two failed rounds after the gap did not confirm")
	}
	var downTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down: %v", err)
	}
	if downTS != 505 {
		t.Errorf("down stored at %d, want 505: the date must not reach back across the gap", downTS)
	}
}

// The families follow the link's rule, or the pills and monitor.flap.* would
// flip on our own tests while the link did not.
func TestFamilyHeldLikeTheLink(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	fail := prober.FamilyResult{Family: "ipv4", Online: false}
	for _, ts := range []int64{10, 15, 20} {
		m.advanceFamily(fail, time.Unix(ts, 0), true)
	}
	fs := m.fams["ipv4"]
	if !fs.online || counter("monitor.flap.ipv4") != 0 {
		t.Fatalf("a family flipped on rounds taken during a speedtest (online=%v flaps=%d)", fs.online, counter("monitor.flap.ipv4"))
	}
	m.advanceFamily(fail, time.Unix(25, 0), false)
	m.advanceFamily(fail, time.Unix(30, 0), false)
	if fs.online || counter("monitor.flap.ipv4") != 1 {
		t.Fatalf("the family did not flip after Down after rounds outside the test (online=%v)", fs.online)
	}
	if fs.since.Unix() != 15 {
		t.Errorf("family down since %d, want 15 (where its failing run reached Down after)", fs.since.Unix())
	}
}

// round hands during to the family debounce as well as the link's. The other
// round tests probe no targets, so their results carry no family at all; this
// one has a real IPv4 family that fails every round.
func TestRoundHoldsTheFamiliesDuringASpeedtest(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	// A closed loopback port: every dial is refused (or times out) straight
	// away, without touching the internet.
	m.prober = prober.New([]config.Target{
		{Name: "closed", Network: "tcp4", Address: "127.0.0.1:1", Family: config.IPv4},
	}, 500*time.Millisecond)
	m.DNSFn = func() bool { return false }
	m.SpeedtestActivityFn = func() (uint64, string) { return 1, "scheduled" } // a test runs throughout
	for i := 0; i < 3; i++ {
		m.round(context.Background())
	}
	fs := m.fams["ipv4"]
	if fs == nil {
		t.Fatal("no ipv4 family after rounds that probed an ipv4 anchor")
	}
	if !fs.online || counter("monitor.flap.ipv4") != 0 {
		t.Fatalf("the family flipped on rounds taken during a speedtest (online=%v flaps=%d)", fs.online, counter("monitor.flap.ipv4"))
	}
	if fs.run.held != 3 {
		t.Errorf("family held = %d, want 3: round must pass during to advanceFamily", fs.run.held)
	}
}

// Every per-round check round feeds must be told when a test overlapped the
// round. The degraded check needs a round that reached its targets to show it,
// which a test cannot arrange without the internet or a latency-sensitive
// loopback server, so its call is pinned here in the source.
func TestRoundPassesDuringToEveryCheck(t *testing.T) {
	src, err := os.ReadFile("monitor.go")
	if err != nil {
		t.Fatalf("read monitor.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (m *Monitor) round(")
	if start < 0 {
		t.Fatal("round not found")
	}
	body = body[start:]
	if end := strings.Index(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	for _, call := range []string{
		"m.advanceFamily(fr, res.TS, during)",
		"m.advance(ctx, res, during, trigger)",
		"m.noteDegraded(res, during)",
	} {
		if !strings.Contains(body, call) {
			t.Errorf("round no longer calls %s: a check that is not told about the speedtest counts the test's own rounds", call)
		}
	}
}

// round reads the wire counter on both sides of the probe, so a test that
// overlapped any part of the round is seen, and an idle counter is not.
func TestRoundReadsTheWireCounterOnBothSides(t *testing.T) {
	for _, c := range []struct {
		name   string
		a0, a1 uint64
		held   bool
	}{
		{"no test", 0, 0, false},
		{"running throughout", 1, 1, true},
		{"started mid-round", 0, 1, true},
		{"ended mid-round", 1, 2, true},
		{"started and ended mid-round", 2, 4, true},
		{"idle between tests", 2, 2, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			stats.ResetForTest()
			m, _ := newTestMonitor(t, 5, 1)
			m.prober = prober.New(nil, time.Second) // no targets: a failed round, not a skipped one
			m.DNSFn = func() bool { return false }
			reads := []uint64{c.a0, c.a1}
			m.SpeedtestActivityFn = func() (uint64, string) {
				v := reads[0]
				reads = reads[1:]
				return v, "scheduled"
			}
			m.round(context.Background())
			if len(reads) != 0 {
				t.Fatalf("round read the counter %d times, want 2 (before and after the probe)", 2-len(reads))
			}
			wantHeld, wantRounds := 0, int64(0)
			if c.held {
				wantHeld, wantRounds = 1, 1
			}
			if m.run.held != wantHeld {
				t.Errorf("held = %d, want %d", m.run.held, wantHeld)
			}
			if got := counter("monitor.speedtest_rounds.scheduled"); got != wantRounds {
				t.Errorf("speedtest_rounds.scheduled = %d, want %d", got, wantRounds)
			}
			if got := counter("monitor.speedtest_bad_rounds.scheduled"); got != wantRounds {
				t.Errorf("speedtest_bad_rounds.scheduled = %d, want %d", got, wantRounds)
			}
		})
	}
}

// The per-trigger key set is closed: an unknown trigger is counted as "other"
// rather than minting a new series.
func TestSpeedtestTriggerIsAClosedSet(t *testing.T) {
	for _, tr := range []string{"startup", "scheduled", "reconnect", "degraded", "manual"} {
		if got := speedtestTrigger(tr); got != tr {
			t.Errorf("speedtestTrigger(%q) = %q", tr, got)
		}
	}
	for _, tr := range []string{"", "first", "Manual", "manual\n"} {
		if got := speedtestTrigger(tr); got != "other" {
			t.Errorf("speedtestTrigger(%q) = %q, want other", tr, got)
		}
	}
}

func TestTailCounterCountsTheFirstRoundAfterATest(t *testing.T) {
	stats.ResetForTest()
	m, _ := newTestMonitor(t, 2, 1)
	failed := prober.Result{Online: false}
	m.noteSpeedtestRound(failed, true, "reconnect")
	m.noteSpeedtestRound(failed, false, "")
	m.noteSpeedtestRound(failed, false, "")
	if got := counter("monitor.speedtest_tail_bad_rounds.reconnect"); got != 1 {
		t.Errorf("tail_bad_rounds.reconnect = %d, want 1: only the first round after the test is its tail", got)
	}
	if got := counter("monitor.speedtest_rounds.reconnect"); got != 1 {
		t.Errorf("speedtest_rounds.reconnect = %d, want 1", got)
	}
	// A good round after a test is no tail.
	m.noteSpeedtestRound(failed, true, "reconnect")
	m.noteSpeedtestRound(prober.Result{Online: true}, false, "")
	if got := counter("monitor.speedtest_tail_bad_rounds.reconnect"); got != 1 {
		t.Errorf("tail_bad_rounds.reconnect = %d after a good tail round, want still 1", got)
	}
}

// A round taken during a test has no reading for the degraded check: the test
// raised the latency. It neither starts a brownout nor ends one.
func TestDegradedSkipsRoundsDuringASpeedtest(t *testing.T) {
	slow := prober.Result{Online: true, Families: map[string]prober.FamilyResult{
		"ipv4": {Family: "ipv4", Online: true, OK: 3, Total: 3, Latency: 500 * time.Millisecond},
	}}
	run := func(during []bool) (dispatchedAt int) {
		stats.ResetForTest()
		m, _ := newTestMonitor(t, 2, 1)
		m.DegradedPingFn = func() float64 { return 100 }
		dispatchedAt = -1
		round := 0
		m.OnDegraded = func() {
			if dispatchedAt < 0 {
				dispatchedAt = round
			}
		}
		for i, d := range during {
			round = i
			m.noteDegraded(slow, d)
		}
		return dispatchedAt
	}
	if got := run([]bool{false, false}); got != 1 {
		t.Fatalf("control: dispatched at round %d, want 1 (two rounds over the threshold)", got)
	}
	if got := run([]bool{false, true, true, true}); got != -1 {
		t.Fatalf("dispatched at round %d on latency a speedtest caused", got)
	}
	if got := run([]bool{false, true, true, true, false}); got != 4 {
		t.Fatalf("dispatched at round %d, want 4: a brownout that outlasts the test is still measured", got)
	}
	if got := counter("monitor.degraded_episodes"); got != 1 {
		t.Errorf("degraded_episodes = %d, want 1", got)
	}
}

// End to end against the real scheduler: rounds taken while its run holds the
// engine are held, and once it finishes the outage is confirmed and dated at the
// round the failing run reached Down after. The rounds that could be mistaken
// for that one (the first failed round, and the round that confirms) are kept
// to other wall-clock seconds, so a date taken from either of them shows.
func TestSpeedtestHoldAgainstTheRealScheduler(t *testing.T) {
	stats.ResetForTest()
	m, st := newTestMonitor(t, 2, 1)
	m.prober = prober.New(nil, time.Second) // every round fails
	m.DNSFn = func() bool { return false }
	bt := &blockingTester{started: make(chan string, 4), release: make(chan struct{})}
	sched := speedtest.NewScheduler(bt, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.SpeedtestActivityFn = func() (uint64, string) {
		a := sched.Activity()
		return a.Seq, a.Trigger
	}
	ctx := context.Background()
	done := make(chan struct{})
	go func() { defer close(done); sched.RunOnce(ctx, "manual") }()
	awaitSignal(t, bt.started, "the manual run to reach the engine")

	// Each round's wall clock, read just before and just after it. The rounds
	// themselves take microseconds, so without the waits below they would all
	// share one second and any of them would pass for the second.
	var at [5][2]time.Time
	round := func(i int) {
		at[i][0] = time.Now()
		m.round(ctx)
		at[i][1] = time.Now()
	}
	nextSecond := func(after time.Time) {
		for time.Now().Unix() <= after.Unix() {
			time.Sleep(10 * time.Millisecond)
		}
	}
	round(0)
	nextSecond(at[0][1])
	round(1) // the run reaches Down after here
	round(2)
	if !m.online {
		t.Fatal("rounds taken while the test held the engine confirmed an outage")
	}
	close(bt.release)
	awaitSignal(t, done, "the manual run to finish")
	round(3)
	nextSecond(at[1][1])
	round(4) // two failed rounds outside the test: confirmed
	if m.online {
		t.Fatal("two failed rounds after the test did not confirm the outage")
	}
	var downTS int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down: %v", err)
	}
	switch {
	case downTS <= at[0][1].Unix():
		t.Errorf("down stored at %d, the first failed round's second; want the second round's", downTS)
	case downTS >= at[4][0].Unix():
		t.Errorf("down stored at %d, the confirming round's second; want the second round's", downTS)
	case downTS < at[1][0].Unix() || downTS > at[1][1].Unix():
		t.Errorf("down stored at %d, want the second round's time [%d, %d]", downTS, at[1][0].Unix(), at[1][1].Unix())
	}
	if got := counter("monitor.speedtest_downs_delayed.manual"); got != 1 {
		t.Errorf("speedtest_downs_delayed.manual = %d, want 1", got)
	}
}

// The late, back-dated 'down' reads back like any other through the store's own
// readers, on the multi-connection file-backed store the daemon runs.
func TestDelayedDownReadsBackOnAFileStore(t *testing.T) {
	stats.ResetForTest()
	st, err := store.Open(filepath.Join(t.TempDir(), "hold.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(config.Config{DownAfter: 2, UpAfter: 1}, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	m.since = base
	if err := st.InsertSamples(ctx, []store.Sample{{
		TS: base.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	for s := 20; s <= 45; s += 5 {
		feedDuring(m, false, at(s))
	}
	feed(m, false, at(50))
	feed(m, false, at(55))
	feed(m, true, at(600))

	evs, err := st.EventsPage(ctx, 10, 0)
	if err != nil {
		t.Fatalf("EventsPage: %v", err)
	}
	var down, up *store.Event
	for i := range evs {
		switch evs[i].Type {
		case "down":
			down = &evs[i]
		case "up":
			up = &evs[i]
		}
	}
	if down == nil || up == nil || len(evs) != 2 {
		t.Fatalf("events = %+v, want one down and one up", evs)
	}
	if down.TS != at(25).Unix() || up.TS != at(600).Unix() || up.DurationS != 575 {
		t.Errorf("outage read back as down %d, up %d (%d s); want down %d, up %d (575 s)",
			down.TS, up.TS, up.DurationS, at(25).Unix(), at(600).Unix())
	}
	o, err := st.UptimeSince(ctx, base.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if o.Down != 575*time.Second {
		t.Errorf("UptimeSince booked %v of downtime, want 575s", o.Down)
	}
}

// heldEvent is one transition a monitor wrote, with the round that wrote it.
type heldEvent struct {
	kind  string
	ts    int64
	dur   int
	round int
}

// runSequence feeds one scripted sequence to a fresh monitor and returns the
// transitions it wrote. The store is never touched: insertEvent is captured.
func runSequence(downAfter, upAfter int, fail, during []bool, hold bool, capture *[]heldEvent, round *int) {
	m := New(config.Config{DownAfter: downAfter, UpAfter: upAfter}, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 100})))
	m.since = time.Unix(0, 0)
	*capture = (*capture)[:0]
	for i := range fail {
		*round = i
		d := hold && during[i]
		trig := ""
		if d {
			trig = "manual"
		}
		m.advance(context.Background(), prober.Result{TS: time.Unix(int64(5*(i+1)), 0), Online: !fail[i]}, d, trig)
	}
}

// The dating rule, exhaustively: every sequence of up to seven rounds over
// {ok, failed} x {during a test, not}, for Down after 1-3 and Up after 1-2, run
// through a held monitor and through a reference that sees no tests (today's
// rule). Every sequence ends with Up after good rounds so every failing run
// resolves. All rounds sit well inside the hold cap.
//
// With Up after 1 the held monitor's outages are exactly the reference's, less
// the ones the hold suppressed. With Up after 2 or more they need not be: a
// single good round ends a held run but not the reference's outage, so the
// reference can date one outage from an earlier run. What still holds there is
// the rule itself: a down is dated at the round its own failing run reached
// Down after, a recovery lands on the same round as the reference's, and the
// reference has at most suppressed more outages.
func TestHeldDatingMatchesTheOldRule(t *testing.T) {
	var mu sync.Mutex // insertEvent is a package seam; the capture is guarded for -race's sake
	var got []heldEvent
	round := 0
	swapInsertEvent(t, func(_ *store.Store, _ context.Context, ts time.Time, kind string, dur int, _ string) error {
		mu.Lock()
		got = append(got, heldEvent{kind: kind, ts: ts.Unix(), dur: dur, round: round})
		mu.Unlock()
		return nil
	})
	const maxLen = 7
	cases := 0
	for downAfter := 1; downAfter <= 3; downAfter++ {
		for upAfter := 1; upAfter <= 2; upAfter++ {
			for n := 1; n <= maxLen; n++ {
				total := 1 << (2 * n)
				for code := 0; code < total; code++ {
					fail := make([]bool, n, n+upAfter)
					during := make([]bool, n, n+upAfter)
					for i := 0; i < n; i++ {
						d := (code >> (2 * i)) & 3
						fail[i], during[i] = d&1 == 1, d&2 == 2
					}
					for i := 0; i < upAfter; i++ {
						fail, during = append(fail, false), append(during, false)
					}
					var ref, held []heldEvent
					runSequence(downAfter, upAfter, fail, during, false, &got, &round)
					ref = append(ref, got...)
					stats.ResetForTest()
					runSequence(downAfter, upAfter, fail, during, true, &got, &round)
					held = append(held, got...)
					suppressed := int(counter("monitor.speedtest_downs_suppressed.manual"))
					if msg := checkHeldAgainstReference(downAfter, upAfter, fail, ref, held, suppressed); msg != "" {
						t.Fatalf("down after %d, up after %d, rounds %s: %s\nreference %+v\nheld      %+v",
							downAfter, upAfter, describeRounds(fail, during), msg, ref, held)
					}
					cases++
				}
			}
		}
	}
	if cases < 100000 {
		t.Fatalf("only %d sequences ran", cases)
	}
}

func describeRounds(fail, during []bool) string {
	s := ""
	for i := range fail {
		c := "ok"
		if fail[i] {
			c = "F"
		}
		if during[i] {
			c += "(d)"
		}
		s += c + " "
	}
	return s
}

type outage struct {
	downTS, upTS int64
	dur          int
	downRound    int
	upRound      int
}

func outagesOf(evs []heldEvent) []outage {
	var out []outage
	for _, e := range evs {
		switch e.kind {
		case "down":
			out = append(out, outage{downTS: e.ts, upTS: -1, downRound: e.round, upRound: -1})
		case "up":
			if len(out) > 0 && out[len(out)-1].upTS < 0 {
				o := &out[len(out)-1]
				o.upTS, o.dur, o.upRound = e.ts, e.dur, e.round
			}
		}
	}
	return out
}

func checkHeldAgainstReference(downAfter, upAfter int, fail []bool, refEv, heldEv []heldEvent, suppressed int) string {
	ref, held := outagesOf(refEv), outagesOf(heldEv)
	if len(ref)-len(held) > suppressed {
		return fmt.Sprintf("the reference has %d more outages than the held monitor, but only %d were suppressed", len(ref)-len(held), suppressed)
	}
	for _, h := range held {
		// The down is dated at the round its own failing run reached Down after.
		start := h.downRound
		for start > 0 && fail[start-1] {
			start--
		}
		if due := start + downAfter - 1; int64(5*(due+1)) != h.downTS {
			return fmt.Sprintf("a down confirmed at round %d is dated %d, want %d (its run reached Down after at round %d)",
				h.downRound, h.downTS, 5*(due+1), due)
		}
		if h.upTS < 0 {
			return "an outage never recovered, though the sequence ends with enough good rounds"
		}
		found := false
		for _, r := range ref {
			if upAfter == 1 {
				found = found || (r.downTS == h.downTS && r.upTS == h.upTS && r.dur == h.dur)
			} else {
				found = found || r.upRound == h.upRound
			}
		}
		if !found {
			return fmt.Sprintf("the held outage %+v has no counterpart in the reference", h)
		}
	}
	if upAfter == 1 && len(ref)-len(held) != suppressed {
		return fmt.Sprintf("the reference has %d more outages, but %d were counted as suppressed", len(ref)-len(held), suppressed)
	}
	return ""
}
