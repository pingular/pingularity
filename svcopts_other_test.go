//go:build !windows

package main

import (
	"strings"
	"testing"
	"text/template"
)

// The self-install systemd unit must match packaging/pingularity.service: read
// flags from /etc/default/pingularity via $PINGULARITY_OPTS, restart 5s after a
// crash, and bound the restart loop. This renders the custom template with the
// same fields kardianos feeds it and checks the result, and doubles as a parse
// guard so a malformed template fails here rather than at `pingularity install`.
func TestSystemdScript(t *testing.T) {
	// Stand-ins for kardianos's unexported cmd/cmdEscape funcs; identity is fine
	// for a path with no spaces or quotes.
	funcs := template.FuncMap{
		"cmd":       func(s string) string { return s },
		"cmdEscape": func(s string) string { return s },
	}
	tmpl, err := template.New("").Funcs(funcs).Parse(systemdScript)
	if err != nil {
		t.Fatalf("systemdScript failed to parse: %v", err)
	}

	data := struct {
		Description      string
		Path             string
		Name             string
		Dependencies     []string
		Arguments        []string
		WorkingDirectory string
		ReloadSignal     string
		Restart          string
	}{
		Description:  "Internet connectivity monitor with a built-in web dashboard.",
		Path:         "/usr/local/bin/pingularity",
		Name:         "pingularity",
		Dependencies: []string{"After=network-online.target", "Wants=network-online.target"},
		Arguments:    []string{"run"},
		ReloadSignal: "HUP",
		Restart:      "always",
	}

	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		t.Fatalf("systemdScript failed to render: %v", err)
	}
	out := b.String()

	mustContain := []string{
		"EnvironmentFile=-/etc/default/pingularity",
		"$PINGULARITY_OPTS",
		"RestartSec=5",
		"StartLimitBurst=5",
		"Restart=always",
		`ExecReload=/bin/kill -HUP "$MAINPID"`,
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("rendered unit missing %q\n---\n%s", want, out)
		}
	}

	mustNotContain := []string{
		"/etc/sysconfig", // the RPM-ism the stock kardianos template uses
		"RestartSec=120", // the stock 2-minute restart delay
	}
	for _, bad := range mustNotContain {
		if strings.Contains(out, bad) {
			t.Errorf("rendered unit still contains %q\n---\n%s", bad, out)
		}
	}
}
