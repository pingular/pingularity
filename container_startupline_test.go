package main

import (
	"strings"
	"testing"
)

// The startup line is the ONE thing a default-level ("off") install prints on a
// healthy boot - without it `docker logs` shows an empty stream that reads as a
// hung process. Pin its contract: exactly one line, carrying the version, the
// listen address, the access mode (truthfully, both ways), and the dashboard
// URL - operational shape only, never a secret.
func TestStartupLine(t *testing.T) {
	line := startupLine("v0.62.0", ":9000", true)
	if strings.Contains(line, "\n") {
		t.Fatalf("startup line is not a single line: %q", line)
	}
	for _, want := range []string{
		"v0.62.0",               // version
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
