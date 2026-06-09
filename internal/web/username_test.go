package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

// A login username at the byte cap made of multi-byte runes must save and reload
// intact; one past the cap is rejected with 400 (not silently truncated). The
// server is the source of truth for the limit the UI's maxlength only hints at.
// Regression for B7.
func TestAuthUsernameUnicodeBoundary(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// Over-cap username is rejected up front. Do this before any password is set, so
	// auth is inactive and the request reaches the handler's validation (rather than
	// being turned away by the guard once auth is on).
	tooLong := strings.Repeat("é", 65) // 130 bytes, over the 128-byte cap
	if w := do(t, h, "POST", "/api/access", fmt.Sprintf(`{"username":%q}`, tooLong)); w.Code != http.StatusBadRequest {
		t.Fatalf("over-cap username: got %d, want 400", w.Code)
	}
	if s.settings.AuthUser() == tooLong {
		t.Fatal("a rejected over-cap username was persisted")
	}

	// A username exactly at the cap saves and reloads intact and valid.
	name := strings.Repeat("é", 64) // 64 * 2 bytes = 128 bytes (the cap)
	body := fmt.Sprintf(`{"username":%q,"password":"secret1"}`, name)
	if w := do(t, h, "POST", "/api/access", body); w.Code != http.StatusOK {
		t.Fatalf("save at the cap: got %d, body=%s", w.Code, w.Body.String())
	}
	if got := s.settings.AuthUser(); got != name || !utf8.ValidString(got) {
		t.Fatalf("username round-trip = %q (valid=%v), want %q", got, utf8.ValidString(got), name)
	}
}
