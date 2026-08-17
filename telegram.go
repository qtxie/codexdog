package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	telegramDefaultAPIBaseURL   = "https://api.telegram.org"
	telegramDefaultPollTimeout  = 30 * time.Second
	telegramDefaultRequestLimit = 64 * 1024 * 1024
	telegramDefaultMaxAttempts  = 3
	telegramDefaultSeenLimit    = 1024
	telegramMessageLimit        = 4096
)

var telegramDefaultBackoff = []time.Duration{250 * time.Millisecond, time.Second, 3 * time.Second}

// telegramClientOptions controls the Bot API transport. The token is kept only
// in memory and is never included in returned errors.
type telegramClientOptions struct {
	Token          string
	BaseURL        string
	HTTPClient     *http.Client
	MaxAttempts    int
	Backoff        []time.Duration
	MaxRetryAfter  time.Duration
	RequestTimeout time.Duration
}

// telegramClient is a small Bot API client. It deliberately uses plain
// net/http so the binary does not acquire another runtime dependency.
type telegramClient struct {
	token          string
	baseURL        string
	httpClient     *http.Client
	maxAttempts    int
	backoff        []time.Duration
	maxRetryAfter  time.Duration
	requestTimeout time.Duration
}

type telegramAPIResponse struct {
	OK          bool                        `json:"ok"`
	Result      json.RawMessage             `json:"result"`
	ErrorCode   int                         `json:"error_code"`
	Description string                      `json:"description"`
	Parameters  *telegramResponseParameters `json:"parameters,omitempty"`
}

type telegramResponseParameters struct {
	RetryAfter    int   `json:"retry_after,omitempty"`
	MigrateToChat int64 `json:"migrate_to_chat_id,omitempty"`
}

// telegramAPIError contains only safe, non-secret request diagnostics.
type telegramAPIError struct {
	Method      string
	StatusCode  int
	ErrorCode   int
	Description string
	RetryAfter  time.Duration
	Retryable   bool
	Cause       error
}

func (e *telegramAPIError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := "Telegram API " + e.Method + " failed"
	details := []string{}
	if e.StatusCode != 0 {
		details = append(details, "HTTP "+strconv.Itoa(e.StatusCode))
	}
	if e.ErrorCode != 0 {
		details = append(details, "code "+strconv.Itoa(e.ErrorCode))
	}
	if len(details) > 0 {
		message += " (" + strings.Join(details, ", ") + ")"
	}
	if e.Description != "" {
		message += ": " + e.Description
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *telegramAPIError) Unwrap() error { return e.Cause }

func newTelegramClient(options telegramClientOptions) (*telegramClient, error) {
	token := strings.TrimSpace(options.Token)
	if token == "" {
		return nil, errors.New("telegram bot token is required")
	}
	if strings.ContainsAny(token, "/?# \t\r\n") {
		return nil, errors.New("telegram bot token contains invalid characters")
	}

	baseURL := strings.TrimSpace(options.BaseURL)
	if baseURL == "" {
		baseURL = telegramDefaultAPIBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("telegram API base URL must be an http(s) URL without query or fragment")
	}
	baseURL = strings.TrimRight(baseURL, "/")

	maxAttempts := options.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = telegramDefaultMaxAttempts
	}
	if maxAttempts < 1 {
		return nil, errors.New("telegram max attempts must be positive")
	}
	backoff := append([]time.Duration(nil), options.Backoff...)
	if len(backoff) == 0 {
		backoff = append([]time.Duration(nil), telegramDefaultBackoff...)
	}
	for _, delay := range backoff {
		if delay < 0 {
			return nil, errors.New("telegram retry backoff cannot be negative")
		}
	}
	maxRetryAfter := options.MaxRetryAfter
	if maxRetryAfter <= 0 {
		maxRetryAfter = 5 * time.Minute
	}
	requestTimeout := options.RequestTimeout
	if requestTimeout <= 0 {
		requestTimeout = telegramDefaultPollTimeout + 15*time.Second
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	return &telegramClient{
		token:          token,
		baseURL:        baseURL,
		httpClient:     httpClient,
		maxAttempts:    maxAttempts,
		backoff:        backoff,
		maxRetryAfter:  maxRetryAfter,
		requestTimeout: requestTimeout,
	}, nil
}

func (c *telegramClient) endpoint(method string) string {
	return c.baseURL + "/bot" + c.token + "/" + method
}

type telegramGetUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message,omitempty"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from,omitempty"`
	Chat      telegramChat  `json:"chat"`
	Date      int64         `json:"date,omitempty"`
	Text      string        `json:"text,omitempty"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

type telegramChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type,omitempty"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

type telegramBot struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

type telegramBotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func (c *telegramClient) GetMe(ctx context.Context) (telegramBot, error) {
	var bot telegramBot
	if err := c.do(ctx, "getMe", map[string]any{}, &bot); err != nil {
		return telegramBot{}, err
	}
	return bot, nil
}

func (c *telegramClient) SetMyCommands(ctx context.Context, commands []telegramBotCommand) error {
	return c.do(ctx, "setMyCommands", map[string]any{"commands": commands}, nil)
}

func (c *telegramClient) DeleteWebhook(ctx context.Context) error {
	return c.do(ctx, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil)
}

func (c *telegramClient) GetUpdates(ctx context.Context, offset int64, pollTimeout time.Duration) ([]telegramUpdate, error) {
	if offset < 0 {
		return nil, errors.New("telegram update offset cannot be negative")
	}
	if pollTimeout < 0 || pollTimeout > 50*time.Second {
		return nil, errors.New("telegram poll timeout must be between 0 and 50 seconds")
	}
	seconds := int(pollTimeout / time.Second)
	if pollTimeout > 0 && seconds == 0 {
		seconds = 1
	}
	payload := telegramGetUpdatesRequest{
		Offset:         offset,
		Timeout:        seconds,
		AllowedUpdates: []string{"message"},
	}
	var updates []telegramUpdate
	if err := c.do(ctx, "getUpdates", payload, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

type telegramSendMessageRequest struct {
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

func (c *telegramClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("telegram message cannot be empty")
	}
	for _, part := range splitTelegramMessage(text, telegramMessageLimit) {
		if err := c.do(ctx, "sendMessage", telegramSendMessageRequest{ChatID: chatID, Text: part}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *telegramClient) do(ctx context.Context, method string, payload any, result any) error {
	for attempt := 1; attempt <= c.maxAttempts; attempt++ {
		err := c.doOnce(ctx, method, payload, result)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var apiErr *telegramAPIError
		if !errors.As(err, &apiErr) || !apiErr.Retryable || attempt >= c.maxAttempts {
			return err
		}
		delay := apiErr.RetryAfter
		if delay <= 0 {
			index := attempt - 1
			if index >= len(c.backoff) {
				index = len(c.backoff) - 1
			}
			delay = c.backoff[index]
		}
		if c.maxRetryAfter > 0 && delay > c.maxRetryAfter {
			delay = c.maxRetryAfter
		}
		if !waitTelegramContext(ctx, delay) {
			return ctx.Err()
		}
	}
	return errors.New("telegram request retry loop exhausted")
}

func (c *telegramClient) doOnce(ctx context.Context, method string, payload any, result any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return &telegramAPIError{Method: method, Description: "encode request: " + sanitizeText(err.Error())}
	}
	requestCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && c.requestTimeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, c.requestTimeout)
	}
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpoint(method), bytes.NewReader(data))
	if err != nil {
		return &telegramAPIError{Method: method, Description: sanitizeText(err.Error()), Retryable: false}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "codexdog/"+version)
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.Canceled) && requestCtx.Err() == nil {
			return &telegramAPIError{Method: method, Description: "request canceled", Retryable: true, Cause: context.Canceled}
		}
		if requestCtx.Err() != nil {
			return &telegramAPIError{Method: method, Description: "request timed out", Retryable: true, Cause: requestCtx.Err()}
		}
		return &telegramAPIError{Method: method, Description: "request failed", Retryable: true, Cause: errors.New(redactTelegramToken(err.Error(), c.token))}
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, telegramDefaultRequestLimit))
	if readErr != nil {
		return &telegramAPIError{Method: method, StatusCode: response.StatusCode, Description: "read response failed", Retryable: response.StatusCode == 0 || response.StatusCode >= 500, Cause: errors.New(redactTelegramToken(readErr.Error(), c.token))}
	}
	var envelope telegramAPIResponse
	if err := json.Unmarshal(body, &envelope); err != nil {
		retryable := response.StatusCode == 0 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 || response.StatusCode >= 200 && response.StatusCode < 300
		return &telegramAPIError{Method: method, StatusCode: response.StatusCode, Description: "invalid API response", Retryable: retryable, Cause: errors.New(redactTelegramToken(err.Error(), c.token))}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || !envelope.OK {
		retryAfter := time.Duration(0)
		if envelope.Parameters != nil && envelope.Parameters.RetryAfter > 0 {
			retryAfter = time.Duration(envelope.Parameters.RetryAfter) * time.Second
		}
		if retryAfter == 0 {
			retryAfter = retryAfterHeader(response.Header.Get("Retry-After"))
		}
		status := response.StatusCode
		if status == 0 {
			status = http.StatusBadGateway
		}
		errorCode := envelope.ErrorCode
		if errorCode == 0 && status == http.StatusTooManyRequests {
			errorCode = status
		}
		description := redactTelegramToken(strings.TrimSpace(envelope.Description), c.token)
		if description == "" {
			description = http.StatusText(status)
		}
		return &telegramAPIError{
			Method:      method,
			StatusCode:  status,
			ErrorCode:   errorCode,
			Description: description,
			RetryAfter:  retryAfter,
			Retryable:   isTelegramRetryableStatus(status) || isTelegramRetryableCode(errorCode),
		}
	}
	if result == nil || len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return &telegramAPIError{Method: method, Description: "invalid result", Retryable: true, Cause: errors.New(redactTelegramToken(err.Error(), c.token))}
	}
	return nil
}

func isTelegramRetryableStatus(status int) bool {
	return status == 0 || status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func isTelegramRetryableCode(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooEarly || code == http.StatusTooManyRequests || code >= 500
}

func retryAfterHeader(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func redactTelegramToken(value, token string) string {
	if token != "" {
		value = strings.ReplaceAll(value, token, "[REDACTED]")
	}
	return sanitizeText(value)
}

func waitTelegramContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// telegramBotAPI is the seam used by the poller and supervisor. A fake can
// implement it without an HTTP server for higher-level tests.
type telegramBotAPI interface {
	GetUpdates(context.Context, int64, time.Duration) ([]telegramUpdate, error)
	SendMessage(context.Context, int64, string) error
}

type telegramMetadataAPI interface {
	GetMe(context.Context) (telegramBot, error)
	SetMyCommands(context.Context, []telegramBotCommand) error
	DeleteWebhook(context.Context) error
}

type telegramPollerOptions struct {
	PollTimeout    time.Duration
	InitialOffset  int64
	SeenLimit      int
	ErrorBackoff   []time.Duration
	OnHandlerError func(telegramUpdate, error)
	OnCommit       func(int64)
}

type telegramPoller struct {
	api            telegramBotAPI
	pollTimeout    time.Duration
	errorBackoff   []time.Duration
	onHandlerError func(telegramUpdate, error)
	onCommit       func(int64)
	seenLimit      int

	mu         sync.Mutex
	nextOffset int64
	seen       map[int64]struct{}
	processing map[int64]struct{}
}

func newTelegramPoller(api telegramBotAPI, options telegramPollerOptions) (*telegramPoller, error) {
	if api == nil {
		return nil, errors.New("telegram poller requires an API client")
	}
	pollTimeout := options.PollTimeout
	if pollTimeout == 0 {
		pollTimeout = telegramDefaultPollTimeout
	}
	if pollTimeout < 0 || pollTimeout > 50*time.Second {
		return nil, errors.New("telegram poll timeout must be between 0 and 50 seconds")
	}
	if options.InitialOffset < 0 {
		return nil, errors.New("telegram initial offset cannot be negative")
	}
	seenLimit := options.SeenLimit
	if seenLimit == 0 {
		seenLimit = telegramDefaultSeenLimit
	}
	if seenLimit < 1 {
		return nil, errors.New("telegram seen limit must be positive")
	}
	errorBackoff := append([]time.Duration(nil), options.ErrorBackoff...)
	if len(errorBackoff) == 0 {
		errorBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 30 * time.Second}
	}
	for _, delay := range errorBackoff {
		if delay < 0 {
			return nil, errors.New("telegram poll error backoff cannot be negative")
		}
	}
	return &telegramPoller{
		api:            api,
		pollTimeout:    pollTimeout,
		errorBackoff:   errorBackoff,
		onHandlerError: options.OnHandlerError,
		onCommit:       options.OnCommit,
		seenLimit:      seenLimit,
		nextOffset:     options.InitialOffset,
		seen:           map[int64]struct{}{},
		processing:     map[int64]struct{}{},
	}, nil
}

func (p *telegramPoller) Offset() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextOffset
}

// SetOffset is intended for restoring a persisted Telegram update cursor on
// startup. Moving the cursor backwards clears the in-memory deduplication set.
func (p *telegramPoller) SetOffset(offset int64) error {
	if offset < 0 {
		return errors.New("telegram update offset cannot be negative")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if offset < p.nextOffset {
		p.seen = map[int64]struct{}{}
	}
	p.nextOffset = offset
	return nil
}

func (p *telegramPoller) Run(ctx context.Context, handler func(context.Context, telegramUpdate) error) error {
	if handler == nil {
		return errors.New("telegram poller requires an update handler")
	}
	errorAttempt := 0
	for ctx.Err() == nil {
		updates, err := p.api.GetUpdates(ctx, p.Offset(), p.pollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var apiErr *telegramAPIError
			if !errors.As(err, &apiErr) || !apiErr.Retryable {
				return err
			}
			delay := apiErr.RetryAfter
			if delay <= 0 {
				index := errorAttempt
				if index >= len(p.errorBackoff) {
					index = len(p.errorBackoff) - 1
				}
				delay = p.errorBackoff[index]
			}
			errorAttempt++
			if !waitTelegramContext(ctx, delay) {
				return ctx.Err()
			}
			continue
		}
		errorAttempt = 0
		for _, update := range updates {
			if !p.beginUpdate(update.UpdateID) {
				continue
			}
			err := handler(ctx, update)
			commit := ctx.Err() == nil
			p.finishUpdate(update.UpdateID, commit)
			if commit && p.onCommit != nil {
				p.onCommit(p.Offset())
			}
			if err != nil && p.onHandlerError != nil {
				p.onHandlerError(update, err)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return ctx.Err()
}

func (p *telegramPoller) beginUpdate(updateID int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if updateID < p.nextOffset || updateID < 0 {
		return false
	}
	if _, ok := p.seen[updateID]; ok {
		return false
	}
	if _, ok := p.processing[updateID]; ok {
		return false
	}
	p.processing[updateID] = struct{}{}
	return true
}

func (p *telegramPoller) finishUpdate(updateID int64, commit bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.processing, updateID)
	if !commit {
		return
	}
	p.seen[updateID] = struct{}{}
	if updateID >= p.nextOffset {
		p.nextOffset = updateID + 1
	}
	if len(p.seen) <= p.seenLimit {
		return
	}
	cutoff := p.nextOffset - int64(p.seenLimit)
	for id := range p.seen {
		if id < cutoff {
			delete(p.seen, id)
		}
	}
}

type telegramAllowlist struct {
	ChatIDs map[int64]struct{}
	UserIDs map[int64]struct{}
}

func newTelegramAllowlist(chatIDs, userIDs []int64) telegramAllowlist {
	result := telegramAllowlist{ChatIDs: map[int64]struct{}{}, UserIDs: map[int64]struct{}{}}
	for _, id := range chatIDs {
		result.ChatIDs[id] = struct{}{}
	}
	for _, id := range userIDs {
		result.UserIDs[id] = struct{}{}
	}
	return result
}

// Allows requires every configured dimension to match. With no configured
// chat or user IDs it denies all messages by default.
func (a telegramAllowlist) Allows(update telegramUpdate) bool {
	if update.Message == nil {
		return false
	}
	if len(a.ChatIDs) == 0 && len(a.UserIDs) == 0 {
		return false
	}
	if len(a.ChatIDs) > 0 {
		if _, ok := a.ChatIDs[update.Message.Chat.ID]; !ok {
			return false
		}
	}
	if len(a.UserIDs) > 0 {
		if update.Message.From == nil {
			return false
		}
		if _, ok := a.UserIDs[update.Message.From.ID]; !ok {
			return false
		}
	}
	return true
}

type telegramCommand struct {
	Name    string
	Mention string
	Args    string
}

func parseTelegramCommand(text string) (telegramCommand, bool) {
	text = strings.TrimSpace(text)
	if text == "" || text[0] != '/' {
		return telegramCommand{}, false
	}
	end := strings.IndexAny(text, " \t\r\n")
	if end < 0 {
		end = len(text)
	}
	word := text[1:end]
	if word == "" {
		return telegramCommand{}, false
	}
	mention := ""
	if at := strings.IndexByte(word, '@'); at >= 0 {
		mention = word[at+1:]
		word = word[:at]
		if mention == "" {
			return telegramCommand{}, false
		}
	}
	for _, character := range word {
		if !unicode.IsLetter(character) && !unicode.IsDigit(character) && character != '_' {
			return telegramCommand{}, false
		}
	}
	args := ""
	if end < len(text) {
		args = strings.TrimSpace(text[end:])
	}
	return telegramCommand{Name: strings.ToLower(word), Mention: mention, Args: args}, true
}

func splitTelegramMessage(text string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	parts := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		size := limit
		if len(runes) < size {
			size = len(runes)
		}
		parts = append(parts, string(runes[:size]))
		runes = runes[size:]
	}
	return parts
}

type telegramOffsetState struct {
	Offset int64 `json:"offset"`
}

func loadTelegramOffset(path string) (int64, error) {
	if strings.TrimSpace(path) == "" {
		return 0, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var state telegramOffsetState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, fmt.Errorf("read Telegram offset: %w", err)
	}
	if state.Offset < 0 {
		return 0, errors.New("Telegram offset cannot be negative")
	}
	return state.Offset, nil
}

func saveTelegramOffset(path string, offset int64) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if offset < 0 {
		return errors.New("Telegram offset cannot be negative")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(telegramOffsetState{Offset: offset})
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

type telegramOutbound struct {
	ChatID int64
	Text   string
}

type telegramController struct {
	client       telegramBotAPI
	metadata     telegramMetadataAPI
	poller       *telegramPoller
	allowlist    telegramAllowlist
	chatIDs      []int64
	execute      func(context.Context, remoteCommand) (string, error)
	logger       *fileLogger
	onStateError func(error)

	mu           sync.Mutex
	botUsername  string
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	out          chan telegramOutbound
	started      bool
	notify       bool
	lastNotify   string
	lastNotifyAt time.Time
}

func newTelegramController(options supervisorOptions, execute func(context.Context, remoteCommand) (string, error), logger *fileLogger, onStateError func(error)) (*telegramController, error) {
	if strings.TrimSpace(options.TelegramToken) == "" {
		return nil, errors.New("Telegram token is empty")
	}
	if len(options.TelegramAllowedChats) == 0 {
		return nil, errors.New("at least one Telegram chat ID is required")
	}
	pollTimeout := options.TelegramPollTimeout
	if pollTimeout <= 0 {
		pollTimeout = telegramDefaultPollTimeout
	}
	client, err := newTelegramClient(telegramClientOptions{Token: options.TelegramToken, RequestTimeout: pollTimeout + 20*time.Second})
	if err != nil {
		return nil, err
	}
	offset, err := loadTelegramOffset(options.TelegramStatePath)
	if err != nil {
		return nil, err
	}
	poller, err := newTelegramPoller(client, telegramPollerOptions{
		PollTimeout:   pollTimeout,
		InitialOffset: offset,
		OnHandlerError: func(update telegramUpdate, err error) {
			if onStateError != nil {
				onStateError(fmt.Errorf("update %d: %w", update.UpdateID, err))
			}
		},
		OnCommit: func(offset int64) {
			if err := saveTelegramOffset(options.TelegramStatePath, offset); err != nil {
				if logger != nil {
					logger.Log("Persist Telegram offset: " + err.Error())
				}
				if onStateError != nil {
					onStateError(fmt.Errorf("persist Telegram offset: %w", err))
				}
			}
		},
	})
	if err != nil {
		return nil, err
	}
	chatIDs := append([]int64(nil), options.TelegramAllowedChats...)
	return &telegramController{
		client:       client,
		metadata:     client,
		poller:       poller,
		allowlist:    newTelegramAllowlist(options.TelegramAllowedChats, options.TelegramAllowedUsers),
		chatIDs:      chatIDs,
		execute:      execute,
		logger:       logger,
		onStateError: onStateError,
		notify:       options.TelegramNotify,
		out:          make(chan telegramOutbound, 128),
	}, nil
}

func (c *telegramController) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return errors.New("Telegram controller is already running")
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.done = make(chan struct{})
	c.started = true
	ctx := c.ctx
	c.mu.Unlock()
	go c.run(ctx)
	return nil
}

func (c *telegramController) run(ctx context.Context) {
	defer close(c.done)
	if c.metadata != nil {
		startupCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		if err := c.metadata.DeleteWebhook(startupCtx); err != nil && ctx.Err() == nil {
			c.reportError(fmt.Errorf("delete Telegram webhook: %w", err))
		}
		bot, err := c.metadata.GetMe(startupCtx)
		if err != nil {
			c.reportError(fmt.Errorf("Telegram bot authentication: %w", err))
		} else {
			c.mu.Lock()
			c.botUsername = strings.ToLower(strings.TrimSpace(bot.Username))
			c.mu.Unlock()
		}
		commands := []telegramBotCommand{
			{Command: "status", Description: "show Codexdog status"},
			{Command: "prompt", Description: "send a prompt"},
			{Command: "pause", Description: "pause the current work"},
			{Command: "resume", Description: "resume the current work"},
			{Command: "goal", Description: "show or change the current goal"},
			{Command: "stop", Description: "stop Codexdog (confirmation required)"},
			{Command: "help", Description: "show available commands"},
		}
		if err := c.metadata.SetMyCommands(startupCtx, commands); err != nil && ctx.Err() == nil {
			c.reportError(fmt.Errorf("set Telegram commands: %w", err))
		}
		cancel()
	}

	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		c.senderLoop(ctx)
	}()
	for ctx.Err() == nil {
		err := c.poller.Run(ctx, c.handleUpdate)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			c.reportError(err)
		}
		if !waitTelegramContext(ctx, 5*time.Second) {
			break
		}
	}
	<-senderDone
}

func (c *telegramController) Stop() {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if c.logger != nil {
				c.logger.Log("Timed out waiting for Telegram controller to stop")
			}
		}
	}
}

func (c *telegramController) Notify(text string) {
	if !c.notify || strings.TrimSpace(text) == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if text == c.lastNotify && now.Sub(c.lastNotifyAt) < 5*time.Second {
		c.mu.Unlock()
		return
	}
	c.lastNotify = text
	c.lastNotifyAt = now
	c.mu.Unlock()
	for _, chatID := range c.chatIDs {
		select {
		case c.out <- telegramOutbound{ChatID: chatID, Text: text}:
		default:
			if c.logger != nil {
				c.logger.Log("Telegram notification queue is full")
			}
			return
		}
	}
}

func (c *telegramController) SendFinal(text string) {
	if !c.notify || strings.TrimSpace(text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, chatID := range c.chatIDs {
		if err := c.client.SendMessage(ctx, chatID, text); err != nil {
			if c.logger != nil {
				c.logger.Log("Send final Telegram notification: " + err.Error())
			}
			return
		}
	}
}

func (c *telegramController) senderLoop(ctx context.Context) {
	for {
		select {
		case message := <-c.out:
			sendCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			err := c.client.SendMessage(sendCtx, message.ChatID, message.Text)
			cancel()
			if err != nil && ctx.Err() == nil {
				c.reportError(fmt.Errorf("send Telegram message: %w", err))
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *telegramController) handleUpdate(ctx context.Context, update telegramUpdate) error {
	message := update.Message
	if message == nil || message.From != nil && message.From.IsBot || !c.allowlist.Allows(update) {
		return nil
	}
	if c.onStateError != nil {
		c.onStateError(nil)
	}
	command, ok := parseTelegramCommand(message.Text)
	if !ok {
		return nil
	}
	c.mu.Lock()
	botUsername := c.botUsername
	c.mu.Unlock()
	if command.Mention != "" {
		if botUsername == "" || !strings.EqualFold(command.Mention, botUsername) {
			return nil
		}
	}
	remote := remoteCommand{Name: command.Name}
	switch command.Name {
	case "prompt":
		remote.Text = command.Args
	case "goal":
		remote.Text = command.Args
	case "stop":
		remote.Text = command.Args
		remote.Confirm = strings.EqualFold(strings.TrimSpace(command.Args), "confirm")
	case "status", "pause", "resume", "help":
		if command.Args != "" {
			remote.Text = command.Args
		}
	default:
		remote.Name = "help"
	}
	if c.execute == nil {
		return errors.New("Telegram command executor is unavailable")
	}
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	result, err := c.execute(commandCtx, remote)
	cancel()
	if err != nil {
		c.enqueueReply(message.Chat.ID, "Error: "+sanitizeText(err.Error()))
		return nil
	}
	c.enqueueReply(message.Chat.ID, result)
	return nil
}

func (c *telegramController) enqueueReply(chatID int64, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	select {
	case c.out <- telegramOutbound{ChatID: chatID, Text: text}:
	default:
		if c.logger != nil {
			c.logger.Log("Telegram reply queue is full")
		}
	}
}

func (c *telegramController) reportError(err error) {
	if err == nil {
		return
	}
	if c.logger != nil {
		c.logger.Log(err.Error())
	}
	if c.onStateError != nil {
		c.onStateError(err)
	}
}
