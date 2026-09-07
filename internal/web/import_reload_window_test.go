package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The import's safety repairs compare the live login settings against a
// snapshot of what this box had BEFORE the backup's config landed. That
// snapshot was taken after the config rows were committed, on the assumption
// that nothing reloads settings in between - but a reload signal (SIGHUP,
// `systemctl reload`) is exactly such a reload, and for a backup whose config
// precedes its data the window is the rest of the restore. A reload there made
// the backup's login name and its login-off live first; the snapshot then
// described the backup, every repair read "nothing changed", and the box was
// left with a foreign login name beside its own hash and login switched off,
// with HTTP 200 and no warning.
func TestReloadInsideTheImportWindowCannotDefeatTheLoginRepairs(t *testing.T) {
	s, _ := newFileTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "admin", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	pr, pw := io.Pipe()
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?config=1&latency=1", pr)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	r.SetBasicAuth("admin", testPassword)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Handler().ServeHTTP(rr, r)
	}()

	// The backup's config streams in first; the rest of the file is still on
	// its way when the reload lands.
	if _, err := io.WriteString(pw, `{"pingularity_export":2,"categories":["config","latency"],"config":[{"key":"auth_user","value":"intruder"},{"key":"auth_enabled","value":"0"}],`); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		all, err := s.store.AllSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if all["auth_user"] == "intruder" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the backup's config rows never reached the database")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// From here on any reload makes the backup's login settings live, so the
	// guard's reconcile gate has to be up already.
	if !s.reconciling.Load() {
		t.Errorf("the backup's login settings are in the database but the reconcile gate is down: a remote request in this window is judged against whatever a reload makes live")
	}
	// And the snapshot the repairs compare against has to have been taken under
	// importMu, or a credential change could still land between it and them.
	if s.importMu.TryLock() {
		s.importMu.Unlock()
		t.Errorf("importMu is free while the backup's config sits committed: the pre-import snapshot was not taken under it, so nothing stops a credential change from landing between the snapshot and the repairs that read it")
	}
	// The reload signal.
	if err := s.settings.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, err := io.WriteString(pw, `"latency":[]}`); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the import never finished")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("import: HTTP %d: %s", rr.Code, rr.Body.String())
	}

	// The restart: whatever the response said must still hold afterwards.
	if err := s.settings.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := s.settings.AuthUser(); got != "admin" {
		t.Errorf("auth_user = %q after the restore, want \"admin\": the reload made the backup's login name live before the handler took its pre-import snapshot, so the repair saw nothing to put back (warnings %v)", got, warningsOf(t, rr))
	}
	if !s.settings.AuthActive() {
		t.Errorf("login is off after restoring an auth-off backup onto a protected box: the repair that keeps the destination's login saw nothing to keep (warnings %v)", warningsOf(t, rr))
	}
}
