package speedtest

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// Every endpoint probe builds a throwaway http.Client. With keep-alives on and
// no IdleConnTimeout, each successfully probed server stranded an idle socket
// plus the abandoned transport's read/write-loop goroutines until the REMOTE
// peer closed - and rankedServers plus annotateFallback probe dozens of
// servers per pass, so a long-lived daemon accumulated fds without bound
// against peers that never close. The probe transport disables keep-alives, so
// a probe's socket must die with its response: the server side sees every
// accepted connection reach StateClosed shortly after the probe returns.
func TestProbeLeavesNoIdleConnections(t *testing.T) {
	allowLoopbackProbes(t)

	var mu sync.Mutex
	opened, closed := 0, 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test=test"))
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		switch st {
		case http.StateNew:
			opened++
		case http.StateClosed:
			closed++
		}
	}
	srv.Start()
	defer srv.Close()

	host := srv.Listener.Addr().String()
	const probes = 8
	for i := 0; i < probes; i++ {
		// probeFallback directly, not fallbackHealth: the cache would collapse
		// the repetition this test exists to exercise.
		s := &ookla.Server{ID: "leak", Host: host, URL: "http://" + host + "/speedtest/upload.php"}
		if got := probeFallback(context.Background(), s); got != endpointOK {
			t.Fatalf("probe %d = %v, want ok", i, got)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		o, c := opened, closed
		mu.Unlock()
		if o >= probes && c == o {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe connections leaked: %d opened, only %d closed - kept-alive sockets outlive the throwaway probe client", o, c)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
