package notify

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// Configured is the answer Send's error cannot give: whether there is a webhook
// to deliver to at all. It must track the live URL (settings change at runtime),
// treat whitespace-only and a nil URLFn as unset - and Send must GO ON returning
// nil for a blank URL, because Outage/SpeedThreshold fire and forget and
// sendRetrying would burn its full backoff on an install that simply has no
// webhook.
func TestConfiguredTracksURLAndSendStaysNoOpWhenBlank(t *testing.T) {
	var url string
	n := New(func() string { return url }, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if n.Configured() {
		t.Error("blank URL reported as configured")
	}
	url = "   \t "
	if n.Configured() {
		t.Error("whitespace-only URL reported as configured")
	}
	if err := n.Send(context.Background(), "hi", map[string]any{"event": "link_down"}); err != nil {
		t.Errorf("Send with a blank URL = %v, want nil (a deliberate no-op)", err)
	}
	url = "https://example.invalid/hook"
	if !n.Configured() {
		t.Error("set URL reported as unconfigured")
	}
	if (&Notifier{}).Configured() {
		t.Error("nil URLFn reported as configured")
	}
}
