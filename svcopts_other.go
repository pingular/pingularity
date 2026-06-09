//go:build !windows

package main

import "github.com/kardianos/service"

// applyServiceOpts sets non-Windows service ordering. Only systemd has a clean
// network-ready gate; launchd on macOS has no equivalent, so there we lean on the
// existing DownAfter debounce to swallow a brief boot-time blip rather than
// confirm a false outage (and fire a reconnect speedtest).
func applyServiceOpts(sc *service.Config) {
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
		// Replace kardianos's stock unit so the self-install service matches the
		// deb/rpm one (packaging/pingularity.service). The default reads
		// /etc/sysconfig (an RPM-ism), has no $PINGULARITY_OPTS in ExecStart, and
		// uses RestartSec=120 - so a self-install user can never add daemon flags
		// post-install, and a crash keeps the monitor down for 2 minutes instead
		// of 5 seconds.
		sc.Option["SystemdScript"] = systemdScript
	}
}

// systemdScript mirrors packaging/pingularity.service for the self-install path:
// flags from /etc/default/pingularity via $PINGULARITY_OPTS, a 5s restart delay,
// and a bounded restart loop so a wedged binary reaches the 'failed' state. The
// cmd/cmdEscape funcs and the template fields come from kardianos's systemd
// backend; the Dependencies range emits the After=/Wants= lines set above.
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

[Install]
WantedBy=multi-user.target
`
