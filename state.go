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
	Version int `json:"version"`
	PID     int `json:"pid"`
	// CWD is the workspace used to identify this supervisor's state file. It
	// intentionally remains stable when a thread changes directory with /cd.
	CWD                            string                    `json:"cwd"`
	EffectiveCWD                   string                    `json:"effectiveCwd,omitempty"`
	CodexVersion                   string                    `json:"codexVersion,omitempty"`
	SessionID                      string                    `json:"sessionId,omitempty"`
	ProjectID                      string                    `json:"projectId,omitempty"`
	ActivePermissionProfile        string                    `json:"activePermissionProfile,omitempty"`
	ActivePermissionProfileExtends string                    `json:"activePermissionProfileExtends,omitempty"`
	ApprovalPolicy                 string                    `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer              string                    `json:"approvalsReviewer,omitempty"`
	SandboxPolicy                  string                    `json:"sandboxPolicy,omitempty"`
	Model                          string                    `json:"model,omitempty"`
	ModelProvider                  string                    `json:"modelProvider,omitempty"`
	PrimaryClient                  string                    `json:"primaryClient,omitempty"`
	PrimaryClientVersion           string                    `json:"primaryClientVersion,omitempty"`
	Phase                          string                    `json:"phase"`
	AppServerPort                  int                       `json:"appServerPort,omitempty"`
	ProxyPort                      int                       `json:"proxyPort,omitempty"`
	ControlPort                    int                       `json:"controlPort,omitempty"`
	ControlToken                   string                    `json:"controlToken,omitempty"`
	CurrentThreadID                string                    `json:"currentThreadId,omitempty"`
	ActiveTurnID                   string                    `json:"activeTurnId,omitempty"`
	ManualPaused                   bool                      `json:"manualPaused,omitempty"`
	LastFailedTurnID               string                    `json:"lastFailedTurnId,omitempty"`
	ResumeRequestedForTurnID       string                    `json:"resumeRequestedForTurnId,omitempty"`
	AutomaticResumeCount           int                       `json:"automaticResumeCount"`
	StallRecoveryCount             int                       `json:"stallRecoveryCount"`
	ProbeAttempt                   int                       `json:"probeAttempt"`
	ConsecutiveProbeSuccesses      int                       `json:"consecutiveProbeSuccesses"`
	HealthState                    string                    `json:"healthState,omitempty"`
	HealthDetail                   string                    `json:"healthDetail,omitempty"`
	HealthModel                    string                    `json:"healthModel,omitempty"`
	HealthProvider                 string                    `json:"healthProvider,omitempty"`
	HealthObservations             []healthObservationState  `json:"healthObservations,omitempty"`
	LastTurnActivityAt             string                    `json:"lastTurnActivityAt,omitempty"`
	StallSuspectedAt               string                    `json:"stallSuspectedAt,omitempty"`
	StallPausedReason              string                    `json:"stallPausedReason,omitempty"`
	NextProbeAt                    string                    `json:"nextProbeAt,omitempty"`
	LastError                      string                    `json:"lastError,omitempty"`
	TelegramEnabled                bool                      `json:"telegramEnabled,omitempty"`
	TelegramAlias                  string                    `json:"telegramAlias,omitempty"`
	TelegramLastError              string                    `json:"telegramLastError,omitempty"`
	TelegramLastUpdateAt           string                    `json:"telegramLastUpdateAt,omitempty"`
	WeChatEnabled                  bool                      `json:"wechatEnabled,omitempty"`
	WeChatLastError                string                    `json:"wechatLastError,omitempty"`
	WeChatLastUpdateAt             string                    `json:"wechatLastUpdateAt,omitempty"`
	TerminalErrorSuspectedAt       string                    `json:"terminalErrorSuspectedAt,omitempty"`
	TokenUsage                     *threadTokenUsageState    `json:"tokenUsage,omitempty"`
	UsageEstimate                  *threadUsageEstimateState `json:"usageEstimate,omitempty"`
	AccountUsage                   *accountUsageSummaryState `json:"accountUsage,omitempty"`
	RateLimits                     []rateLimitState          `json:"rateLimits,omitempty"`
	RateLimitResetCreditsAvailable *int64                    `json:"rateLimitResetCreditsAvailable,omitempty"`
	UsageUpdatedAt                 string                    `json:"usageUpdatedAt,omitempty"`
	UsageLastError                 string                    `json:"usageLastError,omitempty"`
	QueueUpdatedAt                 string                    `json:"queueUpdatedAt,omitempty"`
	QueueClientMessageIDs          map[string]string         `json:"queueClientMessageIds,omitempty"`
	MCPServers                     []mcpServerState          `json:"mcpServers,omitempty"`
	MCPUpdatedAt                   string                    `json:"mcpUpdatedAt,omitempty"`
	MCPLastError                   string                    `json:"mcpLastError,omitempty"`
	Subagents                      []subagentState           `json:"subagents,omitempty"`
	UpdatedAt                      string                    `json:"updatedAt"`
	StoppedReason                  string                    `json:"stoppedReason,omitempty"`
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
