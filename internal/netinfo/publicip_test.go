package netinfo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
)

// A failing echo service can still answer with something address-shaped, and the
// no-redirect policy hands 3xx bodies straight back to us. Only a 200 is the
// echo's real answer.
func TestPublicIPRejectsErrorStatus(t *testing.T) {
	oldV4, oldV6 := ipv4Client, ipv6Client
	defer func() { ipv4Client, ipv6Client = oldV4, oldV6 }()

	for _, status := range []int{
		http.StatusMovedPermanently,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		ipv4Client = canned(status, "203.0.113.9")
		if got := publicIPv4(context.Background()); got != "" {
			t.Errorf("publicIPv4 on HTTP %d = %q, want empty", status, got)
		}
		ipv6Client = canned(status, "2001:db8::1")
		if got := publicIPv6(context.Background()); got != "" {
			t.Errorf("publicIPv6 on HTTP %d = %q, want empty", status, got)
		}
	}

	ipv4Client = canned(http.StatusOK, "203.0.113.9\n")
	if got := publicIPv4(context.Background()); got != "203.0.113.9" {
		t.Errorf("publicIPv4 on HTTP 200 = %q, want 203.0.113.9", got)
	}
	ipv6Client = canned(http.StatusOK, "2001:db8::1\n")
	if got := publicIPv6(context.Background()); got != "2001:db8::1" {
		t.Errorf("publicIPv6 on HTTP 200 = %q, want 2001:db8::1", got)
	}
}

// The reason the status check matters: a rejected echo has to look like a failed
// lookup to fetch, so it keeps the last-known identity and flags the snapshot for
// the fast retry instead of publishing the error body's address as this host's IP.
func TestFetchTreatsEchoErrorStatusAsFailure(t *testing.T) {
	oldV4, oldV6, oldRE := ipv4Client, ipv6Client, resolverEgress
	defer func() { ipv4Client, ipv6Client, resolverEgress = oldV4, oldV6, oldRE }()
	ipv4Client = canned(http.StatusBadGateway, "203.0.113.9")
	ipv6Client = canned(http.StatusBadGateway, "2001:db8::1")
	resolverEgress = func(context.Context) string { return "" }
	// Hermetic: rDNS and Cymru ride net.DefaultResolver, so fail those instantly
	// rather than let a networked machine make real lookups.
	oldRes := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("no resolver in tests")
	}}
	defer func() { net.DefaultResolver = oldRes }()

	m := NewManager(slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.http = canned(500, "") // cfColo / geo answer nothing
	m.LastKnownFn = func() *Info {
		return &Info{PublicIP: "1.2.3.4", ISP: "AS-TEST EXAMPLE", City: "Townsville"}
	}

	info := m.fetch(context.Background())
	if info.PublicIP != "1.2.3.4" {
		t.Errorf("PublicIP = %q, want the last-known 1.2.3.4", info.PublicIP)
	}
	if info.PublicIPv6 != "" {
		t.Errorf("PublicIPv6 = %q, want empty", info.PublicIPv6)
	}
	if info.Error != "ip lookup failed" {
		t.Errorf("Error = %q, want %q", info.Error, "ip lookup failed")
	}
}
