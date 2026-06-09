package netinfo

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveExitDiscovery exercises the real network paths (Cloudflare colo +
// traceroute). Skipped unless PINGULARITY_LIVE_TRACE=1 - it needs internet,
// and the traceroute additionally needs root/CAP_NET_RAW or an enabled
// ping_group_range (without them it must degrade to a nil exit, not fail).
//
//	PINGULARITY_LIVE_TRACE=1 go test ./internal/netinfo/ -run LiveExit -v
func TestLiveExitDiscovery(t *testing.T) {
	if os.Getenv("PINGULARITY_LIVE_TRACE") == "" {
		t.Skip("set PINGULARITY_LIVE_TRACE=1 to run the live discovery test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if colo := m.cfColo(ctx); colo == "" {
		t.Error("cfColo returned empty (no internet, or Cloudflare unreachable)")
	} else {
		t.Logf("cloudflare colo: %s", colo)
	}

	ex, err := m.discoverExit(ctx, "", traceTarget)
	if err != nil {
		t.Logf("traceroute unavailable (expected without root/ping perms): %v", err)
		return
	}
	t.Logf("exit: %+v", ex)
	if ex.IP == "" && ex.NextIP == "" {
		t.Error("discovery succeeded but found neither exit nor handoff hop")
	}
}

// TestLiveDNSProvider exercises the real DNS-resolver enrichment path: resolver
// egress IP -> Team Cymru ASN/name -> "AS<n> NAME", plus RIPE IPmap for the
// city. Skipped unless PINGULARITY_LIVE_TRACE=1.
//
//	PINGULARITY_LIVE_TRACE=1 go test ./internal/netinfo/ -run LiveDNSProvider -v
func TestLiveDNSProvider(t *testing.T) {
	if os.Getenv("PINGULARITY_LIVE_TRACE") == "" {
		t.Skip("set PINGULARITY_LIVE_TRACE=1 to run the live DNS provider test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Deterministic anchor: 1.1.1.1 is Cloudflare, AS13335.
	if p := dnsProvider(ctx, "1.1.1.1"); p == "" {
		t.Error("dnsProvider(1.1.1.1) empty (no internet, or Cymru unreachable)")
	} else {
		t.Logf("dnsProvider(1.1.1.1) = %q", p)
		if !strings.HasPrefix(p, "AS13335") {
			t.Errorf("dnsProvider(1.1.1.1) = %q, want AS13335 prefix", p)
		}
	}
	if city, _, _, ok := ipmapLoc(ctx, m, "1.1.1.1"); ok {
		t.Logf("ipmapLoc(1.1.1.1) = %q", city)
	}

	// The actual upstream resolver this host uses, end to end.
	if eip := resolverEgressIP(ctx); eip != "" {
		t.Logf("resolver egress IP: %s -> provider %q", eip, dnsProvider(ctx, eip))
		if city, _, _, ok := ipmapLoc(ctx, m, eip); ok {
			t.Logf("resolver location: %q", city)
		}
	} else {
		t.Log("resolver egress IP unavailable (whoami.akamai.net lookup failed)")
	}
}
