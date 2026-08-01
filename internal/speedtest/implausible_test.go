package speedtest

import "testing"

// rr builds a Result with the fields the guard and the score read.
func rr(name string, down, up, ping float64) Result {
	return Result{Server: name, DownloadMbps: down, UploadMbps: up, PingMS: ping}
}

// THE ROUND THAT MOTIVATED THIS, VERBATIM. Measured 2026-07-30 on a 50 Mbps-up
// plan: two servers agree the line does ~48, a third reports 151 and claims to
// have moved several times more data than the link can carry in the capture
// window. Scoring cannot fix that - best-of is a MAX-selector, so it can only
// choose the inflated candidate - which is why the guard has to run before the
// comparison rather than inside it.
func TestImplausibleRejectsTheInflatedDirectionFromARealRound(t *testing.T) {
	round := []Result{
		rr("Example ISP, Oldtown", 45.94, 49.26, 6.46),
		rr("Example Networks, Newport", 40.32, 48.03, 34.01),
		rr("Example Telecom, Newport", 45.94, 150.97, 46.87),
	}
	if _, up := implausibleDirections(round); !up {
		t.Fatal("upload not rejected: 151 against a round that agrees on ~48")
	}
	if down, _ := implausibleDirections(round); down {
		t.Error("download rejected too; 45.9/40.3/45.9 is an ordinary spread")
	}
	if got := bestResult(round, "both").Server; got != "Example ISP, Oldtown" {
		t.Fatalf("winner = %q, want Example ISP; the round stored a measurement of buffer absorption, "+
			"not of the connection", got)
	}
	// The other measured round from the same evening, same verdict.
	round = []Result{
		rr("Example ISP, Oldtown", 43.64, 48.93, 5.35),
		rr("Example Networks, Newport", 41.30, 44.22, 34.90),
		rr("Example Telecom, Newport", 43.60, 157.24, 45.57),
	}
	if got := bestResult(round, "both").Server; got != "Example ISP, Oldtown" {
		t.Fatalf("second round: winner = %q, want Example ISP", got)
	}
}

// ...AND THE ROUND BEST-OF EXISTS TO WIN MUST STILL BE WON. Two servers that
// cannot move upload and one that can is the same shape - one reading far above
// the middle - and suppressing it would defeat the feature the guard protects.
// What separates them is whether the OTHERS agree with each other: 48 and 49
// have established a line rate, 10 and 20 have established nothing.
func TestImplausibleBelievesAServerRoutingAroundTwoLimitedOnes(t *testing.T) {
	round := []Result{
		rr("limited-a", 900, 10, 5),
		rr("good", 500, 500, 5),
		rr("limited-b", 950, 20, 5),
	}
	if _, up := implausibleDirections(round); up {
		t.Fatal("rejected a genuine upload: the other two disagree by 2x, so they convict nobody")
	}
	// A moderate spread is likewise left alone - the outlier is only 1.7x the
	// middle, which is a real field, not a contradiction.
	round = []Result{rr("a", 400, 49, 10), rr("b", 400, 90, 10), rr("c", 400, 151, 10)}
	if _, up := implausibleDirections(round); up {
		t.Error("rejected 151 against a middle of 90; that is a spread, not a consensus")
	}
}

// Both halves of the rule are load-bearing, so both are pinned directly.
func TestImplausibleNeedsBothAConsensusAndAnOutlier(t *testing.T) {
	// Tight consensus but the top is inside the factor: believed.
	round := []Result{rr("a", 400, 48, 10), rr("b", 400, 49, 10), rr("c", 400, 95, 10)}
	if _, up := implausibleDirections(round); up {
		t.Error("95 is under 2x the middle of 49; nothing to reject")
	}
	// Tight consensus and the top well past it: rejected.
	round = []Result{rr("a", 400, 48, 10), rr("b", 400, 49, 10), rr("c", 400, 99, 10)}
	if _, up := implausibleDirections(round); !up {
		t.Error("99 is past 2x a consensus of ~48; reject")
	}
}

// A round too small to hold a majority rejects nothing: two readings that
// disagree cannot say which of them is wrong, and a single-server run has
// nothing to compare against at all.
func TestImplausibleNeedsAMajorityToConsult(t *testing.T) {
	for _, round := range [][]Result{
		{rr("solo", 45, 151, 40)},
		{rr("a", 45, 48, 6), rr("b", 45, 151, 40)},
	} {
		if d, u := implausibleDirections(round); d || u {
			t.Errorf("%d result(s): rejected something with no majority to consult", len(round))
		}
		if got := bestResult(round, "both").Server; got != round[len(round)-1].Server && len(round) == 1 {
			t.Errorf("solo round must still return its only result, got %q", got)
		}
	}
}

// A rejected direction is HELD at what the round agrees on, not zeroed. The
// direction was measured; the round simply disagrees about how much. Zeroing
// would eject a server that might be the best in the other direction, which is
// a second wrong answer rather than a correction.
func TestImplausibleHoldsTheDirectionRatherThanDiscardingTheServer(t *testing.T) {
	// Downloads are spread (300/700/900), so they carry no consensus and are
	// left alone; only the upload has a majority to contradict it.
	round := []Result{
		rr("slow", 300, 48, 5),
		rr("mid", 700, 49, 5),
		rr("bigdown-inflated-up", 900, 151, 5),
	}
	if down, up := implausibleDirections(round); down || !up {
		t.Fatalf("down rejected=%v up rejected=%v; want upload only", down, up)
	}
	// Its upload is not believed, but its genuine 900 download still wins.
	if got := bestResult(round, "both").Server; got != "bigdown-inflated-up" {
		t.Fatalf("winner = %q; holding the bad direction must not discard the server", got)
	}
	held := believableCapacity(round[2], "both", round)
	raw := resultCapacity(round[2], "both")
	if held >= raw {
		t.Errorf("capacity %v was not held below the raw %v", held, raw)
	}
	// Held AT the round's middle, not at zero.
	want := resultCapacity(rr("x", 900, 49, 5), "both")
	if held != want {
		t.Errorf("held capacity = %v, want %v (the round's middle upload)", held, want)
	}
}

// The guard runs on the round, so a single-server run is scored exactly as it
// was before - there is no round to consult, and inventing one would mean
// judging a measurement against itself.
func TestImplausibleLeavesSingleServerScoringUntouched(t *testing.T) {
	r := rr("solo", 45, 151, 40)
	if got, want := roundScore(r, "both", nil), resultScore(r, "both"); got != want {
		t.Errorf("solo score = %v, want the unguarded %v", got, want)
	}
}
