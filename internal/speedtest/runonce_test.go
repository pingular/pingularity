package speedtest

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func newRunOnceScheduler(t *testing.T, r Result) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(context.Context) (Result, error) { return r, nil })
	return NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

// A completed run must capture the connection context, evaluate thresholds,
// persist, and fire OnUnhealthy when it fails.
func TestRunOnceRecordsAndAlerts(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1", DownloadBytes: 5_000_000, UploadBytes: 1_000_000})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // min 100 Mbps down -> fails
	s.ConnInfoFn = func(context.Context) ConnInfo { return ConnInfo{ISP: "AS1 Foo", PublicIPv4: "1.2.3.4"} }
	var alerted []string
	s.OnUnhealthy = func(sp store.SpeedSample, failures []string) { alerted = failures }

	sp, err := s.RunOnce(context.Background(), "manual")
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sp.ISP != "AS1 Foo" || sp.PublicIPv4 != "1.2.3.4" {
		t.Errorf("ConnInfoFn context not captured: %+v", sp)
	}
	if sp.Healthy == nil || *sp.Healthy {
		t.Error("run failing a threshold must be marked unhealthy")
	}
	if len(alerted) == 0 {
		t.Error("OnUnhealthy must fire with the failure list")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed"] != 1 {
		t.Errorf("speed rows = %d, want 1", cnt["speed"])
	}
}

// A run whose ctx is already cancelled (shutdown / client disconnect) must NOT
// persist (keeps the WAL crash-consistent) and must NOT count as a failure.
func TestRunOnceCancelledDoesNotPersist(t *testing.T) {
	stats.ResetForTest()
	s, st := newRunOnceScheduler(t, Result{DownloadMbps: 5, Server: "S"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RunOnce(ctx, "manual"); err == nil {
		t.Fatal("cancelled run should return an error")
	}
	if cnt, _ := st.TableCounts(context.Background()); cnt["speed"] != 0 {
		t.Errorf("cancelled run must not persist, got %d speed rows", cnt["speed"])
	}
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("cancelled run must not count as speed.fail, got %d", got)
	}
}

// A run whose result cannot be persisted (the store is closed) must report the
// failure and must NOT advance the breach streak / adaptive cadence or fire an
// alert - a restart re-evaluates from durable history and would disagree with an
// alert sent for a run no row can back.
func TestRunOnceReportsPersistFailure(t *testing.T) {
	stats.ResetForTest()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	tester := testerFunc(func(context.Context) (Result, error) {
		return Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 5_000_000, UploadBytes: 1_000_000}, nil
	})
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // breaches
	var alerts int
	s.OnUnhealthy = func(store.SpeedSample, []string) { alerts++ }
	st.Close() // now every InsertSpeed fails

	_, err = s.RunOnce(context.Background(), "manual")
	if err == nil {
		t.Fatal("a run that could not be persisted must return an error")
	}
	if alerts != 0 {
		t.Fatalf("must not alert on a run that was never stored, got %d alerts", alerts)
	}
	if s.lastUnhealthy.Load() || s.consecBreach != 0 {
		t.Fatalf("must not advance breach/adaptive state on a persist failure: unhealthy=%v consec=%d",
			s.lastUnhealthy.Load(), s.consecBreach)
	}
	// The persist failure must not masquerade as a measurement failure.
	if got := stats.Lifetime().Counters["speed.fail"]; got != 0 {
		t.Errorf("persist failure must not count as speed.fail, got %d", got)
	}
}

// With a consecutive-breach streak of N, a breach must NOT alert until it has
// persisted for N runs, and a recovered run resets the counter.
func TestRunOnceDebouncesAlerts(t *testing.T) {
	stats.ResetForTest()
	// A mutable result so we can flip between breaching and healthy runs.
	res := Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1", DownloadBytes: 5_000_000, UploadBytes: 1_000_000}
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	tester := testerFunc(func(context.Context) (Result, error) { return res, nil })
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // min 100 Mbps down
	s.BreachStreakFn = func() int { return 3 }
	var alerts int
	s.OnUnhealthy = func(store.SpeedSample, []string) { alerts++ }

	run := func() {
		if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
	}

	run() // breach 1/3 - no alert
	run() // breach 2/3 - no alert
	if alerts != 0 {
		t.Fatalf("must not alert before the streak is reached, got %d alerts", alerts)
	}
	run() // breach 3/3 - alert
	if alerts != 1 {
		t.Fatalf("must alert when the streak is reached, got %d alerts", alerts)
	}
	run() // breach 4 - keeps alerting once debounced
	if alerts != 2 {
		t.Fatalf("must keep alerting past the streak, got %d alerts", alerts)
	}

	// A healthy run resets the counter; the next breach must re-arm the streak.
	res.DownloadMbps = 500
	run() // healthy - counter resets, no alert
	res.DownloadMbps = 5
	run() // breach 1/3 again - no alert
	if alerts != 2 {
		t.Fatalf("recovery must reset the streak, got %d alerts", alerts)
	}
}

// Every completed run must leave a nudge on runWake, so a breach found by a
// reconnect/degraded/manual run (on another goroutine) engages the adaptive
// cadence right away instead of after the already-armed base-interval sleep.
func TestRunOnceNudgesLoop(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // breaches
	if _, err := s.RunOnce(context.Background(), "reconnect"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.runWake:
	default:
		t.Fatal("a completed run must nudge runWake for the schedule loop")
	}
}

// Clearing all thresholds mid-breach must reset the adaptive-cadence state.
// lastUnhealthy only updates while thresholds are active, and curInterval keys
// off it - so a stale "unhealthy" would otherwise pin the fast interval forever.
func TestRunOnceClearsAdaptiveWhenThresholdsRemoved(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1})
	s.AdaptiveFn = func() bool { return true }

	// A breaching run latches lastUnhealthy and speeds up the cadence.
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} }
	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if !s.lastUnhealthy.Load() {
		t.Fatal("setup: a breaching run should latch lastUnhealthy")
	}
	if s.curInterval() >= time.Hour {
		t.Fatalf("setup: adaptive cadence should have sped up, got %v", s.curInterval())
	}

	// Operator clears every threshold; the next run is unevaluated (Healthy==nil).
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{} }
	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if s.lastUnhealthy.Load() || s.consecBreach != 0 {
		t.Fatalf("clearing thresholds must reset breach state: unhealthy=%v consec=%d",
			s.lastUnhealthy.Load(), s.consecBreach)
	}
	if got := s.curInterval(); got != time.Hour {
		t.Fatalf("adaptive cadence stuck fast after thresholds cleared: %v, want 1h", got)
	}
}

// Pins that a recovered link stops sampling at the fast adaptive cadence once
// its measurable thresholds pass, even while an enabled loss threshold stays
// permanently unmeasurable (e.g. a server that never supports the UDP probe).
// The no-verdict branch used to leave lastUnhealthy latched from the original
// breach, pinning up to 12x the base rate forever. The breach STREAK is
// deliberately preserved: an unran check can neither clear a genuine breach
// nor record the run green - only the cadence backs off. Contrast the sibling
// TestRunOnceClearsAdaptiveWhenThresholdsRemoved, which covers the
// cleared-thresholds path.
func TestRunOnceUnmeasurableThresholdDoesNotPinAdaptiveCadenceAfterRecovery(t *testing.T) {
	stats.ResetForTest()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	res := Result{DownloadMbps: 50, UploadMbps: 10, PingMS: 20, Server: "S", DownloadBytes: 1, UploadBytes: 1}
	tester := testerFunc(func(context.Context) (Result, error) { return res, nil })
	s := NewScheduler(tester, st, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.AdaptiveFn = func() bool { return true }
	// A download floor the first run breaches, plus a loss limit no run can
	// measure (PacketLoss stays nil - the probe is unsupported on this server).
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100, LossPct: 1} }

	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if !s.lastUnhealthy.Load() || s.curInterval() != adaptiveCap || s.consecBreach != 1 {
		t.Fatalf("setup: breach should latch the fast cadence: unhealthy=%v interval=%v consec=%d",
			s.lastUnhealthy.Load(), s.curInterval(), s.consecBreach)
	}

	// The link recovers; the loss threshold is still unmeasurable, so the run
	// gets no verdict - but its MEASURABLE thresholds all passed.
	res.DownloadMbps = 500
	if _, err := s.RunOnce(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if s.lastUnhealthy.Load() || s.curInterval() != time.Hour {
		t.Fatalf("cadence stayed pinned after a measured-clean no-verdict run: unhealthy=%v interval=%v, want false/1h",
			s.lastUnhealthy.Load(), s.curInterval())
	}
	if s.consecBreach != 1 {
		t.Fatalf("consecBreach = %d, want 1 (the streak must be preserved, not cleared, by an unran check)", s.consecBreach)
	}
}

// OnUnhealthy is an operator webhook that can be slow or dead. It must be
// dispatched only AFTER RunOnce releases the single-flight flag, so a manual run
// fired while the alert is stuck proceeds instead of wrongly returning ErrBusy.
// Exactly one notification must go out per breaching run.
func TestRunOnceReleasesFlagBeforeAlert(t *testing.T) {
	stats.ResetForTest()
	s, _ := newRunOnceScheduler(t, Result{
		DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1",
		DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
	})
	s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // 5 < 100 -> breach

	var alerts int32
	inAlert := make(chan struct{})
	release := make(chan struct{})
	s.OnUnhealthy = func(store.SpeedSample, []string) {
		if atomic.AddInt32(&alerts, 1) == 1 {
			// Model a dead endpoint: hold the FIRST alert open until the test lets go.
			close(inAlert)
			<-release
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.RunOnce(context.Background(), "scheduled")
		done <- err
	}()
	<-inAlert // the first run has measured, persisted, and is now stuck in OnUnhealthy

	// The flag must already be down (the alert fires only after its release), so a
	// manual run during the stuck webhook must be allowed, not bounced with ErrBusy.
	if s.Running() {
		t.Fatal("single-flight flag still held while OnUnhealthy is in flight")
	}
	if _, err := s.RunOnce(context.Background(), "manual"); err != nil {
		t.Fatalf("run during a slow/dead alert must be allowed, got %v", err)
	}

	close(release) // let the first run's webhook return
	if err := <-done; err != nil {
		t.Fatalf("first run: %v", err)
	}
	// One notification per breaching run: the scheduled run and the manual run each
	// breached and each alerted exactly once.
	if got := atomic.LoadInt32(&alerts); got != 2 {
		t.Fatalf("alerts = %d, want 2 (exactly one per breaching run)", got)
	}
}

// A lost challenge measured a rival, not the seat holder: its breach is a fact
// about that rival and must neither page nor be recorded as the line's
// verdict. A challenge the rival WON measured the new seat holder and is
// judged like any run.
func TestRunOnceDoesNotAlertOnALostChallengersBreach(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		wantAlert bool
	}{
		{WinReasonChallenger, false},
		{WinReasonChallengerWon, true},
		{WinReasonChallengerFailed, true},
		{winReasonFastestRank, true},
	} {
		stats.ResetForTest()
		res := Result{DownloadMbps: 5, UploadMbps: 1, PingMS: 20, Server: "S", ServerID: "1", DownloadBytes: 5_000_000, UploadBytes: 1_000_000,
			Selection: &SelectionReport{Candidates: []CandidateReport{{ServerID: "1", Server: "S", RankOrder: 1, Selected: true, Measured: true, Winner: true, WinReason: tc.reason}}}}
		s, _ := newRunOnceScheduler(t, res)
		s.ThresholdsFn = func() settings.Thresholds { return settings.Thresholds{DownMbps: 100} } // min 100 Mbps down -> fails
		var alerted []string
		s.OnUnhealthy = func(sp store.SpeedSample, failures []string) { alerted = failures }
		sp, err := s.RunOnce(context.Background(), "scheduled")
		if err != nil {
			t.Fatalf("%s: RunOnce: %v", tc.reason, err)
		}
		if tc.wantAlert {
			if sp.Healthy == nil || *sp.Healthy || len(alerted) == 0 {
				t.Errorf("%s: a breach on the seat holder must be recorded unhealthy and alert (healthy=%v alerted=%v)", tc.reason, sp.Healthy, alerted)
			}
			if !s.lastUnhealthy.Load() {
				t.Errorf("%s: the adaptive cadence must see the breach", tc.reason)
			}
		} else {
			if sp.Healthy != nil || len(alerted) != 0 {
				t.Errorf("%s: a lost challenger's breach is not a verdict on the line (healthy=%v alerted=%v)", tc.reason, sp.Healthy, alerted)
			}
			if s.lastUnhealthy.Load() || s.consecBreach != 0 {
				t.Errorf("%s: it must not pin the cadence or extend the breach streak", tc.reason)
			}
		}
	}
}
