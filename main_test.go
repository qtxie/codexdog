package main

import (
	"fmt"
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

func TestParseArgumentsRejectsInvalidTimeout(t *testing.T) {
	if _, err := parseArguments([]string{"start", "--stall-timeout-ms", "-1"}); err == nil {
		t.Fatal("negative stall timeout was accepted")
	}
}
