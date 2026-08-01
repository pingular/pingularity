package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/store"
)

// gzGet runs one request through the full chain with the given Accept-Encoding
// (omitted when ae is ""), returning the recorder.
func gzGet(t *testing.T, h http.Handler, path, ae string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.Host = "127.0.0.1:9000" // pass the DNS-rebinding guard
	if ae != "" {
		r.Header.Set("Accept-Encoding", ae)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// gunzip decodes a gzip body, failing the test if it is not valid gzip.
func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return out
}

// seedSpeed inserts n speedtest runs with every optional field populated, so a
// response is both large enough to compress and complete enough to project.
func seedSpeed(t *testing.T, s *Server, n int) {
	t.Helper()
	ctx := context.Background()
	f := func(v float64) *float64 { return &v }
	i64 := func(v int64) *int64 { return &v }
	yes := true
	base := time.Now().Add(-time.Duration(n) * time.Minute).Unix()
	for i := 0; i < n; i++ {
		if err := s.store.InsertSpeed(ctx, store.SpeedSample{
			TS: base + int64(i)*60, DownMbps: 100 + float64(i), UpMbps: 20 + float64(i),
			PingMS: 12.5, JitterMS: f(1.5), Server: "Example Telecom (Springfield)", ServerID: "1234",
			PublicIPv4: "203.0.113.7", PublicIPv6: "2001:db8::1",
			ISP: "Example ISP", ISPLocation: "Springfield, US",
			DNSIP: "203.0.113.53", DNSProvider: "Example DNS", DNSLocation: "Springfield, US",
			PacketLoss: f(0.5), Healthy: &yes,
			DownBytes: i64(123456789), UpBytes: i64(23456789),
			CFColo: "SJC", ExitSummary: "203.0.113.1 → 198.51.100.1", Trigger: "scheduled",
			Engine: "ookla", IdleMS: f(11), LoadedDownMS: f(45), LoadedUpMS: f(38),
			LoadedDownP95MS: f(90), LoadedUpP95MS: f(77),
		}); err != nil {
			t.Fatalf("InsertSpeed: %v", err)
		}
	}
}

// A client that asks for gzip gets it on a body over the threshold, and the
// decoded bytes are exactly what the same request without Accept-Encoding
// returns. Compression must be a transport detail, never a content change.
func TestGzipCompressesLargeJSONAndRoundTrips(t *testing.T) {
	s := newTestServer(t)
	seedSpeed(t, s, 200)
	h := s.Handler()

	plain := gzGet(t, h, "/api/speed?mins=1440", "")
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("no Accept-Encoding: Content-Encoding = %q, want empty", got)
	}
	if plain.Body.Len() < compressMinBytes {
		t.Fatalf("seeded body is %d bytes, too small to exercise the threshold", plain.Body.Len())
	}

	gz := gzGet(t, h, "/api/speed?mins=1440", "gzip")
	if got := gz.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := gz.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary = %q, want it to list Accept-Encoding", got)
	}
	if got := gz.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (not sniffed from gzip bytes)", got)
	}
	if gz.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzip body %d bytes is not smaller than plain %d", gz.Body.Len(), plain.Body.Len())
	}
	if got := string(gunzip(t, gz.Body.Bytes())); got != plain.Body.String() {
		t.Errorf("decoded gzip body differs from the uncompressed response")
	}
	// Content-Length must describe the ENCODED body, not the plaintext.
	if cl := gz.Header().Get("Content-Length"); cl != strconv.Itoa(gz.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d (the compressed size)", cl, gz.Body.Len())
	}
}

// A client that does not offer gzip - or offers it at q=0 - gets plaintext.
func TestGzipNotAppliedWithoutNegotiation(t *testing.T) {
	s := newTestServer(t)
	seedSpeed(t, s, 200)
	h := s.Handler()

	for _, ae := range []string{"", "identity", "gzip;q=0", "br", "*;q=0"} {
		w := gzGet(t, h, "/api/speed?mins=1440", ae)
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want empty", ae, got)
		}
		if !json.Valid(w.Body.Bytes()) {
			t.Errorf("Accept-Encoding %q: body is not plain JSON", ae)
		}
	}
}

// Below the threshold the response is sent uncompressed even though the client
// asked for gzip: the framing would cost more than it saves. /api/heatmap on an
// empty store is the tiny-response case this guards.
func TestGzipSkippedUnderThreshold(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	w := gzGet(t, h, "/api/heatmap?days=1", "gzip")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if w.Body.Len() >= compressMinBytes {
		t.Fatalf("heatmap body is %d bytes, not under the %d threshold", w.Body.Len(), compressMinBytes)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty for a %d-byte body", got, w.Body.Len())
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("small body is not valid JSON: %q", w.Body.String())
	}
}

// The threshold is a size test, not a path test: the same endpoint compresses
// once its body grows past the boundary.
func TestGzipThresholdIsAboutSizeNotPath(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	small := gzGet(t, h, "/api/speed?mins=1440", "gzip")
	if got := small.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("empty /api/speed: Content-Encoding = %q, want empty", got)
	}
	seedSpeed(t, s, 200)
	big := gzGet(t, h, "/api/speed?mins=1440", "gzip")
	if got := big.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("populated /api/speed: Content-Encoding = %q, want gzip", got)
	}
}

// The streaming downloads keep streaming: they are excluded from compression so
// their progress-refreshed write deadline and O(1) memory profile are untouched.
func TestGzipExcludesStreamingEndpoints(t *testing.T) {
	s := newTestServer(t)
	seedSpeed(t, s, 200)
	h := s.Handler()

	for _, p := range []string{"/api/speed/runs.csv", "/api/export?speed=1"} {
		w := gzGet(t, h, p, "gzip")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body=%q", p, w.Code, w.Body.String())
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%s: Content-Encoding = %q, want empty (streaming endpoints are excluded)", p, got)
		}
		if w.Body.Len() < compressMinBytes {
			t.Errorf("%s: body is %d bytes; too small to prove the exclusion matters", p, w.Body.Len())
		}
	}
}

// metricNames lists the distinct family names in an exposition body. Values,
// timestamps and goroutine counts move between scrapes, so structure is what two
// separate scrapes can be compared on.
func metricNames(body string) []string {
	set := map[string]bool{}
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, _ := strings.Cut(line, " ")
		name, _, _ = strings.Cut(name, "{")
		set[name] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// /metrics is compressed (Prometheus negotiates gzip) and decodes to the same
// exposition format the plaintext scrape produces.
func TestGzipMetricsRoundTrips(t *testing.T) {
	s := newTestServer(t)
	// handleMetrics degrades to 503 without a status source.
	s.status = func() LiveStatus {
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	h := s.Handler()

	plain := gzGet(t, h, "/metrics", "")
	if plain.Code != http.StatusOK {
		t.Fatalf("plain /metrics: status = %d", plain.Code)
	}
	if plain.Body.Len() < compressMinBytes {
		t.Fatalf("/metrics is %d bytes, under the threshold; this test would prove nothing", plain.Body.Len())
	}
	gz := gzGet(t, h, "/metrics", "gzip")
	if got := gz.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("/metrics Content-Encoding = %q, want gzip", got)
	}
	if gz.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzip /metrics is %d bytes, not smaller than plain %d", gz.Body.Len(), plain.Body.Len())
	}
	decoded := string(gunzip(t, gz.Body.Bytes()))
	if got, want := metricNames(decoded), metricNames(plain.Body.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("decoded /metrics families\n got: %v\nwant: %v", got, want)
	}
	if !strings.HasPrefix(decoded, "# HELP") {
		t.Errorf("decoded /metrics does not start with an exposition header: %.60q", decoded)
	}
}

// The format tests scrape without Accept-Encoding (see scrape()), so they must
// still receive the raw exposition body. This pins that: a scrape that does not
// negotiate gzip is never encoded, whatever its size.
func TestGzipMetricsPlainScrapeStaysUncompressed(t *testing.T) {
	s := newTestServer(t)
	s.status = func() LiveStatus {
		return LiveStatus{Online: true, Since: time.Unix(1_700_000_000, 0)}
	}
	body := scrape(t, s) // the exact helper the metrics format tests use
	if got := gzGet(t, s.Handler(), "/metrics", "").Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
	if !strings.HasPrefix(body, "# HELP") {
		t.Errorf("scrape() body is not raw exposition format: %.60q", body)
	}
	if len(metricNames(body)) == 0 {
		t.Error("scrape() returned no metric families")
	}
}

// The dashboard asset is served precompressed by serveUI, which sets
// Content-Encoding itself. The middleware must pass that through rather than
// gzip it a second time.
func TestGzipDoesNotDoubleCompressTheUIAsset(t *testing.T) {
	s := newTestServer(t)
	w := gzGet(t, s.Handler(), "/", "gzip")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	body := gunzip(t, w.Body.Bytes())
	if !bytes.Contains(body, []byte("<html")) {
		t.Fatalf("one gunzip did not yield HTML; the asset was compressed twice")
	}
	// A second layer would leave gzip magic at the start of the decoded body.
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		t.Errorf("decoded body is still gzip - double compressed")
	}
	if cl := w.Header().Get("Content-Length"); cl != strconv.Itoa(w.Body.Len()) {
		t.Errorf("Content-Length = %q, want %d", cl, w.Body.Len())
	}
}

// A 304 carries no body, so it must not be given a Content-Encoding.
func TestGzipLeaves304Alone(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	first := gzGet(t, h, "/", "gzip")
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on the UI response")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Accept-Encoding", "gzip")
	r.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("304 Content-Encoding = %q, want empty", got)
	}
	if w.Body.Len() != 0 {
		t.Errorf("304 body = %d bytes, want 0", w.Body.Len())
	}
}

// serveUI's 304 has no body, so the size threshold alone would already keep it
// uncompressed. This pins the compressible(code) guard directly: a handler that
// sets a bodyless status and then writes far MORE than the threshold must still
// not get a Content-Encoding, because 204/304 may not carry a body at all.
func TestGzipNeverEncodesABodylessStatus(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			w.Write(bytes.Repeat([]byte("x"), compressMinBytes*4))
		}))
		r := httptest.NewRequest("GET", "/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		if w.Code != code {
			t.Errorf("status = %d, want %d", w.Code, code)
		}
		if got := w.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%d: Content-Encoding = %q, want empty", code, got)
		}
	}
}

// A handler that writes without setting Content-Type relies on net/http sniffing
// the first bytes it is handed. Those bytes must be the PLAINTEXT: handed deflate
// output the sniffer returns application/x-gzip and the client mis-renders the
// response. Pins the DetectContentType call in decide().
func TestGzipPinsContentTypeFromPlaintext(t *testing.T) {
	body := append([]byte("<!doctype html><title>hi</title>"), bytes.Repeat([]byte("<p>filler</p>"), 200)...)
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body) // deliberately no Content-Type
	}))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	ct := w.Header().Get("Content-Type")
	if strings.Contains(ct, "gzip") || strings.Contains(ct, "octet-stream") {
		t.Errorf("Content-Type = %q - sniffed from the compressed bytes", ct)
	}
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want it sniffed from the plaintext as text/html", ct)
	}
	if got := gunzip(t, w.Body.Bytes()); !bytes.Equal(got, body) {
		t.Errorf("body did not round-trip")
	}
}

// A HEAD is answered with headers only and no encoding applied - there is no
// body to compress.
func TestGzipSkipsHEAD(t *testing.T) {
	s := newTestServer(t)
	seedSpeed(t, s, 200)
	r := httptest.NewRequest("HEAD", "/api/speed?mins=1440", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("HEAD Content-Encoding = %q, want empty", got)
	}
}

// A small error response stays readable plaintext: it is under the threshold, so
// it is never encoded, and its status survives the wrapper.
func TestGzipLeavesErrorResponsesReadable(t *testing.T) {
	s := newTestServer(t)
	w := gzGet(t, s.Handler(), "/api/speed/runs?locate=notanumber", "gzip")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Content-Encoding = %q, want empty", got)
	}
	if !strings.Contains(w.Body.String(), "bad locate timestamp") {
		t.Errorf("body = %q, want the plaintext error", w.Body.String())
	}
}

// The wrapper must stay transparent to http.NewResponseController: writeDeadline
// sets a deadline through it on every non-self-paced route, and exportDeadlineBumper
// rearms one. Both find the real writer only if Unwrap is honoured all the way
// down. A ResponseWriter that records SetWriteDeadline proves the call arrives.
type deadlineRecorder struct {
	http.ResponseWriter
	sets int
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error { d.sets++; return nil }
func (d *deadlineRecorder) Flush()                           {}

func TestGzipWriterPreservesResponseController(t *testing.T) {
	inner := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	gw := &gzipWriter{ResponseWriter: inner, status: http.StatusOK}
	if err := http.NewResponseController(gw).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetWriteDeadline through gzipWriter: %v", err)
	}
	if inner.sets != 1 {
		t.Fatalf("SetWriteDeadline reached the inner writer %d times, want 1", inner.sets)
	}
}

// End to end: the deadline the writeDeadline middleware installs still reaches a
// real writer for a route that is now compressed.
func TestGzipKeepsWriteDeadlineReachable(t *testing.T) {
	s := newTestServer(t)
	seedSpeed(t, s, 200)
	inner := &deadlineRecorder{ResponseWriter: httptest.NewRecorder()}
	r := httptest.NewRequest("GET", "/api/speed?mins=1440", nil)
	r.Host = "127.0.0.1:9000"
	r.Header.Set("Accept-Encoding", "gzip")
	s.Handler().ServeHTTP(inner, r)
	if inner.sets == 0 {
		t.Fatal("writeDeadline never reached the real writer through the gzip wrapper")
	}
}

// A handler that flushes gets its bytes delivered instead of held for a
// Content-Length, and the stream still decodes.
func TestGzipFlushStreamsThrough(t *testing.T) {
	big := bytes.Repeat([]byte("pingularity flush payload "), 200) // well over the threshold
	h := compressResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write(big)
		w.(http.Flusher).Flush()
		w.Write(big)
	}))
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if cl := w.Header().Get("Content-Length"); cl != "" {
		t.Errorf("Content-Length = %q, want none after a flush", cl)
	}
	if got := gunzip(t, w.Body.Bytes()); !bytes.Equal(got, append(append([]byte{}, big...), big...)) {
		t.Errorf("flushed stream did not round-trip (%d bytes decoded)", len(got))
	}
}
