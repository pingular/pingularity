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
// detected once via the markers the runtimes themselves plant. Used to relax
// the loopback-only access filter: a bridged container NATs every external request
// to the gateway, so the filter can't tell a local user from a LAN one and would
// otherwise lock the dashboard out entirely.
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
// Docker swaps in its own public default). More consequentially, it gates the
// default access filter: a bridged container can ONLY be reached over the
// network (even from the host, via the published port), so it must NOT default
// to loopback-only or it would 403 its own port with no way back in.
//
// Because it now gates access, the detection errs SAFE: return false ("not
// bridged", so default to loopback-only) ONLY when we can prove loopback reaches
// us - natively, or in a container that visibly shares the host's network
// namespace. Proof of host networking is seeing an interface that exists only in
// the host namespace: the Docker/Podman default bridge (docker0 / podman0), a
// user-defined bridge (br-...), a CNI bridge (cni-... / cni0), a CNI host-side
// device (cali*/cilium*/flannel*/vxlan.calico), or the host end of any
// container's veth pair (veth...). A bridged container sees only its own eth0, so
// it never matches and is treated as isolated (reachable by default) whatever its
// subnet or interface count. A misread can only leave a host-net container
// reachable that could have been locked down (recoverable via access/auth) - it
// can never lock a bridged container out of its own port. The old address-range
// guess got this wrong both ways: a custom-subnet bridge was locked out, and a
// host on a 172.16/12 LAN was wrongly opened.
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
