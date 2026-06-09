//go:build linux

package netinfo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReadResolvConf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	body := "# comment\n; also comment\n" +
		"nameserver 9.9.9.9\nnameserver 9.9.9.9\n" + // dup -> once
		"nameserver 127.0.0.53\n" +
		"nameserver 2001:db8::63%eth0\n" + // %zone stripped
		"nameserver [2001:db8::1]\n" + // brackets stripped
		"search example.com\noptions edns0\nnameserver not-an-ip\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readResolvConf(path)
	want := []string{"9.9.9.9", "127.0.0.53", "2001:db8::63", "2001:db8::1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readResolvConf = %v, want %v", got, want)
	}
	if readResolvConf(filepath.Join(dir, "missing")) != nil {
		t.Error("missing file should return nil")
	}
}

func TestAllLoopback(t *testing.T) {
	if allLoopback(nil) {
		t.Error("empty -> false")
	}
	if !allLoopback([]string{"127.0.0.53", "::1"}) {
		t.Error("all loopback -> true")
	}
	if allLoopback([]string{"127.0.0.53", "9.9.9.9"}) {
		t.Error("mixed -> false")
	}
}
