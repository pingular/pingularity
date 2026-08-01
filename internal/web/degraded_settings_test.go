package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// degradedServer reproduces the state H1 describes: the data store is perfectly
// healthy, but the ONE read that loads stored configuration failed, so the
// controller is running on compiled-in defaults instead of what the operator
// configured. settings.New returns exactly that controller on an AllSettings
// error, and main logs the error and carries on with it.
//
// The failure is induced with a closed store rather than a mock, so the
// controller reaches the real error branch. The server gets its own healthy
// store, because the point is that everything EXCEPT the settings read works -
// a broken database would prove nothing about the guard.
func degradedServer(t *testing.T) *Server {
	t.Helper()
	unreadable, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	unreadable.Close() // AllSettings now fails the way a torn page or an I/O error would

	def := settings.Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}
	set, err := settings.New(context.Background(), unreadable, def)
	if err == nil {
		t.Fatal("settings.New succeeded against a closed store; this test no longer reproduces H1")
	}
	if set == nil {
		t.Fatal("settings.New returned no controller on error; H1's premise is gone")
	}

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// A DAEMON THAT COULD NOT READ ITS OWN ACCESS CONFIGURATION MUST NOT SERVE.
//
// The auth guard asks the controller whether login is active. On a controller
// that never loaded, that question is answered from compiled-in defaults, which
// have no password - so the answer is "no login here" and every route serves
// unauthenticated, while the operator's password sits intact on disk. Only a
// stderr warning marks it, and nothing clears it but SIGHUP.
//
// Reproduced against the real binary during review: with the settings b-tree
// page damaged, /api/series, /api/logs and POST /api/data/delete all answered
// 200 with no credentials, where the same binary on the intact database
// answered 401.
//
// "Ignore the configuration and serve anyway" is not a decision this codebase
// makes anywhere else - the import reconcile path explicitly fails CLOSED for
// the same ambiguity - so this is the one place that has to be brought in line.
func TestDegradedSettingsRefusesToServe(t *testing.T) {
	s := degradedServer(t)
	h := s.Handler()

	// The routes the review reached unauthenticated, plus a destructive one.
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/api/series?hours=24", ""},
		{"GET", "/api/logs", ""},
		{"GET", "/api/settings", ""},
		{"GET", "/api/speed", ""},
		{"POST", "/api/data/delete", `{"type":"latency"}`},
		{"POST", "/api/import?config=1", `{"settings":[]}`},
		{"POST", "/api/speedtest", ""},
	} {
		w := do(t, h, tc.method, tc.path, tc.body)
		if w.Code == http.StatusOK {
			t.Errorf("%s %s served 200 with no credentials while the stored configuration was "+
				"unreadable - the operator's login was silently discarded", tc.method, tc.path)
			continue
		}
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s answered %d; a daemon that cannot read its access configuration should "+
				"say so with 503, not improvise", tc.method, tc.path, w.Code)
		}
	}
}

// Failing closed must not extend to liveness. A supervisor has to be able to
// tell "this process is alive but cannot serve" from "this process is gone",
// and an operator has to be able to see WHY - so health stays answerable.
func TestDegradedSettingsStillAnswersLiveness(t *testing.T) {
	h := degradedServer(t).Handler()
	for _, p := range []string{"/healthz"} {
		if w := do(t, h, "GET", p, ""); w.Code == http.StatusServiceUnavailable {
			t.Errorf("%s returned 503 on a degraded daemon; liveness must stay answerable so a "+
				"supervisor can tell a wedged process from a dead one", p)
		}
	}
	// Readiness is the opposite: it has to report NOT ready, or nothing upstream
	// ever learns to keep traffic away from a daemon that is refusing everything.
	if w := do(t, h, "GET", "/readyz", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz answered %d on a daemon refusing every route; it must report not ready", w.Code)
	}
}

// The guard must not have become a blanket refusal: a controller that loaded
// normally serves exactly as before.
func TestHealthySettingsAreUnaffected(t *testing.T) {
	h := newTestServer(t).Handler()
	if w := do(t, h, "GET", "/api/series?hours=24", ""); w.Code != http.StatusOK {
		t.Errorf("a healthy daemon answered %d for /api/series; the fail-closed path has leaked "+
			"into normal operation", w.Code)
	}
}
