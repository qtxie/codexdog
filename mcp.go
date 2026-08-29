package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// refreshMCPStatus is best effort. MCP inventory is observability data and
// must never delay turn supervision or provider recovery.
func (s *supervisor) refreshMCPStatus(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	proxy := s.proxy
	threadID := s.state.CurrentThreadID
	shuttingDown := s.shuttingDown
	s.mu.Unlock()
	if proxy == nil || shuttingDown {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	value, err := proxy.Request(requestCtx, "mcpServerStatus/list", map[string]any{
		"threadId": threadID,
		"detail":   "toolsAndAuthOnly",
		"limit":    100,
	})
	cancel()
	if err != nil {
		s.modifyState(func(state *supervisorState) {
			state.MCPLastError = sanitizeText(err.Error())
			state.MCPUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		})
		_ = s.persist()
		return
	}
	servers, ok := readMCPServerStates(value)
	if !ok {
		s.modifyState(func(state *supervisorState) {
			state.MCPLastError = "Codex returned an invalid MCP status response"
			state.MCPUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		})
		_ = s.persist()
		return
	}
	s.modifyState(func(state *supervisorState) {
		state.MCPServers = servers
		state.MCPLastError = ""
		state.MCPUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	})
	_ = s.persist()
}

func formatMCPStatus(state supervisorState) []string {
	if len(state.MCPServers) == 0 {
		if state.MCPLastError != "" {
			return []string{"MCP: unavailable (" + state.MCPLastError + ")"}
		}
		return []string{"MCP: no configured servers or status unavailable"}
	}
	lines := []string{"MCP servers:"}
	for _, server := range state.MCPServers {
		name := server.Name
		if name == "" {
			name = "(unnamed)"
		}
		status := server.RuntimeStatus
		if status == "" {
			status = "unknown"
		}
		line := fmt.Sprintf("- %s: %s", name, status)
		if server.AuthStatus != "" && server.AuthStatus != "unknown" {
			line += ", auth " + server.AuthStatus
		}
		line += fmt.Sprintf(", %d tools", server.ToolCount)
		if strings.TrimSpace(server.PluginID) != "" {
			line += ", plugin " + server.PluginID
		}
		lines = append(lines, line)
	}
	if state.MCPUpdatedAt != "" {
		lines = append(lines, "MCP updated: "+state.MCPUpdatedAt)
	}
	if state.MCPLastError != "" {
		lines = append(lines, "MCP status error: "+state.MCPLastError)
	}
	return lines
}
