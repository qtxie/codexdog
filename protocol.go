package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params map[string]any  `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type turnError struct {
	Message           string
	CodexErrorInfo    any
	AdditionalDetails *string
}

type turn struct {
	ID     string
	Status string
	Error  *turnError
}

type threadGoal struct {
	Objective string
	Status    string
}

type mcpServerState struct {
	Name          string `json:"name"`
	RuntimeStatus string `json:"runtimeStatus,omitempty"`
	AuthStatus    string `json:"authStatus,omitempty"`
	PluginID      string `json:"pluginId,omitempty"`
	ToolCount     int    `json:"toolCount,omitempty"`
}

type subagentState struct {
	ThreadID        string `json:"threadId"`
	ParentThreadID  string `json:"parentThreadId,omitempty"`
	Status          string `json:"status,omitempty"`
	Tool            string `json:"tool,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	Message         string `json:"message,omitempty"`
	LastActivityAt  string `json:"lastActivityAt,omitempty"`
}

// threadSettings is deliberately a narrow, forward-compatible view of the
// app-server ThreadSettings payload. Codexdog records the effective settings
// but never sends them back as recovery overrides.
type threadSettings struct {
	CWD                   string
	PermissionProfile     string
	PermissionProfileBase string
	ApprovalPolicy        string
	ApprovalsReviewer     string
	SandboxPolicy         string
	Model                 string
	ModelProvider         string
}

func parseRPC(data []byte) (rpcMessage, bool) {
	var message rpcMessage
	if err := json.Unmarshal(data, &message); err != nil {
		return rpcMessage{}, false
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return rpcMessage{}, false
	}
	return message, true
}

func rpcIDKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}

func asObject(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok
}

func readString(value any) (string, bool) {
	result, ok := value.(string)
	return result, ok
}

func readBool(value any) (bool, bool) {
	result, ok := value.(bool)
	return result, ok
}

func readNumber(value any) (float64, bool) {
	result, ok := value.(float64)
	return result, ok
}

func readTurn(value any) (turn, bool) {
	object, ok := asObject(value)
	if !ok {
		return turn{}, false
	}
	id, idOK := readString(object["id"])
	status, statusOK := readString(object["status"])
	if !idOK || !statusOK || (status != "completed" && status != "failed" && status != "inProgress" && status != "interrupted") {
		return turn{}, false
	}
	result := turn{ID: id, Status: status}
	if parsed, ok := readTurnError(object["error"]); ok {
		result.Error = &parsed
	}
	return result, true
}

func readTurnError(value any) (turnError, bool) {
	object, ok := asObject(value)
	if !ok {
		return turnError{}, false
	}
	message, ok := readString(object["message"])
	if !ok {
		return turnError{}, false
	}
	result := turnError{Message: message, CodexErrorInfo: object["codexErrorInfo"]}
	if details, ok := object["additionalDetails"].(string); ok {
		result.AdditionalDetails = &details
	}
	return result, true
}

func readThreadGoal(value any) (threadGoal, bool) {
	object, ok := asObject(value)
	if !ok {
		return threadGoal{}, false
	}
	objective, objectiveOK := readString(object["objective"])
	status, statusOK := readString(object["status"])
	if !objectiveOK || objective == "" || !statusOK {
		return threadGoal{}, false
	}
	return threadGoal{Objective: objective, Status: status}, true
}

func decodeResult(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value any
	err := json.Unmarshal(raw, &value)
	return value, err
}

func objectField(object map[string]any, name string) map[string]any {
	value, _ := asObject(object[name])
	return value
}

func readThreadSettings(params map[string]any) (string, threadSettings, bool) {
	threadID, ok := readString(params["threadId"])
	if !ok || threadID == "" {
		return "", threadSettings{}, false
	}
	object, ok := asObject(params["threadSettings"])
	if !ok {
		return "", threadSettings{}, false
	}
	settings := threadSettings{
		CWD:               stringSetting(object["cwd"]),
		ApprovalPolicy:    stringSetting(object["approvalPolicy"]),
		ApprovalsReviewer: stringSetting(object["approvalsReviewer"]),
		SandboxPolicy:     policySetting(object["sandboxPolicy"]),
		Model:             stringSetting(object["model"]),
		ModelProvider:     stringSetting(object["modelProvider"]),
	}
	if profile, ok := asObject(object["activePermissionProfile"]); ok {
		settings.PermissionProfile, _ = readString(profile["id"])
		settings.PermissionProfileBase, _ = readString(profile["extends"])
	}
	return threadID, settings, true
}

func readMCPServerStates(value any) ([]mcpServerState, bool) {
	object, ok := asObject(value)
	if !ok {
		return nil, false
	}
	values, ok := object["data"].([]any)
	if !ok {
		return nil, false
	}
	result := make([]mcpServerState, 0, len(values))
	for _, value := range values {
		server, ok := asObject(value)
		if !ok {
			continue
		}
		name, ok := readString(server["name"])
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		state := mcpServerState{Name: name}
		state.RuntimeStatus, _ = readString(server["runtimeStatus"])
		state.AuthStatus, _ = readString(server["authStatus"])
		state.PluginID, _ = readString(server["pluginId"])
		if tools, ok := asObject(server["tools"]); ok {
			state.ToolCount = len(tools)
		}
		result = append(result, state)
	}
	return result, true
}

func subagentStatusForThread(value any) string {
	if status, ok := readString(value); ok {
		return status
	}
	if object, ok := asObject(value); ok {
		if typeName, ok := readString(object["type"]); ok {
			return typeName
		}
	}
	return ""
}

func stringSetting(value any) string {
	if text, ok := readString(value); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func policySetting(value any) string {
	if text, ok := readString(value); ok {
		return text
	}
	if object, ok := asObject(value); ok {
		if typeName, ok := readString(object["type"]); ok {
			return typeName
		}
	}
	return stringSetting(value)
}
