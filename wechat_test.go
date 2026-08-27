package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWeChatClientLoginAndCredentialPersistence(t *testing.T) {
	t.Parallel()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("iLink-App-Id") != wechatAppID || request.Header.Get("iLink-App-ClientVersion") != fmt.Sprint(wechatAppClientVersion) {
			t.Errorf("missing iLink client headers: %v", request.Header)
		}
		switch request.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			if request.URL.Query().Get("bot_type") != "3" {
				t.Errorf("bot_type = %q", request.URL.Query().Get("bot_type"))
			}
			writeTestJSON(t, response, map[string]any{
				"qrcode":             "qr-token",
				"qrcode_img_content": "https://liteapp.weixin.qq.com/q/qr-token",
			})
		case "/ilink/bot/get_qrcode_status":
			if request.URL.Query().Get("qrcode") != "qr-token" {
				t.Errorf("qrcode = %q", request.URL.Query().Get("qrcode"))
			}
			writeTestJSON(t, response, map[string]any{
				"status":        "confirmed",
				"bot_token":     "secret@im.bot:value",
				"baseurl":       server.URL,
				"ilink_bot_id":  "bot-1",
				"ilink_user_id": "bot-user-1",
			})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialsPath := filepath.Join(t.TempDir(), "nested", "wechat.json")
	client, err := newWeChatClient(wechatClientOptions{
		CredentialsPath: credentialsPath,
		FixedBaseURL:    server.URL,
		HTTPClient:      server.Client(),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	qr, err := client.GetQRCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if qr.QRCode != "qr-token" {
		t.Fatalf("QR = %#v", qr)
	}
	if qr.URL != "https://liteapp.weixin.qq.com/q/qr-token" {
		t.Fatalf("QR URL = %#v", qr)
	}
	status, err := client.PollQRCodeStatus(context.Background(), qr.QRCode)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "confirmed" {
		t.Fatalf("status = %#v", status)
	}
	credentials, ok, err := loadWeChatCredentials(credentialsPath)
	if err != nil || !ok {
		t.Fatalf("load credentials: ok=%v err=%v", ok, err)
	}
	if credentials.BotToken != "secret@im.bot:value" || credentials.BotID != "bot-1" || credentials.ILinkUserID != "bot-user-1" {
		t.Fatalf("credentials = %#v", credentials)
	}
}

func TestWriteWeChatQRCodeAcceptsURLAndBase64(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "qr.png")
	if written, ok, err := writeWeChatQRCode(path, "https://liteapp.weixin.qq.com/q/qr-token"); err != nil || ok || written != "" {
		t.Fatalf("URL QR = path %q, ok %v, err %v", written, ok, err)
	}
	written, ok, err := writeWeChatQRCode(path, "data:image/png;base64,"+base64.StdEncoding.EncodeToString([]byte("png")))
	if err != nil || !ok || written != path {
		t.Fatalf("base64 QR = path %q, ok %v, err %v", written, ok, err)
	}
}

func TestWeChatClientUpdatesSendAndTypingProtocol(t *testing.T) {
	t.Parallel()
	type observedRequest struct {
		Path string
		Body map[string]any
	}
	var mu sync.Mutex
	var observed []observedRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
		if request.Header.Get("Authorization") != "Bearer secret-token" || request.Header.Get("AuthorizationType") != "ilink_bot_token" {
			t.Errorf("authorization headers = %v", request.Header)
		}
		if request.Header.Get("X-WECHAT-UIN") == "" {
			t.Error("X-WECHAT-UIN is empty")
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		observed = append(observed, observedRequest{Path: request.URL.Path, Body: body})
		mu.Unlock()
		baseInfo, _ := body["base_info"].(map[string]any)
		if baseInfo["channel_version"] != wechatChannelVersion || baseInfo["bot_agent"] != wechatBotAgent() {
			t.Errorf("base_info = %#v", baseInfo)
		}
		switch request.URL.Path {
		case "/ilink/bot/getupdates":
			writeTestJSON(t, response, map[string]any{
				"ret":             0,
				"errcode":         0,
				"get_updates_buf": "cursor-2",
				"msgs": []any{map[string]any{
					"from_user_id":  "user-1",
					"context_token": "context-1",
					"item_list":     []any{map[string]any{"type": 1, "text_item": map[string]any{"text": "/status"}}},
				}},
			})
		case "/ilink/bot/sendmessage":
			writeTestJSON(t, response, map[string]any{"ret": 0, "errcode": 0})
		case "/ilink/bot/getconfig":
			writeTestJSON(t, response, map[string]any{"ret": 0, "errcode": 0, "typing_ticket": "ticket-1"})
		case "/ilink/bot/sendtyping":
			writeTestJSON(t, response, map[string]any{"ret": 0, "errcode": 0})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	credentialsPath := filepath.Join(t.TempDir(), "wechat.json")
	if err := saveWeChatCredentials(credentialsPath, wechatCredentials{
		BotToken: "secret-token", BaseURL: server.URL, BotID: "bot-1", GetUpdatesBuf: "cursor-1",
	}); err != nil {
		t.Fatal(err)
	}
	client, err := newWeChatClient(wechatClientOptions{CredentialsPath: credentialsPath, HTTPClient: server.Client()}, true)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := client.GetUpdates(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Text() != "/status" {
		t.Fatalf("messages = %#v", messages)
	}
	if client.Credentials().GetUpdatesBuf != "cursor-2" {
		t.Fatalf("cursor = %q", client.Credentials().GetUpdatesBuf)
	}
	if err := client.RecordContext("user-1", "context-1"); err != nil {
		t.Fatal(err)
	}
	if token, ok := client.Context("user-1"); !ok || token != "context-1" {
		t.Fatalf("context = %q, %v", token, ok)
	}
	if err := client.SendText(context.Background(), "user-1", "hello", "context-1"); err != nil {
		t.Fatal(err)
	}
	if err := client.SendTyping(context.Background(), "user-1", "context-1"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	requests := append([]observedRequest(nil), observed...)
	mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("requests = %#v", requests)
	}
	if requests[0].Body["get_updates_buf"] != "cursor-1" {
		t.Fatalf("getupdates body = %#v", requests[0].Body)
	}
	messageBody := requests[1].Body["msg"].(map[string]any)
	if messageBody["to_user_id"] != "user-1" || messageBody["context_token"] != "context-1" || messageBody["message_state"] != float64(2) {
		t.Fatalf("sendmessage body = %#v", messageBody)
	}
	items := messageBody["item_list"].([]any)
	textItem := items[0].(map[string]any)["text_item"].(map[string]any)
	if textItem["text"] != "hello" {
		t.Fatalf("sendmessage text item = %#v", textItem)
	}
	if requests[3].Body["typing_ticket"] != "ticket-1" {
		t.Fatalf("sendtyping body = %#v", requests[3].Body)
	}
	persisted, _, err := loadWeChatCredentials(credentialsPath)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.GetUpdatesBuf != "cursor-2" || persisted.Contexts["user-1"].Token != "context-1" {
		t.Fatalf("persisted = %#v", persisted)
	}
}

func TestWeChatClientReportsSessionLimit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeTestJSON(t, response, map[string]any{"ret": -2, "errcode": 0})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "wechat.json")
	if err := saveWeChatCredentials(path, wechatCredentials{BotToken: "secret", BaseURL: server.URL, BotID: "bot"}); err != nil {
		t.Fatal(err)
	}
	client, err := newWeChatClient(wechatClientOptions{CredentialsPath: path, HTTPClient: server.Client()}, true)
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendText(context.Background(), "user", "text", "context")
	if err == nil || !strings.Contains(err.Error(), "ret=-2") {
		t.Fatalf("error = %v", err)
	}
}

func TestWeChatClientClearsExpiredCredentials(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		writeTestJSON(t, response, map[string]any{"ret": 401, "errcode": 401, "errmsg": "expired"})
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "wechat.json")
	if err := saveWeChatCredentials(path, wechatCredentials{BotToken: "secret", BaseURL: server.URL, BotID: "bot"}); err != nil {
		t.Fatal(err)
	}
	client, err := newWeChatClient(wechatClientOptions{CredentialsPath: path, HTTPClient: server.Client()}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetUpdates(context.Background(), time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("error = %v", err)
	}
	if _, ok, loadErr := loadWeChatCredentials(path); loadErr != nil || ok {
		t.Fatalf("credentials after expiration: ok=%v err=%v", ok, loadErr)
	}
}

func TestWeChatControllerAuthorizationAndCommands(t *testing.T) {
	t.Parallel()
	fake := &wechatFakeAPI{contexts: map[string]string{}}
	var commands []remoteCommand
	controller := &wechatController{
		client:       fake,
		allowedUsers: map[string]struct{}{"allowed": {}},
		userIDs:      []string{"allowed"},
		execute: func(_ context.Context, command remoteCommand) (string, error) {
			commands = append(commands, command)
			return "command result", nil
		},
		notify: true,
		out:    make(chan wechatOutbound, 8),
	}

	unauthorized := wechatTextMessage("blocked", "ctx-blocked", "/pause")
	unauthorized.MessageType = 2
	if err := controller.handleMessage(context.Background(), unauthorized); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 || len(controller.out) != 0 {
		t.Fatal("non-user message was processed")
	}
	unauthorized.MessageType = 1
	if err := controller.handleMessage(context.Background(), unauthorized); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 0 || len(controller.out) != 0 {
		t.Fatal("unauthorized command was processed")
	}

	if err := controller.handleMessage(context.Background(), wechatTextMessage("blocked", "ctx-uid", "/uid")); err != nil {
		t.Fatal(err)
	}
	uidReply := <-controller.out
	if uidReply.UserID != "blocked" || !strings.Contains(uidReply.Text, "blocked") || uidReply.ContextToken != "ctx-uid" {
		t.Fatalf("uid reply = %#v", uidReply)
	}
	if _, ok := fake.contexts["blocked"]; ok {
		t.Fatal("unauthorized context token was persisted")
	}

	if err := controller.handleMessage(context.Background(), wechatTextMessage("allowed", "ctx-allowed", "/goal pause")); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Name != "goal" || commands[0].Text != "pause" {
		t.Fatalf("commands = %#v", commands)
	}
	reply := <-controller.out
	if reply.Text != "command result" || fake.contexts["allowed"] != "ctx-allowed" || len(fake.typing) != 1 {
		t.Fatalf("reply=%#v contexts=%#v typing=%#v", reply, fake.contexts, fake.typing)
	}

	controller.Notify("lifecycle event")
	notification := <-controller.out
	if notification.UserID != "allowed" || notification.ContextToken != "ctx-allowed" {
		t.Fatalf("notification = %#v", notification)
	}
}

func TestWeChatControllerPollingLifecycle(t *testing.T) {
	t.Parallel()
	fake := &wechatPollingFake{
		updates:  make(chan []wechatMessage, 1),
		sent:     make(chan wechatOutbound, 1),
		contexts: map[string]string{},
	}
	stateUpdates := make(chan error, 1)
	controller := &wechatController{
		client:       fake,
		allowedUsers: map[string]struct{}{"allowed": {}},
		userIDs:      []string{"allowed"},
		pollTimeout:  time.Second,
		execute: func(_ context.Context, command remoteCommand) (string, error) {
			if command.Name != "status" {
				t.Errorf("command = %#v", command)
			}
			return "live status", nil
		},
		onStateError: func(err error) {
			select {
			case stateUpdates <- err:
			default:
			}
		},
		notify: true,
		out:    make(chan wechatOutbound, 8),
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.updates <- []wechatMessage{wechatTextMessage("allowed", "ctx-live", "/status")}
	select {
	case sent := <-fake.sent:
		if sent.UserID != "allowed" || sent.Text != "live status" || sent.ContextToken != "ctx-live" {
			t.Fatalf("sent = %#v", sent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for WeChat reply")
	}
	select {
	case err := <-stateUpdates:
		if err != nil {
			t.Fatalf("state update error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("successful update was not reported")
	}
	controller.Stop()
}

func TestWeChatArgumentParsingAndMessageSplitting(t *testing.T) {
	t.Setenv("CODEXDOG_HOME", t.TempDir())
	t.Setenv("CODEXDOG_WECHAT_USER_IDS", "user-a,user-b,user-a")
	args, err := parseArguments([]string{
		"wechat", "status", "--wechat-user-id", "user-c", "--wechat-poll-timeout-sec", "12", "--wechat-login-timeout-sec", "30", "--wechat-no-browser", "--wechat-no-notify",
	})
	if err != nil {
		t.Fatal(err)
	}
	if args.Command != "wechat" || len(args.CommandArgs) != 1 || args.CommandArgs[0] != "status" {
		t.Fatalf("command = %#v", args)
	}
	if strings.Join(args.Options.WeChatAllowedUsers, ",") != "user-a,user-b,user-c" || args.Options.WeChatPollTimeout != 12*time.Second || args.Options.WeChatLoginTimeout != 30*time.Second || args.Options.WeChatOpenBrowser || args.Options.WeChatNotify {
		t.Fatalf("options = %#v", args.Options)
	}
	if !strings.HasSuffix(args.Options.WeChatCredentialsPath, ".json") || !strings.HasSuffix(args.Options.WeChatQRCodePath, ".png") {
		t.Fatalf("management paths = %#v", args.Options)
	}
	defaults, err := parseArguments([]string{"wechat", "status"})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Options.WeChatLoginTimeout != wechatDefaultLoginTimeout {
		t.Fatalf("default WeChat login timeout = %s, want %s", defaults.Options.WeChatLoginTimeout, wechatDefaultLoginTimeout)
	}
	if !defaults.Options.WeChatOpenBrowser {
		t.Fatal("default WeChat login should open the browser")
	}
	parts := splitWeChatMessage(strings.Repeat("界", wechatMessageLimit+1), wechatMessageLimit)
	if len(parts) != 2 || len([]rune(parts[0])) != wechatMessageLimit || parts[1] != "界" {
		t.Fatalf("parts = %#v", parts)
	}
}

func TestWeChatBrowserCommandsAndURLValidation(t *testing.T) {
	t.Parallel()
	value := "https://liteapp.weixin.qq.com/q/test?a=1&b=2"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "windows", name: "rundll32.exe", args: []string{"url.dll,FileProtocolHandler", value}},
		{goos: "darwin", name: "open", args: []string{value}},
		{goos: "linux", name: "xdg-open", args: []string{value}},
	}
	for _, test := range tests {
		commands := wechatBrowserCommands(test.goos, value)
		if len(commands) == 0 || commands[0].Name != test.name || strings.Join(commands[0].Args, "\x00") != strings.Join(test.args, "\x00") {
			t.Errorf("commands for %s = %#v", test.goos, commands)
		}
	}
	if commands := wechatBrowserCommands("plan9", value); len(commands) != 0 {
		t.Fatalf("unsupported commands = %#v", commands)
	}
	for _, invalid := range []string{"", "javascript:alert(1)", "http://liteapp.weixin.qq.com/q/test"} {
		if err := openWeChatLoginURL(invalid); err == nil {
			t.Errorf("openWeChatLoginURL(%q) succeeded", invalid)
		}
	}
}

func wechatTextMessage(userID, contextToken, text string) wechatMessage {
	return wechatMessage{
		FromUserID:   userID,
		ContextToken: contextToken,
		ItemList: []wechatMessageItem{{
			Type:     wechatMessageText,
			TextItem: &wechatTextItem{Text: text},
		}},
	}
}

func writeTestJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Error(err)
	}
}

type wechatFakeAPI struct {
	contexts map[string]string
	sent     []wechatOutbound
	typing   []wechatOutbound
}

func (f *wechatFakeAPI) GetUpdates(context.Context, time.Duration) ([]wechatMessage, error) {
	return nil, nil
}

func (f *wechatFakeAPI) SendText(_ context.Context, userID, text, contextToken string) error {
	f.sent = append(f.sent, wechatOutbound{UserID: userID, Text: text, ContextToken: contextToken})
	return nil
}

func (f *wechatFakeAPI) SendTyping(_ context.Context, userID, contextToken string) error {
	f.typing = append(f.typing, wechatOutbound{UserID: userID, ContextToken: contextToken})
	return nil
}

func (f *wechatFakeAPI) RecordContext(userID, contextToken string) error {
	f.contexts[userID] = contextToken
	return nil
}

func (f *wechatFakeAPI) Context(userID string) (string, bool) {
	token, ok := f.contexts[userID]
	return token, ok
}

type wechatPollingFake struct {
	mu       sync.Mutex
	updates  chan []wechatMessage
	sent     chan wechatOutbound
	contexts map[string]string
}

func (f *wechatPollingFake) GetUpdates(ctx context.Context, _ time.Duration) ([]wechatMessage, error) {
	select {
	case messages := <-f.updates:
		return messages, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *wechatPollingFake) SendText(_ context.Context, userID, text, contextToken string) error {
	f.sent <- wechatOutbound{UserID: userID, Text: text, ContextToken: contextToken}
	return nil
}

func (f *wechatPollingFake) SendTyping(context.Context, string, string) error { return nil }

func (f *wechatPollingFake) RecordContext(userID, contextToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contexts[userID] = contextToken
	return nil
}

func (f *wechatPollingFake) Context(userID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, ok := f.contexts[userID]
	return token, ok
}
