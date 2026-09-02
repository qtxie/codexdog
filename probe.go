package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type probeResult struct {
	Healthy            bool
	Failure            *classifiedFailure
	RetryAfter         time.Duration
	HealthState        string
	HealthDetail       string
	HealthObservations []healthObservation
}

type providerProbeOptions struct {
	CWD          string
	Timeout      time.Duration
	HealthURL    string
	HealthChecks healthCheckOptions
	Model        string
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
	rpc               rpcRequester
	options           providerProbeOptions
	mu                sync.Mutex
	healthThreadID    string
	healthThreadModel string
	health            *healthChecker
	completions       map[string]turnCompletion
	waiters           map[string]chan turnCompletion
	unsubscribe       func()
}

func newProviderProbe(rpc rpcRequester, options providerProbeOptions) *providerProbe {
	healthOptions := healthOptionsWithLegacyURL(options.HealthChecks, options.HealthURL)
	probe := &providerProbe{rpc: rpc, options: options, completions: map[string]turnCompletion{}, waiters: map[string]chan turnCompletion{}}
	if len(healthOptions.Sources) > 0 {
		probe.health = newHealthChecker(healthOptions)
	}
	probe.unsubscribe = rpc.AddNotificationHandler(probe.handleNotification)
	return probe
}

func (p *providerProbe) Check(ctx context.Context, runtime ...string) probeResult {
	runtimeModel, runtimeProvider := "", ""
	if len(runtime) > 0 {
		runtimeModel = runtime[0]
	}
	if len(runtime) > 1 {
		runtimeProvider = runtime[1]
	}
	model := strings.TrimSpace(p.options.Model)
	if p.health != nil {
		model = p.health.targetModelForProvider(model, runtimeModel, runtimeProvider)
	} else if model == "" {
		model = strings.TrimSpace(runtimeModel)
	}
	observations := []healthObservation(nil)
	healthState, healthDetail := "", ""
	finish := func(result probeResult) probeResult {
		result.HealthState = healthState
		result.HealthDetail = healthDetail
		result.HealthObservations = observations
		return result
	}
	if p.health != nil {
		gate := p.health.check(ctx, model, runtimeProvider, p.options.Timeout)
		healthState = gate.State
		healthDetail = gate.Detail
		observations = gate.Observations
		if gate.State == healthStateUnhealthy || gate.State == healthStateUnknown && p.health.options.UnknownPolicy == healthUnknownBlock {
			return finish(healthGateProbeFailure(gate))
		}
	}
	threadID, err := p.ensureHealthThread(ctx, model)
	if err != nil {
		return finish(probeFailure(err))
	}
	params := map[string]any{
		"threadId":       threadID,
		"input":          []any{map[string]any{"type": "text", "text": "Reply with exactly CODEX_PROVIDER_OK. Do not use tools."}},
		"cwd":            p.options.CWD,
		"approvalPolicy": "never",
		"sandboxPolicy":  map[string]any{"type": "readOnly"},
	}
	if model != "" {
		params["model"] = model
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.options.Timeout)
	defer cancel()
	value, err := p.rpc.Request(requestCtx, "turn/start", params)
	if err != nil {
		return finish(probeFailure(err))
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		failure := classifyFailure(turnError{Message: "Health probe did not return a turn"})
		return finish(probeResult{Failure: &failure})
	}
	completion, err := p.waitForCompletion(requestCtx, started.ID)
	if err != nil {
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = p.rpc.Request(interruptCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": started.ID})
		interruptCancel()
		return finish(probeFailure(err))
	}
	if completion.Status == "completed" {
		return finish(probeResult{Healthy: true})
	}
	failureError := turnError{Message: "Health probe turn " + completion.Status}
	if completion.Error != nil {
		failureError = *completion.Error
	}
	failure := classifyFailure(failureError)
	return finish(probeResult{Failure: &failure})
}

func (p *providerProbe) Dispose() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func (p *providerProbe) ensureHealthThread(ctx context.Context, model string) (string, error) {
	p.mu.Lock()
	if p.healthThreadID != "" && p.healthThreadModel == model {
		id := p.healthThreadID
		p.mu.Unlock()
		return id, nil
	}
	p.mu.Unlock()
	params := map[string]any{
		"cwd": p.options.CWD, "ephemeral": true, "sandbox": "read-only", "approvalPolicy": "never",
		"developerInstructions": "This is a provider health check. Never call tools. Reply only with the requested marker.",
	}
	if model != "" {
		params["model"] = model
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
	p.healthThreadModel = model
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

func probeFailure(err error) probeResult {
	failure := classifyFailure(turnError{Message: err.Error()})
	return probeResult{Failure: &failure}
}

func healthGateProbeFailure(gate healthGateResult) probeResult {
	details := make([]string, 0, len(gate.Observations))
	for _, observation := range gate.Observations {
		detail := observation.Source + "=" + observation.State
		if observation.Detail != "" {
			detail += " (" + observation.Detail + ")"
		}
		details = append(details, detail)
	}
	message := "Provider status sources returned " + gate.State
	if gate.Detail != "" {
		message += ": " + gate.Detail
	}
	if len(details) > 0 {
		message += ": " + strings.Join(details, "; ")
	}
	turnFailure := turnError{Message: compactHealthDetail(message)}
	if len(gate.Observations) == 1 && gate.Observations[0].Type == healthSourceHTTP {
		observation := gate.Observations[0]
		if observation.ConnectionFailed {
			turnFailure.CodexErrorInfo = map[string]any{"httpConnectionFailed": map[string]any{}}
		} else if observation.HTTPStatus != 0 {
			turnFailure.CodexErrorInfo = map[string]any{"httpConnectionFailed": map[string]any{"httpStatusCode": float64(observation.HTTPStatus)}}
		}
	}
	failure := classifyFailure(turnFailure)
	return probeResult{Failure: &failure, RetryAfter: gate.RetryAfter}
}
