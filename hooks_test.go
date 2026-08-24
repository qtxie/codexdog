package main

import (
	"strings"
	"testing"
	"time"
)

func TestHookFailureNotifiesTelegram(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	telegram := &telegramController{
		chatIDs: []int64{42},
		notify:  true,
		out:     make(chan telegramOutbound, 2),
	}
	s.telegram = telegram
	s.handleServerMessage(rpcMessage{Method: "hook/completed", Params: map[string]any{
		"threadId": "thread-1",
		"turnId":   "turn-1",
		"run": map[string]any{
			"id": "hook-1", "eventName": "preToolUse", "handlerType": "command", "executionMode": "async", "status": "failed",
			"entries": []any{map[string]any{"kind": "error", "text": "command exited with code 1"}},
		},
	}})
	select {
	case message := <-telegram.out:
		if message.ChatID != 42 || !strings.Contains(message.Text, "Hook failed: preToolUse (command, async)") || !strings.Contains(message.Text, "command exited with code 1") {
			t.Fatalf("notification = %#v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("hook failure did not enqueue a Telegram notification")
	}

	s.handleServerMessage(rpcMessage{Method: "hook/completed", Params: map[string]any{
		"threadId": "thread-1",
		"run":      map[string]any{"id": "hook-2", "eventName": "postToolUse", "handlerType": "command", "executionMode": "sync", "status": "completed"},
	}})
	select {
	case message := <-telegram.out:
		t.Fatalf("successful hook generated a notification: %#v", message)
	default:
	}
}

func TestBlockedHookMessageUsesStatusMessage(t *testing.T) {
	message, ok := hookFailureMessage(map[string]any{
		"threadId": "thread-1",
		"run": map[string]any{
			"id": "hook-1", "eventName": "permissionRequest", "handlerType": "mcpTool", "executionMode": "sync", "status": "blocked",
			"statusMessage": "approval denied",
		},
	})
	if !ok || !strings.Contains(message, "Hook blocked: permissionRequest (mcpTool, sync)") || !strings.Contains(message, "Details: approval denied") {
		t.Fatalf("message = %q, ok = %t", message, ok)
	}
}
