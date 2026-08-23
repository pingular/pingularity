package settings

import (
	"context"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pingular/pingularity/internal/store"
)

// capLen must cut on a UTF-8 boundary, never mid-codepoint, so a length-capped
// value (e.g. an oversized username) stays valid UTF-8. Regression for B7.
func TestCapLenRuneSafe(t *testing.T) {
	// '€' is 3 bytes; a byte cap that lands inside it must drop the whole rune.
	s := strings.Repeat("a", 127) + "€" // euro occupies bytes [127,130)
	got := capLen(s, 128)
	if !utf8.ValidString(got) {
		t.Fatalf("capLen produced invalid UTF-8: %q", got)
	}
	if len(got) != 127 {
		t.Fatalf("capLen len = %d, want 127 (partial rune dropped whole)", len(got))
	}
	// An exact-fit multi-byte string is untouched.
	fit := strings.Repeat("é", 64) // 128 bytes
	if got := capLen(fit, 128); got != fit {
		t.Fatalf("capLen shortened an exact-fit string")
	}
}

// The session-revocation epoch (bumped on logout) must persist across a restart:
// a controller freshly built from the same store reports the advanced value, so a
// logged-out token stays revoked instead of revalidating once the in-memory epoch
// resets. Regression for B12c.
func TestSessionEpochPersists(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	mk := func() *Controller {
		c, err := New(context.Background(), st, Values{
			Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2,
		})
		if err != nil {
			t.Fatalf("new controller: %v", err)
		}
		return c
	}
	c1 := mk()
	if got := c1.SessionEpoch(); got != 0 {
		t.Fatalf("fresh epoch = %d, want 0", got)
	}
	if err := c1.BumpSessionEpoch(context.Background()); err != nil {
		t.Fatalf("BumpSessionEpoch: %v", err)
	}
	if got := c1.SessionEpoch(); got != 1 {
		t.Fatalf("epoch after bump = %d, want 1", got)
	}
	// "Restart": a new controller over the same store must load the persisted epoch.
	if got := mk().SessionEpoch(); got != 1 {
		t.Fatalf("epoch after restart = %d, want 1 (logout must survive a restart)", got)
	}
}

// A username at the byte cap made of multi-byte runes must save and reload intact
// and valid; normalize must not slice it mid-codepoint. Regression for B7.
func TestAuthUserUnicodeBoundaryReload(t *testing.T) {
	c := newController(t)
	name := strings.Repeat("é", 64) // exactly 128 bytes
	if err := c.SetAuthUser(context.Background(), name); err != nil {
		t.Fatalf("SetAuthUser: %v", err)
	}
	if got := c.AuthUser(); got != name || !utf8.ValidString(got) {
		t.Fatalf("AuthUser round-trip = %q (valid=%v), want %q", got, utf8.ValidString(c.AuthUser()), name)
	}
	// An over-cap username is truncated at a rune boundary, staying valid UTF-8.
	over := strings.Repeat("é", 100) // 200 bytes
	if err := c.SetAuthUser(context.Background(), over); err != nil {
		t.Fatalf("SetAuthUser over-cap: %v", err)
	}
	if got := c.AuthUser(); !utf8.ValidString(got) || len(got) > 128 {
		t.Fatalf("over-cap username not rune-safe: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

// pv builds the pointer fields Patch uses (nil = keep current).
func pv[T any](v T) *T { return &v }

// newController returns a Controller backed by an in-memory store.
func newController(t *testing.T) *Controller {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	c, err := New(context.Background(), st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, Monitoring: true,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	return c
}

// Concurrent single-field setters must compose rather than clobber each other
// (each used to read-modify-write the whole Values without a writer lock).
// Run under -race to also catch unsynchronized access.
func TestConcurrentSettersCompose(t *testing.T) {
	c := newController(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if err := c.SetMonitoring(ctx, false); err != nil {
				t.Errorf("SetMonitoring: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := c.SetAccessLocalOnly(ctx, true); err != nil {
				t.Errorf("SetAccessLocalOnly: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if err := c.SetAuthPassword(ctx, "alice", "hash-x"); err != nil {
				t.Errorf("SetAuthPassword: %v", err)
			}
		}()
	}
	wg.Wait()

	v := c.Snapshot()
	if v.Monitoring {
		t.Error("SetMonitoring(false) was lost")
	}
	if !v.AccessLocalOnly {
		t.Error("SetAccessLocalOnly(true) was lost")
	}
	if v.AuthUser != "alice" || v.AuthHash != "hash-x" || !v.AuthEnabled {
		t.Errorf("SetAuthPassword was lost: user=%q hash=%q enabled=%v", v.AuthUser, v.AuthHash, v.AuthEnabled)
	}
}

// Update (the settings form) must not revert fields it doesn't own - the
// monitoring flag and the access/auth settings - in memory or in the DB.
func TestUpdatePreservesUnownedFields(t *testing.T) {
	c := newController(t)
	ctx := context.Background()

	if err := c.SetAuthPassword(ctx, "bob", "hash-b"); err != nil {
		t.Fatalf("SetAuthPassword: %v", err)
	}
	if err := c.SetMonitoring(ctx, false); err != nil {
		t.Fatalf("SetMonitoring: %v", err)
	}
	if err := c.SetAccessLocalOnly(ctx, true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}

	// A form submit has no fields for the unowned settings at all; they must
	// survive untouched.
	v, err := c.Update(ctx, Patch{
		Latency: pv(10 * time.Second), Timeout: pv(3 * time.Second),
		DownAfter: pv(2), UpAfter: pv(2),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if v.AuthUser != "bob" || v.AuthHash != "hash-b" || !v.AuthEnabled {
		t.Errorf("Update reverted auth: user=%q hash=%q enabled=%v", v.AuthUser, v.AuthHash, v.AuthEnabled)
	}
	if v.Monitoring || !v.AccessLocalOnly {
		t.Errorf("Update reverted monitoring/local-only: %v/%v", v.Monitoring, v.AccessLocalOnly)
	}
	if v.Latency != 10*time.Second {
		t.Errorf("Update did not apply latency: %v", v.Latency)
	}

	// And the DB must agree (Update must not have persisted unowned keys):
	// a reload from the store should still show the setter-written values.
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	v = c.Snapshot()
	if v.AuthUser != "bob" || v.AuthHash != "hash-b" || !v.AuthEnabled || v.Monitoring || !v.AccessLocalOnly {
		t.Errorf("DB state reverted after Update: %+v", v)
	}
}

// normalize must keep the saved iperf3 server list sane: trim, drop empty/flag-
// shaped addresses, dedupe by address (first wins), and cap the count.
func TestNormalizeIperfServers(t *testing.T) {
	base := Values{Latency: 10 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 2, UpAfter: 2}

	v := base
	v.IperfServers = []IperfTarget{
		{Label: "  NAS  ", Addr: "  10.0.0.5  "}, // trimmed
		{Label: "dup", Addr: "10.0.0.5"},         // duplicate addr -> dropped
		{Label: "empty", Addr: "   "},            // empty addr -> dropped
		{Label: "flag", Addr: "-R"},              // flag-shaped -> dropped
		{Label: "VPS", Addr: "vps.example.com:5201"},
	}
	got := normalize(v).IperfServers
	// sanitize stamps every entry's IP version to the canonical "auto" when unset.
	want := []IperfTarget{{Label: "NAS", Addr: "10.0.0.5", IPVer: "auto"}, {Label: "VPS", Addr: "vps.example.com:5201", IPVer: "auto"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sanitize: got %+v want %+v", got, want)
	}

	// Per-server path + auth fields are sanitized like the old global ones.
	v = base
	v.IperfServers = []IperfTarget{
		{Addr: "h:5201", Bind: "  10.0.0.1  ", IPVer: "9", Username: "  bob  ", RSAKey: "  KEY  ", Auth: true},
		{Addr: "h2:5201", Bind: "-evil", IPVer: "6"}, // flag-shaped bind dropped; valid ipver kept
	}
	got = normalize(v).IperfServers
	if len(got) != 2 {
		t.Fatalf("want 2 servers, got %d", len(got))
	}
	if got[0].Bind != "10.0.0.1" || got[0].IPVer != "auto" || got[0].Username != "bob" || got[0].RSAKey != "KEY" || !got[0].Auth {
		t.Errorf("server[0] sanitize = %+v", got[0])
	}
	if got[1].Bind != "" || got[1].IPVer != "6" {
		t.Errorf("server[1] sanitize = %+v (flag bind should drop, ipv6 keep)", got[1])
	}

	// Count cap.
	v = base
	for i := 0; i < maxIperfServers+5; i++ {
		v.IperfServers = append(v.IperfServers, IperfTarget{Addr: "h" + string(rune('a'+i))})
	}
	if n := len(normalize(v).IperfServers); n != maxIperfServers {
		t.Errorf("cap: got %d want %d", n, maxIperfServers)
	}

	// An active address with no saved list is left as-is (no migration/seeding).
	v = base
	v.IperfServer = "10.0.0.9:5201"
	if got := normalize(v).IperfServers; len(got) != 0 {
		t.Errorf("active addr must not seed the list, got %+v", got)
	}
}

// Each server's iperf3 password is write-only: a form submit with an empty password
// keeps the stored one (matched by address); a non-empty one replaces it. The active
// IperfServer selects which target's password the tester reads.
func TestUpdateKeepsIperfPassword(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	const addr = "10.0.0.5:5201"
	set := func(pw string) {
		p := Patch{IperfServer: pv(addr)}
		p.IperfServers = []IperfTarget{{Addr: addr, Auth: true, Username: "bob", Password: pw, RSAKey: "KEYPEM"}}
		if _, err := c.Update(ctx, p); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	set("secret")
	if c.IperfPassword() != "secret" {
		t.Fatalf("password not set: %q", c.IperfPassword())
	}
	set("") // blank submit keeps it (merged by address)
	if c.IperfPassword() != "secret" {
		t.Errorf("blank submit cleared the password: %q", c.IperfPassword())
	}
	set("rotated") // non-empty replaces it
	if c.IperfPassword() != "rotated" {
		t.Errorf("password not replaced: %q", c.IperfPassword())
	}
	// A blank password on a server that doesn't match the stored address stays blank.
	p := Patch{IperfServer: pv("other:5201")}
	p.IperfServers = []IperfTarget{{Addr: "other:5201", Auth: true, Username: "x", RSAKey: "K"}}
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c.IperfPassword() != "" {
		t.Errorf("unrelated server should have no password, got %q", c.IperfPassword())
	}
}

// The bind/ipver/auth getters resolve the saved server whose address matches the
// active IperfServer, so switching the active server switches the whole profile.
func TestActiveIperfResolution(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	p := Patch{}
	p.IperfServers = []IperfTarget{
		{Addr: "nas:5201", Bind: "10.0.0.2", IPVer: "4"},
		{Addr: "vps:5201", IPVer: "6", Auth: true, Username: "bob", Password: "pw", RSAKey: "KEY"},
	}
	p.IperfServer = pv("vps:5201")
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c.IperfIPVer() != "6" || !c.IperfAuth() || c.IperfUsername() != "bob" || c.IperfPassword() != "pw" || c.IperfRSAKey() != "KEY" || c.IperfBind() != "" {
		t.Errorf("active=vps resolved wrong: ipver=%q auth=%v user=%q bind=%q", c.IperfIPVer(), c.IperfAuth(), c.IperfUsername(), c.IperfBind())
	}
	// Switch the active server (creds keep, password merged by address).
	p.IperfServer = pv("nas:5201")
	p.IperfServers[1].Password = "" // blank submit on the unselected server -> kept
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c.IperfIPVer() != "4" || c.IperfBind() != "10.0.0.2" || c.IperfAuth() || c.IperfUsername() != "" {
		t.Errorf("active=nas resolved wrong: ipver=%q bind=%q auth=%v", c.IperfIPVer(), c.IperfBind(), c.IperfAuth())
	}
	// The vps password survived the round-trip even while nas was active.
	p.IperfServer = pv("vps:5201")
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c.IperfPassword() != "pw" {
		t.Errorf("vps password lost across active switch: %q", c.IperfPassword())
	}
	// An active address not in the list resolves to a zero profile (no auth, no bind,
	// empty IP version - which the tester's iperfIPVersion treats as auto).
	p.IperfServer = pv("ghost:5201")
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if c.IperfAuth() || c.IperfIPVer() != "" || c.IperfBind() != "" {
		t.Errorf("unmatched active should be zero profile: auth=%v ipver=%q", c.IperfAuth(), c.IperfIPVer())
	}
}

// A multi-byte/emoji value that exceeds a byte cap must be truncated on a rune
// boundary, never sliced mid-codepoint. The UI's maxlength counts UTF-16 units, so
// a multi-byte password/addr/bind/RSAKey can arrive over the byte cap and reach
// sanitizeIperfServers; a raw s[:n] there would store invalid UTF-8 that JSON/SQLite
// then mangle (the whole field silently corrupted). capLen backs the cut to a valid
// boundary.
func TestSanitizeIperfServersRuneSafeTruncation(t *testing.T) {
	const maxAddr, maxKey = 255, 8192 // mirror sanitizeIperfServers' local consts
	// Build each field so the byte cap lands INSIDE a 4-byte emoji, which a naive
	// slice would split. Offsetting bind/key by one ASCII byte guarantees the cap
	// doesn't happen to fall on a rune boundary for those fields.
	pw := strings.Repeat("😀", 64)          // 256 bytes; cap 255 lands mid-emoji
	addr := strings.Repeat("😀", 64)        // 256 bytes; cap 255 lands mid-emoji
	bind := "b" + strings.Repeat("😀", 64)  // 257 bytes; cap 255 lands mid-emoji
	key := "k" + strings.Repeat("😀", 2048) // 8193 bytes; cap 8192 lands mid-emoji

	// Sanity-check the hazard the fix defends against: a raw byte slice at the cap
	// really does corrupt these inputs, so a passing test is meaningful.
	if utf8.ValidString(pw[:maxAddr]) {
		t.Fatalf("test setup: pw[:%d] is already valid UTF-8; pick a value that splits a rune", maxAddr)
	}

	out := sanitizeIperfServers([]IperfTarget{{Addr: addr, Bind: bind, Password: pw, RSAKey: key}})
	if len(out) != 1 {
		t.Fatalf("want 1 sanitized server, got %d", len(out))
	}
	got := out[0]

	check := func(name, orig, val string, cap int) {
		t.Helper()
		if !utf8.ValidString(val) {
			t.Errorf("%s is not valid UTF-8 after sanitize: %q", name, val)
		}
		if len(val) > cap {
			t.Errorf("%s len = %d, want <= %d", name, len(val), cap)
		}
		// Truncation may only shorten; the kept part must be a rune-boundary prefix of
		// the input, never a mangled or substituted value.
		if !strings.HasPrefix(orig, val) {
			t.Errorf("%s was not a prefix-truncation of the input: got %q", name, val)
		}
		if len(val) == len(orig) {
			t.Errorf("%s was not truncated at all (len %d); the over-cap input should shrink", name, len(val))
		}
	}
	check("password", pw, got.Password, maxAddr)
	check("addr", addr, got.Addr, maxAddr)
	check("bind", bind, got.Bind, maxAddr)
	check("rsakey", key, got.RSAKey, maxKey)
}

// mkEpochController builds a controller over a caller-owned store, so a test can
// reach st.DB() to simulate a write failure (query_only) - newController hides its
// store. No crypter: these tests exercise only the session epoch, not sealing.
func mkEpochController(t *testing.T, st *store.Store) *Controller {
	t.Helper()
	c, err := New(context.Background(), st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2,
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	return c
}

// A Reload must NEVER lower the live session-revocation epoch. BumpSessionEpoch
// advances the in-memory epoch before it persists, so a failed persist leaves memory
// ahead of disk; a Reload that then reads the stale on-disk value must keep the
// higher live epoch (seedSessionEpoch stores max), or every token the logout revoked
// would revalidate. This is the bump-vs-reload race.
func TestReloadNeverLowersSessionEpoch(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	c := mkEpochController(t, st)

	// Advance the live epoch but make the PERSIST fail, so memory (1) runs ahead of
	// disk (still 0) - exactly the best-effort-write window BumpSessionEpoch documents.
	// The :memory: pool is a single connection, so query_only sticks across calls.
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		t.Fatalf("query_only on: %v", err)
	}
	if err := c.BumpSessionEpoch(ctx); err == nil {
		t.Fatalf("BumpSessionEpoch: expected a write error under query_only")
	}
	if got := c.SessionEpoch(); got != 1 {
		t.Fatalf("in-memory epoch = %d, want 1 (advance must survive the failed persist)", got)
	}
	if _, err := st.DB().ExecContext(ctx, "PRAGMA query_only = OFF"); err != nil {
		t.Fatalf("query_only off: %v", err)
	}

	// The Reload now reads epoch 0 off disk. It must NOT lower the live epoch back to 0
	// (that would un-revoke every token this logout revoked).
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := c.SessionEpoch(); got != 1 {
		t.Fatalf("epoch after reload = %d, want 1 (reload must never un-revoke)", got)
	}
}

// Two concurrent logouts must persist a monotonically non-decreasing epoch: their
// advance+persist serialize under the writer lock, so the on-disk value ends at the
// highest bump - a restart then loads it. Without the lock the two persists could
// land out of order and leave a stale epoch on disk. This is the bump-vs-bump
// race. Run under -race to also catch unsynchronized epoch access.
func TestConcurrentLogoutsPersistMonotonicEpoch(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	base := Values{Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2}
	c, err := New(ctx, st, base)
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.BumpSessionEpoch(ctx); err != nil {
				t.Errorf("BumpSessionEpoch: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := c.SessionEpoch(); got != 2 {
		t.Fatalf("live epoch after two logouts = %d, want 2", got)
	}
	// The persisted value must equal the live epoch - the later bump's write won,
	// nothing landed out of order.
	raw, err := st.AllSettings(ctx)
	if err != nil {
		t.Fatalf("AllSettings: %v", err)
	}
	if raw[keyAuthSessEpoch] != "2" {
		t.Fatalf("persisted epoch = %q, want \"2\" (out-of-order persist lost a logout)", raw[keyAuthSessEpoch])
	}
	// Simulated restart: a fresh controller over the same store loads the highest epoch.
	if got := mkEpochController(t, st).SessionEpoch(); got != 2 {
		t.Fatalf("epoch after restart = %d, want 2 (logouts must persist monotonically)", got)
	}
}

// formKeys (serialize) and overlay (parse) are two hand-maintained halves that
// must stay paired - a form field with a formKeys entry but no overlay case (or
// vice versa) silently breaks persistence. This round-trips every form-owned
// field through formKeys->overlay and asserts it survives; non-form fields stay
// at their zero value on both sides (overlay starts from Values{} and formKeys
// emits no key for them), so a full-struct compare is exact.
func TestFormKeysOverlayRoundTrip(t *testing.T) {
	want := Values{
		Latency: 10 * time.Second, LatencyEnabled: true,
		Speed:             3600 * time.Second,
		Retention:         7 * 24 * time.Hour,
		SpeedRetention:    30 * 24 * time.Hour,
		DowntimeRetention: 365 * 24 * time.Hour,
		Timeout:           5 * time.Second,
		DownAfter:         3, UpAfter: 2,
		SpeedServerID: "1234", SpeedAutoLoc: "45.5,-73.5", SpeedAutoLabel: "Montreal",
		SpeedServers: []SavedServer{
			{ID: "1234", Sponsor: "Bell", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5},
			{ID: "5678", Sponsor: "Rogers", Name: "Toronto, ON"}, // no coordinate: the 0,0 case must round-trip too
		},
		SpeedtestEnabled: true, SpeedtestOnReconnect: true, IPv6Mode: "on", ExitTarget: "8.8.8.8", DNSProbe: true, NetinfoEnabled: true,
		SpeedEngine: "iperf3", IperfServer: "10.0.0.5:5201",
		IperfServers: []IperfTarget{
			{Label: "NAS", Addr: "10.0.0.5:5201"},
			{Label: "VPS", Addr: "vps.example.com:5201", Bind: "10.0.0.1", IPVer: "6", Auth: true, Username: "bob", Password: "secret", RSAKey: "KEYPEM", PKCS1: true},
		},
		IperfDur: 15, IperfStreams: 4, OoklaConnections: 8, OoklaLoss: true, SpeedBestOf: true, IperfOmit: 2, SpeedDirection: "down", IperfUDP: true,
		// Direction/Retries are per-engine: distinct values here prove they round-trip independently.
		IperfDirection: "bidir", IperfRetries: 3,
		IperfUDPRate: 200, IperfWindow: 4096, SpeedRetries: 2,
		IperfCongestion: "bbr", IperfNoDelay: true, IperfDSCP: "ef", IperfMSS: 1400,
		ThreshDownMbps: 100, ThreshUpMbps: 20, ThreshPingMS: 50, ThreshJitterMS: 10,
		ThreshLossPct: 5, ThreshConsec: 3, ThreshBloatDownMS: 80, ThreshBloatUpMS: 90,
		AlertOnOutage: true, WebhookURL: "https://example.com/hook",
		HeartbeatURL: "https://hc.example.com/uuid", DigestFreq: "weekly",
		SchedLatEnabled: true, SchedLatWindows: []Window{{Days: "0111110", Start: 540, End: 1020}},
		SchedSpeedEnabled: true, SchedSpeedWindows: []Window{{Days: AllDays, Start: 0, End: 0}},
	}
	got := overlay(Values{}, formKeys(want))
	if !reflect.DeepEqual(want, got) {
		t.Errorf("formKeys->overlay is not symmetric (a form field is mapped in one half but not the other)\n want %+v\n got  %+v", want, got)
	}
}

// Update is a PATCH: fields absent from the patch (nil) must keep their stored
// value - in memory AND in the DB - and a nil slice keeps the list (with its
// write-only passwords) while an explicit empty slice clears it.
func TestUpdatePatchKeepsOmittedFields(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	if _, err := c.Update(ctx, Patch{
		LatencyEnabled: pv(true),
		WebhookURL:     pv("https://example.com/hook"),
		IperfServer:    pv("10.0.0.5:5201"),
		IperfServers:   []IperfTarget{{Addr: "10.0.0.5:5201", Auth: true, Password: "secret"}},
	}); err != nil {
		t.Fatalf("seed Update: %v", err)
	}

	// A partial patch touching one field leaves everything else alone.
	v, err := c.Update(ctx, Patch{SpeedtestEnabled: pv(false)})
	if err != nil {
		t.Fatalf("partial Update: %v", err)
	}
	if v.SpeedtestEnabled {
		t.Error("patched field did not apply")
	}
	if !v.LatencyEnabled || v.WebhookURL != "https://example.com/hook" {
		t.Errorf("omitted fields were reset: latency=%v webhook=%q", v.LatencyEnabled, v.WebhookURL)
	}
	if len(v.IperfServers) != 1 || v.IperfServers[0].Password != "secret" {
		t.Errorf("nil IperfServers must keep the stored list + password, got %+v", v.IperfServers)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if v := c.Snapshot(); !v.LatencyEnabled || v.WebhookURL == "" || len(v.IperfServers) != 1 {
		t.Errorf("DB state lost omitted fields after partial Update: %+v", v)
	}

	// An explicit empty list still clears (and drops its credentials with it).
	if _, err := c.Update(ctx, Patch{IperfServers: []IperfTarget{}}); err != nil {
		t.Fatalf("clear Update: %v", err)
	}
	if got := c.Snapshot().IperfServers; len(got) != 0 {
		t.Errorf("explicit empty list must clear, got %+v", got)
	}
}

// normalize must drop an unusable auto-picker location (NaN/Inf, out-of-range,
// malformed) to "" so the Ookla auto-select falls back instead of centring on
// garbage coordinates; valid pairs survive.
func TestNormalizeAutoLocation(t *testing.T) {
	for loc, want := range map[string]string{
		"45.5,-73.5":  "45.5,-73.5",
		" 45.5,-73.5": "45.5,-73.5", // trimmed
		"NaN,NaN":     "",
		"Inf,0":       "",
		"9999,9999":   "",
		"0,181":       "",
		"garbage":     "",
	} {
		v := normalize(Values{SpeedAutoLoc: loc})
		if v.SpeedAutoLoc != want {
			t.Errorf("normalize SpeedAutoLoc %q = %q, want %q", loc, v.SpeedAutoLoc, want)
		}
	}
}

// OoklaConnections clamps to [0, MaxOoklaConnections]; SpeedRetries clamps to
// [0, MaxSpeedRetries]; an unrecognized SpeedDirection falls back to "both".
func TestSpeedKnobsClamp(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	if _, err := c.Update(ctx, Patch{
		OoklaConnections: pv(99), SpeedDirection: pv("up"), SpeedRetries: pv(2),
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.OoklaConnections() != MaxOoklaConnections {
		t.Errorf("OoklaConnections=%d, want clamped to %d", c.OoklaConnections(), MaxOoklaConnections)
	}
	if c.SpeedDirection() != "up" {
		t.Errorf("SpeedDirection=%q, want \"up\"", c.SpeedDirection())
	}
	if c.SpeedRetries() != 2 {
		t.Errorf("SpeedRetries=%d, want 2", c.SpeedRetries())
	}

	if _, err := c.Update(ctx, Patch{SpeedDirection: pv("sideways"), SpeedRetries: pv(99)}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.SpeedDirection() != "both" {
		t.Errorf("SpeedDirection=%q, want \"both\" (unrecognized value falls back)", c.SpeedDirection())
	}
	if c.SpeedRetries() != MaxSpeedRetries {
		t.Errorf("SpeedRetries=%d, want clamped to %d", c.SpeedRetries(), MaxSpeedRetries)
	}
}

// Update normalizes hostile/out-of-range alert + digest values: a non-finite or
// negative threshold disables (0) so a crafted value can't silently turn an alert
// off; packet loss caps at 100; the consecutive-breach streak clamps to
// [MinStreak,MaxStreak]; an invalid digest cadence falls back to "off"; and valid
// values survive untouched.
func TestNormalizeThresholdGuards(t *testing.T) {
	c := newController(t)
	ctx := context.Background()

	got, err := c.Update(ctx, Patch{
		ThreshDownMbps:    pv(-5.0),        // negative -> 0
		ThreshPingMS:      pv(math.Inf(1)), // +Inf -> 0
		ThreshJitterMS:    pv(math.NaN()),  // NaN -> 0
		ThreshLossPct:     pv(150.0),       // >100 -> 100
		ThreshConsec:      pv(99),          // -> MaxStreak
		ThreshBloatDownMS: pv(-1.0),        // negative -> 0
		ThreshBloatUpMS:   pv(42.0),        // valid, survives
		DigestFreq:        pv("monthly"),   // invalid -> off
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.ThreshDownMbps != 0 || got.ThreshPingMS != 0 || got.ThreshJitterMS != 0 || got.ThreshBloatDownMS != 0 {
		t.Errorf("non-finite/negative thresholds must zero out: %+v", got)
	}
	if got.ThreshLossPct != 100 {
		t.Errorf("loss must cap at 100, got %v", got.ThreshLossPct)
	}
	if got.ThreshConsec != MaxStreak {
		t.Errorf("consecutive must clamp to MaxStreak(%d), got %d", MaxStreak, got.ThreshConsec)
	}
	if got.ThreshBloatUpMS != 42 {
		t.Errorf("valid bloat-up threshold must survive, got %v", got.ThreshBloatUpMS)
	}
	if got.DigestFreq != "off" {
		t.Errorf("invalid digest cadence must fall back to off, got %q", got.DigestFreq)
	}

	// A too-small streak clamps UP to the minimum; a valid cadence survives.
	got2, err := c.Update(ctx, Patch{ThreshConsec: pv(0), DigestFreq: pv("daily")})
	if err != nil {
		t.Fatalf("Update(2): %v", err)
	}
	if got2.ThreshConsec != MinStreak {
		t.Errorf("streak 0 must clamp up to MinStreak(%d), got %d", MinStreak, got2.ThreshConsec)
	}
	if got2.DigestFreq != "daily" {
		t.Errorf("valid cadence must survive, got %q", got2.DigestFreq)
	}
}

// normalize length-caps the free-form string settings so a crafted config
// import (64 MiB body, values stored verbatim) can't plant a multi-MB value
// that is re-persisted on every save, shipped on every GET, and (for Bind)
// handed to iperf3 as an argv element where an oversized arg fails execve.
func TestNormalizeLengthCaps(t *testing.T) {
	base := Values{Latency: 10 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 2, UpAfter: 2}
	big := strings.Repeat("x", 5000)

	v := base
	v.WebhookURL = "https://h/" + big
	v.HeartbeatURL = "https://h/" + big
	v.IperfServer = big
	v.SpeedAutoLabel = big
	v.SpeedServerID = big
	v.IperfServers = []IperfTarget{{Addr: "h:5201", Bind: big}}
	got := normalize(v)

	if len(got.WebhookURL) > maxURL {
		t.Errorf("WebhookURL not capped: len=%d", len(got.WebhookURL))
	}
	if len(got.HeartbeatURL) > maxURL {
		t.Errorf("HeartbeatURL not capped: len=%d", len(got.HeartbeatURL))
	}
	if len(got.IperfServer) > maxServerAddr {
		t.Errorf("IperfServer not capped: len=%d", len(got.IperfServer))
	}
	if len(got.SpeedAutoLabel) > maxLabelLen {
		t.Errorf("SpeedAutoLabel not capped: len=%d", len(got.SpeedAutoLabel))
	}
	if len(got.SpeedServerID) > maxServerID {
		t.Errorf("SpeedServerID not capped: len=%d", len(got.SpeedServerID))
	}
	if len(got.IperfServers) != 1 || len(got.IperfServers[0].Bind) > 255 {
		t.Errorf("iperf Bind not capped: %+v", got.IperfServers)
	}
}

// A password that itself begins with the seal prefix ("enc:v1:") collides with
// the at-rest encryption tag: Seal would pass it through in the clear and the
// next Unseal would blank it. sanitizeIperfServers must reject it at the input
// boundary so it can never be stored in that broken state.
func TestNormalizeRejectsSealPrefixPassword(t *testing.T) {
	base := Values{Latency: 10 * time.Second, Speed: time.Hour, Timeout: 3 * time.Second, DownAfter: 2, UpAfter: 2}
	v := base
	v.IperfServers = []IperfTarget{
		{Addr: "collide:5201", Auth: true, Password: "enc:v1:hunter2"},
		{Addr: "ok:5201", Auth: true, Password: "realpassword"},
	}
	got := normalize(v).IperfServers
	if len(got) != 2 {
		t.Fatalf("want 2 servers, got %d", len(got))
	}
	if got[0].Password != "" {
		t.Errorf("seal-prefix password must be rejected, got %q", got[0].Password)
	}
	if got[1].Password != "realpassword" {
		t.Errorf("a normal password must survive, got %q", got[1].Password)
	}
}

// After the initial settings load fails, the controller runs on defaults; a form
// save must be refused (not silently persist defaults over the stored config).
func TestUpdateRefusedAfterInitReadFailure(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.Close() // make the subsequent AllSettings read fail

	c, err := New(context.Background(), st, Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second, DownAfter: 3, UpAfter: 2,
	})
	if err == nil {
		t.Fatal("New should have surfaced the read error")
	}
	if _, uerr := c.Update(context.Background(), Patch{SpeedtestEnabled: pv(false)}); uerr != ErrSettingsUnavailable {
		t.Fatalf("Update after failed init = %v, want ErrSettingsUnavailable", uerr)
	}
	if serr := c.SetMonitoring(context.Background(), true); serr != ErrSettingsUnavailable {
		t.Fatalf("SetMonitoring after failed init = %v, want ErrSettingsUnavailable", serr)
	}
}

// AuthUser is a free-form string that a config import can set verbatim
// (auth_user isn't in the export deny list). normalize must length-cap it like
// every sibling string, and still default a blank to admin.
func TestNormalizeCapsAuthUser(t *testing.T) {
	if got := normalize(Values{AuthUser: strings.Repeat("x", 10_000)}).AuthUser; len(got) != maxUser {
		t.Fatalf("oversized AuthUser len = %d, want capped to %d", len(got), maxUser)
	}
	if got := normalize(Values{AuthUser: "   "}).AuthUser; got != "admin" {
		t.Fatalf("blank AuthUser = %q, want admin", got)
	}
	if got := normalize(Values{AuthUser: "operator"}).AuthUser; got != "operator" {
		t.Fatalf("normal AuthUser = %q, want operator", got)
	}
}

// An enabled schedule with no valid windows is forced off in normalize, so the
// gate reads as "unscheduled / always allowed" instead of silently answering
// "never" and disabling the whole feature.
func TestNormalizeEmptyScheduleForcesFlagOff(t *testing.T) {
	got := normalize(Values{SchedSpeedEnabled: true, SchedLatEnabled: true})
	if got.SchedSpeedEnabled {
		t.Error("SchedSpeedEnabled with no windows must be forced off")
	}
	if got.SchedLatEnabled {
		t.Error("SchedLatEnabled with no windows must be forced off")
	}
	// A schedule WITH a valid window stays enabled.
	w := []Window{{Days: "1111111", Start: 60, End: 120}}
	got = normalize(Values{
		SchedSpeedEnabled: true, SchedSpeedWindows: w,
		SchedLatEnabled: true, SchedLatWindows: w,
	})
	if !got.SchedSpeedEnabled || len(got.SchedSpeedWindows) != 1 {
		t.Errorf("valid speed window: enabled=%v windows=%d, want true/1", got.SchedSpeedEnabled, len(got.SchedSpeedWindows))
	}
	if !got.SchedLatEnabled || len(got.SchedLatWindows) != 1 {
		t.Errorf("valid lat window: enabled=%v windows=%d, want true/1", got.SchedLatEnabled, len(got.SchedLatWindows))
	}
}

// Retention windows clamp to [0, MaxDuration]: the API pointer path skips secs(),
// so normalize must cap a multi-century value the load path would otherwise
// silently rewrite on reload, and floor a negative to 0.
func TestNormalizeClampsRetention(t *testing.T) {
	got := normalize(Values{
		Retention:         50 * 365 * 24 * time.Hour, // 50y, far past the 10y MaxDuration
		SpeedRetention:    -5 * time.Hour,            // negative
		DowntimeRetention: 0,                         // keep forever
	})
	if got.Retention != MaxDuration {
		t.Errorf("Retention = %v, want clamped to %v", got.Retention, MaxDuration)
	}
	if got.SpeedRetention != 0 {
		t.Errorf("negative SpeedRetention = %v, want 0", got.SpeedRetention)
	}
	if got.DowntimeRetention != 0 {
		t.Errorf("DowntimeRetention 0 (forever) = %v, want 0", got.DowntimeRetention)
	}
}
