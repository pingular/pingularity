package digest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/notify"
	"github.com/pingular/pingularity/internal/store"
)

// storedLastSent reads the persisted watermark, failing the test if it is unset
// or unparseable.
func storedLastSent(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	all, err := st.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	raw := all[keyLastSent]
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("last-sent %q unparseable: %v", raw, err)
	}
	return time.Unix(n, 0)
}

// A digest has nowhere to go when no webhook is configured. Notifier.Send treats
// a blank URL as a deliberate no-op and returns nil - the right contract for
// fire-and-forget alerts - but the digest reads a nil as DELIVERED: it advanced
// the watermark, cleared pending state and logged "digest sent" for a summary
// that never left the host. Every due period was consumed silently, and the
// period in flight when a webhook was finally configured had already been eaten,
// so the first real digest waited a whole extra day (or week).
//
// Integration on purpose, with the real Notifier: a fake sender cannot reproduce
// the nil-on-blank contract that caused this.
func TestNoWebhookDoesNotConsumeDigestPeriod(t *testing.T) {
	m, st, _ := newManager(t)
	ctx := context.Background()

	var mu sync.Mutex
	var hookURL string // configured only midway through the test
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	delivered := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(bodies)
	}

	m.Notify = notify.New(func() string {
		mu.Lock()
		defer mu.Unlock()
		return hookURL
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	// Due (past a daily period) but still inside the stale-watermark slack, so a
	// send here covers the whole window rather than a re-capped last period.
	last := t0.Add(-24*time.Hour - 10*time.Minute)
	if err := st.SetSetting(ctx, keyLastSent, strconv.FormatInt(last.Unix(), 10)); err != nil {
		t.Fatalf("seed last-sent: %v", err)
	}

	m.tick(ctx) // webhook still blank

	if n := delivered(); n != 0 {
		t.Fatalf("unconfigured tick delivered %d payloads, want 0", n)
	}
	if got := storedLastSent(t, st); !got.Equal(last) {
		t.Fatalf("unconfigured tick advanced last-sent to %v, want it held at %v: the period was consumed with nothing delivered", got, last)
	}
	if !m.lastSentMem.IsZero() {
		t.Fatalf("unconfigured tick advanced the in-memory watermark to %v, want zero", m.lastSentMem)
	}

	// Configuring a webhook must let the still-undelivered period go out at the
	// next poll, covering the whole window since the last real send - not wait a
	// further day for a period the previous tick already swallowed.
	mu.Lock()
	hookURL = srv.URL
	mu.Unlock()

	m.tick(ctx)

	if n := delivered(); n != 1 {
		t.Fatalf("after configuring a webhook, delivered %d payloads, want 1", n)
	}
	mu.Lock()
	body := bodies[0]
	mu.Unlock()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if got, want := payload["window_s"], float64(24*3600+600); got != want {
		t.Errorf("window_s = %v, want %v (the whole undelivered window)", got, want)
	}
	if got := storedLastSent(t, st); !got.Equal(t0) {
		t.Errorf("last-sent = %v, want %v after a real delivery", got, t0)
	}
}

// Removing the webhook while a window is pending retry must drop that window,
// exactly as switching the cadence off does. A pending window is never re-capped
// (deliberately - a webhook outage must not shave the earliest slice off an
// undelivered digest), so holding one across a months-long unconfigured stretch
// would mail a giant catch-up report the moment a webhook appears.
func TestRemovedWebhookDropsPendingRetryWindow(t *testing.T) {
	m, _, fs := newManager(t)
	ctx := context.Background()
	t0 := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return t0 }
	m.tick(ctx) // arm at t0

	// Due, inside the slack so the window is not capped: the send fails and the
	// window stays pending for retry.
	fs.fail = true
	m.now = func() time.Time { return t0.Add(24*time.Hour + 5*time.Minute) }
	m.tick(ctx)
	if fs.calls != 1 {
		t.Fatalf("expected one failed send attempt, got %d", fs.calls)
	}
	if !m.pendingSince.Equal(t0) {
		t.Fatalf("pendingSince = %v, want %v held for retry", m.pendingSince, t0)
	}

	fs.unconfigured = true // operator clears the webhook URL
	m.tick(ctx)
	if fs.calls != 1 {
		t.Fatalf("unconfigured tick attempted delivery (%d sends), want none", fs.calls)
	}
	if !m.pendingSince.IsZero() {
		t.Fatalf("pendingSince = %v, want dropped once there is nowhere to deliver", m.pendingSince)
	}

	// A webhook 11 days later gets one bounded digest, not the whole gap.
	fs.unconfigured, fs.fail = false, false
	m.now = func() time.Time { return t0.Add(11 * 24 * time.Hour) }
	m.tick(ctx)
	if fs.calls != 2 {
		t.Fatalf("expected one send after reconfiguring, got %d total", fs.calls)
	}
	if w, _ := fs.lastF["window_s"].(int); w != 86400 {
		t.Errorf("window_s = %d, want 86400 (re-capped to one period)", w)
	}
}
