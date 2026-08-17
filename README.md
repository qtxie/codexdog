# Codexdog

Codexdog is a native Go wrapper around the Codex CLI. It keeps the normal Codex terminal UI, observes structured app-server events, probes a custom provider after turn failures, and resumes the interrupted thread after the provider recovers. An optional stalled-turn watchdog detects active turns that stop producing activity, interrupts them, and sends `continue`.

It preserves the Codex process. Recovery is for a stopped task/turn, not for restarting the Codex CLI process.

Codexdog owns only the app-server and TUI processes it starts and closes them on exit, without scanning for or terminating independent Codex sessions. On Windows, both processes are placed in a kill-on-close Job Object, so their descendants are also terminated if codexdog is closed unexpectedly.

See the [user manual](docs/user-manual.md) for installation on each supported
platform, complete configuration, Telegram setup, operations, and
troubleshooting.

## Requirements

- Go 1.24 or newer to build
- Codex CLI with `app-server` and `--remote` support (tested with 0.146.0)
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

## Telegram remote control

Codexdog can expose the same supervisor controls through a Telegram bot. The
bot uses HTTPS long polling; it does not open an inbound port on the computer.
Configure the token through an environment variable or a private file, and
always configure an explicit chat allowlist:

```powershell
$env:CODEXDOG_TELEGRAM_BOT_TOKEN = "123456:replace-me"
$env:CODEXDOG_TELEGRAM_CHAT_IDS = "-1001234567890"
$env:CODEXDOG_TELEGRAM_USER_IDS = "123456789" # optional additional restriction
codexdog start -C .
```

For a token file, use `--telegram-token-file C:\secrets\codexdog-bot.txt` (or
`CODEXDOG_TELEGRAM_TOKEN_FILE`). The file is read once at startup and the token
is never written to supervisor state or logs. Repeat `--telegram-chat-id` and
`--telegram-user-id` when more than one identity is allowed. A bot token without
a chat allowlist is rejected at startup.

The available commands are:

```text
/status
/prompt TEXT
/pause
/resume
/goal
/goal pause
/goal resume
/goal set OBJECTIVE
/stop confirm
/help
```

`/prompt` steers an active turn with `turn/steer`; when the thread is idle it
resumes the thread and starts a new text turn. `/pause` interrupts an active
turn, cancels provider/stall recovery, and leaves future automatic recovery
disabled until `/resume`. A saved Codex goal is resumed through
`thread/goal/set` without injecting a synthetic prompt. `/stop` requires the
literal confirmation word so an accidental message cannot terminate the
supervisor.

Telegram update offsets are persisted per workspace under the configured state
directory. This prevents already-processed commands from being replayed after
a restart. Lifecycle notifications (turn failures, recovery, resumes, and
shutdown) are enabled by default; disable them with `--telegram-no-notify`.
The poller retries transient Telegram/API transport failures and records the
last Telegram error in `codexdog status --json`; Telegram outages do not change
the Codex provider-recovery state.

The local authenticated control server also accepts the same command layer at
`POST /command` with a JSON body such as `{"name":"status"}`. It remains
loopback-only and requires the bearer token in the state file.

State and redacted rotating logs are stored under `%LOCALAPPDATA%\codex-supervisor` on Windows and `$HOME/.local/state/codex-supervisor` elsewhere. Set `CODEXDOG_HOME`, `CODEX_SUPERVISOR_HOME`, or pass `--state-dir` to change this.

## Recovery behavior

1. Observe `error` and terminal `turn/completed` events from the same connection used by the TUI.
2. Treat every terminal Codex error as recoverable, including authentication, configuration, `4xx`, `5xx`, timeout, usage-limit, and unknown errors. Error type and HTTP status are retained in state and logs for diagnosis.
3. When Codex reports an error with `willRetry=false`, wait up to `--error-grace-ms` for the normal terminal event. Any continued activity restarts this grace period.
4. If the terminal event is missing, read the thread status. A thread waiting for approval or user input is left alone. An active, non-waiting turn is interrupted; an already-idle thread proceeds directly to recovery.
5. Probe through a dedicated ephemeral Codex thread using the same provider and authentication. The canary is instructed not to call tools.
6. Require two successful canaries by default.
7. Continue the exact failed thread and workspace. If the thread has a non-complete persisted goal, reactivate it through `thread/goal/set` and let Codex's automatic goal continuation proceed without injecting a user prompt. Threads without an active goal use the normal semantic continuation prompt.
8. For `cyberPolicy`, retry the same thread with `continue`, retry it with `继续`, then fork through the failed turn and try `continue` once on the new thread. These retries do not wait for provider probes.
9. Provider probes keep retrying regardless of their error type or HTTP status. Stop after five automatic resumptions or after the forked cyber-policy retry fails.

The continuation is semantic: Codex reuses the saved thread and current workspace, but cannot resume at the exact interrupted output token.

## Stalled-turn watchdog

The watchdog is disabled by default. Enable it with a non-zero timeout:

```powershell
codexdog start -C . --stall-timeout-ms 600000 -- --sandbox workspace-write
```

Activity includes turn and item lifecycle events, streamed agent or reasoning output, command output, plan and diff updates, hooks, and token-usage updates. The normal timeout is paused while Codex waits for approval/user input, performs verification or safety buffering, or runs a command/tool. Quiet active tools are never interrupted unless `--tool-stall-timeout-ms` is also set.

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
--probe-model model-name
```

When `--health-url` is omitted, recovery uses the ephemeral Codex canary directly. The health URL is optional and should be a cheap endpoint for the custom provider. Any non-`2xx` response, including `400` or `404`, is treated as unhealthy and retried with the configured backoff. A successful endpoint check still requires the configured number of Codex canaries before resuming.

## Compatibility checks

Run the unit tests, protocol smoke test, and optional provider canary after upgrading Codex:

```powershell
go test ./...
codexdog smoke
codexdog canary
```

`smoke` initializes the installed app-server and proxy, then creates and reads an ephemeral thread without consuming a model turn. `canary` additionally sends one minimal request through the configured provider.

The protocol implementation targets the Codex CLI 0.146.0 app-server schema. Run `smoke` after upgrading Codex because app-server WebSocket transport is still experimental.
