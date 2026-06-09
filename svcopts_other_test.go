//go:build !windows

package main

import (
	"strings"
	"testing"
	"text/template"
)

// The self-install systemd unit tracks packaging/pingularity.service: read flags
// from /etc/default/pingularity via $PINGULARITY_OPTS, restart 5s after a crash,
// bound the restart loop, and apply the root-compatible hardening directives. Full
// de-rooting parity is deliberately out of scope - this unit installs as root, so
// it omits User=/ambient CAP_NET_RAW/StateDirectory and uses ProtectSystem=full not
// =strict (see svcopts_other.go). This renders the custom template with the same
// fields kardianos feeds it and checks the result, and doubles as a parse guard so
// a malformed template fails here rather than at `pingularity install`.
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
		// Root-compatible hardening: these must survive on a root-run unit and
		// stay in sync with the subset carried over from packaging/pingularity.service.
		"NoNewPrivileges=yes",
		"ProtectSystem=full",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"ProtectKernelTunables=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("rendered unit missing %q\n---\n%s", want, out)
		}
	}

	mustNotContain := []string{
		"/etc/sysconfig", // the RPM-ism the stock kardianos template uses
		"RestartSec=120", // the stock 2-minute restart delay
		// A root daemon writes its DB under /var; strict would make that tree
		// read-only, so the root-run unit must use ProtectSystem=full, not strict.
		"ProtectSystem=strict",
	}
	for _, bad := range mustNotContain {
		if strings.Contains(out, bad) {
			t.Errorf("rendered unit still contains %q\n---\n%s", bad, out)
		}
	}
}
