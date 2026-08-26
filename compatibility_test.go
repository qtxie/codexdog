package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssessCodexCompatibility(t *testing.T) {
	for _, test := range []struct {
		version string
		status  string
	}{
		{version: "0.147.9", status: "fail"},
		{version: "0.148.0", status: "warning"},
		{version: "0.149.1", status: "pass"},
		{version: "0.150.0", status: "warning"},
	} {
		if got := assessCodexCompatibility(test.version).Status; got != test.status {
			t.Fatalf("compatibility(%s) = %s, want %s", test.version, got, test.status)
		}
	}
}

func TestRedactDoctorJSON(t *testing.T) {
	raw := json.RawMessage(`{"token":"secret","nested":{"api_key":"value","safe":"kept"}}`)
	redacted := redactDoctorJSON(raw)
	if string(redacted) != `{"nested":{"api_key":"[redacted]","safe":"kept"},"token":"[redacted]"}` {
		t.Fatalf("redacted doctor JSON = %s", redacted)
	}
}

func TestRedactDoctorJSONSummarizesChecksWithoutDetails(t *testing.T) {
	raw := json.RawMessage(`{"overallStatus":"warning","checks":{"network":{"status":"warning","summary":"provider issue","details":{"endpoint":"https://provider.example/secret"},"remediation":"visit https://provider.example/help"}}}`)
	redacted := redactDoctorJSON(raw)
	if strings.Contains(string(redacted), "provider.example") || strings.Contains(string(redacted), "details") {
		t.Fatalf("doctor summary leaked verbose details: %s", redacted)
	}
	if got := codexDoctorStatus(redacted); got != "warning" {
		t.Fatalf("doctor status = %q", got)
	}
}

func TestDoctorSupervisorFromStateIncludesOnlyOperationalMetadata(t *testing.T) {
	state := supervisorState{
		PID:                     123,
		Phase:                   "running",
		CurrentThreadID:         "thread-1",
		EffectiveCWD:            `D:\work\project`,
		ActivePermissionProfile: "workspace",
		ApprovalPolicy:          "on-request",
		SandboxPolicy:           "workspaceWrite",
		Model:                   "gpt-5.6-sol",
		ModelProvider:           "openai",
		PrimaryClient:           "codex-tui",
		PrimaryClientVersion:    "0.149.1",
		AppServerPort:           4101,
		ProxyPort:               4102,
		ControlPort:             4103,
		ControlToken:            "must-not-appear",
	}
	report := doctorSupervisorFromState(state, true)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), state.ControlToken) || report.PrimaryClient != "codex-tui 0.149.1" || report.ControlPort != 4103 || !report.Live {
		t.Fatalf("doctor supervisor = %s", data)
	}
}
