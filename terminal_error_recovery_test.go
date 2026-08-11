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
	want := []string{"thread/read", "turn/interrupt", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	state := s.stateSnapshot()
	if state.Phase != "running" || state.AutomaticResumeCount != 1 || state.TerminalErrorSuspectedAt != "" {
		t.Fatalf("unexpected recovered state: %#v", state)
	}
}

func TestTerminalTransientErrorRecoversIdleThreadWithoutInterrupt(t *testing.T) {
	s, proxy := newTerminalErrorHarness(t, "idle")
	s.handleServerMessage(transientErrorNotification(false))
	waitFor(t, func() bool {
		return s.stateSnapshot().ActiveTurnID == "turn-2" && !s.isSubmittingResume()
	})
	want := []string{"thread/read", "thread/resume", "turn/start"}
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
	want := []string{"thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
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
	want := []string{"thread/read", "turn/interrupt", "thread/read", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}
