package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/web"
)

// A restore that has committed the backup's config rows is mid-repair, and that
// repair is the only thing standing between those rows and a restart pairing the
// backup's login NAME with this machine's password hash. The web server waits
// for the handler - but the shutdown that closes the store is main's, and main
// waited a flat four seconds for its background workers and then closed the
// store anyway. One repair write waiting out SQLite's busy timeout outlasts
// that, so the write that puts the operator's own login name back landed on a
// closed handle and the restart adopted the backup's.
//
// So this drives the real pieces in the real order: a file-backed store, the
// real Serve, a real restore over HTTP, a second connection holding the write
// lock the way any other writer would, and the shutdown drain main itself runs.
func TestShutdownWaitsForARestoresLoginRepair(t *testing.T) {
	ctx0 := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "p.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	set, err := settings.New(ctx0, st, testDefaultsFor(config.Config{}))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	// A password this box owns and a login switched off: the backup's username
	// must not be pinned to that hash, and no credentials are needed to post the
	// restore. (Backups never carry the hash - that is why the repair exists.)
	if err := set.SetAuthPassword(ctx0, "mine", "$2a$10$notarealhashbutstoredallthesame"); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := set.SetAuthEnabled(ctx0, false); err != nil {
		t.Fatalf("login off: %v", err)
	}

	srv := web.New(st, nil, nil, set, nil, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	addr := freeLoopbackAddr(t)
	ctx, cancel := context.WithCancel(ctx0)
	defer cancel()
	var bg sync.WaitGroup
	bg.Add(1)
	serveErr := make(chan error, 1)
	go func() { defer bg.Done(); serveErr <- srv.Serve(ctx, addr) }()
	waitForListener(t, addr)

	// Stream the restore so the config rows land while the request is still
	// open: everything up to the end of the config array, then a pause, then the
	// closing brace. The pause is where the test gets to arrange the world.
	pr, pw := io.Pipe()
	finishBody := make(chan struct{})
	go func() {
		io.WriteString(pw, `{"pingularity_export":2,"categories":["config"],"config":[{"key":"auth_user","value":"from-backup"}]`)
		<-finishBody
		io.WriteString(pw, `}`)
		pw.Close()
	}()
	importDone := make(chan struct{})
	go func() {
		defer close(importDone)
		req, err := http.NewRequest("POST", "http://"+addr+"/api/import?config=1", pr)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if resp, err := (&http.Client{Timeout: time.Minute}).Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// Wait for the backup's row to be committed - from here on only the repair
	// stands between it and a restart.
	waitForSetting(t, dbPath, "auth_user", "from-backup")

	// Any other writer will do; SQLite allows one at a time and makes the rest
	// wait out busy_timeout. This one is holding the lock when the repair tries
	// to take it, which is the whole of the finding's mechanism.
	lock, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	held, err := lock.Conn(ctx0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.ExecContext(ctx0, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("take the write lock: %v", err)
	}
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			held.ExecContext(ctx0, "ROLLBACK")
			held.Close()
		})
	}
	defer release()

	close(finishBody) // the handler finishes the body and enters the reconcile
	cancel()          // and the shutdown lands on it

	// The other writer lets go just after the ordinary grace runs out - inside
	// the busy timeout the repair write is sitting in, and past the point where
	// shutdown used to give up. So the repair can still land, if the store is
	// still open when it does. That is the whole question.
	go func() { time.Sleep(shutdownWorkerGrace + 200*time.Millisecond); release() }()
	drained := make(chan struct{})
	go func() { bg.Wait(); close(drained) }()
	drainWorkers(drained, srv.RestoreInFlight, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// main closes the store the instant the drain returns. By then the repair
	// must have landed.
	if got := readSetting(t, dbPath, "auth_user"); got != "mine" {
		t.Fatalf("the drain gave up with auth_user=%q: main closes the store next, so the write putting the operator's own login name back never lands and the restart pairs the backup's name with this machine's password hash", got)
	}
	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after the drain")
	}
	<-importDone
}

// freeLoopbackAddr picks a loopback port nothing is on.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func waitForListener(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s", addr)
}

// readSetting reads one settings row through its own connection, so it neither
// waits on nor blocks the writer under test (WAL readers never do).
func readSetting(t *testing.T, dbPath, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return v
}

func waitForSetting(t *testing.T, dbPath, key, want string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if readSetting(t, dbPath, key) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never became %q", key, want)
}

// Stop returning is what lets the process exit, so its wait has to outlast
// every wait run's own shutdown can take - otherwise the daemon dies with the
// store still open and the restore's repair still trying to land, which is the
// state waiting was supposed to prevent.
func TestStopOutwaitsTheWorkerDrain(t *testing.T) {
	if worst := shutdownWorkerGrace + web.RestoreDrainBudget(); stopWait() <= worst {
		t.Fatalf("Stop waits %v but run's shutdown can take %v: the process exits out from under the store close", stopWait(), worst)
	}
}

// Both tests above drive the drain with the pieces handed to them, and run()'s
// own assembly - which is where those pieces are actually joined - is not
// unit-testable: delete the one line that tells the drain how to ask about a
// restore, or put Stop's wait back to a flat five seconds, and everything here
// stays green while a shutdown goes back to closing the store under the repair.
// So the wiring is guarded literally, the way the first-run hooks are.
func TestMainWiresTheRestoreDrain(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"restoreDraining = srv.RestoreInFlight",      // the drain's only way to know a restore is mid-repair
		"drainWorkers(done, restoreDraining, p.log)", // and run's shutdown has to be the thing asking
		"case <-time.After(stopWait()):",             // Stop must outlast that drain, or the process exits under it
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("main.go no longer has %q: a shutdown lands on the store while a restore is still putting the login settings back, and nothing here catches it", want)
		}
	}
}
