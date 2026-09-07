package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// newFileTestServer is newTestServer over a FILE-BACKED store. The tests that
// use it have the handler and the test goroutine on the store at the same time,
// and a :memory: store's single connection would hide exactly the failures they
// exist to show. Returns the database path so a test can reopen it the way a
// restart does.
func newFileTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	return New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil))), path
}

// The post-import reconcile must finish: once the backup's config is committed,
// only its repairs stand between the stored settings and a restart adopting
// the backup's login name beside this machine's password hash. It is detached
// from the client for that reason, but it was covered against shutdown only by
// srv.Shutdown's 3s grace - unlike the manual speedtest and the exit re-trace,
// it was not on serveWG. A shutdown landing while a reconcile write waited out
// busy_timeout had Serve return with the reconcile still running, main closed
// the store under it, and the repair write failed on a closed handle.
func TestServeDrainsImportReconcileOnShutdown(t *testing.T) {
	s, path := newFileTestServer(t)
	ctx0 := context.Background()
	if err := s.settings.SetAuthPassword(ctx0, "mine", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx0, true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- s.Serve(ctx, addr) }()
	waitListening(t, addr)

	// Park the reconcile once the backup's settings are live - the moment a
	// settings write waiting out busy_timeout would be sitting in.
	parked := make(chan struct{})
	release := make(chan struct{})
	importReconcileHook = func() { close(parked); <-release }
	t.Cleanup(func() {
		importReconcileHook = nil
		select {
		case <-release:
		default:
			close(release)
		}
	})

	body := `{"pingularity_export":2,"categories":["config"],"config":[{"key":"auth_user","value":"from-backup"}]}`
	go func() {
		req, err := http.NewRequest("POST", "http://"+addr+"/api/import?config=1", strings.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth("mine", testPassword)
		if resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("the import never reached its reconcile")
	}

	cancel() // the shutdown lands inside the reconcile
	// Serve must not return while the reconcile is running: main closes the
	// store the moment it does. 3.5s is the shutdown grace plus a margin - the
	// point at which Serve used to give up on the handler.
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned (err=%v) with the post-import reconcile still running: main closes the store next, and the write that puts the operator's login name back lands on a closed handle", err)
	case <-time.After(3500 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return once the reconcile finished")
	}

	// What main does next, then the restart. Serve waiting is only half of it -
	// main's own drain has to wait for Serve, which is
	// TestShutdownWaitsForARestoresLoginRepair in the main package.
	if err := s.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	all, err := st2.AllSettings(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["auth_user"]; got != "mine" {
		t.Fatalf("a restart reads auth_user=%q, want \"mine\": the backup's login name is paired with this machine's password hash", got)
	}
}

// A restore that has not started yet is refused once shutdown has begun, the way
// a manual speedtest is: Serve is already waiting for the work it tracks, and a
// restore admitted here would commit the backup's config rows against a store
// that is about to close - the half-applied state the reconcile exists to
// prevent, with nothing left running to repair it.
func TestImportIsRefusedOnceShutdownHasBegun(t *testing.T) {
	s, _ := newFileTestServer(t)
	ctx := context.Background()
	if err := s.settings.SetAuthPassword(ctx, "mine", bcryptHashForTest(t, testPassword)); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := s.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	shut, cancel := context.WithCancel(context.Background())
	cancel()
	s.serveCtx = shut // what Serve leaves behind the moment a shutdown starts

	body := `{"pingularity_export":2,"categories":["config"],"config":[{"key":"auth_user","value":"from-backup"}]}`
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(body))
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Content-Type", "application/json")
	r.SetBasicAuth("mine", testPassword)
	s.Handler().ServeHTTP(rr, r)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("import during shutdown answered HTTP %d, want 503: %s", rr.Code, strings.TrimSpace(rr.Body.String()))
	}
	all, err := s.store.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["auth_user"]; got != "mine" {
		t.Fatalf("the refused restore still committed auth_user=%q", got)
	}
}
