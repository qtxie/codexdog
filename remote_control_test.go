package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestRemotePromptSteersActiveTurn(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		if method != "turn/steer" {
			return nil, fmt.Errorf("unexpected method %s", method)
		}
		return map[string]any{}, nil
	}}
	s.proxy = proxy
	s.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"},
	}})
	message, err := s.executeRemoteCommand(context.Background(), remoteCommand{Name: "prompt", Text: "inspect the failing test"})
	if err != nil {
		t.Fatal(err)
	}
	if message != "Prompt sent to the active turn." {
		t.Fatalf("message = %q", message)
	}
	if got := proxy.methods(); fmt.Sprint(got) != "[turn/steer]" {
		t.Fatalf("methods = %v", got)
	}
	proxy.mu.Lock()
	params := proxy.records[0].Params
	proxy.mu.Unlock()
	if params["threadId"] != "thread-1" || params["expectedTurnId"] != "turn-1" {
		t.Fatalf("steer params = %#v", params)
	}
}

func TestRemotePauseSuppressesRecoveryAndResumeStartsTurn(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{}
	proxy.request = func(_ context.Context, method string, params map[string]any) (any, error) {
		switch method {
		case "turn/interrupt":
			if params["turnId"] != "turn-1" {
				return nil, fmt.Errorf("wrong interrupt params: %#v", params)
			}
			go s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
				"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "interrupted"},
			}})
		case "thread/goal/get":
			return map[string]any{}, nil
		case "thread/resume":
			return map[string]any{}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "turn-2", "status": "inProgress"}}, nil
		}
		return map[string]any{}, nil
	}
	s.proxy = proxy
	s.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "inProgress"},
	}})
	if _, err := s.executeRemoteCommand(context.Background(), remoteCommand{Name: "pause"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return s.stateSnapshot().Phase == "paused" && s.stateSnapshot().ActiveTurnID == "" })
	if !s.stateSnapshot().ManualPaused {
		t.Fatal("manual pause flag was not set")
	}
	// A terminal failure arriving after the pause must not start provider recovery.
	s.handleServerMessage(rpcMessage{Method: "turn/completed", Params: map[string]any{
		"threadId": "thread-1", "turn": map[string]any{"id": "late-turn", "status": "failed", "error": map[string]any{"message": "provider down"}},
	}})
	if got := proxy.methods(); fmt.Sprint(got) != "[turn/interrupt]" {
		t.Fatalf("paused recovery issued requests: %v", got)
	}
	message, err := s.executeRemoteCommand(context.Background(), remoteCommand{Name: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if message != "Resume requested." || s.stateSnapshot().ActiveTurnID != "turn-2" || s.stateSnapshot().ManualPaused {
		t.Fatalf("resume result=%q state=%#v", message, s.stateSnapshot())
	}
}

func TestRemoteResumeReactivatesGoalWithoutPrompt(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		switch method {
		case "thread/goal/get":
			return map[string]any{"goal": map[string]any{"objective": "finish the migration", "status": "paused"}}, nil
		case "thread/goal/set":
			return map[string]any{"goal": map[string]any{"objective": "finish the migration", "status": "active"}}, nil
		default:
			return map[string]any{}, nil
		}
	}}
	s.proxy = proxy
	s.modifyState(func(state *supervisorState) {
		state.CurrentThreadID = "thread-1"
		state.ManualPaused = true
		state.Phase = "paused"
	})
	message, err := s.executeRemoteCommand(context.Background(), remoteCommand{Name: "resume"})
	if err != nil {
		t.Fatal(err)
	}
	if message != "Resume requested." || fmt.Sprint(proxy.methods()) != "[thread/goal/get thread/goal/set]" {
		t.Fatalf("message=%q methods=%v", message, proxy.methods())
	}
	state := s.stateSnapshot()
	if state.ManualPaused || state.Phase != "running" || state.ActiveTurnID != "" {
		t.Fatalf("unexpected state: %#v", state)
	}
}

func TestStartContinuationPreservesThreadSettings(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(_ context.Context, method string, params map[string]any) (any, error) {
		if _, exists := params["cwd"]; exists {
			return nil, fmt.Errorf("%s unexpectedly overrides cwd: %#v", method, params)
		}
		switch method {
		case "thread/resume":
			return map[string]any{}, nil
		case "turn/start":
			return map[string]any{"turn": map[string]any{"id": "continued-turn", "status": "inProgress"}}, nil
		default:
			return nil, fmt.Errorf("unexpected method %s", method)
		}
	}}
	s.proxy = proxy
	s.modifyState(func(state *supervisorState) { state.EffectiveCWD = `D:\changed\directory` })
	if err := s.startContinuation(context.Background(), "thread-1", "continue", false, "test"); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(proxy.methods()); got != "[thread/resume turn/start]" {
		t.Fatalf("methods = %s", got)
	}
}

func TestTurnStartedWhilePausedIsInterrupted(t *testing.T) {
	cwd := t.TempDir()
	s := newSupervisor(testSupervisorOptions(cwd), newStateStore(t.TempDir(), cwd))
	proxy := &mockProxy{request: func(_ context.Context, method string, _ map[string]any) (any, error) {
		if method != "turn/interrupt" {
			return nil, fmt.Errorf("unexpected method %s", method)
		}
		return map[string]any{}, nil
	}}
	s.proxy = proxy
	s.modifyState(func(state *supervisorState) {
		state.ManualPaused = true
		state.Phase = "paused"
		state.CurrentThreadID = "thread-1"
	})
	s.handleServerMessage(rpcMessage{Method: "turn/started", Params: map[string]any{
		"threadId": "thread-1", "turn": map[string]any{"id": "unexpected", "status": "inProgress"},
	}})
	waitFor(t, func() bool { return s.stateSnapshot().ActiveTurnID == "" && s.stateSnapshot().Phase == "paused" })
	if got := proxy.methods(); fmt.Sprint(got) != "[turn/interrupt]" {
		t.Fatalf("methods = %v", got)
	}
}

func TestControlCommandRequiresAuthenticationAndStopConfirmation(t *testing.T) {
	state := supervisorState{Version: 1, PID: 1, CWD: t.TempDir(), Phase: "idle", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	control, err := startControlServerWithActions(func() supervisorState { return state }, func(_ context.Context, command remoteCommand) (string, error) {
		if command.Name == "stop" && !command.Confirm {
			return "", errors.New("confirmation required")
		}
		return "ok", nil
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/command", control.Port)
	body, _ := json.Marshal(remoteCommand{Name: "stop"})
	request, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+control.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfirmed response = %d", response.StatusCode)
	}
	confirmed, _ := json.Marshal(remoteCommand{Name: "stop", Confirm: true})
	request, _ = http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(confirmed))
	request.Header.Set("Authorization", "Bearer "+control.Token)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("confirmed response = %d", response.StatusCode)
	}
}
