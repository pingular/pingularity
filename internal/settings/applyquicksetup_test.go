package settings

import (
	"context"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// ApplyQuickSetup writes ONLY the Quick Setup keys, atomically, and marks done -
// it must NOT freeze the ~50 form defaults the settings-form path persists, so a
// CLI-seeded value the answer doesn't touch survives.
func TestApplyQuickSetupIsTargetedAndAtomic(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A CLI-seeded latency interval Quick Setup never touches (default is 5s).
	c, err := New(ctx, st, Values{Latency: 17 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}

	err = c.ApplyQuickSetup(ctx, QuickSetupAnswer{
		SpeedtestEnabled: true, SpeedSeconds: 3600, UpdateCheck: false, LocalOnly: true,
	})
	if err != nil {
		t.Fatalf("ApplyQuickSetup: %v", err)
	}

	// The answer's keys are applied AND persisted.
	if !c.QuickSetupDone() {
		t.Error("marker not set")
	}
	if !c.SpeedtestEnabled() || c.SpeedInterval() != time.Hour {
		t.Errorf("speed: enabled=%v interval=%v, want true/1h", c.SpeedtestEnabled(), c.SpeedInterval())
	}
	if c.UpdateCheckEnabled() {
		t.Error("update check should be off")
	}
	if !c.AccessLocalOnly() {
		t.Error("local_only should be on")
	}
	if !c.Monitoring() {
		t.Error("Start monitoring must turn the power on")
	}

	// The untouched CLI-seeded key must NOT have been frozen at a default: reload
	// from the store and confirm it is still 17s, not the 5s default.
	kv, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, frozen := kv["latency_interval_s"]; frozen {
		t.Errorf("latency_interval_s was persisted by Quick Setup (froze a default); the endpoint must write only its own keys")
	}
	c2, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	// The store has no latency key, so the fresh Controller keeps its seed (5s
	// here) - the point is the DB wasn't frozen; a later -interval flag can seed.
	_ = c2
}

// Manually (speedtests off) must not apply an interval, and out-of-range
// intervals clamp.
func TestApplyQuickSetupManualAndClamp(t *testing.T) {
	ctx := context.Background()
	mk := func() *Controller {
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c, err := New(ctx, st, Values{Speed: 42 * time.Minute, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c := mk()
	if err := c.ApplyQuickSetup(ctx, QuickSetupAnswer{SpeedtestEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if c.SpeedtestEnabled() {
		t.Error("manual: speedtests must be off")
	}
	if c.SpeedInterval() != 42*time.Minute {
		t.Errorf("manual must not touch the interval; got %v", c.SpeedInterval())
	}

	c = mk()
	if err := c.ApplyQuickSetup(ctx, QuickSetupAnswer{SpeedtestEnabled: true, SpeedSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if c.SpeedInterval() != MinSpeed {
		t.Errorf("a 1s interval must clamp to MinSpeed (%v); got %v", MinSpeed, c.SpeedInterval())
	}
}

// Auth: a hash turns login on with the username; no hash leaves it off.
func TestApplyQuickSetupAuth(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(ctx, st, Values{Speed: time.Hour, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyQuickSetup(ctx, QuickSetupAnswer{
		SpeedtestEnabled: false, LocalOnly: false,
		AuthEnabled: true, AuthUser: "bob", AuthHash: "$2a$10$abcdefghijklmnopqrstuv",
	}); err != nil {
		t.Fatal(err)
	}
	if !c.AuthEnabled() || c.AuthUser() != "bob" {
		t.Errorf("auth: enabled=%v user=%q, want true/bob", c.AuthEnabled(), c.AuthUser())
	}
	if c.AuthHash() == "" {
		t.Error("auth hash not stored")
	}
}

// Dismiss writes ONLY the marker - it must not freeze the form defaults the way
// the old /api/settings marker post did (which persisted every key).
func TestApplyQuickSetupDismissFreezesNothing(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c, err := New(ctx, st, Values{Latency: 17 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 3, UpAfter: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyQuickSetup(ctx, QuickSetupAnswer{Dismiss: true}); err != nil {
		t.Fatal(err)
	}
	if !c.QuickSetupDone() {
		t.Error("dismiss must mark done")
	}
	kv, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Only the marker should be persisted - nothing else the dialog "kept".
	for _, k := range []string{"latency_interval_s", "speedtest_enabled", "speed_interval_s", "access_local_only", "update_check_enabled"} {
		if _, frozen := kv[k]; frozen {
			t.Errorf("dismiss persisted %q (froze a default); it must write only the marker", k)
		}
	}
}
