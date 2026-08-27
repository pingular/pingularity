package speedtest

import (
	"context"
	"math"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// THE SUM MADE UPLOAD ALMOST IRRELEVANT, AND THAT IS WHAT BEST-OF EXISTS TO
// CATCH. Under download+upload, a server that delivered a fifth of the real
// upload gave up a few percent of its score on an asymmetric line and won
// anyway - so the round kept the server best-of was supposed to route around.
// The weighted geometric mean makes each direction's weight relative, so the
// same 20% upload shortfall costs the same share of the score whatever the
// plan's asymmetry.
func TestCapacityWeighsDirectionsRelativelyNotInMbps(t *testing.T) {
	// The headline case: A has a huge upload but three-quarters of B's
	// download and six times its ping. B is the more credible measurement.
	a, b := res("bigup", 30, 150, 30), res("balanced", 45, 45, 5)
	if got := bestResult([]Result{a, b}, "both").Server; got != "balanced" {
		t.Errorf("got %q want balanced; the old sum picked bigup 180 to 90 and let one huge "+
			"upload number swamp a 50%% better download and 6x better latency", got)
	}
	// Mirrored, the same shape must win on download too - the rule is about
	// balance, not about preferring whichever direction is smaller.
	a, b = res("bigdown", 150, 30, 30), res("balanced", 45, 45, 5)
	if got := bestResult([]Result{a, b}, "both").Server; got != "bigdown" {
		t.Errorf("got %q want bigdown", got)
	}
}

// A broken upload cannot hide behind a small download lead. This is the case
// the sum handled worst: 1000+1 beats 900+90 outright.
func TestCapacityDoesNotLetABrokenUploadHideBehindDownload(t *testing.T) {
	a, b := res("brokenup", 1000, 1, 10), res("healthy", 900, 90, 10)
	if got := bestResult([]Result{a, b}, "both").Server; got != "healthy" {
		t.Errorf("got %q want healthy; under the old sum 1001 beat 990 and the round stored a "+
			"server whose upload was effectively dead", got)
	}
}

// The ping discount settles near-ties; it must never become the primary key.
// The candidate pool was already filtered and ranked by ping upstream (see
// autoCandidates and raceCities), so weighting it again here double-counts
// latency the user is already measuring continuously elsewhere.
func TestCapacityLeadStillBeatsMuchLowerPing(t *testing.T) {
	a, b := res("fast-far", 500, 500, 20), res("snappy-slow", 250, 250, 5)
	if got := bestResult([]Result{a, b}, "both").Server; got != "fast-far" {
		t.Errorf("got %q want fast-far; twice the capacity must survive a 15ms deficit", got)
	}
	// ...and at equal capacity the discount decides, which is its whole job.
	a, b = res("far", 500, 500, 20), res("near", 500, 500, 5)
	if got := bestResult([]Result{a, b}, "both").Server; got != "near" {
		t.Errorf("got %q want near (equal capacity, lower ping)", got)
	}
}

// Past the cap the discount tops out, so widely-spread servers compare on
// capacity alone - but the exact ping stays live as the tie-break behind it.
func TestCapacityCapsThePingChargeButKeepsPingAsTheTieBreak(t *testing.T) {
	a, b := res("p30", 500, 500, 30), res("p50", 500, 500, 50)
	if sa, sb := resultScore(a, "both"), resultScore(b, "both"); sa != sb {
		t.Fatalf("scores %v vs %v: both pings are past the cap and must be charged alike", sa, sb)
	}
	if got := bestResult([]Result{a, b}, "both").Server; got != "p30" {
		t.Errorf("got %q want p30 (identical scores, so the ping KEY decides)", got)
	}
}

// Absence must not win. A run that never measured ping is charged the
// uncapped stand-in, so it loses to a comparable measured run.
func TestCapacityPunishesAMissingPingWithoutTheCap(t *testing.T) {
	a, b := res("noping", 500, 500, 0), res("measured", 500, 500, 20)
	if got := bestResult([]Result{a, b}, "both").Server; got != "measured" {
		t.Errorf("got %q want measured; an unmeasured ping must not win by absence", got)
	}
	want := 500 * pingWeightMS / (pingWeightMS + float64(unmeasuredPingMS))
	if got := resultScore(a, "both"); math.Abs(got-want) > 1e-9 {
		t.Errorf("score = %v, want %v - the cap protects measured pings, not missing ones", got, want)
	}
}

// A single-direction round scores on the direction that was ASKED FOR. The
// direction has to be passed in: zero is absorbing in a product, so inferring
// a skipped direction from a zero field would score every candidate in the
// round at 0 and hand the winner to the ping tie-break.
func TestCapacityScoresOnlyTheConfiguredDirection(t *testing.T) {
	// Down-only: the upload figure is not consulted, and a zero one does not
	// zero the score.
	a, b := res("fastdown", 900, 0, 10), res("slowdown", 100, 800, 10)
	if got := bestResult([]Result{a, b}, "down").Server; got != "fastdown" {
		t.Errorf("down-only: got %q want fastdown", got)
	}
	if got := resultCapacity(a, "down"); got != 900 {
		t.Errorf("down-only capacity = %v, want 900 - a skipped upload must not zero the round", got)
	}
	// Up-only: the mirror.
	a, b = res("fastup", 0, 900, 10), res("slowup", 800, 100, 10)
	if got := bestResult([]Result{a, b}, "up").Server; got != "fastup" {
		t.Errorf("up-only: got %q want fastup", got)
	}
	if got := resultCapacity(a, "up"); got != 900 {
		t.Errorf("up-only capacity = %v, want 900", got)
	}
	// The same pair judged bidirectionally goes the other way, which is the
	// proof that the direction is doing the work and not the numbers.
	if got := bestResult([]Result{a, b}, "both").Server; got != "slowup" {
		t.Errorf("both: got %q want slowup", got)
	}
}

// A candidate that failed a required direction needs no special case: it
// scores zero and loses to anything that measured both. No exclusion pass, no
// invented fallback.
func TestCapacityIncompleteCandidateCannotBeatACompleteOne(t *testing.T) {
	a, b := res("halfmeasured", 900, 0, 5), res("complete", 20, 20, 30)
	if got := bestResult([]Result{a, b}, "both").Server; got != "complete" {
		t.Errorf("got %q want complete; a round that never measured upload has not shown it is "+
			"faster than one that did", got)
	}
	// If EVERY candidate is incomplete they tie at zero and the tie-breaks
	// decide - a defined outcome rather than an arbitrary one.
	a, b = res("first", 900, 0, 30), res("second", 100, 0, 5)
	if got := bestResult([]Result{a, b}, "both").Server; got != "second" {
		t.Errorf("all-incomplete: got %q want second (tied at zero, so the ping key decides)", got)
	}
}

// AN UNUSABLE TRANSFER MUST NOT POISON THE COMPARISON. The library reports one
// as -1, and math.Pow with a negative base and a fractional exponent is NaN.
// NaN answers false to every comparison in betterResult, so bestIndex's scan
// would silently freeze on whichever candidate it happened to start with -
// a wrong winner with nothing logged and no error anywhere.
func TestCapacityNeverProducesNaNFromAnUnusableTransfer(t *testing.T) {
	bad := res("na", -1, 500, 10)
	for _, dir := range []string{"both", "down", "up"} {
		if got := resultScore(bad, dir); math.IsNaN(got) {
			t.Fatalf("%s: score is NaN; every comparison against it answers false", dir)
		}
	}
	if got := resultCapacity(bad, "both"); got != 0 {
		t.Errorf("capacity = %v, want 0 for an unusable direction", got)
	}
	// ...and the scan still finds the real winner rather than stopping at the
	// poisoned candidate it starts on.
	good := res("good", 300, 300, 10)
	if got := bestResult([]Result{bad, good}, "both").Server; got != "good" {
		t.Errorf("got %q want good", got)
	}
}

// Nothing separates them, so the FIRST entry survives - the pinned server, or
// the lowest-ping one. Capacity being round-independent is what keeps this
// stable: no candidate's score moves because of who else is in the round.
func TestCapacityExactTieKeepsCandidateOrder(t *testing.T) {
	a, b := res("pinned", 400, 200, 12), res("second", 400, 200, 12)
	if got := bestResult([]Result{a, b}, "both").Server; got != "pinned" {
		t.Errorf("got %q want pinned", got)
	}
	// The scoring is per-candidate, so adding a third result cannot change how
	// the first two compare. Normalising by the round's maxima was rejected for
	// being inert; this is the property that would have made it observable if
	// it were not.
	before := resultScore(a, "both")
	_ = bestResult([]Result{a, b, res("huge", 9000, 9000, 1)}, "both")
	if after := resultScore(a, "both"); after != before {
		t.Errorf("score moved from %v to %v because another candidate joined the round", before, after)
	}
}

// THE BOOTSTRAP ROUND CANNOT RANK ON THROUGHPUT IT CANNOT CHECK. The first
// best-of round on a fresh install seeds the history every later plausibility
// check reads, and the two ways of getting it wrong cost differently: seed it
// low and a genuinely faster line corrects it, because a real gain lifts every
// server; seed it high with a number nothing could vet and the baseline sits
// above the artefact for good, so the mechanism meant to catch that class never
// fires again. So it is decided on the one figure a server cannot inflate.
func TestFirstRunIsDecidedOnPingAlone(t *testing.T) {
	// A round the ordinary scoring settles on throughput, and which the
	// round-local guard has no reason to touch: the far server's upload is only
	// twice the middle, so nothing here is provably inflated. That is the point -
	// with no history, "genuinely twice as fast" and "counting bytes it never
	// delivered" look identical, and this is the one round whose answer becomes
	// the baseline for telling them apart later.
	round := []Result{
		rr("near-honest", 45, 48, 5),
		rr("mid", 44, 47, 34),
		rr("far-fast", 90, 95, 38),
	}
	if _, up := implausibleDirections(round); up {
		t.Fatal("premise broke: the guard should find nothing to reject here")
	}
	if got := round[bestIndex(round, "both")].Server; got != "far-fast" {
		t.Fatalf("premise broke: ordinary scoring should pick far-fast, got %q", got)
	}

	o := NewOokla()
	o.PriorDataFn = func() bool { return false }
	if !o.firstRunByPing(bestOfServers) {
		t.Fatal("a best-of round with no history must be decided on ping")
	}
	// Capacity is IGNORED, not outweighed: the round goes to the nearest server
	// even though another measured twice the throughput. That cost is accepted
	// deliberately - a low seed is corrected by the next genuinely fast round,
	// because a real gain lifts every server, while a high seed puts the baseline
	// above the artefact for good.
	if got := round[lowestPingIndex(round)].Server; got != "near-honest" {
		t.Fatalf("first run picked %q, want near-honest - the bootstrap round ranks on the one "+
			"figure a server cannot inflate", got)
	}
}

// ...and it stops applying the moment there IS history, or the round has
// nothing to choose between.
func TestFirstRunRuleIsNarrow(t *testing.T) {
	o := NewOokla()
	o.PriorDataFn = func() bool { return true }
	if o.firstRunByPing(bestOfServers) {
		t.Error("history exists; the round must be scored normally")
	}
	o.PriorDataFn = func() bool { return false }
	if o.firstRunByPing(1) {
		t.Error("a single-server run has nothing to choose between, and auto-select already picked it by ping")
	}
	// Unwired (nil) keeps the behaviour every caller had before the rule existed.
	o.PriorDataFn = nil
	if o.firstRunByPing(bestOfServers) {
		t.Error("an unwired PriorDataFn must not change how a round is judged")
	}
}

// Absence cannot win it either - the same rule the tie-breaks apply.
func TestLowestPingIgnoresUnmeasuredAndKeepsOrderOnTies(t *testing.T) {
	round := []Result{rr("noping", 900, 900, 0), rr("measured", 10, 10, 30)}
	if got := lowestPingIndex(round); round[got].Server != "measured" {
		t.Errorf("got %q; a run with no ping must not win by having none", round[got].Server)
	}
	tie := []Result{rr("first", 10, 10, 12), rr("second", 900, 900, 12)}
	if got := lowestPingIndex(tie); got != 0 {
		t.Errorf("got index %d, want 0 - an exact tie keeps the earlier candidate", got)
	}
}

// The call site, not the predicate. firstRunByPing and lowestPingIndex can both
// be correct while RunReason ignores them - deleting the call leaves every test
// above passing, because none of them runs a round.
func TestRunReasonDecidesTheBootstrapRoundOnPing(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			return Result{Server: "far-fast", ServerID: "1", DownloadMbps: 90, UploadMbps: 95, PingMS: 38}, nil
		case "2":
			return Result{Server: "mid", ServerID: "2", DownloadMbps: 44, UploadMbps: 47, PingMS: 34}, nil
		}
		return Result{Server: "near-honest", ServerID: "3", DownloadMbps: 45, UploadMbps: 48, PingMS: 5}, nil
	})

	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }

	// No history: the nearest server takes it, though another measured twice the
	// throughput and would win on score.
	o.PriorDataFn = func() bool { return false }
	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("bootstrap run: %v", err)
	}
	if res.Server != "near-honest" {
		t.Errorf("bootstrap round stored %q; an unverifiable throughput reading was allowed to "+
			"seed the baseline", res.Server)
	}

	// History present: scored normally, and the faster server wins.
	o.PriorDataFn = func() bool { return true }
	res, err = o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("normal run: %v", err)
	}
	if res.Server != "far-fast" {
		t.Errorf("with history the round stored %q, want far-fast - the rule must not outlive "+
			"the bootstrap", res.Server)
	}
}
