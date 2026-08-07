package speedtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func TestSpeedFailStage(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errors.New("fetch server list: timeout"), "server_list"},
		{errors.New("fetch server 1234: 404"), "server_fetch"},
		{errors.New("no speedtest servers available"), "no_servers"},
		{errors.New("ping: timeout"), "ping"},
		{errors.New("download: connection reset"), "download"},
		{errors.New("upload: connection reset"), "upload"},
		// N/A is wrapped under the download:/upload: prefix in Run, but must still
		// classify as "na" (checked via errors.Is before the prefix cases).
		{fmt.Errorf("download: %w", errMeasurementNA), "na"},
		{fmt.Errorf("upload: %w", errMeasurementNA), "na"},
		{errors.New("something unexpected"), "other"},
	}
	for _, c := range cases {
		if got := speedFailStage(c.err); got != c.want {
			t.Errorf("speedFailStage(%q) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A direction with nil bytes (an iperf3 best-effort run where it failed) must not
// fire a "0 Mbps" breach; a measured-low direction still does.
func TestEvalThresholdsSkipsUnmeasuredDirection(t *testing.T) {
	dB := int64(5_000_000)
	sp := store.SpeedSample{DownMbps: 500, UpMbps: 0, DownBytes: &dB, UpBytes: nil} // upload unmeasured
	if f := evalThresholds(sp, settings.Thresholds{UpMbps: 20}); len(f) != 0 {
		t.Errorf("unmeasured upload (nil bytes) must not breach, got %v", f)
	}
	uB := int64(100)
	sp.UpMbps, sp.UpBytes = 5, &uB // now a real low measurement
	if f := evalThresholds(sp, settings.Thresholds{UpMbps: 20}); len(f) != 1 {
		t.Errorf("measured low upload must breach, got %v", f)
	}
}

func TestEvalThresholds(t *testing.T) {
	jit, loss := 30.0, 4.0
	idle, ldDown, ldUp := 20.0, 200.0, 120.0     // bufferbloat: down +180, up +100
	dB, uB := int64(5_000_000), int64(1_000_000) // measured run has bytes (nil = unmeasured, skipped)
	sp := store.SpeedSample{
		DownMbps: 40, UpMbps: 5, PingMS: 88, JitterMS: &jit, PacketLoss: &loss,
		DownBytes: &dB, UpBytes: &uB,
		IdleMS: &idle, LoadedDownMS: &ldDown, LoadedUpMS: &ldUp,
	}
	all := settings.Thresholds{DownMbps: 100, UpMbps: 20, PingMS: 50, JitterMS: 10, LossPct: 1, BloatDownMS: 50, BloatUpMS: 50}

	// All seven thresholds breached.
	if f := evalThresholds(sp, all); len(f) != 7 {
		t.Fatalf("expected 7 failures, got %d: %v", len(f), f)
	}

	// A healthy run against the same thresholds (idle≈loaded ⇒ no bloat).
	okJit, okLoss, okIdle, okLd := 5.0, 0.0, 20.0, 25.0
	ok := store.SpeedSample{
		DownMbps: 500, UpMbps: 30, PingMS: 12, JitterMS: &okJit, PacketLoss: &okLoss,
		IdleMS: &okIdle, LoadedDownMS: &okLd, LoadedUpMS: &okLd,
	}
	if f := evalThresholds(ok, all); len(f) != 0 {
		t.Fatalf("expected no failures, got %v", f)
	}

	// Disabled thresholds (zero value) are never checked.
	if f := evalThresholds(sp, settings.Thresholds{}); len(f) != 0 {
		t.Fatalf("zero thresholds should not fail, got %v", f)
	}

	// Only ping enabled, and breached.
	if f := evalThresholds(sp, settings.Thresholds{PingMS: 50}); len(f) != 1 || !strings.Contains(f[0], "ping") {
		t.Fatalf("expected single ping failure, got %v", f)
	}

	// Only jitter enabled, and breached.
	if f := evalThresholds(sp, settings.Thresholds{JitterMS: 10}); len(f) != 1 || !strings.Contains(f[0], "jitter") {
		t.Fatalf("expected single jitter failure, got %v", f)
	}

	// Only packet loss enabled, and breached.
	if f := evalThresholds(sp, settings.Thresholds{LossPct: 1}); len(f) != 1 || !strings.Contains(f[0], "packet loss") {
		t.Fatalf("expected single packet-loss failure, got %v", f)
	}

	// A 100% threshold (the UI ceiling) must still fire on total loss, despite the
	// otherwise-strict comparison.
	totalLoss := 100.0
	gone := store.SpeedSample{DownMbps: 0, PacketLoss: &totalLoss}
	if f := evalThresholds(gone, settings.Thresholds{LossPct: 100}); len(f) != 1 || !strings.Contains(f[0], "packet loss") {
		t.Fatalf("100%% threshold must fire on 100%% loss, got %v", f)
	}
	// But 99% loss must NOT breach a 100% threshold.
	near := 99.0
	mostlyGone := store.SpeedSample{DownMbps: 0, PacketLoss: &near}
	if f := evalThresholds(mostlyGone, settings.Thresholds{LossPct: 100}); len(f) != 0 {
		t.Fatalf("99%% loss must not breach a 100%% threshold, got %v", f)
	}

	// Only download bloat enabled, and breached (200-20=180 > 50).
	if f := evalThresholds(sp, settings.Thresholds{BloatDownMS: 50}); len(f) != 1 || !strings.Contains(f[0], "download bloat") {
		t.Fatalf("expected single download-bloat failure, got %v", f)
	}

	// Only upload bloat enabled, and breached (120-20=100 > 50).
	if f := evalThresholds(sp, settings.Thresholds{BloatUpMS: 50}); len(f) != 1 || !strings.Contains(f[0], "upload bloat") {
		t.Fatalf("expected single upload-bloat failure, got %v", f)
	}

	// Jitter threshold set but sample has no jitter recorded → not checked.
	noJit := store.SpeedSample{DownMbps: 500, UpMbps: 30, PingMS: 12}
	if f := evalThresholds(noJit, settings.Thresholds{JitterMS: 10}); len(f) != 0 {
		t.Fatalf("nil jitter should not fail, got %v", f)
	}

	// Packet-loss threshold set but sample never measured loss (nil) → not checked.
	if f := evalThresholds(noJit, settings.Thresholds{LossPct: 1}); len(f) != 0 {
		t.Fatalf("nil packet loss should not fail, got %v", f)
	}

	// Bloat thresholds set but the run has no idle/loaded capture (nil) → not checked.
	if f := evalThresholds(noJit, settings.Thresholds{BloatDownMS: 50, BloatUpMS: 50}); len(f) != 0 {
		t.Fatalf("nil idle/loaded should not fail bloat, got %v", f)
	}
}

// testerFunc adapts a function to the Tester interface.
type testerFunc func(context.Context) (Result, error)

func (f testerFunc) Run(ctx context.Context) (Result, error) { return f(ctx) }

// A second RunOnce while one is in flight must be rejected with ErrBusy, so a
// reconnect during a scheduled test can never launch an overlapping run.
func TestSchedulerSingleFlight(t *testing.T) {
	stats.ResetForTest()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	started := make(chan struct{})
	release := make(chan struct{})
	first := make(chan struct{}, 1)
	first <- struct{}{}
	tester := testerFunc(func(ctx context.Context) (Result, error) {
		select {
		case <-first: // only the first invocation blocks
			close(started)
			<-release
		default:
		}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	go s.RunOnce(context.Background(), "first")
	<-started // first run is now inside tester.Run

	if _, err := s.RunOnce(context.Background(), "second"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent RunOnce should return ErrBusy, got %v", err)
	}

	close(release) // let the first run finish

	// Once it has finished, a fresh run should be allowed again.
	deadline := time.Now().Add(time.Second)
	for s.Running() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := s.RunOnce(context.Background(), "third"); err != nil {
		t.Fatalf("run after completion should be allowed, got %v", err)
	}

	// A failing tester must land in speed.fail but still count as a completed
	// (timed) attempt.
	s.TesterFn = func() Tester {
		return testerFunc(func(context.Context) (Result, error) { return Result{}, errors.New("boom") })
	}
	if _, err := s.RunOnce(context.Background(), "manual"); err == nil {
		t.Fatal("expected the failing tester's error")
	}

	// Counter sums across the four attempts above: first/third/manual ran
	// (manual failed), second bounced off the busy gate.
	snap := stats.Lifetime()
	for name, want := range map[string]int64{
		"speed.run.first":  1,
		"speed.run.third":  1,
		"speed.run.manual": 1,
		"speed.errbusy":    1,
		"speed.fail":       1,
		"speed.duration_n": 3,
	} {
		if got := snap.Counters[name]; got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	// Windows' coarse monotonic clock can legitimately time a sub-tick fake run
	// as exactly 0s, so presence is the honest assertion here - duration_n above
	// already proves all three attempts were timed. Negative would mean the
	// clock ran backwards.
	if v, ok := snap.Floats["speed.duration_s_sum"]; !ok || v < 0 {
		t.Errorf("speed.duration_s_sum = %v (recorded=%v), want recorded and >= 0", v, ok)
	}
}

// A run that comes due while the schedule window is closed must fire shortly
// after the window opens, not a full interval later. (A tick landing outside a
// daily window used to restart the full-interval wait, retrying at roughly the
// same wall-clock time forever.) Boots ENABLED so the startup slot is consumed
// up front and the window-open fire is the SCHEDULED deferral path - the
// gated-boot flavor of window-open now belongs to the startupPending latch and
// is covered by the latch tests.
func TestSchedulerRunsWhenWindowOpens(t *testing.T) {
	stats.ResetForTest()
	defer func(d, r, f time.Duration) { startupDelay, scheduleRecheck, firstEnableDelay = d, r, f }(startupDelay, scheduleRecheck, firstEnableDelay)
	startupDelay = 0
	scheduleRecheck = 5 * time.Millisecond
	firstEnableDelay = 0

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, 300*time.Millisecond, log)

	allowed := atomic.Bool{} // window starts OPEN: boot claims the startup slot
	allowed.Store(true)
	s.EnabledFn = func() bool { return allowed.Load() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Join the loop, don't just cancel it. The fake tester signals from INSIDE
	// tester.Run, so the moment this test stops waiting the loop is still inside
	// RunOnce with counter and store writes ahead of it; unjoined, one of those
	// increments lands in whatever test runs next (under -shuffle that is a real
	// cross-test failure). Loop calls RunOnce synchronously and rechecks ctx right
	// after, so the join is prompt, and this defer is registered after st.Close's
	// so LIFO joins before the store shuts under the write.
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()

	// The boot startup run consumes the slot, then the window closes.
	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("enabled boot did not run the startup test")
	}
	allowed.Store(false)

	// The window stays closed across the next deadline (300ms + jitter <30ms
	// after the startup run): no run.
	select {
	case <-runs:
		t.Fatal("speedtest ran while the schedule window was closed")
	case <-time.After(450 * time.Millisecond):
	}

	// Open the window: the overdue test must fire at the recheck cadence. The
	// old loop would have waited out another full interval, well past this
	// deadline.
	allowed.Store(true)
	select {
	case <-runs:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("speedtest did not fire promptly after the window opened")
	}

	// The startup slot was spent at boot, so the window-open fire is the
	// scheduled path - the one a history gap is traced to.
	snap := stats.Lifetime()
	if got := snap.Counters["speed.run.scheduled"]; got < 1 {
		t.Errorf("speed.run.scheduled = %d, want >= 1 (the deferral path must fire as scheduled)", got)
	}
	if got := snap.Counters["speed.run.startup"]; got != 1 {
		t.Errorf("speed.run.startup = %d, want exactly 1 (boot only)", got)
	}
}

// The startup run belongs to the first moment the scheduler is ENABLED, not to
// boot. A fresh install boots with the scheduler gated (Quick Setup hold +
// speedtests off); when the user consents - Quick Setup's Start monitoring, or
// the Settings toggle later - the first test must fire shortly after, and the
// schedule must anchor to ITS end, not to boot. Before the latch, the consent
// wake only re-derived the boot-anchored deadline: a user who chose "hourly"
// stared at an empty speed panel for the full hour.
func TestSchedulerFirstEnableRunsStartup(t *testing.T) {
	stats.ResetForTest()
	defer func(d, f time.Duration) { startupDelay, firstEnableDelay = d, f }(startupDelay, firstEnableDelay)
	startupDelay = 0
	firstEnableDelay = 0

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	var enabled atomic.Bool // fresh install: gated at boot
	s.EnabledFn = func() bool { return enabled.Load() }
	// The settings-broadcast wake, so consent reaches the loop without waiting
	// out the hour deadline - exactly how set.Changed delivers it in production.
	var wakeMu sync.Mutex
	wakeCh := make(chan struct{})
	s.WakeFn = func() <-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return wakeCh }
	broadcast := func() { wakeMu.Lock(); close(wakeCh); wakeCh = make(chan struct{}); wakeMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()
	// Boot must complete gated before the test enables (the anchor publishes
	// right after the boot check), or the Store below could turn this into an
	// enabled boot.
	waitForAnchor(t, s)
	boot := s.anchor.Load()

	// Gated boot: no startup run.
	select {
	case <-runs:
		t.Fatal("speedtest ran while the scheduler was gated at boot")
	case <-time.After(100 * time.Millisecond):
	}

	// Consent: enable + settings broadcast. The carried-over startup run must
	// fire promptly - not an interval from boot.
	enabled.Store(true)
	broadcast()
	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("consent did not trigger the carried-over startup run")
	}

	// The schedule re-anchors to the consent run's END. Asserted on the anchor
	// itself, not a NextRun window: the boot anchor plus its jitter satisfied
	// any tolerance wide enough to absorb jitter, which mutation testing showed
	// let the re-anchor lines be deleted with every test still green. The
	// reference is the BOOT anchor, not an instant captured just before
	// consent: Windows' monotonic clock ticks at interrupt granularity (up to
	// ~15.6ms), and the consent run completes inside one tick, so
	// After(just-before-consent) was false forever there - the 100ms gated-boot
	// window above is what guarantees the two anchors can never share a tick.
	reDeadline := time.Now().Add(5 * time.Second)
	for {
		if a := s.anchor.Load(); a != nil && a.lastRun.After(boot.lastRun) {
			break
		}
		if time.Now().After(reDeadline) {
			t.Fatal("anchor was never re-derived from the consent run's end (still boot-anchored or withdrawn)")
		}
		time.Sleep(time.Millisecond)
	}

	// One slot per boot: toggling off and on again must NOT fire another
	// immediate run - later toggles follow the normal anchor arithmetic.
	enabled.Store(false)
	broadcast()
	enabled.Store(true)
	broadcast()
	select {
	case <-runs:
		t.Fatal("a second enable toggle fired an immediate run; the startup slot is per boot")
	case <-time.After(150 * time.Millisecond):
	}

	if got := stats.Lifetime().Counters["speed.run.startup"]; got != 1 {
		t.Errorf("speed.run.startup = %d, want exactly 1", got)
	}
}

// A boot with the scheduler enabled claims the startup slot immediately, as it
// always has - and having claimed it, a later off/on toggle must not fire an
// extra run.
func TestSchedulerEnabledBootConsumesStartupSlot(t *testing.T) {
	stats.ResetForTest()
	defer func(d, f time.Duration) { startupDelay, firstEnableDelay = d, f }(startupDelay, firstEnableDelay)
	startupDelay = 0
	firstEnableDelay = 0

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	enabled := atomic.Bool{}
	enabled.Store(true)
	s.EnabledFn = func() bool { return enabled.Load() }
	var wakeMu sync.Mutex
	wakeCh := make(chan struct{})
	s.WakeFn = func() <-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return wakeCh }
	broadcast := func() { wakeMu.Lock(); close(wakeCh); wakeCh = make(chan struct{}); wakeMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()

	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("enabled boot did not run the startup test")
	}

	enabled.Store(false)
	broadcast()
	enabled.Store(true)
	broadcast()
	select {
	case <-runs:
		t.Fatal("off/on toggle after a claimed startup slot fired an extra run")
	case <-time.After(150 * time.Millisecond):
	}
}

// A run that completes while the startup slot is still armed SERVES it - the
// latch must not stack a second full test onto a link still settling from the
// first. This is the runWake path: gated boot (e.g. a restart outside the
// schedule window), an out-of-band run (manual/reconnect) completes, and a
// later enable must NOT fire an immediate startup run.
func TestSchedulerCompletedRunServesArmedSlot(t *testing.T) {
	stats.ResetForTest()
	defer func(d, f time.Duration) { startupDelay, firstEnableDelay = d, f }(startupDelay, firstEnableDelay)
	startupDelay = 0
	firstEnableDelay = 0

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	var enabled atomic.Bool
	s.EnabledFn = func() bool { return enabled.Load() }
	var wakeMu sync.Mutex
	wakeCh := make(chan struct{})
	s.WakeFn = func() <-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return wakeCh }
	broadcast := func() { wakeMu.Lock(); close(wakeCh); wakeCh = make(chan struct{}); wakeMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()
	waitForAnchor(t, s) // boot completed gated; the slot is armed

	// An out-of-band run completes (manual ignores EnabledFn); its runWake
	// nudge reaches the loop whichever select it is sleeping in.
	if _, err := s.RunOnce(ctx, "manual"); err != nil {
		t.Fatalf("manual run: %v", err)
	}
	<-runs
	time.Sleep(50 * time.Millisecond) // let the loop drain the nudge

	// Enabling now must not fire a startup run: the measurement exists.
	enabled.Store(true)
	broadcast()
	select {
	case <-runs:
		t.Fatal("latch fired a duplicate test right after a completed run")
	case <-time.After(300 * time.Millisecond):
	}
	if got := stats.Lifetime().Counters["speed.run.startup"]; got != 0 {
		t.Errorf("speed.run.startup = %d, want 0 (the manual run served the slot)", got)
	}
}

// The same rule inside the pause: a run that completes during firstEnableDelay
// serves the slot (the post-pause drain), and the deadline withdrawn at latch
// entry must be republished - not left absent for the rest of the interval.
func TestSchedulerRunDuringPauseServesSlot(t *testing.T) {
	stats.ResetForTest()
	defer func(d, f time.Duration) { startupDelay, firstEnableDelay = d, f }(startupDelay, firstEnableDelay)
	startupDelay = 0
	firstEnableDelay = 250 * time.Millisecond

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	var enabled atomic.Bool
	s.EnabledFn = func() bool { return enabled.Load() }
	var wakeMu sync.Mutex
	wakeCh := make(chan struct{})
	s.WakeFn = func() <-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return wakeCh }
	broadcast := func() { wakeMu.Lock(); close(wakeCh); wakeCh = make(chan struct{}); wakeMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()
	waitForAnchor(t, s)

	// Consent arms the latch; a reconnect-style run completes inside the pause.
	enabled.Store(true)
	broadcast()
	time.Sleep(50 * time.Millisecond) // loop is now inside the pause
	if _, err := s.RunOnce(ctx, "reconnect"); err != nil {
		t.Fatalf("reconnect run: %v", err)
	}
	<-runs

	// The pause ends: no second test, and the slot is spent, not kept.
	select {
	case <-runs:
		t.Fatal("latch stacked a startup test onto the run that completed during the pause")
	case <-time.After(500 * time.Millisecond):
	}
	broadcast() // a later settings save must not revive it either
	select {
	case <-runs:
		t.Fatal("the served slot survived; a later broadcast fired a startup run")
	case <-time.After(150 * time.Millisecond):
	}
	if got := stats.Lifetime().Counters["speed.run.startup"]; got != 0 {
		t.Errorf("speed.run.startup = %d, want 0", got)
	}
	// The withdrawn deadline came back (the else branch republishes the
	// boot-anchored one).
	if s.anchor.Load() == nil {
		t.Error("anchor left withdrawn after the slot was served during the pause")
	}
}

// A toggle straight back off during the firstEnableDelay pause must not burn
// the startup slot on a run that never happened: the re-check after the pause
// declines, and a LATER consent still gets its first run.
func TestSchedulerEnableToggleDuringPauseKeepsSlot(t *testing.T) {
	stats.ResetForTest()
	defer func(d, f time.Duration) { startupDelay, firstEnableDelay = d, f }(startupDelay, firstEnableDelay)
	startupDelay = 0
	firstEnableDelay = 200 * time.Millisecond

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	runs := make(chan struct{}, 16)
	tester := testerFunc(func(context.Context) (Result, error) {
		runs <- struct{}{}
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, time.Hour, log)

	var enabled atomic.Bool
	s.EnabledFn = func() bool { return enabled.Load() }
	var wakeMu sync.Mutex
	wakeCh := make(chan struct{})
	s.WakeFn = func() <-chan struct{} { wakeMu.Lock(); defer wakeMu.Unlock(); return wakeCh }
	broadcast := func() { wakeMu.Lock(); close(wakeCh); wakeCh = make(chan struct{}); wakeMu.Unlock() }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() { defer close(done); s.Loop(ctx) }()

	// Boot must complete gated before the test enables, or the Store below can
	// beat the loop's own boot check and turn this into an enabled boot. The
	// anchor is published right after that check, so it is the barrier.
	waitForAnchor(t, s)

	// Enable, let the loop enter the pause, then flip off inside it. (If the
	// flip beats the loop's own check instead, the latch never arms - the
	// assertions below hold on either interleaving.)
	enabled.Store(true)
	broadcast()
	time.Sleep(50 * time.Millisecond)
	enabled.Store(false)

	select {
	case <-runs:
		t.Fatal("speedtest ran although consent was withdrawn during the pause")
	case <-time.After(400 * time.Millisecond):
	}

	// The slot survived: consenting again gets the first run.
	enabled.Store(true)
	broadcast()
	select {
	case <-runs:
	case <-time.After(2 * time.Second):
		t.Fatal("slot was burned by the withdrawn consent; re-enable got no first run")
	}
}

// waitForAnchor polls until Loop has published its deadline anchor and NextRun
// reports a real time.
func waitForAnchor(t *testing.T, s *Scheduler) time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nr := s.NextRun(); !nr.IsZero() {
			return nr
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Loop never published a NextRun anchor")
	return time.Time{}
}

// NextRun must be zero until Loop publishes its anchor, then report the same
// deadline Loop waits on: anchor + the live interval (IntervalFn) + that cycle's
// bounded jitter. This backs the dashboard's "next speedtest" time.
func TestSchedulerNextRun(t *testing.T) {
	defer func(d time.Duration) { startupDelay = d }(startupDelay)
	startupDelay = 0

	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 500, UploadMbps: 30, PingMS: 10, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.IntervalFn = func() time.Duration { return time.Hour }

	if got := s.NextRun(); !got.IsZero() {
		t.Fatalf("NextRun before Loop = %v, want zero", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := time.Now()
	go s.Loop(ctx)
	nr := waitForAnchor(t, s)
	after := time.Now()

	// The anchor is the startup run's end (stored at 1s granularity); the jitter
	// for a 1h interval is [0, 60s).
	lo, hi := before.Add(time.Hour-time.Second), after.Add(time.Hour+60*time.Second)
	if nr.Before(lo) || nr.After(hi) {
		t.Fatalf("NextRun = %v, want within [%v, %v]", nr, lo, hi)
	}
}

// While the last run breached a threshold and adaptive cadence is on, NextRun
// reports the shortened deadline (a 1h base collapses to the 5m adaptiveCap),
// not the base interval.
func TestSchedulerNextRunAdaptive(t *testing.T) {
	defer func(d time.Duration) { startupDelay = d }(startupDelay)
	startupDelay = 0

	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.IntervalFn = func() time.Duration { return time.Hour }
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // startup run breaches
	s.AdaptiveFn = func() bool { return true }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := time.Now()
	go s.Loop(ctx)
	nr := waitForAnchor(t, s)
	after := time.Now()

	// curInterval is adaptiveCap (1h/4, capped to 5m); its jitter is [0, 30s).
	lo, hi := before.Add(adaptiveCap-time.Second), after.Add(adaptiveCap+adaptiveCap/10)
	if nr.Before(lo) || nr.After(hi) {
		t.Fatalf("adaptive NextRun = %v, want the shortened deadline within [%v, %v]", nr, lo, hi)
	}
}

// The loss-capability gauge must read 0 when the per-server cooldown skips the
// probe (the measurement produced no value).
func TestPacketLossSkipCounters(t *testing.T) {
	stats.ResetForTest()
	const host = "plgauge.test:8080"
	plMu.Lock()
	plMap[host] = &plState{fails: 2, skipUntil: time.Now().Add(time.Hour).Unix()}
	plMu.Unlock()
	defer func() { plMu.Lock(); delete(plMap, host); plMu.Unlock() }()

	if got := measurePacketLoss(context.Background(), &ookla.Server{Host: host}); got != nil {
		t.Fatalf("cooldown skip should yield nil loss, got %v", *got)
	}
	if got := stats.Lifetime().Counters["speed.loss_skip"]; got != 1 {
		t.Fatalf("speed.loss_skip = %d, want 1", got)
	}

	// A caller-cancelled probe must not count an outcome or advance the
	// cooldown - it says nothing about the server's UDP support.
	stats.ResetForTest()
	const host2 = "plcancel.test:8080"
	defer func() { plMu.Lock(); delete(plMap, host2); plMu.Unlock() }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := measurePacketLoss(ctx, &ookla.Server{Host: host2}); got != nil {
		t.Fatalf("cancelled probe should yield nil, got %v", *got)
	}
	s := stats.Lifetime()
	if s.Counters["speed.loss_none"] != 0 || s.Counters["speed.loss_ok"] != 0 {
		t.Fatalf("cancelled probe counted an outcome: %+v", s.Counters)
	}
	plMu.Lock()
	st := plMap[host2]
	plMu.Unlock()
	if st != nil && st.fails != 0 {
		t.Fatalf("cancelled probe advanced the cooldown: fails=%d", st.fails)
	}
}

// B5: a run that measured NONE of the quantities the active thresholds cover must
// not be judged. thresholdsMeasurable is the guard RunOnce uses to tell "nothing
// checked" from "everything passed" - without it, such a run is recorded green
// and clears a real breach streak. Regression proof: make thresholdsMeasurable
// always return true and the first case flips to a wrong "measurable".
func TestThresholdsMeasurable(t *testing.T) {
	dB := int64(5_000_000)
	// Only the upload threshold is active, and upload was not measured (nil bytes):
	// nothing to judge.
	sp := store.SpeedSample{DownMbps: 500, DownBytes: &dB, UpBytes: nil}
	if thresholdsMeasurable(sp, settings.Thresholds{UpMbps: 20}) {
		t.Error("upload-only threshold with unmeasured upload must be unmeasurable")
	}
	// Download threshold, download measured: judgeable.
	if !thresholdsMeasurable(sp, settings.Thresholds{DownMbps: 100}) {
		t.Error("download threshold with measured download must be measurable")
	}
	// A measured ping makes a mixed set judgeable even when a direction was not
	// measured.
	pinged := store.SpeedSample{DownMbps: 500, DownBytes: &dB, UpBytes: nil, PingMS: 12}
	if !thresholdsMeasurable(pinged, settings.Thresholds{UpMbps: 20, PingMS: 50}) {
		t.Error("a set with a measured ping must be measurable")
	}
	// The iperf3 engine can leave PingMS at 0 when its handshake probe, stream RTT,
	// and idle anchor all fail. A ping-only active threshold on such a run measured
	// nothing, so it must NOT be judged (else it records green and clears a real
	// breach streak).
	if thresholdsMeasurable(sp, settings.Thresholds{PingMS: 50}) {
		t.Error("ping threshold with an unmeasured ping (0) must be unmeasurable")
	}
	// A pointer-gated metric that was not captured is not measurable on its own.
	if thresholdsMeasurable(sp, settings.Thresholds{JitterMS: 10}) {
		t.Error("jitter threshold with nil jitter must be unmeasurable")
	}
	if thresholdsMeasurable(sp, settings.Thresholds{BloatDownMS: 50}) {
		t.Error("bloat threshold with no idle/loaded capture must be unmeasurable")
	}
}

// NextRun must track Loop's monotonic deadline, not a whole-second wall-clock
// truncation of the anchor: with the anchor at now and no jitter, the reported
// due time is one interval away to within a few ms. The old Unix()-based anchor
// both dropped sub-second precision and could skew by any wall-clock step.
func TestSchedulerNextRunTracksMonotonicDeadline(t *testing.T) {
	s := &Scheduler{IntervalFn: func() time.Duration { return time.Hour }}
	if got := s.NextRun(); !got.IsZero() {
		t.Fatalf("NextRun before an anchor = %v, want zero", got)
	}
	s.setAnchor(time.Now(), 0)
	remaining := time.Until(s.NextRun())
	if d := (remaining - time.Hour).Abs(); d > 50*time.Millisecond {
		t.Fatalf("NextRun is %v away, want ~1h (off by %v)", remaining, d)
	}
}

// A SCHEDULED SLOT LOST TO A COLLISION MUST BE VISIBLE. When another trigger
// already holds the single-flight, the scheduled run is refused and its slot is
// not retried - the anchor advances as though it had run, so the next one is a
// whole interval away. That is the right call (a measurement IS in flight, and
// firing again the moment it releases would spend a second full test on a link
// still settling), but it is also the one path that can leave a gap in an
// otherwise regular history, so it must not be silent.
//
// speed.errbusy cannot answer it: reconnect and degraded collisions share that
// counter and lose nothing by being dropped.
func TestScheduledRunSkippedByACollisionIsCounted(t *testing.T) {
	stats.ResetForTest()
	defer func(d, r time.Duration) { startupDelay, scheduleRecheck = d, r }(startupDelay, scheduleRecheck)
	startupDelay = 0
	scheduleRecheck = 5 * time.Millisecond

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// A manual run that holds the flag until the test releases it.
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	tester := testerFunc(func(context.Context) (Result, error) {
		once.Do(func() { close(entered) })
		<-release
		return Result{DownloadMbps: 1, Server: "fake"}, nil
	})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := NewScheduler(tester, st, 20*time.Millisecond, log)

	ctx, cancel := context.WithCancel(context.Background())
	manualDone := make(chan struct{})
	go func() { defer close(manualDone); s.RunOnce(ctx, "manual") }()
	<-entered // the manual run now holds the single-flight

	loopDone := make(chan struct{})
	go func() { defer close(loopDone); s.Loop(ctx) }()

	// The schedule comes due while the manual run still holds the flag.
	deadline := time.After(3 * time.Second)
	for stats.Lifetime().Counters["speed.scheduled_skipped"] == 0 {
		select {
		case <-deadline:
			cancel()
			close(release)
			<-manualDone
			<-loopDone
			t.Fatal("a scheduled run collided with the manual run but nothing recorded the lost slot")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	close(release)
	<-manualDone
	<-loopDone
}
