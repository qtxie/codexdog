package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const queueHelpText = `Queue commands:
/queue list
/queue add TEXT
/queue delete ID
/queue update ID TEXT
/queue reorder ID [ID...]
/queue start [ID]`

type queuedSubmission struct {
	ID                  string
	ClientUserMessageID string
	Text                string
}

func (s *supervisor) remoteQueue(ctx context.Context, raw string) (string, error) {
	args := strings.Fields(strings.TrimSpace(raw))
	if len(args) == 0 || strings.EqualFold(args[0], "help") {
		return queueHelpText, nil
	}
	s.mu.Lock()
	threadID := s.state.CurrentThreadID
	requester := s.queueRPC
	s.mu.Unlock()
	if threadID == "" {
		return "", errors.New("there is no current Codex thread")
	}
	if requester == nil {
		return "", errors.New("the Codex experimental queue API is unavailable for this supervisor")
	}
	action := strings.ToLower(args[0])
	switch action {
	case "list":
		if len(args) != 1 {
			return "Usage: /queue list", nil
		}
		value, err := requester.Request(ctx, "thread/queue/list", map[string]any{"threadId": threadID, "limit": 100})
		if err != nil {
			return "", fmt.Errorf("list queue: %w", err)
		}
		s.markQueueUpdated()
		items := readQueuedSubmissions(value)
		if len(items) == 0 {
			return "Queue is empty.", nil
		}
		lines := make([]string, 0, len(items)+1)
		lines = append(lines, fmt.Sprintf("Queued submissions (%d):", len(items)))
		for _, item := range items {
			lines = append(lines, item.ID+": "+item.Text)
		}
		return strings.Join(lines, "\n"), nil
	case "add":
		text := textAfterFirstWord(raw)
		if text == "" {
			return "Usage: /queue add TEXT", nil
		}
		clientID := "codexdog:queue:" + randomID()
		value, err := requester.Request(ctx, "thread/queue/add", map[string]any{
			"threadId":            threadID,
			"clientUserMessageId": clientID,
			"input":               textInput(text),
		})
		if err != nil {
			return "", fmt.Errorf("add to queue: %w", err)
		}
		object, _ := asObject(value)
		item, ok := readQueuedSubmission(object["queuedSubmission"])
		if !ok {
			return "", errors.New("Codex did not return the queued submission")
		}
		s.recordQueuedSubmission(item)
		return "Queued submission " + item.ID + ".", nil
	case "delete":
		if len(args) != 2 {
			return "Usage: /queue delete ID", nil
		}
		if _, err := requester.Request(ctx, "thread/queue/delete", map[string]any{"threadId": threadID, "queuedSubmissionId": args[1]}); err != nil {
			return "", fmt.Errorf("delete queued submission: %w", err)
		}
		s.markQueueUpdated()
		return "Deleted queued submission " + args[1] + ".", nil
	case "update":
		id, text := queueUpdateArguments(raw)
		if id == "" || text == "" {
			return "Usage: /queue update ID TEXT", nil
		}
		value, err := requester.Request(ctx, "thread/queue/update", map[string]any{
			"threadId":           threadID,
			"queuedSubmissionId": id,
			"input":              textInput(text),
		})
		if err != nil {
			return "", fmt.Errorf("update queued submission: %w", err)
		}
		object, _ := asObject(value)
		if item, ok := readQueuedSubmission(object["queuedSubmission"]); ok {
			s.recordQueuedSubmission(item)
		} else {
			s.markQueueUpdated()
		}
		return "Updated queued submission " + id + ".", nil
	case "reorder":
		if len(args) < 2 {
			return "Usage: /queue reorder ID [ID...]", nil
		}
		if _, err := requester.Request(ctx, "thread/queue/reorder", map[string]any{"threadId": threadID, "queuedSubmissionIds": args[1:]}); err != nil {
			return "", fmt.Errorf("reorder queue: %w", err)
		}
		s.markQueueUpdated()
		return "Queue reordered.", nil
	case "start":
		if len(args) > 2 {
			return "Usage: /queue start [ID]", nil
		}
		params := map[string]any{"threadId": threadID}
		if len(args) == 2 {
			params["queuedSubmissionId"] = args[1]
		}
		value, err := requester.Request(ctx, "thread/queue/start", params)
		if err != nil {
			return "", fmt.Errorf("start queued submission: %w", err)
		}
		object, _ := asObject(value)
		if started, ok := readTurn(object["turn"]); ok {
			if s.recordStartedTurnFromRequest(threadID, started.ID) {
				return "Queued turn started but was interrupted because Codexdog is paused.", nil
			}
			s.markQueueUpdated()
			return "Started queued turn " + started.ID + ".", nil
		}
		return "", errors.New("Codex did not return a started queued turn")
	default:
		return queueHelpText, nil
	}
}

func textInput(text string) []any {
	return []any{map[string]any{"type": "text", "text": text}}
}

func textAfterFirstWord(value string) string {
	_, text, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found {
		return ""
	}
	return strings.TrimSpace(text)
}

func queueUpdateArguments(value string) (string, string) {
	_, rest, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found {
		return "", ""
	}
	id, text, found := strings.Cut(strings.TrimSpace(rest), " ")
	if !found {
		return "", ""
	}
	return strings.TrimSpace(id), strings.TrimSpace(text)
}

func readQueuedSubmissions(value any) []queuedSubmission {
	object, ok := asObject(value)
	if !ok {
		return nil
	}
	values, _ := object["data"].([]any)
	result := make([]queuedSubmission, 0, len(values))
	for _, value := range values {
		if item, ok := readQueuedSubmission(value); ok {
			result = append(result, item)
		}
	}
	return result
}

func readQueuedSubmission(value any) (queuedSubmission, bool) {
	object, ok := asObject(value)
	if !ok {
		return queuedSubmission{}, false
	}
	id, idOK := readString(object["id"])
	clientID, clientOK := readString(object["clientUserMessageId"])
	if !idOK || !clientOK || id == "" {
		return queuedSubmission{}, false
	}
	text := "(non-text input)"
	if inputs, ok := object["input"].([]any); ok {
		for _, input := range inputs {
			inputObject, ok := asObject(input)
			if !ok {
				continue
			}
			if inputType, _ := readString(inputObject["type"]); inputType == "text" {
				if inputText, ok := readString(inputObject["text"]); ok {
					text = inputText
					break
				}
			}
		}
	}
	return queuedSubmission{ID: id, ClientUserMessageID: clientID, Text: text}, true
}

func (s *supervisor) markQueueUpdated() {
	s.modifyState(func(state *supervisorState) { state.QueueUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano) })
	_ = s.persist()
}

func (s *supervisor) recordQueuedSubmission(item queuedSubmission) {
	s.modifyState(func(state *supervisorState) {
		state.QueueUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if state.QueueClientMessageIDs == nil {
			state.QueueClientMessageIDs = map[string]string{}
		}
		state.QueueClientMessageIDs[item.ID] = item.ClientUserMessageID
		for len(state.QueueClientMessageIDs) > 100 {
			for id := range state.QueueClientMessageIDs {
				delete(state.QueueClientMessageIDs, id)
				break
			}
		}
	})
	_ = s.persist()
}
