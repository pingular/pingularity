// Package util holds tiny, dependency-free helpers shared across packages, in one
// place so they aren't reimplemented (and left to drift) per-package.
package util

import (
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// InContainer reports whether the process is running inside a container,
// detected once via the markers the runtimes themselves plant.
//
// ADVISORY ONLY. It once relaxed the loopback-only access filter; it does not
// any more, and nothing about access may be decided from it again. Guessing a
// container's network layout to decide who may reach the dashboard is what put
// an unauthenticated dashboard on the LAN, so access is now an explicit setting
// with an explicit override and this answer has no say in it (there is a test
// that fails if any new caller writes an access decision). What it still does:
// pick the wording of warnings and hints, and tell the dashboard which
// environment it is describing.
var (
	inContainerOnce sync.Once
	inContainerVal  bool
)

// containerMarkerFiles are the marker files runtimes create: Docker's
// /.dockerenv, Podman/CRI-O's /run/.containerenv. A var so tests can point the
// probe at paths they control (and a containerized CI runner's real markers
// can't leak into the "native" case).
var containerMarkerFiles = []string{"/.dockerenv", "/run/.containerenv"}

func InContainer() bool {
	inContainerOnce.Do(func() { inContainerVal = detectContainer() })
	return inContainerVal
}

// detectContainer is the uncached probe behind InContainer, split out so tests
// can drive each marker via t.Setenv and containerMarkerFiles.
func detectContainer() bool {
	// containerd (the default Kubernetes runtime) leaves no marker file, but
	// the kubelet injects this env var into every pod - without it a bridged
	// pod would boot with the loopback-only filter on and 403 every off-host
	// request.
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	// Plain LXC/LXD, systemd-nspawn, and podman builds that don't write
	// /run/.containerenv leave no marker file either; they set the standard
	// `container` env var instead (container=lxc, =systemd-nspawn, =podman...).
	// Deliberately NOT consulted: /proc/1/cgroup string sniffing - under
	// cgroup v2 the unified hierarchy reads "0::/" inside most containers and
	// on hosts alike, so the string proves nothing.
	if os.Getenv("container") != "" {
		return true
	}
	for _, p := range containerMarkerFiles {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// BridgedContainer reports whether we are in a container that does NOT share the
// host's network namespace ("bridged" / isolated). Two things key on it. The UI
// warns that bridged measurements describe the container network, not the host's
// real path (an extra latency hop, a traceroute that dead-ends at the container
// gateway, and - the largest, least obvious distortion - a substituted DNS
// resolver, because a loopback stub is unreachable from a bridged namespace so
// Docker swaps in its own public default). The iperf3 hints key on it too, to
// explain why a host-referential setting cannot work from inside a namespace
// that has no such interface or address.
//
// ADVISORY ONLY, and it no longer gates access. It used to, and the shape of the
// answer still carries that history: it reports "bridged" unless it can PROVE
// host networking, because under the old access gate a wrong "not bridged"
// locked a container out of its own published port and a wrong "bridged" merely
// left one reachable. That asymmetry is gone with the gate. What remains is
// the bias, and it is worth knowing which way it points: proof of host
// networking is an interface that exists only in the host namespace - the
// Docker/Podman default bridge (docker0 / podman0), a user-defined bridge
// (br-...), a CNI bridge (cni-... / cni0), a CNI host-side device
// (cali*/cilium*/flannel*/vxlan.calico), or the host end of a veth pair
// (veth...). A host-network pod on a node running no local workloads may
// present none of them, and is then described as bridged when it is not: the
// operator is told their readings describe a container network that does not
// exist. That is a wrong sentence, not a wrong access decision, which is why
// this is left as it is rather than inverted - inverting it would silently drop
// the warning for genuinely bridged containers whose interface names we do not
// happen to recognise, and those are the ones with distorted readings.
var (
	bridgedOnce sync.Once
	bridgedVal  bool
)

func BridgedContainer() bool {
	bridgedOnce.Do(func() {
		if !InContainer() {
			return // native: loopback works, enforce local-only; never "bridged"
		}
		bridgedVal = !sharesHostNet(ifaceNames())
	})
	return bridgedVal
}

// ifaceNames lists the name of every network interface, up OR down: a bridge
// with no attached container is down but still a host-namespace tell.
func ifaceNames() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ifs))
	for _, in := range ifs {
		out = append(out, in.Name)
	}
	return out
}

// sharesHostNet reports whether names include an interface that exists ONLY in
// the host's network namespace - a reliable positive signal that this process is
// in that namespace (native or --network=host), where loopback reaches the
// listener. A bridged container's namespace holds only its own eth0 (+ lo), never
// these, so their ABSENCE means "isolated, or we cannot tell", which the caller
// treats as isolated (reachable). Kept as a pure function over a name list so it
// is tested directly rather than through a parallel copy of the rule.
func sharesHostNet(names []string) bool {
	for _, n := range names {
		switch {
		case n == "docker0" || n == "podman0":
			return true
		// flannel's bridge and Calico's VXLAN device: fixed names, host-side only.
		case n == "cni0" || n == "vxlan.calico":
			return true
		case strings.HasPrefix(n, "br-"),
			strings.HasPrefix(n, "veth"),
			strings.HasPrefix(n, "cni-"):
			return true
		// CNI host-side families: Calico's per-pod veth ends (cali...), Cilium's
		// cilium_host/cilium_net/cilium_vxlan, flannel's flannel.1/flannel-v6.1.
		// Without these a hostNetwork pod on such a cluster read as bridged
		// (false banner, wrong default). tunl0 is deliberately NOT a tell:
		// fallback tunnel devices (tunl0, sit0, ip6tnl0) are created in EVERY
		// network namespace once their module loads, so a bridged pod on a
		// Calico IPIP node sees tunl0 in its own namespace - matching it would
		// flip such pods to loopback-only and lock them out of their own
		// published port.
		case strings.HasPrefix(n, "cali"),
			strings.HasPrefix(n, "cilium"),
			strings.HasPrefix(n, "flannel"):
			return true
		}
	}
	return false
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
