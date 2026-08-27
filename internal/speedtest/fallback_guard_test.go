package speedtest

import (
	"context"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

func fbServer(id string, distance float64) *ookla.Server {
	return &ookla.Server{ID: id, Sponsor: "S" + id, Name: "N" + id,
		Distance: distance, URL: "http://s" + id + ".example:8080/speedtest/upload.php"}
}

// stubFallback swaps the health oracle for a lookup table, and stubs the ranking
// ping, so selection is tested without a network.
func stubFallback(t *testing.T, m map[string]endpointState) {
	t.Helper()
	oldHealth, oldPing := fallbackHealth, ooklaPing
	fallbackHealth = func(_ context.Context, s *ookla.Server) endpointState {
		if st, ok := m[s.ID]; ok {
			return st
		}
		return endpointOK
	}
	ooklaPing = func(_ context.Context, s *ookla.Server, cb func(time.Duration)) error {
		cb(10 * time.Millisecond)
		s.Latency = 10 * time.Millisecond
		return nil
	}
	t.Cleanup(func() { fallbackHealth, ooklaPing = oldHealth, oldPing })
}

func fbIDs(servers ookla.Servers) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, s.ID)
	}
	return out
}

func fbContains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// A server with no HTTP Legacy Fallback must not be ranked: every transfer
// against it fails, and because its 500 is served instantly it would otherwise
// sort as if healthy.
func TestRankingExcludesMissingFallback(t *testing.T) {
	stubFallback(t, map[string]endpointState{"dead": endpointRetired})
	servers := ookla.Servers{fbServer("dead", 1), fbServer("good", 2), fbServer("also", 3)}

	ranked, _, dropped, _ := rankedServers(context.Background(), servers, "")
	got := fbIDs(ranked)
	if fbContains(got, "dead") {
		t.Fatalf("ranked a server with no fallback: %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("want the two healthy servers, got %v", got)
	}
	if len(dropped) != 1 || dropped[0] != "dead" {
		t.Fatalf("dropped = %v, want [dead] so the caller can log it", dropped)
	}
}

// A probe that failed at transport level is not a verdict: the server stays.
func TestRankingKeepsUnknownFallback(t *testing.T) {
	stubFallback(t, map[string]endpointState{"maybe": endpointUnknown})
	servers := ookla.Servers{fbServer("maybe", 1), fbServer("good", 2)}

	ranked, _, dropped, _ := rankedServers(context.Background(), servers, "")
	if !fbContains(fbIDs(ranked), "maybe") {
		t.Fatalf("an unreachable probe must not condemn a server: %v", fbIDs(ranked))
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped = %v, want none", dropped)
	}
}

// Never rank nothing. A whole region can lack the fallback, and a run that
// probably fails still beats "no speedtest servers available" - the failure at
// least carries a diagnosis naming the cause.
func TestRankingFallsBackWhenAllDead(t *testing.T) {
	stubFallback(t, map[string]endpointState{
		"a": endpointRetired, "b": endpointRetired, "c": endpointRetired,
	})
	servers := ookla.Servers{fbServer("a", 1), fbServer("b", 2), fbServer("c", 3)}

	ranked, _, dropped, _ := rankedServers(context.Background(), servers, "")
	if len(ranked) != 3 {
		t.Fatalf("want every candidate back rather than an empty list, got %v", fbIDs(ranked))
	}
	if len(dropped) != 0 {
		t.Fatalf("nothing was actually dropped, so nothing should be reported: %v", dropped)
	}
}

// The guard must never override an explicit choice. A server pinned by ID is
// returned directly for a single-server run and prepended for a best-of round,
// in both cases WITHOUT passing through ranking.
func TestPinnedServerBypassesTheGuard(t *testing.T) {
	stubFallback(t, map[string]endpointState{"dead": endpointRetired})
	servers := ookla.Servers{fbServer("dead", 1), fbServer("good", 2), fbServer("also", 3)}
	o := &Ookla{}

	t.Run("single server run", func(t *testing.T) {
		targets, sel, _, err := o.pickServers(context.Background(), nil, servers, "dead", 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := fbIDs(targets); len(got) != 1 || got[0] != "dead" {
			t.Fatalf("an explicit pin must be honoured, got %v", got)
		}
		if len(sel) != 1 || !sel[0].Selected {
			t.Fatalf("selection report should mark the pin as selected: %+v", sel)
		}
	})

	t.Run("best-of round", func(t *testing.T) {
		targets, _, _, err := o.pickServers(context.Background(), nil, servers, "dead", 3, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := fbIDs(targets)
		if len(got) == 0 || got[0] != "dead" {
			t.Fatalf("the pin must lead the round even with no fallback, got %v", got)
		}
		for _, id := range got[1:] {
			if id == "dead" {
				t.Fatalf("pin duplicated into the racers: %v", got)
			}
		}
	})
}
