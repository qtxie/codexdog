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
  app-server schema shipped with Codex CLI 0.146.0.
- Go 1.24 or newer only when building from source.

Run these checks before using Codexdog:

```text
codex --version
codexdog version
codexdog smoke
```

`smoke` starts the installed app-server and validates the local protocol without
making a model request. `canary` also makes one minimal request through the
configured provider.

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

A timeout while compacting, including an SSE idle timeout, follows this same
generic terminal-error recovery path.

For a `cyberPolicy` failure, Codexdog first sends `continue`, then sends `继续` if
the same policy failure repeats, and finally forks through the failed turn and
sends `continue` once on the new thread. This sequence does not wait for provider
health probes.

### Optional health URL

Without `--health-url`, each recovery attempt directly runs an ephemeral Codex
canary through the configured provider.

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

## Telegram remote control

Telegram control uses outbound HTTPS long polling and does not open a public
port. Create a bot, keep its token private, and determine the numeric chat ID
that is allowed to control Codexdog. For a group, the chat ID is commonly a
negative number. An optional user allowlist can further restrict who may issue
commands inside an allowed group.

The bot token is accepted through an environment variable or a private token
file. A chat allowlist is mandatory whenever Telegram is enabled.

PowerShell example:

```powershell
$env:CODEXDOG_TELEGRAM_BOT_TOKEN = "123456:replace-me"
$env:CODEXDOG_TELEGRAM_CHAT_IDS = "123456789,-1001234567890"
$env:CODEXDOG_TELEGRAM_USER_IDS = "123456789"
codexdog start -C D:\work\project
```

Bash example using a token file:

```bash
mkdir -p ~/.config
install -m 0600 /path/to/bot-token ~/.config/codexdog-telegram-token
codexdog start -C /work/project \
  --telegram-token-file ~/.config/codexdog-telegram-token \
  --telegram-chat-id 123456789 \
  --telegram-user-id 123456789
```

Available bot commands:

| Command | Effect |
| --- | --- |
| `/status` | Show the supervisor, current thread, probe, and error state. |
| `/prompt TEXT` | Steer the active turn, or start a new turn on the current thread. |
| `/pause` | Interrupt the active turn and suppress automatic recovery. |
| `/resume` | Resume the current thread or reactivate its saved goal. |
| `/goal` | Show the current saved goal. |
| `/goal pause` | Pause the saved goal. |
| `/goal resume` | Reactivate the saved goal without a synthetic prompt. |
| `/goal set TEXT` | Replace the goal objective. |
| `/stop confirm` | Stop Codexdog and the Codex processes it owns. |
| `/help` | Show the command list. |

Lifecycle notifications are enabled by default. Use `--telegram-no-notify` to
keep command replies but disable unsolicited notifications. Telegram update
offsets are saved per workspace so old commands are not replayed after restart.
A Telegram outage is recorded in status and logs but does not change provider
recovery.

Remote prompts execute under the Codex sandbox and approval configuration of
the running session. Treat every allowed chat and user as someone who can direct
that session. Do not expose the bot token, state file, or control token.

## Command reference

| Command | Purpose |
| --- | --- |
| `codexdog start` | Start the supervisor, app-server, proxy, and Codex TUI. |
| `codexdog status` | Read live state for the selected workspace. |
| `codexdog stop` | Ask the selected workspace supervisor to shut down. |
| `codexdog smoke` | Validate app-server and proxy compatibility without a model turn. |
| `codexdog canary` | Run the smoke checks plus one provider model turn. |
| `codexdog version` | Print the Codexdog version. |
| `codexdog help` | Print CLI usage and option defaults. |

The main options are:

| Option | Meaning | Default |
| --- | --- | --- |
| `-C, --cwd DIR` | Workspace used by Codex and the state key. | Current directory |
| `--codex PATH` | Codex executable to launch. | `codex` |
| `-c, --config KEY=VALUE` | Repeatable Codex configuration override. | None |
| `--health-url URL` | Optional HTTP health precheck. | None |
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
| `--telegram-token-file PATH` | Read the Telegram bot token from a file. | None |
| `--telegram-chat-id ID` | Allow a chat; repeatable. | None |
| `--telegram-user-id ID` | Allow a sender; repeatable and optional. | None |
| `--telegram-poll-timeout-sec N` | Telegram long-poll duration from 1 to 50 seconds. | `30` |
| `--telegram-no-notify` | Disable unsolicited Telegram notifications. | Notifications enabled |
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

## State, logs, and shutdown

The default state directory is:

- Windows: `%LOCALAPPDATA%\codex-supervisor`
- Linux and macOS: `$HOME/.local/state/codex-supervisor`

Each workspace gets a hashed `state-*.json`, `supervisor-*.log`, and Telegram
offset file. State includes the loopback control port and a bearer token, so do
not publish it. Logs are capped at 2 MiB with three rotations and redact common
authorization, API-key, and token patterns. Review logs before sharing them
because arbitrary provider messages may still contain sensitive context.

On normal exit, Codexdog shuts down only the app-server and TUI process trees it
started. It does not scan for or terminate unrelated Codex sessions. On Windows,
owned processes also run in a kill-on-close Job Object so descendants are
cleaned up if Codexdog exits unexpectedly.

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

### The health URL always fails

Test the exact URL with a plain unauthenticated HTTP `GET`. Dashboard pages,
redirected login pages, authenticated endpoints, `400`, and `404` responses are
not healthy endpoints for Codexdog. Omitting `--health-url` uses the real Codex
canary alone.

### Codex appears to work forever without output

Enable the stalled-turn watchdog with a timeout suitable for the workload. If a
tool can legitimately stay silent for a long time, leave
`--tool-stall-timeout-ms` at `0` or set it substantially higher than the normal
turn timeout.

### Telegram does not reply

Verify the bot token, chat ID, optional user ID, and outbound HTTPS access to the
Telegram API. The chat allowlist must match `message.chat.id`; the optional user
allowlist must match `message.from.id`. Inspect `telegramLastError` in JSON status
and the workspace supervisor log.

### Codex was upgraded

Run:

```text
codexdog smoke
codexdog canary
```

An app-server schema or transport change may require a Codexdog update even when
the interactive Codex command still works by itself.

## Build workflow behavior

`.github/workflows/build.yml` runs on pushes to `main`, version tags beginning
with `v`, pull requests, and manual dispatch. Each target builds on its native
GitHub-hosted runner, runs module verification, vet, and unit tests, verifies the
compiled target metadata, then uploads a packaged binary and SHA-256 sidecar.

The workflow does not publish a GitHub Release and does not sign or notarize
binaries. Those are separate release-management and credentialed signing steps.
