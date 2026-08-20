package speedtest

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// The upload recorder is the evidence chain for three behaviours: the
// starvation rescue (fires only when the recorder saw attempts), the failure
// diagnosis (its summary names rejection vs starvation), and the
// refused-server exclusion (refusedByServer reads it). It identified upload
// traffic by the literal suffix "/speedtest/upload.php" - which silently
// missed every endpoint whose directory is not named speedtest (the Ookla
// mini shape, /mini/upload.php) and the trailing-slash path the
// relative-redirect fix now adopts (/speedtest/upload.php/). On such servers
// the recorder counted nothing: the rescue could never fire, the diagnosis
// claimed "no upload requests were issued" about a run that made thousands,
// the rejected traffic vanished from the data-used figure, and a refusing
// server was never excluded.

// countingRT answers every request with the given status, so the matcher can
// be probed one request at a time.
type countingRT struct{ status int }

func (c countingRT) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

// TestRecorderSeesEveryUploadShape pins the matcher itself.
func TestRecorderSeesEveryUploadShape(t *testing.T) {
	cases := []struct {
		method, url string
		counted     bool
	}{
		{"POST", "http://s/speedtest/upload.php", true},
		{"POST", "http://s/speedtest/upload.php/", true}, // the adopted relative-redirect shape
		{"POST", "http://s/mini/upload.php", true},       // Ookla mini: directory not named speedtest
		{"POST", "http://s/upload.php", true},            // endpoint at the root
		{"POST", "http://s/backend/xfer.php", true},      // an adopted rewrite target: any POST is an upload on this client
		{"GET", "http://s/speedtest/upload.php", false},  // not an upload attempt
		{"GET", "http://s/speedtest/latency.txt", false}, // ping probe
		{"GET", "http://s/download?size=1000", false},    // download rides GETs
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.url, func(t *testing.T) {
			rec := &uploadRecorder{}
			rt := recordingTransport{base: countingRT{status: 403}, rec: rec}
			req, err := http.NewRequest(c.method, c.url, strings.NewReader("x"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rt.RoundTrip(req); err != nil {
				t.Fatal(err)
			}
			attempts, _ := rec.snapshot()
			if got := attempts == 1; got != c.counted {
				if c.counted {
					t.Fatalf("an upload POST to %s was invisible to the recorder: the rescue cannot fire, the diagnosis lies, and a refusing server is never excluded", c.url)
				}
				t.Fatalf("%s %s was counted as an upload attempt - only POSTs are uploads on this client", c.method, c.url)
			}
		})
	}
}

// TestUploadPartial_NonStandardPathKeepsEvidence: the full chain on an
// endpoint whose directory is not named speedtest - the refused run must
// still bill its spend and still feed the server exclusion.
func TestUploadPartial_NonStandardPathKeepsEvidence(t *testing.T) {
	s := &naServer{mode: "403", path: "/mini/upload.php"}
	res, err, _ := runPartialCase(t, s)
	assertPartial(t, res, err)
	fbMu.Lock()
	v, ok := fbMap["partial-403/mini/upload.php"]
	fbMu.Unlock()
	if !ok || v.state != endpointRetired {
		t.Fatal("a server refusing every upload at a non-standard path was not excluded - the recorder never saw the refusals")
	}
}

// TestUploadStarvationRescueNonStandardPath: the rescue's trigger is the
// recorder's starvation signature; on a blind path it could never fire and a
// rescuable slow uplink stayed unmeasured forever.
func TestUploadStarvationRescueNonStandardPath(t *testing.T) {
	s := &naServer{rateBPS: 400000, capture: ooklaCaptureTime, retries: speedDefaultRetries, path: "/mini/upload.php"}
	res, err := runNACase(t, s)
	if err != nil {
		t.Fatalf("rescue failed, still N/A: %v", err)
	}
	if res.UploadMbps <= 0 {
		t.Fatalf("no rescued throughput on a non-standard path (got %.2f Mbps): the starvation signature was invisible to the recorder, so the single-stream rescue never fired", res.UploadMbps)
	}
}
