package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `pingularity healthz` is the container HEALTHCHECK contract: the official
// images wire HEALTHCHECK ["/pingularity","healthz"] and ship no curl/wget, so
// the binary itself must probe GET /healthz - on 127.0.0.1:9000 unless -addr
// says otherwise - and answer purely by exit code (nil return = exit 0 via
// fail()). These pin the path, the -addr override, and the verdict mapping.
func TestHealthzCmd(t *testing.T) {
	t.Run("200 is healthy", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		if err := healthzCmd([]string{"-addr", strings.TrimPrefix(srv.URL, "http://")}); err != nil {
			t.Fatalf("healthz against a 200 server: %v, want nil (exit 0)", err)
		}
		if gotPath != "/healthz" {
			t.Fatalf("probed %q, want /healthz (the guard-exempt liveness path)", gotPath)
		}
	})

	t.Run("non-200 is unhealthy with a one-line reason", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		err := healthzCmd([]string{"-addr", strings.TrimPrefix(srv.URL, "http://")})
		if err == nil {
			t.Fatal("healthz against a 503 server returned nil, want an error (nonzero exit)")
		}
		if msg := err.Error(); strings.Contains(msg, "\n") || !strings.Contains(msg, "503") {
			t.Fatalf("reason = %q; want a single line naming the status", msg)
		}
	})

	t.Run("unreachable daemon is unhealthy", func(t *testing.T) {
		// A port that just closed: nothing listens there, so the dial must fail
		// fast and report unhealthy rather than hang past the check's timeout.
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := l.Addr().String()
		l.Close()
		if err := healthzCmd([]string{"-addr", addr}); err == nil {
			t.Fatal("healthz against a dead port returned nil, want an error (nonzero exit)")
		}
	})
}

// The default probe address is part of the image contract (the Dockerfiles'
// HEALTHCHECK relies on it); changing it must be a deliberate, coordinated act.
func TestHealthzDefaultAddrContract(t *testing.T) {
	if healthzDefaultAddr != "127.0.0.1:9000" {
		t.Fatalf("healthzDefaultAddr = %q; the official images' HEALTHCHECK depends on 127.0.0.1:9000", healthzDefaultAddr)
	}
}
