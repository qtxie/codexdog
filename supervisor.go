package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"time"
)

const continuationPrompt = "The previous turn ended because the model provider was unavailable. Continue the unfinished task from the existing thread and workspace state. Inspect existing work first, verify what already completed, and do not repeat completed steps."
const stallContinuationPrompt = "continue"

type supervisorOptions struct {
	CWD                   string
	CodexPath             string
	CodexConfig           []string
	TUIArgs               []string
	HealthURL             string
	HealthChecks          healthCheckOptions
	ProbeModel            string
	ProbeTimeout          time.Duration
	TerminalErrorGrace    time.Duration
	ProbeSuccesses        int
	Backoff               []time.Duration
	MaxAutoResumes        int
	StallTimeout          time.Duration
	StallConfirm          time.Duration
	StallInterruptTimeout time.Duration
	MaxStallResumes       int
	ToolStallTimeout      time.Duration
	TelegramToken         string
	TelegramAllowedChats  []int64
	TelegramAllowedUsers  []int64
	TelegramPollTimeout   time.Duration
	TelegramNotify        bool
	TelegramStatePath     string
	TelegramAlias         string
	TelegramDisabled      bool
	WeChatAllowedUsers    []string
	WeChatPollTimeout     time.Duration
	WeChatLoginTimeout    time.Duration
	WeChatOpenBrowser     bool
	WeChatNotify          bool
	WeChatDisabled        bool
	WeChatCredentialsPath string
	WeChatQRCodePath      string
}

type recoveryContext struct {
	ThreadID     string
	FailedTurnID string
	Failure      classifiedFailure
}

type pendingTerminalError struct {
	Recovery     recoveryContext
	Generation   uint64
	Timer        *time.Timer
	Interrupting bool
}

type threadRuntimeStatus struct {
	Type        string
	ActiveFlags []string
}

type proxyRequester interface {
	Request(context.Context, string, map[string]any) (any, error)
}

type supervisorEvent struct {
	client       bool
	connectionID string
	message      rpcMessage
}

const interruptHookWaitTimeout = 10 * time.Minute

type supervisor struct {
	options  supervisorOptions
	store    *stateStore
	logger   *fileLogger
	watchdog *stallWatchdog

	mu            sync.Mutex
	state         supervisorState
	appCmd        *exec.Cmd
	tuiCmd        *exec.Cmd
	proxy         proxyRequester
	usageRPC      proxyRequester
	queueRPC      proxyRequester
	proxyServer   *tuiProxy
	rpc           *jsonRPCClient
	queueClient   *jsonRPCClient
	probe         *providerProbe
	control       *controlServer
	notifications *notificationBroker
	processes     *processTree
	shuttingDown  bool

	recoveryCancel       context.CancelFunc
	recoveryGeneration   uint64
	pendingRecovery      *recoveryContext
	submittingResume     bool
	stallCheckInFlight   bool
	stallGeneration      uint64
	activityTimer        *time.Timer
	usageRefreshTimer    *time.Timer
	usageRefreshInFlight bool

	turnErrors               map[string]turnError
	turnHadRetryableError    map[string]bool
	pendingTerminalErrors    map[string]*pendingTerminalError
	handledTurns             map[string]bool
	threadDirectInput        map[string]bool
	cyberPolicyAttempts      map[string]int
	watchdogInterruptedTurns map[string]bool
	pauseInterruptingTurns   map[string]bool
	turnTerminalWaiters      map[string]chan string
	remoteMu                 sync.Mutex
	telegram                 *telegramController
	wechat                   *wechatController

	events       chan supervisorEvent
	done         chan struct{}
	shutdownOnce sync.Once
}

func newSupervisor(options supervisorOptions, store *stateStore) *supervisor {
	if options.TerminalErrorGrace <= 0 {
		options.TerminalErrorGrace = 5 * time.Second
	}
	return &supervisor{
		options:                  options,
		store:                    store,
		logger:                   newLogger(store.LogPath),
		notifications:            newNotificationBroker(),
		watchdog:                 newStallWatchdog(stallWatchdogOptions{StallTimeout: options.StallTimeout, ConfirmTimeout: options.StallConfirm, ToolStallTimeout: options.ToolStallTimeout}),
		state:                    supervisorState{Version: 2, PID: os.Getpid(), CWD: options.CWD, EffectiveCWD: options.CWD, Phase: "starting", TelegramAlias: options.TelegramAlias, TelegramEnabled: options.TelegramAlias != "", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)},
		turnErrors:               map[string]turnError{},
		turnHadRetryableError:    map[string]bool{},
		pendingTerminalErrors:    map[string]*pendingTerminalError{},
		handledTurns:             map[string]bool{},
		threadDirectInput:        map[string]bool{},
		cyberPolicyAttempts:      map[string]int{},
		watchdogInterruptedTurns: map[string]bool{},
		pauseInterruptingTurns:   map[string]bool{},
		turnTerminalWaiters:      map[string]chan string{},
		events:                   make(chan supervisorEvent, 1024),
		done:                     make(chan struct{}),
	}
}

func (s *supervisor) Run() (int, error) {
	if err := s.store.Initialize(); err != nil {
		return 1, err
	}
	if err := s.logger.Initialize(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, err
	}
	if codexVersion, err := installedCodexVersion(context.Background(), s.options); err != nil {
		s.logger.Log("Could not detect Codex version: " + err.Error())
	} else {
		s.modifyState(func(state *supervisorState) { state.CodexVersion = codexVersion })
	}
	if err := s.persist(); err != nil {
		return 1, err
	}
	s.logger.Log("Starting supervisor in " + s.options.CWD)
	go s.eventLoop()
	processes, err := newProcessTree()
	if err != nil {
		return s.startupFailure(fmt.Errorf("initialize child process management: %w", err))
	}
	s.mu.Lock()
	s.processes = processes
	s.mu.Unlock()

	appPort, err := getFreePort()
	if err != nil {
		return s.startupFailure(err)
	}
	appURL := fmt.Sprintf("ws://127.0.0.1:%d", appPort)
	appDone, err := s.spawnAppServer(appURL)
	if err != nil {
		return s.startupFailure(err)
	}
	if err := waitForReady(appPort, appDone); err != nil {
		return s.startupFailure(err)
	}
	go func() {
		err := <-appDone
		if !s.isShuttingDown() {
			s.logger.Log(fmt.Sprintf("Codex app-server exited unexpectedly: %v", err))
			_ = s.setAttention("Codex app-server exited unexpectedly")
		}
	}()

	proxy := newTUIProxy(appURL)
	proxyPort, err := proxy.Start()
	if err != nil {
		return s.startupFailure(err)
	}
	s.proxy = proxy
	s.proxyServer = proxy
	tuiInitialized := make(chan struct{}, 1)
	proxy.OnServerMessage(func(message rpcMessage, connectionID string) { s.enqueue(false, connectionID, message) })
	proxy.OnClientMessage(func(message rpcMessage, connectionID string) {
		if !proxy.IsPrimary(connectionID) {
			return
		}
		if message.Method == "initialize" {
			s.recordPrimaryClient(proxy.PrimaryClientInfo())
		}
		s.enqueue(true, connectionID, message)
		if message.Method == "initialized" {
			select {
			case tuiInitialized <- struct{}{}:
			default:
			}
		}
	})

	// App-server derives its base upstream User-Agent from the first initialized client.
	proxyURL := fmt.Sprintf("ws://127.0.0.1:%d", proxyPort)
	fmt.Printf("Codexdog is active for %s\n", s.options.CWD)
	fmt.Printf("State: %s\n", s.store.Path)
	tuiDone, err := s.spawnTUI(proxyURL)
	if err != nil {
		return s.startupFailure(err)
	}
	select {
	case <-tuiInitialized:
	case err := <-tuiDone:
		return s.startupFailure(fmt.Errorf("Codex TUI exited with code %d before initialization", processExitCode(err)))
	case <-time.After(10 * time.Second):
		return s.startupFailure(errors.New("timed out waiting for Codex TUI initialization"))
	}

	rpcTimeout := max(30*time.Second, s.options.ProbeTimeout)
	rpc := newJSONRPCClient(appURL, rpcTimeout)
	if err := rpc.Connect(context.Background()); err != nil {
		return s.startupFailure(err)
	}
	probeClientInfo := proxy.PrimaryClientIdentity()
	if err := rpc.InitializeWithClientInfo(context.Background(), probeClientInfo, false); err != nil {
		return s.startupFailure(err)
	}
	s.mu.Lock()
	s.rpc = rpc
	s.usageRPC = rpc
	s.mu.Unlock()
	s.probe = newProviderProbe(rpc, providerProbeOptions{CWD: s.options.CWD, Timeout: s.options.ProbeTimeout, HealthURL: s.options.HealthURL, HealthChecks: s.options.HealthChecks, Model: s.options.ProbeModel})
	queueClient := newJSONRPCClient(appURL, rpcTimeout)
	if err := queueClient.Connect(context.Background()); err != nil {
		s.logger.Log("Experimental queue API unavailable: " + err.Error())
	} else if err := queueClient.InitializeWithOptions(context.Background(), "codexdog_queue", true); err != nil {
		s.logger.Log("Experimental queue API unavailable: " + err.Error())
		queueClient.Close()
	} else {
		s.mu.Lock()
		s.queueClient = queueClient
		s.queueRPC = queueClient
		s.mu.Unlock()
	}

	control, err := startControlServerWithActions(s.stateSnapshot, s.executeRemoteCommand, func() { s.shutdown("stop requested") }, s.notifications)
	if err != nil {
		return s.startupFailure(err)
	}
	s.control = control
	s.modifyState(func(state *supervisorState) {
		state.AppServerPort = appPort
		state.ProxyPort = proxyPort
		state.ControlPort = control.Port
		state.ControlToken = control.Token
		state.Phase = "idle"
	})
	if err := s.persist(); err != nil {
		return s.startupFailure(err)
	}
	s.scheduleUsageRefresh(0)
	go s.refreshMCPStatus(context.Background())
	s.startStallWatchdog()

	if err := s.startTelegram(); err != nil {
		return s.startupFailure(fmt.Errorf("start Telegram control: %w", err))
	}
	if err := s.startWeChat(); err != nil {
		return s.startupFailure(fmt.Errorf("start WeChat control: %w", err))
	}
	stopSignals := s.installSignalHandlers()
	defer stopSignals()
	err = <-tuiDone
	if s.isShuttingDown() {
		return 0, nil
	}
	exitCode := processExitCode(err)
	s.shutdown(fmt.Sprintf("Codex TUI exited with code %d", exitCode))
	return exitCode, nil
}

func (s *supervisor) startupFailure(err error) (int, error) {
	s.logger.Log("Startup failure: " + err.Error())
	s.shutdown("startup failure")
	return 1, err
}

func (s *supervisor) startTelegram() error {
	if strings.TrimSpace(s.options.TelegramToken) == "" {
		return nil
	}
	controller, err := newTelegramController(s.options, s.executeRemoteCommand, s.logger, s.recordTelegramState)
	if err != nil {
		return err
	}
	if err := controller.Start(context.Background()); err != nil {
		return err
	}
	s.mu.Lock()
	s.telegram = controller
	s.state.TelegramEnabled = true
	s.mu.Unlock()
	_ = s.persist()
	s.logger.Log("Telegram remote control enabled")
	controller.Notify("Codexdog remote control is online.\n" + formatRemoteStatus(s.stateSnapshot()))
	return nil
}

func (s *supervisor) startWeChat() error {
	if s.options.WeChatDisabled {
		return nil
	}
	if _, err := os.Stat(s.options.WeChatCredentialsPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	controller, err := newWeChatController(s.options, s.executeRemoteCommand, s.logger, s.recordWeChatState)
	if err != nil {
		return err
	}
	if err := controller.Start(context.Background()); err != nil {
		return err
	}
	s.mu.Lock()
	s.wechat = controller
	s.state.WeChatEnabled = true
	s.mu.Unlock()
	_ = s.persist()
	s.logger.Log("WeChat iLink remote control enabled")
	if len(s.options.WeChatAllowedUsers) == 0 {
		s.logger.Log("WeChat controller is in discovery-only mode; send /uid, then configure an allowed user ID")
	}
	controller.Notify("Codexdog remote control is online.\n" + formatRemoteStatus(s.stateSnapshot()))
	return nil
}

func (s *supervisor) notifyRemoteControls(message string) {
	s.notifications.Publish(message)
	s.mu.Lock()
	telegram := s.telegram
	wechat := s.wechat
	s.mu.Unlock()
	if telegram != nil {
		telegram.Notify(message)
	}
	if wechat != nil {
		wechat.Notify(message)
	}
}

func (s *supervisor) recordTelegramState(err error) {
	s.modifyState(func(state *supervisorState) {
		state.TelegramLastUpdateAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err == nil {
			state.TelegramLastError = ""
		} else {
			state.TelegramLastError = sanitizeText(err.Error())
		}
	})
	_ = s.persist()
}

func (s *supervisor) recordWeChatState(err error) {
	s.modifyState(func(state *supervisorState) {
		state.WeChatLastUpdateAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err == nil {
			state.WeChatLastError = ""
		} else {
			state.WeChatLastError = sanitizeText(err.Error())
			if errors.Is(err, errWeChatTokenExpired) {
				state.WeChatEnabled = false
			}
		}
	})
	_ = s.persist()
}

func (s *supervisor) spawnAppServer(url string) (<-chan error, error) {
	args := []string{"app-server"}
	for _, value := range s.options.CodexConfig {
		args = append(args, "-c", value)
	}
	args = append(args, "--listen", url)
	command := exec.Command(s.options.CodexPath, args...)
	command.Dir = s.options.CWD
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	s.mu.Lock()
	processes := s.processes
	s.mu.Unlock()
	if processes == nil {
		return nil, errors.New("child process management is unavailable")
	}
	if err := processes.Start(command, true); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.appCmd = command
	s.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return done, nil
}

func (s *supervisor) spawnTUI(proxyURL string) (<-chan error, error) {
	args := []string{}
	for _, value := range s.options.CodexConfig {
		args = append(args, "-c", value)
	}
	args = append(args, "--remote", proxyURL, "-C", s.options.CWD)
	args = append(args, s.options.TUIArgs...)
	command := exec.Command(s.options.CodexPath, args...)
	command.Dir = s.options.CWD
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	s.mu.Lock()
	processes := s.processes
	s.tuiCmd = command
	s.mu.Unlock()
	if processes == nil {
		return nil, errors.New("child process management is unavailable")
	}
	if err := processes.Start(command, false); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	return done, nil
}

func (s *supervisor) enqueue(client bool, connectionID string, message rpcMessage) {
	select {
	case s.events <- supervisorEvent{client: client, connectionID: connectionID, message: message}:
	case <-s.done:
	}
}

func (s *supervisor) eventLoop() {
	for {
		select {
		case event := <-s.events:
			if s.proxyServer != nil && event.connectionID != "" && !s.proxyServer.IsPrimary(event.connectionID) {
				continue
			}
			if event.client {
				s.handleClientMessage(event.message)
			} else {
				s.handleServerMessage(event.message)
			}
		case <-s.done:
			return
		}
	}
}

func (s *supervisor) recordPrimaryClient(name, clientVersion string) {
	if name == "" && clientVersion == "" {
		return
	}
	s.modifyState(func(state *supervisorState) {
		state.PrimaryClient = name
		state.PrimaryClientVersion = clientVersion
	})
	_ = s.persist()
}

func (s *supervisor) handleClientMessage(message rpcMessage) {
	if message.Method == "turn/interrupt" {
		turnID, _ := readString(message.Params["turnId"])
		s.mu.Lock()
		s.stallGeneration++
		delete(s.watchdogInterruptedTurns, turnID)
		if pending := s.pendingTerminalErrors[turnID]; pending != nil {
			if pending.Timer != nil {
				pending.Timer.Stop()
			}
			delete(s.pendingTerminalErrors, turnID)
		}
		if len(s.pendingTerminalErrors) == 0 {
			s.state.TerminalErrorSuspectedAt = ""
		}
		waiter := s.turnTerminalWaiters[turnID]
		delete(s.turnTerminalWaiters, turnID)
		s.mu.Unlock()
		if waiter != nil {
			waiter <- ""
		}
		s.watchdog.CancelRecovery()
		s.syncStallState()
		s.logger.Log(fmt.Sprintf("User requested an interrupt%s; automatic recovery cancelled", optionalID(turnID)))
		return
	}
	if message.Method != "turn/start" {
		return
	}
	threadID, _ := readString(message.Params["threadId"])
	s.mu.Lock()
	setCurrentThread(&s.state, threadID)
	recoveryActive := s.recoveryCancel != nil
	submitting := s.submittingResume
	if !submitting && threadID != "" {
		delete(s.cyberPolicyAttempts, threadID)
		for turnID, pending := range s.pendingTerminalErrors {
			if pending.Recovery.ThreadID == threadID {
				if pending.Timer != nil {
					pending.Timer.Stop()
				}
				delete(s.pendingTerminalErrors, turnID)
			}
		}
		if len(s.pendingTerminalErrors) == 0 {
			s.state.TerminalErrorSuspectedAt = ""
		}
		s.stallGeneration++
		s.state.StallRecoveryCount = 0
		clearStallState(&s.state)
	}
	s.state.Phase = "running"
	s.state.ManualPaused = false
	s.state.ProbeAttempt = 0
	s.state.ConsecutiveProbeSuccesses = 0
	s.state.NextProbeAt = ""
	s.mu.Unlock()
	if recoveryActive && !submitting {
		s.logger.Log("User started a turn while recovery was active; automatic recovery cancelled")
		s.cancelRecovery()
		s.modifyState(func(state *supervisorState) { state.AutomaticResumeCount = 0 })
	}
	_ = s.persist()
}

func (s *supervisor) handleServerMessage(message rpcMessage) {
	if message.Method == "" || message.Params == nil {
		return
	}
	s.recordSubagentObservation(message)
	s.recordAgentManagedCollabThreads(message.Params)
	if message.Method == "account/rateLimits/updated" {
		s.scheduleUsageRefresh(0)
		return
	}
	if message.Method == "mcpServer/startupStatus/updated" {
		go s.refreshMCPStatus(context.Background())
	}
	if message.Method == "thread/tokenUsage/updated" {
		s.handleTokenUsageUpdated(message.Params)
	}
	if message.Method == "hook/completed" {
		s.handleHookCompleted(message.Params)
	}
	if message.Method == "thread/settings/updated" {
		s.handleThreadSettingsUpdated(message.Params)
		return
	}
	if message.Method == "thread/queue/changed" {
		s.handleThreadQueueChanged(message.Params)
		return
	}
	if message.Method == "thread/started" {
		s.handleThreadStarted(message.Params)
		return
	}
	if threadID, _ := readString(message.Params["threadId"]); threadID != "" && !s.threadAcceptsDirectInput(threadID) {
		s.handleAgentManagedThreadMessage(message, threadID)
		return
	}
	observation := s.watchdog.Observe(message, time.Now())
	if observation.Activity {
		s.syncStallState()
		s.mu.Lock()
		if observation.SuspicionCleared && s.state.Phase == "suspected-stall" {
			if isUserWaitReason(s.state.StallPausedReason) {
				s.state.Phase = "waiting-for-user"
			} else {
				s.state.Phase = "running"
			}
		}
		s.mu.Unlock()
		s.scheduleActivityPersist()
	}
	if message.Method != "error" && message.Method != "turn/completed" {
		s.deferPendingTerminalError(message)
	}

	switch message.Method {
	case "thread/status/changed":
		s.handleThreadStatus(message.Params)
	case "error":
		s.handleTurnError(message.Params)
	case "turn/started":
		s.handleTurnStarted(message.Params)
	case "turn/completed":
		s.handleTurnCompleted(message.Params)
	}
}

func (s *supervisor) recordAgentManagedCollabThreads(params map[string]any) {
	item, ok := asObject(params["item"])
	if !ok {
		return
	}
	typeName, _ := readString(item["type"])
	if typeName != "collabToolCall" && typeName != "collabAgentToolCall" {
		return
	}
	currentThreadID, _ := readString(params["threadId"])
	senderThreadID, _ := readString(item["senderThreadId"])
	childIDs := []string{}
	if id, ok := readString(item["newThreadId"]); ok {
		childIDs = append(childIDs, id)
	}
	if receiverID, ok := readString(item["receiverThreadId"]); ok && receiverID != "" && receiverID != currentThreadID && senderThreadID == currentThreadID {
		childIDs = append(childIDs, receiverID)
	}
	if receiverIDs, ok := item["receiverThreadIds"].([]any); ok {
		for _, value := range receiverIDs {
			receiverID, ok := readString(value)
			if ok && receiverID != "" && receiverID != currentThreadID &&
				(senderThreadID == "" || senderThreadID == currentThreadID) {
				childIDs = append(childIDs, receiverID)
			}
		}
	}
	if len(childIDs) == 0 {
		return
	}
	s.mu.Lock()
	for _, id := range childIDs {
		if id != "" {
			s.threadDirectInput[id] = false
		}
	}
	s.mu.Unlock()
}

func (s *supervisor) recordSubagentObservation(message rpcMessage) {
	if message.Params == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	changed := false
	s.mu.Lock()
	defer func() {
		s.mu.Unlock()
		if changed {
			_ = s.persist()
		}
	}()
	upsert := func(threadID string) *subagentState {
		if threadID == "" {
			return nil
		}
		for index := range s.state.Subagents {
			if s.state.Subagents[index].ThreadID == threadID {
				return &s.state.Subagents[index]
			}
		}
		s.state.Subagents = append(s.state.Subagents, subagentState{ThreadID: threadID})
		changed = true
		return &s.state.Subagents[len(s.state.Subagents)-1]
	}
	set := func(agent *subagentState, update func(*subagentState)) {
		if agent == nil {
			return
		}
		before := *agent
		update(agent)
		if *agent != before {
			changed = true
		}
	}

	if message.Method == "thread/started" {
		if thread, ok := asObject(message.Params["thread"]); ok {
			threadID, _ := readString(thread["id"])
			accepts, known := threadDirectInputCapability(thread)
			parentID, _ := readString(thread["parentThreadId"])
			if (known && !accepts) || parentID != "" {
				agent := upsert(threadID)
				set(agent, func(value *subagentState) {
					value.ParentThreadID = parentID
					value.Status = "running"
					if status := subagentStatusForThread(thread["status"]); status != "" {
						value.Status = status
					}
					value.LastActivityAt = now
				})
			}
		}
	}

	threadID, _ := readString(message.Params["threadId"])
	if threadID != "" && !s.threadAcceptsDirectInputLocked(threadID) {
		agent := upsert(threadID)
		switch message.Method {
		case "turn/started":
			if _, ok := readTurn(message.Params["turn"]); ok {
				set(agent, func(value *subagentState) { value.Status = "running"; value.Message = ""; value.LastActivityAt = now })
			}
		case "turn/completed":
			if parsed, ok := readTurn(message.Params["turn"]); ok {
				status := parsed.Status
				if status == "failed" {
					status = "errored"
				}
				set(agent, func(value *subagentState) { value.Status = status; value.LastActivityAt = now })
			}
		}
	}

	item, ok := asObject(message.Params["item"])
	if !ok {
		return
	}
	typeName, _ := readString(item["type"])
	if typeName == "subAgentActivity" {
		agentID, _ := readString(item["agentThreadId"])
		agent := upsert(agentID)
		set(agent, func(value *subagentState) {
			if value.ParentThreadID == "" {
				value.ParentThreadID = threadID
			}
			kind, _ := readString(item["kind"])
			switch kind {
			case "started", "interacted":
				value.Status = "running"
			case "interrupted":
				value.Status = "interrupted"
			case "completed":
				value.Status = "completed"
			}
			value.LastActivityAt = now
		})
		return
	}
	if typeName != "collabToolCall" && typeName != "collabAgentToolCall" {
		return
	}
	senderID, _ := readString(item["senderThreadId"])
	if senderID == "" {
		senderID = threadID
	}
	receivers := []string{}
	if value, ok := item["receiverThreadId"].(string); ok && value != "" {
		receivers = append(receivers, value)
	}
	if values, ok := item["receiverThreadIds"].([]any); ok {
		for _, value := range values {
			if receiverID, ok := readString(value); ok && receiverID != "" {
				receivers = append(receivers, receiverID)
			}
		}
	}
	agentsStates, _ := item["agentsStates"].(map[string]any)
	tool, _ := readString(item["tool"])
	model, _ := readString(item["model"])
	reasoning, _ := readString(item["reasoningEffort"])
	callStatus, _ := readString(item["status"])
	for _, receiverID := range receivers {
		agent := upsert(receiverID)
		set(agent, func(value *subagentState) {
			if value.ParentThreadID == "" {
				value.ParentThreadID = senderID
			}
			value.Tool, value.Model, value.ReasoningEffort = tool, model, reasoning
			if callStatus == "interrupted" {
				value.Status = "interrupted"
			} else if callStatus == "completed" || callStatus == "failed" {
				value.Status = callStatus
			} else if value.Status == "" {
				value.Status = "running"
			}
			if statusObject, exists := agentsStates[receiverID]; exists {
				if status := subagentStatusForThread(statusObject); status != "" {
					value.Status = status
				}
				if statusMap, ok := asObject(statusObject); ok {
					value.Message, _ = readString(statusMap["message"])
				}
			}
			value.LastActivityAt = now
		})
	}
}

func (s *supervisor) threadAcceptsDirectInputLocked(threadID string) bool {
	accepts, known := s.threadDirectInput[threadID]
	return !known || accepts
}

func (s *supervisor) handleThreadStarted(params map[string]any) {
	threadObject, ok := asObject(params["thread"])
	if !ok {
		return
	}
	id, ok := readString(threadObject["id"])
	if !ok || id == "" {
		return
	}
	acceptsDirectInput, capabilityKnown := threadDirectInputCapability(threadObject)
	s.mu.Lock()
	if capabilityKnown {
		s.threadDirectInput[id] = acceptsDirectInput
	}
	s.mu.Unlock()
	if capabilityKnown && !acceptsDirectInput {
		s.logger.Log("Observed agent-managed thread " + id + "; direct recovery is disabled")
		return
	}
	s.modifyState(func(state *supervisorState) {
		setCurrentThread(state, id)
		updateThreadMetadata(state, threadObject)
	})
	_ = s.persist()
	s.scheduleUsageRefresh(0)
}

func (s *supervisor) handleThreadSettingsUpdated(params map[string]any) {
	threadID, settings, ok := readThreadSettings(params)
	if !ok || !s.threadAcceptsDirectInput(threadID) {
		return
	}
	s.mu.Lock()
	if s.state.CurrentThreadID != "" && s.state.CurrentThreadID != threadID {
		s.mu.Unlock()
		return
	}
	setCurrentThread(&s.state, threadID)
	applyThreadSettings(&s.state, settings)
	s.mu.Unlock()
	_ = s.persist()
}

func (s *supervisor) handleThreadQueueChanged(params map[string]any) {
	threadID, _ := readString(params["threadId"])
	s.mu.Lock()
	if threadID == "" || threadID != s.state.CurrentThreadID {
		s.mu.Unlock()
		return
	}
	s.state.QueueUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Unlock()
	_ = s.persist()
}

func updateThreadMetadata(state *supervisorState, threadObject map[string]any) {
	if sessionID, ok := readString(threadObject["sessionId"]); ok {
		state.SessionID = sessionID
	}
	if projectID, ok := readString(threadObject["projectId"]); ok {
		state.ProjectID = projectID
	}
	if cwd, ok := readString(threadObject["cwd"]); ok && cwd != "" {
		state.EffectiveCWD = cwd
	}
	if settings, ok := asObject(threadObject["threadSettings"]); ok {
		_, parsed, _ := readThreadSettings(map[string]any{"threadId": state.CurrentThreadID, "threadSettings": settings})
		applyThreadSettings(state, parsed)
	}
}

func applyThreadSettings(state *supervisorState, settings threadSettings) {
	if settings.CWD != "" {
		state.EffectiveCWD = settings.CWD
	}
	state.ActivePermissionProfile = settings.PermissionProfile
	state.ActivePermissionProfileExtends = settings.PermissionProfileBase
	if settings.ApprovalPolicy != "" {
		state.ApprovalPolicy = settings.ApprovalPolicy
	}
	if settings.ApprovalsReviewer != "" {
		state.ApprovalsReviewer = settings.ApprovalsReviewer
	}
	if settings.SandboxPolicy != "" {
		state.SandboxPolicy = settings.SandboxPolicy
	}
	if settings.Model != "" {
		state.Model = settings.Model
	}
	if settings.ModelProvider != "" {
		state.ModelProvider = settings.ModelProvider
	}
}

func threadDirectInputCapability(threadObject map[string]any) (bool, bool) {
	if value, ok := readBool(threadObject["canAcceptDirectInput"]); ok {
		return value, true
	}
	if parentID, ok := readString(threadObject["parentThreadId"]); ok && parentID != "" {
		return false, true
	}
	if isSubagentThreadSource(threadObject["source"]) || isSubagentThreadSource(threadObject["threadSource"]) {
		return false, true
	}
	return true, false
}

func isSubagentThreadSource(value any) bool {
	if source, ok := readString(value); ok {
		return strings.HasPrefix(strings.ToLower(source), "subagent")
	}
	if source, ok := asObject(value); ok {
		for name := range source {
			if strings.HasPrefix(strings.ToLower(name), "subagent") {
				return true
			}
		}
	}
	return false
}

func (s *supervisor) threadAcceptsDirectInput(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	accepts, known := s.threadDirectInput[threadID]
	return !known || accepts
}

func (s *supervisor) handleAgentManagedThreadMessage(message rpcMessage, threadID string) {
	if message.Method != "turn/completed" {
		return
	}
	parsed, ok := readTurn(message.Params["turn"])
	if ok && parsed.Status == "failed" {
		s.logger.Log(fmt.Sprintf("Turn %s failed on agent-managed thread %s; parent orchestration owns recovery", parsed.ID, threadID))
	}
}

func (s *supervisor) handleTurnError(params map[string]any) {
	threadID, threadOK := readString(params["threadId"])
	turnID, turnOK := readString(params["turnId"])
	parsed, errorOK := readTurnError(params["error"])
	if !threadOK || !turnOK || !errorOK {
		return
	}
	willRetry, _ := readBool(params["willRetry"])
	failure := classifyFailure(parsed)
	s.mu.Lock()
	if s.handledTurns[turnID] {
		s.mu.Unlock()
		return
	}
	s.turnErrors[turnID] = parsed
	if willRetry {
		s.turnHadRetryableError[turnID] = true
	}
	hadRetryableError := s.turnHadRetryableError[turnID]
	manualPaused := s.state.ManualPaused
	s.mu.Unlock()
	if manualPaused {
		s.logger.Log("Turn " + turnID + " error ignored while manually paused")
		return
	}
	if !willRetry {
		failure = classifyFailureAfterRetries(parsed, hadRetryableError)
	}
	s.logger.Log(fmt.Sprintf("Turn %s error (willRetry=%t, disposition=%s, code=%s): %s", turnID, willRetry, failure.Disposition, failure.Code, parsed.Message))

	if willRetry {
		s.cancelPendingTerminalError(turnID)
		return
	}
	s.schedulePendingTerminalError(recoveryContext{ThreadID: threadID, FailedTurnID: turnID, Failure: failure})
}

func (s *supervisor) schedulePendingTerminalError(recovery recoveryContext) {
	s.mu.Lock()
	if s.shuttingDown || s.state.ManualPaused || s.handledTurns[recovery.FailedTurnID] {
		s.mu.Unlock()
		return
	}
	pending := s.pendingTerminalErrors[recovery.FailedTurnID]
	if pending == nil {
		pending = &pendingTerminalError{Recovery: recovery}
		s.pendingTerminalErrors[recovery.FailedTurnID] = pending
		s.state.TerminalErrorSuspectedAt = atomicTime(time.Now())
	} else {
		pending.Recovery = recovery
	}
	if !pending.Interrupting && pending.Timer == nil {
		s.armPendingTerminalErrorLocked(pending)
	}
	if s.state.Phase != "needs-attention" && s.state.Phase != "stopped" {
		s.state.Phase = "confirming-error"
	}
	s.state.LastError = sanitizeText(formatFailure(recovery.Failure))
	s.mu.Unlock()
	_ = s.persist()
	s.logger.Log(fmt.Sprintf("Waiting %s for terminal event after error on turn %s", s.options.TerminalErrorGrace, recovery.FailedTurnID))
}

func (s *supervisor) armPendingTerminalErrorLocked(pending *pendingTerminalError) {
	pending.Generation++
	generation := pending.Generation
	turnID := pending.Recovery.FailedTurnID
	pending.Timer = time.AfterFunc(s.options.TerminalErrorGrace, func() {
		s.reconcilePendingTerminalError(turnID, generation)
	})
}

func (s *supervisor) deferPendingTerminalError(message rpcMessage) {
	threadID, _ := readString(message.Params["threadId"])
	turnID, _ := readString(message.Params["turnId"])
	if turnID == "" {
		if nested, ok := asObject(message.Params["turn"]); ok {
			turnID, _ = readString(nested["id"])
		}
	}
	s.mu.Lock()
	deferred := false
	for _, pending := range s.pendingTerminalErrors {
		if s.state.ManualPaused || s.state.Phase == "needs-attention" || s.state.Phase == "stopped" {
			break
		}
		if pending.Interrupting || !terminalErrorMatchesMessage(pending.Recovery, threadID, turnID) {
			continue
		}
		if pending.Timer != nil {
			pending.Timer.Stop()
			pending.Timer = nil
		}
		s.armPendingTerminalErrorLocked(pending)
		deferred = true
	}
	s.mu.Unlock()
	if deferred {
		s.logger.Log("Turn activity deferred terminal-error reconciliation")
	}
}

func terminalErrorMatchesMessage(recovery recoveryContext, threadID, turnID string) bool {
	if turnID != "" {
		return turnID == recovery.FailedTurnID
	}
	return threadID != "" && threadID == recovery.ThreadID
}

func (s *supervisor) cancelPendingTerminalError(turnID string) {
	s.mu.Lock()
	pending := s.pendingTerminalErrors[turnID]
	changed := pending != nil
	if pending != nil {
		if pending.Timer != nil {
			pending.Timer.Stop()
		}
		delete(s.pendingTerminalErrors, turnID)
	}
	if len(s.pendingTerminalErrors) == 0 {
		s.state.TerminalErrorSuspectedAt = ""
		if s.state.Phase == "confirming-error" {
			s.state.Phase = "running"
		}
	}
	s.mu.Unlock()
	if changed {
		_ = s.persist()
	}
}

func (s *supervisor) handleTurnStarted(params map[string]any) {
	threadID, _ := readString(params["threadId"])
	parsed, ok := readTurn(params["turn"])
	manualPaused := false
	if threadID != "" && ok {
		s.watchdog.StartTurn(threadID, parsed.ID, time.Now())
		s.mu.Lock()
		manualPaused = s.state.ManualPaused
		for turnID, pending := range s.pendingTerminalErrors {
			if pending.Recovery.ThreadID == threadID && turnID != parsed.ID {
				if pending.Timer != nil {
					pending.Timer.Stop()
				}
				delete(s.pendingTerminalErrors, turnID)
			}
		}
		if len(s.pendingTerminalErrors) == 0 {
			s.state.TerminalErrorSuspectedAt = ""
		}
		s.mu.Unlock()
	}
	pausedNow := manualPaused
	s.modifyState(func(state *supervisorState) {
		setCurrentThread(state, threadID)
		if ok {
			state.ActiveTurnID = parsed.ID
		}
		pausedNow = pausedNow || state.ManualPaused
		if pausedNow {
			state.Phase = "paused"
		} else {
			state.Phase = "running"
		}
	})
	s.syncStallState()
	_ = s.persist()
	s.scheduleUsageRefresh(time.Second)
	if pausedNow && threadID != "" && ok {
		s.logger.Log("Turn " + parsed.ID + " started while manually paused; interrupting it")
		s.requestPausedTurnInterrupt(threadID, parsed.ID)
	} else if ok {
		s.notifyRemoteControls(fmt.Sprintf("Turn %s started on thread %s.", parsed.ID, valueOrDash(threadID)))
	}
}

func (s *supervisor) requestPausedTurnInterrupt(threadID, turnID string) {
	s.mu.Lock()
	if s.pauseInterruptingTurns[turnID] || s.handledTurns[turnID] {
		s.mu.Unlock()
		return
	}
	if s.pauseInterruptingTurns == nil {
		s.pauseInterruptingTurns = map[string]bool{}
	}
	s.pauseInterruptingTurns[turnID] = true
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.pauseInterruptingTurns, turnID)
			s.mu.Unlock()
		}()
		s.interruptPausedTurn(threadID, turnID)
	}()
}

func (s *supervisor) interruptPausedTurn(threadID, turnID string) {
	if s.proxy == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), max(5*time.Second, s.options.StallInterruptTimeout))
	_, err := s.proxy.Request(ctx, "turn/interrupt", map[string]any{"threadId": threadID, "turnId": turnID})
	cancel()
	if err != nil {
		s.logger.Log("Failed to interrupt a turn started while paused: " + err.Error())
		return
	}
	s.mu.Lock()
	if s.handledTurns == nil {
		s.handledTurns = map[string]bool{}
	}
	s.handledTurns[turnID] = true
	if s.state.ActiveTurnID == turnID {
		s.state.ActiveTurnID = ""
	}
	s.state.ManualPaused = true
	s.state.Phase = "paused"
	s.mu.Unlock()
	s.watchdog.CompleteTurn(turnID)
	s.syncStallState()
	_ = s.persist()
	s.notifyRemoteControls("Turn " + turnID + " started while paused and was interrupted.")
}

func (s *supervisor) recordStartedTurnFromRequest(threadID, turnID string) bool {
	s.watchdog.StartTurn(threadID, turnID, time.Now())
	s.syncStallState()
	paused := false
	s.modifyState(func(state *supervisorState) {
		paused = state.ManualPaused
		if paused {
			state.Phase = "paused"
		} else {
			state.Phase = "running"
		}
		state.ActiveTurnID = turnID
		setCurrentThread(state, threadID)
		state.ProbeAttempt = 0
		state.ConsecutiveProbeSuccesses = 0
	})
	_ = s.persist()
	s.scheduleUsageRefresh(time.Second)
	if paused {
		s.logger.Log("Turn " + turnID + " was returned while manually paused; interrupting it")
		s.requestPausedTurnInterrupt(threadID, turnID)
	}
	return paused
}

func (s *supervisor) handleTurnCompleted(params map[string]any) {
	threadID, threadOK := readString(params["threadId"])
	parsed, turnOK := readTurn(params["turn"])
	if !threadOK || !turnOK {
		return
	}
	defer s.scheduleUsageRefresh(250 * time.Millisecond)
	s.mu.Lock()
	if s.handledTurns[parsed.ID] {
		s.mu.Unlock()
		return
	}
	s.handledTurns[parsed.ID] = true
	pendingError := s.pendingTerminalErrors[parsed.ID]
	if pendingError != nil {
		if pendingError.Timer != nil {
			pendingError.Timer.Stop()
		}
		delete(s.pendingTerminalErrors, parsed.ID)
	}
	if len(s.pendingTerminalErrors) == 0 {
		s.state.TerminalErrorSuspectedAt = ""
	}
	waiter := s.turnTerminalWaiters[parsed.ID]
	delete(s.turnTerminalWaiters, parsed.ID)
	wasWatchdog := s.watchdogInterruptedTurns[parsed.ID]
	delete(s.watchdogInterruptedTurns, parsed.ID)
	manualPaused := s.state.ManualPaused
	storedError, hasStoredError := s.turnErrors[parsed.ID]
	delete(s.turnErrors, parsed.ID)
	hadRetryableError := s.turnHadRetryableError[parsed.ID]
	delete(s.turnHadRetryableError, parsed.ID)
	setCurrentThread(&s.state, threadID)
	s.state.ActiveTurnID = ""
	s.mu.Unlock()
	if waiter != nil {
		waiter <- parsed.Status
	}
	s.watchdog.CompleteTurn(parsed.ID)
	s.syncStallState()

	if parsed.Status == "completed" {
		s.mu.Lock()
		manualPaused = manualPaused || s.state.ManualPaused
		s.stallGeneration++
		delete(s.cyberPolicyAttempts, threadID)
		if manualPaused {
			s.state.Phase = "paused"
			s.state.ManualPaused = true
		} else {
			s.state.Phase = "idle"
		}
		s.state.AutomaticResumeCount = 0
		s.state.StallRecoveryCount = 0
		s.state.ProbeAttempt = 0
		s.state.ConsecutiveProbeSuccesses = 0
		s.state.LastError = ""
		s.state.NextProbeAt = ""
		s.mu.Unlock()
		s.cancelRecovery()
		_ = s.persist()
		s.logger.Log("Turn " + parsed.ID + " completed")
		s.notifyRemoteControls("Turn " + parsed.ID + " completed.")
		return
	}

	if parsed.Status == "interrupted" {
		s.mu.Lock()
		delete(s.cyberPolicyAttempts, threadID)
		s.mu.Unlock()
		s.cancelRecovery()
		s.mu.Lock()
		manualPaused = manualPaused || s.state.ManualPaused
		s.mu.Unlock()
		if manualPaused {
			s.modifyState(func(state *supervisorState) {
				state.Phase = "paused"
				state.ManualPaused = true
			})
			_ = s.persist()
			s.logger.Log("Turn " + parsed.ID + " was interrupted by a manual pause")
			s.notifyRemoteControls("Turn " + parsed.ID + " is paused.")
			return
		}
		if pendingError != nil && pendingError.Interrupting {
			s.modifyState(func(state *supervisorState) { state.Phase = "provider-down" })
			_ = s.persist()
			s.logger.Log("Terminal-error interrupt completed for turn " + parsed.ID)
			s.startRecovery(pendingError.Recovery)
			return
		}
		if wasWatchdog {
			if waiter != nil {
				s.modifyState(func(state *supervisorState) { state.Phase = "resuming" })
				_ = s.persist()
				s.logger.Log("Watchdog interrupt completed for stalled turn " + parsed.ID)
			} else {
				_ = s.setAttention("Watchdog interrupt completed late for " + parsed.ID + "; manual continuation is required")
			}
			return
		}
		s.mu.Lock()
		s.stallGeneration++
		s.state.Phase = "idle"
		s.state.AutomaticResumeCount = 0
		s.state.StallRecoveryCount = 0
		s.state.NextProbeAt = ""
		s.mu.Unlock()
		_ = s.persist()
		s.logger.Log("Turn " + parsed.ID + " was interrupted; no recovery scheduled")
		s.notifyRemoteControls("Turn " + parsed.ID + " was interrupted; no automatic recovery was scheduled.")
		return
	}

	failureError := turnError{Message: "Codex turn failed"}
	if parsed.Error != nil {
		failureError = *parsed.Error
	} else if hasStoredError {
		failureError = storedError
	}
	failure := classifyFailureAfterRetries(failureError, hadRetryableError)
	s.mu.Lock()
	manualPaused = manualPaused || s.state.ManualPaused
	s.mu.Unlock()
	if manualPaused {
		s.cancelRecovery()
		s.modifyState(func(state *supervisorState) {
			state.Phase = "paused"
			state.ManualPaused = true
			state.LastFailedTurnID = parsed.ID
			state.LastError = sanitizeText(formatFailure(failure))
		})
		_ = s.persist()
		s.logger.Log("Turn " + parsed.ID + " failed while manually paused; recovery suppressed")
		return
	}
	s.modifyState(func(state *supervisorState) {
		state.LastFailedTurnID = parsed.ID
		state.LastError = sanitizeText(formatFailure(failure))
	})
	s.notifyRemoteControls("Turn " + parsed.ID + " failed: " + sanitizeText(formatFailure(failure)))
	context := recoveryContext{ThreadID: threadID, FailedTurnID: parsed.ID, Failure: failure}
	if failure.Code == "cyberPolicy" {
		go s.recoverCyberPolicy(context)
	} else {
		s.startRecovery(context)
	}
}

func (s *supervisor) handleThreadStatus(params map[string]any) {
	status, ok := asObject(params["status"])
	if !ok {
		return
	}
	typeName, _ := readString(status["type"])
	flags := stringSlice(status["activeFlags"])
	waiting := contains(flags, "waitingOnApproval") || contains(flags, "waitingOnUserInput")
	s.mu.Lock()
	changed := false
	if s.state.ManualPaused {
		// A remote pause owns the visible phase until an explicit resume.
		if s.state.Phase != "paused" {
			s.state.Phase = "paused"
			changed = true
		}
	} else if typeName == "active" && waiting {
		s.state.Phase = "waiting-for-user"
		changed = true
	} else if typeName == "active" && s.state.Phase == "waiting-for-user" {
		s.state.Phase = "running"
		changed = true
	}
	s.mu.Unlock()
	if changed {
		_ = s.persist()
	}
}

func (s *supervisor) reconcilePendingTerminalError(turnID string, generation uint64) {
	s.mu.Lock()
	pending := s.pendingTerminalErrors[turnID]
	if pending == nil || pending.Generation != generation || pending.Interrupting || s.shuttingDown || s.state.ManualPaused || s.handledTurns[turnID] || s.state.Phase == "needs-attention" || s.state.Phase == "stopped" {
		s.mu.Unlock()
		return
	}
	pending.Timer = nil
	if s.submittingResume || s.recoveryCancel != nil || s.stallCheckInFlight {
		s.armPendingTerminalErrorLocked(pending)
		s.mu.Unlock()
		return
	}
	if s.state.ActiveTurnID != "" && s.state.ActiveTurnID != turnID {
		delete(s.pendingTerminalErrors, turnID)
		if len(s.pendingTerminalErrors) == 0 {
			s.state.TerminalErrorSuspectedAt = ""
		}
		s.mu.Unlock()
		return
	}
	proxy := s.proxy
	recovery := pending.Recovery
	s.mu.Unlock()
	if proxy == nil {
		s.failPendingTerminalError(turnID, generation, errors.New("TUI proxy is unavailable"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.options.StallInterruptTimeout)
	value, err := proxy.Request(ctx, "thread/read", map[string]any{"threadId": recovery.ThreadID, "includeTurns": false})
	cancel()
	if err != nil {
		s.failPendingTerminalError(turnID, generation, fmt.Errorf("thread status check failed: %w", err))
		return
	}
	s.mu.Lock()
	pending = s.pendingTerminalErrors[turnID]
	valid := pending != nil && pending.Generation == generation && !pending.Interrupting && !s.handledTurns[turnID]
	s.mu.Unlock()
	if !valid {
		return
	}
	object, _ := asObject(value)
	status, ok := readThreadRuntimeStatus(object["thread"])
	if !ok {
		s.failPendingTerminalError(turnID, generation, errors.New("Codex did not return the errored thread status"))
		return
	}
	if s.isManuallyPaused() {
		s.watchdog.CancelRecovery()
		return
	}
	if status.Type != "active" {
		s.recoverPendingErrorWithoutTerminal(turnID, generation, false)
		return
	}
	waiting := []string{}
	for _, flag := range status.ActiveFlags {
		if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
			waiting = append(waiting, flag)
		}
	}
	if len(waiting) > 0 {
		s.cancelPendingTerminalError(turnID)
		s.watchdog.SetWaitingFlags(recovery.ThreadID, waiting, time.Now())
		s.syncStallState()
		s.modifyState(func(state *supervisorState) { state.Phase = "waiting-for-user" })
		_ = s.persist()
		s.logger.Log(fmt.Sprintf("Transient-error reconciliation deferred because thread %s is %s", recovery.ThreadID, waiting[0]))
		return
	}

	waiter := make(chan string, 1)
	s.mu.Lock()
	pending = s.pendingTerminalErrors[turnID]
	if pending == nil || pending.Generation != generation || pending.Interrupting || s.handledTurns[turnID] {
		s.mu.Unlock()
		return
	}
	pending.Interrupting = true
	pending.Generation++
	s.turnTerminalWaiters[turnID] = waiter
	s.state.Phase = "interrupting-error"
	s.mu.Unlock()
	_ = s.persist()
	s.logger.Log("Interrupting turn " + turnID + " after an error produced no terminal event")

	ctx, cancel = context.WithTimeout(context.Background(), s.options.StallInterruptTimeout)
	_, err = proxy.Request(ctx, "turn/interrupt", map[string]any{"threadId": recovery.ThreadID, "turnId": turnID})
	cancel()
	if err != nil {
		s.mu.Lock()
		delete(s.turnTerminalWaiters, turnID)
		s.mu.Unlock()
		s.failPendingTerminalError(turnID, 0, fmt.Errorf("turn interrupt failed: %w", err))
		return
	}
	terminalStatus := s.waitForInterruptedTurn(recovery.ThreadID, turnID, waiter)
	if terminalStatus == "" {
		s.mu.Lock()
		delete(s.turnTerminalWaiters, turnID)
		s.mu.Unlock()
		s.reconcileInterruptedTerminalError(turnID)
	}
}

func (s *supervisor) reconcileInterruptedTerminalError(turnID string) {
	s.mu.Lock()
	pending := s.pendingTerminalErrors[turnID]
	if pending == nil || !pending.Interrupting || s.handledTurns[turnID] || s.shuttingDown || s.state.ManualPaused {
		s.mu.Unlock()
		return
	}
	proxy := s.proxy
	recovery := pending.Recovery
	s.mu.Unlock()
	if proxy == nil {
		s.failPendingTerminalError(turnID, 0, errors.New("TUI proxy is unavailable after turn interrupt"))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.options.StallInterruptTimeout)
	value, err := proxy.Request(ctx, "thread/read", map[string]any{"threadId": recovery.ThreadID, "includeTurns": false})
	cancel()
	if err != nil {
		s.failPendingTerminalError(turnID, 0, fmt.Errorf("post-interrupt thread status check failed: %w", err))
		return
	}
	s.mu.Lock()
	pending = s.pendingTerminalErrors[turnID]
	valid := pending != nil && pending.Interrupting && !s.handledTurns[turnID]
	s.mu.Unlock()
	if !valid {
		return
	}
	object, _ := asObject(value)
	status, ok := readThreadRuntimeStatus(object["thread"])
	if !ok {
		s.failPendingTerminalError(turnID, 0, errors.New("Codex did not return the interrupted thread status"))
		return
	}
	if status.Type == "active" {
		s.failPendingTerminalError(turnID, 0, fmt.Errorf("Codex still reports interrupted turn %s as active", turnID))
		return
	}
	s.logger.Log("Interrupted turn " + turnID + " became terminal without turn/completed")
	s.recoverPendingErrorWithoutTerminal(turnID, pending.Generation, true)
}

func (s *supervisor) recoverPendingErrorWithoutTerminal(turnID string, generation uint64, interrupting bool) {
	s.mu.Lock()
	pending := s.pendingTerminalErrors[turnID]
	if pending == nil || pending.Generation != generation || pending.Interrupting != interrupting || s.handledTurns[turnID] || s.state.ManualPaused {
		s.mu.Unlock()
		return
	}
	if pending.Timer != nil {
		pending.Timer.Stop()
	}
	delete(s.pendingTerminalErrors, turnID)
	s.handledTurns[turnID] = true
	delete(s.turnErrors, turnID)
	delete(s.turnHadRetryableError, turnID)
	if s.state.ActiveTurnID == turnID {
		s.state.ActiveTurnID = ""
	}
	s.state.LastFailedTurnID = turnID
	s.state.LastError = sanitizeText(formatFailure(pending.Recovery.Failure))
	if len(s.pendingTerminalErrors) == 0 {
		s.state.TerminalErrorSuspectedAt = ""
	}
	recovery := pending.Recovery
	s.mu.Unlock()
	s.watchdog.CompleteTurn(turnID)
	s.syncStallState()
	_ = s.persist()
	s.logger.Log("Recovering error for turn " + turnID + " after thread status became terminal without turn/completed")
	s.startRecovery(recovery)
}

func (s *supervisor) failPendingTerminalError(turnID string, generation uint64, err error) {
	s.mu.Lock()
	pending := s.pendingTerminalErrors[turnID]
	valid := pending != nil && (generation == 0 || pending.Generation == generation)
	s.mu.Unlock()
	if valid && !s.isShuttingDown() {
		_ = s.setAttention("Transient-error reconciliation failed: " + err.Error())
	}
}

func (s *supervisor) startStallWatchdog() {
	if !s.watchdog.Enabled() {
		return
	}
	interval := max(time.Second, min(5*time.Second, s.options.StallConfirm/2))
	s.logger.Log(fmt.Sprintf("Stall watchdog enabled (timeout=%s, confirmation=%s)", s.options.StallTimeout, s.options.StallConfirm))
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				s.evaluateStall(now)
			case <-s.done:
				return
			}
		}
	}()
}

func (s *supervisor) evaluateStall(now time.Time) {
	s.mu.Lock()
	if s.shuttingDown || s.state.ManualPaused || s.stallCheckInFlight || s.submittingResume || s.recoveryCancel != nil || len(s.pendingTerminalErrors) > 0 || s.state.Phase == "needs-attention" || s.state.Phase == "stopped" {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	decision, ok := s.watchdog.Evaluate(now)
	s.syncStallState()
	if !ok {
		return
	}
	if decision.Kind == "suspected" {
		s.modifyState(func(state *supervisorState) { state.Phase = "suspected-stall" })
		_ = s.persist()
		s.logger.Log(fmt.Sprintf("Turn %s may be stalled after %s without activity", decision.Context.TurnID, decision.Context.Idle))
		return
	}
	s.mu.Lock()
	s.stallCheckInFlight = true
	s.stallGeneration++
	generation := s.stallGeneration
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			s.stallCheckInFlight = false
			s.mu.Unlock()
		}()
		s.confirmAndRecoverStall(decision.Context, generation)
	}()
}

func (s *supervisor) confirmAndRecoverStall(stall stallContext, generation uint64) {
	if s.isManuallyPaused() {
		s.watchdog.CancelRecovery()
		return
	}
	if !s.threadAcceptsDirectInput(stall.ThreadID) {
		s.watchdog.CompleteTurn(stall.TurnID)
		s.syncStallState()
		s.logger.Log("Skipped stall recovery for agent-managed thread " + stall.ThreadID)
		return
	}
	if s.proxy == nil {
		s.watchdog.CancelRecovery()
		_ = s.setAttention("Stall recovery is unavailable before the TUI proxy starts")
		return
	}
	s.mu.Lock()
	stallCount := s.state.StallRecoveryCount
	autoCount := s.state.AutomaticResumeCount
	s.mu.Unlock()
	if stallCount >= s.options.MaxStallResumes {
		_ = s.setAttention(fmt.Sprintf("Stalled-turn resume limit (%d) reached for %s", s.options.MaxStallResumes, stall.ThreadID))
		return
	}
	if autoCount >= s.options.MaxAutoResumes {
		_ = s.setAttention(fmt.Sprintf("Automatic resume limit (%d) reached for %s", s.options.MaxAutoResumes, stall.ThreadID))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.options.StallInterruptTimeout)
	value, err := s.proxy.Request(ctx, "thread/read", map[string]any{"threadId": stall.ThreadID, "includeTurns": false})
	cancel()
	if err != nil {
		s.failStallRecovery(generation, err)
		return
	}
	if s.isManuallyPaused() {
		s.watchdog.CancelRecovery()
		return
	}
	if !s.currentStallRecovery(stall, generation) {
		s.watchdog.CancelRecovery()
		s.syncStallState()
		return
	}
	object, _ := asObject(value)
	status, ok := readThreadRuntimeStatus(object["thread"])
	if !ok {
		s.failStallRecovery(generation, errors.New("Codex did not return the stalled thread status"))
		return
	}
	if status.Type != "active" {
		s.watchdog.CompleteTurn(stall.TurnID)
		s.syncStallState()
		s.modifyState(func(state *supervisorState) { state.ActiveTurnID = "" })
		if status.Type == "idle" {
			s.modifyState(func(state *supervisorState) { state.Phase = "idle" })
			_ = s.persist()
			s.logger.Log(fmt.Sprintf("Stall confirmation cancelled because thread %s is idle", stall.ThreadID))
		} else {
			_ = s.setAttention(fmt.Sprintf("Stall confirmation found thread %s in %s state", stall.ThreadID, status.Type))
		}
		return
	}
	waiting := []string{}
	for _, flag := range status.ActiveFlags {
		if flag == "waitingOnApproval" || flag == "waitingOnUserInput" {
			waiting = append(waiting, flag)
		}
	}
	if len(waiting) > 0 {
		s.watchdog.SetWaitingFlags(stall.ThreadID, waiting, time.Now())
		s.syncStallState()
		s.modifyState(func(state *supervisorState) { state.Phase = "waiting-for-user" })
		_ = s.persist()
		s.logger.Log(fmt.Sprintf("Stall confirmation cancelled because thread %s is %s", stall.ThreadID, waiting[0]))
		return
	}
	if !s.currentStallRecovery(stall, generation) {
		s.watchdog.CancelRecovery()
		s.syncStallState()
		return
	}
	if s.isManuallyPaused() {
		s.watchdog.CancelRecovery()
		return
	}

	s.modifyState(func(state *supervisorState) { state.Phase = "interrupting-stall" })
	_ = s.persist()
	s.logger.Log("Interrupting confirmed stalled turn " + stall.TurnID)
	waiter := make(chan string, 1)
	s.mu.Lock()
	s.watchdogInterruptedTurns[stall.TurnID] = true
	s.turnTerminalWaiters[stall.TurnID] = waiter
	s.mu.Unlock()
	ctx, cancel = context.WithTimeout(context.Background(), s.options.StallInterruptTimeout)
	_, err = s.proxy.Request(ctx, "turn/interrupt", map[string]any{"threadId": stall.ThreadID, "turnId": stall.TurnID})
	cancel()
	if err != nil {
		s.mu.Lock()
		delete(s.turnTerminalWaiters, stall.TurnID)
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(strings.ToLower(err.Error()), "timed out") {
			delete(s.watchdogInterruptedTurns, stall.TurnID)
		}
		s.mu.Unlock()
		s.failStallRecovery(generation, err)
		return
	}
	terminal := s.waitForInterruptedTurn(stall.ThreadID, stall.TurnID, waiter)
	if terminal == "" {
		s.mu.Lock()
		delete(s.turnTerminalWaiters, stall.TurnID)
		s.mu.Unlock()
	}
	if !s.stallGenerationMatches(generation) {
		return
	}
	if terminal == "" {
		s.failStallRecovery(generation, fmt.Errorf("Codex did not finish interrupted turn %s", stall.TurnID))
		return
	}
	if terminal != "interrupted" {
		return
	}
	s.resumeStalledThread(stall, generation)
}

// waitForInterruptedTurn extends the normal interrupt deadline while Codex is
// running a synchronous Interrupt hook. Without this, a valid slow hook can
// be mistaken for a broken app-server turn and trigger duplicate recovery.
func (s *supervisor) waitForInterruptedTurn(threadID, turnID string, waiter <-chan string) string {
	base := s.options.StallInterruptTimeout
	if base <= 0 {
		base = 15 * time.Second
	}
	timer := time.NewTimer(base)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	extended := false
	for {
		select {
		case status := <-waiter:
			return status
		case <-timer.C:
			if !extended && s.watchdog.InterruptHookActive(threadID, turnID) {
				extended = true
				timer.Reset(interruptHookWaitTimeout)
				continue
			}
			return ""
		case <-ticker.C:
			// A hook may start immediately after turn/interrupt returns. The
			// timer path checks again at the deadline, so no extra state is needed.
		}
	}
}

func (s *supervisor) resumeStalledThread(stall stallContext, generation uint64) {
	if s.proxy == nil || !s.stallGenerationMatches(generation) {
		return
	}
	s.mu.Lock()
	s.submittingResume = true
	s.state.Phase = "resuming"
	s.state.ResumeRequestedForTurnID = stall.TurnID
	s.state.AutomaticResumeCount++
	s.state.StallRecoveryCount++
	s.state.NextProbeAt = ""
	count := s.state.StallRecoveryCount
	s.mu.Unlock()
	_ = s.persist()
	defer func() {
		s.mu.Lock()
		s.submittingResume = false
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.proxy.Request(ctx, "thread/resume", map[string]any{"threadId": stall.ThreadID}); err != nil {
		s.failStallRecovery(generation, err)
		return
	}
	value, err := s.proxy.Request(ctx, "turn/start", map[string]any{
		"threadId":            stall.ThreadID,
		"input":               []any{map[string]any{"type": "text", "text": stallContinuationPrompt}},
		"clientUserMessageId": fmt.Sprintf("codexdog:stall:%d:%s", count, stall.TurnID),
	})
	if err != nil {
		s.failStallRecovery(generation, err)
		return
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		s.failStallRecovery(generation, errors.New("Codex did not return a stalled-turn continuation"))
		return
	}
	if s.recordStartedTurnFromRequest(stall.ThreadID, started.ID) {
		return
	}
	s.logger.Log(fmt.Sprintf("Resumed stalled thread %s as turn %s after interrupting %s", stall.ThreadID, started.ID, stall.TurnID))
}

func (s *supervisor) failStallRecovery(generation uint64, err error) {
	if s.stallGenerationMatches(generation) && !s.isShuttingDown() {
		s.watchdog.CancelRecovery()
		s.syncStallState()
		_ = s.setAttention("Stall recovery failed: " + err.Error())
	}
}

func (s *supervisor) currentStallRecovery(stall stallContext, generation uint64) bool {
	return s.stallGenerationMatches(generation) && s.watchdog.IsCurrent(stall)
}

func (s *supervisor) stallGenerationMatches(generation uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stallGeneration == generation
}

func (s *supervisor) recoverCyberPolicy(recovery recoveryContext) {
	if !s.threadAcceptsDirectInput(recovery.ThreadID) {
		s.logger.Log("Skipped cyber policy recovery for agent-managed thread " + recovery.ThreadID)
		return
	}
	if s.proxy == nil {
		_ = s.setAttention("Cyber policy recovery is unavailable before the TUI proxy starts")
		return
	}
	s.mu.Lock()
	if s.state.ManualPaused {
		s.mu.Unlock()
		s.logger.Log("Cyber policy recovery suppressed while manually paused")
		return
	}
	if s.state.AutomaticResumeCount >= s.options.MaxAutoResumes {
		s.mu.Unlock()
		_ = s.setAttention(fmt.Sprintf("Automatic resume limit (%d) reached for %s", s.options.MaxAutoResumes, recovery.ThreadID))
		return
	}
	if s.state.ResumeRequestedForTurnID == recovery.FailedTurnID {
		s.mu.Unlock()
		_ = s.setAttention("Resume was already requested for failed turn " + recovery.FailedTurnID)
		return
	}
	attempts := s.cyberPolicyAttempts[recovery.ThreadID]
	action, ok := nextCyberPolicyAction(attempts)
	if !ok {
		s.mu.Unlock()
		_ = s.setAttention(fmt.Sprintf("Cyber policy recovery exhausted after %d attempts for %s", attempts, recovery.ThreadID))
		return
	}
	s.submittingResume = true
	s.state.Phase = "resuming"
	s.state.ResumeRequestedForTurnID = recovery.FailedTurnID
	s.state.AutomaticResumeCount++
	s.state.NextProbeAt = ""
	s.cyberPolicyAttempts[recovery.ThreadID] = attempts + 1
	s.mu.Unlock()
	_ = s.persist()
	defer func() {
		s.mu.Lock()
		s.submittingResume = false
		s.mu.Unlock()
	}()

	targetThreadID := recovery.ThreadID
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if s.isManuallyPaused() {
		return
	}
	if action.Kind == "fork-thread" {
		value, err := s.proxy.Request(ctx, "thread/fork", map[string]any{"threadId": recovery.ThreadID, "lastTurnId": recovery.FailedTurnID})
		if err != nil {
			_ = s.setAttention("Cyber policy recovery failed: " + err.Error())
			return
		}
		object, _ := asObject(value)
		threadObject, _ := asObject(object["thread"])
		forkedID, ok := readString(threadObject["id"])
		if !ok {
			_ = s.setAttention("Cyber policy recovery failed: Codex did not return a forked thread")
			return
		}
		targetThreadID = forkedID
		s.mu.Lock()
		delete(s.cyberPolicyAttempts, recovery.ThreadID)
		s.cyberPolicyAttempts[targetThreadID] = attempts + 1
		s.mu.Unlock()
	} else {
		if s.isManuallyPaused() {
			return
		}
		if _, err := s.proxy.Request(ctx, "thread/resume", map[string]any{"threadId": recovery.ThreadID}); err != nil {
			_ = s.setAttention("Cyber policy recovery failed: " + err.Error())
			return
		}
	}
	if s.isManuallyPaused() {
		return
	}
	value, err := s.proxy.Request(ctx, "turn/start", map[string]any{
		"threadId":            targetThreadID,
		"input":               []any{map[string]any{"type": "text", "text": action.Prompt}},
		"clientUserMessageId": fmt.Sprintf("codexdog:cyber-policy:%d:%s", attempts+1, recovery.FailedTurnID),
	})
	if err != nil {
		_ = s.setAttention("Cyber policy recovery failed: " + err.Error())
		return
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		_ = s.setAttention("Cyber policy recovery failed: Codex did not return a cyber policy recovery turn")
		return
	}
	if s.recordStartedTurnFromRequest(targetThreadID, started.ID) {
		return
	}
	s.logger.Log(fmt.Sprintf("Cyber policy recovery %d/3 started turn %s on %s", attempts+1, started.ID, targetThreadID))
}

func (s *supervisor) startRecovery(recovery recoveryContext) {
	if !s.threadAcceptsDirectInput(recovery.ThreadID) {
		s.logger.Log("Skipped provider recovery for agent-managed thread " + recovery.ThreadID)
		return
	}
	s.mu.Lock()
	if s.state.ManualPaused {
		s.mu.Unlock()
		s.logger.Log("Automatic recovery suppressed while manually paused")
		return
	}
	if s.state.AutomaticResumeCount >= s.options.MaxAutoResumes {
		s.mu.Unlock()
		_ = s.setAttention(fmt.Sprintf("Automatic resume limit (%d) reached for %s", s.options.MaxAutoResumes, recovery.ThreadID))
		return
	}
	if s.recoveryCancel != nil {
		copy := recovery
		s.pendingRecovery = &copy
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.recoveryCancel = cancel
	s.recoveryGeneration++
	generation := s.recoveryGeneration
	s.state.Phase = "provider-down"
	s.state.ProbeAttempt = 0
	s.state.ConsecutiveProbeSuccesses = 0
	s.mu.Unlock()
	_ = s.persist()
	s.logger.Log("Provider recovery started after " + formatFailure(recovery.Failure))
	s.notifyRemoteControls("Provider recovery started after: " + sanitizeText(formatFailure(recovery.Failure)))
	go func() {
		if err := s.runRecovery(ctx, recovery); err != nil && ctx.Err() == nil {
			_ = s.setAttention("Recovery failed: " + err.Error())
		}
		s.mu.Lock()
		if s.recoveryGeneration != generation {
			s.mu.Unlock()
			return
		}
		s.recoveryCancel = nil
		pending := s.pendingRecovery
		s.pendingRecovery = nil
		shutting := s.shuttingDown
		s.mu.Unlock()
		if pending != nil && !shutting {
			s.startRecovery(*pending)
		}
	}()
}

func (s *supervisor) runRecovery(ctx context.Context, recovery recoveryContext) error {
	if s.probe == nil || s.rpc == nil {
		return errors.New("recovery services are not initialized")
	}
	attempt := 0
	successes := 0
	retryAfter := time.Duration(0)
	for ctx.Err() == nil {
		index := min(attempt, len(s.options.Backoff)-1)
		if index < 0 || len(s.options.Backoff) == 0 {
			return errors.New("no provider probe backoff is configured")
		}
		wait := retryAfter
		if wait == 0 {
			if successes > 0 {
				wait = time.Second
			} else {
				wait = randomJitteredDelay(s.options.Backoff[index])
			}
		}
		s.modifyState(func(state *supervisorState) {
			state.Phase = "provider-down"
			state.ProbeAttempt = attempt + 1
			state.ConsecutiveProbeSuccesses = successes
			state.NextProbeAt = atomicTime(time.Now().Add(wait))
		})
		_ = s.persist()
		if !waitContext(ctx, wait) {
			return nil
		}
		s.modifyState(func(state *supervisorState) { state.Phase = "probing"; state.NextProbeAt = "" })
		_ = s.persist()
		state := s.stateSnapshot()
		result := s.probe.Check(ctx, state.Model, state.ModelProvider)
		if ctx.Err() != nil {
			return nil
		}
		healthModel := strings.TrimSpace(s.options.ProbeModel)
		if s.probe.health != nil {
			healthModel = s.probe.health.targetModelForProvider(healthModel, state.Model, state.ModelProvider)
		} else if healthModel == "" {
			healthModel = state.Model
		}
		s.modifyState(func(current *supervisorState) {
			current.HealthState = result.HealthState
			current.HealthDetail = sanitizeText(result.HealthDetail)
			current.HealthModel = healthModel
			current.HealthProvider = state.ModelProvider
			current.HealthObservations = healthObservationStates(result.HealthObservations)
		})
		_ = s.persist()
		for _, observation := range result.HealthObservations {
			s.logger.Log(formatHealthObservation(observation))
		}
		if result.Healthy {
			successes++
			s.modifyState(func(state *supervisorState) { state.ConsecutiveProbeSuccesses = successes })
			_ = s.persist()
			s.logger.Log(fmt.Sprintf("Provider probe succeeded (%d/%d)", successes, s.options.ProbeSuccesses))
			if successes >= s.options.ProbeSuccesses {
				s.notifyRemoteControls("Provider health checks passed; resuming Codex.")
				return s.resumeThread(ctx, recovery)
			}
		} else {
			successes = 0
			lastError := "Provider probe failed"
			if result.Failure != nil {
				lastError = formatFailure(*result.Failure)
			}
			s.modifyState(func(state *supervisorState) {
				state.ConsecutiveProbeSuccesses = 0
				state.LastError = sanitizeText(lastError)
			})
			_ = s.persist()
			s.logger.Log("Provider probe failed: " + lastError)
		}
		retryAfter = result.RetryAfter
		attempt++
	}
	return nil
}

func (s *supervisor) resumeThread(ctx context.Context, recovery recoveryContext) error {
	if s.proxy == nil || ctx.Err() != nil {
		return nil
	}
	if !s.threadAcceptsDirectInput(recovery.ThreadID) {
		s.logger.Log("Skipped direct resume for agent-managed thread " + recovery.ThreadID)
		return nil
	}
	s.mu.Lock()
	if s.state.ResumeRequestedForTurnID == recovery.FailedTurnID {
		s.mu.Unlock()
		return s.setAttention("Resume was already requested for failed turn " + recovery.FailedTurnID)
	}
	s.submittingResume = true
	s.state.Phase = "resuming"
	s.state.ResumeRequestedForTurnID = recovery.FailedTurnID
	s.state.AutomaticResumeCount++
	s.state.NextProbeAt = ""
	s.mu.Unlock()
	_ = s.persist()
	defer func() {
		s.mu.Lock()
		s.submittingResume = false
		s.mu.Unlock()
	}()
	goal, hasGoal := s.goalForRecovery(ctx, recovery.ThreadID)
	if hasGoal {
		value, err := s.proxy.Request(ctx, "thread/goal/set", map[string]any{
			"threadId": recovery.ThreadID,
			"status":   "active",
		})
		if err != nil {
			return fmt.Errorf("resume goal: %w", err)
		}
		object, _ := asObject(value)
		resumedGoal, ok := readThreadGoal(object["goal"])
		if !ok || resumedGoal.Status != "active" {
			return errors.New("Codex did not confirm the resumed goal")
		}
		s.modifyState(func(state *supervisorState) {
			setCurrentThread(state, recovery.ThreadID)
			state.ProbeAttempt = 0
			state.ConsecutiveProbeSuccesses = 0
		})
		_ = s.persist()
		s.logger.Log(fmt.Sprintf("Resumed %s goal on thread %s after failure %s", goal.Status, recovery.ThreadID, recovery.FailedTurnID))
		s.notifyRemoteControls(fmt.Sprintf("Resumed the %s goal on thread %s.", goal.Status, recovery.ThreadID))
		return nil
	}
	if _, err := s.proxy.Request(ctx, "thread/resume", map[string]any{"threadId": recovery.ThreadID}); err != nil {
		return err
	}
	value, err := s.proxy.Request(ctx, "turn/start", map[string]any{
		"threadId":            recovery.ThreadID,
		"input":               []any{map[string]any{"type": "text", "text": continuationPrompt}},
		"clientUserMessageId": "codexdog:" + recovery.FailedTurnID,
	})
	if err != nil {
		return err
	}
	object, _ := asObject(value)
	started, ok := readTurn(object["turn"])
	if !ok {
		return errors.New("Codex did not return a resumed turn")
	}
	if s.recordStartedTurnFromRequest(recovery.ThreadID, started.ID) {
		return nil
	}
	s.logger.Log(fmt.Sprintf("Resumed thread %s as turn %s after failure %s", recovery.ThreadID, started.ID, recovery.FailedTurnID))
	s.notifyRemoteControls(fmt.Sprintf("Resumed thread %s as turn %s.", recovery.ThreadID, started.ID))
	return nil
}

func (s *supervisor) goalForRecovery(ctx context.Context, threadID string) (threadGoal, bool) {
	value, err := s.proxy.Request(ctx, "thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		s.logger.Log("Goal lookup failed; using prompt-based recovery: " + err.Error())
		return threadGoal{}, false
	}
	object, ok := asObject(value)
	if !ok {
		s.logger.Log("Goal lookup returned an invalid response; using prompt-based recovery")
		return threadGoal{}, false
	}
	if object["goal"] == nil {
		return threadGoal{}, false
	}
	goal, ok := readThreadGoal(object["goal"])
	if !ok || !isResumableGoalStatus(goal.Status) {
		return threadGoal{}, false
	}
	return goal, true
}

func isResumableGoalStatus(status string) bool {
	switch status {
	case "active", "paused", "blocked", "usageLimited", "budgetLimited":
		return true
	default:
		return false
	}
}

func (s *supervisor) cancelRecovery() {
	s.mu.Lock()
	cancel := s.recoveryCancel
	s.recoveryCancel = nil
	s.pendingRecovery = nil
	s.recoveryGeneration++
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *supervisor) setAttention(message string) error {
	s.cancelRecovery()
	s.modifyState(func(state *supervisorState) {
		state.Phase = "needs-attention"
		state.LastError = sanitizeText(message)
		state.NextProbeAt = ""
	})
	err := s.persist()
	s.logger.Log(message)
	s.notifyRemoteControls("Codexdog needs attention: " + sanitizeText(message))
	return err
}

func (s *supervisor) syncStallState() {
	snapshot := s.watchdog.Snapshot()
	s.modifyState(func(state *supervisorState) {
		state.LastTurnActivityAt = atomicTime(snapshot.LastActivityAt)
		state.StallSuspectedAt = atomicTime(snapshot.SuspectedAt)
		state.StallPausedReason = snapshot.PauseReason
	})
}

func clearStallState(state *supervisorState) {
	state.LastTurnActivityAt = ""
	state.StallSuspectedAt = ""
	state.StallPausedReason = ""
}

func (s *supervisor) scheduleActivityPersist() {
	s.mu.Lock()
	if !s.watchdog.Enabled() || s.activityTimer != nil {
		s.mu.Unlock()
		return
	}
	s.activityTimer = time.AfterFunc(2*time.Second, func() {
		s.mu.Lock()
		s.activityTimer = nil
		s.mu.Unlock()
		_ = s.persist()
	})
	s.mu.Unlock()
}

func (s *supervisor) persist() error {
	s.mu.Lock()
	if s.activityTimer != nil {
		s.activityTimer.Stop()
		s.activityTimer = nil
	}
	s.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	state := copySupervisorState(s.state)
	s.mu.Unlock()
	return s.store.Write(state)
}

func (s *supervisor) modifyState(update func(*supervisorState)) {
	s.mu.Lock()
	update(&s.state)
	s.mu.Unlock()
}

func (s *supervisor) stateSnapshot() supervisorState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copySupervisorState(s.state)
}

func copySupervisorState(state supervisorState) supervisorState {
	queueClientMessageIDs := state.QueueClientMessageIDs
	if queueClientMessageIDs != nil {
		state.QueueClientMessageIDs = make(map[string]string, len(queueClientMessageIDs))
		for id, clientID := range queueClientMessageIDs {
			state.QueueClientMessageIDs[id] = clientID
		}
	}
	if state.MCPServers != nil {
		state.MCPServers = append([]mcpServerState(nil), state.MCPServers...)
	}
	if state.Subagents != nil {
		state.Subagents = append([]subagentState(nil), state.Subagents...)
	}
	return state
}

func (s *supervisor) isShuttingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shuttingDown
}

func (s *supervisor) isManuallyPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.ManualPaused
}

func (s *supervisor) installSignalHandlers() func() {
	signals := make(chan os.Signal, 4)
	all := append([]os.Signal{os.Interrupt}, terminationSignals()...)
	signal.Notify(signals, all...)
	finished := make(chan struct{})
	go func() {
		for {
			select {
			case received := <-signals:
				if received == os.Interrupt {
					s.logger.Log("Interrupt received by supervisor; leaving handling to the Codex TUI")
					continue
				}
				s.shutdown(received.String())
			case <-finished:
				return
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(finished)
	}
}

func (s *supervisor) shutdown(reason string) {
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.shuttingDown = true
		s.stallGeneration++
		if s.activityTimer != nil {
			s.activityTimer.Stop()
			s.activityTimer = nil
		}
		if s.usageRefreshTimer != nil {
			s.usageRefreshTimer.Stop()
			s.usageRefreshTimer = nil
		}
		for _, pending := range s.pendingTerminalErrors {
			if pending.Timer != nil {
				pending.Timer.Stop()
			}
		}
		s.pendingTerminalErrors = map[string]*pendingTerminalError{}
		waiters := s.turnTerminalWaiters
		s.turnTerminalWaiters = map[string]chan string{}
		s.watchdogInterruptedTurns = map[string]bool{}
		processes := s.processes
		tui := s.tuiCmd
		app := s.appCmd
		telegram := s.telegram
		s.telegram = nil
		wechat := s.wechat
		s.wechat = nil
		queueClient := s.queueClient
		s.queueClient = nil
		s.queueRPC = nil
		s.mu.Unlock()
		close(s.done)
		for _, waiter := range waiters {
			waiter <- ""
		}
		s.cancelRecovery()
		s.logger.Log("Stopping supervisor: " + reason)
		s.notifications.Publish("Codexdog is stopping: " + sanitizeText(reason))
		s.modifyState(func(state *supervisorState) {
			state.Phase = "stopped"
			state.StoppedReason = reason
			state.ControlToken = ""
			state.TelegramEnabled = false
			state.WeChatEnabled = false
			state.NextProbeAt = ""
			state.TerminalErrorSuspectedAt = ""
		})
		_ = s.persist()
		if telegram != nil {
			telegram.SendFinal("Codexdog is stopping: " + sanitizeText(reason))
			telegram.Stop()
		}
		if wechat != nil {
			wechat.SendFinal("Codexdog is stopping: " + sanitizeText(reason))
			wechat.Stop()
		}
		if s.probe != nil {
			s.probe.Dispose()
		}
		if s.rpc != nil {
			s.rpc.Close()
		}
		if queueClient != nil {
			queueClient.Close()
		}
		if s.proxyServer != nil {
			_ = s.proxyServer.Close()
		}
		if processes != nil {
			if err := processes.Close(); err != nil {
				s.logger.Log("Failed to close child process tree: " + err.Error())
			}
		} else {
			if tui != nil && tui.Process != nil {
				_ = tui.Process.Kill()
			}
			if app != nil && app.Process != nil {
				_ = app.Process.Kill()
			}
		}
		if s.control != nil {
			_ = s.control.Close()
		}
	})
}

func readThreadRuntimeStatus(value any) (threadRuntimeStatus, bool) {
	thread, ok := asObject(value)
	if !ok {
		return threadRuntimeStatus{}, false
	}
	status, ok := asObject(thread["status"])
	if !ok {
		return threadRuntimeStatus{}, false
	}
	typeName, ok := readString(status["type"])
	if !ok {
		return threadRuntimeStatus{}, false
	}
	return threadRuntimeStatus{Type: typeName, ActiveFlags: stringSlice(status["activeFlags"])}, true
}

func stringSlice(value any) []string {
	values, _ := value.([]any)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if item, ok := value.(string); ok {
			result = append(result, item)
		}
	}
	return result
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func isUserWaitReason(value string) bool {
	return value == "waitingOnApproval" || value == "waitingOnUserInput"
}

func optionalID(id string) string {
	if id == "" {
		return ""
	}
	return " for turn " + id
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func getFreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func waitForReady(port int, appDone <-chan error) error {
	deadline := time.Now().Add(20 * time.Second)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case err := <-appDone:
			return fmt.Errorf("Codex app-server exited: %w", err)
		default:
		}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode <= 299 {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("timed out waiting for Codex app-server readiness")
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return 1
}
