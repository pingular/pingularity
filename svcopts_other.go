//go:build !windows

package main

import (
	"runtime"
	"strings"

	"github.com/kardianos/service"
)

// applyServiceOpts sets non-Windows service ordering. Only systemd has a clean
// network-ready gate; launchd on macOS has no equivalent, so there we lean on the
// existing DownAfter debounce to swallow a brief boot-time blip rather than
// confirm a false outage (and fire a reconnect speedtest).
// servicePath is the PATH the service is registered with, or "" to leave the
// platform's default. launchd starts a system daemon with
// /usr/bin:/bin:/usr/sbin:/sbin, which reaches nothing a package manager installs,
// so anything the daemon looks up by name - iperf3 - was found from every terminal
// and not from the service. The package managers' directories come first, as on a
// terminal. systemd's default already covers /usr/local/bin and the Windows SCM
// inherits the machine PATH, so nothing is written there.
func servicePath(goos string) string {
	if goos == "darwin" {
		return "/opt/homebrew/bin:/usr/local/bin:/opt/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return ""
}

func applyServiceOpts(sc *service.Config) {
	if p := servicePath(runtime.GOOS); p != "" {
		if sc.EnvVars == nil {
			sc.EnvVars = map[string]string{}
		}
		sc.EnvVars["PATH"] = p
	}
	// Start after the network is up, else a boot where DHCP/WiFi comes up late
	// records a false outage. These strings go into the unit's [Unit] section
	// verbatim, so only systemd gets them - other init systems expect service
	// names here.
	if service.Platform() == "linux-systemd" {
		sc.Dependencies = []string{"After=network-online.target", "Wants=network-online.target"}
		// ExecReload=/bin/kill -HUP: lets `systemctl reload pingularity` drive the
		// SIGHUP settings reload (see the reload goroutine in main.go).
		if sc.Option == nil {
			sc.Option = service.KeyValue{}
		}
		sc.Option["ReloadSignal"] = "HUP"
		// Replace kardianos's stock unit so the self-install service tracks the
		// operational behaviour of the deb/rpm one (packaging/pingularity.service):
		// $PINGULARITY_OPTS from /etc/default, a 5s restart delay, a bounded restart
		// loop, and the root-compatible subset of that unit's sandboxing. It is NOT
		// full parity - this path installs as root (kardianos emits no User=), so it
		// keeps neither the de-rooting nor ProtectSystem=strict; see the template
		// comment for exactly what is and isn't carried over. The stock default reads
		// /etc/sysconfig (an RPM-ism), has no $PINGULARITY_OPTS in ExecStart, and uses
		// RestartSec=120 - so a self-install user could never add daemon flags
		// post-install, and a crash kept the monitor down for 2 minutes instead of 5
		// seconds.
		sc.Option["SystemdScript"] = systemdScript
		// The template writes each install argument through kardianos's cmd,
		// which double-quotes it and escapes only the quote. systemd then reads
		// ExecStart by its own rules, quotes or not: %x is a specifier (%t the
		// runtime directory, %h the home), $VAR and ${VAR} substitute the
		// environment, and a backslash starts a C escape. So a -metrics-token
		// with a % in it reached the daemon as a different token and the
		// scraper's 401s said nothing about why, a %-mangled -db planted the
		// database at a path nobody named, and an unknown specifier or a
		// trailing backslash left a unit systemd refuses to load. Escape here,
		// on the unit path only: launchd's plist and the Windows SCM expand
		// nothing and must see the bytes as typed.
		sc.Arguments = systemdArgs(sc.Arguments)
	}
}

// systemdArgs escapes install arguments for the unit's ExecStart line per
// systemd.unit(5) and systemd.service(5): a literal percent is written %%, a
// literal dollar $$, and every backslash doubled (inside double quotes systemd
// unescapes C sequences, so a lone one eats the closing quote or turns into a
// newline). The double quote itself stays kardianos's cmd's job. Backslashes
// first, so the doubling never touches what the other two produce.
func systemdArgs(args []string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		a = strings.ReplaceAll(a, `\`, `\\`)
		a = strings.ReplaceAll(a, "%", "%%")
		out[i] = strings.ReplaceAll(a, "$", "$$")
	}
	return out
}

// systemdScript tracks packaging/pingularity.service for the self-install path:
// flags from /etc/default/pingularity via $PINGULARITY_OPTS, a 5s restart delay, a
// bounded restart loop so a wedged binary reaches the 'failed' state, and the
// root-compatible hardening directives (see the [Service] block). It is NOT a
// line-for-line copy: this unit runs as root, so it drops the packaged unit's
// User=/ambient CAP_NET_RAW/StateDirectory de-rooting and uses ProtectSystem=full
// rather than =strict. The cmd/cmdEscape funcs and the template fields come from
// kardianos's systemd backend; the Dependencies range emits the After=/Wants= lines
// set above.
const systemdScript = `[Unit]
Description={{.Description}}
ConditionFileIsExecutable={{.Path|cmdEscape}}
{{range $i, $dep := .Dependencies}}
{{$dep}} {{end}}
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
EnvironmentFile=-/etc/default/{{.Name}}
ExecStart={{.Path|cmdEscape}}{{range .Arguments}} {{.|cmd}}{{end}} $PINGULARITY_OPTS
{{if .WorkingDirectory}}WorkingDirectory={{.WorkingDirectory|cmdEscape}}{{end}}
{{if .ReloadSignal}}ExecReload=/bin/kill -{{.ReloadSignal}} "$MAINPID"{{end}}
{{if .Restart}}Restart={{.Restart}}{{end}}
RestartSec=5
# Defence-in-depth hardening. This self-install unit runs as ROOT (kardianos
# installs with no User=), so it deliberately drops the packaged unit's de-rooting
# (User=/Group=/AmbientCapabilities/CapabilityBoundingSet/StateDirectory) and uses
# ProtectSystem=full rather than =strict - a root daemon writes its DB under /var,
# which strict would render read-only. The directives below still hold under root:
# forbid privilege escalation, keep /usr,/boot,/etc read-only, hide home dirs, give
# a private /tmp, and lock down kernel tunables, SUID/SGID and personality.
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes
ProtectKernelTunables=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
`
