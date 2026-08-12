package speedtest

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// probeDialGuard is the SSRF denylist for every third-party-derived destination.
// It must block RFC 6598 CGNAT space (Tailscale's default range) and the
// reserved documentation/benchmark blocks, which net.IP.IsPrivate does NOT
// cover, alongside the loopback/RFC1918/link-local/metadata cases - and it must
// still let genuinely public hosts through, or every real speedtest server is
// unreachable. IPv4-mapped IPv6 forms must normalize and be caught too.
func TestProbeDialGuardBlocksCGNATAndReserved(t *testing.T) {
	blocked := []string{
		"100.64.0.5:8080",          // RFC 6598 CGNAT (Tailscale) - the FIX-2 gap
		"100.127.255.254:443",      // RFC 6598 top of range
		"[::ffff:100.64.0.5]:8080", // IPv4-mapped form of the same
		"192.0.2.1:80",             // RFC 5737 TEST-NET-1
		"198.18.0.1:80",            // RFC 2544 benchmarking
		"203.0.113.9:80",           // RFC 5737 TEST-NET-3
		"127.0.0.1:9000",           // loopback (the daemon's own UI)
		"10.0.0.1:8080",            // RFC1918
		"169.254.169.254:80",       // link-local cloud metadata
	}
	for _, a := range blocked {
		if err := probeDialGuard("tcp", a, nil); err == nil {
			t.Errorf("probeDialGuard allowed %s; want blocked", a)
		}
	}
	allowed := []string{"93.184.216.34:8080", "1.1.1.1:443", "8.8.8.8:53"}
	for _, a := range allowed {
		if err := probeDialGuard("tcp", a, nil); err != nil {
			t.Errorf("probeDialGuard blocked public %s: %v", a, err)
		}
	}
}

// probeEndpoint follows one redirect by hand and rewrites s.URL to the target.
// When the guarded re-probe of that target is REFUSED (an internal address), the
// rewrite must NOT happen: a poisoned s.URL would later be dialed by the
// separately-built measurement client. This is the retained-URL half of the
// SSRF. The custom guard here stands in for the real one: it allows the "public"
// first hop's port and refuses the "internal" target's port (both are on
// loopback in a test, so we discriminate by port).
func TestProbeEndpointDoesNotPoisonURLOnBlockedRedirect(t *testing.T) {
	var internalHits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&internalHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+internal.Listener.Addr().String()+"/speedtest/upload.php", http.StatusTemporaryRedirect)
	}))
	defer first.Close()

	_, internalPort, _ := net.SplitHostPort(internal.Listener.Addr().String())
	old := probeDialControl
	probeDialControl = func(_, address string, _ syscall.RawConn) error {
		if _, p, _ := net.SplitHostPort(address); p == internalPort {
			return fmt.Errorf("blocked internal port %s", p)
		}
		return nil // the "public" first hop is allowed
	}
	t.Cleanup(func() { probeDialControl = old })

	srv := &ookla.Server{ID: "poison-me", URL: first.URL + "/speedtest/upload.php"}
	orig := srv.URL

	st := probeEndpoint(context.Background(), srv)

	if st != endpointUnknown {
		t.Fatalf("verdict = %v, want endpointUnknown (guard refused the redirect target)", st)
	}
	if srv.URL != orig {
		t.Fatalf("s.URL was POISONED to %q (want left at %q) - a refused redirect must not be adopted", srv.URL, orig)
	}
	if n := atomic.LoadInt32(&internalHits); n != 0 {
		t.Fatalf("probe reached the internal listener %d time(s); the guard should have refused the dial", n)
	}
}

// The measurement transfer - not just the probe - must refuse an internal
// destination. A server whose URL points at loopback (as if probeEndpoint had
// adopted a redirect to it, or currentEndpoint had copied an internal catalogue
// Host) must be unreachable by the real upload/download that moves bytes. The
// guard is NOT relaxed here (no allowLoopbackProbes): loopback is exactly what
// must be refused. The security assertion is zero hits; the transfer erroring is
// incidental (the library may report N/A without an error).
func TestMeasurementTransferRefusesInternalDestination(t *testing.T) {
	var hits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	srv, err := ookla.New().CustomServer(internal.URL + "/speedtest/upload.php")
	if err != nil {
		t.Fatalf("CustomServer: %v", err)
	}
	// Re-home onto a fresh GUARDED measurement client, exactly as RunReason does
	// before every attempt.
	freshManager(nil, srv, &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	upErr := srv.UploadTestContext(ctx)
	dlErr := srv.DownloadTestContext(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("SSRF: the guarded measurement transfer reached the internal listener %d time(s)", n)
	}
	t.Logf("upErr=%v dlErr=%v (hits=0 is the security assertion)", upErr, dlErr)
}

// The list-derived path: currentEndpoint copies the catalogue's Host verbatim
// into s.URL. A hostile catalogue entry with an internal Host must still be
// unreachable by the measurement transfer once the guard is on the client.
func TestCurrentEndpointInternalHostNotReachedByTransfer(t *testing.T) {
	var hits int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer internal.Close()

	srv := &ookla.Server{ID: "list-host", Host: internal.Listener.Addr().String(), URL: "http://example.invalid/speedtest/upload.php"}
	currentEndpoint(srv) // rewrites s.URL to http://<internal-host>/speedtest/upload.php
	if got := "http://" + internal.Listener.Addr().String() + "/speedtest/upload.php"; srv.URL != got {
		t.Fatalf("currentEndpoint set URL=%q, want %q", srv.URL, got)
	}
	freshManager(nil, srv, &ookla.UserConfig{UserAgent: ookla.DefaultUserAgent})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = srv.UploadTestContext(ctx)
	_ = srv.DownloadTestContext(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("SSRF: transfer reached the internal catalogue-Host listener %d time(s)", n)
	}
}
