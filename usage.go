package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type tokenUsageBreakdownState struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type threadTokenUsageState struct {
	ThreadID           string                   `json:"threadId"`
	TurnID             string                   `json:"turnId,omitempty"`
	Last               tokenUsageBreakdownState `json:"last"`
	Total              tokenUsageBreakdownState `json:"total"`
	ModelContextWindow int64                    `json:"modelContextWindow,omitempty"`
}

type accountUsageSummaryState struct {
	LifetimeTokens        *int64 `json:"lifetimeTokens,omitempty"`
	CurrentStreakDays     *int64 `json:"currentStreakDays,omitempty"`
	LongestStreakDays     *int64 `json:"longestStreakDays,omitempty"`
	PeakDailyTokens       *int64 `json:"peakDailyTokens,omitempty"`
	LongestRunningTurnSec *int64 `json:"longestRunningTurnSec,omitempty"`
}

type threadUsageGroupState struct {
	Model                       string `json:"model,omitempty"`
	ReasoningEffort             string `json:"reasoningEffort,omitempty"`
	Speed                       string `json:"speed,omitempty"`
	InputTokens                 *int64 `json:"inputTokens,omitempty"`
	CachedInputTokens           *int64 `json:"cachedInputTokens,omitempty"`
	NetNewInputTokens           *int64 `json:"netNewInputTokens,omitempty"`
	OutputTokens                *int64 `json:"outputTokens,omitempty"`
	TotalTokens                 *int64 `json:"totalTokens,omitempty"`
	EstimatedUsageCreditsMicros int64  `json:"estimatedUsageCreditsMicros"`
}

type threadUsageEstimateState struct {
	ThreadID                    string                  `json:"threadId"`
	EstimatedUsageCreditsMicros int64                   `json:"estimatedUsageCreditsMicros"`
	EstimatedUsageUSDMicros     *int64                  `json:"estimatedUsageUsdMicros,omitempty"`
	Groups                      []threadUsageGroupState `json:"groups,omitempty"`
}

type rateLimitWindowState struct {
	UsedPercent        int    `json:"usedPercent"`
	ResetsAt           *int64 `json:"resetsAt,omitempty"`
	WindowDurationMins *int64 `json:"windowDurationMins,omitempty"`
}

type creditsState struct {
	Balance    string `json:"balance,omitempty"`
	HasCredits bool   `json:"hasCredits"`
	Unlimited  bool   `json:"unlimited"`
}

type spendControlLimitState struct {
	Limit            string `json:"limit"`
	Used             string `json:"used"`
	RemainingPercent int    `json:"remainingPercent"`
	ResetsAt         int64  `json:"resetsAt"`
}

type rateLimitState struct {
	LimitID              string                  `json:"limitId,omitempty"`
	LimitName            string                  `json:"limitName,omitempty"`
	PlanType             string                  `json:"planType,omitempty"`
	Primary              *rateLimitWindowState   `json:"primary,omitempty"`
	Secondary            *rateLimitWindowState   `json:"secondary,omitempty"`
	Credits              *creditsState           `json:"credits,omitempty"`
	IndividualLimit      *spendControlLimitState `json:"individualLimit,omitempty"`
	RateLimitReachedType string                  `json:"rateLimitReachedType,omitempty"`
	SpendControlReached  *bool                   `json:"spendControlReached,omitempty"`
}

func readInt64(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case float64:
		if value != math.Trunc(value) || value < float64(math.MinInt64) || value >= float64(math.MaxInt64) {
			return 0, false
		}
		return int64(value), true
	case json.Number:
		result, err := value.Int64()
		return result, err == nil
	default:
		return 0, false
	}
}

func int64Pointer(value any) *int64 {
	parsed, ok := readInt64(value)
	if !ok {
		return nil
	}
	return &parsed
}

func boolPointer(value any) *bool {
	parsed, ok := readBool(value)
	if !ok {
		return nil
	}
	return &parsed
}

func parseTokenUsageNotification(params map[string]any) (threadTokenUsageState, bool) {
	threadID, threadOK := readString(params["threadId"])
	tokenUsage, usageOK := asObject(params["tokenUsage"])
	if !threadOK || threadID == "" || !usageOK {
		return threadTokenUsageState{}, false
	}
	last, lastOK := parseTokenUsageBreakdown(tokenUsage["last"])
	total, totalOK := parseTokenUsageBreakdown(tokenUsage["total"])
	if !lastOK || !totalOK {
		return threadTokenUsageState{}, false
	}
	result := threadTokenUsageState{ThreadID: threadID, Last: last, Total: total}
	result.TurnID, _ = readString(params["turnId"])
	result.ModelContextWindow, _ = readInt64(tokenUsage["modelContextWindow"])
	return result, true
}

func parseTokenUsageBreakdown(value any) (tokenUsageBreakdownState, bool) {
	object, ok := asObject(value)
	if !ok {
		return tokenUsageBreakdownState{}, false
	}
	total, ok := readInt64(object["totalTokens"])
	if !ok {
		return tokenUsageBreakdownState{}, false
	}
	result := tokenUsageBreakdownState{TotalTokens: total}
	result.InputTokens, _ = readInt64(object["inputTokens"])
	result.CachedInputTokens, _ = readInt64(object["cachedInputTokens"])
	result.CacheWriteInputTokens, _ = readInt64(object["cacheWriteInputTokens"])
	result.OutputTokens, _ = readInt64(object["outputTokens"])
	result.ReasoningOutputTokens, _ = readInt64(object["reasoningOutputTokens"])
	return result, true
}

func parseAccountUsageResponse(value any) (*accountUsageSummaryState, *threadUsageEstimateState, bool) {
	object, ok := asObject(value)
	if !ok {
		return nil, nil, false
	}
	summaryObject, ok := asObject(object["summary"])
	if !ok {
		return nil, nil, false
	}
	summary := &accountUsageSummaryState{
		LifetimeTokens:        int64Pointer(summaryObject["lifetimeTokens"]),
		CurrentStreakDays:     int64Pointer(summaryObject["currentStreakDays"]),
		LongestStreakDays:     int64Pointer(summaryObject["longestStreakDays"]),
		PeakDailyTokens:       int64Pointer(summaryObject["peakDailyTokens"]),
		LongestRunningTurnSec: int64Pointer(summaryObject["longestRunningTurnSec"]),
	}
	threadObject, hasThreadUsage := asObject(object["threadUsage"])
	if !hasThreadUsage {
		return summary, nil, true
	}
	threadID, idOK := readString(threadObject["threadId"])
	credits, creditsOK := readInt64(threadObject["estimatedUsageCreditsMicros"])
	if !idOK || threadID == "" || !creditsOK {
		return nil, nil, false
	}
	estimate := &threadUsageEstimateState{
		ThreadID:                    threadID,
		EstimatedUsageCreditsMicros: credits,
		EstimatedUsageUSDMicros:     int64Pointer(threadObject["estimatedUsageUsdMicros"]),
	}
	groups, _ := threadObject["groups"].([]any)
	for _, value := range groups {
		groupObject, ok := asObject(value)
		if !ok {
			continue
		}
		groupCredits, ok := readInt64(groupObject["estimatedUsageCreditsMicros"])
		if !ok {
			continue
		}
		group := threadUsageGroupState{EstimatedUsageCreditsMicros: groupCredits}
		group.Model, _ = readString(groupObject["model"])
		group.ReasoningEffort, _ = readString(groupObject["reasoningEffort"])
		group.Speed, _ = readString(groupObject["speed"])
		group.InputTokens = int64Pointer(groupObject["inputTokens"])
		group.CachedInputTokens = int64Pointer(groupObject["cachedInputTokens"])
		group.NetNewInputTokens = int64Pointer(groupObject["netNewInputTokens"])
		group.OutputTokens = int64Pointer(groupObject["outputTokens"])
		group.TotalTokens = int64Pointer(groupObject["totalTokens"])
		estimate.Groups = append(estimate.Groups, group)
	}
	return summary, estimate, true
}

func parseRateLimitsResponse(value any) ([]rateLimitState, *int64, bool) {
	object, ok := asObject(value)
	if !ok {
		return nil, nil, false
	}
	resetCredits := (*int64)(nil)
	if resetObject, ok := asObject(object["rateLimitResetCredits"]); ok {
		resetCredits = int64Pointer(resetObject["availableCount"])
	}
	rates := []rateLimitState{}
	if byID, ok := asObject(object["rateLimitsByLimitId"]); ok && len(byID) > 0 {
		for id, value := range byID {
			if rate, ok := parseRateLimitSnapshot(value, id); ok {
				rates = append(rates, rate)
			}
		}
	} else if rate, ok := parseRateLimitSnapshot(object["rateLimits"], ""); ok {
		rates = append(rates, rate)
	} else if _, exists := object["rateLimits"]; !exists {
		return nil, nil, false
	}
	sort.Slice(rates, func(i, j int) bool {
		left := rates[i].LimitID + "\x00" + rates[i].LimitName
		right := rates[j].LimitID + "\x00" + rates[j].LimitName
		return left < right
	})
	return rates, resetCredits, true
}

func parseRateLimitSnapshot(value any, fallbackID string) (rateLimitState, bool) {
	object, ok := asObject(value)
	if !ok {
		return rateLimitState{}, false
	}
	result := rateLimitState{LimitID: fallbackID}
	if value, ok := readString(object["limitId"]); ok {
		result.LimitID = value
	}
	result.LimitName, _ = readString(object["limitName"])
	result.PlanType, _ = readString(object["planType"])
	result.RateLimitReachedType, _ = readString(object["rateLimitReachedType"])
	result.SpendControlReached = boolPointer(object["spendControlReached"])
	result.Primary = parseRateLimitWindow(object["primary"])
	result.Secondary = parseRateLimitWindow(object["secondary"])
	if credits, ok := asObject(object["credits"]); ok {
		parsed := &creditsState{}
		parsed.Balance, _ = readString(credits["balance"])
		parsed.HasCredits, _ = readBool(credits["hasCredits"])
		parsed.Unlimited, _ = readBool(credits["unlimited"])
		result.Credits = parsed
	}
	if limit, ok := asObject(object["individualLimit"]); ok {
		parsed := &spendControlLimitState{}
		parsed.Limit, _ = readString(limit["limit"])
		parsed.Used, _ = readString(limit["used"])
		remaining, remainingOK := readInt64(limit["remainingPercent"])
		resetsAt, resetsOK := readInt64(limit["resetsAt"])
		if remainingOK && resetsOK {
			parsed.RemainingPercent = int(remaining)
			parsed.ResetsAt = resetsAt
			result.IndividualLimit = parsed
		}
	}
	return result, true
}

func parseRateLimitWindow(value any) *rateLimitWindowState {
	object, ok := asObject(value)
	if !ok {
		return nil
	}
	used, ok := readInt64(object["usedPercent"])
	if !ok {
		return nil
	}
	return &rateLimitWindowState{
		UsedPercent:        int(used),
		ResetsAt:           int64Pointer(object["resetsAt"]),
		WindowDurationMins: int64Pointer(object["windowDurationMins"]),
	}
}

func (s *supervisor) handleTokenUsageUpdated(params map[string]any) {
	usage, ok := parseTokenUsageNotification(params)
	if !ok {
		return
	}
	s.mu.Lock()
	currentThreadID := s.state.CurrentThreadID
	if currentThreadID != "" && usage.ThreadID != currentThreadID {
		s.mu.Unlock()
		return
	}
	setCurrentThread(&s.state, usage.ThreadID)
	s.state.TokenUsage = &usage
	s.state.UsageUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.mu.Unlock()
	_ = s.persist()
	s.scheduleUsageRefresh(250 * time.Millisecond)
}

func setCurrentThread(state *supervisorState, threadID string) {
	if threadID == "" {
		return
	}
	if state.CurrentThreadID != threadID {
		state.TokenUsage = nil
		state.UsageEstimate = nil
	}
	state.CurrentThreadID = threadID
}

func (s *supervisor) scheduleUsageRefresh(delay time.Duration) {
	s.mu.Lock()
	if s.shuttingDown || s.usageRPC == nil || s.usageRefreshTimer != nil {
		s.mu.Unlock()
		return
	}
	s.usageRefreshTimer = time.AfterFunc(max(time.Duration(0), delay), func() {
		s.mu.Lock()
		s.usageRefreshTimer = nil
		shuttingDown := s.shuttingDown
		s.mu.Unlock()
		if shuttingDown {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.refreshUsageSnapshot(ctx)
	})
	s.mu.Unlock()
}

func (s *supervisor) refreshUsageSnapshot(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.shuttingDown || s.usageRPC == nil || s.usageRefreshInFlight {
		s.mu.Unlock()
		return
	}
	s.usageRefreshInFlight = true
	requester := s.usageRPC
	threadID := s.state.CurrentThreadID
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.usageRefreshInFlight = false
		s.mu.Unlock()
	}()

	usageParams := map[string]any{}
	if threadID != "" {
		usageParams["threadId"] = threadID
	}
	var usageValue, rateValue any
	var usageErr, rateErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		usageValue, usageErr = requester.Request(ctx, "account/usage/read", usageParams)
	}()
	go func() {
		defer wait.Done()
		rateValue, rateErr = requester.Request(ctx, "account/rateLimits/read", map[string]any{})
	}()
	wait.Wait()

	var summary *accountUsageSummaryState
	var estimate *threadUsageEstimateState
	usageOK := false
	if usageErr == nil {
		summary, estimate, usageOK = parseAccountUsageResponse(usageValue)
		if !usageOK {
			usageErr = fmt.Errorf("invalid account/usage/read response")
		}
	}
	var rates []rateLimitState
	var resetCredits *int64
	rateOK := false
	if rateErr == nil {
		rates, resetCredits, rateOK = parseRateLimitsResponse(rateValue)
		if !rateOK {
			rateErr = fmt.Errorf("invalid account/rateLimits/read response")
		}
	}

	errors := []string{}
	if usageErr != nil {
		errors = append(errors, "usage: "+sanitizeText(usageErr.Error()))
	}
	if rateErr != nil {
		errors = append(errors, "rate limits: "+sanitizeText(rateErr.Error()))
	}
	s.mu.Lock()
	if usageOK {
		s.state.AccountUsage = summary
		if estimate == nil || estimate.ThreadID == s.state.CurrentThreadID {
			s.state.UsageEstimate = estimate
		}
	}
	if rateOK {
		s.state.RateLimits = rates
		s.state.RateLimitResetCreditsAvailable = resetCredits
	}
	if usageOK || rateOK {
		s.state.UsageUpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	s.state.UsageLastError = strings.Join(errors, "; ")
	shuttingDown := s.shuttingDown
	s.mu.Unlock()
	if !shuttingDown {
		_ = s.persist()
	}
}

func usageStatusLines(state supervisorState) []string {
	lines := []string{}
	if state.TokenUsage != nil {
		total := state.TokenUsage.Total
		lines = append(lines, fmt.Sprintf(
			"Tokens: %s total (input %s, cached %s, cache write %s, output %s, reasoning %s)",
			formatInteger(total.TotalTokens), formatInteger(total.InputTokens), formatInteger(total.CachedInputTokens),
			formatInteger(total.CacheWriteInputTokens), formatInteger(total.OutputTokens), formatInteger(total.ReasoningOutputTokens),
		))
		if state.TokenUsage.ModelContextWindow > 0 {
			lines = append(lines, "Model context window: "+formatInteger(state.TokenUsage.ModelContextWindow)+" tokens")
		}
	}
	if state.UsageEstimate != nil {
		line := "Estimated usage: " + formatMicros(state.UsageEstimate.EstimatedUsageCreditsMicros) + " credits"
		if state.UsageEstimate.EstimatedUsageUSDMicros != nil {
			line += " ($" + formatMicros(*state.UsageEstimate.EstimatedUsageUSDMicros) + ")"
		}
		lines = append(lines, line)
	}
	if state.AccountUsage != nil && state.AccountUsage.LifetimeTokens != nil {
		lines = append(lines, "Account lifetime tokens: "+formatInteger(*state.AccountUsage.LifetimeTokens))
	}
	if credits := firstCredits(state.RateLimits); credits != nil {
		switch {
		case credits.Unlimited:
			lines = append(lines, "Credit balance: unlimited")
		case credits.Balance != "":
			lines = append(lines, "Credit balance: "+credits.Balance)
		case credits.HasCredits:
			lines = append(lines, "Credits: available")
		default:
			lines = append(lines, "Credits: none")
		}
	}
	for _, rate := range state.RateLimits {
		lines = append(lines, formatRateLimit(rate))
	}
	if state.RateLimitResetCreditsAvailable != nil {
		lines = append(lines, fmt.Sprintf("Rate-limit reset credits: %d available", *state.RateLimitResetCreditsAvailable))
	}
	if state.UsageUpdatedAt != "" {
		lines = append(lines, "Usage updated: "+state.UsageUpdatedAt)
	}
	if state.UsageLastError != "" {
		lines = append(lines, "Usage data error: "+state.UsageLastError)
	}
	return lines
}

func firstCredits(rates []rateLimitState) *creditsState {
	for _, rate := range rates {
		if rate.Credits != nil {
			return rate.Credits
		}
	}
	return nil
}

func formatRateLimit(rate rateLimitState) string {
	name := rate.LimitName
	if name == "" {
		name = rate.LimitID
	}
	if name == "" {
		name = "default"
	}
	if rate.PlanType != "" {
		name += " (" + rate.PlanType + ")"
	}
	parts := []string{}
	if rate.Primary != nil {
		parts = append(parts, formatRateLimitWindow("primary", rate.Primary))
	}
	if rate.Secondary != nil {
		parts = append(parts, formatRateLimitWindow("secondary", rate.Secondary))
	}
	if rate.IndividualLimit != nil {
		parts = append(parts, fmt.Sprintf("spend %s of %s, %d%% remaining", valueOrDash(rate.IndividualLimit.Used), valueOrDash(rate.IndividualLimit.Limit), rate.IndividualLimit.RemainingPercent))
	}
	if rate.RateLimitReachedType != "" {
		parts = append(parts, "reached: "+rate.RateLimitReachedType)
	}
	if rate.SpendControlReached != nil && *rate.SpendControlReached {
		parts = append(parts, "spend control reached")
	}
	if len(parts) == 0 {
		parts = append(parts, "available")
	}
	return "Rate limit " + name + ": " + strings.Join(parts, "; ")
}

func formatRateLimitWindow(label string, window *rateLimitWindowState) string {
	result := fmt.Sprintf("%s %d%% used", label, window.UsedPercent)
	details := []string{}
	if window.WindowDurationMins != nil {
		details = append(details, formatWindowMinutes(*window.WindowDurationMins))
	}
	if window.ResetsAt != nil {
		details = append(details, "resets "+time.Unix(*window.ResetsAt, 0).UTC().Format(time.RFC3339))
	}
	if len(details) > 0 {
		result += " (" + strings.Join(details, ", ") + ")"
	}
	return result
}

func formatWindowMinutes(minutes int64) string {
	if minutes > 0 && minutes%1440 == 0 {
		return fmt.Sprintf("%dd window", minutes/1440)
	}
	if minutes > 0 && minutes%60 == 0 {
		return fmt.Sprintf("%dh window", minutes/60)
	}
	return fmt.Sprintf("%dm window", minutes)
}

func formatMicros(value int64) string {
	result := strconv.FormatFloat(float64(value)/1_000_000, 'f', 6, 64)
	result = strings.TrimRight(strings.TrimRight(result, "0"), ".")
	if result == "" || result == "-0" {
		return "0"
	}
	return result
}

func formatInteger(value int64) string {
	digits := strconv.FormatInt(value, 10)
	start := 0
	if strings.HasPrefix(digits, "-") {
		start = 1
	}
	for index := len(digits) - 3; index > start; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}
