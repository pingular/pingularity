package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoAnalyticsOrTelemetryDeps guards the module graph: Pingularity ships no
// analytics, telemetry, product-usage, or crash-reporting SDK (see
// docs/security-model.md), and one must not be pulled in - directly or
// transitively - without a deliberate, reviewed change to this denylist. It
// scans go.mod AND go.sum (the full build list), so a transitive pull is caught
// too. Each entry is a module-path fragment specific enough not to match an
// unrelated module.
func TestNoAnalyticsOrTelemetryDeps(t *testing.T) {
	denied := []string{
		"getsentry/sentry-go",
		"bugsnag/bugsnag-go",
		"rollbar/rollbar-go",
		"honeybadger-io/honeybadger-go",
		"datadog/dd-trace-go", "datadog/datadog-go",
		"newrelic/go-agent",
		"go.opentelemetry.io",
		"segmentio/analytics-go",
		"amplitude/analytics-go",
		"posthog/posthog-go",
		"mixpanel/mixpanel-go", "dukex/mixpanel",
		"plausible",
		"matomo",
		"firebase.google.com/go", // Crashlytics
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the module root")
	}
	root := filepath.Dir(thisFile) // this file lives at the module root

	for _, name := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		hay := strings.ToLower(string(b))
		for _, d := range denied {
			if strings.Contains(hay, strings.ToLower(d)) {
				t.Errorf("%s pulls in denied analytics/telemetry/crash SDK %q - "+
					"Pingularity must not depend on one; remove it, or (only for a deliberate, "+
					"reviewed exception) update this denylist", name, d)
			}
		}
	}
}
