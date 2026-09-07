package speedtest

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The selection budget can lapse while candidates are still queued for a
// ranking-ping slot: a union Best-of round hands the phase every racer of
// every city, more than racePingParallel of them. A candidate the budget never
// reached must rank exactly as one whose ping FAILED does - Latency 0, behind
// every server that was really measured - and its report row must agree.
// Left alone, its Latency still holds the one echo the list fetch took, and
// the sort read that as a measured ping: a server nobody pinged this run
// ranked first, and was measured, while its own row read unanswered.
func TestRankingBudgetLapseLeavesUnpingedCandidatesUnanswered(t *testing.T) {
	const measured = 20                   // pings that answer before the budget lapses
	n := racePingParallel + measured + 10 // enough to hold every slot, plus ten that never get one
	saturated := make(chan struct{})      // closed once every slot is held past the budget
	var calls int64
	oldPing, oldHealth := ooklaPing, fallbackHealth
	t.Cleanup(func() { ooklaPing, fallbackHealth = oldPing, oldHealth })
	ooklaPing = func(ctx context.Context, s *ookla.Server, cb func(time.Duration)) error {
		k := atomic.AddInt64(&calls, 1)
		if k <= measured {
			cb(20 * time.Millisecond) // a real floor, slower than the stale echo
			return nil
		}
		if k == measured+racePingParallel {
			close(saturated)
		}
		<-ctx.Done() // holds its slot until the budget lapses
		return ctx.Err()
	}
	fallbackHealth = func(context.Context, *ookla.Server) endpointState { return endpointOK }

	servers := make(ookla.Servers, 0, n)
	for i := 0; i < n; i++ {
		servers = append(servers, &ookla.Server{
			ID: fmt.Sprintf("s%03d", i), Sponsor: fmt.Sprintf("Sp%03d", i), Name: "N",
			Distance: float64(i + 1),
			Latency:  3 * time.Millisecond, // the list fetch's one-shot echo
			URL:      fmt.Sprintf("http://s%03d.example:8080/speedtest/upload.php", i),
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		ranked ookla.Servers
		pings  map[string]*float64
	}
	done := make(chan outcome, 1)
	go func() {
		// all=true: a union Best-of round, every server a candidate.
		ranked, pings, _, _ := rankedServersRaced(ctx, servers, "", nil, true, nil, n)
		done <- outcome{ranked, pings}
	}()
	select {
	case <-saturated:
	case <-time.After(10 * time.Second):
		t.Fatal("the ranking never held every slot")
	}
	time.Sleep(50 * time.Millisecond) // let the last ten park on the slot select
	cancel()                          // the budget lapses: slot holders fail, the queued ten never start
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rankedServersRaced did not return")
	}
	if len(got.ranked) != n {
		t.Fatalf("ranked %d servers, want all %d", len(got.ranked), n)
	}

	answered, lastAnswered, firstUnanswered := 0, -1, -1
	var stale []string
	for i, s := range got.ranked {
		if got.pings[s.ID] != nil {
			answered++
			lastAnswered = i
			continue
		}
		if firstUnanswered < 0 {
			firstUnanswered = i
		}
		if s.Latency != 0 {
			stale = append(stale, fmt.Sprintf("%s(%v)", s.ID, s.Latency))
		}
	}
	if answered != measured {
		t.Fatalf("%d servers answered, want the %d that were pinged before the budget lapsed", answered, measured)
	}
	if len(stale) > 0 {
		t.Errorf("%d unanswered servers kept a stale echo as their ranking latency: %v", len(stale), stale)
	}
	if firstUnanswered < lastAnswered {
		t.Errorf("an unanswered server ranks at %d, ahead of a measured one at %d (head: %s Latency=%v, ping answered: %v)",
			firstUnanswered+1, lastAnswered+1, got.ranked[0].ID, got.ranked[0].Latency, got.pings[got.ranked[0].ID] != nil)
	}
	// The report the picker shows must tell the same story as the rank order:
	// every answered row above every unanswered one.
	rows := candidateRows(got.ranked, got.pings)
	seenUnanswered := false
	for _, r := range rows {
		if r.RankPingMS == nil {
			seenUnanswered = true
		} else if seenUnanswered {
			t.Fatalf("report row %d (%s, %.1fms) sits below an unanswered row", r.RankOrder, r.ServerID, *r.RankPingMS)
		}
	}
}
