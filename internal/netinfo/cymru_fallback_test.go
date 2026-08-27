package netinfo

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pingular/pingularity/internal/stats"
)

// No test in this package may reach a public resolver by accident: the
// fallbacks answer "unreachable" unless a test installs its own (stubCymru).
// The host lookup still follows net.DefaultResolver, which the hermetic tests
// swap for an instantly failing one.
func TestMain(m *testing.M) {
	fail := func(context.Context, string) ([]string, error) { return nil, errTimeout }
	cymruFallbackTXT = []func(context.Context, string) ([]string, error){fail, fail}
	os.Exit(m.Run())
}

// stubCymru swaps the Team Cymru resolvers for canned answers and records the
// order they were asked in. "system" is the host resolver, "fb1"/"fb2" the
// public fallbacks. A nil entry answers; a non-nil one fails with that error.
func stubCymru(t *testing.T, system, fb1, fb2 error) *[]string {
	t.Helper()
	asked := &[]string{}
	mk := func(name string, fail error) func(context.Context, string) ([]string, error) {
		return func(_ context.Context, q string) ([]string, error) {
			*asked = append(*asked, name)
			if fail != nil {
				return nil, fail
			}
			return []string{"1403 | 96.127.192.0/18 | CA | arin | 2011-04-08"}, nil
		}
	}
	oldS, oldF, oldNow := cymruSystemTXT, cymruFallbackTXT, cymruNow
	cymruSystemTXT = mk("system", system)
	cymruFallbackTXT = []func(context.Context, string) ([]string, error){mk("fb1", fb1), mk("fb2", fb2)}
	t.Cleanup(func() {
		cymruSystemTXT, cymruFallbackTXT, cymruNow = oldS, oldF, oldNow
		cymruMu.Lock()
		cymruSuspectUntil = time.Time{}
		cymruMu.Unlock()
	})
	cymruMu.Lock()
	cymruSuspectUntil = time.Time{}
	cymruMu.Unlock()
	return asked
}

var errTimeout = &net.DNSError{Err: "i/o timeout", IsTimeout: true, IsTemporary: true}
var errNX = &net.DNSError{Err: "no such host", IsNotFound: true}

// The host resolver is asked first and, when it answers, alone.
func TestCymruLookupPrefersTheHostResolver(t *testing.T) {
	asked := stubCymru(t, nil, nil, nil)
	asn, err := cymruASNLookup(context.Background(), "96.127.240.62")
	if err != nil || asn != "1403" {
		t.Fatalf("asn = %q err = %v", asn, err)
	}
	if len(*asked) != 1 || (*asked)[0] != "system" {
		t.Errorf("asked %v, want the host resolver alone", *asked)
	}
}

// A host resolver that times out must not take the answer with it: the public
// fallback is asked, and the answer is the same one the host would have given.
// This is the failure measured on a NextDNS host - every exit discovery for an
// evening died on this one lookup, and the race lost its best origin.
func TestCymruLookupFallsBackWhenTheHostResolverFails(t *testing.T) {
	asked := stubCymru(t, errTimeout, nil, nil)
	before := stats.Lifetime().Counters["netinfo.cymru_fallback"]
	asn, err := cymruASNLookup(context.Background(), "96.127.240.62")
	if err != nil || asn != "1403" {
		t.Fatalf("asn = %q err = %v, want the fallback's answer", asn, err)
	}
	if want := []string{"system", "fb1"}; !sameOrder(*asked, want) {
		t.Errorf("asked %v, want %v", *asked, want)
	}
	if got := stats.Lifetime().Counters["netinfo.cymru_fallback"]; got != before+1 {
		t.Errorf("netinfo.cymru_fallback %d -> %d, want +1: an operator should be able to see the host resolver being bypassed", before, got)
	}
	// Once burned, the fallbacks go first: a dozen-hop trace must not pay the
	// dead resolver's timeout at every hop.
	asn, _ = cymruASNLookup(context.Background(), "96.127.240.63")
	if want := []string{"system", "fb1", "fb1"}; asn != "1403" || !sameOrder(*asked, want) {
		t.Errorf("second lookup: asked %v, want %v - the fallback first while the host resolver is suspect", *asked, want)
	}
}

func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A lookup that fails because the CALLER gave up - a closed tab cancelling a
// manual refresh, a trace past its own deadline - says nothing about the
// resolver: it must not be marked suspect, no fallback is asked, and nothing
// is counted.
func TestCymruLookupDoesNotBlameTheResolverForACancelledCaller(t *testing.T) {
	asked := stubCymru(t, &net.DNSError{Err: "operation was canceled"}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := stats.Lifetime().Counters["netinfo.cymru_fallback"]
	if _, err := cymruASNLookup(ctx, "96.127.240.62"); err == nil {
		t.Fatal("a cancelled lookup must fail")
	}
	if want := []string{"system"}; !sameOrder(*asked, want) {
		t.Errorf("asked %v, want %v - no fallback for a caller that gave up", *asked, want)
	}
	if got := stats.Lifetime().Counters["netinfo.cymru_fallback"]; got != before {
		t.Errorf("the fallback counter moved (%d -> %d) on a cancelled caller", before, got)
	}
	// The next, live lookup still trusts the host resolver first (and it answers).
	cymruSystemTXT = func(_ context.Context, q string) ([]string, error) {
		*asked = append(*asked, "system")
		return []string{"1403 | 96.127.192.0/18 | CA | arin | 2011-04-08"}, nil
	}
	cymruASNLookup(context.Background(), "96.127.240.62")
	if want := []string{"system", "system"}; !sameOrder(*asked, want) {
		t.Errorf("asked %v, want %v - the host resolver was marked suspect by a cancellation", *asked, want)
	}
}

// NXDOMAIN is an answer, not a failure: no fallback is consulted, and the
// caller sees "no origin ASN" rather than an error.
func TestCymruLookupTreatsNXDomainAsAnAnswer(t *testing.T) {
	asked := stubCymru(t, errNX, nil, nil)
	asn, err := cymruASNLookup(context.Background(), "10.0.0.1")
	if err != nil || asn != "" {
		t.Fatalf("asn = %q err = %v, want an empty, error-free answer", asn, err)
	}
	if len(*asked) != 1 {
		t.Errorf("asked %v, want no fallback for an NXDOMAIN", *asked)
	}
	// And the host resolver stays trusted: NXDOMAIN was an answer, not a fault.
	cymruASNLookup(context.Background(), "10.0.0.2")
	if want := []string{"system", "system"}; !sameOrder(*asked, want) {
		t.Errorf("asked %v, want %v", *asked, want)
	}
}

// While the host resolver is suspect it still gets its turn, last, and a
// success clears the suspicion - a fallback that is itself blocked (port 53
// intercepted) must not lock a recovered host resolver out for the window.
func TestCymruLookupRecoversTheHostResolver(t *testing.T) {
	asked := stubCymru(t, errTimeout, errTimeout, errTimeout)
	if _, err := cymruASNLookup(context.Background(), "96.127.240.62"); err == nil {
		t.Fatal("every resolver failed; want an error")
	}
	if want := []string{"system", "fb1", "fb2"}; !sameOrder(*asked, want) {
		t.Fatalf("asked %v, want %v", *asked, want)
	}
	// The host resolver comes back: it is asked last this time, answers, and is
	// trusted first again on the next lookup.
	cymruSystemTXT = func(_ context.Context, q string) ([]string, error) {
		*asked = append(*asked, "system")
		return []string{"1403 | 96.127.192.0/18 | CA | arin | 2011-04-08"}, nil
	}
	asn, err := cymruASNLookup(context.Background(), "96.127.240.62")
	if err != nil || asn != "1403" {
		t.Fatalf("asn = %q err = %v", asn, err)
	}
	if got := (*asked)[3:]; len(got) != 3 || got[2] != "system" {
		t.Errorf("asked %v after the outage, want fb1, fb2, then the suspect host resolver", got)
	}
	n := len(*asked)
	cymruASNLookup(context.Background(), "96.127.240.62")
	if got := (*asked)[n:]; len(got) != 1 || got[0] != "system" {
		t.Errorf("asked %v once the host resolver answered, want it trusted first again", got)
	}
}

// The suspect window expires on its own, so a host resolver that was down at
// boot is not bypassed forever.
func TestCymruSuspectWindowExpires(t *testing.T) {
	asked := stubCymru(t, errTimeout, nil, nil)
	now := time.Unix(1_700_000_000, 0)
	cymruNow = func() time.Time { return now }
	cymruASNLookup(context.Background(), "96.127.240.62") // burns the host resolver
	now = now.Add(cymruSuspectFor + time.Second)
	cymruSystemTXT = func(_ context.Context, q string) ([]string, error) {
		*asked = append(*asked, "system")
		return []string{"1403 | 96.127.192.0/18 | CA | arin | 2011-04-08"}, nil
	}
	n := len(*asked)
	cymruASNLookup(context.Background(), "96.127.240.62")
	if got := (*asked)[n:]; len(got) != 1 || got[0] != "system" {
		t.Errorf("asked %v after the window, want the host resolver first again", got)
	}
}
