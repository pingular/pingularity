package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/settings"
	"github.com/pingular/pingularity/internal/store"
)

// iperfContainerHint is the container-networking matcher main.go injects into
// the iperf3 engine (Iperf.EnvHint) in a BRIDGED container (see iperfEnvHintFn).
// Each failure class from the docker-vs-native analysis must map to a hint
// naming the fix, and anything else must map to NO hint - the matcher may only
// explain, never guess. It is a pure function over the error text and the run's
// server/bind/family settings, so the classes are pinned here without a
// container.
func TestIperfHintContainerClasses(t *testing.T) {
	cases := []struct {
		name, errText, server, bind, ipver string
		want                               string // required substring; "" = exactly no hint
	}{
		// I1: loopback server target - the container has its own localhost.
		{"refused v4 loopback", "download: unable to connect to server: Connection refused", "127.0.0.1:5201", "", "auto", "host.docker.internal"},
		{"refused localhost", "connect failed: Connection refused", "localhost", "", "auto", "host.docker.internal"},
		{"refused bracketed v6 loopback", "iperf3: error - unable to connect to server: Connection refused", "[::1]:5201", "", "auto", "host.docker.internal"},
		// A refused NON-loopback server is a plain dead server, not a container issue.
		{"refused non-loopback stays bare", "unable to connect to server: Connection refused", "192.168.1.50:5201", "", "auto", ""},
		// I3: --bind with a host IP fails EADDRNOTAVAIL inside the namespace.
		{"bind EADDRNOTAVAIL", "iperf3: error - unable to connect to server: Cannot assign requested address", "10.0.0.2:5201", "192.168.1.23", "auto", `bind address "192.168.1.23"`},
		// I4: --bind-dev with a host NIC name fails ENODEV inside the namespace.
		{"bind-dev ENODEV", "iperf3: error - unable to connect to server: No such device", "10.0.0.2:5201", "eno1", "auto", `host interface "eno1"`},
		{"bind-dev bad interface", "unable to connect to server: Bad interface name", "10.0.0.2:5201", "eno1", "auto", `host interface "eno1"`},
		// I5: a forced -6 on the IPv6-less default bridge.
		{"forced v6 unreachable", "upload: unable to connect to server: Network unreachable", "iperf.example.com:5201", "", "6", "no IPv6"},
		{"forced v6 cannot assign", "unable to connect to server: Cannot assign requested address", "iperf.example.com:5201", "", "6", "no IPv6"},
		// An explicit bind is the likelier direct cause than the -6 pin.
		{"bind precedes v6 on cannot-assign", "Cannot assign requested address", "iperf.example.com:5201", "192.168.1.23", "6", `bind address "192.168.1.23"`},
		// cannot-assign with neither a bind nor a -6 pin matches no class.
		{"cannot assign, no bind, auto family", "Cannot assign requested address", "10.0.0.2:5201", "", "auto", ""},
		// Unrelated failures map to none, whatever the settings look like.
		{"unrelated auth", "test authorization failed", "127.0.0.1:5201", "", "auto", ""},
		{"unrelated busy", "the server is busy running a test. Try again later.", "localhost", "", "auto", ""},
		{"unrelated no-data with container-y settings", "no data transferred", "10.0.0.2:5201", "192.168.1.23", "6", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := iperfContainerHint(c.errText, c.server, c.bind, c.ipver)
			if c.want == "" {
				if got != "" {
					t.Fatalf("hint = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("hint = %q, want substring %q", got, c.want)
			}
		})
	}
}

// The matcher's injection gate keys on BRIDGED (util.BridgedContainer), not
// merely containerized: in a host-network container localhost IS the host and
// host NICs exist in our namespace, so every bridged hint - host.docker.internal
// for a refused loopback server above all - would send the operator away from a
// server plain 127.0.0.1 does reach. iperfEnvHintFn is that gate as a named
// function, so the key is pinned here instead of buried in run()'s wiring.
func TestIperfEnvHintGateKeysOnBridged(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	set, err := settings.New(ctx, st, settings.Values{
		Latency: 5 * time.Second, Speed: time.Hour, Timeout: 2 * time.Second,
		DownAfter: 3, UpAfter: 2, IperfServer: "127.0.0.1:5201",
	})
	if err != nil {
		t.Fatalf("new settings: %v", err)
	}

	// Host-network container (and native): no matcher at all - the engine's
	// nil-check keeps errors bare, exactly as they should read there.
	if fn := iperfEnvHintFn(false, set); fn != nil {
		t.Fatal("non-bridged got an EnvHint matcher; host-network hints would be wrong (localhost IS the host)")
	}

	// Bridged: the matcher fires on the loopback-server class using the live
	// settings (the configured server feeds the match).
	fn := iperfEnvHintFn(true, set)
	if fn == nil {
		t.Fatal("bridged container got no EnvHint matcher")
	}
	if h := fn("unable to connect to server: Connection refused"); !strings.Contains(h, "host.docker.internal") {
		t.Fatalf("bridged loopback-refused hint = %q, want host.docker.internal", h)
	}
}
