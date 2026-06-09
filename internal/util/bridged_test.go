package util

import (
	"net"
	"testing"
)

// A false warning on a correctly configured host install is worse than a missed
// one, so the host-networked shapes below matter more than the bridged ones.
func TestBridgedFrom(t *testing.T) {
	ips := func(ss ...string) []net.IP {
		out := make([]net.IP, 0, len(ss))
		for _, s := range ss {
			out = append(out, net.ParseIP(s))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		addrs []net.IP
		want  bool
	}{
		{"bridged: docker0 default pool", ips("172.17.0.2"), true},
		{"bridged: a compose network", ips("172.20.0.5"), true},
		{"host net: the bridge is visible next to the NIC", ips("192.168.1.40", "172.17.0.1"), false},
		{"host net: plain LAN host", ips("192.168.1.40"), false},
		{"host net: LAN plus VPN", ips("10.0.0.5", "10.8.0.2"), false},
		{"custom bridge subnet, missed by design", ips("10.99.0.2"), false},
		{"no interfaces up", nil, false},
	} {
		if got := bridgedFrom(tc.addrs); got != tc.want {
			t.Errorf("%s: %v -> %v, want %v", tc.name, tc.addrs, got, tc.want)
		}
	}
}

// This dev box is not a bridged container, so the live path must not warn.
func TestBridgedFromOnThisHost(t *testing.T) {
	if !InContainer() && bridgedFrom(upIPv4()) {
		t.Error("flagged a non-container host as bridged - that is a false warning on a correct install")
	}
}
