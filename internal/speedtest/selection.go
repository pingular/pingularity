package speedtest

import (
	"strings"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// SelectionReport records how a run chose its server: every candidate the
// ranking considered, what was measured for each, how each was scored, and why
// the winner won. The Ookla engine builds it; the Scheduler persists it
// (speed_servers), so a surprising winner is explainable from the DB alone at
// any log level - the 2026-08-02 anomalous-winner run was unexplainable
// precisely because all of this lived only in Debug lines and losers were
// discarded outright. Observability only: nothing here feeds back into
// selection, and building the report must never change a winner.
type SelectionReport struct {
	Candidates []CandidateReport
}

// CandidateReport is one server's row in a SelectionReport. Identity and
// ranking fields are snapshotted at SELECTION time - the ranked list holds
// pointers the measurement phase later mutates (measure re-pings every target),
// so reading them after the loop would show measurement-time values.
type CandidateReport struct {
	ServerID   string
	Server     string // "Sponsor, Name" (serverLabel)
	DistanceKM float64
	RankPingMS *float64 // ranking ping: the FLOOR of ~10 samples, the statistic the city race and the winner decision also use; nil = the ranking ping went unanswered
	RankOrder  int      // 1-based position in rankedServers output; 0 = never ranked (a pin resolved outside the list)
	Selected   bool     // became a measurement target
	Measured   bool     // produced a Result
	// Err is the failure text when an attempted measurement failed. The
	// distinction a blank Err carries: Selected && !Measured && Err == ""
	// means the target was never attempted (an earlier target ended the run).
	Err string

	// The rest is meaningful only when Measured.
	DownMbps   float64
	UpMbps     float64
	PingMS     float64
	PingBestMS *float64
	JitterMS   *float64
	// This candidate's OWN traffic. Deliberately not the round total: the
	// winner Result's bytes are overwritten with totalBytes(results) for the
	// data-used accounting, so the speed row and this row answer different
	// questions ("what did the run cost" vs "what did this server move").
	DownloadBytes int64
	UploadBytes   int64

	CapacityMbps         float64 // resultCapacity(res, dir) - raw
	BelievedCapacityMbps float64 // believableCapacity(res, dir, round)
	CappedDirection      string  // direction(s) the implausibility guard held to the middle for THIS row: "down", "up", "down,up", or ""
	Score                float64 // roundScore(res, dir, round) - what bestIndex compared
	Winner               bool
	WinReason            string // score | ping_bootstrap | fastest_ranked | incumbent | on_net | challenger | challenger_won | challenger_failed | pinned | pinned_bestof | pinned_companion
}

// Win reasons. "fastest_ranked", not "only candidate": a want=1 auto run ranks
// 5-12 candidates by ping race and its winner beat all of them - only the
// pinned single-target path truly has one candidate.
const (
	winReasonScore       = "score"          // best-of: highest roundScore (with tie-breaks)
	winReasonPingBoot    = "ping_bootstrap" // best-of with no speed history: lowest ping decided
	winReasonFastestRank = "fastest_ranked" // want=1 auto: head of the ping-ranked list
	// want=1 auto with the head PROMOTED there (see promoteIncumbent): the
	// last auto run's server, still within noise of the fastest, or failing
	// that the ISP's own server within the same band.
	winReasonIncumbent = "incumbent"
	winReasonOnNet     = "on_net"
	// Exported: main's incumbent lookup must skip a challenger that LOST, and
	// its challenge cadence counts runs since either of these.
	WinReasonChallenger    = "challenger"     // scheduled auto: the incumbent's rival was measured; the incumbent kept the seat
	WinReasonChallengerWon = "challenger_won" // ... and the rival beat the incumbent's recent record: it holds the seat now
	// ... and the rival could not be measured, so the incumbent was, as the
	// fallback: the attempt counts toward the cadence, the seat stays.
	WinReasonChallengerFailed = "challenger_failed"
	// Exported: the web browse-centring reads this back out of persisted
	// speed_servers rows to skip pinned runs, and a compile-time tie beats two
	// copies of the literal drifting apart.
	WinReasonPinned = "pinned" // user-pinned single target: no contest ran
	// A pin under best-of is still a pinned run: the companions are the pin's
	// neighbours and the round says nothing about where auto would have gone.
	// Its winner carries one of these instead of score/ping_bootstrap so the
	// browse centring can skip it; which one says whether the pin itself won.
	WinReasonPinnedBestOf    = "pinned_bestof"    // pinned + best-of: the pin won its round
	WinReasonPinnedCompanion = "pinned_companion" // pinned + best-of: a companion beat the pin
)

// ChallengeRun reports whether a persisted win reason names a challenge run,
// won or lost; ChallengeLost the lost ones alone - the runs whose measured
// server must NOT become the incumbent.
func ChallengeRun(reason string) bool {
	return reason == WinReasonChallenger || reason == WinReasonChallengerWon || reason == WinReasonChallengerFailed
}

func ChallengeLost(reason string) bool { return reason == WinReasonChallenger }

// PinnedRun reports whether a persisted win reason names a run with a pin in
// effect, single-target or best-of, so a reader after "where auto last landed"
// can skip it without knowing every literal.
func PinnedRun(reason string) bool {
	return reason == WinReasonPinned || reason == WinReasonPinnedBestOf || reason == WinReasonPinnedCompanion
}

// candidateRows snapshots the ranked list into report rows, in rank order.
// rankPings carries the EXPLICIT ranking outcome per server ID - the Latency
// field cannot be trusted for this (on a failed ranking ping the library leaves
// whatever the list fetch wrote there, often a stale positive echo sample).
func candidateRows(ranked ookla.Servers, rankPings map[string]*float64) []CandidateReport {
	rows := make([]CandidateReport, 0, len(ranked))
	for i, s := range ranked {
		rows = append(rows, CandidateReport{
			ServerID:   s.ID,
			Server:     serverLabel(s),
			DistanceKM: s.Distance,
			RankPingMS: rankPings[s.ID],
			RankOrder:  i + 1,
		})
	}
	return rows
}

// pinnedRow is the report row for a pin resolved outside the ranked list.
func pinnedRow(s *ookla.Server, selected bool) CandidateReport {
	return CandidateReport{
		ServerID:   s.ID,
		Server:     serverLabel(s),
		DistanceKM: s.Distance,
		RankOrder:  0,
		Selected:   selected,
	}
}

// finishSelection fills the measurement and scoring half of the report after
// the winner decision. Pure over its inputs, like the scoring functions it
// calls - recomputing scores here cannot change the round's outcome. results
// still carries per-candidate bytes at every call site: best := results[win]
// copies the struct, so the totalBytes overwrite touches only the returned
// winner Result.
func finishSelection(cands []CandidateReport, results []Result, errByID map[string]string, dir, winnerID, winReason string) *SelectionReport {
	byID := make(map[string]*Result, len(results))
	for i := range results {
		byID[results[i].ServerID] = &results[i]
	}
	badDown, badUp := implausibleDirections(results)
	midDown := middleOf(results, func(r Result) float64 { return r.DownloadMbps })
	midUp := middleOf(results, func(r Result) float64 { return r.UploadMbps })
	for i := range cands {
		c := &cands[i]
		if e, ok := errByID[c.ServerID]; ok {
			c.Err = e
		}
		r, ok := byID[c.ServerID]
		if !ok {
			continue
		}
		c.Measured = true
		c.DownMbps, c.UpMbps, c.PingMS = r.DownloadMbps, r.UploadMbps, r.PingMS
		c.PingBestMS, c.JitterMS = r.PingBestMS, r.JitterMS
		c.DownloadBytes, c.UploadBytes = r.DownloadBytes, r.UploadBytes
		c.CapacityMbps = resultCapacity(*r, dir)
		c.BelievedCapacityMbps = believableCapacity(*r, dir, results)
		// Mirror believableCapacity's Min exactly: a row is capped only when
		// its own reading exceeds the round middle in a rejected direction.
		var capped []string
		if badDown && r.DownloadMbps > midDown {
			capped = append(capped, "down")
		}
		if badUp && r.UploadMbps > midUp {
			capped = append(capped, "up")
		}
		c.CappedDirection = strings.Join(capped, ",")
		c.Score = roundScore(*r, dir, results)
		if c.ServerID == winnerID {
			c.Winner = true
			c.WinReason = winReason
		}
	}
	return &SelectionReport{Candidates: cands}
}

// pingMSOf converts a library latency to milliseconds for the report; the
// ranking goroutines snapshot it immediately after a SUCCESSFUL ping so a
// later mutation of the server object cannot change what the report says.
func pingMSOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
