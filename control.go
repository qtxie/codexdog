package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const remoteNotificationLimit = 256

type remoteNotification struct {
	InstanceID string `json:"instanceId"`
	Sequence   uint64 `json:"sequence"`
	Timestamp  string `json:"timestamp"`
	Message    string `json:"message"`
}

type remoteNotificationBatch struct {
	InstanceID string               `json:"instanceId"`
	Events     []remoteNotification `json:"events"`
}

// notificationBroker retains a small per-supervisor event history so a local
// remote-control hub can reconnect without losing recent lifecycle messages.
type notificationBroker struct {
	mu         sync.Mutex
	instanceID string
	next       uint64
	events     []remoteNotification
	changed    chan struct{}
}

func newNotificationBroker() *notificationBroker {
	return &notificationBroker{instanceID: randomControlToken(16), changed: make(chan struct{})}
}

func (b *notificationBroker) Publish(message string) {
	message = strings.TrimSpace(sanitizeText(message))
	if b == nil || message == "" {
		return
	}
	b.mu.Lock()
	b.next++
	event := remoteNotification{
		InstanceID: b.instanceID,
		Sequence:   b.next,
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Message:    message,
	}
	b.events = append(b.events, event)
	if len(b.events) > remoteNotificationLimit {
		b.events = append([]remoteNotification(nil), b.events[len(b.events)-remoteNotificationLimit:]...)
	}
	close(b.changed)
	b.changed = make(chan struct{})
	b.mu.Unlock()
}

func (b *notificationBroker) Wait(ctx context.Context, instanceID string, after uint64) remoteNotificationBatch {
	if b == nil {
		return remoteNotificationBatch{}
	}
	for {
		b.mu.Lock()
		batch := remoteNotificationBatch{InstanceID: b.instanceID}
		if instanceID != "" && instanceID != b.instanceID {
			after = 0
		}
		for _, event := range b.events {
			if event.Sequence > after {
				batch.Events = append(batch.Events, event)
			}
		}
		changed := b.changed
		b.mu.Unlock()
		if len(batch.Events) > 0 || ctx.Err() != nil {
			return batch
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return batch
		}
	}
}

type controlServer struct {
	Port   int
	Token  string
	server *http.Server
}

type controlCommandResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

func startControlServer(state func() supervisorState, stop func()) (*controlServer, error) {
	return startControlServerWithActions(state, nil, stop)
}

func startControlServerWithActions(state func() supervisorState, action func(context.Context, remoteCommand) (string, error), stop func(), eventSources ...*notificationBroker) (*controlServer, error) {
	token := randomControlToken(32)
	if token == "" {
		return nil, errors.New("generate control token")
	}
	var events *notificationBroker
	if len(eventSources) > 0 {
		events = eventSources[0]
	}
	mux := http.NewServeMux()
	authorized := func(request *http.Request) bool {
		supplied := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if len(supplied) != len(token) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
	}
	mux.HandleFunc("/status", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(state())
	})
	mux.HandleFunc("/stop", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.WriteHeader(http.StatusAccepted)
		go stop()
	})
	mux.HandleFunc("/command", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if action == nil {
			writer.WriteHeader(http.StatusNotImplemented)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, 16*1024)
		var command remoteCommand
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			writeControlJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid command JSON"})
			return
		}
		ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
		defer cancel()
		message, err := action(ctx, command)
		if err != nil {
			writeControlJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": sanitizeText(err.Error())})
			return
		}
		writeControlJSON(writer, http.StatusOK, map[string]any{"ok": true, "message": message})
	})
	mux.HandleFunc("/events", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if events == nil {
			writer.WriteHeader(http.StatusNotImplemented)
			return
		}
		after := uint64(0)
		if raw := strings.TrimSpace(request.URL.Query().Get("after")); raw != "" {
			parsed, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				writeControlJSON(writer, http.StatusBadRequest, map[string]any{"error": "invalid event cursor"})
				return
			}
			after = parsed
		}
		timeout := 25 * time.Second
		if raw := strings.TrimSpace(request.URL.Query().Get("timeout")); raw != "" {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds < 0 || seconds > 30 {
				writeControlJSON(writer, http.StatusBadRequest, map[string]any{"error": "event timeout must be between 0 and 30 seconds"})
				return
			}
			timeout = time.Duration(seconds) * time.Second
		}
		ctx, cancel := context.WithTimeout(request.Context(), timeout)
		defer cancel()
		writeControlJSON(writer, http.StatusOK, events.Wait(ctx, request.URL.Query().Get("instance"), after))
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return &controlServer{Port: listener.Addr().(*net.TCPAddr).Port, Token: token, server: server}, nil
}

func randomControlToken(size int) string {
	if size <= 0 {
		return ""
	}
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func writeControlJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func (c *controlServer) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.server.Shutdown(ctx)
}

func queryControl(state supervisorState) bool {
	_, ok := queryControlState(state)
	return ok
}

func queryControlState(state supervisorState) (supervisorState, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return queryControlStateContext(ctx, state)
}

func queryControlStateContext(ctx context.Context, state supervisorState) (supervisorState, bool) {
	if state.ControlPort == 0 || state.ControlToken == "" {
		return supervisorState{}, false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/status", state.ControlPort), nil)
	if err != nil {
		return supervisorState{}, false
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return supervisorState{}, false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return supervisorState{}, false
	}
	var live supervisorState
	if err := json.NewDecoder(response.Body).Decode(&live); err != nil {
		return supervisorState{}, false
	}
	return live, true
}

func requestStop(state supervisorState) bool {
	return controlRequest(state, http.MethodPost, "/stop", 2*time.Second)
}

func controlRequest(state supervisorState, method, path string, timeout time.Duration) bool {
	if state.ControlPort == 0 || state.ControlToken == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", state.ControlPort, path), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode <= 299
}

func requestControlCommand(state supervisorState, command remoteCommand) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return requestControlCommandContext(ctx, state, command)
}

func requestControlCommandContext(ctx context.Context, state supervisorState, command remoteCommand) (string, error) {
	if state.ControlPort == 0 || state.ControlToken == "" {
		return "", errors.New("the Codexdog control server is unavailable")
	}
	body, err := json.Marshal(command)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/command", state.ControlPort), strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	var result controlCommandResponse
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 || !result.OK {
		if result.Error != "" {
			return "", errors.New(result.Error)
		}
		return "", fmt.Errorf("control command failed with HTTP %d", response.StatusCode)
	}
	return result.Message, nil
}

func requestControlEvents(ctx context.Context, state supervisorState, instanceID string, after uint64) (remoteNotificationBatch, error) {
	if state.ControlPort == 0 || state.ControlToken == "" {
		return remoteNotificationBatch{}, errors.New("the Codexdog control server is unavailable")
	}
	query := url.Values{}
	query.Set("instance", instanceID)
	query.Set("after", strconv.FormatUint(after, 10))
	query.Set("timeout", "25")
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/events?%s", state.ControlPort, query.Encode())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return remoteNotificationBatch{}, err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return remoteNotificationBatch{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return remoteNotificationBatch{}, fmt.Errorf("control events failed with HTTP %d", response.StatusCode)
	}
	var batch remoteNotificationBatch
	if err := json.NewDecoder(response.Body).Decode(&batch); err != nil {
		return remoteNotificationBatch{}, err
	}
	return batch, nil
}
