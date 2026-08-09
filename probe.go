package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type probeResult struct {
	Healthy    bool
	Failure    *classifiedFailure
	RetryAfter time.Duration
}

type providerProbeOptions struct {
	CWD       string
	Timeout   time.Duration
	HealthURL string
	Model     string
}

type rpcRequester interface {
	Request(context.Context, string, map[string]any) (any, error)
	AddNotificationHandler(notificationHandler) func()
}

type turnCompletion struct {
	Status string
	Error  *turnError
}

type providerProbe struct {
	rpc            rpcRequester
	options        providerProbeOptions
	mu             sync.Mutex
	healthThreadID string
	completions    map[string]turnCompletion
	waiters        map[string]chan turnCompletion
	unsubscribe    func()
}

func newProviderProbe(rpc rpcRequester, options providerProbeOptions) *providerProbe {
	probe := &providerProbe{rpc: rpc, options: options, completions: map[string]turnCompletion{}, waiters: map[string]chan turnCompletion{}}
	probe.unsubscribe = rpc.AddNotificationHandler(probe.handleNotification)
	return probe
}

func (p *providerProbe) Check(ctx context.Context) probeResult {
	if p.options.HealthURL != "" {
		result := p.checkHealthEndpoint(ctx)
		if !result.Healthy {
			return result
		}
	}
	threadID, err := p.ensureHealthThread(ctx)
	if err != nil {
		return probeFailure(err)
	}
	params := map[string]any{
		"threadId":       threadID,
		"input":          []any{map[string]any{"type": "text", "text": "Reply with exactly CODEX_PROVIDER_OK. Do not use tools."}},
		"cwd":            p.options.CWD,
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly"},
	}
	if p.options.Model != "" {
		params["model"] = p.options.Model
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.options.Timeout)
	defer cancel()
	value, err := p.rpc.Request(requestCtx, "turn/start", params)
	if err != nil {
		return probeFailure(err)
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		failure := classifyFailure(turnError{Message: "Health probe did not return a turn"})
		return probeResult{Failure: &failure}
	}
	completion, err := p.waitForCompletion(requestCtx, started.ID)
	if err != nil {
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = p.rpc.Request(interruptCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": started.ID})
		interruptCancel()
		return probeFailure(err)
	}
	if completion.Status == "completed" {
		return probeResult{Healthy: true}
	}
	failureError := turnError{Message: "Health probe turn " + completion.Status}
	if completion.Error != nil {
		failureError = *completion.Error
	}
	failure := classifyFailure(failureError)
	return probeResult{Failure: &failure}
}

func (p *providerProbe) Dispose() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func (p *providerProbe) ensureHealthThread(ctx context.Context) (string, error) {
	p.mu.Lock()
	if p.healthThreadID != "" {
		id := p.healthThreadID
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()
	params := map[string]any{
		"cwd": p.options.CWD, "ephemeral": true, "sandbox": "read-only", "approvalPolicy": "never",
		"developerInstructions": "This is a provider health check. Never call tools. Reply only with the requested marker.",
	}
	if p.options.Model != "" {
		params["model"] = p.options.Model
	}
	value, err := p.rpc.Request(ctx, "thread/start", params)
	if err != nil {
		return "", err
	}
	object, _ := asObject(value)
	threadObject, _ := asObject(object["thread"])
	id, ok := readString(threadObject["id"])
	if !ok {
		return "", fmt.Errorf("health probe did not return a thread id")
	}
	p.mu.Lock()
	p.healthThreadID = id
	p.mu.Unlock()
	return id, nil
}

func (p *providerProbe) handleNotification(message rpcMessage) {
	if message.Method != "turn/completed" || message.Params == nil {
		return
	}
	threadID, _ := readString(message.Params["threadId"])
	p.mu.Lock()
	if threadID == "" || threadID != p.healthThreadID {
		p.mu.Unlock()
		return
	}
	parsed, ok := readTurn(message.Params["turn"])
	if !ok || parsed.Status == "inProgress" {
		p.mu.Unlock()
		return
	}
	completion := turnCompletion{Status: parsed.Status, Error: parsed.Error}
	if waiter := p.waiters[parsed.ID]; waiter != nil {
		delete(p.waiters, parsed.ID)
		p.mu.Unlock()
		waiter <- completion
		return
	}
	p.completions[parsed.ID] = completion
	p.mu.Unlock()
}

func (p *providerProbe) waitForCompletion(ctx context.Context, turnID string) (turnCompletion, error) {
	p.mu.Lock()
	if existing, ok := p.completions[turnID]; ok {
		delete(p.completions, turnID)
		p.mu.Unlock()
		return existing, nil
	}
	waiter := make(chan turnCompletion, 1)
	p.waiters[turnID] = waiter
	p.mu.Unlock()
	select {
	case completion := <-waiter:
		return completion, nil
	case <-ctx.Done():
		p.mu.Lock()
		delete(p.waiters, turnID)
		p.mu.Unlock()
		return turnCompletion{}, fmt.Errorf("provider health probe timed out after %s: %w", p.options.Timeout, ctx.Err())
	}
}

func (p *providerProbe) checkHealthEndpoint(ctx context.Context) probeResult {
	timeout := min(p.options.Timeout, 15*time.Second)
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, p.options.HealthURL, nil)
	if err != nil {
		return probeFailure(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		failure := classifyFailure(turnError{Message: err.Error(), CodexErrorInfo: map[string]any{"httpConnectionFailed": map[string]any{}}})
		return probeResult{Failure: &failure}
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		return probeResult{Healthy: true}
	}
	failure := classifyFailure(turnError{Message: fmt.Sprintf("Provider health endpoint returned HTTP %d", response.StatusCode), CodexErrorInfo: map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": float64(response.StatusCode)}}})
	result := probeResult{Failure: &failure}
	if seconds, err := strconv.ParseFloat(response.Header.Get("Retry-After"), 64); err == nil {
		result.RetryAfter = time.Duration(seconds * float64(time.Second))
	}
	return result
}

func probeFailure(err error) probeResult {
	failure := classifyFailure(turnError{Message: err.Error()})
	return probeResult{Failure: &failure}
}
