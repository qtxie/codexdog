package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const healthGateFailuresBeforeProbe = 3

const (
	healthProbeBaseInstructions      = "You are a provider health-check responder. Return only the requested marker."
	healthProbeDeveloperInstructions = "Do not use tools, access files, inspect the workspace, or explain your answer."
	healthProbeUserPrompt            = "Return the exact marker CODEX_PROVIDER_OK as plain text."
)

type probeResult struct {
	Healthy            bool
	ProbeAttempted     bool
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
	rpc                rpcRequester
	options            providerProbeOptions
	mu                 sync.Mutex
	healthThreadIDs    map[string]struct{}
	healthGateTarget   string
	healthGateFailures int
	health             *healthChecker
	completions        map[string]turnCompletion
	waiters            map[string]chan turnCompletion
	unsubscribe        func()
	probeConfigOnce    sync.Once
	probeMCPConfig     map[string]any
}

func newProviderProbe(rpc rpcRequester, options providerProbeOptions) *providerProbe {
	healthOptions := healthOptionsWithLegacyURL(options.HealthChecks, options.HealthURL)
	probe := &providerProbe{rpc: rpc, options: options, healthThreadIDs: map[string]struct{}{}, completions: map[string]turnCompletion{}, waiters: map[string]chan turnCompletion{}}
	if len(healthOptions.Sources) > 0 {
		probe.health = newHealthChecker(healthOptions)
	}
	probe.unsubscribe = rpc.AddNotificationHandler(probe.handleNotification)
	return probe
}

func (p *providerProbe) Check(ctx context.Context, runtime ...string) probeResult {
	return p.check(ctx, false, runtime...)
}

func (p *providerProbe) CheckNow(ctx context.Context, runtime ...string) probeResult {
	return p.check(ctx, true, runtime...)
}

func (p *providerProbe) check(ctx context.Context, forceProbe bool, runtime ...string) probeResult {
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
	finishProbe := func(result probeResult) probeResult {
		result.ProbeAttempted = true
		return finish(result)
	}
	if p.health != nil {
		gate := p.health.check(ctx, model, runtimeProvider, p.options.Timeout)
		healthState = gate.State
		healthDetail = gate.Detail
		observations = gate.Observations
		runProbe, failures := true, 0
		if !forceProbe {
			runProbe, failures = p.shouldProbeAfterHealthGate(gate.State, model, runtimeProvider)
		}
		if !runProbe {
			if failures > 0 {
				healthDetail = appendHealthDetail(healthDetail, fmt.Sprintf("real Codex probe deferred after %d/%d consecutive status failures", failures, healthGateFailuresBeforeProbe))
			}
			return finish(healthGateProbeFailure(gate))
		}
		if gate.State != healthStateHealthy {
			if forceProbe {
				healthDetail = appendHealthDetail(healthDetail, "running explicitly requested real Codex probe despite status result")
			} else {
				healthDetail = appendHealthDetail(healthDetail, fmt.Sprintf("running real Codex probe after %d consecutive status failures", healthGateFailuresBeforeProbe))
			}
		}
	}
	threadCtx, threadCancel := context.WithTimeout(ctx, p.options.Timeout)
	defer threadCancel()
	threadID, probeCWD, err := p.startHealthThread(threadCtx, model)
	if err != nil {
		return finishProbe(probeFailure(err))
	}
	defer os.RemoveAll(probeCWD)
	params := map[string]any{
		"threadId":       threadID,
		"input":          []any{map[string]any{"type": "text", "text": healthProbeUserPrompt}},
		"cwd":            probeCWD,
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
		return finishProbe(probeFailure(err))
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		failure := classifyFailure(turnError{Message: "Health probe did not return a turn"})
		return finishProbe(probeResult{Failure: &failure})
	}
	completion, err := p.waitForCompletion(requestCtx, started.ID)
	if err != nil {
		interruptCtx, interruptCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = p.rpc.Request(interruptCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": started.ID})
		interruptCancel()
		return finishProbe(probeFailure(err))
	}
	if completion.Status == "completed" {
		return finishProbe(probeResult{Healthy: true})
	}
	failureError := turnError{Message: "Health probe turn " + completion.Status}
	if completion.Error != nil {
		failureError = *completion.Error
	}
	failure := classifyFailure(failureError)
	return finishProbe(probeResult{Failure: &failure})
}

func (p *providerProbe) shouldProbeAfterHealthGate(state, model, provider string) (bool, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	target := strings.TrimSpace(model) + "\x00" + strings.TrimSpace(provider)
	if target != p.healthGateTarget {
		p.healthGateTarget = target
		p.healthGateFailures = 0
	}
	if state == healthStateHealthy {
		p.healthGateFailures = 0
		return true, 0
	}
	if state == healthStateUnknown && p.health.options.UnknownPolicy == healthUnknownBlock {
		p.healthGateFailures = 0
		return false, 0
	}
	p.healthGateFailures++
	failures := p.healthGateFailures
	if failures < healthGateFailuresBeforeProbe {
		return false, failures
	}
	p.healthGateFailures = 0
	return true, failures
}

func appendHealthDetail(detail, addition string) string {
	if strings.TrimSpace(detail) == "" {
		return addition
	}
	return detail + "; " + addition
}

func (p *providerProbe) Dispose() {
	if p.unsubscribe != nil {
		p.unsubscribe()
	}
}

func newProbeCWD() (string, error) {
	probeCWD, err := os.MkdirTemp("", "codexdog-probe-")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(probeCWD, "codexdog-probe-instructions.md"), []byte("Provider health check. Return only the requested marker."), 0600); err != nil {
		_ = os.RemoveAll(probeCWD)
		return "", err
	}
	return probeCWD, nil
}

func (p *providerProbe) startHealthThread(ctx context.Context, model string) (string, string, error) {
	probeCWD, err := newProbeCWD()
	if err != nil {
		return "", "", err
	}
	params := map[string]any{
		"cwd":                   probeCWD,
		"ephemeral":             true,
		"sandbox":               "read-only",
		"approvalPolicy":        "never",
		"baseInstructions":      healthProbeBaseInstructions,
		"developerInstructions": healthProbeDeveloperInstructions,
		"config": map[string]any{
			"mcp_servers": p.probeMCPServers(ctx),
			"features": map[string]any{
				"apps":                         false,
				"browser_use":                  false,
				"browser_use_external":         false,
				"browser_use_full_cdp_access":  false,
				"computer_use":                 false,
				"in_app_browser":               false,
				"multi_agent":                  false,
				"plugins":                      false,
				"shell_snapshot":               false,
				"shell_tool":                   false,
				"skill_mcp_dependency_install": false,
				"unified_exec":                 false,
				"web_search":                   false,
			},
			"memories": map[string]any{
				"use_memories":      false,
				"generate_memories": false,
			},
			"history":                 map[string]any{"persistence": "none"},
			"model_instructions_file": filepath.Join(probeCWD, "codexdog-probe-instructions.md"),
			"web_search":              "disabled",
		},
	}
	if model != "" {
		params["model"] = model
	}
	value, err := p.rpc.Request(ctx, "thread/start", params)
	if err != nil {
		_ = os.RemoveAll(probeCWD)
		return "", "", err
	}
	object, _ := asObject(value)
	threadObject, _ := asObject(object["thread"])
	id, ok := readString(threadObject["id"])
	if !ok {
		_ = os.RemoveAll(probeCWD)
		return "", "", fmt.Errorf("health probe did not return a thread id")
	}
	p.mu.Lock()
	p.healthThreadIDs[id] = struct{}{}
	p.mu.Unlock()
	return id, probeCWD, nil
}

func (p *providerProbe) probeMCPServers(ctx context.Context) map[string]any {
	p.probeConfigOnce.Do(func() {
		p.probeMCPConfig = map[string]any{}
		value, err := p.rpc.Request(ctx, "config/read", map[string]any{"includeLayers": false})
		if err != nil {
			return
		}
		object, _ := asObject(value)
		config, _ := asObject(object["config"])
		servers, _ := asObject(config["mcp_servers"])
		for name := range servers {
			p.probeMCPConfig[name] = map[string]any{"enabled": false}
		}
	})
	return p.probeMCPConfig
}

func (p *providerProbe) handleNotification(message rpcMessage) {
	if message.Method != "turn/completed" || message.Params == nil {
		return
	}
	threadID, _ := readString(message.Params["threadId"])
	p.mu.Lock()
	if threadID == "" {
		p.mu.Unlock()
		return
	}
	if _, ok := p.healthThreadIDs[threadID]; !ok {
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
		delete(p.healthThreadIDs, threadID)
		p.mu.Unlock()
		waiter <- completion
		return
	}
	delete(p.healthThreadIDs, threadID)
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
