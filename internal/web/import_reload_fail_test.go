package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A failed post-import Reload used to be a silent clean success. The backup's
// auth_user is durably committed during the category loop, and every safety
// repair compares live values against a snapshot of the SAME cache the failed
// reload never updated - so all three read "nothing changed", append no
// warning, and the response reports plain success. The next restart then pairs
// the backup's username with this machine's password hash (which never rides
// in a backup): the exact lockout the repairs were written to prevent, weeks
// after the operator was told all was well.

// exhaustReconcileBudget makes the reconcile context arrive already expired,
// which is what an earlier reconcile overrunning the shared budget looks like
// from Reload's point of view (the budget starts before importMu is acquired).
func exhaustReconcileBudget(t *testing.T) {
	t.Helper()
	old := importReconcileBudget
	importReconcileBudget = -time.Second
	t.Cleanup(func() { importReconcileBudget = old })
}

// The invariant: after an import whose reload failed, either the request fails
// loudly or the stored auth/access keys equal their pre-import values - so a
// restart cannot adopt the backup's login name beside this machine's hash.
func TestFailedReloadCannotSmuggleTheBackupsLoginPastTheRepairs(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "before", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	exhaustReconcileBudget(t)

	rr := importConfig(t, s, `{"key":"auth_user","value":"from-backup"}`)

	// The restart. Whatever the response said must still be true afterwards.
	if err := s.settings.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s.settings.AuthUser(); got != "before" {
		t.Errorf("a restart after the import adopts auth_user=%q, want \"before\": the reload failed, every "+
			"repair silently skipped, and the backup's username is now paired with this machine's password "+
			"hash (response was HTTP %d %s)", got, rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	if rr.Code == http.StatusOK && len(warningsOf(t, rr)) == 0 {
		t.Errorf("the reload never made the imported settings live, yet the response is a clean success " +
			"with no warnings at all")
	}
}

// When even the fallback cannot reach a safe state - here the store dies right
// after the failed reload, so the pre-import keys cannot be written back either
// - the request must fail loudly instead of reporting success over a stored
// config that will lock the operator out at the next restart.
func TestFailedReloadWithNoWayBackFailsTheRequestLoudly(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "before", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	exhaustReconcileBudget(t)
	importReconcileHook = func() { s.store.Close() }
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_user","value":"from-backup"}`)

	if rr.Code == http.StatusOK {
		t.Errorf("HTTP 200 for an import whose settings could neither be reloaded nor rolled back: the DB "+
			"holds the backup's login settings, the operator was told nothing, and the next restart locks "+
			"them out (body %s)", strings.TrimSpace(rr.Body.String()))
	}
	if body := rr.Body.String(); !strings.Contains(body, "login") {
		t.Errorf("the failure response never mentions the login settings at risk: %s", strings.TrimSpace(body))
	}
}
