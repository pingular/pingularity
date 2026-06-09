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

	mu         sync.RWMutex   // guards online/since and family state for readers
	online     bool           // current overall debounced state
	since      time.Time      // when the current overall state began
	dnsMS      float64        // last DNS-probe resolve time in ms (guarded by mu)
	dnsOK      bool           // last DNS probe resolved successfully (guarded by mu)
	dnsSeen    bool           // a DNS probe has produced at least one result (guarded by mu)
	dnsGen     uint64         // bumped on pause to discard an in-flight DNS probe's late result (guarded by mu)
	dnsBusy    atomic.Bool    // a DNS probe goroutine is in flight (single-flight)
	dnsWG      sync.WaitGroup // tracks the in-flight DNS probe goroutine so Run waits for it before returning (don't write to a store about to Close)
	okStreak   int            // consecutive online rounds (monitor goroutine only)
	badStreak  int            // consecutive offline rounds (monitor goroutine only)
	lastNoteAt time.Time      // previous round's TS, for asymmetry spacing (monitor goroutine only)

	downPausedAt time.Time     // when the current pause episode began while down; folded into pausedGap on resume (monitor goroutine only)
	pausedGap    time.Duration // total unwatched paused time in the current outage, excluded from its recorded duration (monitor goroutine only)

	degradedStreak int  // consecutive rounds over the degraded-latency threshold (monitor goroutine only)
	degraded       bool // currently inside a degraded episode, so OnDegraded fires once (monitor goroutine only)

	fams     map[string]*familyState // per-address-family state
	famOrder []string                // stable display order of families
	active   map[string]bool         // families probed in the most recent round

	// OnReconnect, if set, fires when the link comes back online; used to trigger
	// a speedtest. Called synchronously from the probe loop; keep it quick or async.
	OnReconnect func()

	// OnDegraded, if set, fires when the link is online but its base latency stays
	// above DegradedPingFn()'s threshold for degradedRounds rounds - catching a
	// brownout the reconnect hook would miss. Fires once per episode; re-arms when
	// latency recovers below the threshold. Called synchronously, like OnReconnect.
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

	if m.probing() {
		m.round(ctx) // probe immediately, don't wait a full interval
	}
	lastRound := time.Now()
	paused := false         // tracks the running→paused edge for pause accounting
	var pauseTick time.Time // last moment paused time was accrued up to
	rounds := 0             // probe rounds completed (for the liveness line)
	lastAlive := time.Now() // last "monitor alive" emission
	// One reusable timer, not a fresh time.After each iteration: settings broadcasts
	// fire `wake` on every change, so a burst would otherwise orphan one timer per
	// wake until it expired. Go 1.23+ Reset/Stop are drain-free.
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	for {
		var wake <-chan struct{}
		if m.WakeFn != nil {
			wake = m.WakeFn()
		}
		// Re-read the interval each round so runtime changes take effect; keep the
		// lastRound anchor so a settings broadcast only re-derives the deadline
		// rather than restarting the wait.
		wait := time.Until(lastRound.Add(m.interval()))
		// Resume edge: probing turned back on while a pause was still open. Close the
		// episode here, crediting only the real switched-off time up to now - deferring
		// it to the next due round would fold on-but-idle wait time into
		// monitor.paused_s and pausedGap.
		if paused && m.probing() {
			stats.AddF("monitor.paused_s", time.Since(pauseTick).Seconds())
			paused = false
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
			now := time.Now()
			if !paused {
				paused = true
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
			}
			pauseTick = now
			m.resetStreaks()
			wait = scheduleRecheck
		case wait <= 0:
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
	m.degradedStreak, m.degraded = 0, false
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

// round performs a single probe, persists samples, and advances the state
// machine.
func (m *Monitor) round(ctx context.Context) {
	res := m.prober.Probe(ctx, time.Now())
	// A skipped round measured nothing (the last enabled family flipped to
	// "off" between Run's probing() gate and the Probe call). Treat it as
	// idle, not failed: advancing the FSM here would confirm a false outage
	// from a round that never touched the network.
	if res.Skipped {
		return
	}

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
	if m.dnsEnabled() && m.dnsBusy.CompareAndSwap(false, true) {
		ts := res.TS
		m.mu.Lock()
		gen := m.dnsGen // snapshot: a pause bumps this, invalidating our result
		m.mu.Unlock()
		m.dnsWG.Add(1)
		go func() {
			defer m.dnsWG.Done()
			defer m.dnsBusy.Store(false)
			dur, ok, derr := resolveTime(ctx)
			ms := util.DurMS(dur)
			reason := ""
			if !ok {
				reason = prober.DialErrClass(derr)
				stats.Inc("dns.fail." + reason) // parallels probe.fail.* on /metrics
			}
			m.log.Debug("dns probe", "ok", ok, "resolve_ms", util.Round1(ms), "reason", reason)
			m.mu.Lock()
			if m.dnsGen != gen {
				// A pause landed while this probe was in flight; the pre-pause
				// seed was already dropped, so don't resurrect a stale reading
				// (nor log a transition or insert a sample against a paused span).
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
// recovers below the threshold.
func (m *Monitor) checkDegraded(best float64, haveReading, online bool) {
	thr := 0.0
	if m.DegradedPingFn != nil {
		thr = m.DegradedPingFn()
	}
	if thr <= 0 || !online {
		m.degradedStreak = 0
		m.degraded = false
		return
	}
	if !haveReading {
		return
	}
	if best > thr {
		m.degradedStreak++
		if m.degradedStreak >= degradedRounds && !m.degraded {
			m.degraded = true
			stats.Inc("monitor.degraded_episodes") // brownout count for /metrics (monitor. is allow-listed)
			if m.OnDegraded != nil {
				m.OnDegraded()
			}
		}
		return
	}
	m.degradedStreak = 0
	m.degraded = false
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
	// A backward clock step (e.g. NTP correcting a fast RTC mid-outage) can make
	// this round's wall clock earlier than m.since. Never stamp an event before
	// the one it closes - an 'up' ordered before its 'down' would make the newest
	// event read as 'down' and book a phantom outage - and never accrue a negative
	// duration into the monotonic outage counter.
	if ts.Before(m.since) {
		ts = m.since
	}
	duration := int(ts.Sub(m.since).Seconds())
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
	m.downPausedAt, m.pausedGap = time.Time{}, 0
	m.online = online
	m.since = ts
	m.mu.Unlock()

	// A scrape-interval-robust outage counter for Prometheus
	// (changes(pingularity_up[..]) can miss an outage shorter than the scrape gap).
	// The duration sum accrues at recovery, when the outage length is known.
	if online {
		m.log.Info("LINK RECONNECTED", "downtime_s", duration)
		stats.AddF("monitor.outage_s_sum", float64(duration))
		if err := m.store.InsertEvent(ctx, ts, "up", duration, ""); err != nil {
			m.log.Error("insert event", "err", err)
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
		if err := m.store.InsertEvent(ctx, ts, "down", -1, ""); err != nil {
			m.log.Error("insert event", "err", err)
		}
	}
	if m.OnTransition != nil {
		m.OnTransition(online, duration)
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
