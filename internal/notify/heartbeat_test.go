package notify

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pingular/pingularity/internal/stats"
)

// An unset heartbeat URL is the off switch, not a failure: it must stay a silent
// no-op with nothing to report.
func TestHeartbeatEmptyURLIsNoError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, url := range []string{"", "   "} {
		if err := Heartbeat(context.Background(), http.DefaultClient, url, log); err != nil {
			t.Errorf("Heartbeat(%q) = %v, want nil", url, err)
		}
	}
}

// The outcome is now the caller's to act on, so each failure mode must actually
// return one - and none of them may carry the URL, which is a credential.
func TestHeartbeatReportsOutcome(t *testing.T) {
	const token = "s3cr3t-t0k3n"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if err := Heartbeat(ctx, ok.Client(), ok.URL+"/ping/"+token, log); err != nil {
		t.Errorf("a 200 check-in reported an error: %v", err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	err := Heartbeat(ctx, bad.Client(), bad.URL+"/ping/"+token, log)
	if err == nil {
		t.Fatal("a 500 must be reported to the caller")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("status error should name the status, got %q", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("status error leaked the URL: %q", err)
	}

	// A URL too malformed to build a request from never reaches the network, so
	// only the return value can tell the user anything.
	const junk = "http://example.com/\x7f/" + token
	err = Heartbeat(ctx, NewHeartbeatClient(), junk, log)
	if err == nil {
		t.Fatal("an unusable URL must be reported to the caller")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("parse error leaked the URL: %q", err)
	}

	// A dead destination: the URL reaches the error as a *url.Error, so this is
	// the path where an unscrubbed return would hand the token to the browser.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL + "/ping/" + token
	dead.Close()
	err = Heartbeat(ctx, NewHeartbeatClient(), deadURL, log)
	if err == nil {
		t.Fatal("an unreachable destination must be reported to the caller")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), deadURL) {
		t.Errorf("transport error leaked the URL: %q", err)
	}
}

// A link-local/metadata URL is refused before any dial. It stays counted and
// warned about (silence there would hide a watchdog that is never pinged), and
// now also returns an error - with no trace of the URL in it.
func TestHeartbeatBlockedDestination(t *testing.T) {
	stats.ResetForTest()
	const token = "s3cr3t-t0k3n"
	const url = "http://169.254.169.254/ping/" + token
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))

	err := Heartbeat(context.Background(), NewHeartbeatClient(), url, log)
	if err == nil {
		t.Fatal("a blocked destination must be reported to the caller")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "169.254.169.254") {
		t.Errorf("blocked error leaked the URL: %q", err)
	}
	if got := stats.Lifetime().Counters["notify.heartbeat.blocked"]; got != 1 {
		t.Errorf("notify.heartbeat.blocked = %d, want 1", got)
	}
	if !strings.Contains(logged.String(), "heartbeat blocked") {
		t.Errorf("blocked heartbeat was not warned about: %q", logged.String())
	}
}

// A heartbeat URL is a credential - Healthchecks puts the check UUID in the
// path. Go attaches the previous URL as Referer on every redirect hop, so a
// watchdog that bounces the ping would hand the token to whatever host it names.
func TestHeartbeatDoesNotLeakTheURLToRedirectTargets(t *testing.T) {
	var got string
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Referer")
	}))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/next", http.StatusFound)
	}))
	defer first.Close()

	if err := Heartbeat(t.Context(), NewHeartbeatClient(), first.URL+"/ping/s3cr3t-uuid-token", slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("redirected ping should still succeed: %v", err)
	}
	if got != "" {
		t.Errorf("redirect target received Referer %q, want none - that header carries the check UUID", got)
	}
}
