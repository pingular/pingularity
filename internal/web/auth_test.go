package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/stats"
	"github.com/pingular/pingularity/internal/store"
)

// newTestServer returns a Server backed by an in-memory store, with no auth
// configured yet. Logs are discarded; newTestServerLog captures them.
func newTestServer(t *testing.T) *Server { return newTestServerLog(t, io.Discard) }

func newTestServerLog(t *testing.T, logDst io.Writer) *Server {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(context.Background(), st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2,
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}
	log := slog.New(slog.NewTextHandler(logDst, nil))
	return New(st, nil, nil, set, nil, "test", log)
}

// do runs one request through the full handler chain (mux + guard).
func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json") // match the SPA; required by decodeJSONBody
	}
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// counter reads one live stats counter (the registry is process-global, so
// tests that assert on it reset it first).
func counter(name string) int64 { return stats.Lifetime().Counters[name] }

// setPassword stores a (cheap, MinCost) bcrypt hash so auth is active.
func setPassword(t *testing.T, s *Server, user, pass string) {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := s.settings.SetAuthPassword(context.Background(), user, string(h)); err != nil {
		t.Fatalf("SetAuthPassword: %v", err)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:1234", true},
		{"[::1]:80", true},
		{"127.0.0.1", true}, // no port
		{"192.168.1.5:9000", false},
		{"8.8.8.8:443", false},
		{"[2606:4700::1111]:443", false},
		{"garbage", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}

func TestToken(t *testing.T) {
	const hash = "$2a$10$abcdefghijklmnopqrstuv" // stand-in bcrypt-shaped key
	now := time.Now()
	tok := issueToken("admin", hash, 0, now)

	if !verifyToken(tok, "admin", hash, 0) {
		t.Fatal("freshly issued token should verify")
	}
	// A different hash (i.e. a changed password) must invalidate the token.
	if verifyToken(tok, "admin", hash+"x", 0) {
		t.Fatal("token must not verify under a changed password hash")
	}
	// A renamed account must invalidate tokens issued under the old name.
	if verifyToken(tok, "root", hash, 0) {
		t.Fatal("token must not verify after the username changed")
	}
	// A bumped epoch (logout) must invalidate a token issued under the old one.
	if verifyToken(tok, "admin", hash, 1) {
		t.Fatal("token must not verify after the session epoch advanced (logout)")
	}
	// Tampering with the payload must fail the MAC.
	if verifyToken("YWRtaW58OTk5OQ."+tok[len(tok)-10:], "admin", hash, 0) {
		t.Fatal("tampered token must not verify")
	}
	// An expired token must fail.
	old := issueToken("admin", hash, 0, now.Add(-sessionTTL-time.Hour))
	if verifyToken(old, "admin", hash, 0) {
		t.Fatal("expired token must not verify")
	}
	if verifyToken("nope", "admin", hash, 0) || verifyToken("", "admin", hash, 0) {
		t.Fatal("malformed token must not verify")
	}
}

func TestAuthExempt(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		{"GET", "/", true},           // static UI shell
		{"GET", "/index.html", true}, // static asset
		{"POST", "/api/auth/login", true},
		{"POST", "/api/auth/logout", true},
		{"GET", "/api/access", true},   // status drives the login overlay
		{"POST", "/api/access", false}, // changing access is gated
		{"GET", "/api/status", false},  // data is gated
		{"GET", "/metrics", false},     // metrics gated (use Basic)
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		r.Host = "127.0.0.1:9000"
		if got := authExempt(r); got != c.want {
			t.Errorf("authExempt(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// GET /api/access is reachable without a session (it drives the login
// overlay), so it must not disclose the username, port, or LAN URLs to
// unauthenticated callers while auth is enforced.
func TestAccessStatusHidesDetailsWhenUnauthenticated(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")

	// Anonymous request: only the overlay flags may appear.
	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "127.0.0.1:9000"
	out := s.accessStatus(r)
	if out["authed"] != false {
		t.Fatalf("anonymous request reported authed: %v", out)
	}
	for _, k := range []string{"user", "port", "lan_urls"} {
		if _, leaked := out[k]; leaked {
			t.Errorf("unauthenticated access status leaks %q", k)
		}
	}

	// Authenticated request (session cookie): full details for the access tab.
	r = httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "127.0.0.1:9000"
	r.AddCookie(&http.Cookie{Name: sessionCookie,
		Value: issueToken("admin", s.settings.AuthHash(), s.settings.SessionEpoch(), time.Now())})
	out = s.accessStatus(r)
	if out["authed"] != true {
		t.Fatalf("session cookie not accepted: %v", out)
	}
	for _, k := range []string{"user", "port", "lan_urls"} {
		if _, ok := out[k]; !ok {
			t.Errorf("authenticated access status missing %q", k)
		}
	}
	if out["user"] != "admin" {
		t.Errorf("user = %v, want admin", out["user"])
	}

	// With auth off entirely the details are public again (the tab needs them).
	if err := s.settings.ClearAuth(context.Background()); err != nil {
		t.Fatalf("ClearAuth: %v", err)
	}
	out = s.accessStatus(httptest.NewRequest("GET", "/api/access", nil))
	if out["authed"] != true {
		t.Fatalf("auth-off request not authed: %v", out)
	}
	if _, ok := out["lan_urls"]; !ok {
		t.Error("auth-off access status missing lan_urls")
	}
}

// After maxLoginFails consecutive failures from one IP, login attempts are
// refused with 429 until the cool-down passes; other IPs are unaffected and a
// success resets the streak. Failed attempts and limiter rejections must also
// move the operational counters.
func TestLoginRateLimit(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")

	login := func(ip, pass string) int {
		body := bytes.NewBufferString(`{"username":"admin","password":"` + pass + `"}`)
		r := httptest.NewRequest("POST", "/api/auth/login", body)
		r.Header.Set("Content-Type", "application/json")
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = ip + ":12345"
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		return w.Code
	}

	for i := 0; i < maxLoginFails; i++ {
		if code := login("10.0.0.1", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, code)
		}
	}
	if code := login("10.0.0.1", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: got %d, want 429", maxLoginFails, code)
	}
	// The right password is refused while blocked too - no success has cached
	// it yet, so nothing can vouch for it without bcrypt work.
	if code := login("10.0.0.1", "secret"); code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP with correct password: got %d, want 429", code)
	}
	// A different IP is unaffected, and its success resets its own streak.
	if code := login("10.0.0.2", "secret"); code != http.StatusOK {
		t.Fatalf("other IP: got %d, want 200", code)
	}
	// Counters: one login_fail per wrong-password 401 (not per 429), one
	// limiter_trip per 429 (the wrong AND the correct password while blocked).
	if got := counter("web.login_fail"); got != maxLoginFails {
		t.Errorf("web.login_fail = %d, want %d", got, maxLoginFails)
	}
	if got := counter("web.limiter_trips"); got != 2 {
		t.Errorf("web.limiter_trips = %d, want 2", got)
	}
	// That success cached the credentials, so the blocked IP presenting the
	// same valid pair now gets through - only failures spend budget...
	if code := login("10.0.0.1", "secret"); code != http.StatusOK {
		t.Fatalf("blocked IP with known-good password: got %d, want 200", code)
	}
	// ...while wrong passwords from it stay refused (the bypass must not
	// clear the bucket).
	if code := login("10.0.0.1", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP with wrong password after bypass: got %d, want 429", code)
	}
}

// A username rename must invalidate the known-good bypass: the cached
// fingerprint is keyed on the (unchanged) password hash, so without the
// username check the OLD username plus the correct password would still be
// waved through a blocked bucket even though checkPassword now rejects it.
func TestKnownGoodRejectsRenamedUsername(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")

	login := func(ip, user, pass string) int {
		body := bytes.NewBufferString(`{"username":"` + user + `","password":"` + pass + `"}`)
		r := httptest.NewRequest("POST", "/api/auth/login", body)
		r.Header.Set("Content-Type", "application/json")
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = ip + ":12345"
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		return w.Code
	}

	// Legit login caches the known-good fingerprint for "admin".
	if code := login("10.0.0.1", "admin", "secret"); code != http.StatusOK {
		t.Fatalf("initial login: got %d, want 200", code)
	}
	// Rename the account; the password hash is untouched.
	if err := s.settings.SetAuthUser(context.Background(), "root"); err != nil {
		t.Fatalf("SetAuthUser: %v", err)
	}
	// Block the bucket with wrong attempts (the correct new pair also blocks the
	// bucket, but any wrong password does).
	for i := 0; i < maxLoginFails; i++ {
		login("10.0.0.1", "root", "wrong")
	}
	if code := login("10.0.0.1", "root", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("bucket not blocked: got %d, want 429", code)
	}
	// The OLD username with the correct password must NOT be waved through.
	if code := login("10.0.0.1", "admin", "secret"); code == http.StatusOK {
		t.Fatal("stale username bypassed the block via known-good cache")
	}
}

// Repeated HTTP Basic failures through the guard are throttled with 429 too.
func TestBasicAuthRateLimit(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	get := func(ip, pass string) int {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = ip + ":12345"
		r.SetBasicAuth("admin", pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := 0; i < maxLoginFails; i++ {
		if code := get("10.0.0.9", "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, code)
		}
	}
	if code := get("10.0.0.9", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: got %d, want 429", maxLoginFails, code)
	}
	if code := get("10.0.0.10", "secret"); code != http.StatusOK {
		t.Fatalf("other IP with correct password: got %d, want 200", code)
	}
	// The Basic path counts failures and limiter trips too.
	if got := counter("web.login_fail"); got != maxLoginFails {
		t.Errorf("web.login_fail = %d, want %d", got, maxLoginFails)
	}
	if got := counter("web.limiter_trips"); got != 1 {
		t.Errorf("web.limiter_trips = %d, want 1", got)
	}
	// A valid-credential caller (e.g. a Prometheus scrape) must not be lockable
	// by someone else's failures: after any success cached the credentials, the
	// blocked IP presenting them still gets through, while wrong passwords from
	// it stay refused.
	if code := get("10.0.0.9", "secret"); code != http.StatusOK {
		t.Fatalf("blocked IP with known-good credentials: got %d, want 200", code)
	}
	if code := get("10.0.0.9", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("blocked IP with wrong password after bypass: got %d, want 429", code)
	}
}

// The DNS-rebinding guard's Host policy: direct/local forms pass with no
// configuration, public FQDNs only via -allow-host.
func TestHostAllowed(t *testing.T) {
	cases := []struct {
		host  string
		extra []string
		want  bool
	}{
		{"192.168.4.43:9000", nil, true},                              // LAN IP literal
		{"127.0.0.1:9000", nil, true},                                 // loopback
		{"[::1]:9000", nil, true},                                     // IPv6 literal with port
		{"[2001:db8::1]", nil, true},                                  // IPv6 literal without port
		{"localhost:9000", nil, true},                                 // localhost
		{"foo.localhost", nil, true},                                  // *.localhost
		{"plex", nil, true},                                           // dotless LAN name
		{"plex:9000", nil, true},                                      // dotless with port
		{"plex.local:9000", nil, true},                                // mDNS
		{"router.lan", nil, true},                                     // router suffix
		{"nas.home.arpa", nil, true},                                  // RFC 8375
		{"ping.example.com:9000", nil, false},                         // public FQDN: rejected…
		{"ping.example.com:9000", []string{"ping.example.com"}, true}, // …unless allowed
		{"PING.Example.COM", []string{"ping.example.com"}, true},      // case-folded
		{"evil.example.net", []string{"ping.example.com"}, false},
		{"example.com.", nil, false}, // trailing dot must not bypass
		{"", nil, false},
		// DNS-rebinding guard is exact-equality, never a suffix/substring match:
		// a neighbouring public name that merely ends with, starts with, or
		// contains the allowed host must NOT pass.
		{"evilping.example.com", []string{"ping.example.com"}, false},      // substring at the front
		{"xping.example.com", []string{"ping.example.com"}, false},         // one-letter prefix
		{"ping.example.com.evil.com", []string{"ping.example.com"}, false}, // allowed host as a left label
		{"aping.example.com:9000", []string{"ping.example.com"}, false},    // prefix, with port
		{"ping.example.com", []string{"ping.example.com"}, true},           // exact
		{"PING.Example.COM", []string{"ping.example.com"}, true},           // case-folded exact
		{"ping.example.com.", []string{"ping.example.com"}, true},          // trailing dot is stripped
		{"ping.example.com:9000", []string{"ping.example.com"}, true},      // exact, with port
	}
	for _, c := range cases {
		if got := hostAllowed(c.host, c.extra); got != c.want {
			t.Errorf("hostAllowed(%q, %v) = %v, want %v", c.host, c.extra, got, c.want)
		}
	}
}

// End-to-end: a request carrying a public-FQDN Host (the rebinding shape) is
// refused by the guard before any handler runs; the same request with a LAN
// Host passes.
func TestGuardRejectsRebindingHost(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "attacker-rebind.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("rebinding Host: code=%d, want 403", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "192.168.4.43:9000"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatalf("LAN-IP Host rejected: %d %s", w.Code, w.Body.String())
	}

	// -allow-host admits a deliberate reverse-proxy domain.
	s.AllowedHosts = []string{"ping.example.com"}
	r = httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "ping.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusForbidden {
		t.Fatalf("allow-listed Host rejected: %d", w.Code)
	}
}

// IPv6 peers are bucketed by /64: rotating SLAAC addresses within one prefix
// must not mint fresh failure budgets, while another /64 is unaffected.
func TestLoginRateLimitIPv6Prefix(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")

	login := func(addr, pass string) int {
		body := bytes.NewBufferString(`{"username":"admin","password":"` + pass + `"}`)
		r := httptest.NewRequest("POST", "/api/auth/login", body)
		r.Header.Set("Content-Type", "application/json")
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		return w.Code
	}

	for i := 0; i < maxLoginFails; i++ {
		addr := fmt.Sprintf("[2001:db8::%x]:1234", i+1)
		if code := login(addr, "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("failure %d from %s: got %d, want 401", i+1, addr, code)
		}
	}
	if code := login("[2001:db8::ffff]:1234", "wrong"); code != http.StatusTooManyRequests {
		t.Fatalf("fresh address in the blocked /64: got %d, want 429", code)
	}
	if code := login("[2001:db8:0:1::1]:1234", "secret"); code != http.StatusOK {
		t.Fatalf("different /64: got %d, want 200", code)
	}
}

// limiterKey collapses IPv6 to /64, keeps IPv4 as-is, and behind a declared
// trusted proxy keys on the hop that proxy appended to X-Forwarded-For.
func TestLimiterKey(t *testing.T) {
	s := newTestServer(t)
	req := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	if k1, k2 := s.limiterKey(req("[2001:db8::1]:1", "")), s.limiterKey(req("[2001:db8::2:3]:1", "")); k1 != k2 {
		t.Errorf("same /64 must share a bucket: %q vs %q", k1, k2)
	}
	if k1, k2 := s.limiterKey(req("[2001:db8::1]:1", "")), s.limiterKey(req("[2001:db8:0:1::1]:1", "")); k1 == k2 {
		t.Errorf("different /64s must not share a bucket: %q", k1)
	}
	if got := s.limiterKey(req("10.0.0.1:99", "")); got != "10.0.0.1" {
		t.Errorf("IPv4 key = %q, want 10.0.0.1", got)
	}
	// X-Forwarded-For from an undeclared peer is ignored (spoofable)...
	if got := s.limiterKey(req("10.0.0.1:99", "1.2.3.4")); got != "10.0.0.1" {
		t.Errorf("untrusted XFF honored: key = %q, want 10.0.0.1", got)
	}
	// ...and only the rightmost hop counts from a trusted proxy (earlier hops
	// are client-supplied).
	if err := s.SetTrustedProxies([]string{"127.0.0.1/32"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	if got := s.limiterKey(req("127.0.0.1:99", "spoofed, 1.2.3.4")); got != "1.2.3.4" {
		t.Errorf("trusted XFF key = %q, want 1.2.3.4", got)
	}
	if got := s.limiterKey(req("127.0.0.1:99", "")); got != "127.0.0.1" {
		t.Errorf("trusted peer without XFF: key = %q, want 127.0.0.1", got)
	}
	// Two-proxy chain: the TCP peer AND the nearest XFF hop are both trusted
	// proxies; the walk must skip them and key on the real client further left.
	// Taking only the rightmost hop would collapse every client onto 10.9.9.9.
	if err := s.SetTrustedProxies([]string{"127.0.0.1/32", "10.9.9.9/32"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}
	if got := s.limiterKey(req("127.0.0.1:99", "203.0.113.7, 10.9.9.9")); got != "203.0.113.7" {
		t.Errorf("proxy-chain key = %q, want 203.0.113.7 (real client)", got)
	}
	// All hops trusted (no real client in the header): fall back to the furthest.
	if got := s.limiterKey(req("127.0.0.1:99", "10.9.9.9")); got != "10.9.9.9" {
		t.Errorf("all-trusted chain key = %q, want 10.9.9.9", got)
	}
	if err := s.SetTrustedProxies([]string{"not-a-cidr"}); err == nil {
		t.Error("SetTrustedProxies must reject garbage")
	}
}

// A password over bcrypt's 72-byte limit is a clear 400, not an opaque 500.
func TestAccessRejectsOverlongPassword(t *testing.T) {
	s := newTestServer(t)
	long := strings.Repeat("a", 73)
	if w := do(t, s.Handler(), "POST", "/api/access", `{"password":"`+long+`"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("73-byte password: got %d, want 400", w.Code)
	}
	if s.settings.HasPassword() {
		t.Error("overlong password must not be stored")
	}
}

// Setting a password normally turns auth on, but an explicit
// auth_enabled:false in the same request wins: the password is stored and the
// login switch stays off.
func TestAccessPasswordWithExplicitDisable(t *testing.T) {
	s := newTestServer(t)
	w := do(t, s.Handler(), "POST", "/api/access", `{"password":"pw","auth_enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/access: %d %s", w.Code, w.Body.String())
	}
	if !s.settings.HasPassword() {
		t.Error("password not stored")
	}
	if s.settings.AuthEnabled() || s.settings.AuthActive() {
		t.Error("explicit auth_enabled:false was overridden by the password branch")
	}
}

// The session cookie's Secure attribute is decided per request: on under the
// public -allow-host domain (the TLS-proxy path), off for IP-literal Hosts so
// login over the advertised plain-HTTP LAN URLs keeps working.
func TestSecureCookiePerRequest(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	s.AllowedHosts = []string{"ping.example.com"}
	// The reverse proxy sits on loopback; declare it trusted so its forwarded
	// scheme is believed. A peer outside this set is untrusted (see below).
	const trustedPeer = "127.0.0.1:5555"
	if err := s.SetTrustedProxies([]string{"127.0.0.1/32"}); err != nil {
		t.Fatalf("SetTrustedProxies: %v", err)
	}

	login := func(peer, host, xfp string) *http.Cookie {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(`{"username":"admin","password":"secret"}`))
		r.Header.Set("Content-Type", "application/json")
		r.RemoteAddr = peer
		r.Host = host
		if xfp != "" {
			r.Header.Set("X-Forwarded-Proto", xfp)
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("login via %s: %d", host, w.Code)
		}
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie {
				return c
			}
		}
		t.Fatalf("no session cookie from %s", host)
		return nil
	}

	if c := login(trustedPeer, "ping.example.com", ""); !c.Secure {
		t.Error("public-domain login must set a Secure cookie")
	} else {
		// The rest of the hardening flags are fixed regardless of scheme.
		if !c.HttpOnly {
			t.Error("session cookie must be HttpOnly (unreadable to page scripts)")
		}
		if c.SameSite != http.SameSiteStrictMode {
			t.Errorf("session cookie SameSite = %v, want Strict", c.SameSite)
		}
		if c.Path != "/" {
			t.Errorf("session cookie Path = %q, want /", c.Path)
		}
		if c.MaxAge != int(sessionTTL.Seconds()) {
			t.Errorf("session cookie MaxAge = %d, want %d", c.MaxAge, int(sessionTTL.Seconds()))
		}
	}
	if c := login(trustedPeer, "192.168.1.5:9000", ""); c.Secure {
		t.Error("LAN-IP login must not set a Secure cookie (browsers drop it over http)")
	}
	// Trusted proxy that rewrote Host to a loopback literal but set
	// X-Forwarded-Proto: the request is really HTTPS, so the cookie must be Secure.
	if c := login(trustedPeer, "127.0.0.1:9000", "https"); !c.Secure {
		t.Error("proxied https (X-Forwarded-Proto) from a trusted proxy must set a Secure cookie despite a rewritten Host")
	}
	// Genuine LAN access reporting http must stay non-Secure so login works.
	if c := login(trustedPeer, "192.168.1.5:9000", "http"); c.Secure {
		t.Error("plain-http LAN access must not set a Secure cookie")
	}
	// A multi-proxy chain sends a list; the leftmost (client) scheme decides.
	if c := login(trustedPeer, "127.0.0.1:9000", "https, https"); !c.Secure {
		t.Error("multi-hop https X-Forwarded-Proto must set a Secure cookie")
	}
	// B10: X-Forwarded-Proto from an UNTRUSTED peer must be ignored - a direct
	// client must not be able to force a Secure cookie with a forged header. With
	// the Host rewritten to loopback and no trusted proxy vouching, the cookie
	// falls back to the Host decision (non-Secure).
	if c := login("203.0.113.7:44321", "127.0.0.1:9000", "https"); c.Secure {
		t.Error("X-Forwarded-Proto from an untrusted peer must not set a Secure cookie")
	}
}

// Logout is a mutating endpoint: POST-only with the JSON content-type CSRF
// guard, so a link prefetch or a cross-site form can't force a logout.
func TestLogoutMethodAndCSRFGuard(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	if w := do(t, h, "GET", "/api/auth/logout", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET logout: %d, want 405", w.Code)
	}
	if w := do(t, h, "POST", "/api/auth/logout", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("POST logout without JSON content-type: %d, want 415", w.Code)
	}
	w := do(t, h, "POST", "/api/auth/logout", "{}")
	if w.Code != http.StatusOK {
		t.Fatalf("POST logout: %d", w.Code)
	}
	var cleared *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie {
			cleared = c
		}
	}
	if cleared == nil || cleared.MaxAge >= 0 {
		t.Fatalf("logout must clear the session cookie (MaxAge<0), got %+v", cleared)
	}
	if cleared.Secure {
		t.Error("IP-literal Host logout: clear cookie must not be Secure")
	}
	// The clearing cookie mirrors the session cookie's hardening flags so it
	// actually overwrites it (a mismatched attribute leaves a second cookie).
	if !cleared.HttpOnly {
		t.Error("logout clear cookie must be HttpOnly")
	}
	if cleared.SameSite != http.SameSiteStrictMode {
		t.Errorf("logout clear cookie SameSite = %v, want Strict", cleared.SameSite)
	}
	if cleared.Path != "/" {
		t.Errorf("logout clear cookie Path = %q, want /", cleared.Path)
	}
}

// The served handler chain (mux + guard) must actually enforce auth on a gated
// endpoint: a refactor that dropped the guard wrap would leave /api/settings and
// friends open with every other test still green. This exercises the real chain.
func TestHandlerEnforcesAuth(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	// No credentials: refused through the full chain.
	r := httptest.NewRequest("GET", "/api/settings", nil)
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/settings through Handler(): got %d, want 401", w.Code)
	}

	// A valid session cookie is accepted.
	r = httptest.NewRequest("GET", "/api/settings", nil)
	r.Host = "127.0.0.1:9000"
	r.AddCookie(&http.Cookie{Name: sessionCookie,
		Value: issueToken("admin", s.settings.AuthHash(), s.settings.SessionEpoch(), time.Now())})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("session-cookie /api/settings through Handler(): got %d, want 200", w.Code)
	}
}

// Logout must revoke the stateless token itself, not just the browser's cookie
// copy: the exact token presented at logout must stop authenticating.
func TestLogoutRevokesToken(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	lr := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"username":"admin","password":"secret"}`))
	lr.Header.Set("Content-Type", "application/json")
	lr.Host = "127.0.0.1:9000"
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from login")
	}

	authed := func() int {
		r := httptest.NewRequest("GET", "/api/settings", nil)
		r.Host = "127.0.0.1:9000"
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := authed(); code != http.StatusOK {
		t.Fatalf("token before logout: got %d, want 200", code)
	}

	or := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader("{}"))
	or.Header.Set("Content-Type", "application/json")
	or.Host = "127.0.0.1:9000"
	or.AddCookie(cookie)
	ow := httptest.NewRecorder()
	h.ServeHTTP(ow, or)
	if ow.Code != http.StatusOK {
		t.Fatalf("logout: %d", ow.Code)
	}
	if code := authed(); code != http.StatusUnauthorized {
		t.Fatalf("captured token after logout: got %d, want 401", code)
	}
}

// reserve caps concurrent password evaluations per bucket at maxLoginFails, so a
// burst that arrives before any failure is recorded can't each slip past the
// budget and all pay a bcrypt compare.
func TestReserveCapsInFlight(t *testing.T) {
	l := newFailLimiter()
	for i := 0; i < maxLoginFails; i++ {
		if !l.reserve("k") {
			t.Fatalf("reserve %d should be granted", i+1)
		}
	}
	if l.reserve("k") {
		t.Fatal("reserve past the in-flight budget must be refused (concurrent burst bypass)")
	}
	if !l.reserve("other") {
		t.Fatal("a different bucket must have its own budget")
	}
	// Completing a reservation as a failure keeps the bucket at its cap.
	l.releaseFail("k")
	if l.reserve("k") {
		t.Fatal("bucket at the failure cap must stay blocked")
	}
}

// A password-only rotation (no username field) must keep the current login name,
// not silently reset it to "admin"; a genuine first-time setup still defaults to
// "admin".
func TestAccessPasswordRotationKeepsUsername(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	// First-time setup enables auth and returns a session cookie for the setter.
	first := do(t, h, "POST", "/api/access", `{"username":"alice","password":"first"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("initial set: %d %s", first.Code, first.Body)
	}
	if s.settings.AuthUser() != "alice" {
		t.Fatalf("username = %q, want alice", s.settings.AuthUser())
	}
	var cookie *http.Cookie
	for _, c := range first.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from initial set")
	}
	// Password-only rotation (now a gated request): authenticate with the session.
	rot := httptest.NewRequest("POST", "/api/access", strings.NewReader(`{"password":"second","current_password":"first"}`))
	rot.Header.Set("Content-Type", "application/json")
	rot.Host = "127.0.0.1:9000"
	rot.AddCookie(cookie)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, rot)
	if rw.Code != http.StatusOK {
		t.Fatalf("rotation: %d %s", rw.Code, rw.Body)
	}
	if got := s.settings.AuthUser(); got != "alice" {
		t.Fatalf("password-only rotation renamed the user to %q; want alice", got)
	}

	s2 := newTestServer(t)
	if w := do(t, s2.Handler(), "POST", "/api/access", `{"password":"pw"}`); w.Code != http.StatusOK {
		t.Fatalf("first-time set: %d %s", w.Code, w.Body)
	}
	if got := s2.settings.AuthUser(); got != "admin" {
		t.Fatalf("first-time no-username set: user = %q, want admin", got)
	}
}

// decodeJSONBody writes its own 415/400; handleAccess must not append a second
// error line on top of it.
func TestAccessBadContentTypeNoDoubleError(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest("POST", "/api/access", strings.NewReader("not json"))
	r.Header.Set("Content-Type", "text/plain")
	r.Host = "127.0.0.1:9000"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content-type: got %d, want 415", w.Code)
	}
	if strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("handler appended a second error line after decodeJSONBody: %q", w.Body.String())
	}
}

// A request carrying X-Forwarded-For while no -allow-host is configured means
// a proxy is likely rewriting Host past the rebinding guard; the operator gets
// a one-shot log warning.
func TestProxyWithoutAllowHostWarning(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServerLog(t, &buf)
	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("proxied request: %d, want 200", w.Code)
	}
	if !strings.Contains(buf.String(), "no -allow-host") {
		t.Errorf("expected rewrite warning in log, got: %s", buf.String())
	}
}

// When local-only is on but requests arrive through a same-host reverse proxy
// (loopback peer with an -allow-host Host), the operator gets a one-shot log
// warning that local-only cannot block that traffic.
func TestLocalOnlyProxyWarning(t *testing.T) {
	var buf bytes.Buffer
	s := newTestServerLog(t, &buf)
	s.AllowedHosts = []string{"ping.example.com"}
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}
	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "ping.example.com"
	r.RemoteAddr = "127.0.0.1:55555"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("proxied loopback request: %d, want 200", w.Code)
	}
	if !strings.Contains(buf.String(), "same-host reverse proxy") {
		t.Errorf("expected proxy warning in log, got: %s", buf.String())
	}
}

// POST /api/access {"local_only":true} must flip the setting live: the very
// next request from a non-loopback peer is refused by the guard.
func TestAccessLocalOnlyToggleEnforced(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	// do()'s requests carry httptest's default non-loopback RemoteAddr, so this
	// POST passes only because local_only is still off when the guard runs.
	if w := do(t, h, "POST", "/api/access", `{"local_only":true}`); w.Code != http.StatusOK {
		t.Fatalf("POST access: %d %s", w.Code, w.Body)
	}
	if !s.settings.AccessLocalOnly() {
		t.Fatal("local_only did not persist")
	}
	if w := do(t, h, "GET", "/api/access", ""); w.Code != http.StatusForbidden {
		t.Fatalf("non-loopback request after local_only: %d, want 403", w.Code)
	}
	// A loopback peer still gets through.
	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:4444"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("loopback request after local_only: %d, want 200", w.Code)
	}
}

// doSession runs an authenticated request through the full chain, carrying a
// fresh session cookie for the current credentials.
func doSession(t *testing.T, s *Server, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Host = "127.0.0.1:9000"
	r.AddCookie(&http.Cookie{Name: sessionCookie,
		Value: issueToken(s.settings.AuthUser(), s.settings.AuthHash(), s.settings.SessionEpoch(), time.Now())})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// POST /api/access {"auth_enabled":false} alone flips the login switch off and
// leaves the stored password hash untouched (a master switch, not a wipe).
func TestAccessAuthToggleOffKeepsHash(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	hashBefore := s.settings.AuthHash()

	w := doSession(t, s, s.Handler(), "POST", "/api/access", `{"auth_enabled":false,"current_password":"secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST access: %d %s", w.Code, w.Body)
	}
	if s.settings.AuthEnabled() || s.settings.AuthActive() {
		t.Error("auth_enabled:false did not disable auth")
	}
	if s.settings.AuthHash() != hashBefore {
		t.Error("disabling auth must not touch the stored password hash")
	}
}

// POST /api/access {"username":"bob"} alone (auth already configured) renames
// the account; the password hash and the enable toggle stay untouched.
func TestAccessUsernameRenameKeepsHash(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	hashBefore := s.settings.AuthHash()

	w := doSession(t, s, s.Handler(), "POST", "/api/access", `{"username":"bob","current_password":"secret"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST access: %d %s", w.Code, w.Body)
	}
	if got := s.settings.AuthUser(); got != "bob" {
		t.Fatalf("username = %q, want bob", got)
	}
	if s.settings.AuthHash() != hashBefore {
		t.Error("a rename must not touch the stored password hash")
	}
	if !s.settings.AuthEnabled() {
		t.Error("a rename must not flip the auth toggle off")
	}
}

// A login with the WRONG username and the correct password is a plain 401 that
// counts as a failed attempt (checkPassword burns a dummy bcrypt compare so
// timing can't probe usernames; this pins the visible contract).
func TestLoginWrongUsernameFailsAndCounts(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")

	w := do(t, s.Handler(), "POST", "/api/auth/login", `{"username":"root","password":"secret"}`)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-username login: %d, want 401", w.Code)
	}
	if got := counter("web.login_fail"); got != 1 {
		t.Errorf("web.login_fail = %d, want 1", got)
	}
}

// Logout is auth-exempt, so a cookie-less POST must NOT bump the token epoch:
// an unauthenticated peer hammering it would otherwise keep every browser
// permanently logged out. Only a logout carrying a valid session revokes.
func TestLogoutWithoutCookieKeepsSessions(t *testing.T) {
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	lr := httptest.NewRequest("POST", "/api/auth/login",
		strings.NewReader(`{"username":"admin","password":"secret"}`))
	lr.Header.Set("Content-Type", "application/json")
	lr.Host = "127.0.0.1:9000"
	lw := httptest.NewRecorder()
	h.ServeHTTP(lw, lr)
	if lw.Code != http.StatusOK {
		t.Fatalf("login: %d", lw.Code)
	}
	var cookie *http.Cookie
	for _, c := range lw.Result().Cookies() {
		if c.Name == sessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie from login")
	}

	authed := func() int {
		r := httptest.NewRequest("GET", "/api/settings", nil)
		r.Host = "127.0.0.1:9000"
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	// Cookie-less logout: ok, but there is nothing to revoke.
	if w := do(t, h, "POST", "/api/auth/logout", "{}"); w.Code != http.StatusOK {
		t.Fatalf("cookie-less logout: %d", w.Code)
	}
	if code := authed(); code != http.StatusOK {
		t.Fatalf("session after cookie-less logout: %d, want 200 (must not be revoked)", code)
	}
	// A logout carrying the valid cookie revokes it.
	or := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader("{}"))
	or.Header.Set("Content-Type", "application/json")
	or.Host = "127.0.0.1:9000"
	or.AddCookie(cookie)
	ow := httptest.NewRecorder()
	h.ServeHTTP(ow, or)
	if ow.Code != http.StatusOK {
		t.Fatalf("logout: %d", ow.Code)
	}
	if code := authed(); code != http.StatusUnauthorized {
		t.Fatalf("session after real logout: %d, want 401", code)
	}
}

// In a container the loopback-only filter can't be enforced (every request is
// NAT'd to the gateway), so a non-loopback peer must be let through - not locked
// out - while the same request on a native host is still rejected.
func TestLocalOnlyContainerBypass(t *testing.T) {
	req := func(s *Server) *httptest.ResponseRecorder {
		if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
			t.Fatalf("SetAccessLocalOnly: %v", err)
		}
		r := httptest.NewRequest("GET", "/api/access", nil)
		r.Host = "192.0.2.10" // an IP literal passes the DNS-rebinding guard
		r.RemoteAddr = "192.0.2.10:5000"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}

	native := newTestServer(t)
	if w := req(native); w.Code != http.StatusForbidden {
		t.Errorf("native non-loopback request: %d, want 403", w.Code)
	}

	var buf bytes.Buffer
	container := newTestServerLog(t, &buf)
	container.InContainer, container.Bridged = true, true
	if w := req(container); w.Code != http.StatusOK {
		t.Errorf("bridged container non-loopback request: %d, want 200 (local-only unenforceable)", w.Code)
	}
	if !strings.Contains(buf.String(), "cannot be enforced in a bridged container") {
		t.Errorf("expected bridged-container warning in log, got: %s", buf.String())
	}
}

// A host-networked container (InContainer but not Bridged) keeps loopback
// meaningful, so local-only must still 403 a non-loopback peer; only a bridged
// container may fall through unenforced.
func TestLocalOnlyEnforcedInHostNetContainer(t *testing.T) {
	req := func(s *Server) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/access", nil)
		r.Host = "192.0.2.10" // an IP literal passes the DNS-rebinding guard
		r.RemoteAddr = "192.0.2.1:1234"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}

	s := newTestServer(t)
	s.InContainer, s.Bridged = true, false
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}
	if w := req(s); w.Code != http.StatusForbidden {
		t.Errorf("host-net container non-loopback request: %d, want 403 (loopback test is meaningful there)", w.Code)
	}
	s.Bridged = true
	if w := req(s); w.Code != http.StatusOK {
		t.Errorf("bridged container non-loopback request: %d, want 200 (warn-only)", w.Code)
	}
}

// accessStatus must distinguish the stored toggle from whether the loopback
// filter is actually live, exactly as auth_enabled vs auth_active.
func TestAccessStatusReportsLocalOnlyEnforcement(t *testing.T) {
	s := newTestServer(t)
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}
	s.Bridged = true
	out := s.accessStatus(httptest.NewRequest("GET", "/api/access", nil))
	if out["local_only"] != true || out["local_only_active"] != false {
		t.Errorf("bridged: local_only=%v local_only_active=%v, want true/false (stored but not enforced)",
			out["local_only"], out["local_only_active"])
	}
	s.Bridged = false
	out = s.accessStatus(httptest.NewRequest("GET", "/api/access", nil))
	if out["local_only"] != true || out["local_only_active"] != true {
		t.Errorf("not bridged: local_only=%v local_only_active=%v, want true/true (enforced)",
			out["local_only"], out["local_only_active"])
	}
}

// Local-only is judged on the real TCP peer only, never on a spoofable
// forwarded header: a non-loopback peer stays blocked no matter what
// X-Forwarded-For / X-Real-IP / Forwarded claim, and a genuine loopback peer
// is admitted even while those headers name a public address.
func TestLocalOnlyIgnoresForwardedHeaders(t *testing.T) {
	s := newTestServer(t)
	if err := s.settings.SetAccessLocalOnly(context.Background(), true); err != nil {
		t.Fatalf("SetAccessLocalOnly: %v", err)
	}
	h := s.Handler()

	// A non-loopback peer, each time forging a loopback address in a different
	// forwarded header, must still be refused 403.
	spoofs := []struct{ header, value string }{
		{"X-Forwarded-For", "127.0.0.1"},
		{"X-Real-IP", "127.0.0.1"},
		{"Forwarded", "for=127.0.0.1"},
	}
	for _, sp := range spoofs {
		r := httptest.NewRequest("GET", "/api/access", nil)
		r.Host = "127.0.0.1:9000" // IP literal passes the DNS-rebinding guard
		r.RemoteAddr = "8.8.8.8:44444"
		r.Header.Set(sp.header, sp.value)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("spoofed %s=%q from a non-loopback peer: got %d, want 403", sp.header, sp.value, w.Code)
		}
	}

	// A genuine loopback peer is admitted even while X-Forwarded-For names a
	// public address (the header is not consulted for the local-only decision).
	r := httptest.NewRequest("GET", "/api/access", nil)
	r.Host = "127.0.0.1:9000"
	r.RemoteAddr = "127.0.0.1:55555"
	r.Header.Set("X-Forwarded-For", "8.8.8.8")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("loopback peer with a public X-Forwarded-For: got %d, want 200", w.Code)
	}
}

// Enabling auth without a password is inert (a master switch, not a lockout):
// the guard gates on AuthActive, not AuthEnabled, so gated routes stay open
// until a password is set, after which they are enforced.
func TestAuthEnabledWithoutPasswordIsInert(t *testing.T) {
	s := newTestServer(t)
	if err := s.settings.SetAuthEnabled(context.Background(), true); err != nil {
		t.Fatalf("SetAuthEnabled: %v", err)
	}
	h := s.Handler()

	// Auth enabled but no hash: AuthActive is false, so a gated route is open.
	if w := do(t, h, "GET", "/api/settings", ""); w.Code != http.StatusOK {
		t.Fatalf("gated route with auth enabled but no password: got %d, want 200", w.Code)
	}
	// Setting the password is still reachable (guard not yet active) and turns
	// auth active.
	if w := do(t, h, "POST", "/api/access", `{"password":"secret"}`); w.Code != http.StatusOK {
		t.Fatalf("set password: got %d, want 200 (%s)", w.Code, w.Body)
	}
	if !s.settings.AuthActive() {
		t.Fatal("setting a password with auth enabled should make auth active")
	}
	// Now the same gated route without a session cookie is enforced.
	if w := do(t, h, "GET", "/api/settings", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("gated route after password set: got %d, want 401", w.Code)
	}
}

// TestBcryptSemBoundsConcurrency proves the process-wide bcrypt semaphore caps
// how many bcrypt operations run at once, so a login burst can't peg every core
// and starve the monitor. Without the cap the observed concurrency would reach
// the goroutine count (capN*4), not capN.
func TestBcryptSemBoundsConcurrency(t *testing.T) {
	capN := cap(bcryptSem)
	if capN < 1 {
		t.Fatalf("bcryptSem cap = %d", capN)
	}
	var mu sync.Mutex
	var cur, max int
	var wg sync.WaitGroup
	for i := 0; i < capN*4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bcryptAcquire()
			defer bcryptRelease()
			mu.Lock()
			cur++
			if cur > max {
				max = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond) // hold the slot so contenders pile up
			mu.Lock()
			cur--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if max > capN {
		t.Fatalf("max concurrent bcrypt = %d, exceeds cap %d", max, capN)
	}
}

// A live session must re-prove the current password before changing any
// access-control setting; without it a hijacked/walk-up session takes over the
// account (sets a new password, renames it, or disables login) knowing only
// the session cookie.
// The Settings drawer POSTs /api/access on every Save, even when nothing
// access-related was touched. Such a no-change request must succeed without the
// current password, or a login-protected install cannot save ordinary settings.
func TestAccessNoChangeNeedsNoStepUp(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	// Exactly what the drawer sends on an untouched Access tab: current state
	// echoed back, empty password fields.
	noop := `{"local_only":false,"auth_enabled":true,"username":"admin","password":"","current_password":""}`
	if w := doSession(t, s, h, "POST", "/api/access", noop); w.Code != http.StatusOK {
		t.Fatalf("no-change access POST: %d %s, want 200 without step-up", w.Code, w.Body)
	}
	// Same-value fields individually are also no-ops, not access changes.
	for _, body := range []string{`{"auth_enabled":true}`, `{"username":"admin"}`, `{"local_only":false}`} {
		if w := doSession(t, s, h, "POST", "/api/access", body); w.Code != http.StatusOK {
			t.Fatalf("POST %s: %d, want 200 (no state change)", body, w.Code)
		}
	}
	// The save flow's second half - an ordinary settings POST - now runs.
	if w := doSession(t, s, h, "POST", "/api/settings", `{}`); w.Code != http.StatusOK {
		t.Fatalf("settings save after no-op access POST: %d %s", w.Code, w.Body)
	}
	if got := counter("web.stepup_fail"); got != 0 {
		t.Fatalf("web.stepup_fail = %d, want 0 (no step-up ran)", got)
	}
	if !s.settings.AuthActive() || s.settings.AuthUser() != "admin" {
		t.Fatal("no-op POSTs changed auth state")
	}
}

// Step-up verification spends the same per-client budget as login: a stolen
// session gets at most maxLoginFails serialized guesses, while the operator's
// known-good password still passes during the block.
func TestStepUpRateLimited(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	// One legitimate step-up caches the known-good fingerprint (as any real
	// operator's earlier successful save would have). A password rotation is the
	// delta - NOT local_only, which would make the guard reject the test's
	// non-loopback requests before they reach the handler.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret","password":"secret2"}`); w.Code != http.StatusOK {
		t.Fatalf("legitimate step-up: %d %s", w.Code, w.Body)
	}
	// A hijacked session grinds guesses: each wrong attempt records a failure...
	for i := 0; i < maxLoginFails; i++ {
		w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"wrong","password":"pwned"}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("guess %d: %d, want 403", i, w.Code)
		}
	}
	// ...and once the budget is spent, further guesses get 429, not bcrypt.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"stillwrong","password":"pwned"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("guess after budget: %d, want 429", w.Code)
	}
	if got := counter("web.limiter_trips"); got == 0 {
		t.Fatal("limiter_trips did not count the blocked step-up")
	}
	if s.checkPassword("admin", "pwned") {
		t.Fatal("a refused guess changed the stored credential")
	}
	// The operator's real password rides the known-good escape valve through
	// the block - the attacker sharing the bucket can't lock them out.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret2","username":"bob"}`); w.Code != http.StatusOK {
		t.Fatalf("known-good step-up during block: %d %s", w.Code, w.Body)
	}
	if got := s.settings.AuthUser(); got != "bob" {
		t.Fatalf("known-good step-up did not apply the change: user=%q", got)
	}
	// The rename re-caches the fingerprint under the new name: a SECOND change
	// during the same block must also pass. (The fingerprint embeds the
	// username, so without the re-cache the escape valve died with the rename
	// and the operator was locked out for the cooldown.)
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret2","username":"carol"}`); w.Code != http.StatusOK {
		t.Fatalf("second known-good step-up after a rename: %d %s", w.Code, w.Body)
	}
	if got := s.settings.AuthUser(); got != "carol" {
		t.Fatalf("second rename did not apply: user=%q", got)
	}
}

// A rename+enable from the disabled-with-hash state carries NO password proof
// (step-up only guards while auth is active), so it must never seed the
// known-good cache: the request's arbitrary current_password would later pass
// a blocked step-up without ever having touched bcrypt.
func TestUnverifiedRenameEnableDoesNotSeedKnownGood(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	if err := s.settings.SetAuthEnabled(context.Background(), false); err != nil {
		t.Fatalf("disable auth: %v", err)
	}
	h := s.Handler()

	// Disabled: the POST is ungated and step-up is skipped, but the mutation
	// ends with auth ACTIVE (hash retained + enable).
	if w := do(t, h, "POST", "/api/access", `{"username":"eve","auth_enabled":true,"current_password":"garbage"}`); w.Code != http.StatusOK {
		t.Fatalf("rename+enable while disabled: %d %s", w.Code, w.Body)
	}
	if !s.settings.AuthActive() || s.settings.AuthUser() != "eve" {
		t.Fatalf("premise broken: active=%v user=%q", s.settings.AuthActive(), s.settings.AuthUser())
	}
	// Burn the limiter budget with wrong guesses...
	for i := 0; i < maxLoginFails; i++ {
		if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"wrong","password":"x"}`); w.Code != http.StatusForbidden {
			t.Fatalf("guess %d: %d, want 403", i, w.Code)
		}
	}
	// ...then replay the unverified value. It must NOT ride the escape valve.
	w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"garbage","password":"pwned"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("unverified cached value passed a blocked step-up: %d %s", w.Code, w.Body)
	}
	if s.checkPassword("eve", "pwned") {
		t.Fatal("the blocked request changed the stored credential")
	}
}

// A VERIFIED rename+disable must still refresh the known-good cache under the
// new name, even though auth ends inactive: re-enabling needs no proof, and
// the operator's next real step-up during the same block must not 429.
func TestVerifiedRenameDisableKeepsEscapeValve(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "secret")
	h := s.Handler()

	// Seed the cache the way any real operator's earlier save would.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret","password":"secret2"}`); w.Code != http.StatusOK {
		t.Fatalf("seed rotation: %d %s", w.Code, w.Body)
	}
	for i := 0; i < maxLoginFails; i++ {
		if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"wrong","password":"x"}`); w.Code != http.StatusForbidden {
			t.Fatalf("guess %d: %d, want 403", i, w.Code)
		}
	}
	// Verified (via the escape valve) rename + disable in one request.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret2","username":"bob","auth_enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("verified rename+disable during block: %d %s", w.Code, w.Body)
	}
	if s.settings.AuthActive() || s.settings.AuthUser() != "bob" {
		t.Fatalf("premise broken: active=%v user=%q", s.settings.AuthActive(), s.settings.AuthUser())
	}
	// Re-enable needs no proof while inactive.
	if w := do(t, h, "POST", "/api/access", `{"auth_enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("re-enable: %d %s", w.Code, w.Body)
	}
	// Still inside the cooldown: the renamed operator's real password must
	// pass - the rename refreshed the fingerprint under the new name.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"secret2","password":"secret3"}`); w.Code != http.StatusOK {
		t.Fatalf("legitimate step-up after verified rename+disable/re-enable: %d %s", w.Code, w.Body)
	}
	if !s.checkPassword("bob", "secret3") {
		t.Fatal("the verified step-up did not apply the change")
	}
}

func TestAccessChangeRequiresCurrentPassword(t *testing.T) {
	stats.ResetForTest()
	s := newTestServer(t)
	setPassword(t, s, "admin", "orig")
	h := s.Handler()

	refused := []string{
		`{"password":"pwned"}`,
		`{"current_password":"wrong","password":"pwned"}`,
		`{"username":"bob"}`,
		`{"auth_enabled":false}`,
	}
	for _, body := range refused {
		if w := doSession(t, s, h, "POST", "/api/access", body); w.Code != http.StatusForbidden {
			t.Fatalf("POST %s: %d, want 403 without the current password", body, w.Code)
		}
	}
	if !s.checkPassword("admin", "orig") || s.checkPassword("admin", "pwned") {
		t.Fatal("a refused request must leave the stored credential untouched")
	}
	if s.settings.AuthUser() != "admin" || !s.settings.AuthActive() {
		t.Fatalf("refused requests changed state: user=%q active=%v", s.settings.AuthUser(), s.settings.AuthActive())
	}
	if got := counter("web.stepup_fail"); got != 4 {
		t.Fatalf("web.stepup_fail = %d, want 4", got)
	}

	// With the current password supplied, each change goes through.
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"orig","password":"new2"}`); w.Code != http.StatusOK {
		t.Fatalf("password change with step-up: %d %s", w.Code, w.Body)
	}
	if !s.checkPassword("admin", "new2") || s.checkPassword("admin", "orig") {
		t.Fatal("step-up password change did not take")
	}
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"new2","username":"bob"}`); w.Code != http.StatusOK {
		t.Fatalf("rename with step-up: %d %s", w.Code, w.Body)
	}
	if got := s.settings.AuthUser(); got != "bob" {
		t.Fatalf("username = %q, want bob", got)
	}
	if w := doSession(t, s, h, "POST", "/api/access", `{"current_password":"new2","auth_enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("toggle off with step-up: %d %s", w.Code, w.Body)
	}
	if s.settings.AuthActive() {
		t.Fatal("auth_enabled:false with step-up did not disable auth")
	}
}

// First-time setup has no current password to prove: with auth inactive the
// step-up gate must stay open, or a fresh install could never set a password.
func TestFirstTimePasswordSetupNeedsNoStepUp(t *testing.T) {
	s := newTestServer(t)
	if w := do(t, s.Handler(), "POST", "/api/access", `{"password":"secret"}`); w.Code != http.StatusOK {
		t.Fatalf("first-time set: %d %s", w.Code, w.Body)
	}
	if !s.settings.AuthActive() {
		t.Fatal("first-time password set did not activate auth")
	}
}
