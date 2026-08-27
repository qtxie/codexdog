package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	wechatFixedBaseURL        = "https://ilinkai.weixin.qq.com"
	wechatChannelVersion      = "2.1.7"
	wechatAppID               = "bot"
	wechatAppClientVersion    = 131335
	wechatDefaultLoginTimeout = 8 * time.Minute
	wechatMaxLoginQRRefreshes = 3
	wechatMessageText         = 1
	wechatMessageLimit        = 4000
)

var errWeChatTokenExpired = errors.New("WeChat iLink bot token expired")

type wechatContextToken struct {
	Token     string `json:"token"`
	UpdatedAt string `json:"updatedAt"`
}

type wechatCredentials struct {
	Version       int                           `json:"version"`
	BotToken      string                        `json:"botToken"`
	BaseURL       string                        `json:"baseUrl"`
	BotID         string                        `json:"botId"`
	ILinkUserID   string                        `json:"ilinkUserId,omitempty"`
	GetUpdatesBuf string                        `json:"getUpdatesBuf,omitempty"`
	Contexts      map[string]wechatContextToken `json:"contexts,omitempty"`
}

func loadWeChatCredentials(path string) (wechatCredentials, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return wechatCredentials{}, false, nil
	}
	if err != nil {
		return wechatCredentials{}, false, fmt.Errorf("read WeChat credentials: %w", err)
	}
	var credentials wechatCredentials
	if err := json.Unmarshal(data, &credentials); err != nil {
		return wechatCredentials{}, false, fmt.Errorf("parse WeChat credentials: %w", err)
	}
	credentials.BotToken = strings.TrimSpace(credentials.BotToken)
	credentials.BaseURL = strings.TrimRight(strings.TrimSpace(credentials.BaseURL), "/")
	if credentials.BotToken == "" || credentials.BaseURL == "" || credentials.BotID == "" {
		return wechatCredentials{}, false, errors.New("WeChat credentials are incomplete; run `codexdog wechat logout` and log in again")
	}
	if credentials.Contexts == nil {
		credentials.Contexts = map[string]wechatContextToken{}
	}
	return credentials, true, nil
}

func saveWeChatCredentials(path string, credentials wechatCredentials) error {
	if strings.TrimSpace(credentials.BotToken) == "" || strings.TrimSpace(credentials.BaseURL) == "" || strings.TrimSpace(credentials.BotID) == "" {
		return errors.New("refusing to save incomplete WeChat credentials")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	credentials.Version = 1
	if credentials.Contexts == nil {
		credentials.Contexts = map[string]wechatContextToken{}
	}
	data, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

type wechatClientOptions struct {
	CredentialsPath string
	FixedBaseURL    string
	HTTPClient      *http.Client
}

type wechatClient struct {
	mu              sync.Mutex
	credentialsPath string
	fixedBaseURL    string
	loginPollURL    string
	httpClient      *http.Client
	credentials     wechatCredentials
}

func newWeChatClient(options wechatClientOptions, loadCredentials bool) (*wechatClient, error) {
	fixedBaseURL := strings.TrimRight(strings.TrimSpace(options.FixedBaseURL), "/")
	if fixedBaseURL == "" {
		fixedBaseURL = wechatFixedBaseURL
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	result := &wechatClient{
		credentialsPath: options.CredentialsPath,
		fixedBaseURL:    fixedBaseURL,
		loginPollURL:    fixedBaseURL,
		httpClient:      client,
		credentials:     wechatCredentials{Version: 1, BaseURL: fixedBaseURL, Contexts: map[string]wechatContextToken{}},
	}
	if !loadCredentials {
		return result, nil
	}
	credentials, ok, err := loadWeChatCredentials(options.CredentialsPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("WeChat bot is not logged in; run `codexdog wechat login`")
	}
	result.credentials = credentials
	return result, nil
}

func (c *wechatClient) Credentials() wechatCredentials {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneWeChatCredentials(c.credentials)
}

func cloneWeChatCredentials(credentials wechatCredentials) wechatCredentials {
	copy := credentials
	copy.Contexts = make(map[string]wechatContextToken, len(credentials.Contexts))
	for userID, token := range credentials.Contexts {
		copy.Contexts[userID] = token
	}
	return copy
}

type wechatQRCode struct {
	QRCode          string `json:"qrcode"`
	QRCodeImageData string `json:"qrcode_img_content"`
	URL             string `json:"url"`
}

type wechatQRCodeStatus struct {
	Status       string `json:"status"`
	RedirectHost string `json:"redirect_host"`
	BotToken     string `json:"bot_token"`
	BaseURL      string `json:"baseurl"`
	BotID        string `json:"ilink_bot_id"`
	ILinkUserID  string `json:"ilink_user_id"`
}

func (c *wechatClient) GetQRCode(ctx context.Context) (wechatQRCode, error) {
	endpoint := c.fixedBaseURL + "/ilink/bot/get_bot_qrcode?bot_type=3"
	var result wechatQRCode
	if err := c.getJSON(ctx, endpoint, 15*time.Second, &result); err != nil {
		return wechatQRCode{}, fmt.Errorf("get WeChat login QR code: %w", err)
	}
	if strings.TrimSpace(result.QRCode) == "" {
		return wechatQRCode{}, errors.New("WeChat login service returned an empty QR code")
	}
	if result.URL == "" && isWeChatQRCodeURL(result.QRCodeImageData) {
		result.URL = strings.TrimSpace(result.QRCodeImageData)
	}
	return result, nil
}

func (c *wechatClient) PollQRCodeStatus(ctx context.Context, qrcode string) (wechatQRCodeStatus, error) {
	c.mu.Lock()
	pollBaseURL := c.loginPollURL
	c.mu.Unlock()
	endpoint := pollBaseURL + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrcode)
	var result wechatQRCodeStatus
	if err := c.getJSON(ctx, endpoint, 45*time.Second, &result); err != nil {
		return wechatQRCodeStatus{}, fmt.Errorf("poll WeChat login QR code: %w", err)
	}
	switch result.Status {
	case "scaned_but_redirect":
		if host := strings.TrimSpace(result.RedirectHost); host != "" {
			parsed, err := url.Parse("https://" + host)
			if err != nil || parsed.Host == "" || parsed.Path != "" {
				return wechatQRCodeStatus{}, errors.New("WeChat login service returned an invalid redirect host")
			}
			c.mu.Lock()
			c.loginPollURL = strings.TrimRight(parsed.String(), "/")
			c.mu.Unlock()
		}
	case "expired":
		c.mu.Lock()
		c.loginPollURL = c.fixedBaseURL
		c.mu.Unlock()
	case "confirmed":
		if strings.TrimSpace(result.BotToken) == "" || strings.TrimSpace(result.BotID) == "" {
			return wechatQRCodeStatus{}, errors.New("WeChat login confirmation omitted bot credentials")
		}
		baseURL := strings.TrimRight(strings.TrimSpace(result.BaseURL), "/")
		if baseURL == "" {
			baseURL = c.fixedBaseURL
		}
		c.mu.Lock()
		c.credentials = wechatCredentials{
			Version:     1,
			BotToken:    result.BotToken,
			BaseURL:     baseURL,
			BotID:       result.BotID,
			ILinkUserID: result.ILinkUserID,
			Contexts:    map[string]wechatContextToken{},
		}
		c.loginPollURL = c.fixedBaseURL
		if err := saveWeChatCredentials(c.credentialsPath, cloneWeChatCredentials(c.credentials)); err != nil {
			c.mu.Unlock()
			return wechatQRCodeStatus{}, fmt.Errorf("save WeChat credentials: %w", err)
		}
		c.mu.Unlock()
	}
	return result, nil
}

type wechatTextItem struct {
	Text string `json:"text"`
}

type wechatMessageItem struct {
	Type     int             `json:"type"`
	TextItem *wechatTextItem `json:"text_item,omitempty"`
}

type wechatMessage struct {
	FromUserID   string              `json:"from_user_id"`
	ToUserID     string              `json:"to_user_id"`
	ClientID     string              `json:"client_id"`
	MessageType  int                 `json:"message_type"`
	ContextToken string              `json:"context_token"`
	ItemList     []wechatMessageItem `json:"item_list"`
}

func (m wechatMessage) Text() string {
	parts := make([]string, 0, len(m.ItemList))
	for _, item := range m.ItemList {
		if item.Type == wechatMessageText && item.TextItem != nil && item.TextItem.Text != "" {
			parts = append(parts, item.TextItem.Text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

type wechatUpdatesResponse struct {
	Ret           int             `json:"ret"`
	ErrCode       any             `json:"errcode"`
	ErrMsg        string          `json:"errmsg"`
	GetUpdatesBuf string          `json:"get_updates_buf"`
	Messages      []wechatMessage `json:"msgs"`
}

func (c *wechatClient) GetUpdates(ctx context.Context, pollTimeout time.Duration) ([]wechatMessage, error) {
	if pollTimeout <= 0 {
		pollTimeout = 35 * time.Second
	}
	c.mu.Lock()
	credentials := c.credentials
	c.mu.Unlock()
	var response wechatUpdatesResponse
	err := c.postJSON(ctx, credentials.BaseURL, "ilink/bot/getupdates", credentials.BotToken, map[string]any{
		"get_updates_buf": credentials.GetUpdatesBuf,
	}, pollTimeout+10*time.Second, &response)
	if err != nil {
		return nil, fmt.Errorf("get WeChat updates: %w", err)
	}
	if err := c.checkAPIResult("getupdates", response.Ret, response.ErrCode, response.ErrMsg); err != nil {
		return nil, err
	}
	if response.GetUpdatesBuf != "" && response.GetUpdatesBuf != credentials.GetUpdatesBuf {
		c.mu.Lock()
		c.credentials.GetUpdatesBuf = response.GetUpdatesBuf
		if err := saveWeChatCredentials(c.credentialsPath, cloneWeChatCredentials(c.credentials)); err != nil {
			c.mu.Unlock()
			return nil, fmt.Errorf("persist WeChat update cursor: %w", err)
		}
		c.mu.Unlock()
	}
	return response.Messages, nil
}

type wechatAPIResponse struct {
	Ret     int    `json:"ret"`
	ErrCode any    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func (c *wechatClient) SendText(ctx context.Context, toUserID, text, contextToken string) error {
	credentials := c.Credentials()
	clientID, err := wechatClientID()
	if err != nil {
		return err
	}
	payload := map[string]any{"msg": map[string]any{
		"from_user_id":  "",
		"to_user_id":    toUserID,
		"client_id":     clientID,
		"message_type":  2,
		"message_state": 2,
		"context_token": contextToken,
		"item_list": []any{map[string]any{
			"type":      wechatMessageText,
			"text_item": map[string]any{"text": text},
		}},
	}}
	var response wechatAPIResponse
	if err := c.postJSON(ctx, credentials.BaseURL, "ilink/bot/sendmessage", credentials.BotToken, payload, 15*time.Second, &response); err != nil {
		return fmt.Errorf("send WeChat message: %w", err)
	}
	return c.checkAPIResult("sendmessage", response.Ret, response.ErrCode, response.ErrMsg)
}

func (c *wechatClient) SendTyping(ctx context.Context, toUserID, contextToken string) error {
	credentials := c.Credentials()
	var config struct {
		wechatAPIResponse
		TypingTicket string `json:"typing_ticket"`
	}
	if err := c.postJSON(ctx, credentials.BaseURL, "ilink/bot/getconfig", credentials.BotToken, map[string]any{
		"ilink_user_id": toUserID,
		"context_token": contextToken,
	}, 10*time.Second, &config); err != nil {
		return fmt.Errorf("get WeChat typing ticket: %w", err)
	}
	if err := c.checkAPIResult("getconfig", config.Ret, config.ErrCode, config.ErrMsg); err != nil {
		return err
	}
	var response wechatAPIResponse
	if err := c.postJSON(ctx, credentials.BaseURL, "ilink/bot/sendtyping", credentials.BotToken, map[string]any{
		"ilink_user_id": toUserID,
		"typing_ticket": config.TypingTicket,
		"status":        1,
	}, 10*time.Second, &response); err != nil {
		return fmt.Errorf("send WeChat typing state: %w", err)
	}
	return c.checkAPIResult("sendtyping", response.Ret, response.ErrCode, response.ErrMsg)
}

func (c *wechatClient) checkAPIResult(operation string, ret int, errCode any, errMsg string) error {
	err := checkWeChatAPIResult(operation, ret, errCode, errMsg)
	if err == nil {
		return nil
	}
	if isWeChatTokenExpired(ret, errCode) {
		_ = c.clearCredentials()
		return fmt.Errorf("%w: %v", errWeChatTokenExpired, err)
	}
	return err
}

func isWeChatTokenExpired(ret int, errCode any) bool {
	if ret == -1 || ret == 401 || ret == 403 {
		return true
	}
	switch value := errCode.(type) {
	case string:
		return strings.EqualFold(value, "TokenExpired") || strings.EqualFold(value, "token_expired")
	default:
		return false
	}
}

func (c *wechatClient) clearCredentials() error {
	c.mu.Lock()
	c.credentials = wechatCredentials{Version: 1, BaseURL: c.fixedBaseURL, Contexts: map[string]wechatContextToken{}}
	c.mu.Unlock()
	if c.credentialsPath == "" {
		return nil
	}
	if err := os.Remove(c.credentialsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (c *wechatClient) RecordContext(userID, contextToken string) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(contextToken) == "" {
		return nil
	}
	c.mu.Lock()
	if c.credentials.Contexts == nil {
		c.credentials.Contexts = map[string]wechatContextToken{}
	}
	existing := c.credentials.Contexts[userID]
	if existing.Token == contextToken {
		c.mu.Unlock()
		return nil
	}
	c.credentials.Contexts[userID] = wechatContextToken{Token: contextToken, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := saveWeChatCredentials(c.credentialsPath, cloneWeChatCredentials(c.credentials)); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("persist WeChat context token: %w", err)
	}
	c.mu.Unlock()
	return nil
}

func (c *wechatClient) Context(userID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	contextToken, ok := c.credentials.Contexts[userID]
	return contextToken.Token, ok && contextToken.Token != ""
}

func (c *wechatClient) getJSON(ctx context.Context, endpoint string, timeout time.Duration, output any) error {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("iLink-App-Id", wechatAppID)
	request.Header.Set("iLink-App-ClientVersion", strconv.Itoa(wechatAppClientVersion))
	return c.doJSON(request, output)
}

func (c *wechatClient) postJSON(ctx context.Context, baseURL, path, token string, payload map[string]any, timeout time.Duration, output any) error {
	body := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		body[key] = value
	}
	body["base_info"] = map[string]any{
		"channel_version": wechatChannelVersion,
		"bot_agent":       wechatBotAgent(),
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(path, "/")
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("AuthorizationType", "ilink_bot_token")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-WECHAT-UIN", randomWeChatUIN())
	request.Header.Set("iLink-App-Id", wechatAppID)
	request.Header.Set("iLink-App-ClientVersion", strconv.Itoa(wechatAppClientVersion))
	return c.doJSON(request, output)
}

// wechatBotAgent identifies CodexDog in iLink request metadata. Tencent documents
// this field as observability metadata; it does not control the native WeChat
// authorization page's product name.
func wechatBotAgent() string {
	return "CodexDog/" + version
}

func (c *wechatClient) doJSON(request *http.Request, output any) error {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("iLink HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode iLink response: %w", err)
	}
	return nil
}

func randomWeChatUIN() string {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		binary.LittleEndian.PutUint32(buffer, uint32(time.Now().UnixNano()))
	}
	value := strconv.FormatUint(uint64(binary.LittleEndian.Uint32(buffer)), 10)
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func wechatClientID() (string, error) {
	buffer := make([]byte, 4)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate WeChat message ID: %w", err)
	}
	return fmt.Sprintf("codexdog:%d-%x", time.Now().UnixMilli(), buffer), nil
}

func checkWeChatAPIResult(operation string, ret int, errCode any, errMsg string) error {
	if ret == 0 && isZeroWeChatErrorCode(errCode) {
		return nil
	}
	if ret == -2 {
		return fmt.Errorf("WeChat %s blocked by the iLink 24-hour/session send limit (ret=-2)", operation)
	}
	return fmt.Errorf("WeChat %s failed (ret=%d, errcode=%v, errmsg=%s)", operation, ret, errCode, sanitizeText(errMsg))
}

func isZeroWeChatErrorCode(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case float64:
		return typed == 0
	case int:
		return typed == 0
	case string:
		return typed == "" || typed == "0"
	default:
		return false
	}
}

type wechatBotAPI interface {
	GetUpdates(context.Context, time.Duration) ([]wechatMessage, error)
	SendText(context.Context, string, string, string) error
	SendTyping(context.Context, string, string) error
	RecordContext(string, string) error
	Context(string) (string, bool)
}

type wechatOutbound struct {
	UserID       string
	Text         string
	ContextToken string
}

type wechatController struct {
	client       wechatBotAPI
	allowedUsers map[string]struct{}
	userIDs      []string
	pollTimeout  time.Duration
	execute      func(context.Context, remoteCommand) (string, error)
	logger       *fileLogger
	onStateError func(error)
	notify       bool
	out          chan wechatOutbound

	mu           sync.Mutex
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	started      bool
	lastNotify   string
	lastNotifyAt time.Time
}

func newWeChatController(options supervisorOptions, execute func(context.Context, remoteCommand) (string, error), logger *fileLogger, onStateError func(error)) (*wechatController, error) {
	client, err := newWeChatClient(wechatClientOptions{CredentialsPath: options.WeChatCredentialsPath}, true)
	if err != nil {
		return nil, err
	}
	allowedUsers := make(map[string]struct{}, len(options.WeChatAllowedUsers))
	for _, userID := range options.WeChatAllowedUsers {
		allowedUsers[userID] = struct{}{}
	}
	return &wechatController{
		client:       client,
		allowedUsers: allowedUsers,
		userIDs:      append([]string(nil), options.WeChatAllowedUsers...),
		pollTimeout:  options.WeChatPollTimeout,
		execute:      execute,
		logger:       logger,
		onStateError: onStateError,
		notify:       options.WeChatNotify,
		out:          make(chan wechatOutbound, 128),
	}, nil
}

func (c *wechatController) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started {
		return errors.New("WeChat controller is already running")
	}
	c.ctx, c.cancel = context.WithCancel(parent)
	c.done = make(chan struct{})
	c.started = true
	go c.run(c.ctx)
	return nil
}

func (c *wechatController) run(ctx context.Context) {
	defer close(c.done)
	senderDone := make(chan struct{})
	go func() {
		defer close(senderDone)
		c.senderLoop(ctx)
	}()
	for ctx.Err() == nil {
		pollStarted := time.Now()
		messages, err := c.client.GetUpdates(ctx, c.pollTimeout)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			c.reportError(err)
			if errors.Is(err, errWeChatTokenExpired) {
				c.cancelRun()
				break
			}
			if !waitWeChatContext(ctx, 3*time.Second) {
				break
			}
			continue
		}
		if len(messages) > 0 && c.onStateError != nil {
			c.onStateError(nil)
		}
		for _, message := range messages {
			if err := c.handleMessage(ctx, message); err != nil {
				c.reportError(err)
			}
		}
		if len(messages) == 0 {
			minimumPoll := 250 * time.Millisecond
			if elapsed := time.Since(pollStarted); elapsed < minimumPoll && !waitWeChatContext(ctx, minimumPoll-elapsed) {
				break
			}
		}
	}
	<-senderDone
}

func (c *wechatController) Stop() {
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
				c.logger.Log("Timed out waiting for WeChat controller to stop")
			}
		}
	}
}

func (c *wechatController) handleMessage(ctx context.Context, message wechatMessage) error {
	if message.MessageType != 0 && message.MessageType != 1 {
		return nil
	}
	userID := strings.TrimSpace(message.FromUserID)
	text := message.Text()
	if userID == "" || text == "" {
		return nil
	}
	command, ok := parseTelegramCommand(text)
	if !ok {
		return nil
	}
	if command.Mention != "" {
		return nil
	}
	if command.Name == "uid" {
		reply := "Your WeChat iLink user ID:\n" + userID
		if _, allowed := c.allowedUsers[userID]; allowed {
			if err := c.client.RecordContext(userID, message.ContextToken); err != nil {
				return err
			}
		} else {
			reply += "\n\nAdd it with --wechat-user-id or CODEXDOG_WECHAT_USER_IDS, then restart Codexdog."
		}
		c.enqueueReply(userID, reply, message.ContextToken)
		return nil
	}
	if _, allowed := c.allowedUsers[userID]; !allowed {
		return nil
	}
	if err := c.client.RecordContext(userID, message.ContextToken); err != nil {
		return err
	}
	remote := remoteCommand{Name: command.Name, Text: command.Args}
	switch command.Name {
	case "stop":
		remote.Confirm = strings.EqualFold(strings.TrimSpace(command.Args), "confirm")
	case "status", "prompt", "pause", "resume", "goal", "queue", "help":
	default:
		remote.Name = "help"
		remote.Text = ""
	}
	if c.execute == nil {
		return errors.New("WeChat command executor is unavailable")
	}
	typingCtx, typingCancel := context.WithTimeout(ctx, 12*time.Second)
	typingErr := c.client.SendTyping(typingCtx, userID, message.ContextToken)
	typingCancel()
	if errors.Is(typingErr, errWeChatTokenExpired) {
		c.reportError(typingErr)
		c.cancelRun()
		return nil
	}
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	result, err := c.execute(commandCtx, remote)
	cancel()
	if err != nil {
		c.enqueueReply(userID, "Error: "+sanitizeText(err.Error()), message.ContextToken)
		return nil
	}
	if command.Name == "help" {
		result += "\n/uid - show your WeChat iLink user ID"
	}
	c.enqueueReply(userID, result, message.ContextToken)
	return nil
}

func (c *wechatController) Notify(text string) {
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
	for _, userID := range c.userIDs {
		contextToken, ok := c.client.Context(userID)
		if !ok {
			continue
		}
		c.enqueueReply(userID, text, contextToken)
	}
}

func (c *wechatController) SendFinal(text string) {
	if !c.notify || strings.TrimSpace(text) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, userID := range c.userIDs {
		contextToken, ok := c.client.Context(userID)
		if !ok {
			continue
		}
		for _, part := range splitWeChatMessage(text, wechatMessageLimit) {
			if err := c.client.SendText(ctx, userID, part, contextToken); err != nil {
				if c.logger != nil {
					c.logger.Log("Send final WeChat notification: " + err.Error())
				}
				return
			}
		}
	}
}

func (c *wechatController) enqueueReply(userID, text, contextToken string) {
	for _, part := range splitWeChatMessage(text, wechatMessageLimit) {
		select {
		case c.out <- wechatOutbound{UserID: userID, Text: part, ContextToken: contextToken}:
		default:
			if c.logger != nil {
				c.logger.Log("WeChat reply queue is full")
			}
			return
		}
	}
}

func (c *wechatController) senderLoop(ctx context.Context) {
	for {
		select {
		case message := <-c.out:
			sendCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := c.client.SendText(sendCtx, message.UserID, message.Text, message.ContextToken)
			cancel()
			if err != nil && ctx.Err() == nil {
				c.reportError(err)
				if errors.Is(err, errWeChatTokenExpired) {
					c.cancelRun()
					return
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (c *wechatController) cancelRun() {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *wechatController) reportError(err error) {
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

func splitWeChatMessage(text string, limit int) []string {
	if limit <= 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	runes := []rune(text)
	parts := make([]string, 0, (len(runes)+limit-1)/limit)
	for len(runes) > 0 {
		size := min(limit, len(runes))
		parts = append(parts, string(runes[:size]))
		runes = runes[size:]
	}
	return parts
}

func waitWeChatContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runWeChatManagement(args parsedArguments, store *stateStore) (int, error) {
	action := "status"
	if len(args.CommandArgs) > 0 {
		action = strings.ToLower(strings.TrimSpace(args.CommandArgs[0]))
	}
	if len(args.CommandArgs) > 1 {
		return 1, errors.New("usage: codexdog wechat [login|status|logout] [options]")
	}
	switch action {
	case "status":
		credentials, ok, err := loadWeChatCredentials(args.Options.WeChatCredentialsPath)
		if err != nil {
			return 1, err
		}
		controllerLive := false
		if state, exists := store.Read(); exists {
			if current, live := queryControlState(state); live {
				controllerLive = current.WeChatEnabled
			}
		}
		if args.JSON {
			result := map[string]any{"loggedIn": ok, "controllerLive": controllerLive}
			if ok {
				result["botId"] = credentials.BotID
				result["ilinkUserId"] = credentials.ILinkUserID
				result["baseUrl"] = credentials.BaseURL
				result["knownUsers"] = len(credentials.Contexts)
			}
			data, marshalErr := json.MarshalIndent(result, "", "  ")
			if marshalErr != nil {
				return 1, marshalErr
			}
			fmt.Printf("%s\n", data)
			return 0, nil
		}
		fmt.Printf("Logged in: %s\n", yesNo(ok))
		fmt.Printf("Controller live: %s\n", yesNo(controllerLive))
		if ok {
			fmt.Printf("Bot ID: %s\n", credentials.BotID)
			fmt.Printf("iLink user ID: %s\n", valueOrDash(credentials.ILinkUserID))
			fmt.Printf("API: %s\n", credentials.BaseURL)
			fmt.Printf("Known users: %d\n", len(credentials.Contexts))
		}
		return 0, nil
	case "login":
		if state, ok := store.Read(); ok && queryControl(state) {
			return 1, errors.New("stop the live Codexdog supervisor before changing its WeChat login")
		}
		if _, ok, err := loadWeChatCredentials(args.Options.WeChatCredentialsPath); err != nil {
			return 1, err
		} else if ok {
			return 1, errors.New("a WeChat bot is already logged in; run `codexdog wechat logout` first")
		}
		return runWeChatLogin(args.Options)
	case "logout":
		if state, ok := store.Read(); ok && queryControl(state) {
			return 1, errors.New("stop the live Codexdog supervisor before logging out its WeChat bot")
		}
		if err := os.Remove(args.Options.WeChatCredentialsPath); errors.Is(err, os.ErrNotExist) {
			fmt.Println("WeChat bot is already logged out.")
			return 0, nil
		} else if err != nil {
			return 1, fmt.Errorf("remove WeChat credentials: %w", err)
		}
		fmt.Println("WeChat bot credentials removed.")
		return 0, nil
	default:
		return 1, fmt.Errorf("unknown WeChat action %q; use login, status, or logout", action)
	}
}

func runWeChatLogin(options supervisorOptions) (int, error) {
	client, err := newWeChatClient(wechatClientOptions{CredentialsPath: options.WeChatCredentialsPath}, false)
	if err != nil {
		return 1, err
	}
	timeout := options.WeChatLoginTimeout
	if timeout <= 0 {
		timeout = wechatDefaultLoginTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fmt.Println("CodexDog is connecting to WeChat via iLink.")
	var qrPath string
	openBrowser := options.WeChatOpenBrowser
	defer func() {
		if qrPath != "" {
			_ = os.Remove(qrPath)
		}
	}()
	showQRCode := func(qrcode wechatQRCode) error {
		if qrPath != "" {
			_ = os.Remove(qrPath)
			qrPath = ""
		}
		path, wroteImage, err := writeWeChatQRCode(options.WeChatQRCodePath, qrcode.QRCodeImageData)
		if err != nil {
			return err
		}
		qrPath = path
		if wroteImage {
			fmt.Printf("Open this QR image and scan it with WeChat: %s\n", path)
		}
		if qrcode.URL != "" {
			fmt.Printf("Login URL: %s\n", qrcode.URL)
			if openBrowser {
				if err := openWeChatLoginURL(qrcode.URL); err != nil {
					fmt.Printf("Could not open the login URL automatically: %v\n", err)
					openBrowser = false
				} else {
					fmt.Println("Opened the login URL in the default browser.")
				}
			}
		}
		if !wroteImage && qrcode.URL == "" {
			fmt.Printf("QR payload: %s\n", qrcode.QRCode)
		}
		return nil
	}

	qrcode, err := client.GetQRCode(ctx)
	if err != nil {
		return 1, err
	}
	if err := showQRCode(qrcode); err != nil {
		return 1, err
	}
	fmt.Println("Waiting for scan confirmation...")
	lastStatus := ""
	qrRefreshes := 0
	for ctx.Err() == nil {
		status, pollErr := client.PollQRCodeStatus(ctx, qrcode.QRCode)
		if pollErr != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				break
			}
			return 1, pollErr
		}
		if status.Status != lastStatus {
			fmt.Printf("WeChat login status: %s\n", valueOrDash(status.Status))
			lastStatus = status.Status
		}
		switch status.Status {
		case "confirmed":
			credentials := client.Credentials()
			fmt.Printf("CodexDog connected to WeChat. Bot ID: %s\n", credentials.BotID)
			return 0, nil
		case "expired":
			if qrRefreshes >= wechatMaxLoginQRRefreshes {
				return 1, errors.New("WeChat login QR code expired too many times; run login again")
			}
			qrRefreshes++
			fmt.Printf("WeChat login QR code expired; generating a new code (%d/%d)...\n", qrRefreshes, wechatMaxLoginQRRefreshes)
			qrcode, err = client.GetQRCode(ctx)
			if err != nil {
				return 1, err
			}
			if err := showQRCode(qrcode); err != nil {
				return 1, err
			}
			lastStatus = ""
		}
		if !waitWeChatContext(ctx, 500*time.Millisecond) {
			break
		}
	}
	return 1, fmt.Errorf("WeChat login timed out after %s", timeout)
}

func writeWeChatQRCode(path, encoded string) (string, bool, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return "", false, nil
	}
	if isWeChatQRCodeURL(encoded) {
		return "", false, nil
	}
	if comma := strings.IndexByte(encoded, ','); strings.HasPrefix(encoded, "data:") && comma >= 0 {
		encoded = encoded[comma+1:]
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", false, fmt.Errorf("decode WeChat QR image: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", false, fmt.Errorf("write WeChat QR image: %w", err)
	}
	return path, true, nil
}

func isWeChatQRCodeURL(value string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return false
	}
	return (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

type wechatBrowserCommand struct {
	Name string
	Args []string
}

func openWeChatLoginURL(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("login URL is not a valid HTTPS URL")
	}
	var lastErr error
	for _, candidate := range wechatBrowserCommands(runtime.GOOS, parsed.String()) {
		path, err := exec.LookPath(candidate.Name)
		if err != nil {
			lastErr = err
			continue
		}
		command := exec.Command(path, candidate.Args...)
		if err := command.Start(); err != nil {
			lastErr = err
			continue
		}
		if command.Process != nil {
			_ = command.Process.Release()
		}
		return nil
	}
	if lastErr == nil {
		lastErr = errors.New("unsupported operating system")
	}
	return fmt.Errorf("no browser launcher succeeded: %w", lastErr)
}

func wechatBrowserCommands(goos, value string) []wechatBrowserCommand {
	switch goos {
	case "windows":
		return []wechatBrowserCommand{{Name: "rundll32.exe", Args: []string{"url.dll,FileProtocolHandler", value}}}
	case "darwin":
		return []wechatBrowserCommand{{Name: "open", Args: []string{value}}}
	case "linux":
		return []wechatBrowserCommand{
			{Name: "xdg-open", Args: []string{value}},
			{Name: "gio", Args: []string{"open", value}},
		}
	default:
		return nil
	}
}
