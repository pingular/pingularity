package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// The end-to-end guard for the two wires the unit tests cannot reach: the one
// that carries -on-corrupt from the command line into the open, and the one
// that asks the store whether it was rebuilt before the boot settles its access
// posture. Both compile whatever they are wired to, and a real install is the
// only place they are ever exercised together - the same shape smoke_test's
// boot exists for. It builds the binary and runs it twice on the same damaged
// database: once as an operator who has said nothing, once as one who has asked
// for the rebuild.
func TestBootOnADamagedDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping build-and-boot test in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "pingularity")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	build := exec.Command(goBin, "build", "-o", bin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build failed: %v", err)
	}

	// An install as it stood before the fault: a password, and network access.
	dbPath := filepath.Join(dir, "pingularity.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSettings(context.Background(), map[string]string{
		"auth_enabled":      "1",
		"auth_user":         "admin",
		"auth_hash":         authHash,
		"access_local_only": "0",
	}); err != nil {
		t.Fatalf("seed the install: %v", err)
	}
	if err := st.InsertSpeedServers(context.Background(), []store.SpeedServerRow{
		{RunTS: time.Now().Add(-time.Hour).Unix(), ServerID: "1234", Server: "Sponsor, City", RankOrder: 1, Selected: true, Measured: true, Winner: true},
	}); err != nil {
		t.Fatalf("seed a speedtest server row: %v", err)
	}
	st.Close()
	// The shape a real upgrade meets, rather than one this build has already
	// migrated: no per-server index yet, so the start is what builds it, and
	// the table it reads from end to end to do that is the damaged one.
	dropIndex(t, dbPath, "idx_speed_servers_server")
	tearTableRoot(t, dbPath, "speed_servers")

	port, releasePort := reserveHighPort(t)
	addr := "127.0.0.1:" + port
	// -latency=false keeps the boot off the network: this test is about what
	// the daemon does with the file, not what it measures.
	args := []string{"run", "-listen", addr, "-db", dbPath, "-latency=false", "-quick-setup=skip", "-access", "network"}

	// Nobody asked for a rebuild: the start fails, says how to recover, and the
	// file stays where it is with the install's settings still in it.
	refuse := exec.Command(bin, args...)
	out, err := refuse.CombinedOutput()
	if err == nil {
		t.Fatal("the daemon started on a damaged database nobody asked it to replace")
	}
	for _, want := range []string{dbPath, ".recover", "-on-corrupt rebuild"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the daemon set the damaged database aside unasked (%v)", matches)
	}
	// Nothing was moved or replaced, and the file still holds what the install
	// had. Not "byte for byte": the migration commits the columns and indexes it
	// manages before it meets the damage, so a legacy file grows a little. What
	// must never happen is the file leaving, or the login leaving with it.
	if got := settingInFile(t, dbPath, "auth_hash"); got != authHash {
		t.Fatalf("the refused database no longer holds the install's login: auth_hash = %q", got)
	}
	if got := settingInFile(t, dbPath, "access_local_only"); got != "0" {
		t.Fatalf("the refused database no longer holds the install's access setting: access_local_only = %q", got)
	}

	// And the operator who asks for it: the daemon comes back monitoring on an
	// empty store, and says so in the two places that reach anyone who is not
	// reading the log - the readiness verdict, and the access posture.
	releasePort()
	cmd := exec.Command(bin, append(args, "-on-corrupt", "rebuild")...)
	logPath := filepath.Join(dir, "rebuild.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start failed: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	base := "http://" + addr
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	var up bool
	for time.Now().Before(deadline) {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			up = resp.StatusCode == http.StatusOK
			if up {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !up {
		t.Fatal("the daemon did not come back on the rebuilt store; /healthz never answered 200")
	}
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) == 0 {
		t.Fatal("the daemon started but set nothing aside")
	}

	ready, err := client.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	readyBody, _ := io.ReadAll(ready.Body)
	ready.Body.Close()
	if ready.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d %q, want 503: an alert on this daemon reports a healthy service over an empty dashboard", ready.StatusCode, strings.TrimSpace(string(readyBody)))
	}

	acc, err := client.Get(base + "/api/access")
	if err != nil {
		t.Fatalf("GET /api/access: %v", err)
	}
	var access map[string]any
	json.NewDecoder(acc.Body).Decode(&access)
	acc.Body.Close()
	if pw, _ := access["has_password"].(bool); pw {
		t.Fatal("fixture: the rebuilt store still has a password, so this proves nothing")
	}
	if lo, _ := access["local_only_active"].(bool); !lo {
		t.Errorf("the rebuilt store is answering the network with no password behind it: %v", access)
	}
	// Holding the access silently would leave an operator with a container that
	// 403s its own published port and nothing anywhere saying why - the shape
	// this project has already been burned by. The warning is the whole
	// explanation, so it has to be in the log a rebuilt start writes.
	boot, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"access held to this machine", "set a password", "restart or reload", "reset-auth"} {
		if !strings.Contains(string(boot), want) {
			t.Errorf("the boot that held itself to loopback never says %q, so the reason is nowhere:\n%s", want, boot)
		}
	}

	// The restart after it, with the same flags, is the one that matters: it
	// opens a healthy database - the empty one the rebuild made - with nothing on
	// the handle to say what happened. It must still hold the network, still say
	// why, and readiness must still not call it ready.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	rebuildArgs := append(args, "-on-corrupt", "rebuild")
	restartLog := filepath.Join(dir, "restart.log")
	stop := bootDaemon(t, bin, rebuildArgs, restartLog, client, base)
	if code, body := fetch(t, client, "GET", base+"/readyz", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "held") {
		t.Errorf("/readyz on the restart = %d %q, want a 503 saying the network is held: the alert that fired at the rebuild went quiet while the LAN still gets 403", code, body)
	}
	if lo, _ := accessOf(t, client, base)["local_only_active"].(bool); !lo {
		t.Error("the restart after a rebuild answers the network with no password behind it: the hold lasted one start, and the open dashboard arrived one restart late")
	}
	stop()
	if got := readFile(t, restartLog); !strings.Contains(got, "access held to this machine") {
		t.Errorf("the restart that held the network never said why:\n%s", got)
	}

	// A rollback in between: an older release does not know the hold, and one
	// ordinary Save there stores network access beside it. The start after that,
	// with the same flags, holds the network all the same, sets the stored
	// choice back, and says so.
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("open for the rollback's Save: %v", err)
	}
	if err := st.SetSetting(context.Background(), "access_local_only", "0"); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	rollbackLog := filepath.Join(dir, "after-rollback.log")
	// Behind a declared same-host reverse proxy, which local-only cannot see past:
	// the start has to say so, because the hold is local-only whatever the stored
	// setting says.
	stop = bootDaemon(t, bin, append(append([]string{}, rebuildArgs...), "-allow-host", "dash.example.invalid"), rollbackLog, client, base)
	if code, body := fetch(t, client, "GET", base+"/readyz", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "held") {
		t.Errorf("/readyz after a rollback stored network access = %d %q, want a 503 saying the network is held", code, body)
	}
	if lo, _ := accessOf(t, client, base)["local_only_active"].(bool); !lo {
		t.Error("network access an older release stored beside the hold opened the rebuilt store with no login")
	}
	// Network access switched on from this machine's own Access tab, with no
	// login to ask for: a choice made at the machine, which releases the hold.
	if code, body := fetch(t, client, "POST", base+"/api/access", `{"local_only":false}`); code != http.StatusOK {
		t.Fatalf("POST /api/access from the machine = %d %q", code, body)
	}
	stop()
	if got := readFile(t, rollbackLog); !strings.Contains(got, "stored_network_access") {
		t.Errorf("the start that set a rolled-back network setting back to local-only never said so:\n%s", got)
	}
	if got := readFile(t, rollbackLog); !strings.Contains(got, "'local only' access is on, but -allow-host declares a reverse proxy") {
		t.Errorf("a held start behind a declared reverse proxy never said local-only cannot block what arrives through it:\n%s", got)
	}

	// And the start after that serves the network the operator chose, and does
	// not claim to be holding anything.
	chosenLog := filepath.Join(dir, "chosen.log")
	stop = bootDaemon(t, bin, rebuildArgs, chosenLog, client, base)
	if lo, _ := accessOf(t, client, base)["local_only_active"].(bool); lo {
		t.Error("the hold took back network access chosen at the machine itself")
	}
	code, body := 0, ""
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if code, body = fetch(t, client, "GET", base+"/readyz", ""); code == http.StatusOK {
			break
		}
	}
	if code != http.StatusOK {
		t.Errorf("/readyz = %d %q with the network open by the operator's own choice, want 200: nothing is being held", code, body)
	}
	stop()
	if got := readFile(t, chosenLog); strings.Contains(got, "access held to this machine") {
		t.Errorf("a start serving the network the operator chose still warns that it is holding it:\n%s", got)
	}

	// A store whose settings cannot be read cannot say whether it carries the
	// hold, so the start holds the network anyway - an access decision fails
	// closed - and says what it could not read, without claiming a rebuild that
	// may never have happened. A settings table torn past reading, on a store
	// that otherwise opens, is the shape: enough rows that the table's root is
	// an interior page, and that page gone.
	tornPath := filepath.Join(dir, "torn-settings.db")
	torn, err := store.Open(tornPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	filler := map[string]string{}
	for i := 0; i < 3000; i++ {
		filler[fmt.Sprintf("filler_%04d", i)] = strings.Repeat("x", 200)
	}
	if err := torn.SetSettings(context.Background(), filler); err != nil {
		t.Fatalf("fill the settings table: %v", err)
	}
	torn.Close()
	tearTableRoot(t, tornPath, "settings")
	tornLog := filepath.Join(dir, "torn.log")
	stop = bootDaemon(t, bin, []string{"run", "-listen", addr, "-db", tornPath, "-latency=false", "-quick-setup=skip", "-access", "network"}, tornLog, client, base)
	// Nothing is served on it: settings that could not be read are refused as
	// they always were, and that refusal - not a hold nobody found - is what a
	// visitor is told.
	code, body = fetch(t, client, "GET", base+"/api/status", "")
	stop()
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "settings could not be loaded") {
		t.Errorf("GET /api/status on a start whose settings could not be read = %d %q, want the 503 saying the settings could not be loaded", code, body)
	}
	got := readFile(t, tornLog)
	if !strings.Contains(got, "could not read or release the access hold") {
		t.Errorf("a start that could not read the access hold never said so:\n%s", got)
	}
	// The start line states the access it was started with, as it did before
	// there was a hold to read: the refusal above is what keeps the network out.
	if !strings.Contains(got, "access network") {
		t.Errorf("a start whose settings could not be read no longer states the access it was started with:\n%s", got)
	}
	if strings.Contains(got, "access held to this machine") {
		t.Errorf("a start that could not read its settings claims the store was rebuilt after damage:\n%s", got)
	}
}

// bootDaemon starts the built binary with args, its output in logPath, waits
// for /healthz to answer, and returns the stop that kills it and waits it out.
func bootDaemon(t *testing.T, bin string, args []string, logPath string, client *http.Client, base string) func() {
	t.Helper()
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start failed: %v", err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		logFile.Close()
	}
	t.Cleanup(stop)
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := client.Get(base + "/healthz")
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return stop
		}
	}
	stop()
	t.Fatalf("the daemon never answered /healthz:\n%s", readFile(t, logPath))
	return nil
}

// fetch makes one request and returns the status and the trimmed body; a body
// makes it a JSON POST, the only kind the API accepts.
func fetch(t *testing.T, client *http.Client, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

// accessOf is the daemon's own account of who may reach it.
func accessOf(t *testing.T, client *http.Client, base string) map[string]any {
	t.Helper()
	_, body := fetch(t, client, "GET", base+"/api/access", "")
	var access map[string]any
	if err := json.Unmarshal([]byte(body), &access); err != nil {
		t.Fatalf("GET /api/access: %v: %s", err, body)
	}
	return access
}

// readFile is a log the test wrote, as text.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The bcrypt hash the fixture install carries, named so the refusal's
// assertions can say it is still there rather than just that a row exists.
const authHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// dropIndex removes one index from a closed database and folds the WAL back, so
// the file the daemon opens is the shape a release without that index left.
func dropIndex(t *testing.T, path, index string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP INDEX IF EXISTS ` + index); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// settingInFile reads one settings row straight from a database the daemon has
// refused to open - the damage is in another table, and the question is whether
// what the install saved is still there.
func settingInFile(t *testing.T, path, key string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); err != nil {
		t.Fatalf("read %s from the refused database: %v", key, err)
	}
	return v
}
