package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type supervisorState struct {
	Version                   int    `json:"version"`
	PID                       int    `json:"pid"`
	CWD                       string `json:"cwd"`
	Phase                     string `json:"phase"`
	AppServerPort             int    `json:"appServerPort,omitempty"`
	ProxyPort                 int    `json:"proxyPort,omitempty"`
	ControlPort               int    `json:"controlPort,omitempty"`
	ControlToken              string `json:"controlToken,omitempty"`
	CurrentThreadID           string `json:"currentThreadId,omitempty"`
	ActiveTurnID              string `json:"activeTurnId,omitempty"`
	LastFailedTurnID          string `json:"lastFailedTurnId,omitempty"`
	ResumeRequestedForTurnID  string `json:"resumeRequestedForTurnId,omitempty"`
	AutomaticResumeCount      int    `json:"automaticResumeCount"`
	StallRecoveryCount        int    `json:"stallRecoveryCount"`
	ProbeAttempt              int    `json:"probeAttempt"`
	ConsecutiveProbeSuccesses int    `json:"consecutiveProbeSuccesses"`
	LastTurnActivityAt        string `json:"lastTurnActivityAt,omitempty"`
	StallSuspectedAt          string `json:"stallSuspectedAt,omitempty"`
	StallPausedReason         string `json:"stallPausedReason,omitempty"`
	NextProbeAt               string `json:"nextProbeAt,omitempty"`
	LastError                 string `json:"lastError,omitempty"`
	TerminalErrorSuspectedAt  string `json:"terminalErrorSuspectedAt,omitempty"`
	UpdatedAt                 string `json:"updatedAt"`
	StoppedReason             string `json:"stoppedReason,omitempty"`
}

type publicSupervisorState struct {
	supervisorState
	Live bool `json:"live"`
}

type stateStore struct {
	Path    string
	LogPath string
	mu      sync.Mutex
}

func newStateStore(root, cwd string) *stateStore {
	key := workspaceKey(cwd)
	return &stateStore{
		Path:    filepath.Join(root, "state-"+key+".json"),
		LogPath: filepath.Join(root, "supervisor-"+key+".log"),
	}
}

func workspaceKey(cwd string) string {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		absolute = cwd
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute))))
	return hex.EncodeToString(sum[:])[:12]
}

func (s *stateStore) Initialize() error {
	return os.MkdirAll(filepath.Dir(s.Path), 0o700)
}

func (s *stateStore) Write(state supervisorState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.Initialize(); err != nil {
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

func (s *stateStore) Read() (supervisorState, bool) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return supervisorState{}, false
	}
	var state supervisorState
	if err := json.Unmarshal(data, &state); err != nil {
		return supervisorState{}, false
	}
	return state, true
}

func publicState(state supervisorState, live bool) publicSupervisorState {
	state.ControlToken = ""
	return publicSupervisorState{supervisorState: state, Live: live}
}

func atomicTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
