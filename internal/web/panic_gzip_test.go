package web

import (
	"net/http"
	"strings"
	"testing"
)

// A recovered panic must produce the same status whether or not the client asked
// for gzip. It did not: compressResponses sat INSIDE guard, so when a route
// panicked the compressor's deferred close ran first on the way out and committed
// its default 200 - and only then did guard's recover() try to send 500, by which
// point the header was already on the wire.
//
// The failure is invisible in exactly the situation that matters. Browsers and
// most API clients send Accept-Encoding: gzip by default, so the 500 that a
// monitoring check or the dashboard's own error handling keys on became a 200
// carrying "internal server error" as its body - a broken endpoint that reads as
// healthy to anything watching status codes.
func TestRecoveredPanicIs500WithAndWithoutGzip(t *testing.T) {
	s := newTestServer(t)

	for _, tc := range []struct {
		name string
		h    http.Handler
	}{
		{
			// The case gzip broke: nothing written, so the status is still ours to set.
			name: "panic before any write",
			h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic("boom")
			}),
		},
		{
			// A body large enough to cross the compression threshold, so the gzip
			// path is genuinely engaged rather than falling back to identity.
			name: "panic before any write, large threshold crossed elsewhere",
			h: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				panic("boom")
			}),
		},
	} {
		for _, ae := range []string{"", "gzip"} {
			stack := s.middleware(tc.h)
			rr := gzGet(t, stack, "/api/status", ae)
			if rr.Code != http.StatusInternalServerError {
				t.Errorf("%s [Accept-Encoding:%q]: status %d, want 500", tc.name, ae, rr.Code)
			}
			// The error body must be readable, not gzip frames the client will try to
			// inflate after we have already told it the response is an error.
			if ce := rr.Header().Get("Content-Encoding"); ce != "" {
				t.Errorf("%s [Accept-Encoding:%q]: error response encoded as %q; the body is plaintext", tc.name, ae, ce)
			}
			if body := rr.Body.String(); !strings.Contains(body, "internal server error") {
				t.Errorf("%s [Accept-Encoding:%q]: body %q lacks the error text", tc.name, ae, body)
			}
		}
	}
}

// A handler that has already written cannot have its status changed - the header
// is gone. That is plain HTTP, not a compression bug, and it must stay true on
// both paths so the fix above is not mistaken for making panics recoverable
// after the fact.
func TestPanicAfterWriteKeepsTheCommittedStatusOnBothPaths(t *testing.T) {
	s := newTestServer(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":"` + strings.Repeat("x", 4096) + `"}`))
		panic("boom after write")
	})
	for _, ae := range []string{"", "gzip"} {
		rr := gzGet(t, s.middleware(h), "/api/status", ae)
		if rr.Code != http.StatusOK {
			t.Errorf("[Accept-Encoding:%q]: status %d, want 200 (already committed)", ae, rr.Code)
		}
	}
}

// The panic must not escape the stack: whatever the ordering, the daemon lives.
func TestPanicNeverEscapesTheMiddlewareStack(t *testing.T) {
	s := newTestServer(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	for _, ae := range []string{"", "gzip"} {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("[Accept-Encoding:%q]: panic escaped: %v", ae, rec)
				}
			}()
			gzGet(t, s.middleware(h), "/api/status", ae)
		}()
	}
}
