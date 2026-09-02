package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseHealthSourceSpec(t *testing.T) {
	tests := []struct {
		value    string
		wantType string
		wantURL  string
		model    string
	}{
		{"https://status.ciii.club/status/codex", healthSourceUptimeKuma, "https://status.ciii.club/status/codex", ""},
		{"https://status.input.im/", healthSourceInputIM, "https://status.input.im/", ""},
		{"http=https://provider.example/health", healthSourceHTTP, "https://provider.example/health", ""},
		{`{"type":"input-im","url":"https://status.input.im/","model":"gpt-5.6-sol"}`, healthSourceInputIM, "https://status.input.im/", "gpt-5.6-sol"},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			got, err := parseHealthSourceSpec(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if got.Type != test.wantType || got.URL != test.wantURL || got.Model != test.model {
				t.Fatalf("source = %#v", got)
			}
		})
	}
	for _, value := range []string{"", "unknown=https://example.com", "uptime-kuma=https://example.com/not-a-page", `{"type":"http","url":"https://example.com","extra":true}`} {
		if _, err := parseHealthSourceSpec(value); err == nil {
			t.Fatalf("invalid source %q was accepted", value)
		}
	}
}

func TestHealthSourceEndpointDerivation(t *testing.T) {
	tests := []struct {
		input     string
		metadata  string
		heartbeat string
	}{
		{"https://status.ciii.club/status/codex", "https://status.ciii.club/api/status-page/codex", "https://status.ciii.club/api/status-page/heartbeat/codex"},
		{"https://example.test/monitor/status/team-a/", "https://example.test/monitor/api/status-page/team-a", "https://example.test/monitor/api/status-page/heartbeat/team-a"},
		{"https://status.ciii.club/api/status-page/codex", "https://status.ciii.club/api/status-page/codex", "https://status.ciii.club/api/status-page/heartbeat/codex"},
	}
	for _, test := range tests {
		metadata, heartbeat, err := uptimeKumaEndpoints(test.input)
		if err != nil {
			t.Fatalf("uptimeKumaEndpoints(%q): %v", test.input, err)
		}
		if metadata != test.metadata || heartbeat != test.heartbeat {
			t.Fatalf("uptimeKumaEndpoints(%q) = %q, %q", test.input, metadata, heartbeat)
		}
	}
	if got, err := inputIMStatusEndpoint("https://status.input.im/"); err != nil || got != "https://status.input.im/api/status" {
		t.Fatalf("inputIMStatusEndpoint = %q, %v", got, err)
	}
}

func TestProjectConfigParsesTypedHealthSources(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, `
version = 1
probe_model = "gpt-5.6-sol"

[health]
policy = "all"
unknown_policy = "block"
max_age_ms = 240000

[[health.sources]]
type = "uptime_kuma"
url = "https://status.ciii.club/status/codex"
name = "ciii"
provider = "ciii"

[[health.sources]]
type = "input_im"
url = "https://status.input.im/"
model = "gpt-5.6-terra"
`)
	args, err := parseArguments([]string{"start", "-C", workspace})
	if err != nil {
		t.Fatal(err)
	}
	health := args.Options.HealthChecks
	if health.Policy != healthPolicyAll || health.UnknownPolicy != healthUnknownBlock || health.MaxAge != 4*time.Minute {
		t.Fatalf("health options = %#v", health)
	}
	if len(health.Sources) != 2 || health.Sources[0].Name != "ciii" || health.Sources[0].Provider != "ciii" || health.Sources[1].Model != "gpt-5.6-terra" {
		t.Fatalf("health sources = %#v", health.Sources)
	}
}

func TestTypedHealthConfigurationRejectsLegacyHealthURL(t *testing.T) {
	clearProjectConfigEnvironment(t)
	workspace := t.TempDir()
	writeProjectConfigFile(t, workspace, `
health_url = "https://provider.example/health"

[health]
policy = "any"
`)
	if _, err := parseArguments([]string{"start", "-C", workspace}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("legacy/typed conflict error = %v", err)
	}
}

func TestUptimeKumaHealthSourceUsesTargetModelAndCachesMetadata(t *testing.T) {
	var mu sync.Mutex
	metadataRequests := 0
	heartbeatRequests := 0
	modelStatus := 1
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status-page/codex":
			mu.Lock()
			metadataRequests++
			mu.Unlock()
			writeHealthJSON(t, response, map[string]any{"publicGroupList": []any{map[string]any{"monitorList": []any{
				map[string]any{"id": 4, "name": "Ciii-codex gpt-5.4"},
				map[string]any{"id": 5, "name": "Ciii-codex gpt-5.4-mini"},
			}}}})
		case "/api/status-page/heartbeat/codex":
			mu.Lock()
			heartbeatRequests++
			status := modelStatus
			mu.Unlock()
			stamp := now.Format("2006-01-02 15:04:05.000")
			writeHealthJSON(t, response, map[string]any{"heartbeatList": map[string]any{
				"4": []any{map[string]any{"status": status, "time": stamp, "msg": ""}},
				"5": []any{map[string]any{"status": 0, "time": stamp, "msg": "mini down"}},
			}})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceUptimeKuma, URL: server.URL + "/status/codex"}}})
	first := checker.check(context.Background(), "gpt-5.4", "", time.Second)
	if first.State != healthStateHealthy || len(first.Observations) != 1 || first.Observations[0].State != healthStateHealthy {
		t.Fatalf("first result = %#v", first)
	}
	mu.Lock()
	modelStatus = 0
	mu.Unlock()
	second := checker.check(context.Background(), "gpt-5.4", "", time.Second)
	if second.State != healthStateUnhealthy || second.Observations[0].State != healthStateUnhealthy {
		t.Fatalf("second result = %#v", second)
	}
	mu.Lock()
	gotMetadata, gotHeartbeats := metadataRequests, heartbeatRequests
	mu.Unlock()
	if gotMetadata != 1 || gotHeartbeats != 2 {
		t.Fatalf("requests = metadata %d, heartbeats %d", gotMetadata, gotHeartbeats)
	}
}

func TestUptimeKumaHealthSourceTreatsPendingAndStaleAsUnknown(t *testing.T) {
	tests := []struct {
		name   string
		status int
		stamp  time.Time
	}{
		{"pending", 2, time.Now().UTC()},
		{"maintenance", 3, time.Now().UTC()},
		{"stale", 1, time.Now().UTC().Add(-10 * time.Minute)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newUptimeKumaFixture(t, "gpt-5.6-sol", test.status, test.stamp)
			defer server.Close()
			checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceUptimeKuma, URL: server.URL + "/status/codex"}}, MaxAge: time.Minute})
			result := checker.check(context.Background(), "gpt-5.6-sol", "", time.Second)
			if result.State != healthStateUnknown || result.Observations[0].State != healthStateUnknown {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestUptimeKumaMetadataRetryAfterIsPreserved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/status-page/codex" {
			response.Header().Set("Retry-After", "4")
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceUptimeKuma, URL: server.URL + "/status/codex"}}})
	result := checker.check(context.Background(), "gpt-5.6-sol", "", time.Second)
	if result.State != healthStateUnknown || result.RetryAfter != 4*time.Second {
		t.Fatalf("result = %#v", result)
	}
}

func TestInputIMHealthSourceUsesPerModelStatusAndFreshness(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name      string
		generated time.Time
		services  []map[string]any
		want      string
	}{
		{"selected healthy despite global failure", now, []map[string]any{
			{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": true}},
			{"model": "gpt-5.4", "last": map[string]any{"ts": now.Unix(), "ok": false, "error": "down"}},
		}, healthStateHealthy},
		{"selected unhealthy", now, []map[string]any{{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": false, "error": "model_not_found"}}}, healthStateUnhealthy},
		{"stale document", now.Add(-10 * time.Minute), []map[string]any{{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": true}}}, healthStateUnknown},
		{"missing model", now, []map[string]any{{"model": "gpt-5.5", "last": map[string]any{"ts": now.Unix(), "ok": true}}}, healthStateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/status" {
					http.NotFound(response, request)
					return
				}
				response.Header().Set("Content-Type", "application/json")
				writeHealthJSON(t, response, map[string]any{"all_ok": false, "generated_at": test.generated.Unix(), "services": test.services})
			}))
			defer server.Close()
			checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}, MaxAge: time.Minute})
			result := checker.check(context.Background(), "gpt-5.6-sol", "", time.Second)
			if result.State != test.want || len(result.Observations) != 1 || result.Observations[0].State != test.want {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestInputIMHealthSourceRejectsMalformedOrOversizedResponses(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{"malformed", "application/json", "{"},
		{"html", "text/html", "<html></html>"},
		{"oversized", "application/json", `{"padding":"` + strings.Repeat("x", healthResponseMaxSize) + `"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", test.contentType)
				_, _ = response.Write([]byte(test.body))
			}))
			defer server.Close()
			checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}})
			result := checker.check(context.Background(), "gpt-5.6-sol", "", time.Second)
			if result.State != healthStateUnknown {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

type staticHealthSource struct {
	config healthSourceConfig
	state  string
}

func (s staticHealthSource) configuration() healthSourceConfig { return s.config }
func (s staticHealthSource) check(_ context.Context, model string, _ time.Duration) healthObservation {
	observation := newHealthObservation(s.config, model)
	observation.State = s.state
	return observation
}

func TestHealthCheckerAggregationPolicies(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		states []string
		want   string
	}{
		{"any accepts one healthy", healthPolicyAny, []string{healthStateHealthy, healthStateUnhealthy}, healthStateHealthy},
		{"any preserves partial unknown", healthPolicyAny, []string{healthStateUnhealthy, healthStateUnknown}, healthStateUnknown},
		{"any rejects all unhealthy", healthPolicyAny, []string{healthStateUnhealthy, healthStateUnhealthy}, healthStateUnhealthy},
		{"all accepts all healthy", healthPolicyAll, []string{healthStateHealthy, healthStateHealthy}, healthStateHealthy},
		{"all rejects one unhealthy", healthPolicyAll, []string{healthStateHealthy, healthStateUnhealthy}, healthStateUnhealthy},
		{"all preserves unknown", healthPolicyAll, []string{healthStateHealthy, healthStateUnknown}, healthStateUnknown},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := &healthChecker{options: healthCheckOptions{Policy: test.policy, UnknownPolicy: healthUnknownCanary, MaxAge: time.Minute}}
			for index, state := range test.states {
				checker.sources = append(checker.sources, staticHealthSource{config: healthSourceConfig{Type: healthSourceHTTP, URL: fmt.Sprintf("https://status-%d.example", index)}, state: state})
			}
			result := checker.check(context.Background(), "model", "", time.Second)
			if result.State != test.want {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestProviderProbeHealthGateControlsRealCanary(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name          string
		statusBody    any
		unknownPolicy string
		wantHealthy   bool
		wantState     string
		wantTurns     int
	}{
		{"healthy runs canary", map[string]any{"generated_at": now.Unix(), "services": []any{map[string]any{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": true}}}}, healthUnknownCanary, true, healthStateHealthy, 1},
		{"unhealthy skips canary", map[string]any{"generated_at": now.Unix(), "services": []any{map[string]any{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": false}}}}, healthUnknownCanary, false, healthStateUnhealthy, 0},
		{"unknown falls back to canary", map[string]any{"generated_at": now.Unix(), "services": []any{}}, healthUnknownCanary, true, healthStateUnknown, 1},
		{"unknown can block", map[string]any{"generated_at": now.Unix(), "services": []any{}}, healthUnknownBlock, false, healthStateUnknown, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				writeHealthJSON(t, response, test.statusBody)
			}))
			defer server.Close()
			rpc := &mockRPC{}
			probe := newProviderProbe(rpc, providerProbeOptions{
				CWD: t.TempDir(), Timeout: time.Second, Model: "gpt-5.6-sol",
				HealthChecks: healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}, UnknownPolicy: test.unknownPolicy},
			})
			defer probe.Dispose()
			result := probe.Check(context.Background(), "runtime-model", "provider")
			if result.Healthy != test.wantHealthy || result.HealthState != test.wantState {
				t.Fatalf("result = %#v", result)
			}
			rpc.mu.Lock()
			turns := rpc.turnStarts
			rpc.mu.Unlock()
			if turns != test.wantTurns {
				t.Fatalf("canary turns = %d, want %d", turns, test.wantTurns)
			}
		})
	}
}

func TestProviderProbeUsesRuntimeModelForStatusSource(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		writeHealthJSON(t, response, map[string]any{"generated_at": now.Unix(), "services": []any{map[string]any{"model": "runtime-model", "last": map[string]any{"ts": now.Unix(), "ok": true}}}})
	}))
	defer server.Close()
	rpc := &mockRPC{}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second, HealthChecks: healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}}})
	defer probe.Dispose()
	result := probe.Check(context.Background(), "runtime-model", "runtime-provider")
	if !result.Healthy || len(result.HealthObservations) != 1 || result.HealthObservations[0].Model != "runtime-model" {
		t.Fatalf("result = %#v", result)
	}
	rpc.mu.Lock()
	threadModel, turnModel := rpc.threadModel, rpc.turnModel
	rpc.mu.Unlock()
	if threadModel != "runtime-model" || turnModel != "runtime-model" {
		t.Fatalf("canary models = thread %q, turn %q", threadModel, turnModel)
	}
}

func TestProviderProbeUsesExplicitSourceModelForStatusAndCanary(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		writeHealthJSON(t, response, map[string]any{"generated_at": now.Unix(), "services": []any{map[string]any{"model": "source-model", "last": map[string]any{"ts": now.Unix(), "ok": true}}}})
	}))
	defer server.Close()
	rpc := &mockRPC{}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second, Model: "probe-model", HealthChecks: healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL, Model: "source-model"}}}})
	defer probe.Dispose()
	result := probe.Check(context.Background(), "runtime-model", "runtime-provider")
	if !result.Healthy || len(result.HealthObservations) != 1 || result.HealthObservations[0].Model != "source-model" {
		t.Fatalf("result = %#v", result)
	}
	rpc.mu.Lock()
	threadModel, turnModel := rpc.threadModel, rpc.turnModel
	rpc.mu.Unlock()
	if threadModel != "source-model" || turnModel != "source-model" {
		t.Fatalf("canary models = thread %q, turn %q", threadModel, turnModel)
	}
}

func TestHealthStatusRequestUserAgentAndRedirectLimit(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	userAgent := ""
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		requests++
		userAgent = request.Header.Get("User-Agent")
		current := requests
		mu.Unlock()
		http.Redirect(response, request, fmt.Sprintf("/redirect/%d", current), http.StatusFound)
	}))
	defer server.Close()
	checker := mustHealthChecker(t, healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}})
	result := checker.check(context.Background(), "gpt-5.6-sol", "", time.Second)
	if result.State != healthStateUnknown || len(result.Observations) != 1 || !strings.Contains(result.Observations[0].Detail, "3 redirects") {
		t.Fatalf("result = %#v", result)
	}
	mu.Lock()
	gotRequests, gotUserAgent := requests, userAgent
	mu.Unlock()
	if gotRequests != 4 {
		t.Fatalf("requests = %d, want 4", gotRequests)
	}
	if gotUserAgent != "codexdog/"+version+" status-check" {
		t.Fatalf("User-Agent = %q", gotUserAgent)
	}
}

func TestDoctorHealthSourcesReportsStatusWithoutCanary(t *testing.T) {
	now := time.Now().UTC()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		writeHealthJSON(t, response, map[string]any{"generated_at": now.Unix(), "services": []any{map[string]any{"model": "gpt-5.6-sol", "last": map[string]any{"ts": now.Unix(), "ok": true}}}})
	}))
	defer server.Close()
	options := supervisorOptions{
		ProbeTimeout: time.Second,
		ProbeModel:   "gpt-5.6-sol",
		HealthChecks: healthCheckOptions{Sources: []healthSourceConfig{{Type: healthSourceInputIM, URL: server.URL}}},
	}
	report := checkDoctorHealthSources(context.Background(), options, "", "provider")
	if report == nil || report.Status != "pass" || report.Model != "gpt-5.6-sol" || len(report.Observations) != 1 {
		t.Fatalf("doctor health report = %#v", report)
	}
}

func TestDoctorSupervisorCopiesPersistedHealthObservations(t *testing.T) {
	state := supervisorState{
		HealthState: "unknown", HealthDetail: "one unknown", HealthModel: "gpt-5.6-sol", HealthProvider: "ciii",
		HealthObservations: []healthObservationState{{Source: "status.ciii.club", Type: healthSourceUptimeKuma, State: healthStateUnknown}},
	}
	report := doctorSupervisorFromState(state, true)
	if report.HealthState != state.HealthState || report.HealthModel != state.HealthModel || report.HealthProvider != state.HealthProvider || len(report.HealthObservations) != 1 {
		t.Fatalf("doctor supervisor = %#v", report)
	}
}

func TestLegacyHealthURLRetainsHTTPStatusSemantics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Retry-After", "2")
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	rpc := &mockRPC{}
	probe := newProviderProbe(rpc, providerProbeOptions{CWD: t.TempDir(), Timeout: time.Second, HealthURL: server.URL})
	defer probe.Dispose()
	result := probe.Check(context.Background(), "", "")
	if result.Healthy || result.HealthState != healthStateUnhealthy || result.RetryAfter != 2*time.Second || result.Failure == nil || result.Failure.Code != "httpConnectionFailed" || result.Failure.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("result = %#v", result)
	}
	rpc.mu.Lock()
	turns := rpc.turnStarts
	rpc.mu.Unlock()
	if turns != 0 {
		t.Fatalf("legacy failed health URL ran %d canaries", turns)
	}
}

func TestHealthSourceProviderFilter(t *testing.T) {
	checker := &healthChecker{
		options: healthCheckOptions{Policy: healthPolicyAny, UnknownPolicy: healthUnknownCanary, MaxAge: time.Minute},
		sources: []healthSource{
			staticHealthSource{config: healthSourceConfig{Type: healthSourceHTTP, URL: "https://ciii.example", Provider: "ciii"}, state: healthStateHealthy},
			staticHealthSource{config: healthSourceConfig{Type: healthSourceHTTP, URL: "https://input.example", Provider: "input"}, state: healthStateUnhealthy},
		},
	}
	result := checker.check(context.Background(), "model", "ciii", time.Second)
	if result.State != healthStateHealthy || len(result.Observations) != 1 || result.Observations[0].Source != "ciii.example" {
		t.Fatalf("result = %#v", result)
	}
	unknown := checker.check(context.Background(), "model", "", time.Second)
	if unknown.State != healthStateUnknown || len(unknown.Observations) != 0 {
		t.Fatalf("unknown-provider result = %#v", unknown)
	}
	if got := checker.targetModelForProvider("probe-model", "runtime-model", "input"); got != "probe-model" {
		t.Fatalf("provider-filtered target model = %q", got)
	}
}

func mustHealthChecker(t *testing.T, options healthCheckOptions) *healthChecker {
	t.Helper()
	normalized, err := normalizeHealthCheckOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	return newHealthChecker(normalized)
}

func newUptimeKumaFixture(t *testing.T, model string, status int, stamp time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/status-page/codex":
			writeHealthJSON(t, response, map[string]any{"publicGroupList": []any{map[string]any{"monitorList": []any{map[string]any{"id": 1, "name": "Provider " + model}}}}})
		case "/api/status-page/heartbeat/codex":
			writeHealthJSON(t, response, map[string]any{"heartbeatList": map[string]any{"1": []any{map[string]any{"status": status, "time": stamp.UTC().Format("2006-01-02 15:04:05.000")}}}})
		default:
			http.NotFound(response, request)
		}
	}))
}

func writeHealthJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("encode fixture: %v", err)
	}
}
