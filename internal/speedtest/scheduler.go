package speedtest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/util"
)

// ErrBusy is returned by RunOnce when a measurement is already in progress, so
// callers can tell "already running" (HTTP 409) from a real failure.
var ErrBusy = errors.New("a speedtest is already in progress")

// Scheduler runs speedtests on a fixed interval, on reconnect, and on demand,
// persisting each result. A single-flight guard keeps tests from overlapping (a
// reconnect during a scheduled run is just skipped).
type Scheduler struct {
	tester    Tester
	store     *store.Store
	log       *slog.Logger
	interval  time.Duration
	running   atomic.Bool
	curServer atomic.Value // string: server of the in-progress run ("" when idle)

	// IntervalFn, if set, supplies the schedule interval live so it can change at
	// runtime. Falls back to the fixed interval when nil.
	IntervalFn func() time.Duration

	// WakeFn, if set, returns a channel closed when settings change, so an
	// interval change takes effect without waiting out the current cycle.
	WakeFn func() <-chan struct{}

	// EnabledFn gates the scheduler's own runs - the scheduled interval and the
	// startup run (speedtest on/off and the master monitoring switch). Reconnect,
	// degraded, and manual runs ignore it; their own callers gate them. TesterFn, if set,
	// selects the tester live (e.g. by engine) instead of the constructed one.
	EnabledFn func() bool
	TesterFn  func() Tester

	// ConnInfoFn, if set, is called after each run to capture the connection
	// context (public IP/ISP/DNS) stored with the result. May refresh that info
	// as a side effect.
	ConnInfoFn func(context.Context) ConnInfo

	// ThresholdsFn, if set, supplies the speed thresholds (0-valued fields are
	// unchecked) used to mark each run healthy/unhealthy.
	ThresholdsFn func() settings.Thresholds

	// BreachStreakFn, if set, supplies how many consecutive breaching runs must
	// occur before OnUnhealthy fires (>=1; 1 = every breach). Debounces single
	// blips. Falls back to alerting on every breach when nil.
	BreachStreakFn func() int

	// OnUnhealthy, if set, fires (to send alerts) when a run fails one or more
	// thresholds AND the breach has persisted for BreachStreakFn() runs.
	OnUnhealthy func(sp store.SpeedSample, failures []string)

	// AdaptiveFn, if set and true, shortens the scheduled interval while the last
	// completed run breached a threshold (see curInterval). Needs ThresholdsFn to
	// have something to fail against.
	AdaptiveFn func() bool

	// BusyFn, if set, reports whether the link is already moving significant data;
	// when true a *scheduled* run is deferred (re-checked shortly) so the test
	// neither competes with real traffic nor measures a saturated link. Reconnect,
	// degraded, and manual runs ignore it.
	BusyFn func() bool

	// consecBreach counts consecutive breaching runs. Mutated only inside RunOnce,
	// which the running CAS serializes, so a plain int is safe.
	consecBreach int

	// lastUnhealthy mirrors whether the last threshold-evaluated run breached, and
	// drives the adaptive cadence (curInterval). The Loop goroutine reads it while
	// RunOnce (possibly on another goroutine: reconnect/degraded) writes it - hence
	// atomic. Reset to false when a run finds no active thresholds, so clearing
	// thresholds mid-breach can't pin the fast cadence forever (see RunOnce).
	lastUnhealthy atomic.Bool

	// runWake (capacity 1) nudges Loop after every completed run, so a breach
	// detected by a reconnect/degraded/manual run engages the adaptive cadence
	// right away instead of after the already-armed base-interval sleep.
	runWake chan struct{}

	// anchor publishes Loop's deadline reference so NextRun can report the same
	// deadline Loop is waiting on. It holds lastRun WITH its monotonic reading
	// (plus the cycle's jitter), so NextRun stays on the monotonic clock Loop
	// uses and a wall-clock step can't skew the reported due time. Written by
	// Loop, read by /api/status.
	anchor atomic.Pointer[schedAnchor]
}

// schedAnchor is Loop's published deadline reference (see Scheduler.anchor).
type schedAnchor struct {
	lastRun time.Time // carries its monotonic reading
	jitter  time.Duration
}

// ConnInfo is the connection context recorded with each speedtest.
type ConnInfo struct {
	PublicIPv4  string
	PublicIPv6  string
	ISP         string
	ISPLocation string
	DNSIP       string
	DNSProvider string
	DNSLocation string
	CFColo      string // Cloudflare PoP
	ExitSummary string // exit router → handoff path
}

func (s *Scheduler) enabled() bool {
	return s.EnabledFn == nil || s.EnabledFn()
}

// Running reports whether a speedtest is in progress (any trigger). Safe for
// concurrent use.
func (s *Scheduler) Running() bool { return s.running.Load() }

// SetCurrentServer records the server the in-progress run selected (called by
// the tester). CurrentServer returns it ("" when idle or not yet selected).
func (s *Scheduler) SetCurrentServer(name string) {
	s.curServer.Store(name)
	if name != "" {
		s.log.Debug("speedtest server selected", "server", name)
	}
}
func (s *Scheduler) CurrentServer() string {
	if v := s.curServer.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (s *Scheduler) curTester() Tester {
	if s.TesterFn != nil {
		if t := s.TesterFn(); t != nil {
			return t
		}
	}
	return s.tester
}

// NewScheduler builds a Scheduler.
func NewScheduler(t Tester, st *store.Store, interval time.Duration, log *slog.Logger) *Scheduler {
	return &Scheduler{tester: t, store: st, log: log, interval: interval, runWake: make(chan struct{}, 1)}
}

// adaptiveFactor / adaptiveCap shape the sped-up cadence while a breach persists:
// base/4, but no slower than adaptiveCap (so even a 1h base samples densely
// during a problem) and no faster than the configured floor (settings.MinSpeed).
const (
	adaptiveFactor = 4
	adaptiveCap    = 5 * time.Minute
)

func (s *Scheduler) curInterval() time.Duration {
	base := s.interval
	if s.IntervalFn != nil {
		if d := s.IntervalFn(); d > 0 {
			base = d
		}
	}
	// Adaptive cadence: while the last completed run breached a threshold, poll
	// faster; back to base as soon as a run is healthy (or thresholds are cleared,
	// which resets lastUnhealthy in RunOnce).
	if s.AdaptiveFn != nil && s.AdaptiveFn() && s.lastUnhealthy.Load() {
		fast := base / adaptiveFactor
		if fast > adaptiveCap {
			fast = adaptiveCap
		}
		if fast < settings.MinSpeed {
			fast = settings.MinSpeed
		}
		if fast < base {
			return fast
		}
	}
	return base
}

// busy reports whether a scheduled run should defer because the link is already
// moving data (manual/reconnect/degraded runs don't consult this).
func (s *Scheduler) busy() bool { return s.BusyFn != nil && s.BusyFn() }

// setAnchor publishes Loop's deadline anchor for NextRun.
func (s *Scheduler) setAnchor(lastRun time.Time, jitter time.Duration) {
	s.anchor.Store(&schedAnchor{lastRun: lastRun, jitter: jitter})
}

// NextRun reports when the next scheduled run is due - Loop's anchor plus the
// current interval and jitter, the same deadline Loop waits on. Zero before
// Loop has started; a past value means the run is due but deferred (schedule
// window closed or link busy) and will fire as soon as that clears.
func (s *Scheduler) NextRun() time.Time {
	a := s.anchor.Load()
	if a == nil {
		return time.Time{}
	}
	// lastRun keeps its monotonic reading, so the deadline and time.Until are
	// measured on the monotonic clock (the one Loop's wait uses); re-expressing it
	// through Now converts to wall time without inheriting a wall-clock step.
	deadline := a.lastRun.Add(s.curInterval() + a.jitter)
	return time.Now().Add(time.Until(deadline))
}

// speedFailStage maps a speedtest error to the stage it failed at - a closed
// enum for the fleet failure histogram (no detail or host text included). The
// stages mirror the error prefixes the Ookla tester returns (see ookla.go).
func speedFailStage(err error) string {
	s := err.Error()
	switch {
	case strings.HasPrefix(s, "fetch server list"):
		return "server_list"
	case strings.HasPrefix(s, "fetch server "):
		return "server_fetch"
	case strings.HasPrefix(s, "no speedtest servers"):
		return "no_servers"
	case strings.HasPrefix(s, "ping:"):
		return "ping"
	case errors.Is(err, errMeasurementNA):
		// Checked before the download:/upload: prefixes it now wraps, so an
		// N/A transfer stays distinct from other transfer failures.
		return "na"
	case strings.HasPrefix(s, "download:"):
		return "download"
	case strings.HasPrefix(s, "upload:"):
		return "upload"
	case strings.HasPrefix(s, "bidir:"):
		return "bidir" // iperf3 --bidir moves both directions in one transfer
	default:
		return "other"
	}
}

// RunOnce performs a single measurement unless one is already in progress, in
// which case it returns ErrBusy. Any other error is the measurement failure
// itself. On success the result is persisted and returned.
func (s *Scheduler) RunOnce(ctx context.Context, reason string) (store.SpeedSample, error) {
	if !s.running.CompareAndSwap(false, true) {
		stats.Inc("speed.errbusy") // collisions, e.g. a reconnect during a scheduled test
		s.log.Debug("speedtest skipped: already running", "reason", reason)
		return store.SpeedSample{}, ErrBusy
	}
	defer s.running.Store(false)
	s.curServer.Store("")       // not selected yet
	defer s.curServer.Store("") // clear when the run ends
	s.log.Debug("speedtest start", "reason", reason)

	// Run accounting (what triggers tests, how often they fail, how long an attempt
	// takes), as speed.run.* counters on /metrics. reason is the closed trigger enum
	// scheduled|manual|reconnect|startup|degraded - never user input.
	stats.Inc("speed.run." + reason)
	start := time.Now()
	defer func() {
		stats.AddF("speed.duration_s_sum", time.Since(start).Seconds())
		stats.Inc("speed.duration_n")
	}()

	s.log.Info("speedtest started", "reason", reason)
	res, err := runTester(ctx, s.curTester(), reason)
	if err != nil {
		// A caller-aborted run (browser disconnect mid-test, shutdown) isn't a
		// measurement failure - keep the fleet failure rate honest.
		if ctx.Err() == nil {
			stats.Inc("speed.fail")
			stats.Inc("speed.fail." + speedFailStage(err)) // which stage (fleet diagnostics)
		}
		s.log.Error("speedtest failed", "reason", reason, "server", s.CurrentServer(), "err", err)
		return store.SpeedSample{}, fmt.Errorf("speedtest: %w", err)
	}

	sp := store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: res.DownloadMbps, UpMbps: res.UploadMbps,
		PingMS: res.PingMS, JitterMS: res.JitterMS, Server: res.Server, ServerID: res.ServerID,
		// nil bytes = direction not measured (iperf3 best-effort partial), so it
		// reads as "unknown", not a real 0, in the chart/table/thresholds.
		PacketLoss: res.PacketLoss, DownBytes: bytesPtr(res.DownloadBytes), UpBytes: bytesPtr(res.UploadBytes),
		Trigger:         reason,
		Engine:          res.Engine,
		IdleMS:          res.IdleMS,
		LoadedDownMS:    res.LoadedDownMS,
		LoadedUpMS:      res.LoadedUpMS,
		LoadedDownMaxMS: res.LoadedDownMaxMS,
		LoadedUpMaxMS:   res.LoadedUpMaxMS,
	}
	// Capture the connection context (public IP/ISP/DNS) this run ran in.
	if s.ConnInfoFn != nil {
		ci := s.ConnInfoFn(ctx)
		sp.PublicIPv4, sp.PublicIPv6 = ci.PublicIPv4, ci.PublicIPv6
		sp.ISP, sp.ISPLocation = ci.ISP, ci.ISPLocation
		sp.DNSIP, sp.DNSProvider, sp.DNSLocation = ci.DNSIP, ci.DNSProvider, ci.DNSLocation
		sp.CFColo, sp.ExitSummary = ci.CFColo, ci.ExitSummary
	}

	// Check the run against the configured thresholds (if any) and record the
	// verdict; alerting happens below.
	var failures []string
	thresholdsActive, judged := false, false
	if s.ThresholdsFn != nil {
		if t := s.ThresholdsFn(); t.Any() {
			thresholdsActive = true
			// Only judge the run if it actually measured at least one quantity the
			// active thresholds cover. Otherwise len(failures)==0 would mean
			// "nothing was checked", not "everything passed" - recording the run
			// green and (below) clearing a real breach streak. Happens with iperf3
			// 'both' when the only threshold is on the direction that failed.
			if thresholdsMeasurable(sp, t) {
				judged = true
				failures = evalThresholds(sp, t)
				healthy := len(failures) == 0
				sp.Healthy = &healthy
				s.log.Debug("speedtest thresholds", "healthy", healthy, "failures", failures)
			} else {
				s.log.Debug("speedtest thresholds active but nothing measurable to check them against")
			}
		}
	}

	// On shutdown (or a client disconnect mid-run) ctx is already cancelled by the
	// time the test finishes; don't persist into a store that p.run is about to
	// Close(), and don't alert during teardown. The run just isn't recorded - the
	// WAL stays crash-consistent.
	if err := ctx.Err(); err != nil {
		return sp, err
	}
	if err := s.store.InsertSpeed(ctx, sp); err != nil {
		// The result is not durable. Do NOT advance the breach streak, the adaptive
		// cadence (lastUnhealthy), or fire an alert on a run that was never recorded -
		// a restart would then re-evaluate from persisted history and disagree with an
		// alert already sent, or pin the fast cadence off a run no one can see. Report
		// the persistence failure so the caller (a manual run) learns it didn't store.
		s.log.Error("speedtest store", "err", err)
		return sp, fmt.Errorf("speedtest: persist result: %w", err)
	}
	// Debounced alerting: fire only once a breach has persisted for the configured
	// number of consecutive runs, so a single blip doesn't page. The counter only
	// moves when thresholds are active; a recovered run resets it. Streak of 1
	// alerts on every breach.
	switch {
	case judged:
		s.lastUnhealthy.Store(len(failures) > 0) // drives the adaptive cadence
		if len(failures) > 0 {
			s.consecBreach++
			streak := 1
			if s.BreachStreakFn != nil {
				if n := s.BreachStreakFn(); n > 1 {
					streak = n
				}
			}
			s.log.Warn("speedtest below threshold",
				"failures", failures, "streak", s.consecBreach, "alert_after", streak)
			if s.consecBreach >= streak && s.OnUnhealthy != nil {
				s.OnUnhealthy(sp, failures)
			}
		} else {
			s.consecBreach = 0
		}
	case thresholdsActive:
		// Active thresholds, but this run measured none of the quantities they
		// cover. Leave the breach streak untouched: a genuine breach still stands,
		// and a run we could not judge must neither clear it nor be recorded green.
	default:
		// No active thresholds this run: reset the breach state. lastUnhealthy and
		// consecBreach update only here, and curInterval keys off lastUnhealthy -
		// so a stale "unhealthy" from before the thresholds were cleared would
		// otherwise pin the fast cadence forever.
		s.lastUnhealthy.Store(false)
		s.consecBreach = 0
	}
	// Nudge the schedule loop (non-blocking; a pending nudge already covers it):
	// this run may have flipped lastUnhealthy, and the loop's current sleep was
	// armed before it (see Loop's runWake case).
	if s.runWake != nil {
		select {
		case s.runWake <- struct{}{}:
		default:
		}
	}
	s.log.Info("speedtest done",
		"server", s.CurrentServer(),
		"down_mbps", util.Round1(res.DownloadMbps),
		"up_mbps", util.Round1(res.UploadMbps),
		"ping_ms", util.Round1(res.PingMS),
		"idle_ms", fptr(res.IdleMS),
		"loaded_down_ms", fptr(res.LoadedDownMS),
		"loaded_up_ms", fptr(res.LoadedUpMS),
		"loaded_down_max_ms", fptr(res.LoadedDownMaxMS),
		"loaded_up_max_ms", fptr(res.LoadedUpMaxMS))

	return sp, nil
}

// fptr renders an optional measurement for logging: rounded, or "n/a".
func fptr(v *float64) any {
	if v == nil {
		return "n/a"
	}
	return util.Round1(*v)
}

// startupDelay is the pause before the startup run, so the first connectivity
// probe lands before we saturate the link. scheduleRecheck caps how long Loop
// idles when a test is due but the schedule window is closed, so it fires soon
// after the window opens rather than a full interval later (a daily window
// narrower than the interval would otherwise never line up). Both vars so tests
// can shrink them.
var (
	startupDelay    = 3 * time.Second
	scheduleRecheck = 30 * time.Second
)

// scheduleJitter returns a bounded random offset in [0, min(interval/10, 60s))
// added to the deadline so fleet installs - which often share a start time and
// an Ookla server pool - don't phase-align and hammer the same servers in
// lockstep. Seeded per process, so the spread varies between installs.
func scheduleJitter(interval time.Duration) time.Duration {
	max := interval / 10
	if max > 60*time.Second {
		max = 60 * time.Second
	}
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

// Loop runs an initial test shortly after start, then one every interval, until
// ctx is cancelled. The next run is anchored to the end of the previous one
// (lastRun + interval), so the wake channel - which fires on every settings
// broadcast, related or not - only re-derives the deadline, never restarts the
// wait. When a run is due but the schedule window is closed, the loop re-checks
// every scheduleRecheck and fires as soon as the window opens.
func (s *Scheduler) Loop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	if s.enabled() {
		s.RunOnce(ctx, "startup")
	}
	lastRun := time.Now()
	// Per-cycle jitter, anchored to lastRun so a wake (settings broadcast)
	// re-derives the same deadline rather than reshuffling it; refreshed only when
	// lastRun advances after a scheduled run.
	jitter := scheduleJitter(s.curInterval())
	s.setAnchor(lastRun, jitter)
	deferred := false // tracks the running→deferred edge, so the reason logs once

	for {
		var wake <-chan struct{}
		if s.WakeFn != nil {
			wake = s.WakeFn()
		}
		// Re-read the interval each cycle so runtime changes take effect; keep the
		// lastRun anchor so a change shifts the deadline, never resets it.
		wait := time.Until(lastRun.Add(s.curInterval() + jitter))
		if wait <= 0 {
			// Due, enabled, schedule window open, and the link isn't already busy. A
			// closed window or busy link defers: poll every scheduleRecheck and fire
			// as soon as it clears, without resetting the anchor. busy() applies only
			// to scheduled runs.
			if s.enabled() && !s.busy() {
				if deferred { // answer "why hasn't it run" without a metrics scrape
					s.log.Info("speedtest resumed")
					deferred = false
				}
				s.RunOnce(ctx, "scheduled")
				lastRun = time.Now()
				jitter = scheduleJitter(s.curInterval())
				s.setAnchor(lastRun, jitter)
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if !deferred { // log the deferral once, on the edge into it (not every recheck)
				reason := "disabled or outside schedule window"
				if s.enabled() {
					reason = "link busy"
				}
				s.log.Info("speedtest deferred", "reason", reason)
				deferred = true
			}
			wait = scheduleRecheck
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
			// Deadline (or window re-check) elapsed; loop re-evaluates.
		case <-wake:
			// Settings changed; recompute the deadline against the same anchor.
		case <-s.runWake:
			// A run completed - possibly a reconnect/degraded/manual one on another
			// goroutine. If it left us in a breach, count the adaptive cadence from
			// that run: the old anchor could otherwise sleep out hours of a base
			// interval before the fast cadence engages.
			if s.AdaptiveFn != nil && s.AdaptiveFn() && s.lastUnhealthy.Load() {
				lastRun = time.Now()
				jitter = scheduleJitter(s.curInterval())
				s.setAnchor(lastRun, jitter)
			}
		}
	}
}

// bytesPtr returns &n, or nil when n <= 0 - a direction that wasn't measured (an
// iperf3 best-effort run where one direction failed), so it's stored as NULL.
func bytesPtr(n int64) *int64 {
	if n <= 0 {
		return nil
	}
	return &n
}

// evalThresholds returns a human-readable failure for each enabled threshold the
// sample misses (download/upload below the minimum, ping/jitter/loss/bloat above
// the maximum).
func evalThresholds(sp store.SpeedSample, t settings.Thresholds) []string {
	var f []string
	// A nil byte count means that direction wasn't measured (iperf3 best-effort,
	// one direction failed); skip it so it can't fire a false "0 Mbps" breach.
	// Ookla always sets bytes, so its thresholds are unaffected.
	if t.DownMbps > 0 && sp.DownBytes != nil && sp.DownMbps < t.DownMbps {
		f = append(f, fmt.Sprintf("download %.1f < %.0f Mbps", sp.DownMbps, t.DownMbps))
	}
	if t.UpMbps > 0 && sp.UpBytes != nil && sp.UpMbps < t.UpMbps {
		f = append(f, fmt.Sprintf("upload %.1f < %.0f Mbps", sp.UpMbps, t.UpMbps))
	}
	if t.PingMS > 0 && validMS(sp.PingMS) && sp.PingMS > t.PingMS {
		f = append(f, fmt.Sprintf("ping %.0f > %.0f ms", sp.PingMS, t.PingMS))
	}
	if t.JitterMS > 0 && sp.JitterMS != nil && *sp.JitterMS > t.JitterMS {
		f = append(f, fmt.Sprintf("jitter %.0f > %.0f ms", *sp.JitterMS, t.JitterMS))
	}
	// Packet loss is optional (nil = not measured, can't breach) and bounded
	// 0..100. Strict "exceeds" like the other maxima, but treat a 100% threshold
	// (the UI ceiling) as "total loss" so it isn't inert - strict > 100 never fires.
	if t.LossPct > 0 && sp.PacketLoss != nil {
		if loss := *sp.PacketLoss; loss > t.LossPct || (t.LossPct >= 100 && loss >= 100) {
			f = append(f, fmt.Sprintf("packet loss %.1f%% > %.0f%%", loss, t.LossPct))
		}
	}
	// Bufferbloat is latency added under load (loaded - idle), per direction. Both
	// samples must exist; an older run with no idle/loaded capture can't breach.
	if t.BloatDownMS > 0 && sp.IdleMS != nil && sp.LoadedDownMS != nil {
		if b := *sp.LoadedDownMS - *sp.IdleMS; b > t.BloatDownMS {
			f = append(f, fmt.Sprintf("download bloat %.0f > %.0f ms", b, t.BloatDownMS))
		}
	}
	if t.BloatUpMS > 0 && sp.IdleMS != nil && sp.LoadedUpMS != nil {
		if b := *sp.LoadedUpMS - *sp.IdleMS; b > t.BloatUpMS {
			f = append(f, fmt.Sprintf("upload bloat %.0f > %.0f ms", b, t.BloatUpMS))
		}
	}
	return f
}

// thresholdsMeasurable reports whether this run measured at least one quantity an
// active threshold covers - i.e. whether evalThresholds could actually have
// judged it. Mirrors evalThresholds's presence guards exactly (kept beside it so
// the two stay in step). Ping is not always present: the iperf3 engine leaves
// PingMS at 0 when its handshake probe, stream RTT, and idle anchor all fail
// (validMS rejects that sentinel), so a latency-less run must not count as judged.
func thresholdsMeasurable(sp store.SpeedSample, t settings.Thresholds) bool {
	switch {
	case t.DownMbps > 0 && sp.DownBytes != nil:
		return true
	case t.UpMbps > 0 && sp.UpBytes != nil:
		return true
	case t.PingMS > 0 && validMS(sp.PingMS):
		return true
	case t.JitterMS > 0 && sp.JitterMS != nil:
		return true
	case t.LossPct > 0 && sp.PacketLoss != nil:
		return true
	case t.BloatDownMS > 0 && sp.IdleMS != nil && sp.LoadedDownMS != nil:
		return true
	case t.BloatUpMS > 0 && sp.IdleMS != nil && sp.LoadedUpMS != nil:
		return true
	}
	return false
}
