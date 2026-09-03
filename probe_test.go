package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	turnModel    string
	threadModel  string
}

func (m *mockRPC) AddNotificationHandler(handler notificationHandler) func() {
	m.mu.Lock()
	m.handler = handler
	m.mu.Unlock()
	return func() { m.mu.Lock(); m.handler = nil; m.mu.Unlock() }
}

func (m *mockRPC) Request(_ context.Context, method string, params map[string]any) (any, error) {
	switch method {
	case "thread/start":
		m.mu.Lock()
		m.threadStarts++
		m.threadModel, _ = readString(params["model"])
		m.mu.Unlock()
		return map[string]any{"thread": map[string]any{"id": "health-thread"}}, nil
	case "turn/start":
		m.mu.Lock()
		m.turnStarts++
		m.turnModel, _ = readString(params["model"])
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
	if result := probe.Check(context.Background(), "", ""); !result.Healthy {
		t.Fatalf("healthy canary failed: %#v", result)
	}
	if result := probe.Check(context.Background(), "", ""); !result.Healthy {
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
	result := probe.Check(context.Background(), "", "")
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
	result := probe.Check(context.Background(), "", "")
	if result.Healthy || result.Failure == nil || result.Failure.Disposition != "transient" || result.Failure.Code != "responseTooManyFailedAttempts" || result.Failure.HTTPStatus != 200 {
		t.Fatalf("unexpected probe result: %#v", result)
	}
}

func TestDeferredStatusCheckPreservesProbeSuccesses(t *testing.T) {
	if got := nextConsecutiveProbeSuccesses(1, probeResult{}); got != 1 {
		t.Fatalf("deferred status check changed successes to %d", got)
	}
	if got := nextConsecutiveProbeSuccesses(1, probeResult{ProbeAttempted: true}); got != 0 {
		t.Fatalf("failed probe left successes at %d", got)
	}
	if got := nextConsecutiveProbeSuccesses(1, probeResult{Healthy: true, ProbeAttempted: true}); got != 2 {
		t.Fatalf("successful probe changed successes to %d", got)
	}
}

func TestRecoveryRetriesAfterHealthEndpointHTTP404(t *testing.T) {
	var mu sync.Mutex
	checks := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		checks++
		current := checks
		mu.Unlock()
		if current == 1 {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	cwd := t.TempDir()
	options := testSupervisorOptions(cwd)
	options.Backoff = []time.Duration{time.Millisecond}
	options.ProbeSuccesses = 1
	options.ProbeTimeout = time.Second
	s := newSupervisor(options, newStateStore(t.TempDir(), cwd))
	rpc := &mockRPC{}
	s.probe = newProviderProbe(rpc, providerProbeOptions{CWD: cwd, Timeout: time.Second, HealthURL: server.URL})
	defer s.probe.Dispose()
	s.rpc = &jsonRPCClient{}
	proxy := &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		if method == "thread/goal/get" {
			return map[string]any{"goal": nil}, nil
		}
		if method == "turn/start" {
			return map[string]any{"turn": map[string]any{"id": "resumed-turn", "status": "inProgress"}}, nil
		}
		return map[string]any{}, nil
	}}
	s.proxy = proxy

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.runRecovery(ctx, recoveryContext{
		ThreadID:     "thread-1",
		FailedTurnID: "failed-turn",
		Failure:      classifyFailure(turnError{Message: "provider returned HTTP 404"}),
	})
	if err != nil {
		t.Fatalf("runRecovery() error = %v", err)
	}
	mu.Lock()
	gotChecks := checks
	mu.Unlock()
	if gotChecks < 2 {
		t.Fatalf("health endpoint checked %d times, want at least 2", gotChecks)
	}
	want := []string{"thread/goal/get", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}
