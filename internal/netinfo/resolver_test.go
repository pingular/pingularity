package netinfo

import (
	"reflect"
	"testing"
)

// filterResolvers is the OS-independent post-processing applied to whatever
// rawResolvers returns: drop loopback stubs, drop duplicates, keep order.
func TestFilterResolvers(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"drops loopback v4+v6", []string{"127.0.0.53", "::1"}, []string{}},
		{"keeps public, drops loopback", []string{"127.0.0.53", "1.1.1.1"}, []string{"1.1.1.1"}},
		{"dedup preserves order", []string{"8.8.8.8", "8.8.4.4", "8.8.8.8"}, []string{"8.8.8.8", "8.8.4.4"}},
		{"drops non-ip", []string{"not-an-ip", "9.9.9.9"}, []string{"9.9.9.9"}},
		{"mixed v4/v6 kept", []string{"1.1.1.1", "2606:4700:4700::1111"}, []string{"1.1.1.1", "2606:4700:4700::1111"}},
	}
	for _, c := range cases {
		if got := filterResolvers(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: filterResolvers(%v) = %v, want %v", c.name, c.in, got, c.want)
		}
	}
}
