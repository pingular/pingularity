package speedtest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// A racer's entry IS its ranking ping: the city race pinged it seconds ago with
// the same probe set and the same statistic, and this phase deliberately does
// not ping it again. That measurement has to survive the selection budget
// lapsing. A union Best-of round hands the phase every racer of every city, far
// more than racePingParallel of them, so racers are exactly the candidates left
// queued for a slot when the ninety seconds run out - and recording one as
// unanswered there discards the only ping the round ever took for it, drops it
// behind every server that answered, and sends the run to one the race clocked
// slower. A racer the race pinged and got nothing back from scores 0, and must
// still rank unanswered - as must a candidate the race never saw at all, which
// is the whole point of the queued-candidate rule.
func TestRankingBudgetLapseKeepsTheRacesOwnMeasurement(t *testing.T) {
	const racers = racePingParallel + 24 // more than the slots, so some must queue
	const others = 10                    // never raced, and never pinged either
	n := racers + others
	saturated := make(chan struct{}) // closed once every slot is held past the budget
	var held int64                   // candidates inside a slot, of either kind
	var mu sync.Mutex
	checked := map[string]bool{} // servers whose fallback the phase got to judge

	hold := func(ctx context.Context) {
		if atomic.AddInt64(&held, 1) == racePingParallel {
			close(saturated)
		}
		<-ctx.Done() // keeps the slot until the budget lapses
	}
	oldPing, oldHealth := ooklaPing, fallbackHealth
	t.Cleanup(func() { ooklaPing, fallbackHealth = oldPing, oldHealth })
	ooklaPing = func(ctx context.Context, s *ookla.Server, cb func(time.Duration)) error {
		hold(ctx)
		return ctx.Err()
	}
	fallbackHealth = func(ctx context.Context, s *ookla.Server) endpointState {
		mu.Lock()
		checked[s.ID] = true
		mu.Unlock()
		hold(ctx)
		return endpointOK
	}

	servers := make(ookla.Servers, 0, n)
	raced := make(map[string]time.Duration, racers)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s%03d", i)
		s := &ookla.Server{
			ID: id, Sponsor: fmt.Sprintf("Sp%03d", i), Name: "N",
			Distance: float64(i + 1),
			Latency:  3 * time.Millisecond, // the list fetch's one-shot echo
			URL:      fmt.Sprintf("http://%s.example:8080/speedtest/upload.php", id),
		}
		if i < racers {
			// racePing REPLACES Latency with the floor of its own ten paced
			// probes before handing the map over - 0 when nothing answered.
			// Every third racer is one of those, spread through the field so
			// whichever candidates end up queued include some of each.
			floor := time.Duration(5+i) * time.Millisecond
			if i%3 == 2 {
				floor = 0
			}
			s.Latency = floor
			raced[id] = floor
		}
		servers = append(servers, s)
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
		ranked, pings, _, _ := rankedServersRaced(ctx, servers, "", raced, true, nil, n)
		done <- outcome{ranked, pings}
	}()
	select {
	case <-saturated:
	case <-time.After(10 * time.Second):
		t.Fatal("the ranking never held every slot")
	}
	time.Sleep(50 * time.Millisecond) // let the rest park on the slot select
	cancel()                          // the budget lapses
	var got outcome
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rankedServersRaced did not return")
	}
	if len(got.ranked) != n {
		t.Fatalf("ranked %d servers, want all %d", len(got.ranked), n)
	}
	byID := make(map[string]*ookla.Server, n)
	for _, s := range got.ranked {
		byID[s.ID] = s
	}

	// The racers the lapse actually caught: the phase never reached their
	// fallback, so they left through the out-of-time arm. Without one of those
	// the test proves nothing.
	mu.Lock()
	queued, queuedSilent := 0, 0
	for id, floor := range raced {
		if checked[id] {
			continue
		}
		queued++
		if floor == 0 {
			queuedSilent++
		}
	}
	mu.Unlock()
	t.Logf("%d of %d racers were still queued for a slot when the budget lapsed, %d of them silent in the race",
		queued, racers, queuedSilent)
	if queued == 0 || queuedSilent == 0 {
		t.Fatal("no racer was caught on the slot select: the lapse this test is about never happened")
	}

	var lost, promoted []string
	for id, floor := range raced {
		s, p := byID[id], got.pings[id]
		if floor == 0 {
			// The race heard nothing from it. It is not a measurement, and a
			// lapse must not turn it into one.
			if p != nil || s.Latency != 0 {
				promoted = append(promoted, fmt.Sprintf("%s(Latency=%v answered=%v)", id, s.Latency, p != nil))
			}
			continue
		}
		if s.Latency != floor || p == nil || *p != pingMSOf(floor) {
			lost = append(lost, fmt.Sprintf("%s(race floor %v -> Latency=%v answered=%v)", id, floor, s.Latency, p != nil))
		}
	}
	if len(lost) > 0 {
		t.Errorf("%d racers lost the measurement the race handed in: %v", len(lost), lost)
	}
	if len(promoted) > 0 {
		t.Errorf("%d racers the race never heard from were recorded as answered: %v", len(promoted), promoted)
	}

	// The rule the queued-candidate arm exists for, unchanged: a candidate
	// nobody pinged carries no latency and no report row.
	var stale []string
	for _, s := range got.ranked {
		if _, wasRaced := raced[s.ID]; wasRaced {
			continue
		}
		if got.pings[s.ID] != nil {
			t.Errorf("%s answered a ranking ping the budget never allowed", s.ID)
		}
		if s.Latency != 0 {
			stale = append(stale, fmt.Sprintf("%s(%v)", s.ID, s.Latency))
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d unpinged candidates kept a stale echo as their ranking latency: %v", len(stale), stale)
	}

	// The race clocked s000 fastest at 5ms. The lapse costs the round its
	// fallback checks, not its ranking: the run must still go to that server.
	if got.ranked[0].ID != "s000" {
		t.Errorf("the ranking is headed by %s (Latency=%v, answered=%v), not s000 - the racer the race clocked fastest at 5ms",
			got.ranked[0].ID, got.ranked[0].Latency, got.pings[got.ranked[0].ID] != nil)
	}
	lastAnswered, firstUnanswered := -1, -1
	for i, s := range got.ranked {
		if got.pings[s.ID] != nil {
			lastAnswered = i
			continue
		}
		if firstUnanswered < 0 {
			firstUnanswered = i
		}
	}
	if firstUnanswered >= 0 && firstUnanswered < lastAnswered {
		t.Errorf("an unanswered server ranks at %d, ahead of a measured one at %d",
			firstUnanswered+1, lastAnswered+1)
	}
	// The picker's report must tell the same story as the rank order.
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
