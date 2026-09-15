package web

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// serverOn wires a Server over one store, the way newMetricsServer does over an
// in-memory one - but on a file-backed store, because these tests are about a
// store that had to be rebuilt from a file.
func serverOn(t *testing.T, st *store.Store) *Server {
	t.Helper()
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	status := func() LiveStatus {
		return LiveStatus{Online: true, Paused: true, Since: time.Unix(1_700_000_000, 0)}
	}
	return New(st, status, nil, set, nil, "v9.9.9", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// probe answers one unauthenticated request the way a load balancer's would.
func probe(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "example.com" // the probes answer any Host, from any peer
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// tearTableRoot trashes one table's b-tree root page in a closed database - a
// single bad sector: the file is still unmistakably a database, and the one
// table that no longer reads is enough to stop an Open.
func tearTableRoot(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var root int
	err = db.QueryRow(`SELECT rootpage FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&root)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ps := int(b[16])<<8 | int(b[17])
	if ps == 1 {
		ps = 65536
	}
	off := (root - 1) * ps
	if root < 2 || off+ps > len(b) {
		t.Fatalf("%s sits on page %d of a %d-byte file", table, root, len(b))
	}
	for i := 0; i < ps; i++ {
		b[off+i] = 0xFF
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A daemon running on a store it rebuilt after finding the database damaged is
// alive and monitoring, and /healthz says so - that is what liveness means, and
// failing it would restart the container the rebuild exists to keep running.
// But it is serving an empty install: the history and every saved setting are
// in the .corrupt file beside it. Readiness is the one automatic word anyone
// gets about that, so it must not say "ready".
func TestReadyzReportsAStoreRebuiltAfterCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(context.Background(), "thresh_down_mbps", "123.5"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")

	st2, err := store.Open(path, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("armed open: %v", err)
	}
	defer st2.Close()
	srv := serverOn(t, st2)

	if code, _ := probe(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: the process is up and monitoring, and a failed liveness probe restarts the very daemon the rebuild kept alive", code)
	}
	code, body := probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d, want 503: an alert on this daemon reports a healthy service over a dashboard with nothing on it, indefinitely", code)
	}
	if !strings.Contains(body, "corrupt") {
		t.Errorf("/readyz says %q, which does not tell whoever reads it what happened", strings.TrimSpace(body))
	}
}

// The store nothing happened to answers ready, as it always has. This is every
// install: no new flag, no new prompt, no new verdict.
func TestReadyzStillAnswersReadyOnAnOrdinaryStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := serverOn(t, st)
	if code, _ := probe(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	if code, body := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q, want 200", code, strings.TrimSpace(body))
	}
}

// The start after a rebuild opens a healthy store, so the store's own verdict is
// gone - but for an install asked for the network, the hold on it is not: every
// request from off the machine is refused for want of a login, and "ready" would
// send a load balancer's traffic into that and tell an alert all is well. So
// readiness answers 503 while the hold stands, and names the rebuild only when
// the hold was actually read: one that could not be read refuses the network
// just the same, but nothing says that store was ever rebuilt, and an operator
// who believed it would run reset-auth on a password that works. Network access
// switched on from this machine releases the hold, and readiness follows.
func TestReadyzReportsANetworkHeldBackByARebuiltStore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := serverOn(t, st)
	hold := AccessHeldAfterRebuild
	srv.AccessHold = func() AccessHold { return hold }

	code, body := probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "held") || !strings.Contains(body, "rebuilt") {
		t.Fatalf("/readyz = %d %q, want a 503 saying the network is held because the store was rebuilt: the alert that fired at the rebuild went quiet at the restart while the LAN still gets 403", code, strings.TrimSpace(body))
	}
	if !strings.Contains(body, "restart or reload") {
		t.Errorf("/readyz says %q, which does not say a reload ends the hold too: an operator who set a password restarts a daemon that only needed a SIGHUP", strings.TrimSpace(body))
	}
	if code, _ := probe(t, srv, "/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: the daemon is up and monitoring; a held network is no reason to restart it", code)
	}

	hold = AccessHeldUnread
	code, body = probe(t, srv, "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "held") {
		t.Errorf("/readyz = %d %q while the hold could not be read, want a 503 saying the network is held", code, strings.TrimSpace(body))
	}
	if strings.Contains(body, "rebuilt after") || strings.Contains(body, "reset-auth") {
		t.Errorf("/readyz claims a rebuild it never read, and points at reset-auth, which clears a working password: %q", strings.TrimSpace(body))
	}

	hold = AccessNotHeld
	if code, body := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q for an install nothing holds, want 200", code, strings.TrimSpace(body))
	}

	hold = AccessHeldAfterRebuild
	srv.holdReleased.Store(true)
	if code, body := probe(t, srv, "/readyz"); code != http.StatusOK {
		t.Errorf("/readyz = %d %q after network access was switched on from the machine, want 200: nothing is being held", code, strings.TrimSpace(body))
	}
}

// peerRequest makes one request through s's handler from peer, the TCP address
// the guard judges, and returns the status and the trimmed body; a body makes it
// a JSON POST.
func peerRequest(t *testing.T, s *Server, peer, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = peer
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

const (
	lanPeer      = "192.168.4.30:51515"
	loopbackPeer = "127.0.0.1:51515"
)

// While the hold a rebuilt store keeps stands in this process, the guard refuses
// every peer that is not this machine whatever the access setting says - a load
// can bring in a network setting the hold does not honour a moment before the
// hold is judged on it - and it tells that peer the way back that works here,
// not the -access network it was already given. Network access switched on from
// this machine ends the hold at once: the store's row goes, and the peer refused
// a moment ago is served.
func TestTheGuardRefusesTheNetworkWhileARebuiltStoreHoldsIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "p.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "thresh_down_mbps", "123.5"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	tearTableRoot(t, path, "pauses")
	st, err = store.Open(path, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("armed open: %v", err)
	}
	defer st.Close()
	srv := serverOn(t, st)
	var logged bytes.Buffer
	srv.log = slog.New(slog.NewTextHandler(&logged, nil))
	srv.AccessHold = func() AccessHold { return AccessHeldAfterRebuild }
	// A network setting in force that the hold does not honour.
	if err := srv.settings.SetAccessLocalOnly(ctx, false); err != nil {
		t.Fatal(err)
	}

	code, body := peerRequest(t, srv, lanPeer, "GET", "/api/status", "")
	if code != http.StatusForbidden {
		t.Fatalf("a LAN peer got %d from a store the hold keeps local, want 403: the network setting it does not honour opened it", code)
	}
	if !strings.Contains(body, "reset-auth") || !strings.Contains(body, "restart or reload") || strings.Contains(body, "start with -access network") {
		t.Errorf("the LAN peer is told %q: it has to name the way back that works on this store, not the flag it already passed", body)
	}
	if code, _ := peerRequest(t, srv, lanPeer, "POST", "/api/settings", `{"thresh_down_mbps":4242}`); code != http.StatusForbidden {
		t.Errorf("a LAN peer's unauthenticated settings write answered %d, want 403", code)
	}
	if all, _ := st.AllSettings(ctx); all["thresh_down_mbps"] == "4242" {
		t.Error("a LAN peer's settings write landed on a held store")
	}
	if _, body := peerRequest(t, srv, loopbackPeer, "GET", "/api/access", ""); !strings.Contains(body, `"local_only_active":true`) {
		t.Errorf("/api/access tells the machine the local-only filter is not in force while the hold enforces it: %s", body)
	}

	// A Save at the machine that changes something else and echoes the local-only
	// scope it was shown is not a network choice: the hold stays, in the store and
	// in this process, where the next start with -access network would otherwise
	// answer the LAN with no login. Nor is it a narrowing to announce - the hold
	// had narrowed it already - even with a reverse proxy declared.
	srv.AllowedHosts = []string{"dash.example.invalid"}
	code, body = peerRequest(t, srv, loopbackPeer, "POST", "/api/access", `{"username":"operator","local_only":true}`)
	if code != http.StatusOK {
		t.Fatalf("a username Save at the machine = %d %q", code, body)
	}
	if strings.Contains(body, "now limited") {
		t.Errorf("a Save that echoed the local-only scope in force was told access is now limited: %s", body)
	}
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Errorf("a Save at the machine that changed only the username released the hold (held=%v, err=%v)", held, err)
	}
	if code, _ := peerRequest(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
		t.Errorf("a LAN peer got %d after a username Save at the machine, want 403", code)
	}
	srv.AllowedHosts = nil
	if err := srv.settings.SetAccessLocalOnly(ctx, false); err != nil {
		t.Fatal(err)
	}

	// The Access tab, from the machine, switches network access on. The setting
	// already says network - a stored choice the hold refuses - but the tab was
	// shown local-only, and this is the change it asks for.
	if _, body := peerRequest(t, srv, loopbackPeer, "GET", "/api/access", ""); !strings.Contains(body, `"local_only":true`) {
		t.Errorf("the Access tab is shown a network scope the hold refuses: %s", body)
	}
	if code, body := peerRequest(t, srv, loopbackPeer, "POST", "/api/access", `{"local_only":false}`); code != http.StatusOK {
		t.Fatalf("POST /api/access from the machine = %d %q", code, body)
	}
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || held {
		t.Errorf("network access switched on at the machine left the hold in the store (held=%v, err=%v): the next load sets the operator's own choice back", held, err)
	}
	if code, body := peerRequest(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusOK {
		t.Errorf("a LAN peer got %d %q after network access was switched on at the machine, want 200: the hold still refuses what the operator opened", code, body)
	}
	// The one line that says the store's last protection went, and with it the
	// only record of it once nobody is reading the dashboard.
	if !strings.Contains(logged.String(), "is released") {
		t.Errorf("releasing the hold said nothing in the log:\n%s", logged.String())
	}
}

// A first-run store rebuilt after corruption offers Quick Setup again, from
// behind the hold. Answering it with network access is the Access tab's choice
// made through the first-run dialog, at the same machine, and ends the hold the
// same way; answering it local-only leaves the hold standing.
func TestQuickSetupAnsweredWithNetworkAccessReleasesTheHold(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localOnly bool
	}{
		{"network access", false},
		{"local-only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			// The row a rebuild writes into the store it builds.
			if err := st.SetSetting(ctx, "access_hold_after_rebuild", "1789350064"); err != nil {
				t.Fatal(err)
			}
			srv := serverOn(t, st)
			srv.AccessHold = func() AccessHold { return AccessHeldAfterRebuild }
			if err := srv.settings.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
				t.Fatal(err)
			}
			if !srv.quickSetupPending(ctx) {
				t.Fatal("fixture: the rebuilt first-run store is not offering Quick Setup")
			}
			answer := fmt.Sprintf(`{"speedtest_enabled":false,"update_check":false,"local_only":%v}`, tc.localOnly)
			if code, body := peerRequest(t, srv, loopbackPeer, "POST", "/api/quick-setup", answer); code != http.StatusOK {
				t.Fatalf("POST /api/quick-setup from the machine = %d %q", code, body)
			}
			held, err := st.AccessHoldAfterRebuild(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.localOnly && !held {
				t.Error("a local-only Quick Setup answer released the hold: nothing chose the network")
			}
			if !tc.localOnly && held {
				t.Error("a Quick Setup answer choosing network access at the machine left the hold standing: the choice the operator made at the machine stays refused")
			}
		})
	}
}

// A hold that could not be read refuses the network, but nobody found it, and
// what a refused network peer is told has to be what refused them. While the
// settings have not loaded either - the hold is read from them, so its read failed
// with theirs - that is the refusal every build gave before there was a hold to
// read: the settings could not be loaded, check the log. Told instead that
// local-only access is on and to start with -access network, an operator who had
// passed it went looking for a setting that was never the problem. Once the
// settings load and only the hold's own read failed, the peer is told that, and
// never that the database was rebuilt - unless a local-only setting refuses them
// on its own account.
func TestAHoldThatCouldNotBeReadTellsANetworkPeerWhatRefusedIt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Settings that never loaded, seeded with the network -access asked for.
	dead, cancel := context.WithCancel(ctx)
	cancel()
	unloaded, err := settings.New(dead, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 2, UpAfter: 1,
	})
	if err == nil || unloaded.Loaded() {
		t.Fatalf("fixture: the settings load must fail (err=%v)", err)
	}
	srv := New(st, func() LiveStatus { return LiveStatus{Online: true} }, nil, unloaded, nil, "v9.9.9",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.AccessHold = func() AccessHold { return AccessHeldUnread }
	for _, peer := range []string{lanPeer, loopbackPeer} {
		for _, q := range []struct{ method, path, body string }{
			{"GET", "/api/status", ""},
			{"POST", "/api/settings", `{"thresh_down_mbps":4242}`},
		} {
			code, body := peerRequest(t, srv, peer, q.method, q.path, q.body)
			if code != http.StatusServiceUnavailable || !strings.Contains(body, "settings could not be loaded") {
				t.Errorf("%s %s from %s = %d %q while neither the settings nor the hold could be read, want the 503 saying the settings could not be loaded", q.method, q.path, peer, code, body)
			}
		}
	}

	// The settings loaded with a network setting in force, and only the hold's
	// own read failed.
	srv = serverOn(t, st)
	srv.AccessHold = func() AccessHold { return AccessHeldUnread }
	code, body := peerRequest(t, srv, lanPeer, "GET", "/api/status", "")
	if code != http.StatusForbidden {
		t.Fatalf("a LAN peer got %d while the hold could not be read, want 403: the access decision failed open", code)
	}
	if !strings.Contains(body, "could not be read") {
		t.Errorf("a LAN peer refused because the hold could not be read is not told so: %q", body)
	}
	for _, untrue := range []string{"rebuilt", "local-only access is on", "-access network"} {
		if strings.Contains(body, untrue) {
			t.Errorf("a LAN peer refused because the hold could not be read is told %q, which is not what refused it: %q", untrue, body)
		}
	}
	// A local-only setting refuses on its own account, and says so as it always has.
	if err := srv.settings.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatal(err)
	}
	if _, body := peerRequest(t, srv, lanPeer, "GET", "/api/status", ""); !strings.Contains(body, "local-only access is on") {
		t.Errorf("a LAN peer refused by a local-only setting is no longer told so: %q", body)
	}
}

// A release that did not happen is not recorded as one. The Access tab's switch
// writes the store's network setting before it ends the hold, so if the hold's
// row will not go, the network setting has landed with the hold still standing
// in the store. Recorded as released anyway, this process would serve the
// network with no login while every later load held it again. The store refusing
// that one delete is the shape: a trigger here, a failing disk in the field.
func TestAHoldThatCouldNotBeReleasedStillRefusesTheNetwork(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The row a rebuild writes into the store it builds.
	if err := st.SetSetting(ctx, "access_hold_after_rebuild", "1789350064"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `CREATE TRIGGER the_hold_stays BEFORE DELETE ON settings
		WHEN OLD.key = 'access_hold_after_rebuild'
		BEGIN SELECT RAISE(ABORT, 'the hold row will not go'); END`); err != nil {
		t.Fatal(err)
	}
	srv := serverOn(t, st)
	var logged bytes.Buffer
	srv.log = slog.New(slog.NewTextHandler(&logged, nil))
	srv.AccessHold = func() AccessHold { return AccessHeldAfterRebuild }

	if code, body := peerRequest(t, srv, loopbackPeer, "POST", "/api/access", `{"local_only":false}`); code != http.StatusOK {
		t.Fatalf("POST /api/access from the machine = %d %q", code, body)
	}
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || !held {
		t.Fatalf("fixture: the hold row went anyway (held=%v, err=%v), so this proves nothing", held, err)
	}
	if code, _ := peerRequest(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
		t.Errorf("a LAN peer got %d after a release the store refused, want 403: this process serves the network with no login while the hold still stands in the store", code)
	}
	if code, body := probe(t, srv, "/readyz"); code != http.StatusServiceUnavailable || !strings.Contains(body, "held") {
		t.Errorf("/readyz = %d %q after a release the store refused, want a 503 saying the network is held", code, strings.TrimSpace(body))
	}
	if !strings.Contains(logged.String(), "could not be released") {
		t.Errorf("a release the store refused said nothing in the log:\n%s", logged.String())
	}
}

// The release says what the store is left with. Switched on in the same Save that
// set a login, the network has one behind it, and "the store has no login - set a
// password" is the wrong line for an operator who has just typed one.
func TestTheReleaseSaysWhetherALoginStandsBehindTheNetwork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		save  string
		login bool
	}{
		{"with no login", `{"local_only":false}`, false},
		{"with a login set in the same Save", `{"auth_enabled":true,"username":"operator","password":"a-new-password-1","local_only":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(filepath.Join(t.TempDir(), "p.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if err := st.SetSetting(ctx, "access_hold_after_rebuild", "1789350064"); err != nil {
				t.Fatal(err)
			}
			srv := serverOn(t, st)
			var logged bytes.Buffer
			srv.log = slog.New(slog.NewTextHandler(&logged, nil))
			srv.AccessHold = func() AccessHold { return AccessHeldAfterRebuild }
			if code, body := peerRequest(t, srv, loopbackPeer, "POST", "/api/access", tc.save); code != http.StatusOK {
				t.Fatalf("POST /api/access from the machine = %d %q", code, body)
			}
			got := logged.String()
			if !strings.Contains(got, "is released") {
				t.Fatalf("switching network access on at the machine released nothing it said:\n%s", got)
			}
			if says := strings.Contains(got, "has no login"); says == tc.login {
				t.Errorf("the release says the store has no login = %v, on a store whose login stands = %v:\n%s", says, tc.login, got)
			}
		})
	}
}
