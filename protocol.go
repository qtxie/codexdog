package main

import (
	"bytes"
	"encoding/json"
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
