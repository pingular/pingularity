package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewerAndIsRelease(t *testing.T) {
	for _, c := range []struct {
		cur, lat string
		want     bool
	}{
		{"0.7.0", "0.8.0", true},   // minor bump
		{"0.7.0", "0.7.1", true},   // patch bump
		{"0.7.0", "1.0.0", true},   // major bump
		{"0.7.0", "v0.8.0", true},  // v-prefixed latest
		{"0.8.0", "0.7.0", false},  // we're ahead
		{"0.7.0", "0.7.0", false},  // equal
		{"0.10.0", "0.9.0", false}, // numeric, not lexical, compare
		{"0.9.0", "0.10.0", true},
		{"dev", "0.8.0", false},    // dev current never compares
		{"0.7.0", "banana", false}, // junk latest never wins
		{"0.7.0", "", false},
	} {
		if got := newer(c.cur, c.lat); got != c.want {
			t.Errorf("newer(%q,%q)=%v want %v", c.cur, c.lat, got, c.want)
		}
	}
	for s, want := range map[string]bool{"0.7.0": true, "v1.2.3": true, "1.2.3-rc1": true, "dev": false, "": false, "banana": false} {
		if got := isRelease(s); got != want {
			t.Errorf("isRelease(%q)=%v want %v", s, got, want)
		}
	}
}

// serve builds a Checker pointed at a test server returning the given status+body.
func serve(t *testing.T, current, status string, code int, body string) (*Checker, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	c := New(current, nil, nil)
	c.url = srv.URL
	_ = status
	return c, srv.Close
}

func TestCheckOnceHappyPath(t *testing.T) {
	c, done := serve(t, "0.7.0", "", 200, `{"version":"0.8.0","url":"ignored","published":"2026-06-18"}`)
	defer done()
	c.checkOnce(context.Background())
	st := c.Status()
	if !st.Available || st.Latest != "0.8.0" {
		t.Fatalf("want available 0.8.0, got %+v", st)
	}
	if st.URL != releasesURL {
		t.Errorf("URL should be the built-in releases page, not the server's url field: %q", st.URL)
	}
	if st.CheckedUnix == 0 {
		t.Errorf("CheckedUnix should be set after a successful check")
	}
}

// Every failure mode must leave the cache untouched (last-known-good kept).
func TestCheckOnceFailuresKeepLastKnown(t *testing.T) {
	for name, tc := range map[string]struct {
		code int
		body string
	}{
		"non-200":           {500, `{"version":"9.9.9"}`},
		"malformed json":    {200, `{not json`},
		"empty body":        {200, ``},
		"html error page":   {200, `<!DOCTYPE html><html>nginx</html>`},
		"bogus version":     {200, `{"version":"banana"}`},
		"missing version":   {200, `{"published":"2026-06-18"}`},
		"oversized garbage": {200, strings.Repeat("x", 200<<10)},
	} {
		t.Run(name, func(t *testing.T) {
			c, done := serve(t, "0.7.0", "", tc.code, tc.body)
			defer done()
			// Prime a known-good result, then a failing check must NOT clobber it.
			c.mu.Lock()
			c.latest = "0.8.0"
			c.mu.Unlock()
			c.checkOnce(context.Background())
			if got := c.Status().Latest; got != "0.8.0" {
				t.Fatalf("failure clobbered last-known-good: latest=%q want 0.8.0", got)
			}
		})
	}
}

// A version field with junk trailing a valid semver prefix must be reduced to
// just the matched MAJOR.MINOR.PATCH before it is cached and served, so a
// tampered endpoint can't inject arbitrary bytes into /api/status.
func TestFetchTrimsTrailingJunk(t *testing.T) {
	junk := strings.Repeat("x", 40<<10)
	c, done := serve(t, "0.7.0", "", 200, `{"version":"9.9.9`+junk+`"}`)
	defer done()
	c.checkOnce(context.Background())
	if got := c.Status().Latest; got != "9.9.9" {
		t.Fatalf("latest = %q (len %d), want clean %q", got, len(got), "9.9.9")
	}
}

func TestUnreachableKeepsLastKnown(t *testing.T) {
	c := New("0.7.0", nil, nil)
	c.url = "http://127.0.0.1:0/nope" // unconnectable
	c.mu.Lock()
	c.latest = "0.8.0"
	c.mu.Unlock()
	c.checkOnce(context.Background())
	if c.Status().Latest != "0.8.0" {
		t.Fatal("unreachable endpoint clobbered last-known-good")
	}
}

func TestDevBuildSkipsCheck(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"version":"0.8.0"}`))
	}))
	defer srv.Close()
	c := New("dev", nil, nil)
	c.url = srv.URL
	c.checkOnce(context.Background())
	if hit {
		t.Error("dev build should not even hit the endpoint")
	}
	if c.Status().Available {
		t.Error("dev build must never report an update")
	}
}

func TestDisabledNeverAvailableNorFetches(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"version":"0.8.0"}`))
	}))
	defer srv.Close()
	c := New("0.7.0", func() bool { return false }, nil)
	c.url = srv.URL
	// Even with a newer version already cached, a disabled checker reports false.
	c.mu.Lock()
	c.latest = "0.8.0"
	c.mu.Unlock()
	c.checkOnce(context.Background())
	if hit {
		t.Error("disabled checker should not hit the endpoint")
	}
	st := c.Status()
	if st.Available {
		t.Error("disabled checker must report Available=false")
	}
	if st.Enabled {
		t.Error("Status.Enabled should reflect the toggle")
	}
}
