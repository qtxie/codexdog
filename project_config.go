package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

const (
	projectConfigFileName = ".codexdog"
	projectConfigMaxBytes = 128 * 1024
)

// projectConfigDocument is intentionally close to the command-line surface.
// TOML is the documented format; JSON and argument arrays are accepted for
// automation and for compatibility with existing shell-oriented workflows.
type projectConfigDocument struct {
	Version *int `toml:"version" json:"version"`

	Args    []string `toml:"args" json:"args"`
	Options []string `toml:"options" json:"options"`
	TUIArgs []string `toml:"tui_args" json:"tuiArgs"`
	Session *string  `toml:"session" json:"session"`

	Codex                   *string  `toml:"codex" json:"codex"`
	CodexConfig             []string `toml:"codex_config" json:"codexConfig"`
	StateDir                *string  `toml:"state_dir" json:"stateDir"`
	HealthURL               *string  `toml:"health_url" json:"healthUrl"`
	ProbeModel              *string  `toml:"probe_model" json:"probeModel"`
	ProbeTimeoutMS          *int64   `toml:"probe_timeout_ms" json:"probeTimeoutMs"`
	TerminalErrorGraceMS    *int64   `toml:"error_grace_ms" json:"errorGraceMs"`
	ProbeSuccesses          *int64   `toml:"probe_successes" json:"probeSuccesses"`
	BackoffMS               []int64  `toml:"backoff_ms" json:"backoffMs"`
	MaxAutoResumes          *int64   `toml:"max_auto_resumes" json:"maxAutoResumes"`
	StallTimeoutMS          *int64   `toml:"stall_timeout_ms" json:"stallTimeoutMs"`
	StallConfirmMS          *int64   `toml:"stall_confirm_ms" json:"stallConfirmMs"`
	StallInterruptTimeoutMS *int64   `toml:"stall_interrupt_timeout_ms" json:"stallInterruptTimeoutMs"`
	MaxStallResumes         *int64   `toml:"max_stall_resumes" json:"maxStallResumes"`
	ToolStallTimeoutMS      *int64   `toml:"tool_stall_timeout_ms" json:"toolStallTimeoutMs"`

	// Flat Telegram keys are accepted alongside the preferred [telegram] table.
	TelegramToken          *string `toml:"telegram_token" json:"telegramToken"`
	TelegramTokenFile      *string `toml:"telegram_token_file" json:"telegramTokenFile"`
	TelegramChatID         *int64  `toml:"telegram_chat_id" json:"telegramChatId"`
	TelegramChatIDs        []int64 `toml:"telegram_chat_ids" json:"telegramChatIds"`
	TelegramUserID         *int64  `toml:"telegram_user_id" json:"telegramUserId"`
	TelegramUserIDs        []int64 `toml:"telegram_user_ids" json:"telegramUserIds"`
	TelegramPollTimeoutSec *int64  `toml:"telegram_poll_timeout_sec" json:"telegramPollTimeoutSec"`
	TelegramNoNotify       *bool   `toml:"telegram_no_notify" json:"telegramNoNotify"`
	TelegramAlias          *string `toml:"telegram_alias" json:"telegramAlias"`
	TelegramDisabled       *bool   `toml:"telegram_disabled" json:"telegramDisabled"`

	Telegram projectConfigTelegram `toml:"telegram" json:"telegram"`
	WeChat   projectConfigWeChat   `toml:"wechat" json:"wechat"`

	// Flat WeChat keys are provided for symmetry with Telegram.
	WeChatUserIDs         []string `toml:"wechat_user_ids" json:"wechatUserIds"`
	WeChatPollTimeoutSec  *int64   `toml:"wechat_poll_timeout_sec" json:"wechatPollTimeoutSec"`
	WeChatLoginTimeoutSec *int64   `toml:"wechat_login_timeout_sec" json:"wechatLoginTimeoutSec"`
	WeChatNoBrowser       *bool    `toml:"wechat_no_browser" json:"wechatNoBrowser"`
	WeChatNoNotify        *bool    `toml:"wechat_no_notify" json:"wechatNoNotify"`
	WeChatDisabled        *bool    `toml:"wechat_disabled" json:"wechatDisabled"`
}

type projectConfigTelegram struct {
	Token          *string `toml:"token" json:"token"`
	TokenFile      *string `toml:"token_file" json:"tokenFile"`
	ChatID         *int64  `toml:"chat_id" json:"chatId"`
	ChatIDs        []int64 `toml:"chat_ids" json:"chatIds"`
	UserID         *int64  `toml:"user_id" json:"userId"`
	UserIDs        []int64 `toml:"user_ids" json:"userIds"`
	PollTimeoutSec *int64  `toml:"poll_timeout_sec" json:"pollTimeoutSec"`
	NoNotify       *bool   `toml:"no_notify" json:"noNotify"`
	Notify         *bool   `toml:"notify" json:"notify"`
	Alias          *string `toml:"alias" json:"alias"`
	Session        *string `toml:"session" json:"session"`
	Disabled       *bool   `toml:"disabled" json:"disabled"`
}

type projectConfigWeChat struct {
	UserIDs         []string `toml:"user_ids" json:"userIds"`
	PollTimeoutSec  *int64   `toml:"poll_timeout_sec" json:"pollTimeoutSec"`
	LoginTimeoutSec *int64   `toml:"login_timeout_sec" json:"loginTimeoutSec"`
	NoBrowser       *bool    `toml:"no_browser" json:"noBrowser"`
	NoNotify        *bool    `toml:"no_notify" json:"noNotify"`
	Notify          *bool    `toml:"notify" json:"notify"`
	Disabled        *bool    `toml:"disabled" json:"disabled"`
}

type projectConfigArguments struct {
	Flags    []string
	TUIArgs  []string
	Path     string
	Presence projectConfigPresence
}

type projectConfigPresence struct {
	TelegramToken     bool
	TelegramTokenFile bool
	TelegramChatIDs   bool
	TelegramUserIDs   bool
	WeChatUserIDs     bool
}

func initialCLICommand(argv []string) string {
	if len(argv) == 0 {
		return "start"
	}
	switch argv[0] {
	case "start", "status", "stop", "smoke", "canary", "doctor", "schema-check", "agents", "queue", "wechat", "telegram":
		return argv[0]
	case "help", "--help", "-h":
		return "help"
	case "version", "--version", "-v":
		return "version"
	default:
		return "start"
	}
}

func projectCWDFromArguments(argv []string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := 0
	if len(argv) > 0 && argv[0] == "start" {
		start = 1
	}
	for index := start; index < len(argv); index++ {
		if argv[index] == "--" {
			break
		}
		if argv[index] != "-C" && argv[index] != "--cwd" {
			if optionTakesValue(argv[index]) {
				index++
			}
			continue
		}
		index++
		if index >= len(argv) || strings.TrimSpace(argv[index]) == "" {
			return "", fmt.Errorf("%s requires a value", argv[index-1])
		}
		cwd = argv[index]
	}
	return filepath.Abs(cwd)
}

func loadProjectConfigArguments(argv []string) (projectConfigArguments, error) {
	if initialCLICommand(argv) != "start" {
		return projectConfigArguments{}, nil
	}
	cwd, err := projectCWDFromArguments(argv)
	if err != nil {
		return projectConfigArguments{}, err
	}
	path := filepath.Join(cwd, projectConfigFileName)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectConfigArguments{}, nil
	}
	if err != nil {
		return projectConfigArguments{}, fmt.Errorf("inspect project config %s: %w", path, err)
	}
	if info.IsDir() {
		return projectConfigArguments{}, fmt.Errorf("project config path %s is a directory", path)
	}
	if info.Size() > projectConfigMaxBytes {
		return projectConfigArguments{}, fmt.Errorf("project config %s exceeds %d bytes", path, projectConfigMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return projectConfigArguments{}, fmt.Errorf("read project config %s: %w", path, err)
	}
	config, err := decodeProjectConfig(data, path)
	if err != nil {
		return projectConfigArguments{}, err
	}
	config.Path = path
	return config, nil
}

func decodeProjectConfig(data []byte, path string) (projectConfigArguments, error) {
	content := strings.TrimSpace(string(data))
	content = strings.TrimPrefix(content, "\ufeff")
	content = strings.TrimSpace(content)
	if content == "" {
		return projectConfigArguments{}, nil
	}
	if content[0] == '{' || content[0] == '[' {
		jsonConfig, jsonErr := decodeJSONProjectConfig([]byte(content), path)
		if jsonErr == nil || content[0] == '{' {
			return jsonConfig, jsonErr
		}
		// TOML tables also begin with '[', so retry as TOML when the
		// JSON-array interpretation does not parse.
		tomlConfig, tomlErr := decodeTOMLProjectConfig(content, path)
		if tomlErr == nil {
			return tomlConfig, nil
		}
		return tomlConfig, tomlErr
	}
	if looksLikeProjectArgumentFile(content) {
		tokens, err := tokenizeProjectConfig(content, path)
		if err != nil {
			return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
		}
		return splitProjectConfigArguments(tokens, path, filepath.Dir(path))
	}
	return decodeTOMLProjectConfig(content, path)
}

func decodeTOMLProjectConfig(content, path string) (projectConfigArguments, error) {
	var document projectConfigDocument
	metadata, err := toml.Decode(content, &document)
	if err != nil {
		return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		return projectConfigArguments{}, fmt.Errorf("parse project config %s: unknown key %s", path, undecoded[0])
	}
	return projectConfigDocumentArguments(document, path)
}

func decodeJSONProjectConfig(data []byte, path string) (projectConfigArguments, error) {
	if data[0] == '[' {
		var tokens []string
		if err := json.Unmarshal(data, &tokens); err != nil {
			return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
		}
		return splitProjectConfigArguments(tokens, path, filepath.Dir(path))
	}
	var document projectConfigDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
	}
	return projectConfigDocumentArguments(document, path)
}

func projectConfigDocumentArguments(document projectConfigDocument, path string) (projectConfigArguments, error) {
	if document.Version != nil && *document.Version != 1 {
		return projectConfigArguments{}, fmt.Errorf("project config %s uses unsupported version %d", path, *document.Version)
	}
	flags := make([]string, 0, 32)
	addString := func(flag string, value *string) {
		if value != nil {
			flags = append(flags, flag, *value)
		}
	}
	addInt := func(flag string, value *int64) {
		if value != nil {
			flags = append(flags, flag, strconv.FormatInt(*value, 10))
		}
	}
	addBoolFlag := func(flag string, value *bool) {
		if value != nil && *value {
			flags = append(flags, flag)
		}
	}
	addIntList := func(flag string, values []int64) {
		for _, value := range values {
			flags = append(flags, flag, strconv.FormatInt(value, 10))
		}
	}
	addStringList := func(flag string, values []string) {
		for _, value := range values {
			flags = append(flags, flag, value)
		}
	}

	addString("--codex", document.Codex)
	addStringList("-c", document.CodexConfig)
	addString("--state-dir", document.StateDir)
	addString("--health-url", document.HealthURL)
	addString("--probe-model", document.ProbeModel)
	addDurationMS("--probe-timeout-ms", document.ProbeTimeoutMS, &flags)
	addDurationMS("--error-grace-ms", document.TerminalErrorGraceMS, &flags)
	addInt("--probe-successes", document.ProbeSuccesses)
	if len(document.BackoffMS) > 0 {
		values := make([]string, 0, len(document.BackoffMS))
		for _, value := range document.BackoffMS {
			values = append(values, strconv.FormatInt(value, 10))
		}
		flags = append(flags, "--backoff-ms", strings.Join(values, ","))
	}
	addInt("--max-auto-resumes", document.MaxAutoResumes)
	addDurationMS("--stall-timeout-ms", document.StallTimeoutMS, &flags)
	addDurationMS("--stall-confirm-ms", document.StallConfirmMS, &flags)
	addDurationMS("--stall-interrupt-timeout-ms", document.StallInterruptTimeoutMS, &flags)
	addInt("--max-stall-resumes", document.MaxStallResumes)
	addDurationMS("--tool-stall-timeout-ms", document.ToolStallTimeoutMS, &flags)

	configuredTelegramChats := document.TelegramChatID != nil || document.TelegramChatIDs != nil || document.Telegram.ChatID != nil || document.Telegram.ChatIDs != nil
	configuredTelegramUsers := document.TelegramUserID != nil || document.TelegramUserIDs != nil || document.Telegram.UserID != nil || document.Telegram.UserIDs != nil
	configuredWeChatUsers := document.WeChatUserIDs != nil || document.WeChat.UserIDs != nil
	telegram := document.Telegram
	if document.Session != nil {
		telegram.Alias = document.Session
	}
	if document.TelegramToken != nil {
		telegram.Token = document.TelegramToken
	}
	if document.TelegramTokenFile != nil {
		telegram.TokenFile = document.TelegramTokenFile
	}
	if document.TelegramChatID != nil {
		telegram.ChatIDs = append([]int64{*document.TelegramChatID}, telegram.ChatIDs...)
	}
	telegram.ChatIDs = append(append([]int64(nil), document.TelegramChatIDs...), telegram.ChatIDs...)
	if document.TelegramUserID != nil {
		telegram.UserIDs = append([]int64{*document.TelegramUserID}, telegram.UserIDs...)
	}
	telegram.UserIDs = append(append([]int64(nil), document.TelegramUserIDs...), telegram.UserIDs...)
	if document.TelegramPollTimeoutSec != nil {
		telegram.PollTimeoutSec = document.TelegramPollTimeoutSec
	}
	if document.TelegramNoNotify != nil {
		telegram.NoNotify = document.TelegramNoNotify
	}
	if document.TelegramAlias != nil {
		telegram.Alias = document.TelegramAlias
	}
	if telegram.Session != nil {
		telegram.Alias = telegram.Session
	}
	if document.TelegramDisabled != nil {
		telegram.Disabled = document.TelegramDisabled
	}
	addString("--telegram-token", telegram.Token)
	addString("--telegram-token-file", telegram.TokenFile)
	addIntList("--telegram-chat-id", telegram.ChatIDs)
	addIntList("--telegram-user-id", telegram.UserIDs)
	addInt("--telegram-poll-timeout-sec", telegram.PollTimeoutSec)
	if telegram.NoNotify != nil && *telegram.NoNotify {
		flags = append(flags, "--telegram-no-notify")
	} else if telegram.Notify != nil && !*telegram.Notify {
		flags = append(flags, "--telegram-no-notify")
	}
	addString("--telegram-alias", telegram.Alias)
	addBoolFlag("--telegram-disabled", telegram.Disabled)

	wechat := document.WeChat
	wechat.UserIDs = append(append([]string(nil), document.WeChatUserIDs...), wechat.UserIDs...)
	if document.WeChatPollTimeoutSec != nil {
		wechat.PollTimeoutSec = document.WeChatPollTimeoutSec
	}
	if document.WeChatLoginTimeoutSec != nil {
		wechat.LoginTimeoutSec = document.WeChatLoginTimeoutSec
	}
	if document.WeChatNoBrowser != nil {
		wechat.NoBrowser = document.WeChatNoBrowser
	}
	if document.WeChatNoNotify != nil {
		wechat.NoNotify = document.WeChatNoNotify
	}
	if document.WeChatDisabled != nil {
		wechat.Disabled = document.WeChatDisabled
	}
	addStringList("--wechat-user-id", wechat.UserIDs)
	addInt("--wechat-poll-timeout-sec", wechat.PollTimeoutSec)
	addInt("--wechat-login-timeout-sec", wechat.LoginTimeoutSec)
	addBoolFlag("--wechat-no-browser", wechat.NoBrowser)
	if wechat.NoNotify != nil && *wechat.NoNotify || wechat.Notify != nil && !*wechat.Notify {
		flags = append(flags, "--wechat-no-notify")
	}
	addBoolFlag("--wechat-disabled", wechat.Disabled)

	flags = append(flags, document.Options...)
	tuiArgs := append([]string(nil), document.TUIArgs...)
	if len(document.Args) > 0 {
		arguments, err := splitProjectConfigArguments(document.Args, path, filepath.Dir(path))
		if err != nil {
			return projectConfigArguments{}, err
		}
		flags = append(flags, arguments.Flags...)
		if len(arguments.TUIArgs) > 0 {
			if len(tuiArgs) > 0 {
				return projectConfigArguments{}, fmt.Errorf("project config %s specifies TUI arguments in both args and tui_args", path)
			}
			tuiArgs = arguments.TUIArgs
		}
	}
	tokens := append([]string(nil), flags...)
	if len(tuiArgs) > 0 {
		tokens = append(tokens, "--")
		tokens = append(tokens, tuiArgs...)
	}
	arguments, err := splitProjectConfigArguments(tokens, path, filepath.Dir(path))
	if err != nil {
		return projectConfigArguments{}, err
	}
	arguments.Presence.TelegramChatIDs = arguments.Presence.TelegramChatIDs || configuredTelegramChats
	arguments.Presence.TelegramUserIDs = arguments.Presence.TelegramUserIDs || configuredTelegramUsers
	arguments.Presence.WeChatUserIDs = arguments.Presence.WeChatUserIDs || configuredWeChatUsers
	return arguments, nil
}

func addDurationMS(flag string, value *int64, flags *[]string) {
	if value != nil {
		*flags = append(*flags, flag, strconv.FormatInt(*value, 10))
	}
}

func resolveProjectConfigPath(value, base string, alwaysRelative bool) string {
	resolved := strings.TrimSpace(value)
	if resolved == "" || filepath.IsAbs(resolved) || !alwaysRelative && !strings.ContainsAny(resolved, `/\\`) && !strings.HasPrefix(resolved, ".") {
		return resolved
	}
	absolute, err := filepath.Abs(filepath.Join(base, resolved))
	if err == nil {
		resolved = absolute
	}
	return resolved
}

func splitProjectConfigArguments(tokens []string, path, base string) (projectConfigArguments, error) {
	if len(tokens) > 0 && tokens[0] == "start" {
		tokens = tokens[1:]
	}
	flags := make([]string, 0, len(tokens))
	tuiArgs := []string(nil)
	separator := -1
	for index, token := range tokens {
		if token == "--" {
			if separator >= 0 {
				return projectConfigArguments{}, fmt.Errorf("project config %s contains more than one -- separator", path)
			}
			separator = index
			continue
		}
		if separator >= 0 {
			continue
		}
		flags = append(flags, token)
	}
	if separator >= 0 {
		tuiArgs = append([]string(nil), tokens[separator+1:]...)
	}
	if err := resolveProjectConfigFlagPaths(flags, base); err != nil {
		return projectConfigArguments{}, fmt.Errorf("parse project config %s: %w", path, err)
	}
	presence := projectConfigPresenceFromFlags(flags)
	return projectConfigArguments{Flags: flags, TUIArgs: tuiArgs, Path: path, Presence: presence}, nil
}

func resolveProjectConfigFlagPaths(flags []string, base string) error {
	for index := 0; index < len(flags); index++ {
		flag := flags[index]
		if flag == "-C" || flag == "--cwd" {
			return fmt.Errorf("project config cannot set %s; use -C on the command line", flag)
		}
		if flag != "--state-dir" && flag != "--telegram-token-file" && flag != "--codex" {
			if optionTakesValue(flag) {
				index++
			}
			continue
		}
		if index+1 >= len(flags) || flags[index+1] == "" {
			return fmt.Errorf("%s requires a value", flag)
		}
		value := flags[index+1]
		always := flag != "--codex"
		flags[index+1] = resolveProjectConfigPath(value, base, always)
		index++
	}
	return nil
}

func projectConfigPresenceFromFlags(flags []string) projectConfigPresence {
	var presence projectConfigPresence
	for index := 0; index < len(flags); index++ {
		flag := flags[index]
		switch flag {
		case "--telegram-token":
			presence.TelegramToken = true
		case "--telegram-token-file":
			presence.TelegramTokenFile = true
		case "--telegram-chat-id":
			presence.TelegramChatIDs = true
		case "--telegram-user-id":
			presence.TelegramUserIDs = true
		case "--wechat-user-id":
			presence.WeChatUserIDs = true
		}
		if optionTakesValue(flag) {
			index++
		}
	}
	return presence
}

func mergeProjectConfigArguments(argv []string, config projectConfigArguments) ([]string, error) {
	if len(config.Flags) == 0 && len(config.TUIArgs) == 0 {
		return argv, nil
	}
	commandIndex := 0
	if len(argv) > 0 && argv[0] == "start" {
		commandIndex = 1
	}
	separator := len(argv)
	for index := commandIndex; index < len(argv); index++ {
		if argv[index] == "--" {
			separator = index
			break
		}
	}
	merged := make([]string, 0, len(argv)+len(config.Flags)+len(config.TUIArgs)+1)
	merged = append(merged, argv[:commandIndex]...)
	merged = append(merged, config.Flags...)
	merged = append(merged, argv[commandIndex:separator]...)
	if separator < len(argv) {
		merged = append(merged, "--")
		merged = append(merged, argv[separator+1:]...)
	} else if len(config.TUIArgs) > 0 {
		merged = append(merged, "--")
		merged = append(merged, config.TUIArgs...)
	}
	return merged, nil
}

func looksLikeProjectArgumentFile(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		first := strings.Fields(line)
		return len(first) > 0 && (strings.HasPrefix(first[0], "-") || first[0] == "start")
	}
	return false
}

func tokenizeProjectConfig(content, path string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inSingle, inDouble, escaped, inComment, haveToken := false, false, false, false, false
	line := 1
	flush := func() {
		if haveToken {
			tokens = append(tokens, current.String())
			current.Reset()
			haveToken = false
		}
	}
	for _, character := range content {
		if inComment {
			if character == '\n' {
				inComment = false
				line++
			}
			continue
		}
		if character == '\n' {
			line++
		}
		if escaped {
			if character != '"' && character != '\\' {
				current.WriteRune('\\')
			}
			current.WriteRune(character)
			haveToken = true
			escaped = false
			continue
		}
		if inSingle {
			if character == '\'' {
				inSingle = false
			} else {
				current.WriteRune(character)
			}
			haveToken = true
			continue
		}
		if inDouble {
			switch character {
			case '"':
				inDouble = false
			case '\\':
				escaped = true
			default:
				current.WriteRune(character)
			}
			haveToken = true
			continue
		}
		switch {
		case character == '\'':
			inSingle, haveToken = true, true
		case character == '"':
			inDouble, haveToken = true, true
		case character == '#':
			if !haveToken {
				inComment = true
				continue
			}
			current.WriteRune(character)
		case unicode.IsSpace(character):
			flush()
		default:
			current.WriteRune(character)
			haveToken = true
		}
	}
	if escaped || inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quote or escape near line %d in %s", line, path)
	}
	flush()
	return tokens, nil
}
