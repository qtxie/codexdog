package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func (s *supervisor) remoteRecent(ctx context.Context, args string) (string, error) {
	s.mu.Lock()
	threadID, proxy := s.state.CurrentThreadID, s.proxy
	s.mu.Unlock()
	if threadID == "" {
		return "", errors.New("there is no current Codex thread")
	}
	if proxy == nil {
		return "", errors.New("Codex TUI is not connected")
	}
	limit := 20
	if value := strings.TrimSpace(args); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return "", errors.New("usage: /recent [1-100]")
		}
		limit = parsed
	}
	value, err := proxy.Request(ctx, "thread/timeline/list", map[string]any{"threadId": threadID, "limit": limit})
	if err == nil {
		if message := formatTimeline(value, limit); message != "" {
			return message, nil
		}
		return "No recent timeline entries.", nil
	}
	// thread/timeline/list is experimental. Older app servers first use the
	// bounded turns page and then fall back to stable thread/read history.
	fallback, fallbackErr := proxy.Request(ctx, "thread/turns/list", map[string]any{
		"threadId":      threadID,
		"limit":         limit,
		"sortDirection": "asc",
		"itemsView":     "summary",
	})
	if fallbackErr != nil {
		fallback, fallbackErr = proxy.Request(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	}
	if fallbackErr != nil {
		return "", fmt.Errorf("recent timeline unavailable: %w", err)
	}
	message := formatRecentTurns(fallback, limit)
	if message == "" {
		message = "No recent turns."
	}
	return "Timeline unavailable on this Codex version; showing recent turns.\n\n" + message, nil
}

func formatTimeline(value any, limit int) string {
	object, ok := asObject(value)
	if !ok {
		return ""
	}
	entries, ok := object["data"].([]any)
	if !ok {
		return ""
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	lines := []string{"Recent timeline:"}
	for _, raw := range entries {
		entry, ok := asObject(raw)
		if !ok {
			continue
		}
		line := formatTimelineEntry(entry)
		if line != "" {
			lines = append(lines, "- "+line)
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func formatTimelineEntry(entry map[string]any) string {
	typeName, _ := readString(entry["type"])
	switch typeName {
	case "turnStarted":
		id, _ := readString(entry["turn_id"])
		if id == "" {
			id, _ = readString(entry["turnId"])
		}
		return "turn " + valueOrDash(id) + " started"
	case "turnCompleted":
		id, _ := readString(entry["turn_id"])
		if id == "" {
			id, _ = readString(entry["turnId"])
		}
		status, _ := readString(entry["status"])
		return "turn " + valueOrDash(id) + " " + valueOrDash(status)
	case "item", "realtime":
		item, _ := asObject(entry["item"])
		if item == nil {
			return typeName
		}
		itemType, _ := readString(item["type"])
		if text, ok := readString(item["text"]); ok && strings.TrimSpace(text) != "" {
			return itemType + ": " + compactTimelineText(text)
		}
		if message, ok := readString(item["message"]); ok && strings.TrimSpace(message) != "" {
			return itemType + ": " + compactTimelineText(message)
		}
		return itemType
	default:
		return typeName
	}
}

func formatRecentTurns(value any, limit int) string {
	object, ok := asObject(value)
	if !ok {
		return ""
	}
	turns, ok := object["data"].([]any)
	if !ok {
		thread, threadOK := asObject(object["thread"])
		if !threadOK {
			return ""
		}
		turns, ok = thread["turns"].([]any)
		if !ok {
			return ""
		}
	}
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	lines := []string{"Recent turns:"}
	for _, raw := range turns {
		turn, ok := asObject(raw)
		if !ok {
			continue
		}
		id, _ := readString(turn["id"])
		status, _ := readString(turn["status"])
		if id != "" || status != "" {
			lines = append(lines, "- turn "+valueOrDash(id)+" "+valueOrDash(status))
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func compactTimelineText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 160 {
		return value[:157] + "..."
	}
	return value
}
