package main

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

type stallWatchdogOptions struct {
	StallTimeout     time.Duration
	ConfirmTimeout   time.Duration
	ToolStallTimeout time.Duration
}

type stallContext struct {
	ThreadID         string
	TurnID           string
	LastActivityAt   time.Time
	Idle             time.Duration
	ActivitySequence uint64
	BlockingTypes    []string
}

type stallDecision struct {
	Kind      string
	Context   stallContext
	ConfirmAt time.Time
}

type stallSnapshot struct {
	ThreadID       string
	TurnID         string
	LastActivityAt time.Time
	SuspectedAt    time.Time
	PauseReason    string
	Recovering     bool
}

type stallObservation struct {
	Activity         bool
	SuspicionCleared bool
}

type activeTurnState struct {
	ThreadID             string
	TurnID               string
	LastActivityAt       time.Time
	ActivitySequence     uint64
	BlockingItems        map[string]string
	WaitingFlags         map[string]bool
	SafetyBuffering      bool
	VerificationRequired bool
	SuspectedAt          time.Time
	Recovering           bool
}

type stallWatchdog struct {
	options stallWatchdogOptions
	mu      sync.Mutex
	active  *activeTurnState
}

var blockingItemTypes = map[string]bool{
	"collabToolCall": true, "commandExecution": true, "contextCompaction": true,
	"dynamicToolCall": true, "fileChange": true, "imageView": true,
	"mcpToolCall": true, "webSearch": true,
}

func newStallWatchdog(options stallWatchdogOptions) *stallWatchdog {
	return &stallWatchdog{options: options}
}

func (w *stallWatchdog) Enabled() bool { return w.options.StallTimeout > 0 }

func (w *stallWatchdog) StartTurn(threadID, turnID string, now time.Time) {
	if !w.Enabled() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.active = &activeTurnState{ThreadID: threadID, TurnID: turnID, LastActivityAt: now, BlockingItems: map[string]string{}, WaitingFlags: map[string]bool{}}
}

func (w *stallWatchdog) CompleteTurn(turnID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != nil && w.active.TurnID == turnID {
		w.active = nil
	}
}

func (w *stallWatchdog) Observe(message rpcMessage, now time.Time) stallObservation {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.active
	if a == nil || message.Method == "" || message.Params == nil || !matchesActiveTurn(message.Params, a) {
		return stallObservation{}
	}
	cleared := !a.SuspectedAt.IsZero()
	switch message.Method {
	case "thread/status/changed":
		if status, ok := asObject(message.Params["status"]); ok {
			a.WaitingFlags = waitingFlags(status["activeFlags"])
		}
	case "model/safetyBuffering/updated":
		value, ok := readBool(message.Params["showBufferingUi"])
		a.SafetyBuffering = !ok || value
	case "model/verification":
		a.VerificationRequired = true
	case "item/started":
		if item, ok := asObject(message.Params["item"]); ok {
			id, idOK := readString(item["id"])
			typeName, typeOK := readString(item["type"])
			if idOK && typeOK && blockingItemTypes[typeName] {
				a.BlockingItems[id] = typeName
			}
		}
	case "item/completed":
		if item, ok := asObject(message.Params["item"]); ok {
			if id, ok := readString(item["id"]); ok {
				delete(a.BlockingItems, id)
			}
		}
	case "hook/started":
		a.BlockingItems[hookKey(message.Params)] = "hook"
	case "hook/completed":
		delete(a.BlockingItems, hookKey(message.Params))
	}
	recordActivity(a, now)
	return stallObservation{Activity: true, SuspicionCleared: cleared}
}

func (w *stallWatchdog) Evaluate(now time.Time) (stallDecision, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.active
	if a == nil || a.Recovering || pauseReason(a, w.options) != "" {
		return stallDecision{}, false
	}
	timeout := w.options.StallTimeout
	if len(a.BlockingItems) > 0 {
		timeout = w.options.ToolStallTimeout
	}
	if timeout <= 0 {
		return stallDecision{}, false
	}
	idle := max(time.Duration(0), now.Sub(a.LastActivityAt))
	if idle < timeout {
		a.SuspectedAt = time.Time{}
		return stallDecision{}, false
	}
	context := contextFor(a, idle)
	if a.SuspectedAt.IsZero() {
		a.SuspectedAt = now
		return stallDecision{Kind: "suspected", Context: context, ConfirmAt: now.Add(w.options.ConfirmTimeout)}, true
	}
	if now.Sub(a.SuspectedAt) < w.options.ConfirmTimeout {
		return stallDecision{}, false
	}
	a.Recovering = true
	return stallDecision{Kind: "confirmed", Context: context}, true
}

func (w *stallWatchdog) IsCurrent(context stallContext) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.active
	return a != nil && a.Recovering && a.ThreadID == context.ThreadID && a.TurnID == context.TurnID && a.ActivitySequence == context.ActivitySequence
}

func (w *stallWatchdog) CancelRecovery() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != nil {
		w.active.Recovering = false
		w.active.SuspectedAt = time.Time{}
	}
}

func (w *stallWatchdog) SetWaitingFlags(threadID string, flags []string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || w.active.ThreadID != threadID {
		return
	}
	w.active.WaitingFlags = map[string]bool{}
	for _, flag := range flags {
		if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
			w.active.WaitingFlags[flag] = true
		}
	}
	recordActivity(w.active, now)
	w.active.Recovering = false
}

func (w *stallWatchdog) Defer(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active != nil {
		recordActivity(w.active, now)
		w.active.Recovering = false
	}
}

func (w *stallWatchdog) Snapshot() stallSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil {
		return stallSnapshot{}
	}
	return stallSnapshot{ThreadID: w.active.ThreadID, TurnID: w.active.TurnID, LastActivityAt: w.active.LastActivityAt, SuspectedAt: w.active.SuspectedAt, PauseReason: pauseReason(w.active, w.options), Recovering: w.active.Recovering}
}

func matchesActiveTurn(params map[string]any, active *activeTurnState) bool {
	threadID, hasThread := readString(params["threadId"])
	turnID, hasTurn := readString(params["turnId"])
	if !hasTurn {
		if nested, ok := asObject(params["turn"]); ok {
			turnID, hasTurn = readString(nested["id"])
		}
	}
	if hasThread && threadID != active.ThreadID || hasTurn && turnID != active.TurnID {
		return false
	}
	return hasThread || hasTurn
}

func waitingFlags(value any) map[string]bool {
	result := map[string]bool{}
	values, _ := value.([]any)
	for _, value := range values {
		if flag, ok := value.(string); ok && (flag == "waitingOnApproval" || flag == "waitingOnUserInput") {
			result[flag] = true
		}
	}
	return result
}

func recordActivity(active *activeTurnState, now time.Time) {
	active.LastActivityAt = now
	active.ActivitySequence++
	active.SuspectedAt = time.Time{}
}

func pauseReason(active *activeTurnState, options stallWatchdogOptions) string {
	for _, flag := range []string{"waitingOnApproval", "waitingOnUserInput"} {
		if active.WaitingFlags[flag] {
			return flag
		}
	}
	if active.VerificationRequired {
		return "verificationRequired"
	}
	if active.SafetyBuffering {
		return "safetyBuffering"
	}
	if len(active.BlockingItems) > 0 && options.ToolStallTimeout <= 0 {
		types := make([]string, 0, len(active.BlockingItems))
		for _, typeName := range active.BlockingItems {
			types = append(types, typeName)
		}
		sort.Strings(types)
		return "activeTool:" + types[0]
	}
	return ""
}

func contextFor(active *activeTurnState, idle time.Duration) stallContext {
	seen := map[string]bool{}
	types := []string{}
	for _, typeName := range active.BlockingItems {
		if !seen[typeName] {
			seen[typeName] = true
			types = append(types, typeName)
		}
	}
	sort.Strings(types)
	return stallContext{ThreadID: active.ThreadID, TurnID: active.TurnID, LastActivityAt: active.LastActivityAt, Idle: idle, ActivitySequence: active.ActivitySequence, BlockingTypes: types}
}

func hookKey(params map[string]any) string {
	if run, ok := asObject(params["run"]); ok {
		if id, ok := readString(run["id"]); ok {
			return "hook:" + id
		}
	}
	return fmt.Sprintf("hook:%s", "active")
}
