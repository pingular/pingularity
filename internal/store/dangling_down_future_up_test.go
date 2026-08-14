package store

import (
	"context"
	"testing"
	"time"
)

// One outage must come out of a prune with exactly ONE closing event: not zero,
// and not two.
//
// The prune-time repair (resolveDanglingDowns) decides whether an outage still
// needs a synthetic recovery written for it, and it decides by pairing 'down'
// with 'up' off the events table. Get that pairing wrong in either direction and
// the damage is permanent, because the same Prune call deletes the samples that
// were the only other proof.
//
// Too few. Every read that derives uptime ignores an event dated past
// currentHorizon - a row from a machine whose clock ran ahead, or an import from
// one, must not answer for "now". A repair that pairs a 'down' with such a row
// concludes the outage is already closed and writes no synthetic recovery, while
// every reader, ignoring that 'up', still sees the outage open and leans on the
// samples to bound it. Prune deletes those samples in the same call, and when the
// row is past pruneFutureSlack it deletes the row too. The evidence is gone, the
// recovery was never written, and the outage is left open forever: uptime
// collapses toward zero and no later prune or restart can undo it.
//
// Too many. The mirror case, and the reason the pairing stops at the prune's own
// future horizon (now + pruneFutureSlack) rather than at currentHorizon. An 'up'
// between the two - past currentHorizon, so no reader counts it yet, but inside
// pruneFutureSlack, so this prune KEEPS it - is invisible to a repair bounded at
// the horizon. The repair then writes a synthetic recovery for an outage that
// already has a closing event on disk, and once the wall clock reaches that event
// both of them count: one outage is reported as two and its downtime is booked
// twice. Permanent for the same reason - the samples are gone, so nothing
// re-derives the truth, and every later prune sees two complete outages and
// leaves them alone.
//
// The rule that satisfies both is about what SURVIVES this prune, not about what
// the live reads currently show: an 'up' this prune is about to delete cannot
// close anything, so the synthetic is needed; an 'up' this prune keeps closes the
// outage by itself once the clock reaches it, so the synthetic is a duplicate.
func TestPruneLeavesEachOutageOneClosingEvent(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	// One outage of 1000s in a 100000s window, whichever way it gets closed.
	const wantDownS = 1000
	const wantUp = 1 - float64(wantDownS)/100000.0

	// The same history three times: an outage that recovered (proven only by
	// samples), with and without a stray 'up' ahead of the clock. How far ahead is
	// the whole subject - it decides whether this prune deletes that row or keeps
	// it - so it is a parameter here rather than one fixed offset. It must be, in
	// fact: an offset past pruneFutureSlack is the one offset that cannot show the
	// duplicate, so a table that only ever tried ten days reported all-clear while
	// every prune inside the 48-hour window minted a phantom outage.
	for _, tc := range []struct {
		name  string
		upAgo int // eventAt offset for the stray 'up'; negative is the future, 0 means no such row
		// How much wall time separates the prune from the reads below. Only the
		// last arm needs any: see the clockAt call.
		pruneLag time.Duration
	}{
		{"control: no future row", 0, 0},
		// Ten days ahead: past currentHorizon, so no reader counts it, and past
		// pruneFutureSlack, so the prune below deletes it. Nothing but a synthetic
		// recovery can ever close this outage.
		{"a future-dated up is present", -10 * 24 * 3600, 0},
		// 125 seconds ahead of the prune, and so 65 seconds ahead of the reads a
		// minute later: past currentHorizon while the repair runs, inside
		// pruneFutureSlack so the prune keeps it, and inside the readers' horizon by
		// the time they look. This row closes the outage on its own.
		{"a future-dated up survives the prune", -65, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := open(t)
			sampleAt(t, st, now, 100000, "cf", "ipv4", true) // window anchor
			eventAt(t, st, now, 90000, "down", -1)           // the outage
			sampleAt(t, st, now, 89000, "cf", "ipv4", true)  // quorum recovery, 1000s later
			if tc.upAgo != 0 {
				eventAt(t, st, now, tc.upAgo, "up", wantDownS)
			}

			// Before the prune all three histories read the same: the readers either
			// ignore the stray row or agree with it.
			before, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
			if err != nil {
				t.Fatalf("UptimeSince before prune: %v", err)
			}
			approx(t, before, wantUp)

			// Pinning the prune's clock a minute back is how the last arm lets wall
			// time pass over its stray row without the test sleeping through it: Prune
			// reads the clock once, through the pruneClock seam, while every read below
			// runs on the real one. A prune that ran a minute ago and a read taken now
			// is exactly the situation, and sleeping the minute would prove the same
			// thing more slowly.
			if tc.pruneLag > 0 {
				at := now.Add(-tc.pruneLag)
				clockAt(t, st, at, at, 0)
			}
			// One ordinary hourly prune: sample retention passes the recovery.
			if _, err := st.Prune(ctx, now.Add(-time.Hour), now.Add(-9999*time.Hour), now.Add(-9999*time.Hour)); err != nil {
				t.Fatalf("Prune: %v", err)
			}
			st.invalidateReadCaches() // as after a restart: cold recCache

			// The deterministic half of the proof, and the only one that does not have
			// to wait for a clock: how many closing events one outage owns on disk.
			var ups int
			if err := st.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type='up'`).Scan(&ups); err != nil {
				t.Fatalf("count closing events: %v", err)
			}
			switch {
			case ups == 0:
				t.Errorf("the outage has no closing event at all after the prune.\n" +
					"The repair paired it with an event no reader counts, so it wrote no synthetic recovery - " +
					"and the prune then deleted the samples that were the only remaining proof. The outage is " +
					"now open forever and uptime falls toward zero; nothing can reopen the evidence.")
			case ups > 1:
				t.Errorf("the outage has %d closing events after the prune, want 1.\n"+
					"The repair wrote a synthetic recovery for an outage that already had an 'up' this prune "+
					"KEPT, so one outage is closed twice. Once the wall clock reaches the kept row both "+
					"closings count: the outage is reported twice and its downtime booked twice, permanently, "+
					"because the samples that proved otherwise are gone.", ups)
			}

			// And the same thing as the operator sees it, on the surface that prints
			// the outage count and the downtime in one sentence.
			n, downS, err := st.ResolvedOutagesSince(ctx, now.Add(-100000*time.Second).Unix())
			if err != nil {
				t.Fatalf("ResolvedOutagesSince: %v", err)
			}
			if n != 1 || downS != wantDownS {
				t.Errorf("after the prune the history reads %d outage(s) / %ds down, want 1 / %ds.\n"+
					"One real outage of %ds is no longer being reported as one outage of %ds.",
					n, downS, wantDownS, wantDownS, wantDownS)
			}

			after, err := ratioOf(st.UptimeSince(ctx, time.Unix(0, 0), 0))
			if err != nil {
				t.Fatalf("UptimeSince after prune: %v", err)
			}
			if diff := after - wantUp; diff > 0.02 || diff < -0.02 {
				t.Errorf("uptime after prune = %.4f, want ~%.4f. The prune changed a settled history.", after, wantUp)
			}
		})
	}
}
