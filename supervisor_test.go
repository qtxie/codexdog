package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordedRequest struct {
	Method string
	Params map[string]any
}

type mockProxy struct {
	mu      sync.Mutex
	request func(context.Context, string, map[string]any) (any, error)
	records []recordedRequest
}

func (m *mockProxy) Request(ctx context.Context, method string, params map[string]any) (any, error) {
	m.mu.Lock()
	m.records = append(m.records, recordedRequest{Method: method, Params: params})
	m.mu.Unlock()
	return m.request(ctx, method, params)
}

func (m *mockProxy) methods() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]string, len(m.records))
	for index, record := range m.records {
		result[index] = record.Method
	}
	return result
}

func testSupervisorOptions(cwd string) supervisorOptions {
	return supervisorOptions{CWD: cwd, CodexPath: "codex", ProbeTimeout: 30 * time.Second, TerminalErrorGrace: 100 * time.Millisecond, ProbeSuccesses: 2, Backoff: []time.Duration{time.Second}, MaxAutoResumes: 5, StallTimeout: 100 * time.Millisecond, StallConfirm: 50 * time.Millisecond, StallInterruptTimeout: time.Second, MaxStallResumes: 2}
}

func cyberFailure(threadID, turnID string) rpcMessage {
	return rpcMessage{Method: "turn/completed", Params: map[string]any{"threadId": threadID, "turn": map[string]any{"id": turnID, "status": "failed", "error": map[string]any{"message": "Request blocked by cyber safety policy", "codexErrorInfo": "cyberPolicy"}}}}
}

func TestSupervisorCyberPolicyRecovery(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	turnNumber := 0
	proxy := &mockProxy{}
	proxy.request = func(_ context.Context, method string, _ map[string]any) (any, error) {
		if method == "thread/fork" {
			return map[string]any{"thread": map[string]any{"id": "fork-1"}}, nil
		}
		if method == "turn/start" {
			turnNumber++
			return map[string]any{"turn": map[string]any{"id": fmt.Sprintf("recovery-%d", turnNumber), "status": "inProgress"}}, nil
		}
		return map[string]any{}, nil
	}
	supervisor.proxy = proxy

	for index, failure := range []rpcMessage{cyberFailure("thread-1", "original"), cyberFailure("thread-1", "recovery-1"), cyberFailure("thread-1", "recovery-2")} {
		supervisor.handleServerMessage(failure)
		want := fmt.Sprintf("recovery-%d", index+1)
		waitFor(t, func() bool {
			return supervisor.stateSnapshot().ActiveTurnID == want && !supervisor.isSubmittingResume()
		})
	}
	wantMethods := []string{"thread/resume", "turn/start", "thread/resume", "turn/start", "thread/fork", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(wantMethods) {
		t.Fatalf("requests = %v, want %v", got, wantMethods)
	}
	proxy.mu.Lock()
	prompts := []string{}
	for _, record := range proxy.records {
		if record.Method == "turn/start" {
			input := record.Params["input"].([]any)[0].(map[string]any)
			prompts = append(prompts, input["text"].(string))
		}
	}
	proxy.mu.Unlock()
	if fmt.Sprint(prompts) != fmt.Sprint([]string{"continue", "继续", "continue"}) {
		t.Fatalf("prompts = %v", prompts)
	}
	if state := supervisor.stateSnapshot(); state.CurrentThreadID != "fork-1" {
		t.Fatalf("fork was not selected: %#v", state)
	}
	supervisor.handleServerMessage(cyberFailure("fork-1", "recovery-3"))
	waitFor(t, func() bool {
		persisted, ok := supervisor.store.Read()
		return supervisor.stateSnapshot().Phase == "needs-attention" && !supervisor.isSubmittingResume() && ok && persisted.Phase == "needs-attention"
	})
	if len(proxy.methods()) != 6 || !strings.Contains(supervisor.stateSnapshot().LastError, "exhausted after 3 attempts") {
		t.Fatalf("unexpected exhausted state: %#v", supervisor.stateSnapshot())
	}
}

func TestSupervisorStallRecovery(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{}
	proxy.request = func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "thread/read":
			return map[string]any{"thread": map[string]any{"id": "thread-1", "status": map[string]any{"type": "active", "activeFlags": []any{}}}}, nil
		case "turn/interrupt":
			supervisor.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted"}}})
			return map[string]any{}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}}, nil
		default:
			return map[string]any{}, nil
		}
	}
	supervisor.proxy = proxy
	supervisor.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
	start := supervisor.watchdog.Snapshot().LastActivityAt
	supervisor.evaluateStall(start.Add(100 * time.Millisecond))
	if supervisor.stateSnapshot().Phase != "suspected-stall" {
		t.Fatal("turn was not marked as suspected")
	}
	supervisor.evaluateStall(start.Add(150 * time.Millisecond))
	waitFor(t, func() bool {
		return supervisor.stateSnapshot().ActiveTurnID == "turn-2" && !supervisor.isSubmittingResume() && !supervisor.isStallCheckInFlight()
	})
	want := []string{"thread/read", "turn/interrupt", "thread/resume", "turn/start"}
	if got := proxy.methods(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	state := supervisor.stateSnapshot()
	if state.Phase != "running" || state.StallRecoveryCount != 1 {
		t.Fatalf("unexpected recovered state: %#v", state)
	}
}

func TestSupervisorDoesNotResumeManualInterrupt(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(context.Context, string, map[string]any) (any, error) { return map[string]any{}, nil }}
	supervisor.proxy = proxy
	supervisor.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"}}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted"}}})
	if len(proxy.methods()) != 0 || supervisor.stateSnapshot().Phase != "idle" {
		t.Fatalf("manual interruption was recovered: %v %#v", proxy.methods(), supervisor.stateSnapshot())
	}
}

func TestSupervisorIgnoresAgentManagedThreadLifecycle(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(context.Context, string, map[string]any) (any, error) {
		t.Fatalf("agent-managed thread triggered a request")
		return nil, nil
	}}
	supervisor.proxy = proxy

	supervisor.handleServerMessage(rpcMessage{Method: "thread/started", Params: map[string]any{
		"thread": map[string]any{
			"id":                   "root-thread",
			"canAcceptDirectInput": true,
		},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "root-thread",
		"turn":     map[string]any{"id": "root-turn", "status": "inProgress"},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "thread/started", Params: map[string]any{
		"thread": map[string]any{
			"id":                   "child-thread",
			"parentThreadId":       "root-thread",
			"canAcceptDirectInput": false,
		},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "child-thread",
		"turn":     map[string]any{"id": "child-turn", "status": "inProgress"},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "error", Params: map[string]any{
		"threadId":  "child-thread",
		"turnId":    "child-turn",
		"willRetry": false,
		"error":     map[string]any{"message": "provider unavailable"},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "child-thread",
		"turn":     map[string]any{"id": "child-turn", "status": "failed"},
	}})

	state := supervisor.stateSnapshot()
	if state.CurrentThreadID != "root-thread" || state.ActiveTurnID != "root-turn" || state.Phase != "running" || state.LastFailedTurnID != "" {
		t.Fatalf("agent-managed lifecycle changed root state: %#v", state)
	}
	if got := supervisor.watchdog.Snapshot(); got.ThreadID != "root-thread" || got.TurnID != "root-turn" {
		t.Fatalf("agent-managed lifecycle replaced watchdog turn: %#v", got)
	}
	if got := proxy.methods(); len(got) != 0 {
		t.Fatalf("agent-managed lifecycle issued recovery requests: %v", got)
	}
}

func TestSupervisorSkipsDirectResumeForAgentManagedThread(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(context.Context, string, map[string]any) (any, error) {
		t.Fatalf("agent-managed thread was sent direct input")
		return nil, nil
	}}
	supervisor.proxy = proxy
	supervisor.handleServerMessage(rpcMessage{Method: "thread/started", Params: map[string]any{
		"thread": map[string]any{"id": "child-thread", "parentThreadId": "root-thread"},
	}})
	if err := supervisor.resumeThread(context.Background(), recoveryContext{ThreadID: "child-thread", FailedTurnID: "child-turn"}); err != nil {
		t.Fatalf("resumeThread() error = %v", err)
	}
	if got := proxy.methods(); len(got) != 0 {
		t.Fatalf("guard issued requests: %v", got)
	}
}

func TestSupervisorIdentifiesAgentThreadFromCollaborationItem(t *testing.T) {
	cwd := t.TempDir()
	supervisor := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(context.Context, string, map[string]any) (any, error) {
		t.Fatalf("collaboration child triggered a request")
		return nil, nil
	}}
	supervisor.proxy = proxy
	supervisor.handleServerMessage(rpcMessage{Method: "thread/started", Params: map[string]any{
		"thread": map[string]any{"id": "root-thread", "canAcceptDirectInput": true},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "root-thread",
		"turn":     map[string]any{"id": "root-turn", "status": "inProgress"},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "item/started", Params: map[string]any{
		"threadId": "root-thread",
		"item": map[string]any{
			"type":           "collabToolCall",
			"senderThreadId": "root-thread",
			"newThreadId":    "child-thread",
		},
	}})
	supervisor.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "child-thread",
		"turn":     map[string]any{"id": "child-turn", "status": "failed"},
	}})
	state := supervisor.stateSnapshot()
	if state.CurrentThreadID != "root-thread" || state.ActiveTurnID != "root-turn" || state.Phase != "running" {
		t.Fatalf("collaboration child changed root state: %#v", state)
	}
	if got := proxy.methods(); len(got) != 0 {
		t.Fatalf("collaboration child issued recovery requests: %v", got)
	}
}

func TestThreadDirectInputCapability(t *testing.T) {
	tests := []struct {
		name    string
		thread  map[string]any
		accepts bool
		known   bool
	}{
		{name: "explicit root", thread: map[string]any{"canAcceptDirectInput": true}, accepts: true, known: true},
		{name: "explicit agent", thread: map[string]any{"canAcceptDirectInput": false}, accepts: false, known: true},
		{name: "explicit capability wins", thread: map[string]any{"canAcceptDirectInput": true, "parentThreadId": "root"}, accepts: true, known: true},
		{name: "parent fallback", thread: map[string]any{"parentThreadId": "root"}, accepts: false, known: true},
		{name: "source fallback", thread: map[string]any{"threadSource": "subagent"}, accepts: false, known: true},
		{name: "session source fallback", thread: map[string]any{"source": map[string]any{"subAgent": map[string]any{}}}, accepts: false, known: true},
		{name: "unknown legacy", thread: map[string]any{}, accepts: true, known: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			accepts, known := threadDirectInputCapability(test.thread)
			if accepts != test.accepts || known != test.known {
				t.Fatalf("threadDirectInputCapability() = (%t, %t), want (%t, %t)", accepts, known, test.accepts, test.known)
			}
		})
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func (s *supervisor) isSubmittingResume() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.submittingResume
}

func (s *supervisor) isStallCheckInFlight() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stallCheckInFlight
}
