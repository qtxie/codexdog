package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type mockRPC struct {
	mu           sync.Mutex
	handler      notificationHandler
	status       string
	failure      *turnError
	turnStarts   int
	threadStarts int
}

func (m *mockRPC) AddNotificationHandler(handler notificationHandler) func() {
	m.mu.Lock()
	m.handler = handler
	m.mu.Unlock()
	return func() { m.mu.Lock(); m.handler = nil; m.mu.Unlock() }
}

func (m *mockRPC) Request(_ context.Context, method string, _ map[string]any) (any, error) {
	switch method {
	case "thread/start":
		m.mu.Lock()
		m.threadStarts++
		m.mu.Unlock()
		return map[string]any{"thread": map[string]any{"id": "health-thread"}}, nil
	case "turn/start":
		m.mu.Lock()
		m.turnStarts++
		turnID := fmt.Sprintf("health-turn-%d", m.turnStarts)
		status := m.status
		handler := m.handler
		m.mu.Unlock()
		if status == "" {
			status = "completed"
		}
		go func() {
			turnValue := map[string]any{"id": turnID, "status": status}
			if status == "failed" {
				failure := m.failure
				if failure == nil {
					failure = &turnError{Message: "provider unavailable", CodexErrorInfo: map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": float64(503)}}}
				}
				turnValue["error"] = map[string]any{"message": failure.Message, "codexErrorInfo": failure.CodexErrorInfo}
			}
			if handler != nil {
				handler(rpcMessage{Method: "turn/completed", Params: map[string]any{"threadId": "health-thread", "turn": turnValue}})
			}
		}()
		return map[string]any{"turn": map[string]any{"id": turnID, "status": "inProgress"}}, nil
	default:
		return map[string]any{}, nil
	}
}

func TestProviderProbe(t *testing.T) {
	rpc := &mockRPC{}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second})
	defer probe.Dispose()
	if result := probe.Check(context.Background()); !result.Healthy {
		t.Fatalf("healthy canary failed: %#v", result)
	}
	if result := probe.Check(context.Background()); !result.Healthy {
		t.Fatalf("second healthy canary failed: %#v", result)
	}
	rpc.mu.Lock()
	threadStarts := rpc.threadStarts
	rpc.mu.Unlock()
	if threadStarts != 1 {
		t.Fatalf("health thread started %d times", threadStarts)
	}
}

func TestProviderProbeClassifiesFailure(t *testing.T) {
	rpc := &mockRPC{status: "failed"}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second})
	defer probe.Dispose()
	result := probe.Check(context.Background())
	if result.Healthy || result.Failure == nil || result.Failure.Disposition != "transient" || result.Failure.HTTPStatus != 503 {
		t.Fatalf("unexpected probe result: %#v", result)
	}
}

func TestProviderProbeTreatsHTTP200StreamDisconnectAsTransient(t *testing.T) {
	rpc := &mockRPC{status: "failed", failure: &turnError{
		Message: "stream disconnected before completion",
		CodexErrorInfo: map[string]any{
			"responseTooManyFailedAttempts": map[string]any{"httpStatusCode": float64(200)},
		},
	}}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second})
	defer probe.Dispose()
	result := probe.Check(context.Background())
	if result.Healthy || result.Failure == nil || result.Failure.Disposition != "transient" || result.Failure.Code != "responseTooManyFailedAttempts" || result.Failure.HTTPStatus != 200 {
		t.Fatalf("unexpected probe result: %#v", result)
	}
}
