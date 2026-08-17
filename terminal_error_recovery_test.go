package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func transientErrorNotification(willRetry bool) rpcMessage {
	return rpcMessage{Method: "error", Params: map[string]any{
		"threadId":  "thread-1",
		"turnId":    "turn-1",
		"willRetry": willRetry,
		"error": map[string]any{
			"message":        "stream disconnected before completion: idle timeout waiting for SSE",
			"codexErrorInfo": "other",
		},
	}}
}

func httpErrorNotification(status int, willRetry bool) rpcMessage {
	return rpcMessage{Method: "error", Params: map[string]any{
		"threadId":  "thread-1",
		"turnId":    "turn-1",
		"willRetry": willRetry,
		"error": map[string]any{
			"message": fmt.Sprintf("provider returned HTTP %d", status),
			"codexErrorInfo": map[string]any{
				"httpConnectionFailed": map[string]any{"httpStatusCode": float64(status)},
			},
		},
	}}
}

func newTerminalErrorHarness(t *testing.T, threadStatus string) (*supervisor, *mockProxy) {
	return newTerminalErrorHarnessWithFlags(t, threadStatus, nil)
}

func newTerminalErrorHarnessWithFlags(t *testing.T, threadStatus string, activeFlags []any) (*supervisor, *mockProxy) {
	t.Helper()
	cwd := t.TempDir()
	options := testSupervisorOptions(cwd)
	options.ProbeSuccesses = 1
	options.Backoff = []time.Duration{time.Millisecond}
	options.StallInterruptTimeout = 500 * time.Millisecond
	s := newSupervisor(options, newStateStore(t.TempDir(), cwd))
	rpc := &mockRPC{}
	s.probe = newProviderProbe(rpc, providerProbeOptions{CWD: cwd, Timeout: time.Second})
	s.rpc = &jsonRPCClient{}
	proxy := &mockProxy{}
	proxy.request = func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": threadStatus, "activeFlags": activeFlags}}}, nil
		case "thread/goal/get":
			return map[string]any{"goal": nil}, nil
		case "turn/interrupt":
			s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
				"threadId": "thread-1",
				"turn":     map[string]any{"id": "turn-1", "status": "interrupted"},
			}})
			return map[string]any{}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	}
	s.proxy = proxy
	s.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "thread-1",
		"turn":     map[string]any{"id": "turn-1", "status": "inProgress"},
	}})
	t.Cleanup(func() { s.shutdown("test complete") })
	return s, proxy
}

func TestTerminalTransientErrorInterruptsAndRecoversWithoutCompletedEvent(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.handleServerMessage(transientErrorNotification(false))
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/read", "turn/interrupt", "thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	state := s.stateSnapshot()
	if state.Phase != "running" || state.AutomaticResumeCount != 1 || state.TerminalErrorSuspectedAt != "" {
		t.Fatalf("unexpected recovered state: %#v", state)
	}
}

func TestTerminalHTTP404InterruptsAndRecoversWithoutCompletedEvent(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.handleServerMessage(httpErrorNotification(404, false))
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/read", "turn/interrupt", "thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestTerminalTransientErrorRecoversIdleThreadWithoutInterrupt(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "idle")
	s.handleServerMessage(transientErrorNotification(false))
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/read", "thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	// A delayed terminal notification for the synthesized failure must not disturb the resumed turn.
	s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "thread-1",
		"turn":     map[string]any{"id": "turn-1", "status": "failed"},
	}})
	if state := s.stateSnapshot(); state.ActiveTurnID != "turn-2" || state.Phase != "running" {
		t.Fatalf("late terminal event changed resumed state: %#v", state)
	}
}

func TestRetryableErrorDoesNotStartFallbackRecovery(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.handleServerMessage(transientErrorNotification(true))
	time.Sleep(3 * s.options.TerminalErrorGrace)
	if got := proxy.methods(); len(got) != 0 {
		t.Fatalf("retryable error triggered fallback requests: %v", got)
	}
}

func TestCompletedEventDuringGraceUsesNormalRecoveryPath(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.handleServerMessage(transientErrorNotification(false))
	s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "thread-1",
		"turn": map[string]any{
			"id":     "turn-1",
			"status": "failed",
		},
	}})
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestGoalRecoveryReactivatesGoalWithoutTurnPrompt(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	var goalSetParams map[string]any
	proxy.request = func(_ context.Context, method string, params map[string]any) (any, error) {
		switch method {
		case "thread/goal/get":
			return map[string]any{"goal": map[string]any{
				"objective": "Finish the migration and keep tests green",
				"status":    "blocked",
			}}, nil
		case "thread/goal/set":
			goalSetParams = params
			s.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
				"threadId": "thread-1",
				"turn":     map[string]any{"id": "goal-turn", "status": "inProgress"},
			}})
			return map[string]any{"goal": map[string]any{
				"objective": "Finish the migration and keep tests green",
				"status":    "active",
			}}, nil
		default:
			return map[string]any{}, nil
		}
	}
	s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "thread-1",
		"turn": map[string]any{
			"id":     "turn-1",
			"status": "failed",
			"error": map[string]any{
				"message":        "provider unavailable",
				"codexErrorInfo": "other",
			},
		},
	}})
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "goal-turn" && !s.isSubmittingResume()
	})
	want := []string{"thread/goal/get", "thread/goal/set"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if len(goalSetParams) != 2 || goalSetParams["threadId"] != "thread-1" || goalSetParams["status"] != "active" {
		t.Fatalf("goal resume params = %#v", goalSetParams)
	}
}

func TestUserInterruptCancelsPendingErrorRecovery(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.handleServerMessage(transientErrorNotification(false))
	s.handleClientMessage(rpcMessage{Method: "turn/interrupt", Params: map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
	}})
	time.Sleep(3 * s.options.TerminalErrorGrace)
	if got := proxy.methods(); len(got) != 0 {
		t.Fatalf("user interrupt did not cancel fallback recovery: %v", got)
	}
	if state := s.stateSnapshot(); state.TerminalErrorSuspectedAt != "" {
		t.Fatalf("pending error state was not cleared: %#v", state)
	}
}

func TestTerminalTransientErrorDoesNotInterruptWaitingThread(t *testing.T) {
	s, proxy := newTerminalErrorHarnessWithFlags(t, "active", []any{"waitingOnApproval"})
	s.handleServerMessage(transientErrorNotification(false))
	waitFor(t, func() bool {
		return s.stateSnapshot().Phase == "waiting-for-user"
	})
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint([]string{"thread/read"}) {
		t.Fatalf("waiting thread requests = %v", got)
	}
	if state := s.stateSnapshot(); state.TerminalErrorSuspectedAt != "" {
		t.Fatalf("pending error state was not cleared: %#v", state)
	}
}

func TestTerminalTransientErrorRecoversWhenInterruptTerminalEventIsMissing(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	s.options.StallInterruptTimeout = 20 * time.Millisecond
	reads := 0
	proxy.request = func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "thread/read":
			reads++
			status := "active"
			if reads > 1 {
				status = "idle"
			}
			return map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": status, "activeFlags": []any{}}}}, nil
		case "turn/interrupt", "thread/resume":
			return map[string]any{}, nil
		case "thread/goal/get":
			return map[string]any{"goal": nil}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	}
	s.handleServerMessage(transientErrorNotification(false))
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/read", "turn/interrupt", "thread/read", "thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

func TestRetryExhaustionStartsProviderRecovery(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "active")
	for attempt := 1; attempt <= 5; attempt++ {
		s.handleServerMessage(rpcMessage{Method: "error", Params: map[string]any{
			"threadId": "thread-1", "turnId": "turn-1", "willRetry": true,
			"error": map[string]any{
				"message": fmt.Sprintf("Reconnecting... %d/5", attempt),
				"codexErrorInfo": map[string]any{
					"responseStreamDisconnected": map[string]any{"httpStatusCode": float64(200)},
				},
			},
		}})
	}
	finalError := map[string]any{"message": "request failed after maximum retries", "codexErrorInfo": "other"}
	s.handleServerMessage(rpcMessage{Method: "error", Params: map[string]any{
		"threadId": "thread-1", "turnId": "turn-1", "willRetry": false, "error": finalError,
	}})
	s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "thread-1",
		"turn":     map[string]any{"id": "turn-1", "status": "failed", "error": finalError},
	}})
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}
