# Codexdog User Manual

Codexdog runs the normal Codex terminal UI under a small supervisor. It watches
Codex app-server events, detects failed or stalled turns, checks the configured
provider, and resumes the same thread when recovery is possible.

Codexdog does not replace Codex configuration, authentication, approvals, or
sandboxing. It also does not restart the Codex CLI after a failed task: the
Codex processes remain alive while Codexdog recovers the stopped turn.

## Requirements

- A supported 64-bit platform: Linux amd64, Windows x86_64, or macOS arm64.
- Codex CLI installed and available as `codex` on `PATH`.
- Codex CLI login and provider configuration that already work without
  Codexdog.
- Codex CLI `app-server` and `--remote` support. Codexdog currently targets the
  app-server schema shipped with Codex CLI 0.150.1.
- Go 1.24 or newer only when building from source.

Run these checks before using Codexdog:

```text
codex --version
codexdog version
codexdog doctor
codexdog schema-check
codexdog smoke
```

`doctor` checks the installed Codex version and prints a compact redacted
diagnostic summary. `schema-check` regenerates the installed stable and
experimental schemas and checks the fields Codexdog uses. `smoke` starts the
installed app-server and validates the local protocol without making a model
request. `canary`, or `doctor --canary`, makes one minimal request through the
configured provider and is an explicit opt-in.

## Install a GitHub Actions build

The `Build` workflow produces these artifacts:

| Platform | Go target | GitHub artifact | Packaged file |
| --- | --- | --- | --- |
| Linux 64-bit | `linux/amd64` | `codexdog-linux-amd64` | `codexdog-linux-amd64.tar.gz` |
| Windows x86_64 | `windows/amd64` | `codexdog-windows-amd64` | `codexdog-windows-amd64.zip` |
| Apple silicon macOS | `darwin/arm64` | `codexdog-darwin-arm64` | `codexdog-darwin-arm64.tar.gz` |

Open a successful workflow run, download the artifact for the destination
computer, and extract the outer GitHub artifact ZIP. The extracted directory
contains the packaged binary and a `.sha256` checksum file.

On Linux:

```bash
sha256sum -c codexdog-linux-amd64.tar.gz.sha256
tar -xzf codexdog-linux-amd64.tar.gz
sudo install -m 0755 codexdog-linux-amd64 /usr/local/bin/codexdog
codexdog version
```

On Windows PowerShell:

```powershell
Get-FileHash -Algorithm SHA256 .\codexdog-windows-amd64.zip
Get-Content .\codexdog-windows-amd64.zip.sha256
Expand-Archive .\codexdog-windows-amd64.zip -DestinationPath .\codexdog
Rename-Item .\codexdog\codexdog-windows-amd64.exe codexdog.exe
.\codexdog\codexdog.exe version
```

Compare the two displayed hashes, then move the `codexdog` directory to a
permanent location and add it to `PATH`.

On Apple silicon macOS:

```bash
shasum -a 256 -c codexdog-darwin-arm64.tar.gz.sha256
tar -xzf codexdog-darwin-arm64.tar.gz
sudo install -m 0755 codexdog-darwin-arm64 /usr/local/bin/codexdog
codexdog version
```

The macOS workflow output is not code-signed or notarized. Depending on the
computer's security policy, macOS may require an administrator to approve the
binary or may require a locally built and signed binary.

The destination computer still needs its own Codex installation, login, and
provider configuration. The Codexdog binary is self-contained and does not
require Go.

## Build from source

Build for the current computer:

```text
go test ./...
go build -trimpath -o codexdog .
```

Cross-compile from PowerShell:

```powershell
$env:CGO_ENABLED = "0"

$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist\codexdog-linux-amd64 .

$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w" -o dist\codexdog-windows-amd64.exe .

$env:GOOS = "darwin"
$env:GOARCH = "arm64"
go build -trimpath -ldflags "-s -w" -o dist\codexdog-darwin-arm64 .
```

In Go target names, `amd64` means the x86_64 architecture.

## Start Codexdog

Start Codex in a workspace:

```text
codexdog start -C /path/to/project
```

On Windows:

```powershell
codexdog start -C D:\work\project
```

The terminal remains attached to the normal Codex TUI. Use another terminal to
query or stop that workspace's supervisor:

```text
codexdog status -C /path/to/project
codexdog status -C /path/to/project --json
codexdog stop -C /path/to/project
```

Codexdog identifies a supervisor by the absolute workspace path. Use the same
`-C` value for `start`, `status`, and `stop`.

On Codex CLI 0.150.1, status also reports the current thread's input, cached,
cache-write, output, reasoning, and total tokens; estimated credit and USD use;
account credit balance; and primary/secondary rate-limit windows. `--json`
exposes these as `tokenUsage`, `usageEstimate`, `accountUsage`, and `rateLimits`.
Some account types and custom providers do not return every billing field. In
that case Codexdog keeps supervising normally and records the read problem in
`usageLastError`.

Status also records the active thread directory, session/project IDs, permission
profile, approval policy, sandbox policy, model, and provider. `-C` remains the
stable workspace and state-file identity. When Codex changes directory with
`/cd`, `effectiveCwd` tracks the thread's actual directory and recovery uses it
without changing the workspace key.

## Pass settings and arguments to Codex

Codexdog parses its own options before `--`. Everything after `--` is passed
unchanged to the Codex TUI:

```text
codexdog start -C . -- resume -s danger-full-access
```

Codex configuration overrides use a repeatable `-c` option before `--`:

```powershell
codexdog start -C . `
  -c 'model="gpt-5.6-sol"' `
  -c 'model_reasoning_effort="high"' `
  -- resume -s danger-full-access
```

`-c 'model_reasoning_effort="high"'` overrides that Codex configuration key for
the app-server, health canaries, and TUI. It does not configure a Codexdog retry
setting.

Configure a custom base URL, provider, API key, and model through the normal
Codex configuration or environment variables. Codexdog inherits the current
environment and forwards its `-c` overrides to both Codex child processes.

Options such as `danger-full-access` retain their normal Codex meaning and risk.
Codexdog does not add a safety boundary around the selected Codex sandbox or
approval policy.

### Project configuration

When `start` has a workspace selected by `-C`, Codexdog automatically reads an
optional `.codexdog` file from that workspace. TOML is the documented format:

```toml
version = 1
codex = "codex"
codex_config = [
  'model="gpt-5.6-sol"',
  'model_reasoning_effort="high"',
]
probe_timeout_ms = 120000
tui_args = ["resume", "-s", "danger-full-access"]

[telegram]
alias = "sub2"
token_file = "secrets/telegram-token.txt"
chat_ids = [-1001234567890]
user_ids = [123456789]
poll_timeout_sec = 30
notify = true

[wechat]
user_ids = ["your-ilink-user-id"]
poll_timeout_sec = 35
notify = true
```

This makes the project command self-contained:

```powershell
codexdog start -C D:\EE\QW\sub2api
```

Top-level recovery setting names match their long CLI options with hyphens
changed to underscores. `codex_config` and `tui_args` are arrays. The Telegram
table also accepts `token`, `no_notify`, and `disabled`; prefer `token_file` or
`CODEXDOG_TELEGRAM_BOT_TOKEN` so a secret is not committed with the project.
The WeChat table also accepts `login_timeout_sec`, `no_browser`, `no_notify`,
and `disabled`.

Relative `state_dir`, `telegram.token_file`, and path-like `codex` values are
resolved from the directory containing `.codexdog`. Values are applied in this
order: built-in defaults, environment variables, `.codexdog`, then explicit
command-line options. Repeatable IDs and `-c` entries from the command line are
added after configured entries. Explicit TUI arguments after a command-line
`--` replace `tui_args` from the file.

For a shared multi-session Telegram hub, leave `state_dir` at its platform
default or set the same absolute/shared directory in every project. A distinct
relative state directory creates a separate hub for that project.

Project configuration is loaded only by `start`; `status`, `stop`, and other
management commands do not load it. A JSON object, JSON argument array, or a
line-oriented CLI argument file is also accepted for automation, but TOML is
the stable human-facing format. The file may not set `-C` or `--cwd` because
its location already defines the workspace.

For compatibility with older scripts, `--session NAME` and top-level
`session = "NAME"` are accepted aliases for `--telegram-alias` and
`[telegram].alias`.

## Native agents and queued work

The proxy pins the spawned Codex TUI as the primary control connection. This
lets the native agents dashboard connect to the same supervised session without
becoming the destination for recovery or remote-control requests:

```text
codexdog agents -C /path/to/project -- --no-alt-screen
```

The experimental app-server queue is exposed only through explicit commands and
requires a live Codexdog supervisor. It is separate from an immediate remote
prompt:

```text
codexdog queue -C /path/to/project list
codexdog queue -C /path/to/project add "review the current diff after this turn"
codexdog queue -C /path/to/project update QUEUE_ID "run focused tests next"
codexdog queue -C /path/to/project reorder QUEUE_ID FIRST_ID SECOND_ID
codexdog queue -C /path/to/project delete QUEUE_ID
codexdog queue -C /path/to/project start QUEUE_ID
```

Queue commands use Codex 0.150.1's experimental queue API. Submission client
message IDs are persisted so an operator can trace a queued request in state.

## Provider recovery

Provider checks are event-driven, not continuous monitoring. Codexdog starts
checking only after it sees a terminal turn error. It does not ping the provider
every few minutes while a turn appears healthy. The stalled-turn watchdog is a
separate path that interrupts and continues a confirmed stalled turn directly.

For a normal failed turn, recovery works as follows:

1. Codexdog records the error. Every terminal error category is eligible,
   including HTTP `4xx`, HTTP `5xx`, authentication, timeout, configuration,
   usage-limit, and unknown errors.
2. If Codex reports `willRetry=false`, Codexdog waits for the terminal event for
   `--error-grace-ms` (5 seconds by default). Continued turn activity restarts
   that grace period.
3. If necessary, Codexdog reads the thread status and interrupts a turn that is
   still marked active. A thread waiting for approval or user input is not
   interrupted.
4. Codexdog retries provider probes using `--backoff-ms`. By default the delays
   are 2, 5, 10, 20, 30, and 60 seconds, with 60 seconds repeated afterward.
5. After the required consecutive canaries succeed, Codexdog resumes the exact
   thread. A non-complete saved `/goal` is reactivated directly; a normal thread
   receives a semantic continuation prompt.
6. Recovery stops in `needs-attention` after the configured consecutive
   automatic resume limit is reached.

Multi-agent v2 child threads are observational only. Codexdog records their
activity but does not issue direct `thread/resume` or `turn/start` requests when
their `canAcceptDirectInput` capability is false (or their `parentThreadId`
identifies them as a child). The parent agent remains responsible for retrying
or continuing that child; only direct-input threads enter provider or stall
recovery.

A timeout while compacting, including an SSE idle timeout, follows this same
generic terminal-error recovery path.

For a `cyberPolicy` failure, Codexdog first sends `continue`, then sends `继续` if
the same policy failure repeats, and finally forks through the failed turn and
sends `continue` once on the new thread. This sequence does not wait for provider
health probes.

### Optional health checks

Without `--health-url` or `--health-source`, each recovery attempt directly runs
an ephemeral Codex canary through the configured provider.

With `--health-url`, Codexdog first sends an HTTP `GET` to that URL. The endpoint
must be a direct, inexpensive service endpoint that returns a `2xx` response
when usable. A status dashboard HTML page is suitable only if the page itself
changes to a non-`2xx` status while the provider is unavailable. Codexdog does
not add custom authentication headers to this request.

Every non-`2xx` status, including `400` and `404`, as well as connection errors
and timeouts, is unhealthy and is retried. A successful URL check is only a
prefilter: the configured number of Codex canaries must still succeed before the
thread resumes.

Example:

```text
codexdog start -C . \
  --health-url https://provider.example/health \
  --probe-successes 2 \
  --probe-timeout-ms 120000
```

Status dashboard pages normally return `200` even when an individual model is
down. Configure them with `--health-source`, which reads the dashboard's JSON
API and selects the effective probe model:

```text
codexdog start -C . \
  --health-source https://status.ciii.club/status/codex \
  --probe-model gpt-5.6-sol

codexdog start -C . \
  --health-source https://status.input.im/ \
  --probe-model gpt-5.6-sol
```

Bare Ciii and Input.im URLs are detected automatically. The explicit forms are
`uptime-kuma=URL`, `input-im=URL`, and `http=URL`. Typed sources return one of
three states:

- `healthy`: a fresh status sample says that the selected model is available;
- `unhealthy`: a fresh sample says that the selected model is unavailable;
- `unknown`: the source is unreachable, stale, malformed, or does not list the
  selected model.

An unhealthy aggregate skips the real canary until the next recovery attempt.
Healthy status still has to pass the real Codex canary. By default, an unknown
status falls back to that canary so an external dashboard outage cannot block
recovery. Set `--health-unknown-policy block` for fail-closed behavior.

With multiple sources, `--health-policy any` accepts one healthy source, rejects
only when all sources are explicitly unhealthy, and otherwise returns unknown.
`--health-policy all` rejects any unhealthy source and requires all sources to
be healthy. Multiple sources should describe the same provider route. A source
can include a `provider` selector in `.codexdog` when one configuration covers
multiple providers.

Status samples older than `--health-max-age-ms` (three minutes by default) are
unknown. Model resolution uses the source's explicit `model`, then
`--probe-model`, then the failed thread's effective model. Status requests use a
15-second maximum timeout, accept at most three redirects, and read at most 1
MiB. No authentication headers are added.

## Stalled-turn watchdog

The watchdog is disabled by default. Enable it by setting a non-zero silent-turn
timeout:

```text
codexdog start -C . --stall-timeout-ms 600000
```

When a running turn produces no recognized activity for that duration,
Codexdog waits through `--stall-confirm-ms`, confirms that the thread still
reports an active turn, sends the equivalent of Esc through `turn/interrupt`,
and starts a new turn with `continue`.

Waiting for an approval or user response does not count as a stall. Active tools
also pause the normal timer. Set `--tool-stall-timeout-ms` only when long-silent
tools should be eligible for recovery. Manual Esc or Ctrl+C interruptions are
never automatically resumed.

Synchronous hooks count as active tools. Asynchronous hooks introduced in Codex
0.148.0 record activity when they start, but their background lifetime does not
pause the watchdog or hide a later stalled turn.

## Telegram remote control

Telegram control uses one detached, user-level hub to manage multiple Codexdog
supervisors through a single bot. It uses outbound HTTPS long polling and each
supervisor's authenticated loopback API; no public port is opened. A chat
allowlist is mandatory. An optional user allowlist further restricts who may
issue commands inside an allowed group.

PowerShell example:

```powershell
$env:CODEXDOG_TELEGRAM_BOT_TOKEN = "123456:replace-me"
$env:CODEXDOG_TELEGRAM_CHAT_IDS = "123456789,-1001234567890"
$env:CODEXDOG_TELEGRAM_USER_IDS = "123456789"
codexdog start -C D:\work\api --telegram-alias api
codexdog start -C D:\work\web --telegram-alias web
```

Bash example using a token file:

```bash
mkdir -p ~/.config
install -m 0600 /path/to/bot-token ~/.config/codexdog-telegram-token
codexdog start -C /work/project \
  --telegram-token-file ~/.config/codexdog-telegram-token \
  --telegram-chat-id 123456789 \
  --telegram-user-id 123456789 \
  --telegram-alias project
```

The first aliased start ensures the hub is running, then registers the
workspace. Later aliased starts reuse it. The hub is not owned by the first
supervisor, so that session may stop without affecting the bot or other
sessions. Later starts may omit bot configuration while the hub remains live;
if they provide it, it must match the hub's global configuration.

Available bot commands:

| Command | Effect |
| --- | --- |
| `/sessions` | List registered aliases, workspaces, and live phases. |
| `/use ALIAS` | Select the target for this chat/user pair. |
| `/at ALIAS COMMAND ...` | Run one command without changing the selection. |
| `/status` | Show selected-session state, usage, limits, and errors. |
| `/prompt TEXT` | Steer or start a turn in the selected session. |
| `/pause` | Interrupt and manually pause the selected session. |
| `/resume` | Resume the selected thread or saved goal. |
| `/goal ...` | Inspect or update the selected session's goal. |
| `/queue ACTION ...` | Manage the selected session's queued submissions. |
| `/agents` | Show selected-session subagent status. |
| `/recent [N]` | Show recent selected-session activity. |
| `/watch [all\|ALIAS ...]` | Show or replace this chat's notification subscriptions. |
| `/unwatch all\|ALIAS ...` | Mute notifications for all or named sessions. |
| `/stop ALIAS confirm` | Stop an explicitly named supervisor. |
| `/help` | Show the command list. |

Selections are independent per `(chat ID, user ID)`. Commands are FIFO within a
session and concurrent across sessions. Notifications do not follow the current
selection: they default to all sessions, are prefixed with `[ALIAS]`, and are
filtered per chat using `/watch` and `/unwatch`. Command replies are never
filtered. Use `--telegram-no-notify` on initial hub setup to disable unsolicited
notifications globally.

The hub persists one global Telegram offset, the alias registry, selections,
subscriptions, and per-session event cursors. Supervisors keep a bounded recent
event stream so a restarted hub can recover notifications emitted while it was
offline. Telegram failure is recorded in hub status and logs but does not alter
provider recovery.

The hub normally needs no direct management. Diagnostic fallbacks are:

```text
codexdog telegram status
codexdog telegram status --json
codexdog telegram stop
codexdog telegram serve
codexdog telegram unregister ALIAS
```

Starting without `--telegram-alias` preserves embedded single-session mode. A
bot token must never be polled by both modes at once. `--telegram-disabled`
ignores inherited Telegram configuration for a supervisor that should have no
Telegram control.

Remote prompts execute under the Codex sandbox and approval configuration of
the selected running session. Treat every allowed chat and user as someone who
can direct every registered session. Do not expose the bot token, hub/supervisor
state files, or control tokens.

## WeChat iLink remote control

WeChat control is implemented directly against Tencent's iLink Bot API in Go.
It performs QR login, persists the bot credential and `getupdates` cursor,
receives messages by outbound HTTPS long polling, and replies using each
sender's latest `context_token`. It does not need Python, Node.js, a webhook, or
an inbound port. The implementation follows the protocol behavior documented
by the MIT licensed
[WeChat-Bridge project](https://github.com/yuuouu/WeChat-Bridge).

Login is scoped to the same workspace key as the supervisor:

```text
codexdog wechat login -C /path/to/project
codexdog wechat status -C /path/to/project
```

`login` opens the QR URL returned by iLink in the default browser and also
prints it, or writes a temporary `wechat-login-*.png` under the state directory
when image data is returned. Browser-launch failure is non-fatal; use
`--wechat-no-browser` to skip the attempt. It waits up to 480 seconds for scan
confirmation and refreshes an expired QR code up to three times; any temporary
PNG is removed when the command finishes. The confirmed bot credential is
retained in a private `wechat-*.json` file.

User IDs are strings assigned by iLink, not numeric Telegram IDs. To bootstrap
the allowlist:

1. Start Codexdog after logging in, without any `--wechat-user-id` setting.
2. Send `/uid` to the WeChat bot. In this discovery-only mode, all other
   commands are ignored.
3. Stop Codexdog and configure exactly the returned ID.
4. Start Codexdog again.

PowerShell:

```powershell
$env:CODEXDOG_WECHAT_USER_IDS = "your-ilink-user-id"
codexdog start -C D:\work\project
```

Bash:

```bash
export CODEXDOG_WECHAT_USER_IDS="your-ilink-user-id"
codexdog start -C /work/project
```

`--wechat-user-id ID` is repeatable and can be used instead of the environment
variable. Every configured ID can issue the per-supervisor `/status`, `/prompt`,
`/pause`, `/resume`, `/goal`, `/queue`, `/agents`, `/recent`, `/stop confirm`, and
`/help` commands; WeChat additionally accepts `/uid`. WeChat remains scoped to
one workspace and does not use Telegram aliases. Unknown commands return the
standard help without exposing a general Codex JSON-RPC or shell interface.

The latest context token is persisted only for allowed users. Lifecycle
notifications use that token and are skipped until an allowed user has sent a
message. Use `--wechat-no-notify` to retain command replies but disable these
notifications. `--wechat-disabled` temporarily prevents a logged-in bot from
starting. To permanently remove the credential, stop the supervisor and run:

```text
codexdog wechat logout -C /path/to/project
```

iLink itself requires the user to message the bot first, normally limits
proactive replies to a 24-hour session window, and may block further sends after
several consecutive bot messages until the user replies. A rejected send is
recorded in `wechatLastError`; Codexdog does not attempt to bypass these service
limits.

## Command reference

| Command | Purpose |
| --- | --- |
| `codexdog start` | Start the supervisor, app-server, proxy, and Codex TUI. |
| `codexdog status` | Read live state for the selected workspace. |
| `codexdog stop` | Ask the selected workspace supervisor to shut down. |
| `codexdog doctor` | Check installed Codex compatibility and compact diagnostics without a model request. |
| `codexdog doctor --canary` | Run diagnostics plus one explicit provider canary. |
| `codexdog schema-check` | Regenerate and validate the installed stable and experimental app-server schemas. |
| `codexdog smoke` | Validate app-server and proxy compatibility without a model turn. |
| `codexdog canary` | Run the smoke checks plus one provider model turn. |
| `codexdog agents` | Open Codex's native agents dashboard against the supervised session. |
| `codexdog queue ACTION ...` | Manage explicit experimental queued submissions on a live supervisor. |
| `codexdog wechat login\|status\|logout` | Manage the workspace's iLink Bot login. |
| `codexdog telegram serve\|status\|stop\|unregister` | Diagnose or manage the shared Telegram hub. |
| `codexdog version` | Print the Codexdog version. |
| `codexdog help` | Print CLI usage and option defaults. |

The main options are:

| Option | Meaning | Default |
| --- | --- | --- |
| `-C, --cwd DIR` | Workspace used by Codex and the state key. | Current directory |
| `--codex PATH` | Codex executable to launch. | `codex` |
| `-c, --config KEY=VALUE` | Repeatable Codex configuration override. | None |
| `--health-url URL` | Optional HTTP health precheck. | None |
| `--health-source SOURCE` | Model-aware source: URL, `TYPE=URL`, or JSON; repeatable. | None |
| `--health-policy POLICY` | Aggregate typed sources with `any` or `all`. | `any` |
| `--health-unknown-policy POLICY` | On unknown status, run the canary or block. | `canary` |
| `--health-max-age-ms MS` | Maximum typed status-sample age. | `180000` |
| `--probe-model MODEL` | Model used by recovery canaries. | Current Codex model |
| `--probe-timeout-ms MS` | Maximum duration of a provider canary. | `120000` |
| `--probe-successes N` | Consecutive canaries required before resume. | `2` |
| `--error-grace-ms MS` | Wait for a terminal event after `willRetry=false`. | `5000` |
| `--backoff-ms LIST` | Comma-separated recovery probe delays. | `2000,5000,10000,20000,30000,60000` |
| `--max-auto-resumes N` | Consecutive provider recovery resume limit. | `5` |
| `--stall-timeout-ms MS` | Normal silent-turn timeout; `0` disables it. | `0` |
| `--stall-confirm-ms MS` | Extra stall confirmation window. | `30000` |
| `--stall-interrupt-timeout-ms MS` | Interrupt and status RPC timeout. | `15000` |
| `--max-stall-resumes N` | Consecutive stall recovery limit. | `2` |
| `--tool-stall-timeout-ms MS` | Silent active-tool timeout; `0` disables it. | `0` |
| `--telegram-token TOKEN` | Set the Telegram bot token directly; prefer an environment variable or private file. | None |
| `--telegram-token-file PATH` | Read the Telegram bot token from a file. | None |
| `--telegram-chat-id ID` | Allow a chat; repeatable. | None |
| `--telegram-user-id ID` | Allow a sender; repeatable and optional. | None |
| `--telegram-poll-timeout-sec N` | Telegram long-poll duration from 1 to 50 seconds. | `30` |
| `--telegram-no-notify` | Disable unsolicited Telegram notifications. | Notifications enabled |
| `--telegram-alias NAME` | Register this supervisor with the shared Telegram hub. | None |
| `--session NAME` | Alias for `--telegram-alias`. | None |
| `--telegram-disabled` | Ignore inherited Telegram configuration. | Off |
| `--wechat-user-id ID` | Allow an iLink sender; repeatable. With none, only `/uid` works. | None |
| `--wechat-poll-timeout-sec N` | iLink long-poll duration from 1 to 50 seconds. | `35` |
| `--wechat-login-timeout-sec N` | QR login deadline in seconds. Expired QR codes are refreshed up to three times within this window. | `480` |
| `--wechat-no-browser` | Do not open the QR login URL in the default browser. | Browser opens automatically |
| `--wechat-no-notify` | Disable unsolicited WeChat notifications. | Notifications enabled |
| `--wechat-disabled` | Do not start the persisted WeChat bot. | Off |
| `--state-dir DIR` | Override the state and log directory. | Platform default |
| `--json` | Emit machine-readable status output. | Off |

Environment variables:

| Variable | Purpose |
| --- | --- |
| `CODEXDOG_HOME` | Override the state and log directory. |
| `CODEX_SUPERVISOR_HOME` | Older alias used when `CODEXDOG_HOME` is unset. |
| `CODEXDOG_TELEGRAM_BOT_TOKEN` | Telegram bot token. |
| `CODEXDOG_TELEGRAM_TOKEN_FILE` | Path to the Telegram bot token file. |
| `CODEXDOG_TELEGRAM_CHAT_IDS` | Comma-separated allowed chat IDs. |
| `CODEXDOG_TELEGRAM_USER_IDS` | Comma-separated allowed user IDs. |
| `CODEXDOG_WECHAT_USER_IDS` | Comma-separated allowed iLink user IDs. |

## State, logs, and shutdown

The default state directory is:

- Windows: `%LOCALAPPDATA%\codex-supervisor`
- Linux and macOS: `$HOME/.local/state/codex-supervisor`

Each workspace gets a hashed `state-*.json`, `supervisor-*.log`, legacy embedded
Telegram offset file, and, after iLink login, a `wechat-*.json` credential file.
Multi-session Telegram adds `telegram-hub.json`, `telegram-hub-offset.json`,
`telegram-hub.lock`, and `telegram-hub.log` once per state directory. Hub state
contains private loopback credentials, aliases, selections, subscriptions, and
event cursors, but never the bot token. Supervisor state includes its alias and
private loopback control credential. Do not publish these files. Logs are capped
at 2 MiB with three rotations and redact common authorization, API-key, and
token patterns. Review logs before sharing them because arbitrary provider
messages may still contain sensitive context.

Codexdog also creates one user-level `telegram-poll-*.lock` per bot token in the
default state directory. This prevents an embedded controller or a second hub,
including one using another project `state_dir`, from taking over the same
Telegram long-poll stream. The file name contains only a truncated token hash.

On normal exit, Codexdog shuts down only the app-server and TUI process trees it
started. It does not scan for or terminate unrelated Codex sessions. On Windows,
owned processes also run in a kill-on-close Job Object so descendants are
cleaned up if Codexdog exits unexpectedly. The detached Telegram hub starts in
an independent process group, does not inherit handles, and breaks away from an
inherited parent Job Object, so it can outlive the session that started it.

Prefer `codexdog stop -C WORKSPACE` or exit the attached TUI normally. If the
state says `needs-attention`, inspect `Last error` and the workspace log, then
send a prompt or resume through Telegram, restart the session, or correct the
provider configuration as appropriate.

## Troubleshooting

### Codex stopped, but no provider checks appear

Provider probing starts only after Codexdog observes a terminal error. A merely
quiet `running` turn does not trigger it. Enable `--stall-timeout-ms` for the
quiet-active-turn case; the watchdog interrupts and continues that turn without
a provider probe. Check `Phase`, `Manual pause`, `Last error`, and `Next provider
probe` with `status --json`.

### Provider recovered, but the task did not resume

Confirm that consecutive canary successes reached `--probe-successes`. Also
check for `paused`, `waiting-for-user`, or `needs-attention`, and whether
`--max-auto-resumes` was reached. A health URL returning `2xx` is insufficient
when the real Codex canary still fails.

If the last error says `direct app-server input is not allowed for multi-agent
v2 sub-agents`, update Codexdog to a build with multi-agent thread filtering.
The message means a child thread was mistakenly targeted with a direct
`thread/resume`/`turn/start`; the parent thread must own that continuation.

### A provider health check is wrong

For legacy `--health-url`, test the exact URL with a plain unauthenticated HTTP
`GET`. Authenticated endpoints, `400`, and `404` responses are unhealthy. Do
not put a dashboard HTML page in `--health-url`; use `--health-source` for Ciii,
Input.im, or Uptime Kuma. Check `Health status`, `Health detail`, and each source
observation in `codexdog status`. Omitting both health options uses the real
Codex canary alone.

### Codex appears to work forever without output

Enable the stalled-turn watchdog with a timeout suitable for the workload. If a
tool can legitimately stay silent for a long time, leave
`--tool-stall-timeout-ms` at `0` or set it substantially higher than the normal
turn timeout.

### Telegram does not reply

Run `codexdog telegram status --json`. Confirm the hub is live and inspect
`telegramLastError`; if startup failed, separately verify the configured bot
token, network connectivity, and access to `api.telegram.org`. The chat allowlist must
match `message.chat.id`; the optional user allowlist must match
`message.from.id`. A `409` error means another process is polling the same bot
token. Current Codexdog processes on the same machine are rejected by the
per-token polling lock; conflicts with an older binary, another application, or
another machine are recorded while the hub remains online and retries. Stop the
other poller, then inspect `telegramLastError` and `telegram-hub.log`.

If `/sessions` lists a session as offline, run `codexdog status -C WORKSPACE`.
An alias left by an abruptly terminated supervisor can be reused for the same
workspace or removed with `codexdog telegram unregister ALIAS`. A different
alias cannot claim the same workspace until the stale registration is removed.

### WeChat does not reply

Run `codexdog wechat status -C WORKSPACE`, confirm that normal Codexdog status
shows `WeChat control: yes`, and inspect `wechatLastError` in JSON status. With
no allowlist, only `/uid` is accepted. With an allowlist, the sender must exactly
match one of the configured iLink IDs. If the error contains `ret=-2`, send a
new message from that WeChat user to reopen the iLink session window. Use
`codexdog wechat logout` followed by `login` when the persisted bot credential
has expired.

### Usage, cost, or rate limits are missing

These fields require Codex CLI 0.150.1 and depend on the signed-in account and
billing route. Inspect `usageLastError` with `codexdog status --json`. A missing
USD estimate or credit balance can be a valid backend response and does not
indicate that recovery or the watchdog is broken.

### Codex was upgraded

Run:

```text
codexdog doctor
codexdog schema-check
codexdog smoke
```

Use `codexdog doctor --canary` only when one provider request is acceptable. An
app-server schema or transport change may require a Codexdog update even when
the interactive Codex command still works by itself.

## Build workflow behavior

`.github/workflows/build.yml` runs on pushes to `main`, version tags beginning
with `v`, pull requests, and manual dispatch. Each target builds on its native
GitHub-hosted runner, runs module verification, vet, and unit tests, verifies the
compiled target metadata, then uploads a packaged binary and SHA-256 sidecar.

After all target builds succeed for a pushed version tag, the workflow verifies
the packaged artifacts against their SHA-256 sidecars and publishes a GitHub
Release with generated release notes. The three archives and their checksum
files are attached to the release. Binaries are not signed or notarized.

`.github/workflows/codex-compatibility.yml` separately runs unit tests, schema
validation, and the no-model-turn smoke test against pinned Codex 0.150.1. A
scheduled `latest` job is allowed to fail so upstream app-server changes are
visible before they become release blockers. The build workflow also runs the
Go race detector on Linux.
