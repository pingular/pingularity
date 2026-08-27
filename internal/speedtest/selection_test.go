package speedtest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"

	"github.com/pingular/pingularity/internal/stats"
)

// stubServerListN is stubServerList with a caller-chosen server count, for the
// selection-report tests that need candidates BEYOND the best-of cut (the
// canned three-server list selects everyone at want=3, so "missed the cut" is
// unreachable through it). Same closed-port URL, so the ranking ping race
// fails instantly and rank order stays nearest-first.
func stubServerListN(t *testing.T, n int) {
	t.Helper()
	old := fetchServerList
	fetchServerList = func(_ context.Context, client *ookla.Speedtest) (ookla.Servers, error) {
		out := make(ookla.Servers, 0, n)
		for i := 1; i <= n; i++ {
			out = append(out, &ookla.Server{
				ID: fmt.Sprint(i), URL: "http://127.0.0.1:1/speedtest/upload.php",
				Lat: "52.1", Lon: "4.1", Sponsor: fmt.Sprintf("S%d", i), Name: fmt.Sprintf("N%d", i),
				Distance: float64(i), Context: client,
			})
		}
		return out, nil
	}
	t.Cleanup(func() { fetchServerList = old })
}

func selCandidate(t *testing.T, rep *SelectionReport, id string) *CandidateReport {
	t.Helper()
	if rep == nil {
		t.Fatal("winner Result carries no SelectionReport")
	}
	for i := range rep.Candidates {
		if rep.Candidates[i].ServerID == id {
			return &rep.Candidates[i]
		}
	}
	t.Fatalf("no candidate row for server %s in %+v", id, rep.Candidates)
	return nil
}

// The report must cover every RANKED candidate - the ones that missed the
// best-of cut included (before this feature they were invisible even at
// Debug) - and the winner row must keep its OWN bytes while the returned
// Result carries the round total (pins the totalBytes overwrite ordering).
func TestSelectionReportCoversRankedCandidatesBeyondTheCut(t *testing.T) {
	requireQuiet(t)
	stubServerListN(t, 5)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		// Values within 2x of each other so the implausibility guard stays
		// out of this test (it has its own); s2 wins on honest score.
		switch srv.ID {
		case "1":
			return Result{Server: "s1", ServerID: "1", DownloadMbps: 100, UploadMbps: 100, PingMS: 10,
				DownloadBytes: 1000, UploadBytes: 100}, nil
		case "2":
			return Result{Server: "s2", ServerID: "2", DownloadMbps: 130, UploadMbps: 130, PingMS: 10,
				DownloadBytes: 2000, UploadBytes: 200}, nil
		}
		return Result{Server: "s3", ServerID: "3", DownloadMbps: 90, UploadMbps: 90, PingMS: 10,
			DownloadBytes: 3000, UploadBytes: 300}, nil
	})
	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true }

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	rep := res.Selection
	if rep == nil || len(rep.Candidates) != 5 {
		t.Fatalf("report rows = %v, want one per ranked candidate (5)", rep)
	}
	var winners, selected int
	for _, c := range rep.Candidates {
		if c.Winner {
			winners++
		}
		if c.Selected {
			selected++
		}
	}
	if winners != 1 {
		t.Errorf("winner rows = %d, want exactly 1", winners)
	}
	if selected != 3 {
		t.Errorf("selected rows = %d, want the top-3 targets", selected)
	}
	for _, id := range []string{"4", "5"} {
		c := selCandidate(t, rep, id)
		if c.Selected || c.Measured || c.RankOrder == 0 {
			t.Errorf("candidate %s: ranked loser must be unselected/unmeasured with a rank, got %+v", id, c)
		}
	}
	win := selCandidate(t, rep, res.ServerID)
	if !win.Winner || win.WinReason != "score" {
		t.Errorf("winner row = %+v, want Winner with reason score", win)
	}
	if win.DownloadBytes != 2000 || win.UploadBytes != 200 {
		t.Errorf("winner row bytes = %d/%d, want its OWN 2000/200 - the round-total "+
			"overwrite must not reach the report", win.DownloadBytes, win.UploadBytes)
	}
	if res.DownloadBytes != 6000 || res.UploadBytes != 600 {
		t.Errorf("returned Result bytes = %d/%d, want the round totals 6000/600", res.DownloadBytes, res.UploadBytes)
	}
	// Ranked but never pinged (the stub's port is closed): the explicit outcome
	// must be nil, whatever the Latency field was left holding.
	for _, c := range rep.Candidates {
		if c.RankPingMS != nil {
			t.Errorf("candidate %s: RankPingMS = %v, want nil for an unanswered ranking ping", c.ServerID, *c.RankPingMS)
		}
	}
}

// A guard round: the report must show raw vs believed capacity and name the
// capped direction on the offending row - and ONLY there - while the winner
// stays exactly what bestIndex chose (observability must not move the choice).
// The guard's log line must reach the WARN ring, not just Debug.
func TestSelectionReportRecordsTheImplausibleDirectionGuard(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1": // the buffer-absorbing server: upload it can't deliver
			return Result{Server: "s1", ServerID: "1", DownloadMbps: 41, UploadMbps: 1100, PingMS: 10}, nil
		case "2":
			return Result{Server: "s2", ServerID: "2", DownloadMbps: 495, UploadMbps: 470, PingMS: 10}, nil
		}
		return Result{Server: "s3", ServerID: "3", DownloadMbps: 500, UploadMbps: 480, PingMS: 10}, nil
	})
	var buf bytes.Buffer
	o := NewOokla()
	o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true }

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ServerID == "1" {
		t.Fatalf("the capped candidate won - the guard's behavior changed")
	}
	capped := selCandidate(t, res.Selection, "1")
	if capped.CappedDirection != "up" {
		t.Errorf("capped row direction = %q, want up", capped.CappedDirection)
	}
	if capped.BelievedCapacityMbps >= capped.CapacityMbps {
		t.Errorf("capped row believed=%v >= raw=%v; the guard's effect is invisible",
			capped.BelievedCapacityMbps, capped.CapacityMbps)
	}
	for _, id := range []string{"2", "3"} {
		if c := selCandidate(t, res.Selection, id); c.CappedDirection != "" || c.BelievedCapacityMbps != c.CapacityMbps {
			t.Errorf("row %s marked capped (%q) though the guard only rejected server 1's upload", id, c.CappedDirection)
		}
	}
	// The handler above is WARN-threshold - the default ring's level - so this
	// only passes if the promotion actually happened.
	if !strings.Contains(buf.String(), "best-of result not believed") {
		t.Errorf("guard fired but nothing reached the Warn ring:\n%s", buf.String())
	}
}

// A bootstrap round (no speed history) is decided on ping; the report must say
// so, or the DB would claim the highest score won a round it lost.
func TestSelectionReportNamesThePingBootstrapRule(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			return Result{Server: "far-fast", ServerID: "1", DownloadMbps: 900, UploadMbps: 900, PingMS: 38}, nil
		case "2":
			return Result{Server: "mid", ServerID: "2", DownloadMbps: 44, UploadMbps: 47, PingMS: 34}, nil
		}
		return Result{Server: "near", ServerID: "3", DownloadMbps: 45, UploadMbps: 48, PingMS: 5}, nil
	})
	o := NewOokla()
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return false }

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	win := selCandidate(t, res.Selection, res.ServerID)
	if win.ServerID != "3" || win.WinReason != "ping_bootstrap" {
		t.Errorf("winner %s reason %q, want server 3 by ping_bootstrap", win.ServerID, win.WinReason)
	}
}

// A candidate that fails mid-round must leave a trace everywhere it was
// silent before: its error in the report row, a Warn line, and the new
// counter - while the run still succeeds off the survivors.
func TestSelectionReportKeepsAFailedCandidatesError(t *testing.T) {
	requireQuiet(t)
	stats.ResetForTest()
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			return Result{Server: "s1", ServerID: "1", DownloadMbps: 400, UploadMbps: 400, PingMS: 10}, nil
		case "2":
			return Result{}, fmt.Errorf("download: connection refused by test")
		}
		return Result{Server: "s3", ServerID: "3", DownloadMbps: 300, UploadMbps: 300, PingMS: 12}, nil
	})
	var buf bytes.Buffer
	o := NewOokla()
	o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true }

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run must survive one failed candidate: %v", err)
	}
	failed := selCandidate(t, res.Selection, "2")
	if !failed.Selected || failed.Measured || !strings.Contains(failed.Err, "connection refused by test") {
		t.Errorf("failed candidate row = %+v, want selected+unmeasured with the error text", failed)
	}
	if got := stats.Lifetime().Counters["speed.bestof_candidate_failed"]; got != 1 {
		t.Errorf("speed.bestof_candidate_failed = %d, want 1", got)
	}
	if !strings.Contains(buf.String(), "speedtest server failed, trying the next") {
		t.Errorf("candidate failure never reached the Warn ring:\n%s", buf.String())
	}
}

// want=1 auto is still a contest - the winner beat every ranked candidate on
// ping - so the report keeps the whole ranked field and says fastest_ranked,
// not "only candidate". A pinned run IS a single candidate: one unranked row,
// reason pinned.
func TestSelectionReportDistinguishesFastestRankedFromPinned(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "s" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 100, PingMS: 10}, nil
	})
	o := NewOokla()
	o.BestOfCountFn = func() int { return 1 } // want=1

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("auto want=1 run: %v", err)
	}
	if len(res.Selection.Candidates) != 3 {
		t.Fatalf("auto want=1 report rows = %d, want all 3 ranked candidates", len(res.Selection.Candidates))
	}
	win := selCandidate(t, res.Selection, res.ServerID)
	if win.WinReason != "fastest_ranked" {
		t.Errorf("auto want=1 win reason = %q, want fastest_ranked (it beat the other ranked candidates)", win.WinReason)
	}

	o.ServerIDFn = func() string { return "2" }
	res, err = o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("pinned run: %v", err)
	}
	if len(res.Selection.Candidates) != 1 {
		t.Fatalf("pinned report rows = %d, want the single pin", len(res.Selection.Candidates))
	}
	pin := res.Selection.Candidates[0]
	if pin.ServerID != "2" || pin.WinReason != "pinned" || pin.RankOrder != 0 {
		t.Errorf("pinned row = %+v, want server 2, reason pinned, rank 0 (no race ran)", pin)
	}
}

// The ranking phase logs one Debug line per candidate under the `server` key
// (per-candidate, not joined, so the PII mask applies) - it logged nothing at
// all before, which is how candidates could vanish without trace.
func TestRankingPhaseLogsEveryCandidate(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		return Result{Server: "s" + srv.ID, ServerID: srv.ID, DownloadMbps: 100, UploadMbps: 100, PingMS: 10}, nil
	})
	var buf bytes.Buffer
	o := NewOokla()
	// The repo's other captures use the TextHandler default (Info) and would
	// count zero Debug lines while passing - the explicit level is the test.
	o.Log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true }

	if _, err := o.RunReason(context.Background(), "manual"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.Count(buf.String(), "speedtest candidate ranked"); got != 3 {
		t.Errorf("candidate-ranked Debug lines = %d, want one per ranked candidate (3):\n%s", got, buf.String())
	}
}

// Pins that a candidate that moved real bytes before failing still lands in
// the run's data-used total - it used to be dropped entirely (measure returned
// an empty Result on a transfer failure, and totalBytes sums only successes),
// so "Data used" understated the real bill by a whole candidate's download on
// a possibly-metered link. Contrast TestSelectionReportCoversRankedCandidates-
// BeyondTheCut, which pins the all-success total.
func TestRunReasonCountsFailedCandidatesBytesInDataUsed(t *testing.T) {
	requireQuiet(t)
	stubServerList(t)
	stubMeasure(t, func(_ *Ookla, _ context.Context, srv *ookla.Server, _ string, _ int) (Result, error) {
		switch srv.ID {
		case "1":
			return Result{Server: "s1", ServerID: "1", DownloadMbps: 400, UploadMbps: 400, PingMS: 10,
				DownloadBytes: 2000, UploadBytes: 200}, nil
		case "2": // downloaded 5000 then failed its upload - the bytes were spent
			return Result{DownloadBytes: 5000, UploadBytes: 0}, fmt.Errorf("upload: connection refused by test")
		}
		return Result{Server: "s3", ServerID: "3", DownloadMbps: 300, UploadMbps: 300, PingMS: 10,
			DownloadBytes: 3000, UploadBytes: 300}, nil
	})
	o := NewOokla()
	o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	o.BestOfCountFn = func() int { return 3 }
	o.PriorDataFn = func() bool { return true }

	res, err := o.RunReason(context.Background(), "manual")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.DownloadBytes != 2000+5000+3000 {
		t.Errorf("DownloadBytes = %d, want 10000 (must include the failed candidate's 5000)", res.DownloadBytes)
	}
	if res.UploadBytes != 200+0+300 {
		t.Errorf("UploadBytes = %d, want 500", res.UploadBytes)
	}
}
