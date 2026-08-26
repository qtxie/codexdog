package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

func TestParseArgumentsTelegramConfiguration(t *testing.T) {
	t.Setenv("CODEXDOG_TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("CODEXDOG_TELEGRAM_CHAT_IDS", "-10,-10")
	t.Setenv("CODEXDOG_TELEGRAM_USER_IDS", "22")
	args, err := parseArguments([]string{"start", "--telegram-chat-id", "-11", "--telegram-poll-timeout-sec", "12", "--telegram-no-notify"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramToken != "env-token" || fmt.Sprint(args.Options.TelegramAllowedChats) != "[-10 -11]" || fmt.Sprint(args.Options.TelegramAllowedUsers) != "[22]" {
		t.Fatalf("Telegram options = %#v", args.Options)
	}
	if args.Options.TelegramPollTimeout != 12*time.Second || args.Options.TelegramNotify {
		t.Fatalf("Telegram timing/notify options = %#v", args.Options)
	}
	if args.Options.TelegramStatePath == "" {
		t.Fatal("Telegram offset path was not derived")
	}
}

func TestValidateTelegramOptions(t *testing.T) {
	if err := validateTelegramOptions(supervisorOptions{TelegramToken: "token"}); err == nil {
		t.Fatal("token without chat allowlist was accepted")
	}
	if err := validateTelegramOptions(supervisorOptions{TelegramAllowedChats: []int64{1}}); err == nil {
		t.Fatal("allowlist without token was accepted")
	}
	if err := validateTelegramOptions(supervisorOptions{TelegramToken: "token", TelegramAllowedChats: []int64{1}}); err != nil {
		t.Fatal(err)
	}
}

func TestParseArgumentsPreservesCodexArguments(t *testing.T) {
	args, err := parseArguments([]string{
		"start", "-C", ".",
		"-c", `model="gpt-5.6-sol"`,
		"-c", `model_reasoning_effort="high"`,
		"--health-url", "https://provider.example/health",
		"--error-grace-ms", "7000",
		"--stall-timeout-ms", "600000",
		"--", "resume", "-s", "danger-full-access",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(args.Options.CodexConfig), fmt.Sprint([]string{`model="gpt-5.6-sol"`, `model_reasoning_effort="high"`}); got != want {
		t.Fatalf("config = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(args.Options.TUIArgs), fmt.Sprint([]string{"resume", "-s", "danger-full-access"}); got != want {
		t.Fatalf("TUI args = %s, want %s", got, want)
	}
	if args.Options.HealthURL != "https://provider.example/health" || args.Options.StallTimeout != 10*time.Minute {
		t.Fatalf("options were not parsed: %#v", args.Options)
	}
	if args.Options.TerminalErrorGrace != 7*time.Second {
		t.Fatalf("terminal error grace = %s", args.Options.TerminalErrorGrace)
	}
}

func TestParseArgumentsRequiresSeparatorForTUIArguments(t *testing.T) {
	if _, err := parseArguments([]string{"start", "resume", "-s", "danger-full-access"}); err == nil {
		t.Fatal("Codex arguments without -- were accepted")
	}
}

func TestParseArgumentsSupportsDoctorAndQueue(t *testing.T) {
	doctor, err := parseArguments([]string{"doctor", "--canary", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	if doctor.Command != "doctor" || !doctor.DoctorCanary || !doctor.JSON {
		t.Fatalf("doctor arguments = %#v", doctor)
	}
	queue, err := parseArguments([]string{"queue", "add", "review the diff", "-C", "."})
	if err != nil {
		t.Fatal(err)
	}
	if queue.Command != "queue" || fmt.Sprint(queue.CommandArgs) != "[add review the diff]" {
		t.Fatalf("queue arguments = %#v", queue)
	}
}

func TestParseArgumentsRejectsInvalidTimeout(t *testing.T) {
	if _, err := parseArguments([]string{"start", "--stall-timeout-ms", "-1"}); err == nil {
		t.Fatal("negative stall timeout was accepted")
	}
}

func TestStatusUsesLiveControlState(t *testing.T) {
	workspace := t.TempDir()
	stateRoot := t.TempDir()
	live := supervisorState{
		Version: 1, PID: os.Getpid(), CWD: workspace, Phase: "running", CurrentThreadID: "thread-live",
		TokenUsage: &threadTokenUsageState{ThreadID: "thread-live", Total: tokenUsageBreakdownState{TotalTokens: 999}},
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	control, err := startControlServer(func() supervisorState { return live }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	persisted := live
	persisted.Phase = "stale"
	persisted.TokenUsage = &threadTokenUsageState{ThreadID: "thread-stale", Total: tokenUsageBreakdownState{TotalTokens: 1}}
	persisted.ControlPort = control.Port
	persisted.ControlToken = control.Token
	if err := newStateStore(stateRoot, workspace).Write(persisted); err != nil {
		t.Fatal(err)
	}

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = write
	code, runErr := run([]string{"status", "-C", workspace, "--state-dir", stateRoot, "--json"})
	os.Stdout = originalStdout
	_ = write.Close()
	output, readErr := io.ReadAll(read)
	_ = read.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if code != 0 {
		t.Fatalf("status exit code = %d", code)
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode status %q: %v", output, err)
	}
	tokenUsage, _ := result["tokenUsage"].(map[string]any)
	total, _ := tokenUsage["total"].(map[string]any)
	if result["live"] != true || result["phase"] != "running" || total["totalTokens"] != float64(999) {
		t.Fatalf("status used stale state: %s", output)
	}
}
