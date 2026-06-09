//go:build windows

package main

import (
	"time"

	"github.com/kardianos/service"
)

// applyServiceOpts sets Windows SCM recovery and startup ordering on the service
// config. The daemon deliberately exits non-zero when its web server dies (a
// self-heal so the manager restarts it); unlike systemd/launchd the SCM takes no
// recovery action by default, so it would just leave the service stopped. Tell it
// to restart. Depending on the TCP/IP and DNS-client services plus delayed
// auto-start means the network stack is up before we probe at boot, avoiding a
// false outage and a spurious reconnect speedtest.
func applyServiceOpts(sc *service.Config) {
	if sc.Option == nil {
		sc.Option = service.KeyValue{}
	}
	sc.Option[service.OnFailure] = service.OnFailureRestart
	sc.Option[service.OnFailureDelayDuration] = (5 * time.Second).String()
	sc.Option[service.OnFailureResetPeriod] = 60 // seconds of health before the failure count resets
	sc.Option["DelayedAutoStart"] = true
	sc.Dependencies = []string{"Tcpip", "Dnscache"}
}
