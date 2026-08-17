package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTelegramClientGetUpdatesAndSplitSendMessage(t *testing.T) {
	type sendRequest struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	var updatesRequest telegramGetUpdatesRequest
	var sends []sendRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/botsecret/getUpdates" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &updatesRequest); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":12,"message":{"message_id":3,"from":{"id":44},"chat":{"id":-55,"type":"private"},"text":"/status"}}]}`))
			return
		}
		if request.URL.Path == "/botsecret/sendMessage" {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			var sent sendRequest
			if err := json.Unmarshal(body, &sent); err != nil {
				t.Fatal(err)
			}
			sends = append(sends, sent)
			_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":99}}`))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := newTelegramClient(telegramClientOptions{
		Token:       "secret",
		BaseURL:     server.URL,
		HTTPClient:  server.Client(),
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := client.GetUpdates(context.Background(), 7, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].UpdateID != 12 || updates[0].Message == nil || updates[0].Message.Chat.ID != -55 {
		t.Fatalf("unexpected updates: %#v", updates)
	}
	if updatesRequest.Offset != 7 || updatesRequest.Timeout != 2 || len(updatesRequest.AllowedUpdates) != 1 || updatesRequest.AllowedUpdates[0] != "message" {
		t.Fatalf("unexpected getUpdates request: %#v", updatesRequest)
	}

	text := strings.Repeat("界", telegramMessageLimit) + "tail"
	if err := client.SendMessage(context.Background(), -55, text); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 2 || sends[0].ChatID != -55 || len([]rune(sends[0].Text)) != telegramMessageLimit || sends[1].Text != "tail" {
		t.Fatalf("unexpected split messages: %#v", sends)
	}
}

func TestTelegramClientRetries429AndDoesNotRetry400(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls++
		current := calls
		mu.Unlock()
		if current == 1 {
			writer.WriteHeader(http.StatusTooManyRequests)
			_, _ = writer.Write([]byte(`{"ok":false,"error_code":429,"description":"retry","parameters":{"retry_after":2}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()
	client, err := newTelegramClient(telegramClientOptions{Token: "token", BaseURL: server.URL, HTTPClient: server.Client(), MaxAttempts: 2, Backoff: []time.Duration{time.Hour}, MaxRetryAfter: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetUpdates(context.Background(), 0, 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if calls != 2 {
		t.Fatalf("429 was not retried: %d calls", calls)
	}
	mu.Unlock()

	permanent := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"ok":false,"error_code":400,"description":"bad request"}`))
	}))
	defer permanent.Close()
	client, err = newTelegramClient(telegramClientOptions{Token: "token", BaseURL: permanent.URL, HTTPClient: permanent.Client(), MaxAttempts: 3, Backoff: []time.Duration{time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = client.SendMessage(context.Background(), 1, "hello")
	var apiErr *telegramAPIError
	if !errors.As(err, &apiErr) || apiErr.Retryable {
		t.Fatalf("400 classification = %v", err)
	}
}

func TestTelegramClientRetryCancellationAndTokenRedaction(t *testing.T) {
	token := "123456:super-secret"
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("dial %s", request.URL.String())
	})
	oneAttempt, err := newTelegramClient(telegramClientOptions{
		Token:       token,
		BaseURL:     "https://telegram.invalid",
		HTTPClient:  &http.Client{Transport: transport},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneAttempt.GetUpdates(context.Background(), 0, 0)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("transport error was not safely redacted: %v", err)
	}

	client, err := newTelegramClient(telegramClientOptions{
		Token:       token,
		BaseURL:     "https://telegram.invalid",
		HTTPClient:  &http.Client{Transport: transport},
		MaxAttempts: 3,
		Backoff:     []time.Duration{time.Hour},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = client.GetUpdates(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retry cancellation error = %v", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked in cancellation error: %v", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = client.GetUpdates(ctx, 0, 0)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), token) {
		t.Fatalf("cancelled request = %v", err)
	}
}

func TestTelegramPollerOffsetDeduplicationAndHandlerErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &telegramFakeAPI{batches: [][]telegramUpdate{{
		{UpdateID: 1},
		{UpdateID: 2},
		{UpdateID: 2},
	}}}
	fake.onCall = func(call int, offset int64) {
		if call == 2 {
			cancel()
		}
	}
	var handled []int64
	var handlerErrors []int64
	poller, err := newTelegramPoller(fake, telegramPollerOptions{
		PollTimeout:    time.Millisecond,
		SeenLimit:      8,
		ErrorBackoff:   []time.Duration{time.Millisecond},
		OnHandlerError: func(update telegramUpdate, _ error) { handlerErrors = append(handlerErrors, update.UpdateID) },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = poller.Run(ctx, func(_ context.Context, update telegramUpdate) error {
		handled = append(handled, update.UpdateID)
		if update.UpdateID == 2 {
			return errors.New("handler failed")
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || fmt.Sprint(handled) != "[1 2]" || fmt.Sprint(handlerErrors) != "[2]" || poller.Offset() != 3 || len(fake.offsets) != 2 || fake.offsets[0] != 0 || fake.offsets[1] != 3 {
		t.Fatalf("poller result err=%v handled=%v handlerErrors=%v offset=%d", err, handled, handlerErrors, poller.Offset())
	}
}

func TestTelegramPollerRetriesTransientError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &telegramFakeAPI{errors: []error{&telegramAPIError{Method: "getUpdates", Retryable: true}, nil}, batches: [][]telegramUpdate{{{UpdateID: 4}}}}
	fake.onCall = func(call int, _ int64) {
		if call == 2 {
			cancel()
		}
	}
	poller, err := newTelegramPoller(fake, telegramPollerOptions{PollTimeout: time.Millisecond, ErrorBackoff: []time.Duration{time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = poller.Run(ctx, func(context.Context, telegramUpdate) error { return nil })
	if !errors.Is(err, context.Canceled) || len(fake.offsets) != 2 || fake.offsets[0] != 0 || fake.offsets[1] != 0 {
		t.Fatalf("transient poll retry err=%v offsets=%v", err, fake.offsets)
	}
}

func TestTelegramAllowlistAndCommandParsing(t *testing.T) {
	update := telegramUpdate{UpdateID: 1, Message: &telegramMessage{Chat: telegramChat{ID: -7}, From: &telegramUser{ID: 9}, Text: "/Prompt@CodexBot  inspect status"}}
	allowlist := newTelegramAllowlist([]int64{-7}, []int64{9})
	if !allowlist.Allows(update) {
		t.Fatal("matching chat/user was rejected")
	}
	if allowlist.Allows(telegramUpdate{Message: &telegramMessage{Chat: telegramChat{ID: -7}, From: &telegramUser{ID: 10}}}) {
		t.Fatal("non-matching user was accepted")
	}
	if newTelegramAllowlist(nil, nil).Allows(update) {
		t.Fatal("empty allowlist accepted an update")
	}
	command, ok := parseTelegramCommand(update.Message.Text)
	if !ok || command.Name != "prompt" || command.Mention != "CodexBot" || command.Args != "inspect status" {
		t.Fatalf("parsed command = %#v, ok=%t", command, ok)
	}
	if _, ok := parseTelegramCommand("plain text"); ok {
		t.Fatal("plain text parsed as command")
	}
}

func TestSplitTelegramMessageLimit(t *testing.T) {
	parts := splitTelegramMessage(strings.Repeat("😀", telegramMessageLimit+1), telegramMessageLimit)
	if len(parts) != 2 || len([]rune(parts[0])) != telegramMessageLimit || len([]rune(parts[1])) != 1 {
		t.Fatalf("unexpected unicode split: lengths=%d,%d", len([]rune(parts[0])), len([]rune(parts[1])))
	}
}

func TestTelegramOffsetPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "telegram.json")
	if err := saveTelegramOffset(path, 42); err != nil {
		t.Fatal(err)
	}
	offset, err := loadTelegramOffset(path)
	if err != nil || offset != 42 {
		t.Fatalf("offset=%d err=%v", offset, err)
	}
}

func TestTelegramControllerDispatchesOnlyAllowedCommands(t *testing.T) {
	api := &telegramControllerFakeAPI{sent: make(chan telegramOutbound, 4), updates: []telegramUpdate{
		{UpdateID: 1, Message: &telegramMessage{Chat: telegramChat{ID: 10}, From: &telegramUser{ID: 20}, Text: "/status"}},
		{UpdateID: 2, Message: &telegramMessage{Chat: telegramChat{ID: 99}, From: &telegramUser{ID: 20}, Text: "/pause"}},
	}}
	poller, err := newTelegramPoller(api, telegramPollerOptions{PollTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	executed := make(chan remoteCommand, 1)
	controller := &telegramController{
		client:    api,
		poller:    poller,
		allowlist: newTelegramAllowlist([]int64{10}, []int64{20}),
		chatIDs:   []int64{10},
		execute: func(_ context.Context, command remoteCommand) (string, error) {
			executed <- command
			return "status reply", nil
		},
		out: make(chan telegramOutbound, 8),
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer controller.Stop()
	select {
	case command := <-executed:
		if command.Name != "status" {
			t.Fatalf("command = %#v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("allowed command was not dispatched")
	}
	select {
	case sent := <-api.sent:
		if sent.ChatID != 10 || sent.Text != "status reply" {
			t.Fatalf("sent = %#v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("command reply was not sent")
	}
	select {
	case extra := <-executed:
		t.Fatalf("unauthorized command was dispatched: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type telegramFakeAPI struct {
	mu        sync.Mutex
	offsets   []int64
	batches   [][]telegramUpdate
	errors    []error
	onCall    func(int, int64)
	callCount int
}

func (f *telegramFakeAPI) GetUpdates(_ context.Context, offset int64, _ time.Duration) ([]telegramUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	call := f.callCount
	f.offsets = append(f.offsets, offset)
	if f.onCall != nil {
		f.onCall(call, offset)
	}
	if len(f.errors) > 0 {
		err := f.errors[0]
		f.errors = f.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(f.batches) == 0 {
		return nil, context.Canceled
	}
	batch := f.batches[0]
	f.batches = f.batches[1:]
	return batch, nil
}

func (f *telegramFakeAPI) SendMessage(context.Context, int64, string) error { return nil }

type telegramControllerFakeAPI struct {
	mu      sync.Mutex
	updates []telegramUpdate
	sent    chan telegramOutbound
}

func (f *telegramControllerFakeAPI) GetUpdates(ctx context.Context, _ int64, _ time.Duration) ([]telegramUpdate, error) {
	f.mu.Lock()
	updates := f.updates
	f.updates = nil
	f.mu.Unlock()
	if updates != nil {
		return updates, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *telegramControllerFakeAPI) SendMessage(_ context.Context, chatID int64, text string) error {
	f.sent <- telegramOutbound{ChatID: chatID, Text: text}
	return nil
}
