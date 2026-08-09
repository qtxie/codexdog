# Codex Provider Supervisor

This wrapper keeps the normal Codex terminal UI, watches structured app-server turn events, resumes an interrupted thread after a custom model provider recovers, and can detect active turns that stop producing activity.

It reacts to provider/network failures and performs a bounded recovery sequence for `cyberPolicy` failures. Authentication, configuration, sandbox, context-window, approval, usage-limit, and user-interruption failures are left for the user.

## Install

Requirements:

- Node.js 20 or newer
- Codex CLI with `app-server` and `--remote` support
- An existing working Codex login and provider configuration

```powershell
npm install
npm run build
```

## Run

```powershell
node dist/cli.js start -C D:\path\to\repo
```

The command starts a localhost-only Codex app-server, a local observing proxy, and the standard Codex TUI. Run it from an interactive terminal, use Codex normally, and keep that terminal open while the task runs.

Pass Codex configuration overrides with repeatable `-c` flags. Put other TUI arguments after `--`:

```powershell
node dist/cli.js start -C . -c 'model="gpt-5.6-sol"' -- --sandbox workspace-write
```

Enable stalled-turn recovery with a conservative ten-minute silence timeout:

```powershell
node dist/cli.js start -C . --stall-timeout-ms 600000 -- --sandbox workspace-write
```

Check or stop a supervisor from another terminal:

```powershell
node dist/cli.js status -C .
node dist/cli.js status -C . --json
node dist/cli.js stop -C .
```

State and redacted rotating logs are stored under `%LOCALAPPDATA%\codex-supervisor` by default. Set `CODEX_SUPERVISOR_HOME` or pass `--state-dir` to change this.

## Recovery behavior

1. Observe `error` and terminal `turn/completed` events from the same connection used by the TUI.
2. Recover only connection, stream, timeout, overload, rate-limit, and upstream `5xx` failures.
3. Probe through a dedicated ephemeral Codex thread so the canary uses the same provider and authentication.
4. Require two successful canaries by default.
5. Start one continuation turn on the exact failed thread.
6. For `cyberPolicy`, retry the same thread with `continue`, retry it with `继续`, then fork through the failed turn and try `continue` once on the new thread. These retries do not wait for provider probes.
7. When the stalled-turn watchdog is enabled, mark a silent turn as suspected, wait through a confirmation window, verify that the thread is still active, interrupt it, and send `continue` only after Codex reports the old turn as interrupted.
8. Stop after five consecutive automatic resumptions, after the forked cyber-policy retry also fails, after two consecutive stalled-turn resumptions, or on any other permanent failure.

The provider probe never reads Codex credentials and is instructed not to call tools. A successful canary consumes a very small model request. Configure `--health-url` to avoid canaries while a provider's cheap health endpoint is still failing.

The continuation is semantic: Codex reuses the saved thread and current workspace, but cannot resume at the exact interrupted output token.

## Stalled-turn watchdog

The watchdog is disabled by default. Set `--stall-timeout-ms` to a non-zero value to enable it. Activity includes turn and item lifecycle events, streamed agent or reasoning output, command output, plan and diff updates, hooks, and token-usage updates.

The normal stall timeout is paused while Codex is waiting for approval or user input, performing account verification or safety buffering, or running a command, MCP call, web search, collaboration call, hook, file change, image view, or context compaction. Quiet active tools are never interrupted by default. Set `--tool-stall-timeout-ms` only when you also want a separate upper bound for those operations.

A suspected turn must remain silent for `--stall-confirm-ms` and still report an active thread status before it is interrupted. Any matching activity during confirmation cancels recovery. User-initiated Esc interruptions remain manual and are never automatically continued.

## Configuration

```text
--probe-timeout-ms 120000
--probe-successes 2
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

Provider configuration belongs in the user-level Codex config. Current Codex versions ignore provider redirects in project `.codex/config.toml` files.

## Protocol compatibility

The implementation targets the generated app-server schema from Codex CLI `0.146.0`. Local WebSocket app-server transport is experimental in Codex. Run the test suite and an end-to-end canary after upgrading Codex:

```powershell
npm test
npm run check
npm run smoke
npm run canary
```

`npm run smoke` initializes the installed app-server and proxy but does not start a model turn or consume provider tokens.
`npm run canary` additionally sends one minimal request through the configured provider.
