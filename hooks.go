package main

import (
	"fmt"
	"strings"
)

func (s *supervisor) handleHookCompleted(params map[string]any) {
	message, ok := hookFailureMessage(params)
	if !ok {
		return
	}
	s.logger.Log(strings.ReplaceAll(message, "\n", " | "))
	s.notifyTelegram(message)
}

func hookFailureMessage(params map[string]any) (string, bool) {
	run, ok := asObject(params["run"])
	if !ok {
		return "", false
	}
	status, ok := readString(run["status"])
	if !ok || (status != "failed" && status != "blocked") {
		return "", false
	}
	eventName, _ := readString(run["eventName"])
	handlerType, _ := readString(run["handlerType"])
	executionMode, _ := readString(run["executionMode"])
	hookID, _ := readString(run["id"])
	threadID, _ := readString(params["threadId"])
	turnID, _ := readString(params["turnId"])

	title := "Hook " + status
	if eventName != "" {
		title += ": " + eventName
	}
	metadata := []string{}
	if handlerType != "" {
		metadata = append(metadata, handlerType)
	}
	if executionMode != "" {
		metadata = append(metadata, executionMode)
	}
	if len(metadata) > 0 {
		title += " (" + strings.Join(metadata, ", ") + ")"
	}
	lines := []string{title}
	if threadID != "" {
		location := "Thread: " + threadID
		if turnID != "" {
			location += ", turn: " + turnID
		}
		lines = append(lines, location)
	}
	if detail := hookFailureDetail(run); detail != "" {
		lines = append(lines, "Details: "+detail)
	} else if hookID != "" {
		lines = append(lines, "Hook ID: "+hookID)
	}
	return sanitizeText(strings.Join(lines, "\n")), true
}

func hookFailureDetail(run map[string]any) string {
	if message, ok := readString(run["statusMessage"]); ok && strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	entries, _ := run["entries"].([]any)
	fallback := ""
	for _, value := range entries {
		entry, ok := asObject(value)
		if !ok {
			continue
		}
		text, ok := readString(entry["text"])
		if !ok || strings.TrimSpace(text) == "" {
			continue
		}
		text = strings.TrimSpace(text)
		kind, _ := readString(entry["kind"])
		if kind == "error" || kind == "stop" || kind == "warning" {
			return fmt.Sprintf("%s: %s", kind, text)
		}
		if fallback == "" {
			fallback = text
		}
	}
	return fallback
}
