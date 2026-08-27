package speedtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// rankPingLatency is the gate that fixes the zero-ping mis-rank: a successful
// ping whose floor rounds to 0 on a coarse clock (Windows sub-ms) must count as
// answered and clamp to the smallest positive duration, so the fastest server
// ranks first instead of dropping to "unanswered" below every slower one. A
// failed or unsampled ping must NOT be answered - its Latency field holds a
// stale list-fetch echo that must stay out of the ranking.
func TestRankPingLatency(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		sampled bool
		in      time.Duration
		wantLat time.Duration
		wantAns bool
	}{
		{"success, real latency", nil, true, 12 * time.Millisecond, 12 * time.Millisecond, true},
		{"success, zero floor (coarse clock)", nil, true, 0, time.Nanosecond, true},
		{"failed ping keeps stale echo out", errors.New("x"), true, 5 * time.Millisecond, 0, false},
		{"nil error but no sample collected", nil, false, 9 * time.Millisecond, 0, false},
		{"failed ping with zero latency", errors.New("x"), false, 0, 0, false},
	}
	for _, c := range cases {
		gotLat, gotAns := rankPingLatency(c.err, c.sampled, c.in)
		if gotLat != c.wantLat || gotAns != c.wantAns {
			t.Errorf("%s: rankPingLatency(%v,%v,%v) = (%v,%v), want (%v,%v)",
				c.name, c.err, c.sampled, c.in, gotLat, gotAns, c.wantLat, c.wantAns)
		}
	}
}

// applyRankPing must ZERO a failed server's Latency (not leave the stale
// discovery echo), and rankLess must then rank a reachable server ahead of it -
// the end-to-end fix for "an unreachable candidate with a stale ~1ms ranks first
// and fails the whole speedtest".
func TestApplyRankPingAndRanking(t *testing.T) {
	// A failed ranking ping zeros the stale field and reports no measurement.
	failed := &ookla.Server{ID: "stale", Latency: 1 * time.Millisecond} // stale discovery echo
	if ms := applyRankPing(failed, errors.New("unreachable"), false, 0); ms != nil {
		t.Errorf("failed ping reported a measurement %v, want nil", *ms)
	}
	if failed.Latency != 0 {
		t.Errorf("failed ping left Latency=%v, want 0 (stale value must not rank)", failed.Latency)
	}

	// A successful ping records the measured value (positive, so it sorts first).
	ok := &ookla.Server{ID: "good", Latency: 1 * time.Millisecond}
	ms := applyRankPing(ok, nil, true, 1*time.Millisecond)
	if ms == nil || ok.Latency <= 0 {
		t.Fatalf("successful ping: ms=%v Latency=%v, want a measurement and Latency>0", ms, ok.Latency)
	}

	// The reachable server (even if slower) must rank ahead of the unreachable one.
	slow := &ookla.Server{ID: "reachable", Latency: 11486 * time.Microsecond} // 11.486ms measured
	if !rankLess(slow, failed) {
		t.Error("a reachable server (11.486ms) must rank ahead of an unreachable one (0), it did not")
	}
	if rankLess(failed, slow) {
		t.Error("an unreachable server must never rank ahead of a reachable one")
	}
}

// rankedServers (the call site) must not let a candidate whose ranking ping
// FAILED win on its stale discovery latency. Drives the site through the ooklaPing
// seam: a reachable server measured at 11ms vs an unreachable one holding a stale
// 1ms. The fix ranks the reachable one first; reverting applyRankPing's zeroing
// lets the stale 1ms win (test fails) - the call-site coverage the helper-only
// tests were missing.
func TestRankedServersDropsUnreachableStaleLatency(t *testing.T) {
	orig := ooklaPing
	defer func() { ooklaPing = orig }()
	ooklaPing = func(ctx context.Context, srv *ookla.Server, cb func(time.Duration)) error {
		if srv.ID == "reachable" {
			srv.Latency = 11 * time.Millisecond // the library sets the measured mean on success
			cb(srv.Latency)
			return nil
		}
		return errors.New("connection refused") // no sample -> failed ranking ping
	}
	servers := ookla.Servers{
		{ID: "unreachable", Sponsor: "A", Name: "near", Distance: 0, Latency: 1 * time.Millisecond}, // stale, low
		{ID: "reachable", Sponsor: "B", Name: "far", Distance: 1, Latency: 0},
	}
	out, _, _, _ := rankedServers(context.Background(), servers, "")
	if len(out) == 0 || out[0].ID != "reachable" {
		var ids []string
		for _, s := range out {
			ids = append(ids, fmt.Sprintf("%s(%v)", s.ID, s.Latency))
		}
		t.Fatalf("reachable must rank first; a failed ping's stale 1ms must not win. order=%v", ids)
	}
}

// ---- Lazy reserves ----------------------------------------------------------
//
// rankedServers keeps a reserve list past the candidate cap so fallback
// exclusions can be replenished - but the reserves must cost third-party
// traffic ONLY when the pool actually comes up short. Pinging and probing them
// unconditionally roughly doubled every selection pass's fan-out for nothing.

// rankContacts records which server IDs the ranking phase actually contacted,
// on either channel (the ranking ping or the fallback health probe).
type rankContacts struct {
	mu     sync.Mutex
	pinged map[string]bool
	probed map[string]bool
}

func (c *rankContacts) contacted(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pinged[id] || c.probed[id]
}

func (c *rankContacts) contactedIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	set := map[string]bool{}
	for id := range c.pinged {
		set[id] = true
	}
	for id := range c.probed {
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// stubRankContacts swaps the ranking ping and the health oracle for recording
// fakes: every pinged server answers (latency tracks distance, so the ranked
// order is deterministic), and health comes from the given table (default OK).
func stubRankContacts(t *testing.T, health map[string]endpointState) *rankContacts {
	t.Helper()
	rc := &rankContacts{pinged: map[string]bool{}, probed: map[string]bool{}}
	oldPing, oldHealth := ooklaPing, fallbackHealth
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		rc.mu.Lock()
		rc.pinged[s.ID] = true
		rc.mu.Unlock()
		lat := time.Duration(s.Distance) * time.Millisecond
		s.Latency = lat
		cb(lat)
		return nil
	}
	fallbackHealth = func(_ context.Context, s *ookla.Server) endpointState {
		rc.mu.Lock()
		rc.probed[s.ID] = true
		rc.mu.Unlock()
		if st, ok := health[s.ID]; ok {
			return st
		}
		return endpointOK
	}
	t.Cleanup(func() { ooklaPing, fallbackHealth = oldPing, oldHealth })
	return rc
}

// lazyFleet builds n servers with unique sponsors at 1..n km, so autoCandidates
// takes the nearest autoPingMax as the pool (all within the distance margin)
// and the rest become reserves, nearest-first: s00..s11 pool, s12.. reserve.
func lazyFleet(n int) ookla.Servers {
	out := make(ookla.Servers, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("s%02d", i)
		out = append(out, &ookla.Server{ID: id, Sponsor: "Sp" + id, Name: "N" + id,
			Distance: float64(i + 1), URL: "http://" + id + ".example:8080/speedtest/upload.php"})
	}
	return out
}

// A healthy pool must generate ZERO reserve traffic: nothing was excluded, so
// there is nothing to replenish and no reason to contact anyone past the cap.
func TestRankedServersHealthyPoolContactsNoReserves(t *testing.T) {
	rc := stubRankContacts(t, nil)
	servers := lazyFleet(20)

	ranked, _, dropped, allDead := rankedServers(context.Background(), servers, "")
	if len(ranked) != autoPingMax || len(dropped) != 0 || allDead {
		t.Fatalf("healthy pool: ranked=%d dropped=%v allDead=%v, want %d/none/false",
			len(ranked), dropped, allDead, autoPingMax)
	}
	for i := autoPingMax; i < len(servers); i++ {
		if id := fmt.Sprintf("s%02d", i); rc.contacted(id) {
			t.Errorf("reserve %s was contacted although the pool needed no replenishing (contacted: %v)",
				id, rc.contactedIDs())
		}
	}
	for i := 0; i < autoPingMax; i++ {
		if id := fmt.Sprintf("s%02d", i); !rc.contacted(id) {
			t.Errorf("pool member %s was never contacted", id)
		}
	}
}

// N exclusions must contact only the N reserves needed to refill the pool -
// not the whole reserve list.
func TestRankedServersReplenishContactsOnlyNeededReserves(t *testing.T) {
	rc := stubRankContacts(t, map[string]endpointState{
		"s03": endpointRetired, "s07": endpointRetired,
	})
	servers := lazyFleet(20)

	ranked, _, dropped, _ := rankedServers(context.Background(), servers, "")
	got := fbIDs(ranked)
	if len(got) != autoPingMax {
		t.Fatalf("pool not replenished to size: %v", got)
	}
	if fbContains(got, "s03") || fbContains(got, "s07") {
		t.Fatalf("ranked an excluded server: %v", got)
	}
	if !fbContains(got, "s12") || !fbContains(got, "s13") {
		t.Fatalf("the two nearest reserves must refill the two exclusions: %v", got)
	}
	if len(dropped) != 2 || !fbContains(dropped, "s03") || !fbContains(dropped, "s07") {
		t.Fatalf("dropped = %v, want the two pool exclusions", dropped)
	}
	for i := 14; i < len(servers); i++ {
		if id := fmt.Sprintf("s%02d", i); rc.contacted(id) {
			t.Errorf("2 exclusions contacted reserve %s beyond the 2 needed (contacted: %v)",
				id, rc.contactedIDs())
		}
	}
}

// A dead reserve widens the next batch by exactly one - the phase keeps
// contacting only what the shortfall still needs.
func TestRankedServersReserveBatchesChain(t *testing.T) {
	rc := stubRankContacts(t, map[string]endpointState{
		"s03": endpointRetired, "s12": endpointRetired,
	})
	servers := lazyFleet(20)

	ranked, _, _, _ := rankedServers(context.Background(), servers, "")
	got := fbIDs(ranked)
	if fbContains(got, "s12") || !fbContains(got, "s13") {
		t.Fatalf("a dead reserve must be passed over for the next one: %v", got)
	}
	if !rc.contacted("s13") {
		t.Fatal("the follow-up batch never ran; the pool stayed short")
	}
	for i := 14; i < len(servers); i++ {
		if id := fmt.Sprintf("s%02d", i); rc.contacted(id) {
			t.Errorf("contacted reserve %s beyond the chained shortfall (contacted: %v)",
				id, rc.contactedIDs())
		}
	}
}
