package monitor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// The degraded trigger latches once per episode and re-arms only when latency
// RECOVERS - which is exactly when there is nothing left to measure. A dispatch
// that bounced off a speedtest already running measured nothing, so it must hand
// the episode back instead of consuming it.

// degradedDispatcher wires a Monitor to a dispatch that reports bounces, the way
// main.degradedDispatch does: the dispatch id is read inside the callback (on the
// monitor goroutine), and any bounce report names that dispatch.
func degradedDispatcher(m *Monitor, dispatch func(id uint64)) func() {
	return func() { dispatch(m.DegradedDispatch()) }
}

// A bounced dispatch must not cost the brownout its measurement: the next
// confirmed degraded round dispatches again, while the episode is still live.
func TestDegradedRetriesAfterABouncedDispatch(t *testing.T) {
	stats.ResetForTest()
	fired := 0
	var m *Monitor
	m = &Monitor{DegradedPingFn: func() float64 { return 100 }}
	m.OnDegraded = degradedDispatcher(m, func(id uint64) {
		fired++
		m.RetryDegraded(id) // every dispatch collides with a run already in flight
	})

	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true) // debounce satisfied: the episode fires
	if fired != 1 {
		t.Fatalf("dispatches after two degraded rounds = %d, want 1", fired)
	}
	m.checkDegraded(500, true, true)
	if fired != 2 {
		t.Fatalf("dispatches after a bounced one = %d, want 2 - the brownout is still unmeasured", fired)
	}
	// The episode is one brownout however many dispatches it took: the /metrics
	// counter must not turn a collision into a second episode.
	if got := stats.Lifetime().Counters["monitor.degraded_episodes"]; got != 1 {
		t.Errorf("monitor.degraded_episodes = %d, want 1 - a re-dispatch is the same episode", got)
	}
	// A dispatch that was ADMITTED still latches: only a bounce re-arms.
	m.OnDegraded = func() { fired++ }
	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true)
	if fired != 3 {
		t.Fatalf("dispatches after an admitted one = %d, want 3 - an accepted run owns the episode", fired)
	}
}

// A bounce reported late, after its episode ended, must not re-fire whatever
// episode happens to be current by then: the report names an episode, and ids
// never repeat.
func TestDegradedRetryIgnoredAfterItsEpisodeEnded(t *testing.T) {
	fired := 0
	var stale uint64
	var m *Monitor
	m = &Monitor{DegradedPingFn: func() float64 { return 100 }}
	m.OnDegraded = degradedDispatcher(m, func(id uint64) {
		fired++
		if stale == 0 {
			stale = id // remember the first episode's dispatch; its bounce lands late, below
		}
	})

	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true) // episode 1 fires
	m.checkDegraded(10, true, true)  // latency recovers: episode 1 is over
	m.RetryDegraded(stale)           // the bounce report finally lands
	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true) // episode 2 fires
	if fired != 2 {
		t.Fatalf("dispatches = %d, want 2 (one per episode)", fired)
	}
	m.checkDegraded(500, true, true)
	if fired != 2 {
		t.Fatalf("a stale bounce re-fired episode 2 (dispatches = %d, want 2)", fired)
	}
}

// A downed link ends the episode too, and the ids handed out never repeat - so
// no report from the old episode can be mistaken for one about the next.
func TestDegradedDispatchIDs(t *testing.T) {
	m := &Monitor{DegradedPingFn: func() float64 { return 100 }, OnDegraded: func() {}}
	if id := m.DegradedDispatch(); id != 0 {
		t.Fatalf("DegradedDispatch = %d before any degradation, want 0", id)
	}
	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true)
	first := m.DegradedDispatch()
	if first == 0 {
		t.Fatal("DegradedDispatch = 0 during a degraded episode")
	}
	m.checkDegraded(500, false, false) // link down: the outage owns this, not degradation
	if id := m.DegradedDispatch(); id != 0 {
		t.Fatalf("DegradedDispatch = %d after the link went down, want 0", id)
	}
	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true)
	if second := m.DegradedDispatch(); second == first {
		t.Fatalf("the next episode reused dispatch id %d", second)
	}
}

// blockingTester holds a run open until released, and records the trigger of
// every run it serves (it implements the scheduler's optional reason-aware half
// exactly as the real engines do).
type blockingTester struct {
	started chan string
	release chan struct{}

	mu      sync.Mutex
	reasons []string
}

func (b *blockingTester) Run(ctx context.Context) (speedtest.Result, error) {
	return b.RunReason(ctx, "")
}

func (b *blockingTester) RunReason(ctx context.Context, reason string) (speedtest.Result, error) {
	b.mu.Lock()
	b.reasons = append(b.reasons, reason)
	b.mu.Unlock()
	select {
	case b.started <- reason:
	default:
	}
	if reason != "degraded" { // only the occupying run blocks
		select {
		case <-b.release:
		case <-ctx.Done():
			return speedtest.Result{}, ctx.Err()
		}
	}
	return speedtest.Result{
		Engine: "fake", Server: "fake", DownloadMbps: 100, UploadMbps: 10,
		DownloadBytes: 1000, UploadBytes: 1000,
	}, nil
}

func (b *blockingTester) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.reasons...)
}

// End to end against the real scheduler: a brownout that begins while another
// run owns the runner must still get its speedtest once the runner frees up.
// Before the retry existed, the collision consumed the episode and no
// reason="degraded" run ever happened.
func TestDegradedSpeedtestSurvivesARunnerCollision(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	bt := &blockingTester{started: make(chan string, 4), release: make(chan struct{})}
	sched := speedtest.NewScheduler(bt, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	// Occupy the runner with a manual run and wait until it really owns it.
	occupied := make(chan struct{})
	go func() { defer close(occupied); sched.RunOnce(ctx, "manual") }()
	awaitSignal(t, bt.started, "the manual run to start")

	dispatched := make(chan struct{}, 4)
	var m *Monitor
	m = &Monitor{DegradedPingFn: func() float64 { return 100 }}
	m.OnDegraded = degradedDispatcher(m, func(id uint64) {
		go func() {
			// The production wiring (main.degradedDispatch): a run that never started
			// hands the episode back.
			if _, err := sched.RunOnce(ctx, "degraded"); errors.Is(err, speedtest.ErrBusy) {
				m.RetryDegraded(id)
			}
			dispatched <- struct{}{}
		}()
	})

	m.checkDegraded(500, true, true)
	m.checkDegraded(500, true, true) // brownout confirmed while the runner is busy
	awaitSignal(t, dispatched, "the colliding dispatch to bounce")
	if got := bt.seen(); len(got) != 1 || got[0] != "manual" {
		t.Fatalf("runs served while busy = %v, want just the manual one", got)
	}

	close(bt.release) // the manual run finishes; the brownout is still going
	awaitSignal(t, occupied, "the manual run to finish")

	m.checkDegraded(500, true, true)
	awaitSignal(t, dispatched, "the retried dispatch")
	if got := bt.seen(); len(got) != 2 || got[1] != "degraded" {
		t.Fatalf("runs = %v, want a degraded run after the collision cleared", got)
	}
}

// awaitSignal blocks until ch yields (a value or a close), failing the test on a
// timeout rather than hanging the package.
func awaitSignal[T any](t *testing.T, ch <-chan T, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
