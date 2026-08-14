package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// newTestServerWith is newTestServer with the store and controller supplied by
// the caller: these tests need a controller whose DEFAULTS differ from the
// store's contents, which is the state an env-var-supplied access scope creates
// and the shared helper cannot express.
func newTestServerWith(t *testing.T, st *store.Store, set *settings.Controller) *Server {
	t.Helper()
	return New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// The access scope an operator sees can come from -access / PINGULARITY_ACCESS
// instead of the store: main seeds the controller's DEFAULTS from the same
// config, so with PINGULARITY_ACCESS=network the effective value is already
// "network" while the settings table holds no access row at all.
//
// That is the exact state the documented container recovery leaves behind, and
// the state in which "open the Access tab and hit Save" has to WORK: the whole
// point of that click is to turn the env var's value into a stored choice so
// the variable can be dropped. The handler's no-change guard used to return
// before writing anything, because the submitted value equalled the live one -
// so Save appeared to succeed, stored nothing, and the next boot without the
// variable was local-only again with a 403 on the published port.
//
// No login is configured here, so the step-up that now guards this same write
// (see TestAccessSaveCannotMakeAnEnvOpenScopePermanentWithoutThePassword) asks
// for nothing: there is no current password to prove, and an install anyone can
// already reconfigure gains no protection from a prompt it cannot pose.
func TestAccessSavePersistsAnEnvSuppliedScope(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// Defaults as main builds them under PINGULARITY_ACCESS=network: network
	// scope in the DEFAULTS, nothing in the store.
	def := settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: false,
	}
	set, err := settings.New(ctx, st, def)
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	if all, err := st.AllSettings(ctx); err != nil {
		t.Fatalf("AllSettings: %v", err)
	} else if _, stored := all["access_local_only"]; stored {
		t.Fatal("precondition: the store must hold no access row - the value comes from the seeded default")
	}
	if set.AccessLocalOnly() {
		t.Fatal("precondition: the effective scope must already be network, so the save is a no-change one")
	}

	s := newTestServerWith(t, st, set)
	// Exactly what the Access tab sends when the operator leaves Network access
	// on and clicks Save: the value already in effect.
	rr := do(t, s.Handler(), "POST", "/api/access", `{"local_only":false}`)
	if rr.Code != 200 {
		t.Fatalf("save: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	all, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if _, stored := all["access_local_only"]; !stored {
		t.Fatal("Save wrote nothing: the operator's choice is still only the env var's, so dropping the variable silently returns the install to local-only and 403s its published port")
	}

	// The stored choice must survive the variable going away: rebuild the
	// controller with the fresh-install default (local-only), as a boot without
	// PINGULARITY_ACCESS would, and the saved network scope must still win.
	reboot, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if reboot.AccessLocalOnly() {
		t.Fatal("after dropping the env var the install fell back to local-only: the saved choice did not persist")
	}
}

// accessScopeFromEnv builds the state an -access / PINGULARITY_ACCESS scope
// leaves behind: the value is live because it was SEEDED as a default, and the
// settings table holds no access row at all, so dropping the variable returns
// the install to whatever a fresh boot defaults to.
func accessScopeFromEnv(t *testing.T, localOnly bool) (*store.Store, *settings.Controller) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: localOnly,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	if all, err := st.AllSettings(context.Background()); err != nil {
		t.Fatalf("AllSettings: %v", err)
	} else if _, stored := all["access_local_only"]; stored {
		t.Fatal("precondition: the scope must come from the seeded default, not the store")
	}
	return st, set
}

// storedScope reports the access row the settings table actually holds - the
// only thing that survives the env var going away.
func storedScope(t *testing.T, st *store.Store) (value string, stored bool) {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	v, ok := all["access_local_only"]
	return v, ok
}

// doSessionFrom is doSession with the peer address chosen by the caller: a save
// made while access is local-only has to arrive from loopback or the access
// filter refuses it long before the handler runs.
func doSessionFrom(t *testing.T, s *Server, h http.Handler, peer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/access", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = peer
	r.AddCookie(&http.Cookie{Name: sessionCookie,
		Value: issueToken(s.settings.AuthUser(), s.settings.AuthHash(), s.settings.SessionEpoch(), time.Now())})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Saving the OPEN scope that an env var supplies is not the no-op it looks
// like: it turns a posture that lasts only as long as PINGULARITY_ACCESS into a
// stored one that outlives it. The operator who set the variable to recover
// access, then removed it expecting the install to go back to local-only, stays
// reachable from the whole network - and the session that did that to them
// never proved it knew the password.
//
// So that one write is SKIPPED, not refused, and the difference is the whole
// design. Refusing it was tried first and broke far more than it fixed: the
// Settings drawer echoes local_only back on EVERY Save and aborts the entire
// save when this call fails, so a 403 here meant no setting of any kind could
// be saved on the most ordinary container setup there is - started with
// PINGULARITY_ACCESS=network, password set, no stored access row. Skipping
// leaves the posture exactly as it was (open, and still conditional on the
// variable), costs the operator nothing they can see, and keeps every unrelated
// save working.
func TestAnEnvOpenScopeIsNotMadePermanentBySavingSettings(t *testing.T) {
	st, set := accessScopeFromEnv(t, false) // PINGULARITY_ACCESS=network
	s := newTestServerWith(t, st, set)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	// A session cookie and nothing else - what a hijacked or walk-up browser
	// has, and equally what an ordinary operator saving an unrelated setting
	// has. Same body the drawer sends: the live scope echoed back.
	w := doSessionFrom(t, s, h, "192.0.2.10:5000", `{"local_only":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("an ordinary Save on an env-opened, login-protected install: %d %s, want 200. The drawer "+
			"posts this before every settings save and gives up if it fails, so a refusal here means no "+
			"setting at all can be saved on that install", w.Code, w.Body)
	}
	if v, stored := storedScope(t, st); stored {
		t.Fatalf("the save wrote access_local_only=%q although nothing changed and nobody proved the password: "+
			"dropping the env var no longer returns the install to local-only, and a session that only had a "+
			"cookie did that", v)
	}

	// The install must still be reachable - skipping the write changes nothing
	// that is in effect right now.
	if set.AccessLocalOnly() {
		t.Fatal("the live access scope closed as a side effect of a save that changed nothing")
	}

	// And the posture really is still conditional: drop the variable, and the
	// install goes back to local-only, which is the property being protected.
	reboot, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if !reboot.AccessLocalOnly() {
		t.Fatal("after dropping the env var the install is still open to the network: the save made an " +
			"env-conditional posture permanent")
	}
}

// Offering the password is what "I mean this" looks like, and it must be enough
// on its own - no toggling required.
//
// This is the documented flow (README: open the Access tab, leave Network access
// on, Save, then drop the variable) and the ONLY route to a permanent choice
// when the operator is working over the network. The alternative - make it a
// real change by switching to local-only first - 403s the very browser doing it
// the instant it lands, so there would be no way back to network access at all.
func TestOfferingThePasswordStoresAnEnvOpenScope(t *testing.T) {
	st, set := accessScopeFromEnv(t, false) // PINGULARITY_ACCESS=network
	s := newTestServerWith(t, st, set)
	setPassword(t, s, "admin", "secret")

	// From a NON-loopback peer, which is where an operator recovering a
	// container actually is - and where the toggle route would lock them out.
	w := doSessionFrom(t, s, s.Handler(), "192.0.2.10:5000", `{"local_only":false,"current_password":"secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("saving the network scope WITH the current password: %d %s, want 200 - "+
			"the operator can no longer make the env-var recovery stick", w.Code, w.Body)
	}
	if v, stored := storedScope(t, st); !stored || v != "0" {
		t.Fatalf("access_local_only = %q (stored=%v), want \"0\": the proven save did not persist", v, stored)
	}
	// The whole point of that save: it must outlive the variable.
	reboot, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: true,
	})
	if err != nil {
		t.Fatalf("reload settings: %v", err)
	}
	if reboot.AccessLocalOnly() {
		t.Fatal("after dropping the env var the install fell back to local-only: the proven choice did not " +
			"persist, so there is no way to make network access permanent at all")
	}
}

// A WRONG password is refused, and that is the only way this branch refuses
// anything. It costs an ordinary save nothing, because the drawer sends the
// field only when someone has typed in it.
func TestAWrongPasswordIsStillRefusedOnTheNoChangeSave(t *testing.T) {
	st, set := accessScopeFromEnv(t, false)
	s := newTestServerWith(t, st, set)
	setPassword(t, s, "admin", "secret")

	w := doSessionFrom(t, s, s.Handler(), "192.0.2.10:5000", `{"local_only":false,"current_password":"wrong"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a wrong current password: %d %s, want 403 - guessing must cost the same here as anywhere else",
			w.Code, w.Body)
	}
	if v, stored := storedScope(t, st); stored {
		t.Fatalf("the refused save still wrote access_local_only=%q", v)
	}
}

// Once the scope IS stored open, every later Save rewrites the same value. That
// has to stay free of any gate: it is what a network install with a stored
// choice does on every single settings save, and it makes nothing more
// permanent than it already is.
func TestSavingAnAlreadyStoredOpenScopeStaysFree(t *testing.T) {
	st, set := accessScopeFromEnv(t, false)
	if err := set.SetAccessLocalOnly(context.Background(), false); err != nil {
		t.Fatalf("store the scope: %v", err)
	}
	s := newTestServerWith(t, st, set)
	setPassword(t, s, "admin", "secret")

	w := doSessionFrom(t, s, s.Handler(), "192.0.2.10:5000", `{"local_only":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-saving an already-stored network scope: %d %s, want 200", w.Code, w.Body)
	}
	if v, stored := storedScope(t, st); !stored || v != "0" {
		t.Fatalf("access_local_only = %q (stored=%v), want \"0\": the stored choice was dropped", v, stored)
	}
}

// The closing direction is free, and must stay free: storing local-only can
// only take reach away, so no session can widen anything with it, and the
// operator locking a box down - or making a local-only default permanent before
// dropping the flag that set it - must not be stopped by a password prompt.
func TestAccessSaveLockdownNeedsNoStepUp(t *testing.T) {
	for _, tc := range []struct{ name, pass string }{
		{"auth active", "secret"}, // a live session, no current_password in the body
		{"no auth configured", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, set := accessScopeFromEnv(t, true) // -access local, or the fresh-install default
			s := newTestServerWith(t, st, set)
			h := s.Handler()
			var w *httptest.ResponseRecorder
			if tc.pass != "" {
				setPassword(t, s, "admin", tc.pass)
				// Loopback: local-only is already in force, so any other peer is
				// refused by the access filter before the handler sees it.
				w = doSessionFrom(t, s, h, "127.0.0.1:4444", `{"local_only":true}`)
			} else {
				r := httptest.NewRequest("POST", "/api/access", strings.NewReader(`{"local_only":true}`))
				r.Header.Set("Content-Type", "application/json")
				r.Host = "127.0.0.1:9000"
				r.RemoteAddr = "127.0.0.1:4444"
				w = httptest.NewRecorder()
				h.ServeHTTP(w, r)
			}
			if w.Code != http.StatusOK {
				t.Fatalf("saving local-only: %d %s, want 200 - closing access cannot expose anything, so it must not demand a password", w.Code, w.Body)
			}
			if v, stored := storedScope(t, st); !stored || v != "1" {
				t.Fatalf("access_local_only = %q (stored=%v), want \"1\": the lockdown did not persist, so the next boot without the flag is network-reachable again", v, stored)
			}
		})
	}
}

// The guard's original job must survive: a save that changes nothing at all
// still writes nothing, so it cannot spend the step-up/limiter budget or
// manufacture rows for an install that only opened the tab.
func TestAccessSaveWithNoAccessFieldWritesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	// Network scope so the test's own (non-loopback) request reaches the
	// handler at all - what is under test here is the empty save, not the
	// access filter.
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, AccessLocalOnly: false,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	before, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	s := newTestServerWith(t, st, set)
	if rr := do(t, s.Handler(), "POST", "/api/access", `{}`); rr.Code != 200 {
		t.Fatalf("empty save: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	after, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("an empty save wrote settings: before=%d after=%d", len(before), len(after))
	}
}
