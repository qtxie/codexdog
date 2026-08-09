package main

import (
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStateStoreAndPublicState(t *testing.T) {
	store := newStateStore(t.TempDir(), t.TempDir())
	state := supervisorState{Version: 1, PID: 42, CWD: t.TempDir(), Phase: "starting", ControlToken: "secret", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	state.Phase = "idle"
	if err := store.Write(state); err != nil {
		t.Fatal(err)
	}
	read, ok := store.Read()
	if !ok || read.Phase != "idle" {
		t.Fatalf("unexpected persisted state: %#v", read)
	}
	data, err := json.Marshal(publicState(state, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "controlToken") || strings.Contains(string(data), "secret") {
		t.Fatalf("public state exposed token: %s", data)
	}
	if !strings.Contains(string(data), `"phase":"idle"`) || !strings.Contains(string(data), `"live":true`) {
		t.Fatalf("public state omitted status fields: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(store.Path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("state permissions are not private: %#o", info.Mode().Perm())
		}
	}
}

func TestControlServer(t *testing.T) {
	state := supervisorState{Version: 1, PID: os.Getpid(), CWD: t.TempDir(), Phase: "idle", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	stopped := make(chan struct{}, 1)
	control, err := startControlServer(func() supervisorState { return state }, func() { stopped <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	state.ControlPort, state.ControlToken = control.Port, control.Token
	if !queryControl(state) {
		t.Fatal("authenticated status request failed")
	}
	bad := state
	bad.ControlToken = "wrong"
	if queryControl(bad) {
		t.Fatal("invalid control token was accepted")
	}
	if !requestStop(state) {
		t.Fatal("authenticated stop request failed")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop callback was not invoked")
	}
}
