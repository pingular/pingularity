package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/web"
)

// What the daemon hands the store is the operator's -on-corrupt choice and
// nothing else: with no choice made, a damaged database stops the start and
// stays where it is; with "rebuild", the same file is set aside and the daemon
// comes back on an empty store. This is the whole path from the flag to the
// file, so a wire crossed here is a wire crossed in production.
func TestOnlyAnExplicitRebuildChoiceArmsTheSetAside(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		rebuild bool
	}{
		{"no choice made", config.Config{}, false},
		{"refuse", config.Config{OnCorrupt: config.OnCorruptRefuse}, false},
		{"rebuild", config.Config{OnCorrupt: config.OnCorruptRebuild}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "pingularity.db")
			st, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := st.SetSetting(ctx, "thresh_down_mbps", "123.5"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			tearTableRoot(t, dbPath, "pauses")

			st2, err := store.Open(dbPath, openOptionsFor(tc.cfg)...)
			if err == nil {
				defer st2.Close()
			}
			aside, _ := filepath.Glob(dbPath + ".*.corrupt")
			switch {
			case tc.rebuild && err != nil:
				t.Fatalf("-on-corrupt rebuild did not reach the store: %v", err)
			case tc.rebuild && len(aside) == 0:
				t.Fatal("-on-corrupt rebuild reached the store but nothing was set aside")
			case !tc.rebuild && err == nil:
				t.Fatal("the damaged database was opened without anyone asking for the rebuild")
			case !tc.rebuild && len(aside) != 0:
				t.Fatalf("the damaged database was set aside without anyone asking (%v)", aside)
			}
		})
	}
}

// And the daemon's own start, which is where that choice is spent: a damaged
// database and no choice made stops it before it has a store, a listener or a
// worker - the file untouched behind it.
func TestStartRefusesADamagedDatabaseWhenNobodyAskedForARebuild(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()
	tearTableRoot(t, dbPath, "pauses")

	p := &program{cfg: config.Config{DBPath: dbPath}}
	if err := p.Start(nil); err == nil {
		p.store.Close()
		t.Fatal("the daemon started on a damaged database nobody asked it to replace")
	}
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) != 0 {
		t.Fatalf("the daemon set the damaged database aside unasked (%v)", matches)
	}
}

// An install that was reachable from the LAN behind a password, whose database
// is damaged, and whose access came from the command line rather than the
// store. The rebuild empties the store - the bcrypt hash goes with the file set
// aside - but the flag is still on the command line, so a boot re-seeds
// "network" over a store that now has nothing to check a visitor against: a box
// that answered 403 to the LAN yesterday serves it an unauthenticated
// dashboard, writes included. Not only at the start that rebuilt the store -
// that one knows - but at the next, which opens a healthy database and has
// nothing on the handle to say what happened, and at every one after. The flag
// loses all of them, and a reload in between changes nothing, until a login is
// set on the store.
func TestARebuiltStoreDoesNotComeBackOnTheNetworkWithoutItsLogin(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")

	// The install as it stood: a password, and network access chosen.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSettings(ctx, map[string]string{
		"auth_enabled":      "1",
		"auth_user":         "admin",
		"auth_hash":         "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
		"access_local_only": "0",
	}); err != nil {
		t.Fatalf("seed the install: %v", err)
	}
	st.Close()

	tearTableRoot(t, dbPath, "pauses")

	// Every start runs with the flags the unit or compose file has always had,
	// and does with them what run() does.
	flags := config.Config{DBPath: dbPath, Access: "network", AccessExplicit: true, OnCorrupt: config.OnCorruptRebuild}
	boot := func(st *store.Store) (*settings.Controller, config.Config, bool) {
		t.Helper()
		holdStands, err := st.AccessHoldAfterRebuild(ctx)
		if err != nil {
			t.Fatalf("read the access hold: %v", err)
		}
		cfg, held := holdAccessLocalAfterRebuild(flags, holdStands)
		set, err := settings.New(ctx, st, testDefaultsFor(cfg))
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		bootAccessDecision(t, cfg, st, set, false)
		return set, cfg, held
	}

	// The start that rebuilds.
	st2, err := store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("reopen after the fault: %v", err)
	}
	if matches, _ := filepath.Glob(dbPath + ".*.corrupt"); len(matches) == 0 {
		st2.Close()
		t.Fatal("fixture: the damaged database was not set aside")
	}
	set, _, held := boot(st2)
	if set.AuthActive() {
		st2.Close()
		t.Fatal("fixture: the rebuilt store still has a password, so this proves nothing")
	}
	if !held || !set.AccessLocalOnly() {
		st2.Close()
		t.Fatal("a store rebuilt without its password came back reachable from the LAN: every request from off the machine is served, unauthenticated, including the writes that change what this daemon measures")
	}
	st2.Close()

	// The restart after it: a healthy database, and the same empty store.
	st3, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	set, cfg, held := boot(st3)
	if !held || !set.AccessLocalOnly() {
		st3.Close()
		t.Fatal("the restart after a rebuild put the store on the LAN with no password behind it: the hold lasted one start, so the open dashboard only arrived one restart later")
	}
	// A reload - a reload signal, a restored backup - runs the access sequence
	// again on the config the boot settled on, and must not find the flag there.
	if err := set.Reload(ctx); err != nil {
		st3.Close()
		t.Fatalf("reload: %v", err)
	}
	if _, err := reconcileAccess(ctx, cfg, set); err != nil {
		st3.Close()
		t.Fatalf("reconcile after the reload: %v", err)
	}
	if !set.AccessLocalOnly() {
		st3.Close()
		t.Fatal("a reload opened the network the boot held back, over a store with no login")
	}

	// A login set from this machine, then a restart: the flag decides again, and
	// now there is a password behind what it decides.
	if err := set.SetAuthPassword(ctx, "admin", authHash); err != nil {
		st3.Close()
		t.Fatalf("set a password: %v", err)
	}
	st3.Close()
	st4, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("restart with a login: %v", err)
	}
	defer st4.Close()
	set, _, held = boot(st4)
	if !set.AuthActive() {
		t.Fatal("fixture: the login did not survive the restart")
	}
	if held || set.AccessLocalOnly() {
		t.Fatal("a store with a login set on it is still held to this machine: the operator did what the warning asked and the network never came back")
	}
}

// The way back for an install that cannot reach its own loopback address to set
// a password - a bridged container with no shell, whose published port the hold
// refuses like any other peer - is `pingularity reset-auth` against the
// database. It releases the hold and says so, and the next start is an unclaimed
// install on the network, which is what running it asked for. On a store with
// no hold it says nothing about one.
func TestResetAuthReleasesTheHoldOnARebuiltStore(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close()
	tearTableRoot(t, dbPath, "pauses")
	st, err = store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	st.Close()

	out := runResetAuth(t, dbPath)
	if !strings.Contains(out, "hold is released") {
		t.Errorf("reset-auth released nothing it said, or said nothing of what it released - the operator does not learn that the next start answers the network with no login:\n%s", out)
	}

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer st.Close()
	holdStands, err := st.AccessHoldAfterRebuild(ctx)
	if err != nil || holdStands {
		t.Fatalf("the hold survived reset-auth (held=%v, err=%v): a bridged container stays off its own published port with no route left to a password", holdStands, err)
	}
	cfg, held := holdAccessLocalAfterRebuild(config.Config{DBPath: dbPath, Access: "network", AccessExplicit: true}, holdStands)
	set, err := settings.New(ctx, st, testDefaultsFor(cfg))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	bootAccessDecision(t, cfg, st, set, false)
	if held || set.AccessLocalOnly() {
		t.Fatal("after reset-auth released the hold, -access network still did not open the network")
	}

	if out := runResetAuth(t, dbPath); strings.Contains(out, "hold") {
		t.Errorf("reset-auth on a store with no hold talks about one:\n%s", out)
	}
}

// runResetAuth runs the reset-auth command against dbPath and returns what it
// printed, the way an operator reads it.
func runResetAuth(t *testing.T, dbPath string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "reset-auth")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	orig := os.Stdout
	os.Stdout = f
	err = resetAuthCmd([]string{"-db", dbPath})
	os.Stdout = orig
	if err != nil {
		t.Fatalf("reset-auth: %v", err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Network access the operator switches on from this machine's own Access tab is
// not the flag: it is chosen at the machine, where the login would have been
// set, the same boundary reset-auth stands on - and it ends the hold, in this
// process at once and at every start after. It has to END it rather than win
// over it, because a network choice found stored while the hold stands is not
// honoured (see the rollback test below), and the store can only tell this
// build's own choice apart by the hold being gone.
func TestNetworkAccessSwitchedOnAtTheMachineReleasesTheHold(t *testing.T) {
	ctx := context.Background()
	dbPath := rebuiltDatabase(t)
	flags := config.Config{DBPath: dbPath, Access: "network", AccessExplicit: true, OnCorrupt: config.OnCorruptRebuild}

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	p, set, heldWarns := bootLikeRun(t, flags, st)
	srv := serverLikeRun(p, st, set)
	if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
		st.Close()
		t.Fatalf("fixture: the held store answered a LAN peer %d before anything was released", code)
	}
	// Nothing about access is stored on this store, so the boot must not say a
	// stored network choice is being refused: the flag is seeded no further than
	// the hold allows, and the warning names only what is really there.
	for _, w := range heldWarns {
		if strings.Contains(fmt.Sprint(w.args...), "stored_network_access") {
			st.Close()
			t.Fatalf("the held boot of a store with no stored access choice warns that one is not honoured: %s %v", w.msg, w.args)
		}
	}
	// Holding needs no stored choice: the boot seeds loopback rather than seed
	// the flag's network and store a correction over it.
	if got, stored := settingsSnapshot(t, st)["access_local_only"]; stored {
		st.Close()
		t.Fatalf("the held boot stored an access choice (access_local_only = %q) it had no need to store", got)
	}
	// The Access tab, from the machine: switch network access on, no login.
	if code, body := asPeer(t, srv, loopbackPeer, "POST", "/api/access", `{"local_only":false}`); code != http.StatusOK {
		st.Close()
		t.Fatalf("POST /api/access from the machine = %d %q", code, body)
	}
	if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusOK {
		st.Close()
		t.Fatalf("network access switched on at the machine still refuses the LAN (%d) in the process that switched it on", code)
	}
	if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || held {
		st.Close()
		t.Fatalf("switching network access on at the machine left the hold in the store (held=%v, err=%v): the next start sets the operator's own choice back to local-only", held, err)
	}
	st.Close()

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer st.Close()
	p, set, warns := bootLikeRun(t, flags, st)
	if set.AccessLocalOnly() || web.AccessHold(p.accessHold.Load()) != web.AccessNotHeld {
		t.Fatal("the start after the operator switched network access on at the machine took it back")
	}
	for _, w := range warns {
		if strings.Contains(w.msg, "access held to this machine") {
			t.Errorf("a start serving the network the operator chose still warns that it is holding it: %s", w.msg)
		}
	}
}

// The rollback. A store rebuilt by this build, run back on an older release that
// does not know the hold: with -access network that release answers the network
// with no login, as the release notes say, and one ordinary Save - which posts
// the scope the drawer shows, and which any visitor to that open dashboard can
// send - stores network access beside the hold. Upgraded again with the same
// flags, that stored choice used to open the store for good, with no login, no
// warning and /readyz 200. It was not made on this build while the hold stood
// (that would have released it), so it is not honoured: the hold stands, the
// Access tab is shown the local-only scope in force, and the warning says what
// happened. It is not overwritten at boot - main stores an access decision only
// from explicit input - and the operator's own next Save stores local-only.
func TestARebuiltStoreDoesNotTakeNetworkAccessARollbackStored(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags config.Config
	}{
		{"under -access network", config.Config{Access: "network", AccessExplicit: true, OnCorrupt: config.OnCorruptRebuild}},
		// A unit that passes no -access at all: the stored choice is all that
		// would open it.
		{"with no -access at all", config.Config{OnCorrupt: config.OnCorruptRebuild}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := rebuiltDatabase(t)
			flags := tc.flags
			flags.DBPath = dbPath

			// The older release's Save, as it lands in the file.
			st, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetSetting(ctx, "access_local_only", "0"); err != nil {
				t.Fatal(err)
			}
			st.Close()

			st, err = store.Open(dbPath)
			if err != nil {
				t.Fatalf("re-upgrade: %v", err)
			}
			defer st.Close()
			p, set, warns := bootLikeRun(t, flags, st)
			if got := web.AccessHold(p.accessHold.Load()); got != web.AccessHeldAfterRebuild {
				t.Fatalf("the re-upgraded start records the hold as %d, want held after rebuild: the network access an older release stored beside the hold is in force, with no login behind it", got)
			}
			if got := settingsSnapshot(t, st)["access_local_only"]; got != "0" {
				t.Errorf("the boot wrote the stored access scope (access_local_only = %q): main persists access only from explicit input", got)
			}
			said := false
			for _, w := range warns {
				if strings.Contains(w.msg, "access held to this machine") && strings.Contains(fmt.Sprint(w.args...), "stored_network_access") {
					said = true
				}
			}
			if !said {
				t.Errorf("nothing said the stored network access was set back and why; warnings: %v", warns)
			}

			srv := serverLikeRun(p, st, set)
			if code, _ := asPeer(t, srv, lanPeer, "GET", "/", ""); code != http.StatusForbidden {
				t.Errorf("a LAN peer got %d from GET / on the re-upgraded store, want 403", code)
			}
			// The machine's own Access tab is shown the scope in force, so a Save
			// there echoes local-only and stores it.
			if _, body := asPeer(t, srv, loopbackPeer, "GET", "/api/access", ""); !strings.Contains(body, `"local_only":true`) {
				t.Errorf("the Access tab is shown network access the hold refuses: %s", body)
			}
			if code, body := asPeer(t, srv, loopbackPeer, "POST", "/api/access", `{"local_only":true}`); code != http.StatusOK {
				t.Fatalf("a Save at the machine = %d %q", code, body)
			}
			if got := settingsSnapshot(t, st)["access_local_only"]; got != "1" {
				t.Errorf("the operator's Save at the machine did not store the local-only scope it was shown (access_local_only = %q)", got)
			}
			if held, err := st.AccessHoldAfterRebuild(ctx); err != nil || !held {
				t.Errorf("a Save echoing local-only released the hold (held=%v, err=%v): an echo is not a choice", held, err)
			}
			if code, _ := asPeer(t, srv, lanPeer, "POST", "/api/settings", `{"thresh_down_mbps":4242}`); code != http.StatusForbidden {
				t.Errorf("a LAN peer's unauthenticated POST /api/settings answered %d, want 403", code)
			}
			if got := settingsSnapshot(t, st)["thresh_down_mbps"]; got == "4242" {
				t.Error("a LAN peer's unauthenticated settings write landed on the re-upgraded store")
			}
			if code, body := asPeer(t, srv, lanPeer, "GET", "/readyz", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "held") {
				t.Errorf("/readyz on the re-upgraded store = %d %q, want a 503 saying the network is held", code, body)
			}

			// And a reload that pulls a stored network choice in under the
			// standing hold - a restore, a reload signal after an edit - gets the
			// same answer, and says so again.
			logged := &lockedLog{}
			p.log = slog.New(slog.NewTextHandler(logged, nil))
			if err := st.SetSetting(ctx, "access_local_only", "0"); err != nil {
				t.Fatal(err)
			}
			if err := set.Reload(ctx); err != nil {
				t.Fatalf("reload: %v", err)
			}
			if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
				t.Errorf("a reload that pulled in a stored network choice under the hold put it in force: a LAN peer got %d", code)
			}
			if got := web.AccessHold(p.accessHold.Load()); got != web.AccessHeldAfterRebuild {
				t.Errorf("the reload records the hold as %d, want held after rebuild", got)
			}
		})
	}
}

// And the transient read. A healthy install - a login, and network access that
// has only ever come from PINGULARITY_ACCESS, so nothing about access is stored -
// whose settings could not be read for a moment at boot. Whether the store
// carries the hold could not be read either, and the network is held for that
// moment, failing closed. But the store was never rebuilt: the load that finally
// reads its settings must give the flag its network back, store nothing the
// flag never stored, and never have said the store was rebuilt - an operator who
// believed that and ran reset-auth would clear a working password and open the
// LAN with no login at all. The same load on a store that does carry the hold
// holds it.
func TestAHoldThatCouldNotBeReadAtBootIsJudgedWhenSettingsLoad(t *testing.T) {
	ctx := context.Background()
	dead, cancel := context.WithCancel(ctx)
	cancel()
	flags := config.Config{Access: "network", AccessExplicit: true}

	t.Run("a healthy install", func(t *testing.T) {
		flags := flags
		flags.DBPath = filepath.Join(t.TempDir(), "pingularity.db")
		st, err := store.Open(flags.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.SetSettings(ctx, map[string]string{"auth_enabled": "1", "auth_user": "admin", "auth_hash": authHash}); err != nil {
			t.Fatal(err)
		}
		p, set, srv, logs := bootWithUnreadableSettings(t, flags, st, dead)
		// Neither could be read, and a network peer is told what every build said
		// for that before there was a hold to read - not that local-only access is
		// on, and not to pass the -access network the install was started with.
		if code, body := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusServiceUnavailable || !strings.Contains(body, "settings could not be loaded") {
			t.Fatalf("a LAN peer got %d %q while neither the settings nor the hold could be read, want the 503 saying the settings could not be loaded", code, body)
		}
		if err := set.Reload(ctx); err != nil {
			t.Fatalf("the settings load that recovers: %v", err)
		}
		if got := web.AccessHold(p.accessHold.Load()); got != web.AccessNotHeld {
			t.Fatalf("the load that read the settings still records a hold (%d) on a store that carries none: the LAN stays refused for the life of the process", got)
		}
		if set.AccessLocalOnly() {
			t.Error("PINGULARITY_ACCESS=network is not in force once the settings read: the install's own network is still refused")
		}
		if _, stored := settingsSnapshot(t, st)["access_local_only"]; stored {
			t.Error("the recovering load stored an access choice the flag never needed stored; dropping the flag later would no longer close the network")
		}
		// Unauthenticated from the LAN: the login answers now, not the hold.
		if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusUnauthorized {
			t.Errorf("a LAN peer with no credentials got %d, want 401 from the login - the network is back and the password stands behind it", code)
		}
		if code, body := asPeer(t, srv, loopbackPeer, "GET", "/readyz", ""); code != http.StatusOK {
			t.Errorf("/readyz = %d %q once the settings read on a store nothing holds, want 200", code, body)
		}
		if got := logs(); !strings.Contains(got, "could not read or release the access hold") {
			t.Errorf("the boot that could not read the hold never said so:\n%s", got)
		} else if strings.Contains(got, "rebuilt after the database was found damaged") {
			t.Errorf("a store that was never rebuilt was said to be:\n%s", got)
		} else if !strings.Contains(got, "nothing holds network access back") {
			t.Errorf("the load that read the hold never said the network it had held is back:\n%s", got)
		}
	})

	// A load after a healthy boot whose own read of the hold fails: the network
	// is held for that load, nothing is written on the guess, and nothing claims
	// a rebuild; the next load that reads it gives the network back.
	t.Run("a later load that cannot read the hold", func(t *testing.T) {
		flags := flags
		flags.DBPath = filepath.Join(t.TempDir(), "pingularity.db")
		st, err := store.Open(flags.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if err := st.SetSettings(ctx, map[string]string{"auth_enabled": "1", "auth_user": "admin", "auth_hash": authHash}); err != nil {
			t.Fatal(err)
		}
		p, set, _ := bootLikeRun(t, flags, st)
		srv := serverLikeRun(p, st, set)
		if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusUnauthorized {
			t.Fatalf("fixture: the healthy boot answered a LAN peer %d, want 401 from its login", code)
		}
		before := fmt.Sprint(settingsSnapshot(t, st))
		var said []warnCall
		sink := func(msg string, args ...any) { said = append(said, warnCall{msg: msg, args: args}) }
		p.applyExplicitAccess(dead, set, false, sink, sink)
		if got := p.currentAccessHold(); got != web.AccessHeldUnread {
			t.Fatalf("a load that could not read the hold records it as %d, want unread", got)
		}
		if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
			t.Errorf("a LAN peer got %d while the hold could not be read, want 403: the access decision failed open", code)
		}
		if code, body := asPeer(t, srv, lanPeer, "GET", "/readyz", ""); code != http.StatusServiceUnavailable || strings.Contains(body, "rebuilt after") {
			t.Errorf("/readyz = %d %q while the hold could not be read, want a 503 that claims no rebuild", code, body)
		}
		if after := fmt.Sprint(settingsSnapshot(t, st)); after != before {
			t.Errorf("a load that could not read the hold wrote to the store on the guess:\nbefore %s\nafter  %s", before, after)
		}
		if fmt.Sprint(said) == "[]" {
			t.Error("a load that could not read the hold said nothing about it")
		}
		// The flag is what that load withholds: the config the rest of the
		// sequence runs with does not carry it.
		if cfg := p.judgeAccessHold(dead, set, sink, sink); cfg.Access == "network" || cfg.AccessExplicit {
			t.Errorf("a load that could not read the hold hands the rest of the access sequence -access network to reconcile (%+v)", cfg)
		}
		// And while local-only is chosen at the machine: -access network is not
		// reconciled over that choice on the guess either.
		if err := set.SetAccessLocalOnly(ctx, true); err != nil {
			t.Fatal(err)
		}
		before = fmt.Sprint(settingsSnapshot(t, st))
		p.applyExplicitAccess(dead, set, false, sink, sink)
		if after := fmt.Sprint(settingsSnapshot(t, st)); after != before {
			t.Errorf("a load that could not read the hold reconciled -access network over the stored choice on a guess:\nbefore %s\nafter  %s", before, after)
		}
		p.applyExplicitAccess(ctx, set, false, sink, sink)
		if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusUnauthorized {
			t.Errorf("a LAN peer got %d once the hold read again, want 401: the network did not come back", code)
		}
	})

	t.Run("a rebuilt store", func(t *testing.T) {
		flags := flags
		flags.DBPath = rebuiltDatabase(t)
		st, err := store.Open(flags.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		p, set, srv, logs := bootWithUnreadableSettings(t, flags, st, dead)
		if err := set.Reload(ctx); err != nil {
			t.Fatalf("the settings load that recovers: %v", err)
		}
		if got := web.AccessHold(p.accessHold.Load()); got != web.AccessHeldAfterRebuild {
			t.Fatalf("the load that read a rebuilt store records the hold as %d, want held after rebuild", got)
		}
		// The scope in force is local-only, whatever the flag seeded: the hold is
		// enforced where requests are judged, and nothing was stored on the
		// guess the unreadable boot could have made.
		if _, body := asPeer(t, srv, loopbackPeer, "GET", "/api/access", ""); !strings.Contains(body, `"local_only":true`) {
			t.Errorf("the load that read a rebuilt store put PINGULARITY_ACCESS=network in force over it: %s", body)
		}
		if _, stored := settingsSnapshot(t, st)["access_local_only"]; stored {
			t.Error("the load that read a rebuilt store stored an access choice")
		}
		if code, _ := asPeer(t, srv, lanPeer, "GET", "/api/status", ""); code != http.StatusForbidden {
			t.Errorf("a LAN peer got %d from the rebuilt store once its settings read, want 403", code)
		}
		if !strings.Contains(logs(), "access held to this machine") {
			t.Error("the load that found the hold standing never said why the network is refused")
		}
		// And it names nothing it did not find: the network in force is the flag's
		// seed, and no network choice is stored on this store.
		if strings.Contains(logs(), "stored_network_access") {
			t.Errorf("the load that found the hold standing says a stored network choice is not honoured, on a store with none stored:\n%s", logs())
		}
	})
}

// The hold read standing at boot is in force from that moment, not only from the
// first load that judges it. A boot whose settings then fail to load serves on
// the seed until a load recovers them, and that load puts the stored values in
// force - and counts the settings loaded - a moment before the hook that judges
// the hold runs on them. A store an older release was rolled back to can carry
// network access beside the hold, and with nothing recorded as held in that
// moment a network peer was served with no login: it could change the settings,
// or claim the dashboard with a password of its own. The load that recovers still
// says why the network is held - the seed found the hold, but it is not a load.
func TestARebuiltStoreStaysHeldThroughTheLoadThatRecoversItsSettings(t *testing.T) {
	ctx := context.Background()
	dead, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name  string
		flags config.Config
	}{
		{"under -access network", config.Config{Access: "network", AccessExplicit: true, OnCorrupt: config.OnCorruptRebuild}},
		{"with no -access at all", config.Config{OnCorrupt: config.OnCorruptRebuild}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := rebuiltDatabase(t)
			flags := tc.flags
			flags.DBPath = dbPath
			st, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			// The older release's Save, as it lands in the file.
			if err := st.SetSetting(ctx, "access_local_only", "0"); err != nil {
				t.Fatal(err)
			}

			// The hold reads; the settings load straight after it does not.
			logged := &lockedLog{}
			p := &program{cfg: flags, store: st, log: slog.New(slog.NewTextHandler(logged, nil))}
			seed := p.accessSeedAtBoot(ctx, func(msg string, args ...any) { p.log.Warn(msg, args...) })
			set, err := settings.New(dead, st, testDefaultsFor(seed))
			if err == nil {
				t.Fatal("fixture: the settings load at boot must fail")
			}
			srv := serverLikeRun(p, st, set)

			// registerSettingsLoadedHook's own sequence, with a network peer's
			// requests landing as it starts: the stored network access is in force,
			// the settings count as loaded, and nothing has judged the hold on them.
			var served []string
			set.OnLoaded(func(firstLoad bool) {
				for _, q := range []struct{ method, path, body string }{
					{"GET", "/api/status", ""},
					{"POST", "/api/settings", `{"thresh_down_mbps":4242}`},
					{"POST", "/api/access", `{"auth_enabled":true,"username":"visitor","password":"not-the-operators-1","local_only":false}`},
				} {
					if code, _ := asPeer(t, srv, lanPeer, q.method, q.path, q.body); code != http.StatusForbidden {
						served = append(served, fmt.Sprintf("%s %s answered %d", q.method, q.path, code))
					}
				}
				p.applyExplicitAccess(ctx, set, firstLoad,
					func(msg string, args ...any) { p.log.Warn(msg, args...) },
					func(msg string, args ...any) { p.log.Info(msg, args...) })
			})
			if err := set.Reload(ctx); err != nil {
				t.Fatalf("the settings load that recovers: %v", err)
			}
			if !set.Loaded() || set.AccessLocalOnly() {
				t.Fatal("fixture: the recovering load did not put the stored network access in force, so this proves nothing")
			}
			if len(served) != 0 {
				t.Fatalf("a network peer was served by a rebuilt store with no login in the moment its settings recovered: %v", served)
			}
			if all := settingsSnapshot(t, st); all["thresh_down_mbps"] == "4242" || all["auth_user"] == "visitor" {
				t.Errorf("a network peer's write landed on the rebuilt store while its settings recovered: thresh_down_mbps=%q auth_user=%q", all["thresh_down_mbps"], all["auth_user"])
			}
			if got := web.AccessHold(p.accessHold.Load()); got != web.AccessHeldAfterRebuild {
				t.Errorf("the recovering load records the hold as %d, want held after rebuild", got)
			}
			if n := strings.Count(logged.String(), "access held to this machine"); n != 1 {
				t.Errorf("the load that recovered the settings said why the network is held %d times, want once:\n%s", n, logged.String())
			}
		})
	}
}

// The warning that says why the network is held comes with each load that finds
// the hold standing after one that did not - not with every reload of a store
// already held, where it would bury the line that mattered, and not with the
// boot's seed, which finds the hold before any load has judged it. The warning
// that the hold could not be read comes once for each time that starts.
func TestTheHoldWarningsComeOnceForEachChange(t *testing.T) {
	ctx := context.Background()
	dead, cancel := context.WithCancel(ctx)
	cancel()
	dbPath := rebuiltDatabase(t)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The rollback's Save: network access stored beside the hold, and no -access
	// at all, so the stored choice is the only thing asking for the network.
	if err := st.SetSetting(ctx, "access_local_only", "0"); err != nil {
		t.Fatal(err)
	}
	p, set, warns := bootLikeRun(t, config.Config{DBPath: dbPath, OnCorrupt: config.OnCorruptRebuild}, st)
	logged := &lockedLog{}
	p.log = slog.New(slog.NewTextHandler(logged, nil))
	sink := func(msg string, args ...any) { p.log.Warn(msg, args...) }
	said := func(what string, wantHeld, wantUnread int) {
		t.Helper()
		held := strings.Count(logged.String(), "access held to this machine")
		for _, w := range warns {
			if strings.Contains(w.msg, "access held to this machine") {
				held++
			}
		}
		if held != wantHeld {
			t.Errorf("after %s the held warning has been given %d times, want %d:\n%s", what, held, wantHeld, logged.String())
		}
		if unread := strings.Count(logged.String(), "could not read or release the access hold"); unread != wantUnread {
			t.Errorf("after %s the unread warning has been given %d times, want %d:\n%s", what, unread, wantUnread, logged.String())
		}
	}
	said("the boot", 1, 0)
	if err := set.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	said("a reload signal on the held store", 1, 0)
	p.applyExplicitAccess(dead, set, false, sink, sink)
	p.applyExplicitAccess(dead, set, false, sink, sink)
	said("two loads that could not read the hold", 1, 1)
	if err := set.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	said("the load that read it standing again", 2, 1)
	// Local-only stored at the machine: nothing asks for the network, so there is
	// nothing to hold back...
	if err := set.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := set.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if got := p.currentAccessHold(); got != web.AccessNotHeld {
		t.Fatalf("fixture: with local-only stored and no -access the load records the hold as %d, want not held", got)
	}
	// ...until a restored backup brings network access back in under the hold.
	if err := st.SetSetting(ctx, "access_local_only", "0"); err != nil {
		t.Fatal(err)
	}
	if err := set.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	said("a load that found the hold standing after one that held nothing back", 3, 1)
}

// And the other direction, which is every ordinary start: a healthy store keeps
// the access the operator asked for. The flag is a container's way back onto its
// own published port, and it must not be spent on installs that never had a
// fault.
func TestAHealthyStoreKeepsTheAccessTheFlagAsksFor(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.SetSetting(ctx, "access_local_only", "1"); err != nil {
		t.Fatalf("seed a stored local-only: %v", err)
	}

	holdStands, err := st.AccessHoldAfterRebuild(ctx)
	if err != nil {
		t.Fatalf("read the access hold: %v", err)
	}
	cfg := config.Config{DBPath: dbPath, Access: "network", AccessExplicit: true}
	cfg, held := holdAccessLocalAfterRebuild(cfg, holdStands)
	if held {
		t.Fatal("a healthy store had its access held to this machine: an install with no fault just lost the network")
	}

	set, err := settings.New(ctx, st, testDefaultsFor(cfg))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	bootAccessDecision(t, cfg, st, set, false)
	if set.AccessLocalOnly() {
		t.Fatal("an explicit -access network no longer overrides a stored local-only, so a container that persisted local-only has no way back onto its published port")
	}
}

// Wherever a document states the access rule - an explicitly passed -access /
// PINGULARITY_ACCESS wins at every start, which is how a container gets back
// onto its own published port - it has to name the store where it does not,
// and the way back from it. That store is the dangerous direction: an operator
// who reads the unqualified rule expects the flag to have decided, and what it
// would have decided is a dashboard on the LAN with no password behind it; one
// who reads of the hold but not of reset-auth has a bridged container that 403s
// its own port and no route to a password. A sentence that promises what the
// code no longer does is how the hold gets deleted by someone tidying up an
// inconsistency. docs/cli.md states the rule in a table row, so rows are read
// one at a time - the table as one paragraph would find the exception in the
// -on-corrupt row and excuse an -access row that never names it.
func TestTheDocsThatPromiseTheAccessFlagAlwaysWinsNameTheStoreItDoesNot(t *testing.T) {
	for _, doc := range []string{filepath.Join("docs", "install.md"), filepath.Join("docs", "security-model.md"), filepath.Join("docs", "cli.md")} {
		stated := 0
		for _, unit := range docUnits(mustReadRepoFile(t, doc)) {
			flat := flatDoc(unit)
			if !strings.Contains(flat, "authoritative at every start") && !strings.Contains(flat, "re-asserts itself at every start") {
				continue
			}
			stated++
			for want, missing := range map[string]string{
				"on-corrupt":  "the store it is not - one rebuilt after the database was found damaged, which holds the network back because the password went with the old file",
				"reset-auth":  "reset-auth, the way back for a bridged container the hold keeps off its own published port",
				"switched on": "network access switched on at the machine, the one network choice the hold gives way to",
			} {
				if !strings.Contains(flat, want) {
					t.Errorf("%s tells an operator an explicit access mode wins at every start and never names %s:\n%s", doc, missing, flat)
				}
			}
		}
		if stated == 0 {
			t.Errorf("%s no longer states the every-start access rule this test qualifies; if the rule moved, move the exception with it", doc)
		}
	}
}

// What a reader of the corruption notes and of the readiness docs acts on, held
// to what the daemon does: the rebuilt store is loopback-only even under -access
// network, restart after restart; reset-auth is how a container gets out of it;
// and /readyz answers 503 while it lasts. Each of these sentences has been
// deleted or reverted once with every test still green.
func TestTheDocsSayWhatARebuiltStoreDoes(t *testing.T) {
	readme := flatDoc(mustReadRepoFile(t, "docs/install.md"))
	i := strings.Index(readme, "`-on-corrupt rebuild` takes the other road")
	if i < 0 {
		t.Fatal("README.md's corruption notes have no `-on-corrupt rebuild` entry")
	}
	entry := readme[i:]
	if j := strings.Index(entry, "What the daemon will *not* do"); j >= 0 {
		entry = entry[:j]
	}
	for want, what := range map[string]string{
		"loopback-only even if you passed `-access network`": "that the rebuilt store refuses the network the flag asks for",
		"every start": "that the hold outlives the start that rebuilt the store",
		"reset-auth":  "the way back for a bridged container",
		"/readyz":     "that readiness reports it",
		"Network access on there yourself releases the hold": "that switching network access on at the machine releases the hold - the one network choice it gives way to",
		"a network setting stored by anything else does not": "that a network setting a rolled-back release stored does not open the store",
		"then restart or reload":                             "that a new password takes effect once the daemon restarts or reloads",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("README.md's -on-corrupt rebuild entry no longer says %s (%q):\n%s", what, want, entry)
		}
	}

	metrics := flatDoc(mustReadRepoFile(t, filepath.Join("docs", "metrics.md")))
	i = strings.Index(metrics, "`GET /readyz`")
	if i < 0 {
		t.Fatal("docs/metrics.md no longer documents /readyz")
	}
	readyz := metrics[i:]
	if j := strings.Index(readyz, "`pingularity healthz"); j >= 0 {
		readyz = readyz[:j]
	}
	for want, what := range map[string]string{
		"on-corrupt rebuild":                   "that a store rebuilt after damage answers 503",
		"reset-auth":                           "what ends the 503 a held network keeps answering",
		"switched on from the machine itself":  "that network access switched on at the machine ends it too",
		"and the daemon restarted or reloaded": "that a password set on the store ends the hold once the daemon restarts or reloads",
		"without claiming a rebuild":           "that a hold that could not be read answers 503 without saying the store was rebuilt",
	} {
		if !strings.Contains(readyz, want) {
			t.Errorf("docs/metrics.md's /readyz entry no longer says %s (%q):\n%s", what, want, readyz)
		}
	}

	// The flag's own two descriptions - the docs/cli.md row and `pingularity
	// help` - are where an operator choosing -on-corrupt rebuild reads what it
	// costs, so both have to say the hold outlasts the start and what ends it.
	var row string
	for _, u := range docUnits(mustReadRepoFile(t, filepath.Join("docs", "cli.md"))) {
		if strings.HasPrefix(u, "| `-on-corrupt` |") {
			row = flatDoc(u)
		}
	}
	help, _ := usageEntry(captureUsage(t), "-on-corrupt")
	for name, text := range map[string]string{"docs/cli.md's -on-corrupt row": row, "the curated help's -on-corrupt entry": flatDoc(help)} {
		if !strings.Contains(text, "reset-auth") || !strings.Contains(text, "until a password is set") {
			t.Errorf("%s no longer says the rebuilt store stays loopback-only until a password is set on it or reset-auth releases it, so choosing the rebuild reads as costing one start:\n%s", name, text)
		}
		for want, what := range map[string]string{
			"switched on from the machine itself":  "that network access switched on at the machine releases the hold too",
			"and the daemon restarted or reloaded": "that a password set on the rebuilt store ends the hold once the daemon restarts or reloads",
			"the copy lists":                       "that the recovered copy is checked for the install's settings before it replaces anything - a torn first page can leave nothing, and put in place that is a brand-new install with no login",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s no longer says %s (%q):\n%s", name, what, want, text)
			}
		}
	}
	// Where the flag says what the hold overrides, it has to say it overrides the
	// flag: an operator choosing -on-corrupt rebuild in a unit that also passes
	// -access network reads here whether the two can open the store together.
	if !strings.Contains(flatDoc(help), "whatever -access says") {
		t.Errorf("the curated help's -on-corrupt entry no longer says the rebuilt store stays loopback-only whatever -access says:\n%s", help)
	}
	if !strings.Contains(row, "even under `-access network`") {
		t.Errorf("docs/cli.md's -on-corrupt row no longer says the rebuilt store stays loopback-only even under -access network:\n%s", row)
	}

	// The other places that say what ends the hold carry the same clause: a
	// password set on the store counts only once the daemon restarts.
	cli := mustReadRepoFile(t, filepath.Join("docs", "cli.md"))
	var accessRow, resetAuthPara string
	for _, u := range docUnits(cli) {
		switch {
		case strings.HasPrefix(u, "| `-access` |"):
			accessRow = flatDoc(u)
		case strings.Contains(flatDoc(u), "It also releases the hold"):
			resetAuthPara = flatDoc(u)
		}
	}
	accessNotes := readme
	if k := strings.Index(accessNotes, "One store does not honour it"); k >= 0 {
		accessNotes = accessNotes[k:]
		if e := strings.Index(accessNotes, "The flip side"); e >= 0 {
			accessNotes = accessNotes[:e]
		}
	} else {
		accessNotes = ""
	}
	resetAuthNote := readme
	if k := strings.Index(resetAuthNote, "also the way back onto the network for a container"); k >= 0 {
		resetAuthNote = resetAuthNote[k:]
		if e := strings.Index(resetAuthNote, "so claim it straight away"); e >= 0 {
			resetAuthNote = resetAuthNote[:e]
		}
	} else {
		resetAuthNote = ""
	}
	containerNote := readme
	if k := strings.Index(containerNote, "bar a store rebuilt from a damaged database on request"); k >= 0 {
		containerNote = containerNote[k:]
		if e := strings.Index(containerNote, "It does not write the choice"); e >= 0 {
			containerNote = containerNote[:e]
		}
	} else {
		containerNote = ""
	}
	for name, text := range map[string]string{
		"docs/cli.md's -access row":          accessRow,
		"docs/cli.md's reset-auth paragraph": resetAuthPara,
		"README.md's -access notes":          accessNotes,
		"README.md's container upgrade note": containerNote,
		"README.md's reset-auth paragraph":   resetAuthNote,
	} {
		if !strings.Contains(text, "and the daemon restarted or reloaded") && !strings.Contains(text, "and the daemon is restarted or reloaded") {
			t.Errorf("%s no longer says a password set on a rebuilt store ends the hold once the daemon restarts or reloads:\n%s", name, text)
		}
	}

	// The refusal's own entry in the README: what to do with what .recover
	// brought back, before it goes anywhere near the database's path.
	i = strings.Index(readme, "- A database that won't open is left where it is.")
	j := strings.Index(readme, "`-on-corrupt rebuild` takes the other road")
	if i < 0 || j < i {
		t.Fatal("README.md's corruption notes no longer open with the refusal entry ahead of the -on-corrupt rebuild entry")
	}
	for want, what := range map[string]string{
		"SELECT key FROM settings": "the command that checks the recovered copy kept the install's settings",
		"do not put that one back": "that a copy with no settings in it must not replace the damaged file",
	} {
		if !strings.Contains(readme[i:j], want) {
			t.Errorf("README.md's refusal entry no longer says %s (%q):\n%s", what, want, readme[i:j])
		}
	}
}

// docUnits splits a markdown document into the pieces a reader takes in one at
// a time: paragraphs, except that a table is read row by row.
func docUnits(doc string) []string {
	var units []string
	for _, para := range strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(para), "|") {
			units = append(units, strings.Split(para, "\n")...)
			continue
		}
		units = append(units, para)
	}
	return units
}

// flatDoc reads markdown the way a sentence check needs it: wrapping collapsed,
// blockquote markers and bold dropped, so a phrase that breaks across lines or
// wears emphasis is still the phrase.
func flatDoc(md string) string {
	var words []string
	for _, w := range strings.Fields(md) {
		if w != ">" {
			words = append(words, w)
		}
	}
	return strings.ReplaceAll(strings.Join(words, " "), "**", "")
}

// In a container, a store with no birth marker and no stored access choice gets
// the ambiguous-provenance warning, whose way out is to restart with -access
// network. A store the daemon rebuilt after finding the database damaged has
// exactly that shape and none of the ambiguity - the daemon made it - and that
// way out is the one thing it does not honour. Its own warning says why the
// port answers 403; this one would send the operator round in a circle.
func TestARebuiltContainerStoreIsNotToldToRestartWithTheFlag(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.SetSetting(ctx, "thresh_down_mbps", "123.5"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	tearTableRoot(t, dbPath, "pauses")
	st, err = store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	defer st.Close()
	holdStands, err := st.AccessHoldAfterRebuild(ctx)
	if err != nil || !holdStands {
		t.Fatalf("fixture: the rebuilt store holds nothing (held=%v, err=%v)", holdStands, err)
	}
	for _, flags := range []config.Config{
		{DBPath: dbPath, Access: "network", AccessExplicit: true},
		{DBPath: dbPath},
	} {
		cfg, _ := holdAccessLocalAfterRebuild(flags, holdStands)
		set, err := settings.New(ctx, st, testDefaultsFor(cfg), settings.WithDatabaseCreated(false))
		if err != nil {
			t.Fatalf("settings: %v", err)
		}
		if warns := bootAccessDecision(t, cfg, st, set, true); len(warns) != 0 {
			t.Errorf("a rebuilt container store (-access %q) was told its access provenance is ambiguous and to restart with -access network, which that store does not honour: %s", flags.Access, warns[0].msg)
		}
	}

	// With the hold released the store is an ordinary unmarked one again, and the
	// warning is the ordinary answer for it.
	if _, err := st.ReleaseAccessHold(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{DBPath: dbPath}
	set, err := settings.New(ctx, st, testDefaultsFor(cfg), settings.WithDatabaseCreated(false))
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	if warns := bootAccessDecision(t, cfg, st, set, true); len(warns) != 1 {
		t.Fatalf("fixture: with the hold released the unmarked container store got %d warnings, want the ordinary one - so this test proves nothing about the hold", len(warns))
	}
}

// rebuiltDatabase leaves, at a fresh path, the database -on-corrupt rebuild made
// of an install that had a login and network access - closed, the way the next
// start finds it.
func rebuiltDatabase(t *testing.T) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pingularity.db")
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
	st.Close()
	tearTableRoot(t, dbPath, "pauses")
	st, err = store.Open(dbPath, store.RebuildOnCorruption())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if !st.RebuiltAfterCorruption() {
		st.Close()
		t.Fatal("fixture: nothing was rebuilt")
	}
	st.Close()
	return dbPath
}

// The TCP peers the guard judges: a machine on the LAN, and this one.
const (
	lanPeer      = "192.168.4.30:51515"
	loopbackPeer = "127.0.0.1:51515"
)

// asPeer makes one request through srv's handler from peer and returns the
// status and the trimmed body; a body makes it a JSON POST, the only kind the
// API takes.
func asPeer(t *testing.T, srv *web.Server, peer, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = peer
	r.Host = "127.0.0.1:9000"
	if peer == lanPeer {
		r.Host = "192.168.4.39:9000"
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}

// bootLikeRun is run()'s access sequence on st, in run()'s order and through
// run()'s own functions: the hold read that seeds the settings defaults, the
// settings load, the post-load access sequence, and the hook every later load
// runs. It returns the program - whose hold the web server reads - the
// controller, and every warning the boot raised.
func bootLikeRun(t *testing.T, flags config.Config, st *store.Store) (*program, *settings.Controller, []warnCall) {
	t.Helper()
	ctx := context.Background()
	p := &program{cfg: flags, store: st, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	var warns []warnCall
	sink := func(msg string, args ...any) { warns = append(warns, warnCall{msg: msg, args: args}) }
	set, err := settings.New(ctx, st, testDefaultsFor(p.accessSeedAtBoot(ctx, sink)))
	if !settings.LoadedOK(err) {
		t.Fatalf("settings: %v", err)
	}
	p.applyExplicitAccess(ctx, set, true, sink, sink)
	p.registerSettingsLoadedHook(ctx, set)
	return p, set, warns
}

// serverLikeRun is the web server run() builds over st, with the wire this file
// is about connected the way run() connects it: the guard and readiness read the
// program's hold.
func serverLikeRun(p *program, st *store.Store, set *settings.Controller) *web.Server {
	srv := web.New(st, func() web.LiveStatus { return web.LiveStatus{Online: true} }, nil, set, nil, "test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.AccessHold = p.currentAccessHold
	return srv
}

// bootWithUnreadableSettings is run()'s boot on a store whose settings - and so
// whether it carries the hold - cannot be read at that moment: dead fails both
// reads the way a transient fault does, leaving the unloaded controller run()
// carries into its retry loop, with the hook every later load runs registered.
// It returns the program, the controller, the server run() builds, and a reader
// for everything logged, early warnings included.
func bootWithUnreadableSettings(t *testing.T, flags config.Config, st *store.Store, dead context.Context) (*program, *settings.Controller, *web.Server, func() string) {
	t.Helper()
	logged := &lockedLog{}
	p := &program{cfg: flags, store: st, log: slog.New(slog.NewTextHandler(logged, nil))}
	seed := p.accessSeedAtBoot(dead, func(msg string, args ...any) { p.log.Warn(msg, args...) })
	set, err := settings.New(dead, st, testDefaultsFor(seed))
	if err == nil {
		t.Fatal("fixture: the settings read at boot must fail")
	}
	if got := p.currentAccessHold(); got != web.AccessHeldUnread {
		t.Fatalf("a boot that could not read the hold records it as %d, want unread: the network is decided on a read that failed", got)
	}
	p.registerSettingsLoadedHook(context.Background(), set)
	return p, set, serverLikeRun(p, st, set), logged.String
}

// lockedLog is a log sink safe to read while a handler may still be writing.
type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
