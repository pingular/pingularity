package speedtest

import (
	"context"
	"errors"
	"testing"
	"time"

	ookla "github.com/showwin/speedtest-go/speedtest"
)

// errFeedbackFake stands in for a transport-level failure in recorder tests.
var errFeedbackFake = errors.New("connection reset by peer")

// Selection judges a server's legacy bundle by GETting latency.txt
// (fallbackHealth), so a host whose static files serve while its upload.php
// refuses every POST ranks endpointOK forever: it wins every pass on
// proximity, fails every run, and nothing feeds the failure back. These tests
// pin the feedback loop that closes that hole - a finished run whose every
// upload POST was answered and none accepted is the honest probe the GET
// cannot be - and pin the guards that keep it from firing on the failures
// that say nothing about the server (a starved uplink, a redirect, a rate
// limit, a mixed run that did confirm bytes).

// rejectionFeedbackCase runs the real measure() against an naServer and
// returns the fbMap verdict recorded for the server, if any.
func rejectionFeedbackCase(t *testing.T, mode string) (verdict fallbackVerdict, found bool) {
	t.Helper()
	s := &naServer{mode: mode, retries: 1}
	id := "reject-feedback-" + mode
	// A "both" run keeps its download as a partial success when only the
	// upload fails (see uploadpartial_test.go) - a healthy partial, not an
	// error, is what proves the run reached and failed the upload phase.
	res, runErr := runNACaseOpts(t, s, naRunOpts{id: id})
	if runErr != nil {
		t.Fatalf("the run must be kept as a partial, got error: %v", runErr)
	}
	if res.DownloadMbps <= 0 || res.UploadMbps != 0 {
		t.Fatalf("expected a kept download and an unmeasured upload, got %+v", res)
	}

	fbMu.Lock()
	verdict, found = fbMap[id]
	fbMu.Unlock()
	return verdict, found
}

// TestUploadRejectionExcludesServer: a run whose every POST the server refused
// (403 - WAF; 500 - the fleet probe's measured never-installed-bundle class)
// must land a retired verdict in the same cache selection reads, so the next
// pass stops picking the server. Before the feedback existed, the evidence
// died in the error string and the server won every subsequent selection.
func TestUploadRejectionExcludesServer(t *testing.T) {
	for _, mode := range []string{"403", "500"} {
		t.Run(mode, func(t *testing.T) {
			v, ok := rejectionFeedbackCase(t, mode)
			if !ok {
				t.Fatal("a fully-refused run left no verdict in fbMap - the server will win the next selection pass and fail forever")
			}
			if v.state != endpointRetired {
				t.Fatalf("verdict = %v, want retired - anything else keeps the server in the pool", v.state)
			}
			if until := time.Until(v.expires); until < fallbackTTL-time.Minute || until > fallbackTTL {
				t.Fatalf("verdict expires in %s, want ~fallbackTTL (%s): shorter re-admits the broken server early, longer outlives the probe contract", until, fallbackTTL)
			}
		})
	}
}

// TestUploadRedirectDoesNotExcludeServer: a 3xx is a move, not a refusal -
// currentEndpoint (list servers) and probeEndpoint (pins) own that case, and
// the redirect target may be perfectly usable. Condemning on it would retire
// migrated-but-healthy hosts, the exact class issues #17/#18 recovered.
func TestUploadRedirectDoesNotExcludeServer(t *testing.T) {
	if _, ok := rejectionFeedbackCase(t, "307"); ok {
		t.Fatal("a redirecting upload endpoint must not be condemned - a move is not a refusal")
	}
}

// TestSelectionReadsTheRejectionVerdict: the verdict the run writes is honored
// by fallbackHealth WITHOUT a network probe (the GET would say OK and undo
// it), and rankedServers drops the server from the pool on it.
func TestSelectionReadsTheRejectionVerdict(t *testing.T) {
	s := &ookla.Server{ID: "reject-feedback-selection", URL: "http://198.51.100.7:8080/speedtest/upload.php"}
	fbMu.Lock()
	fbMap[s.ID] = fallbackVerdict{state: endpointRetired, expires: time.Now().Add(fallbackTTL), fails: fallbackStrikes}
	fbMu.Unlock()
	t.Cleanup(func() {
		fbMu.Lock()
		delete(fbMap, s.ID)
		fbMu.Unlock()
	})

	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState {
		t.Fatal("fallbackHealth must serve the run's verdict from cache - probing here would let the GET (which succeeds on these servers) overwrite it")
		return endpointOK
	}
	t.Cleanup(func() { probeFallback = oldProbe })

	if got := fallbackHealth(context.Background(), s); got != endpointRetired {
		t.Fatalf("fallbackHealth = %v, want retired from the cached run verdict", got)
	}
}

// TestRejectionVerdictExpiresAndGetReadmits: the exclusion is self-healing. On
// expiry the ordinary GET probe judges again, so a server condemned by a
// transient full-run failure costs one fallbackTTL, not forever.
func TestRejectionVerdictExpiresAndGetReadmits(t *testing.T) {
	s := &ookla.Server{ID: "reject-feedback-expiry", URL: "http://198.51.100.8:8080/speedtest/upload.php"}
	fbMu.Lock()
	fbMap[s.ID] = fallbackVerdict{state: endpointRetired, expires: time.Now().Add(-time.Second), fails: fallbackStrikes}
	fbMu.Unlock()
	t.Cleanup(func() {
		fbMu.Lock()
		delete(fbMap, s.ID)
		fbMu.Unlock()
	})

	oldProbe := probeFallback
	probeFallback = func(context.Context, *ookla.Server) endpointState { return endpointOK }
	t.Cleanup(func() { probeFallback = oldProbe })

	if got := fallbackHealth(context.Background(), s); got != endpointOK {
		t.Fatalf("fallbackHealth after expiry = %v, want OK - the GET must readmit a recovered server", got)
	}
}

// TestRefusedByServer pins the evidence rule: fire only on responses the
// server itself sent, none of them acceptances, enough of them to rule out an
// aborted-run edge, and never on load symptoms.
func TestRefusedByServer(t *testing.T) {
	load := func(entries map[int]int, transportErrs int) *uploadRecorder {
		r := &uploadRecorder{}
		for status, n := range entries {
			for i := 0; i < n; i++ {
				r.note(status, nil)
			}
		}
		for i := 0; i < transportErrs; i++ {
			r.note(0, errFeedbackFake)
		}
		return r
	}
	cases := []struct {
		name string
		rec  *uploadRecorder
		want bool
	}{
		{"pure 403 rejection", load(map[int]int{403: 500}, 0), true},
		{"pure 500 rejection (never-installed bundle)", load(map[int]int{500: 500}, 0), true},
		{"mixed 4xx/5xx rejection", load(map[int]int{403: 20, 500: 20}, 0), true},
		{"starvation: transport errors only", load(nil, 8), false},
		{"any 2xx clears the server", load(map[int]int{200: 1, 403: 500}, 0), false},
		{"redirects are not refusals", load(map[int]int{307: 500}, 0), false},
		{"rate limiting is a load symptom", load(map[int]int{429: 500}, 0), false},
		{"bad gateway is a load symptom", load(map[int]int{502: 500}, 0), false},
		{"service unavailable is a load symptom", load(map[int]int{503: 500}, 0), false},
		{"gateway timeout is a load symptom", load(map[int]int{504: 500}, 0), false},
		{"a load symptom acquits a mixed run too", load(map[int]int{403: 50, 503: 50}, 0), false},
		{"request timeout is a load symptom", load(map[int]int{408: 500}, 0), false},
		{"too few refusals (aborted-run edge)", load(map[int]int{403: 3}, 0), false},
		{"refusals mixed with transport errors still fire", load(map[int]int{403: 50}, 5), true},
		{"empty recorder", &uploadRecorder{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rec.refusedByServer(); got != c.want {
				t.Fatalf("refusedByServer() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestNoteUploadRejectionRespectsMapCap: the run-feedback writer must hold the
// same fallbackMapCap invariant the probe writer enforces, or rejection
// verdicts grow the process-lifetime map past its documented bound.
func TestNoteUploadRejectionRespectsMapCap(t *testing.T) {
	fbMu.Lock()
	saved := fbMap
	fbMap = map[string]fallbackVerdict{}
	for i := 0; i < fallbackMapCap; i++ {
		fbMap[string(rune('a'+i%26))+string(rune('0'+i/26))+"-cap"] = fallbackVerdict{state: endpointOK, expires: time.Now().Add(time.Hour)}
	}
	fbMu.Unlock()
	t.Cleanup(func() {
		fbMu.Lock()
		fbMap = saved
		fbMu.Unlock()
	})

	noteUploadRejection(&ookla.Server{ID: "cap-overflow", URL: "http://198.51.100.9:8080/speedtest/upload.php"})

	fbMu.Lock()
	n := len(fbMap)
	_, wrote := fbMap["cap-overflow"]
	fbMu.Unlock()
	if !wrote {
		t.Fatal("the rejection verdict was not written at all")
	}
	if n > fallbackMapCap {
		t.Fatalf("fbMap grew to %d, past its documented cap of %d - the run-feedback writer skips the eviction pass", n, fallbackMapCap)
	}
}

// TestProbeCannotClobberAStandingConviction: the web server picker fans out
// concurrent GET probes from HTTP handler goroutines - one registered before a
// run convicts the server can land after it, and its verdict is OK (the
// broken-upload server's latency.txt succeeding is exactly why it ranked OK).
// The landing must not overwrite the conviction: readmission is expiry's job
// alone. A fresh retired verdict (a re-conviction) still refreshes it, and an
// EXPIRED conviction is replaced normally by whatever the probe found.
func TestProbeCannotClobberAStandingConviction(t *testing.T) {
	const id = "clobber-guard"
	cleanup := func() {
		fbMu.Lock()
		delete(fbMap, id)
		fbMu.Unlock()
	}
	cleanup()
	t.Cleanup(cleanup)

	convict := func(expires time.Time) {
		fbMu.Lock()
		fbMap[id] = fallbackVerdict{state: endpointRetired, expires: expires, fails: fallbackStrikes}
		fbMu.Unlock()
	}
	stateOf := func() endpointState {
		fbMu.Lock()
		defer fbMu.Unlock()
		return fbMap[id].state
	}

	// A late-landing OK probe must not shorten a standing conviction.
	convict(time.Now().Add(fallbackTTL))
	fbMu.Lock()
	storeFallbackVerdictLocked(id, fallbackVerdict{state: endpointOK, expires: time.Now().Add(fallbackTTL)})
	fbMu.Unlock()
	if got := stateOf(); got != endpointRetired {
		t.Fatalf("a landing OK probe overwrote a standing conviction (%v) - the server is silently un-excluded and wins the next selection pass", got)
	}
	// Neither must an unknown one.
	fbMu.Lock()
	storeFallbackVerdictLocked(id, fallbackVerdict{state: endpointUnknown, expires: time.Now().Add(fallbackUnknownTTL)})
	fbMu.Unlock()
	if got := stateOf(); got != endpointRetired {
		t.Fatalf("a landing unknown probe overwrote a standing conviction (%v)", got)
	}
	// A fresh conviction refreshes it.
	fresh := time.Now().Add(fallbackTTL + time.Hour)
	fbMu.Lock()
	storeFallbackVerdictLocked(id, fallbackVerdict{state: endpointRetired, expires: fresh, fails: fallbackStrikes})
	fbMu.Unlock()
	fbMu.Lock()
	gotExp := fbMap[id].expires
	fbMu.Unlock()
	if !gotExp.Equal(fresh) {
		t.Fatal("a re-conviction must refresh the standing verdict")
	}
	// And expiry readmits: an expired conviction is replaced normally.
	convict(time.Now().Add(-time.Second))
	fbMu.Lock()
	storeFallbackVerdictLocked(id, fallbackVerdict{state: endpointOK, expires: time.Now().Add(fallbackTTL)})
	fbMu.Unlock()
	if got := stateOf(); got != endpointOK {
		t.Fatalf("an EXPIRED conviction must be replaceable - the TTL is the designed second chance, got %v", got)
	}
}

// TestConvictionBurstSuspectsOwnNetwork: one refusing server is that server's
// problem; several DIFFERENT servers refusing in quick succession indict the
// client's own network (a DPI/proxy filtering upload POSTs), and the operator
// must be told so instead of a log that blames each server in turn.
func TestConvictionBurstSuspectsOwnNetwork(t *testing.T) {
	fbMu.Lock()
	saved := recentConvictions
	recentConvictions = nil
	fbMu.Unlock()
	t.Cleanup(func() {
		fbMu.Lock()
		recentConvictions = saved
		fbMu.Unlock()
	})
	cleanup := func(ids ...string) {
		fbMu.Lock()
		for _, id := range ids {
			delete(fbMap, id)
		}
		fbMu.Unlock()
	}
	t.Cleanup(func() { cleanup("burst-a", "burst-b", "burst-c") })

	if noteUploadRejection(&ookla.Server{ID: "burst-a", URL: "http://198.51.100.10/u"}) {
		t.Fatal("one conviction must not suspect the client's network")
	}
	// The same server re-convicted is still ONE server.
	if noteUploadRejection(&ookla.Server{ID: "burst-a", URL: "http://198.51.100.10/u"}) {
		t.Fatal("re-convicting the same server is not a burst")
	}
	if noteUploadRejection(&ookla.Server{ID: "burst-b", URL: "http://198.51.100.11/u"}) {
		t.Fatal("two distinct servers are below the burst bar")
	}
	if !noteUploadRejection(&ookla.Server{ID: "burst-c", URL: "http://198.51.100.12/u"}) {
		t.Fatal("three DIFFERENT servers refusing uploads inside the window is the client-network signature and must be named")
	}
}
