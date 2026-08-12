package web

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"
)

// The Quick Setup offer is materialized once at boot (EnsureQuickSetupOffer)
// and read through one shared rule (settings.QuickSetupHold). Each leg of the
// rule must hold alone: no offer clock means no offer, an upgrade is answered
// at boot and can never see the prompt, the grace expires on its own, and an
// answered dialog never returns.
func TestQuickSetupPendingGates(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	// Before boot seeds anything: no offer clock, no offer.
	if s.quickSetupPending(ctx) {
		t.Fatal("an unseeded offer clock must not offer")
	}

	// A fresh install's boot: seeds the clock, opens the offer.
	if err := s.settings.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if !s.quickSetupPending(ctx) {
		t.Fatal("fresh install after boot seeding must offer Quick Setup")
	}

	// The grace expires on its own: a stale clock stops offering (this is what
	// releases the monitoring hold on an install nobody browses to).
	if err := s.store.SetSetting(ctx, "quick_setup_offer_since", strconv.FormatInt(time.Now().Unix()-49*3600, 10)); err != nil {
		t.Fatal(err)
	}
	if s.quickSetupPending(ctx) {
		t.Fatal("an expired offer must not be shown")
	}

	// An upgrade: months-old install anchor, marker unset, no clock. Boot
	// materializes the answer instead of an offer.
	s2 := newTestServer(t)
	if err := s2.store.SetSetting(ctx, "first_seen_ts", strconv.FormatInt(time.Now().Unix()-30*24*3600, 10)); err != nil {
		t.Fatal(err)
	}
	if err := s2.settings.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if s2.quickSetupPending(ctx) {
		t.Fatal("an install with months of history must never see the first-run dialog")
	}
	if !s2.settings.QuickSetupDone() {
		t.Fatal("the upgrade decision must be stored as answered, not recomputed")
	}

	// The marker beats an open clock: answered is answered. Dismiss via the
	// atomic endpoint - /api/settings can no longer set the server-owned marker.
	s3 := newTestServer(t)
	if err := s3.settings.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if w := do(t, s3.Handler(), "POST", "/api/quick-setup", `{"dismiss":true}`); w.Code != 200 {
		t.Fatalf("dismiss POST: %d %s", w.Code, w.Body.String())
	}
	if s3.quickSetupPending(ctx) {
		t.Fatal("an answered dialog must not be offered again")
	}

	// Seeding is idempotent: a second boot neither reopens nor re-marks.
	if err := s3.settings.EnsureQuickSetupOffer(ctx, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if s3.quickSetupPending(ctx) {
		t.Fatal("a later boot must not reopen an answered offer")
	}
}

// The dismissal write is marker-only: nothing else may change. A decline that
// also reset a setting would make "keep the defaults" a lie. Goes through the
// atomic endpoint's dismiss path (the only way to set the marker).
func TestQuickSetupDismissTouchesNothingElse(t *testing.T) {
	s := newTestServer(t)
	before := s.settings.Snapshot()
	if w := do(t, s.Handler(), "POST", "/api/quick-setup", `{"dismiss":true}`); w.Code != 200 {
		t.Fatalf("POST: %d %s", w.Code, w.Body.String())
	}
	after := s.settings.Snapshot()
	if !after.QuickSetupDone {
		t.Fatal("marker did not persist")
	}
	before.QuickSetupDone = true // the one permitted difference
	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	if string(bj) != string(aj) {
		t.Fatalf("dismissal changed more than the marker:\n before %s\n after  %s", bj, aj)
	}
}

// The status payload carries the offer, and it flips off after the answer.
func TestStatusCarriesQuickSetupPending(t *testing.T) {
	s := newTestServer(t)
	s.status = func() LiveStatus { // handleStatus degrades to 503 without one
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	read := func() bool {
		w := do(t, s.Handler(), "GET", "/api/status", "")
		if w.Code != 200 {
			t.Fatalf("status: %d", w.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		v, ok := m["quick_setup_pending"].(bool)
		if !ok {
			t.Fatalf("quick_setup_pending missing or not bool: %v", m["quick_setup_pending"])
		}
		return v
	}
	if err := s.settings.EnsureQuickSetupOffer(context.Background(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	if !read() {
		t.Fatal("fresh install: status must offer Quick Setup")
	}
	if w := do(t, s.Handler(), "POST", "/api/quick-setup", `{"dismiss":true}`); w.Code != 200 {
		t.Fatalf("dismiss POST: %d", w.Code)
	}
	if read() {
		t.Fatal("after the answer, status must stop offering")
	}
}
