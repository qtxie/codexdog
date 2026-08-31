package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArgumentsLoadsProjectConfig(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, `
version = 1
codex = "./bin/codex.exe"
codex_config = ['model="gpt-5.6-sol"', 'model_reasoning_effort="high"']
state_dir = ".state"
probe_timeout_ms = 9000
tui_args = ["resume", "-s", "danger-full-access"]

[telegram]
token_file = "telegram-token.txt"
chat_ids = [-100, 200]
user_ids = [300]
poll_timeout_sec = 12
no_notify = true
alias = "sub2"

[wechat]
user_ids = ["wx-a"]
no_browser = true
disabled = true
`)
	if err := os.WriteFile(filepath.Join(workspace, "telegram-token.txt"), []byte("config-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CODEXDOG_HOME", filepath.Join(workspace, "env-state"))
	t.Setenv("CODEXDOG_TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("CODEXDOG_TELEGRAM_TOKEN_FILE", filepath.Join(workspace, "missing-token"))
	t.Setenv("CODEXDOG_TELEGRAM_CHAT_IDS", "not-an-id")
	t.Setenv("CODEXDOG_TELEGRAM_USER_IDS", "not-an-id")
	t.Setenv("CODEXDOG_WECHAT_USER_IDS", "env-user")

	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	wantCodex := filepath.Join(workspace, "bin", "codex.exe")
	wantState := filepath.Join(workspace, ".state")
	if args.Options.CWD != workspace || args.Options.CodexPath != wantCodex || args.StateRoot != wantState {
		t.Fatalf("paths = cwd %q, codex %q, state %q", args.Options.CWD, args.Options.CodexPath, args.StateRoot)
	}
	if got, want := fmt.Sprint(args.Options.CodexConfig), `[model="gpt-5.6-sol" model_reasoning_effort="high"]`; got != want {
		t.Fatalf("Codex config = %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(args.Options.TUIArgs), "[resume -s danger-full-access]"; got != want {
		t.Fatalf("TUI args = %s, want %s", got, want)
	}
	if args.Options.ProbeTimeout != 9*time.Second {
		t.Fatalf("probe timeout = %s", args.Options.ProbeTimeout)
	}
	if args.Options.TelegramToken != "config-token" || args.Options.TelegramAlias != "sub2" {
		t.Fatalf("Telegram identity = token %q, alias %q", args.Options.TelegramToken, args.Options.TelegramAlias)
	}
	if got := fmt.Sprint(args.Options.TelegramAllowedChats); got != "[-100 200]" {
		t.Fatalf("Telegram chats = %s", got)
	}
	if got := fmt.Sprint(args.Options.TelegramAllowedUsers); got != "[300]" {
		t.Fatalf("Telegram users = %s", got)
	}
	if args.Options.TelegramPollTimeout != 12*time.Second || args.Options.TelegramNotify {
		t.Fatalf("Telegram polling/notify = %s/%v", args.Options.TelegramPollTimeout, args.Options.TelegramNotify)
	}
	if got := fmt.Sprint(args.Options.WeChatAllowedUsers); got != "[wx-a]" || args.Options.WeChatOpenBrowser || !args.Options.WeChatDisabled {
		t.Fatalf("WeChat options = %#v", args.Options)
	}
	if !strings.HasPrefix(args.Options.TelegramStatePath, wantState) {
		t.Fatalf("Telegram state path = %q", args.Options.TelegramStatePath)
	}
}

func TestProjectConfigLoadsFromCurrentDirectory(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "probe_timeout_ms = 4321")
	t.Chdir(workspace)

	args, err := parseArguments([]string{"start"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.CWD != workspace {
		t.Fatalf("cwd = %q, want %q", args.Options.CWD, workspace)
	}
	if args.Options.ProbeTimeout != 4321*time.Millisecond {
		t.Fatalf("probe timeout = %s", args.Options.ProbeTimeout)
	}
}

func TestProjectConfigCommandLineOverrides(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, `
probe_timeout_ms = 1000
tui_args = ["resume", "config-thread"]

[telegram]
token = "config-token"
chat_ids = [10]
poll_timeout_sec = 15
alias = "config"
`)
	t.Setenv("CODEXDOG_TELEGRAM_BOT_TOKEN", "env-token")
	t.Setenv("CODEXDOG_TELEGRAM_CHAT_IDS", "not-an-id")

	args, err := parseArguments([]string{
		"start", "-C", workspace,
		"--probe-timeout-ms", "2000",
		"--telegram-token", "cli-token",
		"--telegram-chat-id", "20",
		"--telegram-poll-timeout-sec", "25",
		"--telegram-alias", "cli",
		"--", "resume", "cli-thread",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.ProbeTimeout != 2*time.Second || args.Options.TelegramToken != "cli-token" || args.Options.TelegramAlias != "cli" {
		t.Fatalf("overridden options = %#v", args.Options)
	}
	if args.Options.TelegramPollTimeout != 25*time.Second || fmt.Sprint(args.Options.TelegramAllowedChats) != "[10 20]" {
		t.Fatalf("Telegram options = %#v", args.Options)
	}
	if got := fmt.Sprint(args.Options.TUIArgs); got != "[resume cli-thread]" {
		t.Fatalf("TUI args = %s", got)
	}
}

func TestProjectConfigOnlyAppliesToStart(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "unknown_key = true\n")

	status, err := parseArguments([]string{"status", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if status.Command != "status" || status.Options.CWD != workspace {
		t.Fatalf("status args = %#v", status)
	}
	if _, err := parseArguments([]string{"start", "-C", workspace}); err == nil || !strings.Contains(err.Error(), projectConfigFileName) || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("start error = %v", err)
	}
}

func TestProjectConfigArgumentFile(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	tokenPath := filepath.Join(workspace, "token file.txt")
	if err := os.WriteFile(tokenPath, []byte("argument-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProjectConfigFile(t, workspace, `
# CLI-style files are accepted for forward-compatible options.
--telegram-token-file "token file.txt"
--telegram-chat-id 42 # end-of-line comment
--telegram-alias sub2
--
resume -s danger-full-access
`)

	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramToken != "argument-token" || args.Options.TelegramAlias != "sub2" || fmt.Sprint(args.Options.TelegramAllowedChats) != "[42]" {
		t.Fatalf("Telegram options = %#v", args.Options)
	}
	if got := fmt.Sprint(args.Options.TUIArgs); got != "[resume -s danger-full-access]" {
		t.Fatalf("TUI args = %s", got)
	}
}

func TestProjectConfigJSONDocument(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "\ufeff{\n"+
		`  "version": 1,`+"\n"+
		`  "telegram": {"token": "json-token", "chatIds": [7], "alias": "json"},`+"\n"+
		`  "tuiArgs": ["resume", "json-thread"]`+"\n"+"}\n")

	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramToken != "json-token" || args.Options.TelegramAlias != "json" || fmt.Sprint(args.Options.TelegramAllowedChats) != "[7]" {
		t.Fatalf("Telegram options = %#v", args.Options)
	}
	if got := fmt.Sprint(args.Options.TUIArgs); got != "[resume json-thread]" {
		t.Fatalf("TUI args = %s", got)
	}
}

func TestProjectConfigSessionAlias(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "session = \"sub2\"\n")
	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramAlias != "sub2" {
		t.Fatalf("Telegram alias = %q", args.Options.TelegramAlias)
	}
	args, err = parseArguments([]string{"start", "-C", workspace, "--session", "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramAlias != "cli" {
		t.Fatalf("CLI session alias = %q", args.Options.TelegramAlias)
	}
}

func TestProjectConfigTelegramTableCanBeFirst(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "[telegram]\nalias = \"sub2\"\n")
	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if args.Options.TelegramAlias != "sub2" {
		t.Fatalf("Telegram alias = %q", args.Options.TelegramAlias)
	}
}

func TestProjectConfigEmptyAllowlistsOverrideEnvironment(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, "[telegram]\nchat_ids = []\n")
	t.Setenv("CODEXDOG_TELEGRAM_CHAT_IDS", "42")
	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	if len(args.Options.TelegramAllowedChats) != 0 {
		t.Fatalf("Telegram chats = %v", args.Options.TelegramAllowedChats)
	}
}

func TestWeChatDisabledIgnoresInheritedUsers(t *testing.T) {
	clearProjectConfigEnvironment(t)
	t.Setenv("CODEXDOG_WECHAT_USER_IDS", "not-a-valid-configuration")
	args, err := parseArguments([]string{"start", "--wechat-disabled"})
	if err != nil {
		t.Fatal(err)
	}
	if !args.Options.WeChatDisabled || len(args.Options.WeChatAllowedUsers) != 0 {
		t.Fatalf("disabled WeChat options = %#v", args.Options)
	}
}

func TestTelegramEnvironmentTokenAndFileStillRejectMismatch(t *testing.T) {
	clearProjectConfigEnvironment(t)
	tokenPath := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(tokenPath, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXDOG_TELEGRAM_BOT_TOKEN", "env-token")
	if _, err := parseArguments([]string{"start", "--telegram-token-file", tokenPath}); err == nil || !strings.Contains(err.Error(), "differs between environment and token file") {
		t.Fatalf("mismatched token sources were accepted: %v", err)
	}
}

func TestProjectConfigRejectsCWD(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, `args = ["-C", "elsewhere"]`)
	if _, err := parseArguments([]string{"start", "-C", workspace}); err == nil || !strings.Contains(err.Error(), "cannot set -C") {
		t.Fatalf("error = %v", err)
	}
}

func TestTokenizeProjectConfigPreservesWindowsPaths(t *testing.T) {
	tokens, err := tokenizeProjectConfig(`--codex "C:\Program Files\Codex\codex.exe"`, projectConfigFileName)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(tokens), `[--codex C:\Program Files\Codex\codex.exe]`; got != want {
		t.Fatalf("tokens = %s, want %s", got, want)
	}
}

func writeProjectConfigFile(t *testing.T, workspace, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(workspace, projectConfigFileName), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearProjectConfigEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"CODEXDOG_HOME",
		"CODEX_SUPERVISOR_HOME",
		"CODEXDOG_TELEGRAM_BOT_TOKEN",
		"CODEXDOG_TELEGRAM_TOKEN_FILE",
		"CODEXDOG_TELEGRAM_CHAT_IDS",
		"CODEXDOG_TELEGRAM_USER_IDS",
		"CODEXDOG_WECHAT_USER_IDS",
	} {
		t.Setenv(name, "")
	}
}
