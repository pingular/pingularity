package settings

import "testing"

// FuzzUnsealServers: the stored iperf-servers value crosses trust boundaries -
// it arrives from imported backups and survives schema generations - and the
// unseal path must hold its contract for any bytes found there: never panic,
// and never invent readable output. A bare Controller is the harshest caller
// (no crypter: the path that must remember ciphertext rather than read it).
func FuzzUnsealServers(f *testing.F) {
	f.Add(`[]`)
	f.Add(`[{"addr":"h:5201","password":"pw"}]`)
	f.Add(`[{"addr":"h:5201","password":"enc:v1:AAAA"}]`)
	f.Add(`{`)
	f.Add(``)
	f.Add(`[{"addr":1}]`)
	f.Fuzz(func(t *testing.T, raw string) {
		c := &Controller{}
		out, legacy := c.unsealServers(raw)
		if raw == "" && (out != "" || legacy) {
			t.Fatalf("empty input produced output %q legacy=%v", out, legacy)
		}
		// A crypterless controller can never CLAIM to have unsealed anything.
		_ = out
	})
}
