package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	healthSourceHTTP       = "http"
	healthSourceUptimeKuma = "uptime_kuma"
	healthSourceInputIM    = "input_im"

	healthPolicyAny = "any"
	healthPolicyAll = "all"

	healthUnknownCanary = "canary"
	healthUnknownBlock  = "block"

	healthStateHealthy   = "healthy"
	healthStateUnhealthy = "unhealthy"
	healthStateUnknown   = "unknown"

	defaultHealthMaxAge   = 3 * time.Minute
	statusSourceTimeout   = 15 * time.Second
	healthResponseMaxSize = 1024 * 1024
	uptimeMetadataTTL     = 5 * time.Minute
)

type healthSourceConfig struct {
	Type     string `toml:"type" json:"type"`
	URL      string `toml:"url" json:"url"`
	Name     string `toml:"name" json:"name,omitempty"`
	Model    string `toml:"model" json:"model,omitempty"`
	Provider string `toml:"provider" json:"provider,omitempty"`
	Legacy   bool   `toml:"-" json:"-"`
}

type healthCheckOptions struct {
	Sources       []healthSourceConfig
	Policy        string
	UnknownPolicy string
	MaxAge        time.Duration
}

type healthObservation struct {
	Source           string        `json:"source"`
	Type             string        `json:"type"`
	Model            string        `json:"model,omitempty"`
	State            string        `json:"state"`
	CheckedAt        time.Time     `json:"checkedAt"`
	ObservedAt       time.Time     `json:"observedAt,omitempty"`
	Detail           string        `json:"detail,omitempty"`
	RetryAfter       time.Duration `json:"-"`
	HTTPStatus       int           `json:"-"`
	ConnectionFailed bool          `json:"-"`
}

type healthObservationState struct {
	Source     string `json:"source"`
	Type       string `json:"type"`
	Model      string `json:"model,omitempty"`
	State      string `json:"state"`
	CheckedAt  string `json:"checkedAt"`
	ObservedAt string `json:"observedAt,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

type healthGateResult struct {
	State        string
	Detail       string
	RetryAfter   time.Duration
	Observations []healthObservation
}

type healthSource interface {
	configuration() healthSourceConfig
	check(context.Context, string, time.Duration) healthObservation
}

type healthChecker struct {
	options healthCheckOptions
	sources []healthSource
}

func (c *healthChecker) targetModelForProvider(probeModel, runtimeModel, provider string) string {
	for _, source := range c.sources {
		configuredProvider := strings.TrimSpace(source.configuration().Provider)
		if configuredProvider != "" && !strings.EqualFold(configuredProvider, strings.TrimSpace(provider)) {
			continue
		}
		if model := strings.TrimSpace(source.configuration().Model); model != "" {
			return model
		}
	}
	if model := strings.TrimSpace(probeModel); model != "" {
		return model
	}
	return strings.TrimSpace(runtimeModel)
}

func defaultHealthCheckOptions() healthCheckOptions {
	return healthCheckOptions{Policy: healthPolicyAny, UnknownPolicy: healthUnknownCanary, MaxAge: defaultHealthMaxAge}
}

func normalizeHealthCheckOptions(options healthCheckOptions) (healthCheckOptions, error) {
	if options.Policy == "" {
		options.Policy = healthPolicyAny
	}
	options.Policy = strings.ToLower(strings.TrimSpace(options.Policy))
	if options.Policy != healthPolicyAny && options.Policy != healthPolicyAll {
		return healthCheckOptions{}, fmt.Errorf("health policy must be %q or %q", healthPolicyAny, healthPolicyAll)
	}
	if options.UnknownPolicy == "" {
		options.UnknownPolicy = healthUnknownCanary
	}
	options.UnknownPolicy = strings.ToLower(strings.TrimSpace(options.UnknownPolicy))
	if options.UnknownPolicy != healthUnknownCanary && options.UnknownPolicy != healthUnknownBlock {
		return healthCheckOptions{}, fmt.Errorf("health unknown policy must be %q or %q", healthUnknownCanary, healthUnknownBlock)
	}
	if options.MaxAge == 0 {
		options.MaxAge = defaultHealthMaxAge
	}
	if options.MaxAge < 0 {
		return healthCheckOptions{}, errors.New("health max age must be positive")
	}
	for index := range options.Sources {
		normalized, err := normalizeHealthSourceConfig(options.Sources[index])
		if err != nil {
			return healthCheckOptions{}, fmt.Errorf("health source %d: %w", index+1, err)
		}
		options.Sources[index] = normalized
	}
	return options, nil
}

func normalizeHealthSourceConfig(config healthSourceConfig) (healthSourceConfig, error) {
	config.Type = normalizeHealthSourceType(config.Type)
	config.URL = strings.TrimSpace(config.URL)
	config.Name = strings.Join(strings.Fields(config.Name), " ")
	config.Model = strings.Join(strings.Fields(config.Model), " ")
	config.Provider = strings.Join(strings.Fields(config.Provider), " ")
	if strings.EqualFold(config.Model, "auto") {
		config.Model = ""
	}
	if config.Type == "auto" || config.Type == "" {
		config.Type = inferHealthSourceType(config.URL)
	}
	switch config.Type {
	case healthSourceHTTP, healthSourceUptimeKuma, healthSourceInputIM:
	default:
		return healthSourceConfig{}, fmt.Errorf("unsupported type %q", config.Type)
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || (!strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https")) || parsed.Host == "" {
		return healthSourceConfig{}, fmt.Errorf("invalid HTTP URL %q", config.URL)
	}
	if config.Type == healthSourceUptimeKuma {
		if _, _, err := uptimeKumaEndpoints(config.URL); err != nil {
			return healthSourceConfig{}, err
		}
	}
	return config, nil
}

func normalizeHealthSourceType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "uptime-kuma", "uptime_kuma", "uptimekuma":
		return healthSourceUptimeKuma
	case "input-im", "input_im", "inputim":
		return healthSourceInputIM
	case "http", "http-status", "http_status":
		return healthSourceHTTP
	case "", "auto":
		return "auto"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func inferHealthSourceType(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err == nil {
		if strings.EqualFold(parsed.Hostname(), "status.input.im") {
			return healthSourceInputIM
		}
		if strings.Contains(strings.ToLower(parsed.Path), "/status/") {
			return healthSourceUptimeKuma
		}
	}
	return healthSourceHTTP
}

func parseHealthSourceSpec(value string) (healthSourceConfig, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return healthSourceConfig{}, errors.New("--health-source requires a non-empty value")
	}
	var config healthSourceConfig
	if strings.HasPrefix(value, "{") {
		decoder := json.NewDecoder(strings.NewReader(value))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&config); err != nil {
			return healthSourceConfig{}, fmt.Errorf("parse --health-source JSON: %w", err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return healthSourceConfig{}, fmt.Errorf("parse --health-source JSON: %w", err)
		}
	} else if lower := strings.ToLower(value); strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		config = healthSourceConfig{Type: "auto", URL: value}
	} else {
		parts := strings.SplitN(value, "=", 2)
		if len(parts) != 2 {
			return healthSourceConfig{}, errors.New("--health-source must be TYPE=URL, a URL, or a JSON object")
		}
		config = healthSourceConfig{Type: parts[0], URL: parts[1]}
	}
	return normalizeHealthSourceConfig(config)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func newHealthChecker(options healthCheckOptions) *healthChecker {
	normalized, err := normalizeHealthCheckOptions(options)
	if err != nil {
		// Argument parsing validates user configuration. Keep direct
		// test/programmatic construction fail-closed if it bypasses that path.
		normalized = defaultHealthCheckOptions()
		normalized.Sources = []healthSourceConfig{{Type: healthSourceHTTP, Name: "invalid health configuration"}}
	}
	checker := &healthChecker{options: normalized}
	for _, config := range normalized.Sources {
		switch config.Type {
		case healthSourceUptimeKuma:
			checker.sources = append(checker.sources, &uptimeKumaHealthSource{config: config})
		case healthSourceInputIM:
			checker.sources = append(checker.sources, inputIMHealthSource{config: config})
		default:
			checker.sources = append(checker.sources, httpStatusHealthSource{config: config})
		}
	}
	return checker
}

func healthOptionsWithLegacyURL(options healthCheckOptions, legacyURL string) healthCheckOptions {
	options.Sources = append([]healthSourceConfig(nil), options.Sources...)
	if strings.TrimSpace(legacyURL) != "" {
		options.Sources = append(options.Sources, healthSourceConfig{Type: healthSourceHTTP, URL: strings.TrimSpace(legacyURL), Name: "legacy health URL", Legacy: true})
	}
	return options
}

func (c *healthChecker) check(ctx context.Context, model, provider string, timeout time.Duration) healthGateResult {
	type indexedObservation struct {
		index       int
		observation healthObservation
	}
	selected := make([]struct {
		index  int
		source healthSource
		model  string
	}, 0, len(c.sources))
	for index, source := range c.sources {
		config := source.configuration()
		if config.Provider != "" && !strings.EqualFold(config.Provider, provider) {
			continue
		}
		targetModel := strings.TrimSpace(model)
		if config.Model != "" {
			targetModel = config.Model
		}
		selected = append(selected, struct {
			index  int
			source healthSource
			model  string
		}{index: index, source: source, model: targetModel})
	}
	if len(selected) == 0 {
		detail := "no health source is configured"
		if provider != "" && len(c.sources) > 0 {
			detail = fmt.Sprintf("no health source matches provider %q", provider)
		}
		return healthGateResult{State: healthStateUnknown, Detail: detail}
	}
	if timeout <= 0 || timeout > statusSourceTimeout {
		timeout = statusSourceTimeout
	}
	results := make(chan indexedObservation, len(selected))
	for _, item := range selected {
		item := item
		go func() {
			requestCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results <- indexedObservation{index: item.index, observation: item.source.check(requestCtx, item.model, c.options.MaxAge)}
		}()
	}
	indexed := make([]indexedObservation, 0, len(selected))
	for range selected {
		indexed = append(indexed, <-results)
	}
	sort.Slice(indexed, func(i, j int) bool { return indexed[i].index < indexed[j].index })
	gate := healthGateResult{Observations: make([]healthObservation, 0, len(indexed))}
	healthy, unhealthy, unknown := 0, 0, 0
	for _, item := range indexed {
		observation := item.observation
		gate.Observations = append(gate.Observations, observation)
		if observation.RetryAfter > gate.RetryAfter {
			gate.RetryAfter = observation.RetryAfter
		}
		switch observation.State {
		case healthStateHealthy:
			healthy++
		case healthStateUnhealthy:
			unhealthy++
		default:
			unknown++
		}
	}
	if c.options.Policy == healthPolicyAll {
		switch {
		case unhealthy > 0:
			gate.State = healthStateUnhealthy
		case healthy == len(indexed):
			gate.State = healthStateHealthy
		default:
			gate.State = healthStateUnknown
		}
	} else {
		switch {
		case healthy > 0:
			gate.State = healthStateHealthy
		case unhealthy == len(indexed):
			gate.State = healthStateUnhealthy
		default:
			gate.State = healthStateUnknown
		}
	}
	gate.Detail = fmt.Sprintf("%d healthy, %d unhealthy, %d unknown (%s policy)", healthy, unhealthy, unknown, c.options.Policy)
	return gate
}

type httpStatusHealthSource struct{ config healthSourceConfig }

func (s httpStatusHealthSource) configuration() healthSourceConfig { return s.config }

func (s httpStatusHealthSource) check(ctx context.Context, model string, _ time.Duration) healthObservation {
	observation := newHealthObservation(s.config, model)
	response, err := fetchHealthResponse(ctx, s.config.URL)
	if err != nil {
		observation.ConnectionFailed = true
		if s.config.Legacy {
			observation.State = healthStateUnhealthy
		} else {
			observation.State = healthStateUnknown
		}
		observation.Detail = compactHealthDetail(err.Error())
		return observation
	}
	defer response.Body.Close()
	observation.HTTPStatus = response.StatusCode
	observation.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), observation.CheckedAt)
	observation.Detail = fmt.Sprintf("HTTP %d", response.StatusCode)
	if response.StatusCode >= 200 && response.StatusCode <= 299 {
		observation.State = healthStateHealthy
	} else {
		observation.State = healthStateUnhealthy
	}
	return observation
}

type uptimeKumaMonitor struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type uptimeKumaHealthSource struct {
	config healthSourceConfig
	mu     sync.Mutex
	until  time.Time
	items  []uptimeKumaMonitor
}

func (s *uptimeKumaHealthSource) configuration() healthSourceConfig { return s.config }

func (s *uptimeKumaHealthSource) check(ctx context.Context, model string, maxAge time.Duration) healthObservation {
	observation := newHealthObservation(s.config, model)
	if model == "" {
		observation.Detail = "target model is unavailable"
		return observation
	}
	metadataURL, heartbeatURL, err := uptimeKumaEndpoints(s.config.URL)
	if err != nil {
		observation.Detail = compactHealthDetail(err.Error())
		return observation
	}
	monitors, retryAfter, err := s.monitors(ctx, metadataURL, observation.CheckedAt)
	observation.RetryAfter = retryAfter
	if err != nil {
		observation.Detail = compactHealthDetail(err.Error())
		return observation
	}
	matches := make([]uptimeKumaMonitor, 0, 1)
	for _, monitor := range monitors {
		if healthModelNameMatches(monitor.Name, model) {
			matches = append(matches, monitor)
		}
	}
	if len(matches) == 0 {
		observation.Detail = fmt.Sprintf("model %q was not found", model)
		return observation
	}
	if len(matches) > 1 {
		observation.Detail = fmt.Sprintf("model %q matched multiple monitors", model)
		return observation
	}
	var document struct {
		HeartbeatList map[string][]struct {
			Status *int   `json:"status"`
			Time   string `json:"time"`
			Msg    string `json:"msg"`
		} `json:"heartbeatList"`
	}
	response, err := fetchHealthJSON(ctx, heartbeatURL, &document)
	if err != nil {
		observation.Detail = compactHealthDetail(err.Error())
		if response != nil {
			observation.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), observation.CheckedAt)
		}
		return observation
	}
	series := document.HeartbeatList[strconv.FormatInt(matches[0].ID, 10)]
	if len(series) == 0 {
		observation.Detail = fmt.Sprintf("monitor %q has no heartbeat", matches[0].Name)
		return observation
	}
	latest := series[len(series)-1]
	observedAt, err := parseUptimeKumaTime(latest.Time)
	if err != nil {
		observation.Detail = fmt.Sprintf("monitor %q returned an invalid heartbeat time", matches[0].Name)
		return observation
	}
	observation.ObservedAt = observedAt
	if detail := healthFreshnessDetail(observation.CheckedAt, observedAt, maxAge); detail != "" {
		observation.Detail = fmt.Sprintf("monitor %q: %s", matches[0].Name, detail)
		return observation
	}
	if latest.Status == nil {
		observation.Detail = fmt.Sprintf("monitor %q omitted heartbeat status", matches[0].Name)
		return observation
	}
	switch *latest.Status {
	case 1:
		observation.State = healthStateHealthy
		observation.Detail = fmt.Sprintf("monitor %q is up", matches[0].Name)
	case 0:
		observation.State = healthStateUnhealthy
		observation.Detail = fmt.Sprintf("monitor %q is down", matches[0].Name)
		if latest.Msg != "" {
			observation.Detail += ": " + compactHealthDetail(latest.Msg)
		}
	default:
		observation.Detail = fmt.Sprintf("monitor %q has status %d", matches[0].Name, *latest.Status)
	}
	return observation
}

func (s *uptimeKumaHealthSource) monitors(ctx context.Context, metadataURL string, now time.Time) ([]uptimeKumaMonitor, time.Duration, error) {
	s.mu.Lock()
	if now.Before(s.until) && len(s.items) > 0 {
		items := append([]uptimeKumaMonitor(nil), s.items...)
		s.mu.Unlock()
		return items, 0, nil
	}
	s.mu.Unlock()
	var document struct {
		PublicGroupList []struct {
			MonitorList []uptimeKumaMonitor `json:"monitorList"`
		} `json:"publicGroupList"`
	}
	response, err := fetchHealthJSON(ctx, metadataURL, &document)
	if err != nil {
		var retryAfter time.Duration
		if response != nil {
			retryAfter = parseRetryAfter(response.Header.Get("Retry-After"), now)
		}
		return nil, retryAfter, err
	}
	items := make([]uptimeKumaMonitor, 0)
	for _, group := range document.PublicGroupList {
		items = append(items, group.MonitorList...)
	}
	if len(items) == 0 {
		return nil, 0, errors.New("Uptime Kuma status page has no public monitors")
	}
	s.mu.Lock()
	s.items = append([]uptimeKumaMonitor(nil), items...)
	s.until = now.Add(uptimeMetadataTTL)
	s.mu.Unlock()
	return items, 0, nil
}

type inputIMHealthSource struct{ config healthSourceConfig }

func (s inputIMHealthSource) configuration() healthSourceConfig { return s.config }

func (s inputIMHealthSource) check(ctx context.Context, model string, maxAge time.Duration) healthObservation {
	observation := newHealthObservation(s.config, model)
	if model == "" {
		observation.Detail = "target model is unavailable"
		return observation
	}
	apiURL, err := inputIMStatusEndpoint(s.config.URL)
	if err != nil {
		observation.Detail = compactHealthDetail(err.Error())
		return observation
	}
	var document struct {
		GeneratedAt *int64 `json:"generated_at"`
		Services    []struct {
			Model string `json:"model"`
			Last  *struct {
				Timestamp *int64 `json:"ts"`
				OK        *bool  `json:"ok"`
				Error     string `json:"error"`
				LatencyMS *int64 `json:"latency_ms"`
			} `json:"last"`
		} `json:"services"`
	}
	response, err := fetchHealthJSON(ctx, apiURL, &document)
	if err != nil {
		observation.Detail = compactHealthDetail(err.Error())
		if response != nil {
			observation.RetryAfter = parseRetryAfter(response.Header.Get("Retry-After"), observation.CheckedAt)
		}
		return observation
	}
	if document.GeneratedAt == nil {
		observation.Detail = "status response omitted generated_at"
		return observation
	}
	generatedAt := time.Unix(*document.GeneratedAt, 0).UTC()
	if detail := healthFreshnessDetail(observation.CheckedAt, generatedAt, maxAge); detail != "" {
		observation.Detail = "status document " + detail
		return observation
	}
	matches := make([]int, 0, 1)
	for index, service := range document.Services {
		if strings.EqualFold(strings.TrimSpace(service.Model), strings.TrimSpace(model)) {
			matches = append(matches, index)
		}
	}
	if len(matches) == 0 {
		observation.Detail = fmt.Sprintf("model %q was not found", model)
		return observation
	}
	if len(matches) > 1 {
		observation.Detail = fmt.Sprintf("model %q matched multiple services", model)
		return observation
	}
	service := document.Services[matches[0]]
	if service.Last == nil || service.Last.Timestamp == nil || service.Last.OK == nil {
		observation.Detail = fmt.Sprintf("model %q has no complete status sample", model)
		return observation
	}
	observation.ObservedAt = time.Unix(*service.Last.Timestamp, 0).UTC()
	if detail := healthFreshnessDetail(observation.CheckedAt, observation.ObservedAt, maxAge); detail != "" {
		observation.Detail = fmt.Sprintf("model %q sample %s", model, detail)
		return observation
	}
	if *service.Last.OK {
		observation.State = healthStateHealthy
		observation.Detail = fmt.Sprintf("model %q is online", model)
	} else {
		observation.State = healthStateUnhealthy
		observation.Detail = fmt.Sprintf("model %q is failing", model)
		if service.Last.Error != "" {
			observation.Detail += ": " + compactHealthDetail(service.Last.Error)
		}
	}
	return observation
}

func newHealthObservation(config healthSourceConfig, model string) healthObservation {
	return healthObservation{
		Source:    healthSourceDisplayName(config),
		Type:      config.Type,
		Model:     model,
		State:     healthStateUnknown,
		CheckedAt: time.Now().UTC(),
	}
}

func healthSourceDisplayName(config healthSourceConfig) string {
	if config.Name != "" {
		return config.Name
	}
	if parsed, err := url.Parse(config.URL); err == nil && parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	if config.Type != "" {
		return config.Type
	}
	return "health source"
}

func fetchHealthResponse(ctx context.Context, rawURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json, text/plain;q=0.8, */*;q=0.1")
	request.Header.Set("User-Agent", "codexdog/"+version+" status-check")
	client := &http.Client{CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("health endpoint exceeded 3 redirects")
		}
		return nil
	}}
	return client.Do(request)
}

func fetchHealthJSON(ctx context.Context, rawURL string, destination any) (*http.Response, error) {
	response, err := fetchHealthResponse(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return response, fmt.Errorf("status endpoint returned HTTP %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json") {
		return response, fmt.Errorf("status endpoint returned non-JSON content type %q", response.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, healthResponseMaxSize+1))
	if err != nil {
		return response, err
	}
	if len(data) > healthResponseMaxSize {
		return response, fmt.Errorf("status response exceeds %d bytes", healthResponseMaxSize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return response, fmt.Errorf("decode status response: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return response, fmt.Errorf("decode status response: %w", err)
	}
	return response, nil
}

func uptimeKumaEndpoints(rawURL string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return "", "", fmt.Errorf("invalid Uptime Kuma URL %q", rawURL)
	}
	cleaned := strings.TrimSuffix(parsed.EscapedPath(), "/")
	lower := strings.ToLower(cleaned)
	marker := "/status/"
	index := strings.LastIndex(lower, marker)
	var prefix, slug string
	if index >= 0 {
		prefix = cleaned[:index]
		slug = cleaned[index+len(marker):]
	} else {
		marker = "/api/status-page/heartbeat/"
		index = strings.LastIndex(lower, marker)
		if index >= 0 {
			prefix = cleaned[:index]
			slug = cleaned[index+len(marker):]
		} else {
			marker = "/api/status-page/"
			index = strings.LastIndex(lower, marker)
			if index >= 0 {
				prefix = cleaned[:index]
				slug = cleaned[index+len(marker):]
			}
		}
	}
	if slug == "" || strings.Contains(slug, "/") {
		return "", "", fmt.Errorf("Uptime Kuma URL %q must identify one /status/SLUG page", rawURL)
	}
	metadata := *parsed
	heartbeats := *parsed
	metadata.RawQuery, metadata.Fragment = "", ""
	heartbeats.RawQuery, heartbeats.Fragment = "", ""
	metadata.Path = path.Join(prefix, "/api/status-page", slug)
	heartbeats.Path = path.Join(prefix, "/api/status-page/heartbeat", slug)
	metadata.RawPath, heartbeats.RawPath = "", ""
	return metadata.String(), heartbeats.String(), nil
}

func inputIMStatusEndpoint(rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("invalid Input.im status URL %q", rawURL)
	}
	if strings.TrimSuffix(parsed.Path, "/") != "/api/status" {
		parsed.Path = "/api/status"
	}
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return parsed.String(), nil
}

func parseUptimeKumaTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02 15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, strings.TrimSpace(value), time.UTC); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Uptime Kuma timestamp %q", value)
}

func healthModelNameMatches(name, model string) bool {
	name = strings.ToLower(strings.Join(strings.Fields(name), " "))
	model = strings.ToLower(strings.TrimSpace(model))
	return name == model || strings.HasSuffix(name, " "+model)
}

func healthFreshnessDetail(now, observed time.Time, maxAge time.Duration) string {
	if observed.IsZero() {
		return "has no observation time"
	}
	if observed.After(now.Add(time.Minute)) {
		return "is more than 1m in the future"
	}
	if now.Sub(observed) > maxAge {
		return fmt.Sprintf("is stale (age %s, maximum %s)", now.Sub(observed).Round(time.Second), maxAge)
	}
	return ""
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds > 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		return retryAt.Sub(now)
	}
	return 0
}

func compactHealthDetail(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	const limit = 240
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func healthObservationStates(observations []healthObservation) []healthObservationState {
	if len(observations) == 0 {
		return nil
	}
	states := make([]healthObservationState, 0, len(observations))
	for _, observation := range observations {
		states = append(states, healthObservationState{
			Source:     observation.Source,
			Type:       observation.Type,
			Model:      observation.Model,
			State:      observation.State,
			CheckedAt:  atomicTime(observation.CheckedAt),
			ObservedAt: atomicTime(observation.ObservedAt),
			Detail:     sanitizeText(compactHealthDetail(observation.Detail)),
		})
	}
	return states
}

func formatHealthObservation(observation healthObservation) string {
	message := fmt.Sprintf("Health source %s (%s", observation.Source, observation.Type)
	if observation.Model != "" {
		message += ", model " + observation.Model
	}
	message += "): " + observation.State
	if observation.Detail != "" {
		message += " - " + observation.Detail
	}
	if !observation.ObservedAt.IsZero() {
		message += " (observed " + atomicTime(observation.ObservedAt) + ")"
	}
	return sanitizeText(compactHealthDetail(message))
}

func formatHealthTarget(model, provider string) string {
	model = strings.TrimSpace(model)
	provider = strings.TrimSpace(provider)
	if model == "" {
		return provider
	}
	if provider == "" {
		return model
	}
	return model + " via " + provider
}

func formatHealthObservationState(observation healthObservationState) string {
	message := fmt.Sprintf("Health source %s (%s", observation.Source, observation.Type)
	if observation.Model != "" {
		message += ", model " + observation.Model
	}
	message += "): " + observation.State
	if observation.Detail != "" {
		message += " - " + observation.Detail
	}
	if observation.ObservedAt != "" {
		message += " (observed " + observation.ObservedAt + ")"
	} else if observation.CheckedAt != "" {
		message += " (checked " + observation.CheckedAt + ")"
	}
	return sanitizeText(compactHealthDetail(message))
}
