package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testTelegramHub(t *testing.T, root string) *telegramHub {
	t.Helper()
	options := supervisorOptions{
		TelegramToken:        "test-token",
		TelegramAllowedChats: []int64{10},
		TelegramPollTimeout:  time.Second,
		TelegramNotify:       true,
	}
	hub, err := newTelegramHub(options, root)
	if err != nil {
		t.Fatal(err)
	}
	hub.telegram = &telegramController{out: make(chan telegramOutbound, 64)}
	return hub
}

func TestNormalizeTelegramAlias(t *testing.T) {
	for input, expected := range map[string]string{" API ": "api", "web-2": "web-2", "docs_test": "docs_test"} {
		actual, err := normalizeTelegramAlias(input)
		if err != nil || actual != expected {
			t.Fatalf("normalize %q = %q, %v", input, actual, err)
		}
	}
	for _, input := range []string{"", "2api", "bad alias", "-api", strings.Repeat("a", 33)} {
		if _, err := normalizeTelegramAlias(input); err == nil {
			t.Fatalf("invalid alias %q was accepted", input)
		}
	}
}

func TestTelegramHubRegistryRejectsAliasAndWorkspaceCollisions(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	api := filepath.Join(root, "api")
	web := filepath.Join(root, "web")
	if err := hub.registerSession("api", api); err != nil {
		t.Fatal(err)
	}
	if err := hub.registerSession("api", api); err != nil {
		t.Fatalf("idempotent registration failed: %v", err)
	}
	if err := hub.registerSession("api", web); err == nil {
		t.Fatal("alias collision was accepted")
	}
	if err := hub.registerSession("other", api); err == nil {
		t.Fatal("workspace collision was accepted")
	}
	state, ok := hub.store.Read()
	if !ok || len(state.Sessions) != 1 || state.Sessions["api"].CWD != api {
		t.Fatalf("persisted registry = %#v", state.Sessions)
	}
}

func TestTelegramHubUnregisterCleansAliasReferences(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	cwd := filepath.Join(root, "api")
	if err := hub.registerSession("api", cwd); err != nil {
		t.Fatal(err)
	}
	hub.mu.Lock()
	hub.state.Selections["10:1"] = "api"
	hub.state.Watches["10"] = telegramHubWatch{Aliases: []string{"api"}}
	hub.state.EventCursors["api"] = telegramHubCursor{InstanceID: "instance", Sequence: 7}
	hub.mu.Unlock()
	if err := hub.persist(); err != nil {
		t.Fatal(err)
	}
	if err := hub.unregisterSession("api", cwd); err != nil {
		t.Fatal(err)
	}
	state, ok := hub.store.Read()
	if !ok {
		t.Fatal("hub state was not persisted")
	}
	if _, exists := state.Sessions["api"]; exists || state.Selections["10:1"] != "" {
		t.Fatalf("session references remain: sessions=%#v selections=%#v", state.Sessions, state.Selections)
	}
	if _, exists := state.EventCursors["api"]; exists || len(state.Watches["10"].Aliases) != 0 {
		t.Fatalf("notification references remain: cursors=%#v watches=%#v", state.EventCursors, state.Watches)
	}
}

func TestTelegramHubSelectionIsPerActor(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	if err := hub.registerSession("api", filepath.Join(root, "api")); err != nil {
		t.Fatal(err)
	}
	if err := hub.registerSession("web", filepath.Join(root, "web")); err != nil {
		t.Fatal(err)
	}
	actorA := telegramActor{ChatID: 10, UserID: 1}
	actorB := telegramActor{ChatID: 10, UserID: 2}
	if _, err := hub.useSession(actorA, "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.useSession(actorB, "web"); err != nil {
		t.Fatal(err)
	}
	if selected, _ := hub.selectedSession(actorA); selected != "api" {
		t.Fatalf("actor A selected %q", selected)
	}
	if selected, _ := hub.selectedSession(actorB); selected != "web" {
		t.Fatalf("actor B selected %q", selected)
	}
	reloaded, err := newTelegramHub(hub.options, root)
	if err != nil {
		t.Fatal(err)
	}
	if selected, _ := reloaded.selectedSession(actorA); selected != "api" {
		t.Fatalf("persisted selection = %q", selected)
	}
}

func TestTelegramHubRoutesConcurrentlyAndSerializesPerSession(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	apiCWD := filepath.Join(root, "api")
	webCWD := filepath.Join(root, "web")
	if err := hub.registerSession("api", apiCWD); err != nil {
		t.Fatal(err)
	}
	if err := hub.registerSession("web", webCWD); err != nil {
		t.Fatal(err)
	}

	apiStarted := make(chan string, 2)
	releaseAPI := make(chan struct{})
	apiControl, apiState := testHubSessionControl(t, apiCWD, func(command remoteCommand) (string, error) {
		apiStarted <- command.Text
		<-releaseAPI
		return "api " + command.Text, nil
	})
	defer apiControl.Close()
	webStarted := make(chan string, 1)
	webControl, webState := testHubSessionControl(t, webCWD, func(command remoteCommand) (string, error) {
		webStarted <- command.Text
		return "web " + command.Text, nil
	})
	defer webControl.Close()
	if err := newStateStore(root, apiCWD).Write(apiState); err != nil {
		t.Fatal(err)
	}
	if err := newStateStore(root, webCWD).Write(webState); err != nil {
		t.Fatal(err)
	}

	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	defer func() {
		hub.cancel()
		hub.wg.Wait()
	}()
	replies := make(chan string, 3)
	reply := func(value string) { replies <- value }
	actor := telegramActor{ChatID: 10, UserID: 1}
	if err := hub.dispatchTelegramCommand(context.Background(), actor, telegramCommand{Name: "at", Args: "api prompt first"}, reply); err != nil {
		t.Fatal(err)
	}
	if err := hub.dispatchTelegramCommand(context.Background(), actor, telegramCommand{Name: "at", Args: "api prompt second"}, reply); err != nil {
		t.Fatal(err)
	}
	if err := hub.dispatchTelegramCommand(context.Background(), actor, telegramCommand{Name: "at", Args: "web prompt independent"}, reply); err != nil {
		t.Fatal(err)
	}

	select {
	case value := <-apiStarted:
		if value != "first" {
			t.Fatalf("first API command = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("API command did not start")
	}
	select {
	case value := <-webStarted:
		if value != "independent" {
			t.Fatalf("web command = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("web command was blocked by API session")
	}
	select {
	case value := <-apiStarted:
		t.Fatalf("second API command started before first completed: %q", value)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseAPI)
	select {
	case value := <-apiStarted:
		if value != "second" {
			t.Fatalf("second API command = %q", value)
		}
	case <-time.After(time.Second):
		t.Fatal("second API command did not start")
	}
	for index := 0; index < 3; index++ {
		select {
		case <-replies:
		case <-time.After(time.Second):
			t.Fatal("missing routed command reply")
		}
	}
}

func testHubSessionControl(t *testing.T, cwd string, action func(remoteCommand) (string, error)) (*controlServer, supervisorState) {
	t.Helper()
	state := supervisorState{Version: 2, PID: 1, CWD: cwd, Phase: "idle", CurrentThreadID: "thread", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	control, err := startControlServerWithActions(func() supervisorState { return state }, func(_ context.Context, command remoteCommand) (string, error) {
		return action(command)
	}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	state.ControlPort = control.Port
	state.ControlToken = control.Token
	return control, state
}

func TestTelegramHubStopRequiresExplicitAlias(t *testing.T) {
	hub := testTelegramHub(t, t.TempDir())
	reply := func(string) {}
	actor := telegramActor{ChatID: 10, UserID: 1}
	if err := hub.dispatchTelegramCommand(context.Background(), actor, telegramCommand{Name: "stop", Args: "confirm"}, reply); err == nil {
		t.Fatal("stop without alias was accepted")
	}
	alias, command, err := parseTelegramAtCommand("api stop confirm")
	if err != nil || alias != "api" || command.Name != "stop" || command.Args != "confirm" {
		t.Fatalf("parsed /at stop = %q, %#v, %v", alias, command, err)
	}
}

func TestTelegramHubRoutesConfirmedStopToNamedSession(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "api")
	hub := testTelegramHub(t, root)
	if err := hub.registerSession("api", cwd); err != nil {
		t.Fatal(err)
	}
	received := make(chan remoteCommand, 1)
	control, state := testHubSessionControl(t, cwd, func(command remoteCommand) (string, error) {
		received <- command
		return "Stop requested.", nil
	})
	defer control.Close()
	if err := newStateStore(root, cwd).Write(state); err != nil {
		t.Fatal(err)
	}
	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	defer func() {
		hub.cancel()
		hub.wg.Wait()
	}()
	replies := make(chan string, 1)
	err := hub.dispatchTelegramCommand(context.Background(), telegramActor{ChatID: 10, UserID: 1}, telegramCommand{Name: "stop", Args: "api confirm"}, func(value string) { replies <- value })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-received:
		if command.Name != "stop" || !command.Confirm {
			t.Fatalf("stop command = %#v", command)
		}
	case <-time.After(time.Second):
		t.Fatal("stop command was not routed")
	}
}

func TestTelegramHubWatchFiltering(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	if err := hub.registerSession("api", filepath.Join(root, "api")); err != nil {
		t.Fatal(err)
	}
	if err := hub.registerSession("web", filepath.Join(root, "web")); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.updateWatch(10, "api", true); err != nil {
		t.Fatal(err)
	}
	hub.notifySession("api", "failed")
	hub.notifySession("web", "completed")
	select {
	case sent := <-hub.telegram.out:
		if sent.Text != "[web] completed" {
			t.Fatalf("notification = %#v", sent)
		}
	case <-time.After(time.Second):
		t.Fatal("watched notification was not queued")
	}
	select {
	case extra := <-hub.telegram.out:
		t.Fatalf("muted notification was queued: %#v", extra)
	case <-time.After(20 * time.Millisecond):
	}
	state, ok := hub.store.Read()
	watch := state.Watches["10"]
	if !ok || !watch.All || fmt.Sprint(watch.Excluded) != "[api]" {
		t.Fatalf("persisted watch = %#v", watch)
	}
}

func TestTelegramHubFollowsBufferedSupervisorEvents(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "api")
	broker := newNotificationBroker()
	state := supervisorState{Version: 2, PID: 1, CWD: cwd, Phase: "running", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	control, err := startControlServerWithActions(func() supervisorState { return state }, nil, func() {}, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	state.ControlPort, state.ControlToken = control.Port, control.Token
	if err := newStateStore(root, cwd).Write(state); err != nil {
		t.Fatal(err)
	}
	broker.Publish("Turn failed: provider unavailable")

	hub := testTelegramHub(t, root)
	if err := hub.registerSession("api", cwd); err != nil {
		t.Fatal(err)
	}
	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	hub.syncSessionWatchers()
	defer func() {
		hub.cancel()
		hub.mu.Lock()
		for _, cancel := range hub.watchers {
			cancel()
		}
		hub.mu.Unlock()
		hub.wg.Wait()
	}()

	seenOnline := false
	seenEvent := false
	deadline := time.After(2 * time.Second)
	for !seenOnline || !seenEvent {
		select {
		case sent := <-hub.telegram.out:
			seenOnline = seenOnline || sent.Text == "[api] Session is online."
			seenEvent = seenEvent || sent.Text == "[api] Turn failed: provider unavailable"
		case <-deadline:
			t.Fatalf("notifications online=%t event=%t", seenOnline, seenEvent)
		}
	}
	waitFor(t, func() bool {
		state, ok := hub.store.Read()
		return ok && state.EventCursors["api"].Sequence == 1
	})
}

func TestTelegramHubFingerprintIsOrderIndependent(t *testing.T) {
	a := supervisorOptions{TelegramToken: "token", TelegramAllowedChats: []int64{2, 1}, TelegramAllowedUsers: []int64{4, 3}, TelegramPollTimeout: time.Second, TelegramNotify: true}
	b := supervisorOptions{TelegramToken: "token", TelegramAllowedChats: []int64{1, 2}, TelegramAllowedUsers: []int64{3, 4}, TelegramPollTimeout: time.Second, TelegramNotify: true}
	if telegramHubConfigFingerprint(a) != telegramHubConfigFingerprint(b) {
		t.Fatal("equivalent configurations produced different fingerprints")
	}
	b.TelegramToken = "different"
	if telegramHubConfigFingerprint(a) == telegramHubConfigFingerprint(b) {
		t.Fatal("different tokens produced the same fingerprint")
	}
}

func TestTelegramHubControlRegistrationAuthentication(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	defer hub.cancel()
	control, err := startTelegramHubControl(hub)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	hub.state.ControlPort, hub.state.ControlToken, hub.state.Phase = control.Port, control.Token, "running"
	if err := hub.persist(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/register", control.Port), strings.NewReader(`{"alias":"unauthorized","cwd":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized || len(hub.snapshot().Sessions) != 0 {
		t.Fatalf("unauthorized registration status=%d sessions=%#v", response.StatusCode, hub.snapshot().Sessions)
	}
	if err := requestTelegramHub(hub.store, "/register", telegramHubRegistrationRequest{Alias: "api", CWD: filepath.Join(root, "api")}); err != nil {
		t.Fatal(err)
	}
	live, ok := queryTelegramHubState(hub.store)
	if !ok || live.Sessions["api"].Alias != "api" {
		t.Fatalf("live hub state = %#v, %t", live, ok)
	}
}

func TestTelegramHubControlRejectsRegistrationWhileStopping(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	defer hub.cancel()
	control, err := startTelegramHubControl(hub)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	hub.state.ControlPort, hub.state.ControlToken, hub.state.Phase = control.Port, control.Token, "stopping"
	if err := hub.persist(); err != nil {
		t.Fatal(err)
	}
	err = requestTelegramHub(hub.store, "/register", telegramHubRegistrationRequest{Alias: "api", CWD: filepath.Join(root, "api")})
	if err == nil || !strings.Contains(err.Error(), "not accepting") {
		t.Fatalf("registration while stopping error = %v", err)
	}
	if len(hub.snapshot().Sessions) != 0 {
		t.Fatalf("registration changed state while stopping: %#v", hub.snapshot().Sessions)
	}
}

func TestPublicTelegramHubStatusOmitsControlToken(t *testing.T) {
	visible := publicTelegramHubStatus(telegramHubState{ControlPort: 1234, ControlToken: "secret-control-token", Phase: "running"}, true)
	data, err := json.Marshal(visible)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-control-token") || strings.Contains(string(data), "controlToken") {
		t.Fatalf("public status leaked control token: %s", data)
	}
}

func TestTelegramHubStateNeverPersistsBotToken(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	if err := hub.persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hub.store.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), hub.options.TelegramToken) {
		t.Fatalf("hub state leaked the bot token: %s", data)
	}
}

func TestEnsureTelegramHubUsesExistingDaemonAndChecksConfiguration(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	hub.ctx, hub.cancel = context.WithCancel(context.Background())
	defer hub.cancel()
	control, err := startTelegramHubControl(hub)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	hub.state.ControlPort, hub.state.ControlToken, hub.state.Phase = control.Port, control.Token, "running"
	if err := hub.persist(); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "api")
	if err := ensureTelegramHub(supervisorOptions{}, root, "api", cwd); err != nil {
		t.Fatal(err)
	}
	if live, ok := queryTelegramHubState(hub.store); !ok || live.Sessions["api"].CWD != cwd {
		t.Fatalf("registered state = %#v, %t", live.Sessions, ok)
	}
	mismatch := hub.options
	mismatch.TelegramToken = "different-token"
	if err := ensureTelegramHub(mismatch, root, "web", filepath.Join(root, "web")); err == nil {
		t.Fatal("configuration mismatch was accepted")
	}
}

func TestEnsureTelegramHubRequiresConfigurationBeforeSpawning(t *testing.T) {
	root := t.TempDir()
	err := ensureTelegramHub(supervisorOptions{}, root, "api", filepath.Join(root, "api"))
	if err == nil || !strings.Contains(err.Error(), "requires a bot token") {
		t.Fatalf("missing hub configuration error = %v", err)
	}
	if _, ok := newTelegramHubStateStore(root).Read(); ok {
		t.Fatal("missing configuration created hub state")
	}
}

func TestEnsureTelegramHubValidatesLocalStartupBeforeSpawning(t *testing.T) {
	root := t.TempDir()
	options := supervisorOptions{TelegramToken: "invalid token", TelegramAllowedChats: []int64{10}, TelegramPollTimeout: time.Second, TelegramNotify: true}
	err := ensureTelegramHub(options, root, "api", filepath.Join(root, "api"))
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Fatalf("invalid token error = %v", err)
	}
	if _, ok := newTelegramHubStateStore(root).Read(); ok {
		t.Fatal("invalid token created hub state")
	}
}

func TestEnsureTelegramHubDoesNotRaceAStoppingHub(t *testing.T) {
	root := t.TempDir()
	store := newTelegramHubStateStore(root)
	state := telegramHubState{Version: telegramHubStateVersion, PID: os.Getpid(), InstanceID: "stopping", Phase: "stopping", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	options := supervisorOptions{TelegramToken: "token", TelegramAllowedChats: []int64{10}, TelegramPollTimeout: time.Second, TelegramNotify: true}
	err := ensureTelegramHub(options, root, "api", filepath.Join(root, "api"))
	if err == nil || !strings.Contains(err.Error(), "is stopping") {
		t.Fatalf("stopping hub error = %v", err)
	}
}

func TestTelegramHubConcurrentSelectionPersistence(t *testing.T) {
	root := t.TempDir()
	hub := testTelegramHub(t, root)
	if err := hub.registerSession("api", filepath.Join(root, "api")); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, _ = hub.useSession(telegramActor{ChatID: 10, UserID: int64(index + 1)}, "api")
		}(index)
	}
	wg.Wait()
	state, ok := hub.store.Read()
	if !ok || len(state.Selections) != 20 {
		t.Fatalf("persisted selections = %d", len(state.Selections))
	}
}
