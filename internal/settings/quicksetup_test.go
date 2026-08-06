package settings

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// One rule decides both the dialog offer and the boot-time monitoring hold;
// its table is small enough to pin whole.
func TestQuickSetupHoldTable(t *testing.T) {
	now := int64(1_800_000_000)
	cases := []struct {
		name  string
		done  bool
		since int64
		now   int64
		want  bool
	}{
		{"unseeded clock never holds", false, 0, now, false},
		// now < grace: if the !=0 leg were dropped, now-0 would be inside the
		// grace and this case would wrongly hold - the mutation that survives
		// the plain zero case above.
		{"unseeded clock never holds even young", false, 0, 100, false},
		{"open offer holds", false, now - 3600, now, true},
		{"grace boundary releases", false, now - quickSetupGrace, now, false},
		{"expired offer releases", false, now - quickSetupGrace - 1, now, false},
		{"answered releases regardless", true, now - 3600, now, false},
		{"answered with no clock releases", true, 0, now, false},
	}
	for _, c := range cases {
		if got := QuickSetupHold(c.done, c.since, c.now); got != c.want {
			t.Errorf("%s: QuickSetupHold(%v,%d) = %v, want %v", c.name, c.done, c.since, got, c.want)
		}
	}
}

// Boot's one-time decision. The deadlock this design exists to avoid: the
// install anchor (first_seen_ts) only persists once samples exist, and the
// hold prevents samples - so the offer runs on its own clock and ALWAYS
// expires, browser or no browser.
func TestEnsureQuickSetupOffer(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Unix()
	open := func(t *testing.T) *Controller {
		t.Helper()
		st, err := store.Open(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		c, err := New(ctx, st, Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	t.Run("fresh install opens the offer", func(t *testing.T) {
		c := open(t)
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != now {
			t.Fatalf("offer clock = %d, want %d", since, now)
		}
		if c.QuickSetupDone() {
			t.Fatal("a fresh install must not be marked answered")
		}
	})

	t.Run("a young upgrade is still an upgrade", func(t *testing.T) {
		// A day-old 0.60.0 install upgrading has a young anchor AND running
		// monitoring: holding it would silently stop probing on an established
		// install. Any anchor at all means history happened - answered.
		c := open(t)
		if err := c.store.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(now-3600, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("an install with an anchor must be materialized as answered")
		}
		if c.QuickSetupOfferSince(ctx) != 0 {
			t.Fatal("no offer clock for an upgrade")
		}
	})

	t.Run("history without an anchor is an upgrade", func(t *testing.T) {
		// The speedtest-only / headless case: months of rows, no anchor ever
		// persisted (the anchor needs a dashboard or digest to compute uptime).
		c := open(t)
		if err := c.store.InsertSpeed(ctx, store.SpeedSample{TS: now - 90*24*3600, DownMbps: 500, UpMbps: 300}); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("measurement history must read as an upgrade even with no anchor")
		}
	})

	t.Run("an upgrade is answered outright", func(t *testing.T) {
		c := open(t)
		if err := c.store.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(now-30*24*3600, 10)); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if !c.QuickSetupDone() {
			t.Fatal("months of history must materialize as answered")
		}
		if c.QuickSetupOfferSince(ctx) != 0 {
			t.Fatal("an upgrade must not get an offer clock")
		}
	})

	t.Run("idempotent across boots", func(t *testing.T) {
		c := open(t)
		if err := c.EnsureQuickSetupOffer(ctx, now); err != nil {
			t.Fatal(err)
		}
		if err := c.EnsureQuickSetupOffer(ctx, now+7200); err != nil {
			t.Fatal(err)
		}
		if since := c.QuickSetupOfferSince(ctx); since != now {
			t.Fatalf("second boot moved the clock: %d, want %d", since, now)
		}
	})
}
