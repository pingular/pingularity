package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"github.com/pingular/pingularity/internal/stats"
)

const (
	sessionCookie = "ping_session"
	sessionTTL    = 30 * 24 * time.Hour
)

// maxUsernameBytes bounds a login username at the persistence boundary. Kept in
// sync with settings' maxUser (its normalize() caps to the same value); rejecting
// here surfaces a clear 400 rather than silently truncating.
const maxUsernameBytes = 128

// Password rate limiting: after maxLoginFails consecutive failures from one
// IP, further attempts are refused (429) for loginBlockFor.
const (
	maxLoginFails = 5
	loginBlockFor = 30 * time.Second
)

// failLimiter throttles password failures per client bucket (see limiterKey),
// capping how fast an attacker can grind bcrypt comparisons via the login
// endpoint or HTTP Basic. State is in-memory only (a restart clears it); stale
// entries are dropped lazily as new failures are recorded. It also carries the
// guard's other per-server state: the known-good credential fingerprint, the
// trusted-proxy list, and the one-shot operator warnings.
type failLimiter struct {
	mu    sync.Mutex
	fails map[string]*failState
	// good is a cheap HMAC fingerprint of the last credentials that passed a
	// bcrypt check, keyed on the current password hash so a password change
	// invalidates it. It lets valid callers through a block (only failures
	// should spend budget) without handing attackers bcrypt-cost work.
	good []byte
	// trusted lists proxy CIDRs whose X-Forwarded-For may key the limiter
	// (see Server.limiterKey). Set once before serving, read-only after.
	trusted []netip.Prefix
	// One-shot operator warnings emitted from the request path.
	proxiedWarn            sync.Once // local-only on, but traffic arrives via a same-host proxy
	rewriteWarn            sync.Once // proxy detected while no -allow-host is configured
	containerLocalOnlyWarn sync.Once // local-only on but unenforceable inside a container
	// Rate-limiter for the DNS-rebinding Host-rejection warning: a flood of crafted
	// Hosts would otherwise fill the log ring with a warning per request. Emit at
	// most once per rejectWarnEvery and report how many were suppressed meanwhile.
	rejectMu      sync.Mutex
	rejectLast    time.Time
	rejectDropped int
	// The session-revocation epoch (bumped on logout, folded into token MACs) now
	// lives in the settings store so it survives a restart - a logged-out token
	// used to revalidate after a restart reset the old in-memory epoch to 0. See
	// settings.Controller.SessionEpoch / BumpSessionEpoch.
}

type failState struct {
	count   int       // consecutive recorded failures
	pending int       // password evaluations in flight (bcrypt not yet finished)
	last    time.Time // time of the most recent failure
}

func newFailLimiter() *failLimiter { return &failLimiter{fails: map[string]*failState{}} }

// rejectWarnEvery bounds how often the Host-rejection warning is emitted.
const rejectWarnEvery = time.Minute

// rejectLog reports whether a Host-rejection warning should be emitted now (at
// most once per rejectWarnEvery) and how many rejections were suppressed since
// the last emitted line. This keeps a flood of rejected requests from filling the
// log ring with one warning each.
func (l *failLimiter) rejectLog() (emit bool, suppressed int) {
	l.rejectMu.Lock()
	defer l.rejectMu.Unlock()
	if l.rejectLast.IsZero() || time.Since(l.rejectLast) >= rejectWarnEvery {
		suppressed = l.rejectDropped
		l.rejectDropped = 0
		l.rejectLast = time.Now()
		return true, suppressed
	}
	l.rejectDropped++
	return false, 0
}

// blocked reports whether ip has exhausted its failure budget and is still in
// the cool-down window.
func (l *failLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.fails[ip]
	return st != nil && st.count >= maxLoginFails && time.Since(st.last) < loginBlockFor
}

// reserve counts one in-flight password evaluation against ip's budget and
// reports whether it may proceed. The budget covers recorded failures AND
// evaluations already in flight, so a concurrent burst can't each slip past a
// not-yet-updated failure count and all pay a full bcrypt - at most
// maxLoginFails compares run at once per bucket. Every true must be paired with
// exactly one releaseFail (failure) or ok (success).
func (l *failLimiter) reserve(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	st := l.fails[ip]
	if st == nil {
		st = &failState{}
		l.fails[ip] = st
	}
	if st.count >= maxLoginFails && time.Since(st.last) >= loginBlockFor {
		st.count = 0 // cool-down served; grant a fresh budget
	}
	if st.count+st.pending >= maxLoginFails {
		return false
	}
	st.pending++
	st.last = time.Now() // keep the entry fresh so prune can't drop an in-flight bucket
	return true
}

// releaseFail completes a reserved evaluation that failed: it turns the pending
// slot into a recorded failure.
func (l *failLimiter) releaseFail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.fails[ip]
	if st == nil {
		return
	}
	if st.pending > 0 {
		st.pending--
	}
	st.count++
	st.last = time.Now()
}

// ok clears ip's failure streak after a successful authentication.
func (l *failLimiter) ok(ip string) {
	l.mu.Lock()
	delete(l.fails, ip)
	l.mu.Unlock()
}

// prune drops entries idle long past any cool-down so the map can't grow
// unbounded (called with l.mu held).
func (l *failLimiter) prune() {
	cutoff := time.Now().Add(-10 * loginBlockFor)
	for ip, st := range l.fails {
		if st.last.Before(cutoff) {
			delete(l.fails, ip)
		}
	}
}

// guard wraps the mux with three access controls, in order: a DNS-rebinding
// Host-header check, a loopback-only filter, and authentication. The filter and
// auth read live settings (toggling either applies without a restart); the
// allowed-host set is fixed at startup.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every route passes through here, so this is the one place to stop a
		// panicking handler from silently killing the connection: recover, log
		// with a stack, and return a 500 (a post-headers write just no-ops).
		// http.ErrAbortHandler is re-panicked so the server's abort path runs.
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "recovered", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		// Liveness/readiness probes must answer an unauthenticated load balancer that
		// hits a bare IP, so they bypass the DNS-rebinding guard, the local-only filter,
		// and auth (they expose no data - just an ok/not-ready verdict). Kept under the
		// panic-recover defer above and the securityHeaders/writeDeadline wrappers.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		// DNS-rebinding protection: a malicious page can re-point its own DNS
		// name at this server and use the victim's browser as a same-origin
		// proxy into the API (cookies don't ride along, but no-auth installs are
		// fully exposed). A direct/local visit never carries a public FQDN in
		// Host, so reject those unless allowed via -allow-host (reverse proxies).
		if !hostAllowed(r.Host, s.AllowedHosts) {
			// Truncate the request-derived Host/path (a header can be tens of KB) and
			// coalesce the warning so a rejection flood can't bloat the log ring.
			if emit, suppressed := s.logins.rejectLog(); emit {
				s.log.Warn("request rejected: unrecognized Host (DNS-rebinding guard)",
					"host", capForLog(r.Host), "ip", clientIP(r), "path", capForLog(r.URL.Path),
					"suppressed_since_last", suppressed,
					"hint", "a public domain needs -allow-host=<domain>")
			}
			http.Error(w, "unrecognized Host header (DNS-rebinding protection); serving Pingularity under a public domain requires -allow-host=<domain>", http.StatusForbidden)
			return
		}
		// X-Forwarded-For with no -allow-host configured usually means a reverse
		// proxy is rewriting Host to a form the guard always admits (an IP or
		// localhost). In that mode the rebinding guard can't vet proxied requests
		// and the session cookie is never marked Secure - warn once so the
		// operator pairs the proxy with a preserved Host plus -allow-host.
		if len(s.AllowedHosts) == 0 && r.Header.Get("X-Forwarded-For") != "" {
			s.logins.rewriteWarn.Do(func() {
				s.log.Warn("reverse proxy detected (X-Forwarded-For present) but no -allow-host is set; if the proxy rewrites Host, the DNS-rebinding guard cannot vet proxied requests",
					"peer", clientIP(r), "host", capForLog(r.Host),
					"hint", "configure the proxy to preserve Host and pass -allow-host=<domain>")
			})
		}
		// Local-only: judged on the real TCP peer, never the spoofable
		// X-Forwarded-For. A same-host reverse proxy still passes - detected and
		// warned about below.
		if s.settings.AccessLocalOnly() {
			switch {
			case s.InContainer:
				// A bridged container NATs every external request to the gateway, so
				// the loopback test can't tell a local user from a LAN one - local-only
				// is unenforceable here and would just lock the dashboard out. The
				// published port (-p) and the login password are the real boundary.
				s.logins.containerLocalOnlyWarn.Do(func() {
					s.log.Warn("local-only access is on but cannot be enforced inside a container; the dashboard stays reachable - restrict it via how you publish the port (-p) and enable the login password")
				})
			case !isLoopbackAddr(r.RemoteAddr):
				s.log.Warn("request rejected: local-only access is on", "ip", clientIP(r), "path", r.URL.Path)
				http.Error(w, "access restricted to localhost", http.StatusForbidden)
				return
			case !hostAllowed(r.Host, nil):
				// A loopback peer whose Host needed -allow-host to pass means a
				// same-host reverse proxy is forwarding a remote visitor - traffic
				// local-only cannot block. Tell the operator once.
				s.logins.proxiedWarn.Do(func() {
					s.log.Warn("local-only is on, but requests are arriving through a same-host reverse proxy, which local-only cannot block", "host", capForLog(r.Host))
				})
			}
		}
		if s.settings.AuthActive() && !authExempt(r) {
			// A configured read-only metrics token is an ALTERNATIVE credential for
			// /metrics only: a scraper presents it (Bearer, or as the Basic password)
			// instead of the dashboard admin login, so Prometheus never holds the
			// account that can change settings. A wrong/absent token just falls through
			// to the normal admin check below, so admin creds still work too.
			if r.URL.Path == "/metrics" && s.MetricsToken != "" && s.metricsTokenOK(r) {
				next.ServeHTTP(w, r)
				return
			}
			// Refuse Basic attempts from a bucket that recently burned through its
			// failure budget, before doing any bcrypt work - unless the credentials
			// match the cached known-good fingerprint (cheap, constant-time), so a
			// valid caller is never locked out by someone else's failures. Behind a
			// reverse proxy the TCP peer is the proxy and all clients share one
			// bucket; SetTrustedProxies switches the key to the proxy-appended
			// X-Forwarded-For hop (a spoofable header must never gate this alone).
			if user, pass, hasBasic := r.BasicAuth(); hasBasic && s.logins.blocked(s.limiterKey(r)) && !s.knownGood(user, pass) {
				stats.Inc("web.limiter_trips")
				s.log.Warn("auth rate-limited (too many failed attempts)", "ip", clientIP(r))
				http.Error(w, "too many failed attempts; try again shortly", http.StatusTooManyRequests)
				return
			}
			if !s.authed(r) {
				// Log only when credentials were supplied and rejected (a real failed
				// attempt) - not every cookieless poll from a logged-out browser,
				// which would flood the log.
				if u, _, ok := r.BasicAuth(); ok {
					s.log.Warn("auth failed", "user", capForLog(u), "ip", clientIP(r), "path", capForLog(r.URL.Path))
				}
				// Offer Basic so curl/Prometheus can authenticate; browsers ignore it
				// and the SPA shows its own login overlay on the 401.
				w.Header().Set("WWW-Authenticate", `Basic realm="pingularity"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authExempt lists what stays reachable without a session: the static UI shell
// (so the login overlay can render), the login/logout endpoints, and GET
// /api/access (the login-overlay flags). /metrics and every other /api/ route
// are gated.
func authExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/auth/login" || p == "/api/auth/logout":
		return true
	case p == "/api/access" && r.Method == http.MethodGet:
		return true
	case p == "/metrics":
		return false
	default:
		return !strings.HasPrefix(p, "/api/") // static assets (UI, font, favicon)
	}
}

// metricsTokenOK reports whether the request presents the configured read-only
// metrics token, accepting the two forms Prometheus can send: an
// "Authorization: Bearer <token>" header, or HTTP Basic with the token as the
// password (any username). Compared in constant time.
func (s *Server) metricsTokenOK(r *http.Request) bool {
	want := s.MetricsToken
	if want == "" {
		return false
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got := strings.TrimPrefix(h, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
	}
	if _, pass, ok := r.BasicAuth(); ok {
		if subtle.ConstantTimeCompare([]byte(pass), []byte(want)) == 1 {
			return true
		}
	}
	return false
}

// authed reports whether the request carries a valid session cookie or correct
// HTTP Basic credentials. Failed Basic attempts feed the per-IP rate limiter.
func (s *Server) authed(r *http.Request) bool {
	hash := s.settings.AuthHash()
	if hash == "" {
		return false
	}
	if c, err := r.Cookie(sessionCookie); err == nil && verifyToken(c.Value, s.settings.AuthUser(), s.tokenKey(), s.settings.SessionEpoch()) {
		return true
	}
	if user, pass, ok := r.BasicAuth(); ok {
		key := s.limiterKey(r)
		// reserve caps concurrent bcrypt work and refuses once the budget
		// (recorded failures + in-flight evaluations) is spent; when it does,
		// only credentials matching the cached known-good fingerprint pass, with
		// no bcrypt work, and the block stays armed for everyone else.
		if !s.logins.reserve(key) {
			return s.knownGood(user, pass)
		}
		if s.checkPassword(user, pass) {
			s.logins.ok(key)
			s.rememberGood(user, pass)
			return true
		}
		s.logins.releaseFail(key)
		stats.Inc("web.login_fail")
	}
	return false
}

// bcryptSem caps concurrent bcrypt work across the whole process. bcrypt at the
// default cost is deliberately CPU-heavy (tens of ms per call); without a cap a
// burst of login attempts - even spread across source IPs to stay under the
// per-key login limiter - can peg every core and starve the monitor and probe
// goroutines. 2xGOMAXPROCS keeps the cores busy on auth without letting it crowd
// out the rest of the process. Acquire before every Generate/Compare.
var bcryptSem = make(chan struct{}, 2*runtime.GOMAXPROCS(0))

func bcryptAcquire() { bcryptSem <- struct{}{} }
func bcryptRelease() { <-bcryptSem }

// hashPassword bcrypts a plaintext password.
func hashPassword(p string) (string, error) {
	bcryptAcquire()
	defer bcryptRelease()
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}

// dummyHash is a valid bcrypt hash (of an unguessable throwaway string) that
// checkPassword compares against when the username doesn't match, so a wrong
// username costs the same time as a wrong password.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("pingularity-dummy-2f1d"), bcrypt.DefaultCost)
	if err != nil {
		panic(err) // cannot happen for a fixed short input
	}
	return h
}()

// checkPassword verifies a username/password against the stored credentials.
func (s *Server) checkPassword(user, pass string) bool {
	bcryptAcquire()
	defer bcryptRelease()
	if user != s.settings.AuthUser() {
		// Burn an equivalent bcrypt compare anyway: returning early would let
		// an attacker probe for valid usernames via response timing.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pass))
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(s.settings.AuthHash()), []byte(pass)) == nil
}

// tokenKey is the HMAC key for session tokens. It mixes the bcrypt hash (so a
// password change still invalidates every outstanding token) with SessionKey, an
// independent secret derived from the key file (0600, beside the DB, NOT in it).
// Keying on the hash ALONE let anyone holding a raw copy of the database - a
// backup, a VM snapshot - compute a valid admin token, since the hash and epoch
// both live in the settings table; adding the key-file-bound secret closes that,
// because a DB-only copy carries no key file. When SessionKey is unset (an
// ephemeral :memory: server), it falls back to the hash alone.
func (s *Server) tokenKey() string {
	h := s.settings.AuthHash()
	if len(s.SessionKey) == 0 {
		return h
	}
	return h + "\x00" + string(s.SessionKey)
}

// issueToken builds a stateless session token: base64(expiry|user).HMAC, keyed
// on key (see tokenKey). Folding the bcrypt hash into that key means changing the
// password invalidates every outstanding token, and tokens survive restarts (the
// hash is persisted) - no server-side session store needed.
func issueToken(user, hash string, epoch int64, now time.Time) string {
	// Expiry first, username last: the username is the remainder after the first
	// separator, so it may contain "|" without breaking the round-trip (the MAC
	// covers the whole payload regardless).
	payload := strconv.FormatInt(now.Add(sessionTTL).Unix(), 10) + "|" + user
	b := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return b + "." + tokenMAC(payload, hash, epoch)
}

// verifyToken checks a token's signature and expiry against the current hash,
// that it was issued for the currently configured user (renaming the account
// invalidates tokens issued under the old name), and that it carries the
// current epoch (a logout bumps the epoch to revoke outstanding tokens).
func verifyToken(tok, user, hash string, epoch int64) bool {
	b, mac, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(b)
	if err != nil {
		return false
	}
	payload := string(raw)
	if subtle.ConstantTimeCompare([]byte(mac), []byte(tokenMAC(payload, hash, epoch))) != 1 {
		return false
	}
	expStr, tokUser, ok := strings.Cut(payload, "|")
	if !ok || tokUser != user {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	return err == nil && time.Now().Unix() < exp
}

func tokenMAC(payload, hash string, epoch int64) string {
	mac := hmac.New(sha256.New, []byte(hash))
	mac.Write([]byte(payload))
	mac.Write([]byte{0})
	mac.Write([]byte(strconv.FormatInt(epoch, 10)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// clientIP is the request's source IP (the real TCP peer; behind a proxy, the
// proxy). Used for auth audit logs; the rate limiter keys on limiterKey.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limiterKey is the failLimiter bucket for a request. IPv6 peers collapse to
// their /64 - one host can rotate through a whole /64 (SLAAC privacy
// addresses), so per-address budgets would cost an attacker nothing. When the
// TCP peer is a declared trusted proxy, the X-Forwarded-For chain is walked from
// the nearest hop outward, skipping addresses that are themselves trusted
// proxies, and the first UNtrusted hop is used - the real client behind a chain
// of same-host/sidecar proxies. Taking only the rightmost hop breaks a two-proxy
// chain: that hop is the second proxy, so every client would collapse into one
// bucket. Untrusted peers ignore X-Forwarded-For entirely (it is fully
// spoofable) and key on the TCP peer.
func (s *Server) limiterKey(r *http.Request) string {
	peer := clientIP(r)
	ip, err := netip.ParseAddr(peer)
	if err != nil {
		return peer
	}
	if s.logins.trustedPeer(ip) {
		// Flatten the header (it may arrive as several lines and/or comma lists)
		// into hops ordered client-first (left) .. nearest-proxy (right).
		var chain []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					chain = append(chain, part)
				}
			}
		}
		// Walk right (nearest) to left (client); stop at the first hop that is not
		// a trusted proxy - the closest address attributable to a real client. A
		// malformed hop stops the walk (nothing further can be trusted), keeping
		// the last good candidate.
		for i := len(chain) - 1; i >= 0; i-- {
			hop, perr := netip.ParseAddr(chain[i])
			if perr != nil {
				break
			}
			ip = hop
			if !s.logins.trustedPeer(hop) {
				break
			}
		}
	}
	ip = ip.Unmap().WithZone("")
	if ip.Is6() {
		if p, err := ip.Prefix(64); err == nil {
			return p.String()
		}
	}
	return ip.String()
}

// SetTrustedProxies declares proxy addresses (CIDRs or bare IPs) whose
// X-Forwarded-For the login rate limiter may key on; see limiterKey. This is
// the wire-up point for a -trusted-proxy flag. Call before Serve - the list
// is read without locking afterwards.
func (s *Server) SetTrustedProxies(cidrs []string) error {
	var ps []netip.Prefix
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		p, err := netip.ParsePrefix(c)
		if err != nil {
			a, aerr := netip.ParseAddr(c)
			if aerr != nil {
				return fmt.Errorf("trusted proxy %q is not a CIDR or IP", c)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		ps = append(ps, p.Masked())
	}
	s.logins.trusted = ps
	return nil
}

// trustedPeer reports whether ip falls inside a declared trusted-proxy range.
func (l *failLimiter) trustedPeer(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	for _, p := range l.trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// credFingerprint is a cheap keyed digest of a credential pair. Keying on the
// bcrypt hash means a password change invalidates cached fingerprints.
func credFingerprint(user, pass, hash string) []byte {
	mac := hmac.New(sha256.New, []byte(hash))
	mac.Write([]byte(user))
	mac.Write([]byte{0})
	mac.Write([]byte(pass))
	return mac.Sum(nil)
}

// rememberGood caches the fingerprint of credentials that just passed a bcrypt
// check, so knownGood can wave them through a rate-limiter block later.
func (s *Server) rememberGood(user, pass string) {
	fp := credFingerprint(user, pass, s.settings.AuthHash())
	s.logins.mu.Lock()
	s.logins.good = fp
	s.logins.mu.Unlock()
}

// knownGood reports whether the credentials match the last pair that passed a
// bcrypt check under the current password hash. Constant-time and bcrypt-free,
// it lets valid callers (a Prometheus scrape, the operator) bypass a limiter
// block without giving attackers CPU-cost work. Empty until the first success
// after startup, so a block can still catch a valid caller once per restart.
// The username must also match the current one: a rename leaves the hash (and
// thus the cached fingerprint) unchanged, so without this a stale fingerprint
// would keep vouching for the old username that checkPassword now rejects.
func (s *Server) knownGood(user, pass string) bool {
	s.logins.mu.Lock()
	cached := s.logins.good
	s.logins.mu.Unlock()
	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(s.settings.AuthUser())) == 1
	fpOK := cached != nil && hmac.Equal(cached, credFingerprint(user, pass, s.settings.AuthHash()))
	return userOK && fpOK
}

// hostAllowed implements the DNS-rebinding guard's Host check. Admitted without
// configuration: IP literals (a rebinding attack needs a DNS name to
// re-resolve), localhost and *.localhost, dotless single-label LAN names
// ("plex"), and suffixes that can't be publicly registered (.local, .lan,
// .home, .internal, .home.arpa). Anything else - a public FQDN - must be in
// extra (-allow-host).
func hostAllowed(hostport string, extra []string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return true
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if !strings.Contains(host, ".") {
		return true
	}
	for _, suf := range []string{".local", ".lan", ".home", ".internal", ".home.arpa"} {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	for _, a := range extra {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" && host == a {
			return true
		}
	}
	return false
}

// isLoopbackAddr reports whether a host:port's IP is loopback.
func isLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleAccess reports (GET) or updates (POST) the access-control settings. GET
// is reachable unauthenticated so the UI can decide whether to prompt for login;
// POST is gated by the guard once auth is active.
func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.accessStatus(r))
	case http.MethodPost:
		var in struct {
			LocalOnly   *bool  `json:"local_only"`
			AuthEnabled *bool  `json:"auth_enabled"`
			Username    string `json:"username"`
			Password    string `json:"password"`
		}
		if err := decodeJSONBody(w, r, &in); err != nil {
			return // response already written (415/400)
		}
		// Validate the username BEFORE persistence: reject invalid UTF-8 (which would
		// corrupt the store and every access-status echo) and anything past the
		// rune-safe cap. normalize() caps it too, but rejecting here gives a clear 400
		// instead of silently storing a truncated login name the operator can't log in
		// with. The cap mirrors settings' maxUser (kept in sync intentionally).
		if u := strings.TrimSpace(in.Username); u != "" {
			if !utf8.ValidString(u) {
				http.Error(w, "username must be valid UTF-8", http.StatusBadRequest)
				return
			}
			if len(u) > maxUsernameBytes {
				http.Error(w, fmt.Sprintf("username too long: at most %d bytes", maxUsernameBytes), http.StatusBadRequest)
				return
			}
		}
		ctx := r.Context()
		if in.Password != "" {
			// bcrypt reads at most 72 bytes and x/crypto rejects longer input
			// outright - tell the caller instead of returning a generic 500.
			if len(in.Password) > 72 {
				http.Error(w, "password too long: at most 72 bytes", http.StatusBadRequest)
				return
			}
			hash, err := hashPassword(in.Password)
			if err != nil {
				http.Error(w, "could not hash password", http.StatusInternalServerError)
				return
			}
			// A password-only rotation (no username field) must keep the current
			// login name, not silently rename it: fall back to the configured user
			// when the request omits one. SetAuthPassword still defaults a truly
			// empty name to "admin" for first-time setup.
			user := strings.TrimSpace(in.Username)
			if user == "" {
				user = s.settings.AuthUser()
			}
			if err := s.settings.SetAuthPassword(ctx, user, hash); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// SetAuthPassword flips auth on (setting a password is normally the
			// act of turning it on), but an explicit auth_enabled:false in the
			// same request wins: store the password, leave the login switch off.
			if in.AuthEnabled != nil && !*in.AuthEnabled {
				if err := s.settings.SetAuthEnabled(ctx, false); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			// A new hash invalidates the old session token (its key changed), so
			// re-issue the cookie - which also logs in whoever just set it.
			s.setSessionCookie(w, s.secureCookie(r))
			s.rememberGood(s.settings.AuthUser(), in.Password)
			s.log.Info("auth password set", "user", s.settings.AuthUser(), "enabled", s.settings.AuthEnabled())
		} else {
			// No password change: apply just the username and the enable toggle.
			// Enabling without a password is allowed but stays inert (see
			// AuthActive), so the toggle is a master switch, not a lockout.
			renamed := false
			if u := strings.TrimSpace(in.Username); u != "" && u != s.settings.AuthUser() {
				if err := s.settings.SetAuthUser(ctx, u); err != nil {
					s.internalError(w, err)
					return
				}
				renamed = true
			}
			if in.AuthEnabled != nil {
				if err := s.settings.SetAuthEnabled(ctx, *in.AuthEnabled); err != nil {
					s.internalError(w, err)
					return
				}
				s.log.Info("auth toggled", "enabled", *in.AuthEnabled)
			}
			// A username token is bound to the account name, so a rename invalidates
			// the caller's current session cookie - the very next request (e.g. the
			// settings POST that a combined Access save fires right after) would 401
			// and be dropped. Reissue a cookie for the new name, exactly as the
			// password branch does. POST /api/access is gated when auth is active, so
			// reaching a successful rename means the caller was authenticated.
			if renamed && s.settings.AuthActive() {
				s.setSessionCookie(w, s.secureCookie(r))
			}
		}
		if in.LocalOnly != nil {
			if err := s.settings.SetAccessLocalOnly(ctx, *in.LocalOnly); err != nil {
				s.internalError(w, err)
				return
			}
			s.log.Info("network access changed", "local_only", *in.LocalOnly)
			// -allow-host declares a reverse proxy, and proxied visitors reach
			// this machine as loopback connections - local-only can't see them.
			if *in.LocalOnly && len(s.AllowedHosts) > 0 {
				s.log.Warn("local-only enabled, but -allow-host declares a reverse proxy: visitors arriving through it are not blocked",
					"allowed_hosts", strings.Join(s.AllowedHosts, ","))
			}
		}
		writeJSON(w, s.accessStatus(r))
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
	}
}

func (s *Server) accessStatus(r *http.Request) map[string]any {
	active := s.settings.AuthActive()
	authed := !active || s.authed(r)
	out := map[string]any{
		"local_only":   s.settings.AccessLocalOnly(),
		"auth_enabled": s.settings.AuthEnabled(), // intent - drives the toggle
		"auth_active":  active,                   // enforced - drives the overlay
		"has_password": s.settings.HasPassword(),
		"authed":       authed,
	}
	// The username, port, and LAN URLs feed the access-settings tab. This
	// endpoint is reachable without a session (the overlay needs the flags
	// above), so don't disclose them to unauthenticated callers.
	if authed {
		port, lan := s.lanURLs()
		out["user"] = s.settings.AuthUser()
		out["port"] = port
		out["lan_urls"] = lan // http://<lan-ip>:<port> for each non-loopback IPv4
	}
	return out
}

// lanEntry is one address the dashboard answers on. The interface name and the
// primary flag let the UI separate the address worth sharing from the virtual
// bridges a host also holds (docker0, VPN, VM taps), which the old flat URL
// list presented as equal peers.
type lanEntry struct {
	URL     string `json:"url"`
	IP      string `json:"ip"`
	Iface   string `json:"iface"`
	Primary bool   `json:"primary"` // the address on the default route - the one to hand to another device
}

// lanURLs returns the listen port and the dashboard addresses reachable from
// other devices (one per non-loopback IPv4 on an up interface), primary first.
func (s *Server) lanURLs() (port string, entries []lanEntry) {
	if _, p, err := net.SplitHostPort(s.listenAddr); err == nil && p != "" {
		port = p
	} else {
		port = "9000"
	}
	entries = []lanEntry{}
	primary := defaultRouteIP()
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			v4 := ip.To4()
			if v4 == nil {
				continue
			}
			entries = append(entries, lanEntry{
				URL: "http://" + v4.String() + ":" + port, IP: v4.String(),
				Iface: ifc.Name, Primary: v4.String() == primary,
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Primary && !entries[j].Primary })
	return port, entries
}

// defaultRouteIP reports the source address the host would use to reach the
// internet - the one another device on the network can actually use. A UDP
// "connect" only asks the kernel's routing table which source it would pick;
// no packet is sent and nothing is contacted. "" when there is no route.
func defaultRouteIP() string {
	c, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer c.Close()
	if ua, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String()
	}
	return ""
}

// handleLogin verifies credentials and sets the session cookie.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(w, r, &in); err != nil {
		return // response already written (415/400)
	}
	user := strings.TrimSpace(in.Username)
	key := s.limiterKey(r)
	if !s.logins.reserve(key) {
		// Budget spent (recorded failures + in-flight evaluations): only cached
		// known-good credentials pass - no bcrypt work, and the block stays armed
		// for everyone else. Behind a shared proxy bucket one attacker must not
		// lock out valid logins.
		if !s.knownGood(user, in.Password) {
			stats.Inc("web.limiter_trips")
			http.Error(w, "too many failed attempts; try again shortly", http.StatusTooManyRequests)
			return
		}
	} else if s.checkPassword(user, in.Password) {
		s.logins.ok(key)
		s.rememberGood(user, in.Password)
	} else {
		s.logins.releaseFail(key)
		stats.Inc("web.login_fail")
		s.log.Warn("login failed", "user", capForLog(user), "ip", clientIP(r))
		http.Error(w, "invalid username or password", http.StatusUnauthorized)
		return
	}
	s.setSessionCookie(w, s.secureCookie(r))
	s.log.Info("login", "user", s.settings.AuthUser())
	writeJSON(w, map[string]bool{"ok": true})
}

// secureCookie decides the session cookie's Secure attribute per request. It
// only ever marks Secure when -allow-host declares a reverse proxy. Given that,
// a proxy-set X-Forwarded-Proto is the reliable scheme signal even when the
// proxy rewrote Host to a loopback literal (a common nginx default): trusting
// it can only make the cookie more restrictive - a direct client can spoof it
// to https and merely get a Secure cookie, never downgrade a victim's. Without
// X-Forwarded-Proto, fall back to the Host: a public FQDN gets Secure, while
// IP-literal/localhost/LAN Hosts (direct plain-HTTP LAN access) get a non-Secure
// cookie, or login over the advertised plain-HTTP LAN URLs would loop forever.
func (s *Server) secureCookie(r *http.Request) bool {
	if len(s.AllowedHosts) == 0 {
		return false
	}
	// X-Forwarded-Proto is a spoofable header, so honour it only when the TCP peer
	// is a declared trusted proxy (the same set the login rate limiter keys on).
	// From an untrusted peer it is ignored - a direct client must not be able to
	// steer the cookie's Secure flag with a forged header - and the decision falls
	// back to the Host below.
	if s.peerTrusted(r) {
		if xfp := r.Header.Get("X-Forwarded-Proto"); xfp != "" {
			// A multi-proxy chain sends a comma-separated list; the leftmost hop is
			// the original client scheme.
			if i := strings.IndexByte(xfp, ','); i >= 0 {
				xfp = xfp[:i]
			}
			return strings.EqualFold(strings.TrimSpace(xfp), "https")
		}
	}
	return !hostAllowed(r.Host, nil)
}

// peerTrusted reports whether the request's real TCP peer is in the declared
// trusted-proxy set (see SetTrustedProxies). Used to decide when a forwarded
// header (X-Forwarded-Proto) may be believed.
func (s *Server) peerTrusted(r *http.Request) bool {
	ip, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	return s.logins.trustedPeer(ip)
}

// setSessionCookie issues a fresh session cookie for the current credentials.
// secure marks it Secure so it never rides a plaintext request; callers pass
// s.secureCookie(r).
func (s *Server) setSessionCookie(w http.ResponseWriter, secure bool) {
	tok := issueToken(s.settings.AuthUser(), s.tokenKey(), s.settings.SessionEpoch(), time.Now())
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(sessionTTL.Seconds()),
	})
}

// handleLogout clears the session cookie. POST plus the JSON content-type
// guard, like every other mutating endpoint - otherwise a link prefetch or a
// cross-site form submission could force a logout. The clear cookie carries
// the same per-request Secure decision as the one it replaces so it actually
// overwrites it.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if !requireJSONCT(w, r) { // body-less POST: CSRF guard (see requireJSONCT)
		return
	}
	// Stateless tokens carry no server-side session to delete, so clearing the
	// cookie alone would leave a captured token valid for its full TTL. Bump the
	// epoch (folded into every token MAC) so outstanding tokens stop verifying -
	// but only when the caller presents a currently-valid session. The bump
	// revokes EVERY browser's session, and this endpoint is auth-exempt, so an
	// unauthenticated peer must not be able to hammer it and keep the dashboard
	// permanently logged out. A request with no (or a stale) cookie has nothing
	// to revoke; it just gets the clearing cookie.
	if c, err := r.Cookie(sessionCookie); err == nil &&
		verifyToken(c.Value, s.settings.AuthUser(), s.tokenKey(), s.settings.SessionEpoch()) {
		// Persisted so the revocation survives a restart (see BumpSessionEpoch). A
		// failed persist still revokes the live process; log it - the durability
		// guarantee is best-effort until the next successful write.
		if err := s.settings.BumpSessionEpoch(r.Context()); err != nil {
			s.log.Warn("persist session-revocation epoch failed; logout revoked in-process but may not survive a restart", "err", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: s.secureCookie(r),
		SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, map[string]bool{"ok": true})
}
