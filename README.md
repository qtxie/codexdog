# Codexdog

Codexdog is a native Go wrapper around the Codex CLI. It keeps the normal Codex terminal UI, observes structured app-server events, probes a custom provider after turn failures, and resumes the interrupted thread after the provider recovers. An optional stalled-turn watchdog detects active turns that stop producing activity, interrupts them, and sends `continue`.

It preserves the Codex process. Recovery is for a stopped task/turn, not for restarting the Codex CLI process.

Codexdog owns only the app-server and TUI processes it starts and closes them on exit, without scanning for or terminating independent Codex sessions. On Windows, both processes are placed in a kill-on-close Job Object, so their descendants are also terminated if codexdog is closed unexpectedly.

See the [user manual](docs/user-manual.md) for installation on each supported
platform, complete configuration, Telegram and WeChat setup, operations, and
troubleshooting.

## Requirements

- Go 1.24 or newer to build
- Codex CLI with `app-server` and `--remote` support (tested with 0.150.1)
- A working Codex login and provider configuration

## Build and install

```powershell
go build -o codexdog.exe .
```

The resulting binary is self-contained. Put it on `PATH` alongside the Codex executable. Cross-compile binaries for another computer with, for example:

```powershell
$env:GOOS = "linux"; $env:GOARCH = "amd64"; go build -o codexdog .
```

The other computer needs Codex CLI and its login/configuration, but does not need Go.

The [Build workflow](.github/workflows/build.yml) tests and packages Linux
amd64, Windows x86_64, and macOS arm64 binaries with SHA-256 checksum files.

## Run

```powershell
codexdog start -C D:\path\to\repo
```

For project-specific defaults, create a TOML file named `.codexdog` in that
workspace. For example, `D:\EE\QW\sub2api\.codexdog` can contain:

```toml
version = 1
codex_config = ['model="gpt-5.6-sol"']
tui_args = ["resume", "-s", "danger-full-access"]

[telegram]
alias = "sub2"
```

Then this command reads the file automatically:

```powershell
codexdog start -C D:\EE\QW\sub2api
```

The first aliased session that creates the Telegram hub also needs the global
bot token and chat allowlist. Keep those in environment variables, or add a
private relative token file and IDs to the table:

```toml
[telegram]
alias = "sub2"
token_file = "secrets/telegram-token.txt"
chat_ids = [-1001234567890]
user_ids = [123456789]
```

Relative `state_dir`, `telegram.token_file`, and path-like `codex` values are
resolved from the project root. Command-line options override `.codexdog`,
which overrides environment variables. Only `start` loads the project config;
management commands continue to use their explicit `-C` and options. Avoid
committing bot tokens or token files.

When several projects share one Telegram bot, keep the default/shared state
directory; a different relative `state_dir` gives each project its own hub.

`--session NAME` and a top-level `session = "NAME"` are accepted aliases for
`--telegram-alias` and `[telegram].alias` when integrating with older scripts.

Pass Codex configuration overrides with repeatable `-c` flags. Put all other Codex TUI arguments after `--`:

```powershell
codexdog start -C . -c 'model="gpt-5.6-sol"' -- resume -s danger-full-access
```

Check or stop a supervisor from another terminal:

```powershell
codexdog status -C .
codexdog status -C . --json
codexdog stop -C .
```

With Codex CLI 0.150.1, text and JSON status include the current thread's token
breakdown and estimated credit/USD usage, plus account credit balance and rate
limit windows. Account-wide fields are shown only when the signed-in account and
billing route provide them. Collection is best effort: a status RPC failure is
reported as `usageLastError` and does not affect turn supervision or recovery.

## Codex 0.150.1 session support

Codexdog records the active thread directory, session and project IDs, permission
profile, approval policy, sandbox policy, model, and model provider from
app-server settings updates. The workspace selected by `-C` remains the stable
state key; the active thread directory follows Codex `/cd` changes and is used
for recovery continuations, so recovery does not silently return to the initial
directory.

The local app-server proxy pins the first connection, which is the Codex TUI
started by Codexdog, as its primary control connection. Native auxiliary tools
can share the session without becoming the recovery target:

```powershell
codexdog agents -C . -- --no-alt-screen
```

Codexdog also reports observed subagent status from Codex 0.150.1 and exposes
the same view through authenticated remote control:

```text
/agents
/recent [N]
```

`/recent` uses the 0.150.1 experimental timeline API and falls back to the
bounded turns page, then stable thread history, when needed. MCP server runtime
and authentication status are included in `status` output.

Experimental queued submissions require a live supervisor. They are explicit
operations and do not change the immediate `/prompt` behavior:

```powershell
codexdog queue -C . list
codexdog queue -C . add "review the current diff after the active turn"
codexdog queue -C . update QUEUE_ID "run focused tests next"
codexdog queue -C . reorder QUEUE_ID FIRST_ID SECOND_ID
codexdog queue -C . delete QUEUE_ID
codexdog queue -C . start QUEUE_ID
```

## Telegram remote control

One Telegram bot can control any number of Codexdog supervisors. Give each
workspace a stable alias when starting it:

```powershell
$env:CODEXDOG_TELEGRAM_BOT_TOKEN = "123456:replace-me"
$env:CODEXDOG_TELEGRAM_CHAT_IDS = "-1001234567890"
$env:CODEXDOG_TELEGRAM_USER_IDS = "123456789" # optional additional restriction
codexdog start -C D:\work\api --telegram-alias api
```

The first aliased start launches a detached, user-level Telegram hub and
registers the workspace. Start other Codex sessions normally; they reuse the
same hub and bot:

```powershell
codexdog start -C D:\work\web --telegram-alias web
codexdog start -C D:\work\docs --telegram-alias docs
```

For a token file, use `--telegram-token-file C:\secrets\codexdog-bot.txt` (or
`CODEXDOG_TELEGRAM_TOKEN_FILE`). The file is read once at startup and the token
is never written to hub or supervisor state or logs. A chat allowlist is always
required. The first start supplies global bot settings. Later starts may omit
them while the hub is running; if supplied, they must exactly match the live
hub configuration.

The hub is a separate process, not a child of any Codex session. Closing one
session therefore leaves the bot and other sessions running. It uses one HTTPS
long-poll stream and only calls each supervisor's authenticated loopback control
API. It never exposes a public port or a general Codex JSON-RPC tunnel.

Codexdog holds a user-level lock keyed by a hash of the bot token, so an
embedded controller or a second hub cannot accidentally replace that long-poll
stream. This protection also applies when projects use different state
directories.

The available commands are:

```text
/sessions
/use ALIAS
/at ALIAS COMMAND [ARGS]
/status
/prompt TEXT
/pause
/resume
/goal
/goal pause
/goal resume
/goal set OBJECTIVE
/queue [list|add TEXT|delete ID|update ID TEXT|reorder ID [ID...]|start [ID]]
/agents
/recent [N]
/watch [all|ALIAS ...]
/unwatch all|ALIAS ...
/stop ALIAS confirm
/help
```

`/use` selects a session independently for each `(chat, user)` pair, so users in
the same group do not change each other's target. `/at` runs one command on a
named session without changing that selection. `/stop` always requires both an
explicit alias and the literal confirmation word.

Lifecycle notifications from every session are tagged, for example
`[api] Turn completed.` Selection affects commands only; notifications default
to all registered sessions. `/watch` replaces a chat's subscriptions and
`/unwatch` mutes named sessions. Replies to commands are always delivered even
when notifications for that session are muted.

Within each session, Telegram commands execute in arrival order. Different
sessions have independent workers, so a slow command in `api` does not block a
command for `web`. `/prompt`, `/pause`, `/resume`, goals, queues, and recovery
retain their existing supervisor behavior.

The global update offset, alias registry, selections, subscriptions, and event
cursors are persisted privately under the configured state directory. A
restarted hub reconnects to live supervisors and replays their bounded recent
event buffers. Disable unsolicited notifications globally with
`--telegram-no-notify` on the first aliased start.

Administrative commands are available for diagnostics:

```powershell
codexdog telegram status
codexdog telegram status --json
codexdog telegram stop
codexdog telegram serve # foreground fallback
codexdog telegram unregister OLD_ALIAS
```

`codexdog start` without `--telegram-alias` retains the original embedded,
single-session Telegram mode. Do not run that mode with the same token as the
multi-session hub. Use `--telegram-disabled` when inherited Telegram environment
variables should be ignored.

The local authenticated control server also accepts the same command layer at
`POST /command` with a JSON body such as `{"name":"status"}`. It remains
loopback-only and requires the bearer token in the state file. Its `/events`
endpoint supplies a bounded, cursor-based lifecycle stream to the hub.

## WeChat iLink remote control

Codexdog also includes a native Go client for Tencent's iLink Bot API. It uses
QR login, credential and update-cursor persistence, outbound long polling,
`context_token` replies, and typing-state updates. It does not require Python,
Node.js, or an inbound port.

Log in once for each workspace:

```powershell
codexdog wechat login -C .
codexdog wechat status -C .
```

The login command opens the QR URL in the default browser and also prints it
(or writes a temporary QR PNG when the service returns image data). It waits up
to eight minutes for confirmation, refreshing an expired QR code automatically.
Use `--wechat-no-browser` to skip browser launch. Use
`--wechat-login-timeout-sec 900` to wait up to 15 minutes. Start Codexdog once
without an allowlist and send `/uid` to the bot; discovery mode
answers only that command. Then restart with the returned iLink user ID:

```powershell
$env:CODEXDOG_WECHAT_USER_IDS = "your-ilink-user-id"
codexdog start -C .
```

The WeChat bot exposes the per-supervisor `/status`, `/prompt`, `/pause`,
`/resume`, `/goal`, `/queue`, `/stop confirm`, and `/help` controls. It remains
workspace-scoped and does not use Telegram hub aliases. Use `--wechat-user-id`
as a repeatable alternative to the environment variable.
Credentials, long-poll cursor, and the latest allowed-user context tokens are
stored in a private per-workspace credential file. Use
`codexdog wechat logout -C .` to remove them; stop the workspace supervisor
first.

iLink enforces its own session rules. A user must message the bot before it can
reply, proactive sends can expire after roughly 24 hours, and WeChat may limit
consecutive bot messages until the user replies. These service limits cannot be
bypassed by Codexdog. The protocol implementation was informed by the MIT
licensed [WeChat-Bridge project](https://github.com/yuuouu/WeChat-Bridge).

State and redacted rotating logs are stored under `%LOCALAPPDATA%\codex-supervisor` on Windows and `$HOME/.local/state/codex-supervisor` elsewhere. Multi-session Telegram hub state, its global update offset, lock, and log live in the same directory. Set `CODEXDOG_HOME`, `CODEX_SUPERVISOR_HOME`, or pass `--state-dir` to change this.

## Recovery behavior

1. Observe `error` and terminal `turn/completed` events from the same connection used by the TUI.
2. Treat every terminal Codex error from a direct-input thread as recoverable, including authentication, configuration, `4xx`, `5xx`, timeout, usage-limit, and unknown errors. Error type and HTTP status are retained in state and logs for diagnosis.
3. When Codex reports an error with `willRetry=false`, wait up to `--error-grace-ms` for the normal terminal event. Any continued activity restarts this grace period.
4. If the terminal event is missing, read the thread status. A thread waiting for approval or user input is left alone. An active, non-waiting turn is interrupted; an already-idle thread proceeds directly to recovery.
5. Probe through a dedicated ephemeral Codex thread using the same provider and authentication. The canary is instructed not to call tools.
6. Require two successful canaries by default.
7. Continue the exact failed thread and workspace. If the thread has a non-complete persisted goal, reactivate it through `thread/goal/set` and let Codex's automatic goal continuation proceed without injecting a user prompt. Threads without an active goal use the normal semantic continuation prompt.
8. For `cyberPolicy`, retry the same thread with `continue`, retry it with `继续`, then fork through the failed turn and try `continue` once on the new thread. These retries do not wait for provider probes.
9. Provider probes keep retrying regardless of their error type or HTTP status. Stop after five automatic resumptions or after the forked cyber-policy retry fails.

The continuation is semantic: Codex reuses the saved thread and current workspace, but cannot resume at the exact interrupted output token.

Codexdog also observes multi-agent v2 child threads so their activity remains
visible through the TUI, but it does not send direct `turn/start` or
`thread/resume` requests to them. Those threads advertise
`canAcceptDirectInput=false` (and a `parentThreadId`); their parent agent owns
continuation. Recovery is scheduled only for threads that accept direct input,
which avoids the app-server error `direct app-server input is not allowed for
multi-agent v2 sub-agents`.

## Stalled-turn watchdog

The watchdog is disabled by default. Enable it with a non-zero timeout:

```powershell
codexdog start -C . --stall-timeout-ms 600000 -- --sandbox workspace-write
```

Activity includes turn and item lifecycle events, streamed agent or reasoning output, command output, plan and diff updates, hooks, and token-usage updates. The normal timeout is paused while Codex waits for approval/user input, performs verification or safety buffering, or runs a command/tool. Synchronous hooks pause the timer like other active tools; Codex 0.148.0 asynchronous hooks record activity when they start but continue running in the background and do not suppress later stall detection. Quiet active tools are never interrupted unless `--tool-stall-timeout-ms` is also set.

A suspected turn must remain silent through `--stall-confirm-ms` and still report an active thread status before it is interrupted. User Esc/Ctrl+C interruptions remain manual and are never automatically continued. At most two stalled-turn resumptions are attempted by default.

## Configuration

```text
--probe-timeout-ms 120000
--probe-successes 2
--error-grace-ms 5000
--backoff-ms 2000,5000,10000,20000,30000,60000
--max-auto-resumes 5
--stall-timeout-ms 0
--stall-confirm-ms 30000
--stall-interrupt-timeout-ms 15000
--max-stall-resumes 2
--tool-stall-timeout-ms 0
--health-url https://provider.example/health
--health-source https://status.ciii.club/status/codex
--health-source https://status.input.im/
--health-policy any
--health-unknown-policy canary
--health-max-age-ms 180000
--probe-model model-name
```

When neither health option is configured, recovery uses the ephemeral Codex canary directly. Legacy `--health-url` should be a cheap endpoint for the custom provider. Any non-`2xx` response, including `400` or `404`, is treated as unhealthy and retried with the configured backoff. A successful endpoint check still requires the configured number of Codex canaries before resuming. After three consecutive status failures, Codexdog runs one fallback canary so a broken status service cannot block recovery forever.

`--health-source` supports model-aware status dashboards. A bare Ciii or Input.im
URL is detected automatically; `uptime-kuma=URL`, `input-im=URL`, and
`http=URL` select an adapter explicitly. Status observations can only skip an
expensive canary while a model is known to be down. Typed sources are checked
concurrently. A healthy or unavailable dashboard never replaces the real Codex
canary. Explicit `canary` and `doctor --canary` commands always run one. Use either legacy
`--health-url` or typed health sources, not both.

```powershell
codexdog start -C . `
  --health-source https://status.ciii.club/status/codex `
  --health-source https://status.input.im/ `
  --health-policy any `
  --probe-model gpt-5.6-sol
```

## Compatibility checks

Run the unit tests, installed-schema check, and protocol smoke test after
upgrading Codex:

```powershell
go test ./...
codexdog doctor
codexdog schema-check
codexdog smoke
codexdog doctor --canary
```

`doctor` reports the installed Codex compatibility and a compact, redacted
Codex diagnostic summary. `schema-check` regenerates stable and experimental
app-server schemas from the installed CLI and checks the protocol surface used
by Codexdog. `smoke` initializes the installed app-server and proxy, then
creates and reads an ephemeral thread without consuming a model turn.
`doctor --canary` explicitly opts into one minimal provider request.

The protocol implementation targets the Codex CLI 0.150.1 app-server schema.
The compatibility workflow keeps 0.150.1 pinned and exercises `latest` as a
non-blocking early-warning job. Run `smoke` after upgrading Codex because
app-server WebSocket transport is still experimental.
