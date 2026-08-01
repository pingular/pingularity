// Package digest sends a periodic summary of connection health - uptime, median
// speeds, outage count + downtime - to the alert webhook, so an operator gets a
// recurring report instead of only ever hearing about failures. The cadence is
// user-selectable (off/daily/weekly); the last-sent time is persisted so a
// restart can't spam digests, and after a long gap (host off for days) the next
// digest covers only the most recent period rather than one giant catch-up report.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/util"
)

// keyLastSent persists when the last digest went out (unix seconds), so cadence
// survives restarts.
const keyLastSent = "digest_last_sent"

// checkInterval is how often the loop wakes to see whether a digest is due. The
// cadence (daily/weekly) is far coarser, so a 30-min poll is cheap and keeps a
// send within half an hour of its due time. A var so tests can shrink it.
var checkInterval = 30 * time.Minute

// Sender delivers a formatted message to the alert destination, returning a
// non-nil error only on a real delivery failure - NOT for an empty URL, which
// is a no-op. *notify.Notifier satisfies it; kept minimal so this package
// doesn't import notify.
type Sender interface {
	Send(ctx context.Context, message string, fields map[string]any) error
}

// Manager runs the periodic-digest loop.
type Manager struct {
	Store  *store.Store
	Notify Sender
	Log    *slog.Logger
	// FreqFn returns the live cadence ("daily"|"weekly"; anything else, incl.
	// "off"/"", disables). Read each tick so a settings change applies live.
	FreqFn func() string

	// RetentionFn returns the live downtime-retention window (0 = keep forever), so
	// UptimeSince can clamp the digest window to what event history actually covers.
	// Without it a weekly digest under a sub-week retention would read the pruned
	// early days as outage-free and overstate uptime. nil = no clamp (0).
	RetentionFn func() time.Duration

	now func() time.Time // overridable in tests

	// pendingSince marks the start of a window whose delivery is being retried after
	// a send failure (zero = nothing pending). A retried window is never re-capped,
	// so a sustained webhook outage can't silently drop the earliest slice of an
	// undelivered digest; the stale-watermark cap below applies only to a fresh
	// (never-attempted) window. Loop-goroutine-only, like the rest of tick's state.
	pendingSince time.Time

	// lastSentMem mirrors the persisted last-sent watermark in memory. setLastSent
	// is best-effort (logs, doesn't propagate), so if its write fails after a
	// successful delivery the stored watermark stays stale and the next tick would
	// re-send the same digest. Taking max(stored, lastSentMem) closes that
	// duplicate window for the life of the process. Loop-goroutine-only.
	lastSentMem time.Time
}

func (m *Manager) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// period maps a cadence string to its interval; 0 means disabled.
func period(freq string) time.Duration {
	switch freq {
	case "daily":
		return 24 * time.Hour
	case "weekly":
		return 7 * 24 * time.Hour
	default:
		return 0
	}
}

// Loop runs until ctx is cancelled, checking on each tick whether a digest is
// due and sending one if so. It checks once shortly after start so a digest that
// came due while the process was down doesn't wait a full poll interval.
func (m *Manager) Loop(ctx context.Context) {
	t := time.NewTicker(checkInterval)
	defer t.Stop()
	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Manager) tick(ctx context.Context) {
	p := period(m.FreqFn())
	// Only the digest's own cadence setting suppresses a digest. There used to be a
	// second gate here on the monitoring master switch, justified by "a paused
	// monitor records no outages, so the digest would email a false 100%" - but the
	// false 100% came from Summarize DISCARDING coverage, and the gate never
	// covered the cases that actually produce it: pause spans are written whenever
	// probing() is false, which includes the latency schedule window and the
	// latency sub-toggle, and both leave Monitoring() true. So an operator on an
	// 8h/day schedule was never gated and got a confident 100.00% every single day,
	// while an operator who pressed the power button got silence - the digest
	// disclosing nothing in one case and saying nothing at all in the other.
	//
	// Summarize now states the observed span, so the honest report is available in
	// every case and suppression buys nothing: silence is indistinguishable from a
	// broken webhook, and it is the same false all-clear that omitting a series
	// gives a Prometheus alert. Always send; say what was observed.
	if p <= 0 {
		m.pendingSince = time.Time{} // disabled: drop any pending retry so re-enabling arms/caps fresh
		return                       // leave last-sent untouched so re-enabling arms cleanly
	}
	now := m.clock()
	// A backward wall-clock step (NTP correction, VM snapshot restore) can leave
	// the in-memory watermark - and any pending retry window - ahead of now.
	// lastSent() already discards a future stored watermark; do the same for the
	// in-memory copies, so the digest re-arms from now instead of going silent
	// until real time climbs back past the stale future value.
	if m.lastSentMem.After(now) {
		m.lastSentMem = time.Time{}
	}
	if m.pendingSince.After(now) {
		m.pendingSince = time.Time{}
	}
	last := m.lastSent(ctx)
	if m.lastSentMem.After(last) {
		last = m.lastSentMem // a prior setLastSent write may have failed; trust memory
	}
	if last.IsZero() {
		// Never-armed install: arm for one period out instead of dumping an
		// all-time summary on the first tick.
		m.setLastSent(ctx, now)
		m.lastSentMem = now
		return
	}
	if now.Sub(last) < p {
		return // not due yet
	}
	since := last
	if m.pendingSince.IsZero() {
		// Fresh window. Cap it at one period, with one poll interval of slack: a
		// normal tick fires up to checkInterval past due (the poll grid resets on
		// restart), and capping then would drop that sliver from every digest.
		// Beyond the slack the watermark is stale (disabled or down for long) -
		// cover only the last period, not the whole paused gap.
		if now.Sub(since) > p+checkInterval {
			since = now.Add(-p)
		}
		m.pendingSince = since
	} else {
		// Retrying a window a previous send failed to deliver: keep its original
		// start (extended to the new now) and do NOT re-cap - re-capping on each
		// retry would silently drop the earliest, still-undelivered slice.
		since = m.pendingSince
	}
	if err := m.send(ctx, since, now); err != nil {
		// Real failure: keep last-sent + pendingSince so the next tick retries this
		// window instead of advancing past an undelivered summary.
		m.Log.Warn("digest send", "err", err)
		return
	}
	m.setLastSent(ctx, now)
	m.lastSentMem = now // advance in memory even if the store write above failed
	m.pendingSince = time.Time{}
}

func (m *Manager) send(ctx context.Context, since, now time.Time) error {
	s, err := m.Summarize(ctx, since, now)
	if err != nil {
		return err
	}
	if err := m.Notify.Send(ctx, s.Message(), s.Fields()); err != nil {
		return err
	}
	m.Log.Info("digest sent",
		"window", s.Window.String(), "observed", s.Obs.Observed.String(),
		"uptime_pct", util.Round2(s.UptimePct()), "measured", s.Obs.Defined(),
		"outages", s.Outages, "downtime_s", s.DowntimeS, "speed_runs", s.Runs)
	return nil
}

// Summary is one period's roll-up.
type Summary struct {
	Since, Until time.Time
	// Window is the span the digest was ASKED for (Until-Since). Obs.Window may be
	// shorter - UptimeSince clamps to when monitoring began and to the retention
	// horizon - and Obs.Observed shorter still. The disclosure line compares the
	// two, so "last 1d" can never again describe eight watched hours in silence.
	Window time.Duration
	// Obs is the uptime evidence, ratio and coverage inseparable. There is no
	// UptimePct FIELD any more: a Summary that carries a percentage but not the
	// coverage behind it is exactly the object this cluster kept re-creating.
	Obs  store.Observation
	Runs int
	// Median speeds/ping are nil when no run measured that direction/metric (e.g.
	// a download-only speed setting never measures upload), so absence is rendered
	// as "-" rather than a fake measured 0.
	MedDownMbps *float64
	MedUpMbps   *float64
	MedPingMS   *float64
	Outages     int
	DowntimeS   int
}

// Summarize rolls one window up without sending it. Exported so a cross-package
// test can put the digest's figures beside /api/status, /metrics and the heatmap
// over the SAME window (internal/web's four-renderer agreement test) - the seam
// this cluster kept drifting at, and the one thing no test in the tree could see
// before, because every cross-component fixture stopped at the store boundary.
func (m *Manager) Summarize(ctx context.Context, since, now time.Time) (Summary, error) {
	s := Summary{Since: since, Until: now, Window: now.Sub(since)}

	// Clamp the window to downtime retention: usually the digest period (<= a week)
	// sits well inside the default 365-day retention and this is a no-op, but an
	// operator who set retention SHORTER than the digest cadence would otherwise have
	// UptimeSince read the pruned early days as outage-free and report a falsely high
	// uptime.
	//
	// The coverage is kept, not dropped. The comment that used to stand here said it
	// could be ignored because "the digest independently suppresses a paused period,
	// so it only reaches here with a genuinely-observed window" - which was false in
	// both halves: the suppression was on the monitoring master switch, and a
	// scheduled-off or latency-disabled window leaves that switch ON while writing
	// pause spans all day. A digest for a window that observed 8 of 24 hours reached
	// this line every day and left with a bare 100.00%.
	var retention time.Duration
	if m.RetentionFn != nil {
		retention = m.RetentionFn()
	}
	obs, err := m.Store.UptimeSince(ctx, since, retention)
	if err != nil {
		return s, fmt.Errorf("digest uptime: %w", err)
	}
	s.Obs = obs

	runs, err := m.Store.SpeedHistory(ctx, since)
	if err != nil {
		return s, fmt.Errorf("digest speed history: %w", err)
	}
	s.Runs = len(runs)
	// A direction an iperf3 partial run never measured is recorded as 0 Mbps;
	// reject it (like PingMS below) so it can't drag the median toward 0, and
	// leave the median nil so absence is reported as "-", not a measured 0.
	s.MedDownMbps = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.DownMbps, r.DownMbps > 0 })
	s.MedUpMbps = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.UpMbps, r.UpMbps > 0 })
	s.MedPingMS = medianBy(runs, func(r store.SpeedSample) (float64, bool) { return r.PingMS, r.PingMS > 0 })

	// Outages resolved in the window. An outage still ongoing at send time isn't
	// counted yet and rolls into the next digest, so reported durations are final.
	// A direct aggregate so the count/downtime can't undercount a busy window.
	// The outage window gets the SAME retention clamp UptimeSince applied above:
	// Prune keeps an outage that straddles the retention floor whole (deleting
	// only its 'down' would orphan the 'up'), so an unclamped query would book
	// the straddler's full downtime while the uptime% describes only the
	// post-floor slice - one line contradicting itself ("1 outage · 25h down"
	// beside 97.96% over 49h observed). Clamping the start makes
	// ResolvedOutagesSince clip the straddler to the window the uptime% covers.
	outageSince := since
	if retention > 0 {
		if floor := now.Add(-retention); floor.After(outageSince) {
			outageSince = floor
		}
	}
	s.Outages, s.DowntimeS, err = m.Store.ResolvedOutagesSince(ctx, outageSince.Unix())
	if err != nil {
		return s, fmt.Errorf("digest outages: %w", err)
	}
	return s, nil
}

// discloseFloor is the shortfall between the window asked for and the span
// actually observed below which the digest says nothing about it.
//
// It exists only to keep a sub-second rounding difference (Since/Until carry
// nanoseconds; UptimeSince works in whole seconds) from appending a disclosure to
// every digest a perfectly healthy 24/7 monitor sends. It is NOT a
// quality threshold and it is not visible outside this package: /metrics'
// definedness guard takes no threshold at all, and deliberately cannot see this
// one. A real unobserved episode is minutes at minimum - the monitor only records
// a pause span for a gap past its detection threshold - so nothing genuine falls
// under a minute.
const discloseFloor = time.Minute

// observedLine is the digest's disclosure: what span the figures above actually
// describe, or "" when the window was observed end to end and there is nothing to
// disclose.
//
// It never suppresses the digest and never scolds. Monitoring is off for a
// scheduled window because the operator asked for it to be, so the sentence
// reports a span and lists the ordinary reasons without implying a fault; the
// alternative wording ("only 8h of 24h were monitored") reads as a warning about
// the user's own setting.
func (s Summary) observedLine() string {
	if s.Window-s.Obs.Observed < discloseFloor {
		return ""
	}
	if !s.Obs.Defined() {
		return fmt.Sprintf("observed none of %s - monitoring was off for the whole period, so there is no uptime figure for it",
			humanWindow(s.Window))
	}
	return fmt.Sprintf("observed %s of %s - the rest was not monitored (scheduled off, paused, asleep, or past retention); the figures above describe what was observed",
		humanWindow(s.Obs.Observed), humanWindow(s.Window))
}

// Message renders the human-readable digest line(s). Exported alongside Fields so
// a cross-package test can compare what the digest actually EMITS with what
// /api/status, /metrics and the heatmap emit for the same window.
func (s Summary) Message() string {
	speed := "no speedtests"
	if s.Runs > 0 {
		speed = fmt.Sprintf("median ↓ %s Mbps · ↑ %s Mbps · %s ms ping (%d test%s)",
			fmtMed(s.MedDownMbps, "%.1f"), fmtMed(s.MedUpMbps, "%.1f"), fmtMed(s.MedPingMS, "%.0f"), s.Runs, plural(s.Runs))
	}
	outages := "no outages"
	if s.Outages > 0 {
		outages = fmt.Sprintf("%d outage%s · %s down", s.Outages, plural(s.Outages), util.HumanDur(s.DowntimeS))
	}
	// A window that observed nothing has no percentage to report - Obs.Ratio is a
	// sentinel 1 there, not a measurement, and printing it as "100.00%" is the
	// original defect. Say so in words, exactly as /metrics omits the ratio series.
	uptime := "Uptime not measured"
	if s.Obs.Defined() {
		uptime = fmt.Sprintf("Uptime %.2f%%", s.UptimePct())
	}
	lines := []string{
		"📊 Pingularity summary · last " + humanWindow(s.Window),
		uptime + " · " + outages,
	}
	if obs := s.observedLine(); obs != "" {
		lines = append(lines, obs)
	}
	return strings.Join(append(lines, speed), "\n")
}

// UptimePct is the up-fraction of OBSERVED time as a percentage. It is a method,
// not a field, so it cannot be read off a Summary that was never handed its
// coverage; check Obs.Defined before presenting it (see Message).
func (s Summary) UptimePct() float64 { return s.Obs.Ratio() * 100 }

// Fields is the structured payload merged into a generic webhook body. A median
// a direction/metric never measured is omitted, not shipped as 0, so a consumer
// can tell "not measured" from a genuine zero.
func (s Summary) Fields() map[string]any {
	f := map[string]any{
		"event":      "digest",
		"window_s":   int(s.Window.Seconds()),
		"outages":    s.Outages,
		"downtime_s": s.DowntimeS,
		"speed_runs": s.Runs,
		// The observed span rides the structured payload too, not just the prose:
		// these fields are machine-consumed, and a consumer charting uptime_pct
		// needs the same qualifier a human reading the message gets. observed_s
		// against window_s is the digest's coverage_ratio.
		"observed_s": int(s.Obs.Observed.Seconds()),
	}
	// Omitted, not zeroed or faked, when the window observed nothing - the same
	// rule the medians below follow, so a consumer can tell "not measured" from a
	// genuine reading. A 100 here for an unwatched day is the exact figure this
	// change exists to stop shipping.
	if s.Obs.Defined() {
		f["uptime_pct"] = util.Round2(s.UptimePct())
	}
	if s.MedDownMbps != nil {
		f["median_down_mbps"] = util.Round1(*s.MedDownMbps)
	}
	if s.MedUpMbps != nil {
		f["median_up_mbps"] = util.Round1(*s.MedUpMbps)
	}
	if s.MedPingMS != nil {
		f["median_ping_ms"] = util.Round1(*s.MedPingMS)
	}
	return f
}

// fmtMed renders a nullable median with format, or "-" when the metric had no
// qualifying sample (e.g. a download-only run never measured upload/ping).
func fmtMed(v *float64, format string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf(format, *v)
}

func (m *Manager) lastSent(ctx context.Context) time.Time {
	all, err := m.Store.AllSettings(ctx)
	if err != nil {
		// On a read error, return zero (re-arm) rather than risk a spurious send;
		// at worst one digest is delayed.
		m.Log.Warn("digest read state", "err", err)
		return time.Time{}
	}
	n, err := strconv.ParseInt(all[keyLastSent], 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	t := time.Unix(n, 0)
	if t.After(m.clock()) {
		// A future watermark (the clock stepped back, e.g. it was wrong at boot
		// before NTP fixed it) would block every digest until real time caught
		// up; treat it as unarmed so tick re-arms from now.
		m.Log.Warn("digest last-sent is in the future; re-arming", "last_sent", t)
		return time.Time{}
	}
	return t
}

func (m *Manager) setLastSent(ctx context.Context, t time.Time) {
	if err := m.Store.SetSetting(ctx, keyLastSent, strconv.FormatInt(t.Unix(), 10)); err != nil {
		m.Log.Warn("digest persist state", "err", err)
	}
}

// medianBy returns the median of pick(r) over the runs that pick accepts,
// ignoring non-finite values; nil when nothing qualifies, so callers render
// absence rather than a fake 0.
func medianBy(runs []store.SpeedSample, pick func(store.SpeedSample) (float64, bool)) *float64 {
	vals := make([]float64, 0, len(runs))
	for _, r := range runs {
		if v, ok := pick(r); ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return nil
	}
	sort.Float64s(vals)
	n := len(vals)
	med := vals[n/2]
	if n%2 == 0 {
		med = (vals[n/2-1] + vals[n/2]) / 2
	}
	return &med
}

// humanWindow renders a digest's span compactly ("1d", "7d", "3d 4h"). Day-scale
// (windows are always >= 24h), unlike util.HumanDur's second-scale for outages.
// humanWindow renders a span at the two coarsest units it has, never rounding a
// real span away to nothing.
//
// It used to stop at whole hours, which put every span under an hour - and the
// sub-hour remainder of every longer one - into the string "0h". Two of the three
// callers describe how much the machine WATCHED, and one of those is the coverage
// disclosure that tells the operator how far to trust the rest of the digest. A
// machine awake for forty-five minutes reported "observed 0h of 1d", which is the
// string a machine that observed nothing would print, and the digest words that
// case as a separate sentence precisely because the two mean different things.
func humanWindow(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	days := int(d / (24 * time.Hour))
	hours := int(d%(24*time.Hour)) / int(time.Hour)
	mins := int(d%time.Hour) / int(time.Minute)
	secs := int(d%time.Minute) / int(time.Second)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	}
	// Below a minute, seconds - including a genuine zero, which is the one span
	// that is allowed to read as nothing.
	return fmt.Sprintf("%ds", secs)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
