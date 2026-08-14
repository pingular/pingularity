package util

import "testing"

// The access default keys on BridgedContainer, so the detector must err SAFE:
// classify as host-net (loopback-only enforceable) ONLY when a host-namespace
// interface is visible; treat everything else as isolated (reachable). This
// table pins both directions - especially the two cases the old address-range
// guess got wrong: a custom-subnet bridge (was locked out) and a host on a
// 172.16/12 LAN (was wrongly opened).
func TestSharesHostNet(t *testing.T) {
	for _, tc := range []struct {
		name string
		ifs  []string
		want bool // true = shares host net = enforce local-only
	}{
		// Host networking: a host-namespace interface is visible.
		{"host: docker0 next to the NIC", []string{"eth0", "docker0", "lo"}, true},
		{"host: podman0", []string{"enp3s0", "podman0", "lo"}, true},
		{"host: a user-defined bridge", []string{"eth0", "br-9a1f2c3d", "lo"}, true},
		{"host: a container veth pair end", []string{"eth0", "veth1a2b3c", "lo"}, true},
		{"host: a CNI bridge", []string{"eth0", "cni-podman0", "lo"}, true},
		// CNI host-side names: a hostNetwork k8s pod on these clusters must not
		// read as bridged (false banner, wrong access default).
		{"host: flannel bridge cni0", []string{"eth0", "cni0", "lo"}, true},
		{"host: calico per-pod veth end", []string{"eth0", "cali1a2b3c4d5e6", "lo"}, true},
		{"host: calico vxlan device", []string{"eth0", "vxlan.calico", "lo"}, true},
		{"host: cilium devices", []string{"eth0", "cilium_host", "cilium_net", "lo"}, true},
		{"host: flannel vxlan device", []string{"eth0", "flannel.1", "lo"}, true},
		// Bridged / isolated: only the container's own eth0 (+ lo). NONE of these
		// may be classified host-net, or the published port 403s with no way in.
		{"bridged: docker default pool", []string{"eth0", "lo"}, false},
		// tunl0 is an ipip FALLBACK device, created in every network namespace
		// once the module loads - a bridged pod on a Calico IPIP node sees it in
		// its own namespace. It must never count as a host tell, or exactly those
		// pods would default loopback-only and lock themselves out.
		{"bridged: pod on a Calico IPIP node sees tunl0", []string{"eth0", "tunl0", "lo"}, false},
		{"bridged: custom subnet (old design locked this out)", []string{"eth0", "lo"}, false},
		{"bridged: attached to two user networks (multi-eth)", []string{"eth0", "eth1", "lo"}, false},
		{"host-net with only one NIC, no bridge up yet (over-expose, never lock out)", []string{"ens0", "lo"}, false},
		{"nothing", nil, false},
		// A bridged container must never see a host-namespace name; guard the
		// substring matching isn't fooled by a plain interface that merely
		// contains the letters.
		{"not a bridge: 'ethbr-ish' does not start with br-", []string{"ethbr0", "lo"}, false},
	} {
		if got := sharesHostNet(tc.ifs); got != tc.want {
			t.Errorf("%s: sharesHostNet(%v) = %v, want %v", tc.name, tc.ifs, got, tc.want)
		}
	}
}

// This dev box is not a bridged container, so the live path must resolve without
// panicking and (off-container) never report bridged.
func TestBridgedContainerOnThisHost(t *testing.T) {
	if !InContainer() && BridgedContainer() {
		t.Error("flagged a non-container host as bridged - a native install must default to loopback-only")
	}
}
