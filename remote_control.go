package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// remoteCommand is the deliberately small command surface exposed to remote
// clients. It is not a general JSON-RPC tunnel: callers cannot invoke an
// arbitrary Codex method or a local process.
type remoteCommand struct {
	Name    string `json:"name"`
	Text    string `json:"text,omitempty"`
	Confirm bool   `json:"confirm,omitempty"`
}

const remoteHelpText = `Commands:
/status - show supervisor and Codex state
/prompt TEXT - send a prompt to the current thread
/pause - interrupt the current turn and pause automatic recovery
/resume - resume the current thread (or its active goal)
/goal - show the current goal
/goal pause|resume - change the current goal status
/goal set TEXT - replace the current goal objective
/queue - manage queued submissions for the current thread
/stop confirm - stop codexdog and the Codex processes it owns
/help - show this help`

func (s *supervisor) executeRemoteCommand(ctx context.Context, command remoteCommand) (string, error) {
	name := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(command.Name), "/"))
	if name == "" {
		return "", errors.New("remote command name is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	switch name {
	case "status":
		refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s.refreshUsageSnapshot(refreshCtx)
		cancel()
		return formatRemoteStatus(s.stateSnapshot()), nil
	case "help":
		return remoteHelpText, nil
	case "prompt":
		return s.remotePrompt(ctx, strings.TrimSpace(command.Text))
	case "pause":
		return s.remotePause(ctx)
	case "resume":
		return s.remoteResume(ctx)
	case "goal":
		return s.remoteGoal(ctx, strings.TrimSpace(command.Text))
	case "queue":
		return s.remoteQueue(ctx, strings.TrimSpace(command.Text))
	case "stop":
		if !command.Confirm && !strings.EqualFold(strings.TrimSpace(command.Text), "confirm") {
			return "Stopping requires confirmation. Send /stop confirm.", nil
		}
		go s.shutdown("remote stop requested")
		return "Stop requested.", nil
	default:
		return "Unknown command.\n\n" + remoteHelpText, nil
	}
}

func formatRemoteStatus(state supervisorState) string {
	lines := []string{
		"Codexdog status",
		"Phase: " + valueOrDash(state.Phase),
		"Workspace: " + valueOrDash(state.CWD),
		"Thread directory: " + valueOrDash(state.EffectiveCWD),
		"Codex: " + valueOrDash(state.CodexVersion),
		"Thread: " + valueOrDash(state.CurrentThreadID),
		"Permission profile: " + valueOrDash(state.ActivePermissionProfile),
		"Sandbox: " + valueOrDash(state.SandboxPolicy),
		"Model: " + valueOrDash(state.Model),
		"Primary client: " + valueOrDash(formatClientIdentity(state.PrimaryClient, state.PrimaryClientVersion)),
		"Turn: " + valueOrDash(state.ActiveTurnID),
		"Manual pause: " + yesNo(state.ManualPaused),
		"Telegram control: " + yesNo(state.TelegramEnabled),
		fmt.Sprintf("Automatic resumes: %d", state.AutomaticResumeCount),
		fmt.Sprintf("Stall resumes: %d", state.StallRecoveryCount),
		fmt.Sprintf("Provider probe: attempt %d, consecutive successes %d", state.ProbeAttempt, state.ConsecutiveProbeSuccesses),
		"Next provider probe: " + valueOrDash(state.NextProbeAt),
		"Last turn activity: " + valueOrDash(state.LastTurnActivityAt),
		"Last error: " + valueOrDash(state.LastError),
		"Updated: " + valueOrDash(state.UpdatedAt),
	}
	if state.TelegramLastError != "" {
		lines = append(lines, "Telegram last error: "+state.TelegramLastError)
	}
	lines = append(lines, usageStatusLines(state)...)
	return strings.Join(lines, "\n")
}

func (s *supervisor) remotePrompt(ctx context.Context, text string) (string, error) {
	if text == "" {
		return "Usage: /prompt TEXT", nil
	}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return "", errors.New("supervisor is stopping")
	}
	threadID := s.state.CurrentThreadID
	turnID := s.state.ActiveTurnID
	phase := s.state.Phase
	manualPaused := s.state.ManualPaused
	proxy := s.proxy
	s.mu.Unlock()
	if proxy == nil {
		return "", errors.New("Codex TUI is not connected")
	}
	if phase == "provider-down" || phase == "probing" || phase == "resuming" || phase == "interrupting-error" || phase == "suspected-stall" || phase == "interrupting-stall" {
		return "", fmt.Errorf("a Codex recovery transition is in progress (phase %s); retry after it finishes", phase)
	}
	if threadID == "" {
		return "", errors.New("there is no current Codex thread")
	}
	if turnID != "" {
		if manualPaused {
			return "", errors.New("the active turn is still being paused; retry after its turn ID clears")
		}
		value, err := proxy.Request(ctx, "turn/steer", map[string]any{
			"threadId":       threadID,
			"input":          []any{map[string]any{"type": "text", "text": text}},
			"expectedTurnId": turnID,
		})
		if err != nil {
			return "", fmt.Errorf("steer turn: %w", err)
		}
		s.modifyState(func(state *supervisorState) {
			state.ManualPaused = false
			state.Phase = "running"
		})
		_ = s.persist()
		s.notifyTelegram(fmt.Sprintf("Remote prompt steered turn %s.", turnID))
		_ = value
		return "Prompt sent to the active turn.", nil
	}

	s.modifyState(func(state *supervisorState) {
		state.ManualPaused = false
		state.Phase = "resuming"
		state.AutomaticResumeCount = 0
		state.StallRecoveryCount = 0
		state.ResumeRequestedForTurnID = ""
	})
	s.cancelRecovery()
	if err := s.startContinuation(ctx, threadID, text, false, "remote-prompt"); err != nil {
		s.modifyState(func(state *supervisorState) {
			state.Phase = "needs-attention"
			state.LastError = sanitizeText(err.Error())
		})
		_ = s.persist()
		return "", err
	}
	s.notifyTelegram("Remote prompt started a new turn.")
	return "Prompt sent; Codex started a new turn.", nil
}

func (s *supervisor) remotePause(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return "", errors.New("supervisor is stopping")
	}
	if s.state.ManualPaused && s.state.ActiveTurnID == "" {
		s.mu.Unlock()
		return "Already paused.", nil
	}
	threadID := s.state.CurrentThreadID
	turnID := s.state.ActiveTurnID
	proxy := s.proxy
	for _, pending := range s.pendingTerminalErrors {
		if pending.Timer != nil {
			pending.Timer.Stop()
		}
	}
	s.pendingTerminalErrors = map[string]*pendingTerminalError{}
	s.turnErrors = map[string]turnError{}
	s.turnHadRetryableError = map[string]bool{}
	s.state.TerminalErrorSuspectedAt = ""
	s.state.ManualPaused = true
	s.state.Phase = "paused"
	s.stallGeneration++
	waiters := s.turnTerminalWaiters
	s.turnTerminalWaiters = map[string]chan string{}
	s.mu.Unlock()
	for _, waiter := range waiters {
		select {
		case waiter <- "":
		default:
		}
	}
	s.cancelRecovery()
	s.watchdog.CancelRecovery()
	_ = s.persist()
	if turnID == "" {
		s.notifyTelegram("Manual pause enabled.")
		return "Paused automatic recovery and future turns.", nil
	}
	if proxy == nil {
		return "Manual pause enabled, but the Codex TUI connection is unavailable.", nil
	}
	interruptCtx, cancel := context.WithTimeout(ctx, max(5*time.Second, s.options.StallInterruptTimeout))
	_, err := proxy.Request(interruptCtx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	cancel()
	if err != nil {
		s.modifyState(func(state *supervisorState) {
			state.LastError = sanitizeText("manual pause interrupt failed: " + err.Error())
		})
		_ = s.persist()
		s.notifyTelegram("Manual pause is enabled, but interrupt failed: " + sanitizeText(err.Error()))
		return "Manual pause enabled, but interrupt failed: " + err.Error(), nil
	}
	s.modifyState(func(state *supervisorState) {
		state.ActiveTurnID = ""
		state.Phase = "paused"
		state.ManualPaused = true
	})
	s.mu.Lock()
	if s.handledTurns == nil {
		s.handledTurns = map[string]bool{}
	}
	s.handledTurns[turnID] = true
	s.mu.Unlock()
	s.watchdog.CompleteTurn(turnID)
	s.syncStallState()
	_ = s.persist()
	s.notifyTelegram("Manual pause enabled; the active turn was interrupted.")
	return "Paused; active turn interrupt requested.", nil
}

func (s *supervisor) remoteResume(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return "", errors.New("supervisor is stopping")
	}
	if s.state.ActiveTurnID != "" {
		active := s.state.ActiveTurnID
		s.mu.Unlock()
		return "A turn is already active: " + active, nil
	}
	if s.recoveryCancel != nil {
		s.state.ManualPaused = false
		phase := s.state.Phase
		s.mu.Unlock()
		_ = s.persist()
		return "Manual pause cleared; automatic provider recovery remains active (" + phase + ").", nil
	}
	threadID := s.state.CurrentThreadID
	proxy := s.proxy
	s.state.ManualPaused = false
	s.state.Phase = "resuming"
	s.state.AutomaticResumeCount = 0
	s.state.StallRecoveryCount = 0
	s.state.ResumeRequestedForTurnID = ""
	s.state.NextProbeAt = ""
	s.mu.Unlock()
	if threadID == "" {
		s.modifyState(func(state *supervisorState) { state.Phase = "idle" })
		_ = s.persist()
		return "", errors.New("there is no current Codex thread")
	}
	if proxy == nil {
		s.modifyState(func(state *supervisorState) { state.Phase = "needs-attention" })
		_ = s.persist()
		return "", errors.New("Codex TUI is not connected")
	}
	s.cancelRecovery()
	if err := s.startContinuation(ctx, threadID, stallContinuationPrompt, true, "remote-resume"); err != nil {
		s.modifyState(func(state *supervisorState) {
			state.Phase = "needs-attention"
			state.LastError = sanitizeText(err.Error())
		})
		_ = s.persist()
		return "", err
	}
	s.notifyTelegram("Manual resume requested.")
	return "Resume requested.", nil
}

func (s *supervisor) remoteGoal(ctx context.Context, args string) (string, error) {
	s.mu.Lock()
	threadID, proxy := s.state.CurrentThreadID, s.proxy
	s.mu.Unlock()
	if threadID == "" {
		return "", errors.New("there is no current Codex thread")
	}
	if proxy == nil {
		return "", errors.New("Codex TUI is not connected")
	}
	if args == "" || strings.EqualFold(args, "show") {
		value, err := proxy.Request(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
		if err != nil {
			return "", fmt.Errorf("get goal: %w", err)
		}
		object, _ := asObject(value)
		goal, ok := readThreadGoal(object["goal"])
		if !ok {
			return "No persisted goal is attached to the current thread.", nil
		}
		objective := strings.TrimSpace(goal.Objective)
		if objective == "" {
			objective = "(no objective returned)"
		}
		return fmt.Sprintf("Goal status: %s\nObjective: %s", goal.Status, objective), nil
	}
	parts := strings.Fields(args)
	switch strings.ToLower(parts[0]) {
	case "pause", "resume":
		status := strings.ToLower(parts[0])
		if status == "pause" {
			status = "paused"
		} else {
			status = "active"
		}
		value, err := proxy.Request(ctx, "thread/goal/set", map[string]any{"threadId": threadID, "status": status})
		if err != nil {
			return "", fmt.Errorf("set goal status: %w", err)
		}
		_ = value
		s.notifyTelegram("Goal status set to " + status + ".")
		return "Goal status set to " + status + ".", nil
	case "set":
		objective := strings.TrimSpace(strings.TrimPrefix(args, parts[0]))
		if objective == "" {
			return "Usage: /goal set OBJECTIVE", nil
		}
		value, err := proxy.Request(ctx, "thread/goal/set", map[string]any{
			"threadId":  threadID,
			"objective": objective,
			"status":    "active",
		})
		if err != nil {
			return "", fmt.Errorf("set goal: %w", err)
		}
		_ = value
		s.notifyTelegram("Goal objective updated.")
		return "Goal objective updated and activated.", nil
	default:
		return "Usage: /goal, /goal pause, /goal resume, or /goal set OBJECTIVE", nil
	}
}

// startContinuation is shared by remote commands and automatic recovery. A
// goal-aware continuation reactivates a saved goal without injecting a user
// prompt; ordinary continuation resumes the thread and starts a text turn.
func (s *supervisor) startContinuation(ctx context.Context, threadID, prompt string, goalAware bool, clientID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.proxy == nil {
		return errors.New("Codex TUI is not connected")
	}
	if goalAware {
		if goal, hasGoal := s.goalForRecovery(ctx, threadID); hasGoal {
			value, err := s.proxy.Request(ctx, "thread/goal/set", map[string]any{"threadId": threadID, "status": "active"})
			if err != nil {
				return fmt.Errorf("resume goal: %w", err)
			}
			object, _ := asObject(value)
			returned, ok := readThreadGoal(object["goal"])
			if !ok || returned.Status != "active" {
				return errors.New("Codex did not confirm the resumed goal")
			}
			s.modifyState(func(state *supervisorState) {
				setCurrentThread(state, threadID)
				state.Phase = "running"
				state.ActiveTurnID = ""
				state.ProbeAttempt = 0
				state.ConsecutiveProbeSuccesses = 0
			})
			_ = s.persist()
			s.logger.Log(fmt.Sprintf("Resumed %s goal on thread %s", goal.Status, threadID))
			return nil
		}
	}
	if _, err := s.proxy.Request(ctx, "thread/resume", map[string]any{"threadId": threadID}); err != nil {
		return fmt.Errorf("resume thread: %w", err)
	}
	params := map[string]any{
		"threadId": threadID,
		"input":    []any{map[string]any{"type": "text", "text": prompt}},
	}
	if clientID != "" {
		params["clientUserMessageId"] = "codexdog:" + clientID + ":" + fmt.Sprintf("%d", time.Now().UnixNano())
	}
	value, err := s.proxy.Request(ctx, "turn/start", params)
	if err != nil {
		return fmt.Errorf("start turn: %w", err)
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		return errors.New("Codex did not return a started turn")
	}
	if s.recordStartedTurnFromRequest(threadID, started.ID) {
		return errors.New("the turn started while the supervisor was paused and was interrupted")
	}
	s.logger.Log(fmt.Sprintf("Started continuation turn %s on %s", started.ID, threadID))
	return nil
}
