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

const version = "0.2.0"

var defaultBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 60 * time.Second}

type parsedArguments struct {
	Command   string
	StateRoot string
	JSON      bool
	Options   supervisorOptions
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
		live := queryControl(state)
		visible := publicState(state, live)
		if args.JSON {
			data, err := json.MarshalIndent(visible, "", "  ")
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s\n", data)
		} else {
			fmt.Printf("Workspace: %s\n", state.CWD)
			fmt.Printf("Live: %s\n", yesNo(live))
			fmt.Printf("Phase: %s\n", state.Phase)
			fmt.Printf("Thread: %s\n", valueOrDash(state.CurrentThreadID))
			fmt.Printf("Turn: %s\n", valueOrDash(state.ActiveTurnID))
			fmt.Printf("Automatic resumes: %d\n", state.AutomaticResumeCount)
			fmt.Printf("Stall resumes: %d\n", state.StallRecoveryCount)
			fmt.Printf("Last turn activity: %s\n", valueOrDash(state.LastTurnActivityAt))
			fmt.Printf("Stall suspected: %s\n", valueOrDash(state.StallSuspectedAt))
			fmt.Printf("Watchdog pause: %s\n", valueOrDash(state.StallPausedReason))
			fmt.Printf("Last error: %s\n", valueOrDash(state.LastError))
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
	case "start":
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
		case "start", "status", "stop", "smoke", "canary":
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
		CWD: cwd, CodexPath: "codex", ProbeTimeout: 120 * time.Second,
		ProbeSuccesses: 2, Backoff: append([]time.Duration(nil), defaultBackoff...), MaxAutoResumes: 5,
		StallConfirm: 30 * time.Second, StallInterruptTimeout: 15 * time.Second, MaxStallResumes: 2,
	}
	jsonOutput := false
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
			options.TUIArgs = append([]string(nil), argv[index+1:]...)
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
		case "-h", "--help":
			command = "help"
		default:
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
	return parsedArguments{Command: command, StateRoot: absoluteRoot, JSON: jsonOutput, Options: options}, nil
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

Options:
  -C, --cwd DIR               Workspace to open (default: current directory)
  --codex PATH                Codex executable (default: codex)
  -c, --config KEY=VALUE      Codex config override; repeatable
  --health-url URL            Optional cheap health endpoint checked before canaries
  --probe-model MODEL         Optional model override for health canaries
  --probe-timeout-ms MS       Canary timeout (default: 120000)
  --probe-successes N         Successes required before resume (default: 2)
  --backoff-ms LIST           Comma-separated retry delays
  --max-auto-resumes N        Consecutive automatic resume limit (default: 5)
  --stall-timeout-ms MS       Silent-turn timeout; 0 disables it (default: 0)
  --stall-confirm-ms MS       Silence confirmation window (default: 30000)
  --stall-interrupt-timeout-ms MS
                               Interrupt/confirmation RPC timeout (default: 15000)
  --max-stall-resumes N       Consecutive stalled-turn resume limit (default: 2)
  --tool-stall-timeout-ms MS  Silent active-tool timeout; 0 disables it (default: 0)
  --state-dir DIR             State and log directory
  --json                      JSON output for status
  -h, --help                  Show help

Examples:
  codexdog start -C D:\work\repo
  codexdog start -C . --stall-timeout-ms 600000
  codexdog start -C . -- resume -s danger-full-access
  codexdog status -C . --json
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

func runProtocolSmoke(options supervisorOptions, canary bool) (int, error) {
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
	configureHiddenProcess(command)
	if err := command.Start(); err != nil {
		return 1, err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	defer func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	}()
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
		fmt.Println("Configured provider canary passed.")
	}
	fmt.Println("Codex app-server protocol smoke test passed.")
	return 0, nil
}
