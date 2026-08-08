package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// A container operator who sets PINGULARITY_OPTS gets no effect from it - the
// official images use an exec-form ENTRYPOINT, nothing in them expands the
// variable, and the binary never parses it - so the warning fires on any
// non-blank value there. On a native install the systemd unit expands the same
// variable into argv, so set-and-working stays silent. Token membership in
// argv is deliberately not the rule: -db is in every container's argv from the
// ENTRYPOINT, so "-db /custom/path" - the most dangerous ignored value - would
// never warn under token matching. (Policy, pinned by the code comment rather
// than an input here: the warning also fires when equivalent flags WERE passed
// as image arguments, because the variable itself is still stale
// configuration; the predicate has no argv input, so that case is by
// construction identical to "flag set".)
func TestIgnoredOptsWarning(t *testing.T) {
	cases := []struct {
		name        string
		opts        string
		inContainer bool
		want        bool
	}{
		{"container, flag set", "-allow-host=x.example.com", true, true},
		{"container, -db override (token-match trap)", "-db /custom/path", true, true},
		{"container, whitespace only", "   \t", true, false},
		{"container, empty", "", true, false},
		{"native systemd, expanded and working", "-allow-host=x.example.com", false, false},
		{"native, empty", "", false, false},
	}
	for _, c := range cases {
		if got := ignoredOptsWarning(c.opts, c.inContainer); got != c.want {
			t.Errorf("%s: ignoredOptsWarning(%q, %v) = %v, want %v", c.name, c.opts, c.inContainer, got, c.want)
		}
	}
}

// Static pin on the stderr message constant: it must name the variable, the
// container remedy, and the native expansion site. Coverage split, honestly:
// automatic CI (this file) pins the predicate and the replay HELPER's
// armed/unarmed behavior; the manually dispatched deep-test workflow pins the
// stderr line (docker-logs grep), the replay's PRODUCTION invocation and
// startup ordering, and its delivery into the About ring (/api/logs grep) -
// plus value-non-leakage via a secret sentinel in the variable, checked
// absent from both docker logs and /api/logs. Deleting the program.run call
// site would pass this file and fail the manual workflow, not automatic CI.
func TestIgnoredOptsMsgNamesTheEssentials(t *testing.T) {
	for _, must := range []string{"PINGULARITY_OPTS", "command:", "/etc/default/pingularity"} {
		if !strings.Contains(ignoredOptsMsg, must) {
			t.Errorf("message must mention %q; got %q", must, ignoredOptsMsg)
		}
	}
}

// The replay helper must fire exactly when runCmd recorded an ignored
// variable, must name the variable, and - being built from static strings with
// no access to the environment - must not carry the value. Drives the method
// directly against a captured slog handler: helper behavior only; the
// production call site is the manual workflow's to pin (see above).
func TestReplayIgnoredOpts(t *testing.T) {
	var buf bytes.Buffer
	p := &program{log: slog.New(slog.NewTextHandler(&buf, nil)), optsIgnored: true}
	p.replayIgnoredOpts()
	out := buf.String()
	if !strings.Contains(out, "PINGULARITY_OPTS") || !strings.Contains(out, "official container images") {
		t.Errorf("replay must emit the structured warning; got %q", out)
	}

	buf.Reset()
	p.optsIgnored = false
	p.replayIgnoredOpts()
	if buf.Len() != 0 {
		t.Errorf("no replay when nothing was ignored; got %q", buf.String())
	}
}
