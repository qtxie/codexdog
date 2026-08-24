package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseUsageAndRateLimitResponses(t *testing.T) {
	usageValue := map[string]any{
		"summary": map[string]any{
			"lifetimeTokens":    float64(9_876_543),
			"currentStreakDays": float64(4),
		},
		"threadUsage": map[string]any{
			"threadId":                    "thread-1",
			"estimatedUsageCreditsMicros": float64(1_250_000),
			"estimatedUsageUsdMicros":     float64(123_456),
			"groups": []any{map[string]any{
				"model":                       "gpt-5.6",
				"reasoningEffort":             "high",
				"totalTokens":                 float64(12_345),
				"estimatedUsageCreditsMicros": float64(1_250_000),
			}},
		},
	}
	summary, estimate, ok := parseAccountUsageResponse(usageValue)
	if !ok || summary == nil || summary.LifetimeTokens == nil || *summary.LifetimeTokens != 9_876_543 {
		t.Fatalf("summary = %#v, ok = %t", summary, ok)
	}
	if estimate == nil || estimate.ThreadID != "thread-1" || estimate.EstimatedUsageCreditsMicros != 1_250_000 || estimate.EstimatedUsageUSDMicros == nil || *estimate.EstimatedUsageUSDMicros != 123_456 {
		t.Fatalf("estimate = %#v", estimate)
	}
	if len(estimate.Groups) != 1 || estimate.Groups[0].TotalTokens == nil || *estimate.Groups[0].TotalTokens != 12_345 {
		t.Fatalf("groups = %#v", estimate.Groups)
	}

	rateValue := map[string]any{
		"rateLimits": map[string]any{},
		"rateLimitsByLimitId": map[string]any{
			"codex": map[string]any{
				"limitId":   "codex",
				"limitName": "Codex",
				"planType":  "plus",
				"primary": map[string]any{
					"usedPercent":        float64(25),
					"resetsAt":           float64(1_800_000_000),
					"windowDurationMins": float64(300),
				},
				"credits": map[string]any{"balance": "12.50", "hasCredits": true, "unlimited": false},
			},
		},
		"rateLimitResetCredits": map[string]any{"availableCount": float64(2)},
	}
	rates, resetCredits, ok := parseRateLimitsResponse(rateValue)
	if !ok || len(rates) != 1 || rates[0].LimitID != "codex" || rates[0].Primary == nil || rates[0].Primary.UsedPercent != 25 {
		t.Fatalf("rates = %#v, ok = %t", rates, ok)
	}
	if rates[0].Credits == nil || rates[0].Credits.Balance != "12.50" || resetCredits == nil || *resetCredits != 2 {
		t.Fatalf("credits = %#v, reset = %#v", rates[0].Credits, resetCredits)
	}
}

func TestTokenUsageNotificationAndStatusFormatting(t *testing.T) {
	usage, ok := parseTokenUsageNotification(map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"tokenUsage": map[string]any{
			"last": map[string]any{
				"inputTokens": float64(100), "cachedInputTokens": float64(25), "cacheWriteInputTokens": float64(5),
				"outputTokens": float64(40), "reasoningOutputTokens": float64(10), "totalTokens": float64(150),
			},
			"total": map[string]any{
				"inputTokens": float64(10_000), "cachedInputTokens": float64(2_500), "cacheWriteInputTokens": float64(500),
				"outputTokens": float64(4_000), "reasoningOutputTokens": float64(1_000), "totalTokens": float64(15_000),
			},
			"modelContextWindow": float64(200_000),
		},
	})
	if !ok || usage.Total.TotalTokens != 15_000 || usage.Total.CacheWriteInputTokens != 500 || usage.ModelContextWindow != 200_000 {
		t.Fatalf("usage = %#v, ok = %t", usage, ok)
	}
	usd := int64(123_456)
	resetCredits := int64(2)
	state := supervisorState{
		TokenUsage: &usage,
		UsageEstimate: &threadUsageEstimateState{
			ThreadID: "thread-1", EstimatedUsageCreditsMicros: 1_250_000, EstimatedUsageUSDMicros: &usd,
		},
		AccountUsage: &accountUsageSummaryState{LifetimeTokens: int64TestPointer(9_876_543)},
		RateLimits: []rateLimitState{{
			LimitID: "codex", LimitName: "Codex", PlanType: "plus",
			Primary: &rateLimitWindowState{UsedPercent: 25, WindowDurationMins: int64TestPointer(300), ResetsAt: int64TestPointer(1_800_000_000)},
			Credits: &creditsState{Balance: "12.50", HasCredits: true},
		}},
		RateLimitResetCreditsAvailable: &resetCredits,
		UsageUpdatedAt:                 "2026-08-21T00:00:00Z",
	}
	formatted := strings.Join(usageStatusLines(state), "\n")
	for _, expected := range []string{
		"Tokens: 15,000 total", "Model context window: 200,000 tokens", "Estimated usage: 1.25 credits ($0.123456)",
		"Account lifetime tokens: 9,876,543", "Credit balance: 12.50", "Rate limit Codex (plus): primary 25% used",
		"Rate-limit reset credits: 2 available",
	} {
		if !strings.Contains(formatted, expected) {
			t.Fatalf("formatted status omitted %q:\n%s", expected, formatted)
		}
	}
}

func TestSupervisorRefreshesUsageSnapshot(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(_ context.Context, method string, params map[string]any) (any, error) {
		switch method {
		case "account/usage/read":
			if params["threadId"] != "thread-1" {
				return nil, fmt.Errorf("missing thread ID: %#v", params)
			}
			return map[string]any{
				"summary": map[string]any{"lifetimeTokens": float64(1000)},
				"threadUsage": map[string]any{
					"threadId": "thread-1", "estimatedUsageCreditsMicros": float64(500_000), "groups": []any{},
				},
			}, nil
		case "account/rateLimits/read":
			return map[string]any{
				"rateLimits": map[string]any{
					"limitId": "codex", "primary": map[string]any{"usedPercent": float64(10)},
					"credits": map[string]any{"hasCredits": false, "unlimited": true},
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	}}
	s.usageRPC = proxy
	s.modifyState(func(state *supervisorState) { state.CurrentThreadID = "thread-1" })
	s.refreshUsageSnapshot(context.Background())
	state := s.stateSnapshot()
	if state.UsageEstimate == nil || state.UsageEstimate.EstimatedUsageCreditsMicros != 500_000 || state.AccountUsage == nil {
		t.Fatalf("usage state = %#v", state)
	}
	if len(state.RateLimits) != 1 || state.RateLimits[0].Credits == nil || !state.RateLimits[0].Credits.Unlimited {
		t.Fatalf("rate-limit state = %#v", state.RateLimits)
	}
	if state.UsageUpdatedAt == "" || state.UsageLastError != "" {
		t.Fatalf("usage metadata = %#v", state)
	}
	if got := proxy.methods(); len(got) != 2 {
		t.Fatalf("methods = %v", got)
	}
}

func TestRemoteStatusRefreshesAndFormatsUsage(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	s.usageRPC = &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "account/usage/read":
			return map[string]any{
				"summary": map[string]any{},
				"threadUsage": map[string]any{
					"threadId": "thread-1", "estimatedUsageCreditsMicros": float64(750_000), "estimatedUsageUsdMicros": float64(250_000), "groups": []any{},
				},
			}, nil
		case "account/rateLimits/read":
			return map[string]any{
				"rateLimits": map[string]any{
					"limitId": "codex", "primary": map[string]any{"usedPercent": float64(30)},
					"credits": map[string]any{"balance": "8.00", "hasCredits": true, "unlimited": false},
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	}}
	s.modifyState(func(state *supervisorState) {
		state.Phase = "idle"
		state.CurrentThreadID = "thread-1"
	})
	message, err := s.executeRemoteCommand(context.Background(), remoteCommand{Name: "status"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Estimated usage: 0.75 credits ($0.25)", "Credit balance: 8.00", "Rate limit codex: primary 30% used"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("remote status omitted %q:\n%s", expected, message)
		}
	}
}

func TestUsageRefreshFailurePreservesSnapshot(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	s.usageRPC = &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		return nil, errors.New(method + " unavailable")
	}}
	s.modifyState(func(state *supervisorState) {
		state.Phase = "idle"
		state.CurrentThreadID = "thread-1"
		state.UsageEstimate = &threadUsageEstimateState{ThreadID: "thread-1", EstimatedUsageCreditsMicros: 42}
		state.RateLimits = []rateLimitState{{LimitID: "codex", Primary: &rateLimitWindowState{UsedPercent: 15}}}
	})
	s.refreshUsageSnapshot(context.Background())
	state := s.stateSnapshot()
	if state.Phase != "idle" || state.UsageEstimate == nil || state.UsageEstimate.EstimatedUsageCreditsMicros != 42 || len(state.RateLimits) != 1 {
		t.Fatalf("failed refresh discarded state: %#v", state)
	}
	if !strings.Contains(state.UsageLastError, "account/usage/read unavailable") || !strings.Contains(state.UsageLastError, "account/rateLimits/read unavailable") {
		t.Fatalf("usage error = %q", state.UsageLastError)
	}
}

func TestSelectingNewThreadClearsThreadUsage(t *testing.T) {
	state := supervisorState{
		CurrentThreadID: "thread-1",
		TokenUsage:      &threadTokenUsageState{ThreadID: "thread-1"},
		UsageEstimate:   &threadUsageEstimateState{ThreadID: "thread-1"},
	}
	setCurrentThread(&state, "thread-1")
	if state.TokenUsage == nil || state.UsageEstimate == nil {
		t.Fatal("selecting the same thread cleared usage")
	}
	setCurrentThread(&state, "thread-2")
	if state.CurrentThreadID != "thread-2" || state.TokenUsage != nil || state.UsageEstimate != nil {
		t.Fatalf("new thread retained stale usage: %#v", state)
	}
}

func int64TestPointer(value int64) *int64 { return &value }
