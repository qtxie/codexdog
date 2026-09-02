package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLiveConfiguredHealthSources(t *testing.T) {
	if os.Getenv("CODEXDOG_LIVE_HEALTH_TEST") != "1" {
		t.Skip("set CODEXDOG_LIVE_HEALTH_TEST=1 to query public status pages")
	}
	tests := []struct {
		name   string
		config healthSourceConfig
		model  string
	}{
		{"ciii", healthSourceConfig{Type: healthSourceUptimeKuma, URL: "https://status.ciii.club/status/codex"}, "gpt-5.6-sol"},
		{"input-im", healthSourceConfig{Type: healthSourceInputIM, URL: "https://status.input.im/"}, "gpt-5.6-sol"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{test.config}, MaxAge: 10 * time.Minute})
			result := checker.check(context.Background(), test.model, "", 20*time.Second)
			if len(result.Observations) != 1 {
				t.Fatalf("result = %#v", result)
			}
			t.Logf("state=%s detail=%s observed=%s", result.State, result.Observations[0].Detail, result.Observations[0].ObservedAt.Format(time.RFC3339))
			if result.State == healthStateUnknown {
				t.Fatalf("live source could not be parsed: %#v", result)
			}
		})
	}
}
