package main

import (
	"fmt"
	"testing"
	"time"
)

func TestParseArgumentsPreservesCodexArguments(t *testing.T) {
	args, err := parseArguments([]string{
		"start", "-C", ".",
		"-c", `model="gpt-5.6-sol"`,
		"-c", `model_reasoning_effort="high"`,
		"--health-url", "https://provider.example/health",
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
