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
	proxiedWarn sync.Once // local-only on, but traffic arrives via a same-host proxy
	rewriteWarn sync.Once // proxy detected while no -allow-host is configured
	// Rate-limiters for the pre-auth rejection warnings: a flood of crafted
	// Hosts or non-loopback requests would otherwise fill the log ring with a
	// warning per request. Each emits at most once per rejectWarnEvery and
	// reports how many were suppressed meanwhile.
	hostReject  logCoalescer // DNS-rebinding Host rejection
	localReject logCoalescer // local-only non-loopback rejection
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

// logCoalescer emits at most one line per window and reports how many were
// suppressed since the last emit, so a rejection flood can't fill the log ring.
type logCoalescer struct {
	mu      sync.Mutex
	last    time.Time
	dropped int
}

func (c *logCoalescer) allow(every time.Duration) (emit bool, suppressed int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last.IsZero() || time.Since(c.last) >= every {
		suppressed = c.dropped
		c.dropped = 0
		c.last = time.Now()
		return true, suppressed
	}
	c.dropped++
	return false, 0
}

// rejectLog reports whether a Host-rejection warning should be emitted now (at
// most once per rejectWarnEvery) and how many rejections were suppressed since
// the last emitted line. This keeps a flood of rejected requests from filling the
// log ring with one warning each.
func (l *failLimiter) rejectLog() (emit bool, suppressed int) {
	return l.hostReject.allow(rejectWarnEvery)
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

// recoverPanics turns a panicking handler into a 500 instead of a killed
// connection: recover, log with a stack, and write the error (a post-headers
// write just no-ops). http.ErrAbortHandler is re-panicked so the server's abort
// path runs.
//
// WHERE this sits is the whole point, not a detail. Deferred functions unwind
// innermost-first, so any middleware that finalizes a response in a defer -
// compressResponses closing its gzip stream - runs BEFORE a recover that is
// further out, and commits its own status first. With recovery only in guard
// (outside compression), a route that panicked before writing produced a clean
// 500 to a plain client and a 200 carrying "internal server error" to a client
// that sent Accept-Encoding: gzip, which is nearly all of them. So recovery is
// mounted INSIDE compression, nearest the routes: it writes the 500 first, and
// the compressor then finalizes around a status that is already correct.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "recovered", rec, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// guard wraps the mux with three access controls, in order: a DNS-rebinding
// Host-header check, a loopback-only filter, and authentication. The filter and
// auth read live settings (toggling either applies without a restart); the
// allowed-host set is fixed at startup. Ahead of all three, a restore's reconcile
// window is refused wholesale - those settings are the backup's, not this box's.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Retained as the OUTER net so a panic in guard's own work (the host check,
		// the local-only filter, auth) is still caught - recoverPanics only covers
		// what it wraps. Route panics are handled further in, before the compressor
		// finalizes; see recoverPanics.
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
		// A restore publishes the backup's settings before its safety repair runs, so
		// for that window the box is configured by whatever the backup said - possibly
		// "no login, reachable from the network". Refuse everything rather than judge
		// the request against settings nobody on this machine chose: loopback cannot
		// tell a local browser from a same-host reverse proxy, so no narrower test
		// closes the window (a proxy that rewrites Host to loopback and drops
		// X-Forwarded-For is indistinguishable from the operator's own browser). The
		// import itself is admitted before the handler raises this flag, so an
		// in-flight restore never locks itself out.
		//
		// 503 with a short Retry-After, not 403: the window is seconds wide, and this
		// tells the dashboard (and a load balancer) to come back rather than to report
		// the box as forbidden. /healthz and /readyz answer throughout, above.
		if s.reconciling.Load() {
			w.Header().Set("Retry-After", "2")
			http.Error(w, "restoring a backup; try again shortly", http.StatusServiceUnavailable)
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
		// warned about below. Restores need no special case here: the reconcile
		// window is refused outright above, before any setting is consulted.
		if s.settings.AccessLocalOnly() {
			// Local-only means loopback-only for EVERYONE, containers included. A
			// container that must be reachable over the network opts in explicitly
			// with -access network (PINGULARITY_ACCESS=network), which turns this
			// filter off. We no longer GUESS a container's network mode from its
			// interfaces: that heuristic mislabeled real setups both ways - it opened
			// a host-net box and 403'd a bridged one, and a spoofable interface name
			// is not an access boundary. An explicit operator signal decides access.
			switch {
			case !isLoopbackAddr(r.RemoteAddr):
				// Coalesced like the Host rejection above: every request is still
				// refused, only the LOG line is throttled - a LAN flood would
				// otherwise evict genuinely useful ring history one line at a time.
				if emit, suppressed := s.logins.localReject.allow(rejectWarnEvery); emit {
					s.log.Warn("request rejected: local-only access is on",
						"ip", clientIP(r), "path", capForLog(r.URL.Path), "suppressed_since_last", suppressed)
				}
				// A short explanation, not a bare 403: the person reading this is
				// usually the operator hitting their own fresh install's published
				// port, and the body is all the interface they have. It states only
				// the setting that refused them - nothing about auth or the install.
				http.Error(w, "this dashboard is private (local-only access is on). "+
					"Enable network access from the machine itself in the Access tab, "+
					"or start with -access network / -e PINGULARITY_ACCESS=network - and set a password.",
					http.StatusForbidden)
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
		// FAIL CLOSED WHEN THE CONFIGURATION NEVER LOADED. If the initial settings
		// read failed, the controller holds compiled-in defaults, and those have no
		// password - so AuthActive() below answers "no login here" whether the
		// operator never set one or the daemon simply could not read the one they
		// did. Serving on that answer silently discards a configured login: every
		// route, including data deletion and import, then answers with no
		// credentials while the password sits intact on disk.
		//
		// The ambiguity cannot be resolved from here, so it is refused instead of
		// guessed. That is the call the import reconcile path already makes for the
		// same ambiguity ("fail CLOSED", handleImport), and the same bargain the
		// store's prune guard makes about an untrusted clock: declining costs a
		// restart, acting costs the thing being protected.
		//
		// /healthz and /readyz are already past us (they return above), so liveness
		// stays answerable and readiness can report NOT ready - which is how a
		// supervisor learns to keep traffic away. Everything else - UI, API and
		// metrics alike - gets 503 until a reload succeeds.
		if !s.settings.Loaded() {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "settings could not be loaded, so access control cannot be applied; "+
				"refusing to serve. check the log, then restart or SIGHUP once the database is readable.",
				http.StatusServiceUnavailable)
			return
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
				// Offer Basic so curl/wget/Prometheus can authenticate - but NOT to
				// our own SPA, which sets X-Pingularity-UI on every fetch: a
				// same-origin fetch answered 401+Basic pops the browser's NATIVE
				// credentials dialog on top of the SPA's own login overlay, once
				// per API poll. The app header is the discriminator because it is
				// origin-independent - unlike Sec-Fetch-Mode, which Chrome omits on
				// a plain-HTTP LAN origin (http://192.168.x.x is not "potentially
				// trustworthy"), exactly the common dashboard install. A cross-site
				// page cannot forge the header onto a credentialed request without a
				// CORS preflight we never grant, and suppressing the challenge only
				// hides the native prompt - it never weakens the actual auth check.
				// curl/wget/Prometheus never send it, so they still get Basic. Logged-in
				// browser visits to /metrics etc. ride the session cookie, so
				// nothing browser-side needs the challenge.
				if r.Header.Get("X-Pingularity-UI") == "" {
					w.Header().Set("WWW-Authenticate", `Basic realm="pingularity"`)
				}
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authExempt lists what stays reachable without a session: /api/auth/login and
// /api/auth/logout, GET /api/access (the flags the login overlay renders from),
// /api/quick-setup (which gates itself in its handler rather than here - see the
// case below), and everything NOT under /api/ - the static UI shell, font and
// favicon, so the login overlay can render at all. Every other /api/ route is
// gated.
//
// The method predicates are asymmetric, so read the table rather than a summary
// of it. /api/access is the ONLY entry scoped by method: GET is exempt, POST
// falls through to the default and is gated, because it changes the password and
// who may reach the dashboard. The other three name a path and nothing else, so
// every method reaches their handlers; each answers POST alone and returns 405 to
// the rest (handleLogin, handleLogout, handleQuickSetup).
//
// /metrics needs its explicit false: it is not under /api/, so without that case
// the default below would exempt it along with the static assets and publish it
// to anyone who can reach the port. Gated does not mean "admin login only" -
// guard accepts a configured -metrics-token there as an alternative credential,
// so a scraper need not hold the account that can change settings.
//
// /healthz and /readyz never reach this function at all: guard answers them
// ahead of every check, including this one.
func authExempt(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case p == "/api/auth/login" || p == "/api/auth/logout":
		return true
	case p == "/api/access" && r.Method == http.MethodGet:
		return true
	case p == "/api/quick-setup":
		// The first-run endpoint self-gates rather than leaning on the session
		// guard, which keeps a lost-response retry idempotent: a cookieless retry
		// AFTER the answer enabled auth hits the "already done" 200 no-op instead of
		// a guard 401. What an unauthenticated caller can reach here is bounded to
		// one harmless outcome - the done/no-op. The `dismiss` marker is reachable
		// without a session only while NO login is active (so a dialog on an
		// out-of-band-secured install can still close); once auth is active the
		// handler applies the same auth check as gated endpoints before writing it
		// (see handleQuickSetup - the marker releases the boot monitoring hold, so
		// an unauthenticated LAN peer must not set it). A full answer that would
		// change access/auth still 403s inside the handler once a login is active.
		// The network/host filters (local-only, DNS-rebind) run BEFORE this
		// exemption, so it never widens who can reach the port.
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

// dummyHash returns a valid bcrypt hash (of an unguessable throwaway string)
// that checkPassword compares against when the username doesn't match, so a
// wrong username costs the same time as a wrong password.
//
// It is computed on first use rather than at package init because main imports
// this package unconditionally: an eager initializer charged EVERY subcommand a
// full DefaultCost bcrypt whether or not it ever served a login. Measured on an
// M-series Mac, that was 43 ms of the 49 ms a `pingularity version` took, and
// the container image's HEALTHCHECK runs `pingularity healthz` every 30s, so an
// idle container burned about two CPU-minutes a day generating a hash it used
// zero times.
//
// The generation deliberately does NOT take a bcryptSem slot: checkPassword
// already holds one when it forces this value, and under a burst of logins
// every slot can be held by a checkPassword call, so re-acquiring here would
// deadlock the whole login path.
var dummyHash = sync.OnceValue(func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("pingularity-dummy-2f1d"), bcrypt.DefaultCost)
	if err != nil {
		panic(err) // cannot happen for a fixed short input
	}
	return h
})

// checkPassword verifies a username/password against the stored credentials.
func (s *Server) checkPassword(user, pass string) bool {
	bcryptAcquire()
	defer bcryptRelease()
	// Force the lazy dummy hash BEFORE looking at the username, so the one-time
	// generation lands on whichever attempt happens to be first and not on the
	// wrong-username branch alone. Forcing it inside the branch instead would
	// make the first wrong username after a restart cost generate+compare while
	// a right one cost compare - a fresh username oracle, just inverted, handed
	// to whoever gets the first login attempt in after a restart.
	dh := dummyHash()
	if user != s.settings.AuthUser() {
		// Burn an equivalent bcrypt compare anyway: returning early would let
		// an attacker probe for valid usernames via response timing.
		_ = bcrypt.CompareHashAndPassword(dh, []byte(pass))
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
		// Normalize a configured host exactly like the request host above,
		// trailing dot included: "fleet.example.com." is the same name as
		// "fleet.example.com", and a config carrying the dot used to match
		// NEITHER spelling of the Host header - it 403'd every proxied request.
		a = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a), "."))
		if a != "" && host == a {
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
	// Same lock the import's reconcile holds. A credential change landing in the
	// middle of a restore used to leave the reconcile guessing which username was
	// the operator's, and it guessed wrong whenever only the password had changed -
	// a password-only update keeps the username, so the hash moved and the name did
	// not. Serializing removes the guess: one of the two goes first, whole.
	s.importMu.Lock()
	defer s.importMu.Unlock()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.accessStatus(r))
	case http.MethodPost:
		var in struct {
			LocalOnly       *bool  `json:"local_only"`
			AuthEnabled     *bool  `json:"auth_enabled"`
			Username        string `json:"username"`
			Password        string `json:"password"`
			CurrentPassword string `json:"current_password"`
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
		// The Settings drawer posts here on EVERY Save, before the settings
		// POST, and ABORTS the whole save if this call fails. So a request that
		// changes no access state must succeed without a step-up (or any limiter
		// spend) - demanding the password to save an unrelated latency tweak
		// locked ordinary saves out of a login-protected install entirely. That
		// is a property of this branch, not an aspiration: see the no-change
		// handling below, which skips a write rather than refusing one.
		// Empty username and password mean "keep current", so only a real
		// difference counts as a change.
		reqUser := strings.TrimSpace(in.Username)
		delta := in.Password != "" ||
			(reqUser != "" && reqUser != s.settings.AuthUser()) ||
			(in.AuthEnabled != nil && *in.AuthEnabled != s.settings.AuthEnabled()) ||
			(in.LocalOnly != nil && *in.LocalOnly != s.settings.AccessLocalOnly())
		if !delta {
			// One thing still has to happen on a no-change save: PERSIST an
			// explicitly submitted access scope. The value an operator sees can
			// come from -access / PINGULARITY_ACCESS rather than the store, and
			// then "Save" is not a no-op to them - it is how they make the
			// choice stick so they can drop the env var. Without this, the
			// documented env-var recovery silently fails to persist and the
			// next boot without the variable is local-only again.
			//
			// The write moves no privilege NOW - the same value is already in
			// effect either way - but it decides how LONG that privilege lasts,
			// and that is not symmetric. Storing local_only=false pins an
			// env-conditional open posture into a permanent one: the operator
			// who later drops PINGULARITY_ACCESS expecting to fall back to
			// local-only stays reachable from the whole network, and a session
			// that never proved the password would have done that to them.
			//
			// So that one write is SKIPPED rather than refused. Refusing it was
			// tried and is wrong here: because the drawer echoes local_only back
			// on every Save and aborts on failure, a 403 breaks every ordinary
			// settings save on the most common container setup there is - opened
			// with PINGULARITY_ACCESS=network, password set, no stored row
			// (reconcileAccess writes one only when the env DISAGREES with the
			// live value, so the normal case never has one). Skipping costs the
			// operator nothing they can see, and leaves the posture exactly as
			// it was: still open, still conditional on the variable that opened
			// it. An operator who genuinely wants it permanent still has a path,
			// and it is the honest one - change the scope deliberately, which is
			// a delta, and prove the password like any other access change.
			//
			// The closing direction writes as before: local_only=true can only
			// take reach away, so no session can widen anything with it.
			//
			// Nothing special is needed for the install whose scope is ALREADY
			// stored open - the case a network install hits on every Save. The
			// refusal design needed an exemption for it, to avoid 403ing those
			// saves; skipping needs none, because writing a value the store
			// already holds and skipping it leave the store in the same state.
			// An exemption here would be untestable by construction.
			// ...and it is skipped only where skipping buys something. With no
			// login configured there is no password a hijacked session could
			// fail to prove, so withholding the write protects nobody and would
			// only break the documented recovery for the installs least able to
			// afford it.
			//
			// And an operator who OFFERS the password is not guessed at: they
			// typed it into the Access tab, which is what asking for this write
			// deliberately looks like, so it is verified and honoured. That is
			// what keeps the documented flow - open the Access tab, leave
			// Network access on, Save, drop the variable - working on a
			// login-protected install. Without it the only way to produce a
			// delta is to switch to local-only first, which instantly 403s the
			// very browser doing it whenever the operator is working over the
			// network: no route to a permanent choice at all.
			//
			// A WRONG password still refuses, and only then. The drawer sends
			// this field only when someone has typed in it, so an ordinary save
			// never reaches the check and never breaks.
			if in.LocalOnly != nil {
				persistScope := *in.LocalOnly || !s.settings.AuthActive()
				if !persistScope && in.CurrentPassword != "" {
					verified, ok := s.requireStepUp(w, r, in.CurrentPassword)
					if !ok {
						return // refusal already written
					}
					persistScope = verified
				}
				switch {
				case persistScope:
					if err := s.settings.SetAccessLocalOnly(r.Context(), *in.LocalOnly); err != nil {
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
				default:
					// Say so in the log: the request succeeded and one thing in
					// it did not happen, which is exactly the shape that is
					// otherwise discovered months later, by the reboot that
					// closes the port.
					s.log.Info("access scope left unstored: it is open only because -access or PINGULARITY_ACCESS says so, "+
						"and making that permanent is an access change - re-enter the current password in the Access tab to store it",
						"ip", clientIP(r))
				}
			}
			writeJSON(w, s.accessStatus(r))
			return
		}
		// Step-up before any access-control change (password, username, login
		// toggle, network scope). stepUpVerified records that THIS request
		// proved the current password (bcrypt or the known-good escape valve).
		// Trust decisions after the mutations - refreshing the known-good cache
		// - key on this, never on the post-mutation auth state: a rename+enable
		// from the disabled-with-hash state ends active WITHOUT any proof, and
		// caching its arbitrary current_password would let it pass a blocked
		// step-up later; a verified rename+disable ends inactive WITH proof,
		// and skipping the refresh strands the escape valve on the dead name.
		stepUpVerified, ok := s.requireStepUp(w, r, in.CurrentPassword)
		if !ok {
			return // refusal already written
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
			// The known-good fingerprint embeds the username, so the cached
			// pair died with the old name - and during a limiter block the
			// escape valve is all a legitimate step-up has. Refresh it under
			// the new name ONLY when this request actually proved the current
			// password, independent of where the enable toggle ended up: a
			// verified rename+disable still deserves the refresh (re-enabling
			// needs no proof, and the next real step-up must not 429), while a
			// no-proof rename+enable from the disabled state must never seed
			// the cache with an unverified value.
			if renamed && stepUpVerified {
				s.rememberGood(s.settings.AuthUser(), in.CurrentPassword)
			}
		}
		// The caveat below describes a CHANGE, so it is gated on the posture actually
		// moving. The drawer echoes local_only on every Save, so comparing against the
		// stored value is what stops an unrelated save on an already-local-only box
		// from repeating "access is now limited" until the operator stops reading it.
		turnedLocalOnly := false
		if in.LocalOnly != nil {
			turnedLocalOnly = *in.LocalOnly && !s.settings.AccessLocalOnly()
			if err := s.settings.SetAccessLocalOnly(ctx, *in.LocalOnly); err != nil {
				s.internalError(w, err)
				return
			}
			// Ungated on purpose, unlike the two caveats that follow. This records the
			// value that was just WRITTEN, and the write runs on every save carrying
			// local_only, so it is the only trace that a save touched network reach at
			// all - gating it on the transition would leave the re-affirming saves
			// (the drawer's every Save) with no record to read back. The caveats
			// describe a CHANGE, which is why they ride the transition: repeated on a
			// box that is already local-only they are noise, and noise is what buries
			// the one line that mattered.
			s.log.Info("network access changed", "local_only", *in.LocalOnly)
			// -allow-host declares a reverse proxy, and proxied visitors reach
			// this machine as loopback connections - local-only can't see them.
			if turnedLocalOnly && len(s.AllowedHosts) > 0 {
				s.log.Warn("local-only enabled, but -allow-host declares a reverse proxy: visitors arriving through it are not blocked",
					"allowed_hosts", strings.Join(s.AllowedHosts, ","))
			}
		}
		status := s.accessStatus(r)
		// Surface the same proxy caveat the log records above to the CALLER, not only
		// the server log: setting local-only while a same-host proxy is declared does
		// NOT keep proxied visitors out, and the operator saving this needs to know.
		if turnedLocalOnly {
			if c := proxyLocalOnlyCaveat(s.AllowedHosts); c != "" {
				status["warnings"] = []string{"Access is now limited to this machine, but " + c}
			}
		}
		writeJSON(w, status)
	default:
		http.Error(w, "use GET or POST", http.StatusMethodNotAllowed)
	}
}

// requireStepUp makes the caller re-prove the CURRENT password before an
// access-control change lands, and reports whether the handler may proceed. A
// live session must not be able to change access settings on the strength of a
// cookie alone - otherwise a hijacked or walk-up session silently takes over the
// account (sets a new password, renames it, disables login, or makes network
// reach permanent). Only enforced once auth is active; first-time setup has no
// current password to prove.
//
// ok=false means the refusal (403 or 429) is already written and the handler
// must return without touching anything. verified reports that this request
// actually PROVED the password, which is not the same as ok: an inactive-auth
// install proves nothing and still proceeds.
//
// checkPassword burns bcrypt under the process-wide bcryptSem, and the attempt
// spends the same per-client budget as login and Basic - a hijacked session must
// not get unlimited serialized password guesses out of this endpoint when the
// login form caps them. Callers hold s.importMu, so this is serialized against a
// restore reconcile exactly like the mutations it guards.
//
// Every access change made THROUGH THIS ENDPOINT routes here, and a second copy
// of this logic would be a second place for the limiter accounting or the escape
// valve to drift. It is not a claim about the process as a whole, and reading it
// as one would be wrong: Quick Setup changes auth and access behind a different
// gate (a blanket refusal once auth is active), and the import/restore path
// writes auth and access directly with no step-up at all.
func (s *Server) requireStepUp(w http.ResponseWriter, r *http.Request, currentPassword string) (verified, ok bool) {
	if !s.settings.AuthActive() {
		return false, true
	}
	refuse := func(why string) {
		stats.Inc("web.stepup_fail")
		s.log.Warn("access change refused: "+why, "user", capForLog(s.settings.AuthUser()), "ip", clientIP(r))
		http.Error(w, "current password is required to change access settings", http.StatusForbidden)
	}
	if currentPassword == "" {
		refuse("current password missing") // not a guess - no budget spent
		return false, false
	}
	key := s.limiterKey(r)
	if !s.logins.reserve(key) {
		// Budget spent: only the cached known-good credential passes, with no
		// bcrypt work - the same escape valve login has, so an attacker sharing
		// the bucket can't lock the operator out.
		if !s.knownGood(s.settings.AuthUser(), currentPassword) {
			stats.Inc("web.limiter_trips")
			http.Error(w, "too many failed attempts; try again shortly", http.StatusTooManyRequests)
			return false, false
		}
		return true, true // matched a pair that passed bcrypt earlier
	}
	if s.checkPassword(s.settings.AuthUser(), currentPassword) {
		s.logins.ok(key)
		s.rememberGood(s.settings.AuthUser(), currentPassword)
		return true, true
	}
	s.logins.releaseFail(key)
	refuse("current password wrong")
	return false, false
}

// proxyLocalOnlyCaveat returns the sentence to append when access is (or is being)
// limited to the local machine but -allow-host declares a same-host reverse proxy.
// Proxied visitors reach the box as loopback connections the local-only filter
// cannot tell from a real local user, so local-only CANNOT block them; the honest
// remedy is a login password. It returns "" when no proxy is declared - there
// local-only is a real restriction and the caller's truthful no-proxy wording
// stands. One predicate, read by both the import repair (web.go) and the
// access-settings action, so the affordance and the action cannot disagree.
func proxyLocalOnlyCaveat(allowedHosts []string) string {
	if len(allowedHosts) == 0 {
		return ""
	}
	return "a reverse proxy is declared (-allow-host), and local-only access CANNOT block visitors " +
		"arriving through it - proxied visitors still reach the dashboard. Set a login password in the " +
		"Access tab to protect it."
}

func (s *Server) accessStatus(r *http.Request) map[string]any {
	active := s.settings.AuthActive()
	authed := !active || s.authed(r)
	out := map[string]any{
		"local_only":        s.settings.AccessLocalOnly(),
		"local_only_active": s.settings.AccessLocalOnly(), // now enforced whenever on (no container bypass)
		"auth_enabled":      s.settings.AuthEnabled(),     // intent - drives the toggle
		"auth_active":       active,                       // enforced - drives the overlay
		"has_password":      s.settings.HasPassword(),
		"authed":            authed,
	}
	// The username, port, and LAN URLs feed the access-settings tab. This
	// endpoint is reachable without a session (the overlay needs the flags
	// above), so don't disclose them to unauthenticated callers.
	if authed {
		port, lan := s.lanURLs()
		out["user"] = s.settings.AuthUser()
		out["port"] = port
		out["lan_urls"] = lan // one http://<ip>:<port> per advertised address; see lanEntriesFor (two binds list more than the socket answers on)
	}
	return out
}

// lanEntry is one address the dashboard answers on, in the shape the Access tab
// renders it: URL is what a row links and copies, Primary picks the row that leads
// and is accented, Iface is only a hover title - on a dual-stack host the extra rows
// are usually the same interface in another family, so the name separates nothing.
type lanEntry struct {
	URL     string `json:"url"`
	IP      string `json:"ip"`
	Iface   string `json:"iface"`
	Primary bool   `json:"primary"` // the IPv4 default-route source - the one to hand to another device; no entry has it on a host with no IPv4 route out, or a bind pinned elsewhere
	// Public means globally routable, not confirmed reachable from outside: whether
	// anything on the internet can open a connection is the router's inbound policy,
	// which this machine cannot see. The UI carries that hedge in its wording.
	Public bool `json:"public"`
}

// hostAddr is one address this host holds and the interface carrying it. Passing a
// list in is what lets a test pose a host to lanEntriesFor: net.Interface.Addrs()
// only ever reports the real machine.
type hostAddr struct {
	iface string
	ip    net.IP
}

// hostAddrs lists every address on an interface that is up and not loopback: a
// down interface answers nothing, and loopback is not another device.
func hostAddrs() []hostAddr {
	ifaces, _ := net.Interfaces()
	out := make([]hostAddr, 0, len(ifaces))
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				out = append(out, hostAddr{iface: ifc.Name, ip: ipn.IP})
			}
		}
	}
	return out
}

// lanURLs returns the listen port and the addresses another device can use to reach
// this dashboard, primary first. lanEntriesFor holds the rule.
func (s *Server) lanURLs() (port string, entries []lanEntry) {
	return lanEntriesFor(s.listenAddr, hostAddrs(), defaultRouteIP())
}

// lanEntriesFor picks the addresses to advertise for a listen address: where the
// dashboard is reachable from, which follows the socket and not the host's interface
// list. It used to print the second one, offering the LAN URL even for a loopback
// bind that refuses connections to it.
//
//   - The bind decides. `-listen 127.0.0.1:9000` answers on loopback alone, so
//     nothing is advertised and the UI falls back to http://localhost:<port>. A
//     bind pinned to one address advertises that address alone.
//   - Both families count. A wildcard bind is dual-stack however it is spelled -
//     ":9000", "0.0.0.0:9000" and "[::]:9000" all answer on both - and listing
//     IPv4 only hid a plain-HTTP dashboard answering on a globally routable IPv6
//     address. (OpenBSD and DragonFly refuse a dual-stack socket, so a wildcard
//     takes one family there and this over-promises in the other. We ship neither.)
//   - The enumeration drops link-local and loopback in both families: 169.254/16
//     and fe80::/10 mean nothing typed into another device (an IPv6 link-local URL
//     needs the reader's own zone index), and loopback is not another device.
//     Private IPv4 and ULA stay - that is the normal home LAN. A pin is the
//     exception, echoed unfiltered because the socket really is bound there, so
//     `-listen 169.254.10.5:9000` does advertise that address.
//   - Primary marks the IPv4 default-route source only. The kernel's outbound IPv6
//     source is a privacy address that rotates every day or so, so flagging it would
//     be advice with an expiry date.
//
// Two listen addresses fall through to the full enumeration though the socket is
// narrower, so the list can name more than it answers on: a hostname other than
// localhost (resolving it would cost a DNS round-trip inside a UI request) and a
// zoned literal like `[fe80::1%en0]:9000` (pinning it would echo the address without
// the zone its reader has to supply).
//
// Public keeps following the address on those two rather than being withheld: the
// socket does answer on one of the listed addresses, the panel already tells the
// reader that both forms fall back to the whole list, and so the tag reads there as
// "one of these candidates is routable" - which is established. Withholding it would
// leave a globally routable plain-HTTP URL listed and described like a LAN address,
// which is the exposure the flag exists to announce.
func lanEntriesFor(listenAddr string, addrs []hostAddr, primary string) (port string, entries []lanEntry) {
	host, p, err := net.SplitHostPort(listenAddr)
	if err != nil || p == "" {
		// Config validation refuses a -listen without a port, so this is the zero
		// value - a Server that never reached Serve.
		host, port = listenAddr, "9000"
	} else {
		port = p
	}
	entries = []lanEntry{}
	bind := net.ParseIP(host) // nil for "", "localhost", a hostname, or a zoned literal
	// A zoned literal is nil to ParseIP, so ask the address half on its own - that is
	// what makes `-listen [::1%lo0]:9000` advertise nothing. For the loopback question
	// only: `bind` stays nil so a zoned link-local misses the pinned branch, which
	// would echo it without its zone.
	loop := bind
	if loop == nil {
		if h, _, zoned := strings.Cut(host, "%"); zoned {
			loop = net.ParseIP(h)
		}
	}
	switch {
	// Names ignore case and a trailing dot only roots one, so "LocalHost:9000" and
	// "localhost.:9000" bind 127.0.0.1 like "localhost:9000" does; an exact compare
	// advertised the LAN address for that loopback-only socket. nonLoopbackListen in
	// main answers the same question the same way.
	case strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") || (loop != nil && loop.IsLoopback()):
		return port, entries // this machine only - nothing another device can use
	case bind != nil && !bind.IsUnspecified():
		// Pinned to one address: that address is the answer, listed even if the
		// enumeration no longer carries it, because the socket is bound there. The
		// interface name is only a label, so an address that has moved just loses it.
		e := lanEntry{IP: bind.String(), Primary: bind.String() == primary, Public: isPublicIP(bind)}
		e.URL = "http://" + net.JoinHostPort(e.IP, port)
		for _, a := range addrs {
			if a.ip.Equal(bind) {
				e.Iface = a.iface
				break
			}
		}
		return port, append(entries, e)
	}
	seen64 := map[string]bool{}
	for _, a := range addrs {
		// IsGlobalUnicast is the rule above in one call: it rejects loopback,
		// link-local, multicast, the IPv4 broadcast and the unspecified address, and
		// admits private IPv4 and ULA.
		if !a.ip.IsGlobalUnicast() {
			continue
		}
		// One interface usually holds several IPv6 addresses out of one /64, because
		// privacy addressing (RFC 8981) keeps minting them - the same dashboard on the
		// same network, so the rest are duplicate rows rather than extra answers. The
		// tie-break is enumeration order, not stability: net.Interface.Addrs() carries
		// no temporary-address flag, so the kept address may be one that rotates, which
		// is why the panel still tells the reader not to bookmark one. IPv4 is left
		// alone - two IPv4 addresses on one interface are two separate answers.
		if a.ip.To4() == nil {
			key := a.iface + "|" + string(a.ip.To16()[:8])
			if seen64[key] {
				continue
			}
			seen64[key] = true
		}
		ip := a.ip.String()
		entries = append(entries, lanEntry{
			// JoinHostPort brackets an IPv6 literal; unbracketed, the trailing :9000
			// reads as part of the address and the result is not a URL.
			URL: "http://" + net.JoinHostPort(ip, port), IP: ip,
			Iface: a.iface, Primary: ip == primary, Public: isPublicIP(a.ip),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Primary && !entries[j].Primary })
	return port, entries
}

// isPublicIP reports whether an address is globally routable - reachable from off
// the network in principle. It cannot say whether anything outside actually gets
// through: that is the router's inbound policy, which is not visible from here.
func isPublicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	// 100.64.0.0/10 is carrier-grade NAT, and it is also where Tailscale hands out
	// its IPv4 addresses. Go calls it global unicast and not private, so without this
	// clause a tailnet address - very common in this audience - would be reported as
	// reachable from the internet. Tailscale's IPv6 range is a ULA, so IsPrivate
	// already covers it.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// defaultRouteIP reports the IPv4 source address the host would use to reach the
// internet - the one another device on the network can actually use. A UDP
// "connect" only asks the kernel's routing table which source it would pick;
// no packet is sent and nothing is contacted. "" when there is no route. IPv4
// only: an IPv6 twin would name an address that rotates - see lanEntriesFor.
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
