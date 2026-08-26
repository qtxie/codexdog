package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRemoteQueueAddUsesIdempotencyKeyAndPersistsIt(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	var request map[string]any
	s.queueRPC = &mockProxy{request: func(_ context.Context, method string, params map[string]any) (any, error) {
		if method != "thread/queue/add" {
			return nil, fmt.Errorf("unexpected method %s", method)
		}
		request = params
		return map[string]any{"queuedSubmission": map[string]any{
			"id": "queue-1", "clientUserMessageId": params["clientUserMessageId"], "input": params["input"],
		}}, nil
	}}
	s.modifyState(func(state *supervisorState) { state.CurrentThreadID = "thread-1" })
	message, err := s.remoteQueue(context.Background(), "add inspect the current diff")
	if err != nil {
		t.Fatal(err)
	}
	if message != "Queued submission queue-1." {
		t.Fatalf("message = %q", message)
	}
	clientID, _ := request["clientUserMessageId"].(string)
	if !strings.HasPrefix(clientID, "codexdog:queue:") || request["threadId"] != "thread-1" {
		t.Fatalf("queue request = %#v", request)
	}
	state := s.stateSnapshot()
	if state.QueueClientMessageIDs["queue-1"] != clientID || state.QueueUpdatedAt == "" {
		t.Fatalf("queue state = %#v", state)
	}
	state.QueueClientMessageIDs["queue-1"] = "changed-outside-supervisor"
	if got := s.stateSnapshot().QueueClientMessageIDs["queue-1"]; got != clientID {
		t.Fatalf("state snapshot exposed the live queue map: got %q, want %q", got, clientID)
	}
	persisted, ok := s.store.Read()
	if !ok || persisted.QueueClientMessageIDs["queue-1"] != clientID {
		t.Fatalf("persisted queue state = %#v", persisted)
	}
}

func TestRemoteQueueStartRecordsReturnedTurn(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	s.queueRPC = &mockProxy{request: func(_ context.Context, method string, params map[string]any) (any, error) {
		if method != "thread/queue/start" || params["threadId"] != "thread-1" {
			return nil, fmt.Errorf("unexpected queue request %s %#v", method, params)
		}
		return map[string]any{"turn": map[string]any{"id": "turn-queued", "status": "inProgress"}}, nil
	}}
	s.modifyState(func(state *supervisorState) { state.CurrentThreadID = "thread-1" })
	message, err := s.remoteQueue(context.Background(), "start queue-1")
	if err != nil {
		t.Fatal(err)
	}
	if message != "Started queued turn turn-queued." || s.stateSnapshot().ActiveTurnID != "turn-queued" {
		t.Fatalf("queue start result=%q state=%#v", message, s.stateSnapshot())
	}
}
