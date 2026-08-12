package speedtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// rankPingLatency is the gate that fixes the zero-ping mis-rank: a successful
// ping whose mean rounds to 0 on a coarse clock (Windows sub-ms) must count as
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
		{"success, zero mean (coarse clock)", nil, true, 0, time.Nanosecond, true},
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
	if ms := applyRankPing(failed, errors.New("unreachable"), false); ms != nil {
		t.Errorf("failed ping reported a measurement %v, want nil", *ms)
	}
	if failed.Latency != 0 {
		t.Errorf("failed ping left Latency=%v, want 0 (stale value must not rank)", failed.Latency)
	}

	// A successful ping records the measured value (positive, so it sorts first).
	ok := &ookla.Server{ID: "good", Latency: 1 * time.Millisecond}
	ms := applyRankPing(ok, nil, true)
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
