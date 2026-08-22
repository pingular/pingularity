package settings

import (
	"context"
	"strings"
	"testing"
)

// Two addresses that differ only past the 255-byte cap store the same value, so the
// dedupe must run on the capped address; otherwise both are kept and the saved list
// holds the same address twice.
func TestSanitizeIperfServersDedupesAfterCap(t *testing.T) {
	prefix := strings.Repeat("h", 255) // the whole address budget
	one, two := prefix+".one:5201", prefix+".two:5201"

	out := sanitizeIperfServers([]IperfTarget{
		{Label: "one", Addr: one, Username: "one"},
		{Label: "two", Addr: two, Username: "two"},
	})
	if len(out) != 1 {
		t.Fatalf("got %d servers, want 1; addrs %q", len(out), addrsOf(out))
	}
	if out[0].Addr != prefix || out[0].Label != "one" || out[0].Username != "one" {
		t.Errorf("survivor should be the first entry, got %+v", out[0])
	}
}

// addrsOf lists the saved addresses by their distinct tail, so a failure message
// stays readable when the addresses are at the length cap.
func addrsOf(ts []IperfTarget) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = capLast(t.Addr, 12)
	}
	return out
}

func capLast(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// Saving two servers whose addresses collide at the cap must leave one entry with its
// own credentials: a duplicate address makes the password merge (keyed by address)
// hand the surviving server the other one's password on the next form save.
func TestUpdateIperfServersCollidingAtCap(t *testing.T) {
	c := newController(t)
	ctx := context.Background()
	prefix := strings.Repeat("h", 255)
	one, two := prefix+".one:5201", prefix+".two:5201"

	p := Patch{IperfServer: pv(one)}
	p.IperfServers = []IperfTarget{
		{Label: "one", Addr: one, Auth: true, Username: "one", Password: "pw-one"},
		{Label: "two", Addr: two, Auth: true, Username: "two", Password: "pw-two"},
	}
	v, err := c.Update(ctx, p)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(v.IperfServers) != 1 {
		t.Fatalf("saved %d servers, want 1; addrs %q", len(v.IperfServers), addrsOf(v.IperfServers))
	}

	// Re-save the form with blank passwords (the API never echoes them).
	p.IperfServers[0].Password, p.IperfServers[1].Password = "", ""
	if _, err := c.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := c.IperfPassword(); got != "pw-one" {
		t.Errorf("active server password = %q, want %q", got, "pw-one")
	}
}
