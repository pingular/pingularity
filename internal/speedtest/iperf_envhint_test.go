package speedtest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// EnvHint's contract: the injected matcher's non-empty return is appended to
// every surfaced transfer failure (download/upload/bidir), the original error
// stays intact underneath, and the engine itself never inspects the
// environment - these tests inject a fake matcher just like main.go injects
// the container one.
func TestIperfEnvHintAppendedToTransferErrors(t *testing.T) {
	const hintText = "HINT: the container has its own localhost"
	hint := func(text string) string {
		if strings.Contains(text, "connection refused") {
			return hintText
		}
		return ""
	}
	cases := []struct{ dir, wantPrefix string }{
		{"down", "download: "},
		{"up", "upload: "},
		{"bidir", "bidir: "},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			installFakeIperf(t, func([]string) ([]byte, error) {
				return []byte(`{"error":"unable to connect to server: connection refused"}`), nil
			})
			it := newRunIperf(c.dir, false)
			it.EnvHint = hint
			_, err := it.Run(context.Background())
			if err == nil {
				t.Fatal("Run succeeded, want a refused-connection failure")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, c.wantPrefix) || !strings.HasSuffix(msg, "("+hintText+")") {
				t.Fatalf("err = %q, want %q prefix with the hint appended", msg, c.wantPrefix)
			}
		})
	}
}

// The hint wrap must keep the original error in the chain: the scheduler's
// cancellation laundering (errors.Is) and stage classification both read
// through it.
func TestIperfEnvHintPreservesErrorChain(t *testing.T) {
	sentinel := errors.New("connection refused")
	installFakeIperf(t, func([]string) ([]byte, error) { return nil, sentinel })
	it := newRunIperf("down", false)
	it.EnvHint = func(string) string { return "HINT" }
	_, err := it.Run(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("hint wrap broke the error chain: %v", err)
	}
}

// A nil hook and a hook returning "" both change nothing: the surfaced error
// text is byte-identical to the unhinted engine's.
func TestIperfEnvHintAbsentOrEmptyChangesNothing(t *testing.T) {
	cases := []struct {
		name string
		hook func(string) string
	}{
		{"nil hook", nil},
		{"empty return", func(string) string { return "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installFakeIperf(t, func([]string) ([]byte, error) {
				return []byte(`{"error":"boom failure"}`), nil
			})
			it := newRunIperf("down", false)
			it.EnvHint = c.hook
			_, err := it.Run(context.Background())
			if err == nil || err.Error() != "download: boom failure" {
				t.Fatalf("err = %v, want exactly %q", err, "download: boom failure")
			}
		})
	}
}

// The two failure classes that never reach Run's error return - a partial
// direction on a "both" run and the best-effort UDP pass - surface only as
// warn lines, so the hint must ride those too.
func TestIperfEnvHintOnPartialAndUDPWarns(t *testing.T) {
	var buf bytes.Buffer
	installFakeIperf(t, func(args []string) ([]byte, error) {
		switch {
		case argvHas(args, "--udp"):
			return []byte(`{"error":"udp pass kaboom"}`), nil
		case argvHas(args, "--reverse"):
			return []byte(fakeDownJSON), nil
		default:
			return []byte(`{"error":"upload kaboom"}`), nil
		}
	})
	it := newRunIperf("both", true)
	it.Log = slog.New(slog.NewTextHandler(&buf, nil))
	it.EnvHint = func(text string) string {
		if strings.Contains(text, "kaboom") {
			return "KABOOM-HINT"
		}
		return ""
	}
	res, err := it.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v, want nil (partial kept)", err)
	}
	logs := buf.String()
	if !strings.Contains(logs, "upload kaboom (KABOOM-HINT)") {
		t.Errorf("partial-direction warn lacks the hint:\n%s", logs)
	}
	if !strings.Contains(logs, "udp pass kaboom (KABOOM-HINT)") {
		t.Errorf("udp-pass warn lacks the hint:\n%s", logs)
	}
	if res.PacketLoss != nil || res.UDPDirection != "" {
		t.Errorf("failed UDP pass recorded loss=%v direction=%q, want nil/empty",
			fptr(res.PacketLoss), res.UDPDirection)
	}
}
