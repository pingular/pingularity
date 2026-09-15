package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedRetiredCityScope writes the two rows a pre-0.100 install left behind when
// its operator scoped automatic selection to a city, straight into the settings
// table the way that build did.
func seedRetiredCityScope(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.store.SetSettingsDiff(context.Background(), map[string]string{
		"speed_auto_loc":   "49.2827,-123.1207",
		"speed_auto_label": "Vancouver, BC",
	}); err != nil {
		t.Fatalf("seed the retired rows: %v", err)
	}
}

func retiredCityRows(t *testing.T, s *Server) map[string]string {
	t.Helper()
	m, err := s.store.AllSettings(context.Background())
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	out := map[string]string{}
	for _, k := range []string{"speed_auto_loc", "speed_auto_label"} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

// The city scope was retired with the server picker, and the settings POST used
// to answer 200 to a body that set it - the value went nowhere, was not echoed
// back, and steered nothing. A provisioning run that pins its speedtest scope
// that way then reports success at every boot forever while the daemon measures
// whatever the city race finds, which is the one failure mode a config API must
// not have: the operator is told the machine is configured the way they asked.
// Refuse it, and name what replaced it, so the script fails where it is wrong.
func TestSettingsRefusesAWriteToTheRetiredCityScope(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	seedRetiredCityScope(t, s)

	// The rest of the body is ordinary and valid: the refusal has to come before
	// anything is applied, or a half-applied save is worse than the silent one.
	w := do(t, h, "POST", "/api/settings", `{"speed_auto_loc":"48.8566,2.3522","speed_auto_label":"Paris, FR","speed_seconds":900}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("POST of a retired city scope answered HTTP %d, want 400 - a write that does nothing must not report success: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	body := w.Body.String()
	for _, want := range []string{"speed_auto_loc", "speed_auto_label", "speed_server_id", "speed_servers"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal never names %q, so it does not say what to use instead: %q", want, strings.TrimSpace(body))
		}
	}
	// The one sender of these keys that is not a script is the older release's
	// own page, left open across the upgrade: it posts the stored city back with
	// every save, and shows whatever comes back in the drawer. Whoever is looking
	// at that drawer needs to be told the one thing that gets their save through.
	if !strings.Contains(body, "reload it") {
		t.Errorf("the refusal never tells a dashboard page loaded before the upgrade to reload, and that page sends these keys with every save: %q", strings.TrimSpace(body))
	}
	if got := s.settings.SpeedInterval().Seconds(); got == 900 {
		t.Error("the refused save applied its other fields; a rejected body must change nothing")
	}
	// The operator's own rows are evidence of what they once asked for, and a
	// rollback should still find them: refusing the write must not rewrite them.
	if got := retiredCityRows(t, s); got["speed_auto_loc"] != "49.2827,-123.1207" || got["speed_auto_label"] != "Vancouver, BC" {
		t.Errorf("the stored rows changed to %v; the refusal must leave them exactly as the older build wrote them", got)
	}
}

// Either key on its own is a scope, and the coordinate on its own is the half
// that steered anything in the release that had them: a refusal that needed
// both would answer a coordinate-only provisioning body 200 again, which is the
// answer it exists to stop. The same city the install already carries is
// refused too - that is precisely the body a run that pinned it under the older
// release sends every time, and "the store would not change" is not the same
// thing as "the daemon does what you asked". Whitespace, on the other hand,
// asks for no scope at all, exactly as a blank value does.
func TestSettingsRefusesEitherRetiredKeyAndAnEchoOfTheStoredCity(t *testing.T) {
	for name, body := range map[string]string{
		"coordinate only":     `{"speed_auto_loc":"48.8566,2.3522","speed_seconds":900}`,
		"label only":          `{"speed_auto_label":"Paris, FR","speed_seconds":900}`,
		"the city it carries": `{"speed_auto_loc":"49.2827,-123.1207","speed_auto_label":"Vancouver, BC","speed_seconds":900}`,
	} {
		s := newTestServer(t)
		seedRetiredCityScope(t, s)
		w := do(t, s.Handler(), "POST", "/api/settings", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: POST answered HTTP %d, want 400: %s", name, w.Code, strings.TrimSpace(w.Body.String()))
		}
		if got := s.settings.SpeedInterval().Seconds(); got == 900 {
			t.Errorf("%s: the refused save applied speed_seconds anyway", name)
		}
	}

	s := newTestServer(t)
	w := do(t, s.Handler(), "POST", "/api/settings", `{"speed_auto_loc":"   ","speed_auto_label":"\t","speed_seconds":900}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST with whitespace-only retired keys answered HTTP %d, want 200 - whitespace asks for no scope, like a blank value: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := s.settings.SpeedInterval().Seconds(); got != 900 {
		t.Errorf("speed_seconds = %v after an accepted save, want 900", got)
	}
}

// An EMPTY value asks for the state the daemon is already in - no city scope at
// all - so answering it 200 tells the caller nothing false. The v0.70.1 page
// posted both keys on every Save, blank when no city was chosen, so a script
// modelled on that body still works; only an actual scope is refused.
func TestSettingsAcceptsAnEmptyRetiredCityScope(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	w := do(t, h, "POST", "/api/settings", `{"speed_auto_loc":"","speed_auto_label":"","speed_seconds":900}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST with blank retired keys answered HTTP %d, want 200: %s", w.Code, strings.TrimSpace(w.Body.String()))
	}
	if got := s.settings.SpeedInterval().Seconds(); got != 900 {
		t.Errorf("speed_seconds = %v after an accepted save, want 900", got)
	}
	var echo map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &echo); err != nil {
		t.Fatal(err)
	}
	if echo["speed_auto_loc"] != nil || echo["speed_auto_label"] != nil {
		t.Errorf("the retired keys came back in the echo (%v %v); they are not settings any more", echo["speed_auto_loc"], echo["speed_auto_label"])
	}
}

// apiSettingsEntry returns docs/api.md's `GET|POST /api/settings` item, up to
// the item after it.
func apiSettingsEntry(t *testing.T) string {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatalf("read docs/api.md: %v", err)
	}
	const item = "- `GET|POST /api/settings`"
	at := strings.Index(string(doc), item)
	if at < 0 {
		t.Fatalf("docs/api.md has no entry starting %q", item)
	}
	entry := string(doc[at:])
	if next := strings.Index(entry[1:], "\n- `"); next >= 0 {
		entry = entry[:next+1]
	}
	return entry
}

// The reference is the half of the contract a script author works from, and
// /api/settings' entry already lists what the daemon refuses outright. A key
// that answers 400 and is not named there is a 400 nobody can anticipate.
func TestRetiredSettingsKeysAreDocumented(t *testing.T) {
	entry := apiSettingsEntry(t)
	for _, k := range retiredSettingsKeys {
		if !strings.Contains(entry, "`"+k+"`") {
			t.Errorf("POST /api/settings refuses %s and docs/api.md's entry for the endpoint never names it:\n%s", k, entry)
		}
	}
	// Naming the keys is not the contract; what the entry says happens to them
	// is. An entry that named both and said they answer 200 would send a script
	// author to write code for the one answer the daemon never gives.
	flat := strings.Join(strings.Fields(entry), " ")
	s := newTestServer(t)
	code := do(t, s.Handler(), "POST", "/api/settings", `{"speed_auto_loc":"49.2827,-123.1207"}`).Code
	if want := fmt.Sprintf("a non-empty value for either is a `%d`", code); !strings.Contains(flat, want) {
		t.Errorf("a non-empty speed_auto_loc answers %d, and docs/api.md's entry never says %q:\n%s", code, want, entry)
	}
	if blank := do(t, s.Handler(), "POST", "/api/settings", `{"speed_auto_loc":"","speed_auto_label":""}`).Code; (blank == http.StatusOK) != strings.Contains(flat, "Sending them empty is still fine") {
		t.Errorf("blank retired keys answer %d, and docs/api.md's entry says sending them empty is still fine = %v:\n%s", blank, strings.Contains(flat, "Sending them empty is still fine"), entry)
	}
}
