package store

import (
	"context"
	"testing"
)

func TestLatestConnInfo(t *testing.T) {
	st := open(t)
	ctx := context.Background()
	// no runs yet -> nil
	if sp, err := st.LatestConnInfo(ctx); err != nil || sp != nil {
		t.Fatalf("empty store: got (%v,%v), want (nil,nil)", sp, err)
	}
	// a run WITHOUT conn info should be ignored
	if err := st.InsertSpeed(ctx, SpeedSample{TS: 100, DownMbps: 50, Server: "x"}); err != nil {
		t.Fatal(err)
	}
	if sp, _ := st.LatestConnInfo(ctx); sp != nil {
		t.Fatalf("run without ISP/IP should be skipped, got %+v", sp)
	}
	// a run WITH conn info
	if err := st.InsertSpeed(ctx, SpeedSample{TS: 200, DownMbps: 412, Server: "Bell, Toronto",
		ISP: "AS1403 EBOX", ISPLocation: "Toronto, Ontario, CA", PublicIPv4: "204.244.221.226",
		DNSIP: "155.138.130.135", DNSProvider: "AS20473 The Constant Company, LLC"}); err != nil {
		t.Fatal(err)
	}
	// a newer run withOUT conn info must NOT shadow the good one
	if err := st.InsertSpeed(ctx, SpeedSample{TS: 300, DownMbps: 60, Server: "y"}); err != nil {
		t.Fatal(err)
	}
	sp, err := st.LatestConnInfo(ctx)
	if err != nil || sp == nil {
		t.Fatalf("got (%v,%v), want the good run", sp, err)
	}
	if sp.ISP != "AS1403 EBOX" || sp.PublicIPv4 != "204.244.221.226" || sp.DNSProvider == "" {
		t.Fatalf("wrong run: isp=%q ip=%q dns=%q", sp.ISP, sp.PublicIPv4, sp.DNSProvider)
	}
}
