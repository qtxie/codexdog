package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const testedCodexVersion = "0.153.4"
const minimumCodexVersion = "0.148.0"

var codexVersionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)

type codexCompatibility struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type protocolSchemaReport struct {
	Status   string   `json:"status"`
	Checks   []string `json:"checks,omitempty"`
	Failures []string `json:"failures,omitempty"`
}

func installedCodexVersion(ctx context.Context, options supervisorOptions) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, options.CodexPath, "--version")
	command.Dir = options.CWD
	data, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("run Codex --version: %w", err)
	}
	match := codexVersionPattern.FindStringSubmatch(string(data))
	if len(match) != 4 {
		return "", errors.New("Codex --version did not return a semantic version")
	}
	return match[1] + "." + match[2] + "." + match[3], nil
}

func assessCodexCompatibility(value string) codexCompatibility {
	version, ok := parseSemanticVersion(value)
	if !ok {
		return codexCompatibility{Status: "warning", Message: "Could not parse the installed Codex version."}
	}
	minimum, _ := parseSemanticVersion(minimumCodexVersion)
	if version.lessThan(minimum) {
		return codexCompatibility{Status: "fail", Message: "Codex " + value + " is below the minimum supported version " + minimumCodexVersion + "."}
	}
	tested, _ := parseSemanticVersion(testedCodexVersion)
	if version.major != tested.major || version.minor != tested.minor {
		return codexCompatibility{Status: "warning", Message: "Codex " + value + " is outside the tested " + tested.majorMinor() + ".x line; run schema-check and smoke before relying on it."}
	}
	return codexCompatibility{Status: "pass", Message: "Codex " + value + " is in the tested " + tested.majorMinor() + ".x line."}
}

type semanticVersion struct {
	major int
	minor int
	patch int
}

func parseSemanticVersion(value string) (semanticVersion, bool) {
	match := codexVersionPattern.FindStringSubmatch(value)
	if len(match) != 4 {
		return semanticVersion{}, false
	}
	major, majorErr := strconv.Atoi(match[1])
	minor, minorErr := strconv.Atoi(match[2])
	patch, patchErr := strconv.Atoi(match[3])
	return semanticVersion{major: major, minor: minor, patch: patch}, majorErr == nil && minorErr == nil && patchErr == nil
}

func (v semanticVersion) lessThan(other semanticVersion) bool {
	if v.major != other.major {
		return v.major < other.major
	}
	if v.minor != other.minor {
		return v.minor < other.minor
	}
	return v.patch < other.patch
}

func (v semanticVersion) majorMinor() string {
	return strconv.Itoa(v.major) + "." + strconv.Itoa(v.minor)
}

func checkProtocolSchema(ctx context.Context, options supervisorOptions) protocolSchemaReport {
	report := protocolSchemaReport{Status: "pass"}
	directory, err := os.MkdirTemp("", "codexdog-schema-")
	if err != nil {
		return protocolSchemaReport{Status: "fail", Failures: []string{"create temporary schema directory: " + err.Error()}}
	}
	defer os.RemoveAll(directory)
	stable := filepath.Join(directory, "stable")
	experimental := filepath.Join(directory, "experimental")
	if err := generateSchema(ctx, options, stable, false); err != nil {
		return protocolSchemaReport{Status: "fail", Failures: []string{err.Error()}}
	}
	if err := generateSchema(ctx, options, experimental, true); err != nil {
		return protocolSchemaReport{Status: "fail", Failures: []string{err.Error()}}
	}
	stableRequirements := map[string][]string{
		"v2/ThreadResumeParams.json":                {"threadId"},
		"v2/TurnStartParams.json":                   {"threadId", "input"},
		"v2/ThreadSettingsUpdatedNotification.json": {"threadId", "threadSettings"},
	}
	experimentalRequirements := map[string][]string{
		"v2/ThreadQueueAddParams.json":     {"threadId", "clientUserMessageId", "input"},
		"v2/ThreadQueueListParams.json":    {"threadId"},
		"v2/ThreadQueueUpdateParams.json":  {"threadId", "queuedSubmissionId", "input"},
		"v2/ThreadQueueDeleteParams.json":  {"threadId", "queuedSubmissionId"},
		"v2/ThreadQueueReorderParams.json": {"threadId", "queuedSubmissionIds"},
		"v2/ThreadQueueStartParams.json":   {"threadId"},
	}
	report.checkFiles(stable, stableRequirements)
	report.checkFiles(experimental, experimentalRequirements)
	if len(report.Failures) > 0 {
		report.Status = "fail"
	}
	return report
}

func generateSchema(ctx context.Context, options supervisorOptions, output string, experimental bool) error {
	commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"app-server", "generate-json-schema"}
	if experimental {
		args = append(args, "--experimental")
	}
	args = append(args, "--out", output)
	command := exec.CommandContext(commandCtx, options.CodexPath, args...)
	command.Dir = options.CWD
	if err := command.Run(); err != nil {
		return fmt.Errorf("generate %s app-server schema: %w", map[bool]string{false: "stable", true: "experimental"}[experimental], err)
	}
	return nil
}

func (r *protocolSchemaReport) checkFiles(root string, requirements map[string][]string) {
	for relative, required := range requirements {
		path := filepath.Join(root, filepath.FromSlash(relative))
		data, err := os.ReadFile(path)
		if err != nil {
			r.Failures = append(r.Failures, "read "+relative+": "+err.Error())
			continue
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			r.Failures = append(r.Failures, "parse "+relative+": "+err.Error())
			continue
		}
		for _, field := range required {
			if _, ok := schema.Properties[field]; !ok {
				r.Failures = append(r.Failures, relative+" is missing property "+field)
				continue
			}
			if !contains(schema.Required, field) {
				r.Failures = append(r.Failures, relative+" no longer requires "+field)
			}
		}
		r.Checks = append(r.Checks, relative)
	}
}

func runSchemaCheck(options supervisorOptions, jsonOutput bool) (int, error) {
	report := checkProtocolSchema(context.Background(), options)
	if jsonOutput {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return 1, err
		}
		fmt.Printf("%s\n", data)
	} else if report.Status == "pass" {
		fmt.Printf("Codex app-server schema check passed (%d contracts).\n", len(report.Checks))
	} else {
		fmt.Printf("Codex app-server schema check failed:\n%s\n", strings.Join(report.Failures, "\n"))
	}
	if report.Status != "pass" {
		return 1, errors.New("Codex app-server schema compatibility check failed")
	}
	return 0, nil
}
