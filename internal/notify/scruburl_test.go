package notify

import (
	"errors"
	"net/url"
	"strings"
	"testing"
)

// Webhook URLs are bearer secrets (Discord/Slack embed the token in the path)
// and *url.Error stringifies the full URL. scrubURLErr must drop the URL while
// keeping the op and underlying cause for the log line.
func TestScrubURLErr(t *testing.T) {
	ue := &url.Error{
		Op:  "Post",
		URL: "https://discord.com/api/webhooks/123/SECRET-TOKEN",
		Err: errors.New("connection refused"),
	}
	got := scrubURLErr(ue).Error()
	if strings.Contains(got, "SECRET-TOKEN") || strings.Contains(got, "discord.com") {
		t.Fatalf("scrubbed error leaked the URL/token: %q", got)
	}
	if !strings.Contains(got, "Post") || !strings.Contains(got, "connection refused") {
		t.Fatalf("scrubbed error lost the op/cause: %q", got)
	}
	// A non-url error passes through unchanged.
	if scrubURLErr(errors.New("boom")).Error() != "boom" {
		t.Fatal("plain error must pass through unchanged")
	}
}
