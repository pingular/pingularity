// Package util holds tiny, dependency-free helpers shared across packages, in one
// place so they aren't reimplemented (and left to drift) per-package.
package util

import (
	"fmt"
	"math"
	"net"
	"os"
	"sync"
	"time"
)

// InContainer reports whether the process is running inside a container (Docker or
// Podman), detected once via the marker files those runtimes create. Used to relax
// the loopback-only access filter: a bridged container NATs every external request
// to the gateway, so the filter can't tell a local user from a LAN one and would
// otherwise lock the dashboard out entirely.
var (
	inContainerOnce sync.Once
	inContainerVal  bool
)

func InContainer() bool {
	inContainerOnce.Do(func() {
		// Docker and Podman/CRI-O leave marker files; containerd (the default
		// Kubernetes runtime) leaves neither, but the kubelet injects this env
		// var into every pod - without it a bridged pod would boot with the
		// loopback-only filter on and 403 every off-host request.
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			inContainerVal = true
			return
		}
		for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
			if _, err := os.Stat(p); err == nil {
				inContainerVal = true
				return
			}
		}
	})
	return inContainerVal
}

// BridgedContainer reports whether we are in a container that does NOT share the
// host's network namespace - the case where every measurement describes the
// container network instead of the host's real path: an extra hop of latency,
// and a traceroute that dead-ends at the container gateway.
//
// It matters because `network_mode: host` is Linux-only, so every Docker Desktop
// user on macOS or Windows is pushed into exactly this mode by the compose file,
// and nothing on screen would otherwise say the numbers changed meaning.
//
// The test is deliberately narrow, because a FALSE warning on a correctly
// configured host install is worse than a missed one: a bridged container has
// exactly one non-loopback interface, addressed out of Docker's default bridge
// pool (172.16/12, which covers docker0's 172.17 and compose's 172.18+). A
// host-networked container sees the host's whole interface set, which - since
// Docker is by definition running - includes the bridge itself. Bridges built on
// a custom subnet are missed, and that is the intended trade.
var (
	bridgedOnce sync.Once
	bridgedVal  bool
)

func BridgedContainer() bool {
	bridgedOnce.Do(func() {
		if !InContainer() {
			return
		}
		bridgedVal = bridgedFrom(upIPv4())
	})
	return bridgedVal
}

// upIPv4 lists the IPv4 addresses of every non-loopback interface that is up.
func upIPv4() []net.IP {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, in := range ifs {
		if in.Flags&net.FlagLoopback != 0 || in.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := in.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP)
			}
		}
	}
	return out
}

// bridgedFrom is the decision itself, over a plain address list so it can be
// tested directly rather than through a parallel copy of the rule.
func bridgedFrom(addrs []net.IP) bool {
	pool := net.IPNet{IP: net.IPv4(172, 16, 0, 0), Mask: net.CIDRMask(12, 32)}
	inPool := false
	for _, ip := range addrs {
		if pool.Contains(ip) {
			inPool = true
		}
	}
	return len(addrs) == 1 && inPool
}

// B2I converts a bool to 1/0, for Prometheus gauges, CSV, and SQLite columns.
func B2I(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Round1 rounds to one decimal place (half away from zero, via math.Round).
func Round1(f float64) float64 { return math.Round(f*10) / 10 }

// Round2 rounds to two decimal places (half away from zero).
func Round2(f float64) float64 { return math.Round(f*100) / 100 }

// DurMS converts a duration to milliseconds as a float (1500µs -> 1.5).
func DurMS(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// HumanDur renders a duration in seconds compactly: "45s", "1m 30s", "1h 2m".
// Negative input clamps to 0.
func HumanDur(s int) string {
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh %dm", s/3600, (s%3600)/60)
	}
}
