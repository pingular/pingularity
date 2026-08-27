package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/logbuf"
	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/speedtest"
	"github.com/pingular/pingularity/internal/store"
	"github.com/pingular/pingularity/internal/update"
)

// The power button: POST toggles SetMonitoring, GET reports state; bad JSON -> 400.
func TestHandleMonitoring(t *testing.T) {
	h := newTestServer(t).Handler()
	if w := do(t, h, "POST", "/api/monitoring", `{"enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("POST monitoring %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/monitoring", "")
	var out map[string]bool
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["enabled"] {
		t.Fatal("monitoring should reflect the POSTed false")
	}
	if w := do(t, h, "POST", "/api/monitoring", `{nope`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
}

// Destructive: POST {type} clears all rows of a kind. Verify it deletes, reports
// the count, and rejects bad method / bad JSON / unknown type.
func TestHandleDataDelete(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	if err := s.store.InsertSamples(context.Background(), []store.Sample{
		{TS: time.Now(), Target: "cloudflare", Family: "ipv4", Success: true, LatencyMS: 10},
		{TS: time.Now(), Target: "google", Family: "ipv4", Success: true, LatencyMS: 12},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	w := do(t, h, "POST", "/api/data/delete", `{"type":"latency"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete %d: %s", w.Code, w.Body)
	}
	var out map[string]int64
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"] != 2 {
		t.Fatalf("deleted = %d, want 2", out["deleted"])
	}
	if w := do(t, h, "GET", "/api/data/delete", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/data/delete", `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/data/delete", `{"type":"bogus"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown type want 400, got %d", w.Code)
	}
}

// The one attacker-influenced surface: it dials a request-body URL. The pure
// guards (method, empty URL, bad JSON) must reject before any network use.
func TestHandleNotifyTest(t *testing.T) {
	h := newTestServer(t).Handler()
	if w := do(t, h, "GET", "/api/notify/test", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/notify/test", `{"url":"   "}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty URL want 400, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/notify/test", `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
}

// The settings round-trip: a posted schedule window must survive POST->GET, and
// out-of-range values must be clamped by normalize/sanitizeWindows.
func TestHandleSettingsRoundTrip(t *testing.T) {
	h := newTestServer(t).Handler()
	body := `{"latency_seconds":10,"speed_seconds":3600,"timeout_seconds":3,"down_after":2,"up_after":1,"ipv6_mode":"auto",
		"sched_lat_enabled":true,
		"sched_lat_windows":[{"days":"0111110","start":540,"end":1020},{"days":"1111111","start":99999,"end":7}]}`
	if w := do(t, h, "POST", "/api/settings", body); w.Code != http.StatusOK {
		t.Fatalf("POST settings %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings %d", w.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["sched_lat_enabled"] != true {
		t.Fatal("sched_lat_enabled did not persist")
	}
	wins, ok := got["sched_lat_windows"].([]any)
	if !ok || len(wins) != 2 {
		t.Fatalf("sched_lat_windows = %v", got["sched_lat_windows"])
	}
	w0 := wins[0].(map[string]any)
	if w0["days"] != "0111110" || w0["start"].(float64) != 540 || w0["end"].(float64) != 1020 {
		t.Fatalf("window 0 did not round-trip: %v", w0)
	}
	if s := wins[1].(map[string]any)["start"].(float64); s != 1439 {
		t.Fatalf("out-of-range start should clamp to 1439, got %v", s)
	}
}

// The alert/digest settings fields must survive a POST->GET round-trip and be
// clamped server-side. The dashboard binds inputs to these exact json tags, so a
// dropped field or tag drift would silently break the UI without this guard.
func TestHandleSettingsAlertFieldsRoundTrip(t *testing.T) {
	h := newTestServer(t).Handler()
	get := func() map[string]any {
		w := do(t, h, "GET", "/api/settings", "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET settings %d", w.Code)
		}
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	base := `"latency_seconds":10,"speed_seconds":3600,"timeout_seconds":3,"down_after":2,"up_after":1,"ipv6_mode":"auto"`
	body := `{` + base + `,"thresh_down_mbps":50,"thresh_up_mbps":10,"thresh_ping_ms":80,"thresh_jitter_ms":15,` +
		`"thresh_loss_pct":150,"thresh_consecutive":99,"thresh_bloat_down_ms":30,"thresh_bloat_up_ms":40,"digest_freq":"weekly"}`
	if w := do(t, h, "POST", "/api/settings", body); w.Code != http.StatusOK {
		t.Fatalf("POST settings %d: %s", w.Code, w.Body)
	}
	got := get()
	for k, want := range map[string]float64{
		"thresh_down_mbps": 50, "thresh_up_mbps": 10, "thresh_ping_ms": 80, "thresh_jitter_ms": 15,
		"thresh_loss_pct": 100 /*clamped from 150*/, "thresh_consecutive": 10, /*clamped from 99*/
		"thresh_bloat_down_ms": 30, "thresh_bloat_up_ms": 40,
	} {
		if f, _ := got[k].(float64); f != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if got["digest_freq"] != "weekly" {
		t.Errorf("digest_freq = %v, want weekly", got["digest_freq"])
	}
	// An invalid cadence must fall back to "off".
	if w := do(t, h, "POST", "/api/settings", `{`+base+`,"digest_freq":"monthly"}`); w.Code != http.StatusOK {
		t.Fatalf("POST settings(2) %d: %s", w.Code, w.Body)
	}
	if v := get()["digest_freq"]; v != "off" {
		t.Errorf("invalid digest_freq must clamp to off, got %v", v)
	}
}

// POST /api/settings is a PATCH: fields absent from the body must keep their
// stored values - a scripted partial update must not reset the other ~50
// settings or wipe the write-only iperf3 passwords.
func TestHandleSettingsPartialPost(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	seed := `{"latency_enabled":true,"webhook_url":"https://example.com/hook","speed_engine":"iperf3",
		"iperf_server":"10.0.0.5:5201",
		"iperf_servers":[{"addr":"10.0.0.5:5201","auth":true,"username":"bob","password":"secret"}]}`
	if w := do(t, h, "POST", "/api/settings", seed); w.Code != http.StatusOK {
		t.Fatalf("seed POST %d: %s", w.Code, w.Body)
	}
	// The partial update: one field only.
	if w := do(t, h, "POST", "/api/settings", `{"speedtest_enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("partial POST %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/settings", "")
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["speedtest_enabled"] != false {
		t.Error("patched field did not apply")
	}
	if got["latency_enabled"] != true || got["webhook_url"] != "https://example.com/hook" {
		t.Errorf("omitted fields were reset: latency_enabled=%v webhook_url=%v",
			got["latency_enabled"], got["webhook_url"])
	}
	srvs, _ := got["iperf_servers"].([]any)
	if len(srvs) != 1 {
		t.Fatalf("omitted iperf_servers were wiped: %v", got["iperf_servers"])
	}
	if sv := srvs[0].(map[string]any); sv["has_password"] != true {
		t.Errorf("stored iperf3 password was lost by the partial update: %v", sv)
	}
	if s.settings.IperfPassword() != "secret" {
		t.Errorf("stored password = %q, want secret", s.settings.IperfPassword())
	}
}

// The picker's saved list is a slice, so it is skipped by the DTO round-trip
// test above (that one walks pointer fields). Without this the star could reach
// the settings body, POST cleanly, and be gone at the next drawer open.
func TestSettingsSpeedServersRoundTrip(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	body := `{"speed_servers":[{"id":"1993","sponsor":"EBOX","name":"Montreal, QC","country":"Canada","lat":45.5,"lon":-73.5}],
		"speed_server_id":"1993"}`
	if w := do(t, h, "POST", "/api/settings", body); w.Code != http.StatusOK {
		t.Fatalf("POST %d: %s", w.Code, w.Body)
	}
	want := []settings.SavedServer{{ID: "1993", Sponsor: "EBOX", Name: "Montreal, QC", Country: "Canada", Lat: 45.5, Lon: -73.5}}
	if got := s.settings.SpeedServers(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored %+v, want %+v", got, want)
	}
	read := func() []any {
		w := do(t, h, "GET", "/api/settings", "")
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		list, _ := got["speed_servers"].([]any)
		return list
	}
	list := read()
	if len(list) != 1 {
		t.Fatalf("GET did not echo the saved list: %v", list)
	}
	if row := list[0].(map[string]any); row["id"] != "1993" || row["lat"] != 45.5 || row["country"] != "Canada" {
		t.Errorf("the coordinate or country did not survive the round trip: %v", row)
	}
	// Absent = keep, the same PATCH rule the iperf3 list follows: a form that
	// never mentions the list must not empty it.
	if w := do(t, h, "POST", "/api/settings", `{"speedtest_enabled":false}`); w.Code != http.StatusOK {
		t.Fatalf("partial POST %d: %s", w.Code, w.Body)
	}
	if len(read()) != 1 {
		t.Error("a partial update wiped the saved server list")
	}
	// An explicit empty list is the user unstarring the last one, and must apply.
	if w := do(t, h, "POST", "/api/settings", `{"speed_servers":[]}`); w.Code != http.StatusOK {
		t.Fatalf("clear POST %d: %s", w.Code, w.Body)
	}
	if n := len(s.settings.SpeedServers()); n != 0 {
		t.Errorf("unstarring every server left %d behind", n)
	}
}

// The browse listing is the picker's only trustworthy source of a server's
// coordinate: ServerInfo withholds Lat/Lon from JSON because the by-ID endpoint
// fills them with the CALLER's position (recentrePin). The list fetch's values
// are the catalogue's own, so they - and only they - go on the wire.
func TestBrowseServersCarryTheCatalogueCoordinate(t *testing.T) {
	b, err := json.Marshal(browseServers([]speedtest.ServerInfo{
		{ID: "1993", Sponsor: "EBOX", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5},
		{ID: "42", Sponsor: "ByID", Name: "Nowhere"}, // as GetOoklaServer leaves it
	}))
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got[0]["lat"] != 45.5 || got[0]["lon"] != -73.5 {
		t.Errorf("the listing dropped the coordinate the picker saves: %v", got[0])
	}
	if _, ok := got[1]["lat"]; ok {
		t.Errorf("a server with no catalogue position must send none, not a zero: %v", got[1])
	}
	// The plain type must still withhold them, or the reason for this wrapper is gone.
	plain, _ := json.Marshal(speedtest.ServerInfo{ID: "1993", Lat: 45.5, Lon: -73.5})
	if strings.Contains(string(plain), "45.5") {
		t.Error("ServerInfo started serializing Lat/Lon; the by-ID reply would now carry our own position")
	}
}

// ...and the browse endpoint has to actually send that shape. The picker stars a
// server straight out of this response, so a listing that dropped the coordinate
// would store every kept server at 0,0 and no test of the wrapper alone notices.
func TestBrowseEndpointSendsTheCoordinate(t *testing.T) {
	old := listOoklaServers
	listOoklaServers = func(_ context.Context, lat, lon float64) ([]speedtest.ServerInfo, error) {
		return []speedtest.ServerInfo{{ID: "1993", Sponsor: "EBOX", Name: "Montreal, QC", Lat: 45.5, Lon: -73.5}}, nil
	}
	t.Cleanup(func() { listOoklaServers = old })
	s := &Server{netinfo: stubNetInfo{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers", nil)
	req.Header.Set("Content-Type", "application/json")
	s.handleSpeedtestServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Servers) != 1 {
		t.Fatalf("servers = %v", body.Servers)
	}
	if body.Servers[0]["lat"] != 45.5 || body.Servers[0]["lon"] != -73.5 {
		t.Errorf("the browse listing did not carry the coordinate: %v", body.Servers[0])
	}
	if body.Servers[0]["sponsor"] != "EBOX" {
		t.Errorf("the wrapper dropped a ServerInfo field: %v", body.Servers[0])
	}
}

// The pin and the auto scope travel as three independent *string keys, and the
// picker relies on two things the DTO's pointer fields give it that nothing
// pinned: an explicit "" is stored as Auto (an omitted key keeps the pin), and
// a pin never clears a saved scope on the daemon side - the scope still centres
// a pinned best-of run's companions when the pin's own position cannot be
// found, and a legacy install has no way to recreate one.
func TestSettingsPinAndScopeTravelIndependently(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	get := func() map[string]any {
		w := do(t, h, "GET", "/api/settings", "")
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if w := do(t, h, "POST", "/api/settings", `{"speed_server_id":"1993"}`); w.Code != http.StatusOK {
		t.Fatalf("POST %d: %s", w.Code, w.Body)
	}
	if got := get(); got["speed_server_id"] != "1993" {
		t.Fatalf("pin did not survive: %v", got["speed_server_id"])
	}
	// A body that never mentions the pin keeps it...
	if w := do(t, h, "POST", "/api/settings", `{"speedtest_enabled":true}`); w.Code != http.StatusOK {
		t.Fatalf("partial POST %d: %s", w.Code, w.Body)
	}
	if got := get(); got["speed_server_id"] != "1993" {
		t.Errorf("an omitted speed_server_id must keep the pin, got %v", got["speed_server_id"])
	}
	// ...and an explicit "" is Auto, stored and echoed as such - the failure the
	// picker rewrite exists to prevent is a Save that posts "" by mistake, and
	// this echo is the only place the loss is visible.
	w := do(t, h, "POST", "/api/settings", `{"speed_server_id":""}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear POST %d: %s", w.Code, w.Body)
	}
	var echo map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &echo); err != nil {
		t.Fatal(err)
	}
	if echo["speed_server_id"] != "" || s.settings.SpeedServerID() != "" {
		t.Errorf("an explicit \"\" must store and echo as Auto, got echo %v stored %q", echo["speed_server_id"], s.settings.SpeedServerID())
	}
	// The retired city scope is not a setting any more: a legacy key in the body
	// is ignored, never echoed, and never resurrected as a centre.
	if w := do(t, h, "POST", "/api/settings", `{"speed_auto_loc":"45.5,-73.5","speed_auto_label":"Montreal"}`); w.Code != http.StatusOK {
		t.Fatalf("legacy scope POST %d: %s", w.Code, w.Body)
	}
	if got := get(); got["speed_auto_loc"] != nil || got["speed_auto_label"] != nil {
		t.Errorf("the retired scope keys must not be echoed, got %v %v", got["speed_auto_loc"], got["speed_auto_label"])
	}
}

// A never-set list reads as [] and never null, on the live values and in the
// defaults block alike. The existing round-trip test decodes into []any, which
// cannot tell the two apart, so this reads the raw body.
func TestSettingsSpeedServersNeverNull(t *testing.T) {
	w := do(t, newTestServer(t).Handler(), "GET", "/api/settings", "")
	var got struct {
		SpeedServers json.RawMessage `json:"speed_servers"`
		Defaults     struct {
			SpeedServers json.RawMessage `json:"speed_servers"`
		} `json:"defaults"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if string(got.SpeedServers) != "[]" || string(got.Defaults.SpeedServers) != "[]" {
		t.Errorf("a never-set list must read as [] (live and defaults), not null or absent: live %s defaults %s", got.SpeedServers, got.Defaults.SpeedServers)
	}
}

// The by-ID lookup fails for two reasons and only one is the user's to fix.
// Everything used to be 404 "server not found" - including a transport error,
// an Ookla 5xx and this handler's own deadline - and the page reads a 404 as
// "no such server" and refuses to save the pin. An outage must not do that.
func TestBrowseByIDStatusDistinguishesNotFoundFromUnreachable(t *testing.T) {
	old := getOoklaServer
	t.Cleanup(func() { getOoklaServer = old })
	ask := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		s := &Server{netinfo: stubNetInfo{}}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers?id=1993", nil)
		req.Header.Set("Content-Type", "application/json")
		s.handleSpeedtestServers(rec, req)
		return rec
	}
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"no such server":  {speedtest.ErrServerNotFound, http.StatusNotFound},
		"wrapped":         {fmt.Errorf("fetch: %w", speedtest.ErrServerNotFound), http.StatusNotFound},
		"transport":       {&url.Error{Op: "Get", URL: "https://www.speedtest.net/", Err: errors.New("dial tcp: connection refused")}, http.StatusBadGateway},
		"malformed body":  {errors.New("XML syntax error on line 1: expected element name after <"), http.StatusBadGateway}, // an Ookla error page, the usual (malformed-HTML) case; a well-formed one still decodes to "not found" - see the handler
		"handler timeout": {&url.Error{Op: "Get", URL: "https://www.speedtest.net/", Err: context.DeadlineExceeded}, http.StatusGatewayTimeout},
	} {
		getOoklaServer = func(_ context.Context, id string) (speedtest.ServerInfo, error) {
			return speedtest.ServerInfo{}, tc.err
		}
		if rec := ask(t); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (body %q)", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// And the shape the picker relies on when it does answer: exactly one row,
	// with fallback_ok absent (not false) when the probe was inconclusive.
	getOoklaServer = func(_ context.Context, id string) (speedtest.ServerInfo, error) {
		return speedtest.ServerInfo{ID: id, Sponsor: "EBOX", Name: "Montreal, QC"}, nil
	}
	rec := ask(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Servers) != 1 || body.Servers[0]["id"] != "1993" {
		t.Fatalf("by-ID reply = %v, want exactly the one server", body.Servers)
	}
	if _, ok := body.Servers[0]["fallback_ok"]; ok {
		t.Errorf("an inconclusive probe must leave fallback_ok absent, not false: %v", body.Servers[0])
	}
}

// The status poll names the last run's server by ID as well as by label, and
// only for engines that have one. The picker keys its "last run" bookkeeping on
// the ID, so the wire has to promise it. A guard on a contract that predates
// this change, not a test of it.
func TestStatusCarriesLastRunServerID(t *testing.T) {
	s := newMetricsServer(t)
	ctx := context.Background()
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: 1000, Server: "Example ISP, Newtown", ServerID: "777", Engine: "ookla"}); err != nil {
		t.Fatal(err)
	}
	speed := func() map[string]any {
		w := do(t, s.Handler(), "GET", "/api/status", "")
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		sp, _ := got["speed"].(map[string]any)
		return sp
	}
	if sp := speed(); sp == nil || sp["server_id"] != "777" || sp["server"] != "Example ISP, Newtown" {
		t.Errorf("speed = %v, want server_id 777 beside the label", sp)
	}
	if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: 2000, Server: "iperf.local", Engine: "iperf3"}); err != nil {
		t.Fatal(err)
	}
	if sp := speed(); sp == nil || sp["engine"] != "iperf3" {
		t.Fatalf("speed = %v, want the iperf3 run", sp)
	} else if _, ok := sp["server_id"]; ok {
		t.Errorf("an iperf3 run has no Ookla ID and must not send one: %v", sp)
	}
}

// The by-ID reply is the one that must NOT carry a coordinate: that endpoint
// answers with the caller's own position for a server on the caller's ISP, and
// the picker stars whatever this reply describes.
func TestBrowseByIDSendsNoCoordinate(t *testing.T) {
	old := getOoklaServer
	getOoklaServer = func(_ context.Context, id string) (speedtest.ServerInfo, error) {
		// As the endpoint answers for an ISP-owned server: our coordinates, its name.
		return speedtest.ServerInfo{ID: id, Sponsor: "EBOX", Name: "Montreal, QC", Lat: 43.65, Lon: -79.38}, nil
	}
	t.Cleanup(func() { getOoklaServer = old })
	s := &Server{netinfo: stubNetInfo{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/speedtest/servers?id=1993", nil)
	req.Header.Set("Content-Type", "application/json")
	s.handleSpeedtestServers(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "43.65") {
		t.Errorf("the by-ID reply carried a coordinate; starring it would file the server at our own address: %s", rec.Body)
	}
}

// decodeJSONBody is the CSRF gate: a POST with a non-JSON content type must be
// rejected with 415 before any handler logic runs.
func TestContentTypeGate(t *testing.T) {
	h := newTestServer(t).Handler()
	r := httptest.NewRequest("POST", "/api/monitoring", strings.NewReader(`{"enabled":true}`))
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Content-Type", "text/plain") // wrong type
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-JSON content type want 415, got %d", w.Code)
	}
}

// The 3-second dashboard poll. With a wired StatusFunc it must return the
// expected shape, and ?dataMins must add the custom window without panicking.
func TestHandleStatusShape(t *testing.T) {
	h := newMetricsServer(t).Handler() // wires a non-nil StatusFunc
	w := do(t, h, "GET", "/api/status?dataMins=60", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"online", "paused", "families", "speedtest_enabled", "data_usage", "data_used_custom"} {
		if _, ok := got[k]; !ok {
			t.Errorf("status response missing key %q", k)
		}
	}
}

// A Server built without a StatusFunc (misconfiguration) must degrade to 503,
// not panic on the nil dereference.
func TestHandleStatusNilGuard(t *testing.T) {
	h := newTestServer(t).Handler() // nil StatusFunc
	if w := do(t, h, "GET", "/api/status", ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil StatusFunc want 503, got %d", w.Code)
	}
}

// A Server built without a NetInfo collector (New(..., ni=nil, ...), which the
// signature permits and newTestServer uses) must degrade the netinfo-backed
// endpoints to 503, not panic on the nil dereference into a recovered 500.
func TestNetinfoNilGuard(t *testing.T) {
	h := newTestServer(t).Handler() // nil NetInfo
	if w := do(t, h, "GET", "/api/netinfo", ""); w.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /api/netinfo nil netinfo want 503, got %d", w.Code)
	}
	// POST + JSON: the endpoint reaches out, so it refuses simple cross-site
	// shapes before it gets as far as the nil guard (see handleSpeedtestServers).
	if w := do(t, h, "POST", "/api/speedtest/servers", `{}`); w.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /api/speedtest/servers nil netinfo want 503, got %d", w.Code)
	}
}

// The CSV export must run free-text columns (server/isp/dns/exit) through csvSafe
// so a hostile field can't execute as a spreadsheet formula on open.
func TestSpeedRunsCSVEscapesFormulaInjection(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.InsertSpeed(context.Background(), store.SpeedSample{
		TS: time.Now().Unix(), DownMbps: 100, UpMbps: 20, PingMS: 10,
		Server: `=HYPERLINK("http://evil","pwn")`, ISP: "+SUM(A1)",
	}); err != nil {
		t.Fatalf("seed speed run: %v", err)
	}
	w := do(t, s.Handler(), "GET", "/api/speed/runs.csv", "")
	if w.Code != http.StatusOK {
		t.Fatalf("csv export %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `'=HYPERLINK`) {
		t.Errorf("Server cell was not escaped (csvSafe not wired):\n%s", body)
	}
	if !strings.Contains(body, `'+SUM`) {
		t.Errorf("ISP cell was not escaped:\n%s", body)
	}
}

func TestHandleSpeedRunDelete(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	for _, ts := range []int64{1000, 2000, 3000} {
		if err := s.store.InsertSpeed(ctx, store.SpeedSample{TS: ts, DownMbps: 100, UpMbps: 20, PingMS: 10}); err != nil {
			t.Fatalf("seed %d: %v", ts, err)
		}
	}
	// Delete the middle run; only that one goes, and the count drops by one.
	w := do(t, h, "POST", "/api/speed/runs/delete", `{"ts":2000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete %d: %s", w.Code, w.Body)
	}
	var out map[string]int64
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"] != 1 {
		t.Fatalf("deleted = %d, want 1", out["deleted"])
	}
	if n, _ := s.store.SpeedCount(ctx); n != 2 {
		t.Fatalf("remaining = %d, want 2", n)
	}
	// Deleting it again is an idempotent no-op (deleted:0), not an error.
	w = do(t, h, "POST", "/api/speed/runs/delete", `{"ts":2000}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-delete %d: %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"] != 0 {
		t.Fatalf("re-delete deleted = %d, want 0", out["deleted"])
	}
	// Method, malformed body, and a missing/zero ts are all rejected.
	if w := do(t, h, "GET", "/api/speed/runs/delete", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/speed/runs/delete", `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/speed/runs/delete", `{"ts":0}`); w.Code != http.StatusBadRequest {
		t.Fatalf("zero ts want 400, got %d", w.Code)
	}
}

// Backup/restore: an export must round-trip back through import on a fresh box,
// including the DNS-resolve series the latency category bundles; no-category
// export -> 400, bad/non-export/newer-version import JSON -> 400.
func TestImportExportRoundTrip(t *testing.T) {
	s := newTestServer(t)
	if err := s.store.InsertSamples(context.Background(), []store.Sample{
		{TS: time.Unix(1000, 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10},
		{TS: time.Unix(1001, 0), Target: "google", Family: "ipv4", Success: true, LatencyMS: 12},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.store.InsertDNS(context.Background(), time.Unix(1000, 0), 23.5, true); err != nil {
		t.Fatalf("seed dns: %v", err)
	}
	w := do(t, s.Handler(), "GET", "/api/export?latency=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export %d: %s", w.Code, w.Body)
	}
	exported := w.Body.String()
	if !strings.Contains(exported, `"dns":[`) {
		t.Errorf("latency export must include the dns series:\n%s", exported)
	}

	s2 := newTestServer(t) // fresh box
	if w := do(t, s2.Handler(), "POST", "/api/import?latency=1", exported); w.Code != http.StatusOK {
		t.Fatalf("import %d: %s", w.Code, w.Body)
	}
	if cnt, _ := s2.store.TableCounts(context.Background()); cnt["samples"] != 2 {
		t.Errorf("imported samples = %d, want 2", cnt["samples"])
	}
	// The dns rows made it too: a re-export from the restored box carries them.
	if re := do(t, s2.Handler(), "GET", "/api/export?latency=1", ""); !strings.Contains(re.Body.String(), `"latency_ms":23.5`) {
		t.Errorf("dns series lost on import round-trip:\n%s", re.Body.String())
	}
	if w := do(t, s.Handler(), "GET", "/api/export", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("no-category export want 400, got %d", w.Code)
	}
	if w := do(t, s2.Handler(), "POST", "/api/import?latency=1", `{bad`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad import JSON want 400, got %d", w.Code)
	}
	// Envelope checks: random JSON isn't an export; a newer format is refused
	// with a friendly message instead of silently importing nothing.
	if w := do(t, s2.Handler(), "POST", "/api/import?latency=1", `{"foo":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("non-export JSON want 400, got %d", w.Code)
	}
	// Relative to exportSchema, not a literal: this asserts "one past whatever we
	// currently write is refused", which stays true across bumps. Hardcoding the
	// number made this test fail the moment the schema legitimately moved to 2.
	future := `{"pingularity_export":` + itoa(exportSchema+1) + `,"latency":[]}`
	w = do(t, s2.Handler(), "POST", "/api/import?latency=1", future)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "newer") {
		t.Fatalf("newer-version import want 400/newer, got %d: %s", w.Code, w.Body)
	}
}

// Body-less mutating POSTs require an application/json content-type, so a
// cross-site no-cors POST (which can't set it without a CORS preflight) is blocked
// even though it carries no body.
func TestBodylessPostRequiresJSONCT(t *testing.T) {
	h := newTestServer(t).Handler()
	// No content-type -> 415 (CSRF guard), despite the empty body.
	if w := do(t, h, "POST", "/api/speedtest", ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("POST /api/speedtest without content-type: got %d, want 415", w.Code)
	}
	// With application/json the guard passes (then 503: speedtests disabled here).
	if w := do(t, h, "POST", "/api/speedtest", "{}"); w.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("POST /api/speedtest with application/json should pass the CSRF guard, got 415")
	}
}

func TestHandleOutageDelete(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	ctx := context.Background()
	base := time.Now().Unix() - 7200
	seed := func(ts int64, typ string, dur int) {
		t.Helper()
		if err := s.store.InsertEvent(ctx, time.Unix(ts, 0), typ, dur, ""); err != nil {
			t.Fatalf("seed %s@%d: %v", typ, ts, err)
		}
	}
	seed(base+1000, "down", -1)
	seed(base+1100, "up", 100)
	seed(base+2000, "down", -1)
	seed(base+2300, "up", 300)

	// Delete the second outage: its up AND its down both go.
	w := do(t, h, "POST", "/api/outages/delete", `{"ts":`+strconv.FormatInt(base+2300, 10)+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete %d: %s", w.Code, w.Body)
	}
	var out map[string]int64
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"] != 2 {
		t.Fatalf("deleted = %d, want 2", out["deleted"])
	}
	if n, _ := s.store.EventCount(ctx); n != 2 {
		t.Fatalf("remaining events = %d, want 2", n)
	}
	// Idempotent re-delete.
	w = do(t, h, "POST", "/api/outages/delete", `{"ts":`+strconv.FormatInt(base+2300, 10)+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-delete %d: %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out["deleted"] != 0 {
		t.Fatalf("re-delete deleted = %d, want 0", out["deleted"])
	}
	// Method, malformed body, and a missing/zero ts are all rejected.
	if w := do(t, h, "GET", "/api/outages/delete", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET want 405, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/outages/delete", `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON want 400, got %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/outages/delete", `{"ts":0}`); w.Code != http.StatusBadRequest {
		t.Fatalf("zero ts want 400, got %d", w.Code)
	}
}

// dtoFrom must populate every form-owned pointer field: a field added to the DTO
// and the Patch mapping but forgotten in dtoFrom stays nil, so GET and the POST
// echo emit null and the dashboard silently reverts the value after each save.
func TestDtoFromPopulatesEveryFormField(t *testing.T) {
	dto := dtoFrom(settings.Values{})
	rv := reflect.ValueOf(dto)
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if f.Name == "Defaults" { // GET-only nested defaults, set separately
			continue
		}
		if f.Type.Kind() == reflect.Ptr && rv.Field(i).IsNil() {
			t.Errorf("dtoFrom leaves %s (json %q) nil; a saved value would not echo back", f.Name, f.Tag.Get("json"))
		}
	}
}

// Full settings round-trip: a populated Values -> dtoFrom -> JSON -> the
// handleSettings Patch mapping -> Update -> echo. Every form pointer field must
// survive; a field dropped from the Patch mapping reverts to its default and is
// caught here. Mirrors the settings-side TestFormKeysOverlayRoundTrip.
func TestSettingsDTORoundTrip(t *testing.T) {
	s := newTestServer(t)
	want := settings.Values{
		Latency: 10 * time.Second, LatencyEnabled: true,
		Speed:             3600 * time.Second,
		Retention:         7 * 24 * time.Hour,
		SpeedRetention:    30 * 24 * time.Hour,
		DowntimeRetention: 365 * 24 * time.Hour,
		Timeout:           5 * time.Second,
		DownAfter:         3, UpAfter: 2,
		SpeedServerID:    "1234",
		SpeedtestEnabled: true, SpeedtestOnReconnect: true, IPv6Mode: "on", ExitTarget: "8.8.8.8", DNSProbe: true, NetinfoEnabled: true,
		SpeedtestAdaptive: true, SpeedtestOnDegraded: true, DegradedPingMS: 120, SpeedtestSkipBusy: true, SpeedBusyMbps: 5,
		SpeedEngine: "iperf3", IperfServer: "10.0.0.5:5201",
		IperfDur: 15, IperfStreams: 4, OoklaConnections: 8, OoklaLoss: true, SpeedBestOfCount: 3, IperfOmit: 2, SpeedDirection: "down", IperfUDP: true,
		// Direction/Retries are per-engine: distinct values from the Ookla pair above.
		IperfDirection: "bidir", IperfRetries: 3,
		IperfUDPRate: 200, IperfWindow: 4096, SpeedRetries: 2,
		IperfCongestion: "bbr", IperfNoDelay: true, IperfDSCP: "ef", IperfMSS: 1400,
		ThreshDownMbps: 100, ThreshUpMbps: 20, ThreshPingMS: 50, ThreshJitterMS: 10,
		ThreshLossPct: 5, ThreshConsec: 3, ThreshBloatDownMS: 80, ThreshBloatUpMS: 90,
		AlertOnOutage: true, WebhookURL: "https://example.com/hook",
		HeartbeatURL: "https://hc.example.com/uuid", DigestFreq: "weekly", WebhookFormat: "ntfy",
		QuickSetupDone:  true, // server-owned: must NOT be settable via /api/settings (asserted below)
		SchedLatEnabled: true, SchedLatWindows: []settings.Window{{Days: "0111110", Start: 540, End: 1020}},
		SchedSpeedEnabled: true, SchedSpeedWindows: []settings.Window{{Days: settings.AllDays, Start: 0, End: 0}},
	}
	body, err := json.Marshal(dtoFrom(want))
	if err != nil {
		t.Fatal(err)
	}
	w := do(t, s.Handler(), "POST", "/api/settings", string(body))
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: %d %s", w.Code, w.Body)
	}
	var echo settingsDTO
	if err := json.Unmarshal(w.Body.Bytes(), &echo); err != nil {
		t.Fatal(err)
	}
	// Compare the persisted result against what we sent, field by field over the
	// DTO pointers, so a Patch-mapping omission (value reverts to default) fails
	// with the exact json tag.
	in := dtoFrom(want)
	got := dtoFrom(s.settings.Snapshot())
	rin, rgot, rt := reflect.ValueOf(in), reflect.ValueOf(got), reflect.TypeOf(in)
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		// QuickSetupDone is the server-owned first-run marker: /api/settings must
		// NOT write it (a generic settings POST could otherwise reopen Quick Setup
		// or re-freeze defaults through it). It is deliberately not in the Patch,
		// so it can't round-trip - asserted explicitly after the loop.
		if f.Type.Kind() != reflect.Ptr || f.Name == "Defaults" || f.Name == "QuickSetupDone" {
			continue
		}
		a, b := rin.Field(i).Elem().Interface(), rgot.Field(i).Elem().Interface()
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s (json %q) did not round-trip: sent %v, persisted %v", f.Name, f.Tag.Get("json"), a, b)
		}
	}
	// The back-door is closed: we POSTed quick_setup_done:true to a fresh server;
	// it must stay unanswered because /api/settings ignores the marker.
	if s.settings.QuickSetupDone() {
		t.Error("quick_setup_done was set through /api/settings - the server-owned marker must be unwritable there")
	}
	// The write-only iperf server list round-trips through its own DTO mapping.
	if len(echo.SchedLatWindows) != 1 || echo.SchedLatWindows[0].Start != 540 {
		t.Errorf("sched_lat_windows lost on round-trip: %+v", echo.SchedLatWindows)
	}
}

// The POST /api/settings echo must carry the same server-clock fields the GET
// response does, or the schedule "now" marker reverts to the browser's timezone
// after any save.
func TestSettingsPostEchoIncludesServerNow(t *testing.T) {
	h := newTestServer(t).Handler()
	w := do(t, h, "POST", "/api/settings", `{"latency_seconds":10}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST settings: %d %s", w.Code, w.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["server_now_unix"]; !ok {
		t.Error("POST echo omits server_now_unix; the schedule now-marker falls back to browser time")
	}
}

// A partial import that applies the config category and then fails on a later
// category must still reload settings, so the applied config takes effect live
// instead of lurking in the DB until the next restart.
func TestImportPartialConfigReloads(t *testing.T) {
	s := newTestServer(t)
	// config applies first; the speed category value is not an array, so its
	// json.Unmarshal fails and the import returns an error after config committed.
	// exit_target is an exportable settings key (unlike auth_hash, which the
	// export denylist strips), so it round-trips through import + Reload.
	payload := `{"pingularity_export":1,` +
		`"config":[{"key":"exit_target","value":"8.8.8.8"}],` +
		`"speed":"not-an-array"}`
	w := do(t, s.Handler(), "POST", "/api/import?config=1&speed=1", payload)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("partial import (bad speed) want 400, got %d: %s", w.Code, w.Body)
	}
	if got := s.settings.ExitTarget(); got != "8.8.8.8" {
		t.Fatalf("applied config not reloaded after partial import: exit_target = %q", got)
	}
}

// The import must accept the product's own default-sized exports: bodies well
// past the old 64 MiB whole-body cap (streamed, so memory stays flat), and row
// counts crossing several 5000-row store batches.
func TestImportStreamsLargeBackups(t *testing.T) {
	// A body bigger than the old cap, mostly padding in a skipped unknown key,
	// must not be rejected up front.
	s := newTestServer(t)
	pad := strings.Repeat("x", 65<<20)
	body := `{"pingularity_export":1,"pad":"` + pad + `","latency":[{"ts":1000,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]}`
	if w := do(t, s.Handler(), "POST", "/api/import?latency=1", body); w.Code != http.StatusOK {
		t.Fatalf("large-body import: got %d: %s", w.Code, w.Body)
	}
	if cnt, _ := s.store.TableCounts(context.Background()); cnt["samples"] != 1 {
		t.Errorf("samples = %d, want 1", cnt["samples"])
	}

	// Multi-batch: more rows than one handler batch (5000).
	src := newTestServer(t)
	rows := make([]store.Sample, 12001)
	for i := range rows {
		rows[i] = store.Sample{TS: time.Unix(int64(1000+i), 0), Target: "cf", Family: "ipv4", Success: true, LatencyMS: 10}
	}
	if err := src.store.InsertSamples(context.Background(), rows); err != nil {
		t.Fatalf("seed: %v", err)
	}
	exported := do(t, src.Handler(), "GET", "/api/export?latency=1", "").Body.String()
	dst := newTestServer(t)
	if w := do(t, dst.Handler(), "POST", "/api/import?latency=1", exported); w.Code != http.StatusOK {
		t.Fatalf("multi-batch import: got %d: %s", w.Code, w.Body)
	}
	if cnt, _ := dst.store.TableCounts(context.Background()); cnt["samples"] != 12001 {
		t.Errorf("samples = %d, want 12001", cnt["samples"])
	}
}

// A category present in the file but not selected in the query is skipped as
// raw tokens: its rows are not applied, and even a corrupt row inside it (an
// unknown column, malformed values) cannot fail the selected import.
func TestImportSkipsUnselectedCategories(t *testing.T) {
	s := newTestServer(t)
	body := `{"pingularity_export":1,` +
		`"speed":[{"ts":2000,"bogus_column_from_the_future":1}],` +
		`"latency":[{"ts":1000,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]}`
	w := do(t, s.Handler(), "POST", "/api/import?latency=1", body)
	if w.Code != http.StatusOK {
		t.Fatalf("import: got %d: %s", w.Code, w.Body)
	}
	cnt, _ := s.store.TableCounts(context.Background())
	if cnt["samples"] != 1 || cnt["speed"] != 0 {
		t.Errorf("samples=%d speed=%d, want 1 and 0", cnt["samples"], cnt["speed"])
	}
}

// Restoring a config backup that had login enabled cannot bring the password
// with it (auth_hash is never exported), so the import must NOT leave the login
// toggle on with nothing enforcing it: auth flips off and the response says so.
func TestImportConfigWithoutPasswordDisablesAuth(t *testing.T) {
	src := newTestServer(t)
	hash, err := hashPassword("hunter2-hunter2")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ctx := context.Background()
	if err := src.settings.SetAuthPassword(ctx, "admin", hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if err := src.settings.SetAuthEnabled(ctx, true); err != nil {
		t.Fatalf("enable auth: %v", err)
	}
	// Source is protected by "LAN access + login" - the dangerous combination to
	// restore onto a passwordless box (audit: access restore must fail closed).
	if err := src.settings.SetAccessLocalOnly(ctx, false); err != nil {
		t.Fatalf("open LAN access: %v", err)
	}
	// Auth is live on the source now, so the export itself must authenticate.
	req := httptest.NewRequest("GET", "/api/export?config=1", nil)
	req.Host = "127.0.0.1:9000"
	req.SetBasicAuth("admin", "hunter2-hunter2")
	rec := httptest.NewRecorder()
	src.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: got %d: %s", rec.Code, rec.Body)
	}
	exported := rec.Body.String()
	if strings.Contains(exported, "auth_hash") {
		t.Fatalf("export leaked auth_hash:\n%s", exported)
	}

	dst := newTestServer(t) // fresh box: no local password
	w := do(t, dst.Handler(), "POST", "/api/import?config=1", exported)
	if w.Code != http.StatusOK {
		t.Fatalf("import: got %d: %s", w.Code, w.Body)
	}
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response: %v", err)
	}
	if len(resp.Warnings) == 0 || !strings.Contains(resp.Warnings[0], "password") {
		t.Errorf("want a no-password warning, got %v", resp.Warnings)
	}
	if dst.settings.AuthEnabled() || dst.settings.AuthActive() {
		t.Errorf("imported auth intent must be cleared when unenforceable: enabled=%v active=%v",
			dst.settings.AuthEnabled(), dst.settings.AuthActive())
	}
	// Fail CLOSED: login couldn't be restored, so LAN access must be forced off -
	// else "LAN + login" would restore as "LAN, no login" and expose the dashboard.
	if !dst.settings.AccessLocalOnly() {
		t.Error("access must be forced local-only when imported auth is unenforceable (would otherwise fail open to the LAN)")
	}

	// Restoring onto a box that HAS its own password keeps login on, silently.
	// (SetAuthPassword also enables auth, so this import must authenticate.)
	dst2 := newTestServer(t)
	if err := dst2.settings.SetAuthPassword(ctx, "admin", hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	req = httptest.NewRequest("POST", "/api/import?config=1", strings.NewReader(exported))
	req.Host = "127.0.0.1:9000"
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "hunter2-hunter2")
	rec = httptest.NewRecorder()
	dst2.Handler().ServeHTTP(rec, req)
	if w = rec; w.Code != http.StatusOK {
		t.Fatalf("import: got %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "warnings") {
		t.Errorf("no warning expected when a local password exists: %s", w.Body)
	}
	if !dst2.settings.AuthActive() {
		t.Error("auth should be active after restoring onto a box with its own password")
	}
}

// The About-tab log viewer endpoint: GET returns {level, redact, lines} from
// the seeded ring, POST sets the level / redaction / clears the buffer, and
// ?download=1 serves the lines as a plain-text attachment.
func TestHandleLogs(t *testing.T) {
	s := newTestServer(t)
	s.Logs = logbuf.New(10)
	s.Logs.Append("ip=1.2.3.4 line one", "ip=[redacted] line one")
	s.Logs.Append("ip=5.6.7.8 line two", "ip=[redacted] line two")
	h := s.Handler()

	type logsResp struct {
		Level  string         `json:"level"`
		Redact bool           `json:"redact"`
		Lines  []logbuf.Entry `json:"lines"`
	}
	get := func(w *httptest.ResponseRecorder) logsResp {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("logs request: %d %s", w.Code, w.Body)
		}
		var out logsResp
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	out := get(do(t, h, "GET", "/api/logs", ""))
	if out.Level != s.settings.LogLevel() {
		t.Errorf("level = %q, want the live setting %q", out.Level, s.settings.LogLevel())
	}
	if out.Redact != s.settings.LogRedactPII() {
		t.Errorf("redact = %v, want the live setting %v", out.Redact, s.settings.LogRedactPII())
	}
	if len(out.Lines) != 2 || out.Lines[0].Raw != "ip=1.2.3.4 line one" || out.Lines[0].Masked != "ip=[redacted] line one" {
		t.Errorf("lines = %+v, want the seeded ring with both forms", out.Lines)
	}

	if out = get(do(t, h, "POST", "/api/logs", `{"level":"debug"}`)); out.Level != "debug" {
		t.Errorf("POST level echo = %q, want debug", out.Level)
	}
	if s.settings.LogLevel() != "debug" {
		t.Errorf("LogLevel = %q, want debug", s.settings.LogLevel())
	}

	if out = get(do(t, h, "POST", "/api/logs", `{"redact":true}`)); !out.Redact || !s.settings.LogRedactPII() {
		t.Error("POST redact:true did not apply")
	}
	if out = get(do(t, h, "POST", "/api/logs", `{"redact":false}`)); out.Redact || s.settings.LogRedactPII() {
		t.Error("POST redact:false did not apply")
	}

	// Default download is the full (raw) form; masked=1 serves the masked form so a
	// shared bug report is clean.
	w := do(t, h, "GET", "/api/logs?download=1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("download Content-Type = %q, want text/plain", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("download Content-Disposition = %q, want an attachment", cd)
	}
	if body := w.Body.String(); !strings.Contains(body, "ip=1.2.3.4 line one") || !strings.Contains(body, "ip=5.6.7.8 line two") {
		t.Errorf("raw download missing the ring lines: %q", body)
	}
	wm := do(t, h, "GET", "/api/logs?download=1&masked=1", "")
	if body := wm.Body.String(); strings.Contains(body, "1.2.3.4") || !strings.Contains(body, "ip=[redacted] line one") {
		t.Errorf("masked download should hide raw IPs and show the masked form: %q", body)
	}

	if out = get(do(t, h, "POST", "/api/logs", `{"clear":true}`)); len(out.Lines) != 0 {
		t.Errorf("clear echo lines = %v, want empty", out.Lines)
	}
	if got := s.Logs.Entries(); len(got) != 0 {
		t.Errorf("ring after clear = %v, want empty", got)
	}

	if w := do(t, h, "DELETE", "/api/logs", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE logs: %d, want 405", w.Code)
	}
	if w := do(t, h, "POST", "/api/logs", `{`); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d, want 400", w.Code)
	}
}

// The update-check endpoint: GET reports status, POST {enabled} flips the
// setting. The checker is the real update.New but its Loop never starts and
// CheckNow only queues a kick, so nothing can touch the network here.
func TestHandleUpdate(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// Without a checker (tests/headless) the endpoint degrades to the toggle.
	w := do(t, h, "GET", "/api/update", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET update (nil checker): %d", w.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if _, ok := out["enabled"]; !ok {
		t.Errorf("nil-checker response missing enabled: %v", out)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s.Update = update.New("1.0.0", s.settings.UpdateCheckEnabled, log)

	post := func(body string) map[string]any {
		t.Helper()
		w := do(t, h, "POST", "/api/update", body)
		if w.Code != http.StatusOK {
			t.Fatalf("POST update: %d %s", w.Code, w.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out = post(`{"enabled":true}`)
	if !s.settings.UpdateCheckEnabled() {
		t.Error("UpdateCheckEnabled = false after enabling")
	}
	if out["enabled"] != true || out["current"] != "1.0.0" || out["available"] != false {
		t.Errorf("status after enable = %v, want enabled/current 1.0.0/no update", out)
	}
	if out = post(`{"enabled":false}`); s.settings.UpdateCheckEnabled() {
		t.Error("UpdateCheckEnabled = true after disabling")
	}
	if out["enabled"] != false {
		t.Errorf("status after disable = %v, want enabled false", out)
	}

	if w := do(t, h, "DELETE", "/api/update", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE update: %d, want 405", w.Code)
	}
	if w := do(t, h, "POST", "/api/update", `{`); w.Code != http.StatusBadRequest {
		t.Errorf("bad JSON: %d, want 400", w.Code)
	}
}

// Rows older than the live retention window import fine but will be pruned
// within the hour; the response must warn so the vanishing doesn't read as a
// broken restore.
func TestImportWarnsWhenRowsPredateRetention(t *testing.T) {
	s := newTestServer(t)
	// First set a 30-day latency retention via a config import.
	cfg := `{"pingularity_export":1,"config":[{"key":"retention_s","value":"2592000"}]}`
	if w := do(t, s.Handler(), "POST", "/api/import?config=1", cfg); w.Code != http.StatusOK {
		t.Fatalf("config import: got %d: %s", w.Code, w.Body)
	}
	// Then restore samples stamped far in the past.
	old := `{"pingularity_export":1,"latency":[{"ts":1000,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]}`
	w := do(t, s.Handler(), "POST", "/api/import?latency=1", old)
	if w.Code != http.StatusOK {
		t.Fatalf("latency import: got %d: %s", w.Code, w.Body)
	}
	if b := w.Body.String(); !strings.Contains(b, "retention") {
		t.Errorf("want a retention warning, got %s", b)
	}
	// Fresh rows (now) must NOT warn.
	fresh := `{"pingularity_export":1,"latency":[{"ts":` +
		strconv.FormatInt(time.Now().Unix(), 10) +
		`,"target":"cf","latency_ms":10,"success":1,"family":"ipv4"}]}`
	if w := do(t, s.Handler(), "POST", "/api/import?latency=1", fresh); strings.Contains(w.Body.String(), "retention") {
		t.Errorf("fresh rows must not trigger the retention warning: %s", w.Body)
	}
}

// B1: a deeply-nested unknown key in an import body must be rejected, not crash
// the daemon. skipJSONValue recurses once per nesting level; without a depth cap
// a deep-enough body overflows the goroutine stack, a runtime FATAL that
// recover() cannot catch, killing the whole daemon mid-import. The cap makes it a
// clean 400. Depth here (2000) is far past maxImportDepth(64) but small enough to
// stay a fast unit test; the real crash needed tens of millions of levels only
// because the old code had no cap at all.
func TestImportRejectsDeepNesting(t *testing.T) {
	s := newTestServer(t)
	const depth = 2000
	body := `{"pingularity_export":1,"junk":` +
		strings.Repeat("[", depth) + strings.Repeat("]", depth) + `}`
	w := do(t, s.Handler(), "POST", "/api/import?latency=1", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("deep-nested import: got %d, want 400", w.Code)
	}
	// The handler is still serving - the request was rejected, not the process.
	if w2 := do(t, s.Handler(), "GET", "/api/access", ""); w2.Code != http.StatusOK {
		t.Fatalf("handler dead after deep import: /api/access = %d", w2.Code)
	}
	// A shallow unknown key is still skipped fine (the cap did not break the
	// normal skip path).
	shallow := `{"pingularity_export":1,"junk":[[[1,2,3]]]}`
	if w3 := do(t, s.Handler(), "POST", "/api/import?latency=1", shallow); w3.Code != http.StatusOK {
		t.Fatalf("shallow unknown key: got %d, want 200", w3.Code)
	}
}

// A stored iperf3 password is write-only: GET /api/settings must never echo the
// plaintext (nor even the "password" key), sending only has_password so the UI
// can show a dot without the secret leaving the host.
func TestSettingsGetNeverEchoesIperfPassword(t *testing.T) {
	h := newTestServer(t).Handler()
	const secret = "s3cr3t-iperf-pw"
	body := `{"iperf_servers":[{"addr":"iperf.example.com:5201","label":"lab","auth":true,"username":"u","password":"` + secret + `"}]}`
	if w := do(t, h, "POST", "/api/settings", body); w.Code != http.StatusOK {
		t.Fatalf("POST settings %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "GET", "/api/settings", "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET settings %d", w.Code)
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatalf("GET /api/settings leaked the iperf password plaintext:\n%s", w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	servers, ok := got["iperf_servers"].([]any)
	if !ok || len(servers) != 1 {
		t.Fatalf("iperf_servers did not round-trip: %v", got["iperf_servers"])
	}
	srv := servers[0].(map[string]any)
	if _, present := srv["password"]; present {
		t.Fatalf("GET echoed the password key: %v", srv)
	}
	if srv["has_password"] != true {
		t.Fatalf("has_password should be true, got %v", srv["has_password"])
	}
}

// iperfServersToDTO must strip the password and set has_password, so the secret
// can't leak via the DTO even if a future caller forgets the endpoint guard.
func TestIperfServersToDTOWithholdsPassword(t *testing.T) {
	out := iperfServersToDTO([]settings.IperfTarget{
		{Addr: "h:5201", Password: "topsecret", Auth: true},
		{Addr: "n:5201"}, // no password
	})
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Password != "" {
		t.Fatalf("DTO carried the password: %q", out[0].Password)
	}
	if !out[0].HasPassword {
		t.Fatal("has_password should be true when a password is stored")
	}
	if out[1].HasPassword {
		t.Fatal("has_password should be false when no password is stored")
	}
}

// The log-clear-clears-disk contract: the /api/logs clear branch must run
// OnLogClear (main wires it to drop the on-disk logs.txt snapshot) after
// emptying the in-memory ring, and a nil OnLogClear must be a harmless no-op.
func TestLogsClearRunsOnLogClear(t *testing.T) {
	s := newTestServer(t)
	s.Logs = logbuf.New(50)
	s.Logs.Append("secret public-ip line", "secret public-ip line")
	called := 0
	s.OnLogClear = func() { called++ }

	if w := do(t, s.Handler(), "POST", "/api/logs", `{"clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("clear logs %d: %s", w.Code, w.Body)
	}
	if called != 1 {
		t.Fatalf("OnLogClear called %d times, want 1", called)
	}

	// Nil OnLogClear must not panic.
	s.OnLogClear = nil
	if w := do(t, s.Handler(), "POST", "/api/logs", `{"clear":true}`); w.Code != http.StatusOK {
		t.Fatalf("clear logs with nil OnLogClear %d: %s", w.Code, w.Body)
	}
}

// The Test button must exercise the payload shape real alerts will use: the
// selected Webhook format override rides along and is applied to the test
// notifier. A self-hosted ntfy (undetectable from the hostname) gets a native
// ntfy test, and a forced-generic config gets JSON even on an ntfy-ish host.
func TestNotifyTestHonorsFormat(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	type hit struct {
		title string
		body  string
		json  bool
	}
	var got hit
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = hit{title: r.Header.Get("X-Title"), body: string(b), json: json.Valid(b) && strings.HasPrefix(strings.TrimSpace(string(b)), "{")}
		w.WriteHeader(http.StatusOK)
	}))
	defer recv.Close()

	// ntfy override on a plain host: native delivery (title header, plain body).
	if w := do(t, h, "POST", "/api/notify/test", `{"url":"`+recv.URL+`","format":"ntfy"}`); w.Code != http.StatusOK {
		t.Fatalf("ntfy-format test: %d %s", w.Code, w.Body)
	}
	if got.title == "" || got.json {
		t.Fatalf("ntfy override not applied: title=%q json=%v body=%q", got.title, got.json, got.body)
	}
	// generic override: JSON payload, no ntfy headers - regardless of host.
	if w := do(t, h, "POST", "/api/notify/test", `{"url":"`+recv.URL+`","format":"generic"}`); w.Code != http.StatusOK {
		t.Fatalf("generic-format test: %d %s", w.Code, w.Body)
	}
	if got.title != "" || !got.json {
		t.Fatalf("generic override not applied: title=%q json=%v body=%q", got.title, got.json, got.body)
	}
	// Unknown/empty format falls back to hostname detection - plain host gets generic.
	if w := do(t, h, "POST", "/api/notify/test", `{"url":"`+recv.URL+`","format":"bogus"}`); w.Code != http.StatusOK {
		t.Fatalf("bogus-format test: %d %s", w.Code, w.Body)
	}
	if !got.json {
		t.Fatalf("bogus format should fall back to detection (generic here): body=%q", got.body)
	}
}
