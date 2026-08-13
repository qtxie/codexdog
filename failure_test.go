package main

import (
	"testing"
	"time"
)

func TestJitteredDelay(t *testing.T) {
	for _, test := range []struct {
		random float64
		want   time.Duration
	}{{0, 800 * time.Millisecond}, {0.5, time.Second}, {1, 1200 * time.Millisecond}} {
		if got := jitteredDelay(time.Second, test.random); got != test.want {
			t.Fatalf("jitteredDelay(%v) = %v, want %v", test.random, got, test.want)
		}
	}
}

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name        string
		error       turnError
		disposition string
		code        string
		status      int
	}{
		{"upstream 5xx", turnError{Message: "upstream unavailable", CodexErrorInfo: map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": float64(503)}}}, "transient", "httpConnectionFailed", 503},
		{"stream disconnect", turnError{Message: "stream ended", CodexErrorInfo: map[string]any{"responseStreamDisconnected": map[string]any{}}}, "transient", "responseStreamDisconnected", 0},
		{"stream disconnect after HTTP 200", turnError{Message: "stream ended", CodexErrorInfo: map[string]any{"responseStreamDisconnected": map[string]any{"httpStatusCode": float64(200)}}}, "transient", "responseStreamDisconnected", 200},
		{"retry limit after HTTP 200", turnError{Message: "response retries exhausted", CodexErrorInfo: map[string]any{"responseTooManyFailedAttempts": map[string]any{"httpStatusCode": float64(200)}}}, "transient", "responseTooManyFailedAttempts", 200},
		{"provider auth", turnError{Message: "unauthorized", CodexErrorInfo: map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": float64(401)}}}, "permanent", "httpConnectionFailed", 401},
		{"string auth", turnError{Message: "login expired", CodexErrorInfo: "unauthorized"}, "permanent", "unauthorized", 0},
		{"overload", turnError{Message: "overloaded", CodexErrorInfo: "serverOverloaded"}, "transient", "serverOverloaded", 0},
		{"timeout message", turnError{Message: "request timed out"}, "transient", "messageMatch", 0},
		{"generic other timeout", turnError{Message: "stream disconnected before completion: idle timeout waiting for SSE", CodexErrorInfo: "other"}, "transient", "messageMatch", 0},
		{"generic object other timeout", turnError{Message: "idle timeout waiting for SSE", CodexErrorInfo: map[string]any{"other": map[string]any{}}}, "transient", "messageMatch", 0},
		{"timeout in additional details", turnError{Message: "request failed", AdditionalDetails: stringPointer("stream disconnected before completion: idle timeout waiting for SSE"), CodexErrorInfo: "other"}, "transient", "messageMatch", 0},
		{"permanent text overrides other timeout", turnError{Message: "unauthorized after connection timeout", CodexErrorInfo: "other"}, "permanent", "other", 0},
		{"unknown", turnError{Message: "unexpected model response"}, "permanent", "unclassified", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyFailure(test.error)
			if got.Disposition != test.disposition || got.Code != test.code || got.HTTPStatus != test.status {
				t.Fatalf("classifyFailure() = %#v", got)
			}
		})
	}
}

func TestClassifyFailureAfterRetries(t *testing.T) {
	if got := classifyFailureAfterRetries(turnError{Message: "request failed after maximum retries", CodexErrorInfo: "other"}, true); got.Disposition != "transient" || got.Code != "retryExhausted" {
		t.Fatalf("retry exhaustion = %#v", got)
	}
	if got := classifyFailureAfterRetries(turnError{Message: "unauthorized after maximum retries", CodexErrorInfo: "unauthorized"}, true); got.Disposition != "permanent" || got.Code != "unauthorized" {
		t.Fatalf("explicit permanent failure = %#v", got)
	}
}

func stringPointer(value string) *string { return &value }

func TestCyberPolicyActions(t *testing.T) {
	wants := []cyberPolicyAction{{"retry-thread", "continue"}, {"retry-thread", "继续"}, {"fork-thread", "continue"}}
	for attempt, want := range wants {
		got, ok := nextCyberPolicyAction(attempt)
		if !ok || got != want {
			t.Fatalf("attempt %d = %#v, %t", attempt, got, ok)
		}
	}
	if _, ok := nextCyberPolicyAction(3); ok {
		t.Fatal("expected the fourth action to be unavailable")
	}
}
