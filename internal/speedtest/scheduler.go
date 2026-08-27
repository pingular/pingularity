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

// ErrAborted is returned when the user cancelled a run (via Abort) BEFORE any
// server produced a usable result - not a measurement failure, and no
// measurement is stored (data the aborted run had already moved still lands in
// a usage-only row; see recordFailedUsage). An abort AFTER a best-of-N server
// has succeeded keeps that (best) result instead, so this only fires when the
// abort lands before the first one.
var ErrAborted = errors.New("speedtest aborted")

// afterClaimHook is called by RunOnce immediately after it claims the
// single-flight flag. Test seam only (nil in production): the interesting window
// for Abort is between that claim and the cancel becoming visible, and it is a
// few instructions wide, so a stress test hits it by luck rather than by design.
var afterClaimHook func()

// Scheduler runs speedtests on a fixed interval, on reconnect, and on demand,
// persisting each result. A single-flight guard keeps tests from overlapping (a
// reconnect during a scheduled run is just skipped).
type Scheduler struct {
	tester    Tester
	store     *store.Store
	log       *slog.Logger
	interval  time.Duration
	curServer atomic.Value             // string: server of the in-progress run ("" when idle)
	runSeq    atomic.Uint64            // hands out run ids; seeded per boot (see bootRunSeed) so they are never reused, restarts included
	curID     atomic.Uint64            // id of the run holding the single-flight claim (0 when idle)
	cur       atomic.Pointer[inflight] // the in-flight run's id and cancel; nil when idle
	abortFor  atomic.Uint64            // the run id an abort was raised for; carries it across RunOnce's startup window

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

	// ReadyFn, if set, reports whether the inputs the startup run's server
	// selection depends on are as ready as they are going to get (in production:
	// netinfo has published once, or nothing is coming - see main.go). Only the
	// LATCH's consent run consults it, and only as a bounded extra wait
	// (firstEnableReadyBound) on top of firstEnableDelay: the ten-second pad
	// alone let the first-ever test race netinfo's first refresh and auto-select
	// from the Ookla API's IP placement instead of the real city race. The boot
	// startup run on an already-configured install deliberately does NOT wait -
	// delaying every reboot's first sample is a worse trade than one
	// possibly-uncentred selection there.
	ReadyFn func() bool

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

	// startupGate, when armed by the latch, is the completions snapshot the
	// startup run must still match AFTER claiming the single-flight guard. The
	// latch's own served-check runs before its RunOnce call, and a run still in
	// flight at that check can complete - counter bumped, guard released - in
	// the instructions between the check and the claim. Validating after the
	// claim closes that hole: a completing run bumps completions BEFORE
	// releasing the guard, so holding the guard makes the comparison race-free.
	// Nil when no latch run is pending (the boot startup run does not gate).
	startupGate atomic.Pointer[uint64]

	// completions counts persisted runs (incremented where runWake is nudged).
	// The startup latch snapshots it at entry and treats ANY advance as the
	// slot being served: the capacity-1 runWake drain alone leaves a window
	// where a run still in flight at the drain completes - nudge sent, guard
	// released - just before the latch's own RunOnce claims the free guard and
	// starts a duplicate test. A counter comparison cannot lose that race.
	completions atomic.Uint64

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

// startupRunWanted reports whether the enabled-at-boot startup test should fire:
// the scheduler was enabled at Loop start AND still is, AND no run completed
// during the settle sleep. entryCompletions is the completions snapshot taken
// before the sleep; if it advanced, a manual/reconnect run already served the
// one-per-boot slot and a startup run would just re-test back-to-back.
func (s *Scheduler) startupRunWanted(initiallyEnabled bool, entryCompletions uint64) bool {
	return initiallyEnabled && s.enabled() && s.completions.Load() == entryCompletions
}

// Running reports whether a speedtest is in progress (any trigger). Safe for
// concurrent use.
// Running reports whether a run holds the single-flight claim. It reads the same
// word as RunID, so the two can never disagree: a caller that sees a run in
// progress always has an id to name it by. Keeping "is one running" in a separate
// bool left a window where the dashboard was told yes while there was still no id
// to send back, and a stop click landing there was dropped.
func (s *Scheduler) Running() bool { return s.curID.Load() != 0 }

// inflight is the in-progress run's identity paired with its cancel, stored as one
// value so a cancel can never be applied to a run other than the one it belongs to.
type inflight struct {
	id     uint64
	cancel context.CancelFunc
}

// RunID returns the id of the run currently in progress, or 0 when idle. Callers
// that later want to stop THAT run pass this back to Abort.
func (s *Scheduler) RunID() uint64 { return s.curID.Load() }

// Abort cancels the run with the given id and reports whether it signalled one.
// An id of 0 means "whatever is running now", for callers that never observed a
// particular run.
//
// A best-of-N run that has already measured at least one server keeps that (best)
// result; an abort before the first result stores nothing (ErrAborted).
//
// The id is what makes this safe to act on late. A stop is decided against a run
// the operator can SEE and arrives some time afterwards - the dashboard polls
// status every few seconds and then opens a confirm() dialog, which blocks that
// tab until it is answered. Resolving the request against whoever held the flag on
// arrival meant a run that started in the meantime was the one killed: an ordinary
// few seconds spent reading the dialog was enough, no race needed. Worse when the
// victim was a reconnect run, since ErrAborted (unlike ErrBusy) does not hand the
// reconnect window back, so the post-outage measurement was suppressed for up to a
// full speed interval.
func (s *Scheduler) Abort(id uint64) bool {
	cur := s.curID.Load()
	if cur == 0 {
		return false // genuinely nothing running
	}
	if id != 0 && id != cur {
		return false // the run this was decided for has already ended
	}
	// Record the request BEFORE cancelling. RunOnce publishes its cancel several
	// statements after it takes its id, and a stop landing in that window used to
	// find nil, do nothing, and report that it had done nothing - while Running()
	// (which is what puts the stop button on screen) already said yes. The id
	// carries the request across the gap: RunOnce checks it once its cancel is live.
	s.recordAbort(cur)
	// Only cancel a run that still IS this one; the pointer and the id move together.
	if f := s.cur.Load(); f != nil && f.id == cur {
		f.cancel()
	}
	return true
}

// recordAbort publishes cur as the raised abort target - unless a NEWER
// request is already recorded (run ids are monotonic, so newer = larger). A
// plain Store let a stalled Abort lose the race the slow way round: it read
// its target while that run was still current, was preempted, and its late
// store then clobbered an abort just raised for the run that replaced it -
// which read back a stale id, ignored it, and ran on although ITS Abort had
// returned true.
func (s *Scheduler) recordAbort(cur uint64) {
	for {
		old := s.abortFor.Load()
		if old >= cur || s.abortFor.CompareAndSwap(old, cur) {
			return
		}
	}
}

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
	s := &Scheduler{tester: t, store: st, log: log, interval: interval, runWake: make(chan struct{}, 1)}
	s.runSeq.Store(bootRunSeed())
	return s
}

// bootRunSeed is where run ids start counting from THIS boot, chosen so ids are
// unique across restarts and not merely within one process. An id makes a stop
// safe to act on late (see Abort), but the dashboard's confirm() dialog can
// outlive the daemon: with the sequence restarting at 1 every boot, a stale
// tab's queued click deterministically named the NEXT boot's startup run - the
// same id, three seconds in - and session cookies survive restarts, so nothing
// in between noticed the process changed.
//
// The arithmetic: ids ride the JSON status API into a JavaScript client, whose
// numbers are exact only below 2^53. UnixMilli is ~2^41 today and shifting it
// left 10 bits (~2^51) leaves room for 1024 runs per millisecond of boot
// spacing - real restarts are seconds apart, and a run takes seconds - while
// staying under 2^53 until roughly the year 2248. Best-effort by design: a
// clock that repeats itself (an RTC-less host booting at the epoch twice) can
// repeat ids, but there is nowhere to persist a sequence a crash can't lose.
func bootRunSeed() uint64 { return uint64(time.Now().UnixMilli()) << 10 }

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
	// A stop can land while this run is still setting up - after the claim below,
	// before the cancel is published - and it must not be lost. The run id carries
	// it across that window, by a store/load pairing with Abort whose ORDER is the
	// whole correctness argument: Abort stores the id it is stopping (abortFor)
	// and only then looks for the cancel (cur); RunOnce publishes its cancel (cur)
	// and only then checks abortFor. Whichever way those four operations
	// interleave, one side sees the other - Abort finds the published cancel and
	// calls it, or the post-publish check finds its own id in abortFor and cancels
	// itself. Reorder either pair and a stop can fall between: Abort finds no
	// cancel yet AND the check reads a not-yet-stored abortFor, so the run
	// continues after the user was told it was stopping
	// (abort_window_test.go drives exactly that window).
	//
	// Claiming the flag and taking an id are ONE operation: the id IS the claim.
	// (A burnt id on a lost race is fine - ids need to be unique, not consecutive.)
	myID := s.runSeq.Add(1)
	if !s.curID.CompareAndSwap(0, myID) {
		stats.Inc("speed.errbusy") // collisions, e.g. a reconnect during a scheduled test
		s.log.Debug("speedtest skipped: already running", "reason", reason)
		return store.SpeedSample{}, ErrBusy
	}
	// afterClaimHook exists only so a test can drive the window between claiming the
	// flag and publishing the cancel; nil in production.
	if afterClaimHook != nil {
		afterClaimHook()
	}
	// Startup preflight (see startupGate): if a run completed since the latch
	// decided the slot was unserved, that run IS the first measurement - bail
	// as ErrBusy (the latch's ErrBusy branch logs the skip and anchors to now,
	// which is within jitter of the serving run's completion).
	if reason == "startup" {
		if g := s.startupGate.Swap(nil); g != nil && s.completions.Load() != *g {
			s.curID.Store(0)
			return store.SpeedSample{}, ErrBusy
		}
	}
	// OnUnhealthy (an HTTP webhook to an operator's endpoint) must fire only AFTER
	// the single-flight flag is released: a slow or dead endpoint held inline would
	// keep 'running' set, so a manual run fired during the alert wrongly bounced off
	// the busy gate with ErrBusy. Registered before the running-flag defer so LIFO
	// runs it LAST, once the flag is already down; the callback touches no scheduler
	// state, so ordering it after the release is safe. Assigned at most once (at the
	// single breach point below), so exactly one notification goes out per breaching
	// run.
	var notify func()
	defer func() {
		if notify != nil {
			notify()
		}
	}()
	defer s.curID.Store(0)
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
	// A cancellable child of ctx so a user Abort() can stop THIS run without tearing
	// down the daemon. Only runTester rides runCtx; everything after (conninfo, the
	// ctx.Err() persist gate, InsertSpeed) stays on the parent ctx - so a user abort
	// still persists the best-of-N result measured so far, while a shutdown (parent
	// cancelled) still skips the store.
	runCtx, cancel := context.WithCancel(ctx)
	s.cur.Store(&inflight{id: myID, cancel: cancel})
	defer func() { s.cur.Store(nil); cancel() }()
	// The cancel is live now. If a stop for THIS run arrived while it was not - the
	// window the id exists to cover - honour it here instead of losing it.
	if s.abortFor.Load() == myID {
		cancel()
	}
	res, err := runTester(runCtx, s.curTester(), reason)
	if err != nil {
		// A failed run's traffic was still spent - RunReason returns the
		// accumulated bytes alongside the error - so record the usage before
		// deciding how the failure is reported, or a total-failure (or
		// user-aborted) run bills the link and "data used" shows nothing.
		s.recordFailedUsage(ctx, res, reason)
		// A user Abort() before any server produced a result: the parent ctx is still
		// live but runCtx was cancelled. Not a failure, and nothing to store.
		if ctx.Err() == nil && runCtx.Err() != nil {
			s.log.Info("speedtest aborted", "reason", reason)
			return store.SpeedSample{}, ErrAborted
		}
		// A caller-aborted run (shutdown) isn't a measurement failure - keep the
		// fleet failure rate honest.
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
		Trigger: reason,
		Engine:  res.Engine,
		// Empty stays empty (stored NULL-equivalent, absent in the API): a run
		// that didn't record the family or probe direction must read as unknown,
		// never as a guessed value.
		IPFamily:        res.IPFamily,
		UDPDirection:    res.UDPDirection,
		PingBestMS:      res.PingBestMS,
		IdleMS:          res.IdleMS,
		LoadedDownMS:    res.LoadedDownMS,
		LoadedUpMS:      res.LoadedUpMS,
		LoadedDownP95MS: res.LoadedDownP95MS,
		LoadedUpP95MS:   res.LoadedUpP95MS,
	}
	// How the centre was chosen rides the speed row itself (see RaceVerdict):
	// one line per run, beside the selection report that explains the server
	// inside that centre. Ookla only; iperf3 has no race to record.
	if res.Race != nil {
		sp.RaceOutcome, sp.RaceOrigins = res.Race.Outcome, res.Race.Origins
		sp.RaceWinnerKind, sp.RaceWinnerLabel = res.Race.WinnerKind, res.Race.WinnerLabel
		sp.RaceWinnerMS = res.Race.WinnerMS
		if res.Race.WinnerLat != 0 || res.Race.WinnerLon != 0 {
			la, lo := res.Race.WinnerLat, res.Race.WinnerLon
			sp.RaceWinnerLat, sp.RaceWinnerLon = &la, &lo
		}
		if res.Race.Racers > 0 {
			n := int64(res.Race.Racers)
			sp.RaceRacers = &n
		}
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
	measuredClean := false // no verdict, but every threshold the run COULD measure passed
	if s.ThresholdsFn != nil {
		if t := s.ThresholdsFn(); t.Any() {
			thresholdsActive = true
			// A direction the run TRIED and lost is a verdict, not a gap: with a
			// minimum configured on it, the breach is that it measures nothing
			// at all. Without this, a kept partial read as healthy - the upload
			// alert silenced by the very failure it watches, and an in-progress
			// breach streak actively reset at the moment the uplink fell off
			// the cliff. Judged before the measurability gate on purpose: a
			// failed direction is a verdict even when it is the ONLY thing the
			// active thresholds cover.
			if t.UpMbps > 0 && res.UploadFailed {
				failures = append(failures, fmt.Sprintf("upload unmeasured (the upload failed) with a %.0f Mbps minimum set", t.UpMbps))
			}
			if t.DownMbps > 0 && res.DownloadFailed {
				failures = append(failures, fmt.Sprintf("download unmeasured (the download failed) with a %.0f Mbps minimum set", t.DownMbps))
			}
			// Only judge the run further if it actually measured at least one
			// quantity the active thresholds cover. Otherwise a clean sweep would
			// mean "nothing was checked", not "everything passed" - recording the
			// run green and (below) clearing a real breach streak. Happens with
			// iperf3 'both' when the only threshold is on the direction that failed.
			if thresholdsMeasurable(sp, t) {
				failures = append(failures, evalThresholds(sp, t)...)
				// A breach is a breach whatever else went unmeasured - evidence of
				// failure needs no corroboration. But a CLEAN sweep only means
				// "everything passed" if everything was actually checked. Without
				// this, an enabled threshold whose inputs the run never captured
				// contributed no failure, len(failures)==0 read as green, and the
				// run cleared a real breach streak on the strength of a check that
				// never ran. Same hazard the branch below already guards for the
				// all-unmeasured case; this is the PARTIAL case, which fell
				// through it.
				switch {
				case len(failures) > 0:
					judged = true
					healthy := false
					sp.Healthy = &healthy
					s.log.Debug("speedtest thresholds", "healthy", false, "failures", failures)
				case thresholdsUnmeasured(sp, t):
					// Unknown, not green: leave Healthy nil and the streak alone.
					// But every threshold this run COULD measure passed - evidence
					// for the cadence (below), even though it is not a verdict.
					measuredClean = true
					s.log.Debug("speedtest thresholds passed what was measurable, but an enabled check had no inputs; recording no verdict")
				default:
					judged = true
					healthy := true
					sp.Healthy = &healthy
					s.log.Debug("speedtest thresholds", "healthy", true, "failures", failures)
				}
			} else if len(failures) > 0 {
				// Nothing measurable, but a tried-and-failed direction already
				// delivered its verdict.
				judged = true
				healthy := false
				sp.Healthy = &healthy
				s.log.Debug("speedtest thresholds", "healthy", false, "failures", failures)
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
	// The measurement FIRST, and on the second the store actually gave it
	// (InsertSpeedTS walks to a free second: ts is the run's identity for
	// delete, merge and the UI, and a shared second silently merges two
	// records into one). The extra-usage row comes after, keyed to that
	// second - written before the measurement it references, a crash or a
	// failed insert between the two left a durable orphan naming a run that
	// never landed, inflating the usage total forever with nothing to sweep
	// it. Written after, the worst a crash costs is the extra row itself: an
	// undercount, the same cost its own insert failure has always been
	// allowed to carry.
	finalTS, err := s.store.InsertSpeedTS(ctx, sp)
	if err != nil {
		// The result is not durable. Do NOT advance the breach streak, the adaptive
		// cadence (lastUnhealthy), or fire an alert on a run that was never recorded -
		// a restart would then re-evaluate from persisted history and disagree with an
		// alert already sent, or pin the fast cadence off a run no one can see. Report
		// the persistence failure so the caller (a manual run) learns it didn't store.
		s.log.Error("speedtest store", "err", err)
		return sp, fmt.Errorf("speedtest: persist result: %w", err)
	}
	sp.TS = finalTS
	// A retried direction, or one that failed while the other succeeded, spent
	// bytes that never reach the row above: only the winning attempt's count
	// lands there, because a byte count on a successful row is what says the
	// direction ran. Bill the remainder as its own usage-only row so a metered
	// link is told the truth about what the run cost, without inventing a second
	// measurement. Same shape and same filtering as a failed run's row.
	s.recordExtraUsage(ctx, res, reason, sp.TS)
	// The run's selection report, keyed to the row just written (sp.TS, never a
	// second clock read - the next second would detach the rows from their run).
	// nil is the normal case for every engine but Ookla. Non-fatal on error: the
	// run itself is durable, and a lost report costs explainability, not
	// history. The ctx.Err() gate above already covers shutdown for both writes.
	if res.Selection != nil {
		if err := s.store.InsertSpeedServers(ctx, selectionRows(sp.TS, res.Selection)); err != nil {
			s.log.Error("speedtest selection store", "err", err)
		}
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
				// Dispatch after the flag release (see the deferred notify above);
				// sp and failures are stable from here on, so the closure keeps the
				// exact values this run judged.
				notify = func() { s.OnUnhealthy(sp, failures) }
			}
		} else {
			s.consecBreach = 0
		}
	case thresholdsActive:
		// Active thresholds, but no verdict this run. Leave the breach streak
		// untouched: a genuine breach still stands, and a run we could not judge
		// must neither clear it nor be recorded green.
		//
		// A permanently-unmeasurable enabled check (e.g. a loss limit against a
		// server that never supports the UDP probe) must not pin the fast cadence
		// forever after one real breach, though. When the MEASURABLE thresholds
		// all passed this run, back the adaptive cadence off - the same principle
		// as the cleared-thresholds reset below - while leaving the breach streak
		// untouched (an unran check can neither clear a genuine breach nor record
		// the run green).
		if measuredClean {
			s.lastUnhealthy.Store(false)
		}
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
	s.completions.Add(1) // before the nudge: latch observers compare, never race
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
		"ping_ms", util.Round1(res.PingMS), "ping_best_ms", f64v(res.PingBestMS),
		"idle_ms", fptr(res.IdleMS),
		"loaded_down_ms", fptr(res.LoadedDownMS),
		"loaded_up_ms", fptr(res.LoadedUpMS),
		"loaded_down_p95_ms", fptr(res.LoadedDownP95MS),
		"loaded_up_p95_ms", fptr(res.LoadedUpP95MS))

	return sp, nil
}

// recordFailedUsage persists the data a FAILED (or user-aborted) run still
// moved, so total-failure runs keep honest data-usage accounting: the
// SpeedDataUsage sums read the speed table, and dropping the returned byte
// counts here recorded 0 for traffic that measurably transited the link. The
// row is accounting, not a measurement - speeds and ping stay 0, Healthy stays
// nil, no thresholds are evaluated, and completions is NOT advanced, so the
// startup latch cannot mistake a failure for a served slot. Skipped on
// shutdown (parent ctx dead: the store is about to close, same rule as the
// success path's persist gate) and when the run moved nothing.
//
// Failed is what keeps that distinction READABLE. Without it the row is
// indistinguishable from a real 0 Mbps measurement to every consumer, because
// the codebase's "was this direction measured?" predicate is `bytes != nil` and
// this row carries real bytes: /metrics would emit a 0 download gauge (a
// permanent false "below threshold" alert) and stamp the last-run freshness
// anchor, and the dashboard would draw a 0 in the chart, the history table and
// the averages. store.SpeedSample.Failed hides it from every measurement read
// while the usage sums still count it.
//
// No store.SpeedSample.UsageRunTS here, unlike recordExtraUsage. That reference
// says "these bytes belong to the run recorded at this ts", and a wholly failed
// run has no such row - RunOnce returns before writing one, which is why this
// function exists. This row IS the whole record of the run, at its own
// timestamp, so DeleteSpeed already removes it by ts like any other row; there
// is nothing for a cascade to reach it from. Pointing it at its own ts would
// only assert a measurement that never happened, and this codebase does not
// write a claim it cannot back.
func (s *Scheduler) recordFailedUsage(ctx context.Context, res Result, reason string) {
	if ctx.Err() != nil || (res.DownloadBytes <= 0 && res.UploadBytes <= 0) {
		return
	}
	sp := store.SpeedSample{
		TS: time.Now().Unix(), Server: res.Server, ServerID: res.ServerID,
		Trigger: reason, Engine: res.Engine, Failed: true,
		DownBytes: bytesPtr(res.DownloadBytes), UpBytes: bytesPtr(res.UploadBytes),
	}
	if err := s.store.InsertSpeed(ctx, sp); err != nil {
		// Non-fatal: the run already failed, and the caller's error is the one
		// that explains it - losing the usage row costs accounting, not history.
		s.log.Error("speedtest usage store", "err", err)
	}
}

// recordExtraUsage bills traffic a SUCCESSFUL run spent beyond what it measured:
// a retried direction's earlier attempts, or a direction that failed while the
// other carried the run. Those bytes cannot go on the measurement row - a byte
// count there is the signal that a direction ran, so adding a failed attempt's
// spend would manufacture a measurement out of traffic that measured nothing.
// They are still real bytes on a metered link, and before this they were simply
// dropped: two 125 MB attempts recorded 125 MB.
//
// Written as the same usage-only row a failed run gets: flagged, so every
// measurement read filters it out, while the data-usage sums still count it.
// measuredTS is the timestamp of the measurement row this spend belongs to; the
// usage row is written one second later so the two never share a key. The speed
// table's backup merge is keyed on ts alone, and its import keeps the FIRST row
// it sees for a key - so two rows in the same second meant a restore silently
// kept one and dropped the other. That lost the measurement, which is the worse
// half: a backup that quietly discards the reading looks like it worked.
// (`failed` cannot join the merge key instead: a key column that is NULL marks
// the row unimportable, and NULL is deliberately how a real run records "not an
// accounting row".)
//
// measuredTS is also written into the row itself, as UsageRunTS: the second it
// SITS on is +1, but the run it BELONGS to is named outright, and that is what
// DeleteSpeed cascades on. Deriving the owner from the position instead - "the
// flagged row one second after the run" - deleted whatever else happened to be
// there, and a manual run failing a second after a scheduled measurement puts
// its own flagged row at exactly that second.
func (s *Scheduler) recordExtraUsage(ctx context.Context, res Result, reason string, measuredTS int64) {
	if ctx.Err() != nil || (res.ExtraDownBytes <= 0 && res.ExtraUpBytes <= 0) {
		return
	}
	sp := store.SpeedSample{
		TS: measuredTS + 1, Server: res.Server, ServerID: res.ServerID,
		Trigger: reason, Engine: res.Engine, Failed: true,
		UsageRunTS: &measuredTS,
		DownBytes:  bytesPtr(res.ExtraDownBytes), UpBytes: bytesPtr(res.ExtraUpBytes),
	}
	if err := s.store.InsertSpeed(ctx, sp); err != nil {
		// Non-fatal for the same reason as recordFailedUsage: the measurement
		// itself is what matters here, and it is stored either way.
		s.log.Error("speedtest extra usage store", "err", err)
	}
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
	// The pause before the consent-triggered startup run (Loop's startupPending
	// latch). Longer than startupDelay on purpose: this one fires while the user
	// is looking at a dashboard they just opened for the first time, so the page
	// gets a beat to settle before the test saturates the link.
	firstEnableDelay = 10 * time.Second
	// How much LONGER than firstEnableDelay the latch will wait for ReadyFn
	// before running anyway. A hung lookup must not hold the first test hostage:
	// 30s from consent is still worlds better than the interval, and the common
	// case (netinfo publishing within a few seconds of the power-on wake) never
	// waits at all.
	firstEnableReadyBound = 20 * time.Second
	firstEnableReadyPoll  = 500 * time.Millisecond
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
//
// The startup run belongs to the first moment the scheduler is ENABLED, not to
// boot. On an already-configured install those coincide and it fires at
// boot+startupDelay as it always has. On a fresh install the boot slot is
// skipped - the user hasn't consented to tests yet - and without a carry-over
// the consent moment inherited nothing: answering Quick Setup with "hourly"
// started monitoring instantly but left the speed panel empty for a full
// interval, because the wake only re-derived a deadline anchored at boot. The
// startupPending latch carries the unclaimed slot forward: the first time a
// wake (or deferral re-check) finds the scheduler enabled, the startup run
// fires - after firstEnableDelay, so the dashboard the user just landed on has
// a beat to settle - and the schedule anchors to its end. One slot per boot:
// once claimed, later off/on toggles follow the normal anchor arithmetic,
// which already fires promptly when more than an interval has passed and
// deliberately doesn't re-test when less has.
func (s *Scheduler) Loop(ctx context.Context) {
	// Read BEFORE the settle sleep: consent that lands during it is a
	// disabled-to-enabled transition and must go through the latch below (pad +
	// readiness), not masquerade as an already-configured boot and run
	// immediately with none of either.
	initiallyEnabled := s.enabled()
	// Snapshot completions BEFORE the settle sleep: a manual or reconnect run that
	// lands during the sleep already serves the one-per-boot slot, so the startup
	// run below must not also fire (a redundant back-to-back test on a link still
	// settling from the first). Mirrors the disabled->enabled latch, which already
	// gates on this counter; the enabled-at-boot run used to skip the check.
	entryCompletions := s.completions.Load()
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupDelay):
	}
	startupPending := !initiallyEnabled
	if s.startupRunWanted(initiallyEnabled, entryCompletions) {
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
		// Subscribe BEFORE reading any state the sleep depends on. Changed()
		// hands out a channel the next broadcast closes and replaces: fetching
		// it AFTER the latch check leaves a window where consent lands between
		// check and fetch - the fetched channel is the post-broadcast
		// replacement, the wake is lost, and the loop sleeps out most of an
		// interval with the latch armed. Fetch-first inverts it: a broadcast
		// before the fetch is seen by the state reads below; one after the
		// fetch closes the channel we sleep on.
		var wake <-chan struct{}
		if s.WakeFn != nil {
			wake = s.WakeFn()
		}
		if startupPending && s.enabled() {
			latchEntryCompletions := s.completions.Load()
			// No scheduled deadline is knowable while the startup run is imminent
			// or in flight - the published one still says boot+interval, which the
			// run about to fire would predate by most of the interval. Withdraw it
			// (status omits the field), the same shape the boot startup run has
			// while the anchor is still unpublished; the post-run setAnchor below
			// restores it.
			s.anchor.Store(nil)
			select {
			case <-ctx.Done():
				return
			case <-time.After(firstEnableDelay):
			}
			// Bounded wait for the selection inputs (see ReadyFn). Bails early
			// if consent is withdrawn - the re-check below keeps the slot.
			if s.ReadyFn != nil && !s.ReadyFn() {
				bound := time.After(firstEnableReadyBound)
			ready:
				for s.enabled() {
					select {
					case <-ctx.Done():
						return
					case <-bound:
						break ready
					case <-time.After(firstEnableReadyPoll):
						if s.ReadyFn() {
							break ready
						}
					}
				}
			}
			// A run that completed since the latch armed (reconnect after a
			// flap, manual) IS the first measurement - the slot is served, and
			// firing the latch anyway would stack a second full test onto a
			// link still settling from the first. Decided by the completions
			// COUNTER, not the runWake drain alone: a run still in flight when
			// the drain looks can complete - nudge sent, single-flight released
			// - in the instructions between the drain and the RunOnce below,
			// and the drain would miss it while RunOnce claims the freed guard
			// and duplicates the test. Only persisted runs count, so a failed
			// attempt doesn't burn the slot. The drain still empties the
			// capacity-1 nudge so the main select doesn't spin on it.
			// A pending runWake token means a persisted run completed and was
			// NOT yet observed by the main select's runWake case (the loop left
			// via wake, or entered the latch directly from the settle sleep).
			// That run serves the slot - HONOR the drained token, don't just
			// discard it. Combined with the counter check (a run that completed
			// during the pause), this closes the race where a run finishing
			// BEFORE latch entry - its increment already baked into
			// latchEntryCompletions - would otherwise leave the slot armed and
			// fire a duplicate.
			select {
			case <-s.runWake:
				startupPending = false
			default:
			}
			if s.completions.Load() != latchEntryCompletions {
				startupPending = false
			}
			// Re-checked after the pause: a toggle straight back off within it
			// means no consent run, and the slot stays available rather than
			// being burned on a run that never happened.
			if startupPending && s.enabled() {
				// An ErrBusy here is a run that STARTED during the pause and is
				// still in flight - it serves the slot like the completed-run
				// case above, but the slot going unfilled must stay traceable
				// (same reasoning as the scheduled collision below): if that run
				// fails, this log line is what a gap in the history leads back to.
				gate := latchEntryCompletions
				s.startupGate.Store(&gate)
				if _, err := s.RunOnce(ctx, "startup"); errors.Is(err, ErrBusy) {
					stats.Inc("speed.scheduled_skipped")
					s.log.Info("startup speedtest skipped: another run was already in progress")
				}
				startupPending = false
				lastRun = time.Now()
				jitter = scheduleJitter(s.curInterval())
				s.setAnchor(lastRun, jitter)
				// An overdue-while-gated stretch may have logged the deferred
				// edge; this run answers it, so the next real deferral logs anew
				// (and the scheduled path doesn't print a stale "resumed").
				deferred = false
			} else {
				// Declined (toggled off mid-pause): the boot-anchored deadline
				// withdrawn above is still the real one - republish it, or
				// status would omit the next run for up to a full interval.
				// Served by a run that completed during the pause: that run IS
				// a measurement, so anchor the schedule to it - the latch can
				// arm long before consent (the 48h hold), and republishing a
				// boot-stale anchor would make wait<=0 and fire a scheduled
				// test right on the heels of the run that just served the slot.
				if !startupPending {
					lastRun = time.Now()
					jitter = scheduleJitter(s.curInterval())
				}
				s.setAnchor(lastRun, jitter)
			}
			continue
		}
		// Re-read the interval each cycle so runtime changes take effect; keep the
		// lastRun anchor so a change shifts the deadline, never resets it.
		wait := time.Until(lastRun.Add(s.curInterval() + jitter))
		// While the startup slot is still armed but the scheduler is gated
		// (outside a daily window, monitoring off), the latch branch above is
		// skipped and this wait would be a full interval - so a window that
		// opens later by the CLOCK (nothing broadcasts that) is slept straight
		// through, and the first measurement slips ~an interval. Poll at the
		// recheck cadence instead, so the latch re-evaluates enabled() as the
		// window opens.
		if startupPending && wait > scheduleRecheck {
			wait = scheduleRecheck
		}
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
				// A scheduled run refused because another trigger already holds the
				// single-flight is SKIPPED, and the anchor advances with it. The
				// asymmetry with the deferral below is deliberate, not an oversight:
				// a closed window or a busy link means NOTHING was measured, so those
				// poll and fire the moment they clear; a collision means a
				// measurement is already in flight, so this slot has effectively
				// been served. Preserving the anchor here would leave wait<=0 true
				// and fire a second full test the instant the first released it -
				// another ~123MB against a link still settling from the one that
				// just finished.
				//
				// The cost is that the run holding the flag may FAIL, in which case
				// the interval passes with nothing recorded. Loop cannot see that
				// outcome from here, and one missed interval after a failed manual
				// test is a smaller problem than a guaranteed double test after
				// every successful one.
				//
				// Counted and logged separately from the other collisions
				// (speed.errbusy covers reconnect and degraded too, which are
				// opportunistic and lose nothing by being dropped): a scheduled slot
				// going unfilled is the one a gap in the history can be traced to.
				if _, err := s.RunOnce(ctx, "scheduled"); errors.Is(err, ErrBusy) {
					stats.Inc("speed.scheduled_skipped")
					s.log.Info("scheduled speedtest skipped: another run was already in progress",
						"next_in", s.curInterval())
				}
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
			// goroutine. If the startup slot was still armed (gated boot, e.g. a
			// restart outside the schedule window), that measurement serves it:
			// without this, the latch would fire a duplicate full test right after
			// one that just finished - exactly the back-to-back double test the
			// scheduled path's ErrBusy handling was engineered against. And a
			// SERVING run re-anchors the schedule, same rule as the latch's own
			// serve: the latch can be armed with the anchor already overdue, and
			// keeping it would fire a scheduled test right on the heels of the
			// run that just served the slot the moment the scheduler enables.
			if startupPending {
				startupPending = false
				lastRun = time.Now()
				jitter = scheduleJitter(s.curInterval())
				s.setAnchor(lastRun, jitter)
			}
			// If it left us in a breach, count the adaptive cadence from
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

// samplePingMS is the latency a STORED run is judged on: the fastest of its ping
// samples when the engine recorded one, else the reported mean. The store-side
// twin of decisionPingMS, and it exists for the same reason - the mean is an
// average of ten samples, so one stalled handshake can breach a ping threshold
// on a link that never slowed down. Older rows and iperf3 rows have no floor and
// fall back to the mean, exactly as before.
func samplePingMS(sp store.SpeedSample) float64 {
	if sp.PingBestMS != nil && validMS(*sp.PingBestMS) {
		return *sp.PingBestMS
	}
	return sp.PingMS
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
	if p := samplePingMS(sp); t.PingMS > 0 && validMS(p) && p > t.PingMS {
		f = append(f, fmt.Sprintf("ping %.0f > %.0f ms", p, t.PingMS))
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

// thresholdsUnmeasured reports whether any threshold that is ENABLED and
// APPLICABLE to this run lacked the inputs to judge it. The counterpart to
// thresholdsMeasurable: that one asks "could we check ANYTHING", this asks
// "did we fail to check SOMETHING we were asked to". A run may be recorded
// green only when this is false.
//
// Applicable is not the same as enabled. A direction that never ran cannot be
// judged and was never promised: an upload limit on a download-only run is
// inert by configuration, not unmeasured, so it must not hold every run
// hostage. The byte counts are the proxy for "this direction ran", the same
// one evalThresholds already uses to avoid firing a false 0 Mbps breach.
//
// Throughput itself is never listed: when a direction ran its Mbps is always
// set, so an enabled Mbps threshold on a direction that ran is always
// judgeable. What can genuinely go missing is a measurement layered ON a
// direction - the loaded-latency sampler behind bufferbloat - or a probe that
// is allowed to fail on its own (ping, jitter, the optional UDP loss probe).
func thresholdsUnmeasured(sp store.SpeedSample, t settings.Thresholds) bool {
	ranDown, ranUp := sp.DownBytes != nil, sp.UpBytes != nil
	switch {
	case t.PingMS > 0 && !validMS(samplePingMS(sp)):
		return true
	case t.JitterMS > 0 && sp.JitterMS == nil:
		return true
	case t.LossPct > 0 && sp.PacketLoss == nil:
		return true
	case t.BloatDownMS > 0 && ranDown && (sp.IdleMS == nil || sp.LoadedDownMS == nil):
		return true
	case t.BloatUpMS > 0 && ranUp && (sp.IdleMS == nil || sp.LoadedUpMS == nil):
		return true
	}
	return false
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
	case t.PingMS > 0 && validMS(samplePingMS(sp)):
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

// selectionRows converts a winner's selection report for persistence - the
// same field-by-field seam RunOnce uses for Result -> store.SpeedSample. It
// lives HERE, not in selection.go: the Scheduler is the package's only store
// client, and the engine files stay persistence-free.
func selectionRows(runTS int64, rep *SelectionReport) []store.SpeedServerRow {
	rows := make([]store.SpeedServerRow, 0, len(rep.Candidates))
	for _, c := range rep.Candidates {
		rows = append(rows, store.SpeedServerRow{
			RunTS: runTS, ServerID: c.ServerID, Server: c.Server, DistanceKM: c.DistanceKM,
			RankOrder: int64(c.RankOrder), RankPingMS: c.RankPingMS,
			Selected: c.Selected, Measured: c.Measured, Err: c.Err,
			DownMbps: c.DownMbps, UpMbps: c.UpMbps, PingMS: c.PingMS,
			PingBestMS: c.PingBestMS, JitterMS: c.JitterMS,
			DownloadBytes: c.DownloadBytes, UploadBytes: c.UploadBytes,
			CapacityMbps: c.CapacityMbps, BelievedCapacityMbps: c.BelievedCapacityMbps,
			CappedDirection: c.CappedDirection, Score: c.Score,
			Winner: c.Winner, WinReason: c.WinReason,
		})
	}
	return rows
}
