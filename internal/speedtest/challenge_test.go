package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

func TestChallengeIncumbentPicksTheStrongestRival(t *testing.T) {
	ms := func(id string, m float64) *ookla.Server {
		s := srv(id, 1)
		s.Latency = time.Duration(m * float64(time.Millisecond))
		return s
	}
	cases := []struct {
		name      string
		ranked    ookla.Servers
		incumbent string
		wantLead  string
		wantHead  string
		wantFrom  int
	}{
		{"incumbent leads: the next answered server is the rival", ookla.Servers{ms("I", 8), ms("R", 9), ms("S", 10)}, "I", WinReasonChallenger, "R", 1},
		{"a rival already leads: measured as the rival, list untouched", ookla.Servers{ms("R", 8), ms("I", 9)}, "I", WinReasonChallenger, "R", 0},
		{"no incumbent: nothing to challenge", ookla.Servers{ms("A", 8), ms("B", 9)}, "", "", "A", 0},
		{"incumbent not in the field: the seat is open, ordinary rules", ookla.Servers{ms("A", 8), ms("B", 9)}, "Z", "", "A", 0},
		{"incumbent did not answer: not seated, ordinary rules", ookla.Servers{ms("A", 8), ms("I", 0)}, "I", "", "A", 0},
		{"incumbent outside the band: the seat is already lost, ordinary rules", ookla.Servers{ms("A", 8), ms("I", 40)}, "I", "", "A", 0},
		{"incumbent at the edge of the band: still seated", ookla.Servers{ms("A", 8), ms("I", 10)}, "I", WinReasonChallenger, "A", 0},
		{"the only answered server is the incumbent: no rival to measure", ookla.Servers{ms("I", 8), ms("B", 0)}, "I", "", "I", 0},
		{"one server: nothing to do", ookla.Servers{ms("I", 8)}, "I", "", "I", 0},
	}
	for _, c := range cases {
		out, from, lead := challengeIncumbent(c.ranked, c.incumbent)
		if lead != c.wantLead || out[0].ID != c.wantHead || from != c.wantFrom {
			t.Errorf("%s: head=%s from=%d lead=%q, want head=%s from=%d lead=%q", c.name, out[0].ID, from, lead, c.wantHead, c.wantFrom, c.wantLead)
		}
		if len(out) != len(c.ranked) {
			t.Errorf("%s: %d servers out of %d in", c.name, len(out), len(c.ranked))
		}
	}
}

func TestChallengeWonNeedsARecordAndAMargin(t *testing.T) {
	cases := []struct {
		name    string
		score   float64
		history []float64
		margin  int
		want    bool
		wantMed float64
	}{
		{"beats the median by more than the margin", 120, []float64{100, 90, 110, 95}, 15, true, 97.5},
		{"beats the median but not by the margin", 110, []float64{100, 90, 110, 95}, 15, false, 97.5},
		{"exactly the bar is not a win", 115, []float64{100, 100, 100}, 15, false, 100},
		{"too little history: the seat stays", 500, []float64{100, 100}, 15, false, 0},
		{"no history: the seat stays", 500, nil, 15, false, 0},
		{"zero margin: any edge wins", 101, []float64{100, 100, 100}, 0, true, 100},
		{"a negative margin reads as zero", 101, []float64{100, 100, 100}, -5, true, 100},
		{"one outlier hour of the incumbent does not decide it", 130, []float64{100, 100, 100, 100, 10}, 15, true, 100},
		{"a score of nothing never wins", 0, []float64{1, 1, 1}, 0, false, 0},
	}
	for _, c := range cases {
		won, med := challengeWon(c.score, c.history, c.margin)
		if won != c.want || med != c.wantMed {
			t.Errorf("%s: won=%v med=%v, want %v %v", c.name, won, med, c.want, c.wantMed)
		}
	}
}

// Through RunReason: a scheduled run that is due measures the rival, records
// the verdict, and never touches the incumbent's seat unless the rival earned
// it; a manual run, or one that is not due, keeps the incumbent.
func TestScheduledChallengeRunMeasuresTheRivalAndJudgesIt(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t) // servers 1, 2, 3 at 1, 2, 3 km
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		down := 100.0
		if srv.ID == "1" {
			down = 200 // the rival is genuinely faster to transfer
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: down, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" } // the incumbent pings fastest (8ms); 1 is the rival at 9ms
	due := true
	o.ChallengeFn = func() bool { return due }
	history := []float64{60, 62, 58, 61}
	o.IncumbentScoresFn = func(id, dir string) []float64 {
		if id != "2" {
			t.Errorf("asked for %s's record, want the incumbent's (2)", id)
		}
		if dir != "both" {
			t.Errorf("asked for the %q record, want the run's own direction (both)", dir)
		}
		return history
	}
	o.ChallengeMarginFn = func() int { return 15 }

	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerID != "1" {
		t.Fatalf("measured %s, want the rival 1", res.ServerID)
	}
	win := winnerRow(res)
	if win.WinReason != WinReasonChallengerWon {
		t.Errorf("reason %q, want challenger_won: score %.1f against a ~60 median", win.WinReason, win.Score)
	}
	if !win.Selected || win.RankOrder != 2 {
		t.Errorf("the report keeps ping order (the rival ranked #2) and marks it measured: %+v", win)
	}

	// The same challenge against a stronger record loses, and says so.
	history = []float64{200, 210, 190, 205}
	res, _ = o.RunReason(context.Background(), "scheduled")
	if w := winnerRow(res); res.ServerID != "1" || w.WinReason != WinReasonChallenger {
		t.Errorf("measured %s reason %q, want the rival measured and 'challenger' (lost)", res.ServerID, w.WinReason)
	}

	// A seat with too short a record is not challenged at all: the incumbent
	// is measured, no challenger row is written, and the slot is not spent.
	history = []float64{60, 62}
	res, _ = o.RunReason(context.Background(), "scheduled")
	if w := winnerRow(res); res.ServerID != "2" || w.WinReason != winReasonFastestRank {
		t.Errorf("short record: measured %s reason %q, want the incumbent 2 as fastest_ranked (challenge deferred)", res.ServerID, w.WinReason)
	}
	history = []float64{60, 62, 58, 61}

	// Not due: the incumbent is kept as usual.
	due = false
	res, _ = o.RunReason(context.Background(), "scheduled")
	if w := winnerRow(res); res.ServerID != "2" || w.WinReason != winReasonFastestRank {
		t.Errorf("not due: measured %s reason %q, want the incumbent 2 as fastest_ranked", res.ServerID, w.WinReason)
	}

	// Due but manual: a manual run is the user's question about their line now.
	due = true
	res, _ = o.RunReason(context.Background(), "manual")
	if w := winnerRow(res); res.ServerID != "2" || w.WinReason == WinReasonChallenger {
		t.Errorf("manual: measured %s reason %q, want the incumbent, no challenge", res.ServerID, w.WinReason)
	}
	if ChallengeRun("challenger_won") != true || ChallengeLost("challenger_won") || !ChallengeLost("challenger") {
		t.Error("the exported predicates disagree with the constants")
	}
}

func winnerRow(res Result) CandidateReport {
	if res.Selection == nil {
		return CandidateReport{}
	}
	for _, c := range res.Selection.Candidates {
		if c.Winner {
			return c
		}
	}
	return CandidateReport{}
}

// A rival that cannot be measured must not sink the run or leave the cadence
// stuck: the incumbent rides along as the fallback target, is measured when
// the rival fails, and the row says challenger_failed - which counts as a
// challenge for the cadence but never hands the seat to the rival.
func TestChallengeFallsBackToTheIncumbentWhenTheRivalFails(t *testing.T) {
	requireQuiet(t)
	healthyEndpoints(t)
	stubServerList(t)
	countingPing(t, map[string]time.Duration{"1": 9 * time.Millisecond, "2": 8 * time.Millisecond, "3": 20 * time.Millisecond})
	measured := []string{}
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		measured = append(measured, srv.ID)
		if srv.ID == "1" {
			return Result{}, errors.New("rival: connection reset")
		}
		return Result{Server: "S" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 20, PingMS: 9}, nil
	})
	o := NewOokla()
	o.LossFn = func() bool { return false }
	o.IncumbentFn = func() string { return "2" }
	o.ChallengeFn = func() bool { return true }
	o.IncumbentScoresFn = func(string, string) []float64 { return []float64{60, 62, 58} }
	res, err := o.RunReason(context.Background(), "scheduled")
	if err != nil {
		t.Fatalf("the run must survive a dead rival: %v", err)
	}
	if len(measured) != 2 || measured[0] != "1" || measured[1] != "2" {
		t.Errorf("measured %v, want the rival first, then the incumbent as the fallback", measured)
	}
	if res.ServerID != "2" {
		t.Errorf("recorded %s, want the incumbent 2", res.ServerID)
	}
	w := winnerRow(res)
	if w.WinReason != WinReasonChallengerFailed {
		t.Errorf("reason %q, want challenger_failed", w.WinReason)
	}
	if !ChallengeRun(WinReasonChallengerFailed) || ChallengeLost(WinReasonChallengerFailed) {
		t.Error("a failed challenge counts for the cadence and keeps the incumbent's seat")
	}
	var rival CandidateReport
	for _, c := range res.Selection.Candidates {
		if c.ServerID == "1" {
			rival = c
		}
	}
	if !rival.Selected || rival.Measured || rival.Err == "" {
		t.Errorf("the rival's row must show it was tried and failed: %+v", rival)
	}
	// An ordinary single-server run still measures exactly one server.
	measured = nil
	o.ChallengeFn = func() bool { return false }
	if _, err := o.RunReason(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	if len(measured) != 1 {
		t.Errorf("not a challenge: measured %v, want one server", measured)
	}
}
