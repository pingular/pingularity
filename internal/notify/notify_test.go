package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

func TestPayloadForDiscord(t *testing.T) {
	b := payloadFor("discord", "hello", map[string]any{"event": "test"})
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["content"] != "hello" {
		t.Errorf("discord payload should use content, got %v", m)
	}
	if _, ok := m["text"]; ok {
		t.Errorf("discord payload should not include text: %v", m)
	}
}

func TestPayloadForSlack(t *testing.T) {
	b := payloadFor("slack", "hi", nil)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["text"] != "hi" {
		t.Errorf("slack payload should use text, got %v", m)
	}
	if _, ok := m["content"]; ok {
		t.Errorf("slack payload should not include content: %v", m)
	}
}

func TestPayloadForGeneric(t *testing.T) {
	b := payloadFor("generic", "msg", map[string]any{"event": "link_down"})
	var m map[string]any
	json.Unmarshal(b, &m)
	// Generic carries the text under every common key so chat (text/content),
	// Gotify (message), and Apprise (body) receivers all show it.
	for _, k := range []string{"text", "content", "message", "body"} {
		if m[k] != "msg" {
			t.Errorf("generic payload missing %s: %v", k, m)
		}
	}
	if m["event"] != "link_down" {
		t.Errorf("generic payload should merge fields: %v", m)
	}
	// Headline + severity derived from the event (Gotify/Apprise/ntfy render these).
	if m["title"] != "Internet down" || m["type"] != "failure" || m["priority"] != float64(5) {
		t.Errorf("generic payload title/type/priority wrong: %v", m)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://discord.com/api/webhooks/1": "discord.com",
		"http://127.0.0.1:9911/hook":         "127.0.0.1:9911",
		"https://hc-ping.com/uuid?x=1":       "hc-ping.com",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q)=%q want %q", in, got, want)
		}
	}
}

// classifyDest must map hosts onto the fixed class enum - the only values that
// may ever reach a counter name.
func TestClassifyDest(t *testing.T) {
	cases := map[string]string{
		"https://discord.com/api/webhooks/1":     "discord",
		"https://discordapp.com/api/webhooks/1":  "discord",
		"https://hooks.slack.com/services/T/B/X": "slack",
		"https://hc-ping.com/uuid":               "healthchecks",
		"https://healthchecks.example.org/ping":  "healthchecks",
		"https://api.telegram.org/botX/send":     "generic",
		"https://ntfy.sh/mytopic":                "ntfy",
		"https://ntfy.example.com/alerts":        "ntfy",
		"http://127.0.0.1:9911/hook":             "generic",
	}
	for in, want := range cases {
		if got := classifyDest(in); got != want {
			t.Errorf("classifyDest(%q) = %q, want %q", in, got, want)
		}
	}
}

// ssrfBlocked must classify link-local AND cloud-metadata literals as blocked
// (mirroring dialGuard), so a metadata webhook is a permanent failure that Outage
// skips the retry backoff for, rather than a transient-looking dial error that
// burns the full backoff holding outageMu. RFC1918/loopback stay allowed -
// self-hosted notifiers are normal use.
func TestSSRFBlockedMetadataLiterals(t *testing.T) {
	blocked := []string{
		"http://169.254.169.254/latest/meta-data", // link-local (AWS/GCP/Azure)
		"http://100.100.100.200/",                 // Alibaba Cloud metadata literal
		"http://192.0.0.192/",                     // Oracle Cloud legacy metadata literal
		"http://[fd00:ec2::254]/latest/meta-data", // AWS IPv6 metadata prefix
	}
	for _, u := range blocked {
		if !ssrfBlocked(u) {
			t.Errorf("ssrfBlocked(%q) = false, want true (metadata/link-local)", u)
		}
	}
	allowed := []string{
		"https://discord.com/api/webhooks/1", // public host
		"http://127.0.0.1:9911/hook",         // localhost notifier
		"http://192.168.1.10:8080/hook",      // LAN notifier
	}
	for _, u := range allowed {
		if ssrfBlocked(u) {
			t.Errorf("ssrfBlocked(%q) = true, want false (legitimate destination)", u)
		}
	}
}

// Webhook and heartbeat deliveries must move the per-class ok/fail/latency
// counters (localhost server → class "generic"; heartbeats are always
// "heartbeat").
func TestSendAndHeartbeatCounters(t *testing.T) {
	stats.ResetForTest()
	var status atomic.Int32
	status.Store(200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Take longer than the clock can miss. A loopback round trip is quick
		// enough that Windows timed it as exactly zero, so the latency sum below
		// stayed at zero and failed a release that had nothing wrong with it.
		time.Sleep(20 * time.Millisecond)
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)
	ctx := context.Background()

	n.Send(ctx, "hi", map[string]any{"event": "test"}) // 200 → ok
	Heartbeat(ctx, srv.Client(), srv.URL, log)         // 200 → heartbeat ok
	status.Store(500)
	n.Send(ctx, "hi", nil) // 500 → fail

	s := stats.Lifetime()
	for name, want := range map[string]int64{
		"notify.generic.ok":      1,
		"notify.generic.fail":    1,
		"notify.generic.lat_n":   2,
		"notify.heartbeat.ok":    1,
		"notify.heartbeat.lat_n": 1,
	} {
		if got := s.Counters[name]; got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	if s.Floats["notify.generic.lat_ms_sum"] <= 0 {
		t.Errorf("notify.generic.lat_ms_sum = %v, want > 0", s.Floats["notify.generic.lat_ms_sum"])
	}
}

// SpeedThreshold's generic payload is a webhook API: automations key off the
// exact field names, so pin them - event, failures (a JSON array), down_mbps/
// up_mbps/ping_ms/server always, jitter_ms only when measured - and the human
// message carries the joined failure list.
func TestSpeedThreshold(t *testing.T) {
	bodies := make(chan []byte, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- b
		w.WriteHeader(200)
	}))
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)

	failures := []string{"download 5.2 < 100 Mbps", "ping 88 > 50 ms"}
	sp := store.SpeedSample{DownMbps: 5.2, UpMbps: 1.3, PingMS: 88, Server: "iperf3: NAS"}
	decode := func() map[string]any {
		t.Helper()
		select {
		case b := <-bodies:
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("payload not JSON: %v", err)
			}
			return m
		case <-time.After(2 * time.Second):
			t.Fatal("no webhook delivery")
			return nil
		}
	}

	// (a) jitter unmeasured (iperf3 TCP): no jitter_ms key at all.
	n.SpeedThreshold(context.Background(), sp, failures)
	m := decode()
	if m["event"] != "speedtest_threshold_failed" {
		t.Errorf("event = %v, want speedtest_threshold_failed", m["event"])
	}
	arr, ok := m["failures"].([]any)
	if !ok || len(arr) != 2 || arr[0] != failures[0] || arr[1] != failures[1] {
		t.Errorf("failures = %v, want the list round-tripped as a JSON array %v", m["failures"], failures)
	}
	for key, want := range map[string]float64{"down_mbps": 5.2, "up_mbps": 1.3, "ping_ms": 88} {
		if got, ok := m[key].(float64); !ok || got != want {
			t.Errorf("%s = %v, want %v", key, m[key], want)
		}
	}
	if m["server"] != "iperf3: NAS" {
		t.Errorf("server = %v, want iperf3: NAS", m["server"])
	}
	if _, present := m["jitter_ms"]; present {
		t.Errorf("jitter_ms present without a measurement: %v", m["jitter_ms"])
	}
	msg, _ := m["text"].(string)
	if !strings.Contains(msg, strings.Join(failures, "; ")) {
		t.Errorf("message %q missing the joined failure list", msg)
	}

	// (b) jitter measured: jitter_ms rides along.
	jit := 30.0
	sp.JitterMS = &jit
	n.SpeedThreshold(context.Background(), sp, failures)
	if m := decode(); m["jitter_ms"] != float64(30) {
		t.Errorf("jitter_ms = %v, want 30", m["jitter_ms"])
	}
}

// A transition alert must survive a transient webhook failure: Outage retries a
// few times, so a couple of 5xx/hiccup responses at the exact moment of the
// up/down transition don't permanently drop the alert.
func TestOutageRetriesTransientFailure(t *testing.T) {
	old := outageRetries
	outageRetries = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { outageRetries = old }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < 3 { // first two attempts fail, third succeeds
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)
	n.Outage(context.Background(), false, 0)

	if got := hits.Load(); got != 3 {
		t.Errorf("Outage made %d attempts, want 3 (2 fail + 1 ok)", got)
	}
}

// Concurrent transition alerts must deliver in order: a down-alert whose
// delivery is slow must still land before the following up-alert, never the
// reverse. main.go dispatches each transition on its own goroutine, so without
// the outageMu serialization a slow down could be overtaken by a fast up.
func TestOutageDeliversInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ev := "up"
		if strings.Contains(string(body), "link_down") {
			ev = "down"
			time.Sleep(200 * time.Millisecond) // the down delivery is the slow one
		}
		mu.Lock()
		order = append(order, ev)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); n.Outage(context.Background(), false, 0) }() // down
	// The down transition happens first in reality (seconds before recovery);
	// give its goroutine a moment to take outageMu before the up fires.
	time.Sleep(50 * time.Millisecond)
	wg.Add(1)
	go func() { defer wg.Done(); n.Outage(context.Background(), true, 30) }() // up
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "down" || order[1] != "up" {
		t.Fatalf("delivery order = %v, want [down up] - the up overtook the slow down", order)
	}
}

// A cancelled context (shutdown) must abort the backoff wait rather than block a
// goroutine for the full retry schedule.
func TestOutageStopsRetryingOnCancel(t *testing.T) {
	old := outageRetries
	outageRetries = []time.Duration{time.Hour, time.Hour} // would block ~2h if the wait didn't abort
	defer func() { outageRetries = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500) // always fail so a retry would be attempted
	}))
	defer srv.Close()

	// Cancel while the first attempt is in flight so the backoff wait sees a done
	// context and returns instead of sleeping an hour.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)

	done := make(chan struct{})
	go func() { n.Outage(ctx, true, 42); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Outage did not abort its retry backoff on context cancel")
	}
}

// A permanent failure must not be retried: an SSRF-blocked destination fails the
// same way every attempt, so Outage must emit exactly one blocked record and skip
// the backoff loop rather than triple-counting the counter and holding outageMu
// across the full schedule.
func TestOutageSkipsRetryOnBlockedDest(t *testing.T) {
	stats.ResetForTest()
	old := outageRetries
	// Long enough that a wrongful retry would visibly stall the test.
	outageRetries = []time.Duration{time.Hour, time.Hour}
	defer func() { outageRetries = old }()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return "http://169.254.169.254/hook" }, log)

	done := make(chan struct{})
	go func() { n.Outage(context.Background(), false, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Outage retried a permanently-blocked destination instead of returning")
	}

	if got := stats.Lifetime().Counters["notify.generic.blocked"]; got != 1 {
		t.Errorf("notify.generic.blocked = %d, want 1 (one transition = one blocked record, no retries)", got)
	}
}

// A non-transient 4xx (e.g. a deleted webhook that answers 404 forever) is
// permanent too: Outage must attempt once and stop, not hold outageMu across the
// backoff for a status the server won't reconsider.
func TestOutageSkipsRetryOnPermanentStatus(t *testing.T) {
	old := outageRetries
	outageRetries = []time.Duration{time.Hour, time.Hour}
	defer func() { outageRetries = old }()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(404)
	}))
	defer srv.Close()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)

	done := make(chan struct{})
	go func() { n.Outage(context.Background(), false, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Outage retried a permanent 404 instead of returning")
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("Outage made %d attempts, want 1 (404 is permanent, no retry)", got)
	}
}

// Send must classify permanence so Outage/digest can tell a settled reject from a
// transient hiccup: blocked and non-transient 4xx wrap errPermanent, while 5xx
// and the transient 4xx (408/425/429) do not.
func TestSendPermanenceClassification(t *testing.T) {
	var status atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	// Blocked destination: permanent regardless of any network reply.
	blocked := New(func() string { return "http://169.254.169.254/hook" }, log)
	if err := blocked.Send(ctx, "hi", nil); !errors.Is(err, errPermanent) {
		t.Errorf("blocked Send err = %v, want wrapping errPermanent", err)
	}

	n := New(func() string { return srv.URL }, log)
	for _, tc := range []struct {
		code      int
		permanent bool
	}{
		{404, true}, {403, true}, {400, true}, // settled 4xx rejects
		{408, false}, {425, false}, {429, false}, // transient 4xx
		{500, false}, {503, false}, // server errors, worth a retry
	} {
		status.Store(int32(tc.code))
		err := n.Send(ctx, "hi", nil)
		if err == nil {
			t.Errorf("status %d: Send returned nil, want an error", tc.code)
			continue
		}
		if got := errors.Is(err, errPermanent); got != tc.permanent {
			t.Errorf("status %d: errors.Is(err, errPermanent) = %v, want %v", tc.code, got, tc.permanent)
		}
	}
}

func TestSSRFBlocked(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://169.254.169.254/latest/meta-data/", true}, // cloud metadata
		{"http://169.254.169.254", true},
		{"http://[fe80::1]/hook", true}, // IPv6 link-local
		{"https://discord.com/api/webhooks/1/x", false},
		{"http://192.168.1.10:8080/hook", false}, // RFC1918 LAN notifier - allowed
		{"http://127.0.0.1:9000/hook", false},    // localhost - allowed
		{"http://ntfy.example.com/topic", false},
		{"not a url", false}, // parse-failure passes through to the request layer
	}
	for _, c := range cases {
		if got := ssrfBlocked(c.url); got != c.want {
			t.Errorf("ssrfBlocked(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestNotifyClientRefusesRedirects(t *testing.T) {
	if c := NewClient(); c.CheckRedirect == nil {
		t.Fatal("notify client follows redirects (SSRF redirect pivot)")
	} else if err := c.CheckRedirect(nil, nil); err == nil {
		t.Error("CheckRedirect should refuse to follow")
	}
}

// The heartbeat client follows redirects but must keep the stdlib's 10-hop cap
// (a custom CheckRedirect silently drops it) and refuse link-local pivots.
func TestHeartbeatClientRedirects(t *testing.T) {
	c := NewHeartbeatClient()
	req, err := http.NewRequest(http.MethodGet, "https://example.com/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckRedirect(req, make([]*http.Request, 3)); err != nil {
		t.Errorf("redirect within the cap refused: %v", err)
	}
	if err := c.CheckRedirect(req, make([]*http.Request, 10)); err == nil {
		t.Error("11th redirect should be refused (hop cap gone)")
	}
	pivot, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/", nil)
	if err := c.CheckRedirect(pivot, nil); err == nil {
		t.Error("redirect to link-local/metadata should be refused")
	}
}

// Pins that a threshold alert survives a transient webhook failure - the
// property Outage already has. A breach is a one-shot event exactly like a
// transition: with no retry, a 503 at the wrong instant dropped the alert
// permanently.
func TestSpeedThresholdRetriesTransientFailure(t *testing.T) {
	stats.ResetForTest()
	old := outageRetries
	outageRetries = []time.Duration{time.Millisecond}
	defer func() { outageRetries = old }()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503) // transient: retry
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	n := New(func() string { return srv.URL }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.SpeedThreshold(context.Background(), store.SpeedSample{DownMbps: 5, UpMbps: 1, PingMS: 20}, []string{"download 5 < 100 Mbps"})
	if got := calls.Load(); got != 2 {
		t.Fatalf("webhook received %d requests, want 2 (one transient failure, one retry)", got)
	}
	if got := stats.Lifetime().Counters["notify.generic.ok"]; got != 1 {
		t.Errorf("notify.generic.ok = %d, want 1", got)
	}
}

// A permanent failure (non-transient 4xx) must not be retried: it will fail
// the same way every attempt, and retrying would multi-count the counters and
// re-emit the same warn.
func TestSpeedThresholdDoesNotRetryPermanentFailure(t *testing.T) {
	stats.ResetForTest()
	old := outageRetries
	outageRetries = []time.Duration{time.Millisecond}
	defer func() { outageRetries = old }()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(400)
	}))
	defer srv.Close()
	n := New(func() string { return srv.URL }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n.SpeedThreshold(context.Background(), store.SpeedSample{DownMbps: 5, UpMbps: 1, PingMS: 20}, []string{"download 5 < 100 Mbps"})
	if got := calls.Load(); got != 1 {
		t.Fatalf("webhook received %d requests, want exactly 1 (permanent failures are not retried)", got)
	}
}

// NTFY IS SPOKEN NATIVELY: the message is the plain-text body and the polish
// rides headers - title, alertMeta's 1-5 priority verbatim (it is ntfy's own
// scale), and an emoji tag per severity. No JSON blob on anyone's phone.
func TestNtfyDeliveryShape(t *testing.T) {
	stats.ResetForTest()
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL + "/alerts" }, log)
	// The override: an arbitrary self-hosted domain the hostname guess cannot
	// place, forced to ntfy by the Alerts tab's format select.
	n.FormatFn = func() string { return "ntfy" }
	if err := n.Send(context.Background(), "Internet down at home", map[string]any{"event": "link_down"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if string(body) != "Internet down at home" {
		t.Errorf("ntfy body must be the bare message, got %q", body)
	}
	if ct := got.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content-type = %q, want text/plain", ct)
	}
	if got.Header.Get("X-Title") != "Internet down" || got.Header.Get("X-Priority") != "5" || got.Header.Get("X-Tags") != "rotating_light" {
		t.Errorf("ntfy headers wrong: title=%q prio=%q tags=%q",
			got.Header.Get("X-Title"), got.Header.Get("X-Priority"), got.Header.Get("X-Tags"))
	}
	if stats.Lifetime().Counters["notify.ntfy.ok"] != 1 {
		t.Error("delivery not counted under the ntfy destination")
	}
}

// The override cuts BOTH ways: forcing "generic" on a recognizable host must
// deliver the rich JSON, not the host's chat shape.
func TestFormatOverrideForcesGeneric(t *testing.T) {
	stats.ResetForTest()
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	n := New(func() string { return srv.URL }, log)
	n.FormatFn = func() string { return "generic" }
	if err := n.Send(context.Background(), "msg", map[string]any{"event": "link_up"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("forced-generic body is not JSON: %q", body)
	}
	if m["title"] != "Internet restored" || m["text"] != "msg" {
		t.Errorf("forced-generic payload wrong: %v", m)
	}
}
