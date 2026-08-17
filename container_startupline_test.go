package main

import (
	"os"
	"strings"
	"testing"
)

// The startup line is the ONE thing a default-level ("off") install prints on a
// healthy boot - without it `docker logs` shows an empty stream that reads as a
// hung process. Pin its contract: exactly one line, carrying the version, the
// listen address, the access mode (truthfully, both ways), and the dashboard
// URL - operational shape only, never a secret.
//
// The version here is a deliberately impossible tag. startupLine only
// interpolates whatever string it is handed, so any real-looking number would
// be a fixture that reads like a claim about a release - and the README example
// pinned below is where a real version legitimately appears.
func TestStartupLine(t *testing.T) {
	line := startupLine("v9.9.9", ":9000", true)
	if strings.Contains(line, "\n") {
		t.Fatalf("startup line is not a single line: %q", line)
	}
	for _, want := range []string{
		"v9.9.9",                // version
		":9000",                 // listen address
		"local-only",            // access mode
		"http://localhost:9000", // where the dashboard is (dashboardURL)
	} {
		if !strings.Contains(line, want) {
			t.Errorf("startup line missing %q: %q", want, line)
		}
	}

	// The access mode must state the other direction truthfully too.
	open := startupLine("dev", "127.0.0.1:9000", false)
	if !strings.Contains(open, "network") || strings.Contains(open, "local-only") {
		t.Errorf("network-access startup line = %q, want access \"network\"", open)
	}
}

// The README quotes a sample startup line in "What `docker logs` shows", and
// operators match it against their own logs to decide whether a container came
// up. A sample that drifts from the real format teaches them to look for a line
// the daemon never prints - and because the sample has to name SOME version, it
// drifts every time the release stream moves.
//
// So pin the FORMAT and leave the VERSION free: whatever version the example
// quotes is parsed back out of it and fed to startupLine, and the result must
// be the example, byte for byte. The doc may name whichever release it likes;
// it can never again invent a shape the binary does not produce. The other two
// arguments are fixed on purpose - the paragraph is the default-container
// walkthrough, so the sample must show the default bind (config.Default's
// ListenAddr) and the fail-closed default access mode (defaultSettings sets
// AccessLocalOnly unless -access network was passed).
func TestREADMEStartupLineExampleIsWhatTheDaemonPrints(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	example := readmeCodeSpanContaining(t, string(readme), "listening on")

	rest, ok := strings.CutPrefix(example, "pingularity ")
	if !ok {
		t.Fatalf("README startup-line example does not begin with the daemon's own prefix: %q", example)
	}
	// The colon that ends the version is part of the format, so a sample that
	// lost it would otherwise "parse" the rest of the sentence as a version and
	// report the mismatch as a baffling doubled line. Say what really broke.
	version, _, ok := strings.Cut(rest, ":")
	if !ok || strings.ContainsAny(version, " `") {
		t.Fatalf("no version could be read out of the README startup-line example - it must read `pingularity <version>: ...`: %q", example)
	}

	if want := startupLine(version, ":9000", true); example != want {
		t.Errorf("the README's sample startup line is not what the daemon prints:\n README: %s\n binary: %s\n(the version is free - the format, the default :9000 bind and the local-only default are not)", example, want)
	}
}

// readmeCodeSpanContaining returns the single backticked code span of doc that
// holds marker, with the source line wrap Markdown allows inside a span folded
// back into the one space it stands for - the printed line has no newlines.
func readmeCodeSpanContaining(t *testing.T, doc, marker string) string {
	t.Helper()
	at := strings.Index(doc, marker)
	if at < 0 {
		t.Fatalf("README no longer contains %q - the docker-logs paragraph was reworded; recheck this test's premise", marker)
	}
	if strings.Contains(doc[at+len(marker):], marker) {
		t.Fatalf("README mentions %q more than once, so this test can no longer tell which span is the startup-line sample; give the sample a marker of its own", marker)
	}
	openAt := strings.LastIndex(doc[:at], "`")
	closeAt := strings.Index(doc[at:], "`")
	if openAt < 0 || closeAt < 0 {
		t.Fatalf("the README text around %q is not inside a backticked code span", marker)
	}
	return strings.Join(strings.Fields(doc[openAt+1:at+closeAt]), " ")
}
