package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// This opt-in test runs the real CLI against a loopback-only fake provider.
func TestCodexProbeRequestContext(t *testing.T) {
	if os.Getenv("CODEXDOG_CODEX_PROBE_TEST") != "1" {
		t.Skip("set CODEXDOG_CODEX_PROBE_TEST=1 to inspect a real CLI request against a local fake provider")
	}
	codexPath, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests [][]byte
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		requests = append(requests, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_probe\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_probe\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"CODEX_PROVIDER_OK\"}]}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":100,\"output_tokens\":10,\"total_tokens\":110}}}\n\n")
	}))
	defer provider.Close()
	codexHome := t.TempDir()
	projectCWD := t.TempDir()
	config := fmt.Sprintf(`model = "gpt-5.6-sol"
model_provider = "probe_fixture"
[features]
enable_request_compression = false
[model_providers.probe_fixture]
name = "Local probe fixture"
base_url = %q
wire_api = "responses"
requires_openai_auth = false
experimental_bearer_token = "local-fixture-only"
[mcp_servers.unused]
command = "codexdog-test-command-does-not-exist"
required = true
`, provider.URL)
	for path, content := range map[string]string{
		filepath.Join(codexHome, "config.toml"): config,
		filepath.Join(projectCWD, "AGENTS.md"):  "PROJECT_CONTEXT_MUST_NOT_REACH_PROBE",
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	port, err := getFreePort()
	if err != nil {
		t.Fatal(err)
	}
	appURL := fmt.Sprintf("ws://127.0.0.1:%d", port)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath, "app-server", "--listen", appURL)
	cmd.Dir = projectCWD
	cmd.Env = []string{}
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "CODEX_SESSION_ID=") || strings.HasPrefix(value, "CODEX_THREAD_ID=") || strings.HasPrefix(value, "CODEX_CI=") || strings.HasPrefix(value, "CODEX_HOME=") {
			continue
		}
		cmd.Env = append(cmd.Env, value)
	}
	cmd.Env = append(cmd.Env, "CODEX_HOME="+codexHome)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	processes, err := newProcessTree()
	if err != nil {
		t.Fatal(err)
	}
	defer processes.Close()
	if err := processes.Start(cmd, true); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		cancel()
		_ = processes.Close()
		<-done
		if t.Failed() {
			t.Log(output.String())
		}
	}()
	rpc := newJSONRPCClient(appURL, 10*time.Second)
	for {
		if err = rpc.Connect(ctx); err == nil {
			break
		}
		if !waitContext(ctx, 100*time.Millisecond) {
			t.Fatal(err)
		}
	}
	defer rpc.Close()
	if err := rpc.InitializeWithClientInfo(ctx, appServerClientInfo{Name: "codex-tui", Version: testedCodexVersion}, false); err != nil {
		t.Fatal(err)
	}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: projectCWD, Timeout: 10 * time.Second})
	defer probe.Dispose()
	for i := 0; i < 2; i++ {
		if result := probe.Check(ctx, "gpt-5.6-sol"); !result.Healthy {
			t.Fatalf("probe %d failed: %#v", i+1, result.Failure)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(requests))
	}
	for i, body := range requests {
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("request is not JSON: %v", err)
		}
		t.Logf("request %d: %d bytes", i+1, len(body))
		if strings.Contains(string(body), "PROJECT_CONTEXT_MUST_NOT_REACH_PROBE") {
			t.Error("AGENTS.md context leaked into the probe")
		}
		if bytes.Count(body, []byte(healthProbeUserPrompt)) != 1 {
			t.Error("probe prompt missing or repeated through history")
		}
		if len(body) > 32*1024 {
			t.Errorf("probe request is unexpectedly large: %d bytes", len(body))
		}
	}
}
