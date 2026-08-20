package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/config"
	"github.com/pingular/pingularity/internal/settings"
)

// The release notes promise that an explicit -access / PINGULARITY_ACCESS
// "overrides a disagreeing stored setting at EVERY start" - it is the
// documented lockout recovery for a container whose stored local-only 403s
// its own published port, with no shell to fix it from. That promise has to
// hold on every path that takes settings from unloaded to loaded, not only
// the boot whose first read succeeded: the retry loop exists precisely for
// the boots where that read failed, and those are the boots most likely to
// be someone's recovery attempt.

// TestRetryLoadedBootHonorsExplicitAccess: initial settings load fails
// (transient store fault), the retry loop later succeeds - and the explicit
// override must be applied THEN, or the operator's documented recovery
// silently did nothing for the whole uptime.
func TestRetryLoadedBootHonorsExplicitAccess(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// The locked-out shape: the store carries access_local_only=true plus the
	// ordinary operator life that makes it read as established.
	if err := st.SetSetting(ctx, "access_local_only", "1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "monitoring", "true"); err != nil {
		t.Fatal(err)
	}

	// The recovery boot: PINGULARITY_ACCESS=network, passed explicitly.
	cfg := config.Config{Access: "network", AccessExplicit: true}

	// The transient fault: the initial read fails, yielding the same
	// usable-but-unloaded controller run() carries into the retry loop.
	dead, cancelDead := context.WithCancel(ctx)
	cancelDead()
	set, err := settings.New(dead, st, testDefaultsFor(cfg))
	if err == nil {
		t.Fatal("fixture: the initial settings load must FAIL")
	}
	if set.Loaded() {
		t.Fatal("fixture: the controller must start unloaded")
	}

	p := &program{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}
	// run() registers the post-load hook before spawning the retry loop; the
	// loop's Reload is what fires the access sequence.
	p.registerSettingsLoadedHook(ctx, set)
	rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
	defer rcancel()
	p.retrySettingsLoad(rctx, set)

	if !set.Loaded() {
		t.Fatal("retry loop returned without loading settings - harness fault, not the bug")
	}
	if set.AccessLocalOnly() {
		t.Fatal("the explicit -access network override was NOT applied on the retry-loaded boot: the container's published port keeps answering 403 for the whole uptime, on exactly the boot the documented recovery is being attempted")
	}
	if got := settingsSnapshot(t, st)["access_local_only"]; got != "0" {
		t.Fatalf("the override must persist like it does on a clean boot, stored access_local_only = %q", got)
	}
}

// TestReloadHonorsExplicitAccess: a reload can pull in a stored value that
// disagrees with the still-set override - an out-of-band edit, a restored
// backup. "Authoritative" means it wins then too, not only at boot.
func TestReloadHonorsExplicitAccess(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	cfg := config.Config{Access: "network", AccessExplicit: true}
	set, err := settings.New(ctx, st, testDefaultsFor(cfg))
	if err != nil {
		t.Fatalf("clean boot load: %v", err)
	}
	// A restored backup (or any out-of-band write) re-locks the port...
	if err := st.SetSetting(ctx, "access_local_only", "1"); err != nil {
		t.Fatal(err)
	}
	p := &program{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}
	p.registerSettingsLoadedHook(ctx, set) // run() registers it before the signal loop starts
	// ...and the operator sends the reload signal, override still set.
	p.handleReload(ctx, set)

	if set.AccessLocalOnly() {
		t.Fatal("the reload adopted the restored local-only over a still-set explicit override - the operator's flag is documented as authoritative and just lost")
	}
	if got := settingsSnapshot(t, st)["access_local_only"]; got != "0" {
		t.Fatalf("stored access_local_only = %q after reload, want false", got)
	}
}

// TestImportReloadHonorsExplicitAccess: a settings import/restore calls
// Controller.Reload directly (internal/web), on a path main's handlers never
// see. The override must hold there too, or restoring a backup that carries
// access_local_only re-locks a container's published port with the operator's
// flag still set - the exact lockout the override exists to prevent.
func TestImportReloadHonorsExplicitAccess(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	cfg := config.Config{Access: "network", AccessExplicit: true}
	set, err := settings.New(ctx, st, testDefaultsFor(cfg))
	if err != nil {
		t.Fatalf("clean boot load: %v", err)
	}
	p := &program{cfg: cfg, log: slog.New(slog.NewTextHandler(io.Discard, nil)), store: st}
	p.registerSettingsLoadedHook(ctx, set)

	// The imported/restored settings carry the lockout...
	if err := st.SetSetting(ctx, "access_local_only", "1"); err != nil {
		t.Fatal(err)
	}
	// ...and the import path reloads the controller directly.
	if err := set.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if set.AccessLocalOnly() {
		t.Fatal("a direct Reload (the import path) adopted the restored local-only over a still-set explicit override")
	}
	if got := settingsSnapshot(t, st)["access_local_only"]; got != "0" {
		t.Fatalf("stored access_local_only = %q after import reload, want 0", got)
	}
}
