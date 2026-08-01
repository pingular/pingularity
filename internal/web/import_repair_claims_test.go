package web

import (
	"context"
	"strings"
	"testing"
)

// The two fail-closed access repairs appended their reassuring warning -
// "access stays limited to this machine" / "access was restricted to this
// machine only" - whether or not the SetAccessLocalOnly write actually
// persisted. The username repair already follows a persist-before-claim rule;
// these did not, so a failed write left the dashboard LAN-open with no login
// while the response asserted the opposite. Whatever the response claims about
// access must be TRUE of the live settings.

// claimsRestricted reports whether any warning claims access ended up limited
// to this machine.
func claimsRestricted(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "access stays limited to this machine") ||
			strings.Contains(w, "access was restricted to this machine") {
			return true
		}
	}
	return false
}

// saysRestrictionFailed reports whether any warning states the restriction
// could NOT be applied.
func saysRestrictionFailed(warnings []string) bool {
	for _, w := range warnings {
		if strings.Contains(w, "FAILED") {
			return true
		}
	}
	return false
}

// A protected, previously local-only box restores a backup that turns login
// off and opens network access; the settings store dies between the reload
// (backup live) and the repair - the moment a disk error or an exhausted
// reconcile budget would surface.
func TestFailedLocalOnlyRepairDoesNotClaimAccessWasRestricted(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := s.settings.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("local: %v", err)
	}
	importReconcileHook = func() { s.store.Close() }
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	warnings := warningsOf(t, rr)
	if s.settings.AccessLocalOnly() {
		t.Fatalf("fixture drift: the repair write was expected to fail, but access is local-only (HTTP %d, warnings %v)",
			rr.Code, warnings)
	}
	if claimsRestricted(warnings) {
		t.Errorf("the repair write failed and the dashboard is LAN-open with no login, yet the response "+
			"claims access was kept to this machine: %v", warnings)
	}
	if !saysRestrictionFailed(warnings) {
		t.Errorf("the repair write failed but no warning says the restriction could NOT be applied: %v", warnings)
	}
}

// The mirror site: a box with no password restores a backup that wants login on
// (unenforceable - the hash never rides) and network access open; the same
// store failure hits the unenforceable-auth repair's SetAccessLocalOnly.
func TestFailedLocalOnlyRepairAfterUnenforceableAuthDoesNotClaimRestriction(t *testing.T) {
	s := newTestServer(t)
	importReconcileHook = func() { s.store.Close() }
	t.Cleanup(func() { importReconcileHook = nil })

	rr := importConfig(t, s, `{"key":"auth_enabled","value":"1"},{"key":"access_local_only","value":"0"}`)
	warnings := warningsOf(t, rr)
	if s.settings.AccessLocalOnly() {
		t.Fatalf("fixture drift: the repair write was expected to fail, but access is local-only (HTTP %d, warnings %v)",
			rr.Code, warnings)
	}
	if claimsRestricted(warnings) {
		t.Errorf("the repair write failed and the dashboard is LAN-open with an unenforceable login, yet "+
			"the response claims access was restricted to this machine: %v", warnings)
	}
	if !saysRestrictionFailed(warnings) {
		t.Errorf("the repair write failed but no warning says the restriction could NOT be applied: %v", warnings)
	}
}

// C1(a): a backup that turns login off must NOT disable the login on a box that
// owned a working one. The old repair traded the login for a local-only
// restriction, which does not protect a box behind a declared same-host proxy;
// the password hash never rides in a backup, so the destination's own hash
// survives and re-enabling auth restores full protection. Restoring an auth-off
// backup onto a previously local-only, password-protected box must therefore
// leave the LOGIN active (not merely re-assert local-only).
func TestImportKeepsDestinationLoginEvenWhenPreviouslyLocalOnly(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := s.settings.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("local: %v", err)
	}
	rr := importConfig(t, s, `{"key":"auth_enabled","value":"0"},{"key":"access_local_only","value":"0"}`)
	warnings := warningsOf(t, rr)
	if !s.settings.AuthActive() {
		t.Fatalf("the backup turned login off and the repair let it, leaving the box unprotected "+
			"(local-only alone cannot shield a proxied box); auth should stay active (HTTP %d, warnings %v)",
			rr.Code, warnings)
	}
	if len(warnings) == 0 {
		t.Errorf("the login was kept against the backup's wishes but no warning tells the operator")
	}
}
