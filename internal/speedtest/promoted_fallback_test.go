package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A promoted incumbent that cannot be measured must not sink the run - and,
// because a failed run writes no winner row, must not freeze the seat: with no
// fallback, every following scheduled run re-promotes the same dead server
// (newIncumbentFn still names it) and measures it alone, forever, while a
// healthy neighbour sits one rank away. The fastest ranked candidate rides
// along as the fallback target - the same mechanics as the challenge rival's
// incumbent fallback - and its win says fastest_ranked: what the winner IS,
// not what the head was promoted for, so the next run's incumbent lookup
// adopts the server that actually measured and the loop breaks itself.
func TestPromotedIncumbentThatFailsFallsBackToTheFastestRanked(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	// 1 is the fastest; the incumbent 2 pings inside the promotion band.
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		if srv.ID == "2" {
			// Answers pings, serves latency.txt, cannot move bytes - the class
			// no conviction covers.
			return Result{}, errors.New("incumbent: transfer stalled")
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 8}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatalf("the run must survive a promoted head that cannot measure: %v", err)
	}
	if len(measured) != 2 || measured[0] != "2" || measured[1] != "1" {
		t.Errorf("measured %v, want the promoted incumbent first, then the fastest ranked as the fallback", measured)
	}
	if res.ServerID != "1" {
		t.Errorf("recorded %s, want the fallback 1", res.ServerID)
	}
	w := winnerRow(res)
	if w.WinReason != winReasonFastestRank {
		t.Errorf("reason %q, want fastest_ranked - an \"incumbent\" tag on a different server would teach the next run's lookup a seat nobody measured", w.WinReason)
	}
	if !w.Selected {
		t.Error("the fallback winner must be marked Selected in the report")
	}
}

// The control: a promoted incumbent that measures fine keeps its seat and its
// honest reason - the fallback exists in the target list but is never spent.
func TestPromotedIncumbentThatMeasuresKeepsItsReason(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(measured) != 1 || measured[0] != "2" {
		t.Errorf("measured %v, want only the promoted incumbent", measured)
	}
	if w := winnerRow(res); res.ServerID != "2" || w.WinReason != winReasonIncumbent {
		t.Errorf("measured %s reason %q, want the incumbent 2 as incumbent", res.ServerID, w.WinReason)
	}
}

// The fallback's presence must not cut the promoted head down at all: the head
// is the run's PRIMARY and keeps the full single-server budget it had before
// the fallback existed (squeezing it killed a slow-but-working incumbent
// inside the retry envelope ooklaRunTimeout is sized for, and migrated the
// seat on a transient). The fallback is paid for by fallbackBudget on top. The
// challenge RIVAL is the opposite case - the run's gamble - so it still gets
// exactly one best-of-sized slice and the incumbent behind it keeps the rest.
func TestFallbackRidesOnItsOwnBudgetWithoutSqueezingTheHead(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	remaining := map[string]time.Duration{}
	stubMeasure(t, func(_ *Ookla, ctx context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if dl, ok := ctx.Deadline(); ok {
			remaining[srv.ID] = time.Until(dl)
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})

	// Promoted head: the whole single-server budget, undiminished.
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	full := ooklaRunTimeout(speedDefaultRetries)
	if got := remaining["2"]; got > full || got < full-15*time.Second {
		t.Errorf("promoted head's slice = %v; want the full single-server budget %v", got.Round(time.Second), full)
	}

	// Challenge rival: still capped at exactly one best-of slice.
	remaining = map[string]time.Duration{}
	o = NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	o.IncumbentScoresFn = func(string, string) []float64 { return []float64{60, 62, 58} }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if got := remaining["1"]; got > bestOfServerTimeout || got < bestOfServerTimeout-15*time.Second {
		t.Errorf("challenge rival's slice = %v; want the bare %v best-of slice", got.Round(time.Second), bestOfServerTimeout)
	}
}

// The insurance has to be REAL, not nominal. The run's ctx starts before the
// list fetch and the ping race, so a slice measured from the head's own start
// re-spends that prologue and leaves the fallback the reserve MINUS it - which
// a slow selection (the 90s the selection budget allows) can drive to nothing,
// skipping the fallback entirely and failing the run. Measured off the real
// deadlines rather than recomputed: an arithmetic assertion here still passed
// with the allowance deleted from RunReason.
func TestTheFallbacksReserveSurvivesASlowPrologue(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})

	runDeadline := slowServerList(t, prologue)

	headDeadline := map[string]time.Time{}
	stubMeasure(t, func(_ *Ookla, ctx context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		headDeadline[srv.ID], _ = ctx.Deadline()
		if srv.ID == "2" {
			return Result{}, errors.New("head: transfer stalled") // so the fallback is reached
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" } // 2 is promoted; 1 rides as the fallback
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	dl, ok := headDeadline["2"]
	if !ok || runDeadline.IsZero() {
		t.Fatalf("did not observe both deadlines: head %v, run %v", headDeadline, *runDeadline)
	}
	// The fallback's own window, measured when it is actually reached. The head
	// here fails immediately, so this is the whole remaining budget rather than
	// the bare reserve - it pins that the fallback is reached and funded, not
	// the tightness of the reserve, which the head assertion above is for.
	if fb, ok := headDeadline["1"]; ok {
		if got := time.Until(fb); got < fallbackBudget {
			t.Errorf("the fallback was measured with %v, want at least the %v reserve",
				got.Round(time.Second), fallbackBudget)
		}
	} else {
		t.Error("the fallback was never measured, so its window was never observed")
	}
	// Whatever the head does with its slice, this much of the run is left for
	// the fallback after it.
	// A second of slack: the clamp lands ON the reserve, so only the
	// nanoseconds between reading the deadline and setting the timeout are
	// lost - the failure this guards against costs the whole prologue.
	if left := runDeadline.Sub(dl); left < fallbackBudget-time.Second {
		t.Errorf("a head that spends its whole slice leaves the fallback %v, want ~%v "+
			"(the %v prologue is being taken out of the fallback's reserve)",
			left.Round(time.Second), fallbackBudget, prologue)
	}
}

// The run's prologue - the list fetch and the ping race - is paid out of the
// run's own ctx before any server is measured, so it is what separates a slice
// measured from the head's start from one measured against the run deadline.
// Long enough to see, short enough to run.
const prologue = 2 * time.Second

// slowServerList stands in for a prologue that takes real time, and reports
// the run's own deadline (read off the fetch's ctx, which is the run ctx).
func slowServerList(t *testing.T, d time.Duration) *time.Time {
	t.Helper()
	var runDeadline time.Time
	old := fetchServerList
	fetchServerList = func(ctx context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		runDeadline, _ = ctx.Deadline()
		time.Sleep(d)
		mk := func(id string, dist float64) *ookla.Server {
			return &ookla.Server{ID: id, URL: "http://127.0.0.1:1/speedtest/upload.php",
				Lat: "52.1", Lon: "4.1", Sponsor: "S" + id, Name: "N" + id, Distance: dist, Context: client}
		}
		return ookla.Servers{mk("1", 1), mk("2", 2), mk("3", 3)}, nil
	}
	t.Cleanup(func() { fetchServerList = old })
	return &runDeadline
}

// And a run that seats NO fallback is bounded where it always was: the head
// cannot spend a reserve only a fallback may spend, so the allowance does not
// quietly loosen every auto run's deadline.
func TestNoFallbackMeansNoExtraWindowForTheHead(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	slowServerList(t, prologue)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	var headDeadline time.Time
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, ctx context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		headDeadline, _ = ctx.Deadline()
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	start := time.Now()
	o := NewOokla() // no incumbent, no challenge: the fastest is simply measured
	o.LossFn = func() bool { return false }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 1 {
		t.Fatalf("measured %v, want the one head", measured)
	}
	if got, base := headDeadline.Sub(start), ooklaRunTimeout(speedDefaultRetries); got > base+time.Second {
		t.Errorf("head runs until %v out, want no more than the base budget %v: the fallback allowance is loosening a run that never seats one",
			got.Round(time.Second), base)
	}
}

// A PINNED run never seats a fallback and never gets the allowance, so nothing
// may be deducted from its budget either - and a caller whose own deadline is
// nearer than the reserve must not have a reserve subtracted from it at all:
// that yields a dead context, a head that fails without being dialled, and a
// seat migrating off an incumbent nobody contacted.
func TestTheReserveIsNeverTakenFromABudgetThatNeverCarriedIt(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	var deadline time.Time
	var expired bool
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, ctx context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		deadline, _ = ctx.Deadline()
		expired = ctx.Err() != nil
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})

	// Pinned: the whole single-server budget, undiminished.
	start := time.Now()
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.ServerIDFn = func() string { return "2" }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	base := ooklaRunTimeout(speedDefaultRetries)
	if got := deadline.Sub(start); got < base-2*time.Second {
		t.Errorf("pinned head's window is %v, want the full %v: a reserve is being taken from a run that never carried one",
			got.Round(time.Second), base)
	}

	// A caller deadline nearer than the reserve: the head still gets it all.
	measured, expired = nil, false
	short, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	start = time.Now()
	o = NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	if _, err := o.RunReason(short, "scheduled"); err != nil {
		t.Fatal(err)
	}
	if expired {
		t.Error("the head was handed an already-expired context: the reserve was deducted from a caller deadline that never included it")
	}
	if got := deadline.Sub(start); got < 55*time.Second {
		t.Errorf("head's window under a 60s caller deadline is %v, want ~60s", got.Round(time.Second))
	}
	if len(measured) != 1 || measured[0] != "2" {
		t.Errorf("measured %v, want only the promoted head - it succeeded, so no fallback should run", measured)
	}
}

// The fallback rides behind an ON-NET promotion too, not only an incumbent
// one: the two reasons are one gate, and a test covering half of it lets the
// other half be deleted silently.
func TestTheOnNetPromotionAlsoCarriesAFallback(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	// Sponsors the ISP matcher can actually match: it ignores words under three
	// characters, so the shared stub's "S1"/"S2" never match anything.
	old := fetchServerList
	fetchServerList = func(_ context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		mk := func(id, sponsor string, dist float64) *ookla.Server {
			return &ookla.Server{ID: id, URL: "http://127.0.0.1:1/speedtest/upload.php",
				Lat: "52.1", Lon: "4.1", Sponsor: sponsor, Name: "N" + id, Distance: dist, Context: client}
		}
		return ookla.Servers{mk("1", "Cogeco", 1), mk("2", "EBOX", 2), mk("3", "Bell", 3)}, nil
	}
	t.Cleanup(func() { fetchServerList = old })
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		if srv.ID == "2" {
			return Result{}, errors.New("on-net box: transfer stalled")
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	// No incumbent; server 2 is the ISP's own box, inside the band behind the
	// fastest, so it is promoted for on_net rather than incumbency.
	o.ISPFn = func() string { return "AS1403 EBOX" }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatalf("an on-net head that cannot measure must not sink the run: %v", err)
	}
	if len(measured) != 2 || measured[0] != "2" || measured[1] != "1" {
		t.Errorf("measured %v, want the on-net head first, then the fastest ranked as its fallback", measured)
	}
	if w := winnerRow(res); res.ServerID != "1" || w.WinReason != winReasonFastestRank {
		t.Errorf("recorded %s reason %q, want the fallback 1 as fastest_ranked", res.ServerID, w.WinReason)
	}
}

// speed.bestof_candidate_failed answers one question - is a dead nearby server
// degrading rounds that otherwise succeed - so it counts a failed candidate of
// a REAL round only. A want=1 run's failed head is the run failing (the
// scheduler's speed.fail.* has it, and promoted_failed/challenge_failed say
// why), and a round that could only find one candidate is the same event. Both
// conjuncts are load-bearing; neither was pinned.
func TestTheCandidateCounterCountsRoundsWithAFieldOnly(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 8 * time.Millisecond, "2": 9 * time.Millisecond, "3": 20 * time.Millisecond})
	// A want=1 run carrying a fallback: the head fails, the fallback answers.
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		if srv.ID == "2" {
			return Result{}, errors.New("head: transfer stalled")
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	before := stats.Lifetime().Counters["speed.bestof_candidate_failed"]
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if got := stats.Lifetime().Counters["speed.bestof_candidate_failed"] - before; got != 0 {
		t.Errorf("a want=1 run's failed head moved the round counter by %d, want 0: it is not a round", got)
	}
	if got := stats.Lifetime().Counters["speed.promoted_failed"]; got == 0 {
		t.Error("the promoted head's own counter did not move, so nothing recorded why the seat changed")
	}
}
