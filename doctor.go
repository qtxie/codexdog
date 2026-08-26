package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var doctorURLPattern = regexp.MustCompile(`https?://[^\s"'<>]+`)

type doctorCheck struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type doctorSupervisor struct {
	StateFound        bool   `json:"stateFound"`
	Live              bool   `json:"live"`
	PID               int    `json:"pid,omitempty"`
	Phase             string `json:"phase,omitempty"`
	ThreadID          string `json:"threadId,omitempty"`
	EffectiveCWD      string `json:"effectiveCwd,omitempty"`
	PermissionProfile string `json:"permissionProfile,omitempty"`
	ApprovalPolicy    string `json:"approvalPolicy,omitempty"`
	SandboxPolicy     string `json:"sandboxPolicy,omitempty"`
	Model             string `json:"model,omitempty"`
	ModelProvider     string `json:"modelProvider,omitempty"`
	PrimaryClient     string `json:"primaryClient,omitempty"`
	AppServerPort     int    `json:"appServerPort,omitempty"`
	ProxyPort         int    `json:"proxyPort,omitempty"`
	ControlPort       int    `json:"controlPort,omitempty"`
}

type doctorReport struct {
	CodexVersion  string               `json:"codexVersion,omitempty"`
	Compatibility codexCompatibility   `json:"compatibility"`
	CodexDoctor   json.RawMessage      `json:"codexDoctor,omitempty"`
	CodexCheck    doctorCheck          `json:"codexDoctorCheck"`
	Schema        protocolSchemaReport `json:"schema"`
	ProtocolSmoke doctorCheck          `json:"protocolSmoke"`
	Canary        *doctorCheck         `json:"canary,omitempty"`
	Supervisor    doctorSupervisor     `json:"supervisor"`
}

func runDoctor(options supervisorOptions, store *stateStore, jsonOutput, canary bool) (int, error) {
	report := doctorReport{
		Compatibility: codexCompatibility{Status: "warning", Message: "Codex version was not checked."},
		CodexCheck:    doctorCheck{Status: "fail"},
		ProtocolSmoke: doctorCheck{Status: "fail"},
	}
	if state, ok := store.Read(); ok {
		report.Supervisor = doctorSupervisorFromState(state, false)
		if live, ok := queryControlState(state); ok {
			report.Supervisor = doctorSupervisorFromState(live, true)
		}
	}
	if version, err := installedCodexVersion(context.Background(), options); err != nil {
		report.Compatibility = codexCompatibility{Status: "fail", Message: err.Error()}
	} else {
		report.CodexVersion = version
		report.Compatibility = assessCodexCompatibility(version)
	}
	if data, err := readCodexDoctor(context.Background(), options); err != nil {
		report.CodexCheck = doctorCheck{Status: "fail", Detail: err.Error()}
	} else {
		report.CodexDoctor = redactDoctorJSON(data)
		report.CodexCheck = doctorCheck{Status: codexDoctorStatus(report.CodexDoctor)}
	}
	report.Schema = checkProtocolSchema(context.Background(), options)
	if code, err := runProtocolSmokeWithOutput(options, false, false); err != nil || code != 0 {
		detail := "protocol smoke failed"
		if err != nil {
			detail = err.Error()
		}
		report.ProtocolSmoke = doctorCheck{Status: "fail", Detail: detail}
	} else {
		report.ProtocolSmoke = doctorCheck{Status: "pass"}
	}
	if canary {
		check := doctorCheck{Status: "pass"}
		if code, err := runProtocolSmokeWithOutput(options, true, false); err != nil || code != 0 {
			check.Status = "fail"
			check.Detail = "provider canary failed"
			if err != nil {
				check.Detail = err.Error()
			}
		}
		report.Canary = &check
	}
	if jsonOutput {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s\n", data)
	} else {
		printDoctorReport(report)
	}
	if doctorFailed(report) {
		return 1, fmt.Errorf("Codexdog doctor found failing checks")
	}
	return 0, nil
}

func doctorSupervisorFromState(state supervisorState, live bool) doctorSupervisor {
	return doctorSupervisor{
		StateFound:        true,
		Live:              live,
		PID:               state.PID,
		Phase:             state.Phase,
		ThreadID:          state.CurrentThreadID,
		EffectiveCWD:      state.EffectiveCWD,
		PermissionProfile: state.ActivePermissionProfile,
		ApprovalPolicy:    state.ApprovalPolicy,
		SandboxPolicy:     state.SandboxPolicy,
		Model:             state.Model,
		ModelProvider:     state.ModelProvider,
		PrimaryClient:     formatClientIdentity(state.PrimaryClient, state.PrimaryClientVersion),
		AppServerPort:     state.AppServerPort,
		ProxyPort:         state.ProxyPort,
		ControlPort:       state.ControlPort,
	}
}

func readCodexDoctor(ctx context.Context, options supervisorOptions) (json.RawMessage, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, options.CodexPath, "doctor", "--json")
	command.Dir = options.CWD
	data, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("run Codex doctor: %w", err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("Codex doctor did not return valid JSON")
	}
	return json.RawMessage(data), nil
}

func redactDoctorJSON(raw json.RawMessage) json.RawMessage {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	if document, ok := value.(map[string]any); ok {
		if checks, ok := document["checks"].(map[string]any); ok {
			result := map[string]any{}
			for _, name := range []string{"codexVersion", "overallStatus", "schemaVersion"} {
				if field, exists := document[name]; exists {
					result[name] = field
				}
			}
			summary := make(map[string]any, len(checks))
			for id, rawCheck := range checks {
				check, ok := rawCheck.(map[string]any)
				if !ok {
					continue
				}
				entry := map[string]any{}
				for _, name := range []string{"status", "summary", "remediation"} {
					if field, exists := check[name]; exists {
						entry[name] = sanitizeDoctorValue(field)
					}
				}
				summary[id] = entry
			}
			result["checks"] = summary
			data, err := json.Marshal(result)
			if err == nil {
				return data
			}
		}
	}
	redactSensitiveFields(value, "")
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func sanitizeDoctorValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	return doctorURLPattern.ReplaceAllString(text, "[redacted URL]")
}

func codexDoctorStatus(raw json.RawMessage) string {
	var document struct {
		OverallStatus string `json:"overallStatus"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		return "pass"
	}
	switch strings.ToLower(document.OverallStatus) {
	case "error", "fail", "failed":
		return "fail"
	case "warning", "warn":
		return "warning"
	default:
		return "pass"
	}
}

func redactSensitiveFields(value any, key string) {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if sensitiveDoctorField(name) {
				typed[name] = "[redacted]"
				continue
			}
			redactSensitiveFields(child, name)
		}
	case []any:
		for _, child := range typed {
			redactSensitiveFields(child, key)
		}
	}
}

func sensitiveDoctorField(name string) bool {
	value := strings.ToLower(name)
	for _, part := range []string{"token", "secret", "password", "authorization", "cookie", "api_key", "apikey"} {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func doctorFailed(report doctorReport) bool {
	if report.Compatibility.Status == "fail" || report.CodexCheck.Status == "fail" || report.Schema.Status == "fail" || report.ProtocolSmoke.Status == "fail" {
		return true
	}
	return report.Canary != nil && report.Canary.Status == "fail"
}

func printDoctorReport(report doctorReport) {
	fmt.Printf("Codex: %s\n", valueOrDash(report.CodexVersion))
	fmt.Printf("Compatibility: %s - %s\n", report.Compatibility.Status, report.Compatibility.Message)
	fmt.Printf("Codex doctor: %s%s\n", report.CodexCheck.Status, optionalDoctorDetail(report.CodexCheck.Detail))
	fmt.Printf("Schema: %s%s\n", report.Schema.Status, optionalDoctorDetail(strings.Join(report.Schema.Failures, "; ")))
	fmt.Printf("Protocol smoke: %s%s\n", report.ProtocolSmoke.Status, optionalDoctorDetail(report.ProtocolSmoke.Detail))
	if report.Canary != nil {
		fmt.Printf("Provider canary: %s%s\n", report.Canary.Status, optionalDoctorDetail(report.Canary.Detail))
	}
	fmt.Printf("Supervisor state: %s\n", yesNo(report.Supervisor.StateFound))
	if report.Supervisor.StateFound {
		fmt.Printf("Supervisor live: %s\n", yesNo(report.Supervisor.Live))
		fmt.Printf("Supervisor phase: %s\n", valueOrDash(report.Supervisor.Phase))
		fmt.Printf("Supervisor PID: %d\n", report.Supervisor.PID)
		fmt.Printf("App-server/proxy/control ports: %s/%s/%s\n", valueOrDash(strconv.Itoa(report.Supervisor.AppServerPort)), valueOrDash(strconv.Itoa(report.Supervisor.ProxyPort)), valueOrDash(strconv.Itoa(report.Supervisor.ControlPort)))
	}
}

func optionalDoctorDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return " - " + detail
}
