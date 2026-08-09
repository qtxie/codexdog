package main

import (
	"testing"
	"time"
)

func watchdogEvent(method string, extra map[string]any) rpcMessage {
	params := map[string]any{"threadId": "thread-1", "turnId": "turn-1"}
	for key, value := range extra {
		params[key] = value
	}
	return rpcMessage{Method: method, Params: params}
}

func testWatchdog() *stallWatchdog {
	return newStallWatchdog(stallWatchdogOptions{StallTimeout: 100 * time.Millisecond, ConfirmTimeout: 50 * time.Millisecond})
}

func TestWatchdogRequiresConfirmation(t *testing.T) {
	watchdog := testWatchdog()
	start := time.Unix(0, 0)
	watchdog.StartTurn("thread-1", "turn-1", start)
	if _, ok := watchdog.Evaluate(start.Add(99 * time.Millisecond)); ok {
		t.Fatal("stall was suspected too early")
	}
	decision, ok := watchdog.Evaluate(start.Add(100 * time.Millisecond))
	if !ok || decision.Kind != "suspected" || !decision.ConfirmAt.Equal(start.Add(150*time.Millisecond)) {
		t.Fatalf("unexpected suspicion: %#v", decision)
	}
	if _, ok := watchdog.Evaluate(start.Add(149 * time.Millisecond)); ok {
		t.Fatal("stall was confirmed too early")
	}
	decision, ok = watchdog.Evaluate(start.Add(150 * time.Millisecond))
	if !ok || decision.Kind != "confirmed" {
		t.Fatalf("unexpected confirmation: %#v", decision)
	}
}

func TestWatchdogActivityAndPauses(t *testing.T) {
	watchdog := testWatchdog()
	start := time.Unix(0, 0)
	watchdog.StartTurn("thread-1", "turn-1", start)
	_, _ = watchdog.Evaluate(start.Add(100 * time.Millisecond))
	observation := watchdog.Observe(watchdogEvent("item/agentMessage/delta", nil), start.Add(120*time.Millisecond))
	if !observation.Activity || !observation.SuspicionCleared {
		t.Fatalf("activity did not clear suspicion: %#v", observation)
	}
	if _, ok := watchdog.Evaluate(start.Add(219 * time.Millisecond)); ok {
		t.Fatal("activity did not reset idle time")
	}

	watchdog.Observe(watchdogEvent("thread/status/changed", map[string]any{"status": map[string]any{"type": "active", "activeFlags": []any{"waitingOnApproval"}}}), start.Add(time.Second))
	if _, ok := watchdog.Evaluate(start.Add(10 * time.Second)); ok || watchdog.Snapshot().PauseReason != "waitingOnApproval" {
		t.Fatal("watchdog did not pause for approval")
	}
	watchdog.Observe(watchdogEvent("thread/status/changed", map[string]any{"status": map[string]any{"type": "active", "activeFlags": []any{}}}), start.Add(10*time.Second))
	watchdog.Observe(watchdogEvent("item/started", map[string]any{"item": map[string]any{"id": "command-1", "type": "commandExecution"}}), start.Add(10*time.Second))
	if _, ok := watchdog.Evaluate(start.Add(20 * time.Second)); ok || watchdog.Snapshot().PauseReason != "activeTool:commandExecution" {
		t.Fatal("watchdog did not pause for an active command")
	}
}

func TestWatchdogIgnoresOtherTurn(t *testing.T) {
	watchdog := testWatchdog()
	start := time.Unix(0, 0)
	watchdog.StartTurn("thread-1", "turn-1", start)
	message := rpcMessage{Method: "item/agentMessage/delta", Params: map[string]any{"threadId": "thread-1", "turnId": "turn-2"}}
	if watchdog.Observe(message, start.Add(90*time.Millisecond)).Activity {
		t.Fatal("activity from another turn was accepted")
	}
	if decision, ok := watchdog.Evaluate(start.Add(100 * time.Millisecond)); !ok || decision.Kind != "suspected" {
		t.Fatal("expected the original turn to be suspected")
	}
}
