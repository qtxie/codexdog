package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReadMCPServerStates(t *testing.T) {
	servers, ok := readMCPServerStates(map[string]any{"data": []any{
		map[string]any{
			"name": "github", "runtimeStatus": "connected", "authStatus": "oAuth", "pluginId": "github-plugin",
			"tools": map[string]any{"search": map[string]any{}, "issue": map[string]any{}},
		},
		map[string]any{"name": "broken", "runtimeStatus": "authenticationRequired", "authStatus": "notLoggedIn"},
	}})
	if !ok || len(servers) != 2 {
		t.Fatalf("servers=%#v ok=%t", servers, ok)
	}
	if servers[0].Name != "github" || servers[0].RuntimeStatus != "connected" || servers[0].ToolCount != 2 || servers[0].PluginID != "github-plugin" {
		t.Fatalf("first server=%#v", servers[0])
	}
	if servers[1].RuntimeStatus != "authenticationRequired" || servers[1].AuthStatus != "notLoggedIn" {
		t.Fatalf("second server=%#v", servers[1])
	}
}

func TestFormatMCPAndSubagentStatus(t *testing.T) {
	state := supervisorState{
		MCPServers: []mcpServerState{{Name: "github", RuntimeStatus: "connected", AuthStatus: "oAuth", ToolCount: 3}},
		Subagents:  []subagentState{{ThreadID: "child-1", Status: "running", Tool: "spawnAgent", Model: "gpt-5.6-terra", Message: "checking tests"}},
	}
	status := strings.Join(formatMCPStatus(state), "\n") + "\n" + formatSubagentStatus(state)
	for _, want := range []string{"github: connected", "auth oAuth", "3 tools", "child-1: running", "via spawnAgent", "checking tests"} {
		if !strings.Contains(status, want) {
			t.Fatalf("status %q does not contain %q", status, want)
		}
	}
}

func TestRecordSubagentActivity(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	s.threadDirectInput["parent-1"] = true
	s.recordSubagentObservation(rpcMessage{Method: "item/started", Params: map[string]any{
		"threadId": "parent-1",
		"item":     map[string]any{"type": "subAgentActivity", "agentThreadId": "child-1", "kind": "started"},
	}})
	state := s.stateSnapshot()
	if len(state.Subagents) != 1 || state.Subagents[0].Status != "running" || state.Subagents[0].ParentThreadID != "parent-1" {
		t.Fatalf("subagent activity was not recorded: %#v", state.Subagents)
	}
}

func TestFormatTimeline(t *testing.T) {
	message := formatTimeline(map[string]any{"data": []any{
		map[string]any{"type": "turnStarted", "turn_id": "turn-1"},
		map[string]any{"type": "item", "item": map[string]any{"type": "agentMessage", "text": "  hello   from   Codex  "}},
		map[string]any{"type": "turnCompleted", "turn_id": "turn-1", "status": "completed"},
	}}, 20)
	for _, want := range []string{"turn turn-1 started", "agentMessage: hello from Codex", "turn turn-1 completed"} {
		if !strings.Contains(message, want) {
			t.Fatalf("timeline %q does not contain %q", message, want)
		}
	}
}

func TestRemoteRecentFallsBackToThreadHistory(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	s.modifyState(func(state *supervisorState) { state.CurrentThreadID = "thread-1" })
	proxy := &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "thread/timeline/list":
			return nil, fmt.Errorf("method not found")
		case "thread/turns/list":
			return map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed"}}}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	}}
	s.proxy = proxy
	message, err := s.remoteRecent(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "Timeline unavailable") || !strings.Contains(message, "turn turn-1 completed") {
		t.Fatalf("fallback message=%q", message)
	}
}

func TestWatchdogTracksInterruptHook(t *testing.T) {
	watchdog := testWatchdog()
	start := testTime()
	watchdog.StartTurn("thread-1", "turn-1", start)
	watchdog.Observe(watchdogEvent("hook/started", map[string]any{
		"run": map[string]any{"id": "interrupt-1", "eventName": "interrupt", "executionMode": "sync"},
	}), start.Add(time.Millisecond))
	if !watchdog.InterruptHookActive("thread-1", "turn-1") || watchdog.Snapshot().PauseReason != "activeTool:hook" {
		t.Fatalf("interrupt hook was not tracked: %#v", watchdog.Snapshot())
	}
	watchdog.Observe(watchdogEvent("hook/completed", map[string]any{
		"run": map[string]any{"id": "interrupt-1", "eventName": "interrupt", "executionMode": "sync"},
	}), start.Add(2*time.Millisecond))
	if watchdog.InterruptHookActive("thread-1", "turn-1") {
		t.Fatal("completed interrupt hook remained active")
	}
}

func testTime() time.Time { return time.Unix(0, 0) }
