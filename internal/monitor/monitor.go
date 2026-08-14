// Package monitor runs the probe loop and the debounced up/down state machine,
// persisting samples and transition events as it goes.
package monitor

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/prober"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/util"
)

// Monitor orchestrates probing, debouncing, and persistence.
type Monitor struct {
	cfg    config.Config
	prober *prober.Prober
	store  *store.Store
	log    *slog.Logger

	mu            sync.RWMutex   // guards online/since and family state for readers
	online        bool           // current overall debounced state
	since         time.Time      // when the current overall state began
	lastEventWall time.Time      // wall time of the last persisted transition event; the next event's stored ts is clamped >= this so a backward WALL step can't order an 'up' before its 'down' (guarded by mu)
	pendingEvents []pendingEvent // transition events whose durable InsertEvent failed, awaiting retry (guarded by mu)
	dnsMS         float64        // last DNS-probe resolve time in ms (guarded by mu)
	dnsOK         bool           // last DNS probe resolved successfully (guarded by mu)
	dnsSeen       bool           // a DNS probe has produced at least one result (guarded by mu)
	dnsGen        uint64         // bumped on pause OR DNS toggle-off to discard an in-flight DNS probe's late result (guarded by mu)
	dnsBusy       atomic.Bool    // a DNS probe goroutine is in flight (single-flight)
	dnsWG         sync.WaitGroup // tracks the in-flight DNS probe goroutine so Run waits for it before returning (don't write to a store about to Close)
	dnsWasEnabled bool           // previous round's DNS-toggle state, to spot the toggle-off edge (monitor goroutine only)
	okStreak      int            // consecutive online rounds (monitor goroutine only)
	badStreak     int            // consecutive offline rounds (monitor goroutine only)
	lastNoteAt    time.Time      // previous round's TS, for asymmetry spacing (monitor goroutine only)

	downPausedAt time.Time     // when the current pause episode began while down; folded into pausedGap on resume (monitor goroutine only)
	pausedGap    time.Duration // total unwatched paused time in the current outage, excluded from its recorded duration (monitor goroutine only)
	frozenGap    time.Duration // wall seconds of this outage's booked suspend rows that the monotonic clock slept through; transition's widen adds them back so the read model's pause subtraction lands on seconds the pair holds (guarded by mu)
	outageGen    uint64        // opaque token identifying the current accumulator incarnation; bumped as transition() resets the two gaps above, stamped into each pendingGap so a refusal/eviction surfacing in a LATER outage is not reversed out of THIS one's accumulators. Monitor-goroutine only, save for the under-mu bump/read in transition and the accumulator functions

	degradedStreak int           // consecutive rounds over the degraded-latency threshold (monitor goroutine only)
	degraded       bool          // currently inside a degraded episode (monitor goroutine only)
	degradedSent   bool          // this episode already dispatched a measurement, so OnDegraded fires once (monitor goroutine only)
	degradedSeq    uint64        // dispatch ids handed out so far (monitor goroutine only)
	degradedID     atomic.Uint64 // id of the dispatch that owns the current episode, 0 when none; published to the dispatcher
	degradedRetry  atomic.Uint64 // dispatch that never started a measurement, stored from the dispatcher's goroutine

	fams     map[string]*familyState // per-address-family state
	famOrder []string                // stable display order of families
	active   map[string]bool         // families probed in the most recent round

	// OnReconnect, if set, fires when the link comes back online; used to trigger
	// a speedtest. Called synchronously from the probe loop; keep it quick or async.
	OnReconnect func()

	// OnDegraded, if set, fires when the link is online but its base latency stays
	// above DegradedPingFn()'s threshold for degradedRounds rounds - catching a
	// brownout the reconnect hook would miss. Fires once per episode; re-arms when
	// latency recovers below the threshold, or when the dispatch reports back that
	// it never started a measurement (RetryDegraded). Called synchronously, like
	// OnReconnect - so DegradedDispatch() read inside it names THIS dispatch.
	OnDegraded func()

	// DegradedPingFn, if set, supplies the latency (ms) above which the link counts
	// as degraded; <=0 disables degradation detection.
	DegradedPingFn func() float64

	// OnTransition, if set, fires on every confirmed overall state change (used for
	// alert notifications). durationS is the just-ended outage length when online is
	// true. Called synchronously; keep it quick or async.
	OnTransition func(online bool, durationS int)

	// IntervalFn, if set, supplies the probe interval live each round (runtime
	// changes). Falls back to cfg.Interval when nil.
	IntervalFn func() time.Duration

	// WakeFn, if set, returns a channel closed when settings change, so an interval
	// change takes effect at once rather than after the current wait elapses.
	WakeFn func() <-chan struct{}

	// DownAfterFn / UpAfterFn supply the debounce thresholds live (fall back to
	// cfg when nil). EnabledFn is the master switch (power button). LatencyFn is
	// the latency-probing sub-toggle. Probing runs only when both are true.
	DownAfterFn func() int
	UpAfterFn   func() int
	EnabledFn   func() bool
	LatencyFn   func() bool
	// DNSFn is the DNS-probe sub-toggle (nil = on). Also gated by probing(), so it
	// never runs when latency probing or the master switch is off.
	DNSFn func() bool
}

// dnsEnabled reports whether the per-round DNS probe should run.
func (m *Monitor) dnsEnabled() bool { return m.DNSFn == nil || m.DNSFn() }

func (m *Monitor) probing() bool {
	if m.LatencyFn != nil && !m.LatencyFn() {
		return false
	}
	return m.monitoring()
}

// scheduleRecheck caps how long Run idles while a round is due but probing is
// paused, so a schedule window (or toggle) reopening is noticed promptly even
// without a settings-change wake.
const scheduleRecheck = 30 * time.Second

// aliveInterval is how often Run emits a liveness "monitor alive" line, so a
// wedged loop (no line) is distinguishable from a healthy idle one at a glance.
const aliveInterval = 5 * time.Minute

// pauseCheckpoint bounds how much unflushed pause span accumulates before Run
// persists it: a long or never-resuming pause writes ~one row per checkpoint, so a
// monitor left paused still has its unobserved time reflected in uptime within this
// margin. startupGapMin is the smallest process-down gap booked as an unobserved
// pause on startup - below it a quick restart isn't worth a row.
const (
	pauseCheckpoint = 5 * time.Minute
	startupGapMin   = 2 * time.Minute
)

// suspendGapSlack is the headroom, ON TOP OF one probe interval, that the WALL gap
// between two loop iterations must exceed before Run books it as unobserved time.
// It exists because the loop is structurally blind to a suspend-to-RAM: `wait` is
// derived from the monotonic anchor lastRound, and CLOCK_MONOTONIC does not advance
// across suspend, so a frozen host wakes believing no time passed. Its dark hours
// then land in UptimeSince's OBSERVED denominator and are scored as up - while the
// SAME real gap taken by a process restart is correctly excluded by the startup-gap
// pause above. That asymmetry (killed = excluded, frozen = counted up) is the bug.
//
// The threshold is `interval + slack`, deliberately not the tempting `2*interval`.
// The interval is operator-set anywhere in [1s, 1h] (config.Min/MaxInterval), so
// 2*interval is TEN SECONDS at the default 5s cadence - within reach of one
// stop-the-world GC, a stalled fsync, or a busy host descheduling the loop. Every
// false positive there writes a pause row that subtracts genuinely observed time
// from the denominator, and that is the worse bug: Store.pausedOverlap SUMs the
// rows it finds, so spurious spans compound, they can drive observed time to zero
// (UptimeSince then reports coverage 0 and callers drop the uptime figure), and
// nothing re-derives them away later. At the other end 2*interval is TWO HOURS at a
// 1h cadence, which would sleep through a whole night's suspend. A fixed slack is
// tighter than that where the interval is long and far safer where it is short.
//
// Ten minutes clears every ordinary overshoot by an order of magnitude: one wait
// (<= max(interval, scheduleRecheck)), one round (targets are dialled concurrently,
// so <= config.MaxTimeout = 30s), plus scheduler and GC jitter. The price is that
// freezes shorter than ~10 minutes stay mis-booked as observed - a bounded,
// self-limiting error, unlike a spurious row.
const suspendGapSlack = 10 * time.Minute

// waitArmedHook, when set by a test, reports the wait the loop just armed and the
// span the next gap check will be judged against. Nil in production.
var waitArmedHook func(armed, threshold time.Duration)

// plausibleWallEpoch is the earliest believable wall-clock reading (2023-01-01 UTC,
// before the project existed), mirroring the store's plausibleEpoch. An RTC-less
// board (a Pi with no battery) boots near 1970 and jumps decades forward the instant
// NTP answers; that step is a clock CORRECTION, not unobserved time, and booking it
// would insert a pause span longer than the entire history - enough on its own to
// zero out UptimeSince's observed denominator. Store.Prune reasons identically and
// simply refuses to run while the clock is implausible: nothing is lost by waiting,
// everything is lost by acting now.
const plausibleWallEpoch = 1672531200

func (m *Monitor) interval() time.Duration {
	if m.IntervalFn != nil {
		if d := m.IntervalFn(); d > 0 {
			return d
		}
	}
	return m.cfg.Interval
}

func (m *Monitor) downAfter() int {
	if m.DownAfterFn != nil {
		if n := m.DownAfterFn(); n > 0 {
			return n
		}
	}
	return m.cfg.DownAfter
}

func (m *Monitor) upAfter() int {
	if m.UpAfterFn != nil {
		if n := m.UpAfterFn(); n > 0 {
			return n
		}
	}
	return m.cfg.UpAfter
}

func (m *Monitor) monitoring() bool {
	if m.EnabledFn != nil {
		return m.EnabledFn()
	}
	return true
}

// familyState is the debounced state of one address family.
type familyState struct {
	online    bool
	since     time.Time
	okStreak  int     // monitor goroutine only
	badStreak int     // monitor goroutine only
	latency   float64 // last round's min latency in ms across this family's successful targets (0 only when no target answered)
}

// FamilyStatus is the public per-family snapshot.
type FamilyStatus struct {
	Family    string
	Online    bool
	Since     time.Time
	LatencyMS float64
}

// Status is a snapshot of the monitor's current debounced state.
type Status struct {
	Online    bool
	Since     time.Time
	Paused    bool
	Probing   bool // rounds are actually running (master switch AND latency toggle/schedule/family gates all open)
	Families  []FamilyStatus
	DNSms     float64 // last round's DNS-resolve time in ms (0 when not yet measured)
	DNSok     bool    // last round's DNS resolution succeeded
	DNSactive bool    // the DNS probe is running (probing on AND DNS sub-toggle on); false means DNSms/DNSok hold no live reading
}

// Snapshot returns the current state; safe to call from other goroutines.
func (m *Monitor) Snapshot() Status {
	// Compute these before taking m.mu: probing()/monitoring()/DNSFn take the settings
	// lock, which must not be held under m.mu.RLock(). probingNow is the one truth for
	// "is anything being measured right now" (false if the master switch, latency
	// sub-toggle, or schedule says no), so the live pills show what's actually running
	// instead of freezing at stale values.
	paused := !m.monitoring()
	probingNow := m.probing()
	dnsOn := probingNow && m.dnsEnabled() // DNS rides the probe round
	m.mu.RLock()
	defer m.mu.RUnlock()
	// A running probe with no result yet (first round's async resolve still in
	// flight) must read as "no reading", not a fake resolver-down: gate on
	// dnsSeen, so /metrics and the pill stay absent until the first answer.
	dnsLive := dnsOn && m.dnsSeen
	st := Status{Online: m.online, Since: m.since, Paused: paused, Probing: probingNow, DNSactive: dnsLive}
	if dnsLive { // drop the reading when the probe is off or unseeded, so no stale/fake pill
		st.DNSms, st.DNSok = m.dnsMS, m.dnsOK
	}
	// Per-family pills only while probing. m.active tracks which families a round
	// actually covered (so IPv6 vanishes when its mode is off) but freezes when probing
	// stops, so gate on probingNow to hide the whole set when nothing is being probed.
	if probingNow {
		for _, fam := range m.famOrder {
			if !m.active[fam] {
				continue // not probed last round (e.g. IPv6 toggled off live)
			}
			fs := m.fams[fam]
			st.Families = append(st.Families, FamilyStatus{
				Family: fam, Online: fs.online, Since: fs.since, LatencyMS: fs.latency,
			})
		}
	}
	return st
}

// New constructs a Monitor. It starts in the optimistic "online" state so the
// first real outage is reported as a transition rather than the baseline.
func New(cfg config.Config, p *prober.Prober, st *store.Store, log *slog.Logger) *Monitor {
	m := &Monitor{cfg: cfg, prober: p, store: st, log: log, online: true,
		fams: map[string]*familyState{}, active: map[string]bool{}}
	for _, t := range cfg.Targets {
		if t.Family != "" && m.fams[t.Family] == nil {
			m.famOrder = append(m.famOrder, t.Family)
			m.fams[t.Family] = &familyState{online: true}
			// Seed every configured family as active so the snapshot is sensible
			// before the first round, which then narrows it to families actually
			// probed.
			m.active[t.Family] = true
		}
	}
	return m
}

// Run probes on the configured interval until ctx is cancelled.
func (m *Monitor) Run(ctx context.Context) error {
	// On shutdown, wait for any in-flight DNS probe goroutine before returning, so
	// it can't write to a store the caller Closes right after Run returns (the
	// goroutine's own ctx check narrows but doesn't close that race).
	defer m.dnsWG.Wait()
	now := time.Now()
	m.mu.Lock()
	m.since = now
	for _, fs := range m.fams {
		fs.since = now
	}
	m.mu.Unlock()

	// Seed the event-clock guard from the newest stored event. transition()'s
	// nondecreasing clamp and strictly-after down-bump rest on lastEventWall, and
	// only this process's own transitions used to feed it - so a restart wiped the
	// guard exactly when it matters most: a widened recovery leaves the newest
	// on-disk 'up' up to a backward step's size in the wall FUTURE, and a fresh
	// process confirming an outage inside that catch-up window stored its 'down'
	// at or before that 'up'. completedOutagesSince breaks ts ties down-before-up,
	// so the tied 'down' steals the pairing and collapses the prior outage to the
	// zero-width shape the widen exists to eliminate, one restart later.
	// LastObservedTS cannot be the source here: it caps at wall now, and a
	// future-dated 'up' - the one record that needs guarding against - is
	// precisely what it excludes. A failed read just leaves the guard unseeded,
	// which is the pre-seeding behavior: transitions still order within this
	// process's lifetime.
	//
	// The seed is honoured only inside store.FutureSlack, the system's own
	// definition of plausibly-future: Prune deletes events past it every hour, so
	// a row inside it is a durable widened recovery that MUST be protected (the
	// whole reason this seeding exists), while a row beyond it is already
	// condemned - protecting it only clamps this process's real transitions out
	// into the same condemned zone, where the next prune erases them while the
	// in-memory guard survives the deletion and drags the transition after them
	// too. Nothing bounds an event's ts on the way in, so the source can be a
	// crafted backup or a boot clock years fast. Skipping leaves the guard
	// unseeded, the same safe fallback as a failed read; the widen it protects
	// never reaches beyond a backward step's size, and every step this bound
	// tolerates is far larger than any the widen can produce.
	//
	// nowFn() rather than the `now` above: this is a WALL judgement against a ts
	// read from the database, the same seam (and the same reason) as the startup
	// gap check below, while `now` is the monotonic-carrying anchor for m.since.
	if evs, err := m.store.EventsPage(ctx, 1, 0); err == nil && len(evs) > 0 &&
		evs[0].TS <= nowFn().Add(store.FutureSlack).Unix() {
		m.mu.Lock()
		m.lastEventWall = time.Unix(evs[0].TS, 0)
		m.mu.Unlock()
	}

	// Spans measured but not yet written; see pendingGap. Declared ahead of the
	// startup-gap write so a failed write there joins the same retry queue as every
	// other, rather than dropping a potentially months-long process-down stretch on
	// one bad write - the immediate first round advances LastObservedTS past the
	// gap, so nothing else would ever re-derive it.
	var held []*pendingGap
	// Process-down time is unobserved: book the gap since the last recorded activity
	// as a pause span so it counts as neither up nor down (else uptime credits a
	// multi-day outage-while-stopped as up). Skipped on a fresh install (no prior
	// activity) and for restart blips below the threshold.
	if last, ok, err := m.store.LastObservedTS(ctx); err == nil && ok {
		// nowFn().Sub rather than time.Since: the anchor is read from the DATABASE and
		// carries no monotonic reading, so this is a wall subtraction either way -
		// routing it through the seam lets a test drive the restart half of the
		// restart/suspend symmetry against the same store the loop's gap check writes to.
		start := time.Unix(last, 0)
		if gap := nowFn().Sub(start); gap > startupGapMin {
			if p := m.flushGap(ctx, &pendingGap{start: start, secs: int64(gap.Seconds()), pause: true, gen: m.outageGen}); p != nil {
				held = appendHeldGap(m, held, p)
			}
		}
	}

	if m.probing() {
		m.round(ctx) // probe immediately, don't wait a full interval
	}
	lastRound := time.Now()
	// The same checkpoint on the WALL clock, kept separately from lastRound because
	// lastRound's monotonic reading is precisely what a suspend freezes; see
	// bookUnobservedGap.
	lastWall := nowFn()
	// The cadence the current wait is being run at; the gap check judges the wait
	// just finished against THIS, not against a value the operator may have changed
	// mid-wait (see bookUnobservedGap).
	lastInterval := m.interval()
	paused := false              // tracks the running→paused edge for pause accounting
	var pauseTick time.Time      // last moment paused time was accrued up to
	var pauseSpanStart time.Time // start of the current unflushed pause span (uptime's observed denominator)
	// flushPause persists the pause span [pauseSpanStart, end) and advances the anchor:
	// forced on resume, else only once a span reaches pauseCheckpoint so a long pause
	// writes ~one row per checkpoint while a never-resuming pause is still reflected
	// within one checkpoint.
	flushPause := func(end time.Time, force bool) {
		if pauseSpanStart.IsZero() {
			return
		}
		// The span is measured on the WALL clock. InsertPause stores this row as
		// [start.Unix(), start.Unix()+duration_s), so a duration taken from
		// end.Sub(pauseSpanStart) - which uses the MONOTONIC reading whenever both
		// values carry one, as two time.Now() readings do - describes a different
		// interval than the row claims the moment the two clocks diverge. A host that
		// suspends while monitoring is switched off is exactly that case: the monotonic
		// clock is frozen for the whole suspend, so a nine-hour freeze would be recorded
		// as the ~30 seconds the loop was awake and the remainder would stay in
		// UptimeSince's observed denominator, scored as up - the same defect
		// bookUnobservedGap fixes for the probing case, reached by the other path.
		// Floor at the monotonic elapsed so a BACKWARD wall step (NTP correcting a fast
		// RTC) can only shorten the row to time that provably passed, never invert it.
		// The FORWARD direction has no such defence and cannot: a step and a suspend
		// leave identical evidence - wall advanced, the monotonic floor did not follow -
		// so this deliberately trusts the wall, because the suspend it exists to catch
		// is routine and a large forward step is not (chrony/ntpd slew sub-second
		// offsets rather than stepping, and step mostly at boot, which the startup-gap
		// path already owns). The residual error is exactly the step size, and
		// pauseCheckpoint keeps ordinary corrections from minting a row of their own -
		// see TestSmallForwardClockStepDoesNotMintAPauseSpan. A cap was considered and
		// rejected: a genuine hibernate can last weeks, so any ceiling would silently
		// truncate real unobserved time, which is the very error this path fixes.
		// (monitor.paused_s beside this keeps its monotonic reading on purpose: it is an
		// operator-facing duration, not part of the uptime denominator, and stats.AddF
		// must never be handed a negative.)
		// An implausible endpoint is a clock being corrected, not unobserved time -
		// the same judgement unobservedGap makes on the probing path, and the same
		// scenario, differing only in whether the master switch happened to be on. A
		// board with no RTC boots near the epoch and stays there until NTP answers;
		// with monitoring off (or outside its schedule window) a pause span is open
		// across that correction, and trusting the wall clock here turned it into a
		// single row claiming every second since 1970. That row is subtracted from
		// every uptime window, so coverage reads nothing on the pill, /metrics, the
		// digest and the whole heatmap.
		//
		// RE-ANCHOR rather than just decline: leaving pauseSpanStart at the boot
		// instant would only defer the same row to the next flush. The corrected
		// instant is the earliest moment this episode can honestly speak for.
		if pauseSpanStart.Unix() < plausibleWallEpoch || end.Unix() < plausibleWallEpoch {
			m.log.Info("pause span spans a clock correction; re-anchoring rather than recording it",
				"from", pauseSpanStart.UTC().Format(time.RFC3339), "to", end.UTC().Format(time.RFC3339))
			stats.Inc("monitor.pause_clock_corrections")
			pauseSpanStart = end
			return
		}
		span := time.Duration(end.Unix()-pauseSpanStart.Unix()) * time.Second
		if mono := end.Sub(pauseSpanStart); mono > span {
			span = mono
		}
		if span <= 0 || (!force && span < pauseCheckpoint) {
			return
		}
		// The write gets the same discipline as the probing path's flushGap, because
		// the stakes are identical: a slice that vanishes here stays in the observed
		// denominator and reads as observed-and-up for as long as the data is kept. A
		// FAILED write is held and re-offered as the exact span that was measured; a
		// REFUSED one is dropped for good, loudly, with the resume edge's deduction
		// kept consistent with the rows that exist (see dropRefusedPauseDeduction).
		// Refusal is reachable from right here: the mono floor above composes with
		// PauseSpanSane's end-by-about-now bound, so a backward wall step larger than
		// that skew mid-pause offers a span the store deterministically refuses -
		// silence there VOIDED the slice, not "only shortened" it. The anchor still
		// advances either way: a held record replays its own span, and a refused one
		// re-anchors at the corrected reading, whose stepped-back minutes the still-
		// open pause covers with its next slice (spans are merged on read).
		if p := m.flushGap(ctx, &pendingGap{start: pauseSpanStart, secs: int64(span.Seconds()), pause: true, gen: m.outageGen}); p != nil {
			held = appendHeldGap(m, held, p)
		}
		pauseSpanStart = end
	}
	rounds := 0             // probe rounds completed (for the liveness line)
	lastAlive := time.Now() // last "monitor alive" emission
	// One reusable timer, not a fresh time.After each iteration: settings broadcasts
	// fire `wake` on every change, so a burst would otherwise orphan one timer per
	// wake until it expired. Go 1.23+ Reset/Stop are drain-free.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		// Wall-gap check, FIRST in the iteration and before anything dispatches a
		// round. It must precede the resume edge below: that edge clears
		// pauseSpanStart, and a gap seen after it would be booked a second time on top
		// of the span the flush just wrote.
		wallNow := nowFn()
		// Re-offer anything a previous iteration measured but could not write. Each
		// record replays its OWN span, so a store that stays down for hours does not
		// inflate the stretch it eventually records.
		held = retryHeldGaps(ctx, m, held)
		// Same contract for buffered transition events: retry every iteration, not
		// only in round() - round() is gated behind probing(), so a pause would
		// otherwise strand a buffered 'down'/'up' for its whole duration (and lose
		// it outright if the daemon restarts first). A no-op when the buffer is
		// empty, and safe on a cancelled ctx (the insert fails, the record is kept).
		m.flushPendingEvents(ctx)
		booked, p := m.bookUnobservedGap(ctx, lastWall, wallNow, !pauseSpanStart.IsZero(), lastInterval,
			wallNow.Sub(lastWall))
		if p != nil {
			held = appendHeldGap(m, held, p)
		}
		if booked {
			// A freeze is an observation gap exactly like a pause, so it obeys the same
			// contract the paused branch enforces below: streaks from before it must not
			// combine with rounds after it to confirm a transition nine hours later.
			m.resetStreaks()
		}
		// The anchor always moves on. A gap that could not be written is carried in
		// `held` as the span that was actually measured, so there is no longer any
		// reason to leave the anchor behind - doing so made the next iteration
		// recompute a longer span from it, and each retry annexed more of the time
		// the monitor had genuinely been watching.
		lastWall = wallNow

		var wake <-chan struct{}
		if m.WakeFn != nil {
			wake = m.WakeFn()
		}
		// Re-read the interval each round so runtime changes take effect; keep the
		// lastRound anchor so a settings broadcast only re-derives the deadline
		// rather than restarting the wait.
		iv := m.interval()
		wait := time.Until(lastRound.Add(iv))
		// Resume edge: probing turned back on while a pause was still open. Close the
		// episode here, crediting only the real switched-off time up to now - deferring
		// it to the next due round would fold on-but-idle wait time into
		// monitor.paused_s and pausedGap.
		if paused && m.probing() {
			stats.AddF("monitor.paused_s", time.Since(pauseTick).Seconds())
			flushPause(nowFn(), true) // close the pause span at the resume edge
			pauseSpanStart = time.Time{}
			paused = false
			// noteResume stays on the REAL clock while the span above is wall: pausedGap
			// is subtracted from Monitor.transition's MONOTONIC elapsed, so it has to be
			// measured on the same clock. Feeding it wall time would double-subtract a
			// suspend that fell inside an outage - the monotonic elapsed already omits it.
			m.noteResume(time.Now())
			m.log.Info("monitor recording resumed")
		}
		switch {
		case !m.probing():
			// Probing is paused (master switch, latency sub-toggle, or schedule
			// window). Evaluate the gate every iteration, not just when a round is
			// due, so a mid-wait settings wake (the operator switching monitoring
			// off) stamps the pause promptly instead of at the next round deadline -
			// otherwise up to one interval of switched-off time is miscounted as
			// observed downtime. Clear the debounce streaks so pre-pause rounds can't
			// combine with post-resume rounds to confirm a transition, then re-check
			// the gate shortly.
			//
			// Pause accounting: one count per episode plus MEASURED paused wall-time
			// (counter: how often and how long the monitor is switched off). Time is
			// accrued between visits, not pre-credited per visit - settings broadcasts
			// re-enter this branch at arbitrary rates, so each visit must add only the
			// time that truly passed.
			now := nowFn()
			if !paused {
				paused = true
				pauseSpanStart = now // begin a new observed-pause span (flushed on checkpoint/resume)
				m.notePause(now)
				// The stored DNS reading is stale from here on; drop the seed so
				// a resume starts as "no reading yet" instead of surfacing it.
				// Bump dnsGen so an in-flight probe launched before the pause
				// can't write its now-stale result back after we clear the seed.
				m.mu.Lock()
				m.dnsSeen = false
				m.dnsGen++
				m.mu.Unlock()
				stats.Inc("monitor.pauses")
				m.log.Info("monitor recording paused")
			} else {
				stats.AddF("monitor.paused_s", now.Sub(pauseTick).Seconds())
				flushPause(now, false) // checkpoint a long / never-resuming pause
			}
			pauseTick = now
			m.resetStreaks()
			wait = scheduleRecheck
		case wait <= 0:
			// No wait is armed on this path, so the only time that can pass before the
			// next gap check is the round itself; suspendGapSlack covers that.
			lastInterval = 0
			m.round(ctx)
			lastRound = time.Now()
			rounds++
			// Liveness pulse: if these stop, the loop is wedged; if they keep coming,
			// the monitor is healthy even during a quiet stretch with no transitions.
			if time.Since(lastAlive) >= aliveInterval {
				m.log.Info("monitor alive", "rounds", rounds, "online", m.online)
				lastAlive = lastRound
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		// Judge the NEXT iteration's gap against the wait actually armed here, not the
		// nominal interval. Every mid-wait wake (a settings broadcast) re-enters this
		// loop and re-arms only the REMAINDER of the interval, so recording the full
		// cadence let the threshold outrun the sleep by however long the loop had
		// already been waiting - on a long interval, a real suspend inside the last
		// few minutes of it fell under the threshold and went unrecorded, which makes
		// coverage read higher than the truth.
		lastInterval = wait
		if waitArmedHook != nil {
			waitArmedHook(wait, lastInterval)
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			// Deadline (or pause re-check) elapsed; loop re-evaluates.
		case <-wake:
			// Settings changed; recompute the deadline against the same anchor.
			timer.Stop()
		}
	}
}

// unobservedGap reports the WALL seconds between two loop checkpoints that the
// monitor cannot have observed, or 0 when the spacing is ordinary.
//
// The comparison runs on .Unix() rather than now.Sub(prev) on purpose: Sub between
// two time.Now() values silently uses the MONOTONIC reading, and the monotonic
// clock standing still across a suspend is the entire defect - a monotonic delta
// here would make this check a permanent no-op that no test could distinguish from
// a working one. (Monitor.transition makes the mirror-image choice for the same
// reason: its outage duration MUST be monotonic, and it clamps the stored event ts
// on .Unix() explicitly.)
//
// A BACKWARD wall step - NTP correcting a fast RTC, or an operator setting the
// clock - yields a negative gap, falls under the threshold and books nothing, the
// same direction of safety Store.InsertPause takes by ignoring durationS <= 0. The
// caller re-anchors on every iteration regardless, so one backward step costs one
// missed detection window rather than blinding the check until the clock catches up.
func unobservedGap(prev, now time.Time, threshold time.Duration) int64 {
	// A zero prev (nothing anchored yet) is negative here, so it is caught too.
	if prev.Unix() < plausibleWallEpoch || now.Unix() < plausibleWallEpoch {
		return 0 // a boot clock being corrected, not unobserved time
	}
	gap := now.Unix() - prev.Unix()
	if gap <= int64(threshold/time.Second) {
		return 0
	}
	return gap
}

// bookUnobservedGap records the wall stretch since the previous loop checkpoint as
// unobserved time when it is too long to be ordinary loop spacing, and reports
// whether such a gap was seen (the caller clears the debounce streaks either way).
//
// The span is anchored at prev, the last moment the loop is known to have been
// running, and covers the whole delta. A freeze can have begun anywhere inside
// [prev, now), so this over-books by at most one iteration's worth of genuinely
// observed time - one wait plus one round. Erring in that direction is deliberate:
// the figure this feeds, pingularity_uptime_coverage_ratio, advertises that "a low
// value means the window was mostly paused/unobserved and its uptime_ratio is thin
// evidence", so it has to fail CLOSED. Unfixed it read 1 - perfect confidence - for
// a night that was nine hours dark.
//
// pauseOpen suppresses the row. While a pause span is open, the checkpoint/resume
// flush already covers this same wall stretch (flushPause measures it on the wall
// clock for exactly this case), and a second span over it would be counted twice:
// Store.pausedOverlap SUMs the rows it finds, so two spans over one stretch
// subtract it from the denominator twice and push observation coverage past 1.0 -
// corruption in the opposite direction, and worse, because nothing re-derives it.
//
// The gap always belongs in the denominator (this pause row). Whether it also has
// to come OUT of the numerator depends on something this code cannot assume: it
// used to state that a suspend inside an outage is already excluded, because
// transition measures elapsed on a monotonic clock "which is frozen for the whole
// freeze". That is true on macOS and Linux and false on Windows, where Go's
// monotonic clock keeps counting through sleep - so there the same stretch was
// booked as downtime and as never-watched at once. bookUnobservedGap now measures
// how far the monotonic clock actually moved and deducts exactly that; see
// unobservedInOutage.
// Returns whether a gap was seen, and the measured-but-unwritten observation
// gap when its row could not be persisted (nil when there was no gap, a pause
// is open, or the row landed on the first flushGap). The caller queues the
// pending record in `held` and replays it via retryHeldGaps; the wall anchor
// ALWAYS advances, because the span travels with the record - advancing loses
// nothing. (Holding the anchor instead was the old behavior, and it made each
// retry recompute a longer span from the stale anchor, annexing time the
// monitor had genuinely been watching.)
//
// monoAdvance is how far the MONOTONIC clock moved across [prev, now]; the caller
// owns that subtraction (Run passes now.Sub(prev) on values straight from
// time.Now, where Sub uses the monotonic readings) because a test cannot: Go
// couples a time.Time's wall and monotonic halves, so no synthesized value can
// have its wall nine hours ahead while its monotonic stood still - which is
// precisely the state a frozen-clock suspend leaves behind, and the case the
// deduction below has to decide on.
func (m *Monitor) bookUnobservedGap(ctx context.Context, prev, now time.Time, pauseOpen bool, waited, monoAdvance time.Duration) (booked bool, pending *pendingGap) {
	// `waited` is the interval that SIZED the wait just completed, not whatever the
	// interval is now. Reading m.interval() here judged a finished wait against a
	// setting that may have changed during it: lowering the probe interval from 1h
	// to 5s made the loop wake immediately and score the 50 minutes it had been
	// correctly sleeping as an unobserved gap, minting a permanent 50-minute pause
	// row that understates coverage for as long as it is retained.
	gap := unobservedGap(prev, now, waited+suspendGapSlack)
	if gap == 0 {
		return false, nil
	}
	// If an outage is open across this gap, decide whether transition() will have
	// counted the unwatched stretch as downtime, and if so fold it into pausedGap
	// so it is subtracted. MEASURED, not assumed - see unobservedInOutage.
	//
	// Only when no explicit pause is open. When one IS open the pause episode
	// already owns this stretch: notePause marked its start, and the resume edge
	// will fold the whole episode into this same accumulator via noteResume. Adding
	// here as well counted one nine-hour lid-close as eighteen, and since
	// transition() SUBTRACTS the accumulator from the outage length, the result was
	// not a slightly wrong figure but a real outage clamped to zero - gone from the
	// log, the heatmap and the digest. This is the same reason the pause ROW below
	// is suppressed on pauseOpen; the numerator needed the same rule and did not
	// have it.
	if pauseOpen {
		// The open pause span already covers this stretch, so there is nothing to
		// write and nothing to adjust.
		return true, nil
	}
	// Measure the numerator correction HERE, against the outage this gap actually
	// happened inside, and apply it straight away. Deferring it until the row lands
	// left it stranded whenever the link came back before the store did: the
	// recovery closes the outage and consumes the accumulator, so a nine-hour
	// suspend was filed as nine hours off the internet and no later write could take
	// it back.
	//
	// The amount is remembered so it can be REVERSED if the store turns out to
	// refuse the span. The row and this correction are two halves of one fact, and
	// the pair has to be kept whole in both directions - a deduction without a row
	// rewrites uptime just as quietly as a row without a deduction.
	p := &pendingGap{start: prev, secs: gap}
	m.mu.Lock()
	if !m.online {
		// Stamp the incarnation these accumulators belong to, under the same lock as
		// the mutation, so a refusal that surfaces in a later outage reverses out of
		// this outage's accumulators only, never a successor's (see revertGapDeduction).
		p.gen = m.outageGen
		p.deduct = unobservedInOutage(time.Duration(gap)*time.Second, monoAdvance)
		m.pausedGap += p.deduct
		// The remainder is the row's MONO-ABSENT part: wall seconds this row books
		// that the outage's monotonic elapsed never contained (a frozen clock's whole
		// freeze; zero when the clock ran straight through). transition() must widen
		// the stored pair by exactly this, or a backward wall step leaves a pair that
		// observedOutageSpans shrinks below its own duration_s when it subtracts this
		// very row from it. (unobservedInOutage clamps the deduction to the wall gap,
		// so the remainder is never negative.)
		p.frozen = time.Duration(gap)*time.Second - p.deduct
		m.frozenGap += p.frozen
	}
	m.mu.Unlock()
	return true, m.flushGap(ctx, p)
}

// maxHeldGaps bounds the records carried for retry. A store that refuses to write
// for long enough to accumulate this many separate suspends is broken in a way
// retrying will not fix, and an unbounded queue would turn that into a slow leak.
const maxHeldGaps = 64

// appendHeldGap adds a record to the retry queue, dropping the oldest if the queue
// is full. A dropped record's deduction goes back: without its row the correction
// has nothing to stand on.
func appendHeldGap(m *Monitor, held []*pendingGap, p *pendingGap) []*pendingGap {
	if len(held) >= maxHeldGaps {
		m.log.Warn("too many unwritten observation gaps; dropping the oldest",
			"dropped_since", held[0].start.UTC().Format(time.RFC3339), "dropped_gap_s", held[0].secs)
		stats.Inc("monitor.unobserved_gap_dropped")
		m.revertGapDeduction(held[0])
		held = held[1:]
	}
	return append(held, p)
}

// retryHeldGaps re-offers every held record and returns those still unwritten.
func retryHeldGaps(ctx context.Context, m *Monitor, held []*pendingGap) []*pendingGap {
	if len(held) == 0 {
		return held
	}
	keep := held[:0]
	for _, p := range held {
		if still := m.flushGap(ctx, p); still != nil {
			keep = append(keep, still)
		}
	}
	return keep
}

// pendingGap is an unobserved stretch that has been measured but whose store row
// has not landed yet. It is IMMUTABLE once built: the retry replays this exact
// span rather than recomputing one from the old anchor to the current time, which
// grew the stretch on every attempt and swallowed hours the monitor really had
// been watching.
type pendingGap struct {
	start  time.Time
	secs   int64
	deduct time.Duration // already added to pausedGap; reversed if the store refuses
	frozen time.Duration // the span's mono-absent remainder, already added to frozenGap; reversed with deduct - the widen must not add back a row that will never exist
	gen    uint64        // Monitor.outageGen at booking time: the accumulator incarnation deduct/frozen were added to (or, on the pause path, the outage the resume fold belongs to). A refusal or eviction reverses/advances only while this still matches; a mismatch means the booking's outage has closed and its accumulators are gone, so leave the current outage's untouched
	// pause marks a span from the pause path - flushPause's checkpoints and resume
	// edge, or the startup gap - rather than a suspend gap. The failed/refused
	// mechanics are shared, but the bookkeeping around them is not: a pause span's
	// outage deduction is the noteResume fold still to come (never deduct above),
	// so a refusal is reconciled through dropRefusedPauseDeduction, and a success
	// needs no gap counters or log line - the episode has its own accounting
	// (monitor.pauses, monitor.paused_s, the paused/resumed lines). One residual
	// asymmetry is accepted: a pause record dropped from a FULL queue cannot give
	// its fold back (the episode may be closed and consumed by then), the same
	// bounded cross-outage slack revertGapDeduction's zero-clamp already carries.
	pause bool
}

// flushGap attempts the write for a measured span - a suspend gap, or a
// pause-path record (see pendingGap.pause). It returns the record back if the
// attempt should be repeated later, or nil once the span is settled - written,
// or refused and its deduction taken back.
func (m *Monitor) flushGap(ctx context.Context, p *pendingGap) *pendingGap {
	stored, err := insertPause(m.store, ctx, p.start, p.secs)
	switch {
	case err != nil:
		// A write that FAILED may succeed later, so keep the record and re-offer the
		// same span. Warn, not Debug: an unobserved stretch that never reaches the
		// store makes coverage read HIGHER than the truth, which is the direction
		// nobody goes looking for.
		m.log.Warn("unobserved span not recorded; will retry the same span", "err", err, "gap_s", p.secs,
			"since", p.start.UTC().Format(time.RFC3339))
		stats.Inc("monitor.unobserved_gap_retries")
		return p
	case !stored:
		// REFUSED, which is a different thing from failed: the store will never accept
		// this span, so retrying it would only waste attempts. Take the numerator
		// correction back, since the row that justified it does not exist - each path
		// holds its own half of that pairing.
		if p.pause {
			m.dropRefusedPauseDeduction(p)
		} else {
			m.revertGapDeduction(p)
		}
		m.log.Warn("unobserved span refused by the store; not recorded and deduction reversed",
			"gap_s", p.secs, "deducted_back_s", p.deduct.Seconds(),
			"since", p.start.UTC().Format(time.RFC3339))
		stats.Inc("monitor.unobserved_gap_refused")
		return nil
	}
	if p.pause {
		// A stored pause-path span needs none of the gap accounting below: the
		// episode already announced itself ("monitor recording paused",
		// monitor.pauses, monitor.paused_s), and counting its rows under
		// unobserved_gaps too would report the same stretch twice.
		return nil
	}
	stats.Inc("monitor.unobserved_gaps")
	stats.AddF("monitor.unobserved_s", float64(p.secs))
	// Default level: this silently reshapes every uptime window it lands in, so an
	// operator asking "why is coverage 62%?" must be able to answer it from the log.
	m.log.Info("unobserved wall-clock gap", "gap_s", p.secs,
		"since", p.start.UTC().Format(time.RFC3339))
	return nil
}

// revertGapDeduction takes back a correction whose row was refused - both
// halves, since they describe the same row: the numerator deduction and the
// mono-absent remainder the widen would have added back.
//
// It reverses into the CURRENT accumulators only while the pending gap still
// belongs to the open incarnation (p.gen == m.outageGen). A gap booked in one
// outage, held because its write failed, then refused (or evicted) during a
// LATER outage carries the earlier token: transition() has already reset and
// re-generationed the accumulators, so subtracting p.deduct/p.frozen here would
// come out of the LATER outage's gaps - shorting its widen and its duration with
// a correction that was never theirs. On a mismatch the booking's outage has
// closed and its accumulators are gone, so drop it without touching the current
// ones and without any accumulator going negative. (The residual the token
// cannot undo: once the earlier outage's pair was already persisted, a refusal
// that lands after it closed cannot retroactively widen it; the token only keeps
// the corruption from reaching the successor.) The zero-clamp stays as defence.
func (m *Monitor) revertGapDeduction(p *pendingGap) {
	if p.deduct <= 0 && p.frozen <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.gen != m.outageGen {
		return
	}
	if m.pausedGap > p.deduct {
		m.pausedGap -= p.deduct
	} else {
		m.pausedGap = 0
	}
	if m.frozenGap > p.frozen {
		m.frozenGap -= p.frozen
	} else {
		m.frozenGap = 0
	}
}

// dropRefusedPauseDeduction is revertGapDeduction's counterpart for the pause
// path. A refused span will never have a row, so the stretch it covers must not
// be deducted from the open outage either - but a pause span's deduction is not
// p.deduct (never set on this path): it is the noteResume fold still to come,
// measured as resume-time minus downPausedAt. Advancing that anchor past the
// refused span excludes exactly the rowless stretch, while the slices whose
// rows exist stay deducted.
//
// The advance applies only while the SAME outage incarnation that measured the
// span is still open (p.gen == m.outageGen) AND the current episode's anchor is
// at or before the span's start: a refusal surfacing after that outage closed
// has no anchor of its own left to move, and a later outage's must not be moved
// for it. The start check alone caught the ordinary case - a later outage's
// anchor is set after the earlier span, so the span sorts before it - but a
// backward wall step (NTP) between the outages can land the new anchor EARLIER
// than the old span's start, and then only the token tells the two apart. Both
// gates are kept: within one outage the start check still fences a span from an
// already-resumed-and-folded earlier episode out of a later one's anchor.
// Monitor-goroutine only, like notePause/noteResume.
func (m *Monitor) dropRefusedPauseDeduction(p *pendingGap) {
	if !m.downPausedAt.IsZero() && p.gen == m.outageGen && !p.start.Before(m.downPausedAt) {
		m.downPausedAt = m.downPausedAt.Add(time.Duration(p.secs) * time.Second)
	}
}

// unobservedInOutage returns how much of an unobserved wall gap has to be
// deducted from an open outage's recorded length, given how far the MONOTONIC
// clock advanced across the same gap.
//
// transition() measures an outage with ts.Sub(m.since), a monotonic subtraction.
// Whether that already excludes a suspend depends on the platform, and the code
// here used to assume it always does. Go only promises that "on SOME systems the
// monotonic clock will stop if the computer goes to sleep" - and Windows is one
// where it does not: the runtime reads _INTERRUPT_TIME (KUSER_SHARED_DATA), the
// BIASED interrupt time, which has sleep added back on wake. So on Windows the
// suspend stayed inside the outage's numerator while the pause row this function
// writes removed it from the observed denominator - the same stretch counted as
// downtime and as never-watched.
//
// Rather than branch on GOOS, compare the two clocks over the gap we just
// detected:
//
//   - monoGap ~ 0: the monotonic clock stopped (macOS, Linux). transition()
//     never saw the stretch, so deduct nothing - deducting would shrink a real
//     outage below its observed length.
//   - monoGap ~ wallGap: the clock ran straight through (Windows; also a process
//     stalled awake, e.g. a long GC pause or a suspended container). transition()
//     counted every second of it, so deduct all of it.
//
// Anything between is a partial freeze; deduct what the clock actually saw. The
// result is clamped to the wall gap so a clock that reports MORE monotonic time
// than wall time cannot over-deduct.
func unobservedInOutage(wallGap, monoGap time.Duration) time.Duration {
	if monoGap <= 0 {
		return 0
	}
	if monoGap > wallGap {
		return wallGap
	}
	return monoGap
}

// resetStreaks clears the overall, per-family, and degraded debounce counters.
// Run calls it when a round is skipped because monitoring is paused, so the
// consecutive-rounds contract holds across a pause: a streak built up before
// pausing can't confirm a transition with a single round after resume.
func (m *Monitor) resetStreaks() {
	m.okStreak, m.badStreak = 0, 0
	for _, fs := range m.fams {
		fs.okStreak, fs.badStreak = 0, 0
	}
	// Same "a streak can't survive a pause" contract for degradation: otherwise a
	// host one round shy of the threshold before a pause could fire OnDegraded on
	// the first post-resume round.
	m.resetDegraded()
}

// notePause marks when a pause began while the link was down. On resume the
// unwatched stretch is folded into pausedGap and excluded from the outage's
// recorded duration - the link may have recovered with nobody looking, so that
// time isn't counted as downtime, while time actually observed down before the
// pause and after the resume still is.
func (m *Monitor) notePause(now time.Time) {
	if !m.online && m.downPausedAt.IsZero() {
		m.downPausedAt = now
	}
}

// noteResume folds the just-ended pause episode's unwatched span into pausedGap.
// Runs on the resume edge before the next round, so a still-down outage keeps
// counting observed time from here while the paused gap stays excluded.
func (m *Monitor) noteResume(now time.Time) {
	if !m.downPausedAt.IsZero() {
		if d := now.Sub(m.downPausedAt); d > 0 {
			m.pausedGap += d
		}
		m.downPausedAt = time.Time{}
	}
}

// resolveTime is prober.ResolveTime behind a var so tests can stub the DNS probe.
var resolveTime = prober.ResolveTime

// insertEvent is Store.InsertEvent behind a var (a method expression) so tests
// can inject a failing store and exercise the pending-event retry buffer without
// wiring a whole store interface. Mirrors the resolveTime seam above.
var insertEvent = (*store.Store).InsertEvent

// insertPause is Store.InsertPause behind a var so tests can capture the pause
// spans the loop records (uptime's observed-time denominator; see Run).
var insertPause = (*store.Store).InsertPause

// nowFn is time.Now behind a var so tests can drive Run's WALL clock - a suspend,
// an NTP step - without waiting real hours. It supplies the wall readings only: the
// pause spans, the startup gap and the wall-gap check. Run's SCHEDULING anchors
// (lastRound, time.Until, the timer) deliberately stay on the real monotonic clock,
// because a suspend has to look to them exactly as it does in production - frozen -
// and because the whole point of unobservedGap is that it disagrees with them.
// Mirrors the resolveTime / insertEvent / insertPause seams.
var nowFn = time.Now

// pendingEvent is a confirmed transition whose durable InsertEvent failed. It is
// retried verbatim on a later round until it lands; only the persisted record is
// buffered, never the callbacks/logs/counters, which already fired when the
// transition was first observed.
type pendingEvent struct {
	ts       time.Time // the already-clamped wall time to persist (retry reuses it exactly)
	kind     string    // "up" | "down"
	duration int       // outage length for "up"; negative stores NULL (as InsertEvent expects)
}

// maxPendingEvents bounds the retry buffer so a persistently broken store can't
// leak memory. Past the cap the oldest record is dropped (a very stale outage
// boundary matters less than the recent ones).
const maxPendingEvents = 64

// round performs a single probe, persists samples, and advances the state
// machine.
func (m *Monitor) round(ctx context.Context) {
	// Retry any transition events whose durable write failed on an earlier round,
	// before anything else, so a store that has since recovered catches up
	// promptly. resolveDanglingDowns can't rescue a lost event - it only bounds a
	// dangling 'down' from sample evidence weeks later at Prune, and can't recreate
	// a 'down' that was never written - so this retry is the only path back.
	m.flushPendingEvents(ctx)

	res := m.prober.Probe(ctx, time.Now())
	// A round raced by shutdown measured nothing real: cancelled dials read as
	// all-failed targets (NOT Skipped), and confirming them would fabricate a
	// LINK DOWN - an FSM flip, a down-alert, a monitor.downs bump and a doomed
	// event write - as the daemon exits. Checked AFTER Probe deliberately, so a
	// ctx cancelled DURING the probe (whose results are cancellation artifacts)
	// is caught too. Treat it as an observation gap, exactly like Skipped below.
	if ctx.Err() != nil {
		m.resetStreaks()
		return
	}
	// A skipped round measured nothing (the last enabled family flipped to
	// "off" between Run's probing() gate and the Probe call). Treat it as
	// idle, not failed: advancing the FSM here would confirm a false outage
	// from a round that never touched the network. Reset the debounce streaks
	// too - this is an observation gap exactly like a pause, so a streak built
	// before it must not confirm a transition with one round after it (the
	// resetStreaks-across-a-gap contract the pause branch also honours).
	if res.Skipped {
		m.resetStreaks()
		return
	}
	stats.Set("probe.last_round_ts", res.TS.Unix()) // freshness anchor (monitor.rounds already counts rounds)

	sms := make([]store.Sample, 0, len(res.Targets))
	for _, tr := range res.Targets {
		ms := util.DurMS(tr.Latency)
		sms = append(sms, store.Sample{
			TS:        res.TS,
			Target:    tr.Target.Name,
			Family:    tr.Target.Family,
			Success:   tr.OK,
			LatencyMS: ms,
		})
		if tr.OK {
			stats.Observe("probe.latency", tr.Latency.Seconds()) // per-target RTT distribution
		}
		if !tr.OK {
			// Why a dial failed (closed enum, no host detail): the probe.fail.* counter
			// on /metrics AND a debug line, so "which target, and why" needs no scrape.
			// Successes are deliberately NOT logged per-target - the family/round
			// summaries below cover the healthy case and keep debug readable (one
			// healthy round was ~11 lines; this drops it to the summaries).
			class := prober.DialErrClass(tr.Err)
			stats.Inc("probe.fail." + class)
			m.log.Debug("probe target", "target", tr.Target.Name, "family", tr.Target.Family,
				"ok", false, "reason", class)
		}
	}
	if err := m.store.InsertSamples(ctx, sms); err != nil {
		m.log.Error("insert samples", "err", err)
	}

	// DNS-resolution latency, a separate signal from the IP-anchor latency above
	// (which deliberately bypasses DNS). Run it OFF the round goroutine: a slow or
	// failing resolver can take up to the lookup timeout, and round duration adds to
	// the cadence, so doing it inline would stretch latency monitoring exactly when
	// DNS is unhealthy. Single-flighted so a hung lookup can't pile up goroutines; if
	// one is still running, skip this round's DNS sample.
	dnsOn := m.dnsEnabled()
	if !dnsOn && m.dnsWasEnabled {
		// Toggle-off edge: the operator just switched the DNS sub-toggle off. Drop
		// the seed so re-enabling starts as "no reading yet" instead of resurfacing
		// the pre-disable value as live (Snapshot gates on dnsSeen), and bump the
		// generation so a probe launched while enabled discards its result rather
		// than publishing under a now-disabled feature - the same guard a pause uses.
		m.mu.Lock()
		m.dnsSeen = false
		m.dnsGen++
		m.mu.Unlock()
	}
	m.dnsWasEnabled = dnsOn
	if dnsOn && m.dnsBusy.CompareAndSwap(false, true) {
		ts := res.TS
		m.mu.Lock()
		gen := m.dnsGen // snapshot: a pause or toggle-off bumps this, invalidating our result
		m.mu.Unlock()
		m.dnsWG.Add(1)
		go func() {
			defer m.dnsWG.Done()
			defer m.dnsBusy.Store(false)
			dur, ok, derr := resolveTime(ctx)
			ms := util.DurMS(dur)
			// A pause or DNS toggle-off that landed while this lookup was in flight
			// bumped dnsGen and dropped the seed, invalidating the whole result -
			// including the /metrics counters. Re-check the generation BEFORE emitting
			// any stats, or a stale probe pollutes dns.attempts / dns.latency /
			// dns.fail.* under a now-disabled feature (the later gen check below only
			// guards the seed/transition, too late for these counters).
			m.mu.Lock()
			stale := m.dnsGen != gen
			m.mu.Unlock()
			if stale {
				return
			}
			stats.Inc("dns.attempts")
			stats.Set("dns.last_attempt_ts", time.Now().Unix())
			if ok {
				stats.Observe("dns.latency", dur.Seconds())
				stats.Set("dns.last_ok_ts", time.Now().Unix())
			}
			// A lookup that failed because the monitor is shutting down (ctx
			// cancelled) is not the resolver going down: treat it as neutral - no
			// dns.fail.* bump and no "dns down" warning - so a normal stop can't
			// poison the recovered-rate counters or log a phantom resolver outage.
			// (InsertDNS is already skipped on cancellation below.)
			if !ok && ctx.Err() != nil {
				return
			}
			reason := ""
			if !ok {
				reason = prober.DialErrClass(derr)
				stats.Inc("dns.fail." + reason) // parallels probe.fail.* on /metrics
			}
			m.log.Debug("dns probe", "ok", ok, "resolve_ms", util.Round1(ms), "reason", reason)
			m.mu.Lock()
			if m.dnsGen != gen {
				// A pause or a DNS toggle-off landed while this probe was in flight;
				// the seed was already dropped, so don't resurrect a stale reading
				// (nor log a transition or insert a sample against a disabled span).
				m.mu.Unlock()
				return
			}
			prevOK, prevSeen := m.dnsOK, m.dnsSeen
			m.dnsMS, m.dnsOK, m.dnsSeen = ms, ok, true
			m.mu.Unlock()
			// DNS up/down transitions at the default level (mirrors LINK DOWN), so
			// "link up but sites won't load" leaves a trace without enabling debug.
			switch {
			case !ok && (!prevSeen || prevOK):
				m.log.Warn("dns down", "reason", reason, "resolve_ms", util.Round1(ms))
			case ok && prevSeen && !prevOK:
				m.log.Info("dns recovered", "resolve_ms", util.Round1(ms))
			}
			if ctx.Err() != nil {
				return // shutting down: don't write to a store that's about to close
			}
			if err := m.store.InsertDNS(ctx, ts, ms, ok); err != nil {
				m.log.Error("insert dns", "err", err)
			}
		}()
	}

	for _, fr := range res.Families {
		m.advanceFamily(fr, res.TS)
		m.log.Debug("family round", "family", fr.Family, "online", fr.Online,
			"ok", fr.OK, "total", fr.Total, "latency_ms", util.Round1(util.DurMS(fr.Latency)))
	}
	m.noteFamilies(res)
	m.advance(ctx, res)
	// Base latency for degradation detection: the lowest latency among families that
	// PASSED quorum (FamilyResult.Latency is already each family's min over its
	// successful targets). Keying off online families means a lone fast responder in
	// a DOWNED family can't mask a brownout on the family carrying traffic, and a
	// partial outage (some anchor answers but quorum failed) stays owned by the
	// up/down machine instead of mis-firing as degraded. Gate on the DEBOUNCED
	// state (m.online, updated by advance above), not the raw per-round res.Online,
	// so a single failed-quorum blip inside a continuous brownout doesn't re-arm the
	// once-per-episode latch. haveBest is false when no family answered this round.
	best, haveBest := 0.0, false
	for _, fr := range res.Families {
		if !fr.Online {
			continue
		}
		if ms := util.DurMS(fr.Latency); !haveBest || ms < best {
			best, haveBest = ms, true
		}
	}
	m.checkDegraded(best, haveBest, m.online)
	m.log.Debug("probe round", "online", res.Online, "best_ms", util.Round1(best),
		"ok_streak", m.okStreak, "bad_streak", m.badStreak)
}

// degradedRounds debounces the degradation trigger: latency must stay over the
// threshold for this many consecutive rounds before OnDegraded fires, so a single
// blip doesn't kick off a test.
const degradedRounds = 2

// checkDegraded fires OnDegraded at most once per degraded episode. online is the
// DEBOUNCED link state (m.online): when the link is down the outage belongs to the
// up/down machine + OnReconnect, so this resets instead of counting as degradation.
// haveReading is false when the round produced no latency sample (every family
// failed quorum) - a blip that didn't down the link; that holds the current
// episode rather than counting as a recovery, so one blip inside a continuous
// brownout can't re-arm and re-fire. The latch re-arms only when a real reading
// recovers below the threshold - or when the dispatch hands the episode back
// because it never got a measurement started (see RetryDegraded).
//
// The episode and the dispatch are two states, not one: the episode counter must
// count brownouts, so a re-dispatch inside one episode must not bump it again.
func (m *Monitor) checkDegraded(best float64, haveReading, online bool) {
	thr := 0.0
	if m.DegradedPingFn != nil {
		thr = m.DegradedPingFn()
	}
	if thr <= 0 || !online {
		m.resetDegraded()
		return
	}
	if !haveReading {
		return
	}
	if best > thr {
		m.degradedStreak++
		if m.degradedStreak < degradedRounds {
			return
		}
		if !m.degraded {
			m.degraded = true
			stats.Inc("monitor.degraded_episodes") // brownout count for /metrics (monitor. is allow-listed)
		}
		// A dispatch that bounced off a speedtest already in flight measured nothing,
		// so it must not consume the episode: re-arm, and the next confirmed round
		// dispatches again. Reports are matched by dispatch id and ids never repeat,
		// so one that arrives late - after its episode ended, or after a later
		// dispatch replaced it - names an id nothing matches: it can neither re-fire
		// an episode already served nor pile a second run on top of one in flight.
		// The swap consumes the report as it is honoured.
		if id := m.degradedID.Load(); m.degradedSent && m.degradedRetry.CompareAndSwap(id, 0) {
			m.degradedSent = false
		}
		if !m.degradedSent && m.OnDegraded != nil {
			m.degradedSent = true
			// Published BEFORE the callback: it reads the id synchronously to name
			// the dispatch a later bounce report is about.
			m.degradedSeq++
			m.degradedID.Store(m.degradedSeq)
			m.OnDegraded()
		}
		return
	}
	m.resetDegraded()
}

// resetDegraded ends the current degraded episode: the next brownout counts and
// dispatches as a new one. The pending-retry report is deliberately left alone -
// it names a dispatch id that is now dead, and ids never repeat.
func (m *Monitor) resetDegraded() {
	m.degradedStreak, m.degraded, m.degradedSent = 0, false, false
	m.degradedID.Store(0)
}

// DegradedDispatch returns the id of the dispatch that owns the degraded episode
// in progress, or 0 when the link is not degraded. Read inside OnDegraded (on the
// monitor goroutine) it names THAT dispatch, which is what RetryDegraded reports
// against.
func (m *Monitor) DegradedDispatch() uint64 { return m.degradedID.Load() }

// RetryDegraded reports that dispatch id never started a measurement - it bounced
// off a speedtest already running - so the episode is re-armed and the next
// confirmed degraded round dispatches again. Without it a single collision costs
// the whole brownout its measurement: the latch otherwise re-arms only when
// latency RECOVERS, which is exactly when there is nothing left to measure. Safe
// from any goroutine; ignored once that dispatch is no longer the live one.
func (m *Monitor) RetryDegraded(id uint64) {
	if id == 0 {
		return
	}
	// The slot only moves FORWARD. Reports come back from another goroutine, so
	// they can arrive out of order: dispatch 1 bounces, the episode re-arms,
	// dispatch 2 goes out and bounces, and only then does 1's report land. A
	// plain store let that late one overwrite the live report - and since the
	// consumer honours only a report naming the CURRENT dispatch, it then
	// matched nothing at all: the episode sat re-armed in name only and the
	// brownout went unmeasured until latency recovered, which is precisely when
	// there is nothing left to measure.
	//
	// Ids are handed out in increasing order and never repeat, so "newer" is
	// just "larger", and a stale report can neither displace a live one nor
	// resurrect an episode of its own (the consumer's id match still decides
	// that).
	for {
		cur := m.degradedRetry.Load()
		if cur >= id {
			return // a same-or-newer report is already pending
		}
		if m.degradedRetry.CompareAndSwap(cur, id) {
			return
		}
	}
}

// noteFamilies records which families this round actually probed, so the
// snapshot only reports live families (e.g. IPv6 disappears from the UI when
// its mode is toggled off, rather than freezing at its last state).
func (m *Monitor) noteFamilies(res prober.Result) {
	act := make(map[string]bool, len(res.Families))
	for fam := range res.Families {
		act[fam] = true
	}
	m.mu.Lock()
	m.active = act
	m.mu.Unlock()

	// A family that stopped being probed (mode toggled off, or gone under "auto")
	// loses its streaks - the resetStreaks contract across a pause: rounds from
	// before the gap can't combine with later ones to confirm a flip.
	for fam, fs := range m.fams {
		if !act[fam] {
			fs.okStreak, fs.badStreak = 0, 0
		}
	}

	// Asymmetry counter: seconds spent with exactly one family failing while both
	// were probed (how often brokenness is v4- or v6-only). Credit the TRUE spacing
	// since the previous round - interval plus round duration - because the failing
	// family's dial timeout is what stretches rounds during asymmetric outages, so a
	// flat per-interval credit would undercount. Gaps beyond any plausible round
	// spacing (resume from pause, suspend) fall back to one interval, so a pause
	// never counts as asymmetric downtime.
	delta := m.interval()
	if !m.lastNoteAt.IsZero() {
		if d := res.TS.Sub(m.lastNoteAt); d > 0 && d <= m.interval()+60*time.Second {
			delta = d
		}
	}
	m.lastNoteAt = res.TS
	v4, ok4 := res.Families["ipv4"]
	v6, ok6 := res.Families["ipv6"]
	if ok4 && ok6 && v4.Online != v6.Online {
		if !v4.Online {
			stats.AddF("monitor.v4_only_down_s", delta.Seconds())
		} else {
			stats.AddF("monitor.v6_only_down_s", delta.Seconds())
		}
	}
}

// advanceFamily debounces and records one family's up/down state. Family
// transitions are logged (and reflected in the live snapshot) but do not write
// to the events table - the overall state owns the outage history/heatmap.
func (m *Monitor) advanceFamily(fr prober.FamilyResult, ts time.Time) {
	fs := m.fams[fr.Family]
	if fs == nil {
		// A family not probed at startup (IPv6 toggled on live, or appearing under
		// "auto"): start it optimistically online, like New.
		fs = &familyState{online: true, since: ts}
		m.mu.Lock()
		m.fams[fr.Family] = fs
		m.famOrder = append(m.famOrder, fr.Family)
		sort.Strings(m.famOrder) // keep display order stable ("ipv4" < "ipv6")
		m.mu.Unlock()
	}
	if fr.Online {
		fs.okStreak++
		fs.badStreak = 0
	} else {
		fs.badStreak++
		fs.okStreak = 0
	}
	flip := 0
	if !fs.online && fs.okStreak >= m.upAfter() {
		flip = 1
	} else if fs.online && fs.badStreak >= m.downAfter() {
		flip = -1
	}

	m.mu.Lock()
	fs.latency = util.DurMS(fr.Latency)
	if flip == 1 {
		fs.online = true
		fs.since = ts
	} else if flip == -1 {
		fs.online = false
		fs.since = ts
	}
	m.mu.Unlock()

	switch flip {
	case 1:
		m.log.Info("family reconnected", "family", fr.Family)
	case -1:
		m.log.Warn("family down", "family", fr.Family)
	}
	if flip != 0 {
		// Confirmed per-family transitions (does v4 or v6 flap more). The counter
		// name only ever comes from the family enum.
		switch fr.Family {
		case "ipv4":
			stats.Inc("monitor.flap.ipv4")
		case "ipv6":
			stats.Inc("monitor.flap.ipv6")
		}
	}
}

// advance applies debouncing: a state flip requires DownAfter consecutive
// failures (or UpAfter consecutive successes) to suppress flapping.
func (m *Monitor) advance(ctx context.Context, res prober.Result) {
	// Every completed round, good or bad: the denominator for bad_rounds, and the
	// liveness signal a wedged probe loop can't fake (rate == 0 on /metrics while
	// monitoring_paused is 0 means the prober stopped, not that the link is quiet).
	stats.Inc("monitor.rounds")
	if res.Online {
		// A failure streak ending while still officially online is a blip:
		// sub-debounce instability that never became a confirmed outage, so the
		// events table never records it. (Paused rounds clear streaks via
		// resetStreaks, not here, so a pause is never counted as a blip.)
		if m.online && m.badStreak > 0 {
			stats.Inc("monitor.blips")
			stats.SetMax("monitor.blip_streak_max", int64(m.badStreak))
			// A blip is the textbook "shows down but seems fine" cause; surface it at
			// the default level so it's visible without reproducing under debug.
			m.log.Info("blip", "bad_streak", m.badStreak, "down_after", m.downAfter())
		}
		m.okStreak++
		m.badStreak = 0
	} else {
		stats.Inc("monitor.bad_rounds") // every failed quorum round, confirmed or not
		m.badStreak++
		m.okStreak = 0
	}

	switch {
	case !m.online && m.okStreak >= m.upAfter():
		m.transition(ctx, true, res)
	case m.online && m.badStreak >= m.downAfter():
		m.transition(ctx, false, res)
	}
}

// transition records and logs a confirmed state change.
func (m *Monitor) transition(ctx context.Context, online bool, res prober.Result) {
	ts := res.TS
	m.mu.Lock()
	// Outage duration comes from ts.Sub(m.since). In production ts is time.Now(),
	// so it carries a MONOTONIC reading and the elapsed measurement is immune to a
	// backward WALL step (NTP correcting a fast RTC mid-outage): the monotonic
	// clock only ever advances. Clamp a negative result to 0 defensively - a
	// monotonic-free ts (a test, or a clock without a monotonic source) under a
	// backward step would otherwise book a negative outage.
	elapsed := ts.Sub(m.since)
	if elapsed < 0 {
		elapsed = 0
	}
	duration := int(elapsed.Seconds())
	if online {
		// The outage may have spanned one or more monitoring pauses: subtract the
		// unwatched paused time (folded in on each resume) so only time actually
		// observed down - before the pause and after the resume - is recorded.
		if gap := int(m.pausedGap.Seconds()); gap > 0 {
			if duration -= gap; duration < 0 {
				duration = 0
			}
		}
	}
	// The PERSISTED event timestamp is a separate concern from the duration above:
	// it is stored as ts.Unix() (WALL seconds) and events are ordered by ts. A
	// backward wall step can make this transition's wall clock earlier than the
	// event it closes; an 'up' stamped before its 'down' would make ORDER BY ts
	// DESC read the 'down' as newest and book a phantom outage. Comparing against
	// m.since with .Before() (as the old code did) is a no-op in production because
	// .Before() uses the monotonic reading, which never steps back - so clamp the
	// stored WALL time nondecreasing here, comparing .Unix() explicitly. For an
	// 'up' a tie (same Unix second) is enough: the pairing breaks ts ties
	// down-before-up, so an 'up' sharing its own down's second still closes it. A
	// 'down' must land strictly AFTER the 'up' before it, because that same tie
	// rule reads a 'down' sharing the PREVIOUS up's second as coming between that
	// up and its down - stealing the pairing, collapsing the prior outage to zero
	// width. The widen below manufactures exactly that tie without any help from
	// the clock: it stamps a recovery ahead of a stepped-back wall, and until the
	// wall catches up every new outage's 'down' clamps to precisely that second.
	evWall := ts
	if !m.lastEventWall.IsZero() {
		if evWall.Unix() < m.lastEventWall.Unix() {
			evWall = m.lastEventWall
		}
		if !online && evWall.Unix() == m.lastEventWall.Unix() {
			evWall = time.Unix(m.lastEventWall.Unix()+1, 0)
		}
	}
	// Ordering is not enough for an 'up': the pair it closes must also be as WIDE
	// as the wall window the read model reconstructs from it. observedOutageSpans
	// takes the stored pair, subtracts the recorded pause rows, then trims to
	// duration_s - and duration above already had the pause fold subtracted, so
	// the pair must span the RAW monotonic elapsed, not the duration: a pair only
	// duration wide has the pause subtracted from it a second time on read, and
	// books duration-minus-pause on uptime, the digest and the heatmap while
	// duration_s claims the full length. (A zero-width pair is worse still: no
	// observed spans at all, downtime deleted from uptime and the digest while
	// the heatmap's fallback anchor still booked it.) So stretch the stored
	// second to down + elapsed - the window an unstepped wall clock would have
	// stored, and the timeline the outage's pause rows were stamped into: the
	// monotonic elapsed is the one honest measurement here, and lastEventWall is
	// the paired 'down' (transitions alternate), already stamped on the pre-step
	// timeline this clamp preserves - the wall clock rejoins it as it catches up.
	// In ordinary runs wall >= monotonic, so this fires only when a backward step
	// (or slew) squeezed the pair below the window it needs.
	//
	// Elapsed alone is only the whole window when every pause row inside the
	// outage is also inside the monotonic elapsed - true for explicit pauses,
	// which advance both clocks. A SUSPEND on a frozen-monotonic platform books
	// its row for wall seconds the elapsed never contained, so those rows must be
	// added on top (frozenGap, measured row by row in bookUnobservedGap) or the
	// read model subtracts them from seconds the pair does not hold and books
	// duration-minus-suspend while duration_s claims the full length. An
	// unstepped wall stores down + elapsed + frozenGap on its own, so this still
	// fires only when a backward step squeezed the pair.
	if online && duration > 0 && !m.lastEventWall.IsZero() {
		if width := int64(elapsed.Seconds()) + int64(m.frozenGap.Seconds()); evWall.Unix()-m.lastEventWall.Unix() < width {
			evWall = time.Unix(m.lastEventWall.Unix()+width, 0)
		}
	}
	m.lastEventWall = evWall
	// A fresh accumulator incarnation begins here. Bumping the generation as the two
	// gaps reset lets a pendingGap booked into the OLD incarnation be recognised as
	// stale if its store row is refused or the record is evicted during this new
	// outage - reversing it now would corrupt the new outage's gaps (see B4).
	m.outageGen++
	m.downPausedAt, m.pausedGap, m.frozenGap = time.Time{}, 0, 0
	m.online = online
	m.since = ts // keep the monotonic-carrying ts, so the NEXT duration stays monotonic
	m.mu.Unlock()

	// A scrape-interval-robust outage counter for Prometheus
	// (changes(pingularity_up[..]) can miss an outage shorter than the scrape gap).
	// The duration sum accrues at recovery, when the outage length is known.
	if online {
		m.log.Info("LINK RECONNECTED", "downtime_s", duration)
		stats.AddF("monitor.outage_s_sum", float64(duration))
		if err := insertEvent(m.store, ctx, evWall, "up", duration, ""); err != nil {
			m.log.Error("insert event", "err", err)
			m.bufferPendingEvent(evWall, "up", duration)
		}
		if m.OnReconnect != nil {
			m.OnReconnect()
		}
	} else {
		// Self-explanatory at the default level: which families failed quorum
		// (ok/total), the dominant dial-failure class, and the debounce threshold.
		m.log.Warn("LINK DOWN", "families", famSummary(res), "reason", dominantFailClass(res),
			"down_after", m.downAfter())
		stats.Inc("monitor.downs")
		if err := insertEvent(m.store, ctx, evWall, "down", -1, ""); err != nil {
			m.log.Error("insert event", "err", err)
			m.bufferPendingEvent(evWall, "down", -1)
		}
	}
	if m.OnTransition != nil {
		m.OnTransition(online, duration)
	}
}

// bufferPendingEvent records a transition whose InsertEvent failed so a later
// round can retry it. A failed insert wrote nothing, so replaying the same record
// is idempotent. The buffer is bounded: past the cap the oldest record is dropped
// with a WARN, because an unbounded buffer under a persistently broken store would
// leak memory and the freshest outage boundaries are the ones worth keeping.
func (m *Monitor) bufferPendingEvent(ts time.Time, kind string, duration int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pendingEvents) >= maxPendingEvents {
		m.pendingEvents = m.pendingEvents[1:]
		stats.Inc("monitor.event_dropped") // a transition lost for good: uptime history gains a gap
		m.log.Warn("pending event buffer full, dropping oldest", "cap", maxPendingEvents)
	}
	m.pendingEvents = append(m.pendingEvents, pendingEvent{ts: ts, kind: kind, duration: duration})
	stats.Set("monitor.pending_events", int64(len(m.pendingEvents))) // retry-queue depth (0 = healthy)
}

// flushPendingEvents retries transition events whose InsertEvent failed on an
// earlier round. A failed insert wrote nothing, so replaying the SAME record is
// safe and idempotent; each success drops the record. Callbacks, logs, and
// counters are NOT replayed - they fired when the transition was first observed.
// Retries stop at the first failure so head-of-line ordering (a 'down' before the
// 'up' that closes it) is preserved and a still-broken store isn't hammered.
func (m *Monitor) flushPendingEvents(ctx context.Context) {
	for {
		m.mu.Lock()
		if len(m.pendingEvents) == 0 {
			m.mu.Unlock()
			return
		}
		ev := m.pendingEvents[0]
		m.mu.Unlock()
		if err := insertEvent(m.store, ctx, ev.ts, ev.kind, ev.duration, ""); err != nil {
			m.log.Debug("retry pending event failed", "kind", ev.kind, "err", err)
			return // still failing: keep it (and the rest) for the next round
		}
		// Only the monitor goroutine mutates the buffer, so the head we just
		// persisted is still the head; drop it and continue with the next.
		m.mu.Lock()
		m.pendingEvents = m.pendingEvents[1:]
		depth := len(m.pendingEvents)
		m.mu.Unlock()
		stats.Set("monitor.pending_events", int64(depth)) // reflect the drain
		m.log.Info("pending event flushed", "kind", ev.kind)
	}
}

// famSummary renders the round's per-family quorum as "ipv4:1/3 ipv6:0/2" for the
// LINK DOWN log (ok/total per family, stable order).
func famSummary(res prober.Result) string {
	parts := make([]string, 0, len(res.Families))
	for _, fam := range []string{"ipv4", "ipv6"} {
		if fr, ok := res.Families[fam]; ok {
			parts = append(parts, fmt.Sprintf("%s:%d/%d", fam, fr.OK, fr.Total))
		}
	}
	return strings.Join(parts, " ")
}

// dominantFailClass returns the most common dial-failure class among the round's
// failed targets (timeout/refused/dns/…), for the LINK DOWN log.
func dominantFailClass(res prober.Result) string {
	counts := map[string]int{}
	for _, tr := range res.Targets {
		if !tr.OK {
			counts[prober.DialErrClass(tr.Err)]++
		}
	}
	best, bestN := "", 0
	for c, n := range counts {
		if n > bestN {
			best, bestN = c, n
		}
	}
	return best
}
