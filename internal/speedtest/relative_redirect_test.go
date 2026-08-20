package speedtest

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// probeEndpoint follows ONE redirect hop by hand (a POST body is not
// replayable, so Go will not). An RFC-legal RELATIVE Location - the ordinary
// nginx rewrite shape, "307 -> /speedtest/upload.php/" - is same-host by
// construction: there is no new destination to guard and nothing stopping the
// hop. It used to be skipped anyway (the adoption branch required a host in
// the Location), and the unfollowed 3xx then fell through to retired,
// blacklisting a pinned server whose upload endpoint was one followable hop
// away - the opposite of the GET probe, which deliberately refuses to condemn
// on an unfollowed redirect.

func relRedirectServer(t *testing.T, targetStatus int) (host string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/speedtest/upload.php", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Redirect(w, r, "/speedtest/upload.php/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/speedtest/upload.php/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(targetStatus)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// TestRelativeRedirectIsFollowedNotCondemned: the hop lands on a working
// endpoint - the verdict is OK and the corrected path is adopted into s.URL,
// self-correcting exactly like an absolute redirect's host is.
func TestRelativeRedirectIsFollowedNotCondemned(t *testing.T) {
	allowLoopbackProbes(t)
	host := relRedirectServer(t, http.StatusOK)
	s := &ookla.Server{ID: "rel-redir-ok", Host: host, URL: "http://" + host + "/speedtest/upload.php"}

	if got := probeEndpoint(context.Background(), s); got != endpointOK {
		t.Fatalf("a same-host relative redirect to a working endpoint classified %v, want ok - the pinned server was just blacklisted for answering with an RFC-legal Location", got)
	}
	if want := "http://" + host + "/speedtest/upload.php/"; s.URL != want {
		t.Fatalf("s.URL = %q, want the corrected path %q adopted so the transfers use the endpoint that actually answers", s.URL, want)
	}
}

// TestRelativeRedirectToADeadEndpointStaysRetired: parity with the absolute
// branch - a real answer at the hop's end adopts the target, and a non-2xx
// there still classifies retired. Following the hop must not launder a
// genuinely gone endpoint into unknown.
func TestRelativeRedirectToADeadEndpointStaysRetired(t *testing.T) {
	allowLoopbackProbes(t)
	host := relRedirectServer(t, http.StatusNotFound)
	s := &ookla.Server{ID: "rel-redir-dead", Host: host, URL: "http://" + host + "/speedtest/upload.php"}

	if got := probeEndpoint(context.Background(), s); got != endpointRetired {
		t.Fatalf("a relative redirect to a 404 classified %v, want retired", got)
	}
}
