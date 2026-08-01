package monitor

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The widen (see transition) stretches a stepped-back recovery's stored pair to
// down + ELAPSED, and f133282 chose that coordinate because an explicit pause
// advances both clocks: the monotonic elapsed already spans the pause's wall
// seconds, so the read model's pause-row subtraction lands on seconds the pair
// holds. A SUSPEND on a frozen-monotonic platform (macOS, Linux) breaks that
// premise from the other side: bookUnobservedGap books a pause row for the WALL
// seconds of the freeze while the monotonic elapsed EXCLUDES them - so a pair
// widened to elapsed alone has the suspend row subtracted from it AGAIN by
// observedOutageSpans, under-booking uptime, the digest and the heatmap by the
// row's overlap. The widen target must be elapsed PLUS the wall seconds this
// outage's suspend rows booked that the monotonic clock slept through.
//
// The invariant, asserted on every surface that shares the one interval model:
// stored pair width minus its pause-row overlap == duration_s == what
// UptimeSince, ResolvedOutagesSince and DowntimeByDay book.

// sumDowntimeByDay books the heatmap's answer over the whole window.
func sumDowntimeByDay(t *testing.T, st *store.Store, since time.Time) int {
	t.Helper()
	days, err := st.DowntimeByDay(context.Background(), since, time.UTC)
	if err != nil {
		t.Fatalf("DowntimeByDay: %v", err)
	}
	total := 0
	for _, d := range days {
		total += d.DowntimeS
	}
	return total
}

// A suspend inside the outage, then a backward wall step squeezing the pair: the
// widened pair must be wide enough that subtracting the suspend's own pause row
// still leaves the full observed length.
func TestWidenAddsBackTheSuspendTheMonotonicClockSleptThrough(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := st.InsertSamples(ctx, []store.Sample{{
		TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	feed(m, false, down) // DOWN at wall `down`
	// Ten minutes in, the lid closes for twenty: the wall-gap check books the
	// freeze exactly as Run does, with the monotonic advance a frozen clock
	// reports - none. transition() therefore never saw these 1200 seconds, so
	// nothing lands in pausedGap (that half is TestUnobservedInOutage...'s).
	if booked, p := m.bookUnobservedGap(ctx, down.Add(600*time.Second), down.Add(1800*time.Second),
		false, m.interval(), 0); !booked || p != nil {
		t.Fatalf("suspend gap not booked cleanly: booked=%v pending=%v", booked, p)
	}
	if m.pausedGap != 0 {
		t.Fatalf("pausedGap = %v, want 0: the frozen clock kept the suspend out of the elapsed, "+
			"so there is nothing to deduct", m.pausedGap)
	}
	// The wall then steps BACK past the down: the recovery's wall second reads
	// down+300 while 900 monotonic (= observed) seconds passed. No synthesized
	// time.Time can carry that decoupling (Add moves both readings), so fabricate
	// the halves separately, exactly as production leaves them: m.since anchors
	// the MONOTONIC measurement, the fed ts carries the wall second.
	m.mu.Lock()
	m.since = down.Add(300*time.Second - 900*time.Second)
	m.mu.Unlock()
	feed(m, true, down.Add(300*time.Second))
	if !m.online {
		t.Fatal("expected UP after recovery")
	}

	var downTS, upTS, dur int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900 (the monotonic elapsed; the frozen suspend is not in it)", dur)
	}
	// The pair itself must hold duration_s once its own suspend row is taken back
	// out: [down, up) minus the 1200s row.
	if got := (upTS - downTS) - 1200; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds; "+
			"observedOutageSpans subtracts the suspend row from the pair a second time", got, dur)
	}
	// And the three surfaces that share the interval model must all book it.
	since := down.Add(-time.Hour)
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := 900 * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v, want %v: the suspend was subtracted from the outage twice", o.Down, want)
	}
	if n, downS, err := st.ResolvedOutagesSince(ctx, since.Unix()); err != nil || n != 1 || int64(downS) != dur {
		t.Errorf("ResolvedOutagesSince = (%d outages, %ds, err=%v), want (1, %ds, nil)", n, downS, err, dur)
	}
	if got := sumDowntimeByDay(t, st, since); int64(got) != dur {
		t.Errorf("DowntimeByDay booked %ds, want %ds", got, dur)
	}
}

// A REFUSED suspend row will never be subtracted by the read model, so its
// mono-absent remainder must not widen the pair either - the row and its two
// corrections are one fact, kept whole in both directions (the same pairing
// revertGapDeduction already enforces for the numerator deduction). A partial
// freeze exercises both halves at once.
func TestARefusedSuspendRowTakesItsWidenBack(t *testing.T) {
	m, _ := newTestMonitor(t, 1, 1)
	old := insertPause
	insertPause = func(*store.Store, context.Context, time.Time, int64) (bool, error) {
		return false, nil // the store deterministically refuses the span
	}
	t.Cleanup(func() { insertPause = old })

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	feed(m, false, down)
	// 1200 wall seconds of gap, of which the monotonic clock saw 400: deduct 400,
	// widen remainder 800 - until the refusal takes both back.
	if booked, p := m.bookUnobservedGap(context.Background(), down.Add(600*time.Second),
		down.Add(1800*time.Second), false, m.interval(), 400*time.Second); !booked || p != nil {
		t.Fatalf("refused gap not settled: booked=%v pending=%v (a refusal is final, not retried)", booked, p)
	}
	m.mu.RLock()
	pausedGap, frozenGap := m.pausedGap, m.frozenGap
	m.mu.RUnlock()
	if pausedGap != 0 {
		t.Errorf("pausedGap = %v after the refusal, want 0: the deduction's row does not exist", pausedGap)
	}
	if frozenGap != 0 {
		t.Errorf("frozenGap = %v after the refusal, want 0: the widen would stretch the pair over "+
			"a row the read model will never subtract, over-booking the outage", frozenGap)
	}
}

// The same invariant with BOTH kinds of unwatched time inside one outage: an
// explicit pause (both clocks advance; its fold is already inside elapsed and
// needs no widen correction) and a frozen suspend (wall-only; it does). The widen
// must add back exactly the suspend's seconds - not the pause's too.
func TestWidenAddsBackOnlyTheMonoAbsentPauseRows(t *testing.T) {
	m, st := newTestMonitor(t, 1, 1)
	ctx := context.Background()

	down := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	if err := st.InsertSamples(ctx, []store.Sample{{
		TS: down.Add(-time.Hour), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 12,
	}}); err != nil {
		t.Fatalf("insert sample: %v", err)
	}

	feed(m, false, down) // DOWN at wall `down`
	// A five-minute explicit pause two minutes in, recorded the way Run does: the
	// row persisted for the read model, the fold accumulated for transition.
	pauseStart := down.Add(120 * time.Second)
	m.notePause(pauseStart)
	if stored, err := st.InsertPause(ctx, pauseStart, 300); err != nil || !stored {
		t.Fatalf("insert pause: stored=%v err=%v", stored, err)
	}
	m.noteResume(pauseStart.Add(300 * time.Second))
	// Then the twenty-minute frozen suspend, as in the test above.
	if booked, p := m.bookUnobservedGap(ctx, down.Add(600*time.Second), down.Add(1800*time.Second),
		false, m.interval(), 0); !booked || p != nil {
		t.Fatalf("suspend gap not booked cleanly: booked=%v pending=%v", booked, p)
	}
	// Backward step + recovery: 1200 monotonic seconds elapsed (900 observed +
	// the 300s pause, which the monotonic clock DID run through), wall at down+300.
	m.mu.Lock()
	m.since = down.Add(300*time.Second - 1200*time.Second)
	m.mu.Unlock()
	feed(m, true, down.Add(300*time.Second))

	var downTS, upTS, dur int64
	if err := st.DB().QueryRow(`SELECT ts FROM events WHERE type='down'`).Scan(&downTS); err != nil {
		t.Fatalf("read down ts: %v", err)
	}
	if err := st.DB().QueryRow(`SELECT ts, duration_s FROM events WHERE type='up'`).Scan(&upTS, &dur); err != nil {
		t.Fatalf("read up event: %v", err)
	}
	if dur != 900 {
		t.Fatalf("outage duration = %d, want 900 (1200s elapsed minus the 300s pause fold)", dur)
	}
	if got := (upTS - downTS) - 300 - 1200; got != dur {
		t.Errorf("stored pair width minus its pause-row overlap = %ds for a duration_s of %ds", got, dur)
	}
	since := down.Add(-time.Hour)
	o, err := st.UptimeSince(ctx, since, 0)
	if err != nil {
		t.Fatalf("UptimeSince: %v", err)
	}
	if want := 900 * time.Second; o.Down != want {
		t.Errorf("UptimeSince booked %v, want %v", o.Down, want)
	}
	if n, downS, err := st.ResolvedOutagesSince(ctx, since.Unix()); err != nil || n != 1 || int64(downS) != dur {
		t.Errorf("ResolvedOutagesSince = (%d outages, %ds, err=%v), want (1, %ds, nil)", n, downS, err, dur)
	}
	if got := sumDowntimeByDay(t, st, since); int64(got) != dur {
		t.Errorf("DowntimeByDay booked %ds, want %ds", got, dur)
	}
}
