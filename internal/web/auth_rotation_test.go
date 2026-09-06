package web

import (
	"context"
	"crypto/hmac"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// A password change that commits while a login's bcrypt compare is still
// running must leave the old password with nothing: no session token that
// opens a gated route under the new hash, and no known-good fingerprint keyed
// on the new hash that would wave it through the limiter's escape valve. The
// compare is long enough (tens of milliseconds at the production cost) that a
// caller looping on the endpoint straddles a change on demand; MinCost hashes
// keep that window reachable here without the wait.
//
// The login form and HTTP Basic on a gated route are driven the same way:
// several callers present the old password in a loop while the password is
// changed underneath them, then the cache, every token handed out, and the
// blocked bucket's escape valve are inspected.

func TestPasswordChangeMidLoginLeavesOldPasswordDead(t *testing.T) {
	s := newTestServer(t)
	login := func(ip, pass string) (int, string) {
		r := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(`{"username":"admin","password":"`+pass+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = ip + ":12345"
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		for _, c := range w.Result().Cookies() {
			if c.Name == sessionCookie && c.MaxAge > 0 {
				return w.Code, c.Value
			}
		}
		return w.Code, ""
	}
	straddlePasswordChange(t, s, login)
}

func TestPasswordChangeMidBasicAuthLeavesOldPasswordDead(t *testing.T) {
	s := newTestServer(t)
	h := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	get := func(ip, pass string) (int, string) {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = ip + ":12345"
		r.SetBasicAuth("admin", pass)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, ""
	}
	straddlePasswordChange(t, s, get)
}

// straddlePasswordChange hammers attempt with the old password from one bucket
// while the password is changed under it, for as many rounds as it takes to
// catch a compare in flight (or until it gives up), and then checks what the
// old password is still able to do.
func straddlePasswordChange(t *testing.T, s *Server, attempt func(ip, pass string) (code int, token string)) {
	t.Helper()
	ctx := context.Background()
	hashOf := func(pass string) string {
		h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
		if err != nil {
			t.Fatalf("bcrypt: %v", err)
		}
		return string(h)
	}
	hOld, hNew := hashOf("old"), hashOf("new")
	gated := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	opens := func(token string) bool {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "10.0.0.3:12345"
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		w := httptest.NewRecorder()
		gated.ServeHTTP(w, r)
		return w.Code == http.StatusOK
	}

	const hammer = "10.0.0.1"
	rounds, poisoned, survived := 0, 0, 0
	for round := 0; round < 200 && poisoned == 0 && survived == 0; round++ {
		rounds++
		if err := s.settings.SetAuthPassword(ctx, "admin", hOld); err != nil {
			t.Fatalf("SetAuthPassword: %v", err)
		}
		s.logins.mu.Lock()
		s.logins.good = nil
		s.logins.fails = map[string]*failState{}
		s.logins.mu.Unlock()

		stop := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		var tokens []string
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					if code, tok := attempt(hammer, "old"); code == http.StatusOK && tok != "" {
						mu.Lock()
						tokens = append(tokens, tok)
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(time.Duration(200+round%7*150) * time.Microsecond)
		if err := s.settings.SetAuthPassword(ctx, "admin", hNew); err != nil {
			t.Fatalf("SetAuthPassword: %v", err)
		}
		time.Sleep(3 * time.Millisecond)
		close(stop)
		wg.Wait()

		s.logins.mu.Lock()
		good := s.logins.good
		s.logins.mu.Unlock()
		if good != nil && hmac.Equal(good, credFingerprint("admin", "old", hNew)) {
			poisoned++
		}
		for _, tok := range tokens {
			if opens(tok) {
				survived++
			}
		}
	}
	t.Logf("rounds=%d poisoned=%d surviving-tokens=%d", rounds, poisoned, survived)
	if poisoned > 0 {
		t.Errorf("known-good cache vouches for the OLD password under the NEW hash after a check straddled the change")
	}
	if survived > 0 {
		t.Errorf("%d session token(s) issued to the OLD password open a gated route under the NEW hash", survived)
	}

	// What the old password can do now. Its bucket is blocked (the hammer's
	// failures after the change, topped up here), so it can only get in through
	// the escape valve - which must not vouch for it.
	for i := 0; i < maxLoginFails; i++ {
		attempt(hammer, "wrong")
	}
	if code, _ := attempt(hammer, "old"); code == http.StatusOK {
		t.Errorf("after the change, the OLD password still gets in through the blocked bucket's escape valve (HTTP %d)", code)
	}
	// The change itself took: from a fresh bucket the new password is accepted
	// and the old one is refused with a real check.
	if code, _ := attempt("10.0.0.2", "new"); code != http.StatusOK {
		t.Errorf("new password from a fresh bucket: got %d, want 200", code)
	}
	if code, _ := attempt("10.0.0.2", "old"); code != http.StatusUnauthorized {
		t.Errorf("old password from a fresh bucket: got %d, want 401", code)
	}
}

// A login whose bcrypt compare is still running when the password changes is
// refused outright - the password it proved is not the password any more - and
// not answered with a cookie that has already died. A costlier hash than the
// suite's MinCost holds the compare open long enough to land the change inside
// it on purpose.
func TestLoginStraddlingPasswordChangeIsRefused(t *testing.T) {
	s := newTestServer(t)
	dummyHash() // the one-off dummy generation must not pad the window measured here
	hOld, err := bcrypt.GenerateFromPassword([]byte("old"), 11)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if err := s.settings.SetAuthPassword(context.Background(), "admin", string(hOld)); err != nil {
		t.Fatalf("SetAuthPassword: %v", err)
	}
	login := func(pass string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/auth/login",
			strings.NewReader(`{"username":"admin","password":"`+pass+`"}`))
		r.Header.Set("Content-Type", "application/json")
		r.Host = "127.0.0.1:9000"
		r.RemoteAddr = "10.0.0.1:12345"
		w := httptest.NewRecorder()
		s.handleLogin(w, r)
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- login("old") }()
	// Wait for the login to be inside its compare (it holds a bcrypt slot), give
	// it a moment past the read of the stored hash, then change the password.
	deadline := time.Now().Add(5 * time.Second)
	for len(bcryptSem) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("login never started its bcrypt compare")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	setPassword(t, s, "admin", "new")
	w := <-done
	if w.Code != http.StatusUnauthorized {
		t.Errorf("login that straddled the change: got %d, want 401", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge > 0 {
			t.Errorf("login that straddled the change was handed a session cookie")
		}
	}
	if w := login("new"); w.Code != http.StatusOK {
		t.Errorf("new password after the change: got %d, want 200", w.Code)
	}
}

// The stored credentials a check ran against can be replaced before the check
// is acted on (see authCreds). Whatever was proved under the replaced pair earns
// nothing: the check itself fails, the escape valve does not vouch for it, a
// fingerprint remembered under it does not pass the live valve, and a cookie
// issued under it does not open a gated route. Once with the token key falling
// back to the hash alone and once with a key-file secret folded in, since the
// two are separate branches of tokenKey.
func TestStaleCredentialsEarnNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  []byte
	}{
		{"hash-only token key", nil},
		{"key-file token key", []byte("key-file secret")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			s.SessionKey = tc.key
			setPassword(t, s, "admin", "old")
			gated := s.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			cookieOpens := func(issued *httptest.ResponseRecorder) bool {
				r := httptest.NewRequest("GET", "/api/status", nil)
				r.Host = "127.0.0.1:9000"
				for _, c := range issued.Result().Cookies() {
					r.AddCookie(c)
				}
				w := httptest.NewRecorder()
				gated.ServeHTTP(w, r)
				return w.Code == http.StatusOK
			}

			cred := s.authCreds()
			// While the pair is live it does everything.
			if !s.checkPassword(cred, "admin", "old") {
				t.Fatal("live pair: checkPassword refused the right password")
			}
			s.rememberGood(cred, "old")
			if !s.knownGood(cred, "admin", "old") {
				t.Fatal("live pair: knownGood does not vouch for the remembered password")
			}
			w := httptest.NewRecorder()
			s.setSessionCookie(w, false, cred)
			if !cookieOpens(w) {
				t.Fatal("live pair: the issued cookie does not open a gated route")
			}

			// A password change lands after cred was read.
			setPassword(t, s, "admin", "new")
			if s.checkPassword(cred, "admin", "old") {
				t.Error("checkPassword proved the old password against a pair the change replaced")
			}
			if s.knownGood(cred, "admin", "old") {
				t.Error("knownGood vouched for the old password under a pair the change replaced")
			}
			w = httptest.NewRecorder()
			s.setSessionCookie(w, false, cred)
			if cookieOpens(w) {
				t.Error("a cookie issued under the replaced pair opens a gated route")
			}
			s.rememberGood(cred, "old") // a remember that lands after the change, keyed on what it proved
			if s.knownGood(s.authCreds(), "admin", "old") {
				t.Error("a fingerprint remembered under the replaced pair passes the live escape valve")
			}
			// The live pair is unaffected.
			live := s.authCreds()
			if !s.checkPassword(live, "admin", "new") {
				t.Error("live pair after the change: checkPassword refused the new password")
			}
			w = httptest.NewRecorder()
			s.setSessionCookie(w, false, live)
			if !cookieOpens(w) {
				t.Error("live pair after the change: the issued cookie does not open a gated route")
			}

			// A rename lands after the pair was read; the hash stays. What was
			// proved under the old name is not carried over to the new one.
			cred = s.authCreds()
			if err := s.settings.SetAuthUser(context.Background(), "root"); err != nil {
				t.Fatalf("SetAuthUser: %v", err)
			}
			w = httptest.NewRecorder()
			s.setSessionCookie(w, false, cred)
			if cookieOpens(w) {
				t.Error("a cookie issued under the old name opens a gated route after the rename")
			}
			s.rememberGood(cred, "new")
			if s.knownGood(s.authCreds(), "root", "new") {
				t.Error("a fingerprint remembered under the old name passes the live escape valve for the new one")
			}
		})
	}
}
