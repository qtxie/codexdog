package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	telegramHubStateVersion = 1
	telegramHubCommandLimit = 64
	telegramHubStartTimeout = 25 * time.Second
)

var (
	errTelegramHubAlreadyRunning = errors.New("the Telegram hub is already running")
	errTelegramHubNotAccepting   = errors.New("the Telegram hub is not accepting new sessions")
)

type telegramHubSession struct {
	Alias        string `json:"alias"`
	CWD          string `json:"cwd"`
	RegisteredAt string `json:"registeredAt"`
}

type telegramHubWatch struct {
	All      bool     `json:"all"`
	Aliases  []string `json:"aliases,omitempty"`
	Excluded []string `json:"excluded,omitempty"`
}

type telegramHubCursor struct {
	InstanceID string `json:"instanceId,omitempty"`
	Sequence   uint64 `json:"sequence,omitempty"`
}

type telegramHubState struct {
	Version              int                           `json:"version"`
	PID                  int                           `json:"pid"`
	InstanceID           string                        `json:"instanceId"`
	Phase                string                        `json:"phase"`
	ControlPort          int                           `json:"controlPort,omitempty"`
	ControlToken         string                        `json:"controlToken,omitempty"`
	ConfigFingerprint    string                        `json:"configFingerprint,omitempty"`
	Sessions             map[string]telegramHubSession `json:"sessions,omitempty"`
	Selections           map[string]string             `json:"selections,omitempty"`
	Watches              map[string]telegramHubWatch   `json:"watches,omitempty"`
	EventCursors         map[string]telegramHubCursor  `json:"eventCursors,omitempty"`
	TelegramLastError    string                        `json:"telegramLastError,omitempty"`
	TelegramLastUpdateAt string                        `json:"telegramLastUpdateAt,omitempty"`
	UpdatedAt            string                        `json:"updatedAt"`
	StoppedReason        string                        `json:"stoppedReason,omitempty"`
}

type publicTelegramHubState struct {
	telegramHubState
	Live bool `json:"live"`
}

func publicTelegramHubStatus(state telegramHubState, live bool) publicTelegramHubState {
	state.ControlToken = ""
	return publicTelegramHubState{telegramHubState: state, Live: live}
}

type telegramHubStateStore struct {
	Path     string
	LockPath string
	LogPath  string
	mu       sync.Mutex
}

func newTelegramHubStateStore(root string) *telegramHubStateStore {
	return &telegramHubStateStore{
		Path:     filepath.Join(root, "telegram-hub.json"),
		LockPath: filepath.Join(root, "telegram-hub.lock"),
		LogPath:  filepath.Join(root, "telegram-hub.log"),
	}
}

func (s *telegramHubStateStore) Read() (telegramHubState, bool) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return telegramHubState{}, false
	}
	var state telegramHubState
	if err := json.Unmarshal(data, &state); err != nil {
		return telegramHubState{}, false
	}
	return state, true
}

func (s *telegramHubStateStore) Write(state telegramHubState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := s.Path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, s.Path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

type telegramHubLock struct {
	path  string
	value string
}

func acquireTelegramHubLock(store *telegramHubStateStore) (*telegramHubLock, error) {
	if err := os.MkdirAll(filepath.Dir(store.LockPath), 0o700); err != nil {
		return nil, err
	}
	value := fmt.Sprintf("%d:%s", os.Getpid(), randomControlToken(12))
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.OpenFile(store.LockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			if _, writeErr := file.WriteString(value); writeErr != nil {
				_ = file.Close()
				_ = os.Remove(store.LockPath)
				return nil, writeErr
			}
			if closeErr := file.Close(); closeErr != nil {
				_ = os.Remove(store.LockPath)
				return nil, closeErr
			}
			return &telegramHubLock{path: store.LockPath, value: value}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if _, live := queryTelegramHubState(store); live {
			return nil, errTelegramHubAlreadyRunning
		}
		data, _ := os.ReadFile(store.LockPath)
		pidText := strings.SplitN(string(data), ":", 2)[0]
		pid, _ := strconv.Atoi(pidText)
		if pid > 0 && processExists(pid) {
			return nil, fmt.Errorf("%w (process %d is starting)", errTelegramHubAlreadyRunning, pid)
		}
		_ = os.Remove(store.LockPath)
	}
	return nil, errors.New("could not acquire the Telegram hub lock")
}

func (l *telegramHubLock) Release() {
	if l == nil {
		return
	}
	data, err := os.ReadFile(l.path)
	if err == nil && string(data) == l.value {
		_ = os.Remove(l.path)
	}
}

type telegramHubControl struct {
	Port   int
	Token  string
	server *http.Server
}

type telegramHubRegistrationRequest struct {
	Alias string `json:"alias"`
	CWD   string `json:"cwd,omitempty"`
}

type telegramHub struct {
	options supervisorOptions
	store   *telegramHubStateStore
	logger  *fileLogger

	mu        sync.Mutex
	persistMu sync.Mutex
	state     telegramHubState
	ctx       context.Context
	cancel    context.CancelFunc
	control   *telegramHubControl
	telegram  *telegramController
	watchers  map[string]context.CancelFunc
	workers   map[string]chan telegramHubCommandJob
	wg        sync.WaitGroup
}

type telegramHubCommandJob struct {
	Alias   string
	Command remoteCommand
	Reply   func(string)
}

func newTelegramHub(options supervisorOptions, root string) (*telegramHub, error) {
	if err := validateTelegramHubOptions(options); err != nil {
		return nil, err
	}
	store := newTelegramHubStateStore(root)
	state, _ := store.Read()
	state.Version = telegramHubStateVersion
	state.PID = os.Getpid()
	state.InstanceID = randomControlToken(16)
	state.Phase = "starting"
	state.ControlPort = 0
	state.ControlToken = ""
	state.ConfigFingerprint = telegramHubConfigFingerprint(options)
	state.TelegramLastError = ""
	state.StoppedReason = ""
	state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	initializeTelegramHubState(&state)
	options.TelegramStatePath = filepath.Join(root, "telegram-hub-offset.json")
	hub := &telegramHub{
		options:  options,
		store:    store,
		logger:   newLogger(store.LogPath),
		state:    state,
		watchers: map[string]context.CancelFunc{},
		workers:  map[string]chan telegramHubCommandJob{},
	}
	controller, err := newTelegramController(options, nil, hub.logger, hub.recordTelegramState)
	if err != nil {
		return nil, err
	}
	controller.dispatch = hub.dispatchTelegramCommand
	hub.telegram = controller
	return hub, nil
}

func initializeTelegramHubState(state *telegramHubState) {
	if state.Sessions == nil {
		state.Sessions = map[string]telegramHubSession{}
	}
	if state.Selections == nil {
		state.Selections = map[string]string{}
	}
	if state.Watches == nil {
		state.Watches = map[string]telegramHubWatch{}
	}
	if state.EventCursors == nil {
		state.EventCursors = map[string]telegramHubCursor{}
	}
}

func (h *telegramHub) Run() error {
	lock, err := acquireTelegramHubLock(h.store)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := h.logger.Initialize(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	if err := h.telegram.acquirePollLock(); err != nil {
		h.mu.Lock()
		h.state.Phase = "stopped"
		h.state.ControlPort = 0
		h.state.ControlToken = ""
		h.state.TelegramLastError = sanitizeText(err.Error())
		h.state.StoppedReason = "Telegram startup failed"
		h.mu.Unlock()
		_ = h.persist()
		return err
	}
	if err := h.preflightTelegram(); err != nil {
		h.telegram.releasePollLock()
		h.mu.Lock()
		h.state.Phase = "stopped"
		h.state.ControlPort = 0
		h.state.ControlToken = ""
		h.state.TelegramLastError = sanitizeText(err.Error())
		h.state.StoppedReason = "Telegram startup failed"
		h.mu.Unlock()
		_ = h.persist()
		return err
	}
	control, err := startTelegramHubControl(h)
	if err != nil {
		h.telegram.releasePollLock()
		return err
	}
	h.control = control
	h.mu.Lock()
	h.state.ControlPort = control.Port
	h.state.ControlToken = control.Token
	h.state.Phase = "running"
	h.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	h.mu.Unlock()
	if err := h.persist(); err != nil {
		_ = control.Close()
		h.telegram.releasePollLock()
		return err
	}
	if err := h.telegram.Start(h.ctx); err != nil {
		_ = control.Close()
		h.telegram.releasePollLock()
		h.mu.Lock()
		h.state.Phase = "stopped"
		h.state.ControlPort = 0
		h.state.ControlToken = ""
		h.state.TelegramLastError = sanitizeText(err.Error())
		h.state.StoppedReason = "Telegram startup failed"
		h.mu.Unlock()
		_ = h.persist()
		return err
	}
	h.syncSessionWatchers()
	h.logger.Log("Telegram multi-session hub started")
	h.telegram.Notify("Codexdog Telegram hub is online.")

	signals := make(chan os.Signal, 2)
	allSignals := append([]os.Signal{os.Interrupt}, terminationSignals()...)
	signal.Notify(signals, allSignals...)
	select {
	case received := <-signals:
		h.mu.Lock()
		h.state.StoppedReason = received.String()
		h.mu.Unlock()
	case <-h.ctx.Done():
	}
	signal.Stop(signals)

	h.mu.Lock()
	if h.state.Phase == "running" {
		h.state.Phase = "stopping"
	}
	stopReason := strings.TrimSpace(h.state.StoppedReason)
	h.mu.Unlock()
	_ = h.persist()
	if stopReason == "" {
		h.logger.Log("Telegram multi-session hub stopping")
	} else {
		h.logger.Log("Telegram multi-session hub stopping: " + sanitizeText(stopReason))
	}
	h.cancel()
	h.telegram.SendFinal(telegramHubStopNotification(h.snapshot()))
	h.telegram.Stop()
	_ = control.Close()
	h.mu.Lock()
	for _, cancel := range h.watchers {
		cancel()
	}
	h.watchers = map[string]context.CancelFunc{}
	h.mu.Unlock()
	h.wg.Wait()
	h.mu.Lock()
	h.state.Phase = "stopped"
	h.state.ControlPort = 0
	h.state.ControlToken = ""
	h.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	h.mu.Unlock()
	_ = h.persist()
	h.logger.Log("Telegram multi-session hub stopped")
	return nil
}

func telegramHubStopNotification(state telegramHubState) string {
	reason := strings.TrimSpace(state.StoppedReason)
	if reason == "" || strings.EqualFold(reason, "stop requested") {
		return "Codexdog Telegram hub is stopping."
	}
	return "Codexdog Telegram hub is stopping: " + sanitizeText(reason) + "."
}

func (h *telegramHub) preflightTelegram() error {
	ctx, cancel := context.WithTimeout(h.ctx, 20*time.Second)
	defer cancel()
	if h.telegram.metadata != nil {
		if err := h.telegram.metadata.DeleteWebhook(ctx); err != nil {
			return fmt.Errorf("delete Telegram webhook: %w", err)
		}
		if _, err := h.telegram.metadata.GetMe(ctx); err != nil {
			return fmt.Errorf("Telegram bot authentication: %w", err)
		}
	}
	if _, err := h.telegram.client.GetUpdates(ctx, h.telegram.poller.Offset(), 0); err != nil {
		if isTelegramPollingConflict(err) {
			h.recordTelegramState(fmt.Errorf("Telegram getUpdates preflight: %w", err))
			return nil
		}
		return fmt.Errorf("Telegram getUpdates preflight: %w", err)
	}
	return nil
}

func isTelegramPollingConflict(err error) bool {
	var apiErr *telegramAPIError
	return errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusConflict || apiErr.ErrorCode == http.StatusConflict)
}

func (h *telegramHub) snapshot() telegramHubState {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.state
	state.Sessions = make(map[string]telegramHubSession, len(h.state.Sessions))
	for alias, session := range h.state.Sessions {
		state.Sessions[alias] = session
	}
	state.Selections = make(map[string]string, len(h.state.Selections))
	for actor, alias := range h.state.Selections {
		state.Selections[actor] = alias
	}
	state.Watches = make(map[string]telegramHubWatch, len(h.state.Watches))
	for chat, watch := range h.state.Watches {
		watch.Aliases = append([]string(nil), watch.Aliases...)
		watch.Excluded = append([]string(nil), watch.Excluded...)
		state.Watches[chat] = watch
	}
	state.EventCursors = make(map[string]telegramHubCursor, len(h.state.EventCursors))
	for alias, cursor := range h.state.EventCursors {
		state.EventCursors[alias] = cursor
	}
	return state
}

func (h *telegramHub) persist() error {
	h.persistMu.Lock()
	defer h.persistMu.Unlock()
	h.mu.Lock()
	h.state.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	h.mu.Unlock()
	return h.store.Write(h.snapshot())
}

func (h *telegramHub) recordTelegramState(err error) {
	h.mu.Lock()
	h.state.TelegramLastUpdateAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err == nil {
		h.state.TelegramLastError = ""
	} else {
		h.state.TelegramLastError = sanitizeText(err.Error())
	}
	h.mu.Unlock()
	_ = h.persist()
}

func startTelegramHubControl(h *telegramHub) (*telegramHubControl, error) {
	token := randomControlToken(32)
	if token == "" {
		return nil, errors.New("generate Telegram hub control token")
	}
	authorized := func(request *http.Request) bool {
		supplied := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		return len(supplied) == len(token) && subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) == 1
	}
	decodeRegistration := func(writer http.ResponseWriter, request *http.Request) (telegramHubRegistrationRequest, bool) {
		request.Body = http.MaxBytesReader(writer, request.Body, 16*1024)
		var value telegramHubRegistrationRequest
		if err := json.NewDecoder(request.Body).Decode(&value); err != nil {
			writeControlJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid registration JSON"})
			return value, false
		}
		return value, true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writeControlJSON(writer, http.StatusOK, h.snapshot())
	})
	mux.HandleFunc("/register", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		value, ok := decodeRegistration(writer, request)
		if !ok {
			return
		}
		if err := h.registerRunningSession(value.Alias, value.CWD); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errTelegramHubNotAccepting) {
				status = http.StatusServiceUnavailable
			}
			writeControlJSON(writer, status, map[string]any{"ok": false, "error": sanitizeText(err.Error())})
			return
		}
		writeControlJSON(writer, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/unregister", func(writer http.ResponseWriter, request *http.Request) {
		if !authorized(request) {
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		value, ok := decodeRegistration(writer, request)
		if !ok {
			return
		}
		if err := h.unregisterSession(value.Alias, value.CWD); err != nil {
			writeControlJSON(writer, http.StatusConflict, map[string]any{"ok": false, "error": sanitizeText(err.Error())})
			return
		}
		writeControlJSON(writer, http.StatusOK, map[string]any{"ok": true})
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
		h.mu.Lock()
		if h.state.Phase == "running" {
			h.state.Phase = "stopping"
			h.state.StoppedReason = "stop requested"
		}
		h.mu.Unlock()
		_ = h.persist()
		writeControlJSON(writer, http.StatusAccepted, map[string]any{"ok": true})
		if h.cancel != nil {
			h.cancel()
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	return &telegramHubControl{Port: listener.Addr().(*net.TCPAddr).Port, Token: token, server: server}, nil
}

func (c *telegramHubControl) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.server.Shutdown(ctx)
}

func normalizeTelegramAlias(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 32 {
		return "", errors.New("Telegram alias must contain 1 to 32 characters")
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9' && index > 0) || (character == '-' || character == '_') && index > 0 {
			continue
		}
		return "", errors.New("Telegram alias must start with a letter and contain only lowercase letters, digits, '-' or '_'")
	}
	return value, nil
}

func (h *telegramHub) registerSession(alias, cwd string) error {
	return h.registerSessionWithPhase(alias, cwd, false)
}

func (h *telegramHub) registerRunningSession(alias, cwd string) error {
	return h.registerSessionWithPhase(alias, cwd, true)
}

func (h *telegramHub) registerSessionWithPhase(alias, cwd string, requireRunning bool) error {
	alias, err := normalizeTelegramAlias(alias)
	if err != nil {
		return err
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return err
	}
	h.mu.Lock()
	if requireRunning && h.state.Phase != "running" {
		h.mu.Unlock()
		return errTelegramHubNotAccepting
	}
	if existing, ok := h.state.Sessions[alias]; ok && workspaceKey(existing.CWD) != workspaceKey(cwd) {
		h.mu.Unlock()
		return fmt.Errorf("Telegram alias %q is already registered for %s", alias, existing.CWD)
	}
	for otherAlias, session := range h.state.Sessions {
		if otherAlias != alias && workspaceKey(session.CWD) == workspaceKey(cwd) {
			h.mu.Unlock()
			return fmt.Errorf("workspace %s is already registered as %q", cwd, otherAlias)
		}
	}
	h.state.Sessions[alias] = telegramHubSession{Alias: alias, CWD: cwd, RegisteredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	h.mu.Unlock()
	if err := h.persist(); err != nil {
		return err
	}
	h.syncSessionWatchers()
	h.logger.Log(fmt.Sprintf("Registered Telegram session %s for %s", alias, cwd))
	return nil
}

func (h *telegramHub) unregisterSession(alias, cwd string) error {
	alias, err := normalizeTelegramAlias(alias)
	if err != nil {
		return err
	}
	h.mu.Lock()
	session, ok := h.state.Sessions[alias]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	if strings.TrimSpace(cwd) != "" && workspaceKey(session.CWD) != workspaceKey(cwd) {
		h.mu.Unlock()
		return fmt.Errorf("Telegram alias %q is registered for a different workspace", alias)
	}
	delete(h.state.Sessions, alias)
	delete(h.state.EventCursors, alias)
	for actor, selected := range h.state.Selections {
		if selected == alias {
			delete(h.state.Selections, actor)
		}
	}
	for chat, watch := range h.state.Watches {
		watch.Aliases = removeString(watch.Aliases, alias)
		watch.Excluded = removeString(watch.Excluded, alias)
		h.state.Watches[chat] = watch
	}
	if cancel := h.watchers[alias]; cancel != nil {
		cancel()
		delete(h.watchers, alias)
	}
	h.mu.Unlock()
	if err := h.persist(); err != nil {
		return err
	}
	h.logger.Log(fmt.Sprintf("Unregistered Telegram session %s", alias))
	return nil
}

func removeString(values []string, removed string) []string {
	result := values[:0]
	for _, value := range values {
		if value != removed {
			result = append(result, value)
		}
	}
	return result
}

func (h *telegramHub) syncSessionWatchers() {
	h.mu.Lock()
	if h.ctx == nil || h.ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	for alias, session := range h.state.Sessions {
		if _, exists := h.watchers[alias]; exists {
			continue
		}
		ctx, cancel := context.WithCancel(h.ctx)
		h.watchers[alias] = cancel
		h.wg.Add(1)
		go func(alias, cwd string) {
			defer h.wg.Done()
			h.followSession(ctx, alias, cwd)
		}(alias, session.CWD)
	}
	h.mu.Unlock()
}

func (h *telegramHub) followSession(ctx context.Context, alias, cwd string) {
	store := newStateStore(filepath.Dir(h.store.Path), cwd)
	h.mu.Lock()
	cursor := h.state.EventCursors[alias]
	h.mu.Unlock()
	wasLive := false
	offlineFailures := 0
	for ctx.Err() == nil {
		persisted, ok := store.Read()
		if !ok {
			offlineFailures++
			wasLive = h.handleSessionOffline(alias, wasLive, offlineFailures)
			if !waitTelegramContext(ctx, time.Second) {
				return
			}
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		liveState, live := queryControlStateContext(probeCtx, persisted)
		cancel()
		if !live {
			offlineFailures++
			wasLive = h.handleSessionOffline(alias, wasLive, offlineFailures)
			if !waitTelegramContext(ctx, time.Second) {
				return
			}
			continue
		}
		offlineFailures = 0
		if !wasLive {
			h.notifySession(alias, "Session is online.")
			wasLive = true
		}
		eventCtx, eventCancel := context.WithTimeout(ctx, 35*time.Second)
		batch, err := requestControlEvents(eventCtx, liveState, cursor.InstanceID, cursor.Sequence)
		eventCancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !waitTelegramContext(ctx, time.Second) {
				return
			}
			continue
		}
		cursorChanged := false
		if batch.InstanceID != "" && batch.InstanceID != cursor.InstanceID {
			cursor = telegramHubCursor{InstanceID: batch.InstanceID}
			cursorChanged = true
		}
		for _, event := range batch.Events {
			if event.InstanceID != cursor.InstanceID || event.Sequence <= cursor.Sequence {
				continue
			}
			h.notifySession(alias, event.Message)
			cursor.Sequence = event.Sequence
			cursorChanged = true
		}
		if cursorChanged {
			h.mu.Lock()
			if _, exists := h.state.Sessions[alias]; exists {
				h.state.EventCursors[alias] = cursor
			}
			h.mu.Unlock()
			_ = h.persist()
		}
	}
}

func (h *telegramHub) handleSessionOffline(alias string, wasLive bool, failures int) bool {
	if wasLive && failures >= 3 {
		h.notifySession(alias, "Session is offline.")
		return false
	}
	return wasLive
}

func (h *telegramHub) notifySession(alias, message string) {
	if !h.options.TelegramNotify || strings.TrimSpace(message) == "" {
		return
	}
	h.mu.Lock()
	watches := make(map[int64]telegramHubWatch, len(h.options.TelegramAllowedChats))
	for _, chatID := range h.options.TelegramAllowedChats {
		watch, exists := h.state.Watches[strconv.FormatInt(chatID, 10)]
		if !exists {
			watch = telegramHubWatch{All: true}
		}
		watch.Aliases = append([]string(nil), watch.Aliases...)
		watch.Excluded = append([]string(nil), watch.Excluded...)
		watches[chatID] = watch
	}
	h.mu.Unlock()
	text := "[" + alias + "] " + strings.TrimSpace(message)
	for chatID, watch := range watches {
		if (watch.All && !containsString(watch.Excluded, alias)) || (!watch.All && containsString(watch.Aliases, alias)) {
			h.telegram.enqueueReply(chatID, text)
		}
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (h *telegramHub) dispatchTelegramCommand(_ context.Context, actor telegramActor, command telegramCommand, reply func(string)) error {
	switch command.Name {
	case "help":
		reply(telegramHubHelpText)
		return nil
	case "sessions":
		go func() { reply(h.formatSessions(actor)) }()
		return nil
	case "use":
		message, err := h.useSession(actor, command.Args)
		if err != nil {
			return err
		}
		reply(message)
		return nil
	case "watch":
		message, err := h.updateWatch(actor.ChatID, command.Args, false)
		if err != nil {
			return err
		}
		reply(message)
		return nil
	case "unwatch":
		message, err := h.updateWatch(actor.ChatID, command.Args, true)
		if err != nil {
			return err
		}
		reply(message)
		return nil
	}

	alias := ""
	routed := command
	if command.Name == "at" {
		var err error
		alias, routed, err = parseTelegramAtCommand(command.Args)
		if err != nil {
			return err
		}
	} else if command.Name == "stop" {
		fields := strings.Fields(command.Args)
		if len(fields) != 2 || !strings.EqualFold(fields[1], "confirm") {
			return errors.New("stopping a session requires /stop ALIAS confirm")
		}
		var err error
		alias, err = normalizeTelegramAlias(fields[0])
		if err != nil {
			return err
		}
		routed.Args = "confirm"
	} else {
		var err error
		alias, err = h.selectedSession(actor)
		if err != nil {
			return err
		}
	}
	remote, ok := telegramCommandToRemote(routed)
	if !ok {
		return errors.New("unknown command; use /help")
	}
	if remote.Name == "stop" && !remote.Confirm {
		return errors.New("stopping a session requires /stop ALIAS confirm")
	}
	return h.enqueueSessionCommand(telegramHubCommandJob{Alias: alias, Command: remote, Reply: reply})
}

func parseTelegramAtCommand(value string) (string, telegramCommand, error) {
	fields := strings.Fields(value)
	if len(fields) < 2 {
		return "", telegramCommand{}, errors.New("usage: /at ALIAS COMMAND [ARGS]")
	}
	alias, err := normalizeTelegramAlias(fields[0])
	if err != nil {
		return "", telegramCommand{}, err
	}
	name := strings.TrimPrefix(strings.ToLower(fields[1]), "/")
	args := ""
	if len(fields) > 2 {
		args = strings.Join(fields[2:], " ")
	}
	return alias, telegramCommand{Name: name, Args: args}, nil
}

func (h *telegramHub) enqueueSessionCommand(job telegramHubCommandJob) error {
	h.mu.Lock()
	if _, exists := h.state.Sessions[job.Alias]; !exists {
		h.mu.Unlock()
		return fmt.Errorf("unknown session %q; use /sessions", job.Alias)
	}
	queue := h.workers[job.Alias]
	if queue == nil {
		queue = make(chan telegramHubCommandJob, telegramHubCommandLimit)
		h.workers[job.Alias] = queue
		h.wg.Add(1)
		go func(alias string, jobs <-chan telegramHubCommandJob) {
			defer h.wg.Done()
			h.sessionCommandWorker(alias, jobs)
		}(job.Alias, queue)
	}
	h.mu.Unlock()
	select {
	case queue <- job:
		return nil
	default:
		return fmt.Errorf("session %q command queue is full", job.Alias)
	}
}

func (h *telegramHub) sessionCommandWorker(_ string, jobs <-chan telegramHubCommandJob) {
	for {
		select {
		case job := <-jobs:
			ctx, cancel := context.WithTimeout(h.ctx, 2*time.Minute)
			message, err := h.executeSessionCommand(ctx, job.Alias, job.Command)
			cancel()
			if err != nil {
				job.Reply("[" + job.Alias + "] Error: " + sanitizeText(err.Error()))
			} else {
				job.Reply("[" + job.Alias + "]\n" + message)
			}
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *telegramHub) executeSessionCommand(ctx context.Context, alias string, command remoteCommand) (string, error) {
	h.mu.Lock()
	session, exists := h.state.Sessions[alias]
	h.mu.Unlock()
	if !exists {
		return "", fmt.Errorf("session %q is no longer registered", alias)
	}
	store := newStateStore(filepath.Dir(h.store.Path), session.CWD)
	persisted, ok := store.Read()
	if !ok {
		return "", errors.New("the session has not started yet")
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	live, ok := queryControlStateContext(probeCtx, persisted)
	cancel()
	if !ok {
		return "", errors.New("the session is offline")
	}
	return requestControlCommandContext(ctx, live, command)
}

func telegramActorKey(actor telegramActor) string {
	return strconv.FormatInt(actor.ChatID, 10) + ":" + strconv.FormatInt(actor.UserID, 10)
}

func (h *telegramHub) selectedSession(actor telegramActor) (string, error) {
	key := telegramActorKey(actor)
	h.mu.Lock()
	if selected := h.state.Selections[key]; selected != "" {
		if _, exists := h.state.Sessions[selected]; exists {
			h.mu.Unlock()
			return selected, nil
		}
		delete(h.state.Selections, key)
	}
	if len(h.state.Sessions) == 1 {
		for alias := range h.state.Sessions {
			h.state.Selections[key] = alias
			h.mu.Unlock()
			_ = h.persist()
			return alias, nil
		}
	}
	h.mu.Unlock()
	return "", errors.New("no session selected; use /sessions and /use ALIAS")
}

func (h *telegramHub) useSession(actor telegramActor, value string) (string, error) {
	alias, err := normalizeTelegramAlias(value)
	if err != nil {
		return "", errors.New("usage: /use ALIAS")
	}
	h.mu.Lock()
	if _, exists := h.state.Sessions[alias]; !exists {
		h.mu.Unlock()
		return "", fmt.Errorf("unknown session %q; use /sessions", alias)
	}
	h.state.Selections[telegramActorKey(actor)] = alias
	h.mu.Unlock()
	if err := h.persist(); err != nil {
		return "", err
	}
	return "[" + alias + "] Selected for commands.", nil
}

func (h *telegramHub) formatSessions(actor telegramActor) string {
	h.mu.Lock()
	sessions := make([]telegramHubSession, 0, len(h.state.Sessions))
	for _, session := range h.state.Sessions {
		sessions = append(sessions, session)
	}
	selected := h.state.Selections[telegramActorKey(actor)]
	h.mu.Unlock()
	if len(sessions) == 0 {
		return "No Codex sessions are registered."
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Alias < sessions[j].Alias })
	type result struct {
		index int
		line  string
	}
	results := make(chan result, len(sessions))
	for index, session := range sessions {
		go func(index int, session telegramHubSession) {
			phase := "offline"
			if persisted, ok := newStateStore(filepath.Dir(h.store.Path), session.CWD).Read(); ok {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if live, ok := queryControlStateContext(ctx, persisted); ok {
					phase = valueOrDash(live.Phase)
				}
				cancel()
			}
			marker := " "
			if session.Alias == selected {
				marker = "*"
			}
			results <- result{index: index, line: fmt.Sprintf("%s %s: %s (%s)", marker, session.Alias, phase, session.CWD)}
		}(index, session)
	}
	lines := make([]string, len(sessions))
	for range sessions {
		value := <-results
		lines[value.index] = value.line
	}
	return "Codex sessions:\n" + strings.Join(lines, "\n")
}

func (h *telegramHub) updateWatch(chatID int64, value string, remove bool) (string, error) {
	chatKey := strconv.FormatInt(chatID, 10)
	fields := strings.Fields(strings.ToLower(value))
	h.mu.Lock()
	current, exists := h.state.Watches[chatKey]
	if !exists {
		current = telegramHubWatch{All: true}
	}
	if len(fields) == 0 {
		h.mu.Unlock()
		return formatTelegramWatch(current), nil
	}
	if len(fields) == 1 && fields[0] == "all" {
		if remove {
			current = telegramHubWatch{}
		} else {
			current = telegramHubWatch{All: true}
		}
		h.state.Watches[chatKey] = current
		h.mu.Unlock()
		if err := h.persist(); err != nil {
			return "", err
		}
		return formatTelegramWatch(current), nil
	}
	aliases := make([]string, 0, len(fields))
	for _, field := range fields {
		alias, err := normalizeTelegramAlias(field)
		if err != nil {
			h.mu.Unlock()
			return "", err
		}
		if _, ok := h.state.Sessions[alias]; !ok {
			h.mu.Unlock()
			return "", fmt.Errorf("unknown session %q", alias)
		}
		if !containsString(aliases, alias) {
			aliases = append(aliases, alias)
		}
	}
	if remove {
		if current.All {
			for _, alias := range aliases {
				if !containsString(current.Excluded, alias) {
					current.Excluded = append(current.Excluded, alias)
				}
			}
		} else {
			for _, alias := range aliases {
				current.Aliases = removeString(current.Aliases, alias)
			}
		}
	} else {
		current = telegramHubWatch{Aliases: aliases}
	}
	sort.Strings(current.Aliases)
	sort.Strings(current.Excluded)
	h.state.Watches[chatKey] = current
	h.mu.Unlock()
	if err := h.persist(); err != nil {
		return "", err
	}
	return formatTelegramWatch(current), nil
}

func formatTelegramWatch(watch telegramHubWatch) string {
	if watch.All {
		if len(watch.Excluded) > 0 {
			return "Notifications: all sessions except " + strings.Join(watch.Excluded, ", ") + "."
		}
		return "Notifications: all sessions."
	}
	if len(watch.Aliases) == 0 {
		return "Notifications: none."
	}
	return "Notifications: " + strings.Join(watch.Aliases, ", ") + "."
}

const telegramHubHelpText = `Commands:
/sessions - list registered Codex sessions
/use ALIAS - select a session for your commands
/at ALIAS COMMAND [ARGS] - run one command without changing selection
/status - show the selected session
/prompt TEXT - send a prompt to the selected session
/pause - pause the selected session
/resume - resume the selected session
/goal [pause|resume|set TEXT] - manage the selected goal
/queue ACTION ... - manage the selected session queue
/agents - show selected-session subagents
/recent [N] - show recent selected-session activity
/watch [all|ALIAS ...] - show or replace this chat's notification subscriptions
/unwatch all|ALIAS ... - mute notifications
/stop ALIAS confirm - stop an explicitly named session
/help - show this help`

func telegramHubConfigFingerprint(options supervisorOptions) string {
	chats := append([]int64(nil), options.TelegramAllowedChats...)
	users := append([]int64(nil), options.TelegramAllowedUsers...)
	sort.Slice(chats, func(i, j int) bool { return chats[i] < chats[j] })
	sort.Slice(users, func(i, j int) bool { return users[i] < users[j] })
	payload, _ := json.Marshal(map[string]any{
		"token":       strings.TrimSpace(options.TelegramToken),
		"chats":       chats,
		"users":       users,
		"pollTimeout": options.TelegramPollTimeout.String(),
		"notify":      options.TelegramNotify,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func telegramHubConfigProvided(options supervisorOptions) bool {
	return strings.TrimSpace(options.TelegramToken) != "" || len(options.TelegramAllowedChats) > 0 || len(options.TelegramAllowedUsers) > 0
}

func validateTelegramHubOptions(options supervisorOptions) error {
	if strings.TrimSpace(options.TelegramToken) == "" {
		return errors.New("the Telegram hub requires a bot token")
	}
	return validateTelegramOptions(options)
}

func queryTelegramHubState(store *telegramHubStateStore) (telegramHubState, bool) {
	state, ok := store.Read()
	if !ok || state.ControlPort == 0 || state.ControlToken == "" {
		return telegramHubState{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/status", state.ControlPort), nil)
	if err != nil {
		return telegramHubState{}, false
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return telegramHubState{}, false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return telegramHubState{}, false
	}
	var live telegramHubState
	if err := json.NewDecoder(response.Body).Decode(&live); err != nil {
		return telegramHubState{}, false
	}
	if live.InstanceID == "" || live.InstanceID != state.InstanceID || live.PID != state.PID || live.Phase != "running" || live.ControlToken != state.ControlToken {
		return telegramHubState{}, false
	}
	return live, true
}

func requestTelegramHub(store *telegramHubStateStore, path string, value any) error {
	state, ok := store.Read()
	if !ok || state.ControlPort == 0 || state.ControlToken == "" {
		return errors.New("the Telegram hub is not running")
	}
	var body bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&body).Encode(value); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", state.ControlPort, path), &body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+state.ControlToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var result struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(response.Body).Decode(&result)
		if result.Error != "" {
			return errors.New(result.Error)
		}
		return fmt.Errorf("Telegram hub request failed with HTTP %d", response.StatusCode)
	}
	return nil
}

func ensureTelegramHub(options supervisorOptions, root, alias, cwd string) error {
	store := newTelegramHubStateStore(root)
	if live, ok := queryTelegramHubState(store); ok {
		if telegramHubConfigProvided(options) {
			if err := validateTelegramOptions(options); err != nil {
				return err
			}
			if live.ConfigFingerprint != telegramHubConfigFingerprint(options) {
				return errors.New("Telegram configuration differs from the running multi-session hub")
			}
		}
		return requestTelegramHub(store, "/register", telegramHubRegistrationRequest{Alias: alias, CWD: cwd})
	}
	previous, _ := store.Read()
	if previous.Phase == "stopping" && previous.PID > 0 && processExists(previous.PID) {
		return errors.New("the Telegram hub is stopping; retry after it exits")
	}
	if _, err := newTelegramHub(options, root); err != nil {
		return fmt.Errorf("start the Telegram hub: %w", err)
	}
	if err := startTelegramHubDetached(options, root); err != nil {
		return err
	}
	deadline := time.Now().Add(telegramHubStartTimeout)
	for time.Now().Before(deadline) {
		if live, ok := queryTelegramHubState(store); ok {
			if live.ConfigFingerprint != telegramHubConfigFingerprint(options) {
				return errors.New("the started Telegram hub has unexpected configuration")
			}
			return requestTelegramHub(store, "/register", telegramHubRegistrationRequest{Alias: alias, CWD: cwd})
		}
		if failed, ok := store.Read(); ok && failed.InstanceID != "" && failed.InstanceID != previous.InstanceID && failed.Phase == "stopped" && failed.TelegramLastError != "" {
			return fmt.Errorf("Telegram hub did not start: %s", failed.TelegramLastError)
		}
		time.Sleep(100 * time.Millisecond)
	}
	state, _ := store.Read()
	if state.TelegramLastError != "" {
		return fmt.Errorf("Telegram hub did not start: %s", state.TelegramLastError)
	}
	return fmt.Errorf("timed out waiting for the Telegram hub; inspect %s", store.LogPath)
}

func unregisterTelegramHubSession(root, alias, cwd string) error {
	if strings.TrimSpace(alias) == "" {
		return nil
	}
	store := newTelegramHubStateStore(root)
	if _, live := queryTelegramHubState(store); !live {
		return nil
	}
	return requestTelegramHub(store, "/unregister", telegramHubRegistrationRequest{Alias: alias, CWD: cwd})
}

func startTelegramHubDetached(options supervisorOptions, root string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"telegram", "serve", "--state-dir", root, "--telegram-poll-timeout-sec", strconv.Itoa(int(options.TelegramPollTimeout / time.Second))}
	if !options.TelegramNotify {
		args = append(args, "--telegram-no-notify")
	}
	command := exec.Command(executable, args...)
	command.Env = telegramHubEnvironment(os.Environ(), options)
	return startDetachedCommand(command)
}

func telegramHubEnvironment(base []string, options supervisorOptions) []string {
	values := map[string]string{
		"CODEXDOG_TELEGRAM_BOT_TOKEN":  strings.TrimSpace(options.TelegramToken),
		"CODEXDOG_TELEGRAM_CHAT_IDS":   joinInt64(options.TelegramAllowedChats),
		"CODEXDOG_TELEGRAM_USER_IDS":   joinInt64(options.TelegramAllowedUsers),
		"CODEXDOG_TELEGRAM_TOKEN_FILE": "",
	}
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if _, replaced := values[strings.ToUpper(name)]; !replaced {
			result = append(result, entry)
		}
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	return result
}

func joinInt64(values []int64) string {
	items := make([]string, 0, len(values))
	for _, value := range values {
		items = append(items, strconv.FormatInt(value, 10))
	}
	return strings.Join(items, ",")
}

func runTelegramManagement(args parsedArguments) (int, error) {
	action := "status"
	if len(args.CommandArgs) > 0 {
		action = strings.ToLower(args.CommandArgs[0])
	}
	store := newTelegramHubStateStore(args.StateRoot)
	switch action {
	case "serve":
		if len(args.CommandArgs) > 1 {
			return 1, errors.New("telegram serve does not accept positional arguments")
		}
		hub, err := newTelegramHub(args.Options, args.StateRoot)
		if err != nil {
			return 1, err
		}
		fmt.Printf("Codexdog Telegram hub is serving from %s\n", args.StateRoot)
		if err := hub.Run(); err != nil {
			return 1, err
		}
		return 0, nil
	case "status":
		if len(args.CommandArgs) > 1 {
			return 1, errors.New("telegram status does not accept positional arguments")
		}
		state, live := queryTelegramHubState(store)
		if !live {
			state, _ = store.Read()
		}
		visible := publicTelegramHubStatus(state, live)
		if args.JSON {
			data, err := json.MarshalIndent(visible, "", "  ")
			if err != nil {
				return 1, err
			}
			fmt.Printf("%s\n", data)
		} else {
			fmt.Printf("Telegram hub live: %s\n", yesNo(live))
			fmt.Printf("Phase: %s\n", valueOrDash(state.Phase))
			aliases := make([]string, 0, len(state.Sessions))
			for alias := range state.Sessions {
				aliases = append(aliases, alias)
			}
			sort.Strings(aliases)
			fmt.Printf("Sessions: %s\n", valueOrDash(strings.Join(aliases, ", ")))
			fmt.Printf("Last Telegram error: %s\n", valueOrDash(state.TelegramLastError))
			fmt.Printf("Stopped reason: %s\n", valueOrDash(state.StoppedReason))
		}
		if live {
			return 0, nil
		}
		return 1, nil
	case "stop":
		if len(args.CommandArgs) > 1 {
			return 1, errors.New("telegram stop does not accept positional arguments")
		}
		if _, live := queryTelegramHubState(store); !live {
			return 1, errors.New("the Telegram hub is not running")
		}
		if err := requestTelegramHub(store, "/stop", nil); err != nil {
			return 1, err
		}
		fmt.Println("Telegram hub stop requested.")
		return 0, nil
	case "unregister":
		if len(args.CommandArgs) != 2 {
			return 1, errors.New("telegram unregister requires an alias")
		}
		if err := requestTelegramHub(store, "/unregister", telegramHubRegistrationRequest{Alias: args.CommandArgs[1]}); err != nil {
			return 1, err
		}
		fmt.Printf("Telegram session %s unregistered.\n", args.CommandArgs[1])
		return 0, nil
	default:
		return 1, fmt.Errorf("unknown telegram action %s; use serve, status, stop, or unregister", action)
	}
}
