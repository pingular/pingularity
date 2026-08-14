package speedtest

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// The starvation ceiling must be derived from the connection count the run's
// transfers actually use - the snapshot in o.uc.MaxConnections that every
// freshManager reuses - not a fresh live ConnectionsFn read. If the user lowers
// the count mid-run, the live read would size the ceiling for the new count
// while the transfers still run at the snapshotted count, disabling the rescue
// and corrupting the starvation diagnosis. runConnections is the single source
// both the manager path and the rescue predicate derive from.
func TestRunConnectionsUsesSnapshotNotLiveRead(t *testing.T) {
	live := 16
	o := &Ookla{ConnectionsFn: func() int { return live }}

	// Before RunReason snapshots (o.uc still nil): the live value is the stand-in.
	if got := o.runConnections(); got != 16 {
		t.Fatalf("with o.uc nil, runConnections()=%d, want the live 16", got)
	}

	// RunReason takes its snapshot into o.uc, exactly as the production path does
	// (uc.MaxConnections = o.ConnectionsFn()).
	o.uc = &ookla.UserConfig{MaxConnections: o.ConnectionsFn()}

	// The user now saves a lower count mid-run.
	live = 1

	if got := o.runConnections(); got != 16 {
		t.Fatalf("after a mid-run change to %d, runConnections()=%d, want the snapshot 16", live, got)
	}
	// And the ceiling the rescue predicate/diagnosis use follows the snapshot: a
	// 16-stream starved upload makes ~16 attempts, which must stay <= the ceiling
	// so the starvation signature is recognised. Derived from the live 1 it would
	// be starvationCeiling(1)=5, and 16 attempts would blow past it - the bug.
	if got, want := starvationCeiling(o.runConnections()), starvationCeiling(16); got != want {
		t.Fatalf("ceiling from snapshot = %d, want %d (starvationCeiling(16))", got, want)
	}
	if bug := starvationCeiling(1); starvationCeiling(o.runConnections()) == bug {
		t.Fatalf("ceiling collapsed to the mid-run live value's %d - rescue would not fire for a 16-stream starve", bug)
	}
}

// freshManager must give every rebuilt attempt its OWN UserConfig. The
// library's NewUserConfig aliases the pointer it is handed (s.config = uc) and
// then writes uc.T, so sharing the run's config across rebuilds meant
// constructing attempt N+1 repointed attempt N's client - possibly a
// not-yet-drained orphan still reading s.config.T from its worker goroutines -
// at the new transport. The run config must therefore come back from every
// rebuild untouched: any write to it would land on all earlier attempts.
func TestFreshManagerAttemptIsolation(t *testing.T) {
	o := &Ookla{}
	uc := &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent, MaxConnections: 4}
	srv := &ookla.Server{ID: "iso"}

	freshManager(o, srv, uc)
	attempt1 := srv.Context
	if attempt1 == nil {
		t.Fatal("freshManager left srv.Context nil")
	}
	if uc.T != nil {
		t.Fatal("attempt 1's rebuild wrote the shared run config's transport (uc.T) - the attempt client aliases the run config")
	}

	freshManager(o, srv, uc)
	if srv.Context == attempt1 {
		t.Fatal("attempt 2 reused attempt 1's client; freshManager exists to prevent exactly that")
	}
	if uc.T != nil {
		t.Fatal("building attempt 2 mutated the shared run config (uc.T) - attempt 1's transport would be repointed through the alias")
	}
}

// Replacing an attempt must release the abandoned client's idle sockets: the
// library transport parks keep-alive conns for a 90s IdleConnTimeout, and a
// retry loop that rebuilds a client per attempt would otherwise stack one
// stranded socket (plus transport goroutines) per rebuild for that long.
func TestFreshManagerClosesReplacedIdleConns(t *testing.T) {
	allowLoopbackProbes(t)

	var mu sync.Mutex
	opened, closed := 0, 0
	fake := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	fake.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			opened++
		case http.StateClosed:
			closed++
		}
	}
	fake.Start()
	defer fake.Close()

	o := &Ookla{}
	srv := &ookla.Server{ID: "idle"}
	freshManager(o, srv, nil)

	// One real request through attempt 1's client (its RoundTrip runs the
	// library transport), so the transport parks an idle keep-alive conn.
	req, err := http.NewRequest(http.MethodGet, fake.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Context.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	// A fully-drained body is returned to the idle pool on EOF; the sleep only
	// absorbs scheduler skew before the replacement tries to close it.
	time.Sleep(100 * time.Millisecond)

	freshManager(o, srv, nil) // replaces attempt 1: must close its idle socket

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		o1, c1 := opened, closed
		mu.Unlock()
		if o1 > 0 && c1 == o1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("replaced attempt kept its socket: %d opened, %d closed - the abandoned client's idle conns were never released", o1, c1)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
