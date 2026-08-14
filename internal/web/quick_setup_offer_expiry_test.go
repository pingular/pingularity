package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// seedQuickSetupOffer performs the boot step that production ALWAYS performs
// before the dashboard can be reached: main.go's materializeQuickSetup runs
// EnsureQuickSetupOffer at startup and on every reload path, which seeds the
// first-run offer clock on a fresh install (or marks an established one
// answered). newTestServer builds the Server without it, so a test that posts a
// full Quick Setup answer starts from a state production never serves - an
// unseeded clock, which settings.QuickSetupHold reads as "no offer" exactly as
// an expired one. handleQuickSetup refuses a closed offer before it validates
// anything, so tests about the answer's CONTENT must open the offer first or
// they assert 403 against every payload and stop discriminating.
func seedQuickSetupOffer(t *testing.T, s *Server) {
	t.Helper()
	if err := s.settings.EnsureQuickSetupOffer(context.Background(), time.Now().Unix()); err != nil {
		t.Fatalf("EnsureQuickSetupOffer: %v", err)
	}
	if !s.quickSetupPending(context.Background()) {
		t.Fatal("seeding the offer clock did not open the offer")
	}
}

// A stale browser tab left open past the 48-hour first-run window - or anything
// else that kept the endpoint URL - must not still be able to apply a whole
// Quick Setup answer: monitoring cadence, network access scope, and a login.
// The status endpoint stops advertising the offer when the window closes, so a
// POST that still lands after it is the client and the server disagreeing about
// whether first-run setup is still open.
func TestQuickSetupStaleTabCannotApplyAnswerAfterOfferExpires(t *testing.T) {
	const answer = `{"speedtest_enabled":true,"speed_seconds":30,"update_check":true,` +
		`"local_only":false,"auth_enabled":true,"username":"latecomer","password":"hunter2"}`
	ctx := context.Background()

	// While the offer is open the very same POST must succeed - the fix must not
	// break the flow it guards.
	open := newTestServer(t)
	seedQuickSetupOffer(t, open)
	if w := do(t, open.Handler(), "POST", "/api/quick-setup", answer); w.Code != http.StatusOK {
		t.Fatalf("POST while the offer is open: %d %s, want 200", w.Code, w.Body.String())
	}
	if !open.settings.QuickSetupDone() {
		t.Fatal("an accepted answer must mark Quick Setup done")
	}
	if !open.settings.AuthActive() {
		t.Fatal("an accepted answer carrying a password must activate the login")
	}

	// Same install, same payload, 49 hours after the offer clock was seeded: the
	// dashboard is no longer offering the dialog, so the endpoint must refuse and
	// leave every stored setting exactly as it found it.
	stale := newTestServer(t)
	if err := stale.store.SetSetting(ctx, "quick_setup_offer_since",
		strconv.FormatInt(time.Now().Unix()-49*3600, 10)); err != nil {
		t.Fatal(err)
	}
	if stale.quickSetupPending(ctx) {
		t.Fatal("a 49-hour-old offer clock must read as a closed offer")
	}
	before, err := json.Marshal(stale.settings.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, stale.Handler(), "POST", "/api/quick-setup", answer)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST after the offer expired: %d %s, want 403", w.Code, w.Body.String())
	}
	after, err := json.Marshal(stale.settings.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("a refused answer changed stored settings:\n before %s\n after  %s", before, after)
	}
	if stale.settings.AuthActive() {
		t.Fatal("a refused answer configured a login")
	}
	if stale.settings.QuickSetupDone() {
		t.Fatal("a refused answer marked Quick Setup done")
	}
}

// An answered install keeps its idempotent retry: the lost-response case (the
// server committed, the client never saw the 200) must stay a no-op success, not
// become a window refusal. The done marker is checked before the window, so a
// retry that arrives after the window closed still reads as "already answered".
func TestQuickSetupRetryAfterAnswerStaysIdempotentPastTheWindow(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t)
	seedQuickSetupOffer(t, s)
	if w := do(t, s.Handler(), "POST", "/api/quick-setup", `{"dismiss":true}`); w.Code != http.StatusOK {
		t.Fatalf("dismiss: %d %s", w.Code, w.Body.String())
	}
	// The window closes while the client is retrying.
	if err := s.store.SetSetting(ctx, "quick_setup_offer_since",
		strconv.FormatInt(time.Now().Unix()-49*3600, 10)); err != nil {
		t.Fatal(err)
	}
	if w := do(t, s.Handler(), "POST", "/api/quick-setup", `{"dismiss":true}`); w.Code != http.StatusOK {
		t.Fatalf("retry after the window closed: %d %s, want 200 (idempotent no-op)", w.Code, w.Body.String())
	}
}
