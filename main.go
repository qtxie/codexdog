package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const version = "0.3.0"

var defaultBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 60 * time.Second}

type parsedArguments struct {
	Command      string
	CommandArgs  []string
	StateRoot    string
	JSON         bool
	DoctorCanary bool
	Options      supervisorOptions
}

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(argv []string) (int, error) {
	args, err := parseArguments(argv)
	if err != nil {
		return 1, err
	}
	if args.Command == "help" {
		printHelp()
		return 0, nil
	}
	if args.Command == "version" {
		fmt.Printf("codexdog %s\n", version)
		return 0, nil
	}
	store := newStateStore(args.StateRoot, args.Options.CWD)
	switch args.Command {
	case "status":
		state, ok := store.Read()
		if !ok {
			fmt.Printf("No supervisor state for %s\n", args.Options.CWD)
			return 1, nil
		}
		liveState, live := queryControlState(state)
		if live {
			state = liveState
		}
		visible := publicState(state, live)
		if args.JSON {
			data, err := json.MarshalIndent(visible, "", "  ")
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s\n", data)
		} else {
			fmt.Printf("Workspace: %s\n", state.CWD)
			fmt.Printf("Thread directory: %s\n", valueOrDash(state.EffectiveCWD))
			fmt.Printf("Codex: %s\n", valueOrDash(state.CodexVersion))
			fmt.Printf("Live: %s\n", yesNo(live))
			fmt.Printf("Phase: %s\n", state.Phase)
			fmt.Printf("Thread: %s\n", valueOrDash(state.CurrentThreadID))
			fmt.Printf("Session: %s\n", valueOrDash(state.SessionID))
			fmt.Printf("Project: %s\n", valueOrDash(state.ProjectID))
			fmt.Printf("Turn: %s\n", valueOrDash(state.ActiveTurnID))
			fmt.Printf("Permission profile: %s\n", valueOrDash(state.ActivePermissionProfile))
			fmt.Printf("Approval policy: %s\n", valueOrDash(state.ApprovalPolicy))
			fmt.Printf("Sandbox: %s\n", valueOrDash(state.SandboxPolicy))
			fmt.Printf("Model: %s\n", valueOrDash(state.Model))
			fmt.Printf("Model provider: %s\n", valueOrDash(state.ModelProvider))
			fmt.Printf("Primary client: %s\n", valueOrDash(formatClientIdentity(state.PrimaryClient, state.PrimaryClientVersion)))
			fmt.Printf("Automatic resumes: %d\n", state.AutomaticResumeCount)
			fmt.Printf("Stall resumes: %d\n", state.StallRecoveryCount)
			fmt.Printf("Last turn activity: %s\n", valueOrDash(state.LastTurnActivityAt))
			fmt.Printf("Stall suspected: %s\n", valueOrDash(state.StallSuspectedAt))
			fmt.Printf("Watchdog pause: %s\n", valueOrDash(state.StallPausedReason))
			fmt.Printf("Terminal error suspected: %s\n", valueOrDash(state.TerminalErrorSuspectedAt))
			fmt.Printf("Manual pause: %s\n", yesNo(state.ManualPaused))
			fmt.Printf("Telegram control: %s\n", yesNo(state.TelegramEnabled))
			fmt.Printf("Queue updated: %s\n", valueOrDash(state.QueueUpdatedAt))
			fmt.Printf("Last error: %s\n", valueOrDash(state.LastError))
			for _, line := range usageStatusLines(state) {
				fmt.Println(line)
			}
			fmt.Printf("Updated: %s\n", state.UpdatedAt)
		}
		if live {
			return 0, nil
		}
		return 1, nil
	case "stop":
		state, ok := store.Read()
		if !ok || !requestStop(state) {
			return 1, fmt.Errorf("no live supervisor for %s", args.Options.CWD)
		}
		fmt.Printf("Stop requested for %s\n", args.Options.CWD)
		return 0, nil
	case "smoke", "canary":
		return runProtocolSmoke(args.Options, args.Command == "canary")
	case "doctor":
		return runDoctor(args.Options, store, args.JSON, args.DoctorCanary)
	case "schema-check":
		return runSchemaCheck(args.Options, args.JSON)
	case "agents":
		return runAgents(args.Options, store)
	case "queue":
		return runQueueCommand(args, store)
	case "start":
		if err := validateTelegramOptions(args.Options); err != nil {
			return 1, err
		}
		if state, ok := store.Read(); ok && queryControl(state) {
			return 1, fmt.Errorf("a supervisor is already running for %s", args.Options.CWD)
		}
		if !stdinIsTerminal() {
			return 1, errors.New("start requires an interactive terminal")
		}
		return newSupervisor(args.Options, store).Run()
	default:
		return 1, fmt.Errorf("unknown command %s", args.Command)
	}
}

func parseArguments(argv []string) (parsedArguments, error) {
	command := "start"
	index := 0
	if len(argv) > 0 {
		switch argv[0] {
		case "start", "status", "stop", "smoke", "canary", "doctor", "schema-check", "agents", "queue":
			command = argv[0]
			index = 1
		case "help", "--help", "-h":
			command = "help"
			index = 1
		case "version", "--version", "-v":
			command = "version"
			index = 1
		}
	}
	cwd, _ := os.Getwd()
	stateRoot := os.Getenv("CODEXDOG_HOME")
	if stateRoot == "" {
		stateRoot = os.Getenv("CODEX_SUPERVISOR_HOME")
	}
	if stateRoot == "" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
		stateRoot = filepath.Join(base, "codex-supervisor")
	}
	options := supervisorOptions{
		CWD: cwd, CodexPath: "codex", ProbeTimeout: 120 * time.Second, TerminalErrorGrace: 5 * time.Second,
		ProbeSuccesses: 2, Backoff: append([]time.Duration(nil), defaultBackoff...), MaxAutoResumes: 5,
		StallConfirm: 30 * time.Second, StallInterruptTimeout: 15 * time.Second, MaxStallResumes: 2,
		TelegramPollTimeout: telegramDefaultPollTimeout, TelegramNotify: true,
	}
	if token := strings.TrimSpace(os.Getenv("CODEXDOG_TELEGRAM_BOT_TOKEN")); token != "" {
		options.TelegramToken = token
	}
	if value := strings.TrimSpace(os.Getenv("CODEXDOG_TELEGRAM_CHAT_IDS")); value != "" {
		parsed, err := parseInt64List(value, "CODEXDOG_TELEGRAM_CHAT_IDS")
		if err != nil {
			return parsedArguments{}, err
		}
		options.TelegramAllowedChats = append(options.TelegramAllowedChats, parsed...)
	}
	if value := strings.TrimSpace(os.Getenv("CODEXDOG_TELEGRAM_USER_IDS")); value != "" {
		parsed, err := parseInt64List(value, "CODEXDOG_TELEGRAM_USER_IDS")
		if err != nil {
			return parsedArguments{}, err
		}
		options.TelegramAllowedUsers = append(options.TelegramAllowedUsers, parsed...)
	}
	telegramTokenFile := strings.TrimSpace(os.Getenv("CODEXDOG_TELEGRAM_TOKEN_FILE"))
	jsonOutput := false
	commandArgs := []string{}
	doctorCanary := false
	valueAfter := func(flag string) (string, error) {
		index++
		if index >= len(argv) || argv[index] == "" {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return argv[index], nil
	}
	for ; index < len(argv); index++ {
		arg := argv[index]
		if arg == "--" {
			if command == "queue" {
				commandArgs = append(commandArgs, argv[index+1:]...)
			} else {
				options.TUIArgs = append([]string(nil), argv[index+1:]...)
			}
			break
		}
		switch arg {
		case "-C", "--cwd":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.CWD, err = filepath.Abs(value)
			if err != nil {
				return parsedArguments{}, err
			}
		case "--codex":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.CodexPath = value
		case "--state-dir":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			stateRoot, err = filepath.Abs(value)
			if err != nil {
				return parsedArguments{}, err
			}
		case "--health-url":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.HealthURL = value
		case "--probe-model":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.ProbeModel = value
		case "--probe-timeout-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.ProbeTimeout = time.Duration(parsed) * time.Millisecond
		case "--probe-successes":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.ProbeSuccesses, err = positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
		case "--error-grace-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.TerminalErrorGrace = time.Duration(parsed) * time.Millisecond
		case "--max-auto-resumes":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.MaxAutoResumes, err = positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
		case "--stall-timeout-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := nonNegativeInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.StallTimeout = time.Duration(parsed) * time.Millisecond
		case "--stall-confirm-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.StallConfirm = time.Duration(parsed) * time.Millisecond
		case "--stall-interrupt-timeout-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.StallInterruptTimeout = time.Duration(parsed) * time.Millisecond
		case "--max-stall-resumes":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.MaxStallResumes, err = positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
		case "--tool-stall-timeout-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := nonNegativeInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.ToolStallTimeout = time.Duration(parsed) * time.Millisecond
		case "--telegram-token-file":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			telegramTokenFile = value
		case "--telegram-chat-id":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := parseTelegramID(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.TelegramAllowedChats = append(options.TelegramAllowedChats, parsed)
		case "--telegram-user-id":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := parseTelegramID(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.TelegramAllowedUsers = append(options.TelegramAllowedUsers, parsed)
		case "--telegram-poll-timeout-sec":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			parsed, err := positiveInteger(value, arg)
			if err != nil {
				return parsedArguments{}, err
			}
			if parsed > 50 {
				return parsedArguments{}, fmt.Errorf("%s must be between 1 and 50", arg)
			}
			options.TelegramPollTimeout = time.Duration(parsed) * time.Second
		case "--telegram-no-notify":
			options.TelegramNotify = false
		case "--backoff-ms":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.Backoff = nil
			for _, item := range strings.Split(value, ",") {
				parsed, err := positiveInteger(strings.TrimSpace(item), arg)
				if err != nil {
					return parsedArguments{}, err
				}
				options.Backoff = append(options.Backoff, time.Duration(parsed)*time.Millisecond)
			}
		case "-c", "--config":
			value, err := valueAfter(arg)
			if err != nil {
				return parsedArguments{}, err
			}
			options.CodexConfig = append(options.CodexConfig, value)
		case "--json":
			jsonOutput = true
		case "--canary":
			if command != "doctor" {
				return parsedArguments{}, fmt.Errorf("%s is only supported by doctor", arg)
			}
			doctorCanary = true
		case "-h", "--help":
			command = "help"
		default:
			if command == "queue" {
				commandArgs = append(commandArgs, arg)
				continue
			}
			return parsedArguments{}, fmt.Errorf("unknown option %s. Put Codex TUI arguments after --", arg)
		}
	}
	absoluteCWD, err := filepath.Abs(options.CWD)
	if err != nil {
		return parsedArguments{}, err
	}
	options.CWD = absoluteCWD
	absoluteRoot, err := filepath.Abs(stateRoot)
	if err != nil {
		return parsedArguments{}, err
	}
	if telegramTokenFile != "" {
		data, readErr := os.ReadFile(telegramTokenFile)
		if readErr != nil {
			return parsedArguments{}, fmt.Errorf("read Telegram token file: %w", readErr)
		}
		fileToken := strings.TrimSpace(string(data))
		if fileToken == "" {
			return parsedArguments{}, errors.New("Telegram token file is empty")
		}
		if options.TelegramToken != "" && options.TelegramToken != fileToken {
			return parsedArguments{}, errors.New("Telegram bot token differs between environment and token file")
		}
		options.TelegramToken = fileToken
	}
	options.TelegramAllowedChats = uniqueInt64(options.TelegramAllowedChats)
	options.TelegramAllowedUsers = uniqueInt64(options.TelegramAllowedUsers)
	options.TelegramStatePath = filepath.Join(absoluteRoot, "telegram-"+workspaceKey(options.CWD)+".json")
	return parsedArguments{Command: command, CommandArgs: commandArgs, StateRoot: absoluteRoot, JSON: jsonOutput, DoctorCanary: doctorCanary, Options: options}, nil
}

func parseTelegramID(value, flag string) (int64, error) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s requires a non-zero integer Telegram ID", flag)
	}
	return parsed, nil
}

func parseInt64List(value, name string) ([]int64, error) {
	items := strings.Split(value, ",")
	result := make([]int64, 0, len(items))
	for _, item := range items {
		parsed, err := parseTelegramID(strings.TrimSpace(item), name)
		if err != nil {
			return nil, err
		}
		result = append(result, parsed)
	}
	return result, nil
}

func uniqueInt64(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validateTelegramOptions(options supervisorOptions) error {
	if strings.TrimSpace(options.TelegramToken) == "" {
		if len(options.TelegramAllowedChats) > 0 || len(options.TelegramAllowedUsers) > 0 {
			return errors.New("Telegram chat/user IDs were provided but no bot token is configured")
		}
		return nil
	}
	if len(options.TelegramAllowedChats) == 0 {
		return errors.New("Telegram control requires at least one --telegram-chat-id (or CODEXDOG_TELEGRAM_CHAT_IDS)")
	}
	return nil
}

func positiveInteger(value, flag string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s requires a positive integer", flag)
	}
	return parsed, nil
}

func nonNegativeInteger(value, flag string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s requires a non-negative integer", flag)
	}
	return parsed, nil
}

func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printHelp() {
	fmt.Print(`Codexdog

Usage:
  codexdog start [options] [-- CODEX_TUI_ARGS...]
  codexdog status [options]
  codexdog stop [options]
  codexdog smoke [options]
  codexdog canary [options]
  codexdog doctor [options] [--canary]
  codexdog schema-check [options]
  codexdog agents [options] [-- CODEX_AGENTS_ARGS...]
  codexdog queue ACTION [ARGS...] [options]

Options:
  -C, --cwd DIR               Workspace to open (default: current directory)
  --codex PATH                Codex executable (default: codex)
  -c, --config KEY=VALUE      Codex config override; repeatable
  --health-url URL            Optional cheap health endpoint checked before canaries
  --probe-model MODEL         Optional model override for health canaries
  --probe-timeout-ms MS       Canary timeout (default: 120000)
  --probe-successes N         Successes required before resume (default: 2)
  --error-grace-ms MS         Terminal-event grace period (default: 5000)
  --backoff-ms LIST           Comma-separated retry delays
  --max-auto-resumes N        Consecutive automatic resume limit (default: 5)
  --stall-timeout-ms MS       Silent-turn timeout; 0 disables it (default: 0)
  --stall-confirm-ms MS       Silence confirmation window (default: 30000)
  --stall-interrupt-timeout-ms MS
                               Interrupt/confirmation RPC timeout (default: 15000)
  --max-stall-resumes N       Consecutive stalled-turn resume limit (default: 2)
  --tool-stall-timeout-ms MS  Silent active-tool timeout; 0 disables it (default: 0)
  --telegram-token-file PATH  Read the Telegram bot token from a private file
  --telegram-chat-id ID       Allow a Telegram chat; repeatable
  --telegram-user-id ID       Optionally restrict allowed senders; repeatable
  --telegram-poll-timeout-sec N
                               Long-poll timeout from 1 to 50 seconds (default: 30)
  --telegram-no-notify         Disable unsolicited lifecycle notifications
  --state-dir DIR             State and log directory
  --json                      JSON output for status, doctor, schema-check, or queue
  --canary                    Run a provider-consuming canary as part of doctor
  -h, --help                  Show help

Examples:
  codexdog start -C D:\work\repo
  codexdog start -C . --stall-timeout-ms 600000
  codexdog start -C . -- resume -s danger-full-access
  codexdog status -C . --json
  codexdog doctor -C .
  codexdog agents -C . -- --no-alt-screen
  codexdog queue add "review the current diff" -C .
  $env:CODEXDOG_TELEGRAM_BOT_TOKEN = "..."
  codexdog start -C . --telegram-chat-id 123456789
`)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func formatClientIdentity(name, clientVersion string) string {
	if name == "" {
		return ""
	}
	if clientVersion == "" {
		return name
	}
	return name + " " + clientVersion
}

func runProtocolSmoke(options supervisorOptions, canary bool) (int, error) {
	return runProtocolSmokeWithOutput(options, canary, true)
}

func runProtocolSmokeWithOutput(options supervisorOptions, canary, output bool) (int, error) {
	port, err := getFreePort()
	if err != nil {
		return 1, err
	}
	url := fmt.Sprintf("ws://127.0.0.1:%d", port)
	args := []string{"app-server"}
	for _, value := range options.CodexConfig {
		args = append(args, "-c", value)
	}
	args = append(args, "--listen", url)
	command := exec.Command(options.CodexPath, args...)
	command.Dir = options.CWD
	command.Stdout, command.Stderr = io.Discard, io.Discard
	processes, err := newProcessTree()
	if err != nil {
		return 1, fmt.Errorf("initialize child process management: %w", err)
	}
	defer processes.Close()
	if err := processes.Start(command, true); err != nil {
		return 1, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if err := waitForReady(port, done); err != nil {
		return 1, err
	}
	rpc := newJSONRPCClient(url, 30*time.Second)
	if err := rpc.Connect(context.Background()); err != nil {
		return 1, err
	}
	defer rpc.Close()
	if err := rpc.Initialize(context.Background()); err != nil {
		return 1, err
	}
	if _, err := rpc.Request(context.Background(), "thread/list", map[string]any{"limit": 1}); err != nil {
		return 1, err
	}
	proxy := newTUIProxy(url)
	proxyPort, err := proxy.Start()
	if err != nil {
		return 1, err
	}
	defer proxy.Close()
	observed := make(chan struct{}, 1)
	proxy.OnServerMessage(func(message rpcMessage, _ string) {
		if rpcIDKey(message.ID) == `"smoke-init"` {
			select {
			case observed <- struct{}{}:
			default:
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	client, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d", proxyPort), nil)
	cancel()
	if err != nil {
		return 1, err
	}
	defer client.Close(websocket.StatusNormalClosure, "smoke complete")
	initData, _ := json.Marshal(map[string]any{"id": "smoke-init", "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "codexdog_smoke", "version": version}}})
	if err := client.Write(context.Background(), websocket.MessageText, initData); err != nil {
		return 1, err
	}
	select {
	case <-observed:
	case <-time.After(10 * time.Second):
		return 1, errors.New("proxy did not observe initialize response")
	}
	notification, _ := json.Marshal(map[string]any{"method": "initialized", "params": map[string]any{}})
	if err := client.Write(context.Background(), websocket.MessageText, notification); err != nil {
		return 1, err
	}
	requestCtx, requestCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer requestCancel()
	if _, err := proxy.Request(requestCtx, "thread/list", map[string]any{"limit": 1}); err != nil {
		return 1, err
	}
	startedValue, err := proxy.Request(requestCtx, "thread/start", map[string]any{"cwd": options.CWD, "ephemeral": true, "sandbox": "read-only", "approvalPolicy": "never"})
	if err != nil {
		return 1, err
	}
	started, _ := asObject(startedValue)
	threadObject, _ := asObject(started["thread"])
	threadID, ok := readString(threadObject["id"])
	if !ok {
		return 1, errors.New("app-server did not return an ephemeral smoke-test thread")
	}
	readValue, err := proxy.Request(requestCtx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": false})
	if err != nil {
		return 1, err
	}
	readObject, _ := asObject(readValue)
	readThread, _ := asObject(readObject["thread"])
	readID, _ := readString(readThread["id"])
	status, _ := asObject(readThread["status"])
	_, statusOK := readString(status["type"])
	if readID != threadID || !statusOK {
		return 1, errors.New("app-server did not return the smoke-test thread status")
	}
	if canary {
		probe := newProviderProbe(rpc, providerProbeOptions{CWD: options.CWD, Timeout: options.ProbeTimeout, HealthURL: options.HealthURL, Model: options.ProbeModel})
		defer probe.Dispose()
		result := probe.Check(context.Background())
		if !result.Healthy {
			if result.Failure == nil {
				return 1, errors.New("provider canary failed")
			}
			return 1, fmt.Errorf("provider canary failed: %s", formatFailure(*result.Failure))
		}
		if output {
			fmt.Println("Configured provider canary passed.")
		}
	}
	if output {
		fmt.Println("Codex app-server protocol smoke test passed.")
	}
	return 0, nil
}
